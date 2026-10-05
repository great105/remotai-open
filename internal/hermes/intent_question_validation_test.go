package hermes

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestControlIntentAndReadinessRejectOverrides(t *testing.T) {
	f := newFixture(t)
	m := f.manager(t, true)
	if err := m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(m.checkout, "apps", "shared", "src")
	os.MkdirAll(dir, 0700)
	os.WriteFile(filepath.Join(dir, "gateway-contract.openrpc.json"), []byte(`{"methods":[{"name":"session.steer"},{"name":"tools.show"}]}`), 0600)
	if _, err := m.ControlIntent(context.Background(), json.RawMessage(`{"intent":"steer","session_id":"fixture-session","text":"fixture","profile":"other"}`)); err == nil {
		t.Error("intent ignored profile override")
	}
	if _, err := m.ControlReadiness(context.Background(), json.RawMessage(`{"session_id":"fixture-session","profile":"other"}`)); err == nil {
		t.Error("readiness ignored profile override")
	}
}
func TestControlClarifyValidatesNativeAnswers(t *testing.T) {
	f := newFixture(t)
	m := f.manager(t, true)
	if err := m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := m.RPC(context.Background(), "ping", nil); err != nil {
		t.Fatal(err)
	}
	m.addEvent([]byte(`{"id":"clarify-schema","method":"clarify","params":{"session_id":"fixture-session","questions":[{"qid":"one","question":"fixture","choices":["a","b"],"multi_select":true}]}}`))
	var s struct{ Attention []AttentionRecord }
	json.Unmarshal(m.ControlSnapshot(), &s)
	raw, _ := json.Marshal(map[string]any{"id": s.Attention[0].ID, "result": map[string]any{"answers": map[string]any{"foreign": "c"}}})
	if m.ControlReply(context.Background(), raw) == nil {
		t.Fatal("unknown question consumed native clarify")
	}
	raw, _ = json.Marshal(map[string]any{"id": s.Attention[0].ID, "result": map[string]any{"answers": map[string]any{"one": "a, b"}}})
	if err := m.ControlReply(context.Background(), raw); err != nil {
		t.Fatal(err)
	}
}
