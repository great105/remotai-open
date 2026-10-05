package server

import (
	"crypto/subtle"
	_ "embed"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"

	"tgcontrol-relay/internal/auth"
	"tgcontrol-relay/internal/db"
)

//go:embed admin_static/admin.html
var adminHTML string

const adminLoginMaxAge = 24 * time.Hour

// requireAdmin — middleware админ-кабинета. Способы пройти:
//   - Authorization: Bearer <AdminToken> — bypass для локали/тестов (если
//     ADMIN_TOKEN задан в конфиге);
//   - Authorization: tma <Telegram Mini App initData> — основной production
//     путь: /start admin в боте открывает /admin как Web App;
//   - Authorization: tma <данные Telegram Login Widget> — запасной путь после
//     настройки домена виджета через BotFather.
//
// В обоих Telegram-вариантах подпись проверяется по BOT_TOKEN, а telegram_id
// должен быть в ADMIN_IDS.
//
// Всё остальное → 403.
func (s *Server) requireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.isAdmin(r) {
			writeErr(w, http.StatusForbidden, "admin access denied")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) isAdmin(r *http.Request) bool {
	h := r.Header.Get("Authorization")
	if s.Config.AdminToken != "" {
		if tok := bearer(h); tok != "" &&
			subtle.ConstantTimeCompare([]byte(tok), []byte(s.Config.AdminToken)) == 1 {
			return true
		}
	}
	if strings.HasPrefix(strings.ToLower(h), "tma ") {
		raw := strings.TrimSpace(h[4:])
		// Сначала основной production-путь — initData Telegram Web App.
		if data, err := auth.ParseInitData(raw, s.Config.BotToken, adminLoginMaxAge); err == nil &&
			data.User != nil && s.isAdminTelegramID(data.User.ID) {
			return true
		}
		// Запасной browser Login Widget. Подписанные данные хранятся в браузере,
		// поэтому replay-окно тоже ограничено сутками.
		if u, err := auth.ParseLoginWidgetData(raw, s.Config.BotToken, adminLoginMaxAge); err == nil {
			return s.isAdminTelegramID(u.ID)
		}
	}
	return false
}

func (s *Server) isAdminTelegramID(userID int64) bool {
	for _, id := range s.Config.AdminIDs {
		if userID == id {
			return true
		}
	}
	return false
}

// handleAdminPage — GET /admin: статическая страница админки (embed).
// Имя бота подставляем в deep-link входа — отдельный endpoint не нужен.
func (s *Server) handleAdminPage(w http.ResponseWriter, r *http.Request) {
	html := strings.Replace(adminHTML, "__BOT_USERNAME__", s.Config.BotUsername, 1)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = io.WriteString(w, html)
}

// handleAdminStats — GET /v1/admin/stats: сводка воронки/активности/источников.
func (s *Server) handleAdminStats(w http.ResponseWriter, r *http.Request) {
	st, err := db.GatherAdminStats(r.Context(), s.DB)
	if err != nil {
		log.Printf("[ADMIN] stats: %v", err)
		writeErr(w, http.StatusInternalServerError, "db error")
		return
	}
	writeJSON(w, http.StatusOK, st)
}

// adminThreadJSON — тред в инбоксе админки.
type adminThreadJSON struct {
	ID          int64           `json:"id"`
	Username    string          `json:"username"`
	FirstName   string          `json:"first_name"`
	Status      string          `json:"status"`
	LastMsgAt   *string         `json:"last_msg_at"`
	UnreadAdmin int             `json:"unread_admin"`
	LastText    string          `json:"last_text"`
	Meta        json.RawMessage `json:"meta,omitempty"`
}

// handleAdminThreads — GET /v1/admin/support/threads: инбокс поддержки.
func (s *Server) handleAdminThreads(w http.ResponseWriter, r *http.Request) {
	threads, err := db.ListSupportThreads(r.Context(), s.DB)
	if err != nil {
		log.Printf("[ADMIN] list threads: %v", err)
		writeErr(w, http.StatusInternalServerError, "db error")
		return
	}
	out := make([]adminThreadJSON, 0, len(threads))
	for _, t := range threads {
		j := adminThreadJSON{
			ID:          t.ID,
			Username:    t.Username,
			FirstName:   t.FirstName,
			Status:      t.Status,
			UnreadAdmin: t.UnreadAdmin,
			LastText:    t.LastText,
		}
		if json.Valid([]byte(t.Meta)) {
			j.Meta = json.RawMessage(t.Meta)
		}
		if t.LastMsgAt.Valid {
			v := t.LastMsgAt.Time.UTC().Format(time.RFC3339)
			j.LastMsgAt = &v
		}
		out = append(out, j)
	}
	writeJSON(w, http.StatusOK, map[string]any{"threads": out})
}

// handleAdminThreadMessages — GET /v1/admin/support/threads/{id}/messages:
// переписка + сброс непрочитанных админом.
func (s *Server) handleAdminThreadMessages(w http.ResponseWriter, r *http.Request) {
	threadID, err := adminThreadID(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad thread id")
		return
	}
	ctx := r.Context()
	if _, err := db.GetSupportThread(ctx, s.DB, threadID); errors.Is(err, db.ErrSupportThreadNotFound) {
		writeErr(w, http.StatusNotFound, "thread not found")
		return
	} else if err != nil {
		log.Printf("[ADMIN] get thread: %v", err)
		writeErr(w, http.StatusInternalServerError, "db error")
		return
	}
	if err := db.MarkSupportReadAdmin(ctx, s.DB, threadID); err != nil {
		log.Printf("[ADMIN] mark read admin: %v", err)
	}
	msgs, err := db.ListSupportMessages(ctx, s.DB, threadID)
	if err != nil {
		log.Printf("[ADMIN] list messages: %v", err)
		writeErr(w, http.StatusInternalServerError, "db error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"messages": supportMsgsToJSON(msgs)})
}

// handleAdminPostMessage — POST /v1/admin/support/threads/{id}/messages {text}:
// ответ админа пользователю (бампит его бейдж непрочитанных).
func (s *Server) handleAdminPostMessage(w http.ResponseWriter, r *http.Request) {
	threadID, err := adminThreadID(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad thread id")
		return
	}
	var req struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	req.Text = strings.TrimSpace(req.Text)
	if req.Text == "" {
		writeErr(w, http.StatusBadRequest, "text is empty")
		return
	}
	if utf8.RuneCountInString(req.Text) > 4000 {
		writeErr(w, http.StatusBadRequest, "text too long (max 4000 chars)")
		return
	}
	msgID, err := db.PostAdminSupportMessage(r.Context(), s.DB, threadID, req.Text)
	if errors.Is(err, db.ErrSupportThreadNotFound) {
		writeErr(w, http.StatusNotFound, "thread not found")
		return
	}
	if err != nil {
		log.Printf("[ADMIN] post message: %v", err)
		writeErr(w, http.StatusInternalServerError, "db error")
		return
	}
	// Ответ обязан догнать человека: бейдж непрочитанного видит только запущенное
	// приложение, поэтому владельцу треда пишет бот (см. notifySupportReply).
	s.notifySupportReply(threadID, req.Text)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": msgID})
}

// handleAdminToggleThread — POST /v1/admin/support/threads/{id}/close:
// переключает open ↔ closed (название историческое, это toggle).
func (s *Server) handleAdminToggleThread(w http.ResponseWriter, r *http.Request) {
	threadID, err := adminThreadID(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad thread id")
		return
	}
	status, err := db.ToggleSupportThread(r.Context(), s.DB, threadID)
	if errors.Is(err, db.ErrSupportThreadNotFound) {
		writeErr(w, http.StatusNotFound, "thread not found")
		return
	}
	if err != nil {
		log.Printf("[ADMIN] toggle thread: %v", err)
		writeErr(w, http.StatusInternalServerError, "db error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "status": status})
}

func adminThreadID(r *http.Request) (int64, error) {
	return strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
}
