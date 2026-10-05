package pty

import (
	"slices"
	"strings"
	"testing"
)

func feed(d *decTracker, chunks ...string) {
	for _, c := range chunks {
		d.scan([]byte(c))
	}
}

func TestDecModes_SetSingle(t *testing.T) {
	d := newDecTracker()
	feed(d, "\x1b[?1049h")
	if got := d.reassertSeq(); got != "\x1b[?1049h" {
		t.Fatalf("got %q, want alt-screen set", got)
	}
}

func TestDecModes_OrderAndMultipleSeqs(t *testing.T) {
	d := newDecTracker()
	// Приходят вразнобой; до-сылка должна идти в каноничном порядке (alt-screen
	// первым), а не в порядке прихода.
	feed(d, "\x1b[?1006h", "\x1b[?1000h", "\x1b[?1049h")
	want := "\x1b[?1049h\x1b[?1000h\x1b[?1006h"
	if got := d.reassertSeq(); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestDecModes_MultiParamOneSeq(t *testing.T) {
	d := newDecTracker()
	feed(d, "\x1b[?1000;1006h")
	want := "\x1b[?1000h\x1b[?1006h"
	if got := d.reassertSeq(); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestDecModes_ExclusiveMouseFamiliesKeepLastSet(t *testing.T) {
	d := newDecTracker()
	feed(d, "\x1b[?1002h", "\x1b[?1000h")
	if got, want := d.snapshot(), []int{1000}; !slices.Equal(got, want) {
		t.Fatalf("tracking modes=%v, want last SET %v", got, want)
	}
	feed(d, "\x1b[?1005h", "\x1b[?1015h", "\x1b[?1006h")
	if got, want := d.snapshot(), []int{1000, 1006}; !slices.Equal(got, want) {
		t.Fatalf("mouse modes=%v, want last encoding %v", got, want)
	}
}

// Стенд эквивалентности (план 13.09, ST-05): состояние режимов кадра обязано
// совпадать с тем, что xterm.js 6.0.0 держит после того же потока. Ожидания —
// замер на @xterm/headless 6.0.0 (14.09.2026).
func TestDecModes_MatchesXtermEnumsAndDECSTR(t *testing.T) {
	cases := []struct {
		name   string
		chunks []string
		want   []int
	}{
		{"reset any tracking member clears the family", []string{"\x1b[?1002h\x1b[?1000l"}, nil},
		{"reset another member", []string{"\x1b[?1003h\x1b[?1006h", "\x1b[?1002l"}, []int{1006}},
		{"1015 is ignored by xterm and keeps SGR", []string{"\x1b[?1000h\x1b[?1006h\x1b[?1015h"}, []int{1000, 1006}},
		{"1005 is ignored by xterm and keeps SGR", []string{"\x1b[?1000h\x1b[?1006h\x1b[?1005h"}, []int{1000, 1006}},
		{"1015l does not reset SGR", []string{"\x1b[?1000h\x1b[?1006h\x1b[?1015l"}, []int{1000, 1006}},
		{"1006l resets the encoding only", []string{"\x1b[?1000h\x1b[?1006h\x1b[?1006l"}, []int{1000}},
		{"DECSTR resets bracketed paste and focus", []string{"\x1b[?2004h\x1b[?1004h\x1b[!p"}, nil},
		{"DECSTR keeps mouse and alt", []string{"\x1b[?1049h\x1b[?1000h\x1b[?1006h\x1b[?2004h\x1b[!p"}, []int{1049, 1000, 1006}},
		{"DECSTR split across chunks", []string{"\x1b[?2004h\x1b[", "!", "p"}, nil},
		{"CSI p without ! is not DECSTR", []string{"\x1b[?2004h\x1b[p"}, []int{2004}},
		{"CSI ! then another final is ignored", []string{"\x1b[?2004h\x1b[!q"}, []int{2004}},
	}
	for _, c := range cases {
		d := newDecTracker()
		feed(d, c.chunks...)
		if got := d.snapshot(); !slices.Equal(got, c.want) {
			t.Errorf("%s: modes=%v, want %v", c.name, got, c.want)
		}
	}
}

func TestDecModes_ResetRemoves(t *testing.T) {
	d := newDecTracker()
	feed(d, "\x1b[?1049h\x1b[?1006h")
	feed(d, "\x1b[?1049l") // приложение вышло из alt-screen
	if got := d.reassertSeq(); got != "\x1b[?1006h" {
		t.Fatalf("got %q, want only mouse left after alt-screen exit", got)
	}
}

func TestDecModes_RISClearsSeededModesAcrossChunks(t *testing.T) {
	d := newDecTracker()
	d.seed([]int{1049, 1006, 2004})
	feed(d, "text\x1b", "c") // RIS may split at a PTY read boundary
	if got := d.snapshot(); len(got) != 0 {
		t.Fatalf("RIS left stale DEC modes active: %v", got)
	}
	if !d.takeDirty() {
		t.Fatal("RIS mode reset was not marked dirty for persistence")
	}
}

func TestDecModes_SplitAcrossChunks(t *testing.T) {
	d := newDecTracker()
	// Последовательность разорвана на границе chunk'а (частый случай при чтении
	// PTY кусками по 8 КБ) — автомат обязан её сшить.
	feed(d, "abc\x1b[?10", "49h def")
	if got := d.reassertSeq(); got != "\x1b[?1049h" {
		t.Fatalf("got %q, want alt-screen across split", got)
	}
}

func TestDecModes_SplitAtEscPrefix(t *testing.T) {
	d := newDecTracker()
	feed(d, "\x1b", "[", "?", "2", "0", "0", "4", "h") // побайтно
	if got := d.reassertSeq(); got != "\x1b[?2004h" {
		t.Fatalf("got %q, want bracketed-paste byte-by-byte", got)
	}
}

func TestDecModes_UntrackedIgnored(t *testing.T) {
	d := newDecTracker()
	feed(d, "\x1b[?12h\x1b[?25h\x1b[?7h") // blink/cursor/wrap — не отслеживаем
	if got := d.reassertSeq(); got != "" {
		t.Fatalf("got %q, want empty for untracked modes", got)
	}
}

func TestDecModes_PlainShellEmpty(t *testing.T) {
	d := newDecTracker()
	feed(d, "PS C:\\> ls\r\nfoo bar\r\n") // обычный вывод без DEC-режимов
	if got := d.reassertSeq(); got != "" {
		t.Fatalf("got %q, want empty for plain shell", got)
	}
}

func TestDecModes_NonHLAborts(t *testing.T) {
	d := newDecTracker()
	// CSI ? ... с финалом не h/l (например незавершённый/чужой) не должен ничего
	// включать, и не должен ломать разбор следующей валидной последовательности.
	feed(d, "\x1b[?1049x\x1b[?1006h")
	if got := d.reassertSeq(); got != "\x1b[?1006h" {
		t.Fatalf("got %q, want only the valid trailing set", got)
	}
}

func TestDecModes_DirtyAndSnapshot(t *testing.T) {
	d := newDecTracker()
	if d.takeDirty() {
		t.Fatal("свежий трекер не должен быть dirty")
	}
	feed(d, "\x1b[?1049h")
	if !d.takeDirty() {
		t.Fatal("после включения режима dirty обязан взвестись")
	}
	if d.takeDirty() {
		t.Fatal("takeDirty должен сбрасывать флаг")
	}
	feed(d, "\x1b[?1049h") // повторное включение уже активного — НЕ изменение
	if d.takeDirty() {
		t.Fatal("no-op set не должен взводить dirty")
	}
	if got := d.snapshot(); !slices.Equal(got, []int{1049}) {
		t.Fatalf("snapshot=%v, want [1049]", got)
	}
}

func TestDecModes_SeedFiltersUntracked(t *testing.T) {
	d := newDecTracker()
	d.seed([]int{1049, 25, 1006, -1}) // 25/-1 — мусор, отфильтровать
	if got := d.snapshot(); !slices.Equal(got, []int{1049, 1006}) {
		t.Fatalf("snapshot=%v, want [1049 1006]", got)
	}
	if d.takeDirty() {
		t.Fatal("seed не должен взводить dirty (сидирование — не изменение)")
	}
}

// syncSeq (полная синхронизация при gap-резюме): активные режимы — SET,
// неактивные из отслеживаемого набора — RESET, чтобы вытащить клиент из
// режима, выключенного в потерянной середине потока.
func TestDecModes_SyncSeq(t *testing.T) {
	d := newDecTracker()
	feed(d, "\x1b[?1049h\x1b[?1006h")
	got := d.syncSeq()
	for _, want := range []string{"\x1b[?1049h", "\x1b[?1006h", "\x1b[?1000l", "\x1b[?2004l"} {
		if !strings.Contains(got, want) {
			t.Fatalf("syncSeq=%q, нет %q", got, want)
		}
	}
	// Пустой трекер: одни RESET'ы — безопасно для обычного shell (сбросит
	// застрявший alt-screen, если клиент в нём оказался).
	empty := newDecTracker().syncSeq()
	if !strings.Contains(empty, "\x1b[?1049l") || strings.Contains(empty, "\x1b[?1049h") {
		t.Fatalf("empty syncSeq=%q", empty)
	}
}

// ЖИВОЙ ДЕФЕКТ (12.08.2026): синхронизация режимов стирала кадр агента.
// syncSeq шёл строго по decModeReassertOrder и начинался с `ESC[?47l
// ESC[?1047l ESC[?1048l` — в xterm.js это activateNormalBuffer() с очисткой
// alt-буфера. Клиент, вернувшийся с gap, имел на экране правильную картинку
// агента, и мы стирали её сами, ещё до `ESC[?1049h`. Замер на @xterm/headless
// 6.0.0 с боевым набором Claude Code: 3 строки кадра → 0 после syncSeq, 3
// после reassertSeq.
func TestDecModes_SyncSeqKeepsAltFrame(t *testing.T) {
	d := newDecTracker()
	feed(d, "\x1b[?1049h\x1b[?1000h\x1b[?1002h\x1b[?1003h\x1b[?1004h\x1b[?1006h\x1b[?2004h")
	got := d.syncSeq()
	// Пока alt активен — ни одного RESET альтернативного экрана: каждый из них
	// уносит нарисованный кадр.
	for _, bad := range []string{"\x1b[?47l", "\x1b[?1047l", "\x1b[?1048l", "\x1b[?1049l"} {
		if strings.Contains(got, bad) {
			t.Fatalf("syncSeq=%q содержит %q — это стирает кадр агента", got, bad)
		}
	}
	// Active alt enters before any harmless non-cell reset. Mouse tracking and
	// encoding are xterm enums, so their inactive RESETs intentionally precede
	// the active SET that must win last.
	if altSet, firstReset := strings.Index(got, "\x1b[?1049h"), strings.Index(got, "l"); altSet < 0 || firstReset < altSet {
		t.Fatalf("syncSeq=%q: active alt was not established before resets", got)
	}
	// Обратный случай: alt выключен, а клиент мог в нём застрять — обязаны его
	// оттуда вывести.
	off := newDecTracker()
	feed(off, "\x1b[?1006h")
	if seq := off.syncSeq(); !strings.Contains(seq, "\x1b[?1049l") {
		t.Fatalf("syncSeq=%q: застрявший в alt-screen клиент не выведен", seq)
	}
	// Точные байты — эталон. Ровно эта строка прогнана через живой xterm
	// (@xterm/headless 6.0.0) поверх нарисованного кадра: 3 строки → 3 строки,
	// тогда как прежняя последовательность давала 3 → 0. Меняешь состав или
	// порядок — перепроверь тем же способом, иначе кадр снова начнёт пропадать.
	want := "\x1b[?1049h\x1b[?1000l\x1b[?1002l\x1b[?1005l\x1b[?1015l" +
		"\x1b[?1003h\x1b[?1004h\x1b[?1006h\x1b[?2004h"
	if got != want {
		t.Fatalf("syncSeq=%q,\n           want %q", got, want)
	}
}
