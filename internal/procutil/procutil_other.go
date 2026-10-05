//go:build !windows

package procutil

import (
	"os/exec"
	"syscall"
)

func configureProcAttr(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	// Own process group so we can signal the whole tree with kill(-pgid).
	cmd.SysProcAttr.Setpgid = true
}

// На Unix консольных окон у дочерних процессов не бывает — прятать нечего.
func hideConsole(*exec.Cmd) {}

func killTree(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	// Negative PID targets the process group created via Setpgid.
	return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}

func killPIDTree(pid int) error {
	if pid <= 0 {
		return nil
	}
	// Интерактивный шелл с управлением заданиями ставит команду переднего плана
	// в свою группу с pgid == pid — она уходит целиком. Если группы нет,
	// остаётся сам процесс: SIGTERM Claude и Codex отрабатывают штатно и
	// закрывают stdio своих MCP.
	if err := syscall.Kill(-pid, syscall.SIGTERM); err == nil {
		return nil
	}
	return syscall.Kill(pid, syscall.SIGTERM)
}
