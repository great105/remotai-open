package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"tgcontrol-relay/internal/config"
	"tgcontrol-relay/internal/db"
)

func testCfg() *config.Config {
	return &config.Config{
		BotToken:        "1234:ABCDEF",
		JWTSecret:       "test-secret-please-change-1234567890",
		JWTTTL:          time.Hour,
		PairCodeTTL:     time.Minute,
		PairMaxAttempts: 5,
		FreeMaxDevices:  3,
		ProMaxDevices:   10,
		TeamMaxDevices:  50,
		BotUsername:     "TestBot",
		CORSOrigins:     []string{"*"},
	}
}

type tgStartResp struct {
	LoginToken string `json:"login_token"`
	DeepLink   string `json:"deep_link"`
	BotName    string `json:"bot_username"`
}

type tgPollResp struct {
	Confirmed  bool   `json:"confirmed"`
	Expired    bool   `json:"expired"`
	JWT        string `json:"jwt"`
	TelegramID int64  `json:"telegram_id"`
}

func tgStart(t *testing.T, baseURL string) tgStartResp {
	t.Helper()
	resp, err := http.Post(baseURL+"/v1/auth/tg/start", "application/json", nil)
	if err != nil {
		t.Fatalf("auth/tg/start: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("auth/tg/start status %d", resp.StatusCode)
	}
	var out tgStartResp
	json.NewDecoder(resp.Body).Decode(&out)
	return out
}

func tgPoll(t *testing.T, baseURL, nonce, bearer string) tgPollResp {
	t.Helper()
	req, _ := http.NewRequest("GET", baseURL+"/v1/auth/tg/poll?login_token="+url.QueryEscape(nonce), nil)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("auth/tg/poll: %v", err)
	}
	defer resp.Body.Close()
	var out tgPollResp
	json.NewDecoder(resp.Body).Decode(&out)
	return out
}

func TestTelegramLoginEndToEnd(t *testing.T) {
	d := initSQLite(t)
	defer d.Close()
	srv := New(testCfg(), d)
	ts := httptest.NewServer(srv.Routes())
	defer ts.Close()
	ctx := context.Background()

	// 1) Клиент начинает вход.
	start := tgStart(t, ts.URL)
	if start.LoginToken == "" {
		t.Fatalf("empty login_token")
	}
	if !strings.Contains(start.DeepLink, "TestBot") || !strings.Contains(start.DeepLink, "login_"+start.LoginToken) {
		t.Fatalf("bad deep_link: %s", start.DeepLink)
	}

	// 2) До подтверждения — ждём.
	if p := tgPoll(t, ts.URL, start.LoginToken, ""); p.Confirmed || p.Expired {
		t.Fatalf("poll before confirm should be pending: %+v", p)
	}

	// 3) Бот подтверждает (telegram_id известен из аутентифицированного сообщения).
	u, err := db.UpsertUser(ctx, d, 7777, "tester", "Tester", "ru")
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := db.ConfirmLoginToken(ctx, d, start.LoginToken, u.ID); err != nil {
		t.Fatalf("confirm: %v", err)
	}

	// 4) Клиент забирает durable JWT.
	p := tgPoll(t, ts.URL, start.LoginToken, "")
	if !p.Confirmed || p.JWT == "" || p.TelegramID != 7777 {
		t.Fatalf("poll after confirm bad: %+v", p)
	}

	// 5) JWT работает на /v1/me.
	req, _ := http.NewRequest("GET", ts.URL+"/v1/me", nil)
	req.Header.Set("Authorization", "Bearer "+p.JWT)
	meResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("me: %v", err)
	}
	defer meResp.Body.Close()
	if meResp.StatusCode != 200 {
		t.Fatalf("me status %d", meResp.StatusCode)
	}
	var me struct {
		TelegramID int64 `json:"telegram_id"`
	}
	json.NewDecoder(meResp.Body).Decode(&me)
	if me.TelegramID != 7777 {
		t.Fatalf("me telegram_id mismatch: %+v", me)
	}

	// 6) Single-use: повторный poll → как истёкший, JWT не выдаётся.
	if p2 := tgPoll(t, ts.URL, start.LoginToken, ""); p2.Confirmed || !p2.Expired {
		t.Fatalf("second poll should be expired (single-use): %+v", p2)
	}
}

// ConfirmLoginToken должен отвергать протухший nonce (иначе бот рапортует
// «✅ подтверждено», а клиент при опросе получает expired — «время вышло»
// после «успеха»). И уже использованный токен — как несуществующий.
func TestConfirmLoginTokenRejectsExpiredAndConsumed(t *testing.T) {
	d := initSQLite(t)
	defer d.Close()
	ctx := context.Background()

	u, err := db.UpsertUser(ctx, d, 9999, "exp", "Exp", "ru")
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}

	// Протухший токен (ttl в прошлом) → ErrLoginTokenExpired.
	expNonce, _ := db.GenerateNonce()
	if _, err := db.CreateLoginToken(ctx, d, expNonce, -time.Minute); err != nil {
		t.Fatalf("create expired: %v", err)
	}
	if err := db.ConfirmLoginToken(ctx, d, expNonce, u.ID); !errors.Is(err, db.ErrLoginTokenExpired) {
		t.Fatalf("expired token: want ErrLoginTokenExpired, got %v", err)
	}

	// Живой токен → подтверждается; повторное подтверждение после consume → not found.
	okNonce, _ := db.GenerateNonce()
	if _, err := db.CreateLoginToken(ctx, d, okNonce, time.Minute); err != nil {
		t.Fatalf("create ok: %v", err)
	}
	if err := db.ConfirmLoginToken(ctx, d, okNonce, u.ID); err != nil {
		t.Fatalf("valid token confirm: %v", err)
	}
	// The same user may retry idempotently, but a forwarded deep link must not
	// let another Telegram account overwrite the first confirmation.
	if err := db.ConfirmLoginToken(ctx, d, okNonce, u.ID); err != nil {
		t.Fatalf("same-user confirm retry: %v", err)
	}
	other, err := db.UpsertUser(ctx, d, 10000, "other", "Other", "ru")
	if err != nil {
		t.Fatalf("upsert other: %v", err)
	}
	if err := db.ConfirmLoginToken(ctx, d, okNonce, other.ID); !errors.Is(err, db.ErrLoginTokenNotFound) {
		t.Fatalf("different-user confirm: want ErrLoginTokenNotFound, got %v", err)
	}
	if err := db.ConsumeLoginToken(ctx, d, okNonce); err != nil {
		t.Fatalf("consume: %v", err)
	}
	if err := db.ConsumeLoginToken(ctx, d, okNonce); !errors.Is(err, db.ErrLoginTokenNotFound) {
		t.Fatalf("second consume: want ErrLoginTokenNotFound, got %v", err)
	}
	if err := db.ConfirmLoginToken(ctx, d, okNonce, u.ID); !errors.Is(err, db.ErrLoginTokenNotFound) {
		t.Fatalf("consumed token: want ErrLoginTokenNotFound, got %v", err)
	}

	// Несуществующий nonce → not found.
	if err := db.ConfirmLoginToken(ctx, d, "no-such-nonce", u.ID); !errors.Is(err, db.ErrLoginTokenNotFound) {
		t.Fatalf("missing token: want ErrLoginTokenNotFound, got %v", err)
	}
}

func TestTelegramLoginMergesAnonAccount(t *testing.T) {
	d := initSQLite(t)
	defer d.Close()
	srv := New(testCfg(), d)
	ts := httptest.NewServer(srv.Routes())
	defer ts.Close()
	ctx := context.Background()

	// Анонимный аккаунт с привязанным ПК (как после confirm-native).
	anon, err := db.CreateAnonUser(ctx, d)
	if err != nil {
		t.Fatalf("anon: %v", err)
	}
	if err := db.InsertDevice(ctx, d, &db.Device{
		ID: "dev-merge", UserID: anon.ID, Name: "AnonPC", Platform: "windows",
	}); err != nil {
		t.Fatalf("insert device: %v", err)
	}
	anonJWT, _, err := srv.JWT.IssueUser(anon.ID, anon.TelegramID, anon.Tier)
	if err != nil {
		t.Fatalf("anon jwt: %v", err)
	}

	// Вход через Telegram (другой, реальный аккаунт).
	start := tgStart(t, ts.URL)
	tgUser, err := db.UpsertUser(ctx, d, 8888, "tg", "TG", "ru")
	if err != nil {
		t.Fatalf("upsert tg: %v", err)
	}
	if err := db.ConfirmLoginToken(ctx, d, start.LoginToken, tgUser.ID); err != nil {
		t.Fatalf("confirm: %v", err)
	}

	// Poll с анонимным Bearer → должен слить аккаунт.
	p := tgPoll(t, ts.URL, start.LoginToken, anonJWT)
	if !p.Confirmed || p.JWT == "" || p.TelegramID != 8888 {
		t.Fatalf("poll bad: %+v", p)
	}

	// Устройство переехало на Telegram-аккаунт.
	devs, err := db.ListDevicesByUser(ctx, d, tgUser.ID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(devs) != 1 || devs[0].ID != "dev-merge" {
		t.Fatalf("device not merged: %+v", devs)
	}
	// Анонимный аккаунт удалён.
	if _, err := db.GetUserByID(ctx, d, anon.ID); err == nil {
		t.Fatalf("anon account should be deleted after merge")
	}
}
