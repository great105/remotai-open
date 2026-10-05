package server

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"

	"tgcontrol-relay/internal/db"
)

// Привязка дополнительных способов входа к СУЩЕСТВУЮЩЕМУ аккаунту (пользователь
// уже авторизован). Это страховка от блокировки/потери любого провайдера:
// у аккаунта всегда должен быть второй путь входа.

// identityJSON — публичное представление способа входа.
type identityJSON struct {
	Provider string `json:"provider"`
	UID      string `json:"uid"`     // email или id у провайдера
	Display  string `json:"display"` // подпись для UI
}

// GET /v1/me/identities — способы входа текущего аккаунта. Telegram (если
// привязан) показывается первым: он живёт в users.telegram_id, а не в
// user_identities (миграция данных — следующий этап).
func (s *Server) handleListMyIdentities(w http.ResponseWriter, r *http.Request) {
	claims, err := s.requireUserAuth(r)
	if err != nil {
		writeErr(w, http.StatusUnauthorized, err.Error())
		return
	}
	u, err := db.GetUserByID(r.Context(), s.DB, claims.UserID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]identityJSON, 0, 4)
	if !db.IsAnonTelegramID(u.TelegramID) {
		out = append(out, identityJSON{Provider: "telegram", UID: itoa(u.TelegramID), Display: firstNonEmpty(u.Username, u.FirstName)})
	}
	ids, err := db.ListIdentities(r.Context(), s.DB, claims.UserID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	for _, it := range ids {
		out = append(out, identityJSON{Provider: it.Provider, UID: it.ProviderUID, Display: it.Display})
	}
	writeJSON(w, http.StatusOK, map[string]any{"identities": out})
}

// POST /v1/me/identities/link — привязать метод к текущему аккаунту.
//
//	{ "provider": "email", "login_token": "...", "code": "123456" }
//	{ "provider": "vk"|"yandex"|"google"|"apple", "access_token": "..." }
//
// Identity, принадлежащую ДРУГОМУ аккаунту, не привязываем (409): слияние двух
// постоянных аккаунтов это отдельная история с подтверждением владения обоими.
func (s *Server) handleLinkIdentity(w http.ResponseWriter, r *http.Request) {
	claims, err := s.requireUserAuth(r)
	if err != nil {
		writeErr(w, http.StatusUnauthorized, err.Error())
		return
	}
	var req struct {
		Provider    string `json:"provider"`
		LoginToken  string `json:"login_token"`
		Code        string `json:"code"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json")
		return
	}

	var uid, display string
	switch req.Provider {
	case "email":
		email, err := db.VerifyEmailLoginCode(r.Context(), s.DB, req.LoginToken, strings.TrimSpace(req.Code), s.Config.EmailMaxAttempts)
		if err != nil {
			writeErr(w, http.StatusUnauthorized, "invalid or expired code")
			return
		}
		uid, display = email, email
	case "vk", "yandex", "google", "apple":
		if !s.oauthAllowed(req.Provider) {
			writeErr(w, http.StatusNotFound, "provider not available")
			return
		}
		p, err := s.verifyOAuthToken(r, req.Provider, req.AccessToken)
		if err != nil {
			writeErr(w, http.StatusUnauthorized, "token verification failed")
			return
		}
		uid, display = p.UID, p.Display
	default:
		writeErr(w, http.StatusBadRequest, "unsupported provider")
		return
	}

	// Уже принадлежит другому аккаунту? Отдаём МАШИННЫЙ код: безкодовый 409
	// клиент раньше трактовал как конфликт пейринга и советовал «Отключить
	// облако» на ПК — совет, который отвязывает рабочую машину, хотя речь про
	// способ входа (находка N131).
	if owner, err := db.GetUserByIdentity(r.Context(), s.DB, req.Provider, uid); err == nil && owner.ID != claims.UserID {
		writeErrCode(w, http.StatusConflict, "identity_taken",
			"Этот способ входа уже привязан к другому аккаунту.")
		return
	}
	if err := db.LinkIdentity(r.Context(), s.DB, claims.UserID, req.Provider, uid, display); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	log.Printf("[AUTH-LINK] user=%d linked %s", claims.UserID, req.Provider)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "provider": req.Provider, "uid": uid})
}

// verifyOAuthToken — общий свитч верификации access/id token у провайдера.
func (s *Server) verifyOAuthToken(r *http.Request, provider, token string) (*oauthProfile, error) {
	switch provider {
	case "vk":
		return verifyVK(r, token, s.Config.VKClientID)
	case "yandex":
		return verifyYandex(r, token)
	case "google":
		return verifyGoogle(r, token)
	case "apple":
		return verifyAppleIDToken(r, token, s.Config.AppleClientID)
	}
	return nil, fmt.Errorf("unsupported provider %q", provider)
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func itoa(v int64) string { return strconv.FormatInt(v, 10) }
