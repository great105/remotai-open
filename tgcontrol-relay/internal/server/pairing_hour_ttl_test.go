package server

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"tgcontrol-relay/internal/config"
	"tgcontrol-relay/internal/db"
)

// Код подключения живёт ЧАС. Проверяем не «что записали в expires_at», а что
// принимающая сторона действительно берёт код возрастом больше прежних 15 минут:
// живой случай 26.08 — человек фотографирует экран с QR и отправляет фото, чтобы
// подключение сделали за него, и старого срока на это не хватало.
//
// Возраст кода моделируем остатком TTL при вставке (ждать час в тесте нельзя):
// остаток 40 мин = коду 20 минут, остаток 1 мин = коду 59 минут.
func TestPairCodeLivesAnHour(t *testing.T) {
	const botToken = "1234:ABCDEF"
	cfg := &config.Config{
		BotToken:        botToken,
		JWTSecret:       "test-secret-please-change-1234567890",
		JWTTTL:          time.Hour,
		PairCodeTTL:     time.Hour,
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

	// Свежий код от десктопа обязан жить час, а не 15 минут: это то, что человек
	// увидит на экране строкой «Код действует до …».
	body := bytes.NewReader([]byte(`{"device_id":"dev-hour","hostname":"host-hour","platform":"windows","agent_version":"ttl"}`))
	resp, err := http.Post(ts.URL+"/v1/pair/request", "application/json", body)
	if err != nil {
		t.Fatalf("pair/request: %v", err)
	}
	var pr struct {
		Code      string    `json:"code"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	json.NewDecoder(resp.Body).Decode(&pr)
	resp.Body.Close()
	if left := time.Until(pr.ExpiresAt); left < 59*time.Minute || left > time.Hour+time.Minute {
		t.Fatalf("свежий код живёт %s, ждали около часа", left.Round(time.Second))
	}

	// Возраст 20 минут — прежний TTL уже отверг бы этот код.
	confirmNative(t, ts.URL, mustCode(t, d, "dev-aged-20", 40*time.Minute), 200)
	// Возраст 59 минут — последняя минута часа ещё рабочая.
	confirmNative(t, ts.URL, mustCode(t, d, "dev-aged-59", time.Minute), 200)
	// А час с секундой — уже нет: граница осталась границей, просто отодвинулась.
	confirmNative(t, ts.URL, mustCode(t, d, "dev-aged-61", -time.Second), 410)

	// Тот же возраст, но дверью Telegram Mini App: ветка отдельная, срок общий.
	code := mustCode(t, d, "dev-aged-tg", 40*time.Minute)
	req, _ := http.NewRequest("POST", ts.URL+"/v1/pair/confirm",
		bytes.NewReader([]byte(`{"code":"`+code+`"}`)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "tma "+buildInitData(t, botToken, 7001, "tester"))
	tgResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("pair/confirm: %v", err)
	}
	defer tgResp.Body.Close()
	if tgResp.StatusCode != 200 {
		raw, _ := readBody(tgResp)
		t.Fatalf("pair/confirm возрастом 20 мин: статус %d, тело %s", tgResp.StatusCode, raw)
	}

	// Десктоп забирает доступ обычным опросом — путь до конца, а не только «200».
	stResp, err := http.Get(ts.URL + "/v1/pair/status?code=" + url.QueryEscape(code))
	if err != nil {
		t.Fatalf("pair/status: %v", err)
	}
	defer stResp.Body.Close()
	var st struct {
		Confirmed bool   `json:"confirmed"`
		Expired   bool   `json:"expired"`
		JWT       string `json:"jwt"`
	}
	json.NewDecoder(stResp.Body).Decode(&st)
	if !st.Confirmed || st.Expired || st.JWT == "" {
		t.Fatalf("десктоп не забрал доступ по коду возрастом 20 мин: %+v", st)
	}
}

// mustCode кладёт код с заданным ОСТАТКОМ жизни: так моделируется возраст кода
// без ожидания реального часа.
func mustCode(t *testing.T, d *sql.DB, deviceID string, left time.Duration) string {
	t.Helper()
	code, err := db.GenerateCode()
	if err != nil {
		t.Fatalf("генерация кода: %v", err)
	}
	if _, err := db.InsertPairingCode(context.Background(), d, code, deviceID,
		"host-"+deviceID, "windows", "ttl", left); err != nil {
		t.Fatalf("вставка кода: %v", err)
	}
	return code
}

func confirmNative(t *testing.T, base, code string, want int) {
	t.Helper()
	resp, err := http.Post(base+"/v1/pair/confirm-native", "application/json",
		bytes.NewReader([]byte(`{"code":"`+code+`"}`)))
	if err != nil {
		t.Fatalf("confirm-native %s: %v", code, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != want {
		raw, _ := readBody(resp)
		t.Fatalf("confirm-native %s: статус %d, ждали %d, тело %s", code, resp.StatusCode, want, raw)
	}
}
