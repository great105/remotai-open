package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"tgcontrol-relay/internal/config"
	"tgcontrol-relay/internal/db"
)

// Неудачные попытки использовать pairing-код считаются (анти-брутфорс, аудит
// 2026-06-10): после db.MaxPairCodeAttempts неверных попыток код блокируется
// с понятной ошибкой (машинный код pair_code_locked).
func TestPairConfirmLocksCodeAfterFailedAttempts(t *testing.T) {
	const botToken = "1234:ABCDEF"
	cfg := &config.Config{
		BotToken:        botToken,
		JWTSecret:       "test-secret-please-change-1234567890",
		JWTTTL:          time.Hour,
		PairCodeTTL:     15 * time.Minute,
		PairMaxAttempts: 5,
		FreeMaxDevices:  3,
		ProMaxDevices:   10,
		TeamMaxDevices:  50,
		BotUsername:     "TestBot",
		CORSOrigins:     []string{"*"},
	}
	d := initSQLite(t)
	defer d.Close()
	ts := httptest.NewServer(New(cfg, d).Routes())
	defer ts.Close()
	ctx := context.Background()

	// Просроченный код: каждая попытка подтверждения неудачна (410 expired),
	// но счётчик при этом растёт.
	if _, err := db.InsertPairingCode(ctx, d, "ABCD-EFGH", "dev-lock", "host", "linux", "test", -time.Minute); err != nil {
		t.Fatalf("insert code: %v", err)
	}

	confirm := func() (int, string, string) {
		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/pair/confirm",
			strings.NewReader(`{"code":"ABCD-EFGH"}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "tma "+buildInitData(t, botToken, 8888, "tester"))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("pair/confirm: %v", err)
		}
		defer resp.Body.Close()
		var body struct {
			Code  string `json:"code"`
			Error string `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&body)
		return resp.StatusCode, body.Code, body.Error
	}

	for i := 0; i < db.MaxPairCodeAttempts; i++ {
		st, _, _ := confirm()
		if st != http.StatusGone {
			t.Fatalf("попытка %d: статус %d, ждали 410 (expired)", i+1, st)
		}
	}
	rec, err := db.GetPairingCode(ctx, d, "ABCD-EFGH")
	if err != nil {
		t.Fatalf("get code: %v", err)
	}
	if rec.Attempts != db.MaxPairCodeAttempts {
		t.Fatalf("attempts = %d, ждали %d", rec.Attempts, db.MaxPairCodeAttempts)
	}

	// Следующая попытка — уже блокировка: 410 + pair_code_locked + понятный текст.
	st, code, msg := confirm()
	if st != http.StatusGone {
		t.Fatalf("после порога: статус %d, ждали 410", st)
	}
	if code != "pair_code_locked" {
		t.Fatalf("после порога: code = %q, ждали pair_code_locked", code)
	}
	if !strings.Contains(msg, "заблокирован") || !strings.Contains(msg, "новый код") {
		t.Fatalf("текст блокировки непонятен: %q", msg)
	}
}

// Успешное подтверждение кода НЕ считается неудачной попыткой: счётчик не
// растёт и не мешает повторному скану (мульти-грант в пределах TTL).
func TestPairConfirmSuccessDoesNotCountAttempt(t *testing.T) {
	const botToken = "1234:ABCDEF"
	cfg := &config.Config{
		BotToken:        botToken,
		JWTSecret:       "test-secret-please-change-1234567890",
		JWTTTL:          time.Hour,
		PairCodeTTL:     15 * time.Minute,
		PairMaxAttempts: 5,
		FreeMaxDevices:  3,
		ProMaxDevices:   10,
		TeamMaxDevices:  50,
		BotUsername:     "TestBot",
		CORSOrigins:     []string{"*"},
	}
	d := initSQLite(t)
	defer d.Close()
	ts := httptest.NewServer(New(cfg, d).Routes())
	defer ts.Close()
	ctx := context.Background()

	resp, err := http.Post(ts.URL+"/v1/pair/request", "application/json",
		strings.NewReader(`{"device_id":"dev-no-count","hostname":"host","platform":"linux"}`))
	if err != nil {
		t.Fatalf("pair/request: %v", err)
	}
	var pairReq struct {
		Code string `json:"code"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&pairReq); err != nil {
		t.Fatalf("pair/request decode: %v", err)
	}
	resp.Body.Close()

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/pair/confirm",
		strings.NewReader(`{"code":"`+pairReq.Code+`"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "tma "+buildInitData(t, botToken, 8888, "tester"))
	confirmResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("pair/confirm: %v", err)
	}
	confirmResp.Body.Close()
	if confirmResp.StatusCode != http.StatusOK {
		t.Fatalf("pair/confirm: статус %d, ждали 200", confirmResp.StatusCode)
	}

	rec, err := db.GetPairingCode(ctx, d, pairReq.Code)
	if err != nil {
		t.Fatalf("get code: %v", err)
	}
	if rec.Attempts != 0 {
		t.Fatalf("после успешного confirm attempts = %d, ждали 0", rec.Attempts)
	}
	if !rec.ConsumedAt.Valid {
		t.Fatalf("код должен быть погашен успешным confirm")
	}
}
