package server

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"tgcontrol-relay/internal/db"
)

// Redirect-флоу OAuth через релей: клиент просит begin → открывает authorize_url
// в браузере → провайдер возвращает браузер на callback релея → релей меняет
// code на access_token, верифицирует, привязывает state к пользователю →
// клиент забирает durable user-JWT через poll (тот же паттерн, что tg start/poll).
// Работает одинаково в APK, вебе и окне exe: deep-link в приложение не нужен.

const oauthStateTTL = 10 * time.Minute

// Authorize/token endpoint-ы провайдеров. Переменные — для подмены в тестах.
var (
	vkAuthorizeURL     = "https://id.vk.ru/authorize"
	vkTokenURL         = "https://id.vk.ru/oauth2/auth"
	yandexAuthorizeURL = "https://oauth.yandex.ru/authorize"
	yandexTokenURL     = "https://oauth.yandex.ru/token"
	googleAuthorizeURL = "https://accounts.google.com/o/oauth2/v2/auth"
	googleTokenURL     = "https://oauth2.googleapis.com/token"
	appleAuthorizeURL  = "https://appleid.apple.com/auth/authorize"
	appleTokenURL      = "https://appleid.apple.com/auth/token"
)

func (s *Server) oauthRedirectURI(provider string) string {
	return strings.TrimSuffix(s.Config.PublicURL, "/") + "/v1/auth/oauth/" + provider + "/callback"
}

// oauthAuthorizeURL собирает ссылку на страницу входа провайдера.
// PKCE обязателен для VK ID; Яндексу и Google он не мешает.
func (s *Server) oauthAuthorizeURL(provider, state, challenge, deviceID string) (string, error) {
	redirect := s.oauthRedirectURI(provider)
	q := url.Values{
		"response_type": {"code"},
		"state":         {state},
		"redirect_uri":  {redirect},
	}
	switch provider {
	case "vk":
		q.Set("client_id", s.Config.VKClientID)
		q.Set("scope", "email")
		q.Set("code_challenge", challenge)
		q.Set("code_challenge_method", "S256")
		q.Set("device_id", deviceID)
		return vkAuthorizeURL + "?" + q.Encode(), nil
	case "yandex":
		q.Set("client_id", s.Config.YandexClientID)
		return yandexAuthorizeURL + "?" + q.Encode(), nil
	case "google":
		q.Set("client_id", s.Config.GoogleClientID)
		q.Set("scope", "openid email profile")
		q.Set("access_type", "online")
		return googleAuthorizeURL + "?" + q.Encode(), nil
	case "apple":
		// form_post: Apple вернёт code POST-формой на callback (GET с кодом в
		// query Apple не поддерживает для веб-потока).
		q.Set("client_id", s.Config.AppleClientID)
		q.Set("scope", "name email")
		q.Set("response_mode", "form_post")
		return appleAuthorizeURL + "?" + q.Encode(), nil
	}
	return "", fmt.Errorf("unsupported provider %q", provider)
}

// oauthExchangeCode меняет code на токены у провайдера. idToken заполнен
// только у Apple (профиль живёт там, userinfo-эндпоинта у Apple нет).
func (s *Server) oauthExchangeCode(r *http.Request, provider, code string, st *db.OAuthState) (accessToken, idToken string, err error) {
	form := url.Values{
		"grant_type":   {"authorization_code"},
		"code":         {code},
		"redirect_uri": {s.oauthRedirectURI(provider)},
	}
	var tokenURL string
	switch provider {
	case "vk":
		tokenURL = vkTokenURL
		form.Set("client_id", s.Config.VKClientID)
		form.Set("client_secret", s.Config.VKClientSecret)
		form.Set("code_verifier", st.CodeVerifier)
		form.Set("device_id", st.DeviceID)
		form.Set("state", st.State)
	case "yandex":
		tokenURL = yandexTokenURL
		form.Set("client_id", s.Config.YandexClientID)
		form.Set("client_secret", s.Config.YandexSecret)
	case "google":
		tokenURL = googleTokenURL
		form.Set("client_id", s.Config.GoogleClientID)
		form.Set("client_secret", s.Config.GoogleSecret)
	case "apple":
		tokenURL = appleTokenURL
		form.Set("client_id", s.Config.AppleClientID)
		secret, serr := appleClientSecret(s.Config.AppleTeamID, s.Config.AppleKeyID, s.Config.AppleClientID, s.Config.AppleKeyPEM)
		if serr != nil {
			return "", "", serr
		}
		form.Set("client_secret", secret)
	default:
		return "", "", fmt.Errorf("unsupported provider %q", provider)
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := oauthHTTP.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	var out struct {
		AccessToken string `json:"access_token"`
		IDToken     string `json:"id_token"`
		Error       string `json:"error"`
		ErrorDesc   string `json:"error_description"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", "", err
	}
	if resp.StatusCode != http.StatusOK || (out.AccessToken == "" && out.IDToken == "") {
		return "", "", fmt.Errorf("token exchange: status %d error %q %q", resp.StatusCode, out.Error, out.ErrorDesc)
	}
	return out.AccessToken, out.IDToken, nil
}

func randURLSafe(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func pkceChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// POST /v1/auth/oauth/{provider}/begin — начать OAuth-вход. Возвращает
// authorize_url для открытия в браузере и state для опроса poll.
func (s *Server) handleAuthOAuthBegin(w http.ResponseWriter, r *http.Request) {
	provider := chi.URLParam(r, "provider")
	if !s.oauthAllowed(provider) {
		writeErr(w, http.StatusNotFound, "provider not available")
		return
	}
	var linkUserID int64
	if r.URL.Query().Get("link") == "1" {
		claims, authErr := s.requireUserAuth(r)
		if authErr != nil {
			writeErr(w, http.StatusUnauthorized, "authentication required for identity linking")
			return
		}
		linkUserID = claims.UserID
	}
	state, err := db.GenerateNonce()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "state gen failed")
		return
	}
	verifier, err := randURLSafe(32)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "verifier gen failed")
		return
	}
	deviceID, err := randURLSafe(16)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "device id gen failed")
		return
	}
	if err := db.CreateOAuthState(r.Context(), s.DB, state, provider, verifier, deviceID, oauthStateTTL, linkUserID); err != nil {
		writeErr(w, http.StatusInternalServerError, "store state failed")
		return
	}
	authURL, err := s.oauthAuthorizeURL(provider, state, pkceChallenge(verifier), deviceID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"state":         state,
		"authorize_url": authURL,
		"expires_at":    time.Now().UTC().Add(oauthStateTTL).Format(time.RFC3339),
	})
}

// GET /v1/auth/oauth/{provider}/callback?code&state — сюда провайдер
// возвращает браузер пользователя. Меняем code на токен, верифицируем,
// подтверждаем state и показываем «вернитесь в приложение».
func (s *Server) handleAuthOAuthCallback(w http.ResponseWriter, r *http.Request) {
	provider := chi.URLParam(r, "provider")
	fail := func(msg string) {
		log.Printf("[AUTH-OAUTH] callback %s: %s", provider, msg)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprintf(w, oauthDonePage, "Не получилось", "Ошибка входа: "+msg+". Закройте вкладку и попробуйте ещё раз.")
	}
	if !s.oauthAllowed(provider) {
		fail("провайдер не включён")
		return
	}
	// code/state приезжают в query (GET) или в POST-форме (Apple form_post).
	code, stateID := r.URL.Query().Get("code"), r.URL.Query().Get("state")
	if code == "" && r.Method == http.MethodPost {
		if err := r.ParseForm(); err == nil {
			code, stateID = r.PostForm.Get("code"), r.PostForm.Get("state")
		}
	}
	if code == "" || stateID == "" {
		fail("пустой code/state")
		return
	}
	st, err := db.GetOAuthState(r.Context(), s.DB, stateID)
	if err != nil || st.Provider != provider {
		fail("сессия не найдена или истекла")
		return
	}
	accessToken, idToken, err := s.oauthExchangeCode(r, provider, code, st)
	if err != nil {
		fail("обмен кода не удался")
		return
	}
	var p *oauthProfile
	switch provider {
	case "vk":
		p, err = verifyVK(r, accessToken, s.Config.VKClientID)
	case "yandex":
		p, err = verifyYandex(r, accessToken)
	case "google":
		p, err = verifyGoogle(r, accessToken)
	case "apple":
		p, err = verifyAppleIDToken(r, idToken, s.Config.AppleClientID)
	}
	if err != nil {
		fail("верификация профиля не удалась")
		return
	}
	var u *db.User
	if st.LinkUserID.Valid {
		if owner, ownerErr := db.GetUserByIdentity(r.Context(), s.DB, provider, p.UID); ownerErr == nil && owner.ID != st.LinkUserID.Int64 {
			fail("этот способ входа уже связан с другим аккаунтом")
			return
		}
		if err := db.LinkIdentity(r.Context(), s.DB, st.LinkUserID.Int64, provider, p.UID, p.Display); err != nil {
			fail("не удалось привязать способ входа")
			return
		}
		u, err = db.GetUserByID(r.Context(), s.DB, st.LinkUserID.Int64)
	} else {
		u, err = s.userForIdentity(r, provider, p.UID, p.Display)
	}
	if err != nil || u == nil {
		fail("внутренняя ошибка")
		return
	}
	if err := db.ConfirmOAuthState(r.Context(), s.DB, stateID, u.ID); err != nil {
		fail("сессия истекла")
		return
	}
	log.Printf("[AUTH-OAUTH] callback confirmed: user=%d provider=%s", u.ID, provider)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if st.LinkUserID.Valid {
		fmt.Fprintf(w, oauthDonePage, "Способ входа привязан", "Возвращайтесь в приложение Remotai, всё уже готово.")
	} else {
		fmt.Fprintf(w, oauthDonePage, "Вход выполнен", "Возвращайтесь в приложение Remotai, всё уже готово.")
	}
}

const oauthDonePage = `<!DOCTYPE html><html lang="ru"><head><meta charset="utf-8"/>
<meta name="viewport" content="width=device-width, initial-scale=1"/>
<title>Remotai</title>
<style>body{margin:0;min-height:100vh;display:flex;align-items:center;justify-content:center;
background:#06080b;color:#e8eef4;font-family:'Segoe UI',system-ui,sans-serif;text-align:center;padding:24px}
h1{font-size:22px;margin:0 0 10px}p{color:#91a0ae;font-size:15px;line-height:1.6;margin:0;max-width:320px}</style>
</head><body><div><h1>%s</h1><p>%s</p></div></body></html>`

// GET /v1/auth/oauth/poll?state=... — клиент опрашивает результат, как tg/poll.
// confirmed → durable user-JWT (single-use). Bearer анонима → merge.
func (s *Server) handleAuthOAuthPoll(w http.ResponseWriter, r *http.Request) {
	stateID := r.URL.Query().Get("state")
	if stateID == "" {
		writeErr(w, http.StatusBadRequest, "state required")
		return
	}
	st, err := db.GetOAuthState(r.Context(), s.DB, stateID)
	if err != nil {
		if errors.Is(err, db.ErrOAuthStateNotFound) {
			writeJSON(w, http.StatusOK, map[string]any{"confirmed": false, "expired": true})
			return
		}
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if st.ConsumedAt.Valid || time.Now().UTC().After(st.ExpiresAt) {
		writeJSON(w, http.StatusOK, map[string]any{"confirmed": false, "expired": true})
		return
	}
	if !st.ConfirmedAt.Valid || !st.UserID.Valid {
		writeJSON(w, http.StatusOK, map[string]any{"confirmed": false, "expired": false})
		return
	}
	u, err := db.GetUserByID(r.Context(), s.DB, st.UserID.Int64)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := db.ConsumeOAuthState(r.Context(), s.DB, stateID); err != nil {
		log.Printf("[AUTH-OAUTH] consume state: %v", err)
	}
	if st.LinkUserID.Valid {
		log.Printf("[AUTH-OAUTH] identity linked: user=%d provider=%s", u.ID, st.Provider)
		writeJSON(w, http.StatusOK, map[string]any{
			"confirmed": true, "expired": false, "linked": true, "provider": st.Provider,
		})
		return
	}
	log.Printf("[AUTH-OAUTH] login confirmed: user=%d provider=%s", u.ID, st.Provider)
	s.issueUserJWT(w, r, u)
}
