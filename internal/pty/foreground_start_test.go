package pty

// Тесты зоны B1 внутри пакета pty.
//
// ST-02 «поколение процесса»: PID без поколения не является достаточным ключом
// (план, ST-02). Проверяется на той ОС, где запущен тест (Windows у владельца,
// Linux в WSL); сборка остальных — GOOS=linux/darwin go vet ./internal/pty.
//
// Здесь же — швы B1, которые видны только изнутри пакета: сигнал «появился
// источник истории агента» (ST-10 B, agent_history.go) и причина пустого кадра
// (ST-05, session_screen.go). Протокольную часть тех же правил проверяют
// internal/web/api_pty_screen_seam_test.go и pty_history_test.go.

import (
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"tgcontrol/internal/agenthooks"
)

// Время старта текущего процесса: не ноль, не в будущем, в пределах часа (тест
// собран и запущен только что) и одинаково между вызовами — иначе поколение
// одного и того же процесса «менялось» бы на каждом опросе.
func TestProcessStartMsIsStableGeneration(t *testing.T) {
	pid := uint32(os.Getpid())
	first := processStartMs(pid)
	now := time.Now().UnixMilli()
	if first <= 0 {
		t.Fatalf("время старта своего процесса неизвестно: %d", first)
	}
	if first > now {
		t.Fatalf("время старта в будущем: %d > %d", first, now)
	}
	if now-first > time.Hour.Milliseconds() {
		t.Fatalf("время старта %d мс назад — не похоже на только что запущенный тест", now-first)
	}
	for i := 0; i < 5; i++ {
		if again := processStartMs(pid); again != first {
			t.Fatalf("время старта нестабильно: %d, затем %d", first, again)
		}
	}
}

// Помощник для теста ниже: процесс-потомок, который живёт, пока открыт stdin.
func TestForegroundStartHelperSleeper(t *testing.T) {
	if os.Getenv("REMOTAI_FG_START_HELPER") != "1" {
		t.Skip("вспомогательный процесс для TestForegroundProcessCarriesStartGeneration")
	}
	done := make(chan struct{})
	go func() { _, _ = io.Copy(io.Discard, os.Stdin); close(done) }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
	}
}

// findForegroundProcess отдаёт поколение найденного процесса, а не только PID:
// потомок создан позже родителя, его StartMs совпадает с прямым запросом по
// тому же PID, а после выхода процесса прежнее поколение по этому PID больше
// не выдаётся.
func TestForegroundProcessCarriesStartGeneration(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	self := uint32(os.Getpid())
	selfStart := processStartMs(self)
	cmd := exec.Command(exe, "-test.run=^TestForegroundStartHelperSleeper$", "-test.count=1")
	cmd.Env = append(os.Environ(), "REMOTAI_FG_START_HELPER=1")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	child := uint32(cmd.Process.Pid)
	exited := false
	defer func() {
		if !exited {
			_ = stdin.Close()
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	}()

	childStart := processStartMs(child)
	if childStart <= 0 || childStart < selfStart {
		t.Fatalf("поколение потомка %d при старте родителя %d", childStart, selfStart)
	}

	// Снимок процессов и кэш переднего плана живут по 500 мс: потомок мог
	// родиться после свежего снимка, поэтому ждём, пока его увидит обход.
	var info ProcessInfo
	deadline := time.Now().Add(5 * time.Second)
	for {
		fgCache.mu.Lock()
		delete(fgCache.entries, self)
		fgCache.mu.Unlock()
		info, err = findForegroundProcess(self)
		if err == nil && info.PID != 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("потомок не найден обходом: info=%+v err=%v", info, err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	// Листом может оказаться не сам потомок, а его служебный ребёнок (conhost
	// на Windows) — важно, что поколение согласовано с PID, который отдан.
	if info.StartMs <= 0 {
		t.Fatalf("передний план без поколения: %+v", info)
	}
	if direct := processStartMs(info.PID); direct != info.StartMs {
		t.Fatalf("поколение из обхода %d, прямой запрос по PID %d даёт %d", info.StartMs, info.PID, direct)
	}
	if info.StartMs < childStart {
		t.Fatalf("передний план %+v старше своего предка (старт %d)", info, childStart)
	}

	_ = stdin.Close()
	_ = cmd.Wait()
	exited = true
	if after := processStartMs(child); after == childStart {
		t.Fatalf("после выхода процесса PID %d всё ещё выдаёт прежнее поколение %d", child, after)
	}
}

// ST-10 B: канал «у сессии появился источник истории» закрывается ровно на
// первом событии хука, которое источник называет. События без источника его не
// трогают, смена источника не закрывает канал повторно (иначе паника двойного
// close), а ожидание, брошенное до сигнала, не держит сессию в реестре.
func TestHistorySourceReadyFiresOnFirstSource(t *testing.T) {
	s := &Session{ID: "hist-ready"}
	ready, cancel := s.HistorySourceReady()
	s.rememberHistorySource(agenthooks.Event{Agent: "kimi", SessionID: "not-a-history-source"})
	s.rememberHistorySource(agenthooks.Event{Agent: "claude"})
	select {
	case <-ready:
		t.Fatal("способность истории объявлена без источника")
	default:
	}
	s.rememberHistorySource(agenthooks.Event{Agent: "claude", SessionID: "sess-1", Transcript: "/tmp/sess-1.jsonl"})
	select {
	case <-ready:
	case <-time.After(time.Second):
		t.Fatal("источник появился, а канал не закрыт")
	}
	cancel() // после сигнала — безвредна
	s.rememberHistorySource(agenthooks.Event{Agent: "codex", SessionID: "thread-2"})

	again, cancelAgain := s.HistorySourceReady()
	defer cancelAgain()
	select {
	case <-again:
	default:
		t.Fatal("источник уже есть, а новое ожидание не закрыто сразу")
	}

	other := &Session{ID: "hist-wait"}
	_, cancelOther := other.HistorySourceReady()
	held := func(s *Session) bool {
		historyWaiters.Lock()
		defer historyWaiters.Unlock()
		_, ok := historyWaiters.m[s]
		return ok
	}
	if !held(other) {
		t.Fatal("ожидание не зарегистрировано")
	}
	cancelOther()
	if held(other) || held(s) {
		t.Fatal("реестр ожиданий держит сессию после отписки или сигнала")
	}
}

// ST-05: у пустого кадра есть причина. Незакрытая CSI — «ещё не готово» и
// проходит само на следующей границе потока; транспорт без упорядоченного ACK
// на resize — «кадров не будет»; прежняя обёртка возвращает то же, что раньше.
func TestScreenCaptureNamesEmptyFrameReason(t *testing.T) {
	if c := (&Session{}).CaptureScreenFrame(); c.Frame != "" || c.Reason != ScreenReasonUnavailable || c.Retry {
		t.Fatalf("сессия без зеркала: %+v", c)
	}
	s := &Session{epoch: "e1", subs: map[chan []byte]*subState{}, dec: newDecTracker()}
	s.startScreen(40, 8)
	defer s.stopScreen()

	s.feedScreenAt([]byte("hello\x1b[31"), 10)
	c := s.CaptureScreenFrame()
	if c.Frame != "" || c.Reason != ScreenReasonNotReady || c.Retry || c.Wake != nil {
		t.Fatalf("незакрытая CSI: %+v", c)
	}
	if frame, _, _, _, _, _, _, retry, wake := s.ScreenFrameAtRevisionWait(); frame != "" || retry || wake != nil {
		t.Fatalf("прежняя обёртка на незакрытой CSI: frame=%q retry=%v", frame, retry)
	}

	s.feedScreenAt([]byte("m!"), 12)
	c = s.CaptureScreenFrame()
	if c.Frame == "" || c.Reason != "" || c.BaseOff != 12 || !strings.Contains(c.Frame, "hello") {
		t.Fatalf("после закрытия CSI: reason=%q base=%d frame=%q", c.Reason, c.BaseOff, c.Frame)
	}
	if frame, _, _, _, _, base, _, _, _ := s.ScreenFrameAtRevisionWait(); frame != c.Frame || base != c.BaseOff {
		t.Fatal("прежняя обёртка разошлась с CaptureScreenFrame")
	}

	s.invalidateScreenDelivery(0)
	if c := s.CaptureScreenFrame(); c.Frame != "" || c.Reason != ScreenReasonUnavailable || c.Retry {
		t.Fatalf("deliveryAuthoritative=false: %+v", c)
	}
}

// holdScreenWorker останавливает воркер зеркала на чанке с beforeApply и
// кладёт за ним ещё один чанк вывода. Ради этого второго чанка всё и затевается:
// после подмены воркер дочитает удержанный чанк, на следующем увидит
// mirror==nil и выйдет, так и не подтвердив барьер, стоящий дальше в очереди.
// release безопасно звать повторно (defer на случай Fatal: иначе stopScreen
// ждал бы удержанный воркер вечно).
func holdScreenWorker(t *testing.T, s *Session) (*sessionScreen, func()) {
	t.Helper()
	sc, _ := s.screen.Load().(*sessionScreen)
	if sc == nil {
		t.Fatal("нет зеркала")
	}
	entered := make(chan struct{})
	rel := make(chan struct{})
	sc.queued.Add(1)
	sc.feed <- screenChunk{data: []byte{'x'}, beforeApply: func() {
		close(entered)
		<-rel
	}}
	<-entered
	sc.queued.Add(1)
	sc.feed <- screenChunk{data: []byte{'y'}}
	var once sync.Once
	return sc, func() { once.Do(func() { close(rel) }) }
}

// waitBarrierQueued ждёт, пока CaptureScreenFrame поставит свой барьер в
// очередь за чанком вывода (len(feed) == 2: вывод и барьер).
func waitBarrierQueued(t *testing.T, sc *sessionScreen) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for len(sc.feed) < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("барьер снимка не встал в очередь: len(feed)=%d", len(sc.feed))
		}
		time.Sleep(time.Millisecond)
	}
}

func captureAsync(s *Session) <-chan ScreenCapture {
	got := make(chan ScreenCapture, 1)
	go func() { got <- s.CaptureScreenFrame() }()
	return got
}

func awaitCapture(t *testing.T, got <-chan ScreenCapture) ScreenCapture {
	t.Helper()
	select {
	case c := <-got:
		return c
	case <-time.After(5 * time.Second):
		t.Fatal("снимок кадра не вернулся")
		return ScreenCapture{}
	}
}

// ST-05, замечание ревью B1 (14.09.2026): подмена зеркала, пока снимок ждал
// барьер очереди, — «ещё не готово», а не «кадров не будет». done старого
// зеркала закрывается и при подмене, поэтому прежний код отвечал unavailable, и
// клиент screen-request-v1 уходил в degraded до переподключения, хотя новое
// зеркало авторитетно и следующий же запрос даёт кадр.
func TestScreenCaptureSwapDuringBarrierIsNotReady(t *testing.T) {
	s := &Session{epoch: "e1", subs: map[chan []byte]*subState{}, dec: newDecTracker()}
	s.startScreen(40, 8)
	defer s.stopScreen()
	sc, release := holdScreenWorker(t, s)
	defer release()

	got := captureAsync(s)
	waitBarrierQueued(t, sc)
	// Reset потока (Hello новой эпохи) подменяет зеркало, пока барьер ждёт.
	s.screenLifeMu.Lock()
	old, retired := s.resetScreenEmptyLocked("", true)
	s.screenLifeMu.Unlock()
	release()
	c := awaitCapture(t, got)
	finishScreenRetire(old, retired)

	if c.Frame != "" || c.Reason != ScreenReasonNotReady || c.Retry {
		t.Fatalf("подмена во время барьера: frame=%d байт reason=%q retry=%v", len(c.Frame), c.Reason, c.Retry)
	}
	if next := s.CaptureScreenFrame(); next.Frame == "" || next.Reason != "" {
		t.Fatalf("после подмены новое зеркало кадра не дало: reason=%q", next.Reason)
	}
}

// То же для отставшего зеркала: его пересборку (recoverScreen) опередил другой
// зритель или reset — наша пересборка отказывает, но кадр у сессии есть.
func TestScreenCaptureSwapDuringStaleRecoveryIsNotReady(t *testing.T) {
	s := &Session{epoch: "e1", subs: map[chan []byte]*subState{}, dec: newDecTracker()}
	s.startScreen(40, 8)
	defer s.stopScreen()
	sc, release := holdScreenWorker(t, s)
	defer release()
	sc.stale.Store(true)

	got := captureAsync(s)
	waitBarrierQueued(t, sc) // барьер recoverScreen
	s.screenLifeMu.Lock()
	old, retired := s.resetScreenEmptyLocked("", true)
	s.screenLifeMu.Unlock()
	release()
	c := awaitCapture(t, got)
	finishScreenRetire(old, retired)

	if c.Frame != "" || c.Reason != ScreenReasonNotReady || c.Retry {
		t.Fatalf("подмена во время пересборки: frame=%d байт reason=%q retry=%v", len(c.Frame), c.Reason, c.Retry)
	}
	if next := s.CaptureScreenFrame(); next.Frame == "" || next.Reason != "" {
		t.Fatalf("после подмены новое зеркало кадра не дало: reason=%q", next.Reason)
	}
}

// Остановка зеркала во время барьера остаётся «кадров не будет»: у сессии
// зеркала больше нет, повторять бессмысленно.
func TestScreenCaptureStopDuringBarrierIsUnavailable(t *testing.T) {
	s := &Session{epoch: "e1", subs: map[chan []byte]*subState{}, dec: newDecTracker()}
	s.startScreen(40, 8)
	sc, release := holdScreenWorker(t, s)
	defer release()

	got := captureAsync(s)
	waitBarrierQueued(t, sc)
	stopped := make(chan struct{})
	go func() { s.stopScreen(); close(stopped) }()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if cur, _ := s.screen.Load().(*sessionScreen); cur == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("stopScreen не снял зеркало")
		}
		time.Sleep(time.Millisecond)
	}
	release()
	c := awaitCapture(t, got)
	<-stopped

	if c.Frame != "" || c.Reason != ScreenReasonUnavailable || c.Retry {
		t.Fatalf("остановка во время барьера: frame=%d байт reason=%q retry=%v", len(c.Frame), c.Reason, c.Retry)
	}
}

// ST-05: resize через снимок — причина resize-pending при прежнем контракте
// retry/wake: сервер ждёт ACK и повторяет сам.
func TestScreenCaptureResizePendingKeepsRetryContract(t *testing.T) {
	s := &Session{epoch: "e1", subs: map[chan []byte]*subState{}, dec: newDecTracker()}
	s.startScreen(40, 8)
	defer s.stopScreen()
	s.feedScreenAt([]byte("ok"), 2)

	s.beginScreenGeometryChange()
	c := s.CaptureScreenFrame()
	if c.Frame != "" || c.Reason != ScreenReasonResizePending || !c.Retry || c.Wake == nil {
		t.Fatalf("resize в пути: %+v", c)
	}
	s.finishScreenGeometryChange()
	select {
	case <-c.Wake:
	case <-time.After(time.Second):
		t.Fatal("завершение resize не разбудило ожидающего")
	}
	if c := s.CaptureScreenFrame(); c.Frame == "" || c.Reason != "" {
		t.Fatalf("после ACK кадра нет: %+v", c)
	}
}
