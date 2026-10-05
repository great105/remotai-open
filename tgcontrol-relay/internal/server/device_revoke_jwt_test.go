package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"tgcontrol-relay/internal/auth"
	"tgcontrol-relay/internal/config"
)

// Отзыв устройства (DELETE /v1/devices/{id}) обязан гасить его device-JWT, а не
// только рвать живое соединение: после отзыва токен перестаёт приниматься на
// agent/connect и agent/stream (blocklist-маркер по device_id, аудит 2026-06-10).
func TestRevokeDeviceRevokesDeviceJWT(t *testing.T) {
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

	// Десктоп выпускает код, пользователь подтверждает его в Mini App —
	// на руках device-JWT агента.
	resp, err := http.Post(ts.URL+"/v1/pair/request", "application/json",
		bytes.NewReader([]byte(`{"device_id":"dev-revoke","hostname":"host","platform":"linux"}`)))
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
	if pairReq.Code == "" {
		t.Fatalf("pair/request: пустой код")
	}

	confirmReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/pair/confirm",
		strings.NewReader(`{"code":"`+pairReq.Code+`"}`))
	confirmReq.Header.Set("Content-Type", "application/json")
	confirmReq.Header.Set("Authorization", "tma "+buildInitData(t, botToken, 8888, "tester"))
	confirmResp, err := http.DefaultClient.Do(confirmReq)
	if err != nil {
		t.Fatalf("pair/confirm: %v", err)
	}
	var confirmBody struct {
		DeviceID string `json:"device_id"`
		JWT      string `json:"jwt"`
	}
	if err := json.NewDecoder(confirmResp.Body).Decode(&confirmBody); err != nil {
		t.Fatalf("pair/confirm decode: %v", err)
	}
	confirmResp.Body.Close()
	if confirmResp.StatusCode != http.StatusOK || confirmBody.JWT == "" {
		t.Fatalf("pair/confirm: статус %d, jwt=%q", confirmResp.StatusCode, confirmBody.JWT)
	}
	deviceJWT := confirmBody.JWT

	agentStream := func() int {
		req, _ := http.NewRequest(http.MethodGet, ts.URL+"/v1/agent/stream?jwt="+deviceJWT+"&token=no-such-stream", nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("agent/stream: %v", err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return resp.StatusCode
	}
	agentConnect := func() int {
		req, _ := http.NewRequest(http.MethodGet, ts.URL+"/v1/agent/connect?jwt="+deviceJWT, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("agent/connect: %v", err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return resp.StatusCode
	}

	// Контроль до отзыва: токен валиден — agent/stream доходит до поиска
	// стрима (404, стрима нет), а не отбрасывается по авторизации (401).
	if st := agentStream(); st != http.StatusNotFound {
		t.Fatalf("agent/stream до отзыва: статус %d, ждали 404", st)
	}

	// Владелец отзывает устройство.
	delReq, _ := http.NewRequest(http.MethodDelete, ts.URL+"/v1/devices/dev-revoke", nil)
	delReq.Header.Set("Authorization", "tma "+buildInitData(t, botToken, 8888, "tester"))
	delResp, err := http.DefaultClient.Do(delReq)
	if err != nil {
		t.Fatalf("DELETE device: %v", err)
	}
	io.Copy(io.Discard, delResp.Body)
	delResp.Body.Close()
	if delResp.StatusCode != http.StatusOK {
		t.Fatalf("DELETE device: статус %d, ждали 200", delResp.StatusCode)
	}

	// Blocklist-маркер записан и виден через IsRevoked.
	if ok, err := auth.IsDeviceRevoked(ctx, d, "dev-revoke"); err != nil || !ok {
		t.Fatalf("IsDeviceRevoked после отзыва: revoked=%v err=%v", ok, err)
	}
	if ok, err := auth.IsRevoked(ctx, d, auth.DeviceRevocationJTI("dev-revoke")); err != nil || !ok {
		t.Fatalf("IsRevoked(marker) после отзыва: revoked=%v err=%v", ok, err)
	}

	// Токен отозванного устройства больше не принимается.
	if st := agentStream(); st != http.StatusUnauthorized {
		t.Fatalf("agent/stream после отзыва: статус %d, ждали 401", st)
	}
	if st := agentConnect(); st != http.StatusUnauthorized {
		t.Fatalf("agent/connect после отзыва: статус %d, ждали 401", st)
	}
}
