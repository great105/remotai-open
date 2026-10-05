//go:build darwin

// Персистентные терминалы на macOS: pty-host живёт отдельным процессом и
// переживает перезапуск агента.
//
// На Linux хост уводят в собственный transient scope (`systemd-run --scope`),
// потому что `systemctl restart` по умолчанию убивает весь cgroup юнита. У
// launchd такого нет: `launchctl kickstart -k` гасит процессы СВОЕЙ службы, а
// отвязанный setsid-процесс к ней уже не относится и остаётся жить. Поэтому
// здесь простой detached-спавн — тот же путь, что у Linux в качестве запасного.
package pty

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"time"
)

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
		// Различаем «хост жив, но молчит» и «хост УМЕР».
		//
		// Голое «dial pty host: connect: no such file or directory» уводит в
		// сторону сети и прав на сокет, хотя на деле процесс уже мёртв (живой
		// мак 10.08.2026). Ошибка обязана называть это прямо и говорить, где
		// смотреть причину — она теперь пишется в pty-host.log.
		alive := processAlive(pid)
		stopHost(id, pid) // не оставляем недостижимого сироту
		if !alive {
			return nil, hostRecord{}, fmt.Errorf(
				"процесс терминала завершился сразу после запуска; причина — в %s (%w)", HostLogPath(id), err)
		}
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

// spawnHost запускает `remotai --pty-host …` отвязанным процессом.
func spawnHost(id string, cols, rows int, cwd, shell string, shellIntegration bool) (uint32, error) {
	exe, err := os.Executable()
	if err != nil {
		return 0, err
	}
	cmd := exec.Command(exe,
		"--pty-host",
		"--id", id,
		"--cwd", cwd,
		"--shell", shell,
		"--cols", strconv.Itoa(cols),
		"--rows", strconv.Itoa(rows),
	)
	// nil — унаследовать окружение как есть (прежний запуск байт в байт).
	cmd.Env = hostSpawnEnv(shellIntegration, os.Environ(), false)
	// Setsid отвязывает хост от сессии агента: перезапуск агента (обновление,
	// launchctl kickstart) его не заденет.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	// Вывод хоста — В ФАЙЛ, иначе его ошибки теряются НАСОВСЕМ.
	//
	// Живой мак 10.08.2026: терминал не открывался, агент писал
	// «dial pty host: … connect: no such file or directory» — то есть хост не
	// поднял свой сокет. ПОЧЕМУ — узнать было неоткуда: процесс отвязанный, его
	// stdout и stderr шли в никуда. Диагностика упиралась в стену на ровном
	// месте: причина существовала, но была невидима.
	if f, err := hostLogFile(); err == nil {
		cmd.Stdout, cmd.Stderr = f, f
		defer f.Close() // дескриптор уже унаследован потомком
	}
	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("spawn pty host: %w", err)
	}
	go cmd.Wait() // не оставляем зомби
	return uint32(cmd.Process.Pid), nil
}

// hostLogFile — куда пишет pty-host. Рядом с логами самого агента
// (`~/Library/Logs/Remotai/`), отдельным файлом: два процесса в один файл
// пишут вперемешку, и разбирать такую кашу потом некому.
func hostLogFile() (*os.File, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(home, "Library", "Logs", "Remotai")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return os.OpenFile(filepath.Join(dir, "pty-host.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
}

func stopHost(id string, pid uint32) {
	killPID(pid)
}

// processAlive — жив ли процесс. `Signal(0)` ничего не посылает, а только
// проверяет: доставить сигнал можно лишь существующему процессу.
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
