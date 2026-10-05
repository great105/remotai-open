package web

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"testing"
)

// Кодек берём живой (openh264): проверять формат кадра на заглушке смысла нет —
// ошибки этого пути живут именно в связке «кодер → провод → декодер».
func newLiveEncoder(t *testing.T) *wsVideoEncoder {
	t.Helper()
	enc := newWSVideoEncoder()
	if !enc.available() {
		t.Skip("openh264 не установлен на этой машине — путь H.264 по веб-сокету не проверить")
	}
	return enc
}

func TestWSVideoFirstFrameIsKeyframe(t *testing.T) {
	enc := newLiveEncoder(t)
	defer enc.close()

	img := newTestFrame(1280, 720)
	nal, key, reconfigured, err := enc.encode(img, 1280, 30, "auto", false)
	if err != nil {
		t.Fatalf("кодирование: %v", err)
	}
	if !reconfigured {
		t.Error("первый кадр обязан сообщить клиенту размер: декодер без этого не собрать")
	}
	if !key {
		t.Fatal("первый кадр обязан быть ключевым — с него стартует декодер")
	}
	if len(nal) < 4 || !bytes.HasPrefix(nal, []byte{0, 0, 0, 1}) {
		t.Fatalf("поток не в Annex-B: первые байты %x", nal[:min(8, len(nal))])
	}
}

// Ради чего всё: видеокодек берёт разницу, а не перерисовывает экран целиком.
func TestWSVideoDeltaIsSmallerThanKeyframe(t *testing.T) {
	enc := newLiveEncoder(t)
	defer enc.close()

	img := newTestFrame(1280, 720)
	keyNAL, _, _, err := enc.encode(img, 1280, 30, "auto", false)
	if err != nil {
		t.Fatalf("ключевой кадр: %v", err)
	}

	// Печать строки: меняется малая часть экрана.
	fill(img, image.Rect(100, 300, 800, 322), color.RGBA{R: 240, G: 240, B: 240, A: 255})
	deltaNAL, key, _, err := enc.encode(img, 1280, 30, "auto", false)
	if err != nil {
		t.Fatalf("дельта: %v", err)
	}
	if key {
		t.Fatal("второй кадр не должен быть ключевым без просьбы")
	}
	if len(deltaNAL) >= len(keyNAL) {
		t.Errorf("дельта не дешевле ключевого кадра: %d против %d байт", len(deltaNAL), len(keyNAL))
	}
	t.Logf("ключевой %d КБ, дельта %.1f КБ", len(keyNAL)/1024, float64(len(deltaNAL))/1024)
}

// Клиент просит ключевой кадр, когда его декодер потерял поток.
func TestWSVideoHonoursKeyframeRequest(t *testing.T) {
	enc := newLiveEncoder(t)
	defer enc.close()

	img := newTestFrame(640, 480)
	if _, _, _, err := enc.encode(img, 640, 30, "auto", false); err != nil {
		t.Fatalf("первый кадр: %v", err)
	}
	fill(img, image.Rect(0, 0, 100, 100), color.RGBA{B: 255, A: 255})
	_, key, _, err := enc.encode(img, 640, 30, "auto", true)
	if err != nil {
		t.Fatalf("по просьбе: %v", err)
	}
	if !key {
		t.Error("просьбу о ключевом кадре проигнорировали — декодер клиента не восстановится")
	}
}

// Смена ширины (человек развернул окно) пересобирает кодер, и клиент обязан
// узнать новый размер: иначе он показывает кашу в старой геометрии.
func TestWSVideoResizeReconfigures(t *testing.T) {
	enc := newLiveEncoder(t)
	defer enc.close()

	img := newTestFrame(1280, 720)
	if _, _, _, err := enc.encode(img, 1280, 30, "auto", false); err != nil {
		t.Fatalf("первый кадр: %v", err)
	}
	_, key, reconfigured, err := enc.encode(img, 960, 30, "auto", false)
	if err != nil {
		t.Fatalf("после смены ширины: %v", err)
	}
	if !reconfigured {
		t.Error("смена ширины обязана пересобрать кодер и сообщить клиенту")
	}
	if !key {
		t.Error("после пересборки первый кадр обязан быть ключевым")
	}
}

func TestPackVideoFrame(t *testing.T) {
	nal := []byte{0, 0, 0, 1, 0x67, 0x42, 0xC0}
	frame := packVideoFrame(9, nal, true)

	if binary.BigEndian.Uint32(frame[0:4]) != 9 {
		t.Error("номер кадра потерян")
	}
	if frame[4] != frameKindH264 {
		t.Fatalf("маркер видеокадра = %#x", frame[4])
	}
	if frame[4] == 0xFF || frame[4] == frameKindRegions {
		t.Fatal("маркер видео совпал с другим форматом — клиент перепутает их")
	}
	if frame[5]&videoFlagKeyframe == 0 {
		t.Error("флаг ключевого кадра не выставлен")
	}
	if !bytes.Equal(frame[6:], nal) {
		t.Error("полезная нагрузка испорчена")
	}

	delta := packVideoFrame(10, nal, false)
	if delta[5]&videoFlagKeyframe != 0 {
		t.Error("дельта помечена ключевым кадром")
	}
}
