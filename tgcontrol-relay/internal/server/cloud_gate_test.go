package server

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"tgcontrol-relay/internal/config"
	"tgcontrol-relay/internal/db"
)

// Гейт легко написать так, что он есть в коде, но не стоит на пути. Эти тесты
// ходят живым маршрутом — тем же, каким ходит телефон.

func gateTestServer(t *testing.T, betaFree bool) (*httptest.Server, *Server, string) {
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
		TeamMaxDevices:  25,
		TrialDays:       30,
		BetaFree:        betaFree,
		BotUsername:     "TestBot",
		CORSOrigins:     []string{"*"},
	}
	d := initSQLite(t)
	t.Cleanup(func() { d.Close() })
	srv := New(cfg, d)
	ts := httptest.NewServer(srv.Routes())
	t.Cleanup(ts.Close)

	_, initData := pairTestDevice(t, ts, botToken, "dev-gate", 7777)
	return ts, srv, initData
}

// Без подписки и без пробы облачный стрим обязан упереться в оплату — и именно
// в неё, а не в «сломалось»: 402 с машинным кодом, который клиент покажет
// человеком понятным текстом.
func TestCloudGateBlocksStreamWithoutSubscription(t *testing.T) {
	ts, srv, initData := gateTestServer(t, false)

	// Проба «сгорела»: аккаунт уже пользовался облаком месяц назад.
	if _, err := srv.DB.Exec(`UPDATE users SET trial_end = datetime('now','-1 day')`); err != nil {
		t.Fatalf("expire trial: %v", err)
	}

	wsBase := "ws" + strings.TrimPrefix(ts.URL, "http")
	streamURL := wsBase + "/v1/client/dev-gate/stream?initData=" + url.QueryEscape(initData) +
		"&path=" + url.QueryEscape("/ws/pty/test")

	_, resp, err := websocket.DefaultDialer.Dial(streamURL, nil)
	if err == nil {
		t.Fatal("стрим открылся без подписки — платной границы нет")
	}
	if resp == nil {
		t.Fatalf("нет HTTP-ответа: %v", err)
	}
	if resp.StatusCode != http.StatusPaymentRequired {
		t.Errorf("HTTP %d, want 402 (иначе клиент не отличит оплату от поломки)", resp.StatusCode)
	}
}

// Первое облачное подключение запускает пробу — и оно же должно пройти:
// человек, впервые вышедший в облако, обязан попасть внутрь, а не в стену.
func TestCloudGateStartsTrialOnFirstConnection(t *testing.T) {
	ts, srv, initData := gateTestServer(t, false)

	var before any
	if err := srv.DB.QueryRow(`SELECT trial_end FROM users LIMIT 1`).Scan(&before); err != nil {
		t.Fatalf("read trial_end: %v", err)
	}
	if before != nil {
		t.Fatalf("до первого облачного подключения пробы быть не должно, получили %v", before)
	}

	wsBase := "ws" + strings.TrimPrefix(ts.URL, "http")
	// Агента нет, поэтому дальше гейта запрос не уедет — но гейт уже отработает,
	// а вместе с ним стартует проба. Нам важен именно этот побочный эффект.
	streamURL := wsBase + "/v1/client/dev-gate/stream?initData=" + url.QueryEscape(initData) +
		"&path=" + url.QueryEscape("/ws/pty/test")
	_, resp, _ := websocket.DefaultDialer.Dial(streamURL, nil)
	if resp != nil && resp.StatusCode == http.StatusPaymentRequired {
		t.Fatal("первое облачное подключение упёрлось в оплату — проба не стартовала")
	}

	var uid int64
	if err := srv.DB.QueryRow(`SELECT id FROM users LIMIT 1`).Scan(&uid); err != nil {
		t.Fatalf("read user: %v", err)
	}
	u, err := db.GetUserByID(t.Context(), srv.DB, uid)
	if err != nil {
		t.Fatalf("GetUserByID: %v", err)
	}
	if !u.TrialEnd.Valid {
		t.Fatal("проба не запустилась при первом облачном подключении")
	}
	if days, ok := u.TrialDaysLeft(); !ok || days < 29 {
		t.Errorf("проба на %d дней, ожидалось 30", days)
	}
}

// Control-WS — вторая дверь в облако, и она обязана быть закрыта тоже.
// Пока гейт стоял только на стримах, пульт всё равно управлял компьютером.
func TestCloudGateBlocksControlWS(t *testing.T) {
	ts, srv, initData := gateTestServer(t, false)

	if _, err := srv.DB.Exec(`UPDATE users SET trial_end = datetime('now','-1 day')`); err != nil {
		t.Fatalf("expire trial: %v", err)
	}

	wsBase := "ws" + strings.TrimPrefix(ts.URL, "http")
	clientURL := wsBase + "/v1/client/dev-gate/ws?initData=" + url.QueryEscape(initData)

	_, resp, err := websocket.DefaultDialer.Dial(clientURL, nil)
	if err == nil {
		t.Fatal("control-WS открылся без подписки — вторая дверь в облако осталась нараспашку")
	}
	if resp == nil {
		t.Fatalf("нет HTTP-ответа: %v", err)
	}
	if resp.StatusCode != http.StatusPaymentRequired {
		t.Errorf("HTTP %d, want 402", resp.StatusCode)
	}
}
