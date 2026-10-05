package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"tgcontrol-relay/internal/config"
)

// Неверный код подключения обязан приходить с МАШИННЫМ кодом: без него клиент
// разбирал только HTTP-статус и показывал бессмысленное «Не найдено.» — человек
// не понимал ни что не найдено, ни что код живёт 15 минут.
func TestPairConfirmUnknownCodeReturnsMachineCode(t *testing.T) {
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

	check := func(name string, req *http.Request) {
		t.Helper()
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("%s: статус %d, ждали 404", name, resp.StatusCode)
		}
		var body struct {
			Code  string `json:"code"`
			Error string `json:"error"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatalf("%s: разбор тела: %v", name, err)
		}
		if body.Code != "pair_code_not_found" {
			t.Fatalf("%s: code = %q, ждали pair_code_not_found", name, body.Code)
		}
		// Фолбэк-текст для старых сборок и curl должен называть срок жизни кода
		// и вести к действию. Требование «назвать длину кода» снято 23.08:
		// живой разбор показал, что 404 приходит не от опечатки (регистр,
		// дефис и двойники 5/S, 2/Z, 8/B, 6/G, U/V релей сворачивает сам), а от
		// кода с давно погасшего экрана — за сутки на бою не было ни одного
		// /v1/pair/request при пустой таблице pairing_codes. Длину человек и
		// так видит в подписи поля ввода.
		if !strings.Contains(body.Error, "15 мин") {
			t.Fatalf("%s: текст без срока жизни кода: %q", name, body.Error)
		}
		if !strings.Contains(body.Error, "Remotai") {
			t.Fatalf("%s: текст не говорит, где взять свежий код: %q", name, body.Error)
		}
	}

	// Mini App (Telegram initData).
	tmaReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/pair/confirm",
		strings.NewReader(`{"code":"XXXX-XXXX"}`))
	tmaReq.Header.Set("Content-Type", "application/json")
	tmaReq.Header.Set("Authorization", "tma "+buildInitData(t, botToken, 8888, "tester"))
	check("pair/confirm", tmaReq)

	// Телефон без Telegram (APK/веб) — тот же ответ.
	nativeReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/pair/confirm-native",
		strings.NewReader(`{"code":"XXXX-XXXX"}`))
	nativeReq.Header.Set("Content-Type", "application/json")
	check("pair/confirm-native", nativeReq)
}

// Срок жизни кода попадает в текст ошибки словами: «60 мин» формально верно, но
// человек в переписке говорит «час», а этот текст читают ровно в тот момент,
// когда подключение не вышло и надо коротко объяснить, что случилось.
func TestHumanTTL(t *testing.T) {
	cases := map[time.Duration]string{
		15 * time.Minute: "15 мин",
		45 * time.Minute: "45 мин",
		time.Hour:        "час",
		90 * time.Minute: "90 мин",
		2 * time.Hour:    "2 часа",
		5 * time.Hour:    "5 часов",
		0:                "1 мин",
	}
	for d, want := range cases {
		if got := humanTTL(d); got != want {
			t.Fatalf("humanTTL(%s) = %q, ждали %q", d, got, want)
		}
	}
}

// Дефолт релея — час: при пустом PAIR_CODE_TTL текст обязан называть именно его.
func TestPairCodeNotFoundMsgHourTTL(t *testing.T) {
	s := &Server{Config: &config.Config{PairCodeTTL: time.Hour}}
	msg := s.pairCodeNotFoundMsg()
	if !strings.Contains(msg, "код живёт час") {
		t.Fatalf("текст не называет часовой срок: %q", msg)
	}
}
