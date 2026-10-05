package hermes

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func regressionManager(t *testing.T) *Manager {
	return &Manager{root: t.TempDir(), epoch: 7, ready: true, control: &controlJournal{Sessions: map[string]string{}, StoredSessions: map[string]string{}, SessionEpochs: map[string]uint64{}, LegacyReplyIDs: map[string]uint64{}}}
}
func TestReviewRegressionSecretSymlink(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".env"), []byte("SYNTHETIC_PRIVATE_SENTINEL"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(".env", filepath.Join(root, "report.txt")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	data, _, err := scopedArtifact(root, "report.txt")
	if err == nil {
		t.Fatalf("private .env reachable through public alias (%d bytes)", len(data))
	}
}
func TestReviewRegressionOutOfOrderNativeStart(t *testing.T) {
	m := regressionManager(t)
	m.control.Tasks = []TaskRecord{{RunID: "admitted-first-not-sent", SessionID: "s", Generation: 7, State: "accepted"}, {RunID: "admitted-second-started-first", SessionID: "s", Generation: 7, State: "running"}}
	m.observeControl([]byte(`{"method":"event","params":{"type":"message.complete","session_id":"s","payload":{"status":"completed"}}}`), 1, 7)
	if m.control.Tasks[0].State != "accepted" || m.control.Tasks[1].State != "completed" {
		t.Fatalf("completion attributed to wrong admission: %+v", m.control.Tasks)
	}
}
func TestReviewRegressionMergedQueue(t *testing.T) {
	m := regressionManager(t)
	m.control.Tasks = []TaskRecord{{RunID: "active", SessionID: "s", Generation: 7, State: "running"}, {RunID: "q1", SessionID: "s", Generation: 7, State: "accepted", NativeStatus: "queued"}, {RunID: "q2", SessionID: "s", Generation: 7, State: "accepted", NativeStatus: "queued"}}
	event := func(kind string, seq uint64) {
		raw, _ := json.Marshal(map[string]any{"method": "event", "params": map[string]any{"type": kind, "session_id": "s", "payload": map[string]any{"status": "completed"}}})
		m.observeControl(raw, seq, 7)
	}
	event("message.complete", 1)
	event("message.start", 2)
	event("message.complete", 3)
	if m.control.Tasks[0].State != "completed" {
		t.Fatal("known active turn not completed")
	}
	for _, task := range m.control.Tasks[1:] {
		if task.State != "interrupted" || task.NativeStatus != "uncertain" {
			t.Fatalf("ambiguous legacy queue was not closed conservatively: %+v", task)
		}
	}
}
func TestReviewRegressionAutostartHasSubscription(t *testing.T) {
	f := newFixture(t)
	m := f.manager(t, true)
	if err := m.SetAutoStart(true); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.StartMaintenance(ctx)
	deadline := time.Now().Add(6 * time.Second)
	for !m.Status().Ready && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if !m.Status().Ready {
		t.Fatal("fixture never ready")
	}
	time.Sleep(1200 * time.Millisecond)
	f.mu.Lock()
	n := f.connections
	f.mu.Unlock()
	if n == 0 {
		t.Fatal("ready unattended runtime has zero event subscriptions without API call")
	}
}

func TestReviewRegressionReportedPathMustWin(t *testing.T) {
	m := regressionManager(t)
	a := t.TempDir()
	b := t.TempDir()
	os.WriteFile(filepath.Join(a, "report.txt"), []byte("stale unrelated file"), 0600)
	os.WriteFile(filepath.Join(b, "report.txt"), []byte("actual newly written file"), 0600)
	task := TaskRecord{RunID: "r", SessionID: "s", Generation: 7, State: "running", Cwd: a}
	raw, _ := json.Marshal(map[string]any{"tool_id": "t", "name": "write_file", "args": map[string]any{"path": "report.txt"}, "result": map[string]any{"success": true, "files_modified": []string{filepath.Join(b, "report.txt")}, "resolved_path": filepath.Join(b, "report.txt")}})
	m.observeResultLocked(&task, raw, 1, 7)
	if len(m.control.Results) > 0 && m.control.Results[0].Verified {
		t.Fatalf("verified unrelated task-CWD file while native resolved_path is outside it: %+v", m.control.Results[0])
	}
}
