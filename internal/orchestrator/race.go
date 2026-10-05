package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"
)

// ── Best-of-N гонка ───────────────────────────────────────────────────
//
// Одна задача параллельно 2-4 агентам (claude vs codex vs kimi …), затем
// независимая модель-судья выбирает лучшее решение, и его diff применяется
// к рабочей директории. Дизайн-решения:
//
//   - Конфликт правок в одном cwd: CLI-агенты реально пишут в файлы, и гонка
//     N агентов в одной директории перемешала бы их правки. Поэтому гонка —
//     «на ответ/патч»: каждый кандидат получает пояс (raceCandidateBelt) с
//     запретом изменять файлы и возвращает решение текстом — unified diff в
//     блоке ```diff. Победивший diff применяет отдельный шаг (git apply
//     --check, затем git apply). Агент, проигнорировавший пояс и написавший
//     в файлы, — известное ограничение: git apply упадёт на --check, и гонка
//     честно отрапортует ApplyError вместо молчаливой порчи дерева.
//   - Судья — модель через OpenRouter (callAPIModel), а не ещё один CLI-
//     агент: ревью это один короткий completion, поднимать ради него CLI
//     дорого и медленно. Независимость обеспечивается анонимностью: судья
//     получает решения как «Кандидат A/B/C» без имён агентов, поэтому
//     само-ревью по имени невозможно даже при совпадении моделей.
//   - Вердикт — JSON {"winner": "A", "reason": "..."}; парсинг устойчивый
//     (fenced ```json, сбалансированный {...}, маркер WINNER: A).
//   - Бюджет MaxCostUSD — post-hoc (стоимость CLI-рана известна только после
//     него, как в delegate.go): превышение отменяет судью и применение, но
//     результаты кандидатов сохраняются в отчёте.

const (
	// maxRaceAgents — верхняя граница кандидатов: буквы A..Z формально хватает,
	// но гонка больше 4 — это уже распыление бюджета, а не сравнение.
	maxRaceAgents = 4
	// maxRaceAnswerRunes — обрезка ответа кандидата в промпте судьи: полный
	// лог CLI-агента в контекст судьи не помещается и не нужен.
	maxRaceAnswerRunes = 12000
)

// raceCandidateBelt — рамка кандидата. Главное: НЕ изменять рабочую
// директорию (правит только финальный шаг после выбора победителя) и вернуть
// правки unified diff'ом в блоке ```diff, иначе применять будет нечего.
const raceCandidateBelt = "[Ты — один из нескольких кандидатов в гонке решений (best-of-N). " +
	"Рабочую директорию НЕ изменяй: файлы правит только финальный шаг после выбора победителя. " +
	"Изучи код (читать можно), реши задачу и верни: (1) правки единым unified diff в блоке " +
	"```diff ... ``` (git-формат, пути относительно рабочей директории; если правки не нужны — " +
	"без блока), (2) краткое объяснение решения. Не делегируй дальше.]"

const raceJudgeSystemPrompt = `You are an impartial judge of a best-of-N coding race.
You receive a task and several anonymous solutions (Candidate A, B, C...).
The executor names are hidden on purpose — judge only the content.
Pick the best solution by correctness, completeness and safety of the changes.
Answer STRICTLY with one JSON object: {"winner": "<letter>", "reason": "<1-3 sentences>"}.
No other text. Reason — in Russian.`

// RaceConfig задаёт параметры гонки.
type RaceConfig struct {
	Agents []string // 2..maxRaceAgents кандидатов: claude, codex, kimi, …
	Judge  string   // short name модели-судьи (sonnet, gpt-4o, …); "" = модель оркестратора
	// MaxCostUSD — бюджет суммарной стоимости кандидатов; 0 = без лимита.
	// Post-hoc: отказ срабатывает после финиша кандидатов, отменяя судью и apply.
	MaxCostUSD float64
	// NoApply отключает применение победившего diff (только отчёт). По
	// умолчанию (false) патч победителя применяется — это и есть смысл гонки.
	NoApply bool
}

// RaceCandidateResult — результат одного кандидата.
type RaceCandidateResult struct {
	Agent    string
	Text     string
	CostUSD  float64
	IsError  bool
	Duration time.Duration
}

// RaceResult — итог гонки.
type RaceResult struct {
	Winner       string                // agent id победителя; "" — не выбран
	Verdict      string                // обоснование судьи (или причина, почему победителя нет)
	Results      []RaceCandidateResult // по одному на агента, в порядке cfg.Agents
	AppliedPatch bool                  // diff победителя применён к cwd
	ApplyError   string                // почему патч не применён (нет diff, git apply отказал, …)
	TotalCost    float64               // суммарная стоимость кандидатов (судья через API не учитывается)
	IsError      bool                  // гонка не состоялась: отмена, все упали, бюджет, ошибка судьи
}

// ExecuteRace запускает best-of-N гонку: параллельный прогон задачи на
// cfg.Agents, анонимное судейство, применение diff победителя.
func (o *Orchestrator) ExecuteRace(ctx context.Context, task, cwd string, cfg RaceConfig, onProgress ProgressFunc) RaceResult {
	result := &RaceResult{Results: []RaceCandidateResult{}}

	agents, errMsg := validateRaceAgents(cfg.Agents)
	if errMsg != "" {
		result.IsError = true
		result.Verdict = errMsg
		return *result
	}
	if o.runAgent == nil {
		result.IsError = true
		result.Verdict = "гонка недоступна: runAgent не задан"
		return *result
	}
	if err := ctx.Err(); err != nil {
		result.IsError = true
		result.Verdict = "Отменено"
		return *result
	}

	// Артефакты запуска, как у Execute/ExecuteResearch: runs/<runID>/.
	// finish через defer — summary.md появится на любом выходе.
	aw := newArtifactWriter(newRunID(), o.model, task, cwd)
	aw.LogEvent(LogEntry{Event: "run_start"})
	aw.LogEvent(LogEntry{Event: "task", Input: task, Summary: cwd})
	defer func() {
		aw.finish(raceSummary(result, agents), result.TotalCost, result.IsError)
	}()

	if onProgress != nil {
		onProgress(fmt.Sprintf("🏁 Гонка: %s", strings.Join(agents, " vs ")))
	}

	// ── Параллельный прогон кандидатов ──
	// Все в одном cwd: правки текстом, файлы не трогаем (см. шапку файла).
	// Каждая горутина пишет только свой слот results[i] — mutex не нужен.
	prompt := raceCandidateBelt + "\n\nЗадача: " + task
	results := make([]RaceCandidateResult, len(agents))
	var wg sync.WaitGroup
	for i, agent := range agents {
		wg.Add(1)
		go func(i int, agent string) {
			defer wg.Done()
			p := prompt
			if agent == "shell" {
				p = task // как в toolRunAgent: shell получает голую команду без белта
			}
			start := time.Now()
			r := o.runAgent(ctx, agent, p, cwd)
			results[i] = RaceCandidateResult{
				Agent: agent, Text: r.Text, CostUSD: r.CostUSD,
				IsError: r.IsError, Duration: time.Since(start),
			}
			if onProgress != nil {
				status := "ok"
				if r.IsError {
					status = "ОШИБКА"
				}
				onProgress(fmt.Sprintf("🏁 Кандидат %c (%s): %s за %s",
					'A'+i, agent, status, time.Since(start).Truncate(time.Second)))
			}
		}(i, agent)
	}
	wg.Wait()

	result.Results = results
	for i, r := range results {
		result.TotalCost += r.CostUSD
		ev := LogEntry{
			Event: "race_candidate", Tool: fmt.Sprintf("%c:%s", 'A'+i, r.Agent),
			Output: truncateLog(r.Text, 2000), CostUSD: r.CostUSD, DurationMs: r.Duration.Milliseconds(),
		}
		if r.IsError {
			ev.Error = "true"
		}
		aw.LogEvent(ev)
	}

	if ctx.Err() != nil {
		result.IsError = true
		result.Verdict = "Отменено"
		return *result
	}

	// ── Живые кандидаты (буквы — по исходному порядку, не сдвигаются) ──
	var alive []int
	for i, r := range results {
		if !r.IsError {
			alive = append(alive, i)
		}
	}
	if len(alive) == 0 {
		result.IsError = true
		result.Verdict = "все кандидаты завершились с ошибкой — судить нечего"
		return *result
	}

	// ── Бюджет: post-hoc, как лимиты делегирования в delegate.go ──
	if cfg.MaxCostUSD > 0 && result.TotalCost > cfg.MaxCostUSD {
		result.IsError = true
		result.Verdict = fmt.Sprintf("бюджет гонки исчерпан ($%.4f из $%.2f): судья не вызывался, патч не применялся",
			result.TotalCost, cfg.MaxCostUSD)
		return *result
	}

	// ── Судья ──
	judgeModel := o.model
	if cfg.Judge != "" {
		judgeModel = ResolveModel(cfg.Judge)
	}
	if onProgress != nil {
		onProgress(fmt.Sprintf("⚖️ Судья оценивает %d решений...", len(alive)))
	}
	messages := []orMessage{
		{Role: "system", Content: raceJudgeSystemPrompt},
		{Role: "user", Content: buildRaceJudgePrompt(task, cwd, results, alive)},
	}
	resp, err := o.callAPIModel(ctx, judgeModel, messages, nil)
	if err != nil {
		result.IsError = true
		result.Verdict = fmt.Sprintf("ошибка судьи: %v", err)
		return *result
	}
	verdictText := resp.Choices[0].Message.Content
	aw.LogEvent(LogEntry{
		Event: "race_verdict", Tool: judgeModel, Output: truncateLog(verdictText, 2000),
		TokensIn: resp.Usage.PromptTokens, TokensOut: resp.Usage.CompletionTokens,
	})

	idx, reason, ok := parseJudgeVerdict(verdictText, len(results))
	if !ok {
		// Вердикт не распарсован — отчёт с сырым ответом судьи, патч не применяем.
		result.Verdict = verdictText
		result.ApplyError = "вердикт судьи не распарсован"
		return *result
	}
	if results[idx].IsError {
		// Судья видел только живых, но на всякий случай страхуемся от буквы
		// упавшего кандидата (модель способна выдумать её и валидной).
		result.Verdict = reason
		result.ApplyError = "судья выбрал кандидата, завершившегося с ошибкой"
		return *result
	}
	result.Winner = results[idx].Agent
	result.Verdict = reason

	// ── Применение diff победителя ──
	if cfg.NoApply {
		return *result
	}
	diff := extractDiffBlock(results[idx].Text)
	if diff == "" {
		result.ApplyError = "победитель не вернул ```diff-блок"
		return *result
	}
	if onProgress != nil {
		onProgress(fmt.Sprintf("⚖️ Победитель: %s. Применяю патч...", result.Winner))
	}
	if err := applyUnifiedDiff(ctx, cwd, diff); err != nil {
		result.ApplyError = err.Error()
		aw.LogEvent(LogEntry{Event: "race_apply", Error: truncateLog(err.Error(), 500)})
		return *result
	}
	result.AppliedPatch = true
	aw.setPatch(diff) // patch.diff = именно применённый патч, даже вне git-репо
	aw.LogEvent(LogEntry{Event: "race_apply", Output: fmt.Sprintf("applied winner diff (%d bytes)", len(diff))})
	return *result
}

// validateRaceAgents нормализует и проверяет список кандидатов: 2..maxRaceAgents,
// orchestrator/researcher запрещены (рекурсия делегирования, как в gateDelegation).
func validateRaceAgents(list []string) ([]string, string) {
	agents := make([]string, 0, len(list))
	for _, a := range list {
		a = normalizeAgentType(a)
		if a == "" {
			continue
		}
		if a == "orchestrator" || a == "researcher" {
			return nil, fmt.Sprintf("агент %q не может участвовать в гонке (infinite recursion, depth=1)", a)
		}
		agents = append(agents, a)
	}
	if len(agents) < 2 {
		return nil, "гонке нужно минимум 2 агента"
	}
	if len(agents) > maxRaceAgents {
		return nil, fmt.Sprintf("гонке нужно не больше %d агентов (дано %d)", maxRaceAgents, len(agents))
	}
	return agents, ""
}

// buildRaceJudgePrompt собирает промпт судьи: задача + решения кандидатов
// АНОНИМНО (Кандидат A/B/C, без имён агентов) — честное независимое ревью.
func buildRaceJudgePrompt(task, cwd string, results []RaceCandidateResult, alive []int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Задача: %s\nРабочая директория: %s\n\n", task, cwd)
	fmt.Fprintf(&b, "Ниже решения %d анонимных кандидатов. Имена исполнителей скрыты намеренно.\n\n", len(alive))
	for _, i := range alive {
		fmt.Fprintf(&b, "=== Кандидат %c ===\n%s\n\n", 'A'+i, truncate(results[i].Text, maxRaceAnswerRunes))
	}
	b.WriteString("Выбери лучшее решение по корректности, полноте и безопасности правок. " +
		"Ответь строго одним JSON-объектом {\"winner\": \"<буква>\", \"reason\": \"<1-3 предложения>\"}.")
	return b.String()
}

// raceSummary — однострочный итог для summary.md.
func raceSummary(res *RaceResult, agents []string) string {
	if res.Winner == "" {
		return "Гонка (" + strings.Join(agents, ", ") + "): " + res.Verdict
	}
	s := fmt.Sprintf("Гонка (%s): победитель %s. %s", strings.Join(agents, ", "), res.Winner, res.Verdict)
	if res.AppliedPatch {
		s += " Патч применён."
	} else if res.ApplyError != "" {
		s += " Патч не применён: " + res.ApplyError
	}
	return s
}

// ── Парсинг вердикта ──────────────────────────────────────────────────

type judgeVerdictJSON struct {
	Winner string `json:"winner"`
	Reason string `json:"reason"`
}

// raceWinnerMarkerRe — fallback-маркер на случай, если судья ответил не JSON.
var raceWinnerMarkerRe = regexp.MustCompile(`(?i)WINNER\s*[:=]\s*"?([A-Za-z])"?`)

// parseJudgeVerdict извлекает (индекс победителя, обоснование) из ответа
// судьи. Устойчив к обёрткам: fenced ```json, произвольный текст вокруг
// JSON-объекта, маркер WINNER: A. ok=false — победитель не определён.
func parseJudgeVerdict(text string, n int) (idx int, reason string, ok bool) {
	for _, candidate := range judgeJSONCandidates(text) {
		var v judgeVerdictJSON
		if json.Unmarshal([]byte(candidate), &v) != nil {
			continue
		}
		if i, valid := winnerIndex(v.Winner, n); valid {
			if v.Reason == "" {
				v.Reason = text
			}
			return i, v.Reason, true
		}
	}
	if m := raceWinnerMarkerRe.FindStringSubmatch(text); len(m) == 2 {
		if i, valid := winnerIndex(m[1], n); valid {
			return i, text, true
		}
	}
	return 0, "", false
}

// winnerIndex переводит букву кандидата ("A"/"b") в индекс с проверкой диапазона.
func winnerIndex(s string, n int) (int, bool) {
	s = strings.ToUpper(strings.TrimSpace(s))
	if len(s) != 1 || s[0] < 'A' || int(s[0]-'A') >= n {
		return 0, false
	}
	return int(s[0] - 'A'), true
}

// judgeJSONCandidates собирает строки-кандидаты на JSON-объект: сначала тела
// fenced ```json-блоков, затем сбалансированные {...}-фрагменты текста
// (скобки внутри строк и экранирование учитываются).
func judgeJSONCandidates(text string) []string {
	var out []string
	for _, b := range fencedBlocks(text) {
		if b.lang == "json" || b.lang == "" {
			out = append(out, b.body)
		}
	}
	depth, start := 0, -1
	inStr, esc := false, false
	for i := 0; i < len(text); i++ {
		c := text[i]
		if inStr {
			switch {
			case esc:
				esc = false
			case c == '\\':
				esc = true
			case c == '"':
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case '{':
			if depth == 0 {
				start = i
			}
			depth++
		case '}':
			depth--
			if depth == 0 && start >= 0 {
				out = append(out, text[start:i+1])
				start = -1
			}
		}
	}
	return out
}

// fencedBlock — один ```lang ... ```-блок из markdown-текста.
type fencedBlock struct {
	lang string
	body string
}

// fencedBlocks разбирает fenced code blocks. Незакрытый блок отбрасывается.
func fencedBlocks(text string) []fencedBlock {
	var out []fencedBlock
	in := false
	lang := ""
	var body []string
	for _, ln := range strings.Split(text, "\n") {
		t := strings.TrimSpace(ln)
		if strings.HasPrefix(t, "```") {
			if !in {
				in = true
				lang = strings.ToLower(strings.TrimSpace(strings.TrimPrefix(t, "```")))
				body = body[:0]
			} else {
				out = append(out, fencedBlock{lang: lang, body: strings.Join(body, "\n")})
				in = false
			}
			continue
		}
		if in {
			body = append(body, ln)
		}
	}
	return out
}

// extractDiffBlock достаёт unified diff из ответа кандидата: первый fenced-блок
// с языком diff/patch или с "diff --git" внутри. Без блока применять нечего —
// в сыром тексте после diff обычно идёт объяснение, которое сломает git apply.
func extractDiffBlock(text string) string {
	for _, b := range fencedBlocks(text) {
		body := strings.TrimSpace(b.body)
		if body == "" {
			continue
		}
		if b.lang == "diff" || b.lang == "patch" || strings.Contains(body, "diff --git ") {
			return body + "\n"
		}
	}
	return ""
}

// applyUnifiedDiff применяет patch к cwd через git apply. Сначала --check:
// патч, который не ложится целиком, не применяется вообще. Работает и вне
// git-репозитория (git apply как замена patch, пути относительно cwd).
func applyUnifiedDiff(ctx context.Context, cwd, diff string) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	check := gitCmd(ctx, cwd, "apply", "--check", "-")
	check.Stdin = strings.NewReader(diff)
	if out, err := check.CombinedOutput(); err != nil {
		return fmt.Errorf("git apply --check: %v: %s", err, truncate(strings.TrimSpace(string(out)), 300))
	}
	apply := gitCmd(ctx, cwd, "apply", "-")
	apply.Stdin = strings.NewReader(diff)
	if out, err := apply.CombinedOutput(); err != nil {
		return fmt.Errorf("git apply: %v: %s", err, truncate(strings.TrimSpace(string(out)), 300))
	}
	return nil
}

// ── Tool "race" для оркестратора ──────────────────────────────────────

// toolRace исполняет инструмент race из ReAct-цикла: та же ExecuteRace, с
// учётом трат в лимитах родителя. Долгая операция (N агентов + судья),
// итерация оркестратора блокируется — как и на обычном run_agent.
func (o *Orchestrator) toolRace(ctx context.Context, input map[string]any, defaultCwd string) (string, string) {
	agentsStr, _ := input["agents"].(string)
	task, _ := input["task"].(string)
	judge, _ := input["judge"].(string)
	if agentsStr == "" || task == "" {
		return "", "agents and task are required"
	}
	var agents []string
	for _, a := range strings.Split(agentsStr, ",") {
		if a = strings.TrimSpace(a); a != "" {
			agents = append(agents, a)
		}
	}
	// Лимиты/бюджет родителя: гонка потребляет N саб-ранов разом, поэтому
	// проверяем исчерпание ДО старта (учёт трат — post-hoc, ниже).
	if gateErr := o.gateDelegation("race"); gateErr != "" {
		return "", gateErr
	}

	res := o.ExecuteRace(ctx, task, defaultCwd, RaceConfig{Agents: agents, Judge: judge}, nil)

	// Post-hoc учёт в лимитах родителя (философия delegate.go): стоимость и
	// число саб-ранов известны только после финиша.
	o.agentRuns += len(res.Results)
	o.agentCost += res.TotalCost

	var b strings.Builder
	if res.Winner != "" {
		fmt.Fprintf(&b, "Гонка завершена: победитель %s\nВердикт судьи: %s\n", res.Winner, res.Verdict)
		if res.AppliedPatch {
			b.WriteString("Патч: ПРИМЕНЁН к рабочей директории.\n")
		} else if res.ApplyError != "" {
			fmt.Fprintf(&b, "Патч: НЕ применён (%s). Решение победителя доступно в артефактах гонки.\n", res.ApplyError)
		} else {
			b.WriteString("Патч: не применялся.\n")
		}
	} else {
		fmt.Fprintf(&b, "Гонка без победителя: %s\n", res.Verdict)
	}
	fmt.Fprintf(&b, "Стоимость кандидатов: $%.4f\n", res.TotalCost)
	for i, r := range res.Results {
		status := "ok"
		if r.IsError {
			status = "ОШИБКА"
		}
		fmt.Fprintf(&b, "  %c %s: %s, %s, $%.4f\n", 'A'+i, r.Agent, status, r.Duration.Truncate(time.Second), r.CostUSD)
	}

	if res.IsError {
		return strings.TrimSpace(b.String()), "race did not complete"
	}
	return strings.TrimSpace(b.String()), ""
}
