package hermes

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestJournalLimitDefinitiveRefusalHasNoDurableOrWireEffect(t *testing.T) {
	f := newFixture(t)
	m := f.manager(t, true)
	if err := m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := m.RPC(context.Background(), "session.create", nil); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(m.root, "control.json")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	m.control.mu.Lock()
	for i := 0; i < 256; i++ {
		m.control.Tasks = append(m.control.Tasks, TaskRecord{State: "completed"})
	}
	m.control.mu.Unlock()
	req, _ := json.Marshal(map[string]any{"client_request_id": "limit-refused", "session_id": "fixture-session", "text": "SYNTHETIC_NEVER_ADMITTED"})
	_, err = m.SubmitTask(context.Background(), req)
	if refusal, ok := err.(interface{ AdmissionRejected() bool }); !ok || !refusal.AdmissionRejected() {
		t.Fatalf("journal bound not a definitive refusal: %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("refusal wrote journal")
	}
	f.mu.Lock()
	n := len(f.promptTexts)
	f.mu.Unlock()
	if n != 0 {
		t.Fatal("refusal wrote native prompt")
	}
	m.control.mu.Lock()
	defer m.control.mu.Unlock()
	if len(m.control.Tasks) != 256 {
		t.Fatal("refusal changed admission count")
	}
}
