package pty

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"tgcontrol/internal/agenthooks"
)

func TestCodexActivityIgnoresRepeatedFramesAndCursor(t *testing.T) {
	m := newScreenMirror(80, 24)
	defer m.Close()
	var a codexActivity
	now := hookT0
	for i := 0; i < 20; i++ {
		feedChunks(m, []byte(fmt.Sprintf("\x1b[H\x1b[2JComplete\r\n> \x1b[%d;1H\x1b[?25h", 1+i%2)), 512)
		fp, ok := m.activityFingerprint()
		if !ok {
			t.Fatal("complete screen unavailable")
		}
		a.observe(fp, ok, now.Add(time.Duration(i)*time.Second))
	}
	if status, at := a.status(now.Add(20 * time.Second)); status != "ready" || !at.Equal(now) {
		t.Fatalf("identical redraws stayed busy: %s %v", status, at)
	}
	// Real progress continues every second; no false idle during a long turn.
	for i := 20; i < 80; i++ {
		feedChunks(m, []byte(fmt.Sprintf("\x1b[HWorking (%ds)\x1b[K", i)), 512)
		fp, ok := m.activityFingerprint()
		at := now.Add(time.Duration(i) * time.Second)
		a.observe(fp, ok, at)
		if status, _ := a.status(at); status != "working" {
			t.Fatal("real progress shown idle")
		}
	}
	a.observe(0, false, now.Add(time.Minute))
	if status, _ := a.status(now.Add(3 * time.Minute)); status != "" {
		t.Fatal("stale mirror proved idle")
	}
}

func TestCodexStopSurvivesOutputUntilSubmittedInput(t *testing.T) {
	var a codexActivity
	a.observe(1, true, hookT0)
	a.stop(hookT0.Add(time.Second))
	// Final output, repaint, typing a draft and bracketed multiline paste.
	a.observe(2, true, hookT0.Add(2*time.Second))
	for _, part := range []string{"draft", "\x1b[20", "0~", "line\r\nline", "\x1b[201", "~", "\x1b[<64;1;1M", "\x1b[12;3R"} {
		a.input([]byte(part), hookT0.Add(3*time.Second))
		if status, _ := a.status(hookT0.Add(3 * time.Second)); status != "ready" {
			t.Fatalf("%q cleared Stop", part)
		}
	}
	a.input([]byte("\r"), hookT0.Add(4*time.Second))
	a.stop(hookT0.Add(2 * time.Second)) // delayed Stop from the previous turn
	if status, _ := a.status(hookT0.Add(4 * time.Second)); status != "working" {
		t.Fatal("next turn not working")
	}
	// A tiny turn can finish before the backend Write returns.
	a.stop(hookT0.Add(6 * time.Second))
	a.input([]byte("\r"), hookT0.Add(5*time.Second))
	if status, _ := a.status(hookT0.Add(6 * time.Second)); status != "ready" {
		t.Fatal("late write completion lost newer Stop")
	}
}

func TestCodexVerifiedNotifyKeepsSilentNextTurnWorking(t *testing.T) {
	var a codexActivity
	a.observe(1, true, hookT0)
	a.stop(hookT0.Add(time.Second))
	a.input([]byte("next\r"), hookT0.Add(2*time.Second))
	if status, _ := a.status(hookT0.Add(10 * time.Minute)); status != "working" {
		t.Fatalf("silent turn with verified notify = %q", status)
	}
	a.stop(hookT0.Add(10*time.Minute + time.Second))
	if status, _ := a.status(hookT0.Add(10*time.Minute + time.Second)); status != "ready" {
		t.Fatalf("verified Stop did not finish turn: %q", status)
	}
}

func TestCodexRuntimeContinuationOverridesOldAndDelayedNotify(t *testing.T) {
	var a codexActivity
	a.stopTurn(hookT0, "turn-1")
	// Automatic continuation: no bytes pass through Session.Write.
	started := hookT0.Add(time.Second)
	a.runtime("working", started, "turn-2")
	if status, at := a.status(hookT0.Add(21 * time.Minute)); status != "working" || !at.Equal(started) {
		t.Fatalf("active goal shown ready: %s %v", status, at)
	}
	a.stopTurn(hookT0.Add(22*time.Minute), "turn-1") // delayed previous-turn hook
	if status, _ := a.status(hookT0.Add(23 * time.Minute)); status != "working" {
		t.Fatal("delayed notify finished current turn")
	}
	a.runtime("ready", hookT0.Add(24*time.Minute), "turn-2")
	if status, _ := a.status(hookT0.Add(25 * time.Minute)); status != "ready" {
		t.Fatal("runtime completion missing")
	}
	// The next submitted input must not inherit a previous runtime completion.
	a.input([]byte("next\r"), hookT0.Add(26*time.Minute))
	if status, _ := a.status(hookT0.Add(27 * time.Minute)); status != "working" {
		t.Fatal("runtime completion survived the next input")
	}
}

type codexStatusTestConn struct{ hookTestConn }

func TestCodexRootRuntimeReachesCardWithoutPTYInput(t *testing.T) {
	fgCache.mu.Lock()
	if fgCache.entries == nil {
		fgCache.entries = make(map[uint32]foregroundCacheEntry)
	}
	fgCache.entries[42424242] = foregroundCacheEntry{at: time.Now(), info: ProcessInfo{Name: "codex", PID: 42424243}}
	fgCache.mu.Unlock()
	t.Cleanup(func() { fgCache.mu.Lock(); delete(fgCache.entries, 42424242); fgCache.mu.Unlock() })
	home := t.TempDir()
	thread := "01a0dc89-4bc5-7c52-9eb5-01d4634274ee"
	dir := filepath.Join(home, "sessions", hookT0.Format("2006"), hookT0.Format("01"), hookT0.Format("02"))
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "rollout-fixture-"+thread+".jsonl")
	meta := `{"type":"session_meta","payload":{"id":"` + thread + `"}}` + "\n"
	if err := os.WriteFile(path, []byte(meta), 0600); err != nil {
		t.Fatal(err)
	}
	id := fmt.Sprintf("codex-runtime-fixture-%d", time.Now().UnixNano())
	spool := agenthooks.SpoolPath(id)
	t.Cleanup(func() { _ = os.Remove(spool) })
	s := newSession(id, "", "shell", 1, codexStatusTestConn{}, hookT0)
	bc := &hookRecordBC{}
	m := &Manager{bc: bc}
	d := s.evState()
	m.pollAgentHooks(s, d, hookT0, nil)
	stop := agenthooks.Event{Agent: "codex", Kind: agenthooks.KindStop, SessionScope: "root", SessionID: thread, ConfigHome: home, TurnID: "turn-1", At: hookT0.Add(time.Second).UnixMilli()}
	if err := agenthooks.Append(spool, stop); err != nil {
		t.Fatal(err)
	}
	m.pollAgentHooks(s, d, stop.Time(), nil)
	if info := m.infoOf(s, stop.Time()); info.Status != "ready" {
		t.Fatal(info.Status)
	}
	started := hookT0.Add(2 * time.Second)
	line := fmt.Sprintf("{\"type\":\"event_msg\",\"timestamp\":%q,\"payload\":{\"type\":\"task_started\",\"turn_id\":\"turn-2\"}}\n", started.Format(time.RFC3339Nano))
	if err := os.WriteFile(path, []byte(meta+line), 0600); err != nil {
		t.Fatal(err)
	}
	m.pollCodexRuntime(s, d, hookT0.Add(21*time.Minute))
	if info := m.infoOf(s, hookT0.Add(21*time.Minute)); info.Status != "working" || info.StatusAt != started.UnixMilli() {
		t.Fatalf("goal card = %s %d", info.Status, info.StatusAt)
	}
	if len(bc.events) != 1 || bc.events[0]["type"] != "pty_list_changed" {
		t.Fatal("card did not get immediate refresh")
	}
	m.pollCodexRuntime(s, d, hookT0.Add(22*time.Minute))
	if len(bc.events) != 1 {
		t.Fatal("unchanged status emitted repeated events")
	}
}

func (codexStatusTestConn) shellPID() uint32 { return 42424242 }

func TestCodexStopReachesListDespiteContinuousOutput(t *testing.T) {
	// Cache an isolated synthetic foreground, never inspect a user's terminal.
	fgCache.mu.Lock()
	if fgCache.entries == nil {
		fgCache.entries = make(map[uint32]foregroundCacheEntry)
	}
	fgCache.entries[42424242] = foregroundCacheEntry{at: time.Now(), info: ProcessInfo{Name: "codex", PID: 42424243}}
	fgCache.mu.Unlock()
	t.Cleanup(func() { fgCache.mu.Lock(); delete(fgCache.entries, 42424242); fgCache.mu.Unlock() })
	id := fmt.Sprintf("codex-status-fixture-%d", time.Now().UnixNano())
	spool := agenthooks.SpoolPath(id)
	t.Cleanup(func() { _ = os.Remove(spool) })
	s := newSession(id, "", "shell", 1, codexStatusTestConn{}, hookT0)
	m := &Manager{}
	d := s.evState()
	// Actual spool -> detector -> list, including a short completed turn.
	m.pollAgentHooks(s, d, hookT0, nil)
	ev := agenthooks.Event{Agent: "codex", Kind: agenthooks.KindStop, SessionScope: "root", At: hookT0.Add(time.Second).UnixMilli()}
	if err := agenthooks.Append(spool, ev); err != nil {
		t.Fatal(err)
	}
	m.pollAgentHooks(s, d, ev.Time(), nil)
	for i := 2; i < 10; i++ {
		at := hookT0.Add(time.Duration(i) * time.Second)
		d.observeChunk([]byte("\x1b[1;1HComplete"), at)
		info := m.infoOf(s, at)
		if info.AgentKind != "codex" || info.Status != "ready" || info.StatusAt != ev.At || info.LastActive != ev.At {
			t.Fatalf("completion did not reach card: kind=%s status=%s at=%d active=%d", info.AgentKind, info.Status, info.StatusAt, info.LastActive)
		}
	}
	s.codexActivity.observe(1, true, time.Now())
	if _, err := s.Write([]byte("next\r")); err != nil {
		t.Fatal(err)
	}
	if info := m.infoOf(s, time.Now()); info.Status != "working" {
		t.Fatalf("new request card = %s", info.Status)
	}
}

func TestCodexSubagentStopDoesNotFinishForeground(t *testing.T) {
	id := fmt.Sprintf("codex-child-fixture-%d", time.Now().UnixNano())
	spool := agenthooks.SpoolPath(id)
	t.Cleanup(func() { _ = os.Remove(spool) })
	s := newSession(id, "", "shell", 1, hookTestConn{}, hookT0)
	m := &Manager{}
	d := s.evState()
	bc := &hookRecordBC{}
	m.pollAgentHooks(s, d, hookT0, bc)
	s.codexActivity.observe(1, true, hookT0)
	s.codexActivity.stop(hookT0)
	s.codexActivity.input([]byte("request\r"), hookT0.Add(time.Second))
	d.activityStart = hookT0.Add(time.Second)
	child := agenthooks.Event{Agent: "codex", Kind: agenthooks.KindStop, SessionScope: "subagent", At: hookT0.Add(20 * time.Second).UnixMilli()}
	unknown := agenthooks.Event{Agent: "codex", Kind: agenthooks.KindStop, At: hookT0.Add(21 * time.Second).UnixMilli()}
	for _, ev := range []agenthooks.Event{child, unknown} {
		if err := agenthooks.Append(spool, ev); err != nil {
			t.Fatal(err)
		}
	}
	m.pollAgentHooks(s, d, hookT0.Add(21*time.Second), bc)
	if status, _ := s.codexActivity.status(hookT0.Add(21 * time.Second)); status != "working" {
		t.Fatalf("child completion changed parent status to %q", status)
	}
	if d.hooks.reportsStop("codex") || len(bc.events) != 0 {
		t.Fatalf("child completion reached foreground detector: hooks=%+v events=%v", d.hooks, bc.events)
	}
	root := agenthooks.Event{Agent: "codex", Kind: agenthooks.KindStop, SessionScope: "root", At: hookT0.Add(25 * time.Second).UnixMilli()}
	if err := agenthooks.Append(spool, root); err != nil {
		t.Fatal(err)
	}
	m.pollAgentHooks(s, d, root.Time(), bc)
	if status, _ := s.codexActivity.status(root.Time()); status != "ready" || !d.hooks.reportsStop("codex") {
		t.Fatalf("root completion missing: status=%q hooks=%+v", status, d.hooks)
	}
	if len(bc.events) != 1 || bc.events[0]["event"] != string(EventFinished) {
		t.Fatalf("root completion event missing: %v", bc.events)
	}
}
