package pty

// Разметка команд в терминале: OSC 133 (FinalTerm). ST-10, T-39.
//
// ЗАЧЕМ. Шелл отмечает в потоке, где приглашение (A), где началась команда
// (B — конец приглашения), где начался её вывод (C) и чем она кончилась
// (D;<код>). По этим отметкам клиент даёт «копировать команду», «копировать
// вывод» и «к началу предыдущей команды». Сами шеллы (кроме fish ≥ 4) таких
// отметок не шлют — их добавляет наш небольшой стартовый код.
//
// ГЛАВНОЕ ОГРАНИЧЕНИЕ — не сломать шелл человека. Поэтому:
//   - выключено по умолчанию (config.ShellIntegration) и действует только на
//     НОВЫЕ терминалы: живые шеллы не трогаем никогда;
//   - пользовательская конфигурация подключается как обычно и ДО разметки:
//     bash — наш --rcfile сам подключает ~/.bashrc; zsh — наш ZDOTDIR
//     подключает пользовательские .zshenv/.zprofile/.zshrc/.zlogin с его
//     ZDOTDIR; PowerShell — профиль грузится как всегда, наш код оборачивает
//     уже итоговую функцию prompt;
//   - cmd, fish, sh, ssh и всё незнакомое запускается как раньше (argv =
//     [shell] — байт в байт прежний запуск); так же при отказе в окружении
//     (REMOTAI_SHELL_INTEGRATION=0) и там, где разметку уже ставит VS Code;
//   - маркеры завершаются BEL: так их вырезает ansiRe (events.go).
//
// Диагностика — только вид шелла, без содержимого команд и вывода (I-15).

import (
	"bytes"
	"embed"
	"encoding/base64"
	"log"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"unicode/utf16"

	"tgcontrol/internal/paths"
)

// EnvShellIntegration — переменная окружения с двумя ролями:
//   - агент → pty-host: "1" значит «запусти шелл с разметкой команд». Флаг
//     командной строки для этого не годится: живой хост старой версии его не
//     знает, а лишняя переменная никому не мешает (I-14). Хост снимает её
//     сразу, шелл её не наследует;
//   - человек → агент: "0" в окружении агента — отказ, он сильнее настройки.
const EnvShellIntegration = "REMOTAI_SHELL_INTEGRATION"

// envUserZDOTDIR — куда смотреть за пользовательскими файлами zsh: прежний
// ZDOTDIR или HOME. Наш .zshenv снимает её, когда стартовые файлы кончились.
const envUserZDOTDIR = "REMOTAI_USER_ZDOTDIR"

// envVSCodeShellIntegration — VS Code уже размечает этот терминал своими
// маркерами (агент запущен из его встроенного терминала). Вторая разметка
// поверх чужой ничего не даст, а риск сломать приглашение добавит.
const envVSCodeShellIntegration = "VSCODE_SHELL_INTEGRATION"

// Скрипты вшиты в бинарь и при запуске пишутся в каталог состояния агента:
// bash и zsh читают стартовые файлы только с диска. Файлы zsh лежат в
// репозитории без точки (go:embed пропускает dot-файлы каталога), на диск
// ложатся под именами, которые ищет zsh.
//
//go:embed shell_integration/bash.sh shell_integration/pwsh.ps1 shell_integration/zsh
var shellIntegrationFS embed.FS

var shellIntegrationFiles = []struct{ src, dst string }{
	{"shell_integration/bash.sh", "bash.sh"},
	{"shell_integration/zsh/zshenv", "zsh/.zshenv"},
	{"shell_integration/zsh/zprofile", "zsh/.zprofile"},
	{"shell_integration/zsh/zshrc", "zsh/.zshrc"},
	{"shell_integration/zsh/zlogin", "zsh/.zlogin"},
}

// shellIntegrationRoot — каталог скриптов: рядом с pty.json и pty-scrollback
// (paths.StateFile). Установленная раскладка — <Base>/shell-integration,
// переносная — ~/.tgcontrol-shell-integration, как у остальных файлов
// состояния: класть скрипты рядом с exe в «Загрузках» нельзя.
func shellIntegrationRoot() string {
	return paths.StateFile("shell-integration")
}

// shellIntegrationKind — какой разметкой умеем снабдить этот шелл: "bash",
// "zsh", "pwsh" (Windows PowerShell и PowerShell 7) или "" — никакой.
// Смотрим только на имя файла: путь к шеллу уже разрешён через LookPath.
func shellIntegrationKind(shell string) string {
	name := shell
	if i := strings.LastIndexAny(name, `/\`); i >= 0 {
		name = name[i+1:]
	}
	name = strings.TrimSuffix(strings.ToLower(name), ".exe")
	switch name {
	case "bash":
		return "bash"
	case "zsh":
		return "zsh"
	case "pwsh", "powershell":
		return "pwsh"
	}
	return ""
}

// shellLaunch — чистое правило запуска шелла: argv (argv[0] — путь к шеллу) и
// переменные, которые лягут поверх окружения. enabled=false, незнакомый шелл,
// отказ в окружении — ровно [shell] и nil, прежний запуск. root — каталог
// скриптов (shellIntegrationRoot), env — окружение, которое унаследует шелл.
func shellLaunch(shell string, enabled bool, root string, env []string) (argv []string, extraEnv []string) {
	return shellLaunchFor(runtime.GOOS, shell, enabled, root, env)
}

// shellLaunchFor — shellLaunch для заданной ОС: правило одно, а проверять его
// нужно для всех трёх систем на любой машине.
func shellLaunchFor(goos, shell string, enabled bool, root string, env []string) ([]string, []string) {
	plain := []string{shell}
	if !enabled || shell == "" {
		return plain, nil
	}
	fold := goos == "windows"
	if v, _ := lookupEnv(env, envVSCodeShellIntegration, fold); v != "" {
		return plain, nil
	}
	if v, ok := lookupEnv(env, EnvShellIntegration, fold); ok && v == "0" {
		return plain, nil
	}
	switch shellIntegrationKind(shell) {
	case "bash":
		// На Windows bash — это Git Bash или bash.exe из WSL: у них свои пути
		// и свой rc, чужой --rcfile с путём Windows им не подходит.
		if goos == "windows" || root == "" {
			return plain, nil
		}
		return []string{shell, "--rcfile", path.Join(root, "bash.sh")}, nil
	case "zsh":
		if goos == "windows" || root == "" {
			return plain, nil
		}
		ours := path.Join(root, "zsh")
		user, _ := lookupEnv(env, "ZDOTDIR", fold)
		if user == "" {
			user, _ = lookupEnv(env, "HOME", fold)
		}
		// Без HOME пользовательские файлы искать негде; ZDOTDIR, уже равный
		// нашему, — это вложенный запуск посреди старта: не зацикливаемся.
		if user == "" || path.Clean(user) == ours {
			return plain, nil
		}
		return plain, []string{"ZDOTDIR=" + ours, envUserZDOTDIR + "=" + user}
	case "pwsh":
		enc := pwshEncodedCommand()
		if enc == "" {
			return plain, nil
		}
		// Без -NoProfile: профиль пользователя грузится как обычно и до нашего
		// кода. -ExecutionPolicy не трогаем: -EncodedCommand политика не
		// ограничивает (проверено живьём под Restricted).
		return []string{shell, "-NoExit", "-EncodedCommand", enc}, nil
	}
	return plain, nil
}

// prepareShellLaunch — shellLaunch с подготовкой диска: скрипты bash/zsh
// пишутся в каталог состояния перед запуском. Не удалось записать — шелл
// запускается как раньше: терминал без разметки лучше, чем без терминала.
func prepareShellLaunch(shell string, enabled bool, env []string) ([]string, []string) {
	if !enabled {
		return []string{shell}, nil
	}
	root := shellIntegrationRoot()
	argv, extraEnv := shellLaunch(shell, true, root, env)
	if (len(argv) > 1 || len(extraEnv) > 0) && shellIntegrationKind(shell) != "pwsh" {
		if err := writeShellIntegrationScripts(root); err != nil {
			log.Printf("[PTY] разметка команд: скрипты не записаны (%v) — шелл без неё", err)
			return []string{shell}, nil
		}
	}
	return argv, extraEnv
}

// localShellLaunch — непостоянный путь (терминал без pty-host): окружение
// шелла — окружение самого агента.
func localShellLaunch(shell string, enabled bool) ([]string, []string) {
	return prepareShellLaunch(shell, enabled, os.Environ())
}

// writeShellIntegrationScripts раскладывает вшитые скрипты по root. Идемпотентно:
// совпадающий файл не переписывается. Запись атомарная (временный файл +
// rename), чтобы шелл, стартующий в соседнем терминале в ту же секунду, не
// прочёл половину файла. Каталог 0700, файлы 0600: там код, который выполнит
// шелл пользователя, и чужим его подменять нельзя.
func writeShellIntegrationScripts(root string) error {
	zdir := filepath.Join(root, "zsh")
	if err := os.MkdirAll(zdir, 0o700); err != nil {
		return err
	}
	_ = os.Chmod(root, 0o700)
	_ = os.Chmod(zdir, 0o700)
	for _, f := range shellIntegrationFiles {
		data, err := shellIntegrationFS.ReadFile(f.src)
		if err != nil {
			return err
		}
		data = normalizeLF(data)
		dst := filepath.Join(root, filepath.FromSlash(f.dst))
		if cur, err := os.ReadFile(dst); err == nil && bytes.Equal(cur, data) {
			_ = os.Chmod(dst, 0o600)
			continue
		}
		if err := writeFileAtomic(dst, data, 0o600); err != nil {
			return err
		}
	}
	return nil
}

func writeFileAtomic(dst string, data []byte, perm os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".remotai-si-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	_ = os.Chmod(name, perm)
	if err := os.Rename(name, dst); err != nil {
		os.Remove(name)
		return err
	}
	return nil
}

// normalizeLF — скрипты исполняются bash и zsh, а сборка идёт и с Windows:
// checkout с core.autocrlf=true подставил бы CRLF, и bash споткнулся бы о
// «$'\r': command not found» на каждой строке (так уже падал install.sh,
// см. .gitattributes). Поэтому на диск всегда уходит LF.
func normalizeLF(b []byte) []byte {
	return bytes.ReplaceAll(b, []byte("\r\n"), []byte("\n"))
}

var (
	pwshEncodedOnce sync.Once
	pwshEncoded     string
)

// pwshEncodedCommand — pwsh.ps1 для -EncodedCommand (см. encodePwsh).
func pwshEncodedCommand() string {
	pwshEncodedOnce.Do(func() {
		src, err := shellIntegrationFS.ReadFile("shell_integration/pwsh.ps1")
		if err != nil {
			return
		}
		pwshEncoded = encodePwsh(string(src))
	})
	return pwshEncoded
}

// encodePwsh — код PowerShell в виде аргумента -EncodedCommand: base64 от
// UTF-16LE. Полнострочные комментарии и пустые строки вырезаются: командная
// строка Windows ограничена 32K символов, а base64 от UTF-16 раздувает код
// вчетверо.
func encodePwsh(src string) string {
	var code []string
	for _, line := range strings.Split(string(normalizeLF([]byte(src))), "\n") {
		t := strings.TrimSpace(line)
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		code = append(code, line)
	}
	units := utf16.Encode([]rune(strings.Join(code, "\n")))
	raw := make([]byte, 2*len(units))
	for i, u := range units {
		raw[2*i] = byte(u)
		raw[2*i+1] = byte(u >> 8)
	}
	return base64.StdEncoding.EncodeToString(raw)
}

// lookupEnv ищет KEY в списке KEY=VAL. Побеждает ПОСЛЕДНЕЕ вхождение — так же
// дубликаты разрешает os/exec. fold — ключи без учёта регистра (Windows).
func lookupEnv(env []string, key string, fold bool) (string, bool) {
	val, found := "", false
	for _, kv := range env {
		eq := strings.IndexByte(kv, '=')
		// eq == 0 — служебные «=C:=C:\…» Windows, это не переменные.
		if eq <= 0 {
			continue
		}
		if k := kv[:eq]; k == key || (fold && strings.EqualFold(k, key)) {
			val, found = kv[eq+1:], true
		}
	}
	return val, found
}

// mergeEnv кладёт extra поверх base: одноимённые переменные base убираются,
// extra дописываются в конец. Пустой extra — base как есть (тот же срез), то
// есть прежний запуск байт в байт.
func mergeEnv(base, extra []string, fold bool) []string {
	if len(extra) == 0 {
		return base
	}
	keys := make(map[string]bool, len(extra))
	norm := func(k string) string {
		if fold {
			return strings.ToUpper(k)
		}
		return k
	}
	for _, kv := range extra {
		if eq := strings.IndexByte(kv, '='); eq > 0 {
			keys[norm(kv[:eq])] = true
		}
	}
	out := make([]string, 0, len(base)+len(extra))
	for _, kv := range base {
		if eq := strings.IndexByte(kv, '='); eq > 0 && keys[norm(kv[:eq])] {
			continue
		}
		out = append(out, kv)
	}
	return append(out, extra...)
}

// hostSpawnEnv — окружение процесса pty-host. nil значит «унаследовать как
// есть»: при выключенной разметке хост запускается ровно как раньше.
//   - включено: EnvShellIntegration=1, но пользовательский отказ "0" в
//     окружении агента сильнее — его оставляем, хост прочтёт «выключено»;
//   - выключено: унаследованную откуда-то «1» снимаем, иначе она включила бы
//     разметку вопреки настройке.
func hostSpawnEnv(enabled bool, env []string, fold bool) []string {
	cur, has := lookupEnv(env, EnvShellIntegration, fold)
	if enabled {
		if has && cur == "0" {
			return nil
		}
		return mergeEnv(env, []string{EnvShellIntegration + "=1"}, fold)
	}
	if has && cur == "1" {
		out := make([]string, 0, len(env))
		for _, kv := range env {
			if eq := strings.IndexByte(kv, '='); eq > 0 {
				k := kv[:eq]
				if k == EnvShellIntegration || (fold && strings.EqualFold(k, EnvShellIntegration)) {
					continue
				}
			}
			out = append(out, kv)
		}
		return out
	}
	return nil
}
