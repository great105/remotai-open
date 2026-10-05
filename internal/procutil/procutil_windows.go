//go:build windows

package procutil

import (
	"os/exec"
	"strconv"
	"syscall"
)

const createNewProcessGroup = 0x00000200 // CREATE_NEW_PROCESS_GROUP
const createNoWindow = 0x08000000        // CREATE_NO_WINDOW

// hideConsole гасит консольное окно дочернего процесса.
//
// Нужны ОБА признака: HideWindow прячет окно, которое создаёт сама Windows по
// STARTUPINFO, а CREATE_NO_WINDOW не даёт консоль вовсе — без него .cmd-обёртки
// (claude, codex, npm) всё равно моргают окном cmd.exe.
func hideConsole(cmd *exec.Cmd) {
	if cmd == nil {
		return
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.HideWindow = true
	cmd.SysProcAttr.CreationFlags |= createNoWindow
}

func configureProcAttr(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	// Isolate the child's process group from ours.
	cmd.SysProcAttr.CreationFlags |= createNewProcessGroup
}

func killTree(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	// taskkill /T walks the process tree (by parent PID) and /F force-kills it,
	// so node/claude/git children die with the cmd.exe wrapper. Invoked from
	// cmd.Cancel while the wrapper is still alive, the tree is intact.
	kill := exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid))
	hideConsole(kill)
	return kill.Run()
}

func killPIDTree(pid int) error {
	if pid <= 0 {
		return nil
	}
	kill := exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(pid))
	hideConsole(kill)
	return kill.Run()
}
