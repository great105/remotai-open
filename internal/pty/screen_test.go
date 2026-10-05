package pty

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

// ГЛАВНАЯ ГРАБЛЯ БИБЛИОТЕКИ: эмулятор отвечает на запросы приложения (DSR, DA,
// CPR) во внутреннюю трубу, и если её никто не вычитывает — Write блокируется
// НАВСЕГДА (charmbracelet/x#939). Цена ошибки не кривая картинка, а мёртвая
// сессия: горутина-кормилица встанет, и терминал у человека замрёт, а причина
// будет выглядеть как «сеть».
func TestScreenMirrorSurvivesTerminalQueries(t *testing.T) {
	m := newScreenMirror(80, 24)
	defer m.Close()

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 2000; i++ {
			m.Write([]byte("\x1b[6n")) // «где курсор?» — эмулятор обязан ответить
		}
		m.Write([]byte("\x1b[c\x1b[5n")) // ещё два запроса другого рода
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Write заблокировался на запросах терминала — дренаж ответов не работает")
	}

	// И зеркало осталось живым: после залпа запросов оно по-прежнему рисует.
	m.Write([]byte("\x1b[H\x1b[2Jпосле залпа"))
	if got := m.Frame(); !strings.Contains(got, "после залпа") {
		t.Fatalf("зеркало не приняло вывод после запросов: %q", got)
	}
}

// vt на ED2 (полное стирание экрана) сохраняет все непустые строки в
// scrollback — xterm.js на телефоне просто стирает. Claude Code
// перерисовывается через ED2 на каждый ресайз и крупный апдейт, и каждая
// перерисовка клала в историю зеркала копию всего экрана: прокрутка у
// владельца состояла из дублей (боевой замер 13.08.2026 — 185 строк истории
// у зеркала против 74 у xterm.js на одном и том же хвосте кольца 512 КБ).
// Подмена ED2 → ED1+ED0 (rewriteEraseAll) стирает то же самое, но историю
// не раздувает.
func TestScreenMirrorED2RepaintDoesNotDuplicateHistory(t *testing.T) {
	m := newScreenMirror(48, 10)
	defer m.Close()
	repaint := func() {
		m.Write([]byte("\x1b[H\x1b[2J"))
		for i := 1; i <= 5; i++ {
			m.Write([]byte(fmt.Sprintf("\x1b[%d;1Hблок строка %d", i, i)))
		}
	}
	repaint()
	repaint()
	repaint()
	history, _ := m.History(500)
	if got := strings.Count(history, "блок строка 1"); got != 1 {
		t.Fatalf("перерисовка через ED2 размножила историю: %d копий вместо 1:\n%q", got, history)
	}
	// Стирание при этом настоящее: старый кадр не просвечивает.
	m.Write([]byte("\x1b[H\x1b[2J\x1b[1;1Hчистый экран"))
	if f := m.Frame(); !strings.Contains(f, "чистый экран") || strings.Contains(f, "блок строка 3") {
		t.Fatalf("подмена ED2 перестала стирать экран: %q", f)
	}
}

// ED2, разрезанный ConPTY по границе буфера (ESC[2 | J), обязан быть подменён
// так же, как целый: хвост-префикс придерживается до следующего чанка. Иначе
// каждая N-я перерисовка проскакивала бы подмену и история снова зарастала.
func TestScreenMirrorED2SplitAcrossChunksStillRewritten(t *testing.T) {
	m := newScreenMirror(48, 10)
	defer m.Close()
	m.Write([]byte("\x1b[1;1Hпервая строка"))
	m.Write([]byte("\x1b[H\x1b[2")) // разрез посреди последовательности
	m.Write([]byte("J"))            // продолжение в следующем чанке
	m.Write([]byte("\x1b[1;1Hновый экран"))
	history, _ := m.History(500)
	if strings.Contains(history, "первая строка") && !strings.Contains(m.Frame(), "первая строка") {
		t.Fatalf("разрезанный ED2 проскочил подмену — стёртый экран уехал в историю:\n%q", history)
	}
	if f := m.Frame(); !strings.Contains(f, "новый экран") || strings.Contains(f, "первая строка") {
		t.Fatalf("разрезанный ED2 не стёр экран: %q", f)
	}
}

// Строка scrollback, напечатанная в ПРЕЖНЕЙ (более широкой) геометрии PTY,
// отдаётся в истории ЦЕЛИКОМ — клиентский xterm сам её завернёт. Обрезка по
// текущей ширине зеркала молча съедала правые части («Codex и Kimi рвут
// консоль», 13.08.2026: реплей кольца одной ширины в зеркале другой).
func TestScreenMirrorHistoryKeepsLinesWiderThanCurrentWidth(t *testing.T) {
	m := newScreenMirror(80, 6)
	defer m.Close()
	wide := "wide-line-" + strings.Repeat("x", 55) + "-tail" // ~70 колонок
	for i := 0; i < 10; i++ {
		m.Write([]byte(wide + "\r\n"))
	}
	m.Resize(48, 6) // телефон пришёл: зеркало сузилось, история осталась широкой
	history, _ := m.History(500)
	if !strings.Contains(history, "-tail") {
		t.Fatalf("правый край широкой строки истории обрезан по новой ширине:\n%q", history)
	}
}

// Кадр обязан быть САМОДОСТАТОЧНЫМ: чистый терминал нужной геометрии должен
// восстановить по нему картинку целиком. Проверяем состав, а точность
// воспроизведения — сквозной пробой через настоящий xterm.js
// (build/qa/probe-screen-frame.mjs).
func TestScreenMirrorFrameIsSelfContained(t *testing.T) {
	m := newScreenMirror(48, 12)
	defer m.Close()
	m.Write([]byte("\x1b[?1049h"))                     // приложение вошло в alt-screen
	m.Write([]byte("\x1b[H\x1b[2J"))                   //
	m.Write([]byte("AGENT FRAME LINE 1\r\n"))          //
	m.Write([]byte("\x1b[38;2;46;230;176m* Running…")) // цветная строка
	m.Write([]byte("\x1b[m\x1b[5;3HLINE FIVE"))        // адресация абсолютом

	f := m.Frame()
	for _, want := range []string{
		"\x1b[?1049h", // вернуть клиента в тот же буфер
		"\x1b[m",      // сбросить стиль ДО очистки, иначе экран зальётся фоном
		"\x1b[2J",     // очистить
		"\x1b[1;1H",   // строки адресуются абсолютно
		"\x1b[5;1H",
		"AGENT FRAME LINE 1",
		"* Running…",
		"LINE FIVE",
		"38;2;46;230;176", // truecolor дожил до кадра
	} {
		if !strings.Contains(f, want) {
			t.Fatalf("в кадре нет %q:\n%q", want, f)
		}
	}
	// Позиция курсора — последней и абсолютной.
	if i := strings.LastIndex(f, "\x1b["); i < 0 || !strings.HasSuffix(f[i:], "H") {
		t.Fatalf("кадр не заканчивается абсолютной позицией курсора: %q", f)
	}
	// Сброс стиля обязан стоять ПЕРЕД очисткой: ESC[2J закрашивает экран
	// текущим фоном, и обратный порядок положил бы кадр на цветную подложку.
	if strings.Index(f, "\x1b[m") > strings.Index(f, "\x1b[2J") {
		t.Fatalf("сброс стиля после очистки — кадр ляжет на чужой фон: %q", f)
	}
}

// Обычная оболочка (без alt-screen) не должна получать вход в альтернативный
// экран: её история реальна, и загонять её в alt значит потерять прокрутку.
func TestScreenMirrorFrameKeepsNormalBuffer(t *testing.T) {
	m := newScreenMirror(48, 12)
	defer m.Close()
	m.Write([]byte("PS C:\\> ls\r\nfoo bar\r\n"))
	if f := m.Frame(); strings.Contains(f, "\x1b[?1049h") {
		t.Fatalf("обычную оболочку загнали в alt-screen: %q", f)
	}
}

func TestNormalScreenFrameExitsStaleClientAltBuffer(t *testing.T) {
	source := newScreenMirror(20, 4)
	defer source.Close()
	source.Write([]byte("NORMAL SCREEN"))
	frame := source.Frame()

	client := newScreenMirror(20, 4)
	defer client.Close()
	client.Write([]byte("\x1b[?47h\x1b[?1047h\x1b[?1049h\x1b[HSTALE ALT"))
	if !client.em.IsAltScreen() {
		t.Fatal("test setup did not enter alt screen")
	}
	client.Write([]byte(frame)) // replace=false screen repair path
	if client.em.IsAltScreen() {
		t.Fatalf("normal frame left client in stale alt buffer: %q", frame)
	}
	history, _ := client.History(100)
	if !strings.Contains(history, "NORMAL SCREEN") || strings.Contains(history, "STALE ALT") {
		t.Fatalf("normal frame restored wrong buffer/history: %q", history)
	}
}

func TestScreenFrameSynchronizesMouseAndBracketedPasteModes(t *testing.T) {
	active := newScreenMirror(20, 4)
	defer active.Close()
	active.Write([]byte("\x1b[?1049h\x1b[?1000h\x1b[?1006h\x1b[?2004hACTIVE"))
	activeFrame := active.Frame()
	for reset, set := range map[string]string{
		"\x1b[?1002l": "\x1b[?1000h",
		"\x1b[?1003l": "\x1b[?1000h",
		"\x1b[?1005l": "\x1b[?1006h",
		"\x1b[?1015l": "\x1b[?1006h",
	} {
		if ri, si := strings.Index(activeFrame, reset), strings.Index(activeFrame, set); ri < 0 || si < 0 || ri > si {
			t.Fatalf("exclusive mode reset must precede active SET: reset=%q set=%q frame=%q", reset, set, activeFrame)
		}
	}

	clean := newScreenMirror(20, 4)
	defer clean.Close()
	clean.Write([]byte(activeFrame))
	clean.mu.Lock()
	activeAlt := clean.em.IsAltScreen()
	activeModes := clean.dec.snapshot()
	clean.mu.Unlock()
	for _, want := range []int{1000, 1006, 2004} {
		found := false
		for _, got := range activeModes {
			found = found || got == want
		}
		if !found {
			t.Fatalf("clean frame lost mode %d: alt=%v modes=%v", want, activeAlt, activeModes)
		}
	}
	if !activeAlt {
		t.Fatalf("clean frame lost active alt buffer: modes=%v", activeModes)
	}

	normal := newScreenMirror(20, 4)
	defer normal.Close()
	normal.Write([]byte("NORMAL"))
	dirty := newScreenMirror(20, 4)
	defer dirty.Close()
	dirty.Write([]byte("\x1b[?1049h\x1b[?1000h\x1b[?1006h\x1b[?2004hDIRTY"))
	dirty.Write([]byte(normal.Frame()))
	dirty.mu.Lock()
	dirtyAlt := dirty.em.IsAltScreen()
	dirtyModes := dirty.dec.snapshot()
	dirty.mu.Unlock()
	if dirtyAlt || len(dirtyModes) != 0 {
		t.Fatalf("normal frame left stale client modes: alt=%v modes=%v", dirtyAlt, dirtyModes)
	}
}

// ГРАБЛЯ: ConPTY режет поток по границе своего буфера, а не по границе кластера
// графем. Разрезанная посреди ZWJ семья разваливается в одиночного человечка
// (charmbracelet/x#935). Кормим ровно так, как это делает живой поток.
func TestScreenMirrorKeepsGraphemeAcrossChunks(t *testing.T) {
	const fam = "\U0001F468\u200D\U0001F469\u200D\U0001F467" // 👨‍👩‍👧
	// Хвост, способный продолжиться, придерживается до простоя — поэтому и
	// эталон, и проверяемые кадры снимаем после паузы, иначе сравнивали бы два
	// одинаково недорисованных кластера.
	settle := func(m *screenMirror) string {
		time.Sleep(pendingIdle + 30*time.Millisecond)
		return m.Frame()
	}
	whole := newScreenMirror(20, 3)
	defer whole.Close()
	whole.Write([]byte(fam))
	want := settle(whole)
	if !strings.Contains(want, fam) {
		t.Fatalf("эталон сам потерял кластер: %q", want)
	}

	for cut := 1; cut < len(fam); cut++ {
		split := newScreenMirror(20, 3)
		split.Write([]byte(fam)[:cut])
		split.Write([]byte(fam)[cut:])
		got := settle(split)
		split.Close()
		if got != want {
			t.Fatalf("разрез на байте %d разваливает кластер:\n получили %q\n ожидали  %q", cut, got, want)
		}
	}
}

func TestGridCellsFromStreamKeepsGraphemeAsOneTerminalCell(t *testing.T) {
	const family = "\U0001F468\u200D\U0001F469\u200D\U0001F467"
	grid := GridCellsFromStream([]byte("A"+family+"B"), 10, 3, 1)
	if len(grid) != 3 || len(grid[0]) != 10 {
		t.Fatalf("cell grid geometry=%dx%d, want 10x3", len(grid[0]), len(grid))
	}
	if grid[0][0] != "A" || grid[0][1] != family || grid[0][2] != "" || grid[0][3] != "B" {
		t.Fatalf("ZWJ cell representation shifted: %#v", grid[0])
	}
}

func TestScreenMirrorResizeDoesNotFlushIncompleteCarry(t *testing.T) {
	utf8Mirror := newScreenMirror(10, 4)
	defer utf8Mirror.Close()
	utf8Mirror.Write([]byte{0xf0, 0x9f}) // first half of U+1F600
	utf8Mirror.Resize(20, 4)
	utf8Mirror.mu.Lock()
	pending := append([]byte(nil), utf8Mirror.pending...)
	utf8Mirror.mu.Unlock()
	if len(pending) != 2 {
		t.Fatalf("resize flushed incomplete UTF-8 as parser input: pending=%x", pending)
	}
	utf8Mirror.Write([]byte{0x98, 0x80, 'X'})
	wantUTF8 := string(rune(0x1f600)) + "X"
	if frame := utf8Mirror.Frame(); !strings.Contains(frame, wantUTF8) {
		t.Fatalf("UTF-8 continuation after resize was not reconstructed: %q", frame)
	}

	edMirror := newScreenMirror(10, 4)
	defer edMirror.Close()
	edMirror.Write([]byte("\x1b[2")) // prefix of mirror-rewritten ED2
	edMirror.Resize(20, 4)
	edMirror.mu.Lock()
	edCarry := append([]byte(nil), edMirror.edCarry...)
	edMirror.mu.Unlock()
	if string(edCarry) != "\x1b[2" {
		t.Fatalf("resize consumed incomplete ED carry: %q", edCarry)
	}
}

// Придерживать хвост можно только разумно долго: двоичный мусор без границ
// кластеров не должен копиться в буфере вечно.
func TestSplitAtClusterBoundaryDoesNotHoardGarbage(t *testing.T) {
	junk := make([]byte, 200)
	for i := range junk {
		junk[i] = 0xFF
	}
	feed, tail := splitAtClusterBoundary(junk)
	if len(tail) != 0 || len(feed) != len(junk) {
		t.Fatalf("мусор придержан: feed=%d tail=%d", len(feed), len(tail))
	}
	// А обрезанный символ — придержан.
	feed, tail = splitAtClusterBoundary([]byte("abc\xd0"))
	if string(feed) != "abc" || len(tail) != 1 {
		t.Fatalf("обрезанный символ не придержан: feed=%q tail=%q", feed, tail)
	}
}

// Геометрия зеркала — это память. Клиент присылает свой размер, и доверять ему
// без границы нельзя.
func TestScreenMirrorClampsGeometry(t *testing.T) {
	m := newScreenMirror(100000, -5)
	defer m.Close()
	cols, rows := m.Size()
	if cols != screenMirrorMaxCols || rows != 24 {
		t.Fatalf("геометрия не зажата: %dx%d", cols, rows)
	}
	m.Resize(48, 30)
	if cols, rows = m.Size(); cols != 48 || rows != 30 {
		t.Fatalf("Resize не применился: %dx%d", cols, rows)
	}
}

// Пустые строки в кадр не пишем: экран уже очищен, а мобильный канал дорогой.
func TestScreenMirrorFrameSkipsEmptyLines(t *testing.T) {
	m := newScreenMirror(48, 30)
	defer m.Close()
	m.Write([]byte("\x1b[H\x1b[2J\x1b[15;5HODNA STROKA"))
	f := m.Frame()
	if n := strings.Count(f, ";1H"); n != 1 {
		t.Fatalf("в кадре %d адресаций строк, ожидалась одна:\n%q", n, f)
	}
}

// Пишет lines строк через "\r\n" — ровно так печатает обычная оболочка.
func feedLines(m *screenMirror, lines int) {
	var sb strings.Builder
	for i := 1; i <= lines; i++ {
		fmt.Fprintf(&sb, "STROKA %02d", i)
		if i < lines {
			sb.WriteString("\r\n")
		}
	}
	m.Write([]byte(sb.String()))
}

// История зеркала — scrollback ПЛЮС видимый экран (шов с кадром, см.
// historyLocked): строки старая → новая, разделитель "\r\n" после каждой,
// КРОМЕ последней видимой — иначе запись добавляет лишнюю прокрутку, и первая
// видимая строка дублируется в scrollback клиента (замер на xterm.js).
func TestScreenMirrorHistoryKeepsOrder(t *testing.T) {
	m := newScreenMirror(40, 10)
	defer m.Close()
	feedLines(m, 25) // 25 строк на экране из 10: scrollback 01..15, экран 16..25

	hist, n := m.History(100)
	if n != 25 {
		t.Fatalf("в истории %d строк, ожидалось 25 (15 scrollback + 10 видимых): %q", n, hist)
	}
	if strings.HasSuffix(hist, "\r\n") {
		t.Fatalf("после последней видимой строки разделителя быть не должно: %q", hist)
	}
	lines := strings.Split(strings.TrimPrefix(hist, "\x1b[m"), "\r\n")
	if len(lines) != 25 {
		t.Fatalf("строк в тексте истории %d, ожидалось 25: %q", len(lines), hist)
	}
	for i := 0; i < 25; i++ {
		want := fmt.Sprintf("STROKA %02d", i+1)
		if !strings.HasSuffix(lines[i], want) {
			t.Fatalf("строка истории %d = %q, ожидалась %q — порядок старая→новая нарушен", i, lines[i], want)
		}
	}
}

// Пустая строка в истории — тоже строка: печать последовательная со счётом
// прокруток, и её пропуск сдвинул бы всё, что ниже, и украл бы строку у
// scrollback клиента.
func TestScreenMirrorHistoryKeepsEmptyLines(t *testing.T) {
	m := newScreenMirror(40, 5)
	defer m.Close()
	m.Write([]byte("V1\r\nV2\r\n\r\n\r\nV5\r\nV6\r\nV7\r\nV8\r\nV9\r\nV10"))

	hist, n := m.History(100)
	if n != 10 {
		t.Fatalf("в истории %d строк, ожидалось 10 (5 scrollback + 5 видимых): %q", n, hist)
	}
	lines := strings.Split(strings.TrimPrefix(hist, "\x1b[m"), "\r\n")
	want := []string{"V1", "V2", "", "", "V5", "V6", "V7", "V8", "V9", "V10"}
	if strings.Join(lines, "|") != strings.Join(want, "|") {
		t.Fatalf("история = %q, ожидалось %q — пустые строки потеряны", lines, want)
	}
}

// У alt-screen нет истории — она принадлежит обычному буферу. Пока приложение
// в альтернативном экране, отдаём пусто: иначе над полноэкранным TUI клиент
// показал бы строки, которые к нему не относятся.
func TestScreenMirrorHistoryEmptyOnAltScreen(t *testing.T) {
	m := newScreenMirror(40, 10)
	defer m.Close()
	feedLines(m, 25)

	m.Write([]byte("\x1b[?1049h"))
	if hist, n := m.History(100); hist != "" || n != 0 {
		t.Fatalf("alt-screen отдал историю: %d строк %q", n, hist)
	}
	// Вышли из alt — история обычного буфера обязана вернуться.
	m.Write([]byte("\x1b[?1049l"))
	if _, n := m.History(100); n != 25 {
		t.Fatalf("после выхода из alt-screen история потеряна: %d строк, ожидалось 25", n)
	}
}

// maxLines ограничивает только scrollback-часть: просят меньше — отдаём
// ПОСЛЕДНИЕ maxLines в том же порядке; видимые строки идут всегда целиком.
func TestScreenMirrorHistoryTrimsToMaxLines(t *testing.T) {
	m := newScreenMirror(40, 10)
	defer m.Close()
	feedLines(m, 25)

	hist, n := m.History(5)
	if n != 15 {
		t.Fatalf("в истории %d строк, ожидалось 15 (5 scrollback + 10 видимых): %q", n, hist)
	}
	if strings.Contains(hist, "STROKA 10") || !strings.Contains(hist, "STROKA 11") {
		t.Fatalf("обрезка обязана оставлять последние строки scrollback (11..15): %q", hist)
	}
	if !strings.Contains(hist, "STROKA 15") || !strings.Contains(hist, "STROKA 25") {
		t.Fatalf("видимые строки обязаны идти целиком (15..25): %q", hist)
	}
}

// ИЗВЕСТНОЕ РАСХОЖДЕНИЕ БИБЛИОТЕКИ (не наше): строка, ушедшая из ОБЛАСТИ
// прокрутки (DECSTBM) или удалённая DL на полном экране, попадает в scrollback
// vt, хотя xterm.js (и настоящий xterm) копят только строки, ушедшие за верх
// ПОЛНОГО экрана. Виноват Screen.DeleteLine в charmbracelet/x/vt: условие
// сохранения «курсор на верхней строке области и область во всю ширину» не
// требует, чтобы область начиналась с верха экрана, а DL от ScrollUp не
// отличается от явного CSI DL. Замер 13.08.2026 на живом xterm.js:
// прокрутка региона 2..10 — эталон 10 строк истории, vt 11.
//
// Тест ПИНИТ текущее поведение, чтобы обновление библиотеки не прошло молча:
// упал — проверь, не починили ли апстрим; починили — сними освобождение
// случая «scrolling» в build/qa/probe-screen-frame.mjs и этот тест.
func TestScreenMirrorHistoryRegionScrollLeakIsKnown(t *testing.T) {
	m := newScreenMirror(48, 30)
	defer m.Close()
	var sb strings.Builder
	for i := 1; i <= 40; i++ {
		fmt.Fprintf(&sb, "LINE %d", i)
		if i < 40 {
			sb.WriteString("\r\n")
		}
	}
	// Область 2..10 и прокрутка внутри неё: ушедшая из области строка обязана
	// быть выброшена, а не сохранена в историю — но vt сохраняет.
	sb.WriteString("\x1b[2;10r\x1b[10;1HIN REGION\r\nNEXT\x1b[r")
	m.Write([]byte(sb.String()))

	// 11 строк scrollback (10 честных + 1 утёкшая) + 30 видимых.
	_, n := m.History(100)
	if n != 41 {
		t.Fatalf("поведение vt изменилось: в истории %d строк, при задокументированной течи ожидалось 41", n)
	}
}

// C-08 (ST-05, T-27): снимок на разрезе внутри незавершённой CSI/OSC/DCS,
// UTF-8, одиночного ESC или префикса ED2 обязан быть withheld — и в режиме
// idle тоже: долив придержанного хвоста по тишине не делает незаконченную
// управляющую строку законченной. После продолжения кадр готов, и его база —
// ровно конец потока.
func TestSnapshotsFromStreamWithholdsInsideSequences(t *testing.T) {
	cases := []struct{ name, prefix, suffix string }{
		{"CSI", "HELLO\x1b[38;5", "mOK"},
		{"OSC", "HELLO\x1b]0;unfinished", "\x07OK"},
		{"DCS", "HELLO\x1bP1;2|unfinished", "\x1b\\OK"},
		{"UTF8", "HELLO\xf0\x9f", "\x98\x80OK"},
		{"ESC", "HELLO\x1b", "[1mOK"},
		{"ED2-prefix", "HELLO\x1b[2", "JOK"},
	}
	for _, tt := range cases {
		for _, idle := range []bool{false, true} {
			data := []byte(tt.prefix + tt.suffix)
			snaps := SnapshotsFromStream(data, 20, 4, []int{3}, []int{len(tt.prefix), len(data)}, 100, idle)
			if len(snaps) != 2 {
				t.Fatalf("%s idle=%v: %d снимков вместо 2", tt.name, idle, len(snaps))
			}
			if s := snaps[0]; s.Ready || s.Frame != "" || s.History != "" || s.AppliedOff != 0 {
				t.Fatalf("%s idle=%v: снимок посреди последовательности не withheld: %+v", tt.name, idle, s)
			}
			if s := snaps[1]; !s.Ready || s.AppliedOff != uint64(len(data)) || !strings.Contains(s.Frame, "OK") {
				t.Fatalf("%s idle=%v: после продолжения кадр не готов или база не та: %+v", tt.name, idle, s)
			}
		}
	}
}

// C-08: на КАЖДОМ готовом разрезе база кадра равна разрезу — при любом плане
// кусков и в обоих режимах тишины. Иначе клиент отбросил бы или задвоил
// хвост после кадра (base_offset — позиция сразу после применённого байта).
func TestSnapshotsFromStreamAppliedOffEqualsCut(t *testing.T) {
	data := []byte("ab\x1b[31mcd\r\nж€\U0001D400é中\x1b]0;t\x07\x1bP1|x\x1b\\\x1b[2J\x1b[Hend\U0001F600z")
	cuts := make([]int, 0, len(data)+1)
	for c := 0; c <= len(data); c++ {
		cuts = append(cuts, c)
	}
	// Положительный контроль: разрезы на заведомо безопасной границе (начало,
	// после простого текста, после CRLF) обязаны быть готовы — стенд не
	// «зелёный потому, что всё withheld».
	mustReady := map[int]bool{0: true, 2: true, strings.Index(string(data), "cd\r\n") + 4: true}
	for _, plan := range [][]int{nil, {1}, {3, 5}} {
		for _, idle := range []bool{false, true} {
			ready := 0
			for _, s := range SnapshotsFromStream(data, 20, 4, plan, cuts, 100, idle) {
				if mustReady[s.Cut] && !s.Ready {
					t.Fatalf("plan=%v idle=%v cut=%d: снимок на безопасной границе withheld", plan, idle, s.Cut)
				}
				if !s.Ready {
					if s.Frame != "" || s.AppliedOff != 0 {
						t.Fatalf("plan=%v idle=%v cut=%d: withheld снимок несёт кадр или базу: %+v", plan, idle, s.Cut, s)
					}
					continue
				}
				ready++
				if s.AppliedOff != uint64(s.Cut) {
					t.Fatalf("plan=%v idle=%v cut=%d: база кадра %d не равна разрезу", plan, idle, s.Cut, s.AppliedOff)
				}
				if s.Cols != 20 || s.Rows != 4 {
					t.Fatalf("plan=%v idle=%v cut=%d: геометрия %dx%d", plan, idle, s.Cut, s.Cols, s.Rows)
				}
			}
			if ready < len(mustReady) {
				t.Fatalf("plan=%v idle=%v: готовых снимков %d из %d", plan, idle, ready, len(cuts))
			}
		}
	}
}

// C-08: придержанная графема (эмодзи или буква с комбинирующим знаком в конце)
// без idle держит снимок, с idle доливается детерминированно — без часов.
func TestSnapshotsFromStreamIdleFlushesHeldGrapheme(t *testing.T) {
	for _, tail := range []string{"\U0001F600", "é"} {
		data := []byte("A" + tail)
		held := SnapshotsFromStream(data, 20, 4, nil, []int{len(data)}, 100, false)[0]
		if held.Ready {
			t.Fatalf("%q: без тишины придержанная графема не держит снимок: %+v", tail, held)
		}
		flushed := SnapshotsFromStream(data, 20, 4, nil, []int{len(data)}, 100, true)[0]
		// Кадр отдаёт графему в NFC (composeForMirror, 14.09): NFD «é» зеркало
		// собирает в один символ — на экране тот же.
		if !flushed.Ready || !strings.Contains(flushed.Frame, string(composeForMirror([]byte(tail)))) || flushed.AppliedOff != uint64(len(data)) {
			t.Fatalf("%q: idle не долил графему: %+v", tail, flushed)
		}
	}
}

// C-08: свежее зеркало на разрезе даёт РОВНО то, что живое зеркало,
// кормленное тем же планом кусков непрерывно и снятое на той же границе.
// Иначе стенд проверял бы не производственный путь, а свою копию.
func TestSnapshotsFromStreamMatchesLiveMirrorAtChunkBoundaries(t *testing.T) {
	var sb strings.Builder
	for i := 1; i <= 12; i++ {
		fmt.Fprintf(&sb, "line %d \x1b[3%dmcol\x1b[m\r\n", i, i%8)
	}
	sb.WriteString("\x1b]0;title\x07é\U0001F600‍\U0001F469 wide 中\x1b[2J\x1b[3;3Hend")
	data := []byte(sb.String())
	plan := []int{3, 5}

	live := newScreenMirror(20, 4)
	defer live.Close()
	snapLive := func(cut int) StreamSnapshot {
		live.mu.Lock()
		live.pendingAt = time.Now().Add(time.Hour)
		live.mu.Unlock()
		f, h, hl, c, r, a := live.SnapshotAt(100)
		s := StreamSnapshot{Cut: cut, Ready: f != "", Frame: f, History: h, HistLines: hl, Cols: c, Rows: r, AppliedOff: a}
		if s.Ready {
			live.mu.Lock()
			s.Grid = streamGridLocked(live)
			s.Scrollback = streamScrollbackLocked(live, 100)
			s.ScrollbackCells = streamScrollbackCellsLocked(live, 100)
			pos := live.em.CursorPosition()
			s.CursorX, s.CursorY, s.Alt = pos.X, pos.Y, live.em.IsAltScreen()
			live.mu.Unlock()
		}
		return s
	}
	boundaries := []int{0}
	want := []StreamSnapshot{snapLive(0)}
	for off, i := 0, 0; off < len(data); i++ {
		n := plan[i%len(plan)]
		if n > len(data)-off {
			n = len(data) - off
		}
		live.WriteAt(data[off:off+n], uint64(off+n))
		off += n
		boundaries = append(boundaries, off)
		want = append(want, snapLive(off))
	}
	got := SnapshotsFromStream(data, 20, 4, plan, boundaries, 100, false)
	if len(got) != len(want) {
		t.Fatalf("снимков %d, ожидалось %d", len(got), len(want))
	}
	ready := 0
	for i := range want {
		if !reflect.DeepEqual(got[i], want[i]) {
			t.Fatalf("разрез %d: свежее зеркало разошлось с живым:\n got  %+v\n want %+v", want[i].Cut, got[i], want[i])
		}
		if got[i].Ready {
			ready++
		}
	}
	if ready == 0 {
		t.Fatal("ни одного готового снимка — сравнение пустое")
	}
}

// §6 (resize): смена геометрии в стенде — ровно продуктовая последовательность.
// Сессия получает вывод и маркер resize через одну FIFO (feedScreenAt,
// resizeScreen — модель упорядоченного ACK), и её кадр на каждом шаге обязан
// совпасть с тем, что SnapshotsFromStreamResized даёт на том же разрезе: до
// маркера (AfterCut при разрезе ровно на нём), после маркера и в конце потока
// после второго resize. Иначе стенд сравнивал бы с xterm не продукт, а копию.
func TestSnapshotsFromStreamResizedMatchesSessionFIFO(t *testing.T) {
	s := &Session{epoch: "e1", subs: map[chan []byte]*subState{}, dec: newDecTracker()}
	s.startScreen(20, 6)
	defer s.stopScreen()
	push := func(p []byte) {
		s.bufMu.Lock()
		s.buf = append(s.buf, p...)
		s.totalBytes += uint64(len(p))
		off := s.totalBytes
		s.bufMu.Unlock()
		s.feedScreenAt(p, off)
		waitScreenIdle(t, s)
	}
	type snap struct {
		frame, history      string
		histLines, cols, rw int
		off                 uint64
	}
	take := func() snap {
		f, h, hl, c, r, off := s.ScreenFrameAt()
		if f == "" {
			t.Fatal("сессия не отдала кадр")
		}
		return snap{f, h, hl, c, r, off}
	}
	partA := []byte("L1 first line is wide\r\nL2\r\nL3 \x1b[31mred\x1b[m\r\nL4\r\nL5\r\nL6 end")
	partB := []byte("\r\nafter-shrink 0123456789\x1b[2;3HX")
	partC := []byte("\x1b[?1049h\x1b[Halt screen\x1b[5;7HY")
	data := append(append(append([]byte(nil), partA...), partB...), partC...)
	a, b := len(partA), len(partA)+len(partB)

	push(partA)
	beforeShrink := take()
	s.resizeScreen(10, 4)
	afterShrink := take()
	push(partB)
	s.resizeScreen(30, 8)
	push(partC)
	end := take()

	if beforeShrink.cols != 20 || afterShrink.cols != 10 || afterShrink.rw != 4 || end.cols != 30 || end.rw != 8 {
		t.Fatalf("геометрия кадров сессии %dx%d → %dx%d → %dx%d", beforeShrink.cols, beforeShrink.rw, afterShrink.cols, afterShrink.rw, end.cols, end.rw)
	}
	if beforeShrink.frame == afterShrink.frame {
		t.Fatal("resize не изменил кадр — сравнение ничего не доказывает")
	}
	stand := func(cut int, shrinkAfterCut bool) snap {
		rs := []StreamResize{{Off: a, Cols: 10, Rows: 4, AfterCut: shrinkAfterCut}, {Off: b, Cols: 30, Rows: 8}}
		got := SnapshotsFromStreamResized(data, 20, 6, nil, []int{cut}, -1, false, rs)[0]
		if !got.Ready {
			t.Fatalf("стенд withheld на разрезе %d", cut)
		}
		return snap{got.Frame, got.History, got.HistLines, got.Cols, got.Rows, got.AppliedOff}
	}
	for _, tc := range []struct {
		name string
		got  snap
		want snap
	}{
		{"разрез до resize (AfterCut)", stand(a, true), beforeShrink},
		{"разрез после resize", stand(a, false), afterShrink},
		{"конец потока после второго resize", stand(len(data), false), end},
	} {
		if !reflect.DeepEqual(tc.got, tc.want) {
			t.Fatalf("%s: стенд разошёлся с сессией:\n got  %+v\n want %+v", tc.name, tc.got, tc.want)
		}
	}
}

// §6 (resize): план кусков режется на позиции маркера, и свежее зеркало на
// каждой границе — ровно живое зеркало, кормленное тем же WriteAt и тем же
// Resize в той же точке (как TestSnapshotsFromStreamMatchesLiveMirrorAtChunkBoundaries).
func TestSnapshotsFromStreamResizedMatchesLiveMirror(t *testing.T) {
	var sb strings.Builder
	for i := 1; i <= 9; i++ {
		fmt.Fprintf(&sb, "row %d \x1b[3%dmcolor\x1b[m wide wide\r\n", i, i%8)
	}
	sb.WriteString("é中\x1b[2;4Hmid\x1b[?1049h\x1b[3;3Halt\x1b[?1049lback")
	data := []byte(sb.String())
	plan := []int{3, 5}
	resizes := []StreamResize{{Off: 1, Cols: 12, Rows: 4}, {Off: 40, Cols: 32, Rows: 8}, {Off: 41, Cols: 16, Rows: 3}, {Off: len(data) - 10, Cols: 24, Rows: 6}}

	live := newScreenMirror(20, 6)
	defer live.Close()
	snapLive := func(cut int) StreamSnapshot {
		live.mu.Lock()
		live.pendingAt = time.Now().Add(time.Hour)
		live.mu.Unlock()
		f, h, hl, c, r, a := live.SnapshotAt(100)
		s := StreamSnapshot{Cut: cut, Ready: f != "", Frame: f, History: h, HistLines: hl, Cols: c, Rows: r, AppliedOff: a}
		if s.Ready {
			live.mu.Lock()
			s.Grid = streamGridLocked(live)
			s.Scrollback = streamScrollbackLocked(live, 100)
			s.ScrollbackCells = streamScrollbackCellsLocked(live, 100)
			pos := live.em.CursorPosition()
			s.CursorX, s.CursorY, s.Alt = pos.X, pos.Y, live.em.IsAltScreen()
			live.mu.Unlock()
		}
		return s
	}
	var boundaries []int
	var want []StreamSnapshot
	ri := 0
	for off, i := 0, 0; ; i++ {
		for ri < len(resizes) && resizes[ri].Off <= off {
			live.Resize(resizes[ri].Cols, resizes[ri].Rows)
			ri++
		}
		boundaries = append(boundaries, off)
		want = append(want, snapLive(off))
		if off >= len(data) {
			break
		}
		n := plan[i%len(plan)]
		if n > len(data)-off {
			n = len(data) - off
		}
		if ri < len(resizes) && resizes[ri].Off < off+n {
			n = resizes[ri].Off - off
		}
		live.WriteAt(data[off:off+n], uint64(off+n))
		off += n
	}
	got := SnapshotsFromStreamResized(data, 20, 6, plan, boundaries, 100, false, resizes)
	geoms := map[string]bool{}
	for i := range want {
		if !reflect.DeepEqual(got[i], want[i]) {
			t.Fatalf("разрез %d: свежее зеркало с resize разошлось с живым:\n got  %+v\n want %+v", want[i].Cut, got[i], want[i])
		}
		if got[i].Ready {
			geoms[fmt.Sprintf("%dx%d", got[i].Cols, got[i].Rows)] = true
		}
	}
	if len(geoms) < 4 {
		t.Fatalf("готовые снимки прошли только геометрии %v — resize до зеркала не дошёл", geoms)
	}
	// Ничья на позиции маркера: AfterCut — снимок ДО resize, без него — после.
	// Разрез — сразу после первого CRLF: граница, где снимок заведомо готов.
	at := strings.Index(sb.String(), "\r\n") + 2
	before := SnapshotsFromStreamResized(data, 20, 6, nil, []int{at}, 100, true, []StreamResize{{Off: at, Cols: 32, Rows: 8, AfterCut: true}})[0]
	after := SnapshotsFromStreamResized(data, 20, 6, nil, []int{at}, 100, true, []StreamResize{{Off: at, Cols: 32, Rows: 8}})[0]
	if !before.Ready || !after.Ready || before.Cols != 20 || before.Rows != 6 || after.Cols != 32 || after.Rows != 8 {
		t.Fatalf("ничья на маркере: до %dx%d ready=%v, после %dx%d ready=%v", before.Cols, before.Rows, before.Ready, after.Cols, after.Rows, after.Ready)
	}
	// Маркер за разрезом зеркала не касается.
	if far := SnapshotsFromStreamResized(data, 20, 6, nil, []int{at - 1}, 100, true, []StreamResize{{Off: at, Cols: 32, Rows: 8}})[0]; far.Cols != 20 || far.Rows != 6 {
		t.Fatalf("resize за разрезом применился: %dx%d", far.Cols, far.Rows)
	}
}

// Широкая графема, которой после сужения не хватает места до края (vt и xterm
// оставляют её в последней колонке без второй половины), в кадр не идёт:
// клиент перенёс бы её на следующую строку, а пустую строку кадр пропускает —
// там она оставалась мусором (стенд §6, seed 20261123, frame-wide-last-column).
func TestScreenFrameSkipsWideGraphemeCutAtRightEdge(t *testing.T) {
	m := newScreenMirror(20, 6)
	defer m.Close()
	m.Write([]byte("abcdefghijk中"))
	m.Resize(12, 6)
	m.mu.Lock()
	c := m.em.CellAt(11, 0)
	m.mu.Unlock()
	if c == nil || c.Content != "中" || c.Width != 2 {
		t.Fatalf("предпосылка: vt держит 中 в последней колонке, получили %+v", c)
	}
	f := m.Frame()
	if strings.Contains(f, "中") {
		t.Fatalf("кадр печатает графему, которой нет места: %q", f)
	}
	dst := newScreenMirror(12, 6)
	defer dst.Close()
	dst.Write([]byte(f))
	dst.mu.Lock()
	defer dst.mu.Unlock()
	var row0 strings.Builder
	for x := 0; x < 12; x++ {
		if c := dst.em.CellAt(x, 0); c != nil {
			row0.WriteString(c.Content)
		}
		if c := dst.em.CellAt(x, 1); c != nil && c.Content != "" && c.Content != " " {
			t.Fatalf("кадр испортил следующую строку: %q в колонке %d", c.Content, x)
		}
	}
	if got := strings.TrimRight(row0.String(), " "); got != "abcdefghijk" {
		t.Fatalf("строка кадра %q, ожидалось abcdefghijk", got)
	}
}

// vtPanicTriggers — входы, на которых библиотека vt паникует (index out of
// range в vt/ultraviolet): P1 и P2 нашёл стенд эквивалентности §6 (15.09.2026)
// на входе, который обычное приложение шлёт в гонке со сменой геометрии, P3 и
// P4 — разбор того же рода (screen_vtpanic.go, шапка). Зеркало кормится из
// горутины sessionScreen, и до 15.09 паника там роняла ВЕСЬ процесс агента.
var vtPanicTriggers = []struct {
	name       string
	cols, rows int
	run        func(m *screenMirror)
}{
	{"P1 DECSTBM 2;6 на четырёх строках + DL", 20, 4, func(m *screenMirror) {
		m.Write([]byte("\x1b[2;6r\x1b[3;1H\x1b[M"))
	}},
	{"P1 область под прежнюю высоту после Resize 6→4 + LF", 20, 6, func(m *screenMirror) {
		m.Write([]byte("x"))
		m.Resize(20, 4)
		m.Write([]byte("\x1b[1;6r\x1b[4;1H\n\n\n"))
	}},
	{"P2 Resize в alt 20→12, ?1049l, HTS", 20, 6, func(m *screenMirror) {
		m.Write([]byte("\x1b[1;19H\x1b[?1049h"))
		m.Resize(12, 6)
		m.Write([]byte("\x1b[?1049l\x1bH"))
	}},
	{"P3 DECSC у края, Resize 20→12, DECRC, HTS", 20, 6, func(m *screenMirror) {
		m.Write([]byte("\x1b[1;19H\x1b7"))
		m.Resize(12, 6)
		m.Write([]byte("\x1b8\x1bH"))
	}},
	{"P3 DECSC у края, Resize 20→12, DECRC, TBC 0", 20, 6, func(m *screenMirror) {
		m.Write([]byte("\x1b[1;19H\x1b7"))
		m.Resize(12, 6)
		m.Write([]byte("\x1b8\x1b[g"))
	}},
	// Восьмибитный HTS идёт мимо обработчиков vt (закрытый обработчик
	// управляющего символа) — его не пускает до vt dropStrayC1 (скептик волны 4).
	{"P3 DECSC у края, Resize 20→12, DECRC, HTS восьмибитный (0x88)", 20, 6, func(m *screenMirror) {
		m.Write([]byte("\x1b[1;19H\x1b7"))
		m.Resize(12, 6)
		m.Write([]byte("\x1b8\x88x"))
	}},
	{"P4 DECSLRM с правым полем за шириной + ICH/DCH", 20, 6, func(m *screenMirror) {
		m.Write([]byte("\x1b[?69h\x1b[1;30s\x1b[1;25H\x1b[3@\x1b[3P"))
	}},
}

// Паники vt не роняют зеркало. Два слоя (screen_vtpanic.go), и тест проверяет
// оба на одних и тех же входах:
//   - с предохранителями (как в продукте) паники нет вовсе, кадр есть;
//   - без предохранителей (noVtGuards) библиотека паникует по-настоящему, и
//     паника остаётся внутри зеркала: кадра и истории нет до RIS, после RIS —
//     обычный кадр.
//
// До 15.09 здесь был пин TestScreenMirrorVtPanicsAreKnown («паника есть»), а
// генератор C-03R обходил оба входа (terminalConformance.ts); обходы сняты.
func TestScreenMirrorVtPanicsDoNotKillMirror(t *testing.T) {
	for _, tc := range vtPanicTriggers {
		for _, guards := range []bool{true, false} {
			label := fmt.Sprintf("%s, предохранители %v", tc.name, guards)
			m := newScreenMirror(tc.cols, tc.rows)
			m.noVtGuards = !guards
			msg := func() (msg string) {
				defer func() {
					if r := recover(); r != nil {
						msg = fmt.Sprint(r)
					}
				}()
				tc.run(m)
				return ""
			}()
			if msg != "" {
				m.Close()
				t.Fatalf("%s: паника vt вырвалась из зеркала: %s", label, msg)
			}
			m.mu.Lock()
			panics, untrusted := m.vtPanics, m.untrusted
			m.mu.Unlock()
			if guards {
				f := m.Frame()
				m.Close()
				if panics != 0 || untrusted || f == "" {
					t.Fatalf("%s: предохранитель не сработал: паник %d, untrusted %v, кадр %q", label, panics, untrusted, f)
				}
				continue
			}
			if panics == 0 || !untrusted {
				m.Close()
				t.Fatalf("%s: без предохранителей паники нет (%d) — вход больше не проверяет сетку", label, panics)
			}
			frame := m.Frame()
			hist, n := m.History(100)
			snap, _, _, _, _, _ := m.SnapshotAt(100)
			m.Write([]byte("\x1bcAFTER"))
			after := m.Frame()
			m.Close()
			if frame != "" || snap != "" || hist != "" || n != 0 {
				t.Fatalf("%s: до RIS зеркало отдало кадр %q / снимок %q / историю %d строк", label, frame, snap, n)
			}
			if !strings.Contains(after, "AFTER") {
				t.Fatalf("%s: после RIS нет кадра: %q", label, after)
			}
		}
	}
	// Контроль: без resize вход P2 не падает и без предохранителей — дело в resize.
	m := newScreenMirror(20, 6)
	defer m.Close()
	m.noVtGuards = true
	m.Write([]byte("\x1b[1;19H\x1b[?1049h\x1b[?1049l\x1bH"))
	if m.vtPanics != 0 {
		t.Fatalf("контроль P2 без resize запаниковал: %d", m.vtPanics)
	}
}

// ST-05 «дёшево, силами зеркала»: кадр переносит ?25, ?1, ?66, ?6, ?7 и IRM.
// Проверка — сквозная: кадр, влитый в ЧИСТОЕ зеркало, воспроизводит режимы
// источника, а влитый в ГРЯЗНОЕ (все режимы наоборот) — тоже; позиция курсора
// остаётся последней последовательностью кадра.
// Сетка и курсор самого зеркала в снимке (стенд C-03, правило «клиент = xterm
// ИЛИ зеркало»): форма activeGrid — пустая клетка " ", продолжение широкой
// графемы ""; у withheld-снимка сетки нет.
func TestSnapshotsFromStreamCarriesMirrorGrid(t *testing.T) {
	data := []byte("ab\x1b[2;3H中x\x1b[")
	snaps := SnapshotsFromStream(data, 6, 3, nil, []int{len(data) - 2, len(data)}, -1, true)
	got, withheld := snaps[0], snaps[1]
	if !got.Ready || withheld.Ready {
		t.Fatalf("ready: %v/%v, ожидалось true/false", got.Ready, withheld.Ready)
	}
	want := [][]string{
		{"a", "b", " ", " ", " ", " "},
		{" ", " ", "中", "", "x", " "},
		{" ", " ", " ", " ", " ", " "},
	}
	if fmt.Sprint(got.Grid) != fmt.Sprint(want) || len(got.Grid) != 3 || len(got.Grid[1]) != 6 {
		t.Fatalf("сетка зеркала %q, ожидалось %q", got.Grid, want)
	}
	if got.CursorX != 5 || got.CursorY != 1 || got.Alt {
		t.Fatalf("курсор (%d,%d) alt=%v, ожидалось (5,1) alt=false", got.CursorX, got.CursorY, got.Alt)
	}
	if withheld.Grid != nil {
		t.Fatalf("у withheld-снимка сетки быть не должно: %q", withheld.Grid)
	}
	alt := SnapshotsFromStream([]byte("\x1b[?1049hZ"), 6, 3, nil, []int{9}, -1, true)[0]
	if !alt.Alt || alt.Grid[0][0] != "Z" || alt.Scrollback != nil {
		t.Fatalf("alt-экран: alt=%v сетка %q история %q", alt.Alt, alt.Grid, alt.Scrollback)
	}
	// История — ровно те строки, что ушли в снимок: без хвостовых пробелов,
	// широкая графема одной строкой, потолок maxHistory как у historyLocked.
	hist := []byte("a1  \r\nb中\r\nc3\r\nd4\r\ne5")
	full := SnapshotsFromStream(hist, 6, 3, nil, []int{len(hist)}, -1, true)[0]
	if fmt.Sprint(full.Scrollback) != fmt.Sprint([]string{"a1", "b中"}) || len(full.Scrollback) != 2 {
		t.Fatalf("история зеркала %q, ожидалось [a1 b中]", full.Scrollback)
	}
	// Ширина строк истории у клиента: хвостовые пробелы без стиля не в счёт,
	// широкая графема — две колонки, пробел с фоном — в счёт.
	if fmt.Sprint(full.ScrollbackCells) != "[2 3]" {
		t.Fatalf("ширина строк истории %v, ожидалось [2 3]", full.ScrollbackCells)
	}
	bg := []byte("\x1b[42mab\x1b[K\r\n\x1b[mc\r\nd\r\ne\r\nf")
	if cells := SnapshotsFromStream(bg, 6, 3, nil, []int{len(bg)}, -1, true)[0].ScrollbackCells; fmt.Sprint(cells) != "[6 1]" {
		t.Fatalf("строка с фоном до края: ширина %v, ожидалось [6 1]", cells)
	}
	if capped := SnapshotsFromStream(hist, 6, 3, nil, []int{len(hist)}, 1, true)[0]; fmt.Sprint(capped.Scrollback) != "[b中]" {
		t.Fatalf("история с потолком 1: %q", capped.Scrollback)
	}
}

func TestScreenFrameCarriesMirrorModes(t *testing.T) {
	modesOf := func(m *screenMirror) mirrorModes {
		m.mu.Lock()
		defer m.mu.Unlock()
		return m.modes
	}
	for _, tc := range []struct {
		name   string
		stream string
		want   mirrorModes
	}{
		{"defaults", "text", mirrorModes{}},
		{"all-set", "\x1b[?25l\x1b[?1h\x1b=\x1b[?7l\x1b[4h\x1b[?6htext",
			mirrorModes{cursorHidden: true, cursorKeys: true, keypad: true, origin: true, noAutoWrap: true, insert: true}},
		{"keypad-by-decset", "\x1b[?66htext", mirrorModes{keypad: true}},
		{"set-then-reset", "\x1b[?25l\x1b[?1h\x1b=\x1b[?7l\x1b[4h\x1b[?6h\x1b[?25h\x1b[?1l\x1b>\x1b[?7h\x1b[4l\x1b[?6ltext", mirrorModes{}},
		// RIS в одном куске с режимом до и после: порядок байтов важен, и IRM
		// (которого нет в умолчаниях vt.resetModes) обязан сброситься тоже.
		{"ris-after", "\x1b[4h\x1b[?25l\x1b[?1h\x1bc", mirrorModes{}},
		{"ris-before", "\x1bc\x1b[4h\x1b[?25l", mirrorModes{insert: true, cursorHidden: true}},
		// CSI ? 4 h — это DECSCLM, а не IRM: путать нельзя.
		{"decsclm-is-not-irm", "\x1b[?4htext", mirrorModes{}},
		// DECSTR (CSI ! p): xterm.js softReset сбрасывает все шесть режимов
		// трекера; vt его не знает. Без сброса кадр вернул бы клиенту скрытый
		// курсор и вставку (ревью C-conformance, is2 у `tput init`).
		{"decstr", "\x1b[?1h\x1b[4h\x1b[?25l\x1b[?7l\x1b=\x1b[?6h\x1b[!ptext", mirrorModes{}},
		{"decstr-then-set", "\x1b[?25l\x1b[4h\x1b[!p\x1b[?1h\x1b[4h", mirrorModes{cursorKeys: true, insert: true}},
		// Промежуточный байт обязателен: CSI p без '!' — не DECSTR.
		{"csi-p-is-not-decstr", "\x1b[?25l\x1b[4h\x1b[p", mirrorModes{cursorHidden: true, insert: true}},
	} {
		src := newScreenMirror(20, 4)
		src.Write([]byte(tc.stream))
		if got := modesOf(src); got != tc.want {
			t.Fatalf("%s: трекер зеркала %+v, ожидалось %+v", tc.name, got, tc.want)
		}
		frame := src.Frame()
		src.Close()
		if i := strings.LastIndex(frame, "\x1b["); i < 0 || !strings.HasSuffix(frame[i:], "H") {
			t.Fatalf("%s: последней в кадре должна остаться позиция курсора: %q", tc.name, frame)
		}
		for _, dirtyPrelude := range []string{"", "\x1b[?25l\x1b[?1h\x1b=\x1b[?7l\x1b[4h\x1b[?6h", "\x1b[?25h\x1b[?1l\x1b>\x1b[?7h\x1b[4l\x1b[?6l"} {
			dst := newScreenMirror(20, 4)
			dst.Write([]byte(dirtyPrelude))
			dst.Write([]byte(frame))
			if got := modesOf(dst); got != tc.want {
				t.Fatalf("%s prelude=%q: кадр восстановил режимы %+v, ожидалось %+v", tc.name, dirtyPrelude, got, tc.want)
			}
			dst.Close()
		}
	}
}

// DECOM и IRM грязного клиента не должны портить отрисовку кадра: строки
// ложатся по абсолютным адресам и перезаписывают, а не раздвигают.
func TestScreenFrameDrawsIndependentOfClientOriginAndInsert(t *testing.T) {
	src := newScreenMirror(20, 4)
	defer src.Close()
	src.Write([]byte("\x1b[1;1HTOP\x1b[4;1HBOTTOM"))
	frame := src.Frame()
	dst := newScreenMirror(20, 4)
	defer dst.Close()
	dst.Write([]byte("junkjunk\x1b[2;3r\x1b[?6h\x1b[4h"))
	dst.Write([]byte("\x1b[r" + frame)) // регион сбрасываем: кадр его не переносит (разрыв scroll-region)
	grid := func(m *screenMirror, y int) string {
		m.mu.Lock()
		defer m.mu.Unlock()
		var sb strings.Builder
		for x := 0; x < m.cols; x++ {
			if c := m.em.CellAt(x, y); c != nil && c.Content != "" {
				sb.WriteString(c.Content)
			} else {
				sb.WriteByte(' ')
			}
		}
		return strings.TrimRight(sb.String(), " ")
	}
	if got := grid(dst, 0); got != "TOP" {
		t.Fatalf("строка 1 после кадра на грязном клиенте = %q", got)
	}
	if got := grid(dst, 3); got != "BOTTOM" {
		t.Fatalf("строка 4 после кадра на грязном клиенте = %q", got)
	}
}

// Стили в истории живут по правилам кадра: самодостаточный сброс в начале,
// диффы между ячейками, закрывающий сброс, если стиль дожил до конца.
func TestScreenMirrorHistoryKeepsStyle(t *testing.T) {
	m := newScreenMirror(40, 5)
	defer m.Close()
	var sb strings.Builder
	sb.WriteString("\x1b[38;2;46;230;176m")
	for i := 1; i <= 9; i++ {
		fmt.Fprintf(&sb, "CVET %d", i)
		if i < 9 {
			sb.WriteString("\r\n")
		}
	}
	m.Write([]byte(sb.String()))

	hist, n := m.History(100)
	if n != 9 {
		t.Fatalf("в истории %d строк, ожидалось 9 (4 scrollback + 5 видимых): %q", n, hist)
	}
	if !strings.HasPrefix(hist, "\x1b[m") {
		t.Fatalf("история не начинается со сброса стиля — ляжет на чужой фон: %q", hist)
	}
	if !strings.Contains(hist, "38;2;46;230;176") {
		t.Fatalf("цвет не дожил до истории: %q", hist)
	}
	// После последней видимой строки разделителя нет — сброс идёт сразу за ней.
	if !strings.HasSuffix(hist, "\x1b[m") || strings.HasSuffix(hist, "\r\n") {
		t.Fatalf("незакрытый стиль утечёт клиенту дальше по потоку: %q", hist)
	}
}
