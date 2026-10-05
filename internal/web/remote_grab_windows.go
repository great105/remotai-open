//go:build windows

package web

import (
	"errors"
	"image"
	"log"
	"runtime"
	"sync"
	"time"

	"github.com/kbinani/screenshot"
	"github.com/kirides/go-d3d/d3d11"
	"github.com/kirides/go-d3d/outputduplication"
)

// Захват экрана на Windows через Desktop Duplication (DXGI).
//
// Зачем. GDI-путь не умеет отвечать на вопрос «менялось ли»: на Linux его
// задаёт X DAMAGE, а здесь ответ был всегда «да» — и на НЕПОДВИЖНОМ экране мы
// 30 раз в секунду снимали весь экран (замер 23.08: 15,7 мс) и считали по нему
// crc32. Примерно половина ядра впустую, на ноутбуке это нагрев и батарея.
// Хуже того, каждый зритель делал СВОЙ захват: два зрителя — двойная работа.
//
// Как устроено. Один фоновый насос (своя нить ОС, потому что COM-объекты
// привязаны к потоку) тянет кадры из дубликатора и держит последний снимок с
// номером поколения. Зрители не захватывают ничего: они спрашивают «поколение
// изменилось?» и берут готовый кадр. Насос сам останавливается, когда зрители
// разошлись, и освобождает видеоресурсы.
//
// Почему именно так, а не «спросить дубликатор в момент кадра»: замер показал,
// что запрос кадра у DXGI стоит ~16 мс (он привязан к обновлению экрана), и
// при опросе из цикла зрителя это ожидание умножалось бы на число зрителей.
//
// Любая осечка — падаем на GDI и продолжаем работать: дубликатор теряется на
// смене разрешения, при переключении пользователя, на экране UAC и под RDP.
// Это норма, а не повод оставить человека без картинки.

const (
	// dxgiRetryPause — сколько не трогаем DXGI после серии отказов.
	dxgiRetryPause = 30 * time.Second
	// dxgiIdleStop — насос без единого обращения столько времени выключается.
	dxgiIdleStop = 10 * time.Second
	// dxgiWaitMs — сколько ждём кадр у системы за один оборот насоса. Ожидание
	// не тратит процессор: поток спит внутри AcquireNextFrame.
	dxgiWaitMs = 100
)

// screenSource — то, чем цикл зрителя берёт кадры. Реализация зависит от
// системы, поэтому вызывающий код одинаков везде.
type screenSource struct {
	bounds  image.Rectangle
	lastGen uint64
}

func newScreenSource(bounds image.Rectangle) *screenSource {
	return &screenSource{bounds: bounds}
}

// Changed — появился ли новый кадр с прошлого раза. Ответ мгновенный: сравнение
// номера поколения, никаких обращений к видеоподсистеме.
func (s *screenSource) Changed(bounds image.Rectangle) bool {
	s.bounds = bounds
	gen, ok := screenPump().generation(bounds)
	if !ok {
		return true // насоса нет (или он не про этот дисплей) — как раньше
	}
	return gen != s.lastGen
}

// Grab — последний кадр. При любой осечке DXGI снимает экран прежним способом.
func (s *screenSource) Grab(bounds image.Rectangle) (*image.RGBA, error) {
	s.bounds = bounds
	if img, gen, ok := screenPump().frame(bounds); ok {
		s.lastGen = gen
		return img, nil
	}
	return screenshot.CaptureRect(bounds)
}

// dxgiPump — фоновый насос кадров.
type dxgiPump struct {
	mu       sync.Mutex
	frameBuf *image.RGBA
	gen      uint64
	bounds   image.Rectangle
	running  bool
	lastUse  time.Time
	pausedTo time.Time
	failures int
}

var (
	pumpOnce sync.Once
	pump     *dxgiPump
)

func screenPump() *dxgiPump {
	pumpOnce.Do(func() { pump = &dxgiPump{} })
	return pump
}

// generation — номер последнего кадра для этого дисплея. ok=false означает
// «данных нет»: насос не запущен, занят другим дисплеем или DXGI на паузе.
func (p *dxgiPump) generation(bounds image.Rectangle) (uint64, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.touchLocked(bounds)
	if p.frameBuf == nil || p.bounds != bounds {
		return 0, false
	}
	return p.gen, true
}

// frame — копия последнего кадра: буфер переиспользуется насосом, а зритель
// живёт с картинкой дольше (масштабирование, хэши, кодек).
func (p *dxgiPump) frame(bounds image.Rectangle) (*image.RGBA, uint64, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.touchLocked(bounds)
	if p.frameBuf == nil || p.bounds != bounds {
		return nil, 0, false
	}
	out := image.NewRGBA(p.frameBuf.Bounds())
	copy(out.Pix, p.frameBuf.Pix)
	return out, p.gen, true
}

// touchLocked отмечает интерес зрителя и при необходимости поднимает насос.
func (p *dxgiPump) touchLocked(bounds image.Rectangle) {
	p.lastUse = time.Now()
	if p.running || time.Now().Before(p.pausedTo) {
		return
	}
	idx := displayIndexForBounds(bounds, activeDisplayBounds())
	if idx < 0 {
		return // кусок экрана или объединение мониторов — DXGI такого не отдаёт
	}
	p.running = true
	p.bounds = bounds
	p.frameBuf = nil
	p.gen = 0
	go p.run(idx, bounds)
}

func (p *dxgiPump) run(display int, bounds image.Rectangle) {
	// Поток закрепляется за горутиной: COM-объекты привязаны к нему.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	defer func() {
		p.mu.Lock()
		p.running = false
		p.frameBuf = nil
		p.mu.Unlock()
	}()

	device, deviceCtx, err := d3d11.NewD3D11Device()
	if err != nil {
		p.pause(err)
		return
	}
	defer device.Release()
	defer deviceCtx.Release()

	dup, err := outputduplication.NewIDXGIOutputDuplication(device, deviceCtx, uint(display))
	if err != nil {
		p.pause(err)
		return
	}
	defer dup.Release()

	buf := image.NewRGBA(image.Rect(0, 0, bounds.Dx(), bounds.Dy()))
	for {
		if p.idle() {
			return // зрители разошлись — освобождаем видеоресурсы
		}
		err := dup.GetImage(buf, dxgiWaitMs)
		switch {
		case err == nil:
			p.publish(buf, bounds)
		case errors.Is(err, outputduplication.ErrNoImageYet):
			// Экран неподвижен — именно ради этого ответа всё и затевалось.
			continue
		default:
			p.pause(err)
			return
		}
	}
}

func (p *dxgiPump) publish(buf *image.RGBA, bounds image.Rectangle) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.frameBuf == nil || p.frameBuf.Bounds() != buf.Bounds() {
		p.frameBuf = image.NewRGBA(buf.Bounds())
	}
	copy(p.frameBuf.Pix, buf.Pix)
	p.bounds = bounds
	p.gen++
	p.failures = 0
}

func (p *dxgiPump) idle() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return time.Since(p.lastUse) > dxgiIdleStop
}

// pause уводит DXGI в сторону после отказа: пересоздавать устройство на каждом
// кадре хуже, чем спокойно снимать экран прежним способом (например, пока на
// экране висит UAC или идёт смена разрешения).
func (p *dxgiPump) pause(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.failures++
	p.pausedTo = time.Now().Add(dxgiRetryPause)
	p.frameBuf = nil
	log.Printf("[SCREEN] DXGI недоступен (%v) — снимаем экран прежним способом %v", err, dxgiRetryPause)
}

// displayIndexForBounds ищет дисплей, совпадающий с запрошенной областью.
// -1 означает «это не целый дисплей» (кусок экрана или объединение мониторов).
func displayIndexForBounds(bounds image.Rectangle, displays []image.Rectangle) int {
	for i, d := range displays {
		if d == bounds {
			return i
		}
	}
	return -1
}

func activeDisplayBounds() []image.Rectangle {
	n := screenshot.NumActiveDisplays()
	out := make([]image.Rectangle, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, screenshot.GetDisplayBounds(i))
	}
	return out
}

// grabScreen — снимок области экрана вне цикла зрителя (превью, разовые
// снимки). Идёт мимо насоса: у разового снимка нет истории поколений.
func grabScreen(bounds image.Rectangle) (*image.RGBA, error) {
	if img, _, ok := screenPump().frame(bounds); ok {
		return img, nil
	}
	return screenshot.CaptureRect(bounds)
}
