package pty

import (
	"strings"

	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/vt"
)

// mirrorModes — режимы, которые vt ведёт у себя (vt mode.go), но наружу не
// отдаёт, а клиенту после кадра они нужны: без них ХВОСТ после снимка ведёт
// себя иначе, чем у непрерывного терминала. Замер стенда раздела 6
// (apk/src/ptyTerm/snapshotConformance.test.ts, фикстуры cursor-hidden, decckm,
// deckpam, decom, decawm-off, irm): курсор, скрытый приложением, после кадра
// виден; стрелки уходят в другом коде (DECCKM); текст без переноса у края
// переносится (DECAWM); вставка IRM превращается в перезапись. ST-05, «дёшево,
// силами зеркала».
//
// ⚠ ОТДЕЛЬНО ОТ decTracker. decModesTracked общий с pty-host и pty.json
// (decmodes.go): новый режим там изменил бы HelloMsg.Modes и персист, и старый
// хост или клиент получили бы то, чего не ждут (I-14). Этот трекер живёт
// только в зеркале и влияет только на кадр.
//
// Источник — колбэки самого vt (EnableMode/DisableMode): кадр описывает
// состояние ЭМУЛЯТОРА, а не параллельного разбора потока, и события приходят
// в порядке байтов внутри em.Write. Нулевое значение — умолчания vt и xterm
// (курсор виден, перенос включён, остальное выключено).
type mirrorModes struct {
	cursorHidden bool // ?25 сброшен
	cursorKeys   bool // ?1 DECCKM
	keypad       bool // ?66 / DECKPAM (ESC =)
	origin       bool // ?6 DECOM
	noAutoWrap   bool // ?7 DECAWM сброшен
	insert       bool // IRM (ANSI 4)
}

func (t *mirrorModes) apply(mode ansi.Mode, on bool) {
	switch mode {
	case ansi.ModeTextCursorEnable:
		t.cursorHidden = !on
	case ansi.ModeCursorKeys:
		t.cursorKeys = on
	case ansi.ModeNumericKeypad:
		t.keypad = on
	case ansi.ModeOrigin:
		t.origin = on
	case ansi.ModeAutoWrap:
		t.noAutoWrap = !on
	case ansi.ModeInsertReplace:
		t.insert = on
	}
}

// attach подключает трекер к эмулятору. Колбэки vt зовёт изнутри em.Write,
// то есть под m.mu зеркала — отдельный замок не нужен.
func (t *mirrorModes) attach(em *vt.Emulator) {
	em.SetCallbacks(vt.Callbacks{
		EnableMode:  func(m ansi.Mode) { t.apply(m, true) },
		DisableMode: func(m ansi.Mode) { t.apply(m, false) },
	})
	// RIS. vt.fullReset сбрасывает режимы через resetModes и колбэки, но IRM
	// в его списке умолчаний нет — колбэк на него не придёт, и режим вставки
	// пережил бы полный сброс. Свой обработчик ESC c вызывается РАНЬШЕ
	// штатного (vt перебирает обработчики с последнего зарегистрированного) и
	// возвращает false, чтобы штатный fullReset всё равно отработал.
	em.RegisterEscHandler('c', func() bool {
		*t = mirrorModes{}
		return false
	})
	// DECSTR (CSI ! p, мягкий сброс). xterm.js (InputHandler.softReset)
	// сбрасывает DECCKM, DECKPAM, DECOM, IRM, показывает курсор и включает
	// DECAWM — ровно нулевое значение трекера. vt DECSTR не знает вовсе
	// (обработчика с промежуточным '!' нет): без этой строки трекер пережил бы
	// сброс, и кадр после переподключения снова выставил бы клиенту скрытый
	// курсор, вставку и прикладные стрелки, которых у непрерывного терминала
	// уже нет. Шлёт его, например, is2 терминфо xterm (`tput init`). Замер
	// ревью C-conformance; фикстура decstr в snapshotConformance.test.ts.
	// false — пусть vt отработает как умеет (сейчас: запишет «unhandled»).
	em.RegisterCsiHandler(ansi.Command(0, '!', 'p'), func(ansi.Params) bool {
		*t = mirrorModes{}
		return false
	})
}

// preDrawSeq — режимы, которые обязаны быть выключены, пока кадр РИСУЕТ: у
// грязного клиента (replace=false) оставшийся DECOM сдвинул бы адресацию
// строк от области прокрутки, а IRM раздвигал бы старое содержимое вместо
// перезаписи.
func (t *mirrorModes) preDrawSeq() string {
	return "\x1b[?6l\x1b[4l"
}

// seq — состояние режимов после кадра, и SET, и RESET: кадр самодостаточен
// и на чистом, и на грязном клиенте (как syncNonAltSeq). Порядок значим:
// DECSET 6 в xterm переводит курсор в начало области, поэтому DECOM
// выставляется ПОСЛЕ отрисовки и ДО финальной адресации курсора; ?25 — там
// же, чтобы последней в кадре оставалась абсолютная позиция курсора.
func (t *mirrorModes) seq() string {
	var sb strings.Builder
	mode := func(private bool, n string, on bool) {
		sb.WriteString("\x1b[")
		if private {
			sb.WriteByte('?')
		}
		sb.WriteString(n)
		if on {
			sb.WriteByte('h')
		} else {
			sb.WriteByte('l')
		}
	}
	mode(true, "1", t.cursorKeys)
	mode(true, "66", t.keypad)
	mode(true, "7", !t.noAutoWrap)
	mode(false, "4", t.insert)
	mode(true, "6", t.origin)
	mode(true, "25", !t.cursorHidden)
	return sb.String()
}
