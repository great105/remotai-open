package server

// «remotai send»: агент (демон на ПК) шлёт владельцу текст и/или файл в
// Telegram через релей (#agent-send). Auth — device JWT, как у
// handleDeviceRevokeSelf; доставка текстом — новый hook UserSendText,
// файлом — тот же spool-путь, что /v1/files/send (sendAgentFileToTelegram).
//
// Всё односторонне, без клавиатур: это вывод скриптов и джобов на ПК, а не
// диалог. Спам ограничен per-device лимитом в памяти (ниже) и общим
// выключателем /notify (tg_notify): если человек попросил молчать, CLI
// получает ok:false muted:true — это не ошибка, а осознанное «не доставлено».

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

	"tgcontrol-relay/internal/db"
)

// agentSendPerHour — потолок сообщений /v1/agent/send на устройство. Текст и
// файл идут в один бакет (текст+файл в одном запросе = 2 сообщения). Пакетная
// переменная, а не конфиг: тесты подменяют на маленькое значение.
var agentSendPerHour = 30

// agentSendLimiter — скользящее окно на устройство, только память (без БД:
// счётчики переживать рестарт не обязаны, а лишняя запись в SQLite на каждый
// «remotai send» не нужна). Карта растёт по числу устройств — ограничена их
// количеством, отдельная очистка не нужна.
type agentSendLimiter struct {
	mu  sync.Mutex
	per map[string]agentSendWindow
}

type agentSendWindow struct {
	start time.Time
	count int
}

// allow резервирует n сообщений. Резерв берётся ДО отправки: упавшая доставка
// токены не возвращает — иначе скрипт с битым chat_id жёг бы TG API без
// ограничений.
func (l *agentSendLimiter) allow(deviceID string, n int) bool {
	if agentSendPerHour <= 0 {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.per == nil {
		l.per = make(map[string]agentSendWindow)
	}
	now := time.Now()
	w := l.per[deviceID]
	if now.Sub(w.start) >= time.Hour {
		w = agentSendWindow{start: now}
	}
	if w.count+n > agentSendPerHour {
		l.per[deviceID] = w // сброшенное окно запоминаем и при отказе
		return false
	}
	w.count += n
	l.per[deviceID] = w
	return true
}

// handleAgentSend — POST /v1/agent/send. Тело (≤64 КБ):
// {"text": "...", "file": {"path": "...", "name": "..."}} — обязательно хотя
// бы одно из двух; текст+файл уходят двумя сообщениями, текст первым.
func (s *Server) handleAgentSend(w http.ResponseWriter, r *http.Request) {
	tok := bearer(r.Header.Get("Authorization"))
	if tok == "" {
		writeErr(w, http.StatusUnauthorized, "device token required")
		return
	}
	claims, err := s.JWT.ParseDevice(tok)
	if err != nil {
		writeErr(w, http.StatusUnauthorized, "invalid device token: "+err.Error())
		return
	}
	dev, err := db.GetDeviceForAgentAuth(r.Context(), s.DB, claims.DeviceID, claims.UserID)
	if err != nil || dev.RevokedAt.Valid {
		writeErr(w, http.StatusForbidden, "device token no longer owns this PC")
		return
	}

	var req struct {
		Text string `json:"text"`
		File *struct {
			Path string `json:"path"`
			Name string `json:"name"`
		} `json:"file"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		writeErrCode(w, http.StatusBadRequest, "bad_request", "invalid JSON")
		return
	}
	req.Text = strings.TrimSpace(req.Text)
	hasText := req.Text != ""
	hasFile := req.File != nil
	if hasFile && strings.TrimSpace(req.File.Path) == "" {
		writeErrCode(w, http.StatusBadRequest, "bad_request", "file.path required")
		return
	}
	if !hasText && !hasFile {
		writeErrCode(w, http.StatusBadRequest, "bad_request", "text or file required")
		return
	}
	if hasText && s.UserSendText == nil || hasFile && s.UserSendFile == nil {
		writeErrCode(w, http.StatusServiceUnavailable, "telegram_unavailable", "telegram bot unavailable")
		return
	}

	// Кому писать. Анонимный аккаунт (пейринг по QR с телефона) не имеет
	// chat_id — доставлять некуда в принципе.
	u, err := db.GetUserByID(r.Context(), s.DB, dev.UserID)
	if err != nil {
		writeErrCode(w, http.StatusInternalServerError, "user_lookup_failed", err.Error())
		return
	}
	if u.TelegramID <= 0 {
		writeErrCode(w, http.StatusConflict, "telegram_not_linked", "link Telegram to receive messages")
		return
	}
	// Человек выключил уведомления (/notify) — это не сбой: CLI покажет
	// «уведомления выключены» и завершится успехом.
	on, err := db.NotifyEnabled(r.Context(), s.DB, u.ID)
	if err != nil {
		writeErrCode(w, http.StatusInternalServerError, "user_lookup_failed", err.Error())
		return
	}
	if !on {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "muted": true})
		return
	}
	if !s.cloudGate(w, r, dev.UserID) {
		return
	}

	need := 0
	if hasText {
		need++
	}
	if hasFile {
		need++
	}
	if !s.agentSends.allow(dev.ID, need) {
		writeErrCode(w, http.StatusTooManyRequests, "rate_limited", "message limit exceeded, try later")
		return
	}

	sent := make([]string, 0, 2)
	if hasText {
		if err := s.UserSendText(r.Context(), u.TelegramID, dev.Name, req.Text); err != nil {
			writeErrCode(w, http.StatusBadGateway, "telegram_send_failed", err.Error())
			return
		}
		sent = append(sent, "text")
	}
	if hasFile {
		agent := s.Hub.Get(dev.ID)
		if agent == nil {
			writeErrCode(w, http.StatusBadGateway, "pc_offline", "computer is offline")
			return
		}
		if _, _, ok := s.sendAgentFileToTelegram(w, r, agent, req.File.Path, req.File.Name, u.TelegramID); !ok {
			return // ответ с кодом ошибки уже записан хелпером
		}
		sent = append(sent, "file")
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "sent": sent})
}
