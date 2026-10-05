package web

// Кадры и ввод виртуального браузера — через сам браузер, а не через экран.
//
// Пока картинка снималась с X-экрана, каждый кадр стоил копии всего экрана
// (W×H×4 байта через сокет) плюс JPEG силами Go. На сервере с одним ядром это
// ядро отбиралось у самого браузера — мы мешали работать тому, что показываем.
// Chrome умеет присылать кадры сам, уже сжатыми и только при изменении
// картинки; ввод он принимает настоящими событиями страницы (см. internal/cdp).
//
// Здесь — связка: живое подключение к вкладке, из которого стрим берёт готовый
// JPEG, а слой ввода — контроллер. Всё это СТРОГО необязательно: нет
// виртуального браузера, не поднялся порт отладки, закрылась вкладка — работает
// прежний путь через экран, без единого различия для клиента.

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/jpeg"
	"log"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"tgcontrol/internal/cdp"
	"tgcontrol/internal/input"
	"tgcontrol/internal/vbrowser"
)

// cdpRetryEvery — как часто пробовать подключиться заново. Чаще смысла нет:
// браузер поднимается секундами, а неудачная попытка стоит HTTP-запроса.
const cdpRetryEvery = 3 * time.Second

type cdpLink struct {
	mu       sync.Mutex
	client   *cdp.Client
	endpoint string
	lastTry  time.Time
	// Вкладка, которую человек выбрал смотреть, и профиль устройства: обе
	// вещи должны пережить переподключение (браузер перезапустили, вкладку
	// сменили) — иначе экран самовольно возвращается на первую вкладку и в
	// десктопный вид.
	wantTarget string
	device     *cdp.Device
	// Параметры, с которыми запущен поток кадров: смена профиля связи меняет
	// их, а Chrome принимает такие настройки только при старте потока.
	quality int
	maxW    int
	maxH    int
}

var browserLink cdpLink

// cdpClient возвращает живое подключение к вкладке виртуального браузера или
// nil. Подключение ленивое и самовосстанавливающееся: вкладку могли закрыть,
// браузер — перезапустить.
func cdpClient(quality, maxW, maxH int) *cdp.Client {
	endpoint := vbrowser.DebugEndpoint()
	if endpoint == "" {
		browserLink.drop()
		return nil
	}

	browserLink.mu.Lock()
	defer browserLink.mu.Unlock()

	if browserLink.client != nil && browserLink.client.Alive() && browserLink.endpoint == endpoint {
		browserLink.retuneLocked(quality, maxW, maxH)
		return browserLink.client
	}
	if browserLink.client != nil && browserLink.client.Alive() {
		// Браузер перезапустили — старое подключение больше ни к чему.
		browserLink.client.Close()
		browserLink.client = nil
	}
	if browserLink.client != nil {
		browserLink.client.Close()
		browserLink.client = nil
	}
	if time.Since(browserLink.lastTry) < cdpRetryEvery {
		return nil
	}
	browserLink.lastTry = time.Now()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, err := cdp.ConnectTarget(ctx, endpoint, browserLink.wantTarget, quality, maxW, maxH)
	if err != nil {
		// Это не авария: браузер мог ещё не открыть вкладку. Пишем один раз на
		// попытку и продолжаем снимать экран по-старому.
		log.Printf("[CDP] %s: %v — кадры идут через захват экрана", endpoint, err)
		return nil
	}
	log.Printf("[CDP] подключено к %s (вкладка %s)", endpoint, client.TargetID())
	client.SetNotify(publishBrowserNotice)
	// Скачивание в headless-браузере по умолчанию запрещено: человек жмёт
	// «Скачать» и не получает ничего. Кладём файлы в обычную папку загрузок —
	// оттуда их видно в файловом менеджере приложения.
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := client.EnableDownloads(ctx, downloadsDir()); err != nil {
			log.Printf("[CDP] загрузки не включились: %v", err)
		}
	}()
	browserLink.client = client
	browserLink.endpoint = endpoint
	browserLink.quality, browserLink.maxW, browserLink.maxH = quality, maxW, maxH
	if browserLink.device == nil {
		// Профиль по умолчанию — ТЕЛЕФОН, а не рабочий стол. Виртуальный
		// браузер поднимают, чтобы смотреть на него с телефона; ждать, пока
		// клиент попросит мобильный вид, значит показать человеку десктопную
		// вёрстку на первом же сайте. Старые версии приложения профиля вообще
		// не запрашивают — для них это единственный способ получить мобильные
		// сайты. Размер каталожный: сайт, увидевший «Pixel 7» с экраном
		// 600×1222, знает, что его обманывают.
		dev := cdp.DeviceProfile("pixel7", 0, 0, 0)
		browserLink.device = &dev
	}
	// Часовой пояс по адресу сервера и язык заголовков — до профиля устройства:
	// Accept-Language уезжает вместе с User-Agent одним вызовом.
	env := serverBrowserEnv()
	if env.Locale != "" {
		client.SetAcceptLanguage(cdp.AcceptLanguageHeader(env.Locale))
	}
	go func(tz, locale string) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		client.ApplyEnvironment(ctx, tz, locale)
	}(env.Timezone, env.Locale)
	if dev := browserLink.device; dev != nil {
		// Профиль телефона переживает переподключение: без этого страница
		// после смены вкладки внезапно становилась десктопной.
		go func(d cdp.Device) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := client.Emulate(ctx, d); err != nil {
				log.Printf("[CDP] профиль устройства не применился: %v", err)
			}
		}(*dev)
	}
	return client
}

// retuneLocked приводит поток кадров к запрошенным параметрам: включает его,
// если смотрящий появился, и перезапускает при смене качества/размера. Порог по
// размеру намеренно грубый — адаптив дёргает ширину на десяток пикселей, а
// каждый перезапуск потока стоит пропущенных кадров.
func (l *cdpLink) retuneLocked(quality, maxW, maxH int) {
	if l.client == nil {
		return
	}
	if quality <= 0 {
		return // подключение только для управления — поток не трогаем
	}
	if !l.client.Streaming() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := l.client.StartScreencast(ctx, quality, maxW, maxH); err != nil {
			log.Printf("[CDP] поток кадров не запустился: %v", err)
			return
		}
		l.quality, l.maxW, l.maxH = quality, maxW, maxH
		return
	}
	if quality == l.quality && abs(maxW-l.maxW) < 64 && abs(maxH-l.maxH) < 64 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := l.client.Retune(ctx, quality, maxW, maxH); err != nil {
		log.Printf("[CDP] перенастройка потока не удалась: %v", err)
		return
	}
	l.quality, l.maxW, l.maxH = quality, maxW, maxH
}

func (l *cdpLink) drop() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.client != nil {
		l.client.Close()
		l.client = nil
	}
}

// currentCDPClient — подключение, если оно уже установлено (для ввода и
// навигации; сам по себе новое соединение не открывает).
func currentCDPClient() *cdp.Client {
	browserLink.mu.Lock()
	defer browserLink.mu.Unlock()
	if browserLink.client != nil && browserLink.client.Alive() {
		return browserLink.client
	}
	return nil
}

// browserController — ввод через страницу, когда она доступна. Возвращает nil,
// если подключения нет: тогда работает обычный контроллер через ОС.
func browserController() input.Controller {
	if c := currentCDPClient(); c != nil {
		return cdp.NewController(c)
	}
	return nil
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

// browserFramesActive — кадры сейчас приходят от самого браузера. Проверка
// изменений через X в этом случае не нужна и вредна: экран может быть
// неподвижен, а свежий кадр страницы уже лежать готовым.
func browserFramesActive() bool { return currentCDPClient() != nil }

// nextRemoteFrame — очередной кадр для отправки клиенту.
//
// Порядок источников: если виртуальный браузер отдаёт кадры сам (CDP), берём
// готовый JPEG у него — ни захвата экрана, ни кодирования. Иначе снимаем экран
// как раньше. changed=false означает «нового кадра нет», и это нормальный ход
// событий: Chrome присылает кадр только когда картинка изменилась.
func nextRemoteFrame(
	bounds image.Rectangle, targetWidth, quality int, buf *bytes.Buffer,
	gate *frameGate, cdpSeq *uint64, force bool,
) (capMs, encMs int64, changed bool, err error) {
	if client := cdpClient(quality, targetWidth, 0); client != nil {
		f, ok := client.LastFrame()
		if !ok {
			return 0, 0, false, nil // вкладка ещё не прислала первый кадр
		}
		if !force && f.Seq == *cdpSeq {
			return 0, 0, false, nil
		}
		*cdpSeq = f.Seq
		buf.Reset()
		buf.Write(f.JPEG)
		return 0, 0, true, nil
	}
	return captureChangedJPEG(bounds, targetWidth, quality, buf, gate, force)
}

// nextRemoteFrameTiled — тот же выбор источника, что у nextRemoteFrame, но для
// зрителей, умеющих частичные кадры. У виртуального браузера кадр приходит уже
// готовым JPEG (тайлить нечего — регионы nil), экран же отдаёт только
// изменившиеся области.
func nextRemoteFrameTiled(
	bounds image.Rectangle, targetWidth, quality int, buf *bytes.Buffer,
	gate *frameGate, tracker *tileTracker, source *screenSource, cdpSeq *uint64, force bool,
) (capMs, encMs int64, changed bool, regions []encodedRegion, err error) {
	if client := cdpClient(quality, targetWidth, 0); client != nil {
		f, ok := client.LastFrame()
		if !ok {
			return 0, 0, false, nil, nil
		}
		if !force && f.Seq == *cdpSeq {
			return 0, 0, false, nil, nil
		}
		*cdpSeq = f.Seq
		buf.Reset()
		buf.Write(f.JPEG)
		tracker.reset() // источник сменился — следующий экранный кадр уедет целиком
		return 0, 0, true, nil, nil
	}
	return captureChangedRegions(bounds, targetWidth, quality, buf, gate, tracker, source, force)
}

// cdpH264Frame — очередной кадр от виртуального браузера для H.264-пути
// (produceH264). Выбор источника тот же, что у nextRemoteFrame: подключение
// живо — кадры берём только у браузера (первый кадр ещё не пришёл →
// changed=false, на захват экрана при этом НЕ падаем); подключения нет —
// ok=false, и вызывающий работает через экран. Кодирование change-driven:
// свежий Seq, keep-alive по таймеру или запрос IDR — иначе кадр пропускаем.
func cdpH264Frame(quality, maxW int, cdpSeq *uint64, keepAlive bool) (img image.Image, ok, changed bool, err error) {
	client := cdpClient(quality, maxW, 0)
	if client == nil {
		return nil, false, false, nil
	}
	f, has := client.LastFrame()
	if !has {
		return nil, true, false, nil // вкладка ещё не прислала первый кадр
	}
	img, changed, err = decodeCDPH264Frame(f, cdpSeq, keepAlive)
	return img, true, changed, err
}

// decodeCDPH264Frame — Seq-ворота и JPEG-декод кадра скринкаста. Тот же Seq
// (кадр уже кодировали) без keep-alive → changed=false, работы нет. Seq
// сдвигается и на битом кадре: пережёвывать один и тот же битый JPEG на
// каждом такте смысла нет. Вынесено от подключения, чтобы решение
// «брать/пропустить» проверялось тестом без живого Chrome.
func decodeCDPH264Frame(f cdp.Frame, cdpSeq *uint64, keepAlive bool) (img image.Image, changed bool, err error) {
	if !keepAlive && f.Seq == *cdpSeq {
		return nil, false, nil
	}
	*cdpSeq = f.Seq
	img, err = jpeg.Decode(bytes.NewReader(f.JPEG))
	if err != nil {
		return nil, false, err
	}
	return img, true, nil
}

// remoteFrameBounds — система координат, в которой клиент шлёт ввод. При кадрах
// от браузера это размер САМОЙ СТРАНИЦЫ (в кадре нет ни вкладок, ни адресной
// строки), поэтому экранные границы здесь не годятся: нажатие уезжало бы вверх
// на высоту интерфейса браузера.
func remoteFrameBounds(screen image.Rectangle) image.Rectangle {
	if c := currentCDPClient(); c != nil {
		if f, ok := c.LastFrame(); ok && f.Width > 0 && f.Height > 0 {
			return image.Rect(0, 0, f.Width, f.Height)
		}
	}
	return screen
}

// routedController шлёт ввод в страницу, когда браузер на связи, и в
// операционную систему во всех остальных случаях. Проверка на каждом событии
// намеренная: браузер могут запустить или остановить посреди сеанса, и человек
// не должен для этого переподключаться.
type routedController struct{ fallback input.Controller }

func (r routedController) pick() input.Controller {
	vbrowser.NoteActivity() // любой ввод сдвигает дедлайн автоостановки по простою
	if c := browserController(); c != nil {
		return c
	}
	return r.fallback
}

func (r routedController) MouseMove(x, y int) error { return r.pick().MouseMove(x, y) }
func (r routedController) MouseClick(x, y int, b string) error {
	return r.pick().MouseClick(x, y, b)
}
func (r routedController) MouseDoubleClick(x, y int) error { return r.pick().MouseDoubleClick(x, y) }
func (r routedController) MouseDown(x, y int, b string) error {
	return r.pick().MouseDown(x, y, b)
}
func (r routedController) MouseUp(x, y int, b string) error { return r.pick().MouseUp(x, y, b) }
func (r routedController) Scroll(dy int) error              { return r.pick().Scroll(dy) }
func (r routedController) ScrollH(dx int) error             { return input.ScrollH(r.pick(), dx) }
func (r routedController) KeyDown(key string) error         { return r.pick().KeyDown(key) }
func (r routedController) KeyUp(key string) error           { return r.pick().KeyUp(key) }
func (r routedController) TypeText(text string) error       { return r.pick().TypeText(text) }

// PasteText вставляет текст одним куском, когда принимающая сторона это умеет
// (страница умеет, клавиатура операционной системы — нет: там вставка и есть
// набор).
func (r routedController) PasteText(text string) error {
	vbrowser.NoteActivity()
	if c := browserController(); c != nil {
		if paster, ok := c.(pasteCapable); ok {
			return paster.PasteText(text)
		}
	}
	return r.fallback.TypeText(text)
}

// TouchPoints уходит в страницу, когда та на связи. У обычного рабочего стола
// касаний нет вовсе — тогда сообщаем об этом наверх, и слой ввода переводит
// жест в мышь (см. applyTouch).
func (r routedController) TouchPoints(kind string, points [][2]float64) error {
	vbrowser.NoteActivity() // касания — тот же ввод, сдвигают дедлайн простоя
	if c := browserController(); c != nil {
		if touch, ok := c.(interface {
			TouchPoints(string, [][2]float64) error
		}); ok {
			return touch.TouchPoints(kind, points)
		}
	}
	return errNoTouch
}

// errNoTouch — «настоящих касаний здесь нет»: слой ввода ловит его и повторяет
// жест мышью.
var errNoTouch = errors.New("touch unsupported")

// routeInput оборачивает системный контроллер маршрутизатором.
func routeInput(ctrl input.Controller) input.Controller {
	if ctrl == nil {
		return nil
	}
	return routedController{fallback: ctrl}
}

// controlCDPClient — подключение для управления вкладкой (адрес, история). В
// отличие от пути кадров, поднимается и когда на экран никто не смотрит:
// человек может открыть адрес до того, как включит просмотр.
func controlCDPClient() *cdp.Client {
	if c := currentCDPClient(); c != nil {
		return c
	}
	return cdpClient(0, 0, 0) // 0 = без потока кадров
}

// browserStreamStopped гасит поток кадров, когда смотреть перестали. Само
// подключение остаётся: оно нужно для навигации и стоит недорого, а вот кадры
// без зрителя — это чистая трата процессора сервера.
func browserStreamStopped() {
	browserLink.mu.Lock()
	client := browserLink.client
	browserLink.quality, browserLink.maxW, browserLink.maxH = 0, 0, 0
	browserLink.mu.Unlock()
	if client == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := client.StopScreencast(ctx); err != nil {
		log.Printf("[CDP] остановка потока: %v", err)
	}
}

// streamViewers — сколько экранов сейчас смотрят трансляцию (оба транспорта:
// WS и WebRTC). Счётчик, а не флаг: телефон и окно на компьютере могут
// смотреть одновременно, и idle-таймер браузера ждёт ухода ПОСЛЕДНЕГО.
var streamViewers atomic.Int64

// browserViewerJoined/Left ведут счёт зрителей для автоостановки виртуального
// браузера по простою (vbrowser.SetViewers).
func browserViewerJoined() {
	vbrowser.SetViewers(int(streamViewers.Add(1)))
}

func browserViewerLeft() {
	n := streamViewers.Add(-1)
	if n < 0 { // старый агент/двойной уход — счётчик не уходит в минус
		n = 0
		streamViewers.Store(0)
	}
	vbrowser.SetViewers(int(n))
	// Уход зрителя — последняя активность: отсчёт простоя начинается отсюда.
	vbrowser.NoteActivity()
}

// rememberDevice запоминает профиль устройства для будущих подключений.
func rememberDevice(dev cdp.Device) {
	browserLink.mu.Lock()
	browserLink.device = &dev
	browserLink.mu.Unlock()
}

// dropBrowserLink закрывает подключение (вкладку закрыли — смотреть нечего).
func dropBrowserLink() { browserLink.drop() }

// switchBrowserTab переводит просмотр на другую вкладку: делает её активной в
// самом браузере (иначе Chrome душит её таймеры как фоновую) и переподключает
// наш канал — кадры и ввод идут ровно в ту вкладку, что на экране.
func switchBrowserTab(ctx context.Context, endpoint, id string) {
	if id == "" {
		return
	}
	if err := cdp.ActivateTab(ctx, endpoint, id); err != nil {
		log.Printf("[CDP] активация вкладки: %v", err)
	}
	browserLink.mu.Lock()
	browserLink.wantTarget = id
	old := browserLink.client
	browserLink.client = nil
	browserLink.mu.Unlock()
	if old != nil {
		// Уходим с вкладки — её последний кадр становится превью в списке
		// вкладок: это ровно то, что человек видел, и снимать его заново не
		// нужно. Уменьшение стоит процессора, поэтому делаем это в стороне.
		if frame, ok := old.LastFrame(); ok {
			go rememberTabPreview(old.TargetID(), frame.JPEG)
		}
		old.Close()
	}
}

// browserNotices — подписчики на события страницы (фокус в поле, загрузка).
// Их столько же, сколько открытых экранов: телефон, окно на компьютере, второй
// телефон — каждому нужно своё уведомление.
var browserNotices struct {
	mu   sync.Mutex
	subs map[int]func(map[string]any)
	next int
}

// subscribeBrowserNotices регистрирует получателя; вызов возвращённой функции
// снимает подписку.
func subscribeBrowserNotices(fn func(map[string]any)) func() {
	browserNotices.mu.Lock()
	defer browserNotices.mu.Unlock()
	if browserNotices.subs == nil {
		browserNotices.subs = map[int]func(map[string]any){}
	}
	browserNotices.next++
	id := browserNotices.next
	browserNotices.subs[id] = fn
	return func() {
		browserNotices.mu.Lock()
		delete(browserNotices.subs, id)
		browserNotices.mu.Unlock()
	}
}

// publishBrowserNotice рассылает событие страницы всем открытым экранам.
func publishBrowserNotice(n cdp.Notice) {
	if n.Kind == "loaded" {
		// Страница открылась — засчитываем сайт в «часто открываю» на стартовом
		// экране и узнаём его значок. Спрашиваем адрес отдельно и в стороне от
		// рассылки: событие обязано уйти зрителям сразу, а не после разговора с
		// браузером.
		go noteVisitedPage()
		go refreshActiveTabIcon()
	}
	broadcastBrowserMsg(browserNoticeMessage(n))
}

// noteVisitedPage записывает открытый сайт в список часто посещаемых.
func noteVisitedPage() {
	client := currentCDPClient()
	if client == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	info := client.Info(ctx)
	if info.URL == "" {
		return
	}
	places.Note(info.URL, info.Title)
}

// browserNoticeMessage keeps the page-event contract in one testable place.
// Native browser UI (file picker and JS dialogs) is not part of the video
// frame, so dropping one of these fields means leaving the user in front of a
// blocked page with no possible action.
func browserNoticeMessage(n cdp.Notice) map[string]any {
	msg := map[string]any{"t": "browser", "kind": n.Kind}
	if n.Kind == "focus" {
		msg["editable"] = n.Editable
	}
	if n.Kind == "scroll" {
		// Положение прокрутки нужно жестам: «потянуть вниз, чтобы обновить»
		// срабатывает только у самого верха, иначе он крал бы листание.
		msg["y"] = n.ScrollY
	}
	if n.Kind == "download" && n.File != "" {
		msg["file"] = n.File
	}
	if n.Kind == "file_chooser" {
		msg["id"] = n.ID
		msg["multiple"] = n.Multiple
	}
	if n.Kind == "file_chooser_closed" || n.Kind == "dialog_closed" {
		msg["id"] = n.ID
	}
	if n.Kind == "dialog" {
		msg["id"] = n.ID
		msg["dialog_type"] = n.DialogType
		msg["message"] = n.Message
		msg["default_prompt"] = n.DefaultPrompt
		msg["url"] = n.URL
	}
	if n.Kind == "popup" {
		msg["url"] = n.URL
	}
	return msg
}

// broadcastBrowserMsg рассылает готовое событие всем открытым экранам.
func broadcastBrowserMsg(msg map[string]any) {
	browserNotices.mu.Lock()
	subs := make([]func(map[string]any), 0, len(browserNotices.subs))
	for _, fn := range browserNotices.subs {
		subs = append(subs, fn)
	}
	browserNotices.mu.Unlock()
	for _, fn := range subs {
		fn(msg)
	}
}

// downloadsDir — куда браузер кладёт скачанные файлы.
// downloadsDir — куда браузер кладёт скачанные файлы. Каталог состояния
// виртуального браузера: он живёт под отдельным пользователем (Credential) и
// в домашний каталог root-а писать не может.
func downloadsDir() string {
	dir := filepath.Join(vbrowser.StateDir(), "downloads")
	if err := os.MkdirAll(dir, 0o700); err == nil {
		return dir
	}
	return os.TempDir()
}

// activeTargetID — вкладка, которую человек смотрит: либо та, к которой уже
// подключены, либо выбранная последним переключением (подключение к ней может
// ещё не подняться).
func activeTargetID() string {
	browserLink.mu.Lock()
	defer browserLink.mu.Unlock()
	if browserLink.client != nil && browserLink.client.Alive() {
		return browserLink.client.TargetID()
	}
	return browserLink.wantTarget
}
