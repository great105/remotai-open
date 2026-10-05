package server

import (
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// Sign in with Apple: у Apple нет userinfo по access_token, профиль приезжает
// в id_token (RS256 JWT). Валидируем подпись по публичным ключам Apple
// (JWKS), issuer и audience. Для redirect-флоу client_secret это отдельный
// ES256 JWT, подписанный приватным ключом из Apple Developer (.p8).

var appleKeysURL = "https://appleid.apple.com/auth/keys" // var — подмена в тестах

// appleJWKS — кэш публичных ключей Apple по kid. TTL осторожный: Apple
// ротирует ключи редко, при неизвестном kid кэш сбрасывается и перечитывается.
var appleJWKS = &jwksCache{ttl: time.Hour}

type jwksCache struct {
	mu        sync.Mutex
	keys      map[string]*rsa.PublicKey
	fetchedAt time.Time
	ttl       time.Duration
}

func (c *jwksCache) get(r *http.Request, kid string) (*rsa.PublicKey, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.keys != nil && time.Since(c.fetchedAt) < c.ttl {
		if k, ok := c.keys[kid]; ok {
			return k, nil
		}
	}
	// Нет ключа или кэш протух: перечитываем (один раз, под мьютексом).
	keys, err := fetchAppleKeys(r)
	if err != nil {
		return nil, err
	}
	c.keys, c.fetchedAt = keys, time.Now()
	k, ok := c.keys[kid]
	if !ok {
		return nil, fmt.Errorf("apple jwks: unknown kid %q", kid)
	}
	return k, nil
}

func fetchAppleKeys(r *http.Request) (map[string]*rsa.PublicKey, error) {
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, appleKeysURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := oauthHTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("apple jwks: status %d", resp.StatusCode)
	}
	var doc struct {
		Keys []struct {
			Kty string `json:"kty"`
			Kid string `json:"kid"`
			N   string `json:"n"`
			E   string `json:"e"`
		} `json:"keys"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return nil, err
	}
	out := make(map[string]*rsa.PublicKey, len(doc.Keys))
	for _, k := range doc.Keys {
		if k.Kty != "RSA" {
			continue
		}
		nb, err := base64.RawURLEncoding.DecodeString(k.N)
		if err != nil {
			return nil, fmt.Errorf("apple jwks: bad n: %w", err)
		}
		eb, err := base64.RawURLEncoding.DecodeString(k.E)
		if err != nil {
			return nil, fmt.Errorf("apple jwks: bad e: %w", err)
		}
		e := 0
		for _, b := range eb {
			e = e<<8 | int(b)
		}
		out[k.Kid] = &rsa.PublicKey{N: new(big.Int).SetBytes(nb), E: e}
	}
	if len(out) == 0 {
		return nil, errors.New("apple jwks: no RSA keys")
	}
	return out, nil
}

// verifyAppleIDToken проверяет id_token от Apple и возвращает профиль.
// audience это Services ID (для веба) или bundle id (для iOS) из конфига.
func verifyAppleIDToken(r *http.Request, idToken, clientID string) (*oauthProfile, error) {
	claims := jwt.MapClaims{}
	tok, err := jwt.ParseWithClaims(idToken, claims, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodRSA); !ok {
			return nil, fmt.Errorf("apple id_token: unexpected alg %v", t.Header["alg"])
		}
		kid, _ := t.Header["kid"].(string)
		if kid == "" {
			return nil, errors.New("apple id_token: no kid")
		}
		return appleJWKS.get(r, kid)
	}, jwt.WithValidMethods([]string{"RS256"}), jwt.WithIssuer("https://appleid.apple.com"), jwt.WithAudience(clientID))
	if err != nil {
		return nil, err
	}
	sub, _ := claims["sub"].(string)
	if !tok.Valid || sub == "" {
		return nil, errors.New("apple id_token: invalid")
	}
	// email приезжает только в первом id_token; display не критичен.
	email, _ := claims["email"].(string)
	return &oauthProfile{UID: sub, Display: email}, nil
}

// appleClientSecret генерирует client_secret для token exchange: ES256 JWT
// (iss=Team ID, sub=Services ID, aud=https://appleid.apple.com), подписанный
// приватным ключом .p8 из Apple Developer. Живёт до 6 месяцев, мы кладём 1 час.
func appleClientSecret(teamID, keyID, clientID, privateKeyPEM string) (string, error) {
	key, err := jwt.ParseECPrivateKeyFromPEM([]byte(privateKeyPEM))
	if err != nil {
		return "", fmt.Errorf("apple private key: %w", err)
	}
	now := time.Now()
	claims := jwt.RegisteredClaims{
		Issuer:    teamID,
		Subject:   clientID,
		Audience:  jwt.ClaimStrings{"https://appleid.apple.com"},
		IssuedAt:  jwt.NewNumericDate(now),
		ExpiresAt: jwt.NewNumericDate(now.Add(time.Hour)),
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodES256, claims)
	tok.Header["kid"] = keyID
	return tok.SignedString(key)
}
