//go:build linux

package codec

import "testing"

// TestOpusIfLibPresent делает реальный круг кодирования, когда libopus
// установлена (sudo apt install libopus0). Без библиотеки тест пропускается —
// тот же паттерн, что у openh264 (TestEncodeIfDLLPresent).
func TestOpusIfLibPresent(t *testing.T) {
	lib := FindOpusLib()
	if lib == "" {
		t.Skip("libopus.so.0 not found (apt install libopus0)")
	}
	enc, err := NewOpus(lib)
	if err != nil {
		t.Fatalf("NewOpus: %v", err)
	}
	defer enc.Close()

	// Тишина: libopus обязана выдать валидный (крошечный) пакет.
	silence := make([]int16, opusFrameSamples*opusChannels)
	pkt, err := enc.Encode(silence)
	if err != nil {
		t.Fatalf("encode silence: %v", err)
	}
	if len(pkt) == 0 || len(pkt) > opusMaxPacket {
		t.Fatalf("silence packet size %d out of range", len(pkt))
	}
	t.Logf("silence frame: %d bytes", len(pkt))

	// Тон: квази-синус грубой формы — должен кодироваться в заметно больший
	// и всё ещё валидный пакет.
	tone := make([]int16, opusFrameSamples*opusChannels)
	for i := range tone {
		tone[i] = int16(((i % 100) - 50) * 300)
	}
	pkt2, err := enc.Encode(tone)
	if err != nil {
		t.Fatalf("encode tone: %v", err)
	}
	if len(pkt2) <= len(pkt) {
		t.Fatalf("tone packet (%d) should exceed silence (%d)", len(pkt2), len(pkt))
	}
	t.Logf("tone frame: %d bytes", len(pkt2))

	// Кадр неправильного размера отвергается до вызова в C.
	if _, err := enc.Encode(silence[:100]); err == nil {
		t.Fatalf("short frame must be rejected")
	}

	// Пакеты — независимые буферы (fan-out делит их между подписчиками).
	pkt3, err := enc.Encode(tone)
	if err != nil {
		t.Fatalf("encode tone #2: %v", err)
	}
	if &pkt2[0] == &pkt3[0] {
		t.Fatalf("packets must not share backing memory")
	}
}
