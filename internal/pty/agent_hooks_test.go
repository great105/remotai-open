package pty

import (
	"testing"
	"time"

	"tgcontrol/internal/agenthooks"
)

var hookT0 = time.UnixMilli(1789000000000)

func hookEv(kind, tool string, at time.Duration) agenthooks.Event {
	return agenthooks.Event{At: hookT0.Add(at).UnixMilli(), Agent: "claude", Kind: kind, Tool: tool}
}

// Ровно та последовательность, что пришла в живом прогоне 11.09.2026:
// SessionStart → UserPromptSubmit → PermissionRequest(Write) → (ответ) →
// PostToolUse(Write) → Stop, а файл статуса Claude шёл idle → busy → waiting →
// busy → idle.
func TestHookStateLivePermissionFlow(t *testing.T) {
	var h hookState
	h.apply(hookEv(agenthooks.KindSessionStart, "", 0), time.Time{})
	h.apply(hookEv(agenthooks.KindPrompt, "", 4*time.Second), time.Time{})
	if sig := h.signal("claude", "busy", true, hookT0.Add(5*time.Second)); sig.waiting || !sig.structured {
		t.Fatalf("работа приняла вид вопроса: %+v", sig)
	}

	perm := hookEv(agenthooks.KindPermission, "Write", 8*time.Second)
	perm.Detail = `C:\Users\user\work\probe.txt`
	h.apply(perm, time.Time{})
	// Файл статуса ещё «busy» — хук сильнее в первые секунды.
	sig := h.signal("claude", "busy", true, hookT0.Add(8500*time.Millisecond))
	if !sig.waiting || sig.hint != "Разрешить изменить файл: probe.txt" || sig.seq != 1 {
		t.Fatalf("вопрос из хука: %+v", sig)
	}
	if sig := h.signal("claude", "waiting", true, hookT0.Add(10*time.Second)); !sig.waiting || sig.seq != 1 {
		t.Fatalf("тот же вопрос стал новым эпизодом: %+v", sig)
	}

	// Параллельный инструмент вопрос не закрывает.
	h.apply(hookEv(agenthooks.KindPostTool, "Read", 11*time.Second), time.Time{})
	if sig := h.signal("claude", "waiting", true, hookT0.Add(11*time.Second)); !sig.waiting {
		t.Fatal("чужой PostToolUse закрыл вопрос")
	}

	h.apply(hookEv(agenthooks.KindPostTool, "Write", 15*time.Second), time.Time{})
	if sig := h.signal("claude", "busy", true, hookT0.Add(15*time.Second)); sig.waiting {
		t.Fatalf("после ответа всё ещё ждёт: %+v", sig)
	}

	finished, dur := h.apply(hookEv(agenthooks.KindStop, "", 18*time.Second), time.Time{})
	if !finished || dur != 14*time.Second {
		t.Fatalf("конец хода: finished=%v dur=%v", finished, dur)
	}
	if !h.reportsStop("claude") || h.reportsStop("codex") {
		t.Fatal("эвристика «закончил» не выключена у Claude (или выключена у чужого агента)")
	}
}

// Вопрос Claude из хука несёт сам вопрос и пункты — до кнопок ответа; запрос
// разрешения после него пункты вопроса не наследует.
func TestHookStateAskUserQuestionOptions(t *testing.T) {
	var h hookState
	ask := hookEv(agenthooks.KindPreTool, "AskUserQuestion", time.Second)
	ask.Detail, ask.Options = "Какой клиент ставить первым?", []string{"Karing", "OneXray"}
	h.apply(ask, time.Time{})
	sig := h.signal("claude", "", false, hookT0.Add(time.Second))
	if !sig.waiting || sig.hint != "Какой клиент ставить первым?" || len(sig.options) != 2 || sig.options[1] != "OneXray" {
		t.Fatalf("вопрос из хука: %+v", sig)
	}
	h.apply(hookEv(agenthooks.KindPostTool, "AskUserQuestion", 2*time.Second), time.Time{})
	h.apply(hookEv(agenthooks.KindPermission, "Bash", 3*time.Second), time.Time{})
	if sig := h.signal("claude", "", false, hookT0.Add(3*time.Second)); !sig.waiting || sig.options != nil {
		t.Fatalf("запрос разрешения унаследовал пункты вопроса: %+v", sig)
	}
}

// Claude запущен руками, без хуков: вопрос виден по одному файлу статуса.
func TestHookStateRuntimeWaitingWithoutHooks(t *testing.T) {
	var h hookState
	sig := h.signal("claude", "waiting", true, hookT0)
	if !sig.waiting || !sig.structured || sig.hint != claudeWaitingHint || sig.seq != 1 {
		t.Fatalf("got %+v", sig)
	}
	if sig := h.signal("claude", "busy", true, hookT0.Add(time.Second)); sig.waiting {
		t.Fatalf("после ответа всё ещё ждёт: %+v", sig)
	}
	if sig := h.signal("claude", "waiting", true, hookT0.Add(2*time.Second)); sig.seq != 2 {
		t.Fatalf("новый вопрос не стал новым эпизодом: %+v", sig)
	}
	if h.reportsStop("claude") {
		t.Fatal("без хуков эвристику «закончил» выключать нельзя")
	}
}

// Esc или отказ: Claude перестал ждать, а PostToolUse не будет.
func TestHookStateRuntimeClosesStaleWait(t *testing.T) {
	var h hookState
	h.apply(hookEv(agenthooks.KindPermission, "Bash", 0), time.Time{})
	h.signal("claude", "waiting", true, hookT0.Add(time.Second))
	if sig := h.signal("claude", "idle", true, hookT0.Add(2*time.Second)); !sig.waiting {
		t.Fatal("вопрос закрыт в пределах hookWaitGrace")
	}
	if sig := h.signal("claude", "idle", true, hookT0.Add(5*time.Second)); sig.waiting {
		t.Fatalf("Claude уже не ждёт, а мы ждём: %+v", sig)
	}
}

// Версия Claude, которая "waiting" не пишет: файл весь диалог говорит "busy".
// Закрыть вопрос по нему нельзя — иначе кнопки пропали бы через 3 секунды.
func TestHookStateOldClaudeKeepsHookWait(t *testing.T) {
	var h hookState
	h.apply(hookEv(agenthooks.KindPermission, "Bash", 0), time.Time{})
	if sig := h.signal("claude", "busy", true, hookT0.Add(time.Minute)); !sig.waiting {
		t.Fatal("вопрос из хука закрыт файлом, который waiting не умеет")
	}
	h.apply(hookEv(agenthooks.KindStop, "", 2*time.Minute), time.Time{})
	if sig := h.signal("claude", "busy", true, hookT0.Add(2*time.Minute)); sig.waiting {
		t.Fatal("Stop не закрыл вопрос")
	}
}

func TestHookStateCodexNotify(t *testing.T) {
	var h hookState
	stop := agenthooks.Event{At: hookT0.Add(30 * time.Second).UnixMilli(), Agent: "codex", Kind: agenthooks.KindStop}
	finished, dur := h.apply(stop, hookT0) // UserPromptSubmit у Codex нет — от начала всплеска
	if !finished || dur != 30*time.Second {
		t.Fatalf("finished=%v dur=%v", finished, dur)
	}
	if !h.reportsStop("codex") {
		t.Fatal("эвристика у Codex не выключена после его notify")
	}
	if sig := h.signal("codex", "", false, hookT0); sig.waiting || sig.structured {
		t.Fatalf("у Codex нет сигнала «жду ответа»: %+v", sig)
	}
}

func TestHookStateAskUserQuestionAndSessionEnd(t *testing.T) {
	var h hookState
	h.apply(hookEv(agenthooks.KindPreTool, "Bash", 0), time.Time{})
	if sig := h.signal("claude", "", false, hookT0); sig.waiting {
		t.Fatal("обычный PreToolUse — не вопрос")
	}
	h.apply(hookEv(agenthooks.KindPreTool, "AskUserQuestion", time.Second), time.Time{})
	if sig := h.signal("claude", "", false, hookT0.Add(time.Second)); !sig.waiting || sig.hint != "Claude задаёт вопрос" {
		t.Fatalf("got %+v", sig)
	}
	h.apply(hookEv(agenthooks.KindSessionEnd, "", 2*time.Second), time.Time{})
	if sig := h.signal("claude", "", false, hookT0.Add(2*time.Second)); sig.waiting || h.reportsStop("claude") {
		t.Fatalf("конец сессии не сбросил состояние: %+v", sig)
	}
}

func TestPermissionHint(t *testing.T) {
	cases := []struct {
		tool, detail, want string
	}{
		{"Bash", "npm test", "Разрешить команду: npm test"},
		{"Edit", "/home/u/app/main.go", "Разрешить изменить файл: main.go"},
		{"WebFetch", "https://example.com", "Разрешить доступ в интернет: https://example.com"},
		{"ExitPlanMode", "", "Claude предлагает план — одобрить?"},
		{"mcp__linear__create_issue", "", "Разрешить mcp__linear__create_issue?"},
		{"", "", claudeWaitingHint},
	}
	for _, c := range cases {
		if got := permissionHint(agenthooks.Event{Tool: c.tool, Detail: c.detail}); got != c.want {
			t.Errorf("%s: got %q, want %q", c.tool, got, c.want)
		}
	}
}
