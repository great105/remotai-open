package relay

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Тест: RequestPairingCode и CheckPairingStatus корректно сериализуют запросы
// и парсят ответы. Использует httptest.Server чтобы изолировать desktop client от
// реального relay.
func TestPairingRoundtrip(t *testing.T) {
	var seenRequest, seenStatus bool
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/pair/request", func(w http.ResponseWriter, r *http.Request) {
		seenRequest = true
		if r.Method != http.MethodPost {
			t.Fatalf("expected POST, got %s", r.Method)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":"AB12-CD34","expires_at":"2099-01-01T00:00:00Z","bot_link":"https://t.me/Bot?start=pair_AB12-CD34"}`))
	})
	mux.HandleFunc("/v1/pair/status", func(w http.ResponseWriter, r *http.Request) {
		seenStatus = true
		if r.URL.Query().Get("code") != "AB12-CD34" {
			t.Fatalf("unexpected code: %q", r.URL.Query().Get("code"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"confirmed":true,"jwt":"eyJtest","device_id":"dev1","expires_at":"2099-02-01T00:00:00Z"}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	resp, err := RequestPairingCode(ctx, srv.URL, "dev1", "host1", "") // first pairing: no device-JWT
	if err != nil {
		t.Fatalf("RequestPairingCode: %v", err)
	}
	if resp.Code != "AB12-CD34" {
		t.Fatalf("unexpected code: %q", resp.Code)
	}
	if !strings.Contains(resp.BotLink, "AB12-CD34") {
		t.Fatalf("bad bot_link: %q", resp.BotLink)
	}

	st, err := CheckPairingStatus(ctx, srv.URL, resp.Code)
	if err != nil {
		t.Fatalf("CheckPairingStatus: %v", err)
	}
	if !st.Confirmed || st.JWT != "eyJtest" || st.DeviceID != "dev1" {
		t.Fatalf("unexpected status: %+v", st)
	}
	if !seenRequest || !seenStatus {
		t.Fatalf("handlers not hit: request=%v status=%v", seenRequest, seenStatus)
	}
}

func TestKeystoreFileFallback(t *testing.T) {
	// Платформо-нейтральный тест: file fallback (под Windows DPAPI работает поверх
	// файла; путь один и тот же).
	//
	// ⚠️ ОБЯЗАТЕЛЬНАЯ изоляция: keystore живёт в ~/.tgcontrol/relay_jwt от
	// os.UserHomeDir() — без редиректа домашней директории этот тест СТИРАЕТ
	// БОЕВОЙ device-JWT машины разработчика (ClearJWT ниже). Именно так
	// `go test ./...` дважды ронял облако прод-агента на этом ПК: ключ
	// исчезал, старый процесс жил на JWT в памяти, а ближайший рестарт
	// (= каждое автообновление) вставал в «[RELAY] not configured».
	// Восстановление — только минтом JWT на релее (см. memory
	// relay-jwt-loss-recovery). t.Setenv сам вернёт значения после теста.
	tmp := t.TempDir()
	t.Setenv("USERPROFILE", tmp) // Windows: os.UserHomeDir
	t.Setenv("HOME", tmp)        // Linux/macOS
	_ = ClearJWT()
	const sample = "eyJfake"
	if err := SaveJWT(sample); err != nil {
		t.Fatalf("SaveJWT: %v", err)
	}
	got, err := LoadJWT()
	if err != nil {
		t.Fatalf("LoadJWT: %v", err)
	}
	if got != sample {
		t.Fatalf("LoadJWT mismatch: %q vs %q", got, sample)
	}
	if err := ClearJWT(); err != nil {
		t.Fatalf("ClearJWT: %v", err)
	}
	got2, err := LoadJWT()
	if err != nil {
		t.Fatalf("LoadJWT after clear: %v", err)
	}
	if got2 != "" {
		t.Fatalf("expected empty after clear, got %q", got2)
	}
}
