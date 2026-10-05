package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"tgcontrol/internal/procutil"
)

var (
	researchCwdMu    sync.Mutex
	researchCwdLocks = map[string]*sync.Mutex{}
)

// lockResearchCwd serializes research runs that target the same working
// directory — research does destructive git operations and two concurrent runs
// (or a run racing its own cleanup) would corrupt the tree.
func lockResearchCwd(cwd string) func() {
	abs, err := filepath.Abs(cwd)
	if err != nil {
		abs = cwd
	}
	researchCwdMu.Lock()
	l := researchCwdLocks[abs]
	if l == nil {
		l = &sync.Mutex{}
		researchCwdLocks[abs] = l
	}
	researchCwdMu.Unlock()
	l.Lock()
	return l.Unlock
}

// gitTreeDirty reports whether the working tree has uncommitted changes.
func gitTreeDirty(ctx context.Context, cwd string) (bool, error) {
	out, err := gitCmd(ctx, cwd, "status", "--porcelain").Output()
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(string(out)) != "", nil
}

// ── Types ─────────────────────────────────────────────────────────────

// Experiment records a single research experiment.
type Experiment struct {
	ID               int      `json:"id"`
	Description      string   `json:"description"`
	Metric           float64  `json:"metric"`
	Baseline         float64  `json:"baseline"`
	Delta            string   `json:"delta"`
	Kept             bool     `json:"kept"`
	Reverted         bool     `json:"reverted"`
	InvariantsPassed bool     `json:"invariants_passed"`
	FilesChanged     []string `json:"files_changed,omitempty"`
	Diff             string   `json:"diff,omitempty"`
	Timestamp        float64  `json:"timestamp"`
	Error            string   `json:"error,omitempty"`
}

// ResearchResult extends Result with experiment tracking.
type ResearchResult struct {
	Result
	Experiments    []Experiment `json:"experiments"`
	BaselineMetric float64      `json:"baseline_metric"`
	FinalMetric    float64      `json:"final_metric"`
	Improvement    string       `json:"improvement"`
	Branch         string       `json:"branch"`
	FinalDiff      string       `json:"final_diff"`
}

// ResearchConfig defines what to optimize.
type ResearchConfig struct {
	EvalCommand         string   `json:"eval_command"`
	MetricPattern       string   `json:"metric_pattern"`        // regex with capture group
	MetricFile          string   `json:"metric_file,omitempty"` // file where eval writes the number
	MetricName          string   `json:"metric_name"`
	LowerIsBetter       bool     `json:"lower_is_better"`
	MaxExperiments      int      `json:"max_experiments"`
	MaxWallClockMinutes int      `json:"max_wall_clock_minutes"` // 0 = default 120
	MaxCostUSD          float64  `json:"max_cost_usd"`           // 0 = default 5.0
	InvariantsCommand   string   `json:"invariants_command"`     // must exit 0 to pass
	ProtectedPaths      []string `json:"protected_paths"`        // globs that agent cannot modify
}

// ExperimentCallback is called after each experiment completes.
type ExperimentCallback func(exp Experiment)

// ── System prompt ─────────────────────────────────────────────────────

const researchSystemPrompt = `You are an autonomous research agent. You run experiments to optimize a metric.

Your workflow for EACH experiment:
1. git_checkpoint — save current state BEFORE making any changes
2. Read relevant code, analyze what could improve the metric
3. Make changes (run_agent or write_file) — PROTECTED files cannot be modified
4. eval_metric — run the evaluation command, get the metric value
5. The system will AUTOMATICALLY run invariants_check after eval — if it fails, experiment is auto-reverted
6. If metric improved AND invariants passed: keep changes
7. If metric worsened or invariants failed: git_revert
8. Call experiment_done with results
9. Plan next experiment based on what you learned
10. When budget is exhausted or you've converged, call finish

Tools:
- git_checkpoint: Save current state. Call BEFORE each experiment.
- git_revert: Revert to last checkpoint. Call when experiment failed or worsened metric.
- eval_metric: Run eval command, returns the metric value. Invariants are checked automatically after this.
- experiment_done: Log experiment result after each experiment.
- run_agent / run_command / read_file / list_files / write_file: Standard tools.
- finish: All experiments done. Call with final summary.

Rules:
- ALWAYS git_checkpoint before making changes
- ALWAYS eval_metric after changes
- git_revert if metric worsened OR if told invariants failed
- Be scientific: change one thing at a time
- Learn from failed experiments — read the experiment log
- NEVER modify protected files
- NEVER run_agent with agent="orchestrator" or agent="researcher"
- Respond in Russian`

// ── Research tool definitions (OpenAI function-calling format) ─────────

var researchToolDefs = []orTool{
	{Type: "function", Function: orToolDef{
		Name: "git_checkpoint", Description: "Save current state as a git commit. Call BEFORE making changes.",
		Parameters: json.RawMessage(`{"type":"object","properties":{"message":{"type":"string","description":"Checkpoint description"}},"required":["message"]}`),
	}},
	{Type: "function", Function: orToolDef{
		Name: "git_revert", Description: "Revert all changes back to the last checkpoint.",
		Parameters: json.RawMessage(`{"type":"object","properties":{}}`),
	}},
	{Type: "function", Function: orToolDef{
		Name: "eval_metric", Description: "Run evaluation command and extract metric. Invariants checked automatically after.",
		Parameters: json.RawMessage(`{"type":"object","properties":{}}`),
	}},
	{Type: "function", Function: orToolDef{
		Name: "experiment_done", Description: "Log the result of an experiment.",
		Parameters: json.RawMessage(`{"type":"object","properties":{"description":{"type":"string","description":"What was changed"},"metric":{"type":"number","description":"Metric value"},"kept":{"type":"boolean","description":"true=kept, false=reverted"}},"required":["description","metric","kept"]}`),
	}},
}

// ── ExecuteResearch ───────────────────────────────────────────────────

// ExecuteResearch runs the research optimization loop with full safety.
func (o *Orchestrator) ExecuteResearch(ctx context.Context, task, cwd string, cfg ResearchConfig, onProgress ProgressFunc, onExperiment ExperimentCallback, stopCh <-chan struct{}) ResearchResult {
	result := ResearchResult{Experiments: []Experiment{}}

	// Счётчики делегирования — на запуск, как в ExecuteWithSteps.
	o.agentRuns = 0
	o.agentCost = 0
	// Лимит саб-ранов research'у не подходит: на дефолтные 10 экспериментов
	// нужно больше 8 делегирований. Здесь делегирование душат свои бюджеты —
	// MaxExperiments / MaxWallClockMinutes / MaxCostUSD (проверки ниже по циклу).
	o.maxAgentRuns = 0

	// Артефакты запуска (как у ExecuteWithSteps). Defer зарегистрирован первым,
	// поэтому finish выполнится ПОСЛЕДНИМ — когда cleanup-defer ниже уже
	// посчитал result.FinalDiff (его и кладём в patch.diff, без git-fallback).
	aw := newArtifactWriter(newRunID(), o.model, task, cwd)
	aw.LogEvent(LogEntry{Event: "run_start"})
	aw.LogEvent(LogEntry{Event: "task", Input: task, Summary: cwd})
	defer func() {
		aw.setPatch(result.FinalDiff)
		aw.setSteps(len(result.Steps))
		aw.finish(result.Summary, result.AgentCost, result.IsError)
	}()

	// ── Apply defaults ──
	if cfg.MaxExperiments <= 0 {
		cfg.MaxExperiments = 10
	}
	if cfg.MaxWallClockMinutes <= 0 {
		cfg.MaxWallClockMinutes = 120
	}
	if cfg.MaxCostUSD <= 0 {
		cfg.MaxCostUSD = 5.0
	}
	if cfg.MetricPattern == "" && cfg.MetricFile == "" {
		cfg.MetricPattern = `([\d.]+)`
	}

	// ── Compile metric regex ──
	var metricRe *regexp.Regexp
	if cfg.MetricPattern != "" {
		var err error
		metricRe, err = regexp.Compile(cfg.MetricPattern)
		if err != nil {
			result.Summary = fmt.Sprintf("Invalid metric_pattern regex: %v", err)
			result.IsError = true
			return result
		}
	}

	// ── Serialize per-cwd and refuse to run on a dirty tree ──
	// Research performs destructive git ops (reset --hard, clean -fd); running
	// them against uncommitted work — or a concurrent session's edits in the
	// same repo — would silently destroy that work.
	unlock := lockResearchCwd(cwd)
	defer unlock()
	if dirty, derr := gitTreeDirty(ctx, cwd); derr != nil {
		result.Summary = fmt.Sprintf("Research needs a git repo: %v", derr)
		result.IsError = true
		return result
	} else if dirty {
		result.Summary = "Рабочее дерево содержит незакоммиченные изменения. Закоммитьте или спрячьте их (git stash) перед запуском research — он выполняет деструктивные git-операции."
		result.IsError = true
		return result
	}

	// ── Git isolation: stash user work, create research branch ──
	startTime := time.Now()
	originalBranch, branchName, err := setupResearchBranch(ctx, cwd)
	if err != nil {
		result.Summary = fmt.Sprintf("Git setup failed: %v", err)
		result.IsError = true
		return result
	}
	result.Branch = branchName
	log.Printf("[Research] Branch: %s (original: %s)", branchName, originalBranch)

	if onProgress != nil {
		onProgress(fmt.Sprintf("🔬 Research branch: %s", branchName))
	}

	// Cleanup on exit: switch back, generate diff
	defer func() {
		diff, _ := getResearchDiff(ctx, cwd, originalBranch, branchName)
		result.FinalDiff = diff
		switchBranch(ctx, cwd, originalBranch)
		restoreStash(ctx, cwd)
		log.Printf("[Research] Returned to branch %s", originalBranch)
	}()

	// ── Init experiment log file ──
	logPath := filepath.Join(cwd, ".research-log.jsonl")

	// ── Build tools and prompt ──
	allTools := make([]orTool, 0, len(toolDefs)+len(researchToolDefs))
	allTools = append(allTools, toolDefs...)
	allTools = append(allTools, researchToolDefs...)

	direction := "lower is better (minimize)"
	if !cfg.LowerIsBetter {
		direction = "higher is better (maximize)"
	}

	protectedInfo := ""
	if len(cfg.ProtectedPaths) > 0 {
		protectedInfo = fmt.Sprintf("\nProtected paths (DO NOT modify): %s", strings.Join(cfg.ProtectedPaths, ", "))
	}
	invariantsInfo := ""
	if cfg.InvariantsCommand != "" {
		invariantsInfo = fmt.Sprintf("\nInvariants check (runs automatically after eval): %s", cfg.InvariantsCommand)
	}

	userPrompt := fmt.Sprintf(
		"Задача: %s\n\nРабочая директория: %s\n"+
			"Research branch: %s\n\n"+
			"=== RESEARCH CONFIG ===\n"+
			"Eval command: %s\n"+
			"Metric: %s (%s)\n"+
			"Budget: %d experiments, %d min wall clock, $%.2f max cost%s%s\n"+
			"======================\n\n"+
			"Start by running eval_metric to establish the baseline, then begin experiments.",
		task, cwd, branchName, cfg.EvalCommand, cfg.MetricName, direction,
		cfg.MaxExperiments, cfg.MaxWallClockMinutes, cfg.MaxCostUSD,
		protectedInfo, invariantsInfo,
	)

	messages := []orMessage{
		{Role: "system", Content: researchSystemPrompt},
		{Role: "user", Content: userPrompt},
	}

	// ── State ──
	expCount := 0
	var baselineMetric float64
	baselineSet := false
	lastInvariantsResult := "" // track for feedback to LLM

	maxIter := cfg.MaxExperiments * 8
	if maxIter > 200 {
		maxIter = 200
	}

	// ── Main loop ──
	for i := range maxIter {
		// ── Budget checks ──
		elapsed := time.Since(startTime)
		if elapsed > time.Duration(cfg.MaxWallClockMinutes)*time.Minute {
			result.Summary = fmt.Sprintf("Wall clock budget exceeded (%d min)", cfg.MaxWallClockMinutes)
			break
		}
		if o.agentCost > cfg.MaxCostUSD {
			result.Summary = fmt.Sprintf("Cost budget exceeded ($%.2f > $%.2f)", o.agentCost, cfg.MaxCostUSD)
			break
		}

		// ── Context/stop checks ──
		select {
		case <-ctx.Done():
			result.Summary = "Отменено"
			result.IsError = true
			result.AgentCost = o.agentCost
			return result
		default:
		}
		if stopCh != nil {
			select {
			case <-stopCh:
				result.Summary = "Прервано пользователем"
				result.IsError = true
				result.AgentCost = o.agentCost
				return result
			default:
			}
		}

		if onProgress != nil {
			budget := fmt.Sprintf("exp %d/%d | %s | $%.2f/$%.2f",
				expCount, cfg.MaxExperiments,
				elapsed.Truncate(time.Second), o.agentCost, cfg.MaxCostUSD)
			onProgress(fmt.Sprintf("🔬 iter %d | %s", i+1, budget))
		}

		trimToolOutputs(messages, 8) // bound context/token growth across experiments
		resp, err := o.callAPI(ctx, messages, allTools)
		if err != nil {
			result.Summary = fmt.Sprintf("Ошибка API: %v", err)
			result.IsError = true
			break
		}

		if len(resp.Choices) == 0 {
			result.Summary = "Пустой ответ API"
			result.IsError = true
			break
		}

		msg := resp.Choices[0].Message
		messages = append(messages, msg)

		if len(msg.ToolCalls) == 0 {
			if msg.Content != "" {
				result.Summary = msg.Content
			} else {
				result.Summary = "Завершено"
			}
			break
		}

		// ── Execute tool calls ──
		for _, tc := range msg.ToolCalls {
			var input map[string]any
			json.Unmarshal([]byte(tc.Function.Arguments), &input)
			toolName := tc.Function.Name

			if toolName == "finish" {
				summary, _ := input["summary"].(string)
				if summary != "" {
					result.Summary = summary
				} else {
					result.Summary = "Завершено"
				}
				result.AgentCost = o.agentCost
				fillResearchSummary(&result, baselineMetric)
				return result
			}

			if onProgress != nil {
				onProgress(fmt.Sprintf("🔧 %s", briefResearchTool(toolName, input)))
			}

			log.Printf("[Research] tool=%s", toolName)

			var output, toolErr string
			switch toolName {
			case "git_checkpoint":
				output, toolErr = toolGitCheckpoint(ctx, input, cwd)

			case "git_revert":
				output, toolErr = toolGitRevert(ctx, cwd)

			case "eval_metric":
				output, toolErr = extractMetric(ctx, cfg, metricRe, cwd)
				if toolErr == "" && !baselineSet {
					if val, parseErr := strconv.ParseFloat(strings.TrimSpace(output), 64); parseErr == nil {
						baselineMetric = val
						baselineSet = true
						result.BaselineMetric = val
					}
				}
				// ── AUTO-RUN INVARIANTS ──
				if toolErr == "" && cfg.InvariantsCommand != "" {
					invOut, invErr := runInvariantsCheck(ctx, cfg.InvariantsCommand, cwd)
					if invErr != "" {
						lastInvariantsResult = "FAILED: " + invErr
						output += "\n\nINVARIANTS FAILED: " + invErr + "\n" + invOut +
							"\nExperiment must be reverted. Call git_revert then experiment_done with kept=false."
						log.Printf("[Research] Invariants FAILED: %s", invErr)
					} else {
						lastInvariantsResult = "PASSED"
						output += "\n\nInvariants passed."
					}
				}

			case "experiment_done":
				invPassed := lastInvariantsResult == "PASSED" || cfg.InvariantsCommand == ""
				output, toolErr = o.recordExperiment(ctx, input, baselineMetric, cfg, invPassed,
					&expCount, &result, onExperiment, cwd, logPath)
				lastInvariantsResult = ""

			case "write_file":
				path, _ := input["path"].(string)
				if isProtected(path, cwd, cfg.ProtectedPaths) {
					output = ""
					toolErr = fmt.Sprintf("PROTECTED: %s cannot be modified", path)
				} else if gateErr := o.gateProtected("write_file", input, cwd); gateErr != "" {
					// Дефолтная политика (секреты, .git, миграции) — с эскалацией
					// к человеку, как в обычном оркестраторе.
					output = ""
					toolErr = gateErr
				} else {
					output, toolErr = o.toolWriteFile(input, cwd)
				}

			case "run_agent":
				if gateErr := o.gateProtected("run_agent", input, cwd); gateErr != "" {
					// Промпт явно зовёт защищённый путь — та же эскалация, что в execTool.
					output, toolErr = "", gateErr
					break
				}
				output, toolErr = o.toolRunAgent(ctx, input, cwd)
				if toolErr == "" && len(cfg.ProtectedPaths) > 0 {
					violated := checkProtectedViolation(ctx, cwd, cfg.ProtectedPaths)
					if len(violated) > 0 {
						toolGitRevert(ctx, cwd)
						output += fmt.Sprintf("\n\nAgent modified protected files: %s — auto-reverted",
							strings.Join(violated, ", "))
						toolErr = "protected files modified by agent"
					}
				}

			default:
				output, toolErr = o.execTool(ctx, toolName, input, cwd)
			}

			step := Step{Tool: toolName, Input: briefResearchTool(toolName, input)}
			content := truncate(output, maxToolOutput)
			if toolErr != "" {
				step.Error = toolErr
				content = fmt.Sprintf("Error: %s\n%s", toolErr, content)
			} else {
				step.Output = truncate(output, 200)
			}
			result.Steps = append(result.Steps, step)

			messages = append(messages, orMessage{
				Role:       "tool",
				ToolCallID: tc.ID,
				Content:    content,
			})
		}
	}

	result.AgentCost = o.agentCost
	fillResearchSummary(&result, baselineMetric)
	return result
}

// ── Git isolation ─────────────────────────────────────────────────────

func setupResearchBranch(ctx context.Context, cwd string) (originalBranch, newBranch string, err error) {
	// Get current branch
	out, e := gitCmd(ctx, cwd, "rev-parse", "--abbrev-ref", "HEAD").Output()
	if e != nil {
		return "", "", fmt.Errorf("not a git repo or no commits: %w", e)
	}
	originalBranch = strings.TrimSpace(string(out))

	// Stash uncommitted work
	gitCmd(ctx, cwd, "stash", "push", "-m",
		fmt.Sprintf("research-stash-%d", time.Now().Unix())).CombinedOutput()

	// Create research branch
	newBranch = fmt.Sprintf("orch-research-%d", time.Now().Unix())
	if _, e := gitCmd(ctx, cwd, "checkout", "-b", newBranch).CombinedOutput(); e != nil {
		return originalBranch, "", fmt.Errorf("cannot create branch: %w", e)
	}
	return originalBranch, newBranch, nil
}

func switchBranch(ctx context.Context, cwd, branch string) {
	gitCmd(ctx, cwd, "checkout", branch).CombinedOutput()
}

func restoreStash(ctx context.Context, cwd string) {
	// Попить ИМЕННО наш research-stash по точному ref, а не верхний stash@{0}:
	// наверху может оказаться стэш пользователя (аудит 2026-06-20).
	out, _ := gitCmd(ctx, cwd, "stash", "list").Output()
	for _, line := range strings.Split(string(out), "\n") {
		if strings.Contains(line, "research-stash-") {
			ref := strings.TrimSpace(strings.SplitN(line, ":", 2)[0]) // "stash@{N}"
			if ref != "" {
				gitCmd(ctx, cwd, "stash", "pop", ref).CombinedOutput()
			}
			return
		}
	}
}

func getResearchDiff(ctx context.Context, cwd, baseBranch, researchBranch string) (string, error) {
	out, err := gitCmd(ctx, cwd, "diff", baseBranch+"..."+researchBranch).Output()
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// ── Git tools ─────────────────────────────────────────────────────────

func toolGitCheckpoint(ctx context.Context, input map[string]any, cwd string) (string, string) {
	message, _ := input["message"].(string)
	if message == "" {
		message = "research checkpoint"
	}
	gitCmd(ctx, cwd, "add", "-A").CombinedOutput()
	out, err := gitCmd(ctx, cwd, "commit", "-m", "[research] "+message, "--allow-empty").CombinedOutput()
	if err != nil {
		return strings.TrimSpace(string(out)), err.Error()
	}
	return "Checkpoint saved: " + message, ""
}

func toolGitRevert(ctx context.Context, cwd string) (string, string) {
	// Откатываем ТОЛЬКО свой checkpoint: HEAD обязан быть [research]-коммитом.
	// Иначе reset --hard HEAD~1 + clean -fd снесли бы настоящий пользовательский
	// коммит и его untracked-файлы (аудит 2026-06-20).
	subj, err := gitCmd(ctx, cwd, "log", "-1", "--pretty=%s").Output()
	if err != nil {
		return "", "cannot read HEAD: " + err.Error()
	}
	if !strings.HasPrefix(strings.TrimSpace(string(subj)), "[research]") {
		return "", "refusing to revert: HEAD is not a [research] checkpoint — nothing of ours to undo"
	}
	gitCmd(ctx, cwd, "checkout", ".").CombinedOutput()
	gitCmd(ctx, cwd, "clean", "-fd").CombinedOutput()
	out, err := gitCmd(ctx, cwd, "reset", "--hard", "HEAD~1").CombinedOutput()
	if err != nil {
		return strings.TrimSpace(string(out)), err.Error()
	}
	return "Reverted to last checkpoint", ""
}

// ── Metric extraction ─────────────────────────────────────────────────

func extractMetric(ctx context.Context, cfg ResearchConfig, metricRe *regexp.Regexp, cwd string) (string, string) {
	// Run eval command
	timeoutCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()

	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.CommandContext(timeoutCtx, "cmd", "/c", cfg.EvalCommand)
	} else {
		cmd = exec.CommandContext(timeoutCtx, "sh", "-c", cfg.EvalCommand)
	}
	// Eval-команда гоняется на каждый эксперимент, её вывод парсится regex'ом.
	// Окно тут — только мигание на рабочем столе, гасим.
	procutil.Hidden(cmd)
	procutil.Prepare(cmd) // kill the whole eval tree on cancel/timeout
	cmd.Dir = cwd

	out, err := cmd.CombinedOutput()
	evalOutput := strings.TrimSpace(string(out))
	if err != nil {
		return evalOutput, fmt.Sprintf("eval command failed (exit non-zero): %v\nThis experiment should be marked as FAILED.", err)
	}

	// Extract metric
	var metricStr string

	if cfg.MetricFile != "" {
		// Method 1: Read from file
		filePath := cfg.MetricFile
		if !filepath.IsAbs(filePath) {
			filePath = filepath.Join(cwd, filePath)
		}
		data, err := os.ReadFile(filePath)
		if err != nil {
			return evalOutput, fmt.Sprintf("cannot read metric file %s: %v\nExperiment FAILED.", cfg.MetricFile, err)
		}
		metricStr = strings.TrimSpace(string(data))
	} else if metricRe != nil {
		// Method 2: Regex extraction
		matches := metricRe.FindStringSubmatch(evalOutput)
		if len(matches) < 2 {
			return evalOutput, fmt.Sprintf("metric pattern %q not found in output.\nExperiment FAILED — metric could not be extracted.", metricRe.String())
		}
		metricStr = matches[1]
	} else {
		return evalOutput, "no metric extraction method configured"
	}

	val, err := strconv.ParseFloat(metricStr, 64)
	if err != nil {
		return evalOutput, fmt.Sprintf("cannot parse metric %q as number: %v\nExperiment FAILED.", metricStr, err)
	}

	return fmt.Sprintf("%.6g", val), ""
}

// ── Invariants check ──────────────────────────────────────────────────

func runInvariantsCheck(ctx context.Context, command, cwd string) (output, errMsg string) {
	timeoutCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.CommandContext(timeoutCtx, "cmd", "/c", command)
	} else {
		cmd = exec.CommandContext(timeoutCtx, "sh", "-c", command)
	}
	// Проверка инвариантов — та же фоновая проба, результат читает оркестратор.
	procutil.Hidden(cmd)
	procutil.Prepare(cmd) // kill the whole invariants-check tree on cancel/timeout
	cmd.Dir = cwd

	out, err := cmd.CombinedOutput()
	text := strings.TrimSpace(string(out))
	if err != nil {
		return text, fmt.Sprintf("invariants check failed: %v", err)
	}
	return text, ""
}

// ── Protected paths ───────────────────────────────────────────────────

func isProtected(path, cwd string, patterns []string) bool {
	if len(patterns) == 0 {
		return false
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(cwd, path)
	}
	path = filepath.Clean(path)
	for _, pattern := range patterns {
		absPattern := pattern
		if !filepath.IsAbs(absPattern) {
			absPattern = filepath.Join(cwd, absPattern)
		}
		absPattern = filepath.Clean(absPattern)
		if matched, _ := filepath.Match(absPattern, path); matched {
			return true
		}
		// Directory protection: путь — сама директория или ВНУТРИ неё.
		// Сравнение по границе сегмента (vendor ≠ vendor2), не по сырому префиксу.
		if path == absPattern || strings.HasPrefix(path, absPattern+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

func checkProtectedViolation(ctx context.Context, cwd string, patterns []string) []string {
	out, _ := gitCmd(ctx, cwd, "diff", "--name-only", "HEAD").Output()
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	var violated []string
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if isProtected(line, cwd, patterns) {
			violated = append(violated, line)
		}
	}
	return violated
}

// ── Experiment recording + JSONL log ──────────────────────────────────

// parseMetricValue принимает metric из tool-аргументов. Модель обычно шлёт
// number, но нередко — строку ("0.95"): type assertion в float64 тогда молча
// давал 0 и портил статистику эксперимента (дельта, FinalMetric, kept-решения).
func parseMetricValue(v any) float64 {
	switch m := v.(type) {
	case float64:
		return m
	case string:
		f, _ := strconv.ParseFloat(strings.TrimSpace(m), 64)
		return f
	}
	return 0
}

func (o *Orchestrator) recordExperiment(ctx context.Context, input map[string]any, baseline float64, cfg ResearchConfig, invPassed bool, expCount *int, result *ResearchResult, onExperiment ExperimentCallback, cwd, logPath string) (string, string) {
	desc, _ := input["description"].(string)
	metric := parseMetricValue(input["metric"])
	kept, _ := input["kept"].(bool)

	*expCount++

	// Calculate delta
	var delta string
	if baseline != 0 {
		pct := (metric - baseline) / baseline * 100
		delta = fmt.Sprintf("%+.1f%%", pct)
	}

	// Get changed files
	filesOut, _ := gitCmd(ctx, cwd, "diff", "--name-only", "HEAD~1").Output()
	var files []string
	for _, f := range strings.Split(strings.TrimSpace(string(filesOut)), "\n") {
		f = strings.TrimSpace(f)
		if f != "" {
			files = append(files, f)
		}
	}

	// Get diff for this experiment
	diffOut, _ := gitCmd(ctx, cwd, "diff", "HEAD~1").Output()

	exp := Experiment{
		ID:               *expCount,
		Description:      desc,
		Metric:           metric,
		Baseline:         baseline,
		Delta:            delta,
		Kept:             kept,
		Reverted:         !kept,
		InvariantsPassed: invPassed,
		FilesChanged:     files,
		Diff:             truncate(string(diffOut), 10000),
		Timestamp:        float64(time.Now().UnixMilli()) / 1000,
	}
	result.Experiments = append(result.Experiments, exp)

	if onExperiment != nil {
		onExperiment(exp)
	}

	if kept {
		result.FinalMetric = metric
	}

	// ── Persistent JSONL log ──
	logEntry, _ := json.Marshal(map[string]any{
		"timestamp":         time.Now().UTC().Format(time.RFC3339),
		"experiment_id":     *expCount,
		"description":       desc,
		"metric":            metric,
		"baseline":          baseline,
		"delta":             delta,
		"kept":              kept,
		"invariants_passed": invPassed,
		"files_changed":     files,
	})
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err == nil {
		f.Write(append(logEntry, '\n'))
		f.Close()
	}

	icon := "✅ KEEP"
	if !kept {
		icon = "❌ REVERT"
	}
	invStatus := ""
	if !invPassed {
		invStatus = " | invariants FAILED"
	}

	log.Printf("[Research] Exp %d: %s | metric=%.6g delta=%s %s%s",
		*expCount, desc, metric, delta, icon, invStatus)

	return fmt.Sprintf("Experiment %d logged: %s (metric: %.6g, %s) %s%s",
		*expCount, desc, metric, delta, icon, invStatus), ""
}

// ── Helpers ───────────────────────────────────────────────────────────

func gitCmd(ctx context.Context, cwd string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "git", args...)
	// Через gitCmd идут ВСЕ git-вызовы research (status, diff, checkout, commit,
	// stash…) — по десятку на эксперимент. Каждый без Hidden = отдельная вспышка
	// консоли: именно так и выглядят «сами открывающиеся пустые терминалы».
	procutil.Hidden(cmd)
	cmd.Dir = cwd
	// Неинтерактивно: не зависать на pager/credential-prompt; и убивать дерево
	// процессов на отмене/таймауте (как остальные exec-сайты research).
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_PAGER=cat", "GIT_OPTIONAL_LOCKS=0")
	procutil.Prepare(cmd)
	return cmd
}

func fillResearchSummary(result *ResearchResult, baseline float64) {
	if len(result.Experiments) == 0 {
		return
	}
	lastKept := baseline
	for _, exp := range result.Experiments {
		if exp.Kept {
			lastKept = exp.Metric
		}
	}
	result.FinalMetric = lastKept
	if baseline != 0 {
		pct := (lastKept - baseline) / baseline * 100
		result.Improvement = fmt.Sprintf("%.6g -> %.6g (%.1f%%)", baseline, lastKept, pct)
	}
}

func briefResearchTool(name string, input map[string]any) string {
	switch name {
	case "git_checkpoint":
		msg, _ := input["message"].(string)
		return "git_checkpoint: " + truncate(msg, 60)
	case "git_revert":
		return "git_revert"
	case "eval_metric":
		return "eval_metric"
	case "experiment_done":
		desc, _ := input["description"].(string)
		return "experiment_done: " + truncate(desc, 60)
	}
	return briefTool(name, input)
}
