package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type controlWebFixture struct {
	hermesWebFixture
	owner  string
	readID string
}

func (f *controlWebFixture) ControlSnapshot() json.RawMessage {
	return json.RawMessage(`{"tasks":[{"run_id":"` + f.owner + `"}],"attention":[],"results":[]}`)
}
func (f *controlWebFixture) SubmitTask(context.Context, json.RawMessage) (json.RawMessage, error) {
	return json.RawMessage(`{"status":"accepted","run_id":"` + f.owner + `"}`), nil
}
func (f *controlWebFixture) ControlCapabilities() json.RawMessage {
	return json.RawMessage(`{"steer":false,"model_quota":"unknown"}`)
}
func (f *controlWebFixture) ControlIntent(context.Context, json.RawMessage) (json.RawMessage, error) {
	return json.RawMessage(`{"status":"queued"}`), nil
}
func (f *controlWebFixture) ControlReply(context.Context, json.RawMessage) error { return nil }
func (f *controlWebFixture) SetAutoStart(enabled bool) error {
	f.status.AutoStart = enabled
	return nil
}
func (f *controlWebFixture) ReadAttention(id string) error { f.readID = id; return nil }
func TestHermesControlReadInboxThroughOwnerAPI(t *testing.T) {
	f := &controlWebFixture{}
	s := &Server{hermesManagers: map[int64]hermesRuntime{1: f}}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/hermes/control/read", strings.NewReader(`{"id":"notice"}`))
	r.SetPathValue("action", "read")
	s.apiHermesControl(w, r, 1)
	if w.Code != 200 || f.readID != "notice" {
		t.Fatalf("read unavailable: %d %s", w.Code, w.Body.String())
	}
}
func (f *controlWebFixture) ControlReadiness(context.Context, json.RawMessage) (json.RawMessage, error) {
	return json.RawMessage(`{"tools":{"status":"ready","total":1}}`), nil
}
func TestHermesControlReadinessRoute(t *testing.T) {
	f := &controlWebFixture{}
	s := &Server{hermesManagers: map[int64]hermesRuntime{1: f}}
	r := httptest.NewRequest("POST", "/api/hermes/control/readiness", strings.NewReader(`{"session_id":"fixture"}`))
	r.SetPathValue("action", "readiness")
	w := httptest.NewRecorder()
	s.apiHermesControl(w, r, 1)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"ready"`) {
		t.Fatalf("readiness unavailable %d %s", w.Code, w.Body.String())
	}
}
func TestHermesControlAutoStartSetting(t *testing.T) {
	f := &controlWebFixture{}
	s := &Server{hermesManagers: map[int64]hermesRuntime{1: f}}
	w := httptest.NewRecorder()
	s.apiHermesSettings(w, httptest.NewRequest("POST", "/api/hermes/settings", strings.NewReader(`{"auto_start":true}`)), 1)
	if w.Code != 200 || !f.Status().AutoStart {
		t.Fatalf("opt-in unavailable: %d %s", w.Code, w.Body.String())
	}
}
func TestHermesControlAPIScopesBackendOwner(t *testing.T) {
	s := &Server{hermesManagers: map[int64]hermesRuntime{1: &controlWebFixture{owner: "one"}, 2: &controlWebFixture{owner: "two"}}}
	api, ok := any(s).(interface {
		apiHermesControl(http.ResponseWriter, *http.Request, int64)
	})
	if !ok {
		t.Fatal("control center API unavailable")
	}
	for _, test := range []struct {
		uid            int64
		action, method string
		status         int
	}{{1, "snapshot", "GET", 200}, {2, "snapshot", "GET", 200}, {2, "submit", "POST", 202}, {2, "intent", "POST", 200}, {2, "reply", "POST", 200}, {2, "unsafe", "POST", 400}} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(test.method, "/api/hermes/control/"+test.action, strings.NewReader(`{"session_id":"fixture"}`))
		r.SetPathValue("action", test.action)
		api.apiHermesControl(w, r, test.uid)
		if w.Code != test.status {
			t.Fatalf("%s: %d %s", test.action, w.Code, w.Body.String())
		}
		if test.action == "snapshot" && !strings.Contains(w.Body.String(), map[int64]string{1: "one", 2: "two"}[test.uid]) {
			t.Fatal("cross-owner journal")
		}
	}
}
