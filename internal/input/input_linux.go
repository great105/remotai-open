//go:build linux

package input

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// xdotool wraps every injection call and normalises the failure into
// ErrInputUnavailable: a headless box without xdotool (or without a reachable
// X server on DISPLAY) used to swallow the error, so the remote-desktop viewer
// tapped into the void with nothing on screen explaining why. The wrapped
// message keeps the concrete cause for the agent log.
func xdotool(args ...string) error {
	out, err := exec.Command("xdotool", args...).CombinedOutput()
	if err == nil {
		return nil
	}
	detail := strings.TrimSpace(string(out))
	if detail == "" {
		detail = err.Error()
	}
	// Неизвестное имя клавиши — это кривой запрос клиента, а не сломанный ввод
	// на машине: такое НЕ должно поднимать у человека плашку «ввод не доходит»
	// (см. jsToX: многосимвольные имена вне таблицы xdotool отвергает).
	if strings.Contains(detail, "No such key name") || strings.Contains(detail, "Unknown key") {
		return fmt.Errorf("xdotool %s: %s", strings.Join(args, " "), detail)
	}
	return fmt.Errorf("%w: xdotool %s: %s", ErrInputUnavailable, strings.Join(args, " "), detail)
}

type controller struct{}

func newController() *controller { return &controller{} }

// CursorPos is not implemented on Linux (an xdotool round-trip per poll isn't
// worth it); callers treat ok=false as "no host-cursor echo".
func CursorPos() (int, int, bool) { return 0, 0, false }

// Каждое действие уходит ОДНОЙ посылкой в живой xdotool (см. xdotool_linux.go):
// клик — это «переставить курсор и нажать», и раньше он стоил двух запусков
// процесса, между которыми успевала вклиниться чужая команда. Теперь обе строки
// уходят вместе, по одной трубе, в исходном порядке.

func (c *controller) MouseMove(x, y int) error {
	return pipe.run(moveCmd(x, y))
}

func (c *controller) MouseClick(x, y int, button string) error {
	return pipe.run(moveCmd(x, y), "click "+xButton(button))
}

func (c *controller) MouseDoubleClick(x, y int) error {
	return pipe.run(moveCmd(x, y), "click --repeat 2 --delay 50 1")
}

func (c *controller) MouseDown(x, y int, button string) error {
	return pipe.run(moveCmd(x, y), "mousedown "+xButton(button))
}

func (c *controller) MouseUp(x, y int, button string) error {
	return pipe.run(moveCmd(x, y), "mouseup "+xButton(button))
}

func (c *controller) Scroll(dy int) error {
	button := "4" // scroll up
	n := dy
	if dy < 0 {
		button, n = "5", -dy // scroll down
	}
	if n <= 0 {
		return nil
	}
	// Один --repeat вместо n отдельных кликов: прокрутка на 15 «щелчков»
	// стоила пятнадцати запусков процесса и отставала от пальца на секунды.
	return pipe.run(fmt.Sprintf("click --repeat %d --delay 12 %s", n, button))
}

// ScrollH — горизонтальное колесо X11: кнопка 6 влево, 7 вправо.
func (c *controller) ScrollH(dx int) error {
	button := "7" // вправо
	n := dx
	if dx < 0 {
		button, n = "6", -dx
	}
	if n <= 0 {
		return nil
	}
	return pipe.run(fmt.Sprintf("click --repeat %d --delay 12 %s", n, button))
}

// moveCmd — «переставить курсор» одной строкой для трубы xdotool.
func moveCmd(x, y int) string {
	return "mousemove " + strconv.Itoa(x) + " " + strconv.Itoa(y)
}

func (c *controller) KeyDown(key string) error {
	// Буква не с латинской раскладки: у неё свой X keysym («Cyrillic_ef», а не
	// «ф»), и xdotool keydown такую клавишу не находит — нажатие пропадало
	// молча. Вводим символ ТЕКСТОМ, xdotool type это умеет.
	if r, ok := PrintableRune(key); ok {
		return c.TypeText(string(r))
	}
	k := jsToX(key)
	if k == "" {
		return nil
	}
	return pipe.run("keydown " + k)
}

func (c *controller) KeyUp(key string) error {
	// Символ уже введён целиком в KeyDown — «отпускать» нечего.
	if _, ok := PrintableRune(key); ok {
		return nil
	}
	k := jsToX(key)
	if k == "" {
		return nil
	}
	return pipe.run("keyup " + k)
}

// TypeText остаётся отдельным процессом: xdotool разбирает строку stdin
// пробелами, поэтому текст с пробелами и переводами строк через трубу не
// передать. Операция редкая (вставка из буфера), скорость здесь не решает.
func (c *controller) TypeText(text string) error {
	return xdotool("type", "--clearmodifiers", text)
}

func xButton(button string) string {
	switch strings.ToLower(button) {
	case "right", "r":
		return "3"
	case "middle", "m":
		return "2"
	default:
		return "1"
	}
}

func jsToX(key string) string {
	switch key {
	case "Enter":
		return "Return"
	case "Backspace":
		return "BackSpace"
	case "Tab":
		return "Tab"
	case "Escape":
		return "Escape"
	case "Delete":
		return "Delete"
	case "Insert":
		return "Insert"
	case "Home":
		return "Home"
	case "End":
		return "End"
	case "PageUp":
		return "Prior"
	case "PageDown":
		return "Next"
	case "ArrowLeft":
		return "Left"
	case "ArrowRight":
		return "Right"
	case "ArrowUp":
		return "Up"
	case "ArrowDown":
		return "Down"
	case "Control":
		return "Control_L"
	case "Shift":
		return "Shift_L"
	case "Alt":
		return "Alt_L"
	case "Meta":
		return "Super_L"
	case "CapsLock":
		return "Caps_Lock"
	case "PrintScreen":
		return "Print"
	case "ContextMenu":
		return "Menu"
	case "NumLock":
		return "Num_Lock"
	case "ScrollLock":
		return "Scroll_Lock"
	case "Pause":
		return "Pause"
	case "Space", " ":
		return "space"
	default:
		// F1–F12 и одиночные символы совпадают с X keysym как есть; прочие
		// многосимвольные имена xdotool отвергнет (ошибка Run() игнорируется).
		return key
	}
}
