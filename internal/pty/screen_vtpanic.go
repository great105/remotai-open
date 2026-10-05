package pty

import (
	"fmt"
	"log"
	"runtime"
	"strings"
	"sync/atomic"

	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/vt"
)

// Паники эмулятора vt и страховка от них.
//
// ЗАЧЕМ. Зеркало кормится из горутины sessionScreen (session_screen.go), а
// придержанный хвост доливается и из горутины писателя (Frame, SnapshotAt), и
// recover не было ни там, ни там: паника библиотеки роняла ВЕСЬ процесс
// агента со всеми сессиями. Стенд эквивалентности §6 (15.09.2026) нашёл две
// такие паники на входе, который обычное приложение шлёт в гонке со сменой
// геометрии; разбор нашёл ещё две того же рода (все четыре — «index out of
// range» в vt/ultraviolet):
//
//	P1: DECSTBM с низом больше высоты (ESC[2;6r на четырёх строках) — vt не
//	    зажимает низ, и DL/IL/LF/SU/SD в такой области читают строку за концом
//	    буфера. Естественно возникает сразу после уменьшения высоты: приложение
//	    ещё рисует под прежние строки.
//	P2: resize в alt-экране зажимает курсор только АКТИВНОГО экрана; после
//	    ?1049l курсор normal за новой шириной, и HTS (ESC H) пишет табуляцию за
//	    концом TabStops.
//	P3: то же через DECRC: курсор, сохранённый до сужения, восстанавливается
//	    как есть (Screen.RestoreCursor не зажимает). TBC 0 (CSI g) падает там же.
//	P4: DECSLRM (при ?69h) с правым полем больше ширины — ICH/DCH за концом
//	    строки. От resize не зависит.
//
// ДВА СЛОЯ.
//
//  1. Предохранители (registerVtGuards): свои обработчики, которые vt зовёт
//     РАНЬШЕ штатных, чинят вход так, как его понял бы xterm: низ области и
//     правое поле зажимаются по экрану (xterm делает то же), HTS и TBC 0 при
//     курсоре за правым краем не делают ничего (у xterm курсор там не бывает,
//     а табуляция в последней колонке ненаблюдаема). Восьмибитный HTS (байт
//     0x88) идёт мимо обработчиков — его, как и прочие одиночные C1, до vt не
//     допускает dropStrayC1 (screen_c1.go): xterm их выбрасывает.
//  2. Страховочная сетка (feedEmulatorLocked, resizeEmulatorLocked): recover
//     на КАЖДОМ входе байтов и resize в эмулятор. Паника — неизвестный дефект
//     библиотеки, и состояние эмулятора после неё не доказано ничем. Поэтому:
//     счётчик, строка журнала БЕЗ содержимого вывода (операция, место в
//     библиотеке, геометрия — I-15), новый эмулятор в текущей геометрии с теми
//     же обработчиками и пометка untrusted до ближайшего RIS, который разобрал
//     уже новый эмулятор. Пока пометка стоит, кадра и истории нет
//     (snapshotReadyLocked, History): клиенту screen-request-v1 — screen-none
//     «unavailable», старому клиенту — прежнее молчание, и он остаётся на сыром
//     потоке, который от зеркала не зависит вовсе.
//
// ⚠ ПОЧЕМУ ДО RIS, А НЕ «ПЕРЕСОБРАТЬ И ЖИТЬ ДАЛЬШЕ». Новый эмулятор пуст, а
// агентские TUI рисуют диффом по ячейкам (шапка screen.go): кадр из него
// затёр бы у клиента правильный экран пустотой с обрывками. Полный кадр снова
// честен только после полного сброса терминала. Пересборка из кольца тут не
// помогает: в кольце тот же вход, на котором библиотека уже упала.

// screenMirrorVtPanics — паники эмулятора vt во всех зеркалах процесса.
var screenMirrorVtPanics atomic.Uint64

// installEmulatorLocked ставит новый эмулятор в текущей геометрии зеркала со
// всеми нашими обработчиками. Зовут newScreenMirror и пересборка после паники:
// обработчики обязаны быть одни и те же, иначе пересобранное зеркало
// разбирало бы поток иначе, чем рождённое. Обработчики берут em из замыкания,
// а не m.em: у каждого эмулятора свои.
func (m *screenMirror) installEmulatorLocked() {
	em := vt.NewEmulator(m.cols, m.rows)
	m.em = em
	// Режимы нового эмулятора — умолчания vt, трекер вместе с ними.
	m.modes = mirrorModes{}
	m.modes.attach(em)
	// ED3 (ESC[3J) у xterm стирает только историю прокрутки АКТИВНОГО буфера:
	// экран остаётся, а в alt-экране (истории нет) не происходит ничего. vt
	// перед очисткой истории стирает ещё и видимый экран — одиночный ESC[3J
	// гасил экран зеркала, и после переподключения человек видел пустоту
	// (разрыв ed3-vt-clears-screen стенда эквивалентности, снят 14.09). Свой
	// обработчик вызывается раньше штатного и, вернув true, заменяет его
	// только для ED3; ED0/1/2 идут штатным путём.
	em.RegisterCsiHandler('J', func(params ansi.Params) bool {
		if n, _, _ := params.Param(0, 0); n != 3 {
			return false
		}
		if !em.IsAltScreen() {
			em.ClearScrollback()
		}
		return true
	})
	// RIS (ESC c). xterm на полном сбросе стирает и историю прокрутки, vt —
	// только экраны (fullReset → Screen.Reset историю не трогает): после
	// `reset` снимок возвращал клиенту историю, которую приложение только что
	// сбросило (разрыв vt-ris-keeps-scrollback стенда, снят 15.09).
	// Screen.Reset в историю ничего не кладёт, поэтому чистить до штатного
	// сброса — то же, что после; false — чтобы fullReset отработал.
	//
	// Он же — единственное, что снимает пометку untrusted: полный сброс,
	// разобранный ЭТИМ эмулятором, делает кадр снова доказуемым.
	em.RegisterEscHandler('c', func() bool {
		em.ClearScrollback()
		m.untrusted = false
		return false
	})
	m.registerVtGuards(em)
	em.SetScrollbackSize(screenMirrorScrollback)
	done := make(chan struct{})
	m.drainDone = done
	go drainEmulator(em, m.drainStop, done)
}

// drainEmulator вычитывает и выбрасывает ответы эмулятора. Без неё Write
// однажды заблокируется навсегда и утащит за собой горутину-кормилицу — то
// есть терминал у человека замрёт, а причина будет выглядеть как «сеть».
// Своя горутина у каждого эмулятора: пересборка после паники закрывает
// старый, и его горутина выходит на EOF.
func drainEmulator(em *vt.Emulator, stop <-chan struct{}, done chan<- struct{}) {
	defer close(done)
	buf := make([]byte, 4096)
	for {
		select {
		case <-stop:
			return
		default:
		}
		n, err := em.Read(buf)
		if err != nil {
			return
		}
		if n == 0 {
			select {
			case <-stop:
				return
			default:
			}
		}
	}
}

// registerVtGuards — предохранители от паник P1–P4 (шапка файла). vt
// перебирает обработчики с последнего зарегистрированного, так что эти идут
// раньше штатных; false — пусть штатный отработает на исправленном входе.
//
// Низ области и правое поле правятся ПРЯМО В ПАРАМЕТРАХ: Params — окно в
// массив самого парсера (ansi.Parser.Params, unsafe.Slice), и штатный
// обработчик DECSTBM читает низ оттуда же (e.parser.Param). Так область
// ставится ровно как у xterm (setScrollRegion: низ больше высоты — высота).
func (m *screenMirror) registerVtGuards(em *vt.Emulator) {
	em.RegisterCsiHandler('r', func(params ansi.Params) bool {
		if !m.noVtGuards {
			clampParam(params, 1, em.Height())
		}
		return false
	})
	em.RegisterCsiHandler('s', func(params ansi.Params) bool {
		if !m.noVtGuards {
			clampParam(params, 1, em.Width())
		}
		return false
	})
	// Курсор за правым краем: HTS и TBC 0 адресуют табуляцию его колонкой.
	offRight := func() bool {
		return !m.noVtGuards && em.CursorPosition().X >= em.Width()
	}
	em.RegisterEscHandler('H', offRight)
	em.RegisterCsiHandler('g', func(params ansi.Params) bool {
		if n, _, _ := params.Param(0, 0); n != 0 {
			return false
		}
		return offRight()
	})
}

// clampParam зажимает i-й параметр сверху, сохраняя признак подпараметров.
func clampParam(params ansi.Params, i, limit int) {
	if v, more, ok := params.Param(i, 0); ok && v > limit {
		params[i] = ansi.Param(ansi.Parameter(limit, more))
	}
}

// feedEmulatorLocked — единственная дверь байтов в эмулятор (WriteAt, долив
// придержанного хвоста, пробы *FromStream). Сначала вход чистится так, как
// его поняла бы xterm клиента (dropStrayC1, screen_c1.go), потом пишется со
// страховкой.
//
// Пока зеркало недостоверно, эмулятор байтов НЕ получает: кадр вернёт только
// RIS, а всё до него полный сброс всё равно сотрёт. Поэтому из куска берётся
// хвост от ПОСЛЕДНЕГО RIS — состояние после него определено одними байтами за
// ним — и пишется в пустой пересобранный эмулятор. Это же и после паники в
// этом самом куске: где в куске упало, не знает никто, а RIS после места
// паники терять нельзя (иначе кадра не будет до следующего, который может не
// прийти никогда). Вход-убийца уже после RIS упадёт снова — пометка снова,
// без третьего круга.
//
// ⚠ Не больше двух пересборок на кусок и ни одной, пока RIS нет. Прежняя
// побайтовая переигровка пересобирала эмулятор на каждом входе-убийце в
// куске, а каждый новый эмулятор — это 4 МБ буфера разборщика (скептик
// волны 4: 200 входов-убийц в куске 6400 Б — 201 паника и 917 МБ выделений).
func (m *screenMirror) feedEmulatorLocked(b []byte) {
	if !m.noVtGuards {
		b, m.utf8Owed = dropStrayC1(b, m.utf8Owed)
	}
	// NFC — ПОСЛЕ фильтра C1: xterm выбрасывает одиночный байт ещё при
	// декодировании UTF-8 и приклеивает знак к базе, так что «e 0x88 U+0301»
	// у клиента — «é». Нормализация до фильтра видела 0x88 разделителем, не
	// соединяла пару, и vt затирал знак (ревью волны 5).
	b = composeForMirror(b)
	if len(b) == 0 {
		return
	}
	if !m.untrusted && m.writeEmulatorLocked(b) {
		return
	}
	if j := lastRIS(b); j >= 0 {
		m.writeEmulatorLocked(b[j:])
	}
}

func (m *screenMirror) writeEmulatorLocked(b []byte) (ok bool) {
	defer func() {
		if r := recover(); r != nil {
			m.vtPanicLocked("write", r)
			ok = false
		}
	}()
	_, _ = m.em.Write(b)
	return true
}

// resizeEmulatorLocked — resize эмулятора со страховкой. m.cols/m.rows уже
// новые: пересборка после паники рождает эмулятор сразу в них.
func (m *screenMirror) resizeEmulatorLocked(cols, rows int) {
	defer func() {
		if r := recover(); r != nil {
			m.vtPanicLocked("resize", r)
		}
	}()
	m.em.Resize(cols, rows)
}

// vtPanicLocked — пересборка зеркала после паники эмулятора (шапка файла).
// Зовётся из отложенной функции, пока стек паники ещё на месте: по нему
// vtPanicSite называет дефект.
func (m *screenMirror) vtPanicLocked(op string, r any) {
	total := screenMirrorVtPanics.Add(1)
	m.vtPanics++
	site := vtPanicSite()
	old, oldDone := m.em, m.drainDone
	m.installEmulatorLocked()
	m.untrusted = true
	// Старый эмулятор закрывается, его вычитывающая горутина выходит на EOF
	// (порядок без гонки внутри vt — stopEmulator, screen_vtstop.go).
	stopEmulator(old, oldDone)
	// Первые паники зеркала — поштучно, дальше каждая сотая: приложение,
	// повторяющее вход-убийцу на каждой перерисовке, иначе залило бы журнал.
	if m.vtPanics <= 3 || m.vtPanics%100 == 0 {
		log.Printf("[PTY] зеркало экрана: паника эмулятора vt при %s (%s, %s) — эмулятор пересобран %dx%d, кадр и история не выдаются до RIS; паник у зеркала %d, в процессе %d",
			op, vtPanicKind(r), site, m.cols, m.rows, m.vtPanics, total)
	}
}

// vtPanicSite — функция, в которой случилась паника: первая не-runtime
// функция под runtime.gopanic. Какой из дефектов vt сработал — без единого
// байта вывода.
func vtPanicSite() string {
	pcs := make([]uintptr, 48)
	frames := runtime.CallersFrames(pcs[:runtime.Callers(2, pcs)])
	panicking := false
	for {
		f, more := frames.Next()
		switch {
		case f.Function == "runtime.gopanic":
			panicking = true
		case panicking && !strings.HasPrefix(f.Function, "runtime."):
			return fmt.Sprintf("%s:%d", f.Function, f.Line)
		}
		if !more {
			return "?"
		}
	}
}

// vtPanicKind — что за паника, без её произвольного текста: сообщение
// runtime.Error содержит только индексы и длины, у остального — только тип.
func vtPanicKind(r any) string {
	if err, ok := r.(runtime.Error); ok {
		return err.Error()
	}
	return fmt.Sprintf("%T", r)
}
