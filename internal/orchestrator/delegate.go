package orchestrator

import (
	"fmt"
	"strings"
)

// ── Делегация через узкий белт ────────────────────────────────────────
//
// run_agent — самый дорогой инструмент оркестратора: каждый вызов это
// отдельный AI-агент со своими тратами. Без рамок модель способна устроить
// неконтролируемую рекурсию делегирования и разогнать стоимость. Политика:
//   - глубина 1: делегировать можно только «листовым» агентам (claude, codex,
//     shell, …); orchestrator/researcher запрещены, agentType нормализуется,
//     поэтому гард не обходится регистром/пробелами (алиасов в GetAgent нет);
//   - лимит саб-ранов на запуск (maxAgentRuns, 0 = без лимита);
//   - бюджет саб-ранов от родителя (maxAgentCostUSD, 0 = без лимита);
//   - узкий белт: AI-саб-агент получает в промпте рамку ask/plan/run/status
//     и запрет на дальнейшую делегацию.

// defaultMaxAgentRuns — дефолтный лимит делегирований на один запуск
// оркестратора. 8 при maxIterations=25: хватает на реальную задачу
// (план → несколько делегирований → проверка), но отсекает бесконечный
// цикл «делегируй ещё раз». Отключается SetMaxAgentRuns(0).
const defaultMaxAgentRuns = 8

// subAgentBelt — рамка, добавляемая к промпту при делегации AI-агенту.
// Короткая, по-русски: только ask/plan/run/status, без самостоятельной
// эскалации и действий вне задачи; ответ — краткий статус + результат.
// shell рамку не получает: там промпт это сама команда, обёртка её сломает.
const subAgentBelt = "[Ты — саб-агент оркестратора. Работай строго по задаче ниже: " +
	"ask/plan/run/status — при критичной неясности задай вопрос, спланируй, выполни, " +
	"верни краткий статус (ok/fail) и результат. Не делегируй дальше, не эскалируй, " +
	"не делай ничего вне задачи.]"

// SetMaxAgentRuns задаёт лимит вызовов run_agent на запуск оркестратора.
// 0 (или отрицательное) отключает лимит. По умолчанию defaultMaxAgentRuns.
func (o *Orchestrator) SetMaxAgentRuns(n int) {
	if n < 0 {
		n = 0
	}
	o.maxAgentRuns = n
}

// SetMaxAgentCostUSD задаёт бюджет суммарной стоимости саб-ранов на запуск —
// родительский лимит трат. 0 = без лимита (это дефолт: стоимость репортит
// не каждый CLI-агент, поэтому бюджет — осознанный opt-in родителя, а не
// скрытый стоп-кран). Учёт post-hoc: стоимость рана известна только после
// него, поэтому отказ срабатывает на следующем делегировании после исчерпания.
func (o *Orchestrator) SetMaxAgentCostUSD(v float64) {
	if v < 0 {
		v = 0
	}
	o.maxAgentCostUSD = v
}

// normalizeAgentType приводит agentType к каноническому ID: без краевых
// пробелов, в нижнем регистре. Без этого гард рекурсии обходился бы
// регистром ("Orchestrator") или пробелами; побочный плюс — "Claude"
// теперь резолвится в claude, а не падает в GetAgent.
func normalizeAgentType(agentType string) string {
	return strings.ToLower(strings.TrimSpace(agentType))
}

// gateDelegation проверяет политику делегирования перед вызовом саб-агента.
// Возвращает "" когда делегировать можно, иначе — текст отказа модели.
func (o *Orchestrator) gateDelegation(agent string) string {
	// Глубина 1: orchestrator/researcher — не листовые агенты. agent уже
	// нормализован, алиасов в реестре нет — точечного сравнения достаточно.
	if agent == "orchestrator" || agent == "researcher" {
		return fmt.Sprintf("cannot delegate to %s (infinite recursion, depth=1)", agent)
	}
	if o.maxAgentRuns > 0 && o.agentRuns >= o.maxAgentRuns {
		return fmt.Sprintf("делегирование отклонено: лимит саб-ранов исчерпан (%d/%d на этот запуск). "+
			"Доделай задачу своими инструментами (run_command, write_file) или вызывай finish.",
			o.agentRuns, o.maxAgentRuns)
	}
	if o.maxAgentCostUSD > 0 && o.agentCost >= o.maxAgentCostUSD {
		remaining := o.maxAgentCostUSD - o.agentCost
		if remaining < 0 {
			remaining = 0
		}
		return fmt.Sprintf("делегирование отклонено: бюджет саб-агентов исчерпан "+
			"(потрачено $%.4f из $%.2f, остаток $%.4f). Доделай своими инструментами или вызывай finish.",
			o.agentCost, o.maxAgentCostUSD, remaining)
	}
	return ""
}
