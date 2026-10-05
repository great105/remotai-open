//go:build !windows && !darwin

// Файл назывался service_linux.go при теге `!windows` — а суффикс имени в Go
// сильнее комментария: под darwin не компилировался НИ ОДИН файл пакета, и
// сборка Mac-агента падала на первом же импорте. Имя приведено к тегу; у macOS
// теперь свой файл (service_darwin.go) с настоящим LaunchAgent.

package service

import (
	"fmt"
	"os"
)

// Install is a no-op on non-Windows platforms.
func Install() error {
	return fmt.Errorf("Windows service installation not supported on this platform")
}

// Uninstall is a no-op on non-Windows platforms.
func Uninstall() error {
	return fmt.Errorf("Windows service removal not supported on this platform")
}

// Start is a no-op on non-Windows platforms.
func Start() error {
	return fmt.Errorf("Windows service start not supported on this platform")
}

// Stop is a no-op on non-Windows platforms.
func Stop() error {
	return fmt.Errorf("Windows service stop not supported on this platform")
}

// IsInstalled always returns false on non-Windows.
func IsInstalled() bool { return false }

// IsRunning always returns false on non-Windows.
func IsRunning() bool { return false }

// RunAsService always returns false on non-Windows.
func RunAsService() bool { return false }

// ManagerCanRestart — умеет ли сервис-менеджер перезапустить агента по нашей
// просьбе. Здесь службы нет вовсе (RunAsService всегда false), поэтому вопрос
// не встаёт; у systemd свой путь — выход с кодом 0 и Restart=always.
func ManagerCanRestart() bool { return false }

// RestartByManager — см. ManagerCanRestart: просить тут некого.
func RestartByManager() error {
	return fmt.Errorf("сервис-менеджер этой платформы перезапуск по просьбе не поддерживает")
}

// UnderSystemd reports whether the process is running as a systemd service.
// systemd sets INVOCATION_ID (a unique 128-bit id) for every unit it spawns;
// it is absent for a plain shell/SSH launch. Used by the auto-updater to pick
// the "apply + os.Exit(0), let Restart=always relaunch" path (the systemd
// analogue of the Windows-SCM branch) instead of a detached self-restart that
// would be killed inside the unit's cgroup.
func UnderSystemd() bool {
	return os.Getenv("INVOCATION_ID") != ""
}

// Run is a no-op on non-Windows platforms.
func Run(appFunc func(stopCh <-chan struct{})) error {
	return fmt.Errorf("Windows service not supported on this platform")
}
