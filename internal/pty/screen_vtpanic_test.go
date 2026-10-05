package pty

import (
	"strings"
	"testing"

	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/vt"
)

// Предохранитель P1 — ровно xterm (setScrollRegion): низ области больше
// высоты зажимается по высоте. LF у низа прокручивает область 2..4, а не
// уводит курсор за конец буфера.
func TestVtGuardDECSTBMClampsLikeXterm(t *testing.T) {
	grid := GridFromStream([]byte("L1\r\nL2\r\nL3\r\nL4\x1b[2;9r\x1b[4;1H\nX"), 20, 4, 0)
	got := make([]string, len(grid))
	for i, row := range grid {
		got[i] = strings.TrimRight(row, " ")
	}
	if want := []string{"L1", "L3", "L4", "X"}; strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("сетка %q, у xterm %q", got, want)
	}
}

// Страховка на resize: паника внутри Resize эмулятора (здесь — из колбэка
// позиции курсора, который vt зовёт, зажимая курсор) пересобирает зеркало
// СРАЗУ в новой геометрии, и после RIS кадр снят в ней же.
func TestScreenMirrorResizePanicRebuildsInNewGeometry(t *testing.T) {
	m := newScreenMirror(20, 6)
	defer m.Close()
	m.Write([]byte("\x1b[3;15Hx"))
	m.mu.Lock()
	m.em.SetCallbacks(vt.Callbacks{CursorPosition: func(_, _ uv.Position) { panic("проверка: паника в Resize") }})
	m.mu.Unlock()
	m.Resize(10, 4)
	m.mu.Lock()
	w, h, panics, untrusted := m.em.Width(), m.em.Height(), m.vtPanics, m.untrusted
	m.mu.Unlock()
	if w != 10 || h != 4 || panics != 1 || !untrusted {
		t.Fatalf("после паники в Resize: эмулятор %dx%d, паник %d, untrusted %v", w, h, panics, untrusted)
	}
	if cols, rows := m.Size(); cols != 10 || rows != 4 || m.Frame() != "" {
		t.Fatalf("зеркало %dx%d отдало кадр до RIS", cols, rows)
	}
	m.Write([]byte("\x1bcok"))
	if f := m.Frame(); !strings.Contains(f, "ok") {
		t.Fatalf("после RIS нет кадра: %q", f)
	}
}

// Вход-убийца и RIS за ним в том же куске: после паники в пустой эмулятор
// пишется хвост от последнего RIS — кадр есть сразу, ближайший RIS не
// потерян, пересборка одна. RIS ДО входа-убийцы пометку не снимает: хвост от
// него несёт тот же вход, и паника повторяется.
func TestScreenMirrorVtPanicReplayFindsRISInSameChunk(t *testing.T) {
	m := newScreenMirror(20, 4)
	defer m.Close()
	m.noVtGuards = true
	m.Write([]byte("\x1b[2;6r\x1b[3;1H\x1b[M\x1bcAFTER"))
	m.mu.Lock()
	panics, untrusted := m.vtPanics, m.untrusted
	m.mu.Unlock()
	if panics != 1 || untrusted {
		t.Fatalf("паник %d (ожидалась 1: хвост от RIS пишется без входа-убийцы), untrusted %v", panics, untrusted)
	}
	if f := m.Frame(); !strings.Contains(f, "AFTER") {
		t.Fatalf("RIS в том же куске потерян: %q", f)
	}

	before := newScreenMirror(20, 4)
	defer before.Close()
	before.noVtGuards = true
	before.Write([]byte("\x1bcBEFORE\x1b[2;6r\x1b[3;1H\x1b[M"))
	if f := before.Frame(); f != "" {
		t.Fatalf("RIS до паники снял пометку: %q", f)
	}
}

// Паника в горутине-кормилице сессии: процесс жив, воркер разбирает
// следующие куски, клиенту screen-request-v1 — честный отказ «unavailable»
// (не «повтори»), разрушительная сверка DEC-режимов по пустому зеркалу
// запрещена; после RIS приложения — обычный кадр на своём месте в потоке.
func TestSessionScreenSurvivesVtPanicUntilRIS(t *testing.T) {
	s := &Session{epoch: "e1", subs: map[chan []byte]*subState{}, dec: newDecTracker()}
	s.startScreen(20, 4)
	defer s.stopScreen()
	sc := s.screen.Load().(*sessionScreen)
	sc.mu.Lock()
	m := sc.mirror
	sc.mu.Unlock()
	m.mu.Lock()
	m.noVtGuards = true
	m.mu.Unlock()

	var off uint64
	feed := func(p string) {
		off += uint64(len(p))
		s.feedScreenAt([]byte(p), off)
	}
	feed("shell$ ")
	if c := s.CaptureScreenFrame(); c.Frame == "" {
		t.Fatalf("до паники нет кадра: %+v", c)
	}
	feed("\x1b[2;6r\x1b[3;1H\x1b[M")
	c := s.CaptureScreenFrame()
	if c.Frame != "" || c.History != "" || c.Reason != ScreenReasonUnavailable || c.Retry {
		t.Fatalf("после паники: reason=%q frame=%q", c.Reason, c.Frame)
	}
	if _, live := s.screenAltLiveAt(off); live {
		t.Fatal("DEC-режимы сверяются по пересобранному пустому зеркалу")
	}
	feed("\x1bcAFTER")
	c = s.CaptureScreenFrame()
	if c.Frame == "" || c.Reason != "" || c.BaseOff != off || !strings.Contains(c.Frame, "AFTER") {
		t.Fatalf("после RIS: reason=%q base=%d (want %d) frame=%q", c.Reason, c.BaseOff, off, c.Frame)
	}
	// Одна: пока пометка стоит, эмулятор байтов не получает, а хвост от RIS
	// входа-убийцы не несёт.
	m.mu.Lock()
	panics := m.vtPanics
	m.mu.Unlock()
	if panics != 1 {
		t.Fatalf("паник у зеркала сессии %d, ожидалась 1", panics)
	}
}

// Пока зеркало недостоверно и RIS нет, эмулятор байтов не получает: вход-
// убийца на каждой перерисовке не пересобирает эмулятор (4 МБ буфера
// разборщика на каждый) — прежняя побайтовая переигровка пересобирала его
// на каждом входе в куске (скептик волны 4: 201 паника и 917 МБ на кусок).
func TestScreenMirrorUntrustedDoesNotRebuildWithoutRIS(t *testing.T) {
	m := newScreenMirror(20, 4)
	defer m.Close()
	m.noVtGuards = true
	killer := "\x1b[2;6r\x1b[3;1H\x1b[M"
	m.Write([]byte(killer))
	for i := 0; i < 5; i++ {
		m.Write([]byte(strings.Repeat(killer, 50) + "x"))
	}
	m.mu.Lock()
	panics, untrusted := m.vtPanics, m.untrusted
	m.mu.Unlock()
	if panics != 1 || !untrusted {
		t.Fatalf("без RIS: паник %d (ожидалась 1), untrusted %v", panics, untrusted)
	}
	m.Write([]byte(killer + "\x1bcOK"))
	m.mu.Lock()
	panics, untrusted = m.vtPanics, m.untrusted
	m.mu.Unlock()
	if panics != 1 || untrusted || !strings.Contains(m.Frame(), "OK") {
		t.Fatalf("после RIS: паник %d, untrusted %v, кадр %q", panics, untrusted, m.Frame())
	}
}

// Одиночные C1 (0x80–0x9F вне UTF-8) зеркало понимает как xterm клиента —
// выбрасывает. Ожидания сняты с @xterm/headless 6.0.0 (15.09): vt исполнял
// их как C1 (0x9B — CSI, 0x90 — DCS, 0x88 — HTS). Продолжения UTF-8 в том же
// диапазоне («ш» = D1 88, «∈» = E2 88 88) и U+0088 (C2 88) — не одиночные.
func TestMirrorDropsStrayC1LikeXterm(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"A\x88B", "AB"},
		{"A\x9b2CX", "A2CX"},
		{"A\x90xyz\x9cB", "AxyzB"},
		{"A\x85B", "AB"},
		{"\x1b[3g\x1b[1;5H\x88\r\tX", strings.Repeat(" ", 19) + "X"},
		{"\xd1\x88\xe2\x88\x88", "ш∈"},
		// Одиночный C1 между базой и знаком: xterm выбрасывает байт при
		// декодировании и приклеивает знак — «é». Фильтр до NFC (ревью волны 5).
		{"e\x88\xcc\x81X", "éX"},
	} {
		grid := GridFromStream([]byte(tc.in), 20, 4, 0)
		if got := strings.TrimRight(grid[0], " "); got != tc.want {
			t.Errorf("%q: строка %q, у xterm %q", tc.in, got, tc.want)
		}
	}
}

// Состояние UTF-8 между кусками: продолжение оборванной последовательности
// в начале следующего куска — не одиночный C1. И горячий путь без копии.
func TestDropStrayC1CarriesUTF8AcrossChunks(t *testing.T) {
	out, owed := dropStrayC1([]byte("x\xd1"), 0)
	if string(out) != "x\xd1" || owed != 1 {
		t.Fatalf("оборванный хвост: %q, owed %d", out, owed)
	}
	if out, owed = dropStrayC1([]byte("\x88y"), owed); string(out) != "\x88y" || owed != 0 {
		t.Fatalf("продолжение из прошлого куска выброшено: %q, owed %d", out, owed)
	}
	if out, owed = dropStrayC1([]byte("\x88y"), 0); string(out) != "y" || owed != 0 {
		t.Fatalf("одиночный 0x88 остался: %q, owed %d", out, owed)
	}
	if out, owed = dropStrayC1([]byte("z\x88\xe2\x88"), 0); string(out) != "z\xe2\x88" || owed != 1 {
		t.Fatalf("одиночный перед оборванным хвостом: %q, owed %d", out, owed)
	}
	// «∈» = E2 88 88 тремя кусками по байту: остаток долга переносится через
	// кусок, целиком ушедший в продолжение (ревью волны 5).
	owed = 0
	for i, part := range []string{"\xe2", "\x88", "\x88Z"} {
		if out, owed = dropStrayC1([]byte(part), owed); string(out) != part {
			t.Fatalf("кусок %d %q выброшен как одиночный C1: %q (owed %d)", i, part, out, owed)
		}
	}
	if owed != 0 {
		t.Fatalf("после «∈Z» долг %d", owed)
	}
	clean := []byte("обычный вывод\r\n")
	if out, _ = dropStrayC1(clean, 0); &out[0] != &clean[0] {
		t.Fatal("чистый кусок скопирован — горячий путь всего вывода")
	}
}

// Стенд (tools/screen-frame -no-vt-guards): отказ после паники виден в
// снимке — Untrusted, а не просто withheld; с предохранителями его нет.
func TestSnapshotsFromStreamMarksVtPanicRefusal(t *testing.T) {
	pre := "ok\x1b[2;9r\x1b[4;1H\x1b[M"
	data := []byte(pre + "\x1bcAFTER")
	cuts := []int{2, len(pre), len(data)}
	for i, s := range SnapshotsFromStreamResized(data, 20, 6, nil, cuts, -1, true, nil) {
		if !s.Ready || s.Untrusted {
			t.Fatalf("с предохранителями, разрез %d: ready=%v untrusted=%v", cuts[i], s.Ready, s.Untrusted)
		}
	}
	raw := SnapshotsFromStreamResized(data, 20, 6, nil, cuts, -1, true, nil, WithoutVtGuards())
	if !raw[0].Ready || raw[0].Untrusted {
		t.Fatalf("до паники: ready=%v untrusted=%v", raw[0].Ready, raw[0].Untrusted)
	}
	if raw[1].Ready || !raw[1].Untrusted || raw[1].Frame != "" || raw[1].Grid != nil {
		t.Fatalf("после паники: ready=%v untrusted=%v", raw[1].Ready, raw[1].Untrusted)
	}
	if !raw[2].Ready || raw[2].Untrusted || !strings.Contains(raw[2].Frame, "AFTER") {
		t.Fatalf("после RIS: ready=%v untrusted=%v frame=%q", raw[2].Ready, raw[2].Untrusted, raw[2].Frame)
	}
}
