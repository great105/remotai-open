package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"tgcontrol/internal/config"
	"tgcontrol/internal/hermes"
)

func TestHermesDeliveryDeviceJWTDoesNotProveTelegramBinding(t *testing.T) {
	cfg := &config.Config{RelayURL: "http://fixture.invalid", RelayJWT: "fixture-jwt", DeviceID: "fixture-pc", APITokenUID: 1}
	if hermesDeliveryOwner(cfg, 1) {
		t.Fatal("device authentication falsely proves Telegram owner binding")
	}
}
func TestHermesDeliveryMutedReceiptIsNotDelivered(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{"ok":true,"muted":true}`)) }))
	defer server.Close()
	cfg := &config.Config{Mode: config.ModeCentralBot, TelegramUserID: "42", RelayURL: server.URL, RelayJWT: "fixture-jwt", DeviceID: "fixture-pc", APITokenUID: 1}
	if (&Server{}).sendHermesDelivery(context.Background(), server.Client(), cfg, 1, hermes.AttentionRecord{ID: "notice"}) == nil {
		t.Fatal("muted receipt marked delivered")
	}
}
