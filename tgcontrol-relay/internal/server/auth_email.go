package server

import (
	"encoding/json"
	"log"
	"net/http"
	"net/mail"
	"strings"
	"time"

	"tgcontrol-relay/internal/db"
	"tgcontrol-relay/internal/metrics"
)

// ── Способы входа ────────────────────────────────────────────────────

// GET /v1/auth/providers — какие методы входа включены на этом инстансе.
// Решает регион (ru/global) + настроенные креды. Клиент строит экран входа
// по этому списку, свой список не хардкодит.
func (s *Server) handleAuthProviders(w http.ResponseWriter, r *http.Request) {
	region := s.Config.Region
	if region == "" {
		region = "ru"
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"region":    region,
		"providers": s.Config.AuthProviders(),
	})
}

// ── Вход по email-коду ───────────────────────────────────────────────

// emailStartRateWindow / emailStartRateMax — поверх IP-лимита: не даём
// спамить одну и ту же почту с ротацией IP.
const (
	emailStartRateWindow = time.Hour
	emailStartRateMax    = 5
)

// POST /v1/auth/email/start {email} — генерирует 6-значный код и шлёт письмо.
// Аккаунт на этом этапе НЕ создаётся: только после верного кода (verify),
// чтобы не плодить пустых пользователей на чужие адреса.
func (s *Server) handleAuthEmailStart(w http.ResponseWriter, r *http.Request) {
	if s.Mailer == nil || s.Config.SMTPHost == "" {
		writeErr(w, http.StatusServiceUnavailable, "email login not configured")
		return
	}
	var req struct {
		Email string `json:"email"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json")
		return
	}
	addr, err := mail.ParseAddress(strings.TrimSpace(req.Email))
	if err != nil || addr.Address == "" || !strings.Contains(addr.Address, "@") {
		writeErr(w, http.StatusBadRequest, "invalid email")
		return
	}
	email := db.NormalizeUID(addr.Address)

	if n, err := db.CountRecentEmailCodes(r.Context(), s.DB, email, emailStartRateWindow); err == nil && n >= emailStartRateMax {
		writeErr(w, http.StatusTooManyRequests, "too many codes, try later")
		return
	}

	code, err := db.RandDigits(6)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "code gen failed")
		return
	}
	token, err := db.GenerateNonce()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "nonce gen failed")
		return
	}
	if err := db.CreateEmailLoginCode(r.Context(), s.DB, token, email, code, s.Config.EmailCodeTTL); err != nil {
		writeErr(w, http.StatusInternalServerError, "store code failed")
		return
	}

	subject := "Remotai: код входа " + code
	body := "Ваш код входа в Remotai:\n\n" + code + "\n\n" +
		"Он действует 10 минут. Если вы не запрашивали код, просто проигнорируйте письмо."
	if err := s.Mailer.Send(email, subject, body); err != nil {
		log.Printf("[AUTH-EMAIL] send to %s failed: %v", email, err)
		writeErr(w, http.StatusBadGateway, "email send failed")
		return
	}
	log.Printf("[AUTH-EMAIL] code sent to %s", email)
	writeJSON(w, http.StatusOK, map[string]any{
		"login_token": token,
		"expires_at":  time.Now().UTC().Add(s.Config.EmailCodeTTL).Format(time.RFC3339),
	})
}

// POST /v1/auth/email/verify {login_token, code} — код верен → выдаём durable
// user-JWT. Аккаунт ищется по identity 'email'; если это первый вход,
// создаём пользователя и прикрепляем identity. Bearer анонимного клиента
// (если прислали) сливается, устройства не теряются.
func (s *Server) handleAuthEmailVerify(w http.ResponseWriter, r *http.Request) {
	var req struct {
		LoginToken string `json:"login_token"`
		Code       string `json:"code"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json")
		return
	}
	req.Code = strings.TrimSpace(req.Code)
	if req.LoginToken == "" || req.Code == "" {
		writeErr(w, http.StatusBadRequest, "login_token and code required")
		return
	}
	email, err := db.VerifyEmailLoginCode(r.Context(), s.DB, req.LoginToken, req.Code, s.Config.EmailMaxAttempts)
	if err != nil {
		writeErr(w, http.StatusUnauthorized, "invalid or expired code")
		return
	}
	u, err := s.userForIdentity(r, "email", email, email)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	metrics.Login()
	log.Printf("[AUTH-EMAIL] login: user=%d email=%s", u.ID, email)
	s.issueUserJWT(w, r, u)
}

// ── Общее для всех провайдеров ───────────────────────────────────────

// userForIdentity находит аккаунт по (provider, uid) или создаёт новый и
// прикрепляет identity. Новый аккаунт рождается анонимным (synthetic
// telegram_id): позже он может слиться в Telegram-аккаунт или принять в себя
// анонима через merge.
func (s *Server) userForIdentity(r *http.Request, provider, uid, display string) (*db.User, error) {
	u, err := db.GetUserByIdentity(r.Context(), s.DB, provider, uid)
	if err == nil {
		return u, nil
	}
	if err != db.ErrIdentityNotFound {
		return nil, err
	}
	u, err = db.CreateAnonUser(r.Context(), s.DB)
	if err != nil {
		return nil, err
	}
	if err := db.LinkIdentity(r.Context(), s.DB, u.ID, provider, uid, display); err != nil {
		return nil, err
	}
	return u, nil
}

// issueUserJWT сливает анонимный Bearer (если прислали) и отвечает user-JWT.
// Общий финал для email/OAuth-логинов; Telegram идёт своим путём (bot-confirm).
func (s *Server) issueUserJWT(w http.ResponseWriter, r *http.Request, u *db.User) {
	if tok := bearer(r.Header.Get("Authorization")); tok != "" && !strings.HasPrefix(strings.ToLower(tok), "tma ") {
		if c, e := s.JWT.ParseUser(tok); e == nil && c.UserID != u.ID && db.IsAnonTelegramID(c.TelegramID) {
			if err := s.mergeAccounts(r.Context(), c.UserID, u.ID); err != nil {
				log.Printf("[AUTH] merge anon %d → %d skipped: %v", c.UserID, u.ID, err)
			} else {
				log.Printf("[AUTH] merged anon %d → %d", c.UserID, u.ID)
			}
		}
	}
	jwtStr, exp, err := s.mintUserSessionJWT(r, u, "")
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "jwt issue failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"confirmed":  true,
		"jwt":        jwtStr,
		"expires_at": exp.Format(time.RFC3339),
		"user_id":    u.ID,
		"first_name": u.FirstName,
	})
}
