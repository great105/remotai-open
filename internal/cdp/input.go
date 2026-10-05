package cdp

// Ввод в страницу настоящими событиями браузера.
//
// Раньше нажатие с телефона проходило путь: клиент → агент → xdotool → X-сервер
// → окно Chrome → страница. Каждое звено добавляло задержку, а координаты
// приходилось попадать в пиксель экрана, включая полосу вкладок и адресную
// строку самого браузера. Здесь событие рождается сразу в странице, в её
// собственных координатах (CSS-пиксели viewport, ровно те, в которых снят
// кадр), поэтому промахнуться нечем.
//
// Тип реализует input.Controller — тот же интерфейс, что и путь через X, так
// что верхний слой (очередь событий, «последний побеждает» для движений,
// предупреждения «ввод не доходит») остаётся неизменным.

import (
	"context"
	"strings"
	"time"
)

// Controller шлёт ввод в страницу через CDP.
type Controller struct {
	c *Client
}

// NewController — контроллер поверх подключения к вкладке.
func NewController(c *Client) *Controller { return &Controller{c: c} }

func (k *Controller) ctx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 3*time.Second)
}

func (k *Controller) mods() int {
	k.c.modMu.Lock()
	defer k.c.modMu.Unlock()
	return k.c.mods
}

func (k *Controller) mouse(kind string, x, y int, button string, clicks int) error {
	k.c.frameMu.Lock()
	k.c.pointerX, k.c.pointerY = x, y
	k.c.frameMu.Unlock()

	ctx, cancel := k.ctx()
	defer cancel()
	params := map[string]any{
		"type":      kind,
		"x":         x,
		"y":         y,
		"modifiers": k.mods(),
	}
	if button != "" {
		params["button"] = button
		params["buttons"] = buttonMask(button)
	}
	if clicks > 0 {
		params["clickCount"] = clicks
	}
	_, err := k.c.Call(ctx, "Input.dispatchMouseEvent", params)
	return err
}

func (k *Controller) MouseMove(x, y int) error {
	return k.mouse("mouseMoved", x, y, "none", 0)
}

func (k *Controller) MouseClick(x, y int, button string) error {
	b := cdpButton(button)
	if err := k.mouse("mousePressed", x, y, b, 1); err != nil {
		return err
	}
	return k.mouse("mouseReleased", x, y, b, 1)
}

func (k *Controller) MouseDoubleClick(x, y int) error {
	if err := k.MouseClick(x, y, "left"); err != nil {
		return err
	}
	// Второй щелчок с clickCount=2 — именно так страница отличает двойной
	// клик от двух одиночных.
	if err := k.mouse("mousePressed", x, y, "left", 2); err != nil {
		return err
	}
	return k.mouse("mouseReleased", x, y, "left", 2)
}

func (k *Controller) MouseDown(x, y int, button string) error {
	return k.mouse("mousePressed", x, y, cdpButton(button), 1)
}

func (k *Controller) MouseUp(x, y int, button string) error {
	return k.mouse("mouseReleased", x, y, cdpButton(button), 1)
}

// Scroll прокручивает страницу под курсором. Единица снаружи — «щелчок
// колеса», в браузере это ~100 CSS-пикселей.
func (k *Controller) Scroll(dy int) error {
	if dy == 0 {
		return nil
	}
	ctx, cancel := k.ctx()
	defer cancel()
	x, y := k.lastPointer()
	_, err := k.c.Call(ctx, "Input.dispatchMouseEvent", map[string]any{
		"type":      "mouseWheel",
		"x":         x,
		"y":         y,
		"deltaX":    0,
		"deltaY":    -dy * 100, // вверх у нас положительный, у страницы — отрицательный
		"modifiers": k.mods(),
	})
	return err
}

// lastPointer — где стоит указатель: колесо в CDP крутится в точке, а не «в
// активном окне», поэтому позицию нужно помнить.
func (k *Controller) lastPointer() (int, int) {
	k.c.frameMu.Lock()
	defer k.c.frameMu.Unlock()
	if k.c.pointerX == 0 && k.c.pointerY == 0 && k.c.lastFrame.Width > 0 {
		return k.c.lastFrame.Width / 2, k.c.lastFrame.Height / 2
	}
	return k.c.pointerX, k.c.pointerY
}

func (k *Controller) KeyDown(key string) error {
	if mod := modBit(key); mod != 0 {
		k.c.modMu.Lock()
		k.c.mods |= mod
		k.c.modMu.Unlock()
	}
	ctx, cancel := k.ctx()
	defer cancel()
	params := keyParams("keyDown", key, k.mods())
	_, err := k.c.Call(ctx, "Input.dispatchKeyEvent", params)
	return err
}

func (k *Controller) KeyUp(key string) error {
	ctx, cancel := k.ctx()
	defer cancel()
	params := keyParams("keyUp", key, k.mods())
	_, err := k.c.Call(ctx, "Input.dispatchKeyEvent", params)
	if mod := modBit(key); mod != 0 {
		k.c.modMu.Lock()
		k.c.mods &^= mod
		k.c.modMu.Unlock()
	}
	return err
}

// typeAsKeysLimit — до какой длины текст ПЕЧАТАЕТСЯ клавишами, а не вставляется
// целиком. Логин, пароль, код из СМС, поисковый запрос — это ввод, и страница
// должна увидеть его как ввод. Текст длиннее — это вставка из буфера, там
// посимвольная эмуляция только тормозила бы (и на настоящем телефоне вставка
// тоже приходит одним куском).
const typeAsKeysLimit = 64

// TypeText вводит текст в страницу.
//
// Раньше это всегда был Input.insertText — «вставка». Для показа он годится, но
// РЕГИСТРАЦИЯ на нём спотыкается: поля с посимвольной проверкой (код из СМС по
// одной цифре в клетке, маска телефона, счётчик длины пароля, подсказки адреса)
// слушают keydown/keypress, а вставка их не порождает — человек видел, как
// текст появляется, а кнопка «Далее» остаётся серой. Поэтому короткий текст
// печатается настоящими нажатиями, а вставкой остаётся только длинный.
func (k *Controller) TypeText(text string) error {
	if text == "" {
		return nil
	}
	runes := []rune(text)
	if len(runes) > typeAsKeysLimit {
		ctx, cancel := k.ctx()
		defer cancel()
		_, err := k.c.Call(ctx, "Input.insertText", map[string]any{"text": text})
		return err
	}
	for _, r := range runes {
		if err := k.typeRune(r); err != nil {
			return err
		}
	}
	return nil
}

// typeRune печатает один символ парой keyDown/keyUp. Перевод строки в тексте —
// это Enter: без него многострочная вставка в поле комментария превращалась бы
// в одну строку.
func (k *Controller) typeRune(r rune) error {
	if r == '\n' || r == '\r' {
		if err := k.KeyDown("Enter"); err != nil {
			return err
		}
		return k.KeyUp("Enter")
	}
	ctx, cancel := k.ctx()
	defer cancel()
	key := string(r)
	mods := k.mods()
	down := map[string]any{
		"type":           "keyDown",
		"key":            key,
		"text":           key,
		"unmodifiedText": key,
		"modifiers":      mods,
	}
	// Виртуальный код есть только у латиницы и цифр. Кириллице и эмодзи его
	// подставлять нечем — страница получит символ через text, как при вводе с
	// экранной клавиатуры телефона.
	if code, ok := virtualKeyCode(strings.ToLower(key)); ok {
		down["windowsVirtualKeyCode"] = code
		down["nativeVirtualKeyCode"] = code
	}
	if _, err := k.c.Call(ctx, "Input.dispatchKeyEvent", down); err != nil {
		return err
	}
	up := map[string]any{"type": "keyUp", "key": key, "modifiers": mods}
	if code, ok := down["windowsVirtualKeyCode"]; ok {
		up["windowsVirtualKeyCode"] = code
		up["nativeVirtualKeyCode"] = code
	}
	_, err := k.c.Call(ctx, "Input.dispatchKeyEvent", up)
	return err
}

// PasteText вставляет текст одним куском — это буфер обмена, а не набор.
func (k *Controller) PasteText(text string) error {
	if text == "" {
		return nil
	}
	ctx, cancel := k.ctx()
	defer cancel()
	_, err := k.c.Call(ctx, "Input.insertText", map[string]any{"text": text})
	return err
}

// keyParams собирает событие клавиши. Для печатных символов добавляем text —
// без него страница получит нажатие, но не получит сам символ.
func keyParams(kind, key string, mods int) map[string]any {
	params := map[string]any{
		"type":      kind,
		"key":       jsKeyName(key),
		"modifiers": mods,
	}
	if code, ok := virtualKeyCode(key); ok {
		params["windowsVirtualKeyCode"] = code
		params["nativeVirtualKeyCode"] = code
	}
	if runes := []rune(key); kind == "keyDown" && len(runes) == 1 && mods&(modCtrl|modMeta|modAlt) == 0 {
		params["text"] = key
		params["unmodifiedText"] = key
	}
	return params
}

// jsKeyName приводит имя клавиши к тому, что ждёт страница в KeyboardEvent.key.
func jsKeyName(key string) string {
	switch key {
	case "Space", " ":
		return " "
	}
	return key
}

// virtualKeyCode — код клавиши для страниц, которые смотрят на keyCode
// (устаревшее поле, но живого кода с ним много).
func virtualKeyCode(key string) (int, bool) {
	switch key {
	case "Backspace":
		return 8, true
	case "Tab":
		return 9, true
	case "Enter":
		return 13, true
	case "Shift":
		return 16, true
	case "Control":
		return 17, true
	case "Alt":
		return 18, true
	case "Pause":
		return 19, true
	case "CapsLock":
		return 20, true
	case "Escape":
		return 27, true
	case "Space", " ":
		return 32, true
	case "PageUp":
		return 33, true
	case "PageDown":
		return 34, true
	case "End":
		return 35, true
	case "Home":
		return 36, true
	case "ArrowLeft":
		return 37, true
	case "ArrowUp":
		return 38, true
	case "ArrowRight":
		return 39, true
	case "ArrowDown":
		return 40, true
	case "Insert":
		return 45, true
	case "Delete":
		return 46, true
	case "Meta":
		return 91, true
	}
	if len(key) == 2 && key[0] == 'F' && key[1] >= '1' && key[1] <= '9' { // F1–F9
		return 111 + int(key[1]-'0'), true
	}
	switch key {
	case "F10":
		return 121, true
	case "F11":
		return 122, true
	case "F12":
		return 123, true
	}
	if runes := []rune(key); len(runes) == 1 {
		r := runes[0]
		if r >= 'a' && r <= 'z' {
			return int(r - 32), true
		}
		if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			return int(r), true
		}
	}
	return 0, false
}

func modBit(key string) int {
	switch key {
	case "Control":
		return modCtrl
	case "Shift":
		return modShift
	case "Alt":
		return modAlt
	case "Meta":
		return modMeta
	}
	return 0
}

func cdpButton(button string) string {
	switch button {
	case "right", "r":
		return "right"
	case "middle", "m":
		return "middle"
	default:
		return "left"
	}
}

func buttonMask(button string) int {
	switch button {
	case "right":
		return 2
	case "middle":
		return 4
	case "none":
		return 0
	default:
		return 1
	}
}

// TouchPoints — касания страницы (реализует интерфейс, который ищет слой ввода).
// Точки приходят уже в координатах страницы: слой выше перевёл доли кадра.
func (k *Controller) TouchPoints(kind string, points [][2]float64) error {
	ctx, cancel := k.ctx()
	defer cancel()
	list := make([]TouchPoint, 0, len(points))
	for i, p := range points {
		list = append(list, TouchPoint{ID: i + 1, X: p[0], Y: p[1]})
	}
	if len(list) > 0 {
		// Помним последнюю точку: инерция после отрыва пальца досылается
		// прокруткой, а ей нужно место на странице.
		k.c.frameMu.Lock()
		k.c.pointerX, k.c.pointerY = int(list[0].X), int(list[0].Y)
		k.c.frameMu.Unlock()
	}
	return k.c.Touch(ctx, kind, list)
}
