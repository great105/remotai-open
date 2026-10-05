package pty

import (
	"bytes"
	"strings"
	"testing"
)

// Хвост для выжимки берётся с КОНЦА буфера: там итог эпизода. И не меньше
// минимума — у полноэкранного агента один кадр перерисовки занимает килобайты,
// поэтому «сколько байт вывел эпизод» легко недобирает сам результат работы.
func TestSummaryTailTakesEndAndKeepsMinimum(t *testing.T) {
	s := &Session{}
	s.buf = append(bytes.Repeat([]byte("x"), 200<<10), []byte("ИТОГ работы")...)

	got := summaryTail(s, 8<<10)
	if !strings.HasSuffix(string(got), "ИТОГ работы") {
		t.Fatal("взяли не конец буфера — итог эпизода потерян")
	}
	if len(got) != 8<<10 {
		t.Fatalf("длина хвоста %d, просили %d", len(got), 8<<10)
	}

	// Просьба «дай 100 байт» поднимается до минимума.
	if n := len(summaryTail(s, 100)); n != 4<<10 {
		t.Fatalf("минимум не соблюдён: %d", n)
	}
	// Буфер короче запроса — отдаём сколько есть, а не паникуем.
	short := &Session{}
	short.buf = []byte("мало")
	if got := string(summaryTail(short, 8<<10)); got != "мало" {
		t.Fatalf("короткий буфер испорчен: %q", got)
	}
}

// Хук опционален и меняется на ходу: без него детектор обязан работать молча,
// с ним — отдавать повод.
func TestSummaryHookOptionalAndReplaceable(t *testing.T) {
	m := &Manager{}
	// Хука нет — вызов не должен падать (так живёт агент без ключа OpenRouter).
	m.fireSummary(SummaryEvent{Kind: SummaryFinished})

	var got []SummaryEvent
	m.SetSummaryHook(func(ev SummaryEvent) { got = append(got, ev) })
	m.fireSummary(SummaryEvent{Kind: SummaryFinished, PtyID: "a"})
	m.fireSummary(SummaryEvent{Kind: SummaryQuestion, PtyID: "b"})
	if len(got) != 2 || got[0].PtyID != "a" || got[1].Kind != SummaryQuestion {
		t.Fatalf("хук получил не то: %+v", got)
	}

	m.SetSummaryHook(nil)
	m.fireSummary(SummaryEvent{Kind: SummaryFinished, PtyID: "c"})
	if len(got) != 2 {
		t.Fatal("снятый хук продолжает получать события")
	}
}
