package orchestrator

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// LogEntry is a single structured log line.
type LogEntry struct {
	Time       string  `json:"time"`
	Session    string  `json:"session,omitempty"`
	RunID      string  `json:"run_id"`
	Iteration  int     `json:"iter,omitempty"`
	Event      string  `json:"event"` // "run_start", "api_req", "api_resp", "thinking", "tool_call", "tool_result", "agent_delegate", "agent_result", "run_end", "error"
	Model      string  `json:"model,omitempty"`
	Tool       string  `json:"tool,omitempty"`
	Input      string  `json:"input,omitempty"`
	Output     string  `json:"output,omitempty"`
	Error      string  `json:"error,omitempty"`
	DurationMs int64   `json:"duration_ms,omitempty"`
	TokensIn   int     `json:"tokens_in,omitempty"`
	TokensOut  int     `json:"tokens_out,omitempty"`
	CostUSD    float64 `json:"cost_usd,omitempty"`
	Summary    string  `json:"summary,omitempty"`
}

// FileLogger writes structured JSON Lines to orchestrator.log
type FileLogger struct {
	mu      sync.Mutex
	file    *os.File
	runID   string
	session string
}

var (
	globalLogger *FileLogger
	logOnce      sync.Once
)

// GetLogger returns the singleton file logger, creating orchestrator.log next to the exe.
func GetLogger() *FileLogger {
	logOnce.Do(func() {
		exePath, _ := os.Executable()
		logPath := filepath.Join(filepath.Dir(exePath), "orchestrator.log")
		f, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[Orchestrator] cannot open log: %v\n", err)
			return
		}
		globalLogger = &FileLogger{file: f}
	})
	return globalLogger
}

// NewRun creates a logger scoped to a specific run.
func (l *FileLogger) NewRun(session, model string) *RunLogger {
	if l == nil {
		return &RunLogger{}
	}
	runID := newRunID()
	rl := &RunLogger{fl: l, runID: runID, session: session, model: model}
	rl.Log(LogEntry{Event: "run_start", Model: model})
	return rl
}

// RunLogger is scoped to a single orchestrator execution.
type RunLogger struct {
	fl        *FileLogger
	runID     string
	session   string
	model     string
	artifacts *ArtifactWriter
}

// RunID returns the identifier assigned to this run at NewRun.
func (r *RunLogger) RunID() string {
	if r == nil {
		return ""
	}
	return r.runID
}

// attachArtifacts mirrors every logged event into the per-run artifact folder
// (runs/<runID>/events.jsonl) — the source of truth for the UI.
func (r *RunLogger) attachArtifacts(w *ArtifactWriter) {
	if r == nil {
		return
	}
	r.artifacts = w
}

// Log writes a single entry.
func (r *RunLogger) Log(entry LogEntry) {
	if r == nil {
		return
	}
	entry.Time = time.Now().Format("2006-01-02 15:04:05.000")
	entry.RunID = r.runID
	entry.Session = r.session
	if entry.Model == "" {
		entry.Model = r.model
	}

	// Зеркало в артефакты запуска; ниже — прежний общий orchestrator.log.
	if r.artifacts != nil {
		r.artifacts.LogEvent(entry)
	}
	if r.fl == nil || r.fl.file == nil {
		return
	}

	r.fl.mu.Lock()
	defer r.fl.mu.Unlock()

	data, err := json.Marshal(entry)
	if err != nil {
		return
	}
	r.fl.file.Write(data)
	r.fl.file.Write([]byte("\n"))
}

// Thinking logs the model's text reasoning.
func (r *RunLogger) Thinking(iter int, text string) {
	r.Log(LogEntry{Event: "thinking", Iteration: iter, Output: truncateLog(text, 2000)})
}

// APICall logs an API request/response.
func (r *RunLogger) APICall(iter int, durationMs int64, tokensIn, tokensOut int) {
	r.Log(LogEntry{Event: "api_resp", Iteration: iter, DurationMs: durationMs, TokensIn: tokensIn, TokensOut: tokensOut})
}

// ToolCall logs the start of a tool execution.
func (r *RunLogger) ToolCall(iter int, tool, input string) {
	r.Log(LogEntry{Event: "tool_call", Iteration: iter, Tool: tool, Input: truncateLog(input, 1000)})
}

// ToolResult logs the result of a tool execution.
func (r *RunLogger) ToolResult(iter int, tool, output, errMsg string, durationMs int64) {
	r.Log(LogEntry{Event: "tool_result", Iteration: iter, Tool: tool, Output: truncateLog(output, 2000), Error: errMsg, DurationMs: durationMs})
}

// RunEnd logs the completion of the orchestrator run.
func (r *RunLogger) RunEnd(summary string, totalCost float64, isError bool) {
	entry := LogEntry{Event: "run_end", Summary: truncateLog(summary, 2000), CostUSD: totalCost}
	if isError {
		entry.Error = "true"
	}
	r.Log(entry)
}

// LogError logs an error event.
func (r *RunLogger) LogError(iter int, errMsg string) {
	r.Log(LogEntry{Event: "error", Iteration: iter, Error: errMsg})
}

func truncateLog(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "...[truncated]"
}
