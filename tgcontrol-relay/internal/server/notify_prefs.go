package server

// Настройки сообщений бота на аккаунт: GET/PUT /v1/me/notify.
//
// Почему ручка нужна: канал «бот пишет в личку» существует только на релее, а
// управлялся ИСКЛЮЧИТЕЛЬНО командой /notify в чате бота. На экране настроек
// клиента тумблеры гасили лишь локальные Capacitor-пуши Android, поэтому человек
// выключал «уведомления», а ночные сообщения про вопрос агента продолжали
// приходить — про сам telegram-канал интерфейс не знал ничего.
//
// Флагов два и они независимы:
//   - agent_waiting (users.tg_notify, миграция 0008) — «AI-агент ждёт ответа»;
//   - support_reply (users.tg_notify_support, миграция 0018) — «поддержка
//     ответила». Погасив ночные вопросы агента, человек не должен потерять ответ
//     на своё же обращение — поэтому один общий выключатель здесь не годится.

import (
	"encoding/json"
	"io"
	"log"
	"net/http"

	"tgcontrol-relay/internal/db"
)

// notifyPrefsJSON — контракт ручки. *_available говорит, дойдёт ли сообщение
// физически: без Telegram у аккаунта (анонимный пейринг по QR) писать некуда, а
// вопросы агента шлёт только релей с включённым NOTIFY_TG. Клиент по этим полям
// решает, показывать тумблер или строку-объяснение, и не гадает по режиму.
type notifyPrefsJSON struct {
	TelegramLinked        bool `json:"telegram_linked"`
	AgentWaiting          bool `json:"agent_waiting"`
	AgentWaitingAvailable bool `json:"agent_waiting_available"`
	SupportReply          bool `json:"support_reply"`
	SupportReplyAvailable bool `json:"support_reply_available"`
}

// handleGetNotifyPrefs — GET /v1/me/notify.
func (s *Server) handleGetNotifyPrefs(w http.ResponseWriter, r *http.Request) {
	claims, err := s.requireUserAuth(r)
	if err != nil {
		writeErrCode(w, http.StatusUnauthorized, "auth_required", "нужна авторизация")
		return
	}
	prefs, err := s.notifyPrefs(r, claims.UserID)
	if err != nil {
		log.Printf("[NOTIFY] prefs user=%d: %v", claims.UserID, err)
		writeErrCode(w, http.StatusInternalServerError, "db_error", "не удалось прочитать настройки")
		return
	}
	writeJSON(w, http.StatusOK, prefs)
}

// handleSetNotifyPrefs — PUT /v1/me/notify {agent_waiting?, support_reply?}.
// Тело частичное: не переданный флаг не трогаем, чтобы два независимых тумблера
// не затирали друг друга при гонке двух клиентов одного аккаунта.
func (s *Server) handleSetNotifyPrefs(w http.ResponseWriter, r *http.Request) {
	claims, err := s.requireUserAuth(r)
	if err != nil {
		writeErrCode(w, http.StatusUnauthorized, "auth_required", "нужна авторизация")
		return
	}
	var req struct {
		AgentWaiting *bool `json:"agent_waiting"`
		SupportReply *bool `json:"support_reply"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<10)).Decode(&req); err != nil {
		writeErrCode(w, http.StatusBadRequest, "bad_json", "неверный формат запроса")
		return
	}
	if req.AgentWaiting == nil && req.SupportReply == nil {
		writeErrCode(w, http.StatusBadRequest, "notify_no_fields",
			"укажите agent_waiting и/или support_reply")
		return
	}
	ctx := r.Context()
	if req.AgentWaiting != nil {
		if err := db.SetNotifyEnabled(ctx, s.DB, claims.UserID, *req.AgentWaiting); err != nil {
			log.Printf("[NOTIFY] set agent_waiting user=%d: %v", claims.UserID, err)
			writeErrCode(w, http.StatusInternalServerError, "db_error", "не удалось сохранить настройку")
			return
		}
	}
	if req.SupportReply != nil {
		if err := db.SetSupportNotifyEnabled(ctx, s.DB, claims.UserID, *req.SupportReply); err != nil {
			log.Printf("[NOTIFY] set support_reply user=%d: %v", claims.UserID, err)
			writeErrCode(w, http.StatusInternalServerError, "db_error", "не удалось сохранить настройку")
			return
		}
	}
	// Отдаём ПОЛНОЕ состояние, а не эхо запроса: клиент рисует экран по ответу и
	// заодно видит, дойдёт ли сообщение вообще (telegram_linked, *_available).
	prefs, err := s.notifyPrefs(r, claims.UserID)
	if err != nil {
		log.Printf("[NOTIFY] prefs after save user=%d: %v", claims.UserID, err)
		writeErrCode(w, http.StatusInternalServerError, "db_error", "не удалось прочитать настройки")
		return
	}
	writeJSON(w, http.StatusOK, prefs)
}

// notifyPrefs собирает текущее состояние обоих флагов.
func (s *Server) notifyPrefs(r *http.Request, userID int64) (notifyPrefsJSON, error) {
	ctx := r.Context()
	var out notifyPrefsJSON
	u, err := db.GetUserByID(ctx, s.DB, userID)
	if err != nil {
		return out, err
	}
	agentOn, err := db.NotifyEnabled(ctx, s.DB, userID)
	if err != nil {
		return out, err
	}
	supportOn, err := db.SupportNotifyEnabled(ctx, s.DB, userID)
	if err != nil {
		return out, err
	}
	linked := u != nil && u.TelegramID > 0 && !db.IsAnonTelegramID(u.TelegramID)
	return notifyPrefsJSON{
		TelegramLinked:        linked,
		AgentWaiting:          agentOn,
		AgentWaitingAvailable: linked && s.Config.NotifyTG && s.UserNotify != nil,
		SupportReply:          supportOn,
		SupportReplyAvailable: linked && s.UserNotifySupport != nil,
	}, nil
}
