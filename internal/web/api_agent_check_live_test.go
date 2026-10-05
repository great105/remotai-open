package web

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"tgcontrol/internal/agentcheck"
	"tgcontrol/internal/agents"
	"tgcontrol/internal/openrouter"
	"tgcontrol/internal/paths"
)

// Живой прогон на машине владельца — НЕ часть обычного `go test`.
//
//	REMOTAI_LIVE_AGENT_CHECK=1       — connection для Claude и Codex (без запросов к моделям);
//	REMOTAI_LIVE_AGENT_CHECK=claude  — плюс ОДИН настоящий check Claude.
//
// Печатает только маршрут, коды источников и итог. Значения (адреса, маски,
// пути) не печатает: вывод теста оседает в логах и чатах.
func TestLiveAgentConnection(t *testing.T) {
	mode := os.Getenv("REMOTAI_LIVE_AGENT_CHECK")
	if mode == "" {
		t.Skip("живой прогон: REMOTAI_LIVE_AGENT_CHECK=1|claude")
	}
	// Переменные сессии Claude Code, из которой запущен тест, у процесса
	// Remotai не бывает: снимаем, чтобы вложенный `claude -p` видел то же
	// окружение, что увидит агент из терминала Remotai.
	for _, kv := range os.Environ() {
		name := kv[:strings.Index(kv, "=")]
		if name == "CLAUDECODE" || strings.HasPrefix(name, "CLAUDE_CODE_") || name == "CLAUDE_PID" || name == "CLAUDE_EFFORT" {
			old := os.Getenv(name)
			os.Unsetenv(name)
			t.Cleanup(func() { os.Setenv(name, old) })
		}
	}
	agents.DetectAgents()
	s := &Server{openrouter: openrouter.NewStore(filepath.Join(paths.Base(), "openrouter.enc"))}

	for _, id := range []string{"claude", "codex"} {
		code, body := agentCallID(t, s.apiAgentConnection, "GET", "/api/agents/"+id+"/connection", "", id)
		var c struct {
			Route, Provider, Check string
			Endpoint, Auth, Model  struct {
				Kind       string
				Source     agentcheck.Source
				Why        string
				Overridden []agentcheck.Shadow
			}
			LoginFile *bool `json:"login_file"`
			CLI       agentcheck.CLIReport
			Notes     []string
		}
		_ = json.Unmarshal([]byte(body), &c)
		login := "?"
		if c.LoginFile != nil {
			login = map[bool]string{true: "да", false: "нет"}[*c.LoginFile]
		}
		t.Logf("%s: http=%d route=%s provider=%s check=%s | адрес: %s/%s | вход: %s из %s (%s), перекрыто %d | модель: %s | файл входа: %s | CLI спрошен=%v метод=%s | заметки=%v",
			id, code, c.Route, c.Provider, c.Check,
			c.Endpoint.Source.Kind, c.Endpoint.Why,
			c.Auth.Kind, c.Auth.Source.Kind, c.Auth.Why, len(c.Auth.Overridden),
			c.Model.Source.Kind, login, c.CLI.Asked, c.CLI.Method, c.Notes)
	}

	if mode != "claude" {
		return
	}
	_, body := agentCallID(t, s.apiAgentCheck, "POST", "/api/agents/claude/check", `{}`, "claude")
	var out struct {
		State  string
		ID     string
		Result agentcheck.Result
	}
	_ = json.Unmarshal([]byte(body), &out)
	for out.State == "running" {
		_, body = agentCallID(t, s.apiAgentCheck, "POST", "/api/agents/claude/check", `{"id":"`+out.ID+`"}`, "claude")
		_ = json.Unmarshal([]byte(body), &out)
	}
	r := out.Result
	t.Logf("claude check: ok=%v via=%s route=%s задержка=%d мс модель=%s причина=%q ответ=%q",
		r.OK, r.Via, r.Route, r.LatencyMS, r.Model, r.Reason, r.Reply)
}
