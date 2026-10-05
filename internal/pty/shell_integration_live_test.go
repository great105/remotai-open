package pty

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// ЖИВЫЕ ПРОВЕРКИ РАЗМЕТКИ КОМАНД (ST-10, T-39f, T-39h). Настоящий PTY,
// настоящий шелл, настоящий поток: доходят ли маркеры OSC 133 до нашего
// кольца в нужном порядке и не ломается ли пользовательская конфигурация.
// CI от шеллов не зависит — прогон только по явному запросу:
//
//	PTY_LIVE=1 go test ./internal/pty -run 'TestShellIntegrationLive' -v -count=1
//
// Linux-часть гоняется в WSL кросс-собранным бинарём (GOOS=linux go test -c).
// Путь через pty-host (флаг в окружении хоста) — дополнительно с
// REMOTAI_EXE=<собранный remotai>.

func siLiveGate(t *testing.T) {
	t.Helper()
	if os.Getenv("PTY_LIVE") == "" {
		t.Skip("нет PTY_LIVE — прогон поднимает настоящий шелл")
	}
}

// siCapture копит поток PTY; читатель один, проверки — по снимку.
type siCapture struct {
	mu  sync.Mutex
	buf []byte
}

func (c *siCapture) drain(r interface{ Read([]byte) (int, error) }) {
	buf := make([]byte, 8192)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			c.mu.Lock()
			c.buf = append(c.buf, buf[:n]...)
			c.mu.Unlock()
		}
		if err != nil {
			return
		}
	}
}

func (c *siCapture) snapshot() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]byte(nil), c.buf...)
}

func (c *siCapture) waitFor(t *testing.T, what string, timeout time.Duration, ok func([]byte) bool) []byte {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		b := c.snapshot()
		if ok(b) {
			return b
		}
		if time.Now().After(deadline) {
			tail := b
			if len(tail) > 3000 {
				tail = tail[len(tail)-3000:]
			}
			t.Fatalf("не дождались: %s\nхвост потока (%d байт всего): %q", what, len(b), tail)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

var osc133Re = regexp.MustCompile(`\x1b\]133;([ABCD])((?:;[^\x07\x1b]*)?)(\x07|\x1b\\)`)

type osc133Mark struct {
	kind string // A, B, C, D
	arg  string // ";0" у D
	term string // "\a" или "ESC\"
	off  int
	end  int
}

func osc133Marks(b []byte) []osc133Mark {
	var out []osc133Mark
	for _, m := range osc133Re.FindAllSubmatchIndex(b, -1) {
		out = append(out, osc133Mark{
			kind: string(b[m[2]:m[3]]), arg: string(b[m[4]:m[5]]), term: string(b[m[6]:m[7]]),
			off: m[0], end: m[1],
		})
	}
	return out
}

func countMarks(marks []osc133Mark, kind string) int {
	n := 0
	for _, m := range marks {
		if m.kind == kind {
			n++
		}
	}
	return n
}

// seqAfter — после позиции from встречаются маркеры kinds по порядку (другие
// маркеры между ними допустимы). Возвращает конец последнего или -1.
func seqAfter(marks []osc133Mark, from int, kinds ...string) int {
	i := 0
	for _, m := range marks {
		if m.off < from || i >= len(kinds) {
			continue
		}
		if m.kind+m.arg == kinds[i] || (m.kind == kinds[i] && !strings.Contains(kinds[i], ";")) {
			i++
			if i == len(kinds) {
				return m.end
			}
		}
	}
	return -1
}

// ── Windows: ConPTY + Windows PowerShell 5.1 и pwsh 7 ─────────────────────

func siWindowsShells() []string {
	var out []string
	for _, p := range []string{
		`C:\Windows\System32\WindowsPowerShell\v1.0\powershell.exe`,
		`C:\Program Files\PowerShell\7\pwsh.exe`,
	} {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			out = append(out, p)
		}
	}
	return out
}

// Ключевой непроверенный риск карты ST-10: пропускает ли ConPTY OSC 133 до
// нашего кольца. Тест отвечает на него фактом и печатает, каким терминатором
// маркеры доезжают (ConPTY может перекодировать BEL в ST).
func TestShellIntegrationLivePowerShell(t *testing.T) {
	siLiveGate(t)
	if runtime.GOOS != "windows" {
		t.Skip("PowerShell в ConPTY — только Windows")
	}
	for _, shell := range siWindowsShells() {
		shell := shell
		t.Run(filepath.Base(shell), func(t *testing.T) {
			argv, extra := shellLaunch(shell, true, t.TempDir(), os.Environ())
			if len(argv) != 4 {
				t.Fatalf("argv=%q", argv)
			}
			p, err := newPlatformPTY(120, 30, t.TempDir(), argv, extra)
			if err != nil {
				t.Fatalf("ConPTY: %v", err)
			}
			defer p.Close()
			c := &siCapture{}
			go c.drain(p)

			b := c.waitFor(t, "первое приглашение A…B (ConPTY пропускает OSC 133?)", 60*time.Second, func(b []byte) bool {
				return seqAfter(osc133Marks(b), 0, "A", "B") >= 0
			})
			marks := osc133Marks(b)
			t.Logf("ФАКТ: ConPTY пропускает OSC 133 — да; терминатор первого маркера %q; начало потока: %q",
				marks[0].term, b[:min(len(b), 400)])

			// Обычная команда: вывод, затем D;0, затем новое приглашение A…B.
			if _, err := p.Write([]byte("Write-Output ('remotai-si-' + (40+2))\r")); err != nil {
				t.Fatal(err)
			}
			b = c.waitFor(t, "вывод команды и D;0 → A → B", 30*time.Second, func(b []byte) bool {
				i := bytes.Index(b, []byte("remotai-si-42"))
				return i >= 0 && seqAfter(osc133Marks(b), i, "D;0", "A", "B") >= 0
			})
			if n := countMarks(osc133Marks(b), "C"); n != 0 {
				t.Fatalf("PowerShell не должен слать C, пришло %d", n)
			}

			// Код нативной команды доходит в D.
			mark := len(b)
			if _, err := p.Write([]byte("cmd /c exit 3\r")); err != nil {
				t.Fatal(err)
			}
			b = c.waitFor(t, "D;3 → A → B после cmd /c exit 3", 30*time.Second, func(b []byte) bool {
				return seqAfter(osc133Marks(b), mark, "D;3", "A", "B") >= 0
			})

			// Пустой Enter: новое приглашение без D (команды не было).
			aBefore := countMarks(osc133Marks(b), "A")
			dBefore := countMarks(osc133Marks(b), "D")
			if _, err := p.Write([]byte("\r")); err != nil {
				t.Fatal(err)
			}
			b = c.waitFor(t, "приглашение после пустого Enter", 20*time.Second, func(b []byte) bool {
				return countMarks(osc133Marks(b), "A") > aBefore
			})
			time.Sleep(500 * time.Millisecond)
			if got := countMarks(osc133Marks(c.snapshot()), "D"); got != dBefore {
				t.Fatalf("пустой Enter дал D: было %d, стало %d", dBefore, got)
			}
		})
	}
}

// Пользовательский prompt (как из профиля: oh-my-posh, starship) сохраняется и
// видит СВОЙ $? — наш код возвращает его перед вызовом. Профиль на этой машине
// не создаём (он лежит в «Документах» владельца), поэтому «профиль» здесь —
// функция prompt, определённая до нашего кода, ровно как это делает профиль.
//
// Второй вид — prompt, который печатает через Write-Host (старый posh-git,
// самодельные профили): его текст выходит ДО возвращённой строки и раньше
// оказывался перед D прошлой команды и мимо A…B (ревью B2). Теперь D и A
// пишутся на экран сразу, до вызова пользовательского prompt.
func TestShellIntegrationLivePowerShellUserPrompt(t *testing.T) {
	siLiveGate(t)
	if runtime.GOOS != "windows" {
		t.Skip("только Windows")
	}
	src, err := shellIntegrationFS.ReadFile("shell_integration/pwsh.ps1")
	if err != nil {
		t.Fatal(err)
	}
	prompts := []struct{ name, def, text string }{
		{"return", `function global:prompt { "USER[$?]> " }`, "USER[%s]> "},
		{"write-host", `function global:prompt { Write-Host "WH[$?]" -NoNewline; '> ' }`, "WH[%s]"},
	}
	for _, shell := range siWindowsShells() {
		for _, up := range prompts {
			shell, up := shell, up
			t.Run(filepath.Base(shell)+"/"+up.name, func(t *testing.T) {
				siLivePowerShellPrompt(t, shell, up.def+"\n"+string(src), up.text)
			})
		}
	}
}

// siLivePowerShellPrompt: code — пользовательский prompt и наш код; text —
// шаблон видимого приглашения с местом под $? ("True"/"False").
func siLivePowerShellPrompt(t *testing.T, shell, code, text string) {
	t.Helper()
	argv := []string{shell, "-NoExit", "-EncodedCommand", encodePwsh(siInvokePromptKey + "\n" + code)}
	p, err := newPlatformPTY(120, 30, t.TempDir(), argv, nil)
	if err != nil {
		t.Fatalf("ConPTY: %v", err)
	}
	defer p.Close()
	c := &siCapture{}
	go c.drain(p)
	// between — текст приглашения лежит между A и ближайшим за ним B.
	between := func(b []byte, from int, want string) bool {
		marks := osc133Marks(b)
		for i, m := range marks {
			if m.off < from || m.kind != "A" {
				continue
			}
			for _, n := range marks[i+1:] {
				if n.kind == "B" {
					if bytes.Contains(b[m.end:n.off], []byte(want)) {
						return true
					}
					break
				}
			}
		}
		return false
	}
	ok, fail := fmt.Sprintf(text, "True"), fmt.Sprintf(text, "False")
	c.waitFor(t, ok+" между A и B", 60*time.Second, func(b []byte) bool { return between(b, 0, ok) })

	mark := len(c.snapshot())
	if _, err := p.Write([]byte("Get-Item C:\\remotai-si-no-such-path\r")); err != nil {
		t.Fatal(err)
	}
	c.waitFor(t, "после ошибки: D;1 → A "+fail+" B", 30*time.Second, func(b []byte) bool {
		return seqAfter(osc133Marks(b), mark, "D;1", "A", "B") >= 0 && between(b, mark, fail)
	})
	// Видимое приглашение не должно оказаться перед D: клиент счёл бы его
	// выводом прошлой команды.
	b := c.snapshot()
	if d := seqAfter(osc133Marks(b), mark, "D;1"); d >= 0 && bytes.Contains(b[mark:d], []byte(fail)) {
		t.Fatalf("текст приглашения %q пришёл раньше D: %q", fail, b[mark:d])
	}

	mark = len(c.snapshot())
	if _, err := p.Write([]byte("Write-Output ok\r")); err != nil {
		t.Fatal(err)
	}
	c.waitFor(t, "после успеха: D;0 → A "+ok+" B", 30*time.Second, func(b []byte) bool {
		return seqAfter(osc133Marks(b), mark, "D;0", "A", "B") >= 0 && between(b, mark, ok)
	})

	// PSReadLine перерисовывает приглашение сам — InvokePrompt (так делают
	// transient prompt oh-my-posh и другие): A и B заново, текст между ними, и
	// никакого D — команды не было. Клавиша — F12 последовательностью VT:
	// Ctrl+L ConPTY при русской раскладке отдаёт буквой «д» (живой прогон).
	time.Sleep(300 * time.Millisecond)
	mark = len(c.snapshot())
	dBefore := countMarks(osc133Marks(c.snapshot()), "D")
	if _, err := p.Write([]byte("\x1b[24~")); err != nil {
		t.Fatal(err)
	}
	c.waitFor(t, "InvokePrompt: A "+ok+" B заново", 30*time.Second, func(b []byte) bool {
		return seqAfter(osc133Marks(b), mark, "A", "B") >= 0 && between(b, mark, ok)
	})
	time.Sleep(300 * time.Millisecond)
	if got := countMarks(osc133Marks(c.snapshot()), "D"); got != dBefore {
		t.Fatalf("InvokePrompt дал D: было %d, стало %d", dBefore, got)
	}
}

// siInvokePromptKey — F12 перерисовывает приглашение через PSReadLine, как
// это делают профили с transient prompt.
const siInvokePromptKey = "Set-PSReadLineKeyHandler -Chord F12 -ScriptBlock { [Microsoft.PowerShell.PSConsoleReadLine]::InvokePrompt() }"

// ── Linux / macOS: bash через newPlatformPTY (T-39f) ───────────────────────

func siBash(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("bash с --rcfile — Linux и macOS")
	}
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash не найден")
	}
	return bash
}

func TestShellIntegrationLiveBash(t *testing.T) {
	siLiveGate(t)
	bash := siBash(t)
	home := t.TempDir()
	// Пользовательский ~/.bashrc: своя переменная, своё приглашение и свой
	// PROMPT_COMMAND — всё это обязано работать, как без нас.
	rc := "FOO=1\nPS1='user\\$ '\nPROMPT_COMMAND='__user_pc=$((__user_pc+1))'\n"
	if err := os.WriteFile(filepath.Join(home, ".bashrc"), []byte(rc), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	root := filepath.Join(t.TempDir(), "shell-integration")
	if err := writeShellIntegrationScripts(root); err != nil {
		t.Fatal(err)
	}
	argv, extra := shellLaunch(bash, true, root, os.Environ())
	t.Logf("argv=%q", argv)
	p, err := newPlatformPTY(100, 30, home, argv, extra)
	if err != nil {
		t.Fatalf("PTY: %v", err)
	}
	defer p.Close()
	c := &siCapture{}
	go c.drain(p)

	// \$ в PS1 — «#» у root (прогон в WSL идёт под root) и «$» у остальных.
	sign := "$"
	if os.Geteuid() == 0 {
		sign = "#"
	}
	c.waitFor(t, "первое приглашение A user"+sign+" B", 20*time.Second, func(b []byte) bool {
		return bytes.Contains(b, []byte("\x1b]133;A\x07user"+sign+" \x1b]133;B\x07"))
	})

	// echo $FOO; sleep 1 → C, «1», D;0, A, B — по порядку.
	mark := len(c.snapshot())
	if _, err := p.Write([]byte("echo $FOO; sleep 1\r")); err != nil {
		t.Fatal(err)
	}
	b := c.waitFor(t, "C → 1 → D;0 → A → B", 20*time.Second, func(b []byte) bool {
		cEnd := seqAfter(osc133Marks(b), mark, "C")
		if cEnd < 0 {
			return false
		}
		one := bytes.Index(b[cEnd:], []byte("1\r\n"))
		return one >= 0 && seqAfter(osc133Marks(b), cEnd+one, "D;0", "A", "B") >= 0
	})
	t.Logf("поток команды: %q", b[mark:])

	// Код ошибки.
	mark = len(b)
	if _, err := p.Write([]byte("false\r")); err != nil {
		t.Fatal(err)
	}
	b = c.waitFor(t, "false → C → D;1", 20*time.Second, func(b []byte) bool {
		return seqAfter(osc133Marks(b), mark, "C", "D;1", "A", "B") >= 0
	})

	// Пустой Enter — новое приглашение без C и D.
	dBefore, aBefore := countMarks(osc133Marks(b), "D"), countMarks(osc133Marks(b), "A")
	if _, err := p.Write([]byte("\r")); err != nil {
		t.Fatal(err)
	}
	c.waitFor(t, "приглашение после пустого Enter", 20*time.Second, func(b []byte) bool {
		return countMarks(osc133Marks(b), "A") > aBefore
	})
	time.Sleep(300 * time.Millisecond)
	if got := countMarks(osc133Marks(c.snapshot()), "D"); got != dBefore {
		t.Fatalf("пустой Enter дал D: было %d, стало %d", dBefore, got)
	}

	// Ctrl+C посреди команды — D;130.
	mark = len(c.snapshot())
	if _, err := p.Write([]byte("sleep 30\r")); err != nil {
		t.Fatal(err)
	}
	c.waitFor(t, "C от sleep 30", 20*time.Second, func(b []byte) bool { return seqAfter(osc133Marks(b), mark, "C") >= 0 })
	time.Sleep(200 * time.Millisecond)
	if _, err := p.Write([]byte{0x03}); err != nil {
		t.Fatal(err)
	}
	c.waitFor(t, "D;130 после Ctrl+C", 20*time.Second, func(b []byte) bool {
		return seqAfter(osc133Marks(b), mark, "C", "D;130", "A", "B") >= 0
	})

	// Пользователь перезаписал PS1 — обёртка встаёт заново; его PROMPT_COMMAND жив.
	mark = len(c.snapshot())
	if _, err := p.Write([]byte("PS1='changed> '; echo \"pc=$__user_pc\"\r")); err != nil {
		t.Fatal(err)
	}
	b = c.waitFor(t, "A changed> B после перезаписи PS1", 20*time.Second, func(b []byte) bool {
		return bytes.Contains(b[mark:], []byte("\x1b]133;A\x07changed> \x1b]133;B\x07"))
	})
	if !regexp.MustCompile(`pc=[1-9]`).Match(b[mark:]) {
		t.Fatalf("пользовательский PROMPT_COMMAND не выполнялся: %q", b[mark:])
	}
}

// Наши обёртки не утекают в окружение детей (ревью B2). Пользователь
// экспортирует PROMPT_COMMAND (рецепт общей истории) и PS1 или включает set -a.
// Раньше экспортировалась уже наша версия: вложенный bash на каждом
// приглашении писал «__remotai_si_precmd: command not found», а в env детей
// оказывались обёрнутый PS1 и (при set -a) наши функции.
func TestShellIntegrationLiveBashNoExportLeak(t *testing.T) {
	siLiveGate(t)
	bash := siBash(t)
	sign := "$"
	if os.Geteuid() == 0 {
		sign = "#"
	}
	cases := []struct{ name, rc string }{
		{"export", "export PROMPT_COMMAND=\"history -a${PROMPT_COMMAND:+; $PROMPT_COMMAND}\"\nexport PS1='user\\$ '\n"},
		{"set-a", "set -a\nPS1='user\\$ '\nPROMPT_COMMAND='true'\n"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			if err := os.WriteFile(filepath.Join(home, ".bashrc"), []byte(tc.rc), 0o600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("HOME", home)
			for _, k := range []string{"PROMPT_COMMAND", "PS1", "PS0"} {
				t.Setenv(k, "")
				os.Unsetenv(k)
			}
			root := filepath.Join(t.TempDir(), "shell-integration")
			if err := writeShellIntegrationScripts(root); err != nil {
				t.Fatal(err)
			}
			argv, extra := shellLaunch(bash, true, root, os.Environ())
			p, err := newPlatformPTY(100, 30, home, argv, extra)
			if err != nil {
				t.Fatalf("PTY: %v", err)
			}
			defer p.Close()
			c := &siCapture{}
			go c.drain(p)
			c.waitFor(t, "первое приглашение A user"+sign+" B", 20*time.Second, func(b []byte) bool {
				return bytes.Contains(b, []byte("\x1b]133;A\x07user"+sign+" \x1b]133;B\x07"))
			})

			// Эхо набранной команды содержит «leak=$(», а не «leak=<число>».
			mark := len(c.snapshot())
			if _, err := p.Write([]byte("echo \"leak=$(env | grep -c -e '133;' -e __remotai_si)\"\r")); err != nil {
				t.Fatal(err)
			}
			leakRe := regexp.MustCompile(`leak=(\d+)`)
			b := c.waitFor(t, "leak=<число>", 20*time.Second, func(b []byte) bool { return leakRe.Match(b[mark:]) })
			if m := leakRe.FindSubmatch(b[mark:]); string(m[1]) != "0" {
				t.Fatalf("в окружение детей утекло %s строк с нашими маркерами или функциями", m[1])
			}

			// Вложенный интерактивный bash: своё приглашение, команда, выход.
			mark = len(c.snapshot())
			if _, err := p.Write([]byte("bash -i\r")); err != nil {
				t.Fatal(err)
			}
			c.waitFor(t, "приглашение вложенного bash", 20*time.Second, func(b []byte) bool {
				return bytes.Contains(b[mark:], []byte("user"+sign+" "))
			})
			if _, err := p.Write([]byte("echo nested-$((40+2))\r")); err != nil {
				t.Fatal(err)
			}
			c.waitFor(t, "вывод вложенного bash", 20*time.Second, func(b []byte) bool {
				return bytes.Contains(b[mark:], []byte("nested-42"))
			})
			if _, err := p.Write([]byte("exit\r")); err != nil {
				t.Fatal(err)
			}
			b = c.waitFor(t, "возврат во внешний: D;0 → A → B", 20*time.Second, func(b []byte) bool {
				i := bytes.Index(b[mark:], []byte("nested-42"))
				return i >= 0 && seqAfter(osc133Marks(b), mark+i, "D;0", "A", "B") >= 0
			})
			if bytes.Contains(b, []byte("command not found")) {
				t.Fatalf("вложенный bash споткнулся о наши хуки: %q", b[mark:])
			}
		})
	}
}

// /etc/bash.bashrc: с нашим --rcfile системный rc выполняется столько же раз,
// сколько без него, — то есть ровно как в обычном терминале (bash с
// SYS_BASHRC читает его сам). Считаем по трассировке строку из Ubuntu-версии
// файла; на других системах тест пропускается.
func TestShellIntegrationLiveBashSystemRCOnce(t *testing.T) {
	siLiveGate(t)
	bash := siBash(t)
	data, err := os.ReadFile("/etc/bash.bashrc")
	if err != nil || !bytes.Contains(data, []byte("shopt -s checkwinsize")) {
		t.Skip("нет /etc/bash.bashrc с checkwinsize — считать нечем")
	}
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, ".bashrc"), []byte("FOO=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(t.TempDir(), "shell-integration")
	if err := writeShellIntegrationScripts(root); err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(`(?m)^\++ shopt -s checkwinsize\s*$`)
	count := func(args ...string) int {
		cmd := exec.Command(bash, args...)
		cmd.Env = mergeEnv(os.Environ(), []string{"HOME=" + home}, false)
		cmd.Stdin = strings.NewReader("exit\n")
		out, _ := cmd.CombinedOutput()
		return len(re.FindAll(out, -1))
	}
	plain := count("-i", "-x")
	ours := count("--rcfile", filepath.Join(root, "bash.sh"), "-i", "-x")
	t.Logf("checkwinsize из системного rc: обычный запуск %d, с разметкой %d", plain, ours)
	if plain == 0 {
		t.Skip("обычный bash не читает /etc/bash.bashrc — сравнивать нечего")
	}
	if ours != plain {
		t.Fatalf("системный rc выполнен %d раз против %d в обычном терминале", ours, plain)
	}
}

// zsh — если есть на машине: пользовательские файлы с его ZDOTDIR, маркеры,
// и после старта ZDOTDIR/REMOTAI_USER_ZDOTDIR возвращены в исходное.
func TestShellIntegrationLiveZsh(t *testing.T) {
	siLiveGate(t)
	if runtime.GOOS == "windows" {
		t.Skip("zsh — Linux и macOS")
	}
	zsh, err := exec.LookPath("zsh")
	if err != nil {
		t.Skip("zsh не найден")
	}
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, ".zshrc"), []byte("FOO=1\nPS1='userz%% '\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	os.Unsetenv("ZDOTDIR")
	root := filepath.Join(t.TempDir(), "shell-integration")
	if err := writeShellIntegrationScripts(root); err != nil {
		t.Fatal(err)
	}
	argv, extra := shellLaunch(zsh, true, root, os.Environ())
	p, err := newPlatformPTY(100, 30, home, argv, extra)
	if err != nil {
		t.Fatalf("PTY: %v", err)
	}
	defer p.Close()
	c := &siCapture{}
	go c.drain(p)
	c.waitFor(t, "первое приглашение zsh A…B", 20*time.Second, func(b []byte) bool {
		return seqAfter(osc133Marks(b), 0, "A", "B") >= 0 && bytes.Contains(b, []byte("userz% "))
	})
	mark := len(c.snapshot())
	if _, err := p.Write([]byte("echo \"foo=$FOO zd=[${ZDOTDIR-unset}] uz=[${REMOTAI_USER_ZDOTDIR-unset}]\"\r")); err != nil {
		t.Fatal(err)
	}
	c.waitFor(t, "вывод и C → D;0 → A → B", 20*time.Second, func(b []byte) bool {
		return bytes.Contains(b[mark:], []byte("foo=1 zd=[unset] uz=[unset]")) &&
			seqAfter(osc133Marks(b), mark, "C", "D;0", "A", "B") >= 0
	})
}

// ── Путь через pty-host: флаг идёт хосту переменной окружения ─────────────

// RunHost читает EnvShellIntegration, снимает её и запускает шелл с
// разметкой. Нужен собранный remotai (он же pty-host):
//
//	REMOTAI_EXE=<путь> PTY_LIVE=1 go test ./internal/pty -run TestShellIntegrationLiveHost -v
func TestShellIntegrationLiveHost(t *testing.T) {
	siLiveGate(t)
	exe := os.Getenv("REMOTAI_EXE")
	if exe == "" {
		t.Skip("нет REMOTAI_EXE — нужен собранный remotai для --pty-host")
	}
	home := t.TempDir()
	shell, probe, want := "", "", ""
	if runtime.GOOS == "windows" {
		shell = `C:\Windows\System32\WindowsPowerShell\v1.0\powershell.exe`
		probe = "Write-Output ('si=[' + [string]$env:REMOTAI_SHELL_INTEGRATION + ']' + (40+2))\r"
		want = "si=[]42"
	} else {
		shell = siBash(t)
		if err := os.WriteFile(filepath.Join(home, ".bashrc"), []byte("FOO=1\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		// HOME — временный: хост кладёт скрипты в свой каталог состояния
		// (~/.config/remotai), и настоящий каталог машины трогать незачем.
		t.Setenv("HOME", home)
		probe = "echo \"si=[${REMOTAI_SHELL_INTEGRATION-unset}]$FOO\"\r"
		want = "si=[unset]1"
	}
	id := fmt.Sprintf("si-live-%d", time.Now().UnixNano())
	cmd := exec.Command(exe, "--pty-host", "--id", id, "--cwd", home, "--shell", shell, "--cols", "100", "--rows", "30")
	cmd.Env = hostSpawnEnv(true, os.Environ(), runtime.GOOS == "windows")
	if err := cmd.Start(); err != nil {
		t.Fatalf("pty-host: %v", err)
	}
	defer func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() }()
	pc, err := dialHost(id, 10*time.Second, 100, 30)
	if err != nil {
		t.Fatalf("dial pty-host: %v (лог: %s)", err, HostLogPath(id))
	}
	defer pc.Close()
	c := &siCapture{}
	go c.drain(pc)
	c.waitFor(t, "первое приглашение A…B через pty-host", 60*time.Second, func(b []byte) bool {
		return seqAfter(osc133Marks(b), 0, "A", "B") >= 0
	})
	mark := len(c.snapshot())
	if _, err := pc.Write([]byte(probe)); err != nil {
		t.Fatal(err)
	}
	c.waitFor(t, want+" и D;0 → A → B (шелл не унаследовал флаг хоста)", 30*time.Second, func(b []byte) bool {
		i := bytes.Index(b[mark:], []byte(want))
		return i >= 0 && seqAfter(osc133Marks(b), mark+i, "D;0", "A", "B") >= 0
	})
	if log, err := os.ReadFile(HostLogPath(id)); err == nil {
		if !strings.Contains(string(log), "разметка команд включена") {
			t.Fatalf("хост не записал, что разметка включена:\n%s", log)
		}
	}
}
