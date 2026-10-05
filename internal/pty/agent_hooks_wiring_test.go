package pty

import (
	"io"
	"os"
	"strconv"
	"testing"
	"time"

	"tgcontrol/internal/agenthooks"
)

// hookTestConn — терминал без процесса: ForegroundProcess вернёт пустой
// процесс (shellPID 0), файла статуса Claude у такой сессии нет — остаются
// только хуки.
type hookTestConn struct{}

func (hookTestConn) Read([]byte) (int, error)    { return 0, io.EOF }
func (hookTestConn) Write(p []byte) (int, error) { return len(p), nil }
func (hookTestConn) Resize(int, int) error       { return nil }
func (hookTestConn) Close() error                { return nil }
func (hookTestConn) shellPID() uint32            { return 0 }
func (hookTestConn) currentCWD() (string, error) { return "", nil }

type hookRecordBC struct{ events []map[string]any }

func (r *hookRecordBC) Broadcast(_ int64, ev any) {
	if m, ok := ev.(map[string]any); ok {
		r.events = append(r.events, m)
	}
}

// Сквозная проводка: строка в очереди терминала (её пишет `remotai hook`) →
// детектор → «ждёт ответа» с текстом вопроса, одно событие на эпизод →
// после ответа и Stop — «закончил» с длительностью хода.
func TestAgentHooksWiring(t *testing.T) {
	id := "hookwire" + strconv.FormatInt(time.Now().UnixNano(), 36)
	spool := agenthooks.SpoolPath(id)
	t.Cleanup(func() { _ = os.Remove(spool) })

	m := &Manager{sessions: map[string]*Session{}}
	s := newSession(id, "cwd", "shell", 1, hookTestConn{}, time.Now())
	d := s.evState()
	bc := &hookRecordBC{}
	now := time.Now()

	m.pollAgentHooks(s, d, now, bc) // очереди ещё нет: терминал новый, читать с нуля
	add := func(kind, tool, detail string, at time.Time) {
		t.Helper()
		ev := agenthooks.Event{At: at.UnixMilli(), Agent: "claude", Kind: kind, Tool: tool, Detail: detail}
		if err := agenthooks.Append(spool, ev); err != nil {
			t.Fatal(err)
		}
	}
	add(agenthooks.KindPrompt, "", "", now.Add(-20*time.Second))
	add(agenthooks.KindPermission, "Bash", "npm test", now.Add(-time.Second))
	m.pollAgentHooks(s, d, now, bc)

	sig := m.agentSignals(s, d, "claude", now)
	if !sig.waiting || sig.hint != "Разрешить команду: npm test" {
		t.Fatalf("вопрос из очереди не дошёл: %+v", sig)
	}
	tail := []byte("Do you want to proceed?\n❯ 1. Yes\n  2. Yes, and don't ask again for npm test\n  3. No\n")
	m.holdStructuredWait(s, d, tail, sig, now, bc)
	m.holdStructuredWait(s, d, tail, m.agentSignals(s, d, "claude", now.Add(time.Second)), now.Add(time.Second), bc)

	var waits []map[string]any
	for _, ev := range bc.events {
		if ev["event"] == string(EventWaitingInput) {
			waits = append(waits, ev)
		}
	}
	if len(waits) != 1 {
		t.Fatalf("ожидалось ОДНО событие waiting_input на эпизод, пришло %d: %v", len(waits), bc.events)
	}
	if waits[0]["hint"] != "Разрешить команду: npm test" || waits[0]["status_at"] == nil {
		t.Fatalf("событие без текста вопроса или штампа эпизода: %v", waits[0])
	}
	t.Logf("пункты с экрана: %v (kind=%v)", waits[0]["hint_options"], waits[0]["hint_kind"])
	if le := d.lastEvt; le.kind != EventWaitingInput || le.hint == "" || !d.questionOnScreen(now.Add(time.Second)) {
		t.Fatalf("статус терминала не «ждёт ответа»: %+v", le)
	}

	add(agenthooks.KindPostTool, "Bash", "npm test", now.Add(2*time.Second))
	add(agenthooks.KindStop, "", "", now.Add(3*time.Second))
	m.pollAgentHooks(s, d, now.Add(3*time.Second), bc)
	if sig := m.agentSignals(s, d, "claude", now.Add(3*time.Second)); sig.waiting {
		t.Fatalf("после ответа всё ещё ждёт: %+v", sig)
	}
	var fin map[string]any
	for _, ev := range bc.events {
		if ev["event"] == string(EventFinished) {
			fin = ev
		}
	}
	if fin == nil {
		t.Fatalf("Stop не дал «закончил»: %v", bc.events)
	}
	if ms, _ := fin["duration_ms"].(int64); ms != 23000 {
		t.Fatalf("длительность хода %v, ожидалось 23000 мс (от UserPromptSubmit)", fin["duration_ms"])
	}
	if !d.hooks.reportsStop("claude") {
		t.Fatal("эвристика тишины не выключена — будет второе «закончил»")
	}
}
