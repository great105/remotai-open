package mcpmgr

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"tgcontrol/internal/procutil"
)

// Target — чей конфиг трогаем: агент + аккаунт.
type Target struct {
	Agent string
	// AccountID — ключ аккаунта в нашем хранилище выключенных ("default" у
	// основного). Нужен, чтобы выключенный сервер одного аккаунта не
	// «включился» в другом.
	AccountID string
	// Exe — исполняемый файл CLI (родной, не .cmd-обёртка; см. NativeExe).
	Exe string
	// ConfigDir — каталог аккаунта. Пусто = основной: переменная НЕ
	// подставляется, агент работает со своим каталогом по умолчанию.
	ConfigDir string
	// Home — домашняя папка пользователя. Подставляется в HOME/USERPROFILE
	// дочернего процесса: так основной аккаунт читается и пишется там же, где
	// мы его читаем (агент под службой видит чужой профиль), а тесты целиком
	// уходят во временный каталог и не могут задеть настоящий конфиг.
	Home string
}

// accountEnvName — переменная аккаунта у агента (то же, что AccountEnv в
// реестре; продублировано, чтобы пакет не тянул реестр ради двух строк).
func accountEnvName(agent string) string {
	switch agent {
	case "claude":
		return "CLAUDE_CONFIG_DIR"
	case "codex":
		return "CODEX_HOME"
	}
	return ""
}

// childEnv — окружение CLI. Унаследованная переменная аккаунта ВЫРЕЗАЕТСЯ:
// иначе «основной» аккаунт, запущенный из-под процесса, у которого случайно
// выставлен CLAUDE_CONFIG_DIR, писал бы в чужой каталог.
func (t Target) childEnv() []string {
	drop := map[string]bool{"HOME": true, "USERPROFILE": true}
	if n := accountEnvName(t.Agent); n != "" {
		drop[n] = true
	}
	env := make([]string, 0, len(os.Environ())+3)
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if drop[strings.ToUpper(k)] {
			continue
		}
		env = append(env, kv)
	}
	if t.Home != "" {
		env = append(env, "HOME="+t.Home)
		if runtime.GOOS == "windows" {
			env = append(env, "USERPROFILE="+t.Home)
		}
	}
	if n := accountEnvName(t.Agent); n != "" && t.ConfigDir != "" {
		env = append(env, n+"="+t.ConfigDir)
	}
	// Без цветов и интерактивных вопросов: ответ читает программа.
	env = append(env, "NO_COLOR=1", "CI=1")
	return env
}

// runner запускает CLI. Подменяется в тестах логики.
type runner func(ctx context.Context, t Target, args ...string) (stdout, stderr []byte, err error)

var errNoCLI = errors.New("no cli")

func runCLI(ctx context.Context, t Target, args ...string) ([]byte, []byte, error) {
	if t.Exe == "" {
		return nil, nil, errNoCLI
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	// Фоновая команда: окно человеку не нужно (AGENTS.md — только Hidden).
	cmd := procutil.Hidden(exec.CommandContext(ctx, t.Exe, args...))
	cmd.Env = t.childEnv()
	// Рабочая папка — домашняя, а не папка агента Remotai: Claude и Codex
	// подмешивают проектный конфиг текущей папки, а он тут ни при чём.
	if t.Home != "" {
		cmd.Dir = t.Home
	}
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	cmd.Stdin = nil
	err := cmd.Run()
	return out.Bytes(), errb.Bytes(), err
}

// NativeExe находит РОДНОЙ исполняемый файл агента по найденному в PATH.
//
// На Windows npm кладёт в PATH `claude.cmd`/`codex.cmd`, а это пакетный файл:
// cmd.exe разбирает его аргументы по своим правилам, и JSON с кавычками или
// значение с `&`/`%` превращается в другую команду. Для записи конфига это
// недопустимо — поэтому идём мимо обёртки прямо к .exe внутри пакета. Не нашли
// родной файл — честно отказываем, а не рискуем.
func NativeExe(agent, found string) string {
	if found == "" {
		return ""
	}
	if runtime.GOOS != "windows" || strings.EqualFold(filepath.Ext(found), ".exe") {
		return found
	}
	base := filepath.Dir(found)
	var patterns []string
	switch agent {
	case "claude":
		patterns = []string{
			filepath.Join(base, "node_modules", "@anthropic-ai", "claude-code", "bin", "claude.exe"),
			filepath.Join(base, "claude.exe"),
		}
	case "codex":
		patterns = []string{
			filepath.Join(base, "node_modules", "@openai", "codex", "node_modules", "@openai", "codex-win32-*", "vendor", "*", "bin", "codex.exe"),
			filepath.Join(base, "node_modules", "@openai", "codex", "vendor", "*", "bin", "codex.exe"),
			filepath.Join(base, "codex.exe"),
		}
	}
	for _, p := range patterns {
		matches, _ := filepath.Glob(p)
		sort.Strings(matches)
		for _, m := range matches {
			if st, err := os.Stat(m); err == nil && !st.IsDir() {
				return m
			}
		}
	}
	return ""
}
