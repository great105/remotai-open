// Package orchestrator implements an autonomous development agent that
// coordinates other AI agents via OpenRouter API (OpenAI-compatible).
package orchestrator

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"tgcontrol/internal/procutil"
)

// ── Public types ──────────────────────────────────────────────────────

// AgentResult holds the result of a delegated agent run.
type AgentResult struct {
	Text    string
	CostUSD float64
	IsError bool
}

// AgentRunFunc delegates a task to an AI agent.
type AgentRunFunc func(ctx context.Context, agentType, prompt, cwd string) AgentResult

// ProgressFunc is called with status updates.
type ProgressFunc func(text string)

// StepEvent is a structured log entry for each orchestrator action.
type StepEvent struct {
	Iteration  int    `json:"iteration"`
	Type       string `json:"type"` // "thinking", "tool_call", "tool_result", "api_call", "error"
	Tool       string `json:"tool,omitempty"`
	Input      string `json:"input,omitempty"`
	Output     string `json:"output,omitempty"`
	Error      string `json:"error,omitempty"`
	DurationMs int64  `json:"duration_ms,omitempty"`
	TokensIn   int    `json:"tokens_in,omitempty"`
	TokensOut  int    `json:"tokens_out,omitempty"`
}

// StepFunc is called with structured step events for detailed logging.
type StepFunc func(step StepEvent)

// Orchestrator coordinates AI agents to complete complex tasks.
type Orchestrator struct {
	apiKey    string
	model     string
	runAgent  AgentRunFunc
	client    *http.Client
	agentCost float64

	// Лимиты делегирования (см. delegate.go). Счётчики agentRuns/agentCost —
	// на запуск: сбрасываются в начале ExecuteWithSteps/ExecuteResearch.
	maxAgentRuns    int
	maxAgentCostUSD float64
	agentRuns       int

	// Protected paths: nil = DefaultProtectedGlobs (см. protected.go).
	protectedGlobs []string
	confirmFn      ConfirmFunc
}

// New creates a new Orchestrator.
func New(apiKey, model string, runAgent AgentRunFunc) *Orchestrator {
	if model == "" {
		model = "sonnet"
	}
	return &Orchestrator{
		apiKey:       apiKey,
		model:        ResolveModel(model),
		runAgent:     runAgent,
		client:       &http.Client{Timeout: 10 * time.Minute},
		maxAgentRuns: defaultMaxAgentRuns,
	}
}

// ModelInfo describes an available model with pricing.
type ModelInfo struct {
	ID       string  `json:"id"`
	Short    string  `json:"short"`
	Name     string  `json:"name"`
	Provider string  `json:"provider"`
	InputM   float64 `json:"input_per_m"`  // $/M input tokens
	OutputM  float64 `json:"output_per_m"` // $/M output tokens
	Ctx      int     `json:"ctx"`
}

// BuiltinModels is the curated list for the UI.
var BuiltinModels = []ModelInfo{
	{"anthropic/claude-sonnet-4.6", "sonnet", "Claude Sonnet 4.6", "Anthropic", 3, 15, 1000000},
	{"anthropic/claude-opus-4.6", "opus", "Claude Opus 4.6", "Anthropic", 5, 25, 1000000},
	{"anthropic/claude-sonnet-4.5", "sonnet-4.5", "Claude Sonnet 4.5", "Anthropic", 3, 15, 1000000},
	{"google/gemini-2.5-pro", "gemini-pro", "Gemini 2.5 Pro", "Google", 1.25, 10, 1048576},
	{"google/gemini-2.5-flash", "gemini-flash", "Gemini 2.5 Flash", "Google", 0.30, 2.50, 1048576},
	{"openai/gpt-4o", "gpt-4o", "GPT-4o", "OpenAI", 2.50, 10, 128000},
	{"deepseek/deepseek-v3.2", "deepseek-v3", "DeepSeek V3.2", "DeepSeek", 0.26, 0.38, 163840},
	{"deepseek/deepseek-r1", "deepseek-r1", "DeepSeek R1", "DeepSeek", 0.70, 2.50, 64000},
	{"meta-llama/llama-4-maverick", "llama-4", "Llama 4 Maverick", "Meta", 0.15, 0.60, 1048576},
}

// ResolveModel maps short names to full OpenRouter model IDs.
func ResolveModel(m string) string {
	for _, mi := range BuiltinModels {
		if mi.Short == m {
			return mi.ID
		}
	}
	// Check if already a full ID
	if strings.Contains(m, "/") {
		return m
	}
	return "anthropic/claude-sonnet-4.6" // fallback
}

// Result holds the outcome of an orchestration run.
type Result struct {
	Summary   string
	Steps     []Step
	AgentCost float64
	IsError   bool
}

// Step records a single tool execution.
type Step struct {
	Tool   string
	Input  string
	Output string
	Error  string
}

// ── Constants ─────────────────────────────────────────────────────────

const (
	maxIterations = 25
	maxToolOutput = 50000
	openRouterURL = "https://openrouter.ai/api/v1/chat/completions"
)

const systemPrompt = `You are an autonomous development orchestrator. You receive high-level tasks and execute them step by step.

Tools:
- run_agent: Delegate coding to AI agents. Use "claude" for complex code tasks. Use "shell" for simple commands.
- race: Best-of-N race — one task to several agents in parallel, judge model picks the winner, winner's diff is applied.
- run_command: Run a shell command, get stdout+stderr.
- read_file: Read a file.
- list_files: List directory contents.
- write_file: Create/overwrite a file.
- finish: Task complete. Call with summary.

Workflow: read files -> plan -> delegate to agents -> verify -> finish.

Rules:
- NEVER run_agent with agent="orchestrator" or agent="researcher" (infinite recursion)
- Protected paths (secrets, migrations, .git) are gated: if a tool returns PROTECTED, the action needs human approval — do NOT retry it, explain in your summary what access was needed
- Verify work before finishing
- Be efficient
- Respond in Russian`

// ── OpenAI-compatible API types (for OpenRouter) ──────────────────────

type orRequest struct {
	Model       string      `json:"model"`
	MaxTokens   int         `json:"max_tokens,omitempty"`
	Messages    []orMessage `json:"messages"`
	Tools       []orTool    `json:"tools,omitempty"`
	Temperature *float64    `json:"temperature,omitempty"`
}

type orMessage struct {
	Role       string       `json:"role"`
	Content    string       `json:"content"`
	ToolCalls  []orToolCall `json:"tool_calls,omitempty"`
	ToolCallID string       `json:"tool_call_id,omitempty"`
}

type orToolCall struct {
	ID       string         `json:"id"`
	Type     string         `json:"type"`
	Function orFunctionCall `json:"function"`
}

type orFunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type orTool struct {
	Type     string    `json:"type"`
	Function orToolDef `json:"function"`
}

type orToolDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

type orResponse struct {
	ID      string     `json:"id"`
	Choices []orChoice `json:"choices"`
	Usage   struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
		Code    any    `json:"code"`
	} `json:"error,omitempty"`
}

type orChoice struct {
	Message      orMessage `json:"message"`
	FinishReason string    `json:"finish_reason"`
}

// ── Tool definitions (OpenAI function-calling format) ─────────────────

var toolDefs = []orTool{
	{Type: "function", Function: orToolDef{
		Name: "run_agent", Description: "Delegate coding task to AI agent (claude, codex, gemini, shell).",
		Parameters: json.RawMessage(`{"type":"object","properties":{"agent":{"type":"string","description":"Agent type: claude, codex, gemini, aider, shell"},"prompt":{"type":"string","description":"Task for the agent"},"cwd":{"type":"string","description":"Working directory (optional)"}},"required":["agent","prompt"]}`),
	}},
	{Type: "function", Function: orToolDef{
		Name: "race", Description: "Best-of-N race: run the same task on 2-4 agents in parallel (e.g. claude,codex,kimi); an independent judge model picks the winner and the winner's diff is applied.",
		Parameters: json.RawMessage(`{"type":"object","properties":{"agents":{"type":"string","description":"Comma-separated agent ids (2-4): claude,codex,kimi"},"task":{"type":"string","description":"The task for all candidates"},"judge":{"type":"string","description":"Judge model short name (optional, default = orchestrator model)"}},"required":["agents","task"]}`),
	}},
	{Type: "function", Function: orToolDef{
		Name: "run_command", Description: "Run a shell command, get stdout+stderr.",
		Parameters: json.RawMessage(`{"type":"object","properties":{"command":{"type":"string","description":"Shell command"},"cwd":{"type":"string","description":"Working directory (optional)"}},"required":["command"]}`),
	}},
	{Type: "function", Function: orToolDef{
		Name: "read_file", Description: "Read file contents.",
		Parameters: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string","description":"File path"}},"required":["path"]}`),
	}},
	{Type: "function", Function: orToolDef{
		Name: "list_files", Description: "List directory contents.",
		Parameters: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string","description":"Directory path"},"pattern":{"type":"string","description":"Glob pattern (optional)"}},"required":["path"]}`),
	}},
	{Type: "function", Function: orToolDef{
		Name: "write_file", Description: "Write/create a file.",
		Parameters: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string","description":"File path"},"content":{"type":"string","description":"File contents"}},"required":["path","content"]}`),
	}},
	{Type: "function", Function: orToolDef{
		Name: "finish", Description: "Task complete. Call with summary.",
		Parameters: json.RawMessage(`{"type":"object","properties":{"summary":{"type":"string","description":"Summary of completed work"}},"required":["summary"]}`),
	}},
}

// ── Execute ───────────────────────────────────────────────────────────

func (o *Orchestrator) Execute(ctx context.Context, task, cwd string, onProgress ProgressFunc, stopCh <-chan struct{}) Result {
	return o.ExecuteWithSteps(ctx, task, cwd, onProgress, nil, stopCh)
}

// ExecuteWithSteps runs the orchestrator with detailed step logging.
func (o *Orchestrator) ExecuteWithSteps(ctx context.Context, task, cwd string, onProgress ProgressFunc, onStep StepFunc, stopCh <-chan struct{}) Result {
	result := Result{}
	// Счётчики делегирования живут один запуск, а не весь срок экземпляра:
	// иначе повторный Execute на том же Orchestrator унаследовал бы траты
	// и лимиты прошлого рана (agentCost раньше копился между ранами).
	o.agentRuns = 0
	o.agentCost = 0
	messages := []orMessage{
		{Role: "system", Content: systemPrompt},
		{Role: "user", Content: fmt.Sprintf("Задача: %s\n\nРабочая директория: %s", task, cwd)},
	}

	// File logger
	rl := GetLogger().NewRun("", o.model)

	// Артефакты запуска: runs/<runID>/{events.jsonl,summary.md,patch.diff} —
	// source of truth для UI (/api/orch/runs). finish через defer, чтобы
	// summary.md появился даже на ранних выходах (отмена/стоп без RunEnd).
	aw := newArtifactWriter(rl.RunID(), o.model, task, cwd)
	rl.attachArtifacts(aw)
	defer func() { aw.finish(result.Summary, result.AgentCost, result.IsError) }()
	// NewRun отписал run_start в общий лог ещё до attach — повторяем в артефакты.
	aw.LogEvent(LogEntry{Event: "run_start"})

	rl.Log(LogEntry{Event: "task", Input: task, Summary: cwd})

	emitStep := func(s StepEvent) {
		if onStep != nil {
			onStep(s)
		}
	}

	for i := range maxIterations {
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
			onProgress(fmt.Sprintf("🧠 Итерация %d/%d", i+1, maxIterations))
		}

		trimToolOutputs(messages, 8) // bound context/token growth across iterations
		apiStart := time.Now()
		resp, err := o.callAPI(ctx, messages, toolDefs)
		apiDur := time.Since(apiStart).Milliseconds()

		if err != nil {
			rl.LogError(i+1, err.Error())
			emitStep(StepEvent{Iteration: i + 1, Type: "error", Error: err.Error(), DurationMs: apiDur})
			result.Summary = fmt.Sprintf("Ошибка API: %v", err)
			result.IsError = true
			result.AgentCost = o.agentCost
			rl.RunEnd(result.Summary, o.agentCost, true)
			return result
		}

		// Log API call stats
		rl.APICall(i+1, apiDur, resp.Usage.PromptTokens, resp.Usage.CompletionTokens)
		emitStep(StepEvent{
			Iteration: i + 1, Type: "api_call", DurationMs: apiDur,
			TokensIn: resp.Usage.PromptTokens, TokensOut: resp.Usage.CompletionTokens,
		})

		if len(resp.Choices) == 0 {
			result.Summary = "Пустой ответ API"
			result.IsError = true
			result.AgentCost = o.agentCost
			return result
		}

		msg := resp.Choices[0].Message
		reason := resp.Choices[0].FinishReason

		// Log thinking/text response
		if msg.Content != "" {
			rl.Thinking(i+1, msg.Content)
			emitStep(StepEvent{Iteration: i + 1, Type: "thinking", Output: truncate(msg.Content, 500)})
		}

		// Add assistant message to history
		messages = append(messages, msg)

		// No tool calls → done
		if len(msg.ToolCalls) == 0 || reason == "stop" {
			if msg.Content != "" {
				result.Summary = msg.Content
			} else {
				result.Summary = "Завершено"
			}
			result.AgentCost = o.agentCost
			rl.RunEnd(result.Summary, o.agentCost, false)
			return result
		}

		// Execute tool calls
		for _, tc := range msg.ToolCalls {
			var args map[string]any
			json.Unmarshal([]byte(tc.Function.Arguments), &args)

			// Handle finish
			if tc.Function.Name == "finish" {
				summary, _ := args["summary"].(string)
				if summary != "" {
					result.Summary = summary
				} else {
					result.Summary = "Завершено"
				}
				rl.ToolCall(i+1, "finish", summary)
				emitStep(StepEvent{Iteration: i + 1, Type: "tool_call", Tool: "finish", Input: truncate(summary, 200)})
				result.AgentCost = o.agentCost
				rl.RunEnd(result.Summary, o.agentCost, false)
				return result
			}

			toolBrief := briefTool(tc.Function.Name, args)
			if onProgress != nil {
				onProgress(fmt.Sprintf("🔧 %s", toolBrief))
			}

			rl.ToolCall(i+1, tc.Function.Name, toolBrief)
			emitStep(StepEvent{Iteration: i + 1, Type: "tool_call", Tool: tc.Function.Name, Input: toolBrief})

			log.Printf("[Orchestrator] tool=%s", tc.Function.Name)
			toolStart := time.Now()
			output, toolErr := o.execTool(ctx, tc.Function.Name, args, cwd)
			toolDur := time.Since(toolStart).Milliseconds()

			rl.ToolResult(i+1, tc.Function.Name, output, toolErr, toolDur)

			step := Step{Tool: tc.Function.Name, Input: toolBrief}
			content := truncate(output, maxToolOutput)
			if toolErr != "" {
				step.Error = toolErr
				content = fmt.Sprintf("Error: %s\n%s", toolErr, content)
				emitStep(StepEvent{Iteration: i + 1, Type: "tool_result", Tool: tc.Function.Name, Error: toolErr, Output: truncate(output, 300), DurationMs: toolDur})
			} else {
				step.Output = truncate(output, 200)
				emitStep(StepEvent{Iteration: i + 1, Type: "tool_result", Tool: tc.Function.Name, Output: truncate(output, 300), DurationMs: toolDur})
			}
			result.Steps = append(result.Steps, step)

			// Add tool result message
			messages = append(messages, orMessage{
				Role:       "tool",
				ToolCallID: tc.ID,
				Content:    content,
			})
		}
	}

	result.Summary = fmt.Sprintf("Превышен лимит итераций (%d)", maxIterations)
	result.IsError = true
	result.AgentCost = o.agentCost
	rl.RunEnd(result.Summary, o.agentCost, true)
	return result
}

// ── Tool execution ────────────────────────────────────────────────────

func (o *Orchestrator) execTool(ctx context.Context, name string, input map[string]any, defaultCwd string) (output, errMsg string) {
	// Protected paths gate: действие по защищённому пути требует подтверждения
	// человека (ConfirmFunc); без него — отказ модели, а не молчаливое выполнение.
	if gateErr := o.gateProtected(name, input, defaultCwd); gateErr != "" {
		return "", gateErr
	}
	switch name {
	case "run_agent":
		return o.toolRunAgent(ctx, input, defaultCwd)
	case "race":
		return o.toolRace(ctx, input, defaultCwd)
	case "run_command":
		return o.toolRunCommand(ctx, input, defaultCwd)
	case "read_file":
		return o.toolReadFile(input, defaultCwd)
	case "list_files":
		return o.toolListFiles(input, defaultCwd)
	case "write_file":
		return o.toolWriteFile(input, defaultCwd)
	default:
		return "", fmt.Sprintf("unknown tool: %s", name)
	}
}

func (o *Orchestrator) toolRunAgent(ctx context.Context, input map[string]any, defaultCwd string) (string, string) {
	agent, _ := input["agent"].(string)
	prompt, _ := input["prompt"].(string)
	cwd, _ := input["cwd"].(string)
	if agent == "" || prompt == "" {
		return "", "agent and prompt are required"
	}
	// Канонический ID до всех гардов: глубина 1 и лимиты не обходятся
	// регистром/пробелами (см. delegate.go).
	agent = normalizeAgentType(agent)
	if gateErr := o.gateDelegation(agent); gateErr != "" {
		return "", gateErr
	}
	if cwd == "" {
		cwd = defaultCwd
	}
	// Узкий белт: AI-саб-агент получает рамку ask/plan/run/status. shell —
	// нет: там промпт это сама команда, обёртка её сломает.
	if agent != "shell" {
		prompt = subAgentBelt + "\n\n" + prompt
	}

	// Log delegation
	rl := GetLogger().NewRun("sub-agent", agent)
	rl.Log(LogEntry{Event: "agent_delegate", Tool: agent, Input: truncateLog(prompt, 2000), Summary: cwd})

	start := time.Now()
	o.agentRuns++
	res := o.runAgent(ctx, agent, prompt, cwd)
	dur := time.Since(start).Milliseconds()
	o.agentCost += res.CostUSD

	rl.Log(LogEntry{Event: "agent_result", Tool: agent, Output: truncateLog(res.Text, 2000), CostUSD: res.CostUSD, DurationMs: dur})

	if res.IsError {
		return res.Text, "agent returned error"
	}
	return res.Text, ""
}

func (o *Orchestrator) toolRunCommand(ctx context.Context, input map[string]any, defaultCwd string) (string, string) {
	command, _ := input["command"].(string)
	cwd, _ := input["cwd"].(string)
	if command == "" {
		return "", "command is required"
	}
	if cwd == "" {
		cwd = defaultCwd
	}
	timeoutCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.CommandContext(timeoutCtx, "cmd", "/c", command)
	} else {
		cmd = exec.CommandContext(timeoutCtx, "sh", "-c", command)
	}
	// Инструмент run_command оркестратора: вывод уходит модели, а не на экран.
	// Оркестратор зовёт его десятки раз подряд — без Hidden это очередь вспышек.
	procutil.Hidden(cmd)
	procutil.Prepare(cmd) // kill the whole tree on cancel/timeout
	cmd.Dir = cwd
	out, err := cmd.CombinedOutput()
	text := strings.TrimSpace(string(out))
	if err != nil {
		if text != "" {
			return text, err.Error()
		}
		return "", err.Error()
	}
	if text == "" {
		text = "(no output)"
	}
	return text, ""
}

func (o *Orchestrator) toolReadFile(input map[string]any, defaultCwd string) (string, string) {
	path, _ := input["path"].(string)
	if path == "" {
		return "", "path is required"
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(defaultCwd, path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err.Error()
	}
	return string(data), ""
}

func (o *Orchestrator) toolListFiles(input map[string]any, defaultCwd string) (string, string) {
	dir, _ := input["path"].(string)
	pattern, _ := input["pattern"].(string)
	if dir == "" {
		dir = defaultCwd
	}
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(defaultCwd, dir)
	}
	if pattern != "" {
		matches, err := filepath.Glob(filepath.Join(dir, pattern))
		if err != nil {
			return "", err.Error()
		}
		names := make([]string, len(matches))
		for i, m := range matches {
			names[i] = filepath.Base(m)
		}
		return strings.Join(names, "\n"), ""
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err.Error()
	}
	var lines []string
	for _, e := range entries {
		suffix := ""
		if e.IsDir() {
			suffix = "/"
		} else if info, err := e.Info(); err == nil {
			suffix = fmt.Sprintf(" (%d)", info.Size())
		}
		lines = append(lines, e.Name()+suffix)
	}
	return strings.Join(lines, "\n"), ""
}

func (o *Orchestrator) toolWriteFile(input map[string]any, defaultCwd string) (string, string) {
	path, _ := input["path"].(string)
	content, _ := input["content"].(string)
	if path == "" {
		return "", "path is required"
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(defaultCwd, path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return "", err.Error()
	}
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		return "", err.Error()
	}
	return fmt.Sprintf("Written %d bytes -> %s", len(content), path), ""
}

// ── OpenRouter API call ───────────────────────────────────────────────

func (o *Orchestrator) callAPI(ctx context.Context, messages []orMessage, tools []orTool) (*orResponse, error) {
	return o.callAPIModel(ctx, o.model, messages, tools)
}

// callAPIModel — callAPI с явной моделью: судья гонки (race.go) ходит своей
// моделью (cfg.Judge), не трогая o.model основного цикла.
func (o *Orchestrator) callAPIModel(ctx context.Context, model string, messages []orMessage, tools []orTool) (*orResponse, error) {
	body, err := json.Marshal(orRequest{
		Model:     model,
		MaxTokens: 8192,
		Messages:  messages,
		Tools:     tools,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal: %w", err)
	}

	const maxAttempts = 5
	var lastErr error
	var retryAfter time.Duration
	for attempt := range maxAttempts {
		if attempt > 0 {
			// Exponential backoff (~1,2,4,8,16s capped at 30s) + small jitter,
			// or honor a server Retry-After. This rides out the transient
			// failures the user hits on a throttled network instead of giving up
			// after ~15s and killing the whole run.
			delay := retryAfter
			retryAfter = 0
			if delay <= 0 {
				base := time.Second << (attempt - 1)
				if base > 30*time.Second {
					base = 30 * time.Second
				}
				jitter := time.Duration(time.Now().UnixNano() % int64(500*time.Millisecond))
				delay = base + jitter
			}
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(delay):
			}
		}

		// Per-attempt timeout: a stalled socket triggers a retry rather than
		// burning the whole 10-minute client budget on one hung request.
		attemptCtx, cancel := context.WithTimeout(ctx, 4*time.Minute)
		req, err := http.NewRequestWithContext(attemptCtx, "POST", openRouterURL, bytes.NewReader(body))
		if err != nil {
			cancel()
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+o.apiKey)
		req.Header.Set("HTTP-Referer", "https://github.com/tgcontrol")
		req.Header.Set("X-Title", "TGControl")

		resp, err := o.client.Do(req)
		if err != nil {
			cancel()
			lastErr = err
			log.Printf("[Orchestrator] API attempt %d transport error: %v", attempt+1, err)
			continue
		}

		respBody, rerr := io.ReadAll(resp.Body)
		statusCode := resp.StatusCode
		if statusCode == 429 {
			retryAfter = parseRetryAfter(resp.Header.Get("Retry-After"))
		}
		resp.Body.Close()
		cancel()
		if rerr != nil {
			lastErr = fmt.Errorf("read body: %w", rerr)
			continue
		}

		if statusCode == 429 || statusCode >= 500 {
			lastErr = fmt.Errorf("HTTP %d: %s", statusCode, truncate(string(respBody), 200))
			log.Printf("[Orchestrator] API retry %d: %v", attempt+1, lastErr)
			continue
		}

		if statusCode != 200 {
			return nil, fmt.Errorf("HTTP %d: %s", statusCode, truncate(string(respBody), 500))
		}

		var result orResponse
		if err := json.Unmarshal(respBody, &result); err != nil {
			return nil, fmt.Errorf("unmarshal: %w", err)
		}
		if result.Error != nil {
			return nil, fmt.Errorf("API error: %s", result.Error.Message)
		}
		// A 200 with no choices is a transient provider hiccup — retry instead of
		// surfacing the opaque "empty response" and killing the run.
		if len(result.Choices) == 0 {
			lastErr = fmt.Errorf("empty choices from API")
			log.Printf("[Orchestrator] API retry %d: empty choices", attempt+1)
			continue
		}

		log.Printf("[Orchestrator] tokens: %d in / %d out, model=%s",
			result.Usage.PromptTokens, result.Usage.CompletionTokens, model)
		return &result, nil
	}

	return nil, fmt.Errorf("API failed after retries: %v", lastErr)
}

// trimToolOutputs bounds context growth across ReAct iterations: it replaces the
// content of older "tool" messages with a short placeholder while keeping the
// most recent keepRecent tool outputs in full. The message sequence (and the
// assistant ↔ tool_call_id pairing) is preserved, so the API request stays valid.
func trimToolOutputs(messages []orMessage, keepRecent int) {
	toolPositions := make([]int, 0)
	for i := range messages {
		if messages[i].Role == "tool" {
			toolPositions = append(toolPositions, i)
		}
	}
	cut := len(toolPositions) - keepRecent
	for j := 0; j < cut; j++ {
		i := toolPositions[j]
		if len(messages[i].Content) > 256 {
			messages[i].Content = "[older tool output trimmed to save context]"
		}
	}
}

// parseRetryAfter parses a Retry-After header expressed in seconds (the form
// OpenRouter/Cloudflare use). Returns 0 for an absent/garbage/oversized value.
func parseRetryAfter(h string) time.Duration {
	h = strings.TrimSpace(h)
	if h == "" {
		return 0
	}
	var secs int
	if _, err := fmt.Sscanf(h, "%d", &secs); err == nil && secs > 0 && secs <= 120 {
		return time.Duration(secs) * time.Second
	}
	return 0
}

// ── Helpers ───────────────────────────────────────────────────────────

func briefTool(name string, input map[string]any) string {
	switch name {
	case "run_agent":
		a, _ := input["agent"].(string)
		p, _ := input["prompt"].(string)
		return fmt.Sprintf("run_agent(%s): %s", a, truncate(p, 80))
	case "race":
		a, _ := input["agents"].(string)
		t, _ := input["task"].(string)
		return fmt.Sprintf("race(%s): %s", a, truncate(t, 80))
	case "run_command":
		c, _ := input["command"].(string)
		return fmt.Sprintf("run_command: %s", truncate(c, 100))
	case "read_file":
		p, _ := input["path"].(string)
		return "read_file: " + p
	case "list_files":
		p, _ := input["path"].(string)
		return "list_files: " + p
	case "write_file":
		p, _ := input["path"].(string)
		return "write_file: " + p
	}
	return name
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "..."
}
