package web

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"testing"

	"tgcontrol/internal/cdp"
)

// testJPEG кодирует маленькую картинку в JPEG 4:2:0 — как кадр CDP-скринкаста.
func testJPEG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetRGBA(x, y, color.RGBA{uint8(x * 7), uint8(y * 5), 64, 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 80}); err != nil {
		t.Fatalf("jpeg encode: %v", err)
	}
	return buf.Bytes()
}

// Без живого браузера (в тесте его нет) источник кадра обязан переключаться
// на захват экрана: cdpH264Frame отвечает ok=false, не ждёт и не падает.
func TestCDPH264FrameNoBrowser(t *testing.T) {
	var seq uint64
	_, ok, changed, err := cdpH264Frame(70, 960, &seq, false)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if ok || changed {
		t.Fatalf("ok=%v changed=%v, want ok=false changed=false (fallback на экран)", ok, changed)
	}
	if seq != 0 {
		t.Fatalf("seq = %d, want 0 (кадр не трогали)", seq)
	}
}

// Seq-ворота CDP-кадров: свежий кадр кодируем, повтор того же Seq — пропуск,
// keep-alive/IDR пробивает ворота, битый JPEG — ошибка со сдвигом Seq (без
// бесконечного пережёвывания одного битого кадра).
func TestDecodeCDPH264FrameGate(t *testing.T) {
	raw := testJPEG(t, 96, 64)
	var seq uint64

	img, changed, err := decodeCDPH264Frame(cdp.Frame{JPEG: raw, Seq: 7}, &seq, false)
	if err != nil || !changed {
		t.Fatalf("first frame: changed=%v err=%v, want frame", changed, err)
	}
	if seq != 7 {
		t.Fatalf("seq = %d, want 7", seq)
	}
	yc, isYCbCr := img.(*image.YCbCr)
	if !isYCbCr || yc.SubsampleRatio != image.YCbCrSubsampleRatio420 {
		t.Fatalf("decoded = %T, want *image.YCbCr 4:2:0 (fast path в энкодер)", img)
	}

	// Тот же Seq повторно — не кодируем и не шлём.
	if _, changed, err = decodeCDPH264Frame(cdp.Frame{JPEG: raw, Seq: 7}, &seq, false); changed || err != nil {
		t.Fatalf("same seq: changed=%v err=%v, want skip", changed, err)
	}
	// keep-alive (или запрос IDR) пробивает ворота даже без нового Seq.
	if _, changed, err = decodeCDPH264Frame(cdp.Frame{JPEG: raw, Seq: 7}, &seq, true); !changed || err != nil {
		t.Fatalf("keep-alive: changed=%v err=%v, want frame", changed, err)
	}
	// Новый Seq — кодируем.
	if _, changed, err = decodeCDPH264Frame(cdp.Frame{JPEG: raw, Seq: 8}, &seq, false); !changed || err != nil {
		t.Fatalf("new seq: changed=%v err=%v, want frame", changed, err)
	}
	// Битый JPEG — ошибка, кадр выброшен; Seq сдвинут, чтобы не ретраить его
	// на каждом такте.
	if _, changed, err = decodeCDPH264Frame(cdp.Frame{JPEG: []byte("not a jpeg"), Seq: 9}, &seq, false); err == nil || changed {
		t.Fatalf("bad jpeg: changed=%v err=%v, want error", changed, err)
	}
	if seq != 9 {
		t.Fatalf("seq = %d after bad jpeg, want 9 (кадр выброшен, не ретраим)", seq)
	}
}
