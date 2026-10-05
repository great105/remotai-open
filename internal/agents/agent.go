package agents

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"tgcontrol/internal/config"
	"tgcontrol/internal/orchestrator"
	"tgcontrol/internal/procutil"
)

// Response is the result of running an agent.
type Response struct {
	Text      string  `json:"text"`
	SessionID string  `json:"session_id,omitempty"`
	IsError   bool    `json:"is_error"`
	CostUSD   float64 `json:"cost_usd,omitempty"`
}

// ProgressFunc is called with progress updates during agent execution.
type ProgressFunc func(text string)

// QuestionFunc is called when an agent asks the user a question.
type QuestionFunc func(question string, options []string)

// StepFunc is called with structured orchestrator step events.
type StepFunc func(step orchestrator.StepEvent)

// RunOptions holds parameters for an agent run.
type RunOptions struct {
	Prompt        string
	Cwd           string
	SessionID     string            // for resume
	SessionConfig map[string]string // per-session overrides
	// ContinuationPath — путь к continuation packet'у (дельта разговора
	// предыдущего агента, см. continuation.go). В промпт подставляется только
	// путь, агент читает файл сам — экономит токены при смене агента в треде.
	ContinuationPath string
	OnProgress       ProgressFunc
	OnStep           StepFunc        // called with detailed orchestrator steps
	OnQuestion       QuestionFunc    // called when agent asks a question
	StopCh           <-chan struct{} // close to signal stop
}

// Agent is the interface all agent adapters implement.
type Agent interface {
	Run(ctx context.Context, opts RunOptions) Response
}

// withStopAndTimeout returns a context that is cancelled when the parent is
// done, when stopCh is closed (user pressed Stop), or after timeout — whichever
// comes first. The returned cancel must be called to release the watcher.
func withStopAndTimeout(parent context.Context, stopCh <-chan struct{}, timeout time.Duration) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(parent, timeout)
	if stopCh != nil {
		go func() {
			select {
			case <-stopCh:
				cancel()
			case <-ctx.Done():
			}
		}()
	}
	return ctx, cancel
}

// ── Shell Agent ──────────────────────────────────────────────────────

type ShellAgent struct{}

func (s *ShellAgent) Run(ctx context.Context, opts RunOptions) Response {
	// Таймаут + реакция на Stop: иначе зависшая команда держит сессию busy
	// бесконечно, а пользовательский «стоп» её не прерывает.
	ctx, cancel := withStopAndTimeout(ctx, opts.StopCh, 10*time.Minute)
	defer cancel()
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.CommandContext(ctx, "cmd", "/c", opts.Prompt)
	} else {
		cmd = exec.CommandContext(ctx, "sh", "-c", opts.Prompt)
	}
	// Команда пришла с телефона, а вывод забирает CombinedOutput и отдаёт в чат:
	// человеку у ПК показывать нечего, без Hidden он видит лишь вспышку cmd.exe.
	procutil.Hidden(cmd)
	procutil.Prepare(cmd) // kill the whole tree on cancel, not just cmd.exe/sh
	cmd.Dir = opts.Cwd

	out, err := cmd.CombinedOutput()
	text := strings.TrimSpace(string(out))
	if text == "" {
		text = "(no output)"
	}
	isError := false
	if err != nil {
		isError = true
		if text == "" {
			text = fmt.Sprintf("Error: %v", err)
		}
	}
	return Response{Text: text, IsError: isError}
}

// ── CLI Agent (generic) ──────────────────────────────────────────────

type CLIAgent struct {
	Descriptor *AgentDescriptor
}

func (a *CLIAgent) buildCmd(prompt, sessionID string) []string {
	desc := a.Descriptor
	cli := desc.Path()

	template := desc.RunArgs
	if sessionID != "" && desc.ResumeArgs != nil {
		template = desc.ResumeArgs
	}

	args := make([]string, 0, len(template)+len(desc.ExtraFlags)+1)
	args = append(args, cli)
	for _, arg := range template {
		arg = strings.ReplaceAll(arg, "{prompt}", prompt)
		arg = strings.ReplaceAll(arg, "{session_id}", sessionID)
		args = append(args, arg)
	}
	args = append(args, desc.ExtraFlags...)
	return args
}

func (a *CLIAgent) Run(ctx context.Context, opts RunOptions) Response {
	args := a.buildCmd(withContinuation(opts.Prompt, opts.ContinuationPath), opts.SessionID)
	if len(args) == 0 {
		return Response{Text: "No command to run", IsError: true}
	}

	if opts.OnProgress != nil {
		opts.OnProgress(fmt.Sprintf("%s Запуск %s...", a.Descriptor.Icon, a.Descriptor.Name))
	}

	timeoutCtx, cancel := withStopAndTimeout(ctx, opts.StopCh, 10*time.Minute)
	defer cancel()

	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmdArgs := append([]string{"/c"}, args...)
		cmd = exec.CommandContext(timeoutCtx, "cmd", cmdArgs...)
	} else {
		cmd = exec.CommandContext(timeoutCtx, args[0], args[1:]...)
	}
	// Агент работает в фоне по запросу с телефона: на Windows это `cmd /c <agent>`,
	// то есть чёрное окно поверх рабочего стола на всё время запуска. Гасим.
	procutil.Hidden(cmd)
	procutil.Prepare(cmd) // kill the whole tree on cancel/timeout, not just cmd.exe
	cmd.Dir = opts.Cwd

	out, err := cmd.CombinedOutput()
	text := strings.TrimSpace(string(out))
	if text == "" {
		text = "(empty response)"
	}
	isError := err != nil
	return Response{Text: text, IsError: isError}
}

// ── Claude Agent (streaming NDJSON) ──────────────────────────────────

type ClaudeAgent struct{}

// Tool icons for progress messages.
var toolIcons = map[string]string{
	"Read": "📖", "Write": "✏️", "Edit": "✏️",
	"Bash": "🐚", "Glob": "🔍", "Grep": "🔍",
	"WebFetch": "🌐", "WebSearch": "🌐",
	"Task": "📋", "TodoWrite": "📋", "Agent": "🤖",
}

const progressThrottle = 3 * time.Second

func toolSummary(name string, input map[string]any) string {
	switch name {
	case "Read", "Glob", "Grep":
		for _, key := range []string{"file_path", "path", "pattern"} {
			if v, ok := input[key].(string); ok && v != "" {
				parts := strings.Split(strings.ReplaceAll(v, "\\", "/"), "/")
				if len(parts) > 2 {
					return strings.Join(parts[len(parts)-2:], "/")
				}
				return v
			}
		}
	case "Write", "Edit":
		if v, ok := input["file_path"].(string); ok && v != "" {
			parts := strings.Split(strings.ReplaceAll(v, "\\", "/"), "/")
			if len(parts) > 2 {
				return strings.Join(parts[len(parts)-2:], "/")
			}
			return v
		}
	case "Bash":
		if cmd, ok := input["command"].(string); ok {
			if idx := strings.LastIndex(cmd, " && "); idx >= 0 {
				cmd = cmd[idx+4:]
			}
			if len(cmd) > 120 {
				return cmd[:120] + "…"
			}
			return cmd
		}
	case "WebSearch":
		if v, ok := input["query"].(string); ok {
			if len(v) > 80 {
				return v[:80]
			}
			return v
		}
	case "WebFetch":
		if v, ok := input["url"].(string); ok {
			if len(v) > 80 {
				return v[:80]
			}
			return v
		}
	}
	return ""
}

func (a *ClaudeAgent) Run(ctx context.Context, opts RunOptions) Response {
	desc := GetDescriptor("claude")
	if desc == nil || !desc.IsDetected() {
		return Response{Text: "Claude CLI not found", IsError: true}
	}

	// Build command with streaming flags
	cli := desc.Path()
	prompt := withContinuation(opts.Prompt, opts.ContinuationPath)
	var args []string
	if opts.SessionID != "" {
		args = []string{cli, "--resume", opts.SessionID, "-p", prompt, "--output-format", "stream-json", "--verbose"}
	} else {
		args = []string{cli, "-p", prompt, "--output-format", "stream-json", "--verbose"}
	}

	// Apply model/permission from session config, falling back to global config
	sc := opts.SessionConfig
	cfg := config.Get()

	model := mapGet(sc, "claude_model")
	if model == "" {
		model = cfg.ClaudeModel
	}
	if model != "" {
		args = append(args, "--model", model)
	}
	permMode := mapGet(sc, "claude_permission_mode")
	if permMode == "" {
		permMode = cfg.ClaudePermissionMode
	}
	if permMode != "" {
		args = append(args, "--permission-mode", permMode)
	}
	effort := mapGet(sc, "claude_effort")
	if effort != "" {
		args = append(args, "--effort", effort)
	}

	if opts.OnProgress != nil {
		opts.OnProgress("🟠 Запуск Claude Code...")
	}

	timeoutCtx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()

	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmdArgs := append([]string{"/c"}, args...)
		cmd = exec.CommandContext(timeoutCtx, "cmd", cmdArgs...)
	} else {
		cmd = exec.CommandContext(timeoutCtx, args[0], args[1:]...)
	}
	// Общение с claude идёт по пайпам stdout/stderr, окно ни при чём: без Hidden
	// на весь запуск (до 30 минут) повисает пустое окно cmd.exe/claude.cmd.
	procutil.Hidden(cmd)
	procutil.Prepare(cmd) // kill the whole tree on cancel/timeout, not just cmd.exe
	cmd.Dir = opts.Cwd
	cmd.Env = filterEnv(os.Environ(), "CLAUDECODE")

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return Response{Text: fmt.Sprintf("Failed to create pipe: %v", err), IsError: true}
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return Response{Text: fmt.Sprintf("Failed to create stderr pipe: %v", err), IsError: true}
	}

	log.Printf("[Claude] Starting: %s", quoteArgs(args))
	if err := cmd.Start(); err != nil {
		return Response{Text: fmt.Sprintf("Failed to start: %v", err), IsError: true}
	}

	// Capture stderr in background (synchronized via stderrDone channel)
	var stderrBuf strings.Builder
	stderrDone := make(chan struct{})
	go func() {
		defer close(stderrDone)
		s := bufio.NewScanner(stderr)
		for s.Scan() {
			line := s.Text()
			log.Printf("[Claude][stderr] %s", line)
			stderrBuf.WriteString(line + "\n")
		}
	}()

	// Parse NDJSON stream
	var allTexts []string
	resultSessionID := ""
	resultCost := 0.0
	isError := false
	lastProgressTime := time.Time{}

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024) // 1MB buffer for large responses

	for scanner.Scan() {
		// Check stop signal
		select {
		case <-opts.StopCh:
			procutil.KillTree(cmd) // kill node/claude tree, not just cmd.exe
			cmd.Wait()             // Reap process to avoid zombie
			<-stderrDone           // Wait for stderr goroutine
			return Response{
				Text:      strings.Join(allTexts, "\n\n"),
				SessionID: resultSessionID,
				IsError:   true,
			}
		default:
		}

		line := scanner.Text()
		if line == "" {
			continue
		}

		log.Printf("[Claude][stdout] %.200s", line)

		var event map[string]any
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			log.Printf("[Claude] JSON parse error: %v", err)
			continue
		}

		msgType, _ := event["type"].(string)

		switch msgType {
		case "system":
			// Extract session_id from system init
			if sid, ok := event["session_id"].(string); ok {
				resultSessionID = sid
			}
			// Show system status as progress
			if opts.OnProgress != nil {
				subtype, _ := event["subtype"].(string)
				switch subtype {
				case "init":
					opts.OnProgress("🟠 Claude Code подключён")
				case "status":
					if status, ok := event["status"].(string); ok && status != "" {
						statusLabels := map[string]string{
							"compacting": "📦 Сжатие контекста...",
							"thinking":   "🧠 Думает...",
							"tool_use":   "🔧 Использует инструменты...",
							"responding": "✍️ Пишет ответ...",
						}
						if label, found := statusLabels[status]; found {
							opts.OnProgress(label)
						} else {
							opts.OnProgress("⏳ " + status)
						}
					}
				case "compact_boundary":
					opts.OnProgress("📦 Контекст сжат, продолжаю...")
				}
			}

		case "assistant":
			// Extract text and tool_use from message content
			msg, _ := event["message"].(map[string]any)
			if msg == nil {
				continue
			}
			// Show "writing" progress when assistant starts responding
			if opts.OnProgress != nil && len(allTexts) == 0 {
				opts.OnProgress("✍️ Пишет ответ...")
			}
			content, _ := msg["content"].([]any)
			for _, block := range content {
				b, ok := block.(map[string]any)
				if !ok {
					continue
				}
				blockType, _ := b["type"].(string)

				switch blockType {
				case "text":
					if text, ok := b["text"].(string); ok && strings.TrimSpace(text) != "" {
						allTexts = append(allTexts, text)
					}
				case "tool_use":
					toolName, _ := b["name"].(string)

					// Detect AskUserQuestion — Claude is asking the user something
					if toolName == "AskUserQuestion" && opts.OnQuestion != nil {
						if inp, ok := b["input"].(map[string]any); ok {
							question, _ := inp["question"].(string)
							var qOptions []string
							if optsList, ok := inp["options"].([]any); ok {
								for _, o := range optsList {
									if s, ok := o.(string); ok {
										qOptions = append(qOptions, s)
									}
								}
							}
							if question != "" {
								opts.OnQuestion(question, qOptions)
							}
						}
					}

					if toolName != "" && opts.OnProgress != nil {
						now := time.Now()
						if now.Sub(lastProgressTime) >= progressThrottle {
							icon := toolIcons[toolName]
							if icon == "" {
								icon = "🔧"
							}
							detail := ""
							if inp, ok := b["input"].(map[string]any); ok {
								detail = toolSummary(toolName, inp)
							}
							msg := icon + " " + toolName
							if detail != "" {
								msg += ": " + detail
							}
							opts.OnProgress(msg)
							lastProgressTime = now
						}
					}
				}
			}

		case "rate_limit_event":
			// Show rate limit status
			if opts.OnProgress != nil {
				if info, ok := event["rate_limit_info"].(map[string]any); ok {
					if status, ok := info["status"].(string); ok && status != "allowed" {
						opts.OnProgress("⏳ Rate limit: " + status)
					}
				}
			}

		case "result":
			if sid, ok := event["session_id"].(string); ok {
				resultSessionID = sid
			}
			if cost, ok := event["cost_usd"].(float64); ok {
				resultCost = cost
			}
			if errFlag, ok := event["is_error"].(bool); ok {
				isError = errFlag
			}
			log.Printf("[Claude] Done: session=%s cost=$%.4f is_error=%v",
				resultSessionID, resultCost, isError)
		}
	}

	// Wait for process to finish, then sync stderr goroutine
	waitErr := cmd.Wait()
	<-stderrDone // Ensure stderr goroutine is done before reading stderrBuf
	log.Printf("[Claude] Process exited: err=%v, texts=%d, stderr=%d bytes",
		waitErr, len(allTexts), stderrBuf.Len())

	resultText := strings.Join(allTexts, "\n\n")
	if resultText == "" {
		// Fallback: use stderr if no stdout text
		if stderrBuf.Len() > 0 {
			resultText = strings.TrimSpace(stderrBuf.String())
		} else {
			resultText = "(empty response)"
		}
	}

	return Response{
		Text:      resultText,
		SessionID: resultSessionID,
		IsError:   isError,
		CostUSD:   resultCost,
	}
}

// ── Codex Agent ──────────────────────────────────────────────────────

type CodexAgent struct{}

var codexApprovalFlags = map[string][]string{
	"full-auto": {"--full-auto"},
	"bypass":    {"--dangerously-bypass-approvals-and-sandbox"},
	"suggest":   {"-a", "untrusted"},
	"auto":      {"-a", "never"},
}

func (a *CodexAgent) Run(ctx context.Context, opts RunOptions) Response {
	desc := GetDescriptor("codex")
	if desc == nil || !desc.IsDetected() {
		return Response{Text: "Codex CLI not found", IsError: true}
	}

	prompt := withContinuation(opts.Prompt, opts.ContinuationPath)
	args := []string{desc.Path()}
	if opts.SessionID != "" {
		args = append(args, "exec", "resume", opts.SessionID, prompt)
	} else {
		args = append(args, "exec", prompt)
	}

	// Model (session override > global config)
	cfg := config.Get()
	model := mapGet(opts.SessionConfig, "codex_model")
	if model == "" {
		model = cfg.CodexModel
	}
	if model == "" {
		model = "gpt-5.3-codex"
	}
	if model != "" {
		args = append(args, "-m", model)
	}

	// Reasoning
	reasoning := mapGet(opts.SessionConfig, "codex_reasoning")
	if reasoning == "" {
		reasoning = cfg.CodexReasoning
	}
	if reasoning != "" && reasoning != "medium" {
		args = append(args, "-c", fmt.Sprintf(`model_reasoning_effort="%s"`, reasoning))
	}

	// Approval mode
	approval := mapGet(opts.SessionConfig, "codex_approval_mode")
	if approval == "" {
		approval = cfg.CodexApprovalMode
	}
	if approval == "" {
		approval = "full-auto"
	}
	if flags, ok := codexApprovalFlags[approval]; ok {
		args = append(args, flags...)
	}

	if opts.OnProgress != nil {
		opts.OnProgress("🟢 Запуск Codex CLI...")
	}

	timeoutCtx, cancel := withStopAndTimeout(ctx, opts.StopCh, 10*time.Minute)
	defer cancel()

	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmdArgs := append([]string{"/c"}, args...)
		cmd = exec.CommandContext(timeoutCtx, "cmd", cmdArgs...)
	} else {
		cmd = exec.CommandContext(timeoutCtx, args[0], args[1:]...)
	}
	// codex на Windows — .cmd-обёртка, то есть лишний cmd.exe со своим окном.
	// Ответ читается из CombinedOutput, окно человеку не нужно.
	procutil.Hidden(cmd)
	procutil.Prepare(cmd) // kill the whole tree on cancel/timeout, not just cmd.exe
	cmd.Dir = opts.Cwd

	out, err := cmd.CombinedOutput()
	text := strings.TrimSpace(string(out))
	if text == "" {
		text = "(empty response)"
	}
	return Response{Text: text, IsError: err != nil}
}

// ── Orchestrator Agent ───────────────────────────────────────────────

type OrchestratorAgent struct{}

func (a *OrchestratorAgent) Run(ctx context.Context, opts RunOptions) Response {
	apiKey := os.Getenv("OPENROUTER_API_KEY")
	if apiKey == "" {
		return Response{
			Text:    "OPENROUTER_API_KEY not set.\nAdd to .env: OPENROUTER_API_KEY=sk-or-...",
			IsError: true,
		}
	}

	cfg := config.Get()
	model := mapGet(opts.SessionConfig, "orchestrator_model")
	if model == "" {
		model = cfg.OrchestratorModel
	}

	runAgent := func(ctx context.Context, agentType, prompt, cwd string) orchestrator.AgentResult {
		agent, err := GetAgent(agentType)
		if err != nil {
			return orchestrator.AgentResult{Text: err.Error(), IsError: true}
		}
		resp := agent.Run(ctx, RunOptions{
			Prompt: prompt,
			Cwd:    cwd,
			SessionConfig: map[string]string{
				"claude_permission_mode": "bypassPermissions",
				"codex_approval_mode":    "full-auto",
			},
		})
		return orchestrator.AgentResult{
			Text:    resp.Text,
			CostUSD: resp.CostUSD,
			IsError: resp.IsError,
		}
	}

	var progress orchestrator.ProgressFunc
	if opts.OnProgress != nil {
		progress = orchestrator.ProgressFunc(opts.OnProgress)
	}

	var stepFn orchestrator.StepFunc
	if opts.OnStep != nil {
		stepFn = orchestrator.StepFunc(opts.OnStep)
	}

	if opts.OnProgress != nil {
		opts.OnProgress("🎯 Запуск оркестратора...")
	}

	orch := orchestrator.New(apiKey, model, runAgent)
	// Лимиты делегирования (узкий белт) из session config. Аддитивный хук:
	// ключи новые, без них поведение дефолтное; существующие вызовы не тронуты.
	if v := mapGet(opts.SessionConfig, "orchestrator_max_agent_runs"); v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil {
			orch.SetMaxAgentRuns(n)
		}
	}
	if v := mapGet(opts.SessionConfig, "orchestrator_max_agent_cost_usd"); v != "" {
		var c float64
		if _, err := fmt.Sscanf(v, "%f", &c); err == nil {
			orch.SetMaxAgentCostUSD(c)
		}
	}
	result := orch.ExecuteWithSteps(ctx, opts.Prompt, opts.Cwd, progress, stepFn, opts.StopCh)
	return buildOrchestratorResponse(result)
}

// ── Researcher Agent (separate from Orchestrator — explicit research mode) ──

type ResearcherAgent struct{}

func (a *ResearcherAgent) Run(ctx context.Context, opts RunOptions) Response {
	apiKey := os.Getenv("OPENROUTER_API_KEY")
	if apiKey == "" {
		return Response{
			Text:    "OPENROUTER_API_KEY not set.\nAdd to .env: OPENROUTER_API_KEY=sk-or-...",
			IsError: true,
		}
	}

	// Research REQUIRES structured config — not free-text prompt
	evalCmd := mapGet(opts.SessionConfig, "research_eval_command")
	if evalCmd == "" {
		return Response{
			Text:    "Research mode requires configuration.\nSet eval_command, metric_pattern, and other settings via the Research UI in Mini App.",
			IsError: true,
		}
	}

	cfg := config.Get()
	model := mapGet(opts.SessionConfig, "orchestrator_model")
	if model == "" {
		model = cfg.OrchestratorModel
	}

	runAgent := func(ctx context.Context, agentType, prompt, cwd string) orchestrator.AgentResult {
		if agentType == "orchestrator" || agentType == "researcher" {
			return orchestrator.AgentResult{Text: "cannot delegate to orchestrator/researcher", IsError: true}
		}
		agent, err := GetAgent(agentType)
		if err != nil {
			return orchestrator.AgentResult{Text: err.Error(), IsError: true}
		}
		resp := agent.Run(ctx, RunOptions{
			Prompt: prompt,
			Cwd:    cwd,
			SessionConfig: map[string]string{
				"claude_permission_mode": "bypassPermissions",
				"codex_approval_mode":    "full-auto",
			},
		})
		return orchestrator.AgentResult{
			Text: resp.Text, CostUSD: resp.CostUSD, IsError: resp.IsError,
		}
	}

	var progress orchestrator.ProgressFunc
	if opts.OnProgress != nil {
		progress = orchestrator.ProgressFunc(opts.OnProgress)
		opts.OnProgress("🔬 Запуск research mode...")
	}

	rcfg := buildResearchConfig(opts.SessionConfig)
	orch := orchestrator.New(apiKey, model, runAgent)
	rresult := orch.ExecuteResearch(ctx, opts.Prompt, opts.Cwd, rcfg, progress, nil, opts.StopCh)
	return buildResearchResponse(rresult)
}

func buildResearchConfig(sc map[string]string) orchestrator.ResearchConfig {
	maxExp := 10
	if v := mapGet(sc, "research_max_experiments"); v != "" {
		fmt.Sscanf(v, "%d", &maxExp)
	}
	maxWall := 120
	if v := mapGet(sc, "research_max_wall_clock"); v != "" {
		fmt.Sscanf(v, "%d", &maxWall)
	}
	maxCost := 5.0
	if v := mapGet(sc, "research_max_cost_usd"); v != "" {
		fmt.Sscanf(v, "%f", &maxCost)
	}

	var protected []string
	if v := mapGet(sc, "research_protected_paths"); v != "" {
		for _, p := range strings.Split(v, ",") {
			p = strings.TrimSpace(p)
			if p != "" {
				protected = append(protected, p)
			}
		}
	}

	pattern := mapGet(sc, "research_metric_pattern")
	if pattern == "" {
		pattern = `([\d.]+)`
	}
	metricName := mapGet(sc, "research_metric_name")
	if metricName == "" {
		metricName = "metric"
	}

	return orchestrator.ResearchConfig{
		EvalCommand:         mapGet(sc, "research_eval_command"),
		MetricPattern:       pattern,
		MetricFile:          mapGet(sc, "research_metric_file"),
		MetricName:          metricName,
		LowerIsBetter:       mapGet(sc, "research_lower_is_better") != "false",
		MaxExperiments:      maxExp,
		MaxWallClockMinutes: maxWall,
		MaxCostUSD:          maxCost,
		InvariantsCommand:   mapGet(sc, "research_invariants_command"),
		ProtectedPaths:      protected,
	}
}

func buildOrchestratorResponse(result orchestrator.Result) Response {
	var sb strings.Builder
	sb.WriteString(result.Summary)
	if len(result.Steps) > 0 {
		sb.WriteString("\n\n--- Шаги ---")
		for i, step := range result.Steps {
			icon := "ok"
			if step.Error != "" {
				icon = "err"
			}
			sb.WriteString(fmt.Sprintf("\n%d. [%s] %s", i+1, icon, step.Input))
		}
	}
	return Response{Text: sb.String(), IsError: result.IsError, CostUSD: result.AgentCost}
}

func buildResearchResponse(result orchestrator.ResearchResult) Response {
	var sb strings.Builder
	sb.WriteString(result.Summary)
	if len(result.Experiments) > 0 {
		sb.WriteString("\n\n--- Эксперименты ---")
		for _, exp := range result.Experiments {
			icon := "✅"
			if exp.Reverted {
				icon = "❌"
			}
			sb.WriteString(fmt.Sprintf("\n%d. %s %s | metric=%.6g %s",
				exp.ID, icon, exp.Description, exp.Metric, exp.Delta))
		}
		if result.Improvement != "" {
			sb.WriteString(fmt.Sprintf("\n\nИтог: %s", result.Improvement))
		}
	}
	return Response{Text: sb.String(), IsError: result.IsError, CostUSD: result.AgentCost}
}

// ── Factory ──────────────────────────────────────────────────────────

// GetAgent creates an agent instance by type ID.
func GetAgent(agentType string) (Agent, error) {
	switch agentType {
	case "claude":
		return &ClaudeAgent{}, nil
	case "codex":
		return &CodexAgent{}, nil
	case "shell":
		return &ShellAgent{}, nil
	case "orchestrator":
		return &OrchestratorAgent{}, nil
	case "researcher":
		return &ResearcherAgent{}, nil
	}

	desc := GetDescriptor(agentType)
	if desc != nil && desc.IsDetected() {
		return &CLIAgent{Descriptor: desc}, nil
	}

	return nil, fmt.Errorf("unknown or unavailable agent: %s", agentType)
}

// ── Helpers ──────────────────────────────────────────────────────────

func mapGet(m map[string]string, key string) string {
	if m == nil {
		return ""
	}
	return m[key]
}

func quoteArgs(args []string) string {
	parts := make([]string, len(args))
	for i, arg := range args {
		if strings.ContainsAny(arg, " \t\"") {
			parts[i] = `"` + strings.ReplaceAll(arg, `"`, `\"`) + `"`
		} else {
			parts[i] = arg
		}
	}
	return strings.Join(parts, " ")
}

func filterEnv(env []string, exclude ...string) []string {
	result := make([]string, 0, len(env))
	for _, e := range env {
		skip := false
		for _, ex := range exclude {
			if strings.HasPrefix(e, ex+"=") {
				skip = true
				break
			}
		}
		if !skip {
			result = append(result, e)
		}
	}
	return result
}
