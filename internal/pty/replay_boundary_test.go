package pty

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// ГРАНИЦА ПЕРЕИГРОВКИ: сигнал звучит РОВНО ОДИН РАЗ и ровно на первом живом
// выводе после буфера. Раньше сигнала не было вовсе — сессия не отличала
// прошлое от настоящего.
func TestReplayWatchFiresOnceAfterSnapshot(t *testing.T) {
	var fired int
	var w replayWatch
	w.set(func() { fired++ })

	w.noteSnapshot()
	w.noteSnapshot()
	if fired != 0 {
		t.Fatalf("переигровка ещё идёт, а граница уже объявлена: %d", fired)
	}
	w.noteOutput()
	w.noteOutput()
	w.noteOutput()
	if fired != 1 {
		t.Fatalf("граница объявлена %d раз вместо одного", fired)
	}
}

// Новая сессия: буфера не было вовсе, значит и границы нет — трогать нечего.
func TestReplayWatchSilentWithoutSnapshot(t *testing.T) {
	var fired int
	var w replayWatch
	w.set(func() { fired++ })
	w.noteOutput()
	w.noteOutput()
	if fired != 0 {
		t.Fatalf("без переигровки граница не объявляется, а объявлена %d раз", fired)
	}
}

// ВНЕШНИЙ АУДИТ 2.57.18, находка P0-03: тихая сессия. Хост отдал весь снапшот,
// агент после этого НИЧЕГО не печатает — и прежняя граница («первый живой
// вывод») не наступала никогда: мусор переигровки оставался в scrollback
// зеркала. Явный frReplayEnd закрывает границу без единого байта живого вывода.
func TestReplayWatchFiresOnReplayEndWithoutOutput(t *testing.T) {
	var fired int
	var w replayWatch
	w.set(func() { fired++ })

	// Снапшот в несколько кусков, затем явная граница — и тишина.
	w.noteSnapshot()
	w.noteSnapshot()
	w.noteSnapshot()
	w.noteReplayEnd()
	if fired != 1 {
		t.Fatalf("тихая сессия: граница по frReplayEnd не объявлена (fired=%d)", fired)
	}
	// Поздний живой вывод не должен звонить второй раз.
	w.noteOutput()
	if fired != 1 {
		t.Fatalf("граница объявлена повторно на живом выводе: %d", fired)
	}
}

// Пустой снапшот: хост шлёт frReplayEnd всегда, но переигровки не было —
// очищать нечего, сигнал обязан молчать.
func TestReplayWatchReplayEndWithoutSnapshotIsSilent(t *testing.T) {
	var fired int
	var w replayWatch
	w.set(func() { fired++ })
	w.noteReplayEnd()
	if fired != 0 {
		t.Fatalf("без снапшота frReplayEnd не должен объявлять границу: %d", fired)
	}
}

// Обратная совместимость: СТАРЫЙ хост frReplayEnd не шлёт — граница по-прежнему
// закрывается первым живым выводом после снапшота.
func TestReplayWatchOldHostFallbackStillWorks(t *testing.T) {
	var fired int
	var w replayWatch
	w.set(func() { fired++ })
	w.noteSnapshot()
	w.noteOutput()
	if fired != 1 {
		t.Fatalf("старый хост: граница по первому frOutput не сработала: %d", fired)
	}
}

// ГЛАВНОЕ. Дубли на скриншоте владельца 14.08.2026 (шапка `Claude Code
// v2.1.228` дважды подряд) — это строки, которые переигровка старого буфера
// нагнала в scrollback ЗЕРКАЛА после автообновления агента. Настоящий xterm на
// том же потоке не оставляет там ничего.
//
// Здесь воспроизводится ровно это: агент перерисовывает экран на месте, буфер
// переигрывается, и без сигнала о границе история зеркала получает копию
// прошлого экрана.
func TestReplayScrollbackDroppedAtBoundary(t *testing.T) {
	repaint := func(m *screenMirror, tag string) {
		m.Write([]byte("\x1b[H\x1b[2J"))
		for i := 1; i <= 6; i++ {
			m.Write([]byte(fmt.Sprintf("\x1b[%d;1H%s строка %d", i, tag, i)))
		}
		// Экран прокрутился вверх — так строки и уходят в scrollback.
		m.Write([]byte("\x1b[6;1H\n\n\n"))
	}

	// Без границы: переигровка оседает в истории.
	dirty := newScreenMirror(48, 8)
	defer dirty.Close()
	repaint(dirty, "переигровка")
	repaint(dirty, "живой вывод")
	hist, _ := dirty.History(500)
	if !strings.Contains(hist, "переигровка строка 1") {
		t.Fatalf("замер не воспроизводит дефект: переигровки нет в истории:\n%q", hist)
	}

	// С границей: то же самое, но между переигровкой и живым выводом приходит
	// сигнал — и прошлое в историю не попадает.
	clean := newScreenMirror(48, 8)
	defer clean.Close()
	repaint(clean, "переигровка")
	clean.DropScrollback()
	repaint(clean, "живой вывод")
	hist2, _ := clean.History(500)
	if strings.Contains(hist2, "переигровка строка 1") {
		t.Fatalf("переигровка всё ещё в истории зеркала:\n%q", hist2)
	}
	if !strings.Contains(hist2, "живой вывод строка 1") {
		t.Fatalf("вместе с переигровкой выбросило и живой вывод:\n%q", hist2)
	}
}

// Маркер едет ТЕМ ЖЕ каналом, что и байты: очистка обязана случиться ПОСЛЕ
// разбора переигровки, иначе она не сделает ничего. И только у агентских
// сессий — у оболочки переигранная история настоящая.
func TestDropReplayScrollbackOrderAndGate(t *testing.T) {
	sess := &Session{}
	sess.startScreen(48, 8)
	defer sess.stopScreen()

	// ⚠ Проверяем ИМЕННО scrollback: `History()` отдаёт scrollback ПЛЮС
	// видимые строки экрана (так задумано — см. historyLocked), а очистка
	// трогает только историю прокрутки. Экран обязан остаться собранным.
	scrollbackOf := func() string {
		hist, n := sess.screenSnapshotHistory()
		lines := strings.Split(hist, "\r\n")
		keep := n - 8 // высота зеркала
		if keep < 0 || keep > len(lines) {
			keep = 0
		}
		return strings.Join(lines[:keep], "\r\n")
	}
	// Строк печатаем БОЛЬШЕ высоты: иначе ничего не прокрутится и в scrollback
	// не попадёт вовсе — замер проверял бы пустоту.
	feedLines := func(tag string) {
		for i := 1; i <= 12; i++ {
			sess.feedScreen([]byte(fmt.Sprintf("%s строка %02d\r\n", tag, i)))
		}
	}

	// Оболочка: вид агента не известен — сигнал игнорируется, история её.
	feedLines("оболочка")
	sess.dropReplayScrollback()
	feedLines("ещё")
	waitScreenDrained(t, sess)
	if sb := scrollbackOf(); !strings.Contains(sb, "оболочка строка 01") {
		t.Fatalf("у оболочки отобрали настоящую историю:\n%q", sb)
	}

	// Агент: сигнал работает, и работает ПОСЛЕ уже поданных байтов.
	sess2 := &Session{}
	sess2.startScreen(48, 8)
	defer sess2.stopScreen()
	sess2.rememberAgentKind("claude")
	for i := 1; i <= 12; i++ {
		sess2.feedScreen([]byte(fmt.Sprintf("переигровка строка %02d\r\n", i)))
	}
	sess2.dropReplayScrollback()
	for i := 1; i <= 12; i++ {
		sess2.feedScreen([]byte(fmt.Sprintf("живой вывод строка %02d\r\n", i)))
	}
	waitScreenDrained(t, sess2)
	hist2, n2 := sess2.screenSnapshotHistory()
	lines2 := strings.Split(hist2, "\r\n")
	keep2 := n2 - 8
	if keep2 < 0 || keep2 > len(lines2) {
		keep2 = 0
	}
	sb2 := strings.Join(lines2[:keep2], "\r\n")
	// ⚠ ЧТО ИМЕННО ОБЯЗАНО ИСЧЕЗНУТЬ. Очистка убирает историю, накопленную
	// переигровкой (строки 01–04 успели уйти вверх), но НЕ стирает экран: то,
	// что переигровка оставила видимым, — это актуальная картинка агента, и её
	// уход вверх под новым выводом законен. Ожидать обратного — значит
	// требовать пустой экран после каждого рестарта.
	if strings.Contains(sb2, "переигровка строка 01") {
		t.Fatalf("переигровка осталась в истории — маркер обогнал байты:\n%q", sb2)
	}
	if !strings.Contains(sb2, "живой вывод строка") {
		t.Fatalf("живой вывод пропал вместе с переигровкой:\n%q", sb2)
	}
}

// frReplayEnd — настоящий barrier: возврат означает, что все
// предыдущие replay-байты разобраны и scrollback уже очищен.
func TestDropReplayScrollbackWaitsForOrderedBarrier(t *testing.T) {
	s := &Session{}
	s.startScreen(48, 8)
	defer s.stopScreen()
	s.rememberAgentKind("claude")
	for i := 0; i < 20; i++ {
		s.feedScreen([]byte(fmt.Sprintf("replay-%02d\r\n", i)))
	}
	waitScreenIdle(t, s)

	sc, _ := s.screen.Load().(*sessionScreen)
	sc.mu.Lock()
	m := sc.mirror
	sc.mu.Unlock()
	m.mu.Lock()
	done := make(chan struct{})
	go func() {
		s.dropReplayScrollback()
		close(done)
	}()
	select {
	case <-done:
		m.mu.Unlock()
		t.Fatal("dropReplayScrollback вернулся до обработки marker")
	case <-time.After(30 * time.Millisecond):
	}
	m.mu.Unlock()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("ordered barrier не завершился")
	}
}

// ВНЕШНИЙ АУДИТ 2.57.18, находка P0-02: исторический вид агента не очищается
// при его выходе, и прежний порядок («сначала lastAgentKind») принимал его за
// ТЕКУЩЕГО. Сценарий боя: Claude завершился → человек в PowerShell напечатал
// важную историю → агент переподключился → граница переигровки по историческому
// "claude" стирала DropScrollback'ом настоящую историю оболочки. Плюс скрытый
// дефект того же гейта: AgentKind не возвращает пустоту вообще (default —
// "other"), поэтому сессия с оболочкой на переднем плане считалась «годной к
// очистке» с видом "shell"/"node".
func TestCleanupAgentKindTrustsCurrentForeground(t *testing.T) {
	cases := []struct {
		name       string
		fgKind     string
		fgKnown    bool
		historical string
		wantKind   string
		wantOK     bool
	}{
		// ГЛАВНОЕ: агент когда-то работал, но СЕЙЧАС на переднем плане оболочка —
		// её историю отнимать нельзя, что бы ни было запомнено.
		{"агент вышел, впереди powershell", "shell", true, "claude", "", false},
		{"агент вышел, впереди bash", "shell", true, "kimi", "", false},
		{"впереди неагентский процесс", "node", true, "claude", "", false},
		{"впереди неизвестное приложение", "other", true, "claude", "", false},
		// Агент на переднем плане прямо сейчас — очистка допустима.
		{"агент сейчас в foreground", "claude", true, "", "claude", true},
		{"агент перебил прошлый вид", "codex", true, "claude", "codex", true},
		// Передний план неизвестен (backend отключён, процесс не разрешился) —
		// только тогда решает история, и только агентская. Это живой случай
		// 14.08: граница переигровки после перезапуска агента приходит раньше
		// первого опроса состояния.
		{"foreground неизвестен, был агент", "", false, "claude", "claude", true},
		{"foreground неизвестен, была оболочка", "", false, "shell", "", false},
		{"foreground неизвестен, ничего не было", "", false, "", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			kind, ok := cleanupAgentKind(c.fgKind, c.fgKnown, c.historical)
			if ok != c.wantOK || kind != c.wantKind {
				t.Fatalf("cleanupAgentKind(%q, %v, %q) = (%q, %v), ждали (%q, %v)",
					c.fgKind, c.fgKnown, c.historical, kind, ok, c.wantKind, c.wantOK)
			}
		})
	}
}

// waitScreenDrained ждёт, пока зеркало разберёт очередь: подача асинхронна.
func waitScreenDrained(t *testing.T, s *Session) {
	t.Helper()
	sc, _ := s.screen.Load().(*sessionScreen)
	if sc == nil {
		t.Fatal("зеркала нет")
	}
	deadline := time.Now().Add(2 * time.Second)
	for sc.queued.Load() > 0 {
		if time.Now().After(deadline) {
			t.Fatal("зеркало не разобрало очередь за 2 с")
		}
		time.Sleep(2 * time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond) // маркер очистки байтов не считает
}

// screenSnapshotHistory — история зеркала сессии как её увидит клиент.
func (s *Session) screenSnapshotHistory() (string, int) {
	sc, _ := s.screen.Load().(*sessionScreen)
	if sc == nil {
		return "", 0
	}
	sc.mu.Lock()
	m := sc.mirror
	sc.mu.Unlock()
	if m == nil {
		return "", 0
	}
	return m.History(500)
}
