package server

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"tgcontrol-relay/internal/db"
)

// Подписываем фейковый id_token тестовым RSA-ключом и отдаём JWKS с
// httptest-сервера: гоняем настоящий путь валидации подписи/iss/aud.
func TestAppleLoginEndToEnd(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa gen: %v", err)
	}
	jwks := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := base64.RawURLEncoding.EncodeToString(key.PublicKey.N.Bytes())
		e := base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.PublicKey.E)).Bytes())
		json.NewEncoder(w).Encode(map[string]any{
			"keys": []map[string]string{{"kty": "RSA", "kid": "test-kid", "n": n, "e": e, "alg": "RS256", "use": "sig"}},
		})
	}))
	defer jwks.Close()

	oldKeys := appleKeysURL
	appleKeysURL = jwks.URL
	defer func() { appleKeysURL = oldKeys }()
	// Сброс кэша JWKS, чтобы не протащить ключи из другого теста.
	appleJWKS.mu.Lock()
	appleJWKS.keys, appleJWKS.fetchedAt = nil, time.Time{}
	appleJWKS.mu.Unlock()

	d := initSQLite(t)
	defer d.Close()
	cfg := emailCfg()
	cfg.Region = "global"
	cfg.AppleClientID = "ru.remotai.web"
	cfg.AppleTeamID, cfg.AppleKeyID, cfg.AppleKeyPEM = "TEAM", "KEY", "unused-here"
	srv := New(cfg, d)
	ts := httptest.NewServer(srv.Routes())
	defer ts.Close()
	ctx := context.Background()

	mkIDToken := func(aud string, ttl time.Duration) string {
		claims := jwt.MapClaims{
			"iss":   "https://appleid.apple.com",
			"aud":   aud,
			"sub":   "apple-sub-1",
			"email": "user@privaterelay.appleid.com",
			"iat":   time.Now().Unix(),
			"exp":   time.Now().Add(ttl).Unix(),
		}
		tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
		tok.Header["kid"] = "test-kid"
		s, err := tok.SignedString(key)
		if err != nil {
			t.Fatalf("sign id_token: %v", err)
		}
		return s
	}

	// 1) Валидный id_token → JWT + identity.
	resp := postJSON(t, ts.URL+"/v1/auth/oauth/apple",
		`{"access_token":"`+mkIDToken("ru.remotai.web", time.Hour)+`"}`, "")
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("apple verify status %d", resp.StatusCode)
	}
	var out struct {
		JWT    string `json:"jwt"`
		UserID int64  `json:"user_id"`
	}
	json.NewDecoder(resp.Body).Decode(&out)
	if out.JWT == "" {
		t.Fatalf("no jwt")
	}
	u, err := db.GetUserByIdentity(ctx, d, "apple", "apple-sub-1")
	if err != nil || u.ID != out.UserID {
		t.Fatalf("identity not linked: %+v %v", u, err)
	}

	// 2) Чужая аудитория → 401.
	resp2 := postJSON(t, ts.URL+"/v1/auth/oauth/apple",
		`{"access_token":"`+mkIDToken("someone.else.app", time.Hour)+`"}`, "")
	resp2.Body.Close()
	if resp2.StatusCode != 401 {
		t.Fatalf("wrong aud: want 401, got %d", resp2.StatusCode)
	}

	// 3) Просроченный id_token → 401.
	resp3 := postJSON(t, ts.URL+"/v1/auth/oauth/apple",
		`{"access_token":"`+mkIDToken("ru.remotai.web", -time.Hour)+`"}`, "")
	resp3.Body.Close()
	if resp3.StatusCode != 401 {
		t.Fatalf("expired: want 401, got %d", resp3.StatusCode)
	}

	// 4) На ru-регионе провайдер закрыт → 404.
	ruCfg := emailCfg()
	ruCfg.AppleClientID, ruCfg.AppleTeamID, ruCfg.AppleKeyID, ruCfg.AppleKeyPEM = "x", "y", "z", "k"
	ruSrv := New(ruCfg, d)
	ruTS := httptest.NewServer(ruSrv.Routes())
	defer ruTS.Close()
	resp4 := postJSON(t, ruTS.URL+"/v1/auth/oauth/apple",
		`{"access_token":"`+mkIDToken("x", time.Hour)+`"}`, "")
	resp4.Body.Close()
	if resp4.StatusCode != 404 {
		t.Fatalf("apple on ru: want 404, got %d", resp4.StatusCode)
	}
}

// client_secret для token exchange: валидный ES256 JWT с нужными iss/sub/aud,
// подписывается .p8-ключом (проверяем публичной частью).
func TestAppleClientSecret(t *testing.T) {
	// Тестовый EC-ключ в PEM (.p8 это тот же PKCS#8 PEM).
	pemKey, pub := makeTestECKey(t)
	secret, err := appleClientSecret("TEAM123", "KEY123", "ru.remotai.web", pemKey)
	if err != nil {
		t.Fatalf("client secret: %v", err)
	}
	tok, err := jwt.Parse(secret, func(t *jwt.Token) (any, error) {
		return pub, nil
	}, jwt.WithValidMethods([]string{"ES256"}))
	if err != nil || !tok.Valid {
		t.Fatalf("secret jwt invalid: %v", err)
	}
	claims := tok.Claims.(jwt.MapClaims)
	if claims["iss"] != "TEAM123" || claims["sub"] != "ru.remotai.web" {
		t.Fatalf("bad claims: %v", claims)
	}
	aud, _ := claims["aud"].([]any)
	if len(aud) != 1 || aud[0] != "https://appleid.apple.com" {
		t.Fatalf("bad aud: %v", claims["aud"])
	}
	if tok.Header["kid"] != "KEY123" {
		t.Fatalf("bad kid: %v", tok.Header)
	}
}

// makeTestECKey генерирует P-256 ключ и отдаёт его в PKCS#8 PEM (формат .p8)
// вместе с публичной частью для проверки подписи.
func makeTestECKey(t *testing.T) (string, *ecdsa.PublicKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("ec gen: %v", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})), &key.PublicKey
}
