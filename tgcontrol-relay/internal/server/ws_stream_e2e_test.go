package server

import (
	"bytes"
	"encoding/json"
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

// TestStreamTunnelEndToEnd exercises the full reverse-stream path:
//
//	client --WS--> relay (/client/{id}/stream) --StreamOpen--> agent control WS
//	agent --WS--> relay (/agent/stream) --paired--> relay pumps bytes both ways
//
// A fake agent echoes whatever arrives on its stream connection, so a byte sent
// by the client must come back through the relay unchanged.
func TestStreamTunnelEndToEnd(t *testing.T) {
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

	deviceJWT, initData := pairTestDevice(t, ts, botToken, "dev-stream", 4242)
	wsBase := "ws" + strings.TrimPrefix(ts.URL, "http")

	// ── Fake agent: connect control WS, handshake, react to stream_open. ──
	hdr := http.Header{}
	hdr.Set("Authorization", "Bearer "+deviceJWT)
	agentCtl, _, err := websocket.DefaultDialer.Dial(wsBase+"/v1/agent/connect", hdr)
	if err != nil {
		t.Fatalf("agent control dial: %v", err)
	}
	defer agentCtl.Close()

	if err := agentCtl.WriteJSON(protocol.Hello{
		Type: protocol.MsgHello, ProtocolVersion: protocol.ProtocolVersion,
		DeviceID: "dev-stream", JWT: deviceJWT, Platform: "linux", Hostname: "h",
	}); err != nil {
		t.Fatalf("agent hello: %v", err)
	}
	var welcome protocol.Welcome
	if err := agentCtl.ReadJSON(&welcome); err != nil || welcome.Type != protocol.MsgWelcome {
		t.Fatalf("agent welcome: %v (%+v)", err, welcome)
	}

	// Agent reader: on stream_open, dial back and echo the stream.
	go func() {
		for {
			var raw json.RawMessage
			if err := agentCtl.ReadJSON(&raw); err != nil {
				return
			}
			var head struct {
				Type string `json:"type"`
			}
			if json.Unmarshal(raw, &head) != nil {
				continue
			}
			if head.Type != protocol.MsgStreamOpen {
				continue
			}
			var so protocol.StreamOpen
			if json.Unmarshal(raw, &so) != nil {
				continue
			}
			go func() {
				h := http.Header{}
				h.Set("Authorization", "Bearer "+deviceJWT)
				sc, _, err := websocket.DefaultDialer.Dial(
					wsBase+"/v1/agent/stream?token="+url.QueryEscape(so.Token), h)
				if err != nil {
					return
				}
				defer sc.Close()
				for { // echo loop = stands in for the local /ws endpoint
					mt, data, err := sc.ReadMessage()
					if err != nil {
						return
					}
					if err := sc.WriteMessage(mt, data); err != nil {
						return
					}
				}
			}()
		}
	}()

	// ── Client: open a stream and round-trip a payload. ──
	streamURL := wsBase + "/v1/client/dev-stream/stream?initData=" + url.QueryEscape(initData) +
		"&path=" + url.QueryEscape("/ws/pty/test")
	client, resp, err := websocket.DefaultDialer.Dial(streamURL, nil)
	if err != nil {
		code := 0
		if resp != nil {
			code = resp.StatusCode
		}
		t.Fatalf("client stream dial: %v (HTTP %d)", err, code)
	}
	defer client.Close()

	payload := []byte("hello-stream-42")
	if err := client.WriteMessage(websocket.BinaryMessage, payload); err != nil {
		t.Fatalf("client write: %v", err)
	}
	client.SetReadDeadline(time.Now().Add(5 * time.Second))
	mt, got, err := client.ReadMessage()
	if err != nil {
		t.Fatalf("client read echo: %v", err)
	}
	if mt != websocket.BinaryMessage || !bytes.Equal(got, payload) {
		t.Fatalf("echo mismatch: mt=%d got=%q want=%q", mt, got, payload)
	}

	// A text frame must round-trip with its type preserved too.
	if err := client.WriteMessage(websocket.TextMessage, []byte(`{"t":"resize"}`)); err != nil {
		t.Fatalf("client write text: %v", err)
	}
	client.SetReadDeadline(time.Now().Add(5 * time.Second))
	mt2, got2, err := client.ReadMessage()
	if err != nil {
		t.Fatalf("client read text echo: %v", err)
	}
	if mt2 != websocket.TextMessage || string(got2) != `{"t":"resize"}` {
		t.Fatalf("text echo mismatch: mt=%d got=%q", mt2, got2)
	}
}

// TestStreamRejectsBadPath ensures the relay refuses non-/ws/ stream paths.
func TestStreamRejectsBadPath(t *testing.T) {
	for _, p := range []string{"/api/secret", "/ws/../etc", "ws://evil/x", ""} {
		if validStreamPath(p) {
			t.Errorf("validStreamPath(%q) = true, want false", p)
		}
	}
	for _, p := range []string{"/ws/pty/abc", "/ws/screen"} {
		if !validStreamPath(p) {
			t.Errorf("validStreamPath(%q) = false, want true", p)
		}
	}
}

// pairTestDevice runs the pairing flow and returns (deviceJWT, userInitData).
func pairTestDevice(t *testing.T, ts *httptest.Server, botToken, deviceID string, uid int64) (string, string) {
	t.Helper()
	body := bytes.NewReader([]byte(`{"device_id":"` + deviceID + `","hostname":"h","platform":"linux","agent_version":"e2e"}`))
	resp, err := http.Post(ts.URL+"/v1/pair/request", "application/json", body)
	if err != nil {
		t.Fatalf("pair/request: %v", err)
	}
	var pr struct {
		Code string `json:"code"`
	}
	json.NewDecoder(resp.Body).Decode(&pr)
	resp.Body.Close()

	initData := buildInitData(t, botToken, uid, "streamer")
	req, _ := http.NewRequest("POST", ts.URL+"/v1/pair/confirm",
		bytes.NewReader([]byte(`{"code":"`+pr.Code+`"}`)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "tma "+initData)
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("pair/confirm: %v", err)
	}
	var cr struct {
		JWT string `json:"jwt"`
	}
	json.NewDecoder(resp2.Body).Decode(&cr)
	resp2.Body.Close()
	if cr.JWT == "" {
		t.Fatalf("no device JWT from confirm")
	}
	return cr.JWT, initData
}
