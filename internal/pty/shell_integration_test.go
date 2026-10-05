package pty

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf16"
)

// T-39e — правило запуска шелла с разметкой команд OSC 133 (ST-10). Правило
// чистое (shellLaunchFor принимает ОС параметром), поэтому все три системы
// проверяются на любой машине. Живые проверки — shell_integration_live_test.go.

const siRoot = "/state/shell-integration"

var siPowerShell = `C:\Windows\System32\WindowsPowerShell\v1.0\powershell.exe`

func decodePwsh(t *testing.T, enc string) string {
	t.Helper()
	raw, err := base64.StdEncoding.DecodeString(enc)
	if err != nil || len(raw)%2 != 0 {
		t.Fatalf("-EncodedCommand не base64 от UTF-16LE: %v (len=%d)", err, len(raw))
	}
	units := make([]uint16, len(raw)/2)
	for i := range units {
		units[i] = uint16(raw[2*i]) | uint16(raw[2*i+1])<<8
	}
	return string(utf16.Decode(units))
}

func embeddedScript(t *testing.T, name string) string {
	t.Helper()
	b, err := shellIntegrationFS.ReadFile("shell_integration/" + name)
	if err != nil {
		t.Fatalf("скрипт %s не вшит: %v", name, err)
	}
	return string(b)
}

func isPlain(argv, extra []string, shell string) bool {
	return len(argv) == 1 && argv[0] == shell && extra == nil
}

// Выключено — прежний запуск байт в байт: argv = [shell], окружение не
// трогается, диск тоже (prepareShellLaunch уходит раньше записи скриптов).
func TestShellLaunchOffIsPlain(t *testing.T) {
	env := []string{"HOME=/home/u", "PATH=/usr/bin"}
	shells := []string{"/bin/bash", "/usr/bin/zsh", siPowerShell, "pwsh", `C:\Windows\System32\cmd.exe`}
	for _, goos := range []string{"linux", "darwin", "windows"} {
		for _, sh := range shells {
			if argv, extra := shellLaunchFor(goos, sh, false, siRoot, env); !isPlain(argv, extra, sh) {
				t.Fatalf("%s/%s при выключенной разметке: argv=%q env=%q, ждали [shell]", goos, sh, argv, extra)
			}
		}
	}
	if argv, extra := prepareShellLaunch("/bin/bash", false, env); !isPlain(argv, extra, "/bin/bash") {
		t.Fatalf("prepareShellLaunch(выключено) = %q %q", argv, extra)
	}
}

// cmd, fish, sh, ssh и всё незнакомое — как раньше. fish ≥ 4 размечает сам,
// cmd разметить нечем, ssh — чужая машина со своим шеллом.
func TestShellLaunchUnsupportedShellsUnchanged(t *testing.T) {
	env := []string{"HOME=/home/u"}
	shells := []string{"cmd.exe", `C:\Windows\System32\cmd.exe`, "/usr/bin/fish", "fish", "/bin/sh",
		"/bin/dash", "ssh", "/usr/bin/ssh", "nu", "/usr/bin/bashful", "zsh-5.9-custom", ""}
	for _, goos := range []string{"linux", "darwin", "windows"} {
		for _, sh := range shells {
			if argv, extra := shellLaunchFor(goos, sh, true, siRoot, env); !isPlain(argv, extra, sh) {
				t.Fatalf("%s/%q тронут: argv=%q env=%q", goos, sh, argv, extra)
			}
		}
	}
}

// Отказ в окружении сильнее настройки; разметка VS Code — тоже повод не
// вмешиваться. На Windows имена переменных без учёта регистра.
func TestShellLaunchRespectsOptOut(t *testing.T) {
	cases := []struct {
		goos string
		env  []string
	}{
		{"linux", []string{"HOME=/home/u", "VSCODE_SHELL_INTEGRATION=1"}},
		{"linux", []string{"HOME=/home/u", "REMOTAI_SHELL_INTEGRATION=0"}},
		{"darwin", []string{"HOME=/Users/u", "REMOTAI_SHELL_INTEGRATION=0"}},
		{"windows", []string{"USERPROFILE=C:\\Users\\u", "remotai_shell_integration=0"}},
		{"windows", []string{"Vscode_Shell_Integration=1"}},
	}
	for _, c := range cases {
		shells := []string{"/bin/bash", "/bin/zsh", "/usr/bin/pwsh"}
		if c.goos == "windows" {
			shells = []string{siPowerShell, "pwsh.exe"}
		}
		for _, sh := range shells {
			if argv, extra := shellLaunchFor(c.goos, sh, true, siRoot, c.env); !isPlain(argv, extra, sh) {
				t.Fatalf("%s %s env=%q: отказ проигнорирован, argv=%q extra=%q", c.goos, sh, c.env, argv, extra)
			}
		}
	}
	// Положительный контроль: "1" (флаг хоста) — не отказ.
	argv, _ := shellLaunchFor("linux", "/bin/bash", true, siRoot, []string{"REMOTAI_SHELL_INTEGRATION=1"})
	if len(argv) != 3 {
		t.Fatalf("REMOTAI_SHELL_INTEGRATION=1 принят за отказ: %q", argv)
	}
}

func TestShellLaunchBash(t *testing.T) {
	for _, goos := range []string{"linux", "darwin"} {
		argv, extra := shellLaunchFor(goos, "/bin/bash", true, siRoot, []string{"HOME=/home/u"})
		want := []string{"/bin/bash", "--rcfile", siRoot + "/bash.sh"}
		if strings.Join(argv, "|") != strings.Join(want, "|") || extra != nil {
			t.Fatalf("%s: argv=%q extra=%q, ждали %q", goos, argv, extra, want)
		}
	}
	// Git Bash и bash.exe из WSL на Windows не трогаем.
	sh := `C:\Program Files\Git\bin\bash.exe`
	if argv, extra := shellLaunchFor("windows", sh, true, siRoot, nil); !isPlain(argv, extra, sh) {
		t.Fatalf("bash на Windows тронут: %q %q", argv, extra)
	}
	// Некуда класть скрипты — запуск как раньше.
	if argv, extra := shellLaunchFor("linux", "/bin/bash", true, "", nil); !isPlain(argv, extra, "/bin/bash") {
		t.Fatalf("без каталога скриптов: %q %q", argv, extra)
	}
}

func TestShellLaunchZsh(t *testing.T) {
	ours := siRoot + "/zsh"
	argv, extra := shellLaunchFor("linux", "/usr/bin/zsh", true, siRoot, []string{"HOME=/home/u"})
	if !isPlain(argv, nil, "/usr/bin/zsh") {
		t.Fatalf("argv zsh должен остаться [shell]: %q", argv)
	}
	if strings.Join(extra, "|") != "ZDOTDIR="+ours+"|REMOTAI_USER_ZDOTDIR=/home/u" {
		t.Fatalf("env zsh без ZDOTDIR: %q", extra)
	}
	// Прежний ZDOTDIR запоминается, а не подменяется HOME.
	_, extra = shellLaunchFor("darwin", "/bin/zsh", true, siRoot,
		[]string{"HOME=/Users/u", "ZDOTDIR=/Users/u/.config/zsh"})
	if strings.Join(extra, "|") != "ZDOTDIR="+ours+"|REMOTAI_USER_ZDOTDIR=/Users/u/.config/zsh" {
		t.Fatalf("прежний ZDOTDIR потерян: %q", extra)
	}
	// Итоговое окружение шелла — ровно один ZDOTDIR, наш.
	final := mergeEnv([]string{"HOME=/Users/u", "ZDOTDIR=/Users/u/.config/zsh"}, extra, false)
	if n := strings.Count(strings.Join(final, "\n"), "ZDOTDIR="+ours); n != 1 || strings.Contains(strings.Join(final, "\n"), "\nZDOTDIR=/Users") {
		t.Fatalf("итоговое окружение: %q", final)
	}
	// ZDOTDIR уже наш (вложенный запуск посреди старта) и нет HOME — не трогаем.
	for _, env := range [][]string{{"HOME=/home/u", "ZDOTDIR=" + ours + "/"}, {"PATH=/bin"}} {
		if argv, extra := shellLaunchFor("linux", "/bin/zsh", true, siRoot, env); !isPlain(argv, extra, "/bin/zsh") {
			t.Fatalf("env=%q: %q %q", env, argv, extra)
		}
	}
	if argv, extra := shellLaunchFor("windows", `C:\msys64\usr\bin\zsh.exe`, true, siRoot, []string{"HOME=C:\\u"}); !isPlain(argv, extra, `C:\msys64\usr\bin\zsh.exe`) {
		t.Fatalf("zsh на Windows тронут: %q %q", argv, extra)
	}
}

// PowerShell: -NoExit -EncodedCommand, профиль НЕ отключается, политика
// выполнения НЕ трогается, код оборачивает prompt и шлёт A/B/D, но не C.
func TestShellLaunchPowerShell(t *testing.T) {
	cases := []struct{ goos, shell string }{
		{"windows", siPowerShell},
		{"windows", `C:\Program Files\PowerShell\7\pwsh.exe`},
		{"windows", "PowerShell.EXE"},
		{"linux", "/usr/bin/pwsh"},
		{"darwin", "/usr/local/bin/pwsh"},
	}
	for _, c := range cases {
		argv, extra := shellLaunchFor(c.goos, c.shell, true, siRoot, []string{"HOME=/h"})
		if len(argv) != 4 || argv[0] != c.shell || argv[1] != "-NoExit" || argv[2] != "-EncodedCommand" || extra != nil {
			t.Fatalf("%s %s: argv=%q extra=%q", c.goos, c.shell, argv, extra)
		}
		for _, a := range argv {
			if strings.EqualFold(a, "-NoProfile") || strings.EqualFold(a, "-ExecutionPolicy") {
				t.Fatalf("аргумент %s ломает профиль или политику пользователя: %q", a, argv)
			}
		}
		// Командная строка Windows ограничена 32767 символами — держим запас.
		if n := len(strings.Join(argv, " ")); n > 16000 {
			t.Fatalf("командная строка %d символов — слишком близко к пределу Windows", n)
		}
	}
	code := decodePwsh(t, pwshEncodedCommand())
	for _, want := range []string{"133;A", "133;B", "133;D;", "$function:prompt", ".Invoke()", "Get-History", "Write-Error", "$global:LASTEXITCODE"} {
		if !strings.Contains(code, want) {
			t.Fatalf("в коде PowerShell нет %q", want)
		}
	}
	if strings.Contains(code, "133;C") {
		t.Fatal("PowerShell не шлёт C: начало вывода клиент выводит сам (карта ST-10)")
	}
	for _, line := range strings.Split(code, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			t.Fatalf("комментарий не вырезан перед кодированием: %q", line)
		}
	}
}

// Пользовательская конфигурация подключается как обычно и ДО разметки.
func TestShellIntegrationScriptsKeepUserConfig(t *testing.T) {
	bash := embeddedScript(t, "bash.sh")
	rc := regexp.MustCompile(`(?m)^\s*\. ~/\.bashrc\s*$`).FindStringIndex(bash)
	pc := strings.Index(bash, "PROMPT_COMMAND=")
	if rc == nil || pc < 0 || rc[0] > pc {
		t.Fatal("bash.sh обязан подключить ~/.bashrc до установки хуков")
	}
	// /etc/bash.bashrc bash с SYS_BASHRC читает сам и при --rcfile (живая
	// проверка: Ubuntu 24.04, bash 5.2.21). Второе подключение прогнало бы
	// системный rc дважды — поэтому явного source здесь быть не должно.
	if regexp.MustCompile(`(?m)^\s*(\.|source)\s+/etc/bash\.bashrc`).MatchString(bash) {
		t.Fatal("bash.sh подключает /etc/bash.bashrc повторно")
	}
	for _, want := range []string{"PS0=", "${__remotai_si_num@P}", `'\e]133;D;%s\a'`, `\[\e]133;A\a\]`, `\[\e]133;B\a\]`} {
		if !strings.Contains(bash, want) {
			t.Fatalf("в bash.sh нет %q", want)
		}
	}
	if regexp.MustCompile(`(?m)^\s*trap\b.*DEBUG`).MatchString(bash) {
		t.Fatal("DEBUG-trap ломает чужие trap-и — разметка обходится без него")
	}
	// Обёртки не уезжают в окружение детей (ревью B2): иначе вложенный bash
	// пишет «__remotai_si_precmd: command not found». Живьём —
	// TestShellIntegrationLiveBashNoExportLeak; здесь сторож для CI без шеллов.
	for _, want := range []string{"builtin export -n PROMPT_COMMAND PS1 PS0", "builtin export -fn __remotai_si_precmd __remotai_si_postcmd"} {
		if !strings.Contains(bash, want) {
			t.Fatalf("bash.sh не снимает экспорт: нет %q", want)
		}
	}
	if strings.Count(string(normalizeLF([]byte(bash))), "\t__remotai_si_unexport\n") < 2 {
		t.Fatal("bash.sh обязан снимать экспорт и при установке, и на каждом приглашении")
	}
	// PowerShell: D и A уходят на экран до вызова пользовательского prompt,
	// иначе текст Write-Host из prompt ложится перед D и мимо A…B (ревью B2).
	// Живьём — TestShellIntegrationLivePowerShellUserPrompt/*/write-host.
	ps := decodePwsh(t, pwshEncodedCommand())
	if w, inv := strings.Index(ps, "$Host.UI.Write($remotaiOut)"), strings.Index(ps, ".Invoke()"); w < 0 || inv < 0 || w > inv {
		t.Fatal("pwsh.ps1: D и A обязаны уйти на экран до вызова пользовательского prompt")
	}

	for _, f := range []struct{ src, user string }{
		{"zsh/zshenv", ".zshenv"}, {"zsh/zprofile", ".zprofile"}, {"zsh/zshrc", ".zshrc"}, {"zsh/zlogin", ".zlogin"},
	} {
		s := embeddedScript(t, f.src)
		swap := strings.Index(s, "ZDOTDIR=$REMOTAI_USER_ZDOTDIR")
		src := strings.Index(s, `builtin source "$ZDOTDIR/`+f.user+`"`)
		back := strings.Index(s, "ZDOTDIR=$__remotai_si_dir")
		if swap < 0 || src < swap || back < src {
			t.Fatalf("%s: пользовательский %s подключается не с его ZDOTDIR (swap=%d source=%d back=%d)", f.src, f.user, swap, src, back)
		}
	}
	zshrc := embeddedScript(t, "zsh/zshrc")
	for _, want := range []string{"add-zsh-hook preexec", "add-zsh-hook precmd", "precmd_functions=(__remotai_si_precmd",
		`HISTFILE=$REMOTAI_USER_ZDOTDIR/.zsh_history`, "__remotai_si_restore"} {
		if !strings.Contains(zshrc, want) {
			t.Fatalf("в zshrc нет %q", want)
		}
	}
	zshenv := embeddedScript(t, "zsh/zshenv")
	if !strings.Contains(zshenv, "unset ZDOTDIR") || !strings.Contains(zshenv, "unset REMOTAI_USER_ZDOTDIR") {
		t.Fatal("zshenv не возвращает ZDOTDIR в исходное состояние")
	}
	if !strings.Contains(embeddedScript(t, "zsh/zlogin"), "__remotai_si_restore") {
		t.Fatal("zlogin не возвращает ZDOTDIR после login-старта")
	}
}

// Все маркеры завершаются BEL: так их вырезает ansiRe и так их понимает любой
// терминал. ST (ESC \) в наших скриптах не встречается.
func TestShellIntegrationMarkersUseBEL(t *testing.T) {
	for _, name := range []string{"bash.sh", "zsh/zshrc", "pwsh.ps1"} {
		marks := 0
		for _, line := range strings.Split(embeddedScript(t, name), "\n") {
			if !strings.Contains(line, "]133;") {
				continue
			}
			marks++
			if !strings.Contains(line, `\a`) && !strings.Contains(line, "$remotaiBel") {
				t.Fatalf("%s: маркер без BEL: %q", name, line)
			}
			if strings.Contains(line, `\e\\`) || strings.Contains(line, `\x1b\\`) || strings.Contains(line, "`e\\") {
				t.Fatalf("%s: маркер с ST: %q", name, line)
			}
		}
		if marks == 0 {
			t.Fatalf("%s: маркеров не найдено", name)
		}
	}
}

func TestWriteShellIntegrationScripts(t *testing.T) {
	root := filepath.Join(t.TempDir(), "shell-integration")
	if err := writeShellIntegrationScripts(root); err != nil {
		t.Fatal(err)
	}
	for _, f := range shellIntegrationFiles {
		dst := filepath.Join(root, filepath.FromSlash(f.dst))
		got, err := os.ReadFile(dst)
		if err != nil {
			t.Fatalf("%s не записан: %v", f.dst, err)
		}
		want, _ := shellIntegrationFS.ReadFile(f.src)
		if string(got) != string(normalizeLF(want)) || strings.Contains(string(got), "\r") {
			t.Fatalf("%s: содержимое расходится с вшитым или с CRLF", f.dst)
		}
		if runtime.GOOS != "windows" {
			if st, _ := os.Stat(dst); st.Mode().Perm() != 0o600 {
				t.Fatalf("%s: права %v, ждали 0600", f.dst, st.Mode().Perm())
			}
		}
	}
	if runtime.GOOS != "windows" {
		if st, _ := os.Stat(root); st.Mode().Perm() != 0o700 {
			t.Fatalf("каталог: права %v, ждали 0700", st.Mode().Perm())
		}
	}
	// Идемпотентно: совпадающий файл не переписывается.
	bash := filepath.Join(root, "bash.sh")
	old := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := os.Chtimes(bash, old, old); err != nil {
		t.Fatal(err)
	}
	// Испорченный файл восстанавливается.
	zshrc := filepath.Join(root, "zsh", ".zshrc")
	if err := os.WriteFile(zshrc, []byte("чужое"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeShellIntegrationScripts(root); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(bash); !st.ModTime().Equal(old) {
		t.Fatalf("совпадающий bash.sh переписан (mtime %v)", st.ModTime())
	}
	if got, _ := os.ReadFile(zshrc); !strings.Contains(string(got), "add-zsh-hook") {
		t.Fatal("испорченный .zshrc не восстановлен")
	}
	entries, _ := filepath.Glob(filepath.Join(root, "*", ".remotai-si-*"))
	more, _ := filepath.Glob(filepath.Join(root, ".remotai-si-*"))
	if len(entries)+len(more) != 0 {
		t.Fatalf("остались временные файлы: %v %v", entries, more)
	}
}

func TestNormalizeLF(t *testing.T) {
	if got := string(normalizeLF([]byte("a\r\nb\nc\r\n"))); got != "a\nb\nc\n" {
		t.Fatalf("normalizeLF = %q", got)
	}
}

// Окружение pty-host: nil — унаследовать как есть (прежний запуск).
func TestHostSpawnEnv(t *testing.T) {
	base := []string{"PATH=/bin", "HOME=/h"}
	if env := hostSpawnEnv(false, base, false); env != nil {
		t.Fatalf("выключено без переменной — окружение тронуто: %q", env)
	}
	env := hostSpawnEnv(true, base, false)
	if strings.Count(strings.Join(env, "\n"), EnvShellIntegration+"=1") != 1 || len(env) != 3 {
		t.Fatalf("включено: %q", env)
	}
	if env := hostSpawnEnv(true, append(base, EnvShellIntegration+"=0"), false); env != nil {
		t.Fatalf("отказ пользователя перебит настройкой: %q", env)
	}
	env = hostSpawnEnv(false, append(base, EnvShellIntegration+"=1"), false)
	if strings.Contains(strings.Join(env, "\n"), EnvShellIntegration) || len(env) != 2 {
		t.Fatalf("выключено: унаследованная «1» дошла до хоста: %q", env)
	}
	env = hostSpawnEnv(false, []string{"Path=C:\\", "remotai_shell_integration=1"}, true)
	if len(env) != 1 || env[0] != "Path=C:\\" {
		t.Fatalf("Windows, регистр: %q", env)
	}
	if env := hostSpawnEnv(true, []string{"Remotai_Shell_Integration=0"}, true); env != nil {
		t.Fatalf("Windows: отказ другим регистром перебит: %q", env)
	}
}

func TestMergeEnvAndLookup(t *testing.T) {
	base := []string{"A=1", "ZDOTDIR=/old", "B=2"}
	same := mergeEnv(base, nil, false)
	if len(same) != len(base) || &same[0] != &base[0] {
		t.Fatal("пустой extra обязан вернуть base как есть")
	}
	if got := strings.Join(mergeEnv(base, []string{"ZDOTDIR=/new"}, false), " "); got != "A=1 B=2 ZDOTDIR=/new" {
		t.Fatalf("mergeEnv = %q", got)
	}
	if got := strings.Join(mergeEnv([]string{"Path=x", "zdotdir=/old"}, []string{"ZDOTDIR=/n"}, true), " "); got != "Path=x ZDOTDIR=/n" {
		t.Fatalf("mergeEnv без регистра = %q", got)
	}
	if got := strings.Join(mergeEnv([]string{"zdotdir=/old"}, []string{"ZDOTDIR=/n"}, false), " "); got != "zdotdir=/old ZDOTDIR=/n" {
		t.Fatalf("POSIX различает регистр: %q", got)
	}
	if v, ok := lookupEnv([]string{"X=1", "=C:=C:\\", "X=2"}, "X", false); !ok || v != "2" {
		t.Fatalf("lookupEnv: побеждает последнее вхождение, got %q", v)
	}
	if _, ok := lookupEnv([]string{"=C:=C:\\"}, "", true); ok {
		t.Fatal("служебные «=C:» Windows — не переменные")
	}
}

func TestShellIntegrationKind(t *testing.T) {
	cases := map[string]string{
		"/bin/bash": "bash", "bash": "bash", `C:\Program Files\Git\bin\BASH.EXE`: "bash",
		"/usr/bin/zsh": "zsh", "/bin/zsh": "zsh",
		siPowerShell: "pwsh", "pwsh": "pwsh", "/usr/bin/pwsh": "pwsh", "PWSH.exe": "pwsh",
		"cmd.exe": "", "fish": "", "/bin/sh": "", "ssh": "", "": "", "/usr/bin/bash5": "",
	}
	for sh, want := range cases {
		if got := shellIntegrationKind(sh); got != want {
			t.Fatalf("shellIntegrationKind(%q) = %q, ждали %q", sh, got, want)
		}
	}
}

// ── Проводка: переключатель → запуск шелла (ревью B2) ─────────────────────
//
// Правило запуска проверено выше, но между Manager.SetShellIntegration и argv
// шелла стоят createWithID (оба пути), newPersistentPTYWith, spawnHost
// (breakaway на Windows, systemd-run/setsid на Linux) и RunHost. Подмена флага
// на false в любом звене проходила зелёной. Здесь цепочка гоняется целиком и
// по-настоящему, но без настоящего шелла: тестовый бинарь сам играет pty-host
// (настоящий RunHost) и «шелл» с именем pwsh — для него правило даёт
// -NoExit -EncodedCommand на любой ОС и ничего не пишет на диск. Звено
// «настройка → Manager» — в internal/web/api_settings_shell_integration_test.go.

// siHelperEnv — путь файла отчёта. Задан — бинарь работает помощником, а не
// гоняет тесты (см. TestMain).
const siHelperEnv = "REMOTAI_SI_TEST_HELPER"

func TestMain(m *testing.M) {
	if out := os.Getenv(siHelperEnv); out != "" {
		os.Exit(siHelperMain(out))
	}
	os.Exit(m.Run())
}

// siShellReport — с чем запустили «шелл»: аргументы и флаг хоста в окружении
// (хост обязан снять его до запуска шелла).
type siShellReport struct {
	Args  []string `json:"args"`
	Integ string   `json:"integ"`
}

func siHelperMain(out string) int {
	args := os.Args[1:]
	if len(args) > 0 && args[0] == "--pty-host" {
		// Роль pty-host: что пришло в окружении — в отчёт, дальше настоящий
		// RunHost с аргументами, которые собрал spawnHost.
		_ = os.WriteFile(out+".host", []byte(os.Getenv(EnvShellIntegration)), 0o600)
		var id, shell, cwd string
		cols, rows := 80, 24
		for i := 1; i+1 < len(args); i += 2 {
			switch v := args[i+1]; args[i] {
			case "--id":
				id = v
			case "--shell":
				shell = v
			case "--cwd":
				cwd = v
			case "--cols":
				cols, _ = strconv.Atoi(v)
			case "--rows":
				rows, _ = strconv.Atoi(v)
			}
		}
		return RunHost(id, shell, cwd, cols, rows)
	}
	b, err := json.Marshal(siShellReport{Args: args, Integ: os.Getenv(EnvShellIntegration)})
	if err != nil {
		return 1
	}
	// Через rename: тест читает файл, пока мы его пишем.
	if err := os.WriteFile(out+".tmp", b, 0o600); err != nil {
		return 1
	}
	if err := os.Rename(out+".tmp", out); err != nil {
		return 1
	}
	return 0
}

// siFakePwsh — копия тестового бинаря под именем pwsh: так её распознаёт
// shellIntegrationKind.
func siFakePwsh(t *testing.T) string {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	name := "pwsh"
	if runtime.GOOS == "windows" {
		name = "pwsh.exe"
	}
	dst := filepath.Join(t.TempDir(), name)
	in, err := os.Open(self)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
	return dst
}

// siLogBuf — копия лога пакета: по ней видно, что хост ушёл в запуск через WMI.
type siLogBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (w *siLogBuf) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Write(p)
}

func (w *siLogBuf) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.String()
}

// siShort — аргументы для сообщения: base64 разметки длиной в килобайты
// читать незачем.
func siShort(args []string) []string {
	out := make([]string, len(args))
	for i, a := range args {
		if len(a) > 40 {
			a = a[:20] + "…(" + strconv.Itoa(len(a)) + ")"
		}
		out[i] = a
	}
	return out
}

// siWiringRun открывает один терминал через Manager и возвращает отчёт
// «шелла», значение флага в окружении хоста и лог хоста (для постоянного пути).
func siWiringRun(t *testing.T, shell string, persistent, on bool) (rep siShellReport, hostEnv, hostLog string) {
	t.Helper()
	dir := t.TempDir()
	out := filepath.Join(dir, "shell.json")
	t.Setenv(siHelperEnv, out)
	// Отказ «0» или «1» в окружении самого прогона исказил бы проверку.
	t.Setenv(EnvShellIntegration, "")
	os.Unsetenv(EnvShellIntegration)
	logs := &siLogBuf{}
	prev := log.Writer()
	log.SetOutput(io.MultiWriter(prev, logs))
	defer log.SetOutput(prev)

	m := &Manager{
		sessions:   make(map[string]*Session),
		meta:       NewMetaStoreAt(filepath.Join(dir, "pty.json")),
		persistent: persistent,
	}
	m.SetShellIntegration(on)
	sess, err := m.Create(0, dir, shell, 80, 24)
	if err != nil {
		if persistent && runtime.GOOS == "windows" && strings.Contains(logs.String(), "WMI") {
			// Задание процесса запрещает breakaway: хост рождает служба WMI со
			// своим окружением, и флаг туда не доходит по построению
			// (persist_windows.go) — проверять здесь нечего.
			t.Skipf("pty-host запущен через WMI: %v", err)
		}
		t.Fatalf("Create (persistent=%v, разметка=%v): %v", persistent, on, err)
	}
	defer func() {
		_ = m.Close(sess.ID)
		if persistent {
			_ = os.Remove(HostLogPath(sess.ID))
		}
	}()
	deadline := time.Now().Add(30 * time.Second)
	for {
		if b, err := os.ReadFile(out); err == nil && json.Unmarshal(b, &rep) == nil {
			break
		}
		if time.Now().After(deadline) {
			hl, _ := os.ReadFile(HostLogPath(sess.ID))
			t.Fatalf("«шелл» не отчитался (persistent=%v, разметка=%v); лог хоста:\n%s", persistent, on, hl)
		}
		time.Sleep(50 * time.Millisecond)
	}
	// Конца сессии не ждём: ConPTY не закрывает вывод, когда шелл вышел, и
	// Done() приходит только после Close (замер: ожидание всегда съедало весь
	// таймаут). «Шелл» к этому моменту уже вышел сам, копию бинаря не держит.
	if persistent {
		b, _ := os.ReadFile(out + ".host")
		hostEnv = string(b)
		l, _ := os.ReadFile(HostLogPath(sess.ID))
		hostLog = string(l)
	}
	return rep, hostEnv, hostLog
}

func siWantIntegrated(t *testing.T, rep siShellReport) {
	t.Helper()
	want := []string{"-NoExit", "-EncodedCommand", pwshEncodedCommand()}
	if strings.Join(rep.Args, "\n") != strings.Join(want, "\n") {
		t.Fatalf("включено: шелл запущен с %q, ждали -NoExit -EncodedCommand <разметка>", siShort(rep.Args))
	}
}

// Непостоянный путь: Manager.SetShellIntegration → createWithID →
// localShellLaunch → newPlatformPTY.
func TestShellIntegrationWiringLocalPath(t *testing.T) {
	shell := siFakePwsh(t)
	rep, _, _ := siWiringRun(t, shell, false, true)
	siWantIntegrated(t, rep)
	rep, _, _ = siWiringRun(t, shell, false, false)
	if len(rep.Args) != 0 {
		t.Fatalf("выключено: шелл запущен с %q, ждали прежний голый запуск", siShort(rep.Args))
	}
}

// Постоянный путь: createWithID → newPersistentPTYWith → spawnHost (окружение
// хоста) → RunHost → prepareShellLaunch → newPlatformPTY. Хост — настоящий
// отвязанный процесс, запущенный теми же флагами, что и в бою.
func TestShellIntegrationWiringPersistentHost(t *testing.T) {
	if testing.Short() {
		t.Skip("поднимает отвязанный pty-host")
	}
	shell := siFakePwsh(t)
	rep, hostEnv, hostLog := siWiringRun(t, shell, true, true)
	if hostEnv != "1" {
		t.Fatalf("включено: хост получил %s=%q, ждали «1»", EnvShellIntegration, hostEnv)
	}
	siWantIntegrated(t, rep)
	if rep.Integ != "" {
		t.Fatalf("шелл унаследовал флаг хоста %s=%q — хост обязан его снять", EnvShellIntegration, rep.Integ)
	}
	if !strings.Contains(hostLog, "разметка команд включена") {
		t.Fatalf("хост не записал, что разметка включена; лог:\n%s", hostLog)
	}

	rep, hostEnv, hostLog = siWiringRun(t, shell, true, false)
	if hostEnv != "" {
		t.Fatalf("выключено: хост получил %s=%q", EnvShellIntegration, hostEnv)
	}
	if len(rep.Args) != 0 {
		t.Fatalf("выключено: шелл запущен с %q, ждали прежний голый запуск", siShort(rep.Args))
	}
	if strings.Contains(hostLog, "разметка команд включена") {
		t.Fatalf("выключено, а хост пишет, что разметка включена; лог:\n%s", hostLog)
	}
}
