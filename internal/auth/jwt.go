// Package auth provides JWT-based authentication, token management,
// and Telegram OIDC integration.
package auth

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// Token lifetimes.
const (
	AccessTokenTTL  = 15 * time.Minute
	RefreshTokenTTL = 30 * 24 * time.Hour // 30 days
)

// TokenPair holds access and refresh tokens.
type TokenPair struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"` // seconds until access token expires
	TokenType    string `json:"token_type"` // "Bearer"
	Family       string `json:"-"`          // internal: token rotation family ID
}

// AccessClaims are the JWT claims for access tokens.
type AccessClaims struct {
	UID      int64  `json:"uid"`
	DeviceID string `json:"device_id,omitempty"`
	Method   string `json:"method"` // "initdata", "oidc", "api_token", "relay"
	jwt.RegisteredClaims
}

// RefreshClaims are the JWT claims for refresh tokens.
type RefreshClaims struct {
	UID    int64  `json:"uid"`
	Family string `json:"family"` // token rotation family ID
	jwt.RegisteredClaims
}

// JWTManager handles JWT creation and validation.
type JWTManager struct {
	signingKey []byte
	mu         sync.RWMutex

	// revokedCheck, if set, reports whether a family is revoked according to the
	// PERSISTENT session store. Without it, logout/revoke would silently stop
	// working (a refresh token would stay valid for 30 days). Set via
	// SetRevocationChecker.
	revokedCheck func(family string) bool
}

// NewJWTManager creates a new JWT manager with the given signing key.
func NewJWTManager(signingKey []byte) *JWTManager {
	return &JWTManager{
		signingKey: signingKey,
	}
}

// SetRevocationChecker wires a persistent revocation lookup (the session store)
// so revocations survive restarts.
func (m *JWTManager) SetRevocationChecker(fn func(family string) bool) {
	m.mu.Lock()
	m.revokedCheck = fn
	m.mu.Unlock()
}

// GenerateSigningKey creates a new random 32-byte signing key, base64-encoded.
func GenerateSigningKey() string {
	key := make([]byte, 32)
	rand.Read(key)
	return base64.StdEncoding.EncodeToString(key)
}

// DecodeSigningKey decodes a base64-encoded signing key.
func DecodeSigningKey(encoded string) ([]byte, error) {
	return base64.StdEncoding.DecodeString(encoded)
}

// IssueTokenPair creates a new access + refresh token pair.
func (m *JWTManager) IssueTokenPair(uid int64, deviceID, method string) (*TokenPair, error) {
	family := generateFamily()
	return m.issueTokenPairWithFamily(uid, deviceID, method, family)
}

func (m *JWTManager) issueTokenPairWithFamily(uid int64, deviceID, method, family string) (*TokenPair, error) {
	now := time.Now()

	// Access token
	accessClaims := AccessClaims{
		UID:      uid,
		DeviceID: deviceID,
		Method:   method,
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(now.Add(AccessTokenTTL)),
			IssuedAt:  jwt.NewNumericDate(now),
			Issuer:    "tgcontrol",
			Subject:   fmt.Sprintf("%d", uid),
		},
	}
	accessToken := jwt.NewWithClaims(jwt.SigningMethodHS256, accessClaims)
	accessStr, err := accessToken.SignedString(m.signingKey)
	if err != nil {
		return nil, fmt.Errorf("sign access token: %w", err)
	}

	// Refresh token
	refreshClaims := RefreshClaims{
		UID:    uid,
		Family: family,
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(now.Add(RefreshTokenTTL)),
			IssuedAt:  jwt.NewNumericDate(now),
			Issuer:    "tgcontrol",
			Subject:   fmt.Sprintf("%d", uid),
		},
	}
	refreshToken := jwt.NewWithClaims(jwt.SigningMethodHS256, refreshClaims)
	refreshStr, err := refreshToken.SignedString(m.signingKey)
	if err != nil {
		return nil, fmt.Errorf("sign refresh token: %w", err)
	}

	return &TokenPair{
		AccessToken:  accessStr,
		RefreshToken: refreshStr,
		ExpiresIn:    int(AccessTokenTTL.Seconds()),
		TokenType:    "Bearer",
		Family:       family,
	}, nil
}

// ValidateAccess validates an access token and returns its claims.
func (m *JWTManager) ValidateAccess(tokenString string) (*AccessClaims, error) {
	token, err := jwt.ParseWithClaims(tokenString, &AccessClaims{}, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", t.Header["alg"])
		}
		return m.signingKey, nil
	})
	if err != nil {
		return nil, err
	}

	claims, ok := token.Claims.(*AccessClaims)
	if !ok || !token.Valid {
		return nil, fmt.Errorf("invalid token claims")
	}

	return claims, nil
}

// RefreshTokens validates a refresh token and issues a new token pair.
// The old refresh token's family is preserved (token rotation).
func (m *JWTManager) RefreshTokens(refreshTokenString string) (*TokenPair, error) {
	token, err := jwt.ParseWithClaims(refreshTokenString, &RefreshClaims{}, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", t.Header["alg"])
		}
		return m.signingKey, nil
	})
	if err != nil {
		return nil, err
	}

	claims, ok := token.Claims.(*RefreshClaims)
	if !ok || !token.Valid {
		return nil, fmt.Errorf("invalid refresh token")
	}

	// Check persistent revocation (survives restarts) — this is what makes
	// logout / "revoke session" actually take effect.
	m.mu.RLock()
	check := m.revokedCheck
	m.mu.RUnlock()

	if check != nil && check(claims.Family) {
		return nil, fmt.Errorf("refresh token family revoked")
	}

	// Issue new pair with same family (rotation)
	return m.issueTokenPairWithFamily(claims.UID, "", "", claims.Family)
}

func generateFamily() string {
	b := make([]byte, 16)
	rand.Read(b)
	return fmt.Sprintf("%x", b)
}
