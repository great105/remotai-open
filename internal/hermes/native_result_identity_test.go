package hermes

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestNativeResolvedOperationPathIsVerifiedAndDownloaded(t *testing.T) {
	m := regressionManager(t)
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "requested.txt"), []byte("unrelated old content"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "actual.txt"), []byte("actual operation fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	task := TaskRecord{RunID: "r", SessionID: "s", Generation: 7, Cwd: root}
	raw, _ := json.Marshal(map[string]any{"tool_id": "actual-operation", "name": "write_file", "args": map[string]string{"path": "requested.txt"}, "result": map[string]any{"success": true, "resolved_path": filepath.Join(root, "actual.txt"), "files_modified": []string{filepath.Join(root, "actual.txt")}}})
	m.observeResultLocked(&task, raw, 1, 7)
	if len(m.control.Results) != 1 || !m.control.Results[0].Verified || m.control.Results[0].Path != "actual.txt" {
		t.Fatalf("wrong operation identity: %+v", m.control.Results)
	}
	data, name, err := m.ReadArtifact(m.control.Results[0].ID)
	if err != nil || name != "actual.txt" || string(data) != "actual operation fixture" {
		t.Fatalf("actual operation download mismatch: %s %v", name, err)
	}
}

func TestVerifiedArtifactDownloadRechecksPrivateAliasEvenWithSameHash(t *testing.T) {
	m := regressionManager(t)
	root := t.TempDir()
	data := []byte("synthetic same-hash fixture")
	os.WriteFile(filepath.Join(root, "report.txt"), data, 0600)
	os.WriteFile(filepath.Join(root, ".env"), data, 0600)
	probe := filepath.Join(root, "probe")
	if err := os.Symlink(".env", probe); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	os.Remove(probe)
	task := TaskRecord{RunID: "r", SessionID: "s", Generation: 7, Cwd: root}
	raw, _ := json.Marshal(map[string]any{"tool_id": "download", "name": "write_file", "args": map[string]string{"path": "report.txt"}, "result": map[string]any{"success": true, "resolved_path": filepath.Join(root, "report.txt")}})
	m.observeResultLocked(&task, raw, 1, 7)
	if len(m.control.Results) != 1 || !m.control.Results[0].Verified {
		t.Fatal("public fixture not verified")
	}
	os.Remove(filepath.Join(root, "report.txt"))
	if err := os.Symlink(".env", filepath.Join(root, "report.txt")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.ReadArtifact(m.control.Results[0].ID); err == nil {
		t.Fatal("download followed private alias despite matching old hash")
	}
}

func TestNativeConflictingPathEvidenceIsUnverified(t *testing.T) {
	m := regressionManager(t)
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "report.txt"), []byte("fixture"), 0600)
	task := TaskRecord{RunID: "r", Generation: 7, Cwd: root}
	raw, _ := json.Marshal(map[string]any{"tool_id": "conflicting", "name": "patch", "args": map[string]string{"path": "report.txt"}, "result": map[string]any{"success": true, "resolved_path": filepath.Join(root, "report.txt"), "files_modified": []string{filepath.Join(root, "other.txt")}}})
	m.observeResultLocked(&task, raw, 1, 7)
	for _, result := range m.control.Results {
		if result.Verified {
			t.Fatal("conflicting native operation identity was verified")
		}
	}
}
