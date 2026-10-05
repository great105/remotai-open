package pty

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"syscall"
	"testing"
	"time"
)

type interruptedReadablePTY struct {
	readableBeforeResizePTY
	interruptions int
}

func (p *interruptedReadablePTY) readAvailable(dst []byte) (int, error) {
	if p.interruptions > 0 {
		p.interruptions--
		return 0, &os.PathError{Op: "poll", Path: "pty", Err: syscall.EINTR}
	}
	return p.readableBeforeResizePTY.readAvailable(dst)
}

func TestHostInterruptedPollPreservesOutputAndResizeBoundary(t *testing.T) {
	p := &interruptedReadablePTY{
		readableBeforeResizePTY: readableBeforeResizePTY{pending: []byte("still alive")},
		interruptions:           3,
	}
	out := make(chan hostOutbound, 2)
	h := &host{pty: p, dec: newDecTracker(), sub: &hostSubscription{out: out, broken: make(chan struct{})}, dead: make(chan struct{})}
	if err := h.queueHostResizeWith(out, make(chan struct{}), resizePayload(90, 25, 7, true), p.Resize); err != nil {
		t.Fatalf("a signal must not stop output or reject resize: %v", err)
	}
	first, second := <-out, <-out
	if first.typ != frOutput || string(first.payload) != "still alive" || second.typ != frResizeAck {
		t.Fatalf("EINTR must be retried before the resize boundary: %v, %v", first, second)
	}
}

type streamStateTestConn struct {
	state     hostStreamState
	modes     []int
	unordered bool
}

func (c *streamStateTestConn) Read([]byte) (int, error)    { return 0, io.EOF }
func (c *streamStateTestConn) Write(p []byte) (int, error) { return len(p), nil }
func (c *streamStateTestConn) Resize(int, int) error       { return nil }
func (c *streamStateTestConn) ResizeOrdered(_ int, _ int, afterApply func()) error {
	if afterApply != nil {
		afterApply()
	}
	return nil
}
func (c *streamStateTestConn) resizeOrderingSupported() bool {
	return c.state.Epoch != "" && !c.unordered
}
func (c *streamStateTestConn) Close() error                { return nil }
func (c *streamStateTestConn) shellPID() uint32            { return 0 }
func (c *streamStateTestConn) currentCWD() (string, error) { return "", nil }
func (c *streamStateTestConn) streamState() (hostStreamState, bool) {
	return c.state, c.state.Epoch != ""
}
func (c *streamStateTestConn) hostModes() []int { return c.modes }

// Возврат клиента с известной позицией: хост до-сылает только хвост.
//
// Дефект, который здесь закрывается. serveClient безусловно отдавал ВЕСЬ свой
// буфер (до 4 МБ), а клиент трубы не отличает историю от живого потока
// (`case frOutput, frSnapshot:` — один и тот же путь). При восстановлении связи
// на ЖИВОЙ сессии это разворачивало всю историю на экране заново; хуже того,
// дубль оседал в кольце сессии и доставался всем, кто подключится позже.
//
// Проверяется чистая арифметика выбора хвоста (snapshotTail — та самая
// функция, которую зовёт serveClient); живой прогон трубы делают
// persist_*_test.go.
func TestHostSendsOnlyTailForKnownPosition(t *testing.T) {
	// Кольцо хранит хвост: выдано 1000 байт, в буфере последние 400.
	h := &host{produced: 1000, buf: make([]byte, 400)}
	for i := range h.buf {
		h.buf[i] = byte('a' + i%26)
	}
	bufStart := h.produced - uint64(len(h.buf)) // 600

	cases := []struct {
		name  string
		known uint64
		want  int
	}{
		{"клиент всё видел — досылать нечего", 1000, 0},
		{"отстал на 150 байт — только они", 850, 150},
		{"ровно на границе кольца — весь буфер", bufStart, 400},
		{"позиция вытеснена из кольца — весь буфер", 100, 400},
		{"позиции нет (старый клиент) — весь буфер", 0, 400},
		{"позиция из будущего (мусор) — весь буфер", 5000, 400},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tail := snapshotTail(h.buf, h.produced, c.known)
			if len(tail) != c.want {
				t.Fatalf("до-слано %d байт, ожидалось %d", len(tail), c.want)
			}
			// И главное: хвост обязан быть КОНЦОМ буфера, а не его началом —
			// иначе клиент получит уже показанное и увидит дубль.
			if len(tail) > 0 && &tail[len(tail)-1] != &h.buf[len(h.buf)-1] {
				t.Fatal("до-сылка не совпадает с концом кольца")
			}
		})
	}
}

// Вытесненная позиция + буфер БОЛЬШЕ clientReplayLimit: до-сылка режется до
// свежего хвоста, а не уходит целиком (до 4 МБ мидстримом живым подписчикам —
// лавина, рвущая медленное мобильное плечо; см. snapshotTail).
func TestSnapshotTailCapsEvictedReplay(t *testing.T) {
	buf := make([]byte, clientReplayLimit+300_000)
	for i := range buf {
		buf[i] = byte('a' + i%26)
	}
	produced := uint64(len(buf)) + 50_000 // кольцо полное, 50К уже вытеснено
	start := produced - uint64(len(buf))

	// Позиция ВНУТРИ кольца — точный хвост без ограничения, даже если он
	// длиннее clientReplayLimit: живой путь reattach, пропуски недопустимы.
	tail := snapshotTail(buf, produced, start+100)
	if len(tail) != len(buf)-100 {
		t.Fatalf("позиция в кольце: до-слано %d вместо %d", len(tail), len(buf)-100)
	}
	if &tail[len(tail)-1] != &buf[len(buf)-1] {
		t.Fatal("точный хвост не совпадает с концом кольца")
	}

	// Позиция ВЫТЕСНЕНА — только свежие clientReplayLimit, и это КОНЕЦ буфера.
	known := start - 1
	tail = snapshotTail(buf, produced, known)
	if len(tail) != clientReplayLimit {
		t.Fatalf("вытесненная позиция: до-слано %d, ожидался предел %d", len(tail), clientReplayLimit)
	}
	if &tail[len(tail)-1] != &buf[len(buf)-1] {
		t.Fatal("ограниченный хвост обязан быть свежим концом кольца, а не его началом")
	}
	// ...и не дублем: первый до-сланный байт новее known, значит в кольце
	// сессии ничего не задвоится.
	if produced-uint64(len(tail)) <= known {
		t.Fatal("до-сланный хвост пересекается с уже показанным — кольцо сессии задвоится")
	}

	// Буфер меньше предела при вытесненной позиции — отдаётся целиком,
	// прежнее поведение.
	small := buf[:1000]
	if got := snapshotTail(small, uint64(len(small))+10, 5); len(got) != len(small) {
		t.Fatalf("малый буфер: до-слано %d вместо %d", len(got), len(small))
	}
}

// Расширенный hello совместим в обе стороны — это условие важнее самой правки:
// хост живёт дольше агента (переживает автообновление), поэтому новый агент
// регулярно встречает СТАРЫЙ хост. Ошибка здесь — не лишний трафик, а
// неоткрывающийся терминал (грабля 2.49.11).
func TestHelloPayloadStaysBackwardCompatible(t *testing.T) {
	// Без позиции — ровно прежние 6 байт, старый хост читает их как раньше.
	short := helloPayload(120, 30, 0)
	if len(short) != 6 {
		t.Fatalf("hello без позиции должен быть 6 байт, а он %d", len(short))
	}

	// С позицией — 14 байт, но ПЕРВЫЕ ШЕСТЬ те же самые: старый хост читает
	// payload[0:6] и остаток игнорирует (см. serveClient), поэтому размер окна
	// он применит, а про позицию просто не узнает.
	long := helloPayload(120, 30, 123456)
	if len(long) != 14 {
		t.Fatalf("hello с позицией должен быть 14 байт, а он %d", len(long))
	}
	for i := 0; i < 6; i++ {
		if long[i] != short[i] {
			t.Fatalf("первые 6 байт разошлись в позиции %d: %d против %d", i, long[i], short[i])
		}
	}
	if got := binary.LittleEndian.Uint16(long[0:]); got != ProtocolVersion {
		t.Fatalf("версия протокола в hello %d вместо %d", got, ProtocolVersion)
	}
	if got := binary.LittleEndian.Uint16(long[2:]); got != 120 {
		t.Fatalf("cols в hello %d вместо 120", got)
	}
	if got := binary.LittleEndian.Uint16(long[4:]); got != 30 {
		t.Fatalf("rows в hello %d вместо 30", got)
	}
	if got := binary.LittleEndian.Uint64(long[6:]); got != 123456 {
		t.Fatalf("позиция в hello %d вместо 123456", got)
	}

	// И обратная сторона: НОВЫЙ хост, получив короткий hello от старого агента,
	// обязан считать позицию нулём — то есть отдать всё, как раньше.
	var known uint64
	if len(short) >= 14 {
		known = binary.LittleEndian.Uint64(short[6:])
	}
	if known != 0 {
		t.Fatalf("короткий hello дал позицию %d вместо нуля", known)
	}

	// Следующее append-only расширение после epoch не должно
	// делать уже известный epoch невидимым.
	withEpoch := helloPayload(120, 30, 123456, "stream-1")
	withFutureTail := append(append([]byte(nil), withEpoch...), 0xaa, 0xbb)
	if got := clientHelloKnownEpoch(withFutureTail); got != "stream-1" {
		t.Fatalf("append-only tail сломал epoch: %q", got)
	}
}

// Host живёт дольше Remotai и produced уже далеко за пределами
// кольца. Новый Session должен начать с replay_start, тогда после
// snapshot второй reconnect шлёт absolute produced и не получает
// кольцо повторно.
func TestHostAbsoluteOffsetSurvivesNewSessionAndSecondReconnect(t *testing.T) {
	buf := make([]byte, scrollbackSize)
	produced := uint64(scrollbackSize) + 777_777
	first := snapshotWindow(buf, produced, 0)
	if first.Start != produced-uint64(len(buf)) {
		t.Fatalf("replay_start=%d, want %d", first.Start, produced-uint64(len(buf)))
	}

	conn := &streamStateTestConn{state: hostStreamState{
		Epoch: "host-stream-1", ReplayStart: first.Start, Produced: produced,
	}}
	s := newSession("id", "cwd", "shell", 1, conn, time.Now())
	if s.epoch != conn.state.Epoch || s.totalBytes != first.Start {
		t.Fatalf("new Session не принял absolute baseline: epoch=%q total=%d", s.epoch, s.totalBytes)
	}

	s.bufMu.Lock()
	s.buf = append(s.buf, first.Data...)
	s.totalBytes += uint64(len(first.Data))
	known := s.totalBytes
	s.bufMu.Unlock()
	if known != produced {
		t.Fatalf("после snapshot Session на позиции %d, host на %d", known, produced)
	}

	second := snapshotWindow(buf, produced, known)
	if len(second.Data) != 0 || second.Start != produced {
		t.Fatalf("второй reconnect повторил %d байт, start=%d", len(second.Data), second.Start)
	}
}

// On the first persistent-host attach, Hello modes are seeded before readLoop
// creates the mirror. The replay tail may contain only cell diffs and omit the
// old ?1049h that entered alt-screen. A blank authoritative main-screen mirror
// must not then "disprove" and prune the host's still-active modes.
func TestNewSessionScreenStartsWithHostModes(t *testing.T) {
	conn := &streamStateTestConn{
		state: hostStreamState{Epoch: "host-stream", ReplayStart: 8000, Produced: 8100},
		modes: []int{1049, 1006, 2004},
	}
	s := newSession("initial-modes", "cwd", "shell", 1, conn, time.Now())
	s.startScreen(60, 12)
	defer s.stopScreen()

	// Stateful replay tail: it updates a cell but does not re-enter alt-screen.
	tail := []byte("\x1b[3;7HREPLAY-DIFF")
	s.feedScreenAt(tail, conn.state.ReplayStart+uint64(len(tail)))
	waitScreenIdle(t, s)

	seq := s.ModeReassertSeq()
	for _, want := range []string{"\x1b[?1049h", "\x1b[?1006h", "\x1b[?2004h"} {
		if !bytes.Contains([]byte(seq), []byte(want)) {
			t.Fatalf("initial host mode %q was pruned after stateful replay: %q", want, seq)
		}
	}
}

func TestReattachModesTreatsEmptyCurrentHostAsAuthoritative(t *testing.T) {
	staleMeta := []int{1049, 1006}
	current := &streamStateTestConn{
		state: hostStreamState{Epoch: "current-host", ReplayStart: 8000, Produced: 8100},
		modes: []int{}, // host observed ?1049l while the agent was offline
	}
	if got := reattachModes(current, staleMeta); len(got) != 0 {
		t.Fatalf("empty current-host modes fell back to stale metadata: %v", got)
	}

	// Old hosts have no absolute stream contract, so an empty mode list is
	// ambiguous and must keep the backward-compatible metadata fallback.
	legacy := &streamStateTestConn{}
	if got := reattachModes(legacy, staleMeta); len(got) != len(staleMeta) || got[0] != staleMeta[0] || got[1] != staleMeta[1] {
		t.Fatalf("legacy host lost metadata fallback: got %v want %v", got, staleMeta)
	}
}

// known валиден только в шкале того же host epoch.
func TestHostRejectsKnownFromAnotherStreamEpoch(t *testing.T) {
	buf := make([]byte, 1000)
	produced := uint64(5000)
	window := snapshotWindowForStream(buf, produced, 4900, "old-stream", "new-stream")
	if len(window.Data) != len(buf) || window.Start != produced-uint64(len(buf)) {
		t.Fatalf("foreign epoch применил known: len=%d start=%d", len(window.Data), window.Start)
	}
}

func TestLegacyHostSecondReconnectUsesCleanFullReplay(t *testing.T) {
	legacy := &streamStateTestConn{}
	s := newSession("legacy", "cwd", "shell", 1, legacy, time.Now())
	s.startScreen(60, 12)
	defer s.stopScreen()
	s.feedScreenAt([]byte("OLD-MIRROR\r\n"), 10)
	waitScreenIdle(t, s)
	s.bufMu.Lock()
	s.buf = append(s.buf, []byte("old-ring")...)
	s.totalBytes = 4_000_000
	oldEpoch := s.epoch
	s.dec.seed([]int{1049, 1006})
	s.altScreen.Store(true)
	s.bufMu.Unlock()

	known, epoch, absolute := hostResumeCoordinates(s)
	if absolute || known != 0 || epoch != "" {
		t.Fatalf("legacy host получил relative known: %d %q absolute=%v", known, epoch, absolute)
	}
	s.resetStreamContinuity("", 0)
	s.bufMu.Lock()
	newEpoch, total, buffered := s.epoch, s.totalBytes, len(s.buf)
	modes := s.dec.snapshot()
	s.bufMu.Unlock()
	if newEpoch == oldEpoch || total != 0 || buffered != 0 || len(modes) != 0 || s.altScreen.Load() {
		t.Fatalf("legacy reset не очистил stream: epoch=%q total=%d buf=%d modes=%v alt=%v", newEpoch, total, buffered, modes, s.altScreen.Load())
	}
	s.feedScreenAt([]byte("NEW-REPLAY\r\n"), 12)
	waitScreenIdle(t, s)
	if frame, _, _, _, _, _ := s.ScreenFrameAt(); frame != "" {
		t.Fatalf("legacy host without ordered resize ACK exposed a screen frame: %q", frame)
	}
	sc, _ := s.screen.Load().(*sessionScreen)
	sc.mu.Lock()
	mirror := sc.mirror
	sc.mu.Unlock()
	frame, _, _, _, _, _ := mirror.SnapshotAt(screenMirrorScrollback)
	if !bytes.Contains([]byte(frame), []byte("NEW-REPLAY")) || bytes.Contains([]byte(frame), []byte("OLD-MIRROR")) {
		t.Fatalf("legacy replay склеился с old mirror: %q", frame)
	}
}

// Смена host epoch (или прыжок replay_start вперёд) обязана сбросить не только
// offset/ring, но и VT mirror с DEC-трекером. Иначе snapshot нового потока
// исполняется поверх старого экрана и возвращает тот же визуальный дубль.
func TestAdoptHostStreamResetClearsMirrorAndDEC(t *testing.T) {
	old := &streamStateTestConn{state: hostStreamState{Epoch: "old-host", ReplayStart: 10, Produced: 10}}
	s := newSession("reset", "cwd", "shell", 1, old, time.Now())
	s.startScreen(60, 12)
	defer s.stopScreen()
	s.feedScreenAt([]byte("OLD-MIRROR\r\n"), 20)
	waitScreenIdle(t, s)
	s.bufMu.Lock()
	s.buf = append(s.buf, []byte("old-ring")...)
	s.totalBytes = 20
	s.dec.seed([]int{1049, 1006})
	s.altScreen.Store(true)
	s.bufMu.Unlock()

	next := &streamStateTestConn{state: hostStreamState{Epoch: "new-host", ReplayStart: 9000, Produced: 9100}}
	if !s.adoptHostStream(next) {
		t.Fatal("чужой host epoch не вызвал stream reset")
	}
	s.bufMu.Lock()
	epoch, total, buffered := s.epoch, s.totalBytes, len(s.buf)
	modes := s.dec.snapshot()
	s.bufMu.Unlock()
	if epoch != "new-host" || total != 9000 || buffered != 0 || len(modes) != 0 || s.altScreen.Load() {
		t.Fatalf("stream reset оставил старое: epoch=%q total=%d buf=%d modes=%v alt=%v", epoch, total, buffered, modes, s.altScreen.Load())
	}
	s.feedScreenAt([]byte("NEW-SNAPSHOT\r\n"), 9014)
	frame, _, _, _, _, _ := s.ScreenFrameAt()
	if !bytes.Contains([]byte(frame), []byte("NEW-SNAPSHOT")) || bytes.Contains([]byte(frame), []byte("OLD-MIRROR")) {
		t.Fatalf("snapshot нового epoch склеился с old mirror: %q", frame)
	}
}

func TestSetConnResetUsesIncomingResizeCapability(t *testing.T) {
	t.Run("ordered to unordered stays raw-only", func(t *testing.T) {
		old := &streamStateTestConn{state: hostStreamState{Epoch: "ordered-old", ReplayStart: 0, Produced: 0}}
		s := newSession("cap-to-raw", "cwd", "shell", 1, old, time.Now())
		s.startScreen(40, 8)
		defer s.stopScreen()

		next := &streamStateTestConn{
			state:     hostStreamState{Epoch: "unordered-new", ReplayStart: 100, Produced: 100},
			unordered: true,
		}
		s.setConn(next)
		raw := []byte("\x1bcNEW-RAW-ONLY")
		s.feedScreenAt(raw, 100+uint64(len(raw)))
		waitScreenIdle(t, s)
		if frame, history, _, _, _, _ := s.ScreenFrameAt(); frame != "" || history != "" {
			t.Fatalf("incoming unordered capability exposed frame=%q history=%q", frame, history)
		}
		if !s.ScreenStale() {
			t.Fatal("incoming unordered capability did not disable screen delivery")
		}
	})

	t.Run("unordered to ordered enables fresh mirror", func(t *testing.T) {
		old := &streamStateTestConn{
			state:     hostStreamState{Epoch: "unordered-old", ReplayStart: 0, Produced: 0},
			unordered: true,
		}
		s := newSession("cap-to-frame", "cwd", "shell", 1, old, time.Now())
		s.startScreen(40, 8)
		defer s.stopScreen()

		next := &streamStateTestConn{state: hostStreamState{Epoch: "ordered-new", ReplayStart: 200, Produced: 200}}
		s.setConn(next)
		raw := []byte("\x1bcNEW-AUTHORITATIVE")
		s.feedScreenAt(raw, 200+uint64(len(raw)))
		waitScreenIdle(t, s)
		if frame, _, _, _, _, _ := s.ScreenFrameAt(); !bytes.Contains([]byte(frame), []byte("NEW-AUTHORITATIVE")) {
			t.Fatalf("incoming ordered capability left fresh mirror disabled: %q", frame)
		}
		if s.ScreenStale() {
			t.Fatal("incoming ordered capability remained raw-only")
		}
	})
}

// Тот же host может вытеснить known offset из своего ring и вернуть
// replay_start вперёд. Host epoch остаётся тем же для следующего known, но
// клиентская continuity обязана смениться, чтобы живой xterm получил reset.
func TestSameHostReplayGapRotatesClientEpochButKeepsHostEpoch(t *testing.T) {
	conn := &streamStateTestConn{state: hostStreamState{Epoch: "stable-host", ReplayStart: 100, Produced: 200}}
	s := newSession("gap", "cwd", "shell", 1, conn, time.Now())
	s.bufMu.Lock()
	s.totalBytes = 150
	oldClientEpoch := s.epoch
	s.bufMu.Unlock()

	conn.state = hostStreamState{Epoch: "stable-host", ReplayStart: 9000, Produced: 9100}
	if !s.adoptHostStream(conn) {
		t.Fatal("replay_start прыгнул, но continuity reset не сработал")
	}
	s.bufMu.Lock()
	clientEpoch, hostEpoch, total := s.epoch, s.hostEpoch, s.totalBytes
	s.bufMu.Unlock()
	if clientEpoch == oldClientEpoch || hostEpoch != "stable-host" || total != 9000 {
		t.Fatalf("gap reset: client=%q old=%q host=%q total=%d", clientEpoch, oldClientEpoch, hostEpoch, total)
	}
	known, knownEpoch, absolute := hostResumeCoordinates(s)
	if !absolute || known != 9000 || knownEpoch != "stable-host" {
		t.Fatalf("host resume потерял absolute epoch: known=%d epoch=%q absolute=%v", known, knownEpoch, absolute)
	}
}

// reset не может отпустить bufMu раньше screen swap. Иначе readLoop успеет
// положить первые байты нового replay в старый mirror, а последующий blank swap
// потеряет их навсегда. Hook делает именно этот шов детерминированным.
func TestStreamResetCannotLoseFirstReplayBytesAtScreenSwap(t *testing.T) {
	s := &Session{epoch: "old", hostEpoch: "old", subs: map[chan []byte]*subState{}, dec: newDecTracker()}
	s.startScreen(60, 12)
	defer s.stopScreen()

	atSeam := make(chan struct{})
	releaseReset := make(chan struct{})
	resetDone := make(chan struct{})
	go func() {
		s.resetStreamContinuityForHostHook("new", "new", 1000, func() {
			close(atSeam)
			<-releaseReset
		})
		close(resetDone)
	}()
	<-atSeam

	writerStarted := make(chan struct{})
	writerAcquired := make(chan struct{})
	writerDone := make(chan struct{})
	go func() {
		close(writerStarted)
		s.bufMu.Lock()
		close(writerAcquired)
		data := []byte("FIRST-NEW-REPLAY\r\n")
		s.buf = append(s.buf, data...)
		s.totalBytes += uint64(len(data))
		off := s.totalBytes
		s.bufMu.Unlock()
		s.feedScreenAt(data, off)
		close(writerDone)
	}()
	<-writerStarted
	select {
	case <-writerAcquired:
		t.Fatal("writer acquired bufMu before stream-reset screen swap")
	case <-time.After(20 * time.Millisecond):
	}

	close(releaseReset)
	<-resetDone
	<-writerDone
	frame, _, _, _, _, _ := s.ScreenFrameAt()
	if !bytes.Contains([]byte(frame), []byte("FIRST-NEW-REPLAY")) {
		t.Fatalf("первые байты replay потеряны на screen swap: %q", frame)
	}
}

func TestHostResizeAckOnlyAfterSuccessfulApply(t *testing.T) {
	payload := resizePayload(120, 33, 77, true)
	var gotCols, gotRows int
	ack, err := applyHostResize(payload, func(cols, rows int) error {
		gotCols, gotRows = cols, rows
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotCols != 120 || gotRows != 33 || binary.LittleEndian.Uint32(ack) != 77 {
		t.Fatalf("resize/ack разошлись: size=%dx%d ack=%v", gotCols, gotRows, ack)
	}

	wantErr := errors.New("resize rejected")
	ack, err = applyHostResize(payload, func(int, int) error { return wantErr })
	if !errors.Is(err, wantErr) || ack != nil {
		t.Fatalf("ошибочный resize подтверждён: ack=%v err=%v", ack, err)
	}
}

func TestHostResizeErrorQueuesNackWithoutAck(t *testing.T) {
	h := &host{}
	out := make(chan hostOutbound, 1)
	wantErr := errors.New("resize rejected")
	payload := resizePayload(120, 33, 78, true)
	err := h.queueHostResizeWith(out, make(chan struct{}), payload, func(int, int) error { return wantErr })
	if !errors.Is(err, wantErr) {
		t.Fatalf("resize error lost: %v", err)
	}
	response := <-out
	if response.typ != frResizeNack || binary.LittleEndian.Uint32(response.payload) != 78 {
		t.Fatalf("rejected resize response=%+v, want matching NACK", response)
	}
}

// Host ACK и live output обязаны ехать через одну FIFO. Пока Resize держит
// bufMu, post-resize output не может встать перед ACK; output, уже стоявший в
// очереди, остаётся перед ним.
func TestHostResizeAckIsOrderedBetweenPreAndPostResizeOutput(t *testing.T) {
	h := &host{}
	out := make(chan hostOutbound, 3)
	writerDone := make(chan struct{})
	out <- hostOutbound{typ: frOutput, payload: []byte("pre")}
	resizeEntered := make(chan struct{})
	releaseResize := make(chan struct{})
	resizeDone := make(chan error, 1)
	payload := resizePayload(120, 33, 91, true)
	go func() {
		resizeDone <- h.queueHostResizeWith(out, writerDone, payload, func(int, int) error {
			close(resizeEntered)
			<-releaseResize
			return nil
		})
	}()
	<-resizeEntered

	postQueued := make(chan struct{})
	go func() {
		h.bufMu.Lock()
		out <- hostOutbound{typ: frOutput, payload: []byte("post")}
		h.bufMu.Unlock()
		close(postQueued)
	}()
	select {
	case <-postQueued:
		t.Fatal("post-resize output entered FIFO before resize ACK")
	case <-time.After(20 * time.Millisecond):
	}
	close(releaseResize)
	if err := <-resizeDone; err != nil {
		t.Fatal(err)
	}
	<-postQueued

	pre, ack, post := <-out, <-out, <-out
	if pre.typ != frOutput || string(pre.payload) != "pre" || ack.typ != frResizeAck || binary.LittleEndian.Uint32(ack.payload) != 91 || post.typ != frOutput || string(post.payload) != "post" {
		t.Fatalf("outbound order: pre=%+v ack=%+v post=%+v", pre, ack, post)
	}
}

// Даже при ПОЛНОЙ outbound-очереди pre-resize frame сохраняет место перед
// ACK. Раньше slow send отпускал bufMu, и два заблокированных sender'а могли
// занять освободившийся слот в обратном порядке.
func TestHostResizeAckCannotOvertakeSlowPreResizeOutput(t *testing.T) {
	out := make(chan hostOutbound, 1)
	sub := &hostSubscription{out: out, broken: make(chan struct{})}
	h := &host{dec: newDecTracker(), sub: sub, dead: make(chan struct{})}
	out <- hostOutbound{typ: frOutput, payload: []byte("older")}
	atSlowSend := make(chan struct{})
	preDone := make(chan struct{})
	go func() {
		h.publishOutputWithHook([]byte("pre"), func() { close(atSlowSend) })
		close(preDone)
	}()
	<-atSlowSend // publishOutput держит bufMu и ждёт свободный slot

	resizeApplied := make(chan struct{})
	resizeDone := make(chan error, 1)
	payload := resizePayload(120, 33, 92, true)
	go func() {
		resizeDone <- h.queueHostResizeWith(out, make(chan struct{}), payload, func(int, int) error {
			close(resizeApplied)
			return nil
		})
	}()
	select {
	case <-resizeApplied:
		t.Fatal("resize crossed a blocked pre-resize output")
	case <-time.After(20 * time.Millisecond):
	}

	older := <-out // frees the only slot; `pre` must claim it while holding bufMu
	<-preDone
	pre := <-out // now resize may acquire bufMu, apply, and enqueue ACK
	if err := <-resizeDone; err != nil {
		t.Fatal(err)
	}
	ack := <-out
	if string(older.payload) != "older" || string(pre.payload) != "pre" || ack.typ != frResizeAck || binary.LittleEndian.Uint32(ack.payload) != 92 {
		t.Fatalf("full-channel order: older=%+v pre=%+v ack=%+v", older, pre, ack)
	}
}

type readableBeforeResizePTY struct {
	pending []byte
	cols    int
	rows    int
}

func (p *readableBeforeResizePTY) Read([]byte) (int, error) { return 0, io.EOF }
func (p *readableBeforeResizePTY) readAvailable(dst []byte) (int, error) {
	if len(p.pending) == 0 {
		return 0, nil
	}
	n := copy(dst, p.pending)
	p.pending = p.pending[n:]
	return n, nil
}
func (p *readableBeforeResizePTY) Write(data []byte) (int, error) { return len(data), nil }
func (p *readableBeforeResizePTY) Resize(cols, rows int) error {
	p.cols, p.rows = cols, rows
	return nil
}
func (p *readableBeforeResizePTY) Close() error                { return nil }
func (p *readableBeforeResizePTY) shellPID() uint32            { return 0 }
func (p *readableBeforeResizePTY) currentCWD() (string, error) { return "", nil }
func (p *readableBeforeResizePTY) exitCode() int               { return 0 }

// Bytes that the PTY has already made readable belong to the old geometry.
// A blocking Read used to return them outside bufMu, allowing Resize+ACK to
// pass while the reader goroutine was descheduled before publishOutput.
func TestHostResizeDrainsAlreadyReadableOutputBeforeAck(t *testing.T) {
	pty := &readableBeforeResizePTY{pending: []byte("pre-resize")}
	out := make(chan hostOutbound, 2)
	sub := &hostSubscription{out: out, broken: make(chan struct{})}
	h := &host{pty: pty, dec: newDecTracker(), sub: sub, dead: make(chan struct{})}
	payload := resizePayload(120, 33, 93, true)

	if err := h.queueHostResizeWith(out, make(chan struct{}), payload, pty.Resize); err != nil {
		t.Fatal(err)
	}
	pre := <-out
	if pre.typ != frOutput || string(pre.payload) != "pre-resize" {
		t.Fatalf("resize ACK overtook already-readable PTY output: first=%+v", pre)
	}
	ack := <-out
	if ack.typ != frResizeAck || binary.LittleEndian.Uint32(ack.payload) != 93 {
		t.Fatalf("second frame is not matching ACK: %+v", ack)
	}
}

// Stream reset and host Hello modes are one bufMu transaction. markLagged is
// the publication point: a WS writer awakened there must not emit a reset
// marker before authoritative alt/mouse modes have been seeded.
func TestStreamResetSeedsHostModesBeforeSubscriberWake(t *testing.T) {
	old := &streamStateTestConn{state: hostStreamState{Epoch: "old", ReplayStart: 0, Produced: 10}}
	s := newSession("mode-wake", "cwd", "shell", 1, old, time.Now())
	s.startScreen(60, 12)
	defer s.stopScreen()
	st := &subState{wake: make(chan struct{}, 1)}
	s.subs[make(chan []byte)] = st
	observed := make(chan string, 1)
	go func() {
		<-st.wake
		observed <- s.ModeReassertSeq()
	}()

	next := &streamStateTestConn{
		state: hostStreamState{Epoch: "new", ReplayStart: 9000, Produced: 9100},
		modes: []int{1049, 1006, 2004},
	}
	s.setConn(next)
	select {
	case seq := <-observed:
		for _, want := range []string{"\x1b[?1049h", "\x1b[?1006h", "\x1b[?2004h"} {
			if !bytes.Contains([]byte(seq), []byte(want)) {
				t.Fatalf("subscriber woke before host modes were seeded: %q", seq)
			}
		}
	case <-time.After(time.Second):
		t.Fatal("stream reset did not wake subscriber")
	}
}

// A bounded send timeout cannot silently keep the same connection alive:
// produced already advanced, while the client did not receive that range.
// Breaking the subscription makes reconnect replay it from the host ring.
func TestHostSlowOutputBreaksContinuityInsteadOfSilentDrop(t *testing.T) {
	out := make(chan hostOutbound, 1)
	out <- hostOutbound{typ: frOutput, payload: []byte("older")}
	sub := &hostSubscription{out: out, broken: make(chan struct{})}
	h := &host{dec: newDecTracker(), sub: sub, dead: make(chan struct{})}

	h.publishOutputWithHookWait([]byte("undelivered"), nil, 20*time.Millisecond)
	select {
	case <-sub.broken:
	default:
		t.Fatal("full outbound timeout kept broken continuity alive")
	}
	h.subMu.Lock()
	current := h.sub
	h.subMu.Unlock()
	if current != nil {
		t.Fatal("timed-out subscription remained current")
	}
	window := snapshotWindow(h.buf, h.produced, 0)
	if string(window.Data) != "undelivered" || window.Start != 0 {
		t.Fatalf("undelivered bytes are not replayable: %+v", window)
	}
}

func TestHostExitDrainsQueuedOutputBeforeExit(t *testing.T) {
	out := make(chan hostOutbound, 3)
	out <- hostOutbound{typ: frOutput, payload: []byte("final-1")}
	out <- hostOutbound{typ: frResizeAck, payload: []byte{7, 0, 0, 0}}
	out <- hostOutbound{typ: frOutput, payload: []byte("final-2")}
	dead := make(chan struct{})
	close(dead) // producer is finished; the queued lane is now a fixed FIFO
	<-dead

	var got []hostOutbound
	err := writeHostExitAfterDrain(out, 23, func(typ frameType, payload []byte) error {
		got = append(got, hostOutbound{typ: typ, payload: append([]byte(nil), payload...)})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 || got[0].typ != frOutput || string(got[0].payload) != "final-1" ||
		got[1].typ != frResizeAck || got[2].typ != frOutput || string(got[2].payload) != "final-2" || got[3].typ != frExit {
		t.Fatalf("host exit overtook queued FIFO: %+v", got)
	}
	if code := int(binary.LittleEndian.Uint32(got[3].payload)); code != 23 {
		t.Fatalf("exit code=%d, want 23", code)
	}
}
