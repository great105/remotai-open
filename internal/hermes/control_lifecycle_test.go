package hermes

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestControlClosedManagerCannotOverwriteNewJournalEpoch(t *testing.T) {
	f := newFixture(t)
	m := f.manager(t, true)
	if err := m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	m.control.mu.Lock()
	m.control.Tasks = append(m.control.Tasks, TaskRecord{RunID: "old", SessionID: "fixture-session", State: "running", Generation: m.Status().BackendGeneration})
	if err := m.saveControlLocked(); err != nil {
		t.Fatal(err)
	}
	m.control.mu.Unlock()
	if err := m.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	fresh, err := New(m.opts)
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close(context.Background())
	m.addEvent([]byte(`{"method":"event","params":{"session_id":"fixture-session","type":"message.complete","payload":{"status":"complete"}}}`))
	data, err := os.ReadFile(filepath.Join(m.root, "control.json"))
	if err != nil {
		t.Fatal(err)
	}
	var disk struct{ Epoch uint64 }
	json.Unmarshal(data, &disk)
	if disk.Epoch != fresh.control.Epoch {
		t.Fatalf("retired manager overwrote journal: %d != %d", disk.Epoch, fresh.control.Epoch)
	}
}
func TestControlTerminalEventExpiresOutstandingRequests(t *testing.T) {
	f := newFixture(t)
	m := f.manager(t, true)
	if err := m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	m.control.mu.Lock()
	m.control.Tasks = append(m.control.Tasks, TaskRecord{RunID: "turn", SessionID: "fixture-session", State: "running", Generation: m.Status().BackendGeneration})
	m.control.mu.Unlock()
	m.addEvent([]byte(`{"id":"q","method":"clarify","params":{"session_id":"fixture-session","questions":[{"qid":"one","question":"fixture"}]}}`))
	m.addEvent([]byte(`{"method":"event","params":{"type":"message.complete","session_id":"fixture-session","payload":{"status":"interrupted"}}}`))
	var state struct {
		Attention []AttentionRecord
		Tasks     []TaskRecord
	}
	json.Unmarshal(m.ControlSnapshot(), &state)
	if len(state.Attention) != 2 || state.Attention[0].State != "expired" || state.Tasks[0].State != "interrupted" {
		t.Fatalf("terminal turn retained live approval: %s", m.ControlSnapshot())
	}
	if err := m.ControlReply(context.Background(), json.RawMessage(`{"id":"missing","result":{}}`)); err == nil {
		t.Fatal("stale request replied")
	}
}
func TestControlLifecycleOldEpochNeverReportsRunning(t *testing.T) {
	f := newFixture(t)
	m := f.manager(t, true)
	if err := m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	m.control.mu.Lock()
	m.control.Tasks = append(m.control.Tasks, TaskRecord{RunID: "old-run", SessionID: "fixture-session", Generation: m.Status().BackendGeneration, State: "running"})
	m.control.Attention = append(m.control.Attention, AttentionRecord{ID: "old-request", Generation: m.Status().BackendGeneration, State: "pending"})
	m.control.mu.Unlock()
	m.mu.Lock()
	m.epoch++
	m.mu.Unlock()
	var s struct {
		Tasks     []TaskRecord      `json:"tasks"`
		Attention []AttentionRecord `json:"attention"`
	}
	json.Unmarshal(m.ControlSnapshot(), &s)
	if s.Tasks[0].State != "interrupted" || s.Tasks[0].NativeStatus != "uncertain" || s.Attention[0].State != "expired" {
		t.Fatalf("stale epoch still active: %s", m.ControlSnapshot())
	}
}
func TestControlInboxReadIsDurableAndDoesNotConsumeRequest(t *testing.T) {
	f := newFixture(t)
	m := f.manager(t, true)
	m.control.mu.Lock()
	m.control.Attention = append(m.control.Attention, AttentionRecord{ID: "notice", State: "unread"}, AttentionRecord{ID: "question", State: "pending", Generation: m.Status().BackendGeneration})
	m.control.mu.Unlock()
	reader, ok := any(m).(interface{ ReadAttention(string) error })
	if !ok {
		t.Fatal("read/unread inbox action unavailable")
	}
	if err := reader.ReadAttention("notice"); err != nil {
		t.Fatal(err)
	}
	if reader.ReadAttention("question") == nil {
		t.Fatal("read consumed a native question")
	}
	raw, err := os.ReadFile(filepath.Join(m.root, "control.json"))
	if err != nil || !strings.Contains(string(raw), `"state":"read"`) {
		t.Fatalf("read state not durable: %v %s", err, raw)
	}
}
