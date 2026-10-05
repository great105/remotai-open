package pty

import (
	"os"
	"path/filepath"
	"testing"

	"tgcontrol/internal/agenthooks"
)

func TestClaudeRuntimeStatusFollowsThisTerminalHook(t *testing.T) {
	const pid = 42424
	profile := t.TempDir()
	other := t.TempDir()
	path := filepath.Join(profile, "sessions", "42424.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"status":"waiting","kind":"claude"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	s := &Session{}
	s.rememberHistorySource(agenthooks.Event{
		Agent: "claude", SessionID: "this-session", ConfigHome: profile,
	})
	got, ok := s.claudeRuntimeStatus(pid)
	if !ok || got.Status != "waiting" {
		t.Fatalf("hook account status = %+v, ok=%v", got, ok)
	}
	otherPath := filepath.Join(other, "sessions", "42424.json")
	if err := os.MkdirAll(filepath.Dir(otherPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(otherPath, []byte(`{"status":"busy","kind":"claude"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	s.rememberHistorySource(agenthooks.Event{
		Agent: "claude", SessionID: "next-session", ConfigHome: other,
	})
	got, ok = s.claudeRuntimeStatus(pid)
	if !ok || got.Status != "busy" {
		t.Fatalf("new hook account status = %+v, ok=%v", got, ok)
	}
}
