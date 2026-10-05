package hermes

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestPrivateArtifactAncestorsAndResultVerification(t *testing.T) {
	for _, name := range []string{".ssh", ".hermes", ".git", ".SSH"} {
		for _, nested := range []bool{false, true} {
			t.Run(name+map[bool]string{false: "/root", true: "/ancestor"}[nested], func(t *testing.T) {
				root := filepath.Join(t.TempDir(), name)
				if nested {
					root = filepath.Join(root, "reports")
				}
				if err := os.MkdirAll(root, 0700); err != nil {
					t.Fatal(err)
				}
				path := filepath.Join(root, "report.txt")
				if err := os.WriteFile(path, []byte("synthetic fixture"), 0600); err != nil {
					t.Fatal(err)
				}
				for _, p := range []string{"report.txt", path} {
					if _, _, err := scopedArtifact(root, p); err == nil {
						t.Fatal("private ancestry laundered")
					}
				}
				m := regressionManager(t)
				task := TaskRecord{RunID: "artifact", SessionID: "s", Generation: 7, Cwd: root}
				payload, _ := json.Marshal(map[string]any{"tool_id": "test", "name": "write_file", "args": map[string]string{"path": "report.txt"}, "result": map[string]bool{"verified": true}})
				m.observeResultLocked(&task, payload, 1, 7)
				if len(m.control.Results) != 1 || m.control.Results[0].Verified {
					t.Fatal("private root verified")
				}
			})
		}
	}
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "report.txt"), []byte("public fixture"), 0600)
	for _, p := range []string{"report.txt", filepath.Join(root, "report.txt")} {
		data, _, err := scopedArtifact(root, p)
		if err != nil || string(data) != "public fixture" {
			t.Fatalf("legitimate artifact: %v", err)
		}
	}
}
