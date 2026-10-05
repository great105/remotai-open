package web

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"log"
	"math"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"github.com/kbinani/screenshot"
	"golang.org/x/image/draw"

	"tgcontrol/internal/audio"
	"tgcontrol/internal/connstat"
	"tgcontrol/internal/input"
	"tgcontrol/internal/observability"
	"tgcontrol/internal/procutil"
	"tgcontrol/internal/service"
	"tgcontrol/internal/vbrowser"
	"tgcontrol/internal/wsutil"
)

// ── Adaptive streaming controller ──────────────────────────────────
//
// Перегрузка измеряется ОТНОСИТЕЛЬНО базовой линии маршрута, а не абсолютным
// порогом. Абсолютные 200 мс («хуже — вниз») и 60 мс («лучше — вверх») делали
// профиль «Авто» непригодным на дальнем плече: замер 23.08 — зритель в
// Монголии, релей во Франкфурте, ping 263 мс; каждый ACK читался как затор,
// поток съезжал в пол (640 px / q35 / 5 fps) и НИКОГДА не поднимался, потому
// что порог подъёма был недостижим физически. Теперь база — скользящий
// минимум RTT за окно, а затор — заметный рост НАД этой базой.
//
//	rtt < base + goodDelta (3 подряд) → вверх: сначала чёткость, потом плавность
//	rtt > base + badDelta  (2 подряд) → вниз:  сначала плавность, потом чёткость
//	потери кадров (dropped)           → вниз немедленно
//
//	Ranges:  FPS 5–30,  Quality 45–85,  Width 800–1920
//
// Порядок деградации намеренно тот же, что в H.264-пути (qosFPSLadder):
// для экранного содержимого мыльный текст — отказ худший, чем низкий fps.

// Окно, за которое держится минимум RTT: маршрут может стать хуже (сменился
// VPN, ушли в роуминг) — база обязана уметь расти, иначе один удачный замер
// навсегда сделает нормальный маршрут «затором».
const rttBaseWindow = 30 * time.Second

type adaptiveCtrl struct {
	mu          sync.Mutex
	profile     string
	fps         int
	quality     int
	targetWidth int
	rtt         time.Duration
	sentTimes   map[uint32]time.Time
	goodStreak  int
	badStreak   int
	clientWidth int
	// Базовая линия маршрута: минимум RTT за текущее окно и его начало.
	baseRTT       time.Duration
	baseWindowMin time.Duration
	baseSince     time.Time
	// Tier-based caps
	maxFPS     int
	maxQuality int
	maxWidth   int
}

// rttThresholds — насколько RTT должен превысить базу, чтобы считаться затором
// (bad), и насколько близко к базе он должен быть, чтобы разрешить подъём
// (good). Пропорция от базы, а не константа: на 20-мс базе затор — это уже
// +60 мс, на 263-мс — +131 мс.
func rttThresholds(base time.Duration) (good, bad time.Duration) {
	good = base + max(20*time.Millisecond, base/8)
	bad = base + max(60*time.Millisecond, base/2)
	return good, bad
}

func newAdaptiveCtrl() *adaptiveCtrl {
	return newAdaptiveCtrlWithLimits(30, 85, 1920)
}

func newAdaptiveCtrlWithLimits(maxFPS, maxQuality, maxWidth int) *adaptiveCtrl {
	if maxFPS <= 0 {
		maxFPS = 30
	}
	if maxQuality <= 0 {
		maxQuality = 85
	}
	if maxWidth <= 0 {
		maxWidth = 1920
	}
	return &adaptiveCtrl{
		profile:     "auto",
		fps:         min(12, maxFPS),
		quality:     min(75, maxQuality),
		targetWidth: min(960, maxWidth),
		sentTimes:   make(map[uint32]time.Time),
		maxFPS:      maxFPS,
		maxQuality:  maxQuality,
		maxWidth:    maxWidth,
	}
}

func (a *adaptiveCtrl) onSend(seq uint32) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.sentTimes[seq] = time.Now()
	for s, t := range a.sentTimes {
		if time.Since(t) > 5*time.Second {
			delete(a.sentTimes, s)
		}
	}
}

func (a *adaptiveCtrl) onACK(seq uint32, dropped int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	sent, ok := a.sentTimes[seq]
	if !ok {
		return
	}
	a.rtt = time.Since(sent)
	delete(a.sentTimes, seq)

	if dropped > 0 {
		a.goodStreak = 0
		a.badStreak += max(1, min(dropped, 3))
		if a.badStreak >= 2 {
			a.downshiftLocked()
			a.badStreak = 0
		}
		return
	}
	a.observeRTTLocked(a.rtt)
	if a.profile != "auto" {
		a.goodStreak = 0
		a.badStreak = 0
		return
	}

	good, bad := rttThresholds(a.baseRTT)
	switch {
	case a.rtt <= good:
		a.badStreak = 0
		a.goodStreak++
		if a.goodStreak >= 3 {
			a.upshiftLocked()
			a.goodStreak = 0
		}
	case a.rtt >= bad:
		a.goodStreak = 0
		a.badStreak++
		if a.badStreak >= 2 {
			a.downshiftLocked()
			a.badStreak = 0
		}
	default:
		a.goodStreak = 0
		a.badStreak = 0
	}
}

// observeRTTLocked ведёт базовую линию маршрута: минимум за окно rttBaseWindow.
// Окно закрывается — минимум окна становится базой, набор начинается заново.
// Так база опускается сразу за улучшившимся маршрутом и поднимается за
// ухудшившимся не позже чем через окно.
func (a *adaptiveCtrl) observeRTTLocked(rtt time.Duration) {
	if rtt <= 0 {
		return
	}
	now := time.Now()
	if a.baseRTT == 0 {
		a.baseRTT, a.baseWindowMin, a.baseSince = rtt, rtt, now
		return
	}
	if a.baseWindowMin == 0 || rtt < a.baseWindowMin {
		a.baseWindowMin = rtt
	}
	if rtt < a.baseRTT {
		a.baseRTT = rtt // улучшение маршрута принимаем немедленно
	}
	if now.Sub(a.baseSince) >= rttBaseWindow {
		a.baseRTT = a.baseWindowMin
		a.baseWindowMin = 0
		a.baseSince = now
	}
}

func (a *adaptiveCtrl) setClientHints(viewportWidth int, dpr float64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if viewportWidth <= 0 {
		return
	}
	if dpr <= 0 || math.IsNaN(dpr) || math.IsInf(dpr, 0) {
		dpr = 1
	}
	target := int(float64(viewportWidth) * minFloat(dpr, 2.5) * 1.15)
	target = max(640, min(a.maxWidth, target))
	a.clientWidth = target
	if a.profile == "auto" {
		a.targetWidth = target
		return
	}
	a.applyProfileLocked(a.profile)
}

func (a *adaptiveCtrl) setProfile(profile string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.applyProfileLocked(profile)
}

func (a *adaptiveCtrl) get() (fps, quality, width int, rtt time.Duration, profile string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.fps, a.quality, a.targetWidth, a.rtt, a.profile
}

// videoWidthCap — потолок ширины кадра для H.264-видеотрека.
//
// Здесь стояла константа 1280 прямо в цикле кодирования, и она резала картинку
// даже там, где тариф разрешает 1920: FullHD-экран уезжал в 1280 и растягивался
// обратно у зрителя — текст мылился на ровном месте. Потолок снизу оставлен
// прежним (1280): у зрителя с узким окном должно остаться, что разглядывать
// при увеличении. Сверху — сколько реально просит зритель, но не выше тарифа.
//
// Ширину НЕ трогает QoS-лесенка: при заторе H.264-путь режет fps и держит
// разрешение — для экранного содержимого это верный размен.
func (a *adaptiveCtrl) videoWidthCap() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	cap := a.clientWidth
	if cap < 1280 {
		cap = 1280
	}
	return min(cap, a.maxWidth)
}

// fpsCeiling — потолок fps для H.264-видеотрека: лицензионный максимум, но не
// выше 60 (быстрее скринкаст Chrome всё равно не отдаёт, а браузерному
// декодеру больше не нужно). JPEG-пути держат свои потолки сами — см.
// applyProfileLocked.
func (a *adaptiveCtrl) fpsCeiling() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return min(a.maxFPS, 60)
}

func (a *adaptiveCtrl) applyProfileLocked(profile string) {
	switch profile {
	case "smooth", "sharp", "saver":
		a.profile = profile
	default:
		a.profile = "auto"
	}
	target := a.clientWidth
	if target <= 0 {
		target = min(960, a.maxWidth)
	}
	switch a.profile {
	case "smooth":
		a.fps = min(a.maxFPS, 30)
		a.quality = min(a.maxQuality, 58)
		a.targetWidth = max(640, min(a.maxWidth, max(target, 960)))
	case "sharp":
		a.fps = min(a.maxFPS, 15)
		a.quality = min(a.maxQuality, 85)
		a.targetWidth = max(640, min(a.maxWidth, max(target, 1440)))
	case "saver":
		a.fps = min(a.maxFPS, 8)
		a.quality = min(a.maxQuality, 45)
		a.targetWidth = max(640, min(a.maxWidth, min(target, 800)))
	default:
		a.fps = max(8, min(a.maxFPS, a.fps))
		a.quality = max(45, min(a.maxQuality, a.quality))
		a.targetWidth = max(640, min(a.maxWidth, target))
	}
	a.goodStreak = 0
	a.badStreak = 0
}

// downshiftLocked отдаёт ОДНУ ступень за раз и в порядке «сначала плавность»:
// fps до 8, затем качество до 45, и только на дне обеих — ширина до 800.
// Раньше ступень била по всем трём сразу, и на дальнем маршруте кадр за
// секунды становился нечитаемым: 640 px при качестве 35.
func (a *adaptiveCtrl) downshiftLocked() {
	switch {
	case a.fps > 8:
		a.fps = max(8, a.fps-3)
	case a.quality > 45:
		a.quality = max(45, a.quality-8)
	case a.targetWidth > 800:
		a.targetWidth = max(800, a.targetWidth-160)
	default:
		a.fps = max(5, a.fps-1) // дно по чёткости — остаётся только плавность
	}
}

// upshiftLocked — зеркало downshift: сначала возвращаем чёткость (ширину и
// качество), плавность добираем последней. Потолок ширины — то, что реально
// нужно зрителю (clientWidth), выше него картинку некуда девать.
func (a *adaptiveCtrl) upshiftLocked() {
	want := a.clientWidth
	if want <= 0 {
		want = a.maxWidth
	}
	want = min(want, a.maxWidth)
	switch {
	case a.targetWidth < want:
		a.targetWidth = min(want, a.targetWidth+160)
	case a.quality < a.maxQuality:
		a.quality = min(a.maxQuality, a.quality+5)
	default:
		a.fps = min(a.maxFPS, a.fps+2)
	}
}

func minFloat(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}

// ── Отказ захвата экрана как честное состояние ─────────────────────
//
// Раньше ошибка захвата просто продолжала цикл (`continue`), и клиент вечно
// стоял на «Жду первый кадр» — без причины и без надежды. Короткие сбои
// действительно самолечатся за кадр-другой, поэтому сигнал уходит только после
// непрерывной серии отказов, а потом повторяется редко (кто подключился позже
// — тоже узнает).
const (
	captureFailGrace  = 3 * time.Second
	captureFailRepeat = 10 * time.Second
)

// captureFailStreak — серия подряд неудачных захватов экрана.
// Используется из одной горутины цикла захвата, синхронизация не нужна.
type captureFailStreak struct {
	since      time.Time
	reportedAt time.Time
}

// fail отмечает неудачный захват и говорит, надо ли ПРЯМО СЕЙЧАС сказать об
// этом клиенту.
func (c *captureFailStreak) fail(now time.Time) bool {
	if c.since.IsZero() {
		c.since = now
		return false
	}
	if now.Sub(c.since) < captureFailGrace {
		return false
	}
	if !c.reportedAt.IsZero() && now.Sub(c.reportedAt) < captureFailRepeat {
		return false
	}
	c.reportedAt = now
	return true
}

// ok сбрасывает серию после удачного захвата. Отдельного «всё снова хорошо»
// клиенту не нужно: следом полетят кадры, и плашка снимется сама.
func (c *captureFailStreak) ok() {
	c.since, c.reportedAt = time.Time{}, time.Time{}
}

// ── WebSocket screen handler ───────────────────────────────────────

func (s *Server) wsScreenHandler(w http.ResponseWriter, r *http.Request) {
	// ── Auth ──
	initData := r.URL.Query().Get("initData")
	uid := s.authenticateWS(initData)
	if uid == 0 {
		http.Error(w, "Unauthorized", 401)
		return
	}
	if len(s.allowed) > 0 && uid != -1 && !s.allowed[uid] {
		http.Error(w, "Forbidden", 403)
		return
	}

	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()

	// Keepalive: server pings + read deadline so a half-open / idle connection
	// (the user's throttled network) is detected instead of leaking the read
	// goroutine and capturing to a dead client.
	ka := wsutil.Start(conn)
	defer ka.Stop()

	log.Printf("[REMOTE] screen stream started uid=%d", uid)

	// Учёт стабильности соединения удалёнки: open + (с длительностью) close,
	// плюс RTT-сэмплы из адаптивного контроллера (см. sendStats).
	cs := connstat.Default.Open(connstat.KindRemote, "screen", uid)
	defer cs.Close("stream-ended")

	if service.RunAsService() && !vbrowser.Running() {
		log.Printf("[REMOTE] refusing screen stream in Windows service session uid=%d", uid)
		_ = conn.WriteJSON(map[string]any{
			"t":    "error",
			"code": "service_mode",
			"msg":  "Remote Desktop requires TGControl to run in the interactive user session.",
		})
		return
	}

	// ── Displays ──
	numDisplays := screenshot.NumActiveDisplays()
	if numDisplays == 0 {
		conn.WriteJSON(map[string]any{"t": "error", "code": "no_display", "msg": "no displays found"})
		return
	}
	displays := make([]map[string]any, numDisplays)
	for i := 0; i < numDisplays; i++ {
		b := screenshot.GetDisplayBounds(i)
		displays[i] = map[string]any{"id": i, "x": b.Min.X, "y": b.Min.Y, "w": b.Dx(), "h": b.Dy()}
	}

	// ── Shared state ──
	var mu sync.Mutex
	var writeMu sync.Mutex
	bounds := screenshot.GetDisplayBounds(0)
	screenW, screenH := bounds.Dx(), bounds.Dy()
	running := true
	var paused atomic.Bool
	// Зритель умеет накладывать куски кадра поверх холста.
	var wantTiles atomic.Bool
	// Зритель умеет декодировать H.264 сам (WebCodecs) — там, где WebRTC не
	// поднялся, это единственный способ дать ему видеокодек вместо JPEG.
	var wantH264 atomic.Bool
	// Подписка на системный звук компьютера: живёт, пока зритель её просит.
	var audioMu sync.Mutex
	var audioStop func()
	stopAudio := func() {
		audioMu.Lock()
		defer audioMu.Unlock()
		if audioStop != nil {
			audioStop()
			audioStop = nil
		}
	}
	defer stopAudio()
	// Просьба клиента прислать ключевой кадр (первый кадр, потеря, ресайз).
	var wantKeyframe atomic.Bool
	// Просьба забыть прошлый кадр: следующий уедет целиком.
	var tilesResync atomic.Bool
	var adapt *adaptiveCtrl
	if s.licenseManager != nil {
		// Лимиты качества — только для облачных подключений: дома без ограничений.
		limits := s.limitsFor(r)
		adapt = newAdaptiveCtrlWithLimits(limits.RemoteDesktopMaxFPS, limits.RemoteDesktopQuality, limits.RemoteDesktopMaxRes)
	} else {
		adapt = newAdaptiveCtrl()
	}
	ctrl := input.New()

	writeJSON := func(v any) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
		return conn.WriteJSON(v)
	}
	writeBin := func(data []byte) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
		return conn.WriteMessage(websocket.BinaryMessage, data)
	}

	// Send initial display info
	writeJSON(map[string]any{
		"t": "info", "sw": screenW, "sh": screenH, "displays": displays,
	})

	// Clipboard ops run on a single worker (depth-1 queue) so a flood of clip_*
	// messages can't spawn unbounded powershell/xclip processes (client-driven DoS).
	clipCh := make(chan func(), 1)
	clipDone := make(chan struct{})
	defer close(clipDone)
	go func() {
		defer observability.RecoverPanic("remote-clip-worker")
		for {
			select {
			case fn := <-clipCh:
				fn()
			case <-clipDone:
				return
			}
		}
	}()
	submitClip := func(fn func()) bool {
		select {
		case clipCh <- fn:
			return true
		default: // worker busy — drop this op rather than pile up processes
			return false
		}
	}

	// Input goes through the applier goroutine: click sleeps must not stall
	// this read loop, and move bursts coalesce to the latest position.
	// notify: предупреждения инъекции ввода уходят клиенту тем же control-каналом
	// (writeJSON сериализован своим мьютексом, звать из горутины applier можно).
	applier := newInputApplier(routeInput(ctrl), func() image.Rectangle {
		mu.Lock()
		screen := bounds
		mu.Unlock()
		// Координаты приходят долями кадра, а кадр может быть страницей
		// браузера, а не экраном целиком (см. remoteFrameBounds).
		return remoteFrameBounds(screen)
	}, clipDone, func(v map[string]any) { _ = writeJSON(v) })

	// ── Read loop (input + control messages) ──
	go func() {
		defer observability.RecoverPanic("remote-ws-read")
		defer func() {
			mu.Lock()
			running = false
			mu.Unlock()
			conn.Close() // unblock any in-flight capture-loop write immediately
		}()
		for {
			_, msg, err := conn.ReadMessage()
			if err != nil {
				return
			}
			ka.Touch() // active client → keep the read deadline fresh
			var evt map[string]any
			if err := json.Unmarshal(msg, &evt); err != nil {
				continue
			}
			t, _ := evt["t"].(string)

			switch t {
			case "ack":
				seq, _ := evt["seq"].(float64)
				dropped, _ := evt["dropped"].(float64)
				adapt.onACK(uint32(seq), int(dropped))

			case "client":
				vw, _ := evt["vw"].(float64)
				dpr, _ := evt["dpr"].(float64)
				adapt.setClientHints(int(vw), dpr)
				// Частичные кадры — только по явному согласию клиента: старая
				// сборка ждёт в бинарном сообщении голый JPEG и на новый формат
				// показала бы пустой экран.
				if h264, ok := evt["h264"].(bool); ok {
					wantH264.Store(h264)
					wantKeyframe.Store(true) // декодер клиента стартует только с IDR
				}
				if tiles, ok := evt["tiles"].(bool); ok {
					wantTiles.Store(tiles)
					// Трекер живёт в горутине отправки — трогаем его не отсюда,
					// а флагом: холст у зрителя ещё пуст, первый кадр должен
					// уехать целиком.
					tilesResync.Store(true)
				}

			case "audio":
				// Звук включают явно: он неожиданно громкий, если человек его
				// не просил, и стоит трафика даже при 384 кбит/с.
				on, _ := evt["on"].(bool)
				if !on {
					stopAudio()
					break
				}
				if !audio.Available() {
					_ = writeJSON(map[string]any{"t": "audio", "available": false})
					break
				}
				audioMu.Lock()
				already := audioStop != nil
				audioMu.Unlock()
				if already {
					break
				}
				packets, unsub, aerr := audio.Subscribe()
				if aerr != nil {
					_ = writeJSON(map[string]any{"t": "audio", "available": false})
					break
				}
				audioMu.Lock()
				audioStop = unsub
				audioMu.Unlock()
				_ = writeJSON(map[string]any{
					"t": "audio", "available": true,
					"rate": audio.SampleRate, "channels": audio.Channels,
				})
				go func() {
					// Кадр звука: [0:4]=0 (номера у звука нет — его не
					// подтверждают и не пропускают), [4]=0x03, дальше PCM.
					for pkt := range packets {
						frame := make([]byte, 5, 5+len(pkt))
						frame[4] = frameKindAudio
						frame = append(frame, pkt...)
						if err := writeBin(frame); err != nil {
							return
						}
					}
				}()

			case "idr":
				// Декодер клиента потерял поток (свернули вкладку, дыра в
				// сети) — без ключевого кадра он не восстановится сам.
				wantKeyframe.Store(true)

			case "profile":
				profile, _ := evt["profile"].(string)
				adapt.setProfile(profile)

			case "pause":
				paused.Store(true)

			case "resume":
				paused.Store(false)

			case "ping":
				writeJSON(map[string]any{"t": "pong", "ts": evt["ts"]})

			case "display":
				id, _ := evt["id"].(float64)
				did := int(id)
				if did >= 0 && did < numDisplays {
					mu.Lock()
					b := screenshot.GetDisplayBounds(did)
					bounds = b
					screenW, screenH = b.Dx(), b.Dy()
					mu.Unlock()
					writeJSON(map[string]any{
						"t": "info", "sw": screenW, "sh": screenH, "displays": displays,
					})
				}

			case "clip_get":
				if !submitClip(func() {
					text, code := clipboardRead()
					if code == "clipboard_empty" {
						writeJSON(map[string]any{"t": "clip", "text": "", "empty": true})
					} else if code != "" {
						writeJSON(map[string]any{"t": "clip_error", "op": "read", "code": code})
					} else {
						writeJSON(map[string]any{"t": "clip", "text": text})
					}
				}) {
					writeJSON(map[string]any{"t": "clip_error", "op": "read", "code": "clipboard_busy"})
				}

			case "clip_set":
				text, _ := evt["text"].(string)
				if text != "" {
					if !submitClip(func() {
						if code := clipboardWrite(text); code != "" {
							writeJSON(map[string]any{"t": "clip_error", "op": "set_text", "code": code})
						} else {
							writeJSON(map[string]any{"t": "clip_result", "op": "set_text", "ok": true})
						}
					}) {
						writeJSON(map[string]any{"t": "clip_error", "op": "set_text", "code": "clipboard_busy"})
					}
				}

			case "clip_set_image":
				data, _ := evt["data"].(string)
				if data != "" {
					if !submitClip(func() {
						if code := clipboardWriteImage(data); code != "" {
							writeJSON(map[string]any{"t": "clip_error", "op": "set_image", "code": code})
						} else {
							writeJSON(map[string]any{"t": "clip_result", "op": "set_image", "ok": true})
						}
					}) {
						writeJSON(map[string]any{"t": "clip_error", "op": "set_image", "code": "clipboard_busy"})
					}
				}

			case "m", "s", "k", "txt", "tp", "paste":
				applier.Route(evt)
			}
		}
	}()

	// ── Capture → encode → send loop ──
	// Ворота одинаковых кадров: сравнение по сырым пикселям, ДО кодирования
	// (раньше хэш считался по готовому JPEG — то есть работа уже была сделана).
	var gate frameGate
	// Кодер H.264 для зрителей с WebCodecs (см. remote_video_ws.go).
	video := newWSVideoEncoder()
	defer video.close()
	// Источник кадров этого зрителя: на Windows он берёт готовые кадры у
	// общего насоса DXGI (один захват на всех), иначе снимает экран сам.
	source := newScreenSource(bounds)
	// Хэши тайлов прошлого кадра: по ним считается, что именно перерисовывать
	// у зрителя (remote_tiles.go). Работает только с клиентом, который умеет
	// частичные кадры и сам об этом сказал в {t:"client"}.
	var tracker tileTracker
	// Номер последнего кадра, взятого у самого браузера (см. nextRemoteFrame).
	var cdpSeq uint64
	var seq uint32
	buf := new(bytes.Buffer)
	seqBuf := make([]byte, 4)
	statsWindow := time.Now()
	lastFrameSent := time.Time{}
	var statsBytes int64
	var statsFrames int
	var statsSkipped int
	var lastCaptureMs int64
	var lastEncodeMs int64
	var capFail captureFailStreak

	sendStats := func() error {
		curFPS, curQ, curW, curRTT, profile := adapt.get()
		cs.RTT(curRTT) // фиксируем задержку для диагностики стабильности
		elapsed := time.Since(statsWindow).Seconds()
		sentFPS := 0.0
		kbps := 0.0
		if elapsed > 0 {
			sentFPS = float64(statsFrames) / elapsed
			kbps = float64(statsBytes*8) / elapsed / 1000
		}
		err := writeJSON(map[string]any{
			"t":         "stats",
			"rtt":       curRTT.Milliseconds(),
			"fps":       curFPS,
			"sentFps":   math.Round(sentFPS*10) / 10,
			"quality":   curQ,
			"width":     curW,
			"kbps":      math.Round(kbps),
			"skipped":   statsSkipped,
			"captureMs": lastCaptureMs,
			"encodeMs":  lastEncodeMs,
			"profile":   profile,
		})
		statsWindow = time.Now()
		statsBytes = 0
		statsFrames = 0
		statsSkipped = 0
		return err
	}

	// Просмотр закончился — поток кадров браузера тоже (см. browserStreamStopped).
	defer browserStreamStopped()

	// Зритель появился/ушёл — счёт для автоостановки браузера по простою.
	browserViewerJoined()
	defer browserViewerLeft()

	// События страницы (курсор встал в поле ввода, началась загрузка, скачался
	// файл) уходят тем же управляющим каналом. Клавиатура телефона открывается
	// именно по ним — как в настоящем браузере, где по тапу в поле она
	// выезжает сама.
	defer subscribeBrowserNotices(func(v map[string]any) { _ = writeJSON(v) })()

	loopMark := time.Now()
	for {
		mu.Lock()
		if !running {
			mu.Unlock()
			break
		}
		curBounds := bounds
		mu.Unlock()
		if paused.Load() {
			time.Sleep(100 * time.Millisecond)
			loopMark = time.Now()
			continue
		}

		curFPS, curQ, curW, _, curProfile := adapt.get()
		// Пока палец ведёт по экрану, важнее частота, чем чёткость: замерший
		// на пол-секунды кадр во время прокрутки читается как «подвисло», а
		// чуть более крупное зерно в движении не видно вовсе.
		curFPS, curQ = boostWhileInteracting(applier, curFPS, curQ)
		// Pace to target FPS: sleep the remainder of the frame period, compensating
		// for the capture/encode/send time of the previous iteration. loopMark is
		// reset right after the sleep, so skipped/duplicate frames still sleep a
		// near-full period instead of busy-looping.
		period := time.Second / time.Duration(curFPS)
		if d := period - time.Since(loopMark); d > 0 {
			time.Sleep(d)
		}
		loopMark = time.Now()

		mu.Lock()
		if !running {
			mu.Unlock()
			break
		}
		mu.Unlock()

		// Экран неподвижен И кадр недавно улетал — пропускаем всё целиком, не
		// трогая ни X, ни кодек. Вопрос «менялось ли» стоит доли миллисекунды
		// (X-расширение DAMAGE), а захват — 4 МБ через сокет; на VPS с одним
		// ядром эта разница и есть разница между «браузер тормозит» и «нет».
		// Раз в keep-alive кадр отправляем в любом случае: DAMAGE покрывает не
		// каждый мыслимый способ изменить картинку, залипнуть нельзя.
		keepAlive := time.Since(lastFrameSent) >= remoteStaticKeepAlive
		// Проверка через X имеет смысл, только если кадр берётся с экрана: у
		// браузера свой признак новизны (номер кадра), и DAMEGE-опрос там лишь
		// мешал бы — на неподвижном экране он запретил бы забрать уже готовый
		// кадр, присланный самим Chrome.
		if !keepAlive && !browserFramesActive() && !source.Changed(curBounds) {
			statsSkipped++
			if time.Since(statsWindow) > 2*time.Second {
				if err := sendStats(); err != nil {
					break
				}
			}
			continue
		}

		var regions []encodedRegion
		var videoNAL []byte
		var videoKey bool
		var capMs, encMs int64
		var changed bool
		var err error
		if wantH264.Load() && video.available() && !browserFramesActive() {
			// Видеокодек кормится тем же источником, но кадр целиком: H.264 сам
			// кодирует только разницу, тайлить его нечего.
			capStart := time.Now()
			var img *image.RGBA
			img, err = source.Grab(curBounds)
			capMs = time.Since(capStart).Milliseconds()
			if err == nil {
				encStart := time.Now()
				var reconfigured bool
				videoNAL, videoKey, reconfigured, err = video.encode(
					img, curW, curFPS, curProfile, wantKeyframe.Swap(false))
				encMs = time.Since(encStart).Milliseconds()
				if err == nil && reconfigured {
					// Размер кадра сменился — клиент пересобирает декодер.
					if werr := writeJSON(map[string]any{
						"t": "video", "codec": "h264", "w": video.w, "h": video.h,
					}); werr != nil {
						break
					}
				}
				changed = len(videoNAL) > 0
			}
			if err != nil {
				// Кодек отвалился (нет библиотеки, сменился экран) — не оставляем
				// зрителя без картинки: возвращаем его на кадры-картинки.
				log.Printf("[REMOTE] H.264 по веб-сокету не вышел (%v) — переходим на JPEG", err)
				wantH264.Store(false)
				video.close()
				err = nil
				continue
			}
		} else if wantTiles.Load() {
			if tilesResync.Swap(false) {
				tracker.reset()
			}
			capMs, encMs, changed, regions, err = nextRemoteFrameTiled(
				curBounds, curW, curQ, buf, &gate, &tracker, source, &cdpSeq, keepAlive)
		} else {
			capMs, encMs, changed, err = nextRemoteFrame(curBounds, curW, curQ, buf, &gate, &cdpSeq, keepAlive)
		}
		if err != nil {
			// Кадров нет и не будет, пока захват падает: скажем причину вместо
			// вечного «Жду первый кадр» (клиент переводит код).
			if capFail.fail(time.Now()) {
				log.Printf("[REMOTE] capture failing for uid=%d: %v", uid, err)
				if werr := writeJSON(map[string]any{
					"t": "error", "code": "capture_failed", "msg": err.Error(),
				}); werr != nil {
					break
				}
			}
			continue
		}
		capFail.ok()
		lastCaptureMs = capMs
		lastEncodeMs = encMs

		// Пиксели те же, что в прошлом отправленном кадре: кодирования не было,
		// отправлять нечего.
		if !changed {
			statsSkipped++
			if time.Since(statsWindow) > 2*time.Second {
				if err := sendStats(); err != nil {
					break
				}
			}
			continue
		}

		seq++
		adapt.onSend(seq)

		// Полный кадр: [seq:4 BE][jpeg…]. Частичный: [seq:4 BE][0x01][count]…
		// (см. packRegionFrame) — различаются по первому байту после номера.
		var frame []byte
		if len(videoNAL) > 0 {
			frame = packVideoFrame(seq, videoNAL, videoKey)
		} else if len(regions) > 0 {
			frame = packRegionFrame(seq, regions)
		} else {
			binary.BigEndian.PutUint32(seqBuf, seq)
			frame = make([]byte, 4+buf.Len())
			copy(frame[:4], seqBuf)
			copy(frame[4:], buf.Bytes())
		}

		if err := writeBin(frame); err != nil {
			break
		}
		lastFrameSent = time.Now()
		statsBytes += int64(len(frame))
		statsFrames++

		// Send adaptive stats to client every 2 seconds
		if time.Since(statsWindow) > 2*time.Second {
			if err := sendStats(); err != nil {
				break
			}
		}
	}

	log.Printf("[REMOTE] screen stream ended uid=%d", uid)
}

// processInput handles mouse/scroll/keyboard input with NaN/Inf guard and coord
// clamping. Возвращает ошибку контроллера ввода: на заблокированном рабочем
// столе (или при активном UAC) инъекция молча отбрасывается операционной
// системой, и раньше об этом не узнавал никто — человек тыкал в замерший кадр и
// решал, что программа сломалась. Теперь ошибка идёт наверх, в inputApplier,
// который отправляет клиенту машинный код по control-каналу.
// remoteStaticKeepAlive — как часто неподвижный экран всё равно уезжает
// клиенту. Нужен по двум причинам: только что подключившийся зритель обязан
// увидеть картинку, не дожидаясь, пока на экране что-то шевельнётся, и слежение
// за изменениями (X DAMAGE) не покрывает совсем экзотические перерисовки —
// залипнуть на устаревшем кадре нельзя.
const remoteStaticKeepAlive = 2 * time.Second

func processInput(ctrl input.Controller, evt map[string]any, bounds image.Rectangle) error {
	t, _ := evt["t"].(string)

	switch t {
	case "m":
		screenW, screenH := bounds.Dx(), bounds.Dy()
		if screenW <= 0 || screenH <= 0 {
			return nil
		}
		rx, _ := evt["x"].(float64)
		ry, _ := evt["y"].(float64)
		if math.IsNaN(rx) || math.IsInf(rx, 0) {
			rx = 0.5
		}
		if math.IsNaN(ry) || math.IsInf(ry, 0) {
			ry = 0.5
		}
		rx = math.Max(0, math.Min(1, rx))
		ry = math.Max(0, math.Min(1, ry))
		x := bounds.Min.X + max(0, min(screenW-1, int(rx*float64(screenW))))
		y := bounds.Min.Y + max(0, min(screenH-1, int(ry*float64(screenH))))

		action, _ := evt["a"].(string)
		button, _ := evt["b"].(string)
		if button == "" {
			button = "left"
		}
		switch action {
		case "move":
			return ctrl.MouseMove(x, y)
		case "click":
			return ctrl.MouseClick(x, y, button)
		case "dblclick":
			return ctrl.MouseDoubleClick(x, y)
		case "down":
			return ctrl.MouseDown(x, y, button)
		case "up":
			return ctrl.MouseUp(x, y, button)
		}

	case "s":
		dy, _ := evt["dy"].(float64)
		dx, _ := evt["dx"].(float64)
		if math.IsNaN(dy) || math.IsInf(dy, 0) || math.IsNaN(dx) || math.IsInf(dx, 0) {
			return nil
		}
		if int(dy) != 0 {
			if err := ctrl.Scroll(int(dy)); err != nil {
				return err
			}
		}
		// Горизонталь (с 2.61.18): старый клиент dx не шлёт — ноль, ничего не
		// делаем; контроллер без такого умения тоже молчит (input.ScrollH).
		return input.ScrollH(ctrl, int(dx))

	case "k":
		key, _ := evt["k"].(string)
		if key == "" {
			return nil
		}
		if down, _ := evt["d"].(bool); down {
			return ctrl.KeyDown(key)
		}
		return ctrl.KeyUp(key)

	case "txt":
		if text, _ := evt["text"].(string); text != "" {
			return ctrl.TypeText(text)
		}

	case "paste":
		// Вставка из буфера — не набор: она приходит одним куском и не должна
		// разыгрываться по клавише (страница получила бы сотню keydown вместо
		// обычной вставки). Приёмник, который вставлять не умеет, печатает.
		text, _ := evt["text"].(string)
		if text == "" {
			return nil
		}
		if p, ok := ctrl.(pasteCapable); ok {
			return p.PasteText(text)
		}
		return ctrl.TypeText(text)

	case "tp":
		// Касания. Страница, получившая настоящий touch, ведёт себя как на
		// телефоне: свайпы, жесты карт и каруселей, подсветка нажатия. Если
		// принимающая сторона касаний не умеет (обычный компьютер вместо
		// браузера), переводим их в мышь — иначе экран просто перестал бы
		// слушаться.
		return applyTouch(ctrl, evt, bounds)
	}
	return nil
}

// touchCapable — приёмник, умеющий настоящие касания (см. cdp.Controller).
type touchCapable interface {
	TouchPoints(kind string, points [][2]float64) error
}

// pasteCapable — приёмник, различающий вставку и набор (страница умеет, а
// клавиатура операционной системы — нет).
type pasteCapable interface {
	PasteText(text string) error
}

// applyTouch переводит доли кадра в координаты и отправляет касание.
func applyTouch(ctrl input.Controller, evt map[string]any, bounds image.Rectangle) error {
	w, h := bounds.Dx(), bounds.Dy()
	if w <= 0 || h <= 0 {
		return nil
	}
	action, _ := evt["a"].(string)
	rawPoints, _ := evt["p"].([]any)
	points := make([][2]float64, 0, len(rawPoints))
	for _, item := range rawPoints {
		pt, ok := item.(map[string]any)
		if !ok {
			continue
		}
		rx, _ := pt["x"].(float64)
		ry, _ := pt["y"].(float64)
		if math.IsNaN(rx) || math.IsNaN(ry) || math.IsInf(rx, 0) || math.IsInf(ry, 0) {
			continue
		}
		rx = math.Max(0, math.Min(1, rx))
		ry = math.Max(0, math.Min(1, ry))
		points = append(points, [2]float64{
			float64(bounds.Min.X) + rx*float64(w),
			float64(bounds.Min.Y) + ry*float64(h),
		})
	}

	if touch, ok := ctrl.(touchCapable); ok {
		err := touch.TouchPoints(action, points)
		if err == nil {
			return nil
		}
		if !errors.Is(err, errNoTouch) {
			return err
		}
		// Касания не приняты — ниже тот же жест уходит мышью.
	}
	// Запасной путь — мышь: у обычного рабочего стола касаний нет.
	if len(points) == 0 {
		return nil
	}
	x, y := int(points[0][0]), int(points[0][1])
	switch action {
	case "start":
		return ctrl.MouseDown(x, y, "left")
	case "move":
		return ctrl.MouseMove(x, y)
	case "end":
		return ctrl.MouseUp(x, y, "left")
	}
	return nil
}

// scaleDown resizes an RGBA image to targetWidth keeping aspect ratio.
func scaleDown(src *image.RGBA, targetWidth int) image.Image {
	srcW := src.Bounds().Dx()
	srcH := src.Bounds().Dy()
	if targetWidth <= 0 || srcW <= 0 || srcH <= 0 {
		return src
	}
	if srcW <= targetWidth {
		return src
	}
	ratio := float64(targetWidth) / float64(srcW)
	targetHeight := int(float64(srcH) * ratio)
	dst := image.NewRGBA(image.Rect(0, 0, targetWidth, targetHeight))
	// Src, not Over: screen captures are opaque; Over pays an extra read+blend
	// of the (zeroed) destination per pixel for an identical result.
	draw.ApproxBiLinear.Scale(dst, dst.Bounds(), src, src.Bounds(), draw.Src, nil)
	return dst
}

// ── Clipboard helpers ──────────────────────────────────────────────

const remoteClipboardImageMax = 200 << 10

// Clipboard helpers return a stable machine code instead of collapsing every
// failure into an empty string. The client can now distinguish an actually
// empty clipboard from a headless Linux agent where xclip is not installed.
//
// Все три помощника запускаются БЕЗ окна: буфер обмена синхронизируется в фоне
// удалёнки, по нескольку раз за сеанс, и человек за ПК не должен видеть ни
// одной вспышки. На Windows окно гасит powershellCommand, на Linux —
// procutil.Hidden (там это no-op, стоит ради единого правила).
func clipboardRead() (string, string) {
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = powershellCommand("-NoProfile", "-command", "Get-Clipboard")
	} else {
		if _, err := exec.LookPath("xclip"); err != nil {
			return "", "xclip_missing"
		}
		cmd = procutil.Hidden(exec.Command("xclip", "-selection", "clipboard", "-o"))
	}
	out, err := cmd.Output()
	if err != nil {
		return "", "clipboard_failed"
	}
	text := strings.TrimRight(string(out), "\r\n")
	if text == "" {
		return "", "clipboard_empty"
	}
	return text, ""
}

func clipboardWrite(text string) string {
	if runtime.GOOS != "windows" {
		if _, err := exec.LookPath("xclip"); err != nil {
			return "xclip_missing"
		}
	}
	var err error
	if runtime.GOOS == "windows" {
		cmd := powershellCommand("-NoProfile", "-command",
			"Set-Clipboard([Console]::In.ReadToEnd())")
		cmd.Stdin = strings.NewReader(text)
		err = cmd.Run()
	} else {
		cmd := procutil.Hidden(exec.Command("xclip", "-selection", "clipboard"))
		cmd.Stdin = strings.NewReader(text)
		err = cmd.Run()
	}
	if err != nil {
		return "clipboard_failed"
	}
	return ""
}

func clipboardWriteImage(b64data string) string {
	raw, err := base64.StdEncoding.DecodeString(b64data)
	if err != nil {
		log.Println("clipboardWriteImage: decode error:", err)
		return "invalid_image"
	}
	if len(raw) > remoteClipboardImageMax {
		return "image_too_large"
	}
	if runtime.GOOS != "windows" {
		if _, err := exec.LookPath("xclip"); err != nil {
			return "xclip_missing"
		}
	}

	// Уникальный файл 0600 на вызов (os.CreateTemp) вместо общего предсказуемого
	// tgcontrol_clip.png 0644 — без гонки и TOCTOU на multi-user хосте.
	f, err := os.CreateTemp("", "tgcontrol_clip_*.png")
	if err != nil {
		log.Println("clipboardWriteImage: temp create error:", err)
		return "clipboard_failed"
	}
	tmpFile := f.Name()
	defer os.Remove(tmpFile)
	_, werr := f.Write(raw)
	f.Close()
	if werr != nil {
		log.Println("clipboardWriteImage: write temp error:", werr)
		return "clipboard_failed"
	}

	if runtime.GOOS == "windows" {
		ps := fmt.Sprintf(
			"Add-Type -Assembly System.Windows.Forms; "+
				"$img = [System.Drawing.Image]::FromFile('%s'); "+
				"[System.Windows.Forms.Clipboard]::SetImage($img); "+
				"$img.Dispose()",
			strings.ReplaceAll(tmpFile, "'", "''"))
		cmd := powershellCommand("-NoProfile", "-sta", "-command", ps)
		if out, err := cmd.CombinedOutput(); err != nil {
			log.Println("clipboardWriteImage:", err, string(out))
			return "clipboard_failed"
		}
	} else {
		cmd := procutil.Hidden(exec.Command("xclip", "-selection", "clipboard", "-t", "image/png", "-i", tmpFile))
		if err := cmd.Run(); err != nil {
			log.Println("clipboardWriteImage:", err)
			return "clipboard_failed"
		}
	}
	return ""
}
