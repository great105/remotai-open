// Package procutil spawns child processes so that cancelling them kills the
// WHOLE process tree, not just the immediate child.
//
// On Windows the agents run as `cmd /c <claude|codex|...>`, so the immediate
// child is cmd.exe and the real worker (node.exe running claude, git, an eval
// command) is its child. A plain Process.Kill / TerminateProcess only kills
// cmd.exe and orphans the worker — leaking processes/handles and holding the
// session's cwd / git-index locks. Prepare wires a tree-kill on cancel.
package procutil

import (
	"os/exec"
	"time"
)

// Prepare wires cmd so that cancelling its context (or calling KillTree)
// terminates the entire child process tree. Call it right after
// exec.CommandContext(...) and before Start/Run/CombinedOutput.
func Prepare(cmd *exec.Cmd) {
	configureProcAttr(cmd)
	c := cmd
	cmd.Cancel = func() error { return killTree(c) }
	// If the tree doesn't die promptly after Cancel, force the issue instead of
	// blocking Wait forever.
	cmd.WaitDelay = 5 * time.Second
}

// KillTree terminates cmd's entire process tree immediately. Safe to call on a
// finished/never-started command (no-op).
func KillTree(cmd *exec.Cmd) error {
	return killTree(cmd)
}

// Hidden запускает команду БЕЗ собственного консольного окна.
//
// remotai.exe собран как оконное приложение и своей консоли не имеет. Поэтому
// каждый запуск консольной программы (а `claude`, `codex`, `npm` на Windows —
// это ещё и .cmd-обёртки, то есть лишний cmd.exe) создаёт СВОЁ чёрное окно.
// Человек за компьютером видит, как «сами собой открываются и закрываются
// пустые терминалы»: вкладка «Панель ПК» спрашивает /api/setup/detect, а тот
// опрашивает версии всех известных агентов — до двух десятков вспышек подряд.
//
// Вызывать для ЛЮБОЙ фоновой команды. Не вызывать там, где окно нужно человеку
// («Открыть терминал на ПК», handoff `remotai attach`) — там оно и есть смысл.
// На не-Windows это пустышка, так что вызов безопасен в общем коде.
func Hidden(cmd *exec.Cmd) *exec.Cmd {
	hideConsole(cmd)
	return cmd
}

// KillPIDTree снимает процесс по PID вместе со всеми потомками. Нужна
// усыплению агента: у Claude под ним живут MCP-серверы и node, и снять один
// claude.exe значило бы оставить их сиротами — ровно ту память, ради которой
// усыпляют.
func KillPIDTree(pid int) error {
	return killPIDTree(pid)
}
