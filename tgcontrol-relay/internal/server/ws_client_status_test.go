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
	"tgcontrol-relay/internal/protocol"
)

// TestClientWSAgentStatusTransition — гонка первого пейринга: телефон
// подключается к /v1/client/{id}/ws РАНЬШЕ, чем ПК доходит до релея.
// Раньше relay отправлял agent_status один раз при открытии соединения —
// клиент навсегда оставался с online:false и рисовал «Переподключение…»
// при живом WS. Теперь вотчер обязан прислать online:true, когда агент
// появится.
func TestClientWSAgentStatusTransition(t *testing.T) {
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
		// Тест про транспорт и статусы: первое подключение запускает пробу,
		// которая даёт доступ через платный гейт (cloud_gate.go).
		TrialDays:   30,
		BotUsername: "TestBot",
		CORSOrigins: []string{"*"},
	}
	d := initSQLite(t)
	defer d.Close()
	srv := New(cfg, d)
	ts := httptest.NewServer(srv.Routes())
	defer ts.Close()

	deviceJWT, initData := pairTestDevice(t, ts, botToken, "dev-status", 5151)
	wsBase := "ws" + strings.TrimPrefix(ts.URL, "http")

	// ── 1. Клиент подключается, агента ещё нет → online:false. ──
	client, _, err := websocket.DefaultDialer.Dial(
		wsBase+"/v1/client/dev-status/ws?initData="+url.QueryEscape(initData), nil)
	if err != nil {
		t.Fatalf("client ws dial: %v", err)
	}
	defer client.Close()

	readStatus := func(timeout time.Duration, want bool) {
		t.Helper()
		deadline := time.Now().Add(timeout)
		for {
			client.SetReadDeadline(deadline)
			var msg map[string]any
			if err := client.ReadJSON(&msg); err != nil {
				t.Fatalf("read (ждали agent_status online=%v): %v", want, err)
			}
			if msg["type"] == "agent_status" {
				if msg["online"] != want {
					t.Fatalf("agent_status online=%v, want %v", msg["online"], want)
				}
				return
			}
		}
	}
	readStatus(3*time.Second, false)

	// ── 2. Агент подключается ПОСЛЕ клиента. ──
	hdr := http.Header{}
	hdr.Set("Authorization", "Bearer "+deviceJWT)
	agentCtl, _, err := websocket.DefaultDialer.Dial(wsBase+"/v1/agent/connect", hdr)
	if err != nil {
		t.Fatalf("agent control dial: %v", err)
	}
	defer agentCtl.Close()
	if err := agentCtl.WriteJSON(protocol.Hello{
		Type: protocol.MsgHello, ProtocolVersion: protocol.ProtocolVersion,
		DeviceID: "dev-status", JWT: deviceJWT, Platform: "linux", Hostname: "h",
	}); err != nil {
		t.Fatalf("agent hello: %v", err)
	}
	var welcome protocol.Welcome
	if err := agentCtl.ReadJSON(&welcome); err != nil || welcome.Type != protocol.MsgWelcome {
		t.Fatalf("agent welcome: %v (%+v)", err, welcome)
	}

	// ── 3. Вотчер (тикер 3с) должен прислать online:true. ──
	readStatus(10*time.Second, true)
}
