package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"tgcontrol/internal/config"
	"tgcontrol/internal/hermes"
	"tgcontrol/internal/relay"
)

// Invoked only inside the isolated registered-test child. Exercises the actual
// relay client, auth injection, buildRequest, mux and business-query policy.
func activityRelayFixture(t *testing.T) {
	cfg := config.GetNoSetup()
	var calls atomic.Int32
	f := &hermesWebFixture{status: hermes.Status{BackendGeneration: 7}, rpc: func(_ context.Context, method string, params any) (json.RawMessage, error) {
		calls.Add(1)
		raw, _ := json.Marshal(params)
		var request map[string]string
		if method != "subagent.tail" || json.Unmarshal(raw, &request) != nil || len(request) != 2 || request["session_id"] != "live-A" || request["subagent_id"] != "child-A" {
			t.Errorf("relay native scope changed: %s %s", method, raw)
		}
		return json.RawMessage(`{"subagent_id":"child-A","available":true,"truncated":false,"text":"12:34:57 tool     | -> terminal(PRIVATE_ARGS)\n12:34:58 result   | terminal ERROR 0.1s: PRIVATE_OUTPUT\n"}`), nil
	}}
	s := &Server{mux: http.NewServeMux(), hermesManagers: map[int64]hermesRuntime{2: f}, allowed: map[int64]bool{2: true}}
	s.registerHermesRoutes()
	peers := make(chan *websocket.Conn, 1)
	finish := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		var hello relay.Hello
		if conn.ReadJSON(&hello) != nil {
			return
		}
		if conn.WriteJSON(relay.Welcome{Type: relay.MsgWelcome, SessionID: "fixture", UserID: 2}) != nil {
			return
		}
		peers <- conn
		<-finish
	}))
	defer upstream.Close()
	cfg.RelayURL = upstream.URL
	cfg.RelayBaseURL = upstream.URL
	cfg.DeviceID = "activity-fixture-device"
	cfg.RelayJWT = "activity-fixture-relay-jwt"
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); relay.New(s.mux).ServeForever(ctx) }()
	defer func() {
		cancel()
		close(finish)
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("activity relay did not stop")
		}
	}()
	var peer *websocket.Conn
	select {
	case peer = <-peers:
	case <-time.After(3 * time.Second):
		t.Fatal("activity relay handshake timed out")
	}
	invoke := func(method, path, body string) relay.CmdResult {
		t.Helper()
		peer.SetWriteDeadline(time.Now().Add(3 * time.Second))
		if err := peer.WriteJSON(relay.Cmd{Type: relay.MsgCmd, RequestID: "activity-fixture", Method: method, Path: path, Body: []byte(body)}); err != nil {
			t.Fatal(err)
		}
		peer.SetReadDeadline(time.Now().Add(3 * time.Second))
		var result relay.CmdResult
		if err := peer.ReadJSON(&result); err != nil {
			t.Fatal(err)
		}
		return result
	}
	const path = "/api/hermes/subagents/activity?session_id=live-A&subagent_id=child-A"
	got := invoke("GET", path, "")
	if got.StatusCode != 200 || calls.Load() != 1 || !strings.Contains(string(got.Body), `"status":"error"`) || strings.Contains(string(got.Body), "PRIVATE_") {
		t.Fatalf("actual relay safe projection failed: %d %s calls=%d", got.StatusCode, got.Body, calls.Load())
	}
	for _, q := range []string{"&uid=1", "&profile=other", "&profile=default", "&session_id=live-A", "&hermes_home=secret", "&trace=1"} {
		before := calls.Load()
		got := invoke("GET", path+q, "")
		if got.StatusCode != 400 || calls.Load() != before {
			t.Fatalf("relay query scope bypass: %s %d %s", q, got.StatusCode, got.Body)
		}
	}
	before := calls.Load()
	got = invoke("POST", "/api/hermes/rpc", `{"method":"subagent.tail","params":{"session_id":"live-A","subagent_id":"child-A"}}`)
	if got.StatusCode != 400 || calls.Load() != before {
		t.Fatalf("relay raw lane bypass: %d %s", got.StatusCode, got.Body)
	}
}
