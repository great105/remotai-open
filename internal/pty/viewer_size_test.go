package pty

import (
	"errors"
	"io"
	"sync"
	"testing"
	"time"
)

// sizeSpy — backend, который только запоминает применённые размеры.
type sizeSpy struct {
	mu    sync.Mutex
	sizes [][2]int
}

func (c *sizeSpy) Read(p []byte) (int, error)  { select {} }
func (c *sizeSpy) Write(p []byte) (int, error) { return len(p), nil }
func (c *sizeSpy) Close() error                { return nil }
func (c *sizeSpy) Resize(cols, rows int) error {
	c.mu.Lock()
	c.sizes = append(c.sizes, [2]int{cols, rows})
	c.mu.Unlock()
	return nil
}
func (c *sizeSpy) ResizeOrdered(cols, rows int, afterApply func()) error {
	if err := c.Resize(cols, rows); err != nil {
		return err
	}
	if afterApply != nil {
		afterApply()
	}
	return nil
}
func (c *sizeSpy) shellPID() uint32            { return 0 }
func (c *sizeSpy) currentCWD() (string, error) { return "", nil }
func (c *sizeSpy) applied() [][2]int {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([][2]int, len(c.sizes))
	copy(out, c.sizes)
	return out
}

func newSizeSession(t *testing.T, bornCols, bornRows int) (*Session, *sizeSpy) {
	t.Helper()
	spy := &sizeSpy{}
	s := newSession("size", "C:\\tmp", "cmd", 1, spy, time.Now())
	s.setBornSize(bornCols, bornRows)
	return s, spy
}

// Телефон уходит и возвращается — размер PTY НЕ меняется вовсе.
//
// Дефект, ради которого это написано (жалоба 04.08.2026 «вывод зациклился»).
// Размер общий и берётся по самому узкому зрителю, поэтому он прыгал: телефон
// подключился — 48 колонок, ушёл — обратно 138, вернулся через секунду — снова
// 48. Каждая смена размера заставляет полноэкранный TUI перерисовать себя
// ЦЕЛИКОМ, а у Kimi кадр был 418 строк при терминале в 33: в буфере накопилось
// 143 копии её приветствия при одном и том же session_id.
func TestPhoneReconnectDoesNotResizePty(t *testing.T) {
	s, spy := newSizeSession(t, 138, 29)

	pc := s.AddViewer()
	if err := s.ResizeFor(pc, 138, 29); err != nil {
		t.Fatal(err)
	}
	phone := s.AddViewer()
	if err := s.ResizeFor(phone, 48, 33); err != nil {
		t.Fatal(err)
	}
	// Уменьшение применяется сразу: тот, кто смотрит сейчас, иначе видит рваные строки.
	if got := spy.applied(); len(got) != 2 || got[1] != [2]int{48, 29} {
		t.Fatalf("после прихода телефона применено %v, ожидалось …48x29", got)
	}
	before := len(spy.applied())

	// Телефон уходит и возвращается быстрее, чем истечёт пауза роста.
	for i := 0; i < 5; i++ {
		s.RemoveViewer(phone)
		phone = s.AddViewer()
		if err := s.ResizeFor(phone, 48, 33); err != nil {
			t.Fatal(err)
		}
	}
	if got := spy.applied(); len(got) != before {
		t.Fatalf("пять переподключений телефона дали %d новых размеров, ожидалось 0: %v", len(got)-before, got[before:])
	}
}

// Телефон ушёл НАСОВСЕМ — размер возвращается к оставшемуся зрителю, но не
// мгновенно: рост ждёт паузы, чтобы вернувшийся через секунду телефон его отменил.
func TestSizeGrowsBackAfterViewerReallyLeft(t *testing.T) {
	s, spy := newSizeSession(t, 138, 29)
	pc := s.AddViewer()
	_ = s.ResizeFor(pc, 138, 29)
	phone := s.AddViewer()
	_ = s.ResizeFor(phone, 48, 33)
	before := len(spy.applied())

	s.RemoveViewer(phone)
	if got := spy.applied(); len(got) != before {
		t.Fatal("рост применён мгновенно — переподключение телефона снова начнёт дёргать размер")
	}
	// Ждём паузу роста.
	deadline := time.Now().Add(viewerGrowQuiet + 3*time.Second)
	for time.Now().Before(deadline) && len(spy.applied()) == before {
		time.Sleep(100 * time.Millisecond)
	}
	got := spy.applied()
	if len(got) == before {
		t.Fatal("размер так и не вырос обратно — окно на ПК осталось в телефонных 48 колонках")
	}
	if last := got[len(got)-1]; last != [2]int{138, 29} {
		t.Fatalf("вернулись к %v вместо 138x29", last)
	}
}

// Смотреть перестали совсем — возвращаемся к размеру, с которым терминал родился.
func TestSizeReturnsToBornWhenNobodyWatches(t *testing.T) {
	s, spy := newSizeSession(t, 120, 30)
	phone := s.AddViewer()
	_ = s.ResizeFor(phone, 48, 33)
	before := len(spy.applied())

	s.RemoveViewer(phone)
	deadline := time.Now().Add(viewerGrowQuiet + 3*time.Second)
	for time.Now().Before(deadline) && len(spy.applied()) == before {
		time.Sleep(100 * time.Millisecond)
	}
	got := spy.applied()
	if len(got) == before {
		t.Fatal("без зрителей размер остался телефонным — следующий, кто откроет терминал, получит 48 колонок")
	}
	if last := got[len(got)-1]; last != [2]int{120, 30} {
		t.Fatalf("вернулись к %v вместо родного 120x30", last)
	}
}

// Боевой случай 04.08.2026: born ШИРЕ, но НИЖЕ телефонного (80x24 против 48x31).
// Прежнее условие отсрочки требовало роста по обеим осям, по строкам это было
// уменьшение — и каждый уход телефона бил полной перерисовкой переписки агента
// немедленно. Телефон уходит и возвращается постоянно, поэтому цена измеряется
// не одной перерисовкой, а сотней копий в буфере.
func TestMixedGrowOnViewerLeaveIsDeferred(t *testing.T) {
	s, spy := newSizeSession(t, 80, 24)
	phone := s.AddViewer()
	_ = s.ResizeFor(phone, 48, 31)
	before := len(spy.applied())

	s.RemoveViewer(phone)
	if got := spy.applied(); len(got) != before {
		t.Fatalf("возврат к born применён мгновенно: %v — каждый уход телефона перерисовывает агенту всю переписку", got[before:])
	}

	// Вернулся через секунду — отложенный возврат отменяется, PTY не дёргается.
	back := s.AddViewer()
	_ = s.ResizeFor(back, 48, 31)
	time.Sleep(viewerGrowQuiet + time.Second)
	if got := spy.applied(); len(got) != before {
		t.Fatalf("вернувшийся телефон не отменил возврат: %v", got[before:])
	}

	// А если ушёл насовсем — размер всё же возвращается к родному.
	s.RemoveViewer(back)
	deadline := time.Now().Add(viewerGrowQuiet + 3*time.Second)
	for time.Now().Before(deadline) && len(spy.applied()) == before {
		time.Sleep(100 * time.Millisecond)
	}
	got := spy.applied()
	if len(got) == before {
		t.Fatal("после настоящего ухода размер так и не вернулся к born")
	}
	if last := got[len(got)-1]; last != [2]int{80, 24} {
		t.Fatalf("вернулись к %v вместо родного 80x24", last)
	}
}

// Исходный размер неизвестен (терминал поднят старой записью без него) —
// поведение остаётся прежним: без зрителей ничего не трогаем.
func TestUnknownBornSizeChangesNothing(t *testing.T) {
	s, spy := newSizeSession(t, 0, 0)
	phone := s.AddViewer()
	_ = s.ResizeFor(phone, 48, 33)
	before := len(spy.applied())

	s.RemoveViewer(phone)
	time.Sleep(viewerGrowQuiet + time.Second)
	if got := spy.applied(); len(got) != before {
		t.Fatalf("без известного исходного размера размер всё же поменяли: %v", got[before:])
	}
}

// Вырожденный зритель не вправе ужать общий терминал до нечитаемого.
func TestTinyViewerCannotSqueezeBelowFloor(t *testing.T) {
	s, spy := newSizeSession(t, 120, 30)
	v := s.AddViewer()
	_ = s.ResizeFor(v, 4, 2)
	got := spy.applied()
	if len(got) == 0 {
		t.Fatal("размер не применён вовсе")
	}
	if last := got[len(got)-1]; last[0] < minPtyCols || last[1] < minPtyRows {
		t.Fatalf("применён размер %v — ниже пола %dx%d", last, minPtyCols, minPtyRows)
	}
}

// Зритель ПРИШЁЛ и сообщил размер больше текущего — применяем СРАЗУ.
//
// Живая жалоба владельца 04.08: «иногда вот так открывается в пол-экрана».
// На скриншоте содержимое занимало верхнюю половину, ниже пустота: PTY стоял в
// 33 строки (размер прошлого захода, с клавиатурой), а экран телефона был вдвое
// выше — агент честно рисовал 33 строки в 60-строчное окно. Это была регрессия
// отложенного роста: он откладывался ВСЕГДА, в том числе для того, кто смотрит
// в экран прямо сейчас.
func TestViewerArrivingGetsSizeImmediately(t *testing.T) {
	s, spy := newSizeSession(t, 100, 30)
	phone := s.AddViewer()
	_ = s.ResizeFor(phone, 48, 33) // прошлый заход с клавиатурой
	before := len(spy.applied())

	// Тот же зритель раскрыл клавиатуру обратно — экран стал выше.
	_ = s.ResizeFor(phone, 48, 60)
	got := spy.applied()
	if len(got) == before {
		t.Fatal("рост от ЖИВОГО зрителя отложен — человек до применения видит пол-экрана")
	}
	if last := got[len(got)-1]; last != [2]int{48, 60} {
		t.Fatalf("применён %v вместо 48x60", last)
	}
}

// Полноэкранный TUI без зрителей НЕ трогаем — даже ради born-размера.
//
// Живая жалоба владельца 09.08.2026: «чё пустой терминал?». Сессия агента
// c70c4bb42accd938: телефон отвалился, PTY отскочил 48x30 → 80x24, телефон
// вернулся — 80x24 → 48x30, и Claude Code, уже закончивший работу, выдал
// последним в буфер `ESC[H` и двадцать два `ESC[K` подряд. Экран стёрт и не
// перерисован: у простаивающего TUI на смену размера нечего нарисовать заново.
// В логе за сутки 335 таких отскоков.
func TestAltScreenSessionKeepsSizeWithoutViewers(t *testing.T) {
	s, spy := newSizeSession(t, 80, 24)
	s.seedModes([]int{1049}) // агент рисует в alt-screen
	phone := s.AddViewer()
	_ = s.ResizeFor(phone, 48, 30)
	before := len(spy.applied())

	s.RemoveViewer(phone)
	time.Sleep(viewerGrowQuiet + 2*time.Second)
	if got := spy.applied(); len(got) != before {
		t.Fatalf("осиротевший TUI всё же перерисовали: %v — это и есть чёрный экран у вернувшегося зрителя", got[before:])
	}

	// Телефон вернулся с тем же размером — PTY не дёргается вовсе.
	back := s.AddViewer()
	_ = s.ResizeFor(back, 48, 30)
	if got := spy.applied(); len(got) != before {
		t.Fatalf("возврат зрителя дал новый размер %v, хотя экран того же размера", got[before:])
	}
}

// Агентская сессия БЕЗ alt-screen (Kimi: режимы [1004, 2004]) без зрителей —
// тоже не трогаем. Жалоба владельца 11.08.2026 «у Kimi вывод странный в
// терминале»: Kimi рисует строками по абсолютным адресам и не стирает экран
// целиком, поэтому каждый отскок к born (48x30 → 80x24 → 48x30 на уход и
// возврат телефона) оставлял в прокрутке кадры чужой ширины и высоты —
// статус-полоса «Working…» запечатывалась посреди переписки. Воспроизведение:
// build/qa/probe-kimi-width-mix.mjs против живого агента.
func TestAgentSessionKeepsSizeWithoutViewers(t *testing.T) {
	s, spy := newSizeSession(t, 80, 24)
	s.rememberAgentKind("kimi") // alt-screen у Kimi нет — только вид агента
	phone := s.AddViewer()
	_ = s.ResizeFor(phone, 48, 30)
	before := len(spy.applied())

	s.RemoveViewer(phone)
	time.Sleep(viewerGrowQuiet + 2*time.Second)
	if got := spy.applied(); len(got) != before {
		t.Fatalf("осиротевшую сессию Kimi отскочили к born: %v — вернувшийся телефон получит прокрутку из кадров чужой ширины", got[before:])
	}

	// Телефон вернулся с тем же размером — PTY не дёргается вовсе.
	back := s.AddViewer()
	_ = s.ResizeFor(back, 48, 30)
	if got := spy.applied(); len(got) != before {
		t.Fatalf("возврат зрителя дал новый размер %v, хотя экран того же размера", got[before:])
	}
}

// Обычный шелл — по-прежнему возвращается к born. Историю, напечатанную в 48
// колонок телефона, потом не починить ничем (жалоба 04.08.2026), а стирать
// перерисовкой у шелла нечего.
func TestPlainShellStillReturnsToBornWithoutViewers(t *testing.T) {
	s, spy := newSizeSession(t, 120, 30)
	phone := s.AddViewer()
	_ = s.ResizeFor(phone, 48, 33)
	before := len(spy.applied())

	s.RemoveViewer(phone)
	deadline := time.Now().Add(viewerGrowQuiet + 3*time.Second)
	for time.Now().Before(deadline) && len(spy.applied()) == before {
		time.Sleep(100 * time.Millisecond)
	}
	got := spy.applied()
	if len(got) == before {
		t.Fatal("шелл без зрителей остался телефонным — следующий, кто откроет терминал, получит 48 колонок")
	}
	if last := got[len(got)-1]; last != [2]int{120, 30} {
		t.Fatalf("вернулись к %v вместо родного 120x30", last)
	}
}

// TUI закрылся (`ESC[?1049l`) — сессия снова обычный шелл, и правило born
// возвращается в силу.
func TestSizeReturnsToBornAfterTuiLeavesAltScreen(t *testing.T) {
	s, spy := newSizeSession(t, 100, 28)
	s.seedModes([]int{1049})
	phone := s.AddViewer()
	_ = s.ResizeFor(phone, 48, 30)

	// Приложение вышло из alt-screen тем же путём, что и в бою — байтами вывода.
	s.bufMu.Lock()
	s.dec.scan([]byte("\x1b[?1049l"))
	s.altScreen.Store(s.dec.altActive())
	s.bufMu.Unlock()
	before := len(spy.applied())

	s.RemoveViewer(phone)
	deadline := time.Now().Add(viewerGrowQuiet + 3*time.Second)
	for time.Now().Before(deadline) && len(spy.applied()) == before {
		time.Sleep(100 * time.Millisecond)
	}
	if got := spy.applied(); len(got) == before {
		t.Fatal("после выхода TUI из alt-screen сессия так и осталась телефонной")
	}
}

// Отложенный рост обязан двигать и ЗЕРКАЛО, а не только PTY.
//
// Дефект (внешний аудит 13.08.2026, T-006): resizeToViewers ресайзил зеркало, а
// отложенный applyGrow — нет, хотя это самый частый путь: телефон ушёл, через
// четыре секунды оставшийся зритель вернул размер побольше. PTY становился
// новым, приложение рисовало под него, а зеркало продолжало разбирать тот же
// поток в старой сетке. Дальше ScreenFrame сообщал клиенту СТАРЫЕ
// screen_cols/screen_rows и собирал строки не по своим местам — снаружи это
// «кадр пришёл, а экран разъехался».
func TestDelayedGrowResizesScreenMirrorToo(t *testing.T) {
	s, spy := newSizeSession(t, 138, 29)
	s.startScreen(138, 29)
	defer s.stopScreen()

	pc := s.AddViewer()
	_ = s.ResizeFor(pc, 138, 29)
	phone := s.AddViewer()
	_ = s.ResizeFor(phone, 48, 33) // PTY ужимается до 48x29 сразу
	if _, _, _, cols, rows := s.ScreenFrame(); cols != 48 || rows != 29 {
		t.Fatalf("зеркало %dx%d после немедленного пути, ожидалось 48x29", cols, rows)
	}
	before := len(spy.applied())

	// Телефон ушёл насовсем — рост уходит в отложенный путь.
	s.RemoveViewer(phone)
	deadline := time.Now().Add(viewerGrowQuiet + 3*time.Second)
	for time.Now().Before(deadline) && len(spy.applied()) == before {
		time.Sleep(100 * time.Millisecond)
	}
	got := spy.applied()
	if len(got) == before {
		t.Fatal("отложенный рост не сработал — проверять нечего")
	}
	ptyCols, ptyRows := got[len(got)-1][0], got[len(got)-1][1]
	_, _, _, mCols, mRows := s.ScreenFrame()
	if mCols != ptyCols || mRows != ptyRows {
		t.Fatalf("после отложенного роста PTY %dx%d, а зеркало %dx%d — кадр соберётся в чужой сетке", ptyCols, ptyRows, mCols, mRows)
	}
}

// Тот же инвариант для прямого Session.Resize (REST /api/pty/{id}/resize и
// зритель без учёта): мимо зеркала не должен проходить НИ ОДИН путь.
func TestDirectResizeMovesScreenMirrorToo(t *testing.T) {
	s, _ := newSizeSession(t, 80, 24)
	s.startScreen(80, 24)
	defer s.stopScreen()

	if err := s.Resize(120, 40); err != nil {
		t.Fatal(err)
	}
	if _, _, _, cols, rows := s.ScreenFrame(); cols != 120 || rows != 40 {
		t.Fatalf("после прямого Resize зеркало осталось %dx%d вместо 120x40", cols, rows)
	}
	if cols, rows := s.AppliedSize(); cols != 120 || rows != 40 {
		t.Fatalf("после прямого Resize applied state остался %dx%d вместо 120x40", cols, rows)
	}
}

func TestDirectResizeAfterViewerUpdatesAppliedState(t *testing.T) {
	s, spy := newSizeSession(t, 80, 24)
	s.startScreen(80, 24)
	defer s.stopScreen()
	v := s.AddViewer()
	if err := s.ResizeFor(v, 48, 30); err != nil {
		t.Fatal(err)
	}
	if err := s.Resize(120, 40); err != nil {
		t.Fatal(err)
	}
	got := spy.applied()
	if len(got) != 2 || got[1] != [2]int{120, 40} {
		t.Fatalf("direct resize did not reach backend after viewer: %v", got)
	}
	if cols, rows := s.AppliedSize(); cols != 120 || rows != 40 {
		t.Fatalf("direct resize state=%dx%d, want 120x40", cols, rows)
	}
	if _, _, _, cols, rows := s.ScreenFrame(); cols != 120 || rows != 40 {
		t.Fatalf("direct resize mirror=%dx%d, want 120x40", cols, rows)
	}
}

func TestConcurrentDirectAndViewerResizeShareOneApplyLane(t *testing.T) {
	conn := &invertedResizeConn{
		firstEntered:  make(chan struct{}),
		secondEntered: make(chan struct{}),
		releaseFirst:  make(chan struct{}),
	}
	s := newSession("direct-viewer-order", "cwd", "shell", 1, conn, time.Now())
	s.startScreen(80, 24)
	defer s.stopScreen()
	v := s.AddViewer()

	directDone := make(chan error, 1)
	go func() { directDone <- s.Resize(120, 40) }()
	<-conn.firstEntered
	viewerDone := make(chan error, 1)
	go func() { viewerDone <- s.ResizeFor(v, 40, 20) }()
	select {
	case <-conn.secondEntered:
		t.Fatal("viewer resize entered backend while direct resize still owned apply lane")
	case <-time.After(30 * time.Millisecond):
	}
	close(conn.releaseFirst)
	if err := <-directDone; err != nil {
		t.Fatal(err)
	}
	if err := <-viewerDone; err != nil {
		t.Fatal(err)
	}

	got := conn.applied()
	if len(got) != 2 || got[0] != [2]int{120, 40} || got[1] != [2]int{40, 20} {
		t.Fatalf("direct/viewer apply order=%v", got)
	}
	if cols, rows := s.AppliedSize(); cols != 40 || rows != 20 {
		t.Fatalf("final applied state=%dx%d, want 40x20", cols, rows)
	}
	if _, _, _, cols, rows := s.ScreenFrame(); cols != 40 || rows != 20 {
		t.Fatalf("final mirror=%dx%d, want 40x20", cols, rows)
	}
}

type blockingResizeConn struct {
	sizeSpy
	entered chan struct{}
	release chan struct{}
}

func (c *blockingResizeConn) Resize(cols, rows int) error {
	close(c.entered)
	<-c.release
	return c.sizeSpy.Resize(cols, rows)
}
func (c *blockingResizeConn) ResizeOrdered(cols, rows int, afterApply func()) error {
	if err := c.Resize(cols, rows); err != nil {
		return err
	}
	if afterApply != nil {
		afterApply()
	}
	return nil
}

// Зеркало меняет геометрию только ПОСЛЕ подтверждения backend. Для нового
// host это означает frResizeAck; пока ack не пришёл, future frame в новой
// сетке не может уехать клиенту поверх PTY в старой сетке.
func TestResizeWaitsForBackendBeforeChangingMirror(t *testing.T) {
	conn := &blockingResizeConn{entered: make(chan struct{}), release: make(chan struct{})}
	s := newSession("resize-order", "cwd", "shell", 1, conn, time.Now())
	s.startScreen(80, 24)
	defer s.stopScreen()
	_, _, _, _, _, _, capturedRevision := s.ScreenFrameAtRevision()

	done := make(chan error, 1)
	go func() { done <- s.Resize(120, 40) }()
	<-conn.entered
	if frame, _, _, cols, rows := s.ScreenFrame(); frame != "" || cols != 0 || rows != 0 {
		t.Fatalf("pending resize published a frame: %dx%d %q", cols, rows, frame)
	}
	if current, err := s.WithCurrentScreenFrameRevision(capturedRevision, func() error {
		t.Fatal("captured pre-resize frame write ran while resize was pending")
		return nil
	}); err != nil || current {
		t.Fatalf("captured frame remained current during resize: current=%v err=%v", current, err)
	}
	sc, _ := s.screen.Load().(*sessionScreen)
	sc.mu.Lock()
	mirror := sc.mirror
	sc.mu.Unlock()
	if cols, rows := mirror.Size(); cols != 80 || rows != 24 {
		t.Fatalf("зеркало обогнало backend ack: %dx%d", cols, rows)
	}
	// Decide that capture is empty while holding screenGeometryMu, then let ACK
	// race exactly before the caller receives its wait subscription. The wake
	// returned by the capture must still be the one completion closes.
	atEmptyDecision := make(chan struct{})
	releaseCapture := make(chan struct{})
	s.beforeScreenGeometryWaitReturn = func() {
		close(atEmptyDecision)
		<-releaseCapture
	}
	type captureResult struct {
		frame string
		retry bool
		wake  <-chan struct{}
	}
	captured := make(chan captureResult, 1)
	go func() {
		frame, _, _, _, _, _, _, retry, wake := s.ScreenFrameAtRevisionWait()
		captured <- captureResult{frame: frame, retry: retry, wake: wake}
	}()
	<-atEmptyDecision
	close(conn.release)
	select {
	case err := <-done:
		t.Fatalf("resize completed before atomic capture subscription returned: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(releaseCapture)
	result := <-captured
	s.beforeScreenGeometryWaitReturn = nil
	if result.frame != "" || !result.retry || result.wake == nil {
		t.Fatalf("pending capture result=%+v, want empty+retry+wake", result)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	select {
	case <-result.wake:
	case <-time.After(time.Second):
		t.Fatal("resize completion did not wake withheld screen retry")
	}
	if _, _, _, cols, rows := s.ScreenFrame(); cols != 120 || rows != 40 {
		t.Fatalf("после ack зеркало %dx%d, want 120x40", cols, rows)
	}
}

type invertedResizeConn struct {
	sizeSpy
	muCall        sync.Mutex
	calls         int
	firstEntered  chan struct{}
	secondEntered chan struct{}
	releaseFirst  chan struct{}
}

func (c *invertedResizeConn) Resize(cols, rows int) error {
	c.muCall.Lock()
	c.calls++
	call := c.calls
	c.muCall.Unlock()
	if call == 1 {
		close(c.firstEntered)
		<-c.releaseFirst
	} else if call == 2 {
		close(c.secondEntered)
	}
	return c.sizeSpy.Resize(cols, rows)
}
func (c *invertedResizeConn) ResizeOrdered(cols, rows int, afterApply func()) error {
	if err := c.Resize(cols, rows); err != nil {
		return err
	}
	if afterApply != nil {
		afterApply()
	}
	return nil
}

// Viewer aggregate and the applied PTY/mirror size are one serial commit.
// Previously viewCols/viewRows were updated before backend apply and the lock
// was released, so a slower old resize could finish after a newer small one.
func TestConcurrentViewerResizeCannotCommitInReverseOrder(t *testing.T) {
	conn := &invertedResizeConn{
		firstEntered:  make(chan struct{}),
		secondEntered: make(chan struct{}),
		releaseFirst:  make(chan struct{}),
	}
	s := newSession("viewer-order", "cwd", "shell", 1, conn, time.Now())
	s.startScreen(80, 24)
	defer s.stopScreen()
	v := s.AddViewer()

	firstDone := make(chan error, 1)
	go func() { firstDone <- s.ResizeFor(v, 120, 40) }()
	<-conn.firstEntered
	secondDone := make(chan error, 1)
	go func() { secondDone <- s.ResizeFor(v, 40, 20) }()

	// Broken code lets call #2 enter and finish while #1 is blocked. Fixed code
	// keeps it behind the aggregate apply lane; either way releasing #1 lets the
	// test finish without relying on scheduler timing for correctness.
	select {
	case <-conn.secondEntered:
	case <-time.After(30 * time.Millisecond):
	}
	close(conn.releaseFirst)
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
	if err := <-secondDone; err != nil {
		t.Fatal(err)
	}

	got := conn.applied()
	if len(got) != 2 || got[0] != [2]int{120, 40} || got[1] != [2]int{40, 20} {
		t.Fatalf("viewer resize completion inverted: %v", got)
	}
	if cols, rows := s.AppliedSize(); cols != 40 || rows != 20 {
		t.Fatalf("aggregate says %dx%d, want 40x20", cols, rows)
	}
	if _, _, _, cols, rows := s.ScreenFrame(); cols != 40 || rows != 20 {
		t.Fatalf("mirror ended at stale geometry %dx%d, want 40x20", cols, rows)
	}
}

type lateOrderedResizeConn struct {
	sizeSpy
	muLate sync.Mutex
	calls  int
	first  func()
}

type lateFailedResizeConn struct {
	sizeSpy
	muDone sync.Mutex
	done   func(error)
}

func (c *lateFailedResizeConn) ResizeOrdered(cols, rows int, afterApply func()) error {
	return c.ResizeOrderedComplete(cols, rows, afterApply, nil)
}

func (c *lateFailedResizeConn) ResizeOrderedComplete(_ int, _ int, _ func(), afterDone func(error)) error {
	c.muDone.Lock()
	c.done = afterDone
	c.muDone.Unlock()
	return errResizeAckTimeout
}

func (c *lateFailedResizeConn) fail(err error) {
	c.muDone.Lock()
	done := c.done
	c.done = nil
	c.muDone.Unlock()
	if done != nil {
		done(err)
	}
}

type legacyAcklessResizeConn struct{ sizeSpy }

func (c *legacyAcklessResizeConn) ResizeOrdered(cols, rows int, afterApply func()) error {
	if err := c.Resize(cols, rows); err != nil {
		return err
	}
	if afterApply != nil {
		afterApply()
	}
	return nil
}

func (c *legacyAcklessResizeConn) resizeOrderingSupported() bool { return false }

func TestLegacyAcklessResizeUsesFallbackOutputLane(t *testing.T) {
	conn := &legacyAcklessResizeConn{}
	s := newSession("legacy-resize-order", "cwd", "shell", 1, conn, time.Now())
	callback := make(chan struct{})
	done := make(chan error, 1)

	s.outputApplyMu.Lock()
	locked := true
	defer func() {
		if locked {
			s.outputApplyMu.Unlock()
		}
	}()
	go func() {
		done <- s.applyBackendGeometry(120, 40, func() { close(callback) })
	}()
	select {
	case err := <-done:
		t.Fatalf("legacy host bypassed fallback output lane: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	s.outputApplyMu.Unlock()
	locked = false
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	select {
	case <-callback:
	default:
		t.Fatal("legacy resize completion callback was lost")
	}
}

func (c *lateOrderedResizeConn) ResizeOrdered(cols, rows int, afterApply func()) error {
	c.muLate.Lock()
	c.calls++
	call := c.calls
	if call == 1 {
		c.first = afterApply
	}
	c.muLate.Unlock()
	_ = c.sizeSpy.Resize(cols, rows) // host already applied before its response
	if call == 1 {
		return errResizeAckTimeout
	}
	if afterApply != nil {
		afterApply()
	}
	return nil
}

func (c *lateOrderedResizeConn) fireFirstAck() {
	c.muLate.Lock()
	cb := c.first
	c.first = nil
	c.muLate.Unlock()
	if cb != nil {
		cb()
	}
}

func newLateViewerSession(t *testing.T) (*Session, *Viewer, *lateOrderedResizeConn, *[][2]int, *sync.Mutex) {
	t.Helper()
	conn := &lateOrderedResizeConn{}
	s := newSession("late-view", "cwd", "shell", 1, conn, time.Now())
	s.viewMu.Lock()
	s.viewCols, s.viewRows = 80, 24
	s.viewMu.Unlock()
	s.startScreen(80, 24)
	t.Cleanup(s.stopScreen)
	v := s.AddViewer()
	persisted := &[][2]int{}
	persistMu := &sync.Mutex{}
	s.persistView = func(cols, rows int) {
		persistMu.Lock()
		*persisted = append(*persisted, [2]int{cols, rows})
		persistMu.Unlock()
	}
	return s, v, conn, persisted, persistMu
}

func TestLateViewerResizeAckCommitsAppliedSizeAndPersistence(t *testing.T) {
	s, v, conn, persisted, persistMu := newLateViewerSession(t)
	if err := s.ResizeFor(v, 120, 40); err == nil {
		t.Fatal("scripted soft timeout was not returned")
	}
	if cols, rows := s.AppliedSize(); cols != 80 || rows != 24 {
		t.Fatalf("timeout prematurely committed aggregate %dx%d", cols, rows)
	}
	if frame, _, _, _, _, _, _ := s.ScreenFrameAtRevision(); frame != "" {
		t.Fatalf("screen frame published while resize ACK may still arrive: %q", frame)
	}
	conn.fireFirstAck()
	if cols, rows := s.AppliedSize(); cols != 120 || rows != 40 {
		t.Fatalf("late ACK left aggregate stale at %dx%d", cols, rows)
	}
	if _, _, _, cols, rows := s.ScreenFrame(); cols != 120 || rows != 40 {
		t.Fatalf("late ACK left mirror stale at %dx%d", cols, rows)
	}
	persistMu.Lock()
	got := append([][2]int(nil), (*persisted)...)
	persistMu.Unlock()
	if len(got) != 1 || got[0] != [2]int{120, 40} {
		t.Fatalf("late ACK persistence=%v, want 120x40", got)
	}
}

func TestScreenFrameRevisionRejectsCaptureAfterResize(t *testing.T) {
	conn := &sizeSpy{}
	s := newSession("frame-revision", "cwd", "shell", 1, conn, time.Now())
	s.startScreen(80, 24)
	defer s.stopScreen()
	raw := []byte("BEFORE-RESIZE")
	s.bufMu.Lock()
	s.buf = append(s.buf, raw...)
	s.totalBytes += uint64(len(raw))
	off := s.totalBytes
	s.bufMu.Unlock()
	s.feedScreenAt(raw, off)
	waitScreenIdle(t, s)

	frame, _, _, _, _, _, revision := s.ScreenFrameAtRevision()
	if frame == "" {
		t.Fatal("ordered backend did not capture initial frame")
	}
	if err := s.Resize(100, 30); err != nil {
		t.Fatal(err)
	}
	wrote := false
	current, err := s.WithCurrentScreenFrameRevision(revision, func() error {
		wrote = true
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if current || wrote {
		t.Fatal("pre-resize frame passed geometry revision check")
	}
}

func TestScreenFrameRevisionLeaseOrdersSendBeforeResize(t *testing.T) {
	conn := &sizeSpy{}
	s := newSession("frame-lease", "cwd", "shell", 1, conn, time.Now())
	s.startScreen(80, 24)
	defer s.stopScreen()
	_, _, _, _, _, _, revision := s.ScreenFrameAtRevision()

	entered := make(chan struct{})
	release := make(chan struct{})
	sendDone := make(chan bool, 1)
	go func() {
		current, _ := s.WithCurrentScreenFrameRevision(revision, func() error {
			close(entered)
			<-release
			return nil
		})
		sendDone <- current
	}()
	<-entered
	resizeDone := make(chan error, 1)
	go func() { resizeDone <- s.Resize(100, 30) }()
	time.Sleep(30 * time.Millisecond)
	conn.mu.Lock()
	resizeCalls := len(conn.sizes)
	conn.mu.Unlock()
	if resizeCalls != 0 {
		t.Fatal("resize overtook an in-flight current frame write lease")
	}
	close(release)
	if !<-sendDone {
		t.Fatal("current frame lost its read lease")
	}
	if err := <-resizeDone; err != nil {
		t.Fatal(err)
	}
}

func TestLateResizeFailureReleasesScreenCapture(t *testing.T) {
	conn := &lateFailedResizeConn{}
	s := newSession("late-failed-frame", "cwd", "shell", 1, conn, time.Now())
	s.startScreen(80, 24)
	defer s.stopScreen()
	if err := s.Resize(100, 30); !errors.Is(err, errResizeAckTimeout) {
		t.Fatalf("resize did not return soft timeout: %v", err)
	}
	if frame, _, _, _, _, _, _ := s.ScreenFrameAtRevision(); frame != "" {
		t.Fatalf("frame published before late terminal outcome: %q", frame)
	}
	conn.fail(io.EOF)
	if frame, _, _, cols, rows, _, _ := s.ScreenFrameAtRevision(); frame == "" || cols != 80 || rows != 24 {
		t.Fatalf("late failure did not release old-geometry capture: frame=%q size=%dx%d", frame, cols, rows)
	}
}

func TestStaleLateViewerCompletionCannotOverwriteNewerSuccess(t *testing.T) {
	s, v, conn, persisted, persistMu := newLateViewerSession(t)
	_ = s.ResizeFor(v, 120, 40) // timeout; callback retained
	if err := s.ResizeFor(v, 40, 20); err != nil {
		t.Fatal(err)
	}
	conn.fireFirstAck() // deliberately out of protocol order: must be ignored

	if cols, rows := s.AppliedSize(); cols != 40 || rows != 20 {
		t.Fatalf("stale late completion overwrote aggregate: %dx%d", cols, rows)
	}
	if _, _, _, cols, rows := s.ScreenFrame(); cols != 40 || rows != 20 {
		t.Fatalf("stale late completion overwrote mirror: %dx%d", cols, rows)
	}
	persistMu.Lock()
	got := append([][2]int(nil), (*persisted)...)
	persistMu.Unlock()
	if len(got) != 1 || got[0] != [2]int{40, 20} {
		t.Fatalf("stale late completion overwrote persistence: %v", got)
	}
}

func TestPendingDirectLateAckCannotTurnMatchingViewerSizeIntoNoop(t *testing.T) {
	s, v, conn, _, _ := newLateViewerSession(t)
	// Viewer already wants the currently applied 80x24.
	if err := s.ResizeFor(v, 80, 24); err != nil {
		t.Fatal(err)
	}
	_ = s.Resize(120, 40) // first real request: host applied, ACK timed out
	// Although AppliedSize still equals this target, an older pending completion
	// can overwrite it. A corrective request must be sent, not optimized away.
	if err := s.ResizeFor(v, 80, 24); err != nil {
		t.Fatal(err)
	}
	conn.fireFirstAck()

	got := conn.applied()
	if len(got) != 2 || got[0] != [2]int{120, 40} || got[1] != [2]int{80, 24} {
		t.Fatalf("matching viewer target was skipped while older resize pending: %v", got)
	}
	if cols, rows := s.AppliedSize(); cols != 80 || rows != 24 {
		t.Fatalf("pending direct ACK overwrote viewer state: %dx%d", cols, rows)
	}
	if _, _, _, cols, rows := s.ScreenFrame(); cols != 80 || rows != 24 {
		t.Fatalf("pending direct ACK overwrote viewer mirror: %dx%d", cols, rows)
	}
}

// А уход зрителя по-прежнему откладывает рост — иначе вернётся ping-pong.
func TestViewerLeavingStillDefersGrow(t *testing.T) {
	s, spy := newSizeSession(t, 138, 29)
	pc := s.AddViewer()
	_ = s.ResizeFor(pc, 138, 29)
	phone := s.AddViewer()
	_ = s.ResizeFor(phone, 48, 33)
	before := len(spy.applied())
	s.RemoveViewer(phone)
	if len(spy.applied()) != before {
		t.Fatal("рост при уходе зрителя применён мгновенно — переподключения снова будут дёргать размер")
	}
}
