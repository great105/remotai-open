//go:build linux

package pty

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"time"
)

// newPersistentPTY spawns a detached host process for the session, connects to
// it over the unix socket and returns the client backend plus the record to
// persist.
func newPersistentPTY(id string, cols, rows int, cwd, shell string) (ptyConn, hostRecord, error) {
	return newPersistentPTYWith(id, cols, rows, cwd, shell, false)
}

// newPersistentPTYWith — то же с флагом разметки команд (ST-10): он уходит
// хосту переменной окружения EnvShellIntegration (см. hostSpawnEnv).
func newPersistentPTYWith(id string, cols, rows int, cwd, shell string, shellIntegration bool) (ptyConn, hostRecord, error) {
	pid, err := spawnHost(id, cols, rows, cwd, shell, shellIntegration)
	if err != nil {
		return nil, hostRecord{}, err
	}
	pc, err := dialHost(id, 5*time.Second, cols, rows)
	if err != nil {
		stopHost(id, pid) // don't leave an unreachable orphan behind
		return nil, hostRecord{}, fmt.Errorf("dial pty host: %w", err)
	}
	rec := hostRecord{
		HostPID:  pid,
		PipeName: pipeName(id),
		CWD:      cwd,
		Shell:    shell,
		Created:  time.Now().UnixMilli(),
		Proto:    int(pc.protoVersion()),
	}
	return pc, rec, nil
}

// spawnHost starts `remotai --pty-host ...` detached and returns a liveness PID.
//
// To survive `systemctl restart` (which by default kills the whole unit cgroup),
// it first tries to launch the host in its OWN transient scope via
// `systemd-run --user --scope` — a separate cgroup that the unit's restart does
// not touch. This is the systemd analogue of Windows CREATE_BREAKAWAY_FROM_JOB.
//
// If systemd-run is unavailable or fails (no systemd, no user bus, no linger),
// it falls back to a plain detached Setsid spawn. That fallback survives on a
// unit configured with KillMode=process (which install.sh and the user-unit
// generator both set), and on a plain local/desktop run there is no unit to kill
// it at all.
func spawnHost(id string, cols, rows int, cwd, shell string, shellIntegration bool) (uint32, error) {
	exe, err := os.Executable()
	if err != nil {
		return 0, err
	}
	// nil — унаследовать окружение как есть (прежний запуск байт в байт).
	// systemd-run --scope запускает хост своим потомком, так что окружение
	// доходит и по этому пути.
	hostEnv := hostSpawnEnv(shellIntegration, os.Environ(), false)
	hostArgs := []string{
		"--pty-host",
		"--id", id,
		"--cwd", cwd,
		"--shell", shell,
		"--cols", strconv.Itoa(cols),
		"--rows", strconv.Itoa(rows),
	}
	sock := pipeName(id)

	// Preferred: own transient scope → own cgroup, immune to unit restart. The
	// scope must go on the RIGHT bus — user bus under a user-unit (needs linger),
	// system bus under a system-unit. systemd-run's own Start() succeeds even when
	// the target bus is absent (it fails a moment later), so we don't trust it:
	// after spawning we wait for the host's socket to appear and, if it doesn't,
	// tear the scope down and fall back to a plain spawn.
	if sr, err := exec.LookPath("systemd-run"); err == nil {
		cmd := exec.Command(sr, systemdRunScopeArgs(id, exe, hostArgs)...)
		cmd.Env = hostEnv
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if err := cmd.Start(); err == nil {
			go cmd.Wait() // reap the systemd-run wrapper
			if waitSocket(sock, 2*time.Second) {
				return uint32(cmd.Process.Pid), nil
			}
			log.Printf("[PTY] systemd-run scope produced no socket — falling back to setsid spawn")
			stopScope(id)
		} else {
			log.Printf("[PTY] systemd-run start failed (%v) — falling back to setsid spawn", err)
		}
	}

	// Fallback: plain detached spawn (survives on KillMode=process).
	cmd := exec.Command(exe, hostArgs...)
	cmd.Env = hostEnv
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("spawn pty host: %w", err)
	}
	go cmd.Wait()
	return uint32(cmd.Process.Pid), nil
}

// systemdRunScopeArgs builds the systemd-run argv, targeting the user bus when
// one is reachable (user-unit / desktop with linger) and the system bus
// otherwise (system-unit service).
func systemdRunScopeArgs(id, exe string, hostArgs []string) []string {
	args := []string{"--scope", "--unit", scopeUnit(id), "--quiet", "--collect"}
	if hasUserBus() {
		args = append([]string{"--user"}, args...)
	}
	args = append(args, exe)
	return append(args, hostArgs...)
}

// hasUserBus reports whether a systemd --user manager is reachable.
func hasUserBus() bool {
	if os.Getenv("DBUS_SESSION_BUS_ADDRESS") != "" {
		return true
	}
	if x := os.Getenv("XDG_RUNTIME_DIR"); x != "" {
		if _, err := os.Stat(filepath.Join(x, "bus")); err == nil {
			return true
		}
	}
	return false
}

// waitSocket polls for the host's listening socket to appear (host is ready).
func waitSocket(path string, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return true
		}
		time.Sleep(25 * time.Millisecond)
	}
	return false
}

func scopeUnit(id string) string { return "remotai-pty-" + id + ".scope" }

// stopScope tears down a transient scope on whichever bus we targeted.
func stopScope(id string) {
	if _, err := exec.LookPath("systemctl"); err != nil {
		return
	}
	if hasUserBus() {
		_ = exec.Command("systemctl", "--user", "stop", scopeUnit(id)).Run()
	} else {
		_ = exec.Command("systemctl", "stop", scopeUnit(id)).Run()
	}
}

// stopHost tears a failed/unreachable host down: first the transient scope (if
// systemd-run was used), then the recorded PID as a fallback.
func stopHost(id string, pid uint32) {
	stopScope(id)
	killPID(pid)
}

// processAlive — жив ли процесс. `Signal(0)` ничего не посылает, а только
// проверяет: доставить сигнал можно лишь существующему процессу.
//
// Зачем это нужно рядом с дозвоном — см. одноимённую функцию в
// persist_windows.go: неудачный дозвон не доказывает смерть хоста, а хоронить
// по нему живой терминал нельзя.
func processAlive(pid uint32) bool {
	if pid == 0 {
		return false
	}
	p, err := os.FindProcess(int(pid))
	if err != nil {
		return false
	}
	return p.Signal(syscall.Signal(0)) == nil
}

func killPID(pid uint32) {
	if pid == 0 {
		return
	}
	if p, err := os.FindProcess(int(pid)); err == nil {
		_ = p.Kill()
	}
}
