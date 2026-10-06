package agenthooks

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const runtimeThread = "01a0dc89-4bc5-7c52-9eb5-01d4634274ee"

var runtimeTime = time.Date(2026, 10, 6, 8, 0, 0, 0, time.UTC)

func runtimeFixture(t *testing.T, extraMeta string) (string, string) {
	t.Helper()
	home := t.TempDir()
	dir := filepath.Join(home, "sessions", "2026", "10", "06")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "rollout-fixture-"+runtimeThread+".jsonl")
	if err := os.WriteFile(path, []byte(`{"type":"session_meta","payload":{"id":"`+runtimeThread+`"`+extraMeta+"}}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return home, path
}

func runtimeEvent(kind, turn string, at time.Time) string {
	return fmt.Sprintf("{\"type\":\"event_msg\",\"timestamp\":%q,\"payload\":{\"type\":%q,\"turn_id\":%q}}\n", at.Format(time.RFC3339Nano), kind, turn)
}

func appendRuntime(t *testing.T, path, data string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err = f.WriteString(data); err != nil {
		t.Fatal(err)
	}
}

func TestCodexRuntimeGoalContinuationWithoutEnter(t *testing.T) {
	home, path := runtimeFixture(t, "")
	appendRuntime(t, path, runtimeEvent("task_started", "turn-1", runtimeTime)+runtimeEvent("task_complete", "turn-1", runtimeTime.Add(time.Second)))
	r := NewCodexRuntimeReader(home, runtimeThread, runtimeTime)
	if r == nil {
		t.Fatal("verified root runtime missing")
	}
	if status, _ := r.Read(runtimeTime.Add(time.Minute)); status != "ready" {
		t.Fatal(status)
	}
	// No PTY input: the goal schedules another autonomous turn.
	started := runtimeTime.Add(2 * time.Second)
	appendRuntime(t, path, runtimeEvent("task_started", "turn-2", started))
	if status, at := r.Read(runtimeTime.Add(21 * time.Minute)); status != "working" || !at.Equal(started) || r.TurnID() != "turn-2" {
		t.Fatalf("continuation: %s %v", status, at)
	}
	// Historical/foreign completion cannot finish the running root turn.
	appendRuntime(t, path, runtimeEvent("task_complete", "child-turn", runtimeTime.Add(22*time.Minute)))
	if status, _ := r.Read(runtimeTime.Add(23 * time.Minute)); status != "working" {
		t.Fatal(status)
	}
	appendRuntime(t, path, runtimeEvent("turn_complete", "turn-2", runtimeTime.Add(24*time.Minute)))
	if status, _ := r.Read(runtimeTime.Add(25 * time.Minute)); status != "ready" {
		t.Fatal(status)
	}
}

func TestCodexRuntimeRejectsSiblingAndUnverifiedSource(t *testing.T) {
	for _, meta := range []string{`,"parent_thread_id":"parent"`, `,"agent_path":"/root/child"`, `,"forked_from_id":"other"`} {
		home, _ := runtimeFixture(t, meta)
		if NewCodexRuntimeReader(home, runtimeThread, runtimeTime) != nil {
			t.Fatal("child runtime accepted")
		}
	}
	home, _ := runtimeFixture(t, "")
	if NewCodexRuntimeReader(home, "../../other", runtimeTime) != nil {
		t.Fatal("unverified ID accepted")
	}
	if NewCodexRuntimeReader(home, "11111111-1111-1111-1111-111111111111", runtimeTime) != nil {
		t.Fatal("sibling selected")
	}
}

func TestCodexRuntimePartialAndOversizedDialogCannotSpoofActivity(t *testing.T) {
	home, path := runtimeFixture(t, "")
	r := NewCodexRuntimeReader(home, runtimeThread, runtimeTime)
	line := runtimeEvent("task_started", "turn-1", runtimeTime)
	appendRuntime(t, path, line[:len(line)-2])
	if status, _ := r.Read(runtimeTime); status != "" {
		t.Fatal("partial record changed status")
	}
	appendRuntime(t, path, line[len(line)-2:])
	if status, _ := r.Read(runtimeTime); status != "working" {
		t.Fatal(status)
	}
	appendRuntime(t, path, `{"type":"response_item","payload":{"text":"task_complete`+strings.Repeat("x", codexRuntimeRecordLimit*2)+`"}}`+"\n")
	appendRuntime(t, path, "{broken}\n"+runtimeEvent("task_complete", "turn-1", runtimeTime.Add(time.Second)))
	if status, _ := r.Read(runtimeTime.Add(time.Minute)); status != "ready" {
		t.Fatal(status)
	}
	if len(r.pending) > codexRuntimeRecordLimit {
		t.Fatal("unbounded dialog buffer")
	}
}

func TestCodexRuntimeCatchesUpBeforeReturningOldCompletion(t *testing.T) {
	home, path := runtimeFixture(t, "")
	appendRuntime(t, path, runtimeEvent("task_complete", "turn-1", runtimeTime))
	appendRuntime(t, path, strings.Repeat("x", codexRuntimeReadLimit+100)+"\n"+runtimeEvent("turn_started", "turn-2", runtimeTime.Add(time.Second)))
	r := NewCodexRuntimeReader(home, runtimeThread, runtimeTime)
	if status, _ := r.Read(runtimeTime.Add(time.Minute)); status != "" || r.offset > codexRuntimeReadLimit {
		t.Fatal("old completion reported during bounded catch-up")
	}
	if status, _ := r.Read(runtimeTime.Add(time.Minute)); status != "working" {
		t.Fatal(status)
	}
}

func TestCodexRuntimeTruncationIdentityAndAbort(t *testing.T) {
	home, path := runtimeFixture(t, "")
	appendRuntime(t, path, runtimeEvent("task_started", "old", runtimeTime)+strings.Repeat(" ", 1000)+"\n")
	r := NewCodexRuntimeReader(home, runtimeThread, runtimeTime)
	r.Read(runtimeTime)
	meta := `{"type":"session_meta","payload":{"id":"` + runtimeThread + `"}}` + "\n"
	if err := os.WriteFile(path, []byte(meta+runtimeEvent("turn_started", "new", runtimeTime.Add(time.Second))+runtimeEvent("turn_aborted", "new", runtimeTime.Add(2*time.Second))), 0600); err != nil {
		t.Fatal(err)
	}
	if status, _ := r.Read(runtimeTime.Add(time.Minute)); status != "ready" || r.TurnID() != "new" {
		t.Fatal(status)
	}
	if err := os.WriteFile(path, []byte(strings.ReplaceAll(meta, runtimeThread, "11111111-1111-1111-1111-111111111111")), 0600); err != nil {
		t.Fatal(err)
	}
	if status, _ := r.Read(runtimeTime.Add(time.Minute)); status != "" {
		t.Fatal("replaced identity accepted")
	}
}
