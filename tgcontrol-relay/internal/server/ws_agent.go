package server

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"

	"tgcontrol-relay/internal/auth"
	"tgcontrol-relay/internal/db"
	"tgcontrol-relay/internal/metrics"
	"tgcontrol-relay/internal/protocol"
)

var agentUpgrader = websocket.Upgrader{
	ReadBufferSize:  1 << 14,
	WriteBufferSize: 1 << 14,
	CheckOrigin:     func(r *http.Request) bool { return true }, // агенты приходят без Origin
}

// handleAgentConnect — WS endpoint для outbound подключения десктоп-агента.
// Auth: JWT в Authorization: Bearer ... ИЛИ ?jwt=... в query (для клиентов где
// нельзя задать заголовок).
func (s *Server) handleAgentConnect(w http.ResponseWriter, r *http.Request) {
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
	revoked, _ := auth.IsRevoked(r.Context(), s.DB, claims.ID)
	if revoked {
		http.Error(w, "token revoked", http.StatusUnauthorized)
		return
	}
	// Blocklist-маркер отзыва устройства (DELETE /v1/devices/{id}, revoke-self):
	// настоящий jti агента серверу неизвестен, поэтому отзыв — по device_id.
	if devRevoked, _ := auth.IsDeviceRevoked(r.Context(), s.DB, claims.DeviceID); devRevoked {
		http.Error(w, "token revoked", http.StatusUnauthorized)
		return
	}
	// Отзыв устройства ставит revoked_at И пишет blocklist-маркер — здесь
	// дополнительно проверяем состояние устройства напрямую (маркер best-effort).
	// Заодно отклоняем JWT, чей user_id больше не совпадает с владельцем (устройство
	// переназначено на другой аккаунт после revoke). При ошибке БД — отказ.
	dev, err := db.GetDeviceForAgentAuth(r.Context(), s.DB, claims.DeviceID, claims.UserID)
	if err != nil {
		http.Error(w, "device not found", http.StatusUnauthorized)
		return
	}
	if dev.RevokedAt.Valid {
		http.Error(w, "device revoked", http.StatusUnauthorized)
		return
	}
	conn, err := agentUpgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("[WS-AGENT] upgrade failed: %v", err)
		return
	}

	// Прочитать Hello
	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	var hello protocol.Hello
	if err := conn.ReadJSON(&hello); err != nil {
		log.Printf("[WS-AGENT] hello read: %v", err)
		_ = conn.WriteJSON(map[string]string{"type": protocol.MsgClose, "reason": "hello timeout"})
		conn.Close()
		return
	}
	conn.SetReadDeadline(time.Time{})

	if hello.DeviceID != claims.DeviceID {
		log.Printf("[WS-AGENT] hello device_id mismatch: hello=%s jwt=%s", hello.DeviceID, claims.DeviceID)
		_ = conn.WriteJSON(map[string]string{"type": protocol.MsgClose, "reason": "device_id mismatch"})
		conn.Close()
		return
	}

	sessionID := uuid.NewString()
	ac := s.Hub.Register(hello.DeviceID, dev.UserID, sessionID, conn)

	// Подписка notifier'а — ДО reader-цикла: события, пришедшие первыми же
	// кадрами (агент переподключился, а вопрос агента уже висит на экране),
	// иначе дропаются как «нет подписчиков». Живёт до ac.Close().
	s.Notifier.Attach(ac)

	// Welcome — с продлением JWT (refresh-on-connect): выдаём свежий device JWT,
	// агент его сохранит, поэтому привязка не протухает, пока ПК хоть иногда
	// онлайн. Если выпуск не удался — просто не продлеваем (старый JWT ещё валиден).
	welcome := protocol.Welcome{
		Type:      protocol.MsgWelcome,
		SessionID: sessionID,
		IssuedAt:  time.Now().Unix(),
		UserID:    dev.UserID,
	}
	if fresh, exp, jerr := s.JWT.IssueDevice(claims.DeviceID, dev.UserID); jerr == nil {
		welcome.RefreshedJWT = fresh
		welcome.RefreshedExpiry = exp.Unix()
	}
	// Через сериализованный writer агента, а НЕ напрямую в conn: с момента
	// Hub.Register агенту уже могут писать команды (кнопка ответа из бота,
	// запрос с телефона), и две параллельные записи в один websocket дают
	// панику «concurrent write» с разрывом только что поднятого соединения.
	if err := ac.SendWelcome(welcome); err != nil {
		ac.Close()
		return
	}

	if err := db.MarkDeviceOnline(r.Context(), s.DB, hello.DeviceID, true, hello.AgentVersion); err != nil {
		log.Printf("[WS-AGENT] mark online: %v", err)
	}
	metrics.AgentConnect()
	log.Printf("[WS-AGENT] agent online: device=%s user=%d session=%s", hello.DeviceID, dev.UserID, sessionID)

	// Ping loop — goes through the AgentConn's serialized writer so it never
	// races a proxied command write (concurrent gorilla writes corrupt frames).
	stopPing := make(chan struct{})
	go func() {
		t := time.NewTicker(25 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-stopPing:
				return
			case <-t.C:
				if err := ac.Ping(); err != nil {
					return
				}
			}
		}
	}()

	// Reader loop
	for {
		conn.SetReadDeadline(time.Now().Add(90 * time.Second))
		var raw json.RawMessage
		if err := conn.ReadJSON(&raw); err != nil {
			log.Printf("[WS-AGENT] read end: %v", err)
			break
		}
		ac.DispatchRaw(raw)
	}

	close(stopPing)
	ac.Close()
	s.Hub.Unregister(hello.DeviceID, sessionID)
	bg, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.MarkDeviceOnline(bg, s.DB, hello.DeviceID, false, ""); err != nil {
		log.Printf("[WS-AGENT] mark offline: %v", err)
	}
	log.Printf("[WS-AGENT] agent offline: device=%s", hello.DeviceID)
}

func bearer(h string) string {
	const p = "Bearer "
	if strings.HasPrefix(h, p) {
		return strings.TrimSpace(h[len(p):])
	}
	return ""
}
