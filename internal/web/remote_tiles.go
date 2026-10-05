package web

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"image"
	"image/jpeg"
)

// Частичный кадр: на экран уходит только то, что на нём изменилось.
//
// Замер 23.08 на живой машине (1920×1080): захват GDI 17,6 мс, JPEG q85
// 17,6 мс, 135 КБ на КАЖДЫЙ кадр. При 30 fps это 32 Мбит/с — столько не
// выдерживает ни один домашний аплинк, тем более через VPN: пакеты встают в
// очередь на исходящем канале, и задержка растёт на сотни миллисекунд поверх
// сетевой. При этом набор текста в терминале меняет 2–5 % экрана.
//
// Поэтому кадр режется на тайлы, хэш каждого сравнивается с прошлым, а
// клиенту уезжают только изменившиеся области. Кодируется тоже только они —
// JPEG платится за проценты картинки, а не за весь экран.
//
// Ограничения намеренные: если изменилось слишком многое, полный кадр и
// дешевле, и надёжнее — тогда работает прежний путь.

const (
	// tileSide — сторона тайла. 128 px это компромисс: мельче — растут
	// накладные расходы JPEG (у каждого куска своя таблица Хаффмана и заголовок
	// ≈600 Б), крупнее — в область попадает много неизменившегося.
	tileSide = 128
	// maxDirtyRegions — сколько отдельных областей готовы отправить за кадр.
	// Больше — заголовки и отдельные JPEG-и съедают выигрыш.
	maxDirtyRegions = 12
	// dirtyFullFrameRatio — доля изменившейся площади, после которой полный
	// кадр выгоднее набора кусков.
	dirtyFullFrameRatio = 0.55
	// frameKindRegions — маркер частичного кадра на проводе. Полный кадр
	// начинается с JPEG SOI (0xFF 0xD8), так что перепутать нельзя.
	frameKindRegions = 0x01
)

// tileTracker помнит хэши тайлов прошлого отправленного кадра.
type tileTracker struct {
	hashes  []uint32
	cols    int
	rows    int
	width   int
	height  int
	quality int
	valid   bool
}

// reset забывает прошлый кадр: следующий уедет целиком. Нужен при смене
// размера/качества и при появлении нового зрителя.
func (t *tileTracker) reset() { t.valid = false }

// tileHashes считает хэш каждого тайла кадра.
func tileHashes(img *image.RGBA, cols, rows int) []uint32 {
	b := img.Bounds()
	out := make([]uint32, cols*rows)
	for ty := 0; ty < rows; ty++ {
		y0 := b.Min.Y + ty*tileSide
		y1 := min(y0+tileSide, b.Max.Y)
		for tx := 0; tx < cols; tx++ {
			x0 := b.Min.X + tx*tileSide
			x1 := min(x0+tileSide, b.Max.X)
			h := crc32.New(castagnoliTable)
			for y := y0; y < y1; y++ {
				start := img.PixOffset(x0, y)
				end := img.PixOffset(x1-1, y) + 4
				_, _ = h.Write(img.Pix[start:end])
			}
			out[ty*cols+tx] = h.Sum32()
		}
	}
	return out
}

// diff возвращает области, изменившиеся с прошлого кадра. full=true означает
// «дешевле отправить кадр целиком» — первый кадр, смена параметров кодирования
// или слишком много изменений.
func (t *tileTracker) diff(img *image.RGBA, quality int) (regions []image.Rectangle, full bool) {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	cols := (w + tileSide - 1) / tileSide
	rows := (h + tileSide - 1) / tileSide
	hashes := tileHashes(img, cols, rows)

	prev := t.hashes
	sameGeometry := t.valid && t.width == w && t.height == h &&
		t.quality == quality && t.cols == cols && t.rows == rows && len(prev) == len(hashes)

	t.hashes, t.cols, t.rows = hashes, cols, rows
	t.width, t.height, t.quality, t.valid = w, h, quality, true

	// Первый кадр этого зрителя или другая геометрия/качество: у клиента нет
	// холста, поверх которого можно класть куски.
	if !sameGeometry {
		return nil, true
	}

	dirty := make([]bool, len(hashes))
	changedTiles := 0
	for i := range hashes {
		if hashes[i] != prev[i] {
			dirty[i] = true
			changedTiles++
		}
	}
	if changedTiles == 0 {
		return nil, false // пиксель в пиксель — отправлять нечего
	}

	regions = mergeDirtyTiles(dirty, cols, rows, w, h)
	if len(regions) == 0 || len(regions) > maxDirtyRegions {
		return nil, true
	}
	area := 0
	for _, r := range regions {
		area += r.Dx() * r.Dy()
	}
	if float64(area) > dirtyFullFrameRatio*float64(w*h) {
		return nil, true // изменилось столько, что целый кадр дешевле
	}
	return regions, false
}

// mergeDirtyTiles собирает булеву сетку изменившихся тайлов в прямоугольники:
// сначала соседние по горизонтали в полосу, затем одинаковые полосы соседних
// рядов — по вертикали. Так «одна изменившаяся строка терминала» уезжает одним
// куском, а не десятком.
func mergeDirtyTiles(dirty []bool, cols, rows, w, h int) []image.Rectangle {
	type span struct{ x0, x1 int } // в тайлах, x1 не включительно
	var out []image.Rectangle
	open := map[span]int{} // открытая полоса → ряд, с которого она идёт

	flush := func(s span, fromRow, toRow int) {
		x0 := s.x0 * tileSide
		y0 := fromRow * tileSide
		x1 := min(s.x1*tileSide, w)
		y1 := min(toRow*tileSide, h)
		if x1 > x0 && y1 > y0 {
			out = append(out, image.Rect(x0, y0, x1, y1))
		}
	}

	for row := 0; row <= rows; row++ {
		current := map[span]bool{}
		if row < rows {
			col := 0
			for col < cols {
				if !dirty[row*cols+col] {
					col++
					continue
				}
				start := col
				for col < cols && dirty[row*cols+col] {
					col++
				}
				current[span{start, col}] = true
			}
		}
		// Полосы, которых в этом ряду нет, закрываем.
		for s, from := range open {
			if !current[s] {
				flush(s, from, row)
				delete(open, s)
			}
		}
		// Новые полосы открываем.
		for s := range current {
			if _, ok := open[s]; !ok {
				open[s] = row
			}
		}
	}
	return out
}

// encodedRegion — закодированный кусок кадра и его место на экране.
type encodedRegion struct {
	rect image.Rectangle
	jpeg []byte
}

// encodeRegions кодирует каждую область отдельным JPEG.
func encodeRegions(img *image.RGBA, regions []image.Rectangle, quality int) ([]encodedRegion, error) {
	out := make([]encodedRegion, 0, len(regions))
	var buf bytes.Buffer
	for _, r := range regions {
		sub, ok := img.SubImage(r.Add(img.Bounds().Min)).(*image.RGBA)
		if !ok {
			continue
		}
		buf.Reset()
		if err := jpeg.Encode(&buf, sub, &jpeg.Options{Quality: quality}); err != nil {
			return nil, err
		}
		data := make([]byte, buf.Len())
		copy(data, buf.Bytes())
		out = append(out, encodedRegion{rect: r, jpeg: data})
	}
	return out, nil
}

// packRegionFrame собирает частичный кадр для провода:
//
//	[seq:4][0x01][count:1]{[x:2][y:2][w:2][h:2][len:4][jpeg…]}×count
//
// Полный кадр остаётся прежним ([seq:4][jpeg…]) и различается по первому байту
// после номера: у JPEG это всегда 0xFF.
func packRegionFrame(seq uint32, regions []encodedRegion) []byte {
	size := 4 + 1 + 1
	for _, r := range regions {
		size += 12 + len(r.jpeg)
	}
	out := make([]byte, 0, size)
	var num [4]byte
	binary.BigEndian.PutUint32(num[:], seq)
	out = append(out, num[:]...)
	out = append(out, frameKindRegions, byte(len(regions)))
	for _, r := range regions {
		var hdr [12]byte
		binary.BigEndian.PutUint16(hdr[0:2], uint16(r.rect.Min.X))
		binary.BigEndian.PutUint16(hdr[2:4], uint16(r.rect.Min.Y))
		binary.BigEndian.PutUint16(hdr[4:6], uint16(r.rect.Dx()))
		binary.BigEndian.PutUint16(hdr[6:8], uint16(r.rect.Dy()))
		binary.BigEndian.PutUint32(hdr[8:12], uint32(len(r.jpeg)))
		out = append(out, hdr[:]...)
		out = append(out, r.jpeg...)
	}
	return out
}
