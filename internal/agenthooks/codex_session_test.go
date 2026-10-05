package agenthooks

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCodexSessionScope(t *testing.T) {
	home := t.TempDir()
	at := time.Date(2026, 9, 26, 12, 0, 0, 0, time.Local)
	root := "01a0dc89-4bc5-7c52-9eb5-01d4634274ee"
	child := "01a0dc8a-e363-7052-88d1-88822d5072c0"
	childWithPath := "01a0dc8b-993c-7da3-9263-37c0061f4383"
	write := func(day time.Time, id, content string) {
		t.Helper()
		dir := filepath.Join(home, "sessions", day.Format("2006"), day.Format("01"), day.Format("02"))
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "rollout-2026-09-26T10-00-00-"+id+".jsonl"), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write(at, root, `{"type":"session_meta","payload":{"id":"`+root+`","irrelevant":"`+strings.Repeat("x", 24<<10)+`"}}`+"\n"+`{"type":"event_msg","payload":{"secret":"never read"}}`)
	write(at.AddDate(0, 0, -7), child, `{"type":"session_meta","payload":{"id":"`+child+`","parent_thread_id":"`+root+`"}}`+"\n")
	write(at, childWithPath, `{"type":"session_meta","payload":{"id":"`+childWithPath+`","agent_path":"/root/helper"}}`+"\n")
	for _, tc := range []struct {
		id, want string
	}{
		{root, "root"},
		{child, "subagent"},
		{childWithPath, "subagent"},
		{"11111111-1111-1111-1111-111111111111", ""},
		{"../malicious", ""},
	} {
		if got := CodexSessionScope(home, tc.id, at); got != tc.want {
			t.Errorf("scope(%s) = %q, want %q", tc.id, got, tc.want)
		}
	}
}

func TestCodexSessionScopeRejectsUnverifiedMetadata(t *testing.T) {
	home := t.TempDir()
	at := time.Date(2026, 9, 26, 12, 0, 0, 0, time.Local)
	id := "01a0dc89-4bc5-7c52-9eb5-01d4634274ee"
	dir := filepath.Join(home, "sessions", "2026", "09", "26")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "rollout-test-"+id+".jsonl")
	for _, first := range []string{`{"type":"event_msg","payload":{"id":"` + id + `"}}`, `{"type":"session_meta","payload":{"id":"different"}}`, `{broken`} {
		if err := os.WriteFile(path, []byte(first+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
		if got := CodexSessionScope(home, id, at); got != "" {
			t.Fatalf("unverified metadata accepted: %q", got)
		}
	}
}
