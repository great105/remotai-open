package agents

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"tgcontrol/internal/agenthooks"
	"tgcontrol/internal/paths"
)

// LaunchArgs — аргументы, которые клиент дописывает к КАЖДОМУ локальному
// запуску этого агента (новому и «продолжить»): подключают структурные сигналы
// агента — «закончил», «ждёт разрешения», «задал вопрос» (internal/agenthooks).
//
// Живут на агенте, а не в клиенте, по общему правилу реестра: путь к файлу
// настроек и к remotai знает только компьютер. Пусто — у агента таких сигналов
// нет (или подключать их здесь небезопасно), и клиент ничего не дописывает.
// На SSH-сервер не едут: пути в них — этого компьютера.
func (d *AgentDescriptor) LaunchArgs() []string {
	exe, err := os.Executable()
	if err != nil || isTestBinary(exe) {
		// Под `go test` путь — это тестовый бинарь, который исчезнет после
		// прогона: записать его в настоящие настройки значило бы сломать хуки
		// живому агенту до его следующего запроса.
		return []string{}
	}
	switch d.ID {
	case "claude":
		path, err := agenthooks.EnsureClaudeSettings(filepath.Join(paths.Base(), "agent-hooks"), exe)
		if err != nil {
			return []string{}
		}
		return []string{"--settings", path}
	case "codex":
		// Свой notify человека, который мы не разобрали, подменять нельзя:
		// `-c notify=…` его заменит, а вызвать его вместо Codex будет нечем.
		if _, st := agenthooks.OriginalCodexNotify(hookCodexHome()); st == agenthooks.CodexNotifyUnknown {
			return []string{}
		}
		arg, ok := agenthooks.CodexNotifyArg(exe, runtime.GOOS == "windows")
		if !ok {
			return []string{}
		}
		return []string{"-c", arg}
	}
	return []string{}
}

func hookCodexHome() string {
	if h := os.Getenv("CODEX_HOME"); h != "" {
		return h
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".codex")
}

func isTestBinary(exe string) bool {
	base := strings.TrimSuffix(strings.ToLower(filepath.Base(exe)), ".exe")
	return strings.HasSuffix(base, ".test")
}
