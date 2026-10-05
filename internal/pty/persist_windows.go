//go:build windows

package pty

import (
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/windows"
)

// createBreakawayFromJob lets the spawned host escape a Job Object with
// KILL_ON_JOB_CLOSE (e.g. when remotai was launched under WebView2/Edge), so it
// survives the parent's exit. The other detach flags are defined in
// handoff_windows.go (same package): detachedProcess, createNewProcessGroup,
// createNoWindow.
const createBreakawayFromJob = 0x01000000

// newPersistentPTY spawns a detached host process for the session, connects to
// it and returns the client backend plus the record to persist.
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
		killPID(pid) // don't leave an unreachable orphan behind
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

// spawnHost starts `remotai --pty-host ...` detached and returns its PID. It
// first tries to break away from any Job Object; if the job forbids that, it
// retries without the flag.
func spawnHost(id string, cols, rows int, cwd, shell string, shellIntegration bool) (uint32, error) {
	exe, err := os.Executable()
	if err != nil {
		return 0, err
	}
	// nil — унаследовать окружение как есть (прежний запуск байт в байт).
	hostEnv := hostSpawnEnv(shellIntegration, os.Environ(), true)
	args := []string{
		"--pty-host",
		"--id", id,
		"--cwd", cwd,
		"--shell", shell,
		"--cols", strconv.Itoa(cols),
		"--rows", strconv.Itoa(rows),
	}

	start := func(flags uint32) (*exec.Cmd, error) {
		cmd := exec.Command(exe, args...)
		cmd.Env = hostEnv
		cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: flags}
		return cmd, cmd.Start()
	}

	base := uint32(detachedProcess | createNewProcessGroup | createNoWindow)
	cmd, err := start(base | createBreakawayFromJob)
	if err != nil {
		log.Printf("[PTY] host spawn with breakaway failed (%v) — trying WMI job-escape", err)
		if hostEnv != nil {
			// Процесс через WMI рождает служба WMI со СВОИМ окружением: флаг
			// разметки команд туда не доходит, и этот терминал откроется без
			// неё — как раньше, без поломки.
			log.Printf("[PTY] разметка команд не передаётся через WMI — терминал %s без неё", id)
		}
		if pid, werr := spawnHostViaWMI(exe, args); werr == nil {
			return pid, nil
		} else {
			log.Printf("[PTY] WMI job-escape failed (%v) — spawning in-job (host will die with agent)", werr)
		}
		cmd, err = start(base)
		if err != nil {
			return 0, fmt.Errorf("spawn pty host: %w", err)
		}
	}
	go cmd.Wait() // reap the detached child handle
	return uint32(cmd.Process.Pid), nil
}

// spawnHostViaWMI creates the host through WMI Win32_Process.Create. The
// creator is the WMI provider service (not us), so the process lands OUTSIDE
// our Job Object even when that job forbids CREATE_BREAKAWAY_FROM_JOB
// (ERROR_ACCESS_DENIED) — the host then survives an agent restart, as
// persistent terminals intend. Chain: breakaway → WMI → in-job (degraded).
func spawnHostViaWMI(exe string, args []string) (uint32, error) {
	quoted := make([]string, 0, len(args)+1)
	quote := func(s string) string {
		// CommandLineToArgvW: a backslash right before the closing quote must
		// be doubled, embedded quotes backslash-escaped.
		s = strings.ReplaceAll(s, `"`, `\"`)
		if strings.HasSuffix(s, `\`) {
			s += `\`
		}
		return `"` + s + `"`
	}
	quoted = append(quoted, quote(exe))
	for _, a := range args {
		quoted = append(quoted, quote(a))
	}
	// PowerShell single-quoted string: embedded single quotes are doubled.
	psCmdline := strings.ReplaceAll(strings.Join(quoted, " "), `'`, `''`)
	// Win32_ProcessStartup.ShowWindow=0 (SW_HIDE): WMI создаёт процесс с
	// видимой консолью по умолчанию — без этого на каждый терминал всплывает
	// чёрное окно remotai.exe.
	ps := `$si = New-CimInstance -ClassName Win32_ProcessStartup -Property @{ ShowWindow = [uint16]0 } -ClientOnly; ` +
		`$r = Invoke-CimMethod -ClassName Win32_Process -MethodName Create -Arguments @{ CommandLine = '` +
		psCmdline + `'; ProcessStartupInformation = $si }; if ($r.ReturnValue -ne 0) { exit $r.ReturnValue }; [Console]::Out.Write($r.ProcessId)`
	cmd := exec.Command("powershell", "-NoProfile", "-WindowStyle", "Hidden", "-Command", ps)
	// HideWindow не действует на новую консоль (у родителя её уже нет) —
	// CREATE_NO_WINDOW гасит окно самого powershell.exe.
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}
	out, err := cmd.Output()
	if err != nil {
		return 0, fmt.Errorf("wmi create: %w", err)
	}
	pid64, err := strconv.ParseUint(strings.TrimSpace(string(out)), 10, 32)
	if err != nil || pid64 == 0 {
		return 0, fmt.Errorf("wmi create: bad pid %q", strings.TrimSpace(string(out)))
	}
	return uint32(pid64), nil
}

func killPID(pid uint32) {
	if pid == 0 {
		return
	}
	if p, err := os.FindProcess(int(pid)); err == nil {
		_ = p.Kill()
	}
}

// stillActive — код, который GetExitCodeProcess отдаёт, пока процесс НЕ вышел.
const stillActive = 259

// processAlive — жив ли процесс хоста.
//
// ⚠ ЗАЧЕМ ОТДЕЛЬНО ОТ ДОЗВОНА. Неудачный дозвон НЕ доказывает смерть хоста.
// Труба однопользовательская (nMaxInstances=1), и в промежутке между «клиент
// отцепился» и «хост создал следующий экземпляр» свободного экземпляра нет
// вовсе: CreateFile отвечает ERROR_FILE_NOT_FOUND — ровно тем же, чем и на
// действительно мёртвого хоста. Различить их можно только спросив про процесс.
//
// Живой случай владельца (17.08.2026): три неудачных дозвона за три секунды
// объявили терминал завершённым, а host_pid=18052 жил дальше, и внутри него
// работал Claude Code со всеми MCP-серверами. Сессия осталась недостижимой на
// часы. os.FindProcess на Windows успешен ВСЕГДА и для проверки не годится —
// нужен именно код выхода.
//
// PROCESS_QUERY_LIMITED_INFORMATION, а не полный QUERY_INFORMATION: право
// слабее, работает и для процессов другой сессии/уровня целостности, а кода
// выхода достаточно.
func processAlive(pid uint32) bool {
	if pid == 0 {
		return false
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h)
	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		return false
	}
	return code == stillActive
}

// hostBusyErr — дозвон не прошёл, потому что труба ЗАНЯТА, а не потому что
// хоста нет. Труба у хоста однопользовательская (nMaxInstances=1, «один хост —
// один клиент»), поэтому второй процесс агента всегда получает ERROR_PIPE_BUSY
// — и это доказательство ЖИЗНИ хоста, а не его смерти. Раньше reattachHosts не
// различал ошибки и помечал такие терминалы потерянными: 31.07.2026 второй
// экземпляр агента за 23 секунды объявил «ждёт восстановления» все 16 живых
// терминалов владельца.
func hostBusyErr(err error) bool {
	return errors.Is(err, windows.ERROR_PIPE_BUSY)
}
