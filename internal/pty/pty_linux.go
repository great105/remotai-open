//go:build linux

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

// conPTY on Linux uses POSIX PTY (openpty via ioctl).
type conPTY struct {
	ptmx *os.File
	cmd  *exec.Cmd
}

// newPlatformPTY запускает шелл в новом псевдотерминале. argv — готовая
// командная строка (argv[0] — путь к шеллу): при выключенной разметке команд
// это ровно [shell], то есть прежний запуск; extraEnv ложится поверх
// окружения (shellLaunch, ST-10).
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

	ptmx, err := posixOpenPTY()
	if err != nil {
		return nil, err
	}

	// Set window size.
	ws := struct {
		Row, Col, Xpixel, Ypixel uint16
	}{uint16(rows), uint16(cols), 0, 0}
	syscall.Syscall(syscall.SYS_IOCTL, ptmx.Fd(),
		uintptr(syscall.TIOCSWINSZ), uintptr(unsafe.Pointer(&ws)))

	// Get slave name.
	slavePath, err := ptsName(ptmx)
	if err != nil {
		ptmx.Close()
		return nil, err
	}

	// O_NOCTTY — чтобы pty-host (он лидер сессии без управляющего терминала)
	// не забрал этот терминал себе: тогда оболочка не сможет сделать его своим
	// и получит EPERM. На маке это уронило терминалы насовсем (10.08.2026), на
	// Linux до сих пор спасал root — привилегированному процессу ядро разрешает
	// отобрать терминал. Под обычным пользователем такой защиты нет.
	slave, err := os.OpenFile(slavePath, os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		ptmx.Close()
		return nil, err
	}

	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = cwd
	cmd.Env = mergeEnv(buildLinuxPTYEnv(""), extraEnv, false)
	cmd.Stdin = slave
	cmd.Stdout = slave
	cmd.Stderr = slave
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
	ws := struct {
		Row, Col, Xpixel, Ypixel uint16
	}{uint16(rows), uint16(cols), 0, 0}
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, p.ptmx.Fd(),
		uintptr(syscall.TIOCSWINSZ), uintptr(unsafe.Pointer(&ws)))
	if errno != 0 {
		return errno
	}
	return nil
}

func (p *conPTY) Close() error {
	if p.cmd.Process != nil {
		// Шелл запущен с Setsid — он лидер своей process group. Убиваем всю
		// группу (отрицательный pid), иначе потомки (агент внутри терминала и
		// его воркеры) осиротеют и живут вечно; Kill() самого шелла — фолбэк.
		_ = syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL)
		p.cmd.Process.Kill()
	}
	return p.ptmx.Close()
}

// Persistent (restart-surviving) terminals on Linux live in a detached pty-host
// process reached over a unix socket — see host_linux.go (newHostServer),
// pipe_linux.go (transport), pipe_client_linux.go (client), persist_linux.go
// (spawn/reattach). newPersistentPTY / reattachHosts / RunHost are now the real
// cross-platform implementations (persist_linux.go, persist_common.go,
// host_common.go), no longer the stubs that used to sit here.

func defaultShell() string {
	if sh := os.Getenv("SHELL"); sh != "" {
		return sh
	}
	if _, err := exec.LookPath("bash"); err == nil {
		return "bash"
	}
	return "sh"
}

// buildLinuxPTYEnv returns the child environment for a PTY shell with user-local
// tool dirs (~/.local/bin, npm-global, nvm, …) prepended to PATH. Under a
// systemd service the inherited PATH is the minimal system PATH, so agents
// installed into the user profile (claude/codex) would be invisible. Mirrors
// buildEnvBlock() on Windows. HOME is left as inherited — under `User=` systemd
// sets it correctly.
func buildLinuxPTYEnv(home string) []string {
	env := os.Environ()
	dirs := envpath.UserBinDirs(home)
	if len(dirs) == 0 {
		return env
	}
	prefix := strings.Join(dirs, ":")
	for i, v := range env {
		if strings.HasPrefix(v, "PATH=") {
			env[i] = "PATH=" + prefix + ":" + v[len("PATH="):]
			return env
		}
	}
	// No PATH inherited — set one seeded with the standard system dirs.
	return append(env, "PATH="+prefix+":/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin")
}

// ── POSIX PTY helpers ────────────────────────────────────────────

func posixOpenPTY() (*os.File, error) {
	ptmx, err := os.OpenFile("/dev/ptmx", os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	// grantpt + unlockpt
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, ptmx.Fd(), uintptr(0x40045431), 0); errno != 0 {
		// TIOCSPTLCK unlock
	}
	var unlock int
	syscall.Syscall(syscall.SYS_IOCTL, ptmx.Fd(), uintptr(0x40045431), uintptr(unsafe.Pointer(&unlock)))
	return ptmx, nil
}

func ptsName(ptmx *os.File) (string, error) {
	var n uint32
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, ptmx.Fd(), uintptr(0x80045430), uintptr(unsafe.Pointer(&n)))
	if errno != 0 {
		return "", fmt.Errorf("TIOCGPTN: %w", errno)
	}
	return fmt.Sprintf("/dev/pts/%d", n), nil
}
