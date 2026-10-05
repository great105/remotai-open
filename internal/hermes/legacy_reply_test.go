package hermes

import (
	"context"
	"encoding/json"
	"testing"
)

func TestLegacyReplyCannotAnswerReusedNativeID(t *testing.T) {
	f := newFixture(t)
	m := f.manager(t, true)
	if err := m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := m.RPC(context.Background(), "ping", nil); err != nil {
		t.Fatal(err)
	}
	m.addEvent([]byte(`{"id":"reused","method":"approval","params":{"session_id":"old","choices":["once","deny"]}}`))
	m.mu.Lock()
	m.epoch++
	m.mu.Unlock()
	m.ControlSnapshot()
	m.addEvent([]byte(`{"id":"reused","method":"approval","params":{"session_id":"new","choices":["once","deny"]}}`))
	if err := m.Reply(context.Background(), "reused", json.RawMessage(`{"choice":"once"}`)); err == nil {
		t.Fatal("stale legacy reply consumed replacement request")
	}
	var s struct{ Attention []AttentionRecord }
	json.Unmarshal(m.ControlSnapshot(), &s)
	raw, _ := json.Marshal(map[string]any{"id": s.Attention[1].ID, "result": map[string]string{"choice": "once"}})
	if err := m.ControlReply(context.Background(), raw); err != nil {
		t.Fatal(err)
	}
}
