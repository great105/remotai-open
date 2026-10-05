package web

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log"
	"net/http"
	"sync"

	"tgcontrol/internal/auth"
	"tgcontrol/internal/config"
)

// AuthManager holds JWT and session management state.
type AuthManager struct {
	jwt      *auth.JWTManager
	sessions *auth.SessionStore
	oidc     *auth.TelegramOIDC

	// PKCE state storage: state -> pkce pair
	pkceMu sync.Mutex
	pkce   map[string]*auth.PKCEPair
}

// NewAuthManager creates and initializes the auth manager.
func NewAuthManager(botToken string) *AuthManager {
	cfg := config.GetNoSetup()

	// Ensure JWT signing key exists
	if cfg.JWTSigningKey == "" {
		cfg.JWTSigningKey = auth.GenerateSigningKey()
		cfg.Save()
	}

	signingKey, err := auth.DecodeSigningKey(cfg.JWTSigningKey)
	if err != nil {
		// Generate new key if decode fails
		cfg.JWTSigningKey = auth.GenerateSigningKey()
		cfg.Save()
		signingKey, _ = auth.DecodeSigningKey(cfg.JWTSigningKey)
	}

	am := &AuthManager{
		jwt:      auth.NewJWTManager(signingKey),
		sessions: auth.NewSessionStore(),
		pkce:     make(map[string]*auth.PKCEPair),
	}

	// Make refresh-token revocation consult the PERSISTENT session store so
	// logout / "revoke session" survives a restart (otherwise a refresh token
	// stays usable for 30 days after logout).
	am.jwt.SetRevocationChecker(func(family string) bool {
		sess := am.sessions.Get(family)
		return sess != nil && sess.Revoked
	})

	if botToken != "" {
		am.oidc = auth.NewTelegramOIDC(botToken)
	}

	return am
}

// ValidateAccessToken validates a Bearer JWT and returns the UID.
func (am *AuthManager) ValidateAccessToken(tokenString string) (int64, error) {
	claims, err := am.jwt.ValidateAccess(tokenString)
	if err != nil {
		return 0, err
	}
	return claims.UID, nil
}

// ── Auth API Endpoints ──────────────────────────────────────────────

// POST /api/auth/login — exchange initData for JWT tokens
func (s *Server) apiAuthLogin(w http.ResponseWriter, r *http.Request) {
	if s.authManager == nil {
		jsonError(w, "Auth not configured", 500)
		return
	}

	var req struct {
		InitData string `json:"init_data"`
	}
	if err := readJSON(r, &req); err != nil {
		jsonError(w, "Invalid request", 400)
		return
	}

	if req.InitData == "" {
		jsonError(w, "init_data required", 400)
		return
	}

	data := ValidateInitData(req.InitData, s.botToken, initDataMaxAge)
	if data == nil {
		jsonError(w, "Invalid init_data", 401)
		return
	}

	uid := ExtractUID(data)
	if uid == 0 {
		jsonError(w, "Could not extract user ID", 401)
		return
	}

	if len(s.allowed) > 0 && !s.allowed[uid] {
		jsonError(w, "User not allowed", 403)
		return
	}

	deviceID := config.GetOrCreateDeviceID()
	pair, err := s.authManager.jwt.IssueTokenPair(uid, deviceID, "initdata")
	if err != nil {
		jsonError(w, "Token generation failed", 500)
		return
	}

	// Create auth session
	s.authManager.sessions.Create(uid, pair.Family, "initdata", r.UserAgent(), r.RemoteAddr)

	log.Printf("[AUTH] JWT login: uid=%d method=initdata", uid)
	jsonResp(w, pair)
}

// POST /api/auth/login/oidc — exchange Telegram OIDC code for JWT tokens
func (s *Server) apiAuthLoginOIDC(w http.ResponseWriter, r *http.Request) {
	if s.authManager == nil || s.authManager.oidc == nil {
		jsonError(w, "OIDC not configured", 500)
		return
	}

	var req struct {
		Code         string `json:"code"`
		CodeVerifier string `json:"code_verifier"`
		RedirectURI  string `json:"redirect_uri"`
		State        string `json:"state"`
	}
	if err := readJSON(r, &req); err != nil {
		jsonError(w, "Invalid request", 400)
		return
	}

	// Validate PKCE state
	if req.State != "" {
		s.authManager.pkceMu.Lock()
		_, ok := s.authManager.pkce[req.State]
		if ok {
			delete(s.authManager.pkce, req.State)
		}
		s.authManager.pkceMu.Unlock()
		if !ok {
			jsonError(w, "Invalid state parameter", 400)
			return
		}
	}

	// Exchange code for tokens
	tokens, err := s.authManager.oidc.ExchangeCode(req.Code, req.RedirectURI, req.CodeVerifier)
	if err != nil {
		log.Printf("[AUTH] OIDC exchange failed: %v", err)
		jsonError(w, "OIDC token exchange failed", 401)
		return
	}

	// Parse user from id_token
	user, err := s.authManager.oidc.ParseIDToken(tokens.IDToken)
	if err != nil {
		log.Printf("[AUTH] OIDC parse failed: %v", err)
		jsonError(w, "Failed to parse user info", 401)
		return
	}

	if user.ID == 0 {
		jsonError(w, "Could not extract user ID", 401)
		return
	}

	if len(s.allowed) > 0 && !s.allowed[user.ID] {
		jsonError(w, "User not allowed", 403)
		return
	}

	deviceID := config.GetOrCreateDeviceID()
	pair, err := s.authManager.jwt.IssueTokenPair(user.ID, deviceID, "oidc")
	if err != nil {
		jsonError(w, "Token generation failed", 500)
		return
	}

	s.authManager.sessions.Create(user.ID, pair.Family, "oidc", r.UserAgent(), r.RemoteAddr)

	log.Printf("[AUTH] JWT login: uid=%d method=oidc username=%s", user.ID, user.Username)
	jsonResp(w, pair)
}

// POST /api/auth/login/token — migrate static API token to JWT
func (s *Server) apiAuthLoginToken(w http.ResponseWriter, r *http.Request) {
	if s.authManager == nil {
		jsonError(w, "Auth not configured", 500)
		return
	}

	var req struct {
		APIToken string `json:"api_token"`
	}
	if err := readJSON(r, &req); err != nil {
		jsonError(w, "Invalid request", 400)
		return
	}

	uid := ValidateAPIToken(req.APIToken)
	if uid == 0 {
		jsonError(w, "Invalid API token", 401)
		return
	}

	if len(s.allowed) > 0 && !s.allowed[uid] {
		jsonError(w, "User not allowed", 403)
		return
	}

	deviceID := config.GetOrCreateDeviceID()
	pair, err := s.authManager.jwt.IssueTokenPair(uid, deviceID, "api_token")
	if err != nil {
		jsonError(w, "Token generation failed", 500)
		return
	}

	s.authManager.sessions.Create(uid, pair.Family, "api_token", r.UserAgent(), r.RemoteAddr)

	log.Printf("[AUTH] JWT login: uid=%d method=api_token (migration)", uid)
	jsonResp(w, pair)
}

// POST /api/auth/refresh — refresh token pair
func (s *Server) apiAuthRefresh(w http.ResponseWriter, r *http.Request) {
	if s.authManager == nil {
		jsonError(w, "Auth not configured", 500)
		return
	}

	var req struct {
		RefreshToken string `json:"refresh_token"`
	}
	if err := readJSON(r, &req); err != nil {
		jsonError(w, "Invalid request", 400)
		return
	}

	pair, err := s.authManager.jwt.RefreshTokens(req.RefreshToken)
	if err != nil {
		log.Printf("[AUTH] Refresh failed: %v", err)
		jsonError(w, "Invalid refresh token", 401)
		return
	}

	// Record activity so Cleanup() doesn't evict an actively-used session and the
	// last-used timestamp reflects reality.
	s.authManager.sessions.Touch(pair.Family)

	jsonResp(w, pair)
}

// POST /api/auth/logout — revoke current session
func (s *Server) apiAuthLogout(w http.ResponseWriter, r *http.Request, uid int64) {
	if s.authManager != nil {
		s.authManager.sessions.RevokeAll(uid)
	}
	log.Printf("[AUTH] Logout: uid=%d", uid)
	jsonResp(w, map[string]bool{"ok": true})
}

// GET /api/auth/sessions — list active auth sessions
func (s *Server) apiAuthSessions(w http.ResponseWriter, r *http.Request, uid int64) {
	if s.authManager == nil {
		jsonResp(w, map[string]any{"sessions": []any{}})
		return
	}

	sessions := s.authManager.sessions.ListActive(uid)
	jsonResp(w, map[string]any{"sessions": sessions})
}

// GET /api/auth/oidc/url — generate OIDC authorization URL
func (s *Server) apiAuthOIDCURL(w http.ResponseWriter, r *http.Request) {
	if s.authManager == nil || s.authManager.oidc == nil {
		jsonError(w, "OIDC not configured", 500)
		return
	}

	redirectURI := r.URL.Query().Get("redirect_uri")
	if redirectURI == "" {
		jsonError(w, "redirect_uri required", 400)
		return
	}

	// Generate state and PKCE
	state := generateRandomHex(16)
	pkce, err := auth.GeneratePKCE()
	if err != nil {
		jsonError(w, "PKCE generation failed", 500)
		return
	}

	// Store PKCE for later validation
	s.authManager.pkceMu.Lock()
	s.authManager.pkce[state] = pkce
	s.authManager.pkceMu.Unlock()

	authURL := s.authManager.oidc.AuthURL(redirectURI, state, pkce.Challenge)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"url":           authURL,
		"state":         state,
		"code_verifier": pkce.Verifier,
	})
}

func generateRandomHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// registerAuthRoutes adds JWT auth endpoints to the server.
func (s *Server) registerAuthRoutes() {
	// Public (no auth) endpoints — rate limited
	s.mux.HandleFunc("POST /api/auth/login", s.rateLimit(s.authLimiter, s.apiAuthLogin))
	s.mux.HandleFunc("POST /api/auth/login/oidc", s.rateLimit(s.authLimiter, s.apiAuthLoginOIDC))
	s.mux.HandleFunc("POST /api/auth/login/token", s.rateLimit(s.authLimiter, s.apiAuthLoginToken))
	s.mux.HandleFunc("POST /api/auth/refresh", s.rateLimit(s.authLimiter, s.apiAuthRefresh))
	s.mux.HandleFunc("GET /api/auth/oidc/url", s.apiAuthOIDCURL)

	// Protected endpoints
	s.mux.HandleFunc("POST /api/auth/logout", s.authWrap(s.apiAuthLogout))
	s.mux.HandleFunc("GET /api/auth/sessions", s.authWrap(s.apiAuthSessions))
}
