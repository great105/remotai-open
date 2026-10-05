package pty

import (
	"path/filepath"
	"strings"
)

// AgentKind classifies a process name into a short stable identifier the
// frontend uses to select the icon / quick-keys preset.
func AgentKind(processName string) string {
	n := strings.ToLower(processName)
	n = strings.TrimSuffix(n, ".exe")
	// Strip any directory prefix if a full path slips in.
	n = filepath.Base(n)
	switch n {
	case "claude":
		return "claude"
	case "codex":
		return "codex"
	case "gemini":
		return "gemini"
	// «kimi-code» — так процесс Kimi называет СЕБЯ на Linux/macOS (process.title
	// у Node-скрипта npm-пакета @moonshot-ai/kimi-code): в /proc/<pid>/stat и
	// kern.proc это comm `kimi-code`, а не `kimi`. Живой стенд WSL 07.09.2026:
	// `fg=kimi-code kind=other` — агент запускался, но продукт держал его за
	// обычный процесс (ни клавиш агента, ни «агент закончил», ни режима прокрутки).
	case "kimi", "kimi-cli", "kimi-code":
		return "kimi"
	case "grok":
		return "grok"
	case "aider":
		return "aider"
	case "opencode":
		return "opencode"
	// «github-copilot-» — comm в /proc обрезан до 15 байт (TASK_COMM_LEN).
	case "copilot", "github-copilot-cli", "github-copilot-":
		return "copilot"
	case "cursor-agent":
		return "cursor-agent"
	case "cline":
		return "cline"
	case "kilo", "kilocode":
		return "kilo"
	case "q", "amazon-q", "qcli":
		return "amazon-q"
	case "git":
		return "git"
	case "npm", "node", "nodejs", "npx", "yarn", "pnpm":
		return "node"
	case "go", "gofmt":
		return "go"
	case "python", "python3", "py":
		return "python"
	case "docker":
		return "docker"
	case "powershell", "pwsh", "cmd", "bash", "sh", "zsh", "fish", "dash":
		return "shell"
	default:
		return "other"
	}
}

// IsAgentKind reports whether kind is an interactive AI coding agent. Such
// agents spawn long-lived helper children (MCP servers, tool subprocesses)
// that never exit for the life of the session, so the foreground walker must
// STOP at the agent rather than descend into helpers; the waiting/idle
// transitions are likewise keyed on the foreground being the agent itself.
func IsAgentKind(kind string) bool {
	switch kind {
	case "claude", "codex", "gemini", "kimi", "aider", "opencode",
		"copilot", "cursor-agent", "cline", "kilo", "amazon-q", "grok":
		return true
	}
	return false
}

// isRuntimeExe reports whether name is a generic script runtime. npm/pip
// distributed CLIs (kimi, gemini, opencode, …) run as node.exe/python.exe,
// so the executable name alone doesn't reveal the agent — the command line
// does (see matchAgentCmdline).
func isRuntimeExe(name string) bool {
	switch name {
	case "node", "nodejs", "bun", "deno", "python", "python3", "py":
		return true
	}
	return false
}

// agentCmdlineMarkers maps a resolved agent kind to distinctive substrings of
// the process command line (npm package dirs, shim names). Ordered: the first
// match wins. Checked only for generic runtimes (isRuntimeExe), so plain
// `node server.js` never matches.
var agentCmdlineMarkers = []struct {
	kind    string
	markers []string
}{
	{"kimi", []string{"@moonshot-ai", "@moonshotai", "kimi-cli", "kimi-code", "kimi.cmd", "kimi.js", `\kimi\`, "/kimi/"}},
	{"claude", []string{"claude-code", "@anthropic-ai", "claude.cmd", `claude\cli`, "claude/bin"}},
	{"codex", []string{"@openai/codex", "codex-cli", "codex.cmd", `\codex\`, "/codex/"}},
	{"gemini", []string{"gemini-cli", "@google/gemini", "gemini.cmd"}},
	{"opencode", []string{"opencode"}},
	{"cursor-agent", []string{"cursor-agent"}},
	{"copilot", []string{"@github/copilot", "copilot-cli", "@githubnext"}},
	{"cline", []string{"cline-cli", `\cline\`, "/cline/"}},
	{"kilo", []string{"kilo-code", "kilocode"}},
	{"aider", []string{`\aider\`, "/aider/", "aider.cmd"}},
	{"amazon-q", []string{"amazon-q", "q-cli", `\amazon q\`, "/amazon-q/"}},
	// У grok на переднем плане обычно оказывается уже свой бинарь (его имя ловит
	// AgentKind), но путь до него лежит через node: `bin/grok` из npm-пакета —
	// это трамплин, который распаковывает и запускает `$GROK_HOME/bin/grok.exe`.
	// Пока жив трамплин, видно только node, поэтому маркеры нужны.
	// `\grok\` и `/grok/` намеренно НЕ берём: домашний каталог агента — `.grok`,
	// и такой маркер пришлось бы писать с точкой, а он всё равно ничего не
	// добавляет к имени пакета.
	{"grok", []string{"@xai-official", "grok.cmd", "grok-cli"}},
}

// matchAgentCmdline returns the agent kind revealed by a runtime process
// command line, or "" if it doesn't look like a known agent CLI.
func matchAgentCmdline(cmdline string) string {
	if cmdline == "" {
		return ""
	}
	lower := strings.ToLower(cmdline)
	for _, m := range agentCmdlineMarkers {
		for _, marker := range m.markers {
			if strings.Contains(lower, marker) {
				return m.kind
			}
		}
	}
	return matchAgentScriptName(cmdline)
}

// matchAgentScriptName — вторая линия после маркеров npm-пакетов: имя самого
// скрипта, который запустил runtime. На Linux/macOS npm кладёт в PATH симлинк
// `/usr/local/bin/gemini → …/@google/gemini-cli/bundle/gemini.js`, и в
// /proc/<pid>/cmdline остаётся `node /usr/local/bin/gemini` — ни одного маркера
// пакета, хотя это тот же Gemini CLI. Живой стенд WSL 07.09.2026: `fg=node
// kind=node` при открытом Gemini. Берём первые аргументы после runtime (флаги
// вида `--max-old-space-size=…` пропускаем), базовое имя без расширения и
// спрашиваем AgentKind: только известный агент даёт ответ, `node server.js`
// по-прежнему никто.
func matchAgentScriptName(cmdline string) string {
	tokens := splitCmdline(cmdline)
	if len(tokens) < 2 {
		return ""
	}
	checked := 0
	for _, tok := range tokens[1:] {
		if strings.HasPrefix(tok, "-") {
			continue
		}
		base := strings.ToLower(pathBase(strings.Trim(tok, `"'`)))
		for _, ext := range []string{".js", ".cjs", ".mjs", ".cmd", ".exe", ".ps1", ".py"} {
			base = strings.TrimSuffix(base, ext)
		}
		if kind := AgentKind(base); IsAgentKind(kind) {
			return kind
		}
		checked++
		if checked >= 3 {
			break
		}
	}
	return ""
}

// pathBase — базовое имя для ОБЕИХ раскладок разделителей: cmdline Windows
// приходит с `\`, а filepath.Base на Linux/macOS их не режет (тест на CI).
func pathBase(p string) string {
	if i := strings.LastIndexAny(p, `/\`); i >= 0 {
		return p[i+1:]
	}
	return p
}

// splitCmdline режет командную строку по пробелам, уважая двойные кавычки
// (Windows) — /proc отдаёт аргументы через NUL, которые processCmdline уже
// заменил на пробелы.
func splitCmdline(cmdline string) []string {
	var out []string
	var cur strings.Builder
	inQuote := false
	flush := func() {
		if cur.Len() > 0 {
			out = append(out, cur.String())
			cur.Reset()
		}
	}
	for _, r := range cmdline {
		switch {
		case r == '"':
			inQuote = !inQuote
		case r == ' ' && !inQuote:
			flush()
		default:
			cur.WriteRune(r)
		}
	}
	flush()
	return out
}
