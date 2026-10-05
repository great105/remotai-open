//go:build darwin

// Ввод мыши и клавиатуры на macOS — ЗАГЛУШКА до этапа Remote Desktop.
//
// Настоящая реализация — CGEvent (CoreGraphics): создать событие, задать
// координаты/код клавиши, отправить в системную очередь. Делать её сейчас
// незачем: ввод нужен ТОЛЬКО удалённому экрану, а экран на маке требует
// отдельной большой работы (захват через ScreenCaptureKit + бандл .app, к
// которому macOS привяжет разрешения Screen Recording и Accessibility). Пока
// удалёнки нет, честная ошибка лучше молчаливого «нажатие ушло в никуда»:
// клиент по ErrInputUnavailable покажет «управление недоступно», а не будет
// делать вид, что кликнул.
//
// Терминалы, файлы, мониторинг и SSH ввода не касаются и работают полностью.
package input

type darwinController struct{}

func newController() Controller { return &darwinController{} }

// CursorPos — где сейчас курсор. Пока ввода нет, отвечаем «не знаем» (третьим
// значением false), как и Linux-вариант: удалённый экран по нему рисует свой
// курсор только там, где система его реально сообщает.
func CursorPos() (int, int, bool) { return 0, 0, false }

func (c *darwinController) MouseMove(x, y int) error                 { return ErrInputUnavailable }
func (c *darwinController) MouseClick(x, y int, button string) error { return ErrInputUnavailable }
func (c *darwinController) MouseDoubleClick(x, y int) error          { return ErrInputUnavailable }
func (c *darwinController) MouseDown(x, y int, button string) error  { return ErrInputUnavailable }
func (c *darwinController) MouseUp(x, y int, button string) error    { return ErrInputUnavailable }
func (c *darwinController) Scroll(dy int) error                      { return ErrInputUnavailable }
func (c *darwinController) KeyDown(key string) error                 { return ErrInputUnavailable }
func (c *darwinController) KeyUp(key string) error                   { return ErrInputUnavailable }
func (c *darwinController) TypeText(text string) error               { return ErrInputUnavailable }
