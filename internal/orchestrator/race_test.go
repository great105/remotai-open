package orchestrator

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// raceStub отвечает per-agent из карты; агент вне карты отвечает нейтрально.
func raceStub(answers map[string]AgentResult) AgentRunFunc {
	return func(ctx context.Context, agentType, prompt, cwd string) AgentResult {
		if r, ok := answers[agentType]; ok {
			return r
		}
		return AgentResult{Text: "answer from " + agentType}
	}
}

func judgeResponse(t *testing.T, verdict string) *queueTransport {
	t.Helper()
	return &queueTransport{responses: []*http.Response{apiResponse(verdict)}}
}

// ── Счастливый путь: победитель выбран, результат собран ──────────────

func TestExecuteRaceWinnerSelected(t *testing.T) {
	var mu sync.Mutex
	var gotPrompts []string
	stub := func(ctx context.Context, agentType, prompt, cwd string) AgentResult {
		mu.Lock()
		gotPrompts = append(gotPrompts, prompt) // порядок недетерминирован, но все три должны прийти
		mu.Unlock()
		return AgentResult{Text: "решение варианта " + agentType, CostUSD: 0.25}
	}
	qt := judgeResponse(t, `{"winner":"B","reason":"полнее и безопаснее остальных"}`)
	o := newTestOrchestrator(qt, stub)

	res := o.ExecuteRace(context.Background(), "почини баг", t.TempDir(),
		RaceConfig{Agents: []string{"claude", "codex", "kimi"}}, nil)

	if res.IsError {
		t.Fatalf("result: %+v", res)
	}
	if res.Winner != "codex" {
		t.Errorf("Winner = %q, want codex (кандидат B)", res.Winner)
	}
	if res.Verdict != "полнее и безопаснее остальных" {
		t.Errorf("Verdict = %q", res.Verdict)
	}
	if len(res.Results) != 3 {
		t.Fatalf("Results = %d, want 3", len(res.Results))
	}
	for i, want := range []string{"claude", "codex", "kimi"} {
		if res.Results[i].Agent != want || res.Results[i].IsError {
			t.Errorf("Results[%d] = %+v, want agent %q ok", i, res.Results[i], want)
		}
	}
	if res.TotalCost != 0.75 {
		t.Errorf("TotalCost = %v, want 0.75", res.TotalCost)
	}
	// Кандидаты без diff-блока → патч не применён, причина объяснена.
	if res.AppliedPatch {
		t.Error("AppliedPatch must be false without a diff block")
	}
	if !strings.Contains(res.ApplyError, "diff") {
		t.Errorf("ApplyError = %q, want про отсутствие diff-блока", res.ApplyError)
	}
	// Пояс кандидата: запрет правок + требование diff-блока, задача сохранена.
	if len(gotPrompts) != 3 {
		t.Fatalf("prompts = %d, want 3", len(gotPrompts))
	}
	for _, p := range gotPrompts {
		if !strings.Contains(p, "НЕ изменяй") || !strings.Contains(p, "```diff") ||
			!strings.Contains(p, "почини баг") {
			t.Errorf("candidate prompt missing belt or task: %q", p)
		}
	}
}

// ── Применение патча победителя ───────────────────────────────────────

func TestExecuteRaceAppliesWinnerPatch(t *testing.T) {
	dir := initGitRepo(t)

	// Настоящий unified diff: правим a.txt, снимаем git diff, откатываем —
	// гарантированно валидный патч для git apply.
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("two\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	diffOut, err := exec.Command("git", "-C", dir, "diff").Output()
	if err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", dir, "checkout", "--", "a.txt").CombinedOutput(); err != nil {
		t.Fatalf("restore a.txt: %v\n%s", err, out)
	}

	stub := raceStub(map[string]AgentResult{
		"claude": {Text: "правки не нужны, всё и так хорошо"},
		"codex":  {Text: "Вот решение:\n```diff\n" + string(diffOut) + "```\nОбъяснение: замена одной строки."},
	})
	qt := judgeResponse(t, `{"winner":"B","reason":"есть конкретный патч"}`)
	o := newTestOrchestrator(qt, stub)

	res := o.ExecuteRace(context.Background(), "обнови a.txt", dir,
		RaceConfig{Agents: []string{"claude", "codex"}}, nil)

	if res.IsError || res.Winner != "codex" {
		t.Fatalf("result: %+v", res)
	}
	if !res.AppliedPatch {
		t.Fatalf("AppliedPatch = false, ApplyError = %q", res.ApplyError)
	}
	data, err := os.ReadFile(filepath.Join(dir, "a.txt"))
	if err != nil {
		t.Fatal(err)
	}
	// git на Windows может отдать CRLF (core.autocrlf) — сравниваем нормализовано.
	if got := strings.ReplaceAll(string(data), "\r\n", "\n"); got != "two\n" {
		t.Errorf("a.txt = %q, want %q (патч победителя применён)", data, "two\n")
	}
}

// ── Все кандидаты упали → судья не вызывается ─────────────────────────

func TestExecuteRaceAllCandidatesFailed(t *testing.T) {
	stub := func(ctx context.Context, agentType, prompt, cwd string) AgentResult {
		return AgentResult{Text: "boom: " + agentType, IsError: true}
	}
	qt := judgeResponse(t, `{"winner":"A"}`)
	o := newTestOrchestrator(qt, stub)

	res := o.ExecuteRace(context.Background(), "task", t.TempDir(),
		RaceConfig{Agents: []string{"claude", "codex"}}, nil)

	if !res.IsError {
		t.Error("IsError must be true when all candidates failed")
	}
	if res.Winner != "" {
		t.Errorf("Winner = %q, want empty", res.Winner)
	}
	if qt.calls != 0 {
		t.Errorf("judge API calls = %d, want 0 — судить нечего", qt.calls)
	}
	if res.AppliedPatch {
		t.Error("AppliedPatch must be false")
	}
}

// ── Отмена ctx до старта → кандидаты не запускаются ───────────────────

func TestExecuteRaceCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var calls atomic.Int32
	stub := func(ctx context.Context, agentType, prompt, cwd string) AgentResult {
		calls.Add(1)
		return AgentResult{Text: "ok"}
	}
	o := newTestOrchestrator(&queueTransport{}, stub)

	res := o.ExecuteRace(ctx, "task", t.TempDir(),
		RaceConfig{Agents: []string{"claude", "codex"}}, nil)

	if !res.IsError || res.Verdict != "Отменено" {
		t.Fatalf("result: %+v", res)
	}
	if calls.Load() != 0 {
		t.Errorf("agent calls = %d, want 0 на отменённом ctx", calls.Load())
	}
}

// ── Валидация конфигурации ────────────────────────────────────────────

func TestExecuteRaceValidation(t *testing.T) {
	o := newTestOrchestrator(&queueTransport{}, stubAgentOK("ok", 0))
	dir := t.TempDir()

	cases := []struct {
		name   string
		agents []string
	}{
		{"один агент", []string{"claude"}},
		{"пусто", nil},
		{"orchestrator — рекурсия", []string{"claude", "Orchestrator"}},
		{"researcher — рекурсия", []string{"claude", " researcher "}},
		{"слишком много", []string{"claude", "codex", "kimi", "gemini", "aider"}},
	}
	for _, c := range cases {
		res := o.ExecuteRace(context.Background(), "task", dir, RaceConfig{Agents: c.agents}, nil)
		if !res.IsError || res.Verdict == "" {
			t.Errorf("%s: result = %+v, want IsError с причиной", c.name, res)
		}
	}
}

// ── Анонимность судьи: в промпте нет имён агентов ─────────────────────

func TestExecuteRaceJudgeAnonymity(t *testing.T) {
	stub := raceStub(map[string]AgentResult{
		"claude": {Text: "первый вариант решения"},
		"codex":  {Text: "второй вариант решения"},
		"kimi":   {Text: "третий вариант решения"},
	})
	qt := judgeResponse(t, `{"winner":"A","reason":"равные, беру первое"}`)
	o := newTestOrchestrator(qt, stub)

	res := o.ExecuteRace(context.Background(), "нейтральная задача", t.TempDir(),
		RaceConfig{Agents: []string{"claude", "codex", "kimi"}}, nil)
	if res.IsError {
		t.Fatalf("result: %+v", res)
	}
	if qt.calls != 1 || len(qt.lastBody) == 0 {
		t.Fatalf("judge API calls = %d, want 1 с телом запроса", qt.calls)
	}

	// Достаём контент сообщений из запроса судьи (поле model содержит
	// "anthropic/claude-…" по определению — его не проверяем).
	var req struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(qt.lastBody, &req); err != nil {
		t.Fatalf("judge request body: %v", err)
	}
	var all string
	for _, m := range req.Messages {
		all += m.Content + "\n"
	}
	for _, name := range []string{"claude", "codex", "kimi"} {
		if strings.Contains(strings.ToLower(all), name) {
			t.Errorf("judge prompt содержит имя агента %q — анонимность нарушена:\n%s", name, all)
		}
	}
	if !strings.Contains(all, "Кандидат A") || !strings.Contains(all, "Кандидат C") {
		t.Errorf("judge prompt должен адресовать кандидатов по буквам:\n%s", all)
	}
}

// ── Парсинг вердикта ──────────────────────────────────────────────────

func TestParseJudgeVerdict(t *testing.T) {
	cases := []struct {
		name       string
		text       string
		n          int
		wantIdx    int
		wantOK     bool
		wantReason string
	}{
		{"чистый JSON", `{"winner":"B","reason":"лучше всех"}`, 3, 1, true, "лучше всех"},
		{"fenced json", "Вот мой вердикт:\n```json\n{\"winner\":\"C\",\"reason\":\"x\"}\n```", 3, 2, true, "x"},
		{"текст вокруг JSON", "Проанализировал. {\"winner\": \"A\", \"reason\": \"ok\"} Итог ясен.", 3, 0, true, "ok"},
		{"маркер WINNER", "Сравнил все три. WINNER: B — он точнее.", 3, 1, true, ""},
		{"буква вне диапазона", `{"winner":"D","reason":"x"}`, 3, 0, false, ""},
		{"две буквы", `{"winner":"AB","reason":"x"}`, 3, 0, false, ""},
		{"мусор", "я затрудняюсь ответить", 3, 0, false, ""},
		{"JSON со скобками в reason", `{"winner":"A","reason":"объект {a: 1} корректен"}`, 2, 0, true, "объект {a: 1} корректен"},
		{"lowercase буква", `{"winner":"b","reason":"нижний регистр"}`, 3, 1, true, "нижний регистр"},
	}
	for _, c := range cases {
		idx, reason, ok := parseJudgeVerdict(c.text, c.n)
		if ok != c.wantOK {
			t.Errorf("%s: ok = %v, want %v", c.name, ok, c.wantOK)
			continue
		}
		if !ok {
			continue
		}
		if idx != c.wantIdx {
			t.Errorf("%s: idx = %d, want %d", c.name, idx, c.wantIdx)
		}
		if c.wantReason != "" && reason != c.wantReason {
			t.Errorf("%s: reason = %q, want %q", c.name, reason, c.wantReason)
		}
	}
}

// ── Извлечение diff-блока ─────────────────────────────────────────────

func TestExtractDiffBlock(t *testing.T) {
	diff := "diff --git a/x.go b/x.go\n--- a/x.go\n+++ b/x.go\n@@ -1 +1 @@\n-old\n+new"
	if got := extractDiffBlock("Ответ:\n```diff\n" + diff + "\n```\nПояснение."); got != diff+"\n" {
		t.Errorf("fenced diff: %q", got)
	}
	// Блок без языка, но с diff --git внутри — тоже патч.
	if got := extractDiffBlock("```\n" + diff + "\n```"); got != diff+"\n" {
		t.Errorf("langless fenced with diff: %q", got)
	}
	// Сырой diff без fenced-блока не берём: после него обычно проза,
	// которая сломает git apply — честный ApplyError лучше битого применения.
	if got := extractDiffBlock("вот патч: " + diff); got != "" {
		t.Errorf("raw diff must be ignored, got %q", got)
	}
	if got := extractDiffBlock("просто текст"); got != "" {
		t.Errorf("plain text: %q", got)
	}
}

// ── Отчёт tool race в ReAct-цикле ─────────────────────────────────────

func TestToolRace(t *testing.T) {
	qt := judgeResponse(t, `{"winner":"A","reason":"первый лучше"}`)
	o := newTestOrchestrator(qt, raceStub(nil))

	out, errMsg := o.toolRace(context.Background(),
		map[string]any{"agents": "claude, codex", "task": "почини баг"}, t.TempDir())
	if errMsg != "" {
		t.Fatalf("errMsg = %q (out %q)", errMsg, out)
	}
	if !strings.Contains(out, "победитель claude") || !strings.Contains(out, "первый лучше") {
		t.Errorf("out = %q", out)
	}
	// Post-hoc учёт: 2 саб-рана засчитаны в лимитах родителя.
	if o.agentRuns != 2 {
		t.Errorf("agentRuns = %d, want 2", o.agentRuns)
	}

	// Отсутствующие аргументы — ошибка.
	if _, errMsg := o.toolRace(context.Background(), map[string]any{"task": "x"}, t.TempDir()); errMsg == "" {
		t.Error("missing agents must be an error")
	}
	// Исчерпанный бюджет родителя — отказ до старта гонки.
	o2 := newTestOrchestrator(&queueTransport{}, raceStub(nil))
	o2.SetMaxAgentCostUSD(1.0)
	o2.agentCost = 1.5
	if _, errMsg := o2.toolRace(context.Background(),
		map[string]any{"agents": "claude,codex", "task": "x"}, t.TempDir()); errMsg == "" {
		t.Error("exhausted parent budget must refuse the race")
	}
}
