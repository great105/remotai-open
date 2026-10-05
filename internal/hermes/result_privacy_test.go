package hermes

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestControlResultsDoNotPersistSecretFilesOrRawContent(t *testing.T) {
	f := newFixture(t)
	m := f.manager(t, true)
	if err := m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	cwd := t.TempDir()
	os.WriteFile(filepath.Join(cwd, ".env"), []byte("fixture-private-result-sentinel"), 0600)
	m.control.mu.Lock()
	m.control.Tasks = append(m.control.Tasks, TaskRecord{RunID: "secret-result", SessionID: "fixture-session", Generation: m.Status().BackendGeneration, State: "running", Cwd: cwd})
	m.control.mu.Unlock()
	raw, _ := json.Marshal(map[string]any{"method": "event", "params": map[string]any{"session_id": "fixture-session", "type": "tool.complete", "payload": map[string]any{"tool_id": "private-file", "name": "write_file", "args": map[string]string{"path": ".env"}, "result": map[string]any{"verified": true}, "inline_diff": "fixture-private-result-sentinel"}}})
	m.addEvent(raw)
	data, err := os.ReadFile(filepath.Join(m.root, "control.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "fixture-private-result-sentinel") {
		t.Fatal("secret file content persisted in result journal")
	}
	var s struct{ Results []ResultRecord }
	json.Unmarshal(m.ControlSnapshot(), &s)
	if s.Results[0].Verified {
		t.Fatal("secret file admitted as downloadable artifact")
	}
}
