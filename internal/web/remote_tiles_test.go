package web

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"testing"
)

func newTestFrame(w, h int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x % 251), G: uint8(y % 253), B: 40, A: 255})
		}
	}
	return img
}

func fill(img *image.RGBA, r image.Rectangle, c color.RGBA) {
	for y := r.Min.Y; y < r.Max.Y; y++ {
		for x := r.Min.X; x < r.Max.X; x++ {
			img.Set(x, y, c)
		}
	}
}

func TestFirstFrameGoesWhole(t *testing.T) {
	var tr tileTracker
	_, full := tr.diff(newTestFrame(1920, 1080), 85)
	if !full {
		t.Fatal("первый кадр обязан уехать целиком: класть куски ещё некуда")
	}
}

func TestUnchangedFrameSendsNothing(t *testing.T) {
	var tr tileTracker
	img := newTestFrame(1920, 1080)
	tr.diff(img, 85)

	regions, full := tr.diff(img, 85)
	if full || len(regions) != 0 {
		t.Fatalf("неподвижный экран: full=%v, областей %d — ждали пусто", full, len(regions))
	}
}

// Главный случай ради которого всё: печать в терминале меняет проценты экрана.
func TestSmallChangeSendsSmallArea(t *testing.T) {
	var tr tileTracker
	img := newTestFrame(1920, 1080)
	tr.diff(img, 85)

	// Строка терминала: 600×20 где-то в середине.
	fill(img, image.Rect(300, 500, 900, 520), color.RGBA{R: 255, A: 255})
	regions, full := tr.diff(img, 85)
	if full {
		t.Fatal("изменение одной строки не должно требовать полного кадра")
	}
	if len(regions) != 1 {
		t.Fatalf("ждали одну область, получили %d: %v", len(regions), regions)
	}
	area := regions[0].Dx() * regions[0].Dy()
	full1080 := 1920 * 1080
	if share := float64(area) / float64(full1080); share > 0.10 {
		t.Errorf("область раздута: %d px = %.1f%% кадра", area, share*100)
	}
	if !regions[0].Overlaps(image.Rect(300, 500, 900, 520)) {
		t.Errorf("область не покрывает изменение: %v", regions[0])
	}
}

// Изменения по всей ширине обязаны склеиться в ОДИН кусок, а не в 15 тайлов.
func TestHorizontalStripMergesIntoOneRegion(t *testing.T) {
	var tr tileTracker
	img := newTestFrame(1920, 1080)
	tr.diff(img, 85)

	fill(img, image.Rect(0, 400, 1920, 460), color.RGBA{G: 255, A: 255})
	regions, full := tr.diff(img, 85)
	if full {
		t.Fatal("полоса в 60 px — это не полный кадр")
	}
	if len(regions) != 1 {
		t.Fatalf("полоса разъехалась на %d кусков: %v", len(regions), regions)
	}
}

// Соседние ряды с одинаковым горизонтальным охватом склеиваются по вертикали.
func TestVerticalMergeKeepsOneRegion(t *testing.T) {
	var tr tileTracker
	img := newTestFrame(1920, 1080)
	tr.diff(img, 85)

	fill(img, image.Rect(200, 100, 500, 700), color.RGBA{B: 255, A: 255})
	regions, full := tr.diff(img, 85)
	if full {
		t.Fatal("столбик не должен требовать полного кадра")
	}
	if len(regions) != 1 {
		t.Fatalf("вертикальное слияние не сработало: %d кусков %v", len(regions), regions)
	}
}

func TestBigChangeFallsBackToFullFrame(t *testing.T) {
	var tr tileTracker
	img := newTestFrame(1920, 1080)
	tr.diff(img, 85)

	fill(img, image.Rect(0, 0, 1920, 800), color.RGBA{R: 10, G: 200, B: 30, A: 255})
	_, full := tr.diff(img, 85)
	if !full {
		t.Fatal("изменилось три четверти экрана — целый кадр дешевле кусков")
	}
}

// Смена качества/ширины обязана перерисовать всё: у клиента на холсте лежит
// картинка, снятая в других параметрах.
func TestQualityChangeForcesFullFrame(t *testing.T) {
	var tr tileTracker
	img := newTestFrame(1280, 720)
	tr.diff(img, 85)

	if _, full := tr.diff(img, 60); !full {
		t.Fatal("другое качество — только полный кадр")
	}
}

func TestRegionFrameRoundTrip(t *testing.T) {
	img := newTestFrame(640, 480)
	regions := []image.Rectangle{image.Rect(0, 0, 128, 128), image.Rect(256, 128, 512, 256)}
	encoded, err := encodeRegions(img, regions, 80)
	if err != nil {
		t.Fatalf("кодирование областей: %v", err)
	}
	frame := packRegionFrame(7, encoded)

	if binary.BigEndian.Uint32(frame[0:4]) != 7 {
		t.Errorf("номер кадра потерян")
	}
	if frame[4] != frameKindRegions {
		t.Fatalf("маркер частичного кадра = %#x", frame[4])
	}
	// Клиент отличает частичный кадр от полного по первому байту после номера:
	// у JPEG там всегда SOI 0xFF.
	if frame[4] == 0xFF {
		t.Fatal("маркер совпал с началом JPEG — клиент перепутает форматы")
	}
	if int(frame[5]) != len(regions) {
		t.Fatalf("областей в заголовке %d, ждали %d", frame[5], len(regions))
	}

	off := 6
	for i, want := range regions {
		x := int(binary.BigEndian.Uint16(frame[off : off+2]))
		y := int(binary.BigEndian.Uint16(frame[off+2 : off+4]))
		w := int(binary.BigEndian.Uint16(frame[off+4 : off+6]))
		h := int(binary.BigEndian.Uint16(frame[off+6 : off+8]))
		n := int(binary.BigEndian.Uint32(frame[off+8 : off+12]))
		if x != want.Min.X || y != want.Min.Y || w != want.Dx() || h != want.Dy() {
			t.Errorf("область %d приехала как %d,%d %dx%d вместо %v", i, x, y, w, h, want)
		}
		if n <= 0 || off+12+n > len(frame) {
			t.Fatalf("область %d: длина %d не помещается в кадр", i, n)
		}
		if frame[off+12] != 0xFF || frame[off+13] != 0xD8 {
			t.Errorf("область %d не начинается с JPEG SOI", i)
		}
		off += 12 + n
	}
	if off != len(frame) {
		t.Errorf("в кадре остался хвост: разобрано %d из %d байт", off, len(frame))
	}
}

// Сквозная проверка склейки: «клиент» держит холст, накладывает на него
// присланные куски и обязан получить ту же картинку, что видит компьютер.
// Ловит главный риск частичных кадров — сдвиг координат: с ним экран
// разъезжается, а юнит-тесты областей остаются зелёными.
func TestClientRebuildsFrameFromRegions(t *testing.T) {
	base := newTestFrame(1024, 768)

	var tr tileTracker
	if _, full := tr.diff(base, 85); !full {
		t.Fatal("первый кадр обязан быть полным")
	}

	// Холст зрителя: полный кадр, декодированный из JPEG (как в браузере).
	var whole bytes.Buffer
	if err := jpeg.Encode(&whole, base, &jpeg.Options{Quality: 85}); err != nil {
		t.Fatalf("полный кадр: %v", err)
	}
	decoded, err := jpeg.Decode(bytes.NewReader(whole.Bytes()))
	if err != nil {
		t.Fatalf("декод полного кадра: %v", err)
	}
	canvas := image.NewRGBA(decoded.Bounds())
	draw.Draw(canvas, canvas.Bounds(), decoded, decoded.Bounds().Min, draw.Src)

	// Компьютер: две правки в разных концах экрана.
	next := image.NewRGBA(base.Bounds())
	copy(next.Pix, base.Pix)
	spots := []struct {
		rect image.Rectangle
		col  color.RGBA
	}{
		{image.Rect(60, 40, 300, 90), color.RGBA{R: 250, G: 20, B: 20, A: 255}},
		{image.Rect(700, 600, 1000, 700), color.RGBA{R: 10, G: 240, B: 90, A: 255}},
	}
	for _, s := range spots {
		fill(next, s.rect, s.col)
	}

	regions, full := tr.diff(next, 85)
	if full {
		t.Fatal("две небольшие правки не должны требовать полного кадра")
	}
	encoded, err := encodeRegions(next, regions, 85)
	if err != nil {
		t.Fatalf("кодирование областей: %v", err)
	}

	// Зритель: разбирает кадр с провода и кладёт куски на холст.
	frame := packRegionFrame(42, encoded)
	off := 6
	count := int(frame[5])
	for i := 0; i < count; i++ {
		x := int(binary.BigEndian.Uint16(frame[off : off+2]))
		y := int(binary.BigEndian.Uint16(frame[off+2 : off+4]))
		n := int(binary.BigEndian.Uint32(frame[off+8 : off+12]))
		part, err := jpeg.Decode(bytes.NewReader(frame[off+12 : off+12+n]))
		if err != nil {
			t.Fatalf("область %d не декодируется: %v", i, err)
		}
		at := image.Rect(x, y, x+part.Bounds().Dx(), y+part.Bounds().Dy())
		if !at.In(canvas.Bounds()) {
			t.Fatalf("область %d вылезла за холст: %v", i, at)
		}
		draw.Draw(canvas, at, part, part.Bounds().Min, draw.Src)
		off += 12 + n
	}

	near := func(got color.Color, want color.RGBA, where string) {
		t.Helper()
		r, g, b, _ := got.RGBA()
		dr := int(r>>8) - int(want.R)
		dg := int(g>>8) - int(want.G)
		db := int(b>>8) - int(want.B)
		if dr*dr+dg*dg+db*db > 3*20*20 { // допуск на потери JPEG
			t.Errorf("%s: получили rgb(%d,%d,%d), ждали rgb(%d,%d,%d)",
				where, r>>8, g>>8, b>>8, want.R, want.G, want.B)
		}
	}

	// В изменённых местах — новая картинка.
	for i, s := range spots {
		cx := (s.rect.Min.X + s.rect.Max.X) / 2
		cy := (s.rect.Min.Y + s.rect.Max.Y) / 2
		near(canvas.At(cx, cy), s.col, fmt.Sprintf("центр правки %d (%d,%d)", i, cx, cy))
	}
	// В нетронутых — прежняя, без сдвигов и мусора.
	for _, p := range []image.Point{{X: 500, Y: 300}, {X: 20, Y: 700}, {X: 1000, Y: 20}} {
		want := base.RGBAAt(p.X, p.Y)
		near(canvas.At(p.X, p.Y), want, fmt.Sprintf("нетронутая точка (%d,%d)", p.X, p.Y))
	}
}
