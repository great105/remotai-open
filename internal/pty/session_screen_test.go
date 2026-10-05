package pty

import (
	"fmt"
	"io"
	"strings"
	"testing"
	"time"
)

type unorderedResizeRaceConn struct {
	data          []byte
	allowRead     chan struct{}
	readReturned  chan struct{}
	resizeApplied chan struct{}
	releaseResize chan struct{}
	closed        chan struct{}
	didRead       bool
}

func (c *unorderedResizeRaceConn) Read(p []byte) (int, error) {
	if !c.didRead {
		select {
		case <-c.allowRead:
			c.didRead = true
			n := copy(p, c.data)
			close(c.readReturned)
			return n, nil
		case <-c.closed:
			return 0, io.EOF
		}
	}
	<-c.closed
	return 0, io.EOF
}

func (c *unorderedResizeRaceConn) Write(p []byte) (int, error) { return len(p), nil }
func (c *unorderedResizeRaceConn) Resize(int, int) error {
	close(c.resizeApplied) // backend geometry is already new here
	<-c.releaseResize      // but the legacy call has not returned yet
	return nil
}
func (c *unorderedResizeRaceConn) Close() error {
	close(c.closed)
	return nil
}
func (c *unorderedResizeRaceConn) shellPID() uint32            { return 0 }
func (c *unorderedResizeRaceConn) currentCWD() (string, error) { return "", nil }

// Сессия отдаёт КАДР ЭКРАНА там, где хвост потока бесполезен.
//
// Поток здесь ровно такой, каким его печатают агенты: экран нарисован ОДИН раз,
// дальше — точечные адресации отдельных ячеек. Если отрезать «хвост» такого
// потока (как делал clientReplayTail), восстановить по нему экран нельзя: в
// хвосте нет ни полного стирания, ни строк, которые не менялись.
func TestSessionScreenFrameRecoversWhatTailCannot(t *testing.T) {
	s := &Session{epoch: "e1", subs: map[chan []byte]*subState{}, dec: newDecTracker()}
	s.startScreen(60, 20)
	defer s.stopScreen()

	// Первая отрисовка — весь экран.
	s.feedScreen([]byte("\x1b[?1049h\x1b[H\x1b[2J"))
	for i := 1; i <= 8; i++ {
		s.feedScreen([]byte("STROKA " + string(rune('0'+i)) + " ---------------\r\n"))
	}
	// Дальше приложение меняет ОТДЕЛЬНЫЕ ячейки — как Ink и ratatui.
	for i := 0; i < 200; i++ {
		s.feedScreen([]byte("\x1b[3;30H*"))
		s.feedScreen([]byte("\x1b[6;30H+"))
	}

	frame, _, _, cols, rows := s.ScreenFrame()
	if frame == "" {
		t.Fatal("сессия не отдала кадр")
	}
	if cols != 60 || rows != 20 {
		t.Fatalf("геометрия кадра %dx%d, ожидалась 60x20 — клиент применит его не в той ширине", cols, rows)
	}
	for _, want := range []string{"STROKA 1", "STROKA 8", "\x1b[?1049h", "\x1b[2J"} {
		if !strings.Contains(frame, want) {
			t.Fatalf("в кадре нет %q — экран не собран:\n%q", want, frame)
		}
	}
	// Кадр обязан быть в разы дешевле хвоста: ради этого всё и делалось.
	if len(frame) > 8<<10 {
		t.Fatalf("кадр %d байт — слишком дорого для экрана 60x20", len(frame))
	}
}

// waitScreenIdle дожидается разбора очереди зеркала: сверка режимов с зеркалом
// (screenAltLive) сознательно отказывается решать при неразобранной очереди.
func waitScreenIdle(t *testing.T, s *Session) {
	t.Helper()
	sc, _ := s.screen.Load().(*sessionScreen)
	if sc == nil {
		t.Fatal("нет зеркала")
	}
	deadline := time.Now().Add(3 * time.Second)
	for sc.queued.Load() != 0 {
		if time.Now().After(deadline) {
			t.Fatal("очередь зеркала не разобралась")
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// Кадр обязан НАЗЫВАТЬ позицию потока, до которой он вывод уже содержит.
//
// Без этого писатель отдавал кадр, снятый впереди клиента: зеркало применяло
// чанк, который ещё лежал в канале подписчика и уходил следом — клиент исполнял
// те же VT-команды второй раз. На тексте это видимый дубль, на LF, вставке
// строк и alt-screen повтор неидемпотентен и двигает экран (внешний аудит
// 13.08.2026, находка T-003).
func TestScreenFrameReportsStreamPosition(t *testing.T) {
	s := &Session{epoch: "e1", subs: map[chan []byte]*subState{}, dec: newDecTracker()}
	s.startScreen(60, 20)
	defer s.stopScreen()

	// Кормим как readLoop: вместе с позицией потока ПОСЛЕ каждого куска.
	var off uint64
	for _, chunk := range []string{"первая строка\r\n", "вторая строка\r\n", "третья\r\n"} {
		off += uint64(len(chunk))
		s.feedScreenAt([]byte(chunk), off)
	}
	waitScreenIdle(t, s)

	frame, _, _, _, _, baseOff := s.ScreenFrameAt()
	if frame == "" {
		t.Fatal("кадр не снялся")
	}
	if baseOff != off {
		t.Fatalf("кадр объявил базу потока %d, а скормлено %d байт — писатель отдаст кадр не на своём месте", baseOff, off)
	}

	// Ещё кусок: база обязана уехать вместе с содержимым, а не остаться старой.
	tail := "четвёртая\r\n"
	off += uint64(len(tail))
	s.feedScreenAt([]byte(tail), off)
	waitScreenIdle(t, s)
	if _, _, _, _, _, b2 := s.ScreenFrameAt(); b2 != off {
		t.Fatalf("после нового вывода база кадра %d вместо %d", b2, off)
	}
}

// A synthetic screen frame may only be inserted at a terminal parser
// boundary. If the live stream currently ends inside CSI/OSC/DCS, sending the
// frame would make the client parser consume part (or all) of the frame as the
// unfinished control string. Merely aborting at the start of the frame is not
// sufficient: the delayed suffix would then be rendered after the frame.
func TestSessionScreenFrameWaitsForParserGround(t *testing.T) {
	tests := []struct {
		name   string
		prefix string
		suffix string
	}{
		{name: "CSI", prefix: "\x1b[38;5", suffix: "m"},
		{name: "OSC", prefix: "\x1b]0;unfinished", suffix: "\x07"},
		{name: "DCS", prefix: "\x1bP1;2|unfinished", suffix: "\x1b\\"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &Session{epoch: "e1", subs: map[chan []byte]*subState{}, dec: newDecTracker()}
			s.startScreen(60, 12)
			defer s.stopScreen()

			var off uint64
			feed := func(part string) {
				off += uint64(len(part))
				s.feedScreenAt([]byte(part), off)
			}
			feed("HELLO")
			feed(tt.prefix)
			if frame, history, _, _, _, base := s.ScreenFrameAt(); frame != "" || history != "" || base != 0 {
				t.Fatalf("snapshot escaped unfinished %s: frame=%q history=%q base=%d", tt.name, frame, history, base)
			}

			feed(tt.suffix)
			feed("\x1b[2;1HAFTER")
			frame, _, _, _, _, base := s.ScreenFrameAt()
			if frame == "" || !strings.Contains(frame, "AFTER") {
				t.Fatalf("snapshot did not resume after completed %s: %q", tt.name, frame)
			}
			if base != off {
				t.Fatalf("snapshot base after completed %s = %d, want %d", tt.name, base, off)
			}
		})
	}
}

func TestSessionScreenFrameWaitsForCompleteUTF8(t *testing.T) {
	s := &Session{epoch: "e1", subs: map[chan []byte]*subState{}, dec: newDecTracker()}
	s.startScreen(60, 12)
	defer s.stopScreen()

	// U+1F600 split exactly as a PTY read may split it. A frame between the
	// halves would force the client's decoder to replace the prefix, then show
	// the trailing continuation bytes as garbage after the frame.
	s.feedScreenAt([]byte{0xf0, 0x9f}, 2)
	if frame, _, _, _, _, _ := s.ScreenFrameAt(); frame != "" {
		t.Fatalf("snapshot escaped split UTF-8 prefix: %q", frame)
	}
	s.feedScreenAt([]byte{0x98, 0x80, 'X'}, 5)
	frame, _, _, _, _, base := s.ScreenFrameAt()
	if frame == "" || !strings.Contains(frame, "😀X") {
		t.Fatalf("snapshot did not resume after complete UTF-8: %q", frame)
	}
	if base != 5 {
		t.Fatalf("snapshot base after complete UTF-8 = %d, want 5", base)
	}
}

func TestScreenMirrorSnapshotWaitsForHeldGrapheme(t *testing.T) {
	m := newScreenMirror(20, 4)
	defer m.Close()
	m.WriteAt([]byte("😀"), uint64(len("😀")))

	// The ANSI parser is already ground, but splitAtClusterBoundary briefly
	// holds a potentially joinable emoji. Make the timing deterministic: a
	// frame must not advance appliedOff past bytes absent from the emulator.
	m.mu.Lock()
	m.pendingAt = time.Now().Add(time.Hour)
	m.mu.Unlock()
	if frame, history, _, _, _, base := m.SnapshotAt(screenMirrorScrollback); frame != "" || history != "" || base != 0 {
		t.Fatalf("snapshot escaped held grapheme: frame=%q history=%q base=%d", frame, history, base)
	}

	m.mu.Lock()
	m.pendingAt = time.Now().Add(-pendingIdle - time.Second)
	m.mu.Unlock()
	frame, _, _, _, _, base := m.SnapshotAt(screenMirrorScrollback)
	if frame == "" || !strings.Contains(frame, "😀") {
		t.Fatalf("snapshot did not resume after held grapheme flush: %q", frame)
	}
	if base != uint64(len("😀")) {
		t.Fatalf("snapshot base after grapheme flush = %d, want %d", base, len("😀"))
	}
}

// Наследованный alt-screen: сканер трекера не видит смерть процесса, и TUI,
// исчезнувший без ?1049l/RIS, числился «в альте» навсегда — modes
// маркера несли ?1049h+мышь, телефон честно входил в alt-screen, и жест
// прокрутки уходил приложению SGR-колесом вместо родной истории (боевой случай
// 13.08.2026, сессия «Отчет»: ноль переключений режимов в 512 КБ хвоста при
// полном наборе в маркере). Правду знает зеркало: vt исполняет весь поток.
func TestModeReassertDropsStaleAltByMirror(t *testing.T) {
	s := &Session{epoch: "e1", subs: map[chan []byte]*subState{}, dec: newDecTracker()}
	s.startScreen(60, 20)
	defer s.stopScreen()

	// Наследство от pty-host: когда-то в сессии жил полноэкранник с мышью,
	// выхода сканер не увидел. Зеркало живёт в основном буфере.
	s.seedModes([]int{1049, 1000, 1002, 1003, 1006, 2004})
	s.feedScreen([]byte("\x1b[1;1Hобычный чат агента\r\n"))
	waitScreenIdle(t, s)

	seq := s.ModeReassertSeq()
	for _, junk := range []string{"\x1b[?1049h", "\x1b[?1047h", "\x1b[?47h", "\x1b[?1048h"} {
		if strings.Contains(seq, junk) {
			t.Fatalf("маркер реассертит наследованный alt-screen (%q): %q", junk, seq)
		}
	}
	if !strings.Contains(seq, "\x1b[?1006h") || !strings.Contains(seq, "\x1b[?2004h") {
		t.Fatalf("вместе с мусором вычищено настоящее: %q", seq)
	}
	if s.altScreen.Load() {
		t.Fatal("altScreen остался true после сверки с зеркалом")
	}

	// Настоящий альт при этом жив: приложение вошло — зеркало знает, маркер несёт.
	s.feedScreen([]byte("\x1b[?1049h\x1b[H"))
	waitScreenIdle(t, s)
	s.bufMu.Lock()
	s.dec.scan([]byte("\x1b[?1049h")) // в бою эти же байты видит readLoop
	s.bufMu.Unlock()
	if seq := s.ModeReassertSeq(); !strings.Contains(seq, "\x1b[?1049h") {
		t.Fatalf("настоящий alt-screen потерян: %q", seq)
	}
}

// readLoop scans DEC modes and advances totalBytes under bufMu, then feeds the
// mirror after unlock. A resync in that exact seam must not treat the still-main
// mirror as proof that the freshly scanned ?1049h is stale.
func TestModePruneWaitsForScannedBytesToReachMirror(t *testing.T) {
	s := &Session{epoch: "e1", subs: map[chan []byte]*subState{}, dec: newDecTracker()}
	s.startScreen(60, 20)
	defer s.stopScreen()

	enterAlt := []byte("\x1b[?1049h")
	s.bufMu.Lock()
	s.buf = append(s.buf, enterAlt...)
	s.totalBytes += uint64(len(enterAlt))
	s.dec.scan(enterAlt)
	s.altScreen.Store(s.dec.altActive())
	s.bufMu.Unlock()
	// Deliberately do not call feedScreenAt yet: this is scan-before-feed.

	if seq := s.ModeReassertSeq(); !strings.Contains(seq, "\x1b[?1049h") {
		t.Fatalf("unfed but current alt mode was destructively pruned: %q", seq)
	}
	if !s.altScreen.Load() {
		t.Fatal("scan-before-feed seam cleared altScreen")
	}
}

// Переполненная очередь сначала честно помечает mirror stale, а следующий
// запрос кадра автоматически пересобирает его из авторитетного Session ring.
// Старое поведение оставляло такую сессию без screen-frame навсегда.
func TestSessionScreenRecoversAfterFeedOverflow(t *testing.T) {
	s := &Session{epoch: "e1", subs: map[chan []byte]*subState{}, dec: newDecTracker()}
	s.startScreen(60, 20)
	defer s.stopScreen()

	// Детерминированно удерживаем worker и заполняем его channel: следующий
	// chunk обязан попасть в default-ветку feedScreen и пометить mirror stale.
	sc, _ := s.screen.Load().(*sessionScreen)
	if sc == nil {
		t.Fatal("нет зеркала")
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	sc.queued.Add(1)
	sc.feed <- screenChunk{
		data: []byte{'x'},
		beforeApply: func() {
			close(entered)
			<-release
		},
	}
	<-entered
	for i := 0; i < screenFeedDepth+1; i++ {
		s.feedScreen([]byte{'x'})
	}
	if !s.ScreenStale() {
		close(release)
		t.Fatal("переполнение канала не пометило зеркало stale")
	}
	close(release)
	waitScreenIdle(t, s)

	// readLoop всегда держит тот же поток в ring; именно он является источником
	// правды для пересборки после потери chunks в неблокирующей очереди mirror.
	// A truncated tail becomes delivery-authoritative only at a parser-proven
	// full reset; without RIS it must be withheld from an already-correct raw
	// client rather than overwrite it with partial state.
	authoritative := []byte("\x1bc\x1b[2J\x1b[HRECOVERED-AFTER-OVERFLOW\r\n")
	s.bufMu.Lock()
	s.buf = append(s.buf[:0], authoritative...)
	s.totalBytes = 50_000 + uint64(len(authoritative))
	wantBase := s.totalBytes
	s.bufMu.Unlock()

	frame, _, _, _, _, base := s.ScreenFrameAt()
	if !strings.Contains(frame, "RECOVERED-AFTER-OVERFLOW") {
		t.Fatalf("зеркало не восстановилось после overflow: %q", frame)
	}
	if base != wantBase {
		t.Fatalf("база восстановленного кадра %d, want %d", base, wantBase)
	}
	if s.ScreenStale() {
		t.Fatal("зеркало осталось stale после автовосстановления")
	}
}

// Отставшее зеркало не остаётся stale до конца Session: запрос
// кадра пересобирает его из авторитетного Session ring.
func TestSessionScreenRecoversFromStaleUsingAuthoritativeRing(t *testing.T) {
	s := &Session{epoch: "e1", subs: map[chan []byte]*subState{}, dec: newDecTracker()}
	s.startScreen(60, 12)
	defer s.stopScreen()

	data := []byte("before\r\nafter-recovery\r\n")
	s.bufMu.Lock()
	s.buf = append(s.buf, data...)
	// The complete suffix from mirror offset 0 is still present, so this is the
	// preferred in-place continuity recovery rather than a truncated-tail guess.
	s.totalBytes = uint64(len(data))
	wantBase := s.totalBytes
	s.bufMu.Unlock()
	sc, _ := s.screen.Load().(*sessionScreen)
	sc.stale.Store(true)

	frame, _, _, _, _, base := s.ScreenFrameAt()
	if frame == "" || !strings.Contains(frame, "after-recovery") {
		t.Fatalf("stale mirror не восстановилось: frame=%q", frame)
	}
	if base != wantBase {
		t.Fatalf("база кадра %d, want %d", base, wantBase)
	}
	if s.ScreenStale() {
		t.Fatal("зеркало осталось stale после пересборки")
	}
}

// После первого потерянного chunk continuity уже разорвана. Post-gap chunks
// нельзя продолжать применять: их высокий off скроет дыру от ring catch-up.
func TestStaleMirrorStopsAtFirstGapBeforeRecovery(t *testing.T) {
	s := &Session{epoch: "e1", subs: map[chan []byte]*subState{}, dec: newDecTracker()}
	s.startScreen(60, 12)
	defer s.stopScreen()

	s.feedScreenAt([]byte("A"), 1)
	waitScreenIdle(t, s)
	sc, _ := s.screen.Load().(*sessionScreen)
	sc.stale.Store(true) // middle chunk was dropped at off=2
	s.feedScreenAt([]byte("POST"), 6)
	waitScreenIdle(t, s)

	sc.mu.Lock()
	m := sc.mirror
	sc.mu.Unlock()
	m.mu.Lock()
	applied := m.appliedOff
	m.mu.Unlock()
	if applied != 1 {
		t.Fatalf("post-gap chunk advanced appliedOff to %d; recovery will miss the hole", applied)
	}
}

// Ring tail is not a self-contained terminal transcript: the active ?1049h
// may have been evicted and the remaining bytes may be only cell diffs. Such a
// best-effort recovered frame must never disprove authoritative host DEC modes.
func TestStatefulTailRecoveryCannotPruneActiveAltMode(t *testing.T) {
	s := &Session{epoch: "e1", subs: map[chan []byte]*subState{}, dec: newDecTracker()}
	s.startScreen(60, 12)
	defer s.stopScreen()
	s.seedModes([]int{1049, 1006, 2004})

	tail := []byte("\x1b[3;7HSTATEFUL-DIFF") // no self-contained alt-screen entry
	s.bufMu.Lock()
	s.buf = append(s.buf[:0], tail...)
	s.totalBytes = 100_000 + uint64(len(tail))
	s.bufMu.Unlock()
	sc, _ := s.screen.Load().(*sessionScreen)
	sc.stale.Store(true)
	if frame, _, _, _, _, _ := s.ScreenFrameAt(); frame != "" {
		t.Fatalf("truncated stateful tail was published over the raw client: %q", frame)
	}
	if !s.ScreenStale() {
		t.Fatal("delivery-nonauthoritative tail was reported as a safe screen")
	}

	seq := s.ModeReassertSeq()
	if !strings.Contains(seq, "\x1b[?1049h") || !strings.Contains(seq, "\x1b[?1006h") {
		t.Fatalf("stateful tail pruned authoritative active modes: %q", seq)
	}
	if !s.altScreen.Load() {
		t.Fatal("stateful tail cleared authoritative altScreen")
	}
}

func TestNonAuthoritativeTailRecoveryCanLeaveStaleAltOnLaterRIS(t *testing.T) {
	s := &Session{epoch: "e1", subs: map[chan []byte]*subState{}, dec: newDecTracker()}
	s.startScreen(60, 12)
	defer s.stopScreen()
	s.seedModes([]int{1049, 1006, 2004})

	// Force a truncated-tail recovery, whose mirror correctly cannot disprove
	// the seeded modes by itself.
	tail := []byte("\x1b[3;7HSTATEFUL-DIFF")
	s.bufMu.Lock()
	s.buf = append(s.buf[:0], tail...)
	s.totalBytes = 100_000 + uint64(len(tail))
	s.bufMu.Unlock()
	sc, _ := s.screen.Load().(*sessionScreen)
	sc.stale.Store(true)
	if frame, _, _, _, _, _ := s.ScreenFrameAt(); frame != "" {
		t.Fatalf("truncated stateful tail was published before RIS: %q", frame)
	}

	// A later RIS is a new authoritative baseline even though that recovered
	// tail never was. readLoop scans DEC and feeds the mirror in this order.
	ris := []byte("\x1bcAFTER-RIS")
	s.bufMu.Lock()
	s.buf = append(s.buf, ris...)
	s.totalBytes += uint64(len(ris))
	off := s.totalBytes
	s.dec.scan(ris)
	s.altScreen.Store(s.dec.altActive())
	s.bufMu.Unlock()
	s.feedScreenAt(ris, off)
	waitScreenIdle(t, s)
	if frame, _, _, _, _, _ := s.ScreenFrameAt(); !strings.Contains(frame, "AFTER-RIS") {
		t.Fatalf("parser-proven RIS did not reauthorize delivery: %q", frame)
	}
	if s.ScreenStale() {
		t.Fatal("parser-proven RIS left delivery marked stale")
	}

	if seq := s.ModeReassertSeq(); strings.Contains(seq, "\x1b[?1049h") || strings.Contains(seq, "\x1b[?1006h") {
		t.Fatalf("later RIS could not clear stale recovered modes: %q", seq)
	}
	if s.altScreen.Load() {
		t.Fatal("later RIS left altScreen active")
	}
}

// Resize, пришедший пока stale mirror тяжело пересобирается, не должен быть
// отменён swap'ом fresh mirror, созданного в старой геометрии.
func TestSessionScreenRecoveryKeepsConcurrentResize(t *testing.T) {
	s := &Session{epoch: "e1", subs: map[chan []byte]*subState{}, dec: newDecTracker()}
	s.startScreen(60, 12)
	defer s.stopScreen()
	data := []byte("RECOVERY-WITH-RESIZE\r\n")
	s.bufMu.Lock()
	s.buf = append(s.buf, data...)
	s.totalBytes = uint64(len(data))
	s.bufMu.Unlock()
	sc, _ := s.screen.Load().(*sessionScreen)
	sc.stale.Store(true)

	atSwap := make(chan struct{})
	release := make(chan struct{})
	done := make(chan bool, 1)
	go func() {
		done <- s.recoverScreenWithHook(sc, func() {
			close(atSwap)
			<-release
		})
	}()
	<-atSwap
	resizeDone := make(chan struct{})
	go func() {
		s.resizeScreen(90, 20)
		close(resizeDone)
	}()
	select {
	case <-resizeDone:
		t.Fatal("ordered resize bypassed recovery already finalizing a stale mirror")
	case <-time.After(30 * time.Millisecond):
	}
	close(release)
	if !<-done {
		t.Fatal("stale recovery aborted")
	}
	<-resizeDone
	frame, _, _, cols, rows, _ := s.ScreenFrameAt()
	if cols != 90 || rows != 20 || !strings.Contains(frame, "RECOVERY-WITH-RESIZE") {
		t.Fatalf("recovery откатил resize: %dx%d frame=%q", cols, rows, frame)
	}
}

// Output ordered before frResizeAck belongs to the old PTY geometry. If the
// mirror dropped that output, the ACK callback must recover it before resizing
// the mirror; replaying old-width CUP/text after the resize produces a
// different grid from the real terminal.
func TestStaleMirrorCatchesPreResizeBytesBeforeGeometryChange(t *testing.T) {
	const (
		oldCols = 10
		newCols = 20
		rows    = 4
	)
	s := &Session{epoch: "e1", subs: map[chan []byte]*subState{}, dec: newDecTracker()}
	s.startScreen(oldCols, rows)
	defer s.stopScreen()

	initial := []byte("\x1b[H\x1b[2JBASE")
	s.bufMu.Lock()
	s.buf = append(s.buf, initial...)
	s.totalBytes += uint64(len(initial))
	initialOff := s.totalBytes
	s.bufMu.Unlock()
	s.feedScreenAt(initial, initialOff)
	waitScreenIdle(t, s)

	// This old-geometry output reached the authoritative Session ring but was
	// the first chunk dropped by the async mirror queue.
	missed := []byte("\x1b[1;9HABCD")
	s.bufMu.Lock()
	s.buf = append(s.buf, missed...)
	s.totalBytes += uint64(len(missed))
	finalOff := s.totalBytes
	s.bufMu.Unlock()
	sc, _ := s.screen.Load().(*sessionScreen)
	sc.stale.Store(true)

	// Models the ordered ACK completion callback.
	s.resizeScreen(newCols, rows)
	got, _, _, gotCols, gotRows, gotOff := s.ScreenFrameAt()

	ref := newScreenMirror(oldCols, rows)
	defer ref.Close()
	ref.WriteAt(initial, initialOff)
	ref.WriteAt(missed, finalOff) // real terminal parsed this before resize
	ref.Resize(newCols, rows)
	want, _, _, _, _, wantOff := ref.SnapshotAt(screenMirrorScrollback)
	if got != want || gotCols != newCols || gotRows != rows || gotOff != wantOff {
		t.Fatalf("stale pre-resize output parsed in wrong geometry:\n got  %dx%d off=%d %q\n want %dx%d off=%d %q", gotCols, gotRows, gotOff, got, newCols, rows, wantOff, want)
	}
}

func TestUnrecoverableStaleResizeWithholdsBlankReplacementFrame(t *testing.T) {
	s := &Session{epoch: "e1", subs: map[chan []byte]*subState{}, dec: newDecTracker()}
	s.startScreen(10, 4)
	defer s.stopScreen()

	// The raw client can still have a perfectly correct screen even though the
	// mirror alone lost continuity. A resize fallback may establish a new
	// geometry baseline, but publishing its synthetic blank frame would wipe
	// that live client while a quiet shell/TUI has not repainted yet.
	s.bufMu.Lock()
	s.totalBytes = 100_000
	s.buf = []byte("truncated-stateful-tail")
	s.bufMu.Unlock()
	sc, _ := s.screen.Load().(*sessionScreen)
	sc.stale.Store(true)
	if !s.resetStaleScreenForResize(sc, 20, 6) {
		t.Fatal("stale resize fallback did not swap the mirror")
	}
	if frame, history, _, _, _, _ := s.ScreenFrameAt(); frame != "" || history != "" {
		t.Fatalf("unrecoverable resize published destructive blank frame=%q history=%q", frame, history)
	}
}

func TestScreenResizeWaitsForQueuedOldGeometryOutput(t *testing.T) {
	const (
		oldCols = 10
		newCols = 20
		rows    = 4
	)
	s := &Session{epoch: "e1", subs: map[chan []byte]*subState{}, dec: newDecTracker()}
	s.startScreen(oldCols, rows)
	defer s.stopScreen()

	initial := []byte("\x1b[H\x1b[2JBASE")
	s.bufMu.Lock()
	s.buf = append(s.buf, initial...)
	s.totalBytes += uint64(len(initial))
	initialOff := s.totalBytes
	s.bufMu.Unlock()
	s.feedScreenAt(initial, initialOff)
	waitScreenIdle(t, s)

	queued := []byte("\x1b[1;9HABCD")
	s.bufMu.Lock()
	s.buf = append(s.buf, queued...)
	s.totalBytes += uint64(len(queued))
	finalOff := s.totalBytes
	s.bufMu.Unlock()
	sc, _ := s.screen.Load().(*sessionScreen)
	dequeued := make(chan struct{})
	release := make(chan struct{})
	sc.queued.Add(int64(len(queued)))
	sc.feed <- screenChunk{
		data: queued,
		off:  finalOff,
		beforeApply: func() {
			close(dequeued)
			<-release
		},
	}
	<-dequeued

	resizeDone := make(chan struct{})
	go func() {
		s.resizeScreen(newCols, rows) // ordered ACK callback
		close(resizeDone)
	}()
	select {
	case <-resizeDone:
		close(release)
		t.Fatal("resize overtook dequeued pre-ACK screen output")
	case <-time.After(30 * time.Millisecond):
	}
	close(release)
	<-resizeDone

	got, _, _, gotCols, gotRows, gotOff := s.ScreenFrameAt()
	ref := newScreenMirror(oldCols, rows)
	defer ref.Close()
	ref.WriteAt(initial, initialOff)
	ref.WriteAt(queued, finalOff)
	ref.Resize(newCols, rows)
	want, _, _, _, _, wantOff := ref.SnapshotAt(screenMirrorScrollback)
	if got != want || gotCols != newCols || gotRows != rows || gotOff != wantOff {
		t.Fatalf("queued pre-resize output crossed geometry marker:\n got  %dx%d off=%d %q\n want %dx%d off=%d %q", gotCols, gotRows, gotOff, got, newCols, rows, wantOff, want)
	}
}

func TestScreenResizeFlushesCompletePendingGraphemeInOldGeometry(t *testing.T) {
	const (
		oldCols = 10
		newCols = 20
		rows    = 4
	)
	s := &Session{epoch: "e1", subs: map[chan []byte]*subState{}, dec: newDecTracker()}
	s.startScreen(oldCols, rows)
	defer s.stopScreen()

	// splitAtClusterBoundary deliberately holds a terminal emoji briefly so a
	// following VS/ZWJ/modifier can join it. The resize ACK is an ordered
	// control-plane boundary: the complete UTF-8 glyph was delivered to the
	// real PTY before resize and must therefore be committed in the old grid.
	pre := append([]byte("\x1b[1;9H"), []byte{0xf0, 0x9f, 0x98, 0x80}...)
	s.bufMu.Lock()
	s.buf = append(s.buf, pre...)
	s.totalBytes += uint64(len(pre))
	preOff := s.totalBytes
	s.bufMu.Unlock()
	s.feedScreenAt(pre, preOff)
	waitScreenIdle(t, s)

	s.resizeScreen(newCols, rows)
	post := []byte("X")
	s.bufMu.Lock()
	s.buf = append(s.buf, post...)
	s.totalBytes += uint64(len(post))
	postOff := s.totalBytes
	s.bufMu.Unlock()
	s.feedScreenAt(post, postOff)
	waitScreenIdle(t, s)

	got, _, _, gotCols, gotRows, gotOff := s.ScreenFrameAt()
	ref := newScreenMirror(oldCols, rows)
	defer ref.Close()
	ref.WriteAt(pre, preOff)
	ref.mu.Lock()
	ref.pendingAt = time.Now().Add(-pendingIdle - time.Second)
	ref.flushPendingLocked()
	ref.mu.Unlock()
	ref.Resize(newCols, rows)
	ref.WriteAt(post, postOff)
	want, _, _, _, _, wantOff := ref.SnapshotAt(screenMirrorScrollback)
	if got != want || gotCols != newCols || gotRows != rows || gotOff != wantOff {
		t.Fatalf("pending pre-resize grapheme crossed geometry marker:\n got  %dx%d off=%d %q\n want %dx%d off=%d %q", gotCols, gotRows, gotOff, got, newCols, rows, wantOff, want)
	}
}

func TestUnorderedBackendResizeSerializesPostResizeOutput(t *testing.T) {
	const (
		oldCols = 10
		newCols = 20
		rows    = 4
	)
	post := []byte("\x1b[1;9HABCD")
	conn := &unorderedResizeRaceConn{
		data:          post,
		allowRead:     make(chan struct{}),
		readReturned:  make(chan struct{}),
		resizeApplied: make(chan struct{}),
		releaseResize: make(chan struct{}),
		closed:        make(chan struct{}),
	}
	s := newSession("unordered-resize", "cwd", "shell", 1, conn, time.Now())
	s.setBornSize(oldCols, rows)
	go s.readLoop(nil)

	deadline := time.Now().Add(3 * time.Second)
	for s.screen.Load() == nil {
		if time.Now().After(deadline) {
			t.Fatal("screen worker did not start")
		}
		time.Sleep(time.Millisecond)
	}

	resizeDone := make(chan error, 1)
	go func() { resizeDone <- s.Resize(newCols, rows) }()
	<-conn.resizeApplied
	close(conn.allowRead) // output produced after backend changed geometry
	<-conn.readReturned

	// Give the broken implementation a deterministic chance to apply the
	// post-resize bytes to the old mirror while Resize is still blocked. The
	// fixed implementation holds the single fallback ordering lane, so the
	// mirror cannot advance until the resize marker has been queued.
	oldApplied := false
	applyDeadline := time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(applyDeadline) {
		sc, _ := s.screen.Load().(*sessionScreen)
		if sc != nil {
			sc.mu.Lock()
			mirror := sc.mirror
			sc.mu.Unlock()
			if mirror != nil {
				mirror.mu.Lock()
				oldApplied = mirror.appliedOff >= uint64(len(post))
				mirror.mu.Unlock()
			}
		}
		if oldApplied {
			break
		}
		time.Sleep(time.Millisecond)
	}
	close(conn.releaseResize)
	if err := <-resizeDone; err != nil {
		t.Fatal(err)
	}
	finalDeadline := time.Now().Add(3 * time.Second)
	for {
		sc, _ := s.screen.Load().(*sessionScreen)
		applied := false
		if sc != nil {
			sc.mu.Lock()
			mirror := sc.mirror
			sc.mu.Unlock()
			if mirror != nil {
				mirror.mu.Lock()
				applied = mirror.appliedOff >= uint64(len(post))
				mirror.mu.Unlock()
			}
		}
		if applied {
			break
		}
		if time.Now().After(finalDeadline) {
			t.Fatal("post-resize output did not reach mirror")
		}
		time.Sleep(time.Millisecond)
	}

	if oldApplied {
		t.Fatal("post-resize output committed while fallback resize owned the output lane")
	}
	s.bufMu.Lock()
	raw := append([]byte(nil), s.buf...)
	s.bufMu.Unlock()
	if !strings.Contains(string(raw), "ABCD") {
		t.Fatalf("raw delivery was lost while screen snapshot was invalidated: %q", raw)
	}
	if frame, history, _, _, _, _ := s.ScreenFrameAt(); frame != "" || history != "" {
		t.Fatalf("non-ACK resize published an unprovable frame=%q history=%q", frame, history)
	}
	if !s.ScreenStale() {
		t.Fatal("non-ACK resize did not expose withheld screen state")
	}

	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-s.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("readLoop did not stop")
	}
}

func TestUnorderedBackendWithholdsScreenBeforeFirstResize(t *testing.T) {
	conn := &unorderedResizeRaceConn{
		allowRead:     make(chan struct{}),
		readReturned:  make(chan struct{}),
		resizeApplied: make(chan struct{}),
		releaseResize: make(chan struct{}),
		closed:        make(chan struct{}),
	}
	s := newSession("unordered-no-pending", "cwd", "shell", 1, conn, time.Now())
	s.startScreen(20, 4)
	defer s.stopScreen()

	raw := []byte("\x1bcVISIBLE-ONLY-AS-RAW")
	s.bufMu.Lock()
	s.buf = append(s.buf, raw...)
	s.totalBytes += uint64(len(raw))
	off := s.totalBytes
	s.bufMu.Unlock()
	s.feedScreenAt(raw, off)
	waitScreenIdle(t, s)

	if frame, history, _, _, _, _ := s.ScreenFrameAt(); frame != "" || history != "" {
		t.Fatalf("unordered backend captured a pre-resize pending frame=%q history=%q", frame, history)
	}
	if !s.ScreenStale() {
		t.Fatal("unordered backend did not advertise conservative raw-only delivery")
	}
}

func TestUnorderedBackendResizeWithholdsFrameWhenPreReadLosesLane(t *testing.T) {
	pre := []byte("\x1bc\x1b[1;9HPRE-RESIZE")
	conn := &unorderedResizeRaceConn{
		data:          pre,
		allowRead:     make(chan struct{}),
		readReturned:  make(chan struct{}),
		resizeApplied: make(chan struct{}),
		releaseResize: make(chan struct{}),
		closed:        make(chan struct{}),
	}
	s := newSession("unordered-pre-read", "cwd", "shell", 1, conn, time.Now())
	s.setBornSize(10, 4)
	readAtCommitSeam := make(chan struct{})
	releaseCommit := make(chan struct{})
	s.beforeOutputApply = func() {
		close(readAtCommitSeam) // conn.Read already returned the old bytes
		<-releaseCommit
	}
	go s.readLoop(nil)

	deadline := time.Now().Add(3 * time.Second)
	for s.screen.Load() == nil {
		if time.Now().After(deadline) {
			t.Fatal("screen worker did not start")
		}
		time.Sleep(time.Millisecond)
	}
	close(conn.allowRead)
	<-conn.readReturned
	<-readAtCommitSeam

	resizeDone := make(chan error, 1)
	go func() { resizeDone <- s.Resize(20, 4) }()
	<-conn.resizeApplied
	close(conn.releaseResize)
	if err := <-resizeDone; err != nil {
		t.Fatal(err)
	}
	close(releaseCommit)

	deadline = time.Now().Add(3 * time.Second)
	for {
		s.bufMu.Lock()
		got := append([]byte(nil), s.buf...)
		s.bufMu.Unlock()
		if strings.Contains(string(got), "PRE-RESIZE") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("already-read raw output was lost: %q", got)
		}
		time.Sleep(time.Millisecond)
	}
	if frame, history, _, _, _, _ := s.ScreenFrameAt(); frame != "" || history != "" {
		t.Fatalf("pre-read/non-ACK ambiguity published frame=%q history=%q", frame, history)
	}
	if !s.ScreenStale() {
		t.Fatal("stale-generation RIS incorrectly re-authorized delivery")
	}

	// Even a Read begun after Resize returns can consume pre-resize bytes that
	// were already buffered in the kernel/SSH transport. Its apparently new
	// generation and RIS therefore cannot prove new-geometry provenance.
	fresh := []byte("\x1bcNEW-GENERATION")
	s.bufMu.Lock()
	s.buf = append(s.buf, fresh...)
	s.totalBytes += uint64(len(fresh))
	freshOff := s.totalBytes
	s.bufMu.Unlock()
	s.feedScreenAt(fresh, freshOff)
	waitScreenIdle(t, s)
	if frame, history, _, _, _, _ := s.ScreenFrameAt(); frame != "" || history != "" {
		t.Fatalf("buffered old-grid RIS re-authorized frame=%q history=%q", frame, history)
	}
	if !s.ScreenStale() {
		t.Fatal("non-ACK invalidation was not permanent until stream reset")
	}

	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-s.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("readLoop did not stop")
	}
}

func TestNonACKInvalidationWinsQueuedRISWorker(t *testing.T) {
	s := &Session{epoch: "e1", subs: map[chan []byte]*subState{}, dec: newDecTracker()}
	s.startScreen(20, 4)
	defer s.stopScreen()

	sc, _ := s.screen.Load().(*sessionScreen)
	if sc == nil {
		t.Fatal("screen worker did not start")
	}
	raw := []byte("\x1bcQUEUED-OLD-GRID")
	entered := make(chan struct{})
	release := make(chan struct{})
	ack := make(chan struct{})
	sc.queued.Add(int64(len(raw)))
	sc.feed <- screenChunk{
		data:       raw,
		off:        uint64(len(raw)),
		generation: 99, // even an apparently new generation is not transport proof
		beforeApply: func() {
			close(entered)
			<-release
		},
		barrier: ack,
	}
	<-entered

	// The worker has already dequeued a RIS chunk. Invalidation must still win
	// if that worker reaches its authorization decision afterwards.
	s.invalidateScreenDelivery(1)
	close(release)
	select {
	case <-ack:
	case <-time.After(3 * time.Second):
		t.Fatal("queued RIS worker did not finish")
	}

	if frame, history, _, _, _, _ := s.ScreenFrameAt(); frame != "" || history != "" {
		t.Fatalf("queued RIS raced invalidation and published frame=%q history=%q", frame, history)
	}
	if !s.ScreenStale() {
		t.Fatal("queued RIS worker restored permanently invalidated authority")
	}
}

// Кормление зеркала НИКОГДА не должно тормозить чтение PTY: цена задержки —
// замерший терминал у человека, а цена пропуска — всего лишь откат на хвост.
func TestFeedScreenNeverBlocks(t *testing.T) {
	s := &Session{epoch: "e1", subs: map[chan []byte]*subState{}, dec: newDecTracker()}
	s.startScreen(60, 20)
	defer s.stopScreen()

	big := make([]byte, 32<<10)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < screenFeedDepth*10; i++ {
			s.feedScreen(big)
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("feedScreen заблокировался — так он остановит чтение PTY и терминал замрёт")
	}
}

// Без зеркала (старая сессия, восстановленная не через readLoop) всё обязано
// работать по-прежнему: кадра нет, вызывающий откатывается на хвост.
func TestSessionWithoutScreenIsSafe(t *testing.T) {
	s := &Session{epoch: "e1", subs: map[chan []byte]*subState{}, dec: newDecTracker()}
	s.feedScreen([]byte("что-то"))
	s.resizeScreen(80, 24)
	if frame, history, _, _, _ := s.ScreenFrame(); frame != "" || history != "" {
		t.Fatal("сессия без зеркала отдала снапшот")
	}
	if s.ScreenStale() {
		t.Fatal("сессия без зеркала считается отставшей")
	}
	s.stopScreen() // не должно паниковать
}

// Геометрия зеркала обязана следовать за PTY: кадр, снятый в чужой ширине,
// ляжет не на свои строки — это ровно тот дефект, который мы чиним.
func TestSessionScreenFollowsGeometry(t *testing.T) {
	s := &Session{epoch: "e1", subs: map[chan []byte]*subState{}, dec: newDecTracker()}
	s.startScreen(80, 24)
	defer s.stopScreen()
	s.resizeScreen(48, 30)
	if _, _, _, cols, rows := s.ScreenFrame(); cols != 48 || rows != 30 {
		t.Fatalf("зеркало осталось в %dx%d после смены размера PTY", cols, rows)
	}
}

// Снапшот обычной оболочки несёт и ИСТОРИЮ: scrollback ПЛЮС текущие видимые
// строки. Экранные строки присутствуют и в history, и в кадре — это НЕ дубль
// при применении: клиент пишет историю в чистый терминал (видимые строки
// финишируют на экране), затем кадр гасит экран через ED2 (в xterm без
// scrollback-сдвига) и перерисовывает его абсолютно. В scrollback клиента
// остаются ровно строки scrollback. Точность шва ячейка в ячейку проверяет
// проба на настоящем xterm.js (build/qa/probe-screen-frame.mjs).
func TestSessionScreenFrameCarriesHistory(t *testing.T) {
	s := &Session{epoch: "e1", subs: map[chan []byte]*subState{}, dec: newDecTracker()}
	s.startScreen(40, 10)
	defer s.stopScreen()

	var sb strings.Builder
	for i := 1; i <= 25; i++ {
		fmt.Fprintf(&sb, "STROKA %02d", i)
		if i < 25 {
			sb.WriteString("\r\n")
		}
	}
	s.feedScreen([]byte(sb.String()))

	frame, history, histLines, cols, rows := s.ScreenFrame()
	if frame == "" {
		t.Fatal("сессия не отдала кадр")
	}
	if cols != 40 || rows != 10 {
		t.Fatalf("геометрия снапшота %dx%d, ожидалась 40x10", cols, rows)
	}
	// 25 строк на экране из 10: история = scrollback (01..15) + видимые (16..25).
	if histLines != 25 {
		t.Fatalf("в истории %d строк, ожидалось 25: %q", histLines, history)
	}
	i01 := strings.Index(history, "STROKA 01")
	i15 := strings.Index(history, "STROKA 15")
	i16 := strings.Index(history, "STROKA 16")
	i25 := strings.Index(history, "STROKA 25")
	if i01 < 0 || i15 < 0 || i16 < 0 || i25 < 0 || !(i01 < i15 && i15 < i16 && i16 < i25) {
		t.Fatalf("история собрана не так (ожидался порядок 01..15..16..25): %q", history)
	}
	if !strings.Contains(frame, "STROKA 25") {
		t.Fatalf("в кадре нет последней строки: %q", frame)
	}
}
