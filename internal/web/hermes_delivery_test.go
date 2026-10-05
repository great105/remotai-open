package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"tgcontrol/internal/config"
	"tgcontrol/internal/hermes"
)

func TestHermesPrivateDeliveryUsesAuthorizedOwnerRoute(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/v1/agent/send" || r.Header.Get("Authorization") != "Bearer fixture-jwt" {
			t.Error("wrong authorized transport")
		}
		var payload struct {
			Text string `json:"text"`
		}
		_ = json.NewDecoder(r.Body).Decode(&payload)
		if strings.Contains(payload.Text, "secret-fixture") || !strings.Contains(payload.Text, "device=fixture-pc") || !strings.Contains(payload.Text, "session=stored-fixture") {
			t.Error("private deeplink missing or leaked content")
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true,"sent":["text"]}`))
	}))
	defer server.Close()
	cfg := &config.Config{Mode: config.ModeCentralBot, TelegramUserID: "42", RelayURL: server.URL, RelayJWT: "fixture-jwt", DeviceID: "fixture-pc", APITokenUID: 1}
	sender, ok := any(&Server{}).(interface {
		sendHermesDelivery(context.Context, *http.Client, *config.Config, int64, hermes.AttentionRecord) error
	})
	if !ok {
		t.Fatal("authorized background Telegram route unavailable")
	}
	record := hermes.AttentionRecord{ID: "epoch-request", StoredSessionID: "stored-fixture", Kind: "question", Params: json.RawMessage(`{"question":"secret-fixture"}`)}
	if err := sender.sendHermesDelivery(context.Background(), server.Client(), cfg, 2, record); err == nil {
		t.Fatal("foreign owner sent")
	}
	if calls != 0 {
		t.Fatal("foreign owner contacted delivery route")
	}
	if err := sender.sendHermesDelivery(context.Background(), server.Client(), cfg, 1, record); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("sends=%d", calls)
	}
}
