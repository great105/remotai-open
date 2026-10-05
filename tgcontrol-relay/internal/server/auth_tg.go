package server

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"tgcontrol-relay/internal/db"
	"tgcontrol-relay/internal/metrics"
)

// loginTokenTTL — короткое окно на завершение входа через Telegram.
const loginTokenTTL = 5 * time.Minute

// POST /v1/auth/tg/start — клиент (APK/web/exe, НЕ Mini App) начинает вход.
// Возвращает одноразовый nonce и deep-link в бота. Клиент открывает ссылку,
// пользователь жмёт Start, бот привязывает nonce к telegram_id (ConfirmLoginToken),
// после чего клиент забирает durable user-JWT через poll.
func (s *Server) handleAuthTgStart(w http.ResponseWriter, r *http.Request) {
	if s.Config.BotUsername == "" {
		writeErr(w, http.StatusServiceUnavailable, "telegram login not configured")
		return
	}
	nonce, err := db.GenerateNonce()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "nonce gen failed")
		return
	}
	if _, err := db.CreateLoginToken(r.Context(), s.DB, nonce, loginTokenTTL); err != nil {
		writeErr(w, http.StatusInternalServerError, "store login token failed")
		return
	}
	deepLink := "https://t.me/" + s.Config.BotUsername + "?start=login_" + nonce
	writeJSON(w, http.StatusOK, map[string]any{
		"login_token":  nonce,
		"deep_link":    deepLink,
		"bot_username": s.Config.BotUsername,
		"expires_at":   time.Now().UTC().Add(loginTokenTTL).Format(time.RFC3339),
	})
}

// GET /v1/auth/tg/poll?login_token=... — клиент опрашивает результат входа.
// Пока бот не подтвердил: {confirmed:false, expired:false}. После подтверждения:
// выдаём durable user-JWT (single-use, токен сжигается). Если клиент прислал свой
// текущий АНОНИМНЫЙ Bearer-JWT — сливаем анонимный аккаунт в Telegram-аккаунт,
// чтобы не потерять уже привязанные ПК.
func (s *Server) handleAuthTgPoll(w http.ResponseWriter, r *http.Request) {
	nonce := r.URL.Query().Get("login_token")
	if nonce == "" {
		writeErr(w, http.StatusBadRequest, "login_token required")
		return
	}
	t, err := db.GetLoginToken(r.Context(), s.DB, nonce)
	if err != nil {
		if errors.Is(err, db.ErrLoginTokenNotFound) {
			writeJSON(w, http.StatusOK, map[string]any{"confirmed": false, "expired": true})
			return
		}
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	// Уже обменян или просрочен → как истёкший (повторно JWT не выдаём).
	if t.ConsumedAt.Valid || time.Now().UTC().After(t.ExpiresAt) {
		writeJSON(w, http.StatusOK, map[string]any{"confirmed": false, "expired": true})
		return
	}
	// Бот ещё не подтвердил.
	if !t.ConfirmedAt.Valid || !t.UserID.Valid {
		writeJSON(w, http.StatusOK, map[string]any{"confirmed": false, "expired": false})
		return
	}
	// Claim the one-time login before minting anything. Concurrent poll requests
	// may both observe confirmed_at, but only one is allowed to issue a JWT.
	if err := db.ConsumeLoginToken(r.Context(), s.DB, nonce); err != nil {
		if errors.Is(err, db.ErrLoginTokenNotFound) {
			writeJSON(w, http.StatusOK, map[string]any{"confirmed": false, "expired": true})
			return
		}
		writeErr(w, http.StatusInternalServerError, "consume login token failed")
		return
	}

	tgUserID := t.UserID.Int64

	// Слияние анонимного аккаунта (если клиент аутентифицирован анон-Bearer-JWT).
	if tok := bearer(r.Header.Get("Authorization")); tok != "" && !strings.HasPrefix(strings.ToLower(tok), "tma ") {
		if c, e := s.JWT.ParseUser(tok); e == nil && c.UserID != tgUserID && db.IsAnonTelegramID(c.TelegramID) {
			if err := s.mergeAccounts(r.Context(), c.UserID, tgUserID); err != nil {
				log.Printf("[AUTH-TG] merge anon %d → tg %d skipped: %v", c.UserID, tgUserID, err)
			} else {
				log.Printf("[AUTH-TG] merged anon %d → tg %d", c.UserID, tgUserID)
			}
		}
	}

	u, err := db.GetUserByID(r.Context(), s.DB, tgUserID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	jwtStr, exp, err := s.mintUserSessionJWT(r, u, "")
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "jwt issue failed")
		return
	}
	metrics.Login()
	log.Printf("[AUTH-TG] login confirmed: user=%d tg=%d", u.ID, u.TelegramID)
	writeJSON(w, http.StatusOK, map[string]any{
		"confirmed":   true,
		"jwt":         jwtStr,
		"expires_at":  exp.Format(time.RFC3339),
		"telegram_id": u.TelegramID,
		"username":    u.Username,
		"first_name":  u.FirstName,
	})
}

// POST /v1/auth/tg/miniapp — Mini App обменивает подписанный initData на durable
// user-JWT, чтобы личность работала и вне Telegram-сессии (и единообразно с
// нативными клиентами). Auth: initData (requireUserAuth, Source=="initdata").
func (s *Server) handleAuthTgMiniApp(w http.ResponseWriter, r *http.Request) {
	claims, err := s.requireUserAuth(r)
	if err != nil {
		writeErr(w, http.StatusUnauthorized, err.Error())
		return
	}
	if claims.Source != "initdata" {
		writeErr(w, http.StatusBadRequest, "Telegram initData required")
		return
	}
	u, err := db.GetUserByID(r.Context(), s.DB, claims.UserID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	jwtStr, exp, err := s.mintUserSessionJWT(r, u, "")
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "jwt issue failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"jwt":         jwtStr,
		"expires_at":  exp.Format(time.RFC3339),
		"telegram_id": u.TelegramID,
	})
}

// POST /v1/auth/qr/approve {login_token} — вход на большом экране подтверждает
// ТЕЛЕФОН, а не Telegram.
//
// Зачем отдельная дверь. QR со страницы входа несёт ссылку на бота, и открыть
// её умеет системная камера. Но человек с приложением в руках наводит на код
// НАШ сканер — и это правильный жест: он уже вошёл здесь, аккаунт тот самый.
// Гонять его через Telegram ради подтверждения того, что мы и так знаем, —
// лишний круг (живой случай 23.08: сканер ответил «Это не код Remotai» на код,
// который Remotai сам и показал).
//
// Подтверждаем тем же путём, что и бот (ConfirmLoginToken): компьютер заберёт
// durable-JWT своим обычным опросом /v1/auth/tg/poll и даже не заметит разницы.
// Требуется настоящий аккаунт: анонимной сессией чужой вход не подтвердить.
func (s *Server) handleAuthQRApprove(w http.ResponseWriter, r *http.Request) {
	claims, err := s.requireUserAuth(r)
	if err != nil {
		writeErr(w, http.StatusUnauthorized, "authentication required")
		return
	}
	var body struct {
		LoginToken string `json:"login_token"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<12)).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	nonce := strings.TrimSpace(body.LoginToken)
	if nonce == "" {
		writeErr(w, http.StatusBadRequest, "login_token required")
		return
	}
	if err := db.ConfirmLoginToken(r.Context(), s.DB, nonce, claims.UserID); err != nil {
		switch {
		case errors.Is(err, db.ErrLoginTokenExpired):
			writeErrCode(w, http.StatusGone, "login_expired",
				"Код входа устарел. Обновите страницу на компьютере и покажите новый QR.")
		case errors.Is(err, db.ErrLoginTokenNotFound):
			writeErrCode(w, http.StatusNotFound, "login_not_found",
				"Такого кода входа нет. Покажите свежий QR на компьютере.")
		default:
			writeErr(w, http.StatusInternalServerError, err.Error())
		}
		return
	}
	log.Printf("[AUTH] qr login approved by user %d", claims.UserID)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
