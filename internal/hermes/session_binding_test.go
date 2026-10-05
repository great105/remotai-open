package hermes

import (
	"context"
	"encoding/json"
	"testing"
)

func TestControlAdmissionRequiresLiveNativeSessionBinding(t *testing.T) {
	f := newFixture(t)
	m := f.manager(t, true)
	if err := m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	m.observeSessionSnapshot(json.RawMessage(`{"session_id":"fixture-session","stored_session_id":"stored-fixture","info":{"cwd":""}}`))
	for _, raw := range []string{`{"client_request_id":"wrong-durable","session_id":"fixture-session","stored_session_id":"foreign-store","text":"fixture"}`, `{"client_request_id":"unknown-live","session_id":"foreign-live","stored_session_id":"stored-fixture","text":"fixture"}`} {
		if _, err := m.SubmitTask(context.Background(), json.RawMessage(raw)); err == nil {
			t.Error("unbound session admitted")
		}
	}
	var s struct{ Tasks []TaskRecord }
	json.Unmarshal(m.ControlSnapshot(), &s)
	if len(s.Tasks) != 0 {
		t.Error("invalid session persisted")
	}
}
func TestControlRetiredBridgeCannotHydrateSessionBinding(t *testing.T) {
	f := newFixture(t)
	m := f.manager(t, true)
	if err := m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := m.RPC(context.Background(), "ping", nil); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	old := m.bridge
	m.bridge = nil
	m.epoch++
	m.mu.Unlock()
	defer old.close()
	_, err := old.call(context.Background(), "session.create", nil)
	if err != nil {
		t.Fatal(err)
	}
	m.control.mu.Lock()
	count := len(m.control.Sessions)
	m.control.mu.Unlock()
	if count != 0 {
		t.Fatal("retired socket hydrated current session authority")
	}
}
