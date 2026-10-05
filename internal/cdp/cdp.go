// Package cdp — минимальный клиент Chrome DevTools Protocol для виртуального
// браузера.
//
// Зачем он вообще нужен, если экран и так снимается:
//
//	X-путь на каждый кадр тянет с экрана W×H×4 байта (1280×800 — это 4 МБ через
//	сокет), масштабирует и кодирует JPEG силами Go. На типовом VPS с ОДНИМ ядром
//	этот конвейер соревнуется за процессор с самим браузером — то есть мы
//	отбираем у страницы ровно тот ресурс, ради показа которого работаем.
//
//	Chrome умеет отдавать кадры сам: Page.startScreencast присылает готовый
//	JPEG И ТОЛЬКО КОГДА картинка изменилась. Захвата нет, кодирования нет,
//	опроса нет — а кодек внутри браузера быстрее нашего.
//
// Второй выигрыш — ввод. Input.dispatch* порождает НАСТОЯЩИЕ события браузера
// в координатах страницы: не нужно ни синтетики через X, ни попадания курсором
// в пиксель. И третий — навигация: адрес, «назад», «вперёд», перезагрузка
// становятся вызовами, а не попыткой попасть пальцем в мелкую кнопку
// интерфейса самого браузера.
//
// Границы: это НЕ полноценная библиотека CDP, а ровно те методы, которые нужны
// удалённому экрану. Всё, что не получилось (нет порта отладки, браузер занят,
// вкладка закрылась), обязано приводить к откату на X-путь, а не к чёрному
// экрану — поэтому каждая ошибка здесь возвращается наверх, а не глушится.
package cdp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

// Frame — свежий кадр страницы: уже сжатый JPEG и размер, в котором он снят.
type Frame struct {
	JPEG   []byte
	Width  int // CSS-пиксели viewport (в них же приходят координаты ввода)
	Height int
	Seq    uint64
}

// pageTarget — вкладка, к которой можно подключиться.
type pageTarget struct {
	ID    string `json:"id"`
	Type  string `json:"type"`
	URL   string `json:"url"`
	Title string `json:"title"`
	WSURL string `json:"webSocketDebuggerUrl"`
}

// Client — подключение к одной вкладке.
type Client struct {
	endpoint string // "127.0.0.1:PORT"
	targetID string

	conn   *websocket.Conn
	sendMu sync.Mutex // одна запись за раз: gorilla этого требует

	nextID  atomic.Int64
	pending sync.Map // id -> chan rpcResult

	frameMu   sync.Mutex
	lastFrame Frame
	frameSeq  atomic.Uint64
	// Где стоит указатель — колесо в CDP крутится в точке, а не «в активном
	// окне»: без этой памяти прокрутка уходила бы всегда в центр экрана.
	pointerX int
	pointerY int

	// Состояние модификаторов для Input.dispatch*: CDP хочет их битовой маской
	// в каждом событии, а клавиатура присылает отдельные keydown/keyup.
	modMu sync.Mutex
	mods  int

	// Идёт ли поток кадров: подключение живёт и без него (управление вкладкой),
	// и включать поток дважды нельзя.
	streaming atomic.Bool

	// Профиль устройства, которым притворяется вкладка (см. emulate.go), и
	// признак «страница грузится» — из него растёт полоса загрузки в адресной
	// строке.
	device  atomic.Pointer[Device]
	loading atomic.Bool

	// Скрипт профиля устройства (stealth.go): его идентификатор нужен, чтобы
	// снять прошлый при смене вида сайта. Рядом — язык для Accept-Language и
	// настоящий User-Agent браузера (спрашивать его на каждое переприменение
	// профиля незачем, он не меняется до перезапуска браузера).
	stealthScript  atomic.Pointer[string]
	acceptLanguage atomic.Pointer[string]
	realUA         atomic.Pointer[string]

	// Последняя позиция прокрутки: её присылает наблюдатель страницы, и по ней
	// клиент решает, можно ли начинать «потянуть вниз, чтобы обновить».
	scrollY atomic.Int64

	// notify — заметные события страницы наружу (фокус в поле ввода, начало и
	// конец загрузки, переход). Клавиатура телефона открывается именно по
	// первому из них: без сигнала человек тапает в поле и ничего не происходит.
	notifyMu sync.Mutex
	notify   func(Notice)

	// Нативные окна Chrome не входят в Page.startScreencast: выбор файла,
	// alert/confirm/prompt человек иначе вообще не увидит. Перехватываем их и
	// держим ровно одно актуальное состояние каждого типа, пока интерфейс
	// Remotai не ответит отдельным API-вызовом.
	uiMu        sync.Mutex
	uiSeq       uint64
	fileChooser *FileChooser
	dialog      *JavaScriptDialog

	closed  atomic.Bool
	done    chan struct{}
	closeMu sync.Once
}

// Битовая маска модификаторов CDP.
const (
	modAlt   = 1
	modCtrl  = 2
	modMeta  = 4
	modShift = 8
)

// Connect подключается к первой странице браузера на endpoint. quality > 0
// включает поток кадров; quality <= 0 — только управление (навигация из
// адресной строки работает и тогда, когда никто не смотрит на экран, а гнать
// кадры в никуда — зря греть процессор сервера).
func Connect(ctx context.Context, endpoint string, quality, maxWidth, maxHeight int) (*Client, error) {
	return ConnectTarget(ctx, endpoint, "", quality, maxWidth, maxHeight)
}

// ConnectTarget подключается к конкретной вкладке (пустой id — к активной).
func ConnectTarget(ctx context.Context, endpoint, targetID string, quality, maxWidth, maxHeight int) (*Client, error) {
	target, err := pickPage(ctx, endpoint, targetID)
	if err != nil {
		return nil, err
	}
	dialer := websocket.Dialer{HandshakeTimeout: 5 * time.Second, ReadBufferSize: 1 << 16}
	conn, _, err := dialer.DialContext(ctx, target.WSURL, nil)
	if err != nil {
		return nil, fmt.Errorf("cdp dial: %w", err)
	}
	// Кадр страницы целиком приходит одним сообщением — на большом экране это
	// сотни килобайт, поэтому предел чтения поднимаем явно.
	conn.SetReadLimit(32 << 20)

	c := &Client{endpoint: endpoint, targetID: target.ID, conn: conn, done: make(chan struct{})}
	go c.readLoop()

	if _, err := c.Call(ctx, "Page.enable", nil); err != nil {
		c.Close()
		return nil, err
	}
	// Нативный picker Chrome рисуется вне страницы и в CDP-кадр не попадает.
	// Intercept заменяет его событием Page.fileChooserOpened; DOM нужен, чтобы
	// затем назначить выбранные на телефоне файлы настоящему <input type=file>.
	if _, err := c.Call(ctx, "DOM.enable", nil); err != nil {
		log.Printf("[CDP] DOM не включился — выбор файлов будет недоступен: %v", err)
	}
	if _, err := c.Call(ctx, "Page.setInterceptFileChooserDialog", map[string]any{
		"enabled": true,
	}); err != nil {
		log.Printf("[CDP] перехват выбора файлов не включился: %v", err)
	}
	// Наблюдатель за полями ввода — не роскошь: без него клавиатура телефона
	// не открывается по тапу, и «настоящий браузер» рассыпается на первом же
	// поле поиска. Неудача здесь не повод рвать подключение.
	if err := c.watchFocus(ctx); err != nil {
		log.Printf("[CDP] наблюдатель фокуса не включился: %v", err)
	}
	if quality > 0 {
		if err := c.StartScreencast(ctx, quality, maxWidth, maxHeight); err != nil {
			c.Close()
			return nil, err
		}
		c.streaming.Store(true)
	}
	return c, nil
}

// firstPage возвращает вкладку, на которую стоит смотреть. Chrome держит в
// /json/list сначала активные вкладки; devtools-страницы и служебные цели
// пропускаем.
func pickPage(ctx context.Context, endpoint, wantID string) (pageTarget, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+endpoint+"/json/list", nil)
	if err != nil {
		return pageTarget{}, err
	}
	resp, err := (&http.Client{Timeout: 3 * time.Second}).Do(req)
	if err != nil {
		return pageTarget{}, fmt.Errorf("cdp list: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return pageTarget{}, err
	}
	var targets []pageTarget
	if err := json.Unmarshal(body, &targets); err != nil {
		return pageTarget{}, fmt.Errorf("cdp list parse: %w", err)
	}
	var first pageTarget
	for _, t := range targets {
		if t.Type != "page" || t.WSURL == "" {
			continue
		}
		if wantID != "" && t.ID == wantID {
			return t, nil
		}
		if first.ID == "" {
			first = t
		}
	}
	// Выбранную вкладку закрыли — показываем любую оставшуюся, а не пустой
	// экран: для человека это то же самое, что закрыть вкладку в браузере.
	if first.ID != "" {
		return first, nil
	}
	return pageTarget{}, fmt.Errorf("cdp: открытых вкладок нет")
}

// TargetID — вкладка, к которой подключены (снаружи следят, не сменилась ли).
func (c *Client) TargetID() string { return c.targetID }

// Alive — соединение ещё живо.
func (c *Client) Alive() bool { return !c.closed.Load() }

type rpcMessage struct {
	ID     int64           `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

type rpcResult struct {
	raw json.RawMessage
	err error
}

// readLoop разбирает ответы и события. Кадры складывает «последний побеждает»:
// показать всё равно можно только самый свежий, а копить их — значит показывать
// прошлое.
func (c *Client) readLoop() {
	defer c.markClosed()
	for {
		_, data, err := c.conn.ReadMessage()
		if err != nil {
			return
		}
		var msg rpcMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			continue
		}
		if msg.ID != 0 {
			if ch, ok := c.pending.LoadAndDelete(msg.ID); ok {
				result := rpcResult{raw: msg.Result}
				if msg.Error != nil {
					result.err = fmt.Errorf("cdp: %s", msg.Error.Message)
				}
				select {
				case ch.(chan rpcResult) <- result:
				default:
				}
			}
			continue
		}
		switch msg.Method {
		case "Page.screencastFrame":
			c.onScreencastFrame(msg.Params)
		case "Page.frameStartedLoading":
			c.loading.Store(true)
			c.emit(Notice{Kind: "loading"})
		case "Page.frameStoppedLoading", "Page.loadEventFired":
			c.loading.Store(false)
			// Профиль ставим и здесь, а не только на переходе: ПЕРВЫЙ сайт
			// открывается в новом процессе отрисовки, и настройка, сделанная
			// на пустой вкладке, до него не доезжает — замер показывал
			// maxTouchPoints=0 ровно на первой странице и 1 на всех
			// последующих. Повтор дешёвый, а первая страница — та самая, по
			// которой человек судит, «телефон» перед ним или «компьютер».
			c.reapplyDevice(nil)
			c.emit(Notice{Kind: "loaded"})
		case "Page.frameNavigated":
			c.reapplyDevice(msg.Params)
			c.emit(Notice{Kind: "navigated"})
		case "Page.navigatedWithinDocument":
			// Переход внутри приложения-страницы (history.pushState): документ
			// тот же, профиль переприменять не надо, но адрес и кнопка «назад»
			// изменились — без этого адресная строка показывала бы прошлый
			// раздел сайта, пока человек не перезагрузит его руками.
			c.emit(Notice{Kind: "navigated"})
		case "Runtime.bindingCalled":
			c.onBinding(msg.Params)
		case "Browser.downloadWillBegin":
			var p struct {
				SuggestedFilename string `json:"suggestedFilename"`
			}
			if json.Unmarshal(msg.Params, &p) == nil {
				c.emit(Notice{Kind: "download", File: p.SuggestedFilename})
			}
		case "Page.fileChooserOpened":
			c.onFileChooserOpened(msg.Params)
		case "Page.javascriptDialogOpening":
			c.onJavaScriptDialogOpening(msg.Params)
		case "Page.javascriptDialogClosed":
			c.onJavaScriptDialogClosed()
		case "Page.windowOpen":
			var p struct {
				URL string `json:"url"`
			}
			if json.Unmarshal(msg.Params, &p) == nil {
				c.emit(Notice{Kind: "popup", URL: limitRunes(p.URL, 2048)})
			}
		}
	}
}

type screencastParams struct {
	Data      string `json:"data"`
	SessionID int    `json:"sessionId"`
	Metadata  struct {
		DeviceWidth  float64 `json:"deviceWidth"`
		DeviceHeight float64 `json:"deviceHeight"`
	} `json:"metadata"`
}

func (c *Client) onScreencastFrame(raw json.RawMessage) {
	var p screencastParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return
	}
	// Подтверждение обязательно: без него Chrome не пришлёт следующий кадр.
	// Отправляем ДО декодирования — так поток не ждёт нашей работы.
	go func(session int) {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_, _ = c.Call(ctx, "Page.screencastFrameAck", map[string]any{"sessionId": session})
	}(p.SessionID)

	jpegData, err := base64.StdEncoding.DecodeString(p.Data)
	if err != nil || len(jpegData) == 0 {
		return
	}
	seq := c.frameSeq.Add(1)
	c.frameMu.Lock()
	c.lastFrame = Frame{
		JPEG:   jpegData,
		Width:  int(p.Metadata.DeviceWidth),
		Height: int(p.Metadata.DeviceHeight),
		Seq:    seq,
	}
	c.frameMu.Unlock()
}

// FileChooser — перехваченный <input type=file>.
type FileChooser struct {
	ID            uint64
	BackendNodeID int64
	Mode          string
}

// JavaScriptDialog — alert/confirm/prompt/beforeunload, который Chrome обычно
// рисует своим нативным окном (его нет в кадре страницы).
type JavaScriptDialog struct {
	ID            uint64
	Type          string
	Message       string
	DefaultPrompt string
	URL           string
}

// Notice — событие страницы, которое видит интерфейс.
type Notice struct {
	Kind          string  // focus/scroll/loading/loaded/navigated/download/file_chooser/file_chooser_closed/dialog/dialog_closed/popup
	ScrollY       float64 // для "scroll": положение прокрутки страницы
	Editable      bool    // для "focus": курсор встал в поле ввода
	File          string  // для "download": имя скачанного файла
	ID            uint64  // file_chooser/dialog
	Multiple      bool    // file_chooser
	DialogType    string  // dialog: alert/confirm/prompt/beforeunload
	Message       string  // dialog
	DefaultPrompt string  // dialog prompt
	URL           string  // dialog/popup
}

func limitRunes(value string, max int) string {
	r := []rune(value)
	if len(r) <= max {
		return value
	}
	return string(r[:max])
}

func (c *Client) nextUIIDLocked() uint64 {
	c.uiSeq++
	return c.uiSeq
}

func (c *Client) onFileChooserOpened(raw json.RawMessage) {
	var p struct {
		Mode          string `json:"mode"`
		BackendNodeID int64  `json:"backendNodeId"`
	}
	if err := json.Unmarshal(raw, &p); err != nil || p.BackendNodeID == 0 {
		return
	}
	c.uiMu.Lock()
	chooser := &FileChooser{
		ID:            c.nextUIIDLocked(),
		BackendNodeID: p.BackendNodeID,
		Mode:          p.Mode,
	}
	c.fileChooser = chooser
	c.uiMu.Unlock()
	c.emit(Notice{
		Kind:     "file_chooser",
		ID:       chooser.ID,
		Multiple: chooser.Mode == "selectMultiple",
	})
}

func (c *Client) onJavaScriptDialogOpening(raw json.RawMessage) {
	var p struct {
		Type          string `json:"type"`
		Message       string `json:"message"`
		DefaultPrompt string `json:"defaultPrompt"`
		URL           string `json:"url"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return
	}
	c.uiMu.Lock()
	dialog := &JavaScriptDialog{
		ID:            c.nextUIIDLocked(),
		Type:          p.Type,
		Message:       limitRunes(p.Message, 4000),
		DefaultPrompt: limitRunes(p.DefaultPrompt, 4000),
		URL:           limitRunes(p.URL, 2048),
	}
	c.dialog = dialog
	c.uiMu.Unlock()
	c.emit(Notice{
		Kind:          "dialog",
		ID:            dialog.ID,
		DialogType:    dialog.Type,
		Message:       dialog.Message,
		DefaultPrompt: dialog.DefaultPrompt,
		URL:           dialog.URL,
	})
}

func (c *Client) onJavaScriptDialogClosed() {
	c.uiMu.Lock()
	var id uint64
	if c.dialog != nil {
		id = c.dialog.ID
	}
	c.dialog = nil
	c.uiMu.Unlock()
	c.emit(Notice{Kind: "dialog_closed", ID: id})
}

// SetFileInputFiles завершает перехваченный выбор файла настоящим CDP-вызовом.
// chooserID не даёт позднему ответу от старого шита назначить файлы уже другому
// полю на странице.
func (c *Client) SetFileInputFiles(ctx context.Context, chooserID uint64, files []string) error {
	c.uiMu.Lock()
	chooser := c.fileChooser
	if chooser == nil || chooser.ID != chooserID {
		c.uiMu.Unlock()
		return fmt.Errorf("cdp: выбор файла уже закрыт")
	}
	backendNodeID := chooser.BackendNodeID
	mode := chooser.Mode
	c.uiMu.Unlock()
	if mode != "selectMultiple" && len(files) > 1 {
		return fmt.Errorf("cdp: это поле принимает только один файл")
	}

	if _, err := c.Call(ctx, "DOM.setFileInputFiles", map[string]any{
		"files":         files,
		"backendNodeId": backendNodeID,
	}); err != nil {
		return err
	}
	c.uiMu.Lock()
	if c.fileChooser != nil && c.fileChooser.ID == chooserID {
		c.fileChooser = nil
	}
	c.uiMu.Unlock()
	c.emit(Notice{Kind: "file_chooser_closed", ID: chooserID})
	return nil
}

// CancelFileChooser закрывает наш перехваченный picker. Нативного окна уже нет
// (его подавил setInterceptFileChooserDialog), поэтому CDP-команда не нужна.
func (c *Client) CancelFileChooser(chooserID uint64) error {
	c.uiMu.Lock()
	if c.fileChooser == nil || c.fileChooser.ID != chooserID {
		c.uiMu.Unlock()
		return fmt.Errorf("cdp: выбор файла уже закрыт")
	}
	c.fileChooser = nil
	c.uiMu.Unlock()
	c.emit(Notice{Kind: "file_chooser_closed", ID: chooserID})
	return nil
}

// HandleJavaScriptDialog отвечает на alert/confirm/prompt/beforeunload.
func (c *Client) HandleJavaScriptDialog(
	ctx context.Context,
	dialogID uint64,
	accept bool,
	promptText string,
) error {
	c.uiMu.Lock()
	dialog := c.dialog
	if dialog == nil || dialog.ID != dialogID {
		c.uiMu.Unlock()
		return fmt.Errorf("cdp: диалог уже закрыт")
	}
	c.uiMu.Unlock()

	params := map[string]any{"accept": accept}
	if dialog.Type == "prompt" {
		params["promptText"] = limitRunes(promptText, 4000)
	}
	if _, err := c.Call(ctx, "Page.handleJavaScriptDialog", params); err != nil {
		return err
	}
	// Page.javascriptDialogClosed несёт единственный надёжный момент закрытия.
	// Не чистим состояние раньше него: обработчик страницы может немедленно
	// открыть следующий dialog, и запоздалое closed старого не должно стереть
	// уже новое окно.
	return nil
}

// SetNotify задаёт получателя событий страницы.
func (c *Client) SetNotify(fn func(Notice)) {
	c.notifyMu.Lock()
	c.notify = fn
	c.notifyMu.Unlock()
}

func (c *Client) emit(n Notice) {
	c.notifyMu.Lock()
	fn := c.notify
	c.notifyMu.Unlock()
	if fn != nil {
		fn(n)
	}
}

// pageWatcher — крошечный наблюдатель, подсаживаемый в каждую страницу. Он
// сообщает наружу два факта: курсор встал в поле ввода (по нему приложение
// открывает клавиатуру телефона само — как в настоящем браузере) и где сейчас
// прокрутка.
//
// Прокрутка нужна жестам: «потянуть вниз, чтобы обновить» обязан срабатывать
// ТОЛЬКО у самого верха страницы, иначе он крал бы обычное листание. Спрашивать
// об этом отдельным запросом в момент жеста нельзя — через облако это лишние
// сотни миллисекунд ровно там, где палец ждёт мгновенного отклика.
const pageWatcher = `(() => {
  if (window.__remotaiFocus) return;
  window.__remotaiFocus = true;
  const editable = (el) => {
    if (!el) return false;
    const tag = (el.tagName || "").toLowerCase();
    if (tag === "textarea" || tag === "select") return true;
    if (tag === "input") {
      const t = (el.type || "text").toLowerCase();
      return !["button","submit","reset","checkbox","radio","file","image","range","color"].includes(t);
    }
    return el.isContentEditable === true;
  };
  const send = (payload) => { try { window.remotaiFocus(payload); } catch (e) {} };
  document.addEventListener("focusin", (e) => send(editable(e.target) ? "1" : "0"), true);
  document.addEventListener("focusout", () => send("0"), true);
  if (editable(document.activeElement)) send("1");

  let last = -1, timer = 0;
  const report = () => {
    timer = 0;
    const y = Math.round(window.scrollY || 0);
    if (y === last) return;
    last = y;
    send("y:" + y);
  };
  document.addEventListener("scroll", () => {
    if (timer) return;
    timer = setTimeout(report, 120);
  }, true);
  report();
})()`

// watchFocus включает наблюдателя за страницей: и для будущих страниц, и для
// той, что открыта прямо сейчас (addScriptToEvaluateOnNewDocument срабатывает
// только со следующей навигации).
func (c *Client) watchFocus(ctx context.Context) error {
	if _, err := c.Call(ctx, "Runtime.enable", nil); err != nil {
		return err
	}
	if _, err := c.Call(ctx, "Runtime.addBinding", map[string]any{"name": "remotaiFocus"}); err != nil {
		return err
	}
	if _, err := c.Call(ctx, "Page.addScriptToEvaluateOnNewDocument", map[string]any{
		"source": pageWatcher,
	}); err != nil {
		return err
	}
	_, err := c.Call(ctx, "Runtime.evaluate", map[string]any{
		"expression": pageWatcher, "returnByValue": true,
	})
	return err
}

// reapplyDevice возвращает профиль телефона после перехода на новый сайт.
//
// Эмуляция живёт у ДОКУМЕНТА, а не у вкладки: открыв другой сайт (тем более в
// новом процессе рендерера), страница снова считает себя десктопной. Замер на
// живой машине это и показал: сразу после включения профиля
// navigator.maxTouchPoints равен единице, а после перехода — нулю. Поэтому
// профиль ставим заново на каждой навигации главного фрейма.
func (c *Client) reapplyDevice(raw json.RawMessage) {
	dev := c.device.Load()
	if dev == nil || dev.Name == "desktop" {
		return
	}
	// raw == nil — вызов не из события фрейма (например, после загрузки
	// страницы), проверять родителя нечего.
	if raw != nil {
		var p struct {
			Frame struct {
				ParentID string `json:"parentId"`
			} `json:"frame"`
		}
		if err := json.Unmarshal(raw, &p); err != nil || p.Frame.ParentID != "" {
			return // вложенный фрейм — у него своего профиля нет
		}
	}
	go func(d Device) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := c.Emulate(ctx, d); err != nil {
			log.Printf("[CDP] профиль устройства после перехода: %v", err)
		}
	}(*dev)
}

func (c *Client) onBinding(raw json.RawMessage) {
	var p struct {
		Name    string `json:"name"`
		Payload string `json:"payload"`
	}
	if err := json.Unmarshal(raw, &p); err != nil || p.Name != "remotaiFocus" {
		return
	}
	if y, ok := strings.CutPrefix(p.Payload, "y:"); ok {
		value, err := strconv.ParseFloat(y, 64)
		if err != nil {
			return
		}
		c.scrollY.Store(int64(value))
		c.emit(Notice{Kind: "scroll", ScrollY: value})
		return
	}
	c.emit(Notice{Kind: "focus", Editable: p.Payload == "1"})
}

// ScrollY — последняя известная позиция прокрутки страницы (её присылает
// наблюдатель). Нужна жестам, которые обязаны срабатывать только у верха.
func (c *Client) ScrollY() float64 { return float64(c.scrollY.Load()) }

// LastFrame возвращает самый свежий кадр (ok=false — кадров ещё не было).
func (c *Client) LastFrame() (Frame, bool) {
	c.frameMu.Lock()
	defer c.frameMu.Unlock()
	if c.lastFrame.Seq == 0 || len(c.lastFrame.JPEG) == 0 {
		return Frame{}, false
	}
	return c.lastFrame, true
}

// Call отправляет команду и ждёт ответа.
func (c *Client) Call(ctx context.Context, method string, params map[string]any) (json.RawMessage, error) {
	if c.closed.Load() {
		return nil, fmt.Errorf("cdp: соединение закрыто")
	}
	id := c.nextID.Add(1)
	payload := map[string]any{"id": id, "method": method}
	if params != nil {
		payload["params"] = params
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	ch := make(chan rpcResult, 1)
	c.pending.Store(id, ch)
	defer c.pending.Delete(id)

	c.sendMu.Lock()
	_ = c.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	err = c.conn.WriteMessage(websocket.TextMessage, data)
	c.sendMu.Unlock()
	if err != nil {
		c.markClosed()
		return nil, err
	}

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.done:
		return nil, fmt.Errorf("cdp: соединение закрыто")
	case res := <-ch:
		return res.raw, res.err
	}
}

// Streaming — идёт ли сейчас поток кадров.
func (c *Client) Streaming() bool { return c.streaming.Load() }

// StopScreencast гасит поток, оставляя подключение для управления.
func (c *Client) StopScreencast(ctx context.Context) error {
	if !c.streaming.Swap(false) {
		return nil
	}
	_, err := c.Call(ctx, "Page.stopScreencast", nil)
	return err
}

// StartScreencast включает поток кадров.
func (c *Client) StartScreencast(ctx context.Context, quality, maxWidth, maxHeight int) error {
	params := map[string]any{"format": "jpeg", "everyNthFrame": 1}
	if quality > 0 {
		params["quality"] = quality
	}
	if maxWidth > 0 {
		params["maxWidth"] = maxWidth
	}
	if maxHeight > 0 {
		params["maxHeight"] = maxHeight
	}
	if _, err := c.Call(ctx, "Page.startScreencast", params); err != nil {
		return err
	}
	c.streaming.Store(true)
	return nil
}

// Retune меняет качество/размер потока: Chrome принимает их только при старте,
// поэтому перезапускаем поток. Зовётся редко — на смену профиля связи.
func (c *Client) Retune(ctx context.Context, quality, maxWidth, maxHeight int) error {
	if _, err := c.Call(ctx, "Page.stopScreencast", nil); err != nil {
		return err
	}
	return c.StartScreencast(ctx, quality, maxWidth, maxHeight)
}

// Navigate открывает адрес в текущей вкладке.
func (c *Client) Navigate(ctx context.Context, url string) error {
	_, err := c.Call(ctx, "Page.navigate", map[string]any{"url": url})
	return err
}

// Reload перезагружает страницу.
func (c *Client) Reload(ctx context.Context) error {
	_, err := c.Call(ctx, "Page.reload", nil)
	return err
}

// history — ответ Page.getNavigationHistory.
type history struct {
	CurrentIndex int `json:"currentIndex"`
	Entries      []struct {
		ID int `json:"id"`
	} `json:"entries"`
}

// Go шагает по истории вкладки: delta=-1 назад, +1 вперёд.
func (c *Client) Go(ctx context.Context, delta int) error {
	raw, err := c.Call(ctx, "Page.getNavigationHistory", nil)
	if err != nil {
		return err
	}
	var h history
	if err := json.Unmarshal(raw, &h); err != nil {
		return err
	}
	idx := h.CurrentIndex + delta
	if idx < 0 || idx >= len(h.Entries) {
		return nil // идти некуда — это не ошибка
	}
	_, err = c.Call(ctx, "Page.navigateToHistoryEntry", map[string]any{"entryId": h.Entries[idx].ID})
	return err
}

// URL — адрес текущей страницы (для адресной строки в интерфейсе).
func (c *Client) URL(ctx context.Context) string {
	raw, err := c.Call(ctx, "Page.getNavigationHistory", nil)
	if err != nil {
		return ""
	}
	var h struct {
		CurrentIndex int `json:"currentIndex"`
		Entries      []struct {
			URL string `json:"url"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(raw, &h); err != nil {
		return ""
	}
	if h.CurrentIndex >= 0 && h.CurrentIndex < len(h.Entries) {
		return h.Entries[h.CurrentIndex].URL
	}
	return ""
}

func (c *Client) markClosed() {
	c.closed.Store(true)
	c.closeMu.Do(func() {
		// При смене вкладки/переподключении старые нативные окна больше нельзя
		// обслужить этим CDP-клиентом. Явно убираем их у всех зрителей, иначе
		// шит остаётся поверх уже новой страницы и любое действие отвечает
		// «выбор уже закрыт».
		c.uiMu.Lock()
		var chooserID, dialogID uint64
		if c.fileChooser != nil {
			chooserID = c.fileChooser.ID
		}
		if c.dialog != nil {
			dialogID = c.dialog.ID
		}
		c.fileChooser = nil
		c.dialog = nil
		c.uiMu.Unlock()
		close(c.done)
		if chooserID != 0 {
			c.emit(Notice{Kind: "file_chooser_closed", ID: chooserID})
		}
		if dialogID != 0 {
			c.emit(Notice{Kind: "dialog_closed", ID: dialogID})
		}
	})
}

// Close закрывает соединение (браузер при этом продолжает работать).
func (c *Client) Close() {
	c.markClosed()
	_ = c.conn.Close()
}
