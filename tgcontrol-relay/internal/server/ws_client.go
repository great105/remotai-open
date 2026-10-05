package server

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"

	"tgcontrol-relay/internal/db"
	"tgcontrol-relay/internal/protocol"
	"tgcontrol-relay/internal/relayhub"
)

var clientUpgrader = websocket.Upgrader{
	ReadBufferSize:  1 << 14,
	WriteBufferSize: 1 << 14,
	CheckOrigin:     func(r *http.Request) bool { return true }, // Mini App может прийти с любого Telegram-домена
}

// handleClientWS — WS endpoint, который Mini App / APK открывают для долгоживущей
// связи с конкретным устройством. Все сообщения JSON.
//
// Client → Server:
//
//	{ "type": "cmd", request_id, method, path, query, body, headers }
//	{ "type": "ping" }
//	{ "type": "hidden" } / { "type": "visible" } — кадр видимости (см. присутствие)
//
// Server → Client:
//
//	{ "type": "cmd_result", request_id, status_code, body, headers, error }
//	{ "type": "event", channel, payload }
//	{ "type": "agent_status", online: bool }
//	{ "type": "pong" }
func (s *Server) handleClientWS(w http.ResponseWriter, r *http.Request) {
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
	if err != nil {
		http.Error(w, "device access required", http.StatusForbidden)
		return
	}
	// Платная граница: управление своим ПК через облако. Здесь же стартует
	// проба — это и есть «первое облачное подключение» из канона.
	if !s.cloudGate(w, r, dev.UserID) {
		return
	}

	conn, err := clientUpgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("[WS-CLIENT] upgrade: %v", err)
		return
	}
	defer conn.Close()
	accessCtx, cancelAccess := context.WithCancel(r.Context())
	defer cancelAccess()
	go s.watchCloudAccess(accessCtx, conn, dev.UserID, 30*time.Second)

	// Присутствие: пока клиент не сказал обратного, живой control-WS = «человек в
	// приложении», и релейный notifier не дублирует в Telegram вопрос агента,
	// который и так виден на экране.
	//
	// Кадры видимости (см. switch ниже) снимают и возвращают присутствие, НЕ
	// разрывая сокет: забытая вкладка remotai.ru/app держала присутствие вечно —
	// сокет живой, ping идёт, а человек экрана не видит, и уведомление «агент
	// ждёт ответа» не приходило никогда.
	//
	// Совместимость: старый клиент кадров не шлёт → присутствие стоит от коннекта
	// до разрыва, ровно как раньше. Нативный APK кадров тоже не шлёт СОЗНАТЕЛЬНО:
	// в фоне он поднимает системное уведомление сам, и Telegram-сообщение было бы
	// вторым сигналом на один вопрос.
	//
	// setPresent зовут только из read-цикла и из defer — обе точки в этой же
	// горутине, поэтому синхронизация не нужна. Возвращает true, если состояние
	// действительно изменилось: повтор того же кадра не должен ни дёргать хаб,
	// ни писать в лог.
	presence := s.Hub.ClientAttach(deviceID, claims.UserID)
	setPresent := func(on bool) bool {
		if on == (presence != nil) {
			return false
		}
		if on {
			presence = s.Hub.ClientAttach(deviceID, claims.UserID)
		} else {
			presence()
			presence = nil
		}
		return true
	}
	defer func() { setPresent(false) }()

	// Все записи в conn — через мьютекс: события, agent_status, pong и
	// cmd_result пишутся из разных горутин, а gorilla паникует на
	// конкурентной записи.
	lconn := &lockedWSConn{conn: conn}

	// Статус агента и подписка на его события — динамические. Агент мог быть
	// офлайн в момент подключения клиента (гонка первого пейринга: телефон
	// открывает WS на пару секунд раньше, чем ПК доходит до релея) или
	// переподключиться с новым session — вотчер сверяет текущий AgentConn,
	// при смене шлёт клиенту свежий agent_status и пересоздаёт подписку.
	stop := make(chan struct{})
	watcherDone := make(chan struct{})
	go func() {
		defer close(watcherDone)
		ticker := time.NewTicker(3 * time.Second)
		defer ticker.Stop()
		var agent *relayhub.AgentConn
		var evCh chan protocol.Event
		unsub := func() {
			if agent != nil && evCh != nil {
				agent.UnsubscribeEvents(evCh)
			}
			evCh = nil
		}
		defer unsub()
		resync := func(force bool) bool {
			cur := s.Hub.Get(deviceID)
			if !force && cur == agent {
				return true
			}
			unsub()
			agent = cur
			if agent != nil {
				evCh = agent.SubscribeEvents(64)
			}
			return lconn.WriteJSON(map[string]any{"type": "agent_status", "online": agent != nil}) == nil
		}
		if !resync(true) {
			return
		}
		for {
			if evCh != nil {
				select {
				case <-stop:
					return
				case <-ticker.C:
					if !resync(false) {
						return
					}
				case e, ok := <-evCh:
					if !ok {
						// Агент отключился — хаб закрыл канал; ближайший resync
						// заметит нового (или отправит online:false).
						evCh = nil
						continue
					}
					if lconn.WriteJSON(map[string]any{
						"type":    "event",
						"channel": e.Channel,
						"payload": e.Payload,
					}) != nil {
						return
					}
				}
			} else {
				select {
				case <-stop:
					return
				case <-ticker.C:
					if !resync(false) {
						return
					}
				}
			}
		}
	}()

	for {
		conn.SetReadDeadline(time.Now().Add(120 * time.Second))
		var raw json.RawMessage
		if err := conn.ReadJSON(&raw); err != nil {
			break
		}
		var head struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(raw, &head); err != nil {
			continue
		}
		switch head.Type {
		case "ping":
			_ = lconn.WriteJSON(map[string]string{"type": "pong"})
		case "hidden", "visible":
			// Вкладка/мини-апп ушли в фон или вернулись. Сокет держим живым
			// (возврат должен быть мгновенным и без реплея), но фоновая вкладка
			// присутствием больше не считается. Лог — чтобы «уведомления не
			// приходят» разбиралось по журналу, а не догадками.
			visible := head.Type == "visible"
			if setPresent(visible) {
				log.Printf("[WS-CLIENT] visibility device=%s user=%d visible=%v",
					deviceID, claims.UserID, visible)
			}
		case "cmd":
			s.forwardClientCmd(r.Context(), lconn, deviceID, raw, role != db.RoleViewer)
		}
	}
	close(stop)
	<-watcherDone
}

// lockedWSConn сериализует записи в websocket-соединение (gorilla не допускает
// конкурентных писателей).
type lockedWSConn struct {
	mu   sync.Mutex
	conn *websocket.Conn
}

func (l *lockedWSConn) WriteJSON(v any) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.conn.WriteJSON(v)
}

// forwardClientCmd берёт cmd от клиента, отправляет агенту через Hub, ждёт ответ
// и пересылает обратно.
func (s *Server) forwardClientCmd(ctx context.Context, client *lockedWSConn, deviceID string, raw json.RawMessage, canOperate bool) {
	var cmd protocol.Cmd
	if err := json.Unmarshal(raw, &cmd); err != nil {
		return
	}
	if cmd.RequestID == "" {
		cmd.RequestID = uuid.NewString()
	}
	cmd.Type = protocol.MsgCmd
	if !canOperate && cmd.Method != http.MethodGet && cmd.Method != http.MethodHead {
		_ = client.WriteJSON(protocol.CmdResult{
			Type: protocol.MsgCmdResult, RequestID: cmd.RequestID,
			StatusCode: http.StatusForbidden, Error: "viewer access is read-only",
		})
		return
	}

	agent := s.Hub.Get(deviceID)
	if agent == nil {
		_ = client.WriteJSON(protocol.CmdResult{
			Type: protocol.MsgCmdResult, RequestID: cmd.RequestID,
			StatusCode: http.StatusBadGateway, Error: relayhub.ErrAgentOffline.Error(),
		})
		return
	}
	res, err := agent.Send(ctx, cmd, 60*time.Second)
	if err != nil {
		_ = client.WriteJSON(protocol.CmdResult{
			Type: protocol.MsgCmdResult, RequestID: cmd.RequestID,
			StatusCode: http.StatusGatewayTimeout, Error: err.Error(),
		})
		return
	}
	_ = client.WriteJSON(res)
}
