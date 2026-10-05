package hermes

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestControlApprovalCannotEscalateNativeChoices(t *testing.T) {
	f := newFixture(t)
	m := f.manager(t, true)
	if err := m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := m.RPC(context.Background(), "ping", nil); err != nil {
		t.Fatal(err)
	}
	m.addEvent([]byte(`{"id":"native-approval","method":"approval","params":{"session_id":"fixture-session","request_id":"native-approval","command":"fixture command","choices":["deny","once"],"allow_session":false,"allow_permanent":false}}`))
	var s struct{ Attention []AttentionRecord }
	json.Unmarshal(m.ControlSnapshot(), &s)
	id := s.Attention[0].ID
	raw, _ := json.Marshal(map[string]any{"id": id, "result": map[string]string{"choice": "always"}})
	if m.ControlReply(context.Background(), raw) == nil {
		t.Fatal("approval escalated beyond native choices")
	}
	raw, _ = json.Marshal(map[string]any{"id": id, "result": map[string]string{"choice": "once"}})
	if err := m.ControlReply(context.Background(), raw); err != nil {
		t.Fatal(err)
	}
}
func TestControlSecretAnswerNeverPersists(t *testing.T) {
	f := newFixture(t)
	m := f.manager(t, true)
	if err := m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := m.RPC(context.Background(), "ping", nil); err != nil {
		t.Fatal(err)
	}
	m.addEvent([]byte(`{"id":"secret-fixture","method":"secret","params":{"session_id":"fixture-session","prompt":"private prompt sentinel"}}`))
	var s struct{ Attention []AttentionRecord }
	json.Unmarshal(m.ControlSnapshot(), &s)
	if len(s.Attention[0].Params) > 0 {
		t.Fatal("secret display persisted")
	}
	raw, _ := json.Marshal(map[string]any{"id": s.Attention[0].ID, "result": map[string]string{"value": "private answer sentinel"}})
	if err := m.ControlReply(context.Background(), raw); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(m.root, "control.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "private") {
		t.Fatal("secret leaked into durable journal")
	}
}
