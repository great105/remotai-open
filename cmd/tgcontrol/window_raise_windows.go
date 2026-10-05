//go:build windows

package main

import (
	"os"
	"strings"
	"sync"
	"syscall"
	"unsafe"
)

// Поднять УЖЕ открытое окно Remotai, ничего в нём не открывая.
//
// Зачем отдельно от desktopui.Show: Show у существующего окна делает Navigate на
// переданный адрес, то есть «показать окно» из трея или повторный запуск ярлыка
// выбрасывали человека из терминала в настройки. Здесь только ShowWindow +
// SetForegroundWindow: содержимое окна остаётся тем, на чём его оставили.
var (
	user32raise             = syscall.NewLazyDLL("user32.dll")
	procEnumWindows         = user32raise.NewProc("EnumWindows")
	procGetWindowThreadPID  = user32raise.NewProc("GetWindowThreadProcessId")
	procIsWindowVisible     = user32raise.NewProc("IsWindowVisible")
	procGetWindowTextW      = user32raise.NewProc("GetWindowTextW")
	procShowWindowRaise     = user32raise.NewProc("ShowWindow")
	procSetForegroundWindow = user32raise.NewProc("SetForegroundWindow")
	procIsIconic            = user32raise.NewProc("IsIconic")
)

const swRestoreRaise = 9

// Колбэк создаётся ОДИН раз: syscall.NewCallback навсегда занимает слот в
// таблице процесса, а «показать окно» человек нажимает сколько угодно раз.
var (
	raiseMu      sync.Mutex
	raiseFound   uintptr
	raiseSelfPID uint32
	raiseCB      = syscall.NewCallback(func(hwnd uintptr, _ uintptr) uintptr {
		var pid uint32
		procGetWindowThreadPID.Call(hwnd, uintptr(unsafe.Pointer(&pid)))
		if pid != raiseSelfPID {
			return 1
		}
		if visible, _, _ := procIsWindowVisible.Call(hwnd); visible == 0 {
			return 1
		}
		// У процесса есть и служебные окна (трей, сообщения) — берём то,
		// которое реально называется окном приложения.
		buf := make([]uint16, 128)
		n, _, _ := procGetWindowTextW.Call(hwnd, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
		if n == 0 || !strings.HasPrefix(syscall.UTF16ToString(buf[:n]), "Remotai") {
			return 1
		}
		raiseFound = hwnd
		return 0 // нашли — перечисление можно прекращать
	})
)

// raiseAppWindow возвращает false, если окно не найдено: вызывающий тогда
// открывает его обычным путём (desktopui.Show / браузер).
func raiseAppWindow() bool {
	raiseMu.Lock()
	defer raiseMu.Unlock()
	raiseFound = 0
	raiseSelfPID = uint32(os.Getpid())
	procEnumWindows.Call(raiseCB, 0)
	if raiseFound == 0 {
		return false
	}
	if iconic, _, _ := procIsIconic.Call(raiseFound); iconic != 0 {
		procShowWindowRaise.Call(raiseFound, swRestoreRaise)
	}
	procSetForegroundWindow.Call(raiseFound)
	return true
}
