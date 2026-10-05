package server

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"tgcontrol-relay/internal/metrics"
)

// Endpoint-ы верификации access_token у OAuth-провайдеров. Вынесены в
// переменные, чтобы тесты подменяли их на httptest-сервер.
var (
	vkUserInfoURL     = "https://id.vk.ru/oauth2/user_info"
	yandexUserInfoURL = "https://login.yandex.ru/info?format=json"
	googleUserInfoURL = "https://openidconnect.googleapis.com/v1/userinfo"
)

var oauthHTTP = &http.Client{Timeout: 10 * time.Second}

// oauthProfile — нормализованный профиль от любого провайдера.
type oauthProfile struct {
	UID     string // стабильный id у провайдера
	Display string // имя или email для UI
}

// POST /v1/auth/oauth/{provider} {access_token} — клиент прошёл OAuth-флоу у
// провайдера (браузер/SDK) и прислал access_token; сервер верифицирует его
// через userinfo-эндпоинт и выдаёт свой durable user-JWT. Провайдер должен
// быть включён регионом и кредами (AuthProviders).
func (s *Server) handleAuthOAuth(w http.ResponseWriter, r *http.Request) {
	provider := chi.URLParam(r, "provider")
	if !s.oauthAllowed(provider) {
		writeErr(w, http.StatusNotFound, "provider not available")
		return
	}
	var req struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.AccessToken) == "" {
		writeErr(w, http.StatusBadRequest, "access_token required")
		return
	}

	var (
		p   *oauthProfile
		err error
	)
	switch provider {
	case "vk":
		p, err = verifyVK(r, req.AccessToken, s.Config.VKClientID)
	case "yandex":
		p, err = verifyYandex(r, req.AccessToken)
	case "google":
		p, err = verifyGoogle(r, req.AccessToken)
	case "apple":
		// У Apple нет userinfo по access_token: профиль в id_token, клиент
		// (нативный SDK) присылает его в том же поле.
		p, err = verifyAppleIDToken(r, req.AccessToken, s.Config.AppleClientID)
	default:
		err = fmt.Errorf("unsupported provider %q", provider)
	}
	if err != nil {
		log.Printf("[AUTH-OAUTH] %s verify failed: %v", provider, err)
		writeErr(w, http.StatusUnauthorized, "token verification failed")
		return
	}

	u, err := s.userForIdentity(r, provider, p.UID, p.Display)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	metrics.Login()
	log.Printf("[AUTH-OAUTH] login: user=%d provider=%s", u.ID, provider)
	s.issueUserJWT(w, r, u)
}

// oauthAllowed сверяет провайдера со списком включённых (регион + креды).
func (s *Server) oauthAllowed(provider string) bool {
	for _, p := range s.Config.AuthProviders() {
		if p.ID == provider && p.Kind == "oauth" {
			return true
		}
	}
	return false
}

// ── VK ID ────────────────────────────────────────────────────────────
// POST https://id.vk.ru/oauth2/user_info (form: access_token, client_id)
// → {"user":{"user_id":"123","first_name":"...","last_name":"...","email":"..."}}
func verifyVK(r *http.Request, token, clientID string) (*oauthProfile, error) {
	form := url.Values{"access_token": {token}}
	if clientID != "" {
		form.Set("client_id", clientID)
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, vkUserInfoURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := oauthHTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("vk user_info: status %d", resp.StatusCode)
	}
	var out struct {
		User struct {
			UserID    string `json:"user_id"`
			FirstName string `json:"first_name"`
			LastName  string `json:"last_name"`
			Email     string `json:"email"`
		} `json:"user"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	if out.User.UserID == "" {
		return nil, fmt.Errorf("vk user_info: empty user_id")
	}
	display := strings.TrimSpace(out.User.FirstName + " " + out.User.LastName)
	if display == "" {
		display = out.User.Email
	}
	return &oauthProfile{UID: out.User.UserID, Display: display}, nil
}

// ── Яндекс ID ────────────────────────────────────────────────────────
// GET https://login.yandex.ru/info?format=json (Authorization: OAuth <token>)
// → {"id":"123","real_name":"...","display_name":"...","default_email":"..."}
func verifyYandex(r *http.Request, token string) (*oauthProfile, error) {
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, yandexUserInfoURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "OAuth "+token)
	resp, err := oauthHTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("yandex info: status %d", resp.StatusCode)
	}
	var out struct {
		ID           string `json:"id"`
		RealName     string `json:"real_name"`
		DisplayName  string `json:"display_name"`
		DefaultEmail string `json:"default_email"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	if out.ID == "" {
		return nil, fmt.Errorf("yandex info: empty id")
	}
	display := out.RealName
	if display == "" {
		display = out.DisplayName
	}
	if display == "" {
		display = out.DefaultEmail
	}
	return &oauthProfile{UID: out.ID, Display: display}, nil
}

// ── Google ───────────────────────────────────────────────────────────
// GET https://openidconnect.googleapis.com/v1/userinfo (Bearer <token>)
// → {"sub":"123","name":"...","email":"..."}
func verifyGoogle(r *http.Request, token string) (*oauthProfile, error) {
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, googleUserInfoURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := oauthHTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("google userinfo: status %d", resp.StatusCode)
	}
	var out struct {
		Sub   string `json:"sub"`
		Name  string `json:"name"`
		Email string `json:"email"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	if out.Sub == "" {
		return nil, fmt.Errorf("google userinfo: empty sub")
	}
	display := out.Name
	if display == "" {
		display = out.Email
	}
	return &oauthProfile{UID: out.Sub, Display: display}, nil
}
