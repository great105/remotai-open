package pty

import "testing"

func TestMatchAgentCmdline(t *testing.T) {
	cases := []struct {
		name    string
		cmdline string
		want    string
	}{
		{"kimi npm shim", `node "C:\Users\user\AppData\Roaming\npm\node_modules\@moonshot-ai\kimi-code\bin\kimi.js"`, "kimi"},
		{"kimi unix", `/usr/bin/node /home/u/.npm-global/lib/node_modules/@moonshot-ai/kimi-code/dist/cli.js -p hi`, "kimi"},
		{"claude npm", `node "C:\Users\user\AppData\Roaming\npm\node_modules\@anthropic-ai\claude-code\cli.js"`, "claude"},
		{"codex npm", `node /usr/local/lib/node_modules/@openai/codex/bin/codex.js exec`, "codex"},
		{"gemini npm", `node /usr/lib/node_modules/@google/gemini-cli/dist/index.js`, "gemini"},
		{"opencode", `node /home/u/.opencode/bin/opencode run "fix"`, "opencode"},
		// Трамплин grok: сам бинарь распакован в $GROK_HOME/bin, но запускает его
		// node из npm-пакета — до подмены на переднем плане видно только его.
		{"grok npm", `node "C:\Users\user\AppData\Roaming\npm\node_modules\@xai-official\grok\bin\grok"`, "grok"},
		{"grok unix", `/usr/bin/node /usr/lib/node_modules/@xai-official/grok/bin/grok -p hi`, "grok"},
		// Linux/macOS: в PATH лежит симлинк npm без имени пакета в пути — живой
		// стенд WSL 07.09.2026 (`node /usr/local/bin/gemini`, дочерний с флагом V8).
		{"gemini via PATH symlink", `node /usr/local/bin/gemini`, "gemini"},
		{"gemini child with v8 flag", `/usr/local/bin/node --max-old-space-size=7794 /usr/local/bin/gemini`, "gemini"},
		{"kimi via PATH symlink", `node /home/u/.npm-global/bin/kimi -p hi`, "kimi"},
		{"codex via PATH symlink", `node /usr/local/bin/codex exec "fix"`, "codex"},
		{"windows shim without package dir", `node "C:\Tools\bin\gemini.js"`, "gemini"},
		{"plain node script", `node server.js`, ""},
		{"node with agent-like dir but plain script", `node /srv/gemini/server.js`, ""},
		{"plain python", `python -m http.server 8080`, ""},
		{"empty", "", ""},
	}
	for _, c := range cases {
		if got := matchAgentCmdline(c.cmdline); got != c.want {
			t.Errorf("%s: matchAgentCmdline(%q) = %q, want %q", c.name, c.cmdline, got, c.want)
		}
	}
}

func TestAgentKindKnownAgents(t *testing.T) {
	for exe, want := range map[string]string{
		"kimi":              "kimi",
		"KIMI.EXE":          "kimi",
		"kimi-code":         "kimi",
		"github-copilot-":   "copilot",
		"grok":              "grok",
		"GROK.EXE":          "grok",
		"aider":             "aider",
		"opencode":          "opencode",
		"cursor-agent":      "cursor-agent",
		"copilot":           "copilot",
		"cline":             "cline",
		"kilo":              "kilo",
		"q":                 "amazon-q",
		"node":              "node",
		"powershell.exe":    "shell",
		"weird-binary-9000": "other",
	} {
		if got := AgentKind(exe); got != want {
			t.Errorf("AgentKind(%q) = %q, want %q", exe, got, want)
		}
	}
}

func TestIsAgentKind(t *testing.T) {
	for _, kind := range []string{"claude", "codex", "gemini", "kimi", "aider", "opencode", "copilot", "cursor-agent", "cline", "kilo", "amazon-q", "grok"} {
		if !IsAgentKind(kind) {
			t.Errorf("IsAgentKind(%q) = false, want true", kind)
		}
	}
	for _, kind := range []string{"node", "git", "shell", "other", ""} {
		if IsAgentKind(kind) {
			t.Errorf("IsAgentKind(%q) = true, want false", kind)
		}
	}
}
