//go:build windows

package input

import (
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unicode/utf16"
	"unsafe"
)

// A failing Windows input syscall means the desktop is locked or a UAC /
// secure-desktop prompt is up (UIPI), so injected input is dropped. The
// sentinel lives in input.go (ErrInputBlocked) — the remote-desktop layer turns
// it into a machine code for the client.

var lastInputErrLog atomic.Int64

func logInputBlocked(what string) {
	now := time.Now().Unix()
	last := lastInputErrLog.Load()
	if now-last >= 5 && lastInputErrLog.CompareAndSwap(last, now) {
		log.Printf("[INPUT] %s failed — desktop may be locked or a UAC prompt is active (input dropped)", what)
	}
}

var (
	user32               = syscall.NewLazyDLL("user32.dll")
	procSetCursorPos     = user32.NewProc("SetCursorPos")
	procGetCursorPos     = user32.NewProc("GetCursorPos")
	procSendInput        = user32.NewProc("SendInput")
	procGetSystemMetrics = user32.NewProc("GetSystemMetrics")
	procMapVirtualKeyW   = user32.NewProc("MapVirtualKeyW")
)

var _ = procSetCursorPos // kept: handy for debugging against SendInput

const (
	mousefMove        = 0x0001
	mousefLeftDown    = 0x0002
	mousefLeftUp      = 0x0004
	mousefRightDown   = 0x0008
	mousefRightUp     = 0x0010
	mousefMiddleDown  = 0x0020
	mousefMiddleUp    = 0x0040
	mousefWheel       = 0x0800
	mousefHWheel      = 0x1000 // MOUSEEVENTF_HWHEEL — горизонтальное колесо
	mousefVirtualDesk = 0x4000
	mousefAbsolute    = 0x8000

	kbfExtended = 0x0001
	kbfKeyUp    = 0x0002
	kbfUnicode  = 0x0004

	inputMouse    = 0
	inputKeyboard = 1

	// GetSystemMetrics indices for the virtual screen (bounding box of all
	// monitors) — used to normalise absolute mouse coords to 0..65535.
	smXVirtualScreen  = 76
	smYVirtualScreen  = 77
	smCXVirtualScreen = 78
	smCYVirtualScreen = 79
)

type controller struct{}

func newController() *controller { return &controller{} }

func (c *controller) MouseMove(x, y int) error {
	// SendInput (not SetCursorPos): a real injected MOVE event goes through the
	// raw-input pipeline, so games/apps reading WM_INPUT see the motion too —
	// SetCursorPos only warps the cursor. Also keeps move+click on one API.
	return c.sendMouseInput(x, y, mousefMove|mousefAbsolute|mousefVirtualDesk, 0)
}

// CursorPos returns the HOST cursor position in virtual-screen pixels. Used to
// echo the real cursor back to remote clients rendering a local one.
func CursorPos() (int, int, bool) {
	var pt struct{ x, y int32 }
	r, _, _ := procGetCursorPos.Call(uintptr(unsafe.Pointer(&pt)))
	if r == 0 {
		return 0, 0, false
	}
	return int(pt.x), int(pt.y), true
}

func (c *controller) MouseClick(x, y int, button string) error {
	down, up := buttonFlags(button)
	move := uint32(mousefMove | mousefAbsolute | mousefVirtualDesk)
	if err := c.sendMouseInput(x, y, move|down, 0); err != nil {
		return err
	}
	time.Sleep(15 * time.Millisecond)
	return c.sendMouseInput(x, y, move|up, 0)
}

func (c *controller) MouseDoubleClick(x, y int) error {
	if err := c.MouseClick(x, y, "left"); err != nil {
		return err
	}
	time.Sleep(60 * time.Millisecond)
	return c.MouseClick(x, y, "left")
}

func (c *controller) MouseDown(x, y int, button string) error {
	down, _ := buttonFlags(button)
	return c.sendMouseInput(x, y, mousefMove|mousefAbsolute|mousefVirtualDesk|down, 0)
}

func (c *controller) MouseUp(x, y int, button string) error {
	_, up := buttonFlags(button)
	return c.sendMouseInput(x, y, mousefMove|mousefAbsolute|mousefVirtualDesk|up, 0)
}

func (c *controller) Scroll(dy int) error {
	// Positive dy = scroll up (wheel forward), negative = scroll down.
	// mouseData carries the wheel delta; no positional move needed.
	return c.sendMouseInput(0, 0, mousefWheel, int32(dy*120))
}

// ScrollH — горизонтальное колесо (MOUSEEVENTF_HWHEEL). Положительный dx —
// вправо, как у настоящего колеса с наклоном.
func (c *controller) ScrollH(dx int) error {
	return c.sendMouseInput(0, 0, mousefHWheel, int32(dx*120))
}

// sendMouseInput injects one MOUSEINPUT via SendInput. Replaces the legacy
// mouse_event path: clicks now carry MOUSEEVENTF_MOVE|ABSOLUTE so the button
// event lands at exactly (x,y) in a single atomic injection. SetCursorPos +
// a separate mouse_event could dissociate the click position from the cursor,
// so clicks registered nowhere even though the cursor moved (the reported bug).
// Coords are normalised against the virtual screen (all monitors) for
// MOUSEEVENTF_VIRTUALDESK.
func (c *controller) sendMouseInput(x, y int, flags uint32, mouseData int32) error {
	var nx, ny int32
	if flags&mousefAbsolute != 0 {
		ox, oy, cx, cy := virtualScreen()
		nx = absCoord(int32(x), ox, cx)
		ny = absCoord(int32(y), oy, cy)
	}

	// INPUT (x64): type@0, union@8. MOUSEINPUT: dx@8, dy@12, mouseData@16,
	// dwFlags@20, time@24, dwExtraInfo@32. sizeof(INPUT)=40 (matches TypeText).
	var buf [40]byte
	*(*uint32)(unsafe.Pointer(&buf[0])) = inputMouse
	*(*int32)(unsafe.Pointer(&buf[8])) = nx
	*(*int32)(unsafe.Pointer(&buf[12])) = ny
	*(*int32)(unsafe.Pointer(&buf[16])) = mouseData
	*(*uint32)(unsafe.Pointer(&buf[20])) = flags

	n, _, _ := procSendInput.Call(1, uintptr(unsafe.Pointer(&buf[0])), 40)
	if n != 1 {
		logInputBlocked("SendInput(mouse)")
		return ErrInputBlocked
	}
	return nil
}

func getSystemMetrics(index int) int {
	r, _, _ := procGetSystemMetrics.Call(uintptr(index))
	return int(int32(r))
}

// virtualScreen returns the virtual-screen geometry with a short TTL cache:
// moves arrive at up to 120Hz and each absolute injection needs all four
// metrics; monitor topology changes are rare, 2s staleness is invisible.
var vsCache struct {
	mu             sync.Mutex
	at             time.Time
	ox, oy, cx, cy int32
}

func virtualScreen() (ox, oy, cx, cy int32) {
	vsCache.mu.Lock()
	defer vsCache.mu.Unlock()
	if time.Since(vsCache.at) > 2*time.Second {
		vsCache.ox = int32(getSystemMetrics(smXVirtualScreen))
		vsCache.oy = int32(getSystemMetrics(smYVirtualScreen))
		vsCache.cx = int32(getSystemMetrics(smCXVirtualScreen))
		vsCache.cy = int32(getSystemMetrics(smCYVirtualScreen))
		vsCache.at = time.Now()
	}
	return vsCache.ox, vsCache.oy, vsCache.cx, vsCache.cy
}

// absCoord normalises a virtual-screen pixel coordinate to the 0..65535 range
// MOUSEEVENTF_ABSOLUTE expects. origin/size describe one virtual-screen axis.
func absCoord(px, origin, size int32) int32 {
	if size <= 1 {
		return 0
	}
	v := (int64(px-origin) * 65535) / int64(size-1)
	if v < 0 {
		v = 0
	}
	if v > 65535 {
		v = 65535
	}
	return int32(v)
}

func (c *controller) KeyDown(key string) error {
	if vk := keyToVK(key); vk != 0 {
		return c.sendKey(vk, false)
	}
	// Буква не с текущей раскладки (кириллица, умляуты): нажимать нечего —
	// ВВОДИМ её как текст. Иначе vk == 0 и человек печатал в пустоту: на ПК не
	// появлялось ни буквы, а локально нажатие уже погашено клиентом.
	if r, ok := PrintableRune(key); ok {
		return c.TypeText(string(r))
	}
	return nil
}

func (c *controller) KeyUp(key string) error {
	// Символ, введённый через TypeText, там же и «отпущен»: пары событий у него
	// нет, второй раз печатать его нельзя.
	if vk := keyToVK(key); vk != 0 {
		return c.sendKey(vk, true)
	}
	return nil
}

// sendKey injects one key event via SendInput, keeping VK semantics (layout
// stays correct on non-QWERTY keyboards) but ALSO carrying the hardware
// scancode from MapVirtualKey plus KEYEVENTF_EXTENDEDKEY where the E0 prefix
// is required. The legacy keybd_event path sent scancode 0 and no extended
// flag, so raw-input/DirectInput consumers ignored arrows/Del/Home entirely
// or saw their numpad twins.
func (c *controller) sendKey(vk uint16, up bool) error {
	if vk == 0 {
		return nil
	}
	scan, _, _ := procMapVirtualKeyW.Call(uintptr(vk), 0) // MAPVK_VK_TO_VSC
	var flags uint32
	if up {
		flags |= kbfKeyUp
	}
	if isExtendedVK(vk) {
		flags |= kbfExtended
	}

	// INPUT (x64): type@0, union@8. KEYBDINPUT: wVk@8, wScan@10, dwFlags@12,
	// time@16, dwExtraInfo@24. sizeof(INPUT)=40 (matches TypeText).
	var buf [40]byte
	*(*uint32)(unsafe.Pointer(&buf[0])) = inputKeyboard
	*(*uint16)(unsafe.Pointer(&buf[8])) = vk
	*(*uint16)(unsafe.Pointer(&buf[10])) = uint16(scan)
	*(*uint32)(unsafe.Pointer(&buf[12])) = flags

	n, _, _ := procSendInput.Call(1, uintptr(unsafe.Pointer(&buf[0])), 40)
	if n != 1 {
		logInputBlocked("SendInput(key)")
		return ErrInputBlocked
	}
	return nil
}

// isExtendedVK reports whether the key's hardware scancode carries the E0
// prefix (KEYEVENTF_EXTENDEDKEY). Without it, arrows/Ins/Del/Home/End/PgUp/
// PgDn resolve to their numpad twins for scancode-aware consumers.
func isExtendedVK(vk uint16) bool {
	switch vk {
	case 0x21, 0x22, 0x23, 0x24, // PageUp PageDown End Home
		0x25, 0x26, 0x27, 0x28, // arrows
		0x2C, 0x2D, 0x2E, // PrintScreen Insert Delete
		0x5B, 0x5C, 0x5D, // LWin RWin Apps
		0x90,       // NumLock
		0x6F,       // numpad divide
		0xA3, 0xA5: // RControl RAlt
		return true
	}
	return false
}

func (c *controller) TypeText(text string) error {
	encoded := utf16.Encode([]rune(text))
	if len(encoded) == 0 {
		return nil
	}

	// Send in chunks to avoid overflowing the system input queue.
	const chunkSize = 20 // characters per batch
	for off := 0; off < len(encoded); off += chunkSize {
		end := off + chunkSize
		if end > len(encoded) {
			end = len(encoded)
		}
		chunk := encoded[off:end]

		// 2 INPUT structs per character (key down + key up), sizeof(INPUT) = 40
		buf := make([]byte, len(chunk)*2*40)
		for i, wch := range chunk {
			down := buf[(i*2)*40 : (i*2+1)*40]
			up := buf[(i*2+1)*40 : (i*2+2)*40]

			*(*uint32)(unsafe.Pointer(&down[0])) = inputKeyboard
			*(*uint16)(unsafe.Pointer(&down[10])) = wch
			*(*uint32)(unsafe.Pointer(&down[12])) = kbfUnicode

			*(*uint32)(unsafe.Pointer(&up[0])) = inputKeyboard
			*(*uint16)(unsafe.Pointer(&up[10])) = wch
			*(*uint32)(unsafe.Pointer(&up[12])) = kbfUnicode | kbfKeyUp
		}

		n := uintptr(len(chunk) * 2)
		injected, _, _ := procSendInput.Call(n, uintptr(unsafe.Pointer(&buf[0])), 40)
		if injected != n {
			logInputBlocked("SendInput")
			return ErrInputBlocked
		}

		if end < len(encoded) {
			time.Sleep(5 * time.Millisecond)
		}
	}
	return nil
}

func buttonFlags(button string) (down, up uint32) {
	switch strings.ToLower(button) {
	case "right", "r":
		return mousefRightDown, mousefRightUp
	case "middle", "m":
		return mousefMiddleDown, mousefMiddleUp
	default:
		return mousefLeftDown, mousefLeftUp
	}
}

func keyToVK(key string) uint16 {
	if len(key) == 1 {
		ch := key[0]
		switch {
		case ch >= 'a' && ch <= 'z':
			return uint16(ch - 'a' + 0x41)
		case ch >= 'A' && ch <= 'Z':
			return uint16(ch - 'A' + 0x41)
		case ch >= '0' && ch <= '9':
			return uint16(ch)
		}
		switch ch {
		case ' ':
			return 0x20
		case ';':
			return 0xBA
		case '=':
			return 0xBB
		case ',':
			return 0xBC
		case '-':
			return 0xBD
		case '.':
			return 0xBE
		case '/':
			return 0xBF
		case '`':
			return 0xC0
		case '[':
			return 0xDB
		case '\\':
			return 0xDC
		case ']':
			return 0xDD
		case '\'':
			return 0xDE
		}
	}
	switch key {
	case "Backspace":
		return 0x08
	case "Tab":
		return 0x09
	case "Enter":
		return 0x0D
	case "Shift":
		return 0x10
	case "Control":
		return 0x11
	case "Alt":
		return 0x12
	case "Pause":
		return 0x13
	case "CapsLock":
		return 0x14
	case "Escape":
		return 0x1B
	case "Space":
		return 0x20
	case "PageUp":
		return 0x21
	case "PageDown":
		return 0x22
	case "End":
		return 0x23
	case "Home":
		return 0x24
	case "ArrowLeft":
		return 0x25
	case "ArrowUp":
		return 0x26
	case "ArrowRight":
		return 0x27
	case "ArrowDown":
		return 0x28
	case "PrintScreen":
		return 0x2C
	case "Insert":
		return 0x2D
	case "Delete":
		return 0x2E
	case "Meta":
		return 0x5B
	case "ContextMenu":
		return 0x5D
	case "F1":
		return 0x70
	case "F2":
		return 0x71
	case "F3":
		return 0x72
	case "F4":
		return 0x73
	case "F5":
		return 0x74
	case "F6":
		return 0x75
	case "F7":
		return 0x76
	case "F8":
		return 0x77
	case "F9":
		return 0x78
	case "F10":
		return 0x79
	case "F11":
		return 0x7A
	case "F12":
		return 0x7B
	case "NumLock":
		return 0x90
	case "ScrollLock":
		return 0x91
	}
	return 0
}
