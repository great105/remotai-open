package hermes

import (
	"context"
	"encoding/json"
	"testing"
)

func TestControlSchemaRejectsCaseAliasesAndNullScalars(t *testing.T) {
	f := newFixture(t)
	m := f.manager(t, true)
	if err := m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{`{"client_request_id":"case","session_id":"fixture-session","text":"fixture","Queued":false}`, `{"client_request_id":"null","session_id":"fixture-session","text":"fixture","queued":null}`} {
		if _, err := m.SubmitTask(context.Background(), json.RawMessage(raw)); err == nil {
			t.Error("non-schema field accepted")
		}
	}
}
func TestControlSecretReplyValidatesValueWithoutConsuming(t *testing.T) {
	f := newFixture(t)
	m := f.manager(t, true)
	if err := m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := m.RPC(context.Background(), "ping", nil); err != nil {
		t.Fatal(err)
	}
	m.addEvent([]byte(`{"id":"secret-shape","method":"secret","params":{"session_id":"fixture-session"}}`))
	var s struct{ Attention []AttentionRecord }
	json.Unmarshal(m.ControlSnapshot(), &s)
	raw, _ := json.Marshal(map[string]any{"id": s.Attention[0].ID, "result": map[string]any{"value": map[string]string{"password": "fixture"}}})
	if m.ControlReply(context.Background(), raw) == nil {
		t.Fatal("malformed secret value consumed")
	}
	raw, _ = json.Marshal(map[string]any{"id": s.Attention[0].ID, "result": map[string]string{"value": "fixture"}})
	if err := m.ControlReply(context.Background(), raw); err != nil {
		t.Fatal(err)
	}
}
