package hermes

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"
)

type controlRuntime interface {
	SubmitTask(context.Context, json.RawMessage) (json.RawMessage, error)
	ControlSnapshot() json.RawMessage
}

func control(t *testing.T, m *Manager) controlRuntime {
	t.Helper()
	c, ok := any(m).(controlRuntime)
	if !ok {
		t.Fatal("durable admission unavailable")
	}
	return c
}
func TestControlAdmissionRejectsStaleGeneration(t *testing.T) {
	f := newFixture(t)
	m := f.manager(t, true)
	if err := m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]any{"client_request_id": "stale", "session_id": "fixture-session", "stored_session_id": "stored-fixture", "text": "fixture only", "generation": m.Status().BackendGeneration + 1})
	if _, err := m.SubmitTask(context.Background(), raw); err == nil {
		t.Fatal("stale-generation task admitted")
	}
	var s struct{ Tasks []TaskRecord }
	json.Unmarshal(m.ControlSnapshot(), &s)
	if len(s.Tasks) != 0 {
		t.Fatal("stale task persisted")
	}
}
func TestControlAdmissionDuplicateAndRestart(t *testing.T) {
	f := newFixture(t)
	m := f.manager(t, true)
	if err := m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	c := control(t, m)
	if _, err := m.RPC(context.Background(), "session.create", nil); err != nil {
		t.Fatal(err)
	}
	req := json.RawMessage(`{"client_request_id":"test-1","session_id":"fixture-session","stored_session_id":"stored-fixture","text":"fixture only"}`)
	var wg sync.WaitGroup
	results := make(chan string, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			raw, err := c.SubmitTask(context.Background(), req)
			if err != nil {
				results <- err.Error()
				return
			}
			var r map[string]any
			_ = json.Unmarshal(raw, &r)
			results <- r["run_id"].(string)
		}()
	}
	wg.Wait()
	close(results)
	first := ""
	for id := range results {
		if first == "" {
			first = id
		}
		if id != first {
			t.Fatalf("duplicate run %q != %q", id, first)
		}
	}
	if err := m.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	reopened, err := New(m.opts)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close(context.Background())
	var state struct {
		Tasks []struct {
			RunID string `json:"run_id"`
			State string `json:"state"`
		} `json:"tasks"`
	}
	_ = json.Unmarshal(control(t, reopened).ControlSnapshot(), &state)
	if len(state.Tasks) != 1 || state.Tasks[0].RunID != first || state.Tasks[0].State != "interrupted" {
		t.Fatalf("crash uncertainty lost: %s", control(t, reopened).ControlSnapshot())
	}
	var semantic map[string]any
	json.Unmarshal(req, &semantic)
	semantic["session_id"] = "new-live-session"
	semantic["generation"] = 999
	changed, _ := json.Marshal(semantic)
	if _, err := control(t, reopened).SubmitTask(context.Background(), changed); err != nil {
		t.Fatalf("receipt identity tied to transient session/generation: %v", err)
	}
	raw, err := control(t, reopened).SubmitTask(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	var retry map[string]any
	_ = json.Unmarshal(raw, &retry)
	if retry["run_id"] != first {
		t.Fatal("restart replayed ambiguous task")
	}
}

func TestControlAttentionAtomicReplyAndStaleRestart(t *testing.T) {
	f := newFixture(t)
	m := f.manager(t, true)
	if err := m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, err := m.RPC(context.Background(), "fixture.approval", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	var state struct {
		Attention []struct {
			ID    string `json:"id"`
			State string `json:"state"`
		} `json:"attention"`
	}
	_ = json.Unmarshal(control(t, m).ControlSnapshot(), &state)
	if len(state.Attention) != 1 || state.Attention[0].State != "pending" {
		t.Fatalf("pending request not retained: %s", control(t, m).ControlSnapshot())
	}
	replier, ok := any(m).(interface {
		ControlReply(context.Context, json.RawMessage) error
	})
	if !ok {
		t.Fatal("pinned reply unavailable")
	}
	raw, _ := json.Marshal(map[string]any{"id": state.Attention[0].ID, "result": map[string]string{"choice": "once"}})
	var wg sync.WaitGroup
	success := make(chan bool, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); success <- replier.ControlReply(context.Background(), raw) == nil }()
	}
	wg.Wait()
	close(success)
	n := 0
	for ok := range success {
		if ok {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("consumed %d times", n)
	}
	select {
	case <-f.replies:
	case <-time.After(time.Second):
		t.Fatal("native reply missing")
	}
	select {
	case <-f.replies:
		t.Fatal("duplicate native reply")
	case <-time.After(20 * time.Millisecond):
	}
	if err := m.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	reopened, err := New(m.opts)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close(context.Background())
	if err := any(reopened).(interface {
		ControlReply(context.Context, json.RawMessage) error
	}).ControlReply(context.Background(), raw); err == nil {
		t.Fatal("stale reply accepted")
	}
}
