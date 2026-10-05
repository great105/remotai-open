package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"tgcontrol-relay/internal/db"
	"tgcontrol-relay/internal/notify"
)

func serverAccessToken(t *testing.T, s *Server) (string, int64) {
	t.Helper()
	var uid int64
	if err := s.DB.QueryRow(`SELECT user_id FROM devices WHERE id='dev-gate'`).Scan(&uid); err != nil {
		t.Fatal(err)
	}
	tok, _, err := s.JWT.IssueDevice("dev-gate", uid)
	if err != nil {
		t.Fatal(err)
	}
	return tok, uid
}

func accessRequest(t *testing.T, base, token, method string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(method, base+"/v1/agent/server-access", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, out
}

func TestServerAccessReadDoesNotStartTrialAndPostSharesCloudTrial(t *testing.T) {
	ts, s, _ := gateTestServer(t, true)
	tok, uid := serverAccessToken(t, s)
	status, out := accessRequest(t, ts.URL, tok, http.MethodGet)
	if status != 200 || out["allowed"] != false || out["trial_available"] != true {
		t.Fatalf("GET: %d %v", status, out)
	}
	u, err := db.GetUserByID(t.Context(), s.DB, uid)
	if err != nil || u.TrialEnd.Valid {
		t.Fatalf("GET spent trial: %v %v", u, err)
	}
	status, out = accessRequest(t, ts.URL, tok, http.MethodPost)
	if status != 200 || out["allowed"] != true {
		t.Fatalf("POST: %d %v", status, out)
	}
	u, _ = db.GetUserByID(t.Context(), s.DB, uid)
	if days, ok := u.TrialDaysLeft(); !ok || days < 29 {
		t.Fatalf("trial days=%d present=%v", days, ok)
	}
	end := u.TrialEnd.Time
	w := httptest.NewRecorder()
	if !s.cloudGate(w, httptest.NewRequest("GET", "/", nil), uid) {
		t.Fatalf("same trial denied cloud: %s", w.Body.String())
	}
	u, _ = db.GetUserByID(t.Context(), s.DB, uid)
	if !u.TrialEnd.Time.Equal(end) {
		t.Fatal("cloud restarted the server trial")
	}
	if _, err := s.DB.Exec(`UPDATE users SET trial_end=datetime('now','-1 day') WHERE id=?`, uid); err != nil {
		t.Fatal(err)
	}
	_, out = accessRequest(t, ts.URL, tok, http.MethodPost)
	if out["allowed"] != false || out["trial_available"] != false || out["code"] != "server_subscription_required" {
		t.Fatalf("expired trial renewed or beta bypassed: %v", out)
	}
}

func TestServerAccessUsesAccountEntitlement(t *testing.T) {
	for _, tc := range []struct {
		name, sql string
		allowed   bool
	}{
		{"founder", `UPDATE users SET founder=1`, true},
		{"admin grant", `UPDATE users SET tier='pro'`, true},
		{"paid", `INSERT INTO subscriptions(user_id,tier,current_period_end) SELECT id,'pro',datetime('now','+30 days') FROM users`, true},
		{"fleet", `INSERT INTO subscriptions(user_id,tier,current_period_end) SELECT id,'fleet',datetime('now','+30 days') FROM users`, true},
		{"expired paid", `INSERT INTO subscriptions(user_id,tier,current_period_end) SELECT id,'pro',datetime('now','-1 day') FROM users`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts, s, _ := gateTestServer(t, true)
			tok, uid := serverAccessToken(t, s)
			if _, err := s.DB.Exec(tc.sql); err != nil {
				t.Fatal(err)
			}
			_, out := accessRequest(t, ts.URL, tok, http.MethodGet)
			if out["allowed"] != tc.allowed {
				t.Fatalf("access=%v", out)
			}
			if tc.allowed {
				accessRequest(t, ts.URL, tok, http.MethodPost)
				w := httptest.NewRecorder()
				if !s.cloudGate(w, httptest.NewRequest("GET", "/", nil), uid) {
					t.Fatalf("paid cloud denied: %s", w.Body.String())
				}
				u, _ := db.GetUserByID(t.Context(), s.DB, uid)
				if u.TrialEnd.Valid {
					t.Fatal("paid or founder access consumed trial")
				}
			}
		})
	}
}

func TestServerAccessRejectsInvalidRevokedAndTransferredDevices(t *testing.T) {
	ts, s, _ := gateTestServer(t, true)
	tok, _ := serverAccessToken(t, s)
	for _, bad := range []string{"", "invalid"} {
		if status, _ := accessRequest(t, ts.URL, bad, "POST"); status != 401 {
			t.Fatalf("invalid token: %d", status)
		}
	}
	if _, err := s.DB.Exec(`UPDATE devices SET revoked_at=CURRENT_TIMESTAMP WHERE id='dev-gate'`); err != nil {
		t.Fatal(err)
	}
	if status, _ := accessRequest(t, ts.URL, tok, "POST"); status != 401 {
		t.Fatalf("revoked token: %d", status)
	}
	foreign, _, _ := s.JWT.IssueDevice("dev-gate", 9999)
	if status, _ := accessRequest(t, ts.URL, foreign, "POST"); status != 401 {
		t.Fatalf("foreign token: %d", status)
	}
}

func TestExpiredAccountCannotUseCloudPeerTurnOrBot(t *testing.T) {
	ts, s, initData := gateTestServer(t, true)
	tok, uid := serverAccessToken(t, s)
	pairTestDevice(t, ts, "1234:ABCDEF", "dev-target", 7777)
	if _, err := s.DB.Exec(`UPDATE users SET trial_end=datetime('now','-1 day') WHERE id=?`, uid); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ path, method, auth string }{
		{"/v1/client/dev-gate/request", "POST", "tma " + initData},
		{"/v1/agent/peer/dev-target/request", "POST", "Bearer " + tok},
		{"/v1/turn/credentials", "GET", "Bearer " + tok},
	} {
		req, _ := http.NewRequest(tc.method, ts.URL+tc.path, nil)
		req.Header.Set("Authorization", tc.auth)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 402 {
			t.Errorf("%s: got %d want 402", tc.path, resp.StatusCode)
		}
	}
	if err := s.SendPtyInput(t.Context(), notify.InputRequest{UserID: uid, DeviceID: "dev-gate", PtyID: "test", Key: "enter"}); !errors.Is(err, notify.ErrSubscriptionRequired) {
		t.Fatalf("bot input bypassed subscription: %v", err)
	}
}

func TestCloudAccessWatchClosesExpiredViewer(t *testing.T) {
	_, s, _ := gateTestServer(t, true)
	_, uid := serverAccessToken(t, s)
	if _, err := s.DB.Exec(`UPDATE users SET trial_end=datetime('now','-1 day') WHERE id=?`, uid); err != nil {
		t.Fatal(err)
	}
	ws := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := clientUpgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		s.watchCloudAccess(r.Context(), conn, uid, time.Millisecond)
	}))
	defer ws.Close()
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(ws.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetReadDeadline(time.Now().Add(time.Second))
	_, _, err = conn.ReadMessage()
	if !websocket.IsCloseError(err, websocket.ClosePolicyViolation) {
		t.Fatalf("viewer remains usable: %v", err)
	}
}

func TestSharedDeviceUsesOwnerSubscription(t *testing.T) {
	s, ts := newInfrastructureTestServer(t)
	s.UserSendFile = func(context.Context, int64, string, io.Reader) error {
		t.Error("unexpected file delivery without an agent")
		return nil
	}
	owner, _ := issueTestUser(t, s, 15001, "paid-owner")
	member, token := issueTestUser(t, s, 15002, "operator")
	_, outsiderToken := issueTestUser(t, s, 15003, "outsider")
	workspace, err := db.CreateCompanyWorkspace(t.Context(), s.DB, owner.ID, "Paid fleet")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec(`INSERT INTO workspace_members(workspace_id,user_id,role,added_by) VALUES(?,?,?,?)`, workspace.ID, member.ID, db.RoleOperator, owner.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.InsertDevice(t.Context(), s.DB, &db.Device{
		ID: "shared-paid", UserID: owner.ID, WorkspaceID: workspace.ID,
		Name: "Shared server", Platform: "linux", DeviceType: db.DeviceServer,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec(`INSERT INTO subscriptions(user_id,tier,current_period_end) VALUES(?,'fleet',datetime('now','+30 days'))`, owner.ID); err != nil {
		t.Fatal(err)
	}
	proxyURL := ts.URL + "/v1/client/shared-paid/request"
	turnURL := ts.URL + "/v1/turn/credentials?device_id=shared-paid"
	streamURL := ts.URL + "/v1/client/shared-paid/stream?path=/ws/pty/test"
	assertControlStatus := func(want int) {
		t.Helper()
		headers := http.Header{"Authorization": {"Bearer " + token}, "X-Remotai-Client": {"android"}}
		conn, resp, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(ts.URL, "http")+"/v1/client/shared-paid/ws", headers)
		if conn != nil {
			defer conn.Close()
		}
		if resp != nil && resp.Body != nil {
			defer resp.Body.Close()
		}
		if resp == nil || resp.StatusCode != want {
			t.Fatalf("control WS: response=%v err=%v want=%d", resp, err, want)
		}
		if want == http.StatusSwitchingProtocols && err != nil {
			t.Fatalf("control WS: %v", err)
		}
	}
	// No agent is connected: these paths must reach the offline check instead
	// of requiring a second payment or consuming the member's personal trial.
	status, body := requestJSON(t, "POST", proxyURL, token, map[string]any{"method": "GET", "path": "/api/system/stats"})
	if status != 502 || body["code"] != "pc_offline" {
		t.Fatalf("shared HTTP: %d %v", status, body)
	}
	status, body = requestJSON(t, "POST", ts.URL+"/v1/files/send", token, map[string]any{"device_id": "shared-paid", "path": "/test.txt"})
	if status != 502 || body["code"] != "pc_offline" {
		t.Fatalf("shared file delivery: %d %v", status, body)
	}
	if status, _ := requestJSON(t, "GET", streamURL, token, nil); status != 502 {
		t.Fatalf("shared stream: %d", status)
	}
	assertControlStatus(http.StatusSwitchingProtocols)
	if status, _ := requestJSON(t, "GET", turnURL, token, nil); status != 200 {
		t.Fatalf("shared TURN: %d", status)
	}
	if err := s.SendPtyInput(t.Context(), notify.InputRequest{UserID: member.ID, DeviceID: "shared-paid", PtyID: "test", Key: "enter"}); !errors.Is(err, notify.ErrOffline) {
		t.Fatalf("shared bot input: %v", err)
	}
	for _, uid := range []int64{owner.ID, member.ID} {
		u, err := db.GetUserByID(t.Context(), s.DB, uid)
		if err != nil || u.TrialEnd.Valid {
			t.Fatalf("shared access consumed trial for %d: %v %v", uid, u, err)
		}
	}
	if status, _ := requestJSON(t, "GET", turnURL, outsiderToken, nil); status != 403 {
		t.Fatalf("outsider used owner's TURN plan: %d", status)
	}
	// A paid member does not replace an expired device owner's entitlement.
	if _, err := s.DB.Exec(`UPDATE subscriptions SET current_period_end=datetime('now','-1 day') WHERE user_id=?`, owner.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec(`UPDATE users SET trial_end=datetime('now','-1 day') WHERE id=?`, owner.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec(`UPDATE users SET tier='pro' WHERE id=?`, member.ID); err != nil {
		t.Fatal(err)
	}
	status, _ = requestJSON(t, "POST", proxyURL, token, map[string]any{"method": "GET", "path": "/api/system/stats"})
	if status != 402 {
		t.Fatalf("expired owner's shared HTTP: %d", status)
	}
	status, _ = requestJSON(t, "POST", ts.URL+"/v1/files/send", token, map[string]any{"device_id": "shared-paid", "path": "/test.txt"})
	if status != 402 {
		t.Fatalf("expired owner's shared file delivery: %d", status)
	}
	for _, endpoint := range []string{streamURL, turnURL} {
		if status, _ := requestJSON(t, "GET", endpoint, token, nil); status != 402 {
			t.Fatalf("expired owner's %s: %d", endpoint, status)
		}
	}
	assertControlStatus(http.StatusPaymentRequired)
	if err := s.SendPtyInput(t.Context(), notify.InputRequest{UserID: member.ID, DeviceID: "shared-paid", PtyID: "test", Key: "enter"}); !errors.Is(err, notify.ErrSubscriptionRequired) {
		t.Fatalf("expired owner's shared bot input: %v", err)
	}
}

func TestCloudGateFailsClosedWhenDatabaseUnavailable(t *testing.T) {
	_, s, _ := gateTestServer(t, true)
	_, uid := serverAccessToken(t, s)
	s.DB.Close()
	w := httptest.NewRecorder()
	if s.cloudGate(w, httptest.NewRequest("GET", "/", nil).WithContext(context.Background()), uid) || w.Code != 503 {
		t.Fatalf("failed open: %d", w.Code)
	}
}

func TestExpiredAccountCannotSendThroughTelegram(t *testing.T) {
	ts, s, _ := gateTestServer(t, true)
	tok, uid := serverAccessToken(t, s)
	if _, err := s.DB.Exec(`UPDATE users SET trial_end=datetime('now','-1 day') WHERE id=?`, uid); err != nil {
		t.Fatal(err)
	}
	s.UserSendText = func(context.Context, int64, string, string) error {
		t.Error("unpaid Telegram delivery ran")
		return nil
	}
	req, _ := http.NewRequest("POST", ts.URL+"/v1/agent/send", strings.NewReader(`{"text":"test"}`))
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 402 {
		t.Fatalf("unpaid send: %d", resp.StatusCode)
	}
}

func TestExpiredAccountReceivesNoAgentNotifications(t *testing.T) {
	e := newNotifyEnv(t)
	if _, err := e.srv.DB.Exec(`UPDATE users SET trial_end=datetime('now','-1 day')`); err != nil {
		t.Fatal(err)
	}
	ag := e.dialAgent(t, http.StatusOK, `{"ok":true}`)
	ag.sendPtyEvent(t, waitingPayload("pty-expired", "Подтвердите: y/n"))
	select {
	case <-e.notices:
		t.Fatal("unpaid cloud notification sent")
	case <-time.After(250 * time.Millisecond):
	}
}
