package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"tgcontrol/internal/agenthooks"
)

func TestClaudeHookCarriesSelectedAccountToTerminal(t *testing.T) {
	spool := filepath.Join(t.TempDir(), "hooks.jsonl")
	profile := filepath.Join(t.TempDir(), "claude-account")
	t.Setenv(agenthooks.EnvFile, spool)
	t.Setenv("CLAUDE_CONFIG_DIR", profile)

	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer read.Close()
	if _, err := write.Write([]byte(`{"session_id":"selected-session","hook_event_name":"SessionStart"}`)); err != nil {
		t.Fatal(err)
	}
	if err := write.Close(); err != nil {
		t.Fatal(err)
	}
	stdin := os.Stdin
	os.Stdin = read
	defer func() { os.Stdin = stdin }()

	if code := runHook([]string{"claude"}); code != 0 {
		t.Fatalf("hook exit code = %d", code)
	}
	data, err := os.ReadFile(spool)
	if err != nil {
		t.Fatal(err)
	}
	var event agenthooks.Event
	if err := json.Unmarshal(data, &event); err != nil {
		t.Fatal(err)
	}
	if event.Agent != "claude" || event.SessionID != "selected-session" || event.ConfigHome != profile {
		t.Fatalf("selected account was not delivered in hook: %+v", event)
	}
}
