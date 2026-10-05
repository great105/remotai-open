//go:build windows

package web

import (
	"errors"
	"image"
	"testing"
	"time"
)

func TestDisplayIndexForBounds(t *testing.T) {
	displays := []image.Rectangle{
		image.Rect(0, 0, 1920, 1080),
		image.Rect(1920, 0, 3840, 1080),
	}
	if got := displayIndexForBounds(image.Rect(1920, 0, 3840, 1080), displays); got != 1 {
		t.Errorf("второй монитор не найден: %d", got)
	}
	// Кусок экрана DXGI не отдаёт — такое должно уходить на прежний путь.
	if got := displayIndexForBounds(image.Rect(0, 0, 960, 540), displays); got != -1 {
		t.Errorf("кусок экрана принят за дисплей: %d", got)
	}
	// Объединение мониторов — тоже не дисплей.
	if got := displayIndexForBounds(image.Rect(0, 0, 3840, 1080), displays); got != -1 {
		t.Errorf("объединение мониторов принято за дисплей: %d", got)
	}
}

// Пока насос не отдал ни одного кадра, зритель обязан работать прежним путём:
// «менялось ли» — да (снимем и сравним по хэшу), кадр — через GDI.
func TestScreenSourceFallsBackWithoutPump(t *testing.T) {
	p := &dxgiPump{}
	if _, ok := p.generation(image.Rect(0, 0, 1920, 1080)); ok {
		t.Error("пустой насос не должен отдавать поколение")
	}
	if _, _, ok := p.frame(image.Rect(0, 0, 1920, 1080)); ok {
		t.Error("пустой насос не должен отдавать кадр")
	}
}

// Отказ DXGI (UAC, смена разрешения, RDP) уводит его на паузу, и зритель
// продолжает получать картинку прежним способом, а не пустой экран.
func TestPumpPauseStopsServingFrames(t *testing.T) {
	p := &dxgiPump{}
	bounds := image.Rect(0, 0, 1280, 720)
	p.publish(image.NewRGBA(bounds), bounds)
	if _, ok := p.generation(bounds); !ok {
		t.Fatal("после кадра поколение обязано быть")
	}

	p.pause(errors.New("access lost"))
	if _, ok := p.generation(bounds); ok {
		t.Error("на паузе насос не должен отдавать поколение")
	}
	if _, _, ok := p.frame(bounds); ok {
		t.Error("на паузе насос не должен отдавать кадр")
	}
	if p.pausedTo.Before(time.Now()) {
		t.Error("пауза не установлена")
	}
}

// Каждый новый кадр обязан двигать поколение: по нему зритель понимает, что
// перерисовывать. Одинаковое поколение = «ничего не менялось».
func TestPumpGenerationAdvancesPerFrame(t *testing.T) {
	p := &dxgiPump{}
	bounds := image.Rect(0, 0, 640, 480)
	p.publish(image.NewRGBA(bounds), bounds)
	first, _ := p.generation(bounds)
	p.publish(image.NewRGBA(bounds), bounds)
	second, _ := p.generation(bounds)
	if second == first {
		t.Errorf("поколение не сдвинулось: %d → %d", first, second)
	}
}

// Кадр отдаётся КОПИЕЙ: насос переиспользует свой буфер под следующий кадр, а
// зритель живёт с картинкой дольше — масштабирование, хэши, кодек.
func TestPumpFrameIsCopy(t *testing.T) {
	p := &dxgiPump{}
	bounds := image.Rect(0, 0, 64, 64)
	src := image.NewRGBA(bounds)
	src.Pix[0] = 200
	p.publish(src, bounds)

	got, _, ok := p.frame(bounds)
	if !ok {
		t.Fatal("кадра нет")
	}
	got.Pix[0] = 7 // зритель что-то делает со своей копией
	again, _, _ := p.frame(bounds)
	if again.Pix[0] != 200 {
		t.Errorf("зритель испортил буфер насоса: %d", again.Pix[0])
	}
}

// Насос заведён под конкретный дисплей: запрос про другую геометрию не должен
// получать чужой кадр (иначе на втором мониторе показался бы первый).
func TestPumpDoesNotServeOtherDisplay(t *testing.T) {
	p := &dxgiPump{}
	first := image.Rect(0, 0, 1920, 1080)
	second := image.Rect(1920, 0, 3840, 1080)
	p.publish(image.NewRGBA(image.Rect(0, 0, 1920, 1080)), first)

	if _, _, ok := p.frame(second); ok {
		t.Error("насос отдал кадр чужого дисплея")
	}
}
