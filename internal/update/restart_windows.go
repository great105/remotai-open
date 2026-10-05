//go:build windows

package update

import (
	"fmt"
	"os/exec"
	"syscall"
)

const (
	detachedProcess       = 0x00000008
	createNewProcessGroup = 0x00000200
	createNoWindow        = 0x08000000
)

// startDetached запускает exe независимо от текущего процесса.
//
// Прямой CreateProcess из процесса, чей бинарник только что подменён через
// rename (.old/.new), может падать с ERROR_NOT_SUPPORTED («The request is
// not supported») — поймано на установленном экземпляре
// (%LOCALAPPDATA%\Programs\Remotai) при обновлении из панели: Apply прошёл,
// а перезапуск падал, и процесс оставался на старой версии до ручного
// рестарта. Std-хендлы не наследуем (у фонового процесса они мертвы после
// FreeConsole), а при ошибке прямого запуска уходим через `cmd /c start` —
// этот путь давно работает в трее (spawnSelfDelayed).
func startDetached(exe string, args []string) error {
	direct := exec.Command(exe, args...)
	direct.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: detachedProcess | createNewProcessGroup,
	}
	directErr := direct.Start()
	if directErr == nil {
		return nil
	}

	parts := append([]string{"/C", "start", "", exe}, args...)
	fallback := exec.Command("cmd", parts...)
	fallback.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: createNoWindow,
	}
	if err := fallback.Start(); err != nil {
		return fmt.Errorf("direct start: %v; cmd-start fallback: %w", directErr, err)
	}
	return nil
}
