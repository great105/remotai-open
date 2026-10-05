//go:build windows

package web

import (
	"log"
	"runtime"
	"sync"
	"syscall"
	"unsafe"
)

// PrtScr на самом компьютере — снимок экрана без телефона и без отдельной
// программы.
//
// Просьба владельца 08.09: «чтобы в настройках можно было включить скрины,
// чтобы не ставить отдельно». До этого он держал для снимков стороннюю
// программу («Скрин в папку»), а путь к файлу набирал агенту руками. Файл
// кладётся туда же, куда его кладёт кнопка 📷 в приложении, — общей функцией
// saveScreenshotFile, чтобы два способа снять экран не разъехались.
//
// ⚠ КЛАВИША МОЖЕТ БЫТЬ ЗАНЯТА, и это нормальный исход, а не ошибка программы.
// Windows отдаёт PrtScr первому, кто её попросил: если включено системное
// «Использовать клавишу PRINT SCREEN для запуска функции "Ножницы"» или ещё
// работает сторонняя программа владельца, RegisterHotKey вернёт 1409
// (ERROR_HOTKEY_ALREADY_REGISTERED). Мы обязаны СКАЗАТЬ об этом человеку в
// настройках, а не молчать: молчащий тумблер «включено» при неработающей
// клавише — худшее из возможных поведений.
var (
	user32               = syscall.NewLazyDLL("user32.dll")
	procRegisterHotKey   = user32.NewProc("RegisterHotKey")
	procUnregisterHotKey = user32.NewProc("UnregisterHotKey")
	procGetMessageW      = user32.NewProc("GetMessageW")
	procPostThreadMsgW   = user32.NewProc("PostThreadMessageW")
	kernel32             = syscall.NewLazyDLL("kernel32.dll")
	procGetCurrentThread = kernel32.NewProc("GetCurrentThreadId")
)

const (
	vkSnapshot = 0x2C // PrtScr
	wmHotkey   = 0x0312
	wmQuit     = 0x0012
	hotkeyID   = 0xA17 // произвольный, лишь бы не совпал с чужим в этом же потоке
	// errHotkeyTaken — Windows уже отдала эту клавишу другому приложению.
	errHotkeyTaken = 1409
)

type msg struct {
	hwnd   uintptr
	msg    uint32
	wParam uintptr
	lParam uintptr
	time   uint32
	pt     struct{ x, y int32 }
}

type hotkeyState struct {
	mu       sync.Mutex
	running  bool
	threadID uint32
	// lastError — машинная причина для клиента: пусто = клавиша занята нами.
	lastError string
}

var screenshotHotkey hotkeyState

// screenshotHotkeyStatus — что показать в настройках: работает ли клавиша и
// если нет, то почему.
func screenshotHotkeyActive() bool {
	screenshotHotkey.mu.Lock()
	defer screenshotHotkey.mu.Unlock()
	return screenshotHotkey.running
}

func screenshotHotkeyError() string {
	screenshotHotkey.mu.Lock()
	defer screenshotHotkey.mu.Unlock()
	return screenshotHotkey.lastError
}

// setScreenshotHotkey включает или выключает клавишу. Идемпотентна: зовётся и
// при старте, и при каждой смене настройки, перезапуск агента не нужен.
func setScreenshotHotkey(on bool) {
	screenshotHotkey.mu.Lock()
	already := screenshotHotkey.running
	tid := screenshotHotkey.threadID
	screenshotHotkey.mu.Unlock()

	if on == already {
		return
	}
	if !on {
		// Разбудить поток может только сообщение: GetMessageW блокирует.
		if tid != 0 {
			procPostThreadMsgW.Call(uintptr(tid), wmQuit, 0, 0)
		}
		return
	}
	go hotkeyLoop()
}

func hotkeyLoop() {
	// RegisterHotKey с hwnd=0 адресует сообщения ОЧЕРЕДИ ПОТОКА, поэтому
	// регистрация и цикл обязаны жить на одном и том же потоке ОС.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	tid, _, _ := procGetCurrentThread.Call()
	ok, _, err := procRegisterHotKey.Call(0, hotkeyID, 0, vkSnapshot)
	if ok == 0 {
		reason := "hotkey_failed"
		if errno, is := err.(syscall.Errno); is && uintptr(errno) == errHotkeyTaken {
			reason = "hotkey_taken"
		}
		screenshotHotkey.mu.Lock()
		screenshotHotkey.running = false
		screenshotHotkey.lastError = reason
		screenshotHotkey.mu.Unlock()
		log.Printf("[SHOT] PrtScr занять не удалось (%s): %v", reason, err)
		return
	}
	screenshotHotkey.mu.Lock()
	screenshotHotkey.running = true
	screenshotHotkey.threadID = uint32(tid)
	screenshotHotkey.lastError = ""
	screenshotHotkey.mu.Unlock()
	log.Printf("[SHOT] PrtScr занят: снимок уходит в %s", "~/Remotai/files")

	defer func() {
		procUnregisterHotKey.Call(0, hotkeyID)
		screenshotHotkey.mu.Lock()
		screenshotHotkey.running = false
		screenshotHotkey.threadID = 0
		screenshotHotkey.mu.Unlock()
		log.Printf("[SHOT] PrtScr отпущен")
	}()

	var m msg
	for {
		ret, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		// 0 = WM_QUIT (просили выключить), -1 = ошибка очереди.
		if ret == 0 || int32(ret) == -1 {
			return
		}
		if m.msg != wmHotkey {
			continue
		}
		// uid=0: снимок с клавиатуры делает тот, кто сидит за компьютером, а не
		// конкретный вошедший в приложение. Кэш кадра превью тут не при чём.
		shot, _, reason := saveScreenshotFile(0, 0)
		if reason != "" {
			log.Printf("[SHOT] PrtScr: снимок не сохранён (%s)", reason)
			continue
		}
		log.Printf("[SHOT] PrtScr → %v", shot["name"])
	}
}
