package hermes

import (
	"context"
	"encoding/json"
	"testing"
)

func TestControlRejectsUnknownRequestKeys(t *testing.T) {
	f := newFixture(t)
	m := f.manager(t, true)
	if err := m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		call func() error
	}{
		{"submit", func() error {
			_, err := m.SubmitTask(context.Background(), json.RawMessage(`{"client_request_id":"schema","session_id":"fixture-session","text":"fixture","profile":"foreign"}`))
			return err
		}},
		{"reply", func() error {
			m.addEvent([]byte(`{"id":"schema-secret","method":"secret","params":{"session_id":"fixture-session"}}`))
			var s struct{ Attention []AttentionRecord }
			json.Unmarshal(m.ControlSnapshot(), &s)
			raw, _ := json.Marshal(map[string]any{"id": s.Attention[0].ID, "result": map[string]string{"value": "fixture"}, "session_id": "foreign"})
			return m.ControlReply(context.Background(), raw)
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.call() == nil {
				t.Fatal("unknown request key accepted")
			}
		})
	}
}

func TestControlReplyRejectsUnknownApprovalResultBeforeConsumption(t *testing.T) {
	f := newFixture(t)
	m := f.manager(t, true)
	if err := m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := m.RPC(context.Background(), "ping", nil); err != nil {
		t.Fatal(err)
	}
	m.addEvent([]byte(`{"id":"schema-approval","method":"approval","params":{"session_id":"fixture-session","choices":["once","deny"]}}`))
	var s struct{ Attention []AttentionRecord }
	json.Unmarshal(m.ControlSnapshot(), &s)
	raw, _ := json.Marshal(map[string]any{"id": s.Attention[0].ID, "result": map[string]any{"choice": "once", "profile": "foreign"}})
	if m.ControlReply(context.Background(), raw) == nil {
		t.Fatal("unknown result key accepted")
	}
	raw, _ = json.Marshal(map[string]any{"id": s.Attention[0].ID, "result": map[string]string{"choice": "once"}})
	if err := m.ControlReply(context.Background(), raw); err != nil {
		t.Fatal(err)
	}
}
