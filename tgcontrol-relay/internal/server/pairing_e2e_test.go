package server

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"tgcontrol-relay/internal/config"
	"tgcontrol-relay/internal/db"
)

// initSQLite — in-memory sqlite c миграциями.
func initSQLite(t *testing.T) *sql.DB {
	t.Helper()
	d, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("db open: %v", err)
	}
	if err := db.Migrate(d); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return d
}

func TestPairingEndToEnd(t *testing.T) {
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
	}
	d := initSQLite(t)
	defer d.Close()
	srv := New(cfg, d)
	ts := httptest.NewServer(srv.Routes())
	defer ts.Close()

	// 1) Десктоп получает код.
	body := bytes.NewReader([]byte(`{"device_id":"dev-e2e","hostname":"host-e2e","platform":"linux","agent_version":"e2e"}`))
	resp, err := http.Post(ts.URL+"/v1/pair/request", "application/json", body)
	if err != nil {
		t.Fatalf("pair/request: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("pair/request status %d", resp.StatusCode)
	}
	var pr struct {
		Code string `json:"code"`
	}
	json.NewDecoder(resp.Body).Decode(&pr)
	resp.Body.Close()
	if pr.Code == "" {
		t.Fatalf("empty code")
	}

	// 2) Пользователь в Mini App подтверждает (имитируем initData).
	initData := buildInitData(t, botToken, 7777, "tester")
	confirmBody := bytes.NewReader([]byte(`{"code":"` + pr.Code + `"}`))
	req, _ := http.NewRequest("POST", ts.URL+"/v1/pair/confirm", confirmBody)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "tma "+initData)
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("pair/confirm: %v", err)
	}
	if resp2.StatusCode != 200 {
		raw, _ := readBody(resp2)
		t.Fatalf("pair/confirm status %d body=%s", resp2.StatusCode, raw)
	}
	var cr struct {
		OK       bool   `json:"ok"`
		DeviceID string `json:"device_id"`
		JWT      string `json:"jwt"`
	}
	json.NewDecoder(resp2.Body).Decode(&cr)
	resp2.Body.Close()
	if !cr.OK || cr.JWT == "" || cr.DeviceID != "dev-e2e" {
		t.Fatalf("confirm response not ok: %+v", cr)
	}

	// 3) Десктоп опрашивает status и видит JWT.
	statusResp, err := http.Get(ts.URL + "/v1/pair/status?code=" + url.QueryEscape(pr.Code))
	if err != nil {
		t.Fatalf("pair/status: %v", err)
	}
	var st struct {
		Confirmed bool   `json:"confirmed"`
		JWT       string `json:"jwt"`
	}
	json.NewDecoder(statusResp.Body).Decode(&st)
	statusResp.Body.Close()
	if !st.Confirmed || st.JWT == "" {
		t.Fatalf("status not confirmed: %+v", st)
	}

	// 4) GET /v1/devices с initData.
	req3, _ := http.NewRequest("GET", ts.URL+"/v1/devices", nil)
	req3.Header.Set("Authorization", "tma "+initData)
	resp3, err := http.DefaultClient.Do(req3)
	if err != nil {
		t.Fatalf("devices: %v", err)
	}
	var dv struct {
		Devices []map[string]any `json:"devices"`
	}
	json.NewDecoder(resp3.Body).Decode(&dv)
	resp3.Body.Close()
	if len(dv.Devices) != 1 || dv.Devices[0]["id"] != "dev-e2e" {
		t.Fatalf("devices list bad: %+v", dv)
	}

	// Cleanup goroutines if Run was used (мы используем Routes() напрямую — нет фоновых горутин)
	_ = context.Background()
}

func readBody(r *http.Response) (string, error) {
	data, err := io.ReadAll(r.Body)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// buildInitData — синтетический initData c корректным HMAC.
func buildInitData(t *testing.T, botToken string, uid int64, username string) string {
	t.Helper()
	user := `{"id":` + strconv.FormatInt(uid, 10) + `,"first_name":"E2E","username":"` + username + `","language_code":"ru"}`
	authDate := strconv.FormatInt(time.Now().Unix(), 10)
	values := url.Values{}
	values.Set("auth_date", authDate)
	values.Set("query_id", "AAEAAA")
	values.Set("user", user)
	var sb strings.Builder
	sb.WriteString("auth_date=" + authDate + "\n")
	sb.WriteString("query_id=AAEAAA\n")
	sb.WriteString("user=" + user)
	sec := hmac.New(sha256.New, []byte("WebAppData"))
	sec.Write([]byte(botToken))
	mac := hmac.New(sha256.New, sec.Sum(nil))
	mac.Write([]byte(sb.String()))
	values.Set("hash", hex.EncodeToString(mac.Sum(nil)))
	return values.Encode()
}
