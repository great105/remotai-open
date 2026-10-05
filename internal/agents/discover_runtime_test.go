package agents

import (
	"os"
	"path/filepath"
	"testing"
)

func TestClaudeRuntimeStatusAtUsesSelectedAccount(t *testing.T) {
	const pid = 42424
	selected := t.TempDir()
	other := t.TempDir()
	for dir, body := range map[string]string{
		selected: `{"status":"waiting","statusUpdatedAt":1780000000000,"kind":"claude","startedAt":1779999999000}`,
		other:    `{"status":"busy","statusUpdatedAt":1780000000001,"kind":"claude"}`,
	} {
		path := filepath.Join(dir, "sessions", "42424.json")
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	got, ok := ClaudeRuntimeStatusAt(pid, selected)
	if !ok || got.Status != "waiting" || got.StartedAt != 1779999999000 {
		t.Fatalf("selected account status = %+v, ok=%v", got, ok)
	}
	if _, ok := ClaudeRuntimeStatusAt(pid, filepath.Join(t.TempDir(), "missing")); ok {
		t.Fatal("missing selected account must not fall back to another profile")
	}
}
