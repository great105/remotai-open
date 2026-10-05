package input

import "errors"

// Sentinel errors every platform controller returns instead of a bare
// platform-specific message: the remote-desktop layer maps them to machine
// codes for the client ({t:"warn",code:"input_blocked"} / "input_unavailable").
// Before this, injection failures were swallowed by the callers, so a locked
// Windows desktop looked exactly like a broken program — taps did nothing and
// nothing on screen said why.
var (
	// ErrInputBlocked — the OS took the call but dropped the injection: the
	// desktop is locked or a UAC / secure-desktop prompt is up (UIPI). Only the
	// human at the physical machine can clear it.
	ErrInputBlocked = errors.New("input blocked (locked desktop / UIPI)")

	// ErrInputUnavailable — the injection tooling itself is missing or refuses
	// to work (Linux: no xdotool in PATH, no X server on DISPLAY). Different
	// remedy from ErrInputBlocked, so it must not be reported as "locked".
	ErrInputUnavailable = errors.New("input tooling unavailable")
)

// PrintableRune распознаёт имя клавиши, состоящее ровно из ОДНОГО символа вне
// ASCII: «ф», «ü», «ß». Такую клавишу нажать нечем — виртуального кода на
// Windows у неё нет (keyToVK → 0), а X keysym отличается от самого символа
// («Cyrillic_ef», не «ф»), поэтому нажатие терялось молча: клиент его уже
// погасил, а здесь не отправлялось ничего. Платформенные KeyDown вводят такой
// символ как ТЕКСТ. Имена вроде "Enter"/"F5"/"ArrowUp" сюда не попадают.
func PrintableRune(key string) (rune, bool) {
	runes := []rune(key)
	if len(runes) != 1 || runes[0] < 128 {
		return 0, false
	}
	return runes[0], true
}

// Controller provides cross-platform mouse and keyboard control.
type Controller interface {
	MouseMove(x, y int) error
	MouseClick(x, y int, button string) error
	MouseDoubleClick(x, y int) error
	MouseDown(x, y int, button string) error
	MouseUp(x, y int, button string) error
	Scroll(dy int) error
	KeyDown(key string) error
	KeyUp(key string) error
	TypeText(text string) error
}

// HorizontalScroller — необязательное умение: горизонтальное колесо. До
// 02.09.2026 протокол нёс только dy, и таблицы/таймлайны с телефона
// листались лишь по вертикали. Кто не умеет (виртуальный браузер, macOS) —
// просто не реализует; ScrollH тогда молча ничего не делает.
type HorizontalScroller interface {
	ScrollH(dx int) error
}

// ScrollH прокручивает по горизонтали, если контроллер умеет. Положительный
// dx — вправо.
func ScrollH(c Controller, dx int) error {
	if dx == 0 {
		return nil
	}
	if h, ok := c.(HorizontalScroller); ok {
		return h.ScrollH(dx)
	}
	return nil
}

// New creates a platform-specific Controller.
func New() Controller {
	return newController()
}
