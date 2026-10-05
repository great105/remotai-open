package hermes

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAutoStartDefaultsRespectSavedChoice(t *testing.T) {
	for _, tc := range []struct {
		name  string
		state string
		want  bool
	}{
		{"new installation", "", true},
		{"legacy missing setting", `{"schema":1,"auto_update":true}`, true},
		{"explicit enabled", `{"schema":1,"auto_start":true}`, true},
		{"explicit disabled", `{"schema":1,"auto_start":false}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			if tc.state != "" {
				if err := os.WriteFile(filepath.Join(root, "manager.json"), []byte(tc.state), 0600); err != nil {
					t.Fatal(err)
				}
			}
			for i := 0; i < 2; i++ {
				m, err := New(Options{Root: root, lookPath: func(string) (string, error) { return "", os.ErrNotExist }})
				if err != nil {
					t.Fatal(err)
				}
				if got := m.Status().AutoStart; got != tc.want {
					t.Fatalf("load %d: auto_start = %v, want %v", i, got, tc.want)
				}
				if m.Status().Running {
					t.Fatal("reading defaults started a backend")
				}
			}
		})
	}
}

func TestEnabledOwnersUsesAutoStartDefaultForLegacyState(t *testing.T) {
	root := t.TempDir()
	for _, tc := range []struct{ name, state string }{
		{"1", `{"schema":1,"managed":true}`},
		{"2", `{"schema":1,"managed":true,"auto_start":false}`},
		{"3", `{"schema":1,"managed":false}`},
	} {
		dir := filepath.Join(root, tc.name)
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "manager.json"), []byte(tc.state), 0600); err != nil {
			t.Fatal(err)
		}
	}
	owners, err := (&Manager{}).EnabledOwners(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(owners) != 1 || owners[0] != 1 {
		t.Fatalf("enabled owners = %v, want [1]", owners)
	}
}
