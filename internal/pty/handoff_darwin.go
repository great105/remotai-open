//go:build darwin

// «Открыть на ПК»: перенести работу из телефона в терминал на самом маке.
package pty

import (
	"fmt"
	"os/exec"
	"strings"

	"tgcontrol/internal/procutil"
)

// OpenOnHost открывает Terminal.app в нужной папке и, если задана команда,
// выполняет её.
//
// Через osascript, а не `open -a Terminal <путь>`: последний умеет только
// открыть папку, а нам нужно выполнить в новом окне ещё и команду (ради этого
// handoff и существует — человек продолжает на компьютере ровно ту работу,
// которую вёл с телефона). Кавычки экранируем: путь с пробелом или апострофом
// иначе разорвал бы скрипт.
func OpenOnHost(cwd, command string) error {
	script := "cd " + shellQuote(cwd)
	if command != "" {
		script += "; " + command
	}
	osa := fmt.Sprintf(`tell application "Terminal"
	activate
	do script %s
end tell`, appleScriptString(script))

	cmd := exec.Command("/usr/bin/osascript", "-e", osa)
	procutil.Hidden(cmd)
	if out, err := cmd.CombinedOutput(); err != nil {
		msg := strings.TrimSpace(string(out))
		if msg != "" {
			return fmt.Errorf("osascript: %s", msg)
		}
		return fmt.Errorf("osascript: %w", err)
	}
	return nil
}

// shellQuote — одинарные кавычки для shell: внутри них спецсимволы не
// раскрываются, а сам апостроф закрывается и вставляется отдельно.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// appleScriptString — строковый литерал AppleScript (двойные кавычки, экранируем
// обратный слэш и кавычку).
func appleScriptString(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return `"` + s + `"`
}
