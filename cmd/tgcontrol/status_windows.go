//go:build windows

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"tgcontrol/internal/localize"
	"unsafe"

	"golang.org/x/sys/windows/registry"

	"tgcontrol/internal/paths"
	"tgcontrol/internal/web"
)

// cliAutostartState — фоновая проба задачи автозапуска. Окно гасим: смотрим
// только код возврата, а вывод schtasks человеку не нужен (при запуске из
// оконного процесса он превращался бы в мигающее чёрное окно).
func cliAutostartState() (bool, string) {
	return web.AutostartEnabled(), localize.Text("Планировщик задач Windows")
}

func cleanupInstalledArtifacts() error {
	base := filepath.Clean(paths.Base())
	local := filepath.Clean(os.Getenv("LOCALAPPDATA"))
	if local == "." || local == "" || !strings.HasPrefix(strings.ToLower(base), strings.ToLower(local+string(os.PathSeparator))) {
		return removeUserPath(base)
	}
	_ = removeUserPath(base)
	if err := os.Remove(filepath.Join(base, "remotai.com")); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf(localize.Text("удалить CLI launcher: %w"), err)
	}
	selfCopy := filepath.Join(base, "remotai.exe")
	current, _ := os.Executable()
	if !strings.EqualFold(filepath.Clean(current), filepath.Clean(selfCopy)) {
		if err := os.Remove(selfCopy); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf(localize.Text("удалить %s: %w"), selfCopy, err)
		}
	}
	return nil
}

func removeUserPath(dir string) error {
	k, err := registry.OpenKey(registry.CURRENT_USER, "Environment", registry.QUERY_VALUE|registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	cur, typ, err := k.GetStringValue("Path")
	if err != nil && err != registry.ErrNotExist {
		return err
	}
	kept := make([]string, 0)
	for _, p := range strings.Split(cur, ";") {
		if p = strings.TrimSpace(p); p != "" && !strings.EqualFold(filepath.Clean(p), filepath.Clean(dir)) {
			kept = append(kept, p)
		}
	}
	next := strings.Join(kept, ";")
	if typ == registry.EXPAND_SZ {
		err = k.SetExpandStringValue("Path", next)
	} else {
		err = k.SetStringValue("Path", next)
	}
	if err != nil {
		return err
	}
	user32 := syscall.NewLazyDLL("user32.dll")
	env, _ := syscall.UTF16PtrFromString("Environment")
	const hwndBroadcast, wmSettingChange, smtoAbortIfHung = 0xffff, 0x001A, 0x0002
	user32.NewProc("SendMessageTimeoutW").Call(
		hwndBroadcast, wmSettingChange, 0, uintptr(unsafe.Pointer(env)),
		smtoAbortIfHung, 3000, 0,
	)
	return nil
}
