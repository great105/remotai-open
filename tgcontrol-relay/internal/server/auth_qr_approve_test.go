package server

import (
	"context"
	"net/http"
	"testing"
	"time"

	"tgcontrol-relay/internal/db"
)

// Телефон подтверждает вход на большом экране сам: компьютер забирает JWT
// своим обычным опросом и не замечает, что подтверждал не бот.
func TestQRApproveLetsDesktopIn(t *testing.T) {
	srv, ts := newInfrastructureTestServer(t)
	_, userJWT := issueTestUser(t, srv, 4242, "qr-owner")

	nonce, err := db.GenerateNonce()
	if err != nil {
		t.Fatalf("nonce: %v", err)
	}
	if _, err := db.CreateLoginToken(context.Background(), srv.DB, nonce, 5*time.Minute); err != nil {
		t.Fatalf("создать токен входа: %v", err)
	}

	status, _ := requestJSON(t, http.MethodPost, ts.URL+"/v1/auth/qr/approve", userJWT,
		map[string]any{"login_token": nonce})
	if status != http.StatusOK {
		t.Fatalf("подтверждение вернуло %d, ждали 200", status)
	}

	// Компьютер опрашивает как обычно и получает durable-JWT.
	pollStatus, pollBody := requestJSON(t, http.MethodGet,
		ts.URL+"/v1/auth/tg/poll?login_token="+nonce, "", nil)
	if pollStatus != http.StatusOK {
		t.Fatalf("опрос вернул %d", pollStatus)
	}
	if confirmed, _ := pollBody["confirmed"].(bool); !confirmed {
		t.Fatalf("вход не подтверждён: %v", pollBody)
	}
	if jwt, _ := pollBody["jwt"].(string); jwt == "" {
		t.Error("компьютеру не выдали durable-JWT")
	}
}

// Без аккаунта чужой вход подтвердить нельзя — иначе снятый чужой QR отдавал
// бы доступ кому угодно.
func TestQRApproveRequiresAccount(t *testing.T) {
	srv, ts := newInfrastructureTestServer(t)

	nonce, err := db.GenerateNonce()
	if err != nil {
		t.Fatalf("nonce: %v", err)
	}
	if _, err := db.CreateLoginToken(context.Background(), srv.DB, nonce, 5*time.Minute); err != nil {
		t.Fatalf("создать токен входа: %v", err)
	}

	status, _ := requestJSON(t, http.MethodPost, ts.URL+"/v1/auth/qr/approve", "",
		map[string]any{"login_token": nonce})
	if status != http.StatusUnauthorized {
		t.Fatalf("без входа получили %d, ждали 401", status)
	}
}

// Устаревший и несуществующий код отвечают РАЗНЫМ машинным кодом: человеку
// нужно понять, показать свежий QR или он поймал чужую картинку.
func TestQRApproveDistinguishesExpiredFromMissing(t *testing.T) {
	srv, ts := newInfrastructureTestServer(t)
	_, userJWT := issueTestUser(t, srv, 777, "qr-owner-2")

	stale, _ := db.GenerateNonce()
	if _, err := db.CreateLoginToken(context.Background(), srv.DB, stale, -time.Minute); err != nil {
		t.Fatalf("создать протухший токен: %v", err)
	}
	status, body := requestJSON(t, http.MethodPost, ts.URL+"/v1/auth/qr/approve", userJWT,
		map[string]any{"login_token": stale})
	if status != http.StatusGone {
		t.Fatalf("протухший код вернул %d, ждали 410", status)
	}
	if code, _ := body["code"].(string); code != "login_expired" {
		t.Errorf("код ошибки %q, ждали login_expired", code)
	}

	status, body = requestJSON(t, http.MethodPost, ts.URL+"/v1/auth/qr/approve", userJWT,
		map[string]any{"login_token": "нет-такого"})
	if status != http.StatusNotFound {
		t.Fatalf("несуществующий код вернул %d, ждали 404", status)
	}
	if code, _ := body["code"].(string); code != "login_not_found" {
		t.Errorf("код ошибки %q, ждали login_not_found", code)
	}
}
