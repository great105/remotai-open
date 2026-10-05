package orchestrator

import (
	"context"
	"math"
	"net/http"
	"strings"
	"testing"
)

// ── Делегация через узкий белт (delegate.go) ──────────────────────────

// countingAgent считает фактические вызовы саб-агента и отвечает фиксированно.
func countingAgent(calls *int, text string, cost float64) AgentRunFunc {
	return func(ctx context.Context, agentType, prompt, cwd string) AgentResult {
		*calls++
		return AgentResult{Text: text, CostUSD: cost}
	}
}

func TestDelegateRunLimit(t *testing.T) {
	calls := 0
	o := newTestOrchestrator(&queueTransport{}, countingAgent(&calls, "ok", 0))
	o.SetMaxAgentRuns(2)

	for i := range 2 {
		if _, errMsg := o.toolRunAgent(context.Background(),
			map[string]any{"agent": "claude", "prompt": "p"}, t.TempDir()); errMsg != "" {
			t.Fatalf("делегирование %d из 2 отклонено: %s", i+1, errMsg)
		}
	}
	_, errMsg := o.toolRunAgent(context.Background(),
		map[string]any{"agent": "claude", "prompt": "p"}, t.TempDir())
	if errMsg == "" || !strings.Contains(errMsg, "лимит саб-ранов") {
		t.Fatalf("третье делегирование должно быть отклонено: %q", errMsg)
	}
	if !strings.Contains(errMsg, "2/2") {
		t.Errorf("в отказе должны быть цифры лимита: %q", errMsg)
	}
	if calls != 2 {
		t.Errorf("runAgent вызван %d раз, want 2 — отказ до реального вызова", calls)
	}
}

func TestDelegateRunLimitDefaultAndDisable(t *testing.T) {
	o := newTestOrchestrator(&queueTransport{}, stubAgentOK("ok", 0))
	// Дефолт без настройки — лимит включён и равен defaultMaxAgentRuns.
	if o.maxAgentRuns != defaultMaxAgentRuns {
		t.Errorf("дефолт maxAgentRuns = %d, want %d", o.maxAgentRuns, defaultMaxAgentRuns)
	}
	// 0 = без лимита; отрицательное сводится к 0.
	o.SetMaxAgentRuns(-3)
	if o.maxAgentRuns != 0 {
		t.Errorf("отрицательный лимит должен сводиться к 0, got %d", o.maxAgentRuns)
	}
	for range defaultMaxAgentRuns + 2 {
		if _, errMsg := o.toolRunAgent(context.Background(),
			map[string]any{"agent": "claude", "prompt": "p"}, t.TempDir()); errMsg != "" {
			t.Fatalf("при выключенном лимите отказа быть не должно: %s", errMsg)
		}
	}
}

func TestDelegateBudget(t *testing.T) {
	calls := 0
	o := newTestOrchestrator(&queueTransport{}, countingAgent(&calls, "ok", 0.30))
	o.SetMaxAgentCostUSD(0.50)

	// $0.00 < $0.50 → делегируем ($0.30); $0.30 < $0.50 → ещё раз ($0.60).
	for i := range 2 {
		if _, errMsg := o.toolRunAgent(context.Background(),
			map[string]any{"agent": "claude", "prompt": "p"}, t.TempDir()); errMsg != "" {
			t.Fatalf("делегирование %d: %s", i+1, errMsg)
		}
	}
	// $0.60 >= $0.50 → отказ с остатком в сообщении.
	_, errMsg := o.toolRunAgent(context.Background(),
		map[string]any{"agent": "claude", "prompt": "p"}, t.TempDir())
	if errMsg == "" || !strings.Contains(errMsg, "бюджет") {
		t.Fatalf("превышение бюджета должно отклонять: %q", errMsg)
	}
	for _, want := range []string{"$0.6000", "$0.50", "остаток $0.0000"} {
		if !strings.Contains(errMsg, want) {
			t.Errorf("в сообщении об отказе нет %q: %q", want, errMsg)
		}
	}
	if calls != 2 {
		t.Errorf("runAgent вызван %d раз, want 2", calls)
	}
	if math.Abs(o.agentCost-0.60) > 1e-9 {
		t.Errorf("agentCost = %v, want 0.60 (кумулятивно по ранам)", o.agentCost)
	}
}

func TestDelegateBudgetDefaultUnlimited(t *testing.T) {
	o := newTestOrchestrator(&queueTransport{}, stubAgentOK("ok", 100))
	// Без SetMaxAgentCostUSD бюджет выключен: любые траты пропускаются.
	for range 3 {
		if _, errMsg := o.toolRunAgent(context.Background(),
			map[string]any{"agent": "claude", "prompt": "p"}, t.TempDir()); errMsg != "" {
			t.Fatalf("дефолт — без бюджета: %s", errMsg)
		}
	}
}

func TestDelegateNormalizesAgentType(t *testing.T) {
	var got []string
	o := newTestOrchestrator(&queueTransport{}, func(ctx context.Context, agentType, prompt, cwd string) AgentResult {
		got = append(got, agentType)
		return AgentResult{Text: "ok"}
	})
	// Регистр и краевые пробелы нормализуются до канонического ID.
	if _, errMsg := o.toolRunAgent(context.Background(),
		map[string]any{"agent": " Claude ", "prompt": "p"}, t.TempDir()); errMsg != "" {
		t.Fatalf("нормализованный агент должен делегироваться: %s", errMsg)
	}
	if len(got) != 1 || got[0] != "claude" {
		t.Fatalf("runAgent получил %v, want [claude]", got)
	}
	// Гард глубины 1 не обходится регистром/пробелами.
	for _, agent := range []string{"Orchestrator", " RESEARCHER ", "oRcHeStRaToR"} {
		if _, errMsg := o.toolRunAgent(context.Background(),
			map[string]any{"agent": agent, "prompt": "p"}, t.TempDir()); errMsg == "" {
			t.Errorf("делегирование %q должно быть отклонено", agent)
		}
	}
	if len(got) != 1 {
		t.Errorf("обход гарда дошёл до runAgent: %v", got)
	}
}

func TestDelegateBeltFrame(t *testing.T) {
	prompts := map[string]string{}
	o := newTestOrchestrator(&queueTransport{}, func(ctx context.Context, agentType, prompt, cwd string) AgentResult {
		prompts[agentType] = prompt
		return AgentResult{Text: "ok"}
	})
	dir := t.TempDir()
	if _, errMsg := o.toolRunAgent(context.Background(), map[string]any{"agent": "claude", "prompt": "сделай X"}, dir); errMsg != "" {
		t.Fatal(errMsg)
	}
	if _, errMsg := o.toolRunAgent(context.Background(), map[string]any{"agent": "shell", "prompt": "echo hi"}, dir); errMsg != "" {
		t.Fatal(errMsg)
	}

	// AI-агент: рамка ask/plan/run/status + исходная задача.
	if !strings.Contains(prompts["claude"], "ask/plan/run/status") ||
		!strings.Contains(prompts["claude"], "сделай X") {
		t.Errorf("AI-агент должен получить рамку + задачу: %q", prompts["claude"])
	}
	// shell: промпт это команда, рамка её сломает — идёт verbatim.
	if prompts["shell"] != "echo hi" {
		t.Errorf("shell-промпт должен идти без рамки: %q", prompts["shell"])
	}
}

func TestExecuteResetsDelegationCounters(t *testing.T) {
	mk := func() *queueTransport {
		return &queueTransport{responses: []*http.Response{
			apiResponse("", toolCallJSON("c1", "run_agent", `{"agent":"claude","prompt":"p"}`)),
			apiResponse("", toolCallJSON("c2", "finish", `{"summary":"done"}`)),
		}}
	}
	o := newTestOrchestrator(mk(), stubAgentOK("ok", 0.25))

	res1 := o.ExecuteWithSteps(context.Background(), "task", t.TempDir(), nil, nil, nil)
	if res1.AgentCost != 0.25 {
		t.Fatalf("первый запуск: AgentCost = %v, want 0.25", res1.AgentCost)
	}

	// Второй запуск на том же экземпляре: счётчики обязаны начать с нуля,
	// а не тащить $0.25 и один саб-ран прошлого рана.
	o.client = &http.Client{Transport: mk()}
	res2 := o.ExecuteWithSteps(context.Background(), "task", t.TempDir(), nil, nil, nil)
	if res2.AgentCost != 0.25 {
		t.Errorf("второй запуск: AgentCost = %v, want 0.25 (без накопления между ранами)", res2.AgentCost)
	}
	if o.agentRuns != 1 {
		t.Errorf("agentRuns = %d после второго запуска, want 1", o.agentRuns)
	}
}
