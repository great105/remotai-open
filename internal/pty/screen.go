package pty

import (
	"bytes"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	ansiparser "github.com/charmbracelet/x/ansi/parser"
	"github.com/charmbracelet/x/vt"
	"golang.org/x/text/unicode/norm"
)

// Зеркало экрана сессии: что человек УВИДЕЛ БЫ, подключись он прямо сейчас.
//
// ЗАЧЕМ. Клиенту при открытии терминала уходил хвост СЫРОГО потока
// (clientReplayLimit, 512 КБ), и предполагалось, что xterm на телефоне соберёт
// из него экран. Для агентских CLI это невозможно в принципе: Claude Code,
// Codex, Gemini рисуют ДИФФОМ ПО ЯЧЕЙКАМ — перерисовывают только то, что
// изменилось с прошлого кадра. Полного кадра в хвосте не бывает вовсе, и
// увеличение буфера тут не помогает ни при каком размере.
//
// Замеры, на которых это стоит (12.08.2026, боевые буферы машины владельца,
// 12 файлов × 32 КБ): в 9 из 12 НОЛЬ полных стираний экрана; в сессии с записи
// экрана 940 курсорных адресаций задели 4 строки из 30, покрытие сетки — 59
// ячеек из 2400 (2,5 %). Прогон настоящего Ink: 30 тиков спиннера — 3470 байт
// и ноль строк истории. Документация вендора: «Fullscreen rendering sends only
// the cells that changed between frames». Подробности —
// Контекст/Журнал/2026-08-12_pustoy-ekran-eto-potok-a-nuzhno-sostoyanie.md.
//
// ГДЕ ЖИВЁТ. В агенте (Session), а НЕ в pty-host. Хост живёт своей сборкой и
// переживает обновление remotai: на машине владельца 11 живых хостов, старшему
// трое суток. Зеркало в хосте не появилось бы у существующих сессий вовсе — а
// болят именно они, и релиз выглядел бы как невыпуск.
//
// ЧЕГО ЭТО НЕ ДАЁТ. Прокрутки истории у полноэкранных приложений: у alt-screen
// нет scrollback ни в одном эмуляторе мира — это свойство протокола. И
// гиперссылок OSC 8: библиотека их искажает, поэтому в кадр они не попадают
// (текст остаётся, ссылка теряется).

// screenMirrorScrollback — сколько строк истории держит зеркало.
//
// ⚠ НЕ БРАТЬ ДЕФОЛТ БИБЛИОТЕКИ (10000). Замер: uv.Cell весит 112 байт, и на
// дефолте выходит ~78 МБ на сессию — на VPS с гигабайтом это oom-killer,
// который у нас уже был. Плюс вытеснение реализовано как slices.Delete(…, 0, 1),
// то есть O(n) memmove на КАЖДУЮ ушедшую строку: 1000 строк — 60 тыс. строк/с,
// 50000 строк — 9 тыс. строк/с. Нам история в зеркале нужна чтобы
// показать несколько экранов назад у обычной оболочки.
//
// Эти строки уходят клиенту снапшотом (History → поле history ответа на ctrl
// screen), поэтому потолок перепроверен 13.08.2026 и оставлен: библиотека
// обрезает хвостовые пустые ячейки (Push), и обычная оболочка со строками по
// десятки символов держит здесь килобайты — единицы мегабайт. Худший случай —
// наводнение полноширинными строками при 400 колонках: 500×400×112 Б ≈ 22 МБ
// поверх ~9 МБ самой сетки при той же геометрии. Это десктопный агент, а не
// VPS, и рисующее такое приложение само платит за свою печать; резать потолок
// из-за него — значит укоротить прокрутку всем ради выродка.
const screenMirrorScrollback = 500

// screenMirrorMaxCols/Rows — потолок геометрии зеркала. Клиент присылает свой
// размер, и доверять ему без границы нельзя: сетка — это память.
const (
	screenMirrorMaxCols = 400
	screenMirrorMaxRows = 200
)

// screenMirror — эмулятор терминала, которому скармливается тот же поток, что
// уходит в кольцевой буфер. Отдаёт самодостаточный кадр экрана.
//
// Потокобезопасен сам: вызывающий (Session) кормит его из отдельной горутины,
// а не из горячей секции под bufMu. Это принципиально: пропускная способность
// эмулятора — около 3 МБ/с на обычном тексте, и `cat` большого файла под общим
// замком остановил бы рассылку всем подписчикам и чтение самого PTY.
type screenMirror struct {
	mu     sync.Mutex
	em     *vt.Emulator
	cols   int
	rows   int
	closed bool
	// dec mirrors stateful client modes that are not represented by terminal
	// cells (mouse protocol, bracketed paste, focus and alt-buffer switches).
	// A self-contained frame must repair these modes as well as the grid.
	dec *decTracker
	// modes — режимы vt, которые кадр обязан перенести клиенту, но которых
	// нет в общем с pty-host decTracker (?25, ?1, ?66, ?6, ?7, IRM): см.
	// screen_modes.go (ST-05, «дёшево, силами зеркала»).
	modes mirrorModes

	// appliedOff — позиция потока сессии сразу после последнего разобранного
	// куска. По ней писатель понимает, какие байты кадр уже покрывает
	// (см. Session.ScreenFrameAt).
	appliedOff uint64

	// boundaryParser видит ИСХОДНЫЙ поток (до rewriteEraseAll) и доказывает,
	// что синтетический кадр вставляется только в GroundState. Если последний
	// PTY chunk оборвал CSI/OSC/DCS/UTF-8, кадр нельзя посылать: его ESC-байты
	// станут продолжением управляющей строки, а пришедший после кадра suffix —
	// видимым мусором. Emulator использует тот же parser, но не открывает его
	// состояние наружу.
	boundaryParser *ansi.Parser
	// resetGeneration increments only when the real ANSI parser dispatches RIS
	// (ESC c), not when those bytes appear inside OSC/DCS payload. A mirror
	// rebuilt from a truncated tail becomes delivery-authoritative again only
	// after such a proven full terminal reset.
	resetGeneration uint64

	// Хвост чанка, обрывающийся посреди символа или кластера графем. ConPTY
	// режет поток по границе своего буфера, а не по границе кластера, и
	// эмулятор на разрезанном ZWJ разваливает семью 👨‍👩‍👧‍👦 в одиночного 👨
	// (воспроизведено; апстрим charmbracelet/x#935 открыт). Держим хвост до
	// следующего чанка.
	pending   []byte
	pendingAt time.Time

	// Хвост чанка, оканчивающийся возможным началом ED2 (ESC, ESC[, ESC[2):
	// разрезанный по границе буфера ConPTY ESC[2J иначе проскочил бы подмену
	// rewriteEraseAll. Держится строго до следующего чанка, БЕЗ долива по
	// таймеру: эмулятор стейтфулен, и префикс, отданный отдельно, склеился бы
	// внутри него с продолжением в цельный ED2 мимо подмены.
	edCarry []byte

	// Байты, которые эмулятор ответил на запросы приложения (DSR, DA, CPR).
	// Их НЕЛЬЗЯ никуда отправлять — на запросы отвечает настоящий терминал у
	// человека, и второй ответ сломал бы приложению разбор. Но и не вычитывать
	// их нельзя: внутренняя труба переполняется, и Write ВИСНЕТ НАВСЕГДА
	// (воспроизведено 2000 запросами ESC[6n; апстрим charmbracelet/x#939).
	// drainStop живёт с зеркалом, drainDone — с текущим эмулятором: пересборка
	// после паники (screen_vtpanic.go) заводит новый эмулятор со своей
	// вычитывающей горутиной.
	drainStop chan struct{}
	drainDone chan struct{}

	// untrusted — эмулятор паниковал, и зеркало пересобрано пустым
	// (screen_vtpanic.go). Картинка в нём не доказана ничем: кадра и истории
	// нет, пока новый эмулятор не разберёт RIS (ESC c).
	untrusted bool
	// vtPanics — сколько паник эмулятора пережило это зеркало.
	vtPanics int
	// noVtGuards — только тесты и стенд (WithoutVtGuards): предохранители
	// registerVtGuards и фильтр dropStrayC1 пропускают вход как есть, чтобы
	// проверить страховочную сетку на настоящих паниках библиотеки.
	noVtGuards bool
	// utf8Owed — байты продолжения, которые ещё должна последовательность
	// UTF-8, оборванная концом прошлого куска (dropStrayC1, screen_c1.go).
	// Состояние ПОТОКА, а не эмулятора: пересборка его не сбрасывает.
	utf8Owed int
}

func newScreenMirror(cols, rows int) *screenMirror {
	cols, rows = clampScreenSize(cols, rows)
	m := &screenMirror{
		dec:       newDecTracker(),
		cols:      cols,
		rows:      rows,
		drainStop: make(chan struct{}),
	}
	m.boundaryParser = newBoundaryParser(func() { m.resetGeneration++ })
	// Эмулятор со всеми обработчиками (ED3, RIS, предохранители от паник vt)
	// и своей вычитывающей горутиной — screen_vtpanic.go.
	m.installEmulatorLocked()
	return m
}

// newBoundaryParser keeps only parser state. A one-element parameter/data
// buffer avoids the default 64 KiB OSC/DCS capture per live terminal; payload
// contents are irrelevant for deciding whether the parser is at a boundary.
func newBoundaryParser(onRIS func()) *ansi.Parser {
	p := new(ansi.Parser)
	p.SetParamsSize(1)
	p.SetDataSize(1)
	p.SetHandler(ansi.Handler{HandleEsc: func(cmd ansi.Cmd) {
		if onRIS != nil && cmd.Prefix() == 0 && cmd.Intermediate() == 0 && cmd.Final() == 'c' {
			onRIS()
		}
	}})
	p.Reset()
	return p
}

func clampScreenSize(cols, rows int) (int, int) {
	if cols <= 0 {
		cols = 80
	}
	if rows <= 0 {
		rows = 24
	}
	if cols > screenMirrorMaxCols {
		cols = screenMirrorMaxCols
	}
	if rows > screenMirrorMaxRows {
		rows = screenMirrorMaxRows
	}
	return cols, rows
}

// Write скармливает зеркалу очередной кусок вывода PTY.
func (m *screenMirror) Write(p []byte) { m.WriteAt(p, 0) }

// WriteAt — то же плюс позиция потока сразу после куска. Ноль означает «звать
// некому» (пробы и тесты, где потока с позициями нет) и позицию не двигает.
func (m *screenMirror) WriteAt(p []byte, off uint64) {
	if len(p) == 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return
	}
	// После атомарной пересборки stale mirror поздний feeder может
	// донести чанк, который уже вошёл в ring-snapshot. Absolute off делает
	// его точно распознаваемым дублем; off=0 — legacy/тестовый путь.
	if off > 0 && off <= m.appliedOff {
		return
	}
	m.dec.scan(p)
	// Feed exactly the bytes delivered to the client. rewriteEraseAll below is
	// mirror-only and must not influence whether the real client parser is at a
	// safe insertion boundary.
	for _, b := range p {
		m.boundaryParser.Advance(b)
	}
	if off > m.appliedOff {
		m.appliedOff = off
	}
	raw := p
	if len(m.edCarry) > 0 {
		raw = append(m.edCarry, p...)
		m.edCarry = nil
	}
	subst, carry := rewriteEraseAll(raw)
	if len(carry) > 0 {
		m.edCarry = append([]byte(nil), carry...)
	}
	if len(subst) == 0 {
		return
	}
	data := subst
	if len(m.pending) > 0 {
		data = append(m.pending, subst...)
		m.pending = nil
	}
	feed, tail := splitAtClusterBoundary(data)
	if len(tail) > 0 {
		m.pending = append(m.pending[:0], tail...)
		m.pendingAt = time.Now()
	}
	if len(feed) > 0 {
		m.feedEmulatorLocked(feed)
	}
}

// composeForMirror собирает NFD-пары в составной символ (NFC) перед эмулятором.
//
// ⚠ vt пишет несамостоятельный знак (Mn) в клетку ПОД КУРСОРОМ и курсор не
// двигает, а xterm клиента приклеивает его к клетке слева. Замер 14.09
// (FrameFromStream, любые нарезки): «7e» + e+U+0301 + «dc» давало кадр
// «7eedc» — следующий символ затирал знак, и после переподключения акцент
// пропадал; так же «й» и «ё» из имён файлов macOS (NFD). Затирание
// «L313 J» ESC[1;1H e+U+0301 давало «é13 J» вместо «é313 J».
//
// Меняется только вход зеркала: клиенту уходят исходные байты, кадр отдаёт
// ту же букву в NFC — на экране она та же. Управляющие байты ASCII
// нормализация не трогает. Остаток — разрез ровно между базой и знаком и знаки
// без составной формы: разрыв vt-combining-split стенда эквивалентности.
func composeForMirror(b []byte) []byte {
	if norm.NFC.IsNormal(b) {
		return b
	}
	return norm.NFC.Bytes(b)
}

// rewriteEraseAll заменяет ED2 (ESC[2J) на пару ED1+ED0 (ESC[1J ESC[0J).
//
// ⚠ vt на ED2 СОХРАНЯЕТ все непустые строки экрана в scrollback
// (Screen.ClearWithScrollback), а xterm.js на телефоне просто стирает экран.
// Агентские TUI перерисовываются через ED2 — Claude Code шлёт его на каждый
// ресайз и крупный апдейт, — и каждая перерисовка клала в историю зеркала
// копию всего экрана. Боевой замер 13.08.2026 (хвост кольца 512 КБ сессии
// владельца, 5×ED2): 185 строк истории у зеркала против 74 строк буфера у
// xterm.js на ОДНОМ И ТОМ ЖЕ потоке — прокрутка на телефоне состояла из
// дублей переписки. Телефонные качели высоты (48x31→48x12→48x10→48x31 за 22
// секунды в боевом логе) множили перерисовки, и история зарастала за минуты.
//
// ED1 (начало экрана → курсор) + ED0 (курсор → конец экрана) стирают в vt те
// же ячейки той же blank-ячейкой с текущим фоном и не двигают курсор — по
// обработчикам это FillArea без побочных эффектов, ровно поведение xterm.
// ED3 (стирание истории) не трогаем: vt и xterm тут совпадают.
func rewriteEraseAll(b []byte) (out, carry []byte) {
	// Быстрый путь: ESC нет вовсе — нечего ни менять, ни придерживать.
	if bytes.IndexByte(b, 0x1b) < 0 {
		return b, nil
	}
	if i := bytes.Index(b, ed2Seq); i >= 0 {
		out = make([]byte, 0, len(b)+2*(len(ed2Subst)-len(ed2Seq)))
		for i >= 0 {
			out = append(out, b[:i]...)
			out = append(out, ed2Subst...)
			b = b[i+len(ed2Seq):]
			i = bytes.Index(b, ed2Seq)
		}
		out = append(out, b...)
	} else {
		out = b
	}
	// Хвост, который в следующем чанке может продолжиться в ED2: ConPTY режет
	// поток по границе буфера, не спрашивая последовательность.
	for n := len(ed2Seq) - 1; n > 0; n-- {
		if len(out) >= n && bytes.Equal(out[len(out)-n:], ed2Seq[:n]) {
			return out[:len(out)-n], out[len(out)-n:]
		}
	}
	return out, nil
}

var (
	ed2Seq   = []byte("\x1b[2J")
	ed2Subst = []byte("\x1b[1J\x1b[0J")
)

// maxPendingTail — сколько байт готовы придержать в ожидании продолжения
// кластера. Больше этого — значит поток не про графемы (двоичный мусор), и
// держать нечего: отдаём как есть, иначе будем копить вечно.
const maxPendingTail = 64

// splitAtClusterBoundary отрезает хвост, который обрывается посреди символа
// UTF-8 или посреди кластера графем (ZWJ, вариационный селектор, тон кожи).
// Возвращает то, что можно отдать эмулятору, и то, что подождёт продолжения.
func splitAtClusterBoundary(b []byte) (feed, tail []byte) {
	if len(b) == 0 {
		return nil, nil
	}
	// Незавершённая последовательность UTF-8 в самом конце.
	cut := len(b)
	for i := 0; i < utf8.UTFMax && cut > 0; i++ {
		r, size := utf8.DecodeLastRune(b[:cut])
		if r != utf8.RuneError || size > 1 {
			break
		}
		cut--
	}
	if len(b)-cut >= utf8.UTFMax {
		// Это не обрезанный символ, а мусор — отдаём как есть.
		cut = len(b)
	}
	// Кластер, который МОЖЕТ продолжиться в следующем чанке. Мало придержать
	// хвост, кончающийся соединителем: разрез бывает и РОВНО МЕЖДУ эмодзи и
	// соединителем — тогда 👨 уже уехал в эмулятор отдельным символом, и
	// пришедшее следом «ZWJ 👩» приклеить уже не к чему (тест на разрез по
	// байту 4 ловил именно это). Поэтому придерживаем и последнюю графему,
	// если она вообще способна участвовать в кластере.
	// Правило: соединители и модификаторы тянут за собой предыдущую графему,
	// а самостоятельную графему придерживаем только одну — иначе строка из
	// независимых эмодзи целиком уезжала бы в ожидание и не показывалась.
	first, afterJoiner := true, false
	for cut > 0 {
		r, size := utf8.DecodeLastRune(b[:cut])
		if isClusterJoiner(r) {
			cut -= size
			afterJoiner = true
			continue
		}
		// База, к которой лепится соединитель, уходит в ожидание вместе с ним
		// ВСЕГДА, какой бы она ни была: иначе «e» уезжала в эмулятор отдельно, а
		// придержанный знак потом ложился в клетку под курсором и затирался
		// (NFD «é», замер 14.09). Управляющий байт базой не бывает.
		if afterJoiner && r >= 0x20 && r != 0x7f {
			cut -= size
			first, afterJoiner = false, false
			continue
		}
		if mayContinueCluster(r) && first {
			cut -= size
			first, afterJoiner = false, false
			continue
		}
		break
	}
	if cut < 0 {
		cut = 0
	}
	if len(b)-cut > maxPendingTail {
		return b, nil
	}
	return b[:cut], b[cut:]
}

// isClusterJoiner — руна, которая заведомо ЛЕПИТСЯ к предыдущей графеме и без
// неё смысла не имеет. Такой хвост придерживаем всегда.
func isClusterJoiner(r rune) bool {
	switch {
	case r == 0x200D: // соединитель нулевой ширины
		return true
	case r >= 0xFE00 && r <= 0xFE0F: // селекторы варианта
		return true
	case r >= 0x1F3FB && r <= 0x1F3FF: // модификаторы тона кожи
		return true
	case r >= 0x0300 && r <= 0x036F: // комбинирующие диакритики
		return true
	}
	return false
}

// mayContinueCluster — может ли руна оказаться не концом графемы, а её
// серединой. Держим список узким: чем больше придерживаем, тем дольше символ
// не виден в зеркале (доливается по простою, см. flushPendingLocked).
func mayContinueCluster(r rune) bool {
	if isClusterJoiner(r) {
		return true
	}
	switch {
	case r >= 0x1F000 && r <= 0x1FAFF: // эмодзи, флаги, регионы
		return true
	case r >= 0x2600 && r <= 0x27BF: // ☀ ✅ ✈ — берут селектор варианта
		return true
	case r >= 0x0300 && r <= 0x036F: // комбинирующие диакритики
		return true
	}
	return false
}

// pendingIdle — сколько ждём продолжение кластера, прежде чем показать
// придержанный хвост как есть. Поток агента идёт пачками по десять раз в
// секунду, поэтому продолжение приходит на порядок быстрее; а если приложение
// замолчало на середине эмодзи — лучше показать одиночный символ, чем не
// показать ничего.
const pendingIdle = 100 * time.Millisecond

// flushPendingLocked отдаёт эмулятору придержанный хвост, если продолжение так
// и не пришло. Вызывается перед снятием кадра — иначе последний напечатанный
// символ не попал бы в картинку.
func (m *screenMirror) flushPendingLocked() {
	if len(m.pending) == 0 || time.Since(m.pendingAt) < pendingIdle {
		return
	}
	m.feedEmulatorLocked(m.pending)
	m.pending = nil
}

// flushCompletePendingLocked commits a fully decoded grapheme at an ordered
// control-plane boundary. Unlike the idle flush above, it must not pass an
// incomplete UTF-8 tail to the emulator: those bytes become a character only
// when a later output chunk completes them, potentially after a resize.
func (m *screenMirror) flushCompletePendingLocked() {
	if len(m.pending) == 0 || !utf8.Valid(m.pending) {
		return
	}
	m.feedEmulatorLocked(m.pending)
	m.pending = nil
}

// Resize меняет геометрию зеркала. Зеркало обязано жить в той же геометрии,
// что и PTY: приложение рисует по абсолютным адресам, и кадр, снятый в 80
// колонках, в 48-колоночном терминале разъедется.
func (m *screenMirror) Resize(cols, rows int) {
	cols, rows = clampScreenSize(cols, rows)
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return
	}
	// The ACK/resize marker is in the same FIFO as output. A complete pending
	// emoji/cluster therefore belongs to the old geometry even though the
	// chunk splitter normally waits briefly for a possible VS/ZWJ/modifier.
	// Incomplete UTF-8 and ED/parser carry remain pending until later bytes make
	// them meaningful; emitting either here would feed parser garbage.
	m.flushCompletePendingLocked()
	if cols == m.cols && rows == m.rows {
		return
	}
	m.cols, m.rows = cols, rows
	m.resizeEmulatorLocked(cols, rows)
}

// Size отдаёт геометрию, в которой снят кадр: клиент должен применить его в
// ней же, а свой размер выставить уже после (так же поступает VS Code —
// forceExactSize, и того же требует документация xterm-аддона сериализации).
func (m *screenMirror) Size() (cols, rows int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cols, m.rows
}

// DropScrollback выбрасывает историю прокрутки зеркала, оставляя собранный
// экран. Зовётся ровно в одном месте — на границе переигровки буфера
// (Session.dropReplayScrollback), и смысл там же: строки, которые нагнала
// вверх переигровка, историей человека не являются.
func (m *screenMirror) DropScrollback() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return
	}
	m.em.Scrollback().Clear()
}

func (m *screenMirror) Close() {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	m.closed = true
	em, drained := m.em, m.drainDone
	m.mu.Unlock()
	close(m.drainStop)
	stopEmulator(em, drained)
}

// Frame собирает САМОДОСТАТОЧНЫЙ кадр экрана: последовательность, которая на
// чистом терминале нужной геометрии воспроизводит текущую картинку целиком.
func (m *screenMirror) Frame() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return ""
	}
	if !m.snapshotReadyLocked() {
		return ""
	}
	return m.frameLocked()
}

// snapshotReadyLocked proves that every byte named by appliedOff has both
// reached the emulator and left the raw client parser at a safe boundary.
// pending may contain a complete but briefly held emoji cluster even while the
// ANSI parser is GroundState; until it is flushed a frame would claim bytes it
// does not actually contain.
//
// После паники эмулятора (untrusted) готовности нет до RIS: пересобранное
// зеркало пусто, и кадр из него затёр бы у клиента правильный экран
// (screen_vtpanic.go).
func (m *screenMirror) snapshotReadyLocked() bool {
	m.flushPendingLocked()
	return !m.untrusted && len(m.pending) == 0 && len(m.edCarry) == 0 && m.boundaryParser.State() == ansiparser.GroundState
}

// frameLocked — тело Frame без замка: вызывают Frame и Snapshot.
//
// ⚠ Почему не Render() из библиотеки: он отдаёт строки через "\n" без
// возврата каретки, без входа в alt-screen, без очистки и без позиции курсора.
// Влитый как есть, он даёт лесенку и наложение поверх старого содержимого —
// то есть ровно тот дефект, который мы чиним.
//
// ⚠ Почему каждая строка адресуется АБСОЛЮТНО (CSI y;1H), а не печатается
// подряд: наш Go-эмулятор и xterm.js в браузере считают ширину эмодзи, ZWJ и
// флагов по-разному, и ни одна конфигурация их не мирит. При печати подряд
// одна лишняя колонка переносит строку и сдвигает ВЕСЬ кадр ниже; при
// абсолютной адресации ошибка остаётся внутри своей строки — ровно как сегодня
// у сырого потока, где агент сам ставит CUP перед каждой строкой.
func (m *screenMirror) frameLocked() string {
	var sb strings.Builder
	sb.Grow(m.cols * m.rows / 2)

	// A frame is authoritative for the active buffer even on replace=false
	// repair. First unwind every xterm alt-screen variant in the stateful order
	// 1049 (saved cursor), 1047, 47. Otherwise a client that lost ?1049l in a
	// queue gap would paint a normal-shell frame into its stale alt buffer and
	// keep broken scroll routing. If the mirror itself is alt, enter one known
	// canonical mode again before drawing.
	sb.WriteString("\x1b[?1049l\x1b[?1047l\x1b[?47l")
	if m.em.IsAltScreen() {
		sb.WriteString("\x1b[?1049h")
	}
	sb.WriteString(m.dec.syncNonAltSeq())
	// DECOM и IRM грязного клиента сдвинули бы адресацию и раздвигали бы
	// строки кадра: пока рисуем, оба выключены (screen_modes.go).
	sb.WriteString(m.modes.preDrawSeq())
	// Сброс стиля до очистки: ESC[2J закрашивает экран ТЕКУЩИМ фоном, и без
	// сброса кадр лёг бы на цветную подложку от последнего SGR приложения.
	sb.WriteString("\x1b[m\x1b[H\x1b[2J")

	var cur uv.Style
	for y := 0; y < m.rows; y++ {
		line, lineStyle := m.renderLine(y, &cur)
		if line == "" {
			continue
		}
		fmt.Fprintf(&sb, "\x1b[%d;1H", y+1)
		sb.WriteString(line)
		cur = lineStyle
	}
	if !cur.IsZero() {
		sb.WriteString("\x1b[m")
	}
	// Режимы приложения (?1, ?66, ?7, IRM, ?6, ?25) — после отрисовки и ДО
	// позиции курсора: DECSET 6 переводит курсор в начало области, и
	// финальная абсолютная адресация обязана идти после него (T-26).
	sb.WriteString(m.modes.seq())

	pos := m.em.CursorPosition()
	// Абсолютная позиция курсора в конце — обязательна. Относительное
	// восстановление промахивается на колонку, когда последняя строка ровно в
	// ширину экрана (состояние отложенного переноса), а полноэкранные TUI
	// рисуют рамку именно во всю ширину.
	fmt.Fprintf(&sb, "\x1b[%d;%dH", pos.Y+1, pos.X+1)
	return sb.String()
}

// History собирает ИСТОРИЮ зеркала — строки scrollback (ушедшие за верх
// экрана) ПЛЮС текущие видимые строки: клиент печатает их последовательно в
// чистый терминал перед кадром и получает точный шов (см. historyLocked).
// Возвращает текст и ОБЩЕЕ число строк в нём (клиенту — для контроля приёма).
// Alt-screen отдаёт ("", 0): у полноэкранных приложений истории нет по
// определению протокола. maxLines ограничивает только scrollback-часть,
// видимые строки идут всегда целиком.
func (m *screenMirror) History(maxLines int) (string, int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return "", 0
	}
	m.flushPendingLocked()
	if m.untrusted {
		// История пересобранного после паники зеркала — не история человека.
		return "", 0
	}
	return m.historyLocked(maxLines)
}

// historyLocked — тело History без замка: вызывают History и Snapshot.
//
// ИСТОРИЯ = scrollback + ТЕКУЩИЕ ВИДИМЫЕ СТРОКИ экрана (модель сериализации
// VS Code: снапшот покрывает scrollback и экран целиком). Почему не один
// scrollback: клиент на reset-пути пишет историю в чистый терминал и следом
// отдаёт кадр, а кадр начинается с ESC[H ESC[2J — ED2 в xterm
// (scrollOnEraseInDisplay=false) гасит видимый экран, НЕ сдвигая его в
// scrollback. Прислав только scrollback (S строк), мы бы оставили последние R
// строк на видимом экране клиента, и ED2 кадра уничтожил бы их бесследно —
// терялись самые свежие строки истории, ровно те, что над экраном. Поэтому
// видимые дописываем всегда: клиент пишет S+R строк, scrollback получает
// ровно S, видимые финишируют на экране и перерисовываются кадром абсолютно.
// Дублей нет: видимые строки в scrollback не попадают, кадр их перезаписывает.
//
// Разделитель "\r\n" — после КАЖДОЙ строки, КРОМЕ ПОСЛЕДНЕЙ ВИДИМОЙ. Замер на
// настоящем xterm.js 13.08.2026: запись S+R строк в терминал высотой R сама
// прокручивает ровно S раз, а "\r\n" после последней строки добавляет ещё одну
// прокрутку — первая видимая строка задублировалась в scrollback, а низ
// экрана остался пустым. По той же причине пустые строки (включая хвостовые
// пустые видимые) печатаем ВСЕ: каждая пропущенная сдвигала бы счёт
// прокруток, и scrollback клиента недополучал бы строки снизу. Геометрия
// обязана быть применена клиентом ДО записи истории (screen_cols/screen_rows
// того же сообщения) — счёт прокруток верен только в высоте R.
//
// Печать последовательная, старая → новая; адресации нет — это удел кадра.
// Текст самодостаточен по стилям: открываем сбросом SGR, дальше тот же дифф,
// что в кадре, сквозь строки.
//
// Слайс Scrollback().Lines() ЖИВОЙ — читаем его только под m.mu, как и сетку в
// кадре. Строки в нём обрезаны справа (хвостовые пустые ячейки режет
// библиотека) — renderHistoryLine это учитывает.
//
// ⚠ ИЗВЕСТНОЕ РАСХОЖДЕНИЕ библиотеки с xterm: строки, ушедшие из ОБЛАСТИ
// прокрутки (DECSTBM с верхом не на первой строке) или удалённые DL на полном
// экране, vt тоже кладёт в scrollback, а xterm — нет. Для обычной оболочки
// (полноэкранная прокрутка) история совпадает с эталоном байт в байт — это
// проверяет проба на случае «history-scroll»; приложения с регионами в
// обычном буфере могут дать лишние продублированные строки. Замер и пин —
// TestScreenMirrorHistoryRegionScrollLeakIsKnown, освобождение в пробе —
// historyInformative. Лечится только правкой DeleteLine в vt (форк).
func (m *screenMirror) historyLocked(maxLines int) (string, int) {
	if maxLines <= 0 || m.em.IsAltScreen() {
		return "", 0
	}
	lines := m.em.Scrollback().Lines()
	if len(lines) > maxLines {
		lines = lines[len(lines)-maxLines:]
	}
	total := len(lines) + m.rows

	var sb strings.Builder
	sb.Grow(total * 40)
	sb.WriteString("\x1b[m")
	var cur uv.Style
	for i := range lines {
		text, lineStyle := renderHistoryLine(lines[i], m.cols, &cur)
		sb.WriteString(text)
		sb.WriteString("\r\n")
		cur = lineStyle
	}
	// Видимые строки — все R, включая пустые (см. шапку про счёт прокруток).
	for y := 0; y < m.rows; y++ {
		text, lineStyle := m.renderLine(y, &cur)
		sb.WriteString(text)
		if y < m.rows-1 {
			sb.WriteString("\r\n")
		}
		cur = lineStyle
	}
	if !cur.IsZero() {
		sb.WriteString("\x1b[m")
	}
	return sb.String(), total
}

// Snapshot снимает кадр, историю и геометрию ОДНИМ удержанием замка. Между
// ними не может проскочить запись, которая уронила бы строку (ушла с экрана
// после History, но попала в scrollback до Frame) или показала её дважды —
// а геометрия гарантированно та, в которой снято содержимое.
func (m *screenMirror) Snapshot(maxHistory int) (frame, history string, histLines, cols, rows int) {
	frame, history, histLines, cols, rows, _ = m.SnapshotAt(maxHistory)
	return
}

// SnapshotAt — то же самое плюс позиция потока, до которой кадр учитывает
// вывод (см. Session.ScreenFrameAt). Читается ПОД ТЕМ ЖЕ замком, что и
// содержимое: иначе между «сняли кадр» и «узнали позицию» проскочила бы
// запись, и позиция описывала бы уже не этот кадр.
func (m *screenMirror) SnapshotAt(maxHistory int) (frame, history string, histLines, cols, rows int, appliedOff uint64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return "", "", 0, 0, 0, 0
	}
	if !m.snapshotReadyLocked() {
		return "", "", 0, 0, 0, 0
	}
	frame = m.frameLocked()
	history, histLines = m.historyLocked(maxHistory)
	return frame, history, histLines, m.cols, m.rows, m.appliedOff
}

// FrameFromStream прогоняет запись потока через зеркало и отдаёт кадр. Нужна
// сквозной пробе (tools/screen-frame → build/qa/probe-screen-frame.mjs),
// которая сверяет нашу картинку с настоящим xterm.js ячейка в ячейку.
//
// chunk > 0 режет поток на куски: живой ConPTY именно так и делает, и
// разрезанные на границе буфера кластеры графем — отдельный источник расхождений.
func FrameFromStream(data []byte, cols, rows, chunk int) string {
	m := newScreenMirror(cols, rows)
	defer m.Close()
	feedChunks(m, data, chunk)
	// Придержанный хвост кластера обязан долиться: проба снимает кадр сразу,
	// а не через секунду после последнего байта.
	m.mu.Lock()
	if len(m.pending) > 0 {
		m.feedEmulatorLocked(m.pending)
		m.pending = nil
	}
	m.mu.Unlock()
	return m.Frame()
}

// StreamSnapshot — снимок зеркала на разрезе потока, ровно в том виде, в
// каком его отдал бы производственный SnapshotAt (раздел 6 плана, путь B).
// Ready=false означает «кадр withheld»: на этом разрезе клиент не получил бы
// ничего, и все поля кадра пусты — так же, как на проводе.
type StreamSnapshot struct {
	Cut        int    `json:"cut"`
	Ready      bool   `json:"ready"`
	Frame      string `json:"frame"`
	History    string `json:"history"`
	HistLines  int    `json:"histLines"`
	Cols       int    `json:"cols"`
	Rows       int    `json:"rows"`
	AppliedOff uint64 `json:"appliedOff"`
	// Untrusted — снимок withheld не «пока», а до RIS: эмулятор паниковал, и
	// зеркало пересобрано (screen_vtpanic.go). Это отказ, а не различие
	// состояния: стенд его объявляет, а не сравнивает.
	Untrusted bool `json:"untrusted,omitempty"`
	// Состояние САМОГО зеркала на разрезе (только при Ready): сетка активного
	// экрана в форме GridCellsFromStream и курсор vt. Нужны стенду C-03 для
	// поклеточного правила «клетка восстановленного клиента = xterm ИЛИ
	// зеркало» в момент снимка: различие, которого нет ни там ни там, —
	// дефект сборщика кадра, и никакой библиотечный разрыв его не прячет.
	// Снимается после SnapshotAt, то есть с долитой графемой, — точнее, чем
	// отдельный прогон GridCellsFromStream с другим планом кусков.
	Grid    [][]string `json:"grid,omitempty"`
	CursorX int        `json:"cursorX"`
	CursorY int        `json:"cursorY"`
	Alt     bool       `json:"alt"`
	// Строки истории зеркала, ушедшие в снимок (старые → новые; тот же срез,
	// что historyLocked): голый текст без хвостовых пробелов, продолжения
	// широких графем пропущены — как translateToString(true) у xterm. В
	// alt-экране пусто (история там не отдаётся). Для того же правила «клиент =
	// зеркало», но по истории: широкое поле *.scrollback* объясняет различие с
	// xterm, только если клиент получил ровно историю зеркала.
	Scrollback []string `json:"scrollback,omitempty"`
	// Ширина каждой строки истории в колонках, как её напечатает клиент
	// (renderHistoryLine: до последней непустой клетки, пробел с фоном — тоже).
	// Строка шире экрана клиента у него занимает несколько рядов: стенду для
	// прогноза шва истории (разрыв history-seam-wide-lines).
	ScrollbackCells []int `json:"scrollbackCells,omitempty"`
}

// SnapshotsFromStream снимает зеркало на каждом разрезе из cuts: свежее
// зеркало, в него data[:cut] ПРОИЗВОДСТВЕННЫМ путём WriteAt с абсолютной
// позицией после каждого куска (как readLoop), затем SnapshotAt(maxHistory).
//
// ЗАЧЕМ (ST-05, T-26/T-27). FrameFromStream кормит зеркало через Write(off=0)
// и насильно доливает придержанную графему — позиция не ведётся, готовность не
// проверяется, и «снимок посреди CSI» в нём непредставим. Здесь проверяется
// именно то, что уходит клиенту: withhold на незавершённой последовательности
// и base_offset, совпадающий с разрезом.
//
// chunks — план кусков по кругу (7,13 → 7,13,7,…); последний кусок
// обрезается разрезом: снимок бывает только между кусками, и разрез как раз
// такая граница. Пустой план (или неположительный размер) — всё одним куском.
//
// idle — детерминированная замена 100 мс тишины (pendingIdle): придержанный
// хвост кластера считается «давно без продолжения» и доливается в
// SnapshotAt. Без idle момент удержания сдвигается в будущее, чтобы медленная
// машина не долила его сама по часам (приём из
// TestScreenMirrorSnapshotWaitsForHeldGrapheme). Разрез по часам — отдельный
// сценарий (раздел 6.2: задержки проверяются отдельно от фрагментации).
//
// maxHistory < 0 — производственный потолок сессии (screenMirrorScrollback,
// см. Session.screenFrameAtCurrent).
//
// Свежее зеркало на каждый разрез — O(n²) по байтам: для больших потоков
// вызывающий берёт выборку разрезов. Один процесс на фикстуру (tools/screen-frame
// -snapshots-json), а не запуск на разрез.
func SnapshotsFromStream(data []byte, cols, rows int, chunks []int, cuts []int, maxHistory int, idle bool) []StreamSnapshot {
	return SnapshotsFromStreamResized(data, cols, rows, chunks, cuts, maxHistory, idle, nil)
}

// StreamResize — смена геометрии в потоке стенда эквивалентности (раздел 6).
// В продукте это маркер resize в ТОЙ ЖЕ очереди, что и вывод (screenChunk.resize,
// Session.resizeScreen): байты до маркера разобраны в старой сетке, после — в
// новой, а кусок вывода маркером не разрезается — маркер встаёт между чтениями.
// Off — позиция потока, на которой стоит маркер. AfterCut решает только ничью:
// при разрезе РОВНО на Off снимок снят ДО маркера (resize — первое событие
// хвоста); без AfterCut — после (resize — последнее событие префикса).
type StreamResize struct {
	Off      int  `json:"off"`
	Cols     int  `json:"cols"`
	Rows     int  `json:"rows"`
	AfterCut bool `json:"afterCut,omitempty"`
}

// appliesBefore — вошёл ли resize в префикс снимка на разрезе cut.
func (r StreamResize) appliesBefore(cut int) bool {
	return r.Off < cut || (r.Off == cut && !r.AfterCut)
}

// StreamOption — настройка зеркала стенда (SnapshotsFromStreamResized).
type StreamOption func(*screenMirror)

// WithoutVtGuards снимает предохранители от известных паник vt
// (registerVtGuards): стенд проверяет страховочную сетку на НАСТОЯЩИХ паниках
// библиотеки — снимок withheld (Untrusted) до RIS, после RIS снова кадр.
func WithoutVtGuards() StreamOption { return func(m *screenMirror) { m.noVtGuards = true } }

// SnapshotsFromStreamResized — SnapshotsFromStream со сменами геометрии в
// потоке: зеркало получает ПРОИЗВОДСТВЕННЫЙ Resize ровно в той точке потока,
// где его получил бы воркер sessionScreen (FIFO вывода и маркеров), — между
// кусками; кусок плана, накрывающий позицию маркера, режется на ней. Сам
// Resize (со своим доливом законченной графемы, flushCompletePendingLocked)
// не подменяется ничем: стенд проверяет продуктовую последовательность, а не
// свою копию (TestSnapshotsFromStreamResizedMatchesSessionFIFO).
func SnapshotsFromStreamResized(data []byte, cols, rows int, chunks []int, cuts []int, maxHistory int, idle bool, resizes []StreamResize, opts ...StreamOption) []StreamSnapshot {
	if maxHistory < 0 {
		maxHistory = screenMirrorScrollback
	}
	resizes = append([]StreamResize(nil), resizes...)
	sort.SliceStable(resizes, func(i, j int) bool { return resizes[i].Off < resizes[j].Off })
	out := make([]StreamSnapshot, 0, len(cuts))
	for _, cut := range cuts {
		if cut < 0 {
			cut = 0
		}
		if cut > len(data) {
			cut = len(data)
		}
		m := newScreenMirror(cols, rows)
		for _, o := range opts {
			o(m)
		}
		feedChunksResized(m, data[:cut], chunks, resizes, cut)
		m.mu.Lock()
		if len(m.pending) > 0 {
			if idle {
				m.pendingAt = time.Now().Add(-pendingIdle - time.Second)
			} else {
				m.pendingAt = time.Now().Add(time.Hour)
			}
		}
		m.mu.Unlock()
		frame, history, histLines, fcols, frows, applied := m.SnapshotAt(maxHistory)
		snap := StreamSnapshot{
			Cut:        cut,
			Ready:      frame != "",
			Frame:      frame,
			History:    history,
			HistLines:  histLines,
			Cols:       fcols,
			Rows:       frows,
			AppliedOff: applied,
		}
		m.mu.Lock()
		snap.Untrusted = m.untrusted
		m.mu.Unlock()
		if snap.Ready {
			m.mu.Lock()
			snap.Grid = streamGridLocked(m)
			snap.Scrollback = streamScrollbackLocked(m, maxHistory)
			snap.ScrollbackCells = streamScrollbackCellsLocked(m, maxHistory)
			pos := m.em.CursorPosition()
			snap.CursorX, snap.CursorY = pos.X, pos.Y
			snap.Alt = m.em.IsAltScreen()
			m.mu.Unlock()
		}
		m.Close()
		out = append(out, snap)
	}
	return out
}

// streamScrollbackLocked — текст строк истории, которые historyLocked отдал
// бы в снимок (тот же потолок maxLines, пусто в alt). Вызывать под m.mu.
func streamScrollbackLocked(m *screenMirror, maxLines int) []string {
	if maxLines <= 0 || m.em.IsAltScreen() {
		return nil
	}
	lines := m.em.Scrollback().Lines()
	if len(lines) > maxLines {
		lines = lines[len(lines)-maxLines:]
	}
	out := make([]string, 0, len(lines))
	for _, ln := range lines {
		var sb strings.Builder
		skip := 0
		for x := range ln {
			if skip > 0 {
				skip--
				continue
			}
			if ln[x].Content == "" {
				sb.WriteByte(' ')
				continue
			}
			sb.WriteString(ln[x].Content)
			if ln[x].Width > 1 {
				skip = ln[x].Width - 1
			}
		}
		out = append(out, strings.TrimRight(sb.String(), " "))
	}
	return out
}

// streamScrollbackCellsLocked — ширина в колонках каждой строки истории того же
// среза, что streamScrollbackLocked (historyLineCells). Вызывать под m.mu.
func streamScrollbackCellsLocked(m *screenMirror, maxLines int) []int {
	if maxLines <= 0 || m.em.IsAltScreen() {
		return nil
	}
	lines := m.em.Scrollback().Lines()
	if len(lines) > maxLines {
		lines = lines[len(lines)-maxLines:]
	}
	out := make([]int, 0, len(lines))
	for _, ln := range lines {
		out = append(out, historyLineCells(ln))
	}
	return out
}

// historyLineCells — сколько колонок строка истории займёт у клиента: тот же
// «последний печатаемый» признак, что в renderCells (пустая клетка и пробел
// без стиля в хвосте не печатаются), широкая графема — целиком.
func historyLineCells(line uv.Line) int {
	for x := len(line) - 1; x >= 0; x-- {
		c := &line[x]
		if c.Content == "" || (c.Content == " " && c.Style.IsZero()) {
			continue
		}
		if c.Width > 1 {
			return x + c.Width
		}
		return x + 1
	}
	return 0
}

// streamGridLocked — сетка активного экрана зеркала для StreamSnapshot: ровно
// cols клеток в строке, как GridCellsFromStream, но в форме activeGrid стенда
// (terminalConformance.ts): продолжение широкой графемы — явная "", ПУСТАЯ
// клетка — " ". В GridCellsFromStream обе пустые строки, и отличить их без
// ширин на стороне TS нельзя; здесь ширина известна. Вызывать под m.mu.
func streamGridLocked(m *screenMirror) [][]string {
	out := make([][]string, m.rows)
	for y := 0; y < m.rows; y++ {
		row := make([]string, m.cols)
		skip := 0
		for x := 0; x < m.cols; x++ {
			if skip > 0 {
				skip--
				continue
			}
			c := m.em.CellAt(x, y)
			if c == nil || c.Content == "" {
				row[x] = " "
				continue
			}
			row[x] = c.Content
			if c.Width > 1 {
				skip = c.Width - 1
			}
		}
		out[y] = row
	}
	return out
}

// feedChunksAt кормит зеркало по плану кусков с абсолютной позицией конца
// каждого куска — тем же WriteAt(p, off), что и производственный readLoop.
func feedChunksAt(m *screenMirror, data []byte, chunks []int) {
	feedChunksResized(m, data, chunks, nil, len(data))
}

// feedChunksResized — feedChunksAt плюс маркеры resize (отсортированы по Off):
// маркер исполняется, когда поток дошёл до его позиции, если он входит в
// префикс разреза cut (StreamResize.appliesBefore); кусок, накрывающий позицию
// маркера, режется на ней, следующий берёт очередной размер плана. Без
// маркеров — ровно прежний feedChunksAt.
func feedChunksResized(m *screenMirror, data []byte, chunks []int, resizes []StreamResize, cut int) {
	off, i, ri := 0, 0, 0
	for {
		for ri < len(resizes) && resizes[ri].Off <= off {
			if resizes[ri].appliesBefore(cut) {
				m.Resize(resizes[ri].Cols, resizes[ri].Rows)
			}
			ri++
		}
		if off >= len(data) {
			return
		}
		n := len(data) - off
		if len(chunks) > 0 {
			if c := chunks[i%len(chunks)]; c > 0 && c < n {
				n = c
			}
			i++
		}
		if ri < len(resizes) && resizes[ri].Off < off+n {
			n = resizes[ri].Off - off
		}
		m.WriteAt(data[off:off+n], uint64(off+n))
		off += n
	}
}

// HistoryFromStream — то же, что FrameFromStream, но для истории: прогоняет
// запись потока через зеркало и отдаёт собранный хвост scrollback. Сквозная
// проба (build/qa/probe-screen-frame.mjs) сверяет его со scrollback настоящего
// xterm.js на том же потоке.
func HistoryFromStream(data []byte, cols, rows, chunk, maxLines int) (string, int) {
	m := newScreenMirror(cols, rows)
	defer m.Close()
	feedChunks(m, data, chunk)
	// Как и в FrameFromStream: придержанный хвост кластера обязан долиться.
	m.mu.Lock()
	if len(m.pending) > 0 {
		m.feedEmulatorLocked(m.pending)
		m.pending = nil
	}
	m.mu.Unlock()
	return m.History(maxLines)
}

// GridFromStream отдаёт СЕТКУ зеркала построчно — голый текст, без стилей.
// Нужна пробе, чтобы отличить два разных дефекта: разошёлся сам эмулятор
// (тогда виновата библиотека и лечится она) или разошёлся сборщик кадра (тогда
// виноваты мы). Без этого различения починка — гадание.
func GridFromStream(data []byte, cols, rows, chunk int) []string {
	cells := GridCellsFromStream(data, cols, rows, chunk)
	out := make([]string, 0, len(cells))
	for _, row := range cells {
		var sb strings.Builder
		for _, content := range row {
			if content == "" {
				sb.WriteByte(' ')
			} else {
				sb.WriteString(content)
			}
		}
		out = append(out, sb.String())
	}
	return out
}

// GridCellsFromStream returns an explicit terminal-cell matrix. Every row has
// exactly cols entries; a wide/ZWJ grapheme lives in one entry and each of its
// continuation columns is an explicit empty string. This is the lossless form
// for cross-emulator probes: splitting GridFromStream's display text into JS
// code points breaks one grapheme into several fake cells and shifts the
// comparison even when both terminal grids are identical.
func GridCellsFromStream(data []byte, cols, rows, chunk int) [][]string {
	m := newScreenMirror(cols, rows)
	defer m.Close()
	feedChunks(m, data, chunk)
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.pending) > 0 {
		m.feedEmulatorLocked(m.pending)
		m.pending = nil
	}
	out := make([][]string, m.rows)
	for y := 0; y < m.rows; y++ {
		row := make([]string, m.cols)
		skip := 0
		for x := 0; x < m.cols; x++ {
			if skip > 0 {
				skip--
				// Continuation of the preceding wide cell stays explicit "".
				continue
			}
			c := m.em.CellAt(x, y)
			if c == nil || c.Content == "" {
				continue
			}
			row[x] = c.Content
			if c.Width > 1 {
				skip = c.Width - 1
			}
		}
		out[y] = row
	}
	return out
}

func feedChunks(m *screenMirror, data []byte, chunk int) {
	if chunk <= 0 {
		m.Write(data)
		return
	}
	for off := 0; off < len(data); off += chunk {
		end := off + chunk
		if end > len(data) {
			end = len(data)
		}
		m.Write(data[off:end])
	}
}

// renderLine собирает одну строку кадра, начиная со стиля prev. Возвращает
// пустую строку, если в строке нечего рисовать: пустые строки кадра пропускаем —
// экран уже очищен, и лишние байты на мобильном канале не нужны.
func (m *screenMirror) renderLine(y int, prev *uv.Style) (string, uv.Style) {
	return renderCells(m.cols, func(x int) *uv.Cell {
		c := m.em.CellAt(x, y)
		// ⚠ Широкая графема, которой не хватает места до края, в кадр НЕ идёт.
		// Так бывает только после сужения: и vt, и xterm обрезают строку по
		// новой ширине и оставляют графему в последней колонке без второй
		// половины. Напечатанная в кадре, она у клиента переносится на
		// следующую строку — а пустые строки кадр пропускает, и там она так и
		// оставалась мусором (стенд эквивалентности §6, seed 20261123;
		// разрыв frame-wide-last-column). Пустая клетка у края — меньшее зло.
		if c != nil && c.Width > 1 && x+c.Width > m.cols {
			return nil
		}
		return c
	}, prev)
}

// renderHistoryLine — то же для строки scrollback. Такая строка может быть
// КОРОЧЕ ширины экрана (библиотека обрезает хвостовые пустые ячейки, и за
// концом строки рисовать нечего) — или ДЛИННЕЕ: она напечатана в прежней
// геометрии PTY (окно exe 190 колонок, born 80), а зеркало уже сузилось до
// телефона. Рендерить её надо ЦЕЛИКОМ: клиентский xterm сам завернёт длинную
// строку, а обрезка по текущей ширине молча съедала правые части — «Codex и
// Kimi рвут консоль» (жалоба 13.08.2026: реплей 48-колоночного кольца в
// born-зеркале 80x24 и наоборот).
func renderHistoryLine(line uv.Line, width int, prev *uv.Style) (string, uv.Style) {
	if len(line) > width {
		width = len(line)
	}
	return renderCells(width, func(x int) *uv.Cell {
		if x >= len(line) {
			return nil
		}
		return &line[x]
	}, prev)
}

// renderCells — общее ядро renderLine и renderHistoryLine: рендерит строку
// ячеек в текст с SGR-диффами, начиная со стиля prev. Источник ячеек —
// cellAt (сетка экрана или линия scrollback), nil означает пустую ячейку.
// Одна логика на оба пути, чтобы они не разъехались: грабли вроде ECH-пробелов
// ловились именно расхождением «тут помним, там забыли».
func renderCells(width int, cellAt func(x int) *uv.Cell, prev *uv.Style) (string, uv.Style) {
	last := -1
	for x := width - 1; x >= 0; x-- {
		c := cellAt(x)
		if c == nil {
			continue
		}
		// Ячейка «пустая» только если это пробел БЕЗ фона: пробел с цветным
		// фоном — это нарисованная плашка, и терять её нельзя.
		if c.Content == " " && c.Style.IsZero() {
			continue
		}
		if c.Content == "" {
			continue
		}
		last = x
		break
	}
	if last < 0 {
		return "", *prev
	}

	var sb strings.Builder
	cur := *prev
	// Сколько ячеек занял последний напечатанный символ сверх своей: правая
	// половина широкого (CJK, эмодзи) в сетке присутствует, но рисовать её не
	// надо — её уже закрыла левая.
	skip := 0
	for x := 0; x <= last; x++ {
		if skip > 0 {
			skip--
			continue
		}
		c := cellAt(x)
		if c == nil {
			sb.WriteString(" ")
			continue
		}
		if !c.Style.Equal(&cur) {
			sb.WriteString(c.Style.Diff(&cur))
			cur = c.Style
		}
		// ⚠ ПУСТУЮ ЯЧЕЙКУ НАДО ПЕЧАТАТЬ ПРОБЕЛОМ, А НЕ ПРОПУСКАТЬ. Ячейка с
		// нулевой шириной и пустым содержимым — это не «нечего рисовать», а
		// затёртое место: так выглядит результат ECH (`CSI n X`, «стереть n
		// символов»), которым агенты гасят хвост строки. Пропуск такой ячейки
		// съезжал всю строку ВЛЕВО на один символ — сквозная проба поймала это
		// на боевом дампе как «got[x] == want[x+1]» после `ESC[29X`.
		if c.Content == "" {
			sb.WriteString(" ")
			continue
		}
		sb.WriteString(c.Content)
		if c.Width > 1 {
			skip = c.Width - 1
		}
	}
	return sb.String(), cur
}
