package web

import (
	"bytes"
	"hash/crc32"
	"image"
	"image/jpeg"
	"net/http"
	"strconv"
	"time"

	"github.com/kbinani/screenshot"

	"tgcontrol/internal/service"
	"tgcontrol/internal/vbrowser"
)

// captureJPEG grabs the given display bounds, downscales to targetWidth (keeping
// aspect) and JPEG-encodes into buf. Shared by the WebSocket (/ws/screen) and
// the WebRTC frames-DataChannel producers so both pipelines capture+encode
// identically. Returns capture/encode durations in milliseconds for stats.
//
// NOTE: this is the GDI BitBlt + CPU JPEG path. The DXGI dirty-rect capture and
// the Media Foundation H.264 encoder (M2) plug in behind the same call sites via
// the capture.Source / codec.Encoder interfaces; until then both transports use
// this.
func captureJPEG(bounds image.Rectangle, targetWidth, quality int, buf *bytes.Buffer) (capMs, encMs int64, err error) {
	captureStart := time.Now()
	img, err := grabScreen(bounds)
	if err != nil {
		return 0, 0, err
	}
	capMs = time.Since(captureStart).Milliseconds()

	scaled := scaleDown(img, targetWidth)

	buf.Reset()
	encodeStart := time.Now()
	if err := jpeg.Encode(buf, scaled, &jpeg.Options{Quality: quality}); err != nil {
		return capMs, 0, err
	}
	encMs = time.Since(encodeStart).Milliseconds()
	return capMs, encMs, nil
}

// frameGate — «этот кадр уже отправляли». Сравнение идёт по СЫРЫМ пикселям, до
// масштабирования и кодирования.
//
// Так было не всегда: JPEG-путь считал хэш по уже готовому JPEG, то есть на
// неподвижной странице честно делал захват (4 МБ через X-сокет), масштабирование
// и кодирование — и выбрасывал результат. Экономился только трафик. На машине с
// одним ядром (типовой VPS) это ядро отбиралось у самого браузера: страница
// тормозила ровно потому, что мы усердно кодировали её неподвижную картинку.
// H.264-ветка так проверяла всегда (crc32 по img.Pix) — здесь тот же приём.
type frameGate struct {
	hash    uint32
	width   int
	quality int
	has     bool
}

// unchanged — кадр совпал с прошлым отправленным И параметры кодирования те же.
// Смена ширины/качества (адаптив по каналу) обязана перекодировать даже
// пиксель-в-пиксель тот же экран, иначе клиент застрянет на старом размере.
func (g *frameGate) unchanged(hash uint32, width, quality int) bool {
	return g.has && g.hash == hash && g.width == width && g.quality == quality
}

func (g *frameGate) remember(hash uint32, width, quality int) {
	g.hash, g.width, g.quality, g.has = hash, width, quality, true
}

// captureChangedJPEG захватывает экран и кодирует кадр ТОЛЬКО если картинка
// (или параметры кодирования) изменились. changed=false означает «на экране всё
// то же самое» — вызывающий пропускает кадр, не тратя ни масштабирование, ни
// JPEG. force=true нужен для keep-alive: даже неподвижный экран раз в пару
// секунд обязан долететь до клиента, который только что подключился.
func captureChangedJPEG(
	bounds image.Rectangle, targetWidth, quality int, buf *bytes.Buffer,
	gate *frameGate, force bool,
) (capMs, encMs int64, changed bool, err error) {
	captureStart := time.Now()
	img, err := grabScreen(bounds)
	if err != nil {
		return 0, 0, false, err
	}
	capMs = time.Since(captureStart).Milliseconds()

	hash := crc32.Checksum(img.Pix, castagnoliTable)
	if !force && gate.unchanged(hash, targetWidth, quality) {
		return capMs, 0, false, nil
	}

	scaled := scaleDown(img, targetWidth)
	buf.Reset()
	encodeStart := time.Now()
	if err := jpeg.Encode(buf, scaled, &jpeg.Options{Quality: quality}); err != nil {
		return capMs, 0, false, err
	}
	encMs = time.Since(encodeStart).Milliseconds()
	gate.remember(hash, targetWidth, quality)
	return capMs, encMs, true, nil
}

// captureChangedRegions — тот же захват, но кадр уезжает КУСКАМИ: кодируются и
// отправляются только изменившиеся области (см. remote_tiles.go). Быстрый выход
// прежний — crc32 всего кадра до масштабирования: на неподвижном экране мы не
// платим ни за scaleDown, ни за хэши тайлов.
//
// regions=nil при changed=true означает «кадр целиком лежит в buf» — первый
// кадр зрителя, keep-alive, смена параметров или слишком много изменений.
func captureChangedRegions(
	bounds image.Rectangle, targetWidth, quality int, buf *bytes.Buffer,
	gate *frameGate, tracker *tileTracker, source *screenSource, force bool,
) (capMs, encMs int64, changed bool, regions []encodedRegion, err error) {
	captureStart := time.Now()
	img, err := source.Grab(bounds)
	if err != nil {
		return 0, 0, false, nil, err
	}
	capMs = time.Since(captureStart).Milliseconds()

	hash := crc32.Checksum(img.Pix, castagnoliTable)
	if !force && gate.unchanged(hash, targetWidth, quality) {
		return capMs, 0, false, nil, nil
	}

	scaled, ok := scaleDown(img, targetWidth).(*image.RGBA)
	if !ok { // масштабирование вернуло не RGBA — работаем прежним путём
		return captureChangedJPEGFrom(img, targetWidth, quality, buf, gate, hash, capMs)
	}

	// Keep-alive и новый зритель обязаны получить полный кадр: класть куски
	// поверх чужого (или отсутствующего) холста нельзя.
	if force {
		tracker.reset()
	}

	encodeStart := time.Now()
	regs, full := tracker.diff(scaled, quality)
	if !full && len(regs) == 0 {
		gate.remember(hash, targetWidth, quality)
		return capMs, 0, false, nil, nil
	}
	if full {
		buf.Reset()
		if err := jpeg.Encode(buf, scaled, &jpeg.Options{Quality: quality}); err != nil {
			return capMs, 0, false, nil, err
		}
		gate.remember(hash, targetWidth, quality)
		return capMs, time.Since(encodeStart).Milliseconds(), true, nil, nil
	}

	encoded, err := encodeRegions(scaled, regs, quality)
	if err != nil {
		return capMs, 0, false, nil, err
	}
	gate.remember(hash, targetWidth, quality)
	return capMs, time.Since(encodeStart).Milliseconds(), true, encoded, nil
}

// captureChangedJPEGFrom — хвост прежнего пути для уже снятого кадра.
func captureChangedJPEGFrom(
	img *image.RGBA, targetWidth, quality int, buf *bytes.Buffer,
	gate *frameGate, hash uint32, capMs int64,
) (int64, int64, bool, []encodedRegion, error) {
	scaled := scaleDown(img, targetWidth)
	buf.Reset()
	encodeStart := time.Now()
	if err := jpeg.Encode(buf, scaled, &jpeg.Options{Quality: quality}); err != nil {
		return capMs, 0, false, nil, err
	}
	gate.remember(hash, targetWidth, quality)
	return capMs, time.Since(encodeStart).Milliseconds(), true, nil, nil
}

// castagnoliTable — та же таблица, что в H.264-ветке: на x86 crc32c уходит в
// аппаратную инструкцию, а 4 МБ кадра хэшируются на каждом такте.
var castagnoliTable = crc32.MakeTable(crc32.Castagnoli)

func remotePreviewWidth(raw string) int {
	width, err := strconv.Atoi(raw)
	if err != nil || width == 0 {
		return 640
	}
	if width < 320 {
		return 320
	}
	if width > 1280 {
		return 1280
	}
	return width
}

// apiRemotePreview returns one low-quality frame while WebRTC/WS is still
// negotiating. It makes a slow connection visibly progressive without opening
// a second streaming session or keeping a capture loop alive.
func (s *Server) apiRemotePreview(w http.ResponseWriter, r *http.Request, _ int64) {
	if service.RunAsService() && !vbrowser.Running() {
		jsonErrorCode(w, http.StatusConflict, "service_mode",
			"Remote Desktop requires TGControl to run in the interactive user session.", nil)
		return
	}
	if screenshot.NumActiveDisplays() == 0 {
		jsonErrorCode(w, http.StatusServiceUnavailable, "no_display", "no displays found", nil)
		return
	}

	var buf bytes.Buffer
	if _, _, err := captureJPEG(
		screenshot.GetDisplayBounds(0),
		remotePreviewWidth(r.URL.Query().Get("w")),
		55,
		&buf,
	); err != nil {
		jsonErrorCode(w, http.StatusServiceUnavailable, "capture_failed", "screen preview failed", nil)
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Length", strconv.Itoa(buf.Len()))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(buf.Bytes())
}
