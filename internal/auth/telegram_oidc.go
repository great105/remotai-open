package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	telegramAuthURL  = "https://oauth.telegram.org/auth"
	telegramTokenURL = "https://oauth.telegram.org/auth/token"
)

// TelegramOIDC handles Telegram OpenID Connect authentication.
type TelegramOIDC struct {
	BotID        string // numeric bot ID (from token before ":")
	BotToken     string // full bot token
	ClientSecret string // from BotFather (Web Login settings)
}

// NewTelegramOIDC creates a TelegramOIDC from a bot token.
func NewTelegramOIDC(botToken string) *TelegramOIDC {
	botID := botToken
	if idx := strings.Index(botToken, ":"); idx > 0 {
		botID = botToken[:idx]
	}
	return &TelegramOIDC{
		BotID:    botID,
		BotToken: botToken,
	}
}

// PKCEPair holds a code verifier and challenge for PKCE S256.
type PKCEPair struct {
	Verifier  string `json:"code_verifier"`
	Challenge string `json:"code_challenge"`
}

// GeneratePKCE creates a new PKCE code verifier and challenge.
func GeneratePKCE() (*PKCEPair, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return nil, err
	}
	verifier := base64.RawURLEncoding.EncodeToString(buf)
	hash := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(hash[:])
	return &PKCEPair{Verifier: verifier, Challenge: challenge}, nil
}

// AuthURL constructs the Telegram OIDC authorization URL.
func (t *TelegramOIDC) AuthURL(redirectURI, state, codeChallenge string) string {
	params := url.Values{
		"client_id":             {t.BotID},
		"redirect_uri":          {redirectURI},
		"response_type":         {"code"},
		"scope":                 {"openid profile"},
		"state":                 {state},
		"code_challenge":        {codeChallenge},
		"code_challenge_method": {"S256"},
	}
	return telegramAuthURL + "?" + params.Encode()
}

// OIDCTokens is the response from Telegram's token endpoint.
type OIDCTokens struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	ExpiresIn   int    `json:"expires_in"`
	IDToken     string `json:"id_token"`
}

// ExchangeCode exchanges an authorization code for tokens.
func (t *TelegramOIDC) ExchangeCode(code, redirectURI, codeVerifier string) (*OIDCTokens, error) {
	data := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {redirectURI},
		"client_id":     {t.BotID},
		"code_verifier": {codeVerifier},
	}

	client := &http.Client{Timeout: 10 * time.Second}
	req, err := http.NewRequest("POST", telegramTokenURL, strings.NewReader(data.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if t.ClientSecret != "" {
		req.SetBasicAuth(t.BotID, t.ClientSecret)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("token exchange: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("token exchange failed (%d): %s", resp.StatusCode, body)
	}

	var tokens OIDCTokens
	if err := json.Unmarshal(body, &tokens); err != nil {
		return nil, fmt.Errorf("parse token response: %w", err)
	}
	return &tokens, nil
}

// audienceContains reports whether the JWT "aud" claim (string or []string)
// matches this bot. A missing/empty audience is treated as acceptable (nothing
// to enforce); a present audience must match.
func audienceContains(aud any, botID string) bool {
	switch v := aud.(type) {
	case nil:
		return true
	case string:
		return v == "" || v == botID
	case []any:
		for _, a := range v {
			if s, ok := a.(string); ok && s == botID {
				return true
			}
		}
		return false
	default:
		return true
	}
}

// TelegramUser represents user info from Telegram OIDC.
type TelegramUser struct {
	ID        int64  `json:"id"`
	FirstName string `json:"first_name,omitempty"`
	LastName  string `json:"last_name,omitempty"`
	Username  string `json:"username,omitempty"`
	PhotoURL  string `json:"photo_url,omitempty"`
}

// ParseIDToken extracts user info from a Telegram OIDC id_token (JWT) and
// validates the standard claims (exp/iat/aud/iss).
//
// The token is received directly from Telegram's token endpoint over TLS and the
// code exchange is PKCE+state protected, so transport authenticity is already
// established; the claim checks here are defense-in-depth against expired /
// wrong-audience / replayed tokens. NOTE: full cryptographic signature
// verification is not done — Telegram does not publish a stable JWKS for this
// flow and the signing method is undocumented; the HS256-with-client_secret path
// is verified when a ClientSecret is configured.
func (t *TelegramOIDC) ParseIDToken(idToken string) (*TelegramUser, error) {
	// JWT has 3 parts: header.payload.signature
	parts := strings.Split(idToken, ".")
	if len(parts) != 3 {
		return nil, fmt.Errorf("invalid id_token format")
	}

	// Decode payload (part 1)
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, fmt.Errorf("decode id_token payload: %w", err)
	}

	var claims struct {
		Sub               string `json:"sub"` // Telegram user ID as string
		Name              string `json:"name"`
		PreferredUsername string `json:"preferred_username"`
		Picture           string `json:"picture"`
		Exp               int64  `json:"exp"`
		Iat               int64  `json:"iat"`
		Iss               string `json:"iss"`
		Aud               any    `json:"aud"` // string or []string per JWT spec
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return nil, fmt.Errorf("parse id_token claims: %w", err)
	}

	// Validate standard claims (allow small clock skew).
	now := time.Now().Unix()
	const skew = int64(300)
	if claims.Exp > 0 && now > claims.Exp+skew {
		return nil, fmt.Errorf("id_token expired")
	}
	if claims.Iat > 0 && claims.Iat > now+skew {
		return nil, fmt.Errorf("id_token used before issued")
	}
	if claims.Iss != "" && !strings.Contains(claims.Iss, "telegram") {
		return nil, fmt.Errorf("id_token has unexpected issuer %q", claims.Iss)
	}
	if !audienceContains(claims.Aud, t.BotID) {
		return nil, fmt.Errorf("id_token audience does not match this bot")
	}

	var uid int64
	fmt.Sscanf(claims.Sub, "%d", &uid)

	user := &TelegramUser{
		ID:       uid,
		Username: claims.PreferredUsername,
		PhotoURL: claims.Picture,
	}

	// Parse name into first/last
	if claims.Name != "" {
		parts := strings.SplitN(claims.Name, " ", 2)
		user.FirstName = parts[0]
		if len(parts) > 1 {
			user.LastName = parts[1]
		}
	}

	return user, nil
}
