package web

// inputApplier serializes remote-desktop input injection for one session and
// keeps the transport read-path non-blocking. Two lanes:
//
//   - pointer moves — a depth-1 "latest wins" mailbox: injecting every queued
//     move is pointless (only the newest position matters) and lets a burst of
//     touch-move JSON pile up behind a click's built-in sleeps. A client seq
//     guards against reordering on the unreliable "input" DataChannel — a
//     late-arriving OLD move must not yank the cursor backwards.
//   - everything else (clicks/keys/text/scroll) — an ordered queue. Clicks
//     carry their own coordinates, so they don't race the move mailbox.
//
// Before this, input was injected directly on the read path: MouseClick's
// 15–90ms sleeps stalled every event behind them, and a 120Hz move storm from
// the touchscreen queued up during any hiccup, replaying stale positions.

import (
	"errors"
	"image"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"tgcontrol/internal/input"
	"tgcontrol/internal/observability"
)

// inputWarnEvery — как часто повторять сигнал «ввод не доходит». Пока рабочий
// стол заблокирован (или висит UAC), операционная система отбрасывает КАЖДОЕ
// событие — до 120 движений в секунду; клиенту нужна одна стойкая плашка, а не
// поток предупреждений. Повтор нужен для тех, кто подключился к сессии позже.
const inputWarnEvery = 10 * time.Second

type inputApplier struct {
	ctrl     input.Controller
	boundsFn func() image.Rectangle
	done     <-chan struct{}

	// notify отправляет клиенту служебное сообщение по control-каналу
	// (writeJSON в WS-транспорте, ctrl-DataChannel в WebRTC). nil = сообщать
	// некому (в тестах).
	notify func(map[string]any)

	// Состояние сигнализации об отказах ввода. Пишется и читается ТОЛЬКО из
	// горутины run (applyMove/applyEvent), поэтому без мьютекса.
	warnCode string    // код последнего отправленного предупреждения ("" = ввод доходит)
	warnAt   time.Time // когда его отправили

	kick chan struct{}
	evCh chan map[string]any

	mu       sync.Mutex
	haveMove bool
	moveSeq  int64
	moveX    float64
	moveY    float64

	scrollAcc  float64 // fractional wheel remainder (run goroutine only)
	scrollAccX float64 // то же для горизонтального колеса

	// lastMoveAt (unixnano) — when the client last steered the pointer. Used to
	// suppress the host cursor-position echo while the user's finger is down.
	lastMoveAt atomic.Int64

	// lastInputAt (unixnano) — любое действие человека, не только движение
	// указателя. По нему поток кадров понимает, что идёт жест, и на это время
	// меняет обмен «чёткость ↔ частота» (см. boostWhileInteracting).
	lastInputAt atomic.Int64
}

// interactionWindow — сколько после последнего действия считать, что человек
// ещё ведёт жест. Пол-секунды: короче — режим мигает между прокрутками,
// длиннее — картинка остаётся зернистой, когда всё давно замерло.
const interactionWindow = 500 * time.Millisecond

// boostWhileInteracting поднимает частоту и опускает качество, пока идёт жест.
// В покое возвращает то, что решил адаптив по каналу.
func boostWhileInteracting(a *inputApplier, fps, quality int) (int, int) {
	if a == nil {
		return fps, quality
	}
	last := a.lastInputAt.Load()
	if last == 0 || time.Since(time.Unix(0, last)) > interactionWindow {
		return fps, quality
	}
	if fps < 24 {
		fps = 24
	}
	if quality > 55 {
		quality = 55
	}
	return fps, quality
}

// newInputApplier запускает горутину инъекции. notify (может быть nil) —
// отправка служебных сообщений клиенту по control-каналу: через него уходят
// предупреждения о том, что ввод до компьютера не доходит.
func newInputApplier(ctrl input.Controller, boundsFn func() image.Rectangle, done <-chan struct{}, notify func(map[string]any)) *inputApplier {
	a := &inputApplier{
		ctrl:     ctrl,
		boundsFn: boundsFn,
		done:     done,
		notify:   notify,
		kick:     make(chan struct{}, 1),
		evCh:     make(chan map[string]any, 64),
	}
	go a.run()
	return a
}

// Move queues an absolute pointer move (normalized 0..1), latest wins.
// seq: client-side sequence for the unreliable channel; pass -1 for ordered
// transports (WS / ctrl DC) to auto-sequence.
func (a *inputApplier) Move(x, y float64, seq int64) {
	if math.IsNaN(x) || math.IsInf(x, 0) || math.IsNaN(y) || math.IsInf(y, 0) {
		return
	}
	a.mu.Lock()
	if seq < 0 {
		seq = a.moveSeq + 1
	} else if seq <= a.moveSeq {
		a.mu.Unlock()
		return // stale or reordered — never move the cursor backwards in time
	}
	a.moveSeq = seq
	a.moveX, a.moveY = x, y
	a.haveMove = true
	a.mu.Unlock()
	a.lastMoveAt.Store(time.Now().UnixNano())
	select {
	case a.kick <- struct{}{}:
	default:
	}
}

// Submit queues a non-move input event. Drops (returns false) when the queue
// is full — better than wedging the transport read-path.
func (a *inputApplier) Submit(evt map[string]any) bool {
	select {
	case a.evCh <- evt:
		return true
	default:
		return false
	}
}

func (a *inputApplier) run() {
	defer observability.RecoverPanic("remote-input-applier")
	for {
		select {
		case <-a.done:
			return
		case <-a.kick:
			a.applyMove()
		case evt := <-a.evCh:
			a.applyMove() // flush the freshest position before a click/key
			a.applyEvent(evt)
		}
	}
}

func (a *inputApplier) applyMove() {
	a.mu.Lock()
	if !a.haveMove {
		a.mu.Unlock()
		return
	}
	x, y := a.moveX, a.moveY
	a.haveMove = false
	a.mu.Unlock()
	a.reportInput(processInput(a.ctrl, map[string]any{"t": "m", "a": "move", "x": x, "y": y}, a.boundsFn()))
}

// reportInput превращает результат инъекции в машинный код для клиента (тексты
// подставляет клиент):
//
//	input_blocked     — рабочий стол заблокирован или активен UAC: разблокировать
//	                    может только человек у самого компьютера;
//	input_unavailable — вводить нечем (Linux без xdotool / без доступного X);
//	input_ok          — ввод снова доходит, плашку можно снять без таймеров.
//
// Прочие ошибки (например, неизвестное имя клавиши) не про доставку ввода и
// клиенту не показываются.
func (a *inputApplier) reportInput(err error) {
	if a.notify == nil {
		return
	}
	code := ""
	switch {
	case err == nil:
	case errors.Is(err, input.ErrInputBlocked):
		code = "input_blocked"
	case errors.Is(err, input.ErrInputUnavailable):
		code = "input_unavailable"
	default:
		return
	}
	if code == "" {
		if a.warnCode == "" {
			return
		}
		a.warnCode, a.warnAt = "", time.Time{}
		a.notify(map[string]any{"t": "warn", "code": "input_ok"})
		return
	}
	now := time.Now()
	if code == a.warnCode && now.Sub(a.warnAt) < inputWarnEvery {
		return
	}
	a.warnCode, a.warnAt = code, now
	a.notify(map[string]any{"t": "warn", "code": code})
}

func (a *inputApplier) applyEvent(evt map[string]any) {
	if t, _ := evt["t"].(string); t == "s" {
		// Wheel deltas are fractional on touch (momentum); int truncation per
		// event loses the fraction and turns slow scrolls into dead zones.
		dy, _ := evt["dy"].(float64)
		dx, _ := evt["dx"].(float64)
		if math.IsNaN(dy) || math.IsInf(dy, 0) || math.IsNaN(dx) || math.IsInf(dx, 0) {
			return
		}
		a.scrollAcc += dy
		whole := math.Trunc(a.scrollAcc)
		a.scrollAcc -= whole
		// Горизонтальная ось копится отдельно (с 2.61.18): у тачпада с телефона
		// dx приходит дробями, как и dy.
		a.scrollAccX += dx
		wholeX := math.Trunc(a.scrollAccX)
		a.scrollAccX -= wholeX
		if whole == 0 && wholeX == 0 {
			return
		}
		evt["dy"] = whole
		evt["dx"] = wholeX
	}
	a.reportInput(processInput(a.ctrl, evt, a.boundsFn()))
}

// Route dispatches a decoded remote-input event into the right lane. Returns
// false if the event queue was full and the event was dropped.
func (a *inputApplier) Route(evt map[string]any) bool {
	a.lastInputAt.Store(time.Now().UnixNano())
	if t, _ := evt["t"].(string); t == "m" {
		if act, _ := evt["a"].(string); act == "move" || act == "" {
			x, _ := evt["x"].(float64)
			y, _ := evt["y"].(float64)
			a.Move(x, y, -1)
			return true
		}
	}
	return a.Submit(evt)
}
