package relay

import (
	"crypto/tls"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"tgcontrol/internal/config"
	"tgcontrol/internal/dnsfallback"
	"tgcontrol/internal/wsutil"
)

// streamTLSCache enables TLS session resumption across stream dial-backs. Each
// terminal/Remote-Desktop open dials a fresh wss:// connection to the relay's
// /v1/agent/stream; without a shared cache every dial pays a full TLS
// handshake (~1 extra RTT to the relay). Reusing tickets cuts the handshake to
// a resumed 1-RTT, so opening a terminal over the cloud feels noticeably
// snappier. Safe for concurrent use.
var streamTLSCache = tls.NewLRUClientSessionCache(64)

// handleStreamOpen bridges a relay-requested streaming WS (PTY I/O or Remote
// Desktop frames) to the agent's own local /ws endpoint.
//
// It dials TWO connections:
//   - outbound to the relay's /v1/agent/stream (carrying the one-time token),
//   - to 127.0.0.1<path> on the local web server (authenticated with the API
//     token, exactly like a LAN client would),
//
// then pipes raw frames between them. The relay sits in the middle as a
// transparent byte-pump, so the PTY/Remote protocols travel unchanged.
func (c *Client) handleStreamOpen(so StreamOpen) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[RELAY] panic in handleStreamOpen: %v", r)
		}
	}()

	if !validStreamPath(so.Path) {
		log.Printf("[RELAY] stream rejected: invalid path %q", so.Path)
		return
	}

	relayWS := config.GetNoSetup().RelayStreamWS()
	if relayWS == "" {
		log.Printf("[RELAY] stream: no relay stream URL configured")
		return
	}

	// 1. Dial back to the relay for this stream. A shared TLS session cache lets
	// repeated stream opens resume the TLS session instead of a full handshake.
	relayURL := relayWS + "?token=" + url.QueryEscape(so.Token)
	hdr := http.Header{}
	hdr.Set("Authorization", "Bearer "+c.currentJWT())
	dialer := &websocket.Dialer{
		HandshakeTimeout: 15 * time.Second,
		TLSClientConfig:  &tls.Config{ClientSessionCache: streamTLSCache},
		NetDialContext:   dnsfallback.DialContext, // тот же запасной резолвер, что у управляющего канала
		// …и тот же прокси. Без него на машине, где наружу ходят только через
		// прокси в переменных окружения, получалась необъяснимая картина:
		// компьютер числится «на связи» (управляющий канал прокси учитывает),
		// списки и файлы работают, а терминал и «Экран» вечно висят на
		// «Подключение к терминалу…» — потому что обратный дозвон стрима шёл
		// напрямую и не доходил. К локальному плечу Go прокси не применяет,
		// так что loopback не ломается.
		Proxy: http.ProxyFromEnvironment,
	}
	relayConn, resp, err := dialer.Dial(relayURL, hdr)
	if err != nil {
		if resp != nil {
			log.Printf("[RELAY] stream dial relay: %v (HTTP %d)", err, resp.StatusCode)
		} else {
			log.Printf("[RELAY] stream dial relay: %v", err)
		}
		return
	}
	defer relayConn.Close()

	// 2. Dial the local /ws endpoint (same auth as a LAN client).
	cfg := config.Get()
	localURL := localStreamURL(cfg.Port(), so.Path, cfg.APIToken)
	localConn, localResp, err := dialer.Dial(localURL, nil)
	if err != nil {
		// Код ответа обязателен: «bad handshake» сам по себе не отличает
		// 401 (протух токен) от 404 (сессии нет) и от 403. 19.08.2026 по жалобе
		// «терминалы отваливаются» в логе стояли 28 таких строк — и ни одна не
		// говорила, наша это вина или нет; разбор упёрся в то, что ответ
		// выбрасывался в «_». Дозвон до релея (выше) код печатает давно.
		if localResp != nil {
			log.Printf("[RELAY] stream dial local %s: %v (HTTP %d)", so.Path, err, localResp.StatusCode)
		} else {
			log.Printf("[RELAY] stream dial local %s: %v", so.Path, err)
		}
		_ = relayConn.WriteControl(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseInternalServerErr, "local dial failed"),
			time.Now().Add(time.Second))
		return
	}
	defer localConn.Close()

	log.Printf("[RELAY] stream bridged path=%s id=%s", so.Path, so.StreamID)
	pipeStream(relayConn, localConn)
}

// pipeStream copies WebSocket messages both ways between the relay leg and the
// local /ws/pty (or /ws/screen) leg, preserving the text/binary frame type.
// Returns when either side closes or errors.
//
// It also records WHICH leg dropped first. When the far (relay/client) leg goes
// away before the local PTY does — the common case when a phone backgrounds the
// app and its WS is frozen — we send the local handler a private close code
// (CloseClientGone) so connstat attributes the disconnect to the client instead
// of an opaque connection-lost. A local-first teardown (process exit, handler
// shutdown) needs no signal: the agent already classifies that precisely.
func pipeStream(relayConn, localConn *websocket.Conn) {
	relayConn.SetReadLimit(8 << 20)
	localConn.SetReadLimit(8 << 20)

	var once sync.Once
	closeBoth := func(farEndFirst bool) {
		once.Do(func() {
			if farEndFirst {
				_ = localConn.WriteControl(websocket.CloseMessage,
					websocket.FormatCloseMessage(wsutil.CloseClientGone, "client-gone"),
					time.Now().Add(time.Second))
			}
			relayConn.Close()
			localConn.Close()
		})
	}

	// relay → local: a read error here means the far (relay/client) leg closed
	// first; a write error means the local leg is the one that failed.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			mt, data, err := relayConn.ReadMessage()
			if err != nil {
				closeBoth(true)
				return
			}
			// Bound the write so a stalled peer can't wedge this pump goroutine
			// (and leak the bridged sockets) forever.
			_ = localConn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := localConn.WriteMessage(mt, data); err != nil {
				closeBoth(false)
				return
			}
		}
	}()

	// local → relay: read error = local leg closed first; write error = far end.
	for {
		mt, data, err := localConn.ReadMessage()
		if err != nil {
			closeBoth(false)
			break
		}
		_ = relayConn.SetWriteDeadline(time.Now().Add(10 * time.Second))
		if err := relayConn.WriteMessage(mt, data); err != nil {
			closeBoth(true)
			break
		}
	}
	<-done
}

// streamQueryAllowed — параметры, которые клиенту разрешено передать в пути
// стрима. Всё остальное вырезается: путь приходит снаружи, и без белого списка
// через него можно было бы подсунуть свой initData (наш встал бы вторым, а
// http.Request.URL.Query().Get отдаёт ПЕРВОЕ значение) или любой другой
// служебный параметр локального обработчика.
var streamQueryAllowed = map[string]bool{
	// resume=<epoch>:<offset> — «дошли этой позиции, дошли остальное».
	// Ради него всё и затевалось: без проброса каждое переподключение телефона
	// тянуло весь буфер (512 КиБ) и приходило с маркером reset, то есть
	// стирало прокрученную историю на экране.
	"resume": true,
}

// localStreamURL собирает адрес локального /ws-обработчика из пути, пришедшего
// от релея.
//
// Раньше здесь была простая склейка «путь + ?initData=…», и она молча ломалась,
// как только в пути появлялся собственный запрос: получалось два знака вопроса
// («/ws/pty/x?resume=a:1?initData=token:…»), initData уезжал ВНУТРЬ значения
// resume, и локальный обработчик отвечал отказом авторизации. Поэтому путь
// разбираем, чужие параметры отбрасываем, свой initData ставим сами.
func localStreamURL(port int, path, apiToken string) string {
	u, err := url.Parse(path)
	if err != nil {
		u = &url.URL{Path: path}
	}
	q := url.Values{}
	for key, values := range u.Query() {
		if streamQueryAllowed[key] && len(values) > 0 {
			q.Set(key, values[0])
		}
	}
	q.Set("initData", "token:"+apiToken)
	// Метка транспорта: локальный обработчик иначе не отличит облачный стрим от
	// домашнего — сюда всё приходит с 127.0.0.1, потому что мост открываем мы сами.
	// Ставим ПОСЛЕ фильтрации по белому списку: пришедший от клиента transport
	// отбрасывается там же, где и любой чужой параметр, подменить метку нельзя.
	q.Set("transport", "relay")
	return fmt.Sprintf("ws://127.0.0.1:%d%s?%s", port, u.EscapedPath(), q.Encode())
}

// validStreamPath mirrors the relay-side check: the agent only ever dials its
// own local /ws endpoints, never an arbitrary URL.
func validStreamPath(p string) bool {
	if !strings.HasPrefix(p, "/ws/") {
		return false
	}
	if strings.Contains(p, "..") || strings.Contains(p, "://") {
		return false
	}
	if strings.ContainsAny(p, " \t\r\n") {
		return false
	}
	return true
}
