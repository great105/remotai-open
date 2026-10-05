package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"tgcontrol-relay/internal/db"
)

// supportMsgJSON — сообщение поддержки в формате контракта с клиентом.
type supportMsgJSON struct {
	ID        int64  `json:"id"`
	Sender    string `json:"sender"` // user | admin
	Text      string `json:"text"`
	CreatedAt string `json:"created_at"` // RFC3339
}

func supportMsgsToJSON(msgs []db.SupportMessage) []supportMsgJSON {
	out := make([]supportMsgJSON, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, supportMsgJSON{
			ID:        m.ID,
			Sender:    m.Sender,
			Text:      m.Text,
			CreatedAt: m.CreatedAt.UTC().Format(time.RFC3339),
		})
	}
	return out
}

// handleSupportList — GET /v1/support/messages. Возвращает переписку юзера
// и ОБНУЛЯЕТ его непрочитанные (клиент рассчитывает на это для бейджа).
func (s *Server) handleSupportList(w http.ResponseWriter, r *http.Request) {
	claims, err := s.requireUserAuth(r)
	if err != nil {
		writeErr(w, http.StatusUnauthorized, "authentication required")
		return
	}
	ctx := r.Context()
	thread, err := db.GetSupportThreadByUser(ctx, s.DB, claims.UserID)
	if errors.Is(err, db.ErrSupportThreadNotFound) {
		writeJSON(w, http.StatusOK, map[string]any{"messages": []supportMsgJSON{}})
		return
	}
	if err != nil {
		log.Printf("[SUPPORT] get thread: %v", err)
		writeErr(w, http.StatusInternalServerError, "db error")
		return
	}
	// Прочитано юзером — бейдж сбрасываем до ответа, чтобы даже при гонке
	// со следующим polling не висел лишний непрочитанный.
	if err := db.MarkSupportReadUser(ctx, s.DB, thread.ID); err != nil {
		log.Printf("[SUPPORT] mark read user: %v", err)
	}
	msgs, err := db.ListSupportMessages(ctx, s.DB, thread.ID)
	if err != nil {
		log.Printf("[SUPPORT] list messages: %v", err)
		writeErr(w, http.StatusInternalServerError, "db error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"messages": supportMsgsToJSON(msgs)})
}

// handleSupportPost — POST /v1/support/messages {text} → {ok, id}.
// Тред создаётся при первом сообщении. Админу улетает уведомление в Telegram.
func (s *Server) handleSupportPost(w http.ResponseWriter, r *http.Request) {
	claims, err := s.requireUserAuth(r)
	if err != nil {
		writeErr(w, http.StatusUnauthorized, "authentication required")
		return
	}
	var req struct {
		Text string         `json:"text"`
		Meta map[string]any `json:"meta"`
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

	metaJSON := ""
	if len(req.Meta) > 0 {
		if raw, marshalErr := json.Marshal(req.Meta); marshalErr == nil && len(raw) <= 4096 {
			metaJSON = string(raw)
		}
	}
	_, msgID, err := db.PostUserSupportMessage(r.Context(), s.DB, claims.UserID, req.Text)
	if err != nil {
		log.Printf("[SUPPORT] post user message: %v", err)
		writeErr(w, http.StatusInternalServerError, "db error")
		return
	}
	if err := db.SetSupportThreadMetaIfEmpty(r.Context(), s.DB, claims.UserID, metaJSON); err != nil {
		log.Printf("[SUPPORT] save context: %v", err)
	}
	s.notifyAdminSupport(claims.UserID, req.Text, metaJSON)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": msgID})
}

// handleSupportUnread — GET /v1/support/unread → {count}. Бейдж непрочитанных.
func (s *Server) handleSupportUnread(w http.ResponseWriter, r *http.Request) {
	claims, err := s.requireUserAuth(r)
	if err != nil {
		writeErr(w, http.StatusUnauthorized, "authentication required")
		return
	}
	n, err := db.SupportUnreadForUser(r.Context(), s.DB, claims.UserID)
	if err != nil {
		log.Printf("[SUPPORT] unread: %v", err)
		writeErr(w, http.StatusInternalServerError, "db error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"count": n})
}

// notifyAdminSupport — уведомление админу в Telegram о новом сообщении.
// Асинхронно и fire-and-forget: поддержка не должна падать из-за TG API.
func (s *Server) notifyAdminSupport(userID int64, text, meta string) {
	if s.AdminNotify == nil {
		return
	}
	name := ""
	if u, err := db.GetUserByID(context.Background(), s.DB, userID); err == nil && u != nil {
		switch {
		case u.Username != "":
			name = "@" + u.Username
		case u.FirstName != "":
			name = u.FirstName
		}
	}
	if name == "" {
		name = "id " + strconv.FormatInt(userID, 10)
	}
	short := []rune(text)
	if len(short) > 200 {
		short = short[:200]
	}
	msg := "🆘 Поддержка\n" + name + ": " + string(short) + "\nОтветить: https://remotai.ru/admin"
	if meta != "" {
		msg += "\nКонтекст: " + meta
	}
	go s.AdminNotify(msg)
}

// supportReplyNotifyWindow — не чаще одного сообщения бота на тред за это время.
// Админ отвечает сериями («сейчас посмотрю» → «включите вот это» → «проверьте»),
// и человеку нужен один пинг, а не три. Отметку снимает чтение переписки
// (db.MarkSupportReadUser), поэтому «ответил через час после прочтения» доходит
// сразу.
const supportReplyNotifyWindow = 5 * time.Minute

// supportReplyPreviewRunes — сколько символов ответа кладём в сообщение бота.
// Полный текст человек прочитает в чате поддержки по кнопке.
const supportReplyPreviewRunes = 300

// notifySupportReply — «поддержка ответила» доходит до человека в Telegram.
//
// Почему это здесь: ответ админа поднимал только счётчик непрочитанных, а его
// видит лишь запущенное приложение (в Telegram-мини-аппе и вебе локальных
// уведомлений нет вовсе) — человек считал, что поддержка молчит.
//
// Асинхронно и fire-and-forget: доступность Telegram API не должна влиять на
// ответ админке. Пишем только постоянным Telegram-аккаунтам: у анонимного
// (пейринг по QR) личного чата не существует.
func (s *Server) notifySupportReply(threadID int64, text string) {
	if s.UserNotifySupport == nil {
		return
	}
	preview := []rune(strings.TrimSpace(text))
	if len(preview) > supportReplyPreviewRunes {
		preview = append(preview[:supportReplyPreviewRunes:supportReplyPreviewRunes], '…')
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()

		thread, err := db.GetSupportThread(ctx, s.DB, threadID)
		if err != nil {
			log.Printf("[SUPPORT] notify reply: тред %d: %v", threadID, err)
			return
		}
		u, err := db.GetUserByID(ctx, s.DB, thread.UserID)
		if err != nil || u == nil {
			log.Printf("[SUPPORT] notify reply: пользователь %d: %v", thread.UserID, err)
			return
		}
		if u.TelegramID <= 0 || db.IsAnonTelegramID(u.TelegramID) {
			return // писать некуда: аккаунт без Telegram
		}
		if on, err := db.SupportNotifyEnabled(ctx, s.DB, u.ID); err != nil || !on {
			return
		}
		// Заявку берём ПЕРЕД отправкой: она же и дедупликация. Отправка занимает
		// сотни миллисекунд, и второй ответ админа, пришедший в этот момент, иначе
		// уехал бы вторым сообщением. При ошибке Telegram отметку не снимаем:
		// биться в недоступный API на каждом ответе хуже, чем пропустить пинг —
		// бейдж непрочитанного в приложении остаётся.
		claimed, err := db.ClaimSupportReplyNotify(ctx, s.DB, threadID, supportReplyNotifyWindow)
		if err != nil {
			log.Printf("[SUPPORT] notify reply: отметка треда %d: %v", threadID, err)
			return
		}
		if !claimed {
			return
		}
		if err := s.UserNotifySupport(ctx, u.TelegramID, string(preview)); err != nil {
			log.Printf("[SUPPORT] notify reply user=%d: %v", u.ID, err)
			return
		}
		log.Printf("[SUPPORT] reply notice sent: user=%d thread=%d", u.ID, threadID)
	}()
}
