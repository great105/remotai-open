package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"tgcontrol-relay/internal/config"
)

// Компьютер выключен — самый частый отказ дня, и клиент должен узнавать его по
// МАШИННОМУ коду, а не по англоязычному тексту. Раньше это была безликая 502,
// которую приложение показывало как «Ошибка сервера — попробуйте позже» (а на
// части экранов — как «Нет терминалов» и вечный спиннер).
func TestProxyOfflineReturnsMachineCode(t *testing.T) {
	const botToken = "1234:ABCDEF"
	cfg := &config.Config{
		BotToken:        botToken,
		JWTSecret:       "test-secret-please-change-1234567890",
		JWTTTL:          time.Hour,
		PairCodeTTL:     time.Minute,
		PairMaxAttempts: 5,
		FreeMaxDevices:  3,
		ProMaxDevices:   10,
		TeamMaxDevices:  50,
		BotUsername:     "TestBot",
		CORSOrigins:     []string{"*"},
		// Прокси теперь проходит денежный гейт (proxy.go): тест не про деньги,
		// поэтому пускаем как в остальных серверных тестах.
		TrialDays: 30,
	}
	d := initSQLite(t)
	defer d.Close()
	srv := New(cfg, d)
	ts := httptest.NewServer(srv.Routes())
	defer ts.Close()

	// Устройство привязано, но агент к релею не подключён (ПК выключен).
	_, initData := pairTestDevice(t, ts, botToken, "dev-offline", 7777)

	req, err := http.NewRequest(http.MethodPost,
		ts.URL+"/v1/client/dev-offline/request",
		bytes.NewReader([]byte(`{"method":"GET","path":"/api/system/stats"}`)))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "tma "+initData)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status=%d, ожидали 502", resp.StatusCode)
	}
	var body struct {
		Error string `json:"error"`
		Code  string `json:"code"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Code != "pc_offline" {
		t.Fatalf("code=%q, ожидали pc_offline (клиент различает офлайн по коду, а не по тексту %q)", body.Code, body.Error)
	}
}
