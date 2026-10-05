package hermes

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestAdmissionKeepsSingleNextTaskAndItsIdempotentReceipt(t *testing.T) {
	f := newFixture(t)
	m := f.manager(t, true)
	if err := m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := m.RPC(context.Background(), "session.create", nil); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(m.checkout, "apps", "shared", "src")
	os.MkdirAll(dir, 0700)
	os.WriteFile(filepath.Join(dir, "gateway-contract.openrpc.json"), []byte(`{"methods":[{"name":"prompt.submit"},{"name":"gateway.capabilities"}]}`), 0600)
	req := func(id string, queued bool) json.RawMessage {
		raw, _ := json.Marshal(map[string]any{"client_request_id": id, "session_id": "fixture-session", "stored_session_id": "stored-fixture", "text": id, "queued": queued})
		return raw
	}
	first, err := m.SubmitTask(context.Background(), req("active", false))
	if err != nil {
		t.Fatal(err)
	}
	next, err := m.SubmitTask(context.Background(), req("next", true))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.SubmitTask(context.Background(), req("excess", true)); err == nil {
		t.Fatal("second queued input admitted and can merge upstream")
	} else if rejection, ok := err.(interface{ AdmissionRejected() bool }); !ok || !rejection.AdmissionRejected() {
		t.Fatalf("definitive preadmission refusal is untyped: %T", err)
	}
	retry, err := m.SubmitTask(context.Background(), req("next", true))
	if err != nil {
		t.Fatal(err)
	}
	var a, b map[string]any
	json.Unmarshal(next, &a)
	json.Unmarshal(retry, &b)
	if a["run_id"] != b["run_id"] {
		t.Fatal("queue retry created another native input")
	}
	var conflicting map[string]any
	json.Unmarshal(req("next", true), &conflicting)
	conflicting["text"] = "different content for existing ID"
	conflict, _ := json.Marshal(conflicting)
	if _, err := m.SubmitTask(context.Background(), conflict); err == nil {
		t.Fatal("fingerprint conflict admitted")
	} else if _, definitive := err.(interface{ AdmissionRejected() bool }); definitive {
		t.Fatal("existing-ID conflict can release another content identity")
	}
	_ = first
	deadline := time.Now().Add(time.Second)
	for {
		f.mu.Lock()
		texts := append([]string(nil), f.promptTexts...)
		f.mu.Unlock()
		if len(texts) == 2 {
			if texts[0] != "active" || texts[1] != "next" {
				t.Fatalf("wire order differs from admission: %v", texts)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("expected exactly two socket writes, got %v", texts)
		}
		time.Sleep(time.Millisecond)
	}
	m.control.mu.Lock()
	if len(m.control.Tasks) != 2 {
		t.Fatal("declined task persisted")
	}
	m.control.mu.Unlock()
}

func TestBoundedTurnCorrelationStartsActiveBeforeQueuedAndClosesBoth(t *testing.T) {
	m := regressionManager(t)
	m.control.Tasks = []TaskRecord{{ClientRequestID: "active", RunID: "active", SessionID: "s", Generation: 7, State: "accepted"}, {ClientRequestID: "next", RunID: "next", SessionID: "s", Generation: 7, State: "accepted", Queued: true}}
	for i, kind := range []string{"message.start", "message.complete", "message.start", "message.complete"} {
		raw, _ := json.Marshal(map[string]any{"method": "event", "params": map[string]any{"type": kind, "session_id": "s", "payload": map[string]any{"status": "completed"}}})
		m.observeControl(raw, uint64(i+1), 7)
		if i == 0 && (m.control.Tasks[0].State != "running" || m.control.Tasks[1].State != "accepted") {
			t.Fatalf("start assigned to queue: %+v", m.control.Tasks)
		}
	}
	for _, task := range m.control.Tasks {
		if task.State != "completed" || task.NativeTurnSeq == 0 {
			t.Fatalf("bounded queue did not complete correlated native turns: %+v", task)
		}
	}
}
