package server

import (
	"context"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/gorilla/websocket"

	"tgcontrol-relay/internal/auth"
	"tgcontrol-relay/internal/db"
	"tgcontrol-relay/internal/protocol"
)

// Streaming bridge — tunnels binary WS streams (PTY I/O, Remote Desktop frames)
// between a remote client and a desktop agent's local /ws endpoint.
//
// The request/response control channel (handleClientWS / handleClientRequest)
// can't carry continuous binary streams, so for /ws/pty and /ws/screen we set
// up a dedicated reverse tunnel:
//
//	phone --WS--> relay (/v1/client/{id}/stream)
//	                 |  StreamOpen over control WS
//	                 v
//	            agent dials back --WS--> relay (/v1/agent/stream)
//	                 |
//	            agent --WS--> 127.0.0.1/ws/{pty,screen}
//
// The relay is a transparent byte-pump in the middle and never parses the
// stream payload, so xterm.js / the canvas renderer work byte-for-byte the same
// as on the LAN path.

const (
	streamHandshakeTimeout = 15 * time.Second
	streamReadLimit        = 8 << 20 // 8 MB — generous for big JPEG frames / pastes
	streamPingInterval     = 30 * time.Second
	// streamPongWait — сколько плечо терпит молчание, прежде чем считать
	// собеседника мёртвым. Пинги мы слали и раньше, но ОТВЕТ никто не сверял:
	// телефон, умерший без FIN (метро, авиарежим, разрыв VPN), оставался
	// зрителем терминала до 15 минут. А зритель — это его размер: PTY держит
	// МИНИМУМ по зрителям, поэтому человек, севший за компьютер, видел TUI,
	// перерисованный в коробку призрачного телефона, и «👁 2» при одном
	// открытом экране. Больнее всего это на ТИХОМ терминале: там нет трафика,
	// который выявил бы разрыв сам.
	streamPongWait = 70 * time.Second
	// streamWriteWait — потолок одной записи, чтобы застрявшее плечо не держало
	// насос вечно. У зеркального агентского pipeStream он есть, у релея не было.
	streamWriteWait = 10 * time.Second
)

var streamUpgrader = websocket.Upgrader{
	ReadBufferSize:  1 << 14,
	WriteBufferSize: 1 << 14,
	CheckOrigin:     func(r *http.Request) bool { return true },
	// EnableCompression — permessage-deflate на стрим-плечах. Заведено 11.08.2026
	// по жалобе «открываю терминал, жду секунд 20, пока появится текст». Замер по
	// этим самым логам: КАЖДОЕ открытие терминала — 0,5–0,9 МБ вниз (медиана
	// ~580 КБ) при 250–1700 байтах вверх, потому что 68% открытий приходят без
	// ?resume= и агент досылает весь хвост кольца. По мобильному плечу это и есть
	// те 20 секунд. Буферы терминалов — ANSI-поток с непрерывной перерисовкой TUI —
	// жмутся в 8,1 раза (замер gzip -6 по живым pty-scrollback: 416 КБ → 51 КБ).
	//
	// Отображение не затрагивается вовсе: релей остаётся прозрачным байт-насосом,
	// после распаковки в xterm.js приходят те же байты. Расширение СОГЛАСУЕМОЕ —
	// сторона, которая его не поддержала, работает как раньше, без сжатия.
	// Поэтому включение безопасно и на агентском плече (строка 176): браузер
	// просит deflate сам, а Go-агент не просит, пока не выставит его в Dialer.
	EnableCompression: true,
}

// handleClientStream — GET /v1/client/{deviceID}/stream?jwt=...&path=/ws/pty/{id}
//
// The remote client opens this WS to start a streaming session. We ask the
// agent to bridge it, wait for the agent's reverse connection, then pump bytes.
func (s *Server) handleClientStream(w http.ResponseWriter, r *http.Request) {
	deviceID := chi.URLParam(r, "deviceID")
	claims, err := s.requireUserAuth(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	dev, err := s.deviceForUser(r, deviceID, claims.UserID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	role, err := db.DeviceRole(r.Context(), s.DB, deviceID, claims.UserID)
	if err != nil || role == db.RoleViewer {
		http.Error(w, "viewer access does not allow interactive streams", http.StatusForbidden)
		return
	}
	// Платная граница: терминал и экран через облако. Проверяем ДО апгрейда в
	// WebSocket — отказ по HTTP клиент показывает человеку, а закрытый сокет
	// выглядит как «сломалось».
	if !s.cloudGate(w, r, dev.UserID) {
		return
	}

	path := r.URL.Query().Get("path")
	if !validStreamPath(path) {
		http.Error(w, "invalid stream path", http.StatusBadRequest)
		return
	}
	// Стрим удалённого рабочего стола — считаем фичу screen (в горутине).
	if strings.HasPrefix(path, "/ws/screen") {
		s.bumpFeature(claims.UserID, "screen")
	}

	agent := s.Hub.Get(deviceID)
	if agent == nil {
		http.Error(w, "agent offline", http.StatusBadGateway)
		return
	}

	ps := s.Hub.RegisterStream(deviceID, claims.UserID, path)
	if err := agent.SendStreamOpen(protocol.StreamOpen{StreamID: ps.ID, Token: ps.Token, Path: path}); err != nil {
		s.Hub.RemoveStream(ps.Token)
		http.Error(w, "agent unreachable", http.StatusBadGateway)
		return
	}

	client, err := streamUpgrader.Upgrade(w, r, nil)
	if err != nil {
		s.Hub.RemoveStream(ps.Token)
		log.Printf("[WS-STREAM] client upgrade: %v", err)
		return
	}
	defer client.Close()
	accessCtx, cancelAccess := context.WithCancel(r.Context())
	defer cancelAccess()
	go s.watchCloudAccess(accessCtx, client, dev.UserID, 30*time.Second)

	select {
	case agentConn := <-ps.AgentCh:
		close(ps.Taken) // tell the agent handler we own the conn now
		log.Printf("[WS-STREAM] bridged device=%s path=%s stream=%s ua=%q origin=%q",
			deviceID, path, ps.ID, r.Header.Get("User-Agent"), r.Header.Get("Origin"))
		// Регистрируем живой мост, чтобы отзыв устройства/гранта мог его порвать
		// (стрим-плечо — отдельный сокет от control-WS; см. Hub.CloseLiveBridges).
		lb := s.Hub.AddLiveBridge(deviceID, claims.UserID, func() {
			client.Close()
			agentConn.Close()
		})
		// Открытый терминал/экран — вторая половина «человек в приложении»:
		// сюда попадает даже тот, у кого control-WS не поднялся (см. presence.go).
		detachPresence := s.Hub.ClientAttach(deviceID, claims.UserID)
		pipeStreams(client, agentConn, ps.ID)
		detachPresence()
		s.Hub.RemoveLiveBridge(lb)
		agentConn.Close()
	case <-time.After(streamHandshakeTimeout):
		s.Hub.RemoveStream(ps.Token)
		_ = client.WriteControl(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseTryAgainLater, "agent stream timeout"),
			time.Now().Add(time.Second))
		log.Printf("[WS-STREAM] agent did not dial back: device=%s stream=%s", deviceID, ps.ID)
	}
}

// handleAgentStream — GET /v1/agent/stream?jwt=<deviceJWT>&token=<streamToken>
//
// The agent dials this in response to a StreamOpen. We match the token to a
// pending stream and hand the connection to the waiting client handler.
func (s *Server) handleAgentStream(w http.ResponseWriter, r *http.Request) {
	tok := bearer(r.Header.Get("Authorization"))
	if tok == "" {
		tok = r.URL.Query().Get("jwt")
	}
	if tok == "" {
		http.Error(w, "missing token", http.StatusUnauthorized)
		return
	}
	claims, err := s.JWT.ParseDevice(tok)
	if err != nil {
		http.Error(w, "invalid token: "+err.Error(), http.StatusUnauthorized)
		return
	}
	if revoked, _ := auth.IsRevoked(r.Context(), s.DB, claims.ID); revoked {
		http.Error(w, "token revoked", http.StatusUnauthorized)
		return
	}
	// Маркер отзыва устройства по device_id — единственная защита этого
	// эндпоинта от JWT отозванного устройства (строку devices здесь не читаем).
	if devRevoked, _ := auth.IsDeviceRevoked(r.Context(), s.DB, claims.DeviceID); devRevoked {
		http.Error(w, "token revoked", http.StatusUnauthorized)
		return
	}

	streamToken := r.URL.Query().Get("token")
	ps, err := s.Hub.ClaimStream(streamToken, claims.DeviceID, claims.UserID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}

	conn, err := streamUpgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("[WS-STREAM] agent upgrade: %v", err)
		return
	}

	// Hand the connection to the waiting client handler. AgentCh is buffered so
	// this never blocks even if the client raced ahead. Then wait until the
	// client takes ownership (closes Taken); if it gave up, clean up ourselves.
	ps.AgentCh <- conn
	select {
	case <-ps.Taken:
		// Client owns the conn lifecycle now — return, leaving it open.
	case <-time.After(streamHandshakeTimeout + 5*time.Second):
		conn.Close()
	}
}

// pipeStreams copies WebSocket messages both ways, preserving the text/binary
// frame type, with a periodic ping so idle PTY streams survive proxy idle
// timeouts. Returns when either side closes or errors. Каждый насос логирует,
// какое плечо умерло первым и с какой ошибкой — иначе обрыв моста неотличим
// от ухода клиента (диагностика «терминал не подключается»).
func pipeStreams(a, b *websocket.Conn, streamID string) {
	a.SetReadLimit(streamReadLimit)
	b.SetReadLimit(streamReadLimit)

	// Дедлайн чтения + продление по pong и по любому пришедшему кадру: молчание
	// дольше streamPongWait означает, что собеседника больше нет. Живой поток
	// продлевает дедлайн сам (каждый кадр — доказательство жизни), тихий
	// терминал держится на пингах раз в 30 с, то есть двух подряд потерянных
	// ответов достаточно для разрыва.
	arm := func(c *websocket.Conn) {
		_ = c.SetReadDeadline(time.Now().Add(streamPongWait))
		c.SetPongHandler(func(string) error {
			return c.SetReadDeadline(time.Now().Add(streamPongWait))
		})
	}
	arm(a)
	arm(b)

	var closeOnce sync.Once
	shutdown := func() {
		closeOnce.Do(func() {
			a.Close()
			b.Close()
		})
	}

	pump := func(dst, src *websocket.Conn, label string) {
		defer shutdown()
		var msgs, bytes int64
		for {
			mt, data, err := src.ReadMessage()
			if err != nil {
				log.Printf("[WS-STREAM] %s %s: read end after %d msgs / %d bytes: %v", streamID, label, msgs, bytes, err)
				return
			}
			// Пришли данные — собеседник жив, дедлайн отсчитывается заново.
			_ = src.SetReadDeadline(time.Now().Add(streamPongWait))
			_ = dst.SetWriteDeadline(time.Now().Add(streamWriteWait))
			if err := dst.WriteMessage(mt, data); err != nil {
				log.Printf("[WS-STREAM] %s %s: write end after %d msgs / %d bytes: %v", streamID, label, msgs, bytes, err)
				return
			}
			msgs++
			bytes += int64(len(data))
		}
	}

	done := make(chan struct{})
	go func() { pump(a, b, "agent→client"); close(done) }()
	go pump(b, a, "client→agent")

	// Keepalive — WriteControl is safe concurrently with WriteMessage.
	ticker := time.NewTicker(streamPingInterval)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			shutdown()
			return
		case <-ticker.C:
			deadline := time.Now().Add(5 * time.Second)
			if err := a.WriteControl(websocket.PingMessage, nil, deadline); err != nil {
				shutdown()
				return
			}
			if err := b.WriteControl(websocket.PingMessage, nil, deadline); err != nil {
				shutdown()
				return
			}
		}
	}
}

// streamQueryAllowed — параметры запроса, которые разрешено пронести в пути
// стрима до агента. Путь приходит от клиента, поэтому список закрытый: агент
// дописывает к этому пути СВОЙ initData, и посторонний параметр с тем же именем
// встал бы первым (Query().Get отдаёт первое значение).
//
// resume=<epoch>:<offset> — «у меня уже есть поток до этой позиции». Без него
// каждое переподключение телефона тянет весь буфер терминала (512 КиБ) и
// приходит с маркером reset, то есть стирает прокрученную историю на экране.
var streamQueryAllowed = map[string]bool{"resume": true}

// validStreamPath restricts the agent to dialling its own local /ws endpoints.
func validStreamPath(p string) bool {
	if !strings.HasPrefix(p, "/ws/") {
		return false
	}
	if strings.Contains(p, "..") || strings.ContainsAny(p, " \t\r\n") {
		return false
	}
	// No scheme/host injection — must be a bare path.
	if strings.Contains(p, "://") {
		return false
	}
	u, err := url.Parse(p)
	if err != nil {
		return false
	}
	// Запрос допускается, но только из известных параметров: иначе через путь
	// можно было бы передать локальному обработчику агента что угодно.
	for key := range u.Query() {
		if !streamQueryAllowed[key] {
			return false
		}
	}
	return true
}
