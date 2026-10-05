package pty

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"tgcontrol/internal/agenthistory"
)

func newSleepTestManager(t *testing.T, sessions ...*Session) *Manager {
	t.Helper()
	m := &Manager{
		sessions: map[string]*Session{},
		meta:     NewMetaStoreAt(filepath.Join(t.TempDir(), "pty.json")),
	}
	for _, s := range sessions {
		m.sessions[s.ID] = s
	}
	return m
}

// selfShellConn — «шелл» терминала = сам процесс теста. Детей у него нет,
// поэтому на переднем плане шелл (имя берётся из Session.Shell).
type selfShellConn struct{ eofConn }

func (selfShellConn) shellPID() uint32 { return uint32(os.Getpid()) }

func shellSession(id string) *Session {
	return &Session{ID: id, Shell: "powershell.exe", pty: selfShellConn{}}
}

func sleepCode(t *testing.T, err error) string {
	t.Helper()
	var se *SleepError
	if !errors.As(err, &se) {
		t.Fatalf("ждали SleepError, получили %v", err)
	}
	return se.Code
}

func TestSleepRefusesMissingAndSSH(t *testing.T) {
	m := newSleepTestManager(t, &Session{ID: "ssh1", Shell: "ssh"})
	if _, err := m.Sleep("nope"); sleepCode(t, err) != "not_found" {
		t.Fatalf("несуществующий терминал: %v", err)
	}
	if _, err := m.Sleep("ssh1"); sleepCode(t, err) != "ssh" {
		t.Fatalf("SSH-терминал: %v", err)
	}
}

func TestWakeClaimsOnceUntilTTL(t *testing.T) {
	m := newSleepTestManager(t, shellSession("t1"))
	if _, err := m.Wake("t1"); sleepCode(t, err) != "not_sleeping" {
		t.Fatalf("без сна: %v", err)
	}
	rec := &SleepRecord{Agent: "claude", SessionID: "7caa5648-8121-42c3-8868-d270b407a029",
		Flags: []string{"--dangerously-skip-permissions"}, At: time.Now().UnixMilli()}
	if err := m.meta.SetSleep("t1", rec); err != nil {
		t.Fatal(err)
	}
	got, err := m.Wake("t1")
	if err != nil || got.SessionID != rec.SessionID || got.WakingAt == 0 || len(got.Flags) != 1 {
		t.Fatalf("первый экран должен получить запись: %+v %v", got, err)
	}
	// Второй экран в тот же момент — отказ, иначе вторая команда продолжения
	// напечаталась бы в чат уже проснувшегося агента.
	if _, err := m.Wake("t1"); sleepCode(t, err) != "waking" {
		t.Fatalf("второй экран: %v", err)
	}
	// Агент так и не появился — через wakeClaimTTL запись снова доступна.
	stale := *m.meta.Get("t1").Sleep
	stale.WakingAt = time.Now().Add(-wakeClaimTTL - time.Second).UnixMilli()
	if err := m.meta.SetSleep("t1", &stale); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Wake("t1"); err != nil {
		t.Fatalf("после TTL запись должна выдаваться снова: %v", err)
	}
}

func TestSleepRecordSurvivesRestartAndClearsWhenAgentBack(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pty.json")
	store := NewMetaStoreAt(path)
	rec := &SleepRecord{Agent: "codex", SessionID: "019a1b2c-3d4e-5f60-7182-93a4b5c6d7e8", At: 42}
	if err := store.SetSleep("t1", rec); err != nil {
		t.Fatal(err)
	}
	reopened := NewMetaStoreAt(path)
	if got := reopened.Get("t1").Sleep; got == nil || got.SessionID != rec.SessionID {
		t.Fatalf("сон должен пережить перезапуск Remotai: %+v", got)
	}
	s := &Session{ID: "t1"}
	m := &Manager{sessions: map[string]*Session{"t1": s}, meta: reopened}
	if got := m.sleepInfoFor(s, "shell", ProcessInfo{PID: 10, Name: "powershell"}); got == nil || got.Agent != "codex" {
		t.Fatalf("пока агента нет, терминал спит: %+v", got)
	}
	if got := m.sleepInfoFor(s, "codex", ProcessInfo{PID: 77, Name: "codex", StartMs: 500}); got != nil {
		t.Fatalf("агент вернулся — сна нет: %+v", got)
	}
	if reopened.Get("t1").Sleep != nil {
		t.Fatal("запись сна должна стереться, когда агент снова в терминале")
	}
}

func TestSleepSessionIDIgnoresHookOfPreviousAgent(t *testing.T) {
	s := &Session{ID: "t1"}
	s.historySource = agenthistory.Source{Agent: "codex", SessionID: "019a1b2c-3d4e-5f60-7182-93a4b5c6d7e8"}
	s.historyAt = time.Now()
	fg := ProcessInfo{PID: 999999, Name: "codex", StartMs: time.Now().Add(-time.Hour).UnixMilli()}
	if got := s.sleepSessionID("codex", fg); got != s.historySource.SessionID {
		t.Fatalf("хук текущего агента: %q", got)
	}
	// Хук пришёл раньше старта процесса — это беседа прошлого агента.
	fg.StartMs = time.Now().Add(time.Hour).UnixMilli()
	if got := s.sleepSessionID("codex", fg); got != "" {
		t.Fatalf("беседа прошлого агента не должна подниматься: %q", got)
	}
	if got := s.sleepSessionID("claude", ProcessInfo{PID: 999999}); got != "" {
		t.Fatalf("чужой вид агента: %q", got)
	}
}

// Скептик 29.09: пока снятый процесс ещё виден (taskkill не отработал, кэш
// снимка до 500 мс), опрос не должен стирать запись — иначе «Разбудить»
// пропадала сразу после сна.
func TestSleepRecordSurvivesDyingSleptProcess(t *testing.T) {
	store := NewMetaStoreAt(filepath.Join(t.TempDir(), "pty.json"))
	rec := &SleepRecord{Agent: "claude", SessionID: "7caa5648-8121-42c3-8868-d270b407a029", At: 1, PID: 4242, StartMs: 1000}
	if err := store.SetSleep("t1", rec); err != nil {
		t.Fatal(err)
	}
	s := &Session{ID: "t1"}
	m := &Manager{sessions: map[string]*Session{"t1": s}, meta: store}
	if got := m.sleepInfoFor(s, "claude", ProcessInfo{PID: 4242, Name: "claude", StartMs: 1000}); got == nil {
		t.Fatal("тот же, ещё не снятый процесс — сон продолжается")
	}
	if store.Get("t1").Sleep == nil {
		t.Fatal("запись стёрта снимком усыплённого процесса")
	}
	// Тот же PID, но другое поколение — это уже новый агент.
	if got := m.sleepInfoFor(s, "claude", ProcessInfo{PID: 4242, Name: "claude", StartMs: 9000}); got != nil {
		t.Fatal("новый процесс с тем же PID — агент проснулся")
	}
}

// Будить можно только в шелл: иначе команда продолжения уйдёт сообщением в
// запущенную в терминале программу.
func TestWakeRefusesWhenShellIsNotForeground(t *testing.T) {
	m := newSleepTestManager(t, &Session{ID: "t1", Shell: "powershell.exe", pty: eofConn{}})
	rec := &SleepRecord{Agent: "claude", SessionID: "7caa5648-8121-42c3-8868-d270b407a029", At: 1}
	if err := m.meta.SetSleep("t1", rec); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Wake("t1"); sleepCode(t, err) != "busy" {
		t.Fatalf("передний план неизвестен — будить нельзя: %v", err)
	}
	if got := m.meta.Get("t1").Sleep; got == nil || got.WakingAt != 0 {
		t.Fatalf("отказ не должен забирать запись: %+v", got)
	}
}

func TestResumeIDFromArgs(t *testing.T) {
	const sid = "019a1b2c-3d4e-5f60-7182-93a4b5c6d7e8"
	cases := []struct {
		kind string
		argv []string
		want string
	}{
		{"codex", []string{"node", "codex.js", "resume", sid, "--search"}, sid},
		{"claude", []string{"claude.exe", "--resume", sid, "--dangerously-skip-permissions"}, sid},
		{"claude", []string{"claude.exe", "-r", sid}, sid},
		{"claude", []string{"claude.exe", "--resume=" + sid}, sid},
		{"codex", []string{"node", "codex.js", "resume", "--last"}, ""},
		{"claude", []string{"claude.exe", "--continue"}, ""},
		{"claude", []string{"node", "codex.js", "resume", sid}, ""},
	}
	for _, c := range cases {
		if got := resumeIDFromArgs(c.kind, c.argv); got != c.want {
			t.Fatalf("%s %v: %q, ждали %q", c.kind, c.argv, got, c.want)
		}
	}
}
