package hermes

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestControlVerifiedNativePatchResultNullError(t *testing.T) {
	f := newFixture(t)
	m := f.manager(t, true)
	if err := m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	cwd := t.TempDir()
	os.WriteFile(filepath.Join(cwd, "report.txt"), []byte("native shape fixture"), 0600)
	m.control.mu.Lock()
	m.control.Tasks = append(m.control.Tasks, TaskRecord{RunID: "native-run", SessionID: "fixture-session", StoredSessionID: "stored-native", Generation: m.Status().BackendGeneration, State: "running", Cwd: cwd})
	m.control.mu.Unlock()
	raw, _ := json.Marshal(map[string]any{"method": "event", "params": map[string]any{"session_id": "fixture-session", "type": "tool.complete", "payload": map[string]any{"tool_id": "native-patch", "name": "patch", "args": map[string]string{"mode": "patch"}, "result": map[string]any{"success": true, "error": nil, "files_modified": []string{filepath.Join(cwd, "report.txt"), "../outside.txt"}}, "inline_diff": "+ fixture"}}})
	m.addEvent(raw)
	var state struct{ Results []ResultRecord }
	json.Unmarshal(m.ControlSnapshot(), &state)
	if len(state.Results) != 2 || !state.Results[0].Verified || state.Results[1].Verified || state.Results[0].ToolID != "native-patch" {
		t.Fatalf("native structured result unsupported or unsafe: %s", m.ControlSnapshot())
	}
	os.WriteFile(filepath.Join(cwd, "report.txt"), []byte("modified"), 0600)
	if _, _, err := m.ReadArtifact(state.Results[0].ID); err == nil {
		t.Fatal("download ignored changed on-disk hash")
	}
}
func TestControlVerifiedResultCannotExposeArbitraryFiles(t *testing.T) {
	f := newFixture(t)
	m := f.manager(t, true)
	if err := m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	cwd := t.TempDir()
	if err := os.WriteFile(filepath.Join(cwd, "report.txt"), []byte("fixture report"), 0600); err != nil {
		t.Fatal(err)
	}
	info, _ := json.Marshal(map[string]any{"method": "event", "params": map[string]any{"type": "session.info", "session_id": "fixture-session", "payload": map[string]any{"cwd": cwd, "stored_session_id": "stored-fixture"}}})
	m.addEvent(info)
	_, err := control(t, m).SubmitTask(context.Background(), json.RawMessage(`{"client_request_id":"result-1","session_id":"fixture-session","stored_session_id":"stored-fixture","text":"fixture only"}`))
	if err != nil {
		t.Fatal(err)
	}
	// Native tool.complete belongs to a started turn, never to a queued receipt.
	m.addEvent([]byte(`{"method":"event","params":{"type":"message.start","session_id":"fixture-session"}}`))
	tool := func(path string) {
		raw, _ := json.Marshal(map[string]any{"method": "event", "params": map[string]any{"type": "tool.complete", "session_id": "fixture-session", "payload": map[string]any{"tool_id": path, "name": "write_file", "args": map[string]string{"path": path}, "result": map[string]any{"verified": true}}}})
		m.addEvent(raw)
	}
	tool("report.txt")
	tool("../outside.txt")
	tool("missing.txt")
	var snapshot struct {
		Results []struct {
			ID       string `json:"id"`
			Verified bool   `json:"verified"`
			RunID    string `json:"run_id"`
		} `json:"results"`
	}
	_ = json.Unmarshal(control(t, m).ControlSnapshot(), &snapshot)
	if len(snapshot.Results) != 3 || !snapshot.Results[0].Verified || snapshot.Results[1].Verified || snapshot.Results[2].Verified || snapshot.Results[0].RunID == "" {
		t.Fatalf("verification missing or unsafe: %s", control(t, m).ControlSnapshot())
	}
	reader, ok := any(m).(interface {
		ReadArtifact(string) ([]byte, string, error)
	})
	if !ok {
		t.Fatal("scoped download unavailable")
	}
	data, _, err := reader.ReadArtifact(snapshot.Results[0].ID)
	if err != nil || string(data) != "fixture report" {
		t.Fatalf("download: %q %v", data, err)
	}
	for _, id := range []string{snapshot.Results[1].ID, "C:/Windows/win.ini", "../report.txt"} {
		if _, _, err := reader.ReadArtifact(id); err == nil {
			t.Fatalf("exposed %s", id)
		}
	}
}
