//go:build darwin

// Терминалы на macOS. POSIX-часть та же, что на Linux (/dev/ptmx, отдельная
// сессия у шелла, kill всей группы при закрытии) — расходятся только системные
// вызовы разблокировки псевдотерминала: у Linux это TIOCSPTLCK/TIOCGPTN, у
// macOS — grantpt/unlockpt/ptsname через свои ioctl. Их за нас уже разбирает
// golang.org/x/sys/unix, поэтому магических констант здесь нет.
package pty

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"

	"tgcontrol/internal/envpath"
)

type conPTY struct {
	ptmx *os.File
	cmd  *exec.Cmd
}

// newPlatformPTY — см. pty_linux.go: argv при выключенной разметке команд
// равен [shell] (прежний запуск), extraEnv ложится поверх окружения (ST-10).
func newPlatformPTY(cols, rows int, cwd string, argv, extraEnv []string) (*conPTY, error) {
	if len(argv) == 0 {
		return nil, fmt.Errorf("не задан шелл")
	}
	if cols < 1 {
		cols = 80
	}
	if rows < 1 {
		rows = 24
	}

	ptmx, slavePath, err := openPTY()
	if err != nil {
		return nil, err
	}

	setWinsize(ptmx, cols, rows)

	// O_NOCTTY ОБЯЗАТЕЛЕН — без него терминал достаётся не тому процессу.
	//
	// ЖИВОЙ МАК 10.08.2026: терминалы не открывались вообще, в логе хоста
	// «PTY: fork/exec /bin/zsh: operation not permitted» — на любой оболочке
	// (bash/zsh/sh) и из любого каталога. Цепочка: pty-host запущен с Setsid,
	// то есть он лидер сессии БЕЗ управляющего терминала. По POSIX открытие
	// терминала таким процессом без O_NOCTTY делает этот терминал управляющим
	// ДЛЯ НЕГО. Дальше оболочка делает setsid (своя сессия) и просит
	// TIOCSCTTY — а терминал уже занят чужой сессией, и ядро отвечает EPERM.
	// Go оборачивает это в «fork/exec …», из-за чего ошибка выглядит как
	// проблема запуска бинаря, хотя запуск тут ни при чём.
	//
	// Так же делает эталонный creack/pty: `os.O_RDWR|syscall.O_NOCTTY`.
	slave, err := os.OpenFile(slavePath, os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		ptmx.Close()
		return nil, err
	}

	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = cwd
	cmd.Env = mergeEnv(buildDarwinPTYEnv(""), extraEnv, false)
	cmd.Stdin = slave
	cmd.Stdout = slave
	cmd.Stderr = slave
	// Setsid+Setctty: шелл становится лидером своей сессии, а slave — его
	// управляющим терминалом. Без этого Ctrl+C и job control внутри терминала
	// не работают, а kill группы при закрытии убивал бы не тех.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true}

	if err := cmd.Start(); err != nil {
		slave.Close()
		ptmx.Close()
		return nil, err
	}
	slave.Close()

	return &conPTY{ptmx: ptmx, cmd: cmd}, nil
}

func (p *conPTY) Read(buf []byte) (int, error)   { return p.ptmx.Read(buf) }
func (p *conPTY) Write(data []byte) (int, error) { return p.ptmx.Write(data) }

func (p *conPTY) readAvailable(buf []byte) (int, error) {
	fds := []unix.PollFd{{Fd: int32(p.ptmx.Fd()), Events: unix.POLLIN | unix.POLLHUP | unix.POLLERR}}
	n, err := unix.Poll(fds, 0)
	if err != nil || n == 0 {
		return 0, err
	}
	revents := fds[0].Revents
	if revents&(unix.POLLIN|unix.POLLHUP|unix.POLLERR) == 0 {
		return 0, nil
	}
	n, err = p.ptmx.Read(buf)
	if n == 0 && err == nil && revents&(unix.POLLHUP|unix.POLLERR) != 0 {
		return 0, io.EOF
	}
	return n, err
}

func (p *conPTY) Resize(cols, rows int) error {
	return setWinsize(p.ptmx, cols, rows)
}

func (p *conPTY) Close() error {
	if p.cmd.Process != nil {
		// Шелл — лидер группы (Setsid), поэтому гасим всю группу: иначе агент,
		// запущенный внутри терминала, и его воркеры остаются сиротами.
		_ = syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL)
		_ = p.cmd.Process.Kill()
	}
	return p.ptmx.Close()
}

func setWinsize(f *os.File, cols, rows int) error {
	return unix.IoctlSetWinsize(int(f.Fd()), unix.TIOCSWINSZ, &unix.Winsize{
		Row: uint16(rows), Col: uint16(cols),
	})
}

// ── Открытие псевдотерминала ─────────────────────────────────────
//
// В x/sys/unix обёрток posix_openpt/grantpt/unlockpt/ptsname для darwin нет
// (они живут в libc), поэтому зовём ioctl напрямую. Константы — из
// <sys/ttycom.h>:
//
//	TIOCPTYGRANT _IO('t', 84)                  → права на slave
//	TIOCPTYUNLK  _IO('t', 82)                  → снять замок
//	TIOCPTYGNAME _IOC(IOC_OUT,'t',83,128)      → имя slave (буфер 128 байт)
const (
	tiocPtyGrant = 0x20007454
	tiocPtyUnlk  = 0x20007452
	tiocPtyGname = 0x40807453
	ptyGnameSize = 128
)

func openPTY() (*os.File, string, error) {
	ptmx, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		return nil, "", err
	}
	if err := ioctlNoArg(ptmx.Fd(), tiocPtyGrant); err != nil {
		ptmx.Close()
		return nil, "", fmt.Errorf("grantpt: %w", err)
	}
	if err := ioctlNoArg(ptmx.Fd(), tiocPtyUnlk); err != nil {
		ptmx.Close()
		return nil, "", fmt.Errorf("unlockpt: %w", err)
	}
	var buf [ptyGnameSize]byte
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, ptmx.Fd(), tiocPtyGname,
		uintptr(unsafe.Pointer(&buf[0]))); errno != 0 {
		ptmx.Close()
		return nil, "", fmt.Errorf("ptsname: %w", errno)
	}
	name := string(buf[:])
	if i := strings.IndexByte(name, 0); i >= 0 {
		name = name[:i]
	}
	if name == "" {
		ptmx.Close()
		return nil, "", fmt.Errorf("ptsname вернул пустое имя")
	}
	return ptmx, name, nil
}

func ioctlNoArg(fd uintptr, req uintptr) error {
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, req, 0); errno != 0 {
		return errno
	}
	return nil
}

func defaultShell() string {
	if sh := os.Getenv("SHELL"); sh != "" {
		return sh
	}
	// На macOS с Catalina умолчание — zsh; bash в системе древний (3.2) и
	// стоит только для совместимости.
	if _, err := exec.LookPath("zsh"); err == nil {
		return "zsh"
	}
	return "/bin/sh"
}

// buildDarwinPTYEnv добавляет пользовательские каталоги инструментов в PATH
// (зеркало buildLinuxPTYEnv). На маке это критичнее, чем где-либо: launchd даёт
// службе почти пустое окружение, а Homebrew ставит всё в /opt/homebrew/bin —
// без этого агенты (claude, codex) в терминале «не установлены».
func buildDarwinPTYEnv(home string) []string {
	env := os.Environ()
	dirs := append(envpath.UserBinDirs(home), "/opt/homebrew/bin", "/opt/homebrew/sbin", "/usr/local/bin")
	prefix := strings.Join(dirs, ":")
	for i, v := range env {
		if strings.HasPrefix(v, "PATH=") {
			env[i] = "PATH=" + prefix + ":" + v[len("PATH="):]
			return env
		}
	}
	return append(env, "PATH="+prefix+":/usr/bin:/bin:/usr/sbin:/sbin")
}
