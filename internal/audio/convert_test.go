package audio

import (
	"encoding/binary"
	"math"
	"testing"
)

func TestDownmixResampleHalvesRate(t *testing.T) {
	// Секунда стерео 48 кГц.
	src := make([]float32, 48000*2)
	got := DownmixResample(src, 2, 48000, nil)
	if len(got) != 24000 {
		t.Errorf("из секунды 48 кГц вышло %d сэмплов, ждали 24000", len(got))
	}
}

func TestDownmixKeepsSignal(t *testing.T) {
	// Синус 1 кГц в обоих каналах: после сведения он обязан остаться синусом
	// той же амплитуды, а не половинной (частая ошибка — сложить, а не усреднить).
	const n = 4800
	src := make([]float32, n*2)
	for i := 0; i < n; i++ {
		v := float32(math.Sin(2*math.Pi*1000*float64(i)/48000)) * 0.5
		src[i*2], src[i*2+1] = v, v
	}
	out := DownmixResample(src, 2, 48000, nil)

	peak := 0
	for _, s := range out {
		if int(s) > peak {
			peak = int(s)
		}
	}
	want := 16383
	if peak < want*8/10 || peak > want*12/10 {
		t.Errorf("амплитуда после сведения %d, ждали около %d", peak, want)
	}
}

// Микшер Windows умеет выдавать значения за пределами −1…1. Без ограничения
// они заворачиваются в противоположный знак — это громкий щелчок.
func TestClampInsteadOfWraparound(t *testing.T) {
	// Пара кадров одного знака: усреднение их не гасит, ограничение обязано
	// сработать. (Смешивать здесь плюс и минус нельзя — среднее вышло бы
	// маленьким, и тест проверял бы не то.)
	loud := DownmixResample([]float32{2.5, 2.5, 2.5, 2.5}, 2, 48000, nil)
	if len(loud) == 0 || loud[0] != 32767 {
		t.Errorf("положительный перегруз не ограничен: %v", loud)
	}
	quiet := DownmixResample([]float32{-3.0, -3.0, -3.0, -3.0}, 2, 48000, nil)
	if len(quiet) == 0 || quiet[0] != -32768 {
		t.Errorf("отрицательный перегруз не ограничен: %v", quiet)
	}
}

func TestIsSilent(t *testing.T) {
	quiet := make([]int16, FrameSamples)
	for i := range quiet {
		quiet[i] = 5 // шум ниже порога слышимости
	}
	if !IsSilent(quiet) {
		t.Error("тихий кусок должен считаться тишиной — молчащий ПК не занимает канал")
	}

	loud := make([]int16, FrameSamples)
	loud[100] = 5000
	if IsSilent(loud) {
		t.Error("кусок со звуком принят за тишину — человек не услышит уведомление")
	}
}

func TestPackPCMIsLittleEndian(t *testing.T) {
	data := PackPCM([]int16{1, -1, 32767})
	if len(data) != 6 {
		t.Fatalf("длина %d, ждали 6", len(data))
	}
	if binary.LittleEndian.Uint16(data[0:2]) != 1 {
		t.Error("первый сэмпл испорчен")
	}
	if int16(binary.LittleEndian.Uint16(data[2:4])) != -1 {
		t.Error("знак потерян")
	}
	if int16(binary.LittleEndian.Uint16(data[4:6])) != 32767 {
		t.Error("максимум испорчен")
	}
}

func TestFrameSizeIs20ms(t *testing.T) {
	if FrameSamples != 480 {
		t.Errorf("кусок %d сэмплов — это не 20 мс при %d Гц", FrameSamples, SampleRate)
	}
}
