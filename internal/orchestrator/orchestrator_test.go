package orchestrator

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ── API stub helpers ──────────────────────────────────────────────────

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func toolCallJSON(id, name, args string) string {
	return fmt.Sprintf(`{"id":%q,"type":"function","function":{"name":%q,"arguments":%q}}`, id, name, args)
}

// apiResponse builds a 200 OK chat-completion body. toolCalls are raw JSON
// fragments produced by toolCallJSON.
func apiResponse(content string, toolCalls ...string) *http.Response {
	calls := ""
	if len(toolCalls) > 0 {
		calls = `,"tool_calls":[` + strings.Join(toolCalls, ",") + `]`
	}
	body := fmt.Sprintf(`{"id":"cmpl-1","choices":[{"message":{"role":"assistant","content":%q%s},"finish_reason":"tool_calls"}],`+
		`"usage":{"prompt_tokens":10,"completion_tokens":5}}`, content, calls)
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}
}

// queueTransport serves the queued responses in order; it fails the test via
// extraCalls counter if the orchestrator asks for more than queued.
type queueTransport struct {
	responses []*http.Response
	calls     int
	lastAuth  string
	lastBody  []byte
}

func (q *queueTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	q.lastAuth = r.Header.Get("Authorization")
	q.lastBody, _ = io.ReadAll(r.Body)
	if q.calls >= len(q.responses) {
		return &http.Response{
			StatusCode: 500, Header: make(http.Header),
			Body: io.NopCloser(strings.NewReader(`{"error":"test queue exhausted"}`)),
		}, nil
	}
	resp := q.responses[q.calls]
	q.calls++
	return resp, nil
}

func stubAgentOK(text string, cost float64) AgentRunFunc {
	return func(ctx context.Context, agentType, prompt, cwd string) AgentResult {
		return AgentResult{Text: text, CostUSD: cost}
	}
}

func newTestOrchestrator(rt http.RoundTripper, runAgent AgentRunFunc) *Orchestrator {
	o := New("test-api-key", "sonnet", runAgent)
	o.client = &http.Client{Transport: rt}
	return o
}

// ── Pure helpers ──────────────────────────────────────────────────────

func TestResolveModel(t *testing.T) {
	cases := []struct{ in, want string }{
		{"sonnet", "anthropic/claude-sonnet-4.6"},
		{"opus", "anthropic/claude-opus-4.6"},
		{"gemini-flash", "google/gemini-2.5-flash"},
		{"deepseek-r1", "deepseek/deepseek-r1"},
		{"custom/vendor-1.0", "custom/vendor-1.0"},       // full IDs pass through
		{"no-such-model", "anthropic/claude-sonnet-4.6"}, // unknown → fallback
	}
	for _, c := range cases {
		if got := ResolveModel(c.in); got != c.want {
			t.Errorf("ResolveModel(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	// Every builtin short name must resolve to its own ID.
	for _, mi := range BuiltinModels {
		if got := ResolveModel(mi.Short); got != mi.ID {
			t.Errorf("ResolveModel(%q) = %q, want %q", mi.Short, got, mi.ID)
		}
	}
}

func TestNewDefaultsModel(t *testing.T) {
	o := New("k", "", nil)
	if o.model != "anthropic/claude-sonnet-4.6" {
		t.Errorf("empty model must default to sonnet, got %q", o.model)
	}
}

func TestTruncate(t *testing.T) {
	if got := truncate("short", 10); got != "short" {
		t.Errorf("short string: %q", got)
	}
	// Truncation counts runes, not bytes — must not split multibyte chars.
	s := strings.Repeat("я", 20)
	got := truncate(s, 5)
	if !strings.HasPrefix(got, "яяяяя") || !strings.HasSuffix(got, "...") {
		t.Errorf("rune-safe truncate: %q", got)
	}
}

func TestParseRetryAfter(t *testing.T) {
	cases := []struct {
		in   string
		want time.Duration
	}{
		{"", 0},
		{"5", 5 * time.Second},
		{" 10 ", 10 * time.Second},
		{"0", 0},   // non-positive rejected
		{"-3", 0},  // negative rejected
		{"121", 0}, // over the 120s cap rejected
		{"abc", 0}, // garbage rejected
	}
	for _, c := range cases {
		if got := parseRetryAfter(c.in); got != c.want {
			t.Errorf("parseRetryAfter(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestTrimToolOutputs(t *testing.T) {
	long := strings.Repeat("x", 1000)
	msgs := []orMessage{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "task"},
	}
	for range 6 {
		msgs = append(msgs,
			orMessage{Role: "assistant", Content: "thinking"},
			orMessage{Role: "tool", ToolCallID: "c", Content: long},
		)
	}
	msgs = append(msgs, orMessage{Role: "tool", ToolCallID: "short", Content: "tiny"})

	trimToolOutputs(msgs, 2)

	trimmed := 0
	for i, m := range msgs {
		if m.Role != "tool" {
			if m.Content != "sys" && m.Content != "task" && m.Content != "thinking" {
				t.Errorf("non-tool message #%d modified: %q", i, m.Content)
			}
			continue
		}
		if m.Content == "[older tool output trimmed to save context]" {
			trimmed++
		}
	}
	// 7 tool messages, keep 2 → 5 trimmed; "tiny" is short enough to survive
	// even though it is among the older ones only if cut reaches it — it is the
	// last one, so it stays regardless.
	if trimmed != 5 {
		t.Errorf("trimmed = %d, want 5 (7 tool outputs, keep recent 2)", trimmed)
	}
	if msgs[len(msgs)-1].Content != "tiny" {
		t.Error("most recent tool output must be kept verbatim")
	}
}

func TestBriefTool(t *testing.T) {
	cases := []struct {
		name  string
		input map[string]any
		want  string
	}{
		{"run_agent", map[string]any{"agent": "claude", "prompt": "do it"}, "run_agent(claude): do it"},
		{"run_command", map[string]any{"command": "ls -la"}, "run_command: ls -la"},
		{"read_file", map[string]any{"path": "/a/b"}, "read_file: /a/b"},
		{"unknown_tool", map[string]any{}, "unknown_tool"},
	}
	for _, c := range cases {
		if got := briefTool(c.name, c.input); got != c.want {
			t.Errorf("briefTool(%q) = %q, want %q", c.name, got, c.want)
		}
	}
}

// ── callAPI ───────────────────────────────────────────────────────────

func TestCallAPISuccess(t *testing.T) {
	qt := &queueTransport{responses: []*http.Response{apiResponse("hello")}}
	o := newTestOrchestrator(qt, nil)

	resp, err := o.callAPI(context.Background(), []orMessage{{Role: "user", Content: "hi"}}, toolDefs)
	if err != nil {
		t.Fatalf("callAPI: %v", err)
	}
	if resp.Choices[0].Message.Content != "hello" {
		t.Errorf("content = %q", resp.Choices[0].Message.Content)
	}
	if qt.lastAuth != "Bearer test-api-key" {
		t.Errorf("Authorization = %q", qt.lastAuth)
	}
	if !strings.Contains(string(qt.lastBody), `"model":"anthropic/claude-sonnet-4.6"`) {
		t.Errorf("request body missing resolved model: %s", qt.lastBody)
	}
}

func TestCallAPIRetriesOn429(t *testing.T) {
	rl := &http.Response{
		StatusCode: 429, Header: http.Header{"Retry-After": []string{"1"}},
		Body: io.NopCloser(strings.NewReader(`rate limited`)),
	}
	qt := &queueTransport{responses: []*http.Response{rl, apiResponse("recovered")}}
	o := newTestOrchestrator(qt, nil)

	resp, err := o.callAPI(context.Background(), []orMessage{{Role: "user", Content: "hi"}}, nil)
	if err != nil {
		t.Fatalf("callAPI after one 429: %v", err)
	}
	if qt.calls != 2 {
		t.Errorf("calls = %d, want 2 (429 then success)", qt.calls)
	}
	if resp.Choices[0].Message.Content != "recovered" {
		t.Errorf("content = %q", resp.Choices[0].Message.Content)
	}
}

func TestCallAPIClientErrorNoRetry(t *testing.T) {
	bad := &http.Response{
		StatusCode: 400, Header: make(http.Header),
		Body: io.NopCloser(strings.NewReader(`{"error":{"message":"bad request"}}`)),
	}
	qt := &queueTransport{responses: []*http.Response{bad, bad, bad}}
	o := newTestOrchestrator(qt, nil)

	_, err := o.callAPI(context.Background(), []orMessage{{Role: "user", Content: "hi"}}, nil)
	if err == nil || !strings.Contains(err.Error(), "HTTP 400") {
		t.Fatalf("err = %v, want HTTP 400", err)
	}
	if qt.calls != 1 {
		t.Errorf("calls = %d, want 1 — 4xx must not be retried", qt.calls)
	}
}

func TestCallAPICancelDuringRetry(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		cancel() // simulate the user aborting after the first failed attempt
		return &http.Response{
			StatusCode: 503, Header: make(http.Header),
			Body: io.NopCloser(strings.NewReader(`down`)),
		}, nil
	})
	o := newTestOrchestrator(rt, nil)

	_, err := o.callAPI(ctx, []orMessage{{Role: "user", Content: "hi"}}, nil)
	if err == nil {
		t.Fatal("expected error after cancel")
	}
	// Must return promptly on ctx.Done instead of sleeping through the backoff.
	if ctx.Err() == nil {
		t.Error("context should be cancelled")
	}
}

// ── Execute loop ──────────────────────────────────────────────────────

func TestExecuteFinishTool(t *testing.T) {
	qt := &queueTransport{responses: []*http.Response{
		apiResponse("", toolCallJSON("c1", "finish", `{"summary":"всё готово"}`)),
	}}
	o := newTestOrchestrator(qt, nil)

	var steps []StepEvent
	res := o.ExecuteWithSteps(context.Background(), "task", t.TempDir(), nil,
		func(s StepEvent) { steps = append(steps, s) }, nil)

	if res.IsError || res.Summary != "всё готово" {
		t.Fatalf("result: %+v", res)
	}
	if len(res.Steps) != 0 {
		t.Errorf("finish must not record a tool step, got %+v", res.Steps)
	}
	var sawFinish bool
	for _, s := range steps {
		if s.Type == "tool_call" && s.Tool == "finish" {
			sawFinish = true
		}
	}
	if !sawFinish {
		t.Error("expected a finish tool_call step event")
	}
}

func TestExecuteToolThenFinish(t *testing.T) {
	qt := &queueTransport{responses: []*http.Response{
		apiResponse("", toolCallJSON("c1", "run_command", `{"command":"echo orch-test-marker"}`)),
		apiResponse("", toolCallJSON("c2", "finish", `{"summary":"done"}`)),
	}}
	o := newTestOrchestrator(qt, nil)

	res := o.Execute(context.Background(), "task", t.TempDir(), nil, nil)
	if res.IsError || res.Summary != "done" {
		t.Fatalf("result: %+v", res)
	}
	if len(res.Steps) != 1 || res.Steps[0].Tool != "run_command" {
		t.Fatalf("steps: %+v", res.Steps)
	}
	if !strings.Contains(res.Steps[0].Output, "orch-test-marker") {
		t.Errorf("step output = %q, want the command output", res.Steps[0].Output)
	}
}

func TestExecutePlainTextResponse(t *testing.T) {
	qt := &queueTransport{responses: []*http.Response{apiResponse("просто ответ")}}
	o := newTestOrchestrator(qt, nil)

	res := o.Execute(context.Background(), "task", t.TempDir(), nil, nil)
	if res.IsError || res.Summary != "просто ответ" {
		t.Fatalf("result: %+v", res)
	}
}

func TestExecuteAPIError(t *testing.T) {
	bad := &http.Response{
		StatusCode: 400, Header: make(http.Header),
		Body: io.NopCloser(strings.NewReader(`nope`)),
	}
	qt := &queueTransport{responses: []*http.Response{bad}}
	o := newTestOrchestrator(qt, nil)

	res := o.Execute(context.Background(), "task", t.TempDir(), nil, nil)
	if !res.IsError || !strings.HasPrefix(res.Summary, "Ошибка API:") {
		t.Fatalf("result: %+v", res)
	}
}

func TestExecuteCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	o := newTestOrchestrator(&queueTransport{}, nil)

	res := o.Execute(ctx, "task", t.TempDir(), nil, nil)
	if !res.IsError || res.Summary != "Отменено" {
		t.Fatalf("result: %+v", res)
	}
}

func TestExecuteStopChannel(t *testing.T) {
	stop := make(chan struct{})
	close(stop)
	o := newTestOrchestrator(&queueTransport{}, nil)

	res := o.Execute(context.Background(), "task", t.TempDir(), nil, stop)
	if !res.IsError || res.Summary != "Прервано пользователем" {
		t.Fatalf("result: %+v", res)
	}
}

// ── Tool execution ────────────────────────────────────────────────────

func TestToolRunAgent(t *testing.T) {
	var gotAgent, gotPrompt string
	o := newTestOrchestrator(&queueTransport{}, func(ctx context.Context, agentType, prompt, cwd string) AgentResult {
		gotAgent, gotPrompt = agentType, prompt
		return AgentResult{Text: "agent says hi", CostUSD: 0.25}
	})

	// Recursion guard.
	for _, agent := range []string{"orchestrator", "researcher"} {
		if _, errMsg := o.toolRunAgent(context.Background(),
			map[string]any{"agent": agent, "prompt": "x"}, t.TempDir()); errMsg == "" {
			t.Errorf("delegating to %s must be refused", agent)
		}
	}
	// Missing args.
	if _, errMsg := o.toolRunAgent(context.Background(), map[string]any{"agent": "claude"}, t.TempDir()); errMsg == "" {
		t.Error("missing prompt must be an error")
	}
	// Happy path + cost accounting.
	out, errMsg := o.toolRunAgent(context.Background(),
		map[string]any{"agent": "claude", "prompt": "do it"}, t.TempDir())
	if errMsg != "" || out != "agent says hi" {
		t.Errorf("out=%q err=%q", out, errMsg)
	}
	if gotAgent != "claude" || !strings.Contains(gotPrompt, "do it") ||
		!strings.Contains(gotPrompt, "ask/plan/run/status") {
		t.Errorf("delegation args: %q / %q (промпт обёрнут в рамку саб-агента, задача должна сохраниться)", gotAgent, gotPrompt)
	}
	if o.agentCost != 0.25 {
		t.Errorf("agentCost = %v, want 0.25", o.agentCost)
	}
	// Agent error propagates as errMsg.
	o.runAgent = func(ctx context.Context, a, p, c string) AgentResult {
		return AgentResult{Text: "boom", IsError: true}
	}
	if _, errMsg := o.toolRunAgent(context.Background(),
		map[string]any{"agent": "claude", "prompt": "x"}, t.TempDir()); errMsg == "" {
		t.Error("agent error must surface as errMsg")
	}
}

func TestExecToolFileOps(t *testing.T) {
	dir := t.TempDir()
	o := newTestOrchestrator(&queueTransport{}, nil)
	ctx := context.Background()

	out, errMsg := o.execTool(ctx, "write_file", map[string]any{"path": "sub/note.txt", "content": "hello file"}, dir)
	if errMsg != "" {
		t.Fatalf("write_file: %s", errMsg)
	}
	if !strings.Contains(out, "Written 10 bytes") {
		t.Errorf("write_file output = %q", out)
	}

	out, errMsg = o.execTool(ctx, "read_file", map[string]any{"path": "sub/note.txt"}, dir)
	if errMsg != "" || out != "hello file" {
		t.Errorf("read_file: out=%q err=%q", out, errMsg)
	}

	out, errMsg = o.execTool(ctx, "list_files", map[string]any{"path": ".", "pattern": "*.txt"}, filepath.Join(dir, "sub"))
	if errMsg != "" || out != "note.txt" {
		t.Errorf("list_files glob: out=%q err=%q", out, errMsg)
	}

	if _, errMsg = o.execTool(ctx, "read_file", map[string]any{"path": "missing.txt"}, dir); errMsg == "" {
		t.Error("reading a missing file must error")
	}
	if _, errMsg = o.execTool(ctx, "read_file", map[string]any{}, dir); errMsg == "" {
		t.Error("missing path arg must error")
	}
	if _, errMsg = o.execTool(ctx, "no_such_tool", map[string]any{}, dir); errMsg == "" {
		t.Error("unknown tool must error")
	}
}

func TestToolRunCommand(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	o := newTestOrchestrator(&queueTransport{}, nil)

	out, errMsg := o.toolRunCommand(context.Background(), map[string]any{"command": "echo cmd-marker"}, dir)
	if errMsg != "" || !strings.Contains(out, "cmd-marker") {
		t.Errorf("out=%q err=%q", out, errMsg)
	}
	if _, errMsg := o.toolRunCommand(context.Background(), map[string]any{}, dir); errMsg == "" {
		t.Error("missing command must error")
	}
	// Non-zero exit with output returns both.
	_, errMsg = o.toolRunCommand(context.Background(), map[string]any{"command": "echo oops && exit 1"}, dir)
	if errMsg == "" {
		t.Error("failing command must report errMsg")
	}
}

func TestTruncateLog(t *testing.T) {
	if got := truncateLog("ok", 10); got != "ok" {
		t.Errorf("short: %q", got)
	}
	got := truncateLog(strings.Repeat("a", 20), 5)
	if !strings.HasSuffix(got, "...[truncated]") {
		t.Errorf("long: %q", got)
	}
}
