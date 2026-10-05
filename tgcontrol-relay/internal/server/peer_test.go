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

func peerTestServer(t *testing.T) (*httptest.Server, string) {
	t.Helper()
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
		TrialDays:       30,
		BotUsername:     "TestBot",
		CORSOrigins:     []string{"*"},
	}
	d := initSQLite(t)
	t.Cleanup(func() { d.Close() })
	ts := httptest.NewServer(New(cfg, d).Routes())
	t.Cleanup(ts.Close)
	return ts, botToken
}

func peerAsk(t *testing.T, ts *httptest.Server, jwt, target string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/v1/agent/peer/"+target+"/request",
		bytes.NewReader([]byte(`{"method":"GET","path":"/api/health"}`)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if jwt != "" {
		req.Header.Set("Authorization", "Bearer "+jwt)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// ГЛАВНОЕ СВОЙСТВО ЭТОГО КАНАЛА: сервер дотягивается только до СВОИХ машин.
// Ошибка здесь означала бы чужой компьютер в руках чужого аккаунта, поэтому
// проверка стоит отдельным тестом, а не подпунктом.
func TestПирНеВидитЧужойКомпьютер(t *testing.T) {
	ts, botToken := peerTestServer(t)
	mineJWT, _ := pairTestDevice(t, ts, botToken, "srv-mine", 111)
	pairTestDevice(t, ts, botToken, "pc-alien", 222) // другой пользователь

	resp := peerAsk(t, ts, mineJWT, "pc-alien")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status=%d, ожидали 403 — устройство чужого аккаунта", resp.StatusCode)
	}
}

// Свой компьютер, но он не на связи: ответ обязан отличать «нельзя» от
// «выключен» машинным кодом, иначе на сервере это неразличимо.
func TestПирПолучаетКодОфлайна(t *testing.T) {
	ts, botToken := peerTestServer(t)
	mineJWT, _ := pairTestDevice(t, ts, botToken, "srv-a", 333)
	pairTestDevice(t, ts, botToken, "pc-b", 333) // тот же пользователь

	resp := peerAsk(t, ts, mineJWT, "pc-b")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status=%d, ожидали 502", resp.StatusCode)
	}
	var body struct {
		Code string `json:"code"`
	}
	json.NewDecoder(resp.Body).Decode(&body)
	if body.Code != "pc_offline" {
		t.Fatalf("code=%q, ожидали pc_offline", body.Code)
	}
}

func TestПирБезКлючаНеПускается(t *testing.T) {
	ts, botToken := peerTestServer(t)
	pairTestDevice(t, ts, botToken, "pc-c", 444)

	resp := peerAsk(t, ts, "", "pc-c")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status=%d, ожидали 401", resp.StatusCode)
	}
}

// Список соседей: своё устройство помечено, чужие не видны.
func TestСписокСоседейТолькоСвои(t *testing.T) {
	ts, botToken := peerTestServer(t)
	mineJWT, _ := pairTestDevice(t, ts, botToken, "srv-d", 555)
	pairTestDevice(t, ts, botToken, "pc-e", 555)
	pairTestDevice(t, ts, botToken, "pc-alien2", 666)

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/v1/agent/peers", nil)
	req.Header.Set("Authorization", "Bearer "+mineJWT)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	var body struct {
		Devices []struct {
			ID   string `json:"device_id"`
			Self bool   `json:"self"`
		} `json:"devices"`
	}
	json.NewDecoder(resp.Body).Decode(&body)

	seen := map[string]bool{}
	selfCount := 0
	for _, d := range body.Devices {
		seen[d.ID] = true
		if d.Self {
			selfCount++
		}
	}
	if seen["pc-alien2"] {
		t.Fatal("в списке оказался компьютер чужого аккаунта")
	}
	if !seen["pc-e"] || !seen["srv-d"] {
		t.Fatalf("свои устройства потерялись: %#v", seen)
	}
	if selfCount != 1 {
		t.Fatalf("ровно одно устройство обязано быть помечено «это я», получено %d", selfCount)
	}
}
