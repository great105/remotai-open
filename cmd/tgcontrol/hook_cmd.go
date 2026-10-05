package main

// `remotai hook <claude|codex>` — вызывает САМ ИИ-агент, из своего хука
// (Claude Code) или как программу notify (Codex). Человек эту команду не
// набирает: её подставляет в запуск агента клиент (см. launch_args в
// /api/agents и internal/agenthooks).
//
// Три правила, которые нельзя нарушать:
//   - ничего не печатать: у части хуков Claude stdout становится контекстом
//     модели (SessionStart, UserPromptSubmit);
//   - всегда выходить с 0 и быстро: ненулевой код хук Claude показывает
//     человеку ошибкой, а PreToolUse с кодом 2 вообще блокирует инструмент;
//   - у Codex обязательно позвать прежний notify человека: наш `-c notify=…`
//     его заменил, и без нас он просто перестанет работать (у владельца им
//     живёт плагин computer-use).
//
// Терминал, в котором работает агент, известен из окружения PTY
// (agenthooks.EnvFile ставит pty-host). Нет переменной — агент запущен не в
// нашем терминале или хост старый: событие некуда класть, молча выходим.

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"tgcontrol/internal/agenthooks"
	"tgcontrol/internal/procutil"
)

func runHook(args []string) (code int) {
	// Хук не имеет права уронить агента ни при каком раскладе.
	defer func() {
		if recover() != nil {
			code = 0
		}
	}()
	if len(args) == 0 {
		return 0
	}
	now := time.Now()
	spool := os.Getenv(agenthooks.EnvFile)
	switch args[0] {
	case "claude":
		// stdin читаем всегда: Claude пишет в него JSON и ждёт, пока его заберут.
		raw, _ := io.ReadAll(io.LimitReader(os.Stdin, agenthooks.MaxInput))
		if spool == "" {
			return 0
		}
		if ev, ok := agenthooks.ParseClaude(raw, now); ok {
			// The runtime status file lives under the selected Claude account,
			// not necessarily ~/.claude. Keep the path local to this PTY's hook.
			ev.ConfigHome = os.Getenv("CLAUDE_CONFIG_DIR")
			_ = agenthooks.Append(spool, ev)
		}
	case "codex":
		// Codex кладёт JSON последним аргументом.
		payload := ""
		if len(args) > 1 {
			payload = args[len(args)-1]
		}
		if spool != "" {
			if ev, ok := agenthooks.ParseCodexNotify(payload, now); ok {
				ev.ConfigHome = os.Getenv("CODEX_HOME")
				if ev.ConfigHome == "" {
					if home, err := os.UserHomeDir(); err == nil {
						ev.ConfigHome = filepath.Join(home, ".codex")
					}
				}
				ev.SessionScope = agenthooks.CodexSessionScope(ev.ConfigHome, ev.SessionID, now)
				_ = agenthooks.Append(spool, ev)
			}
		}
		chainCodexNotify(payload)
	}
	return 0
}

// chainCodexNotify вызывает notify из config.toml человека с тем же JSON —
// так, как вызвал бы его сам Codex, не будь нашего `-c notify=…`.
func chainCodexNotify(payload string) {
	home := os.Getenv("CODEX_HOME")
	if home == "" {
		h, err := os.UserHomeDir()
		if err != nil {
			return
		}
		home = filepath.Join(h, ".codex")
	}
	argv, state := agenthooks.OriginalCodexNotify(home)
	if state != agenthooks.CodexNotifyChainable {
		return
	}
	cmd := exec.Command(argv[0], append(argv[1:], payload)...)
	procutil.Hidden(cmd)
	if cmd.Start() == nil {
		// Codex своего notify не ждёт — не ждём и мы.
		_ = cmd.Process.Release()
	}
}
