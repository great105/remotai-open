package agents

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/shirou/gopsutil/v4/process"
)

// DiscoveredSession represents an existing agent session found on the machine.
type DiscoveredSession struct {
	AgentType       string `json:"agent_type"` // "claude", "codex", etc.
	SessionID       string `json:"session_id"`
	Cwd             string `json:"cwd"`
	PID             int    `json:"pid"`
	StartedAt       int64  `json:"started_at"` // unix ms
	Name            string `json:"name"`       // display name or auto-generated
	IsAlive         bool   `json:"is_alive"`   // process still running
	Status          string `json:"status,omitempty"`
	StatusUpdatedAt int64  `json:"status_updated_at,omitempty"`
	Kind            string `json:"kind,omitempty"`
}

// RuntimeStatus is the precise state Claude Code writes for its live process.
type RuntimeStatus struct {
	Status    string
	UpdatedAt int64
	Kind      string
	StartedAt int64
	// SessionID — номер беседы, который Claude сам пишет в файл своего PID.
	// Нужен усыплению: он есть и у Claude, запущенного без наших хуков.
	SessionID string
}

// DiscoverAll scans for existing sessions from all known agents.
func DiscoverAll() []DiscoveredSession {
	var all []DiscoveredSession
	all = append(all, discoverClaude()...)
	// Future: all = append(all, discoverCodex()...)
	return all
}

// discoverClaude reads ~/.claude/sessions/*.json
func discoverClaude() []DiscoveredSession {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}

	sessDir := filepath.Join(home, ".claude", "sessions")
	entries, err := os.ReadDir(sessDir)
	if err != nil {
		return nil
	}

	var result []DiscoveredSession
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}

		data, err := os.ReadFile(filepath.Join(sessDir, entry.Name()))
		if err != nil {
			continue
		}

		var sess struct {
			PID             int    `json:"pid"`
			SessionID       string `json:"sessionId"`
			Cwd             string `json:"cwd"`
			StartedAt       int64  `json:"startedAt"`
			Name            string `json:"name"`
			Status          string `json:"status"`
			StatusUpdatedAt any    `json:"statusUpdatedAt"`
			Kind            string `json:"kind"`
		}
		if err := json.Unmarshal(data, &sess); err != nil {
			continue
		}
		if sess.SessionID == "" {
			continue
		}

		alive := isProcessAlive(sess.PID, sess.StartedAt)

		// Generate display name from cwd
		name := sess.Name
		if name == "" {
			name = filepath.Base(sess.Cwd)
		}

		result = append(result, DiscoveredSession{
			AgentType:       "claude",
			SessionID:       sess.SessionID,
			Cwd:             sess.Cwd,
			PID:             sess.PID,
			StartedAt:       sess.StartedAt,
			Name:            name,
			IsAlive:         alive,
			Status:          sess.Status,
			StatusUpdatedAt: timestampMillis(sess.StatusUpdatedAt),
			Kind:            sess.Kind,
		})
	}

	return result
}

// isProcessAlive checks if a process with the given PID exists.
func isProcessAlive(pid int, startedAt int64) bool {
	if pid <= 0 {
		return false
	}
	if runtime.GOOS == "windows" {
		return checkWindowsProcess(pid, startedAt)
	}
	// Unix: signal 0
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = p.Signal(os.Signal(nil))
	return err == nil
}

func checkWindowsProcess(pid int, startedAt int64) bool {
	exists, err := process.PidExists(int32(pid))
	if err != nil || !exists {
		return false
	}
	if startedAt <= 0 {
		return true
	}
	p, err := process.NewProcess(int32(pid))
	if err != nil {
		return false
	}
	createdAt, err := p.CreateTime()
	if err != nil || createdAt <= 0 {
		return true
	}
	// A process may start shortly before Claude writes the session file. A
	// process created materially after the recorded start means PID reuse.
	return createdAt <= startedAt+2*60*1000
}

// ClaudeRuntimeStatus reads the exact ~/.claude/sessions/<pid>.json state used
// by Claude Code. It is intentionally best-effort; other agents simply fall
// back to PTY output-based detection.
func ClaudeRuntimeStatus(pid int) (RuntimeStatus, bool) {
	return ClaudeRuntimeStatusAt(pid, "")
}

// ClaudeRuntimeStatusAt reads the status for the Claude profile selected by
// CLAUDE_CONFIG_DIR. An empty configDir means the standard ~/.claude profile.
// The caller must use the directory reported by this PTY's own hook: looking
// through other accounts can accidentally attribute a stale PID to this one.
func ClaudeRuntimeStatusAt(pid int, configDir string) (RuntimeStatus, bool) {
	if pid <= 0 {
		return RuntimeStatus{}, false
	}
	if configDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return RuntimeStatus{}, false
		}
		configDir = filepath.Join(home, ".claude")
	}
	data, err := os.ReadFile(filepath.Join(configDir, "sessions", fmt.Sprintf("%d.json", pid)))
	if err != nil {
		return RuntimeStatus{}, false
	}
	var sess struct {
		Status          string `json:"status"`
		StatusUpdatedAt any    `json:"statusUpdatedAt"`
		Kind            string `json:"kind"`
		StartedAt       int64  `json:"startedAt"`
		SessionID       string `json:"sessionId"`
	}
	if json.Unmarshal(data, &sess) != nil || sess.Status == "" {
		return RuntimeStatus{}, false
	}
	return RuntimeStatus{
		Status: sess.Status, UpdatedAt: timestampMillis(sess.StatusUpdatedAt),
		Kind: sess.Kind, StartedAt: sess.StartedAt, SessionID: sess.SessionID,
	}, true
}

func timestampMillis(v any) int64 {
	switch x := v.(type) {
	case float64:
		if x > 0 {
			return int64(x)
		}
	case string:
		if n, err := strconv.ParseInt(x, 10, 64); err == nil {
			return n
		}
		if t, err := time.Parse(time.RFC3339Nano, x); err == nil {
			return t.UnixMilli()
		}
	}
	return 0
}

// FormatAge formats a duration since startedAt (unix ms) as human-readable.
func FormatAge(startedAtMs int64) string {
	if startedAtMs <= 0 {
		return "?"
	}
	d := time.Since(time.UnixMilli(startedAtMs))
	if d < time.Minute {
		return fmt.Sprintf("%dс", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dмин", int(d.Minutes()))
	}
	if d < 24*time.Hour {
		return fmt.Sprintf("%dч %dмин", int(d.Hours()), int(d.Minutes())%60)
	}
	return fmt.Sprintf("%dд %dч", int(d.Hours())/24, int(d.Hours())%24)
}
