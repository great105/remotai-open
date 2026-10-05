package pty

import (
	"fmt"
	"strings"
)

// decModesTracked — набор DEC private-режимов, которые имеет смысл восстановить
// после полного reset (их включающая последовательность могла вытесниться из
// буфера). Прочие режимы игнорируем, чтобы карта оставалась маленькой.
var decModesTracked = map[int]bool{
	47: true, 1047: true, 1048: true, 1049: true, // alternate screen buffer
	1000: true, 1002: true, 1003: true, // mouse tracking (normal/button/any)
	1004: true,                         // focus reporting
	1005: true, 1006: true, 1015: true, // mouse encoding (utf8/SGR/urxvt)
	2004: true, // bracketed paste
}

// decModeReassertOrder — порядок до-сылки: alt-screen первым, чтобы переигранный
// контент лёг в alt-буфер, затем мышь и остальное.
var decModeReassertOrder = []int{47, 1047, 1048, 1049, 1000, 1002, 1003, 1004, 1005, 1006, 1015, 2004}

// decTracker — сканер DEC private-режимов (CSI ? Pm h/l, DECSET/DECRST) по
// сырому потоку вывода. Маленький автомат тянет незавершённую
// последовательность через границы chunk'ов. НЕ потокобезопасен — вызывающий
// держит свой мьютекс (Session — bufMu сервера, pty-host — свой bufMu).
//
// Живёт в ДВУХ местах: в Session (сервер remotai.exe, смертен при
// автообновлении → подстрахован персистом в pty.json) и в pty-host (переживает
// рестарты сервера и видит ВСЕ байты с рождения сессии — авторитетный
// источник, отдаёт набор в HelloMsg.Modes при attach).
type decTracker struct {
	modes  map[int]bool
	state  int    // 0=idle, 1=ESC, 2=ESC[, 3=сбор параметров после ESC[?, 4=ESC[! (ждём p: DECSTR)
	params []byte // цифры/«;» в состоянии 3 (может тянуться через chunk'и)
	dirty  bool   // набор менялся с последнего takeDirty (для write-through персиста)
}

func newDecTracker() *decTracker { return &decTracker{modes: make(map[int]bool)} }

// scan обновляет режимы по сырому выводу.
func (d *decTracker) scan(data []byte) {
	for _, b := range data {
		switch d.state {
		case 0:
			if b == 0x1b {
				d.state = 1
			}
		case 1:
			switch b {
			case '[':
				d.state = 2
			case 'c':
				// RIS (Reset to Initial State) resets DEC private modes too. It
				// is the authoritative escape hatch after a truncated-tail mirror
				// recovery: stale seeded alt/mouse modes must not live forever.
				d.resetTracked()
				d.state = 0
			case 0x1b:
				// stay
			default:
				d.state = 0
			}
		case 2:
			switch b {
			case '?':
				d.state = 3
				d.params = d.params[:0]
			case '!':
				d.state = 4
			case 0x1b:
				d.state = 1
			default:
				d.state = 0 // другой CSI — нас не интересует
			}
		case 4:
			if b == 'p' {
				d.softReset()
			}
			if b == 0x1b {
				d.state = 1
			} else {
				d.state = 0
			}
		case 3:
			if (b >= '0' && b <= '9') || b == ';' {
				if len(d.params) < 64 { // защита от роста на мусоре
					d.params = append(d.params, b)
				}
				continue
			}
			if b == 'h' || b == 'l' {
				d.apply(d.params, b == 'h')
			}
			if b == 0x1b {
				d.state = 1
			} else {
				d.state = 0
			}
		}
	}
}

// softReset — DECSTR (CSI ! p). xterm.js сбрасывает decPrivateModes к
// значениям по умолчанию (coreService.reset): из отслеживаемых это bracketed
// paste и focus events. Мышь, её кодировка и alt-экран DECSTR не трогает.
// Без этого кадр после переподключения снова включал ?2004 и ?1004, которые
// приложение уже сбросило (стенд эквивалентности, фикстура decstr-dec-tracker).
func (d *decTracker) softReset() {
	for _, n := range []int{1004, 2004} {
		if d.modes[n] {
			delete(d.modes, n)
			d.dirty = true
		}
	}
}

// dropInputModes снимает режимы, в которых терминал сам шлёт программе байты:
// мышь, фокус, bracketed paste. Нужен усыплению: снятый агент не успевает их
// выключить, и шелл получал `ESC[O`/`ESC[I` от смены фокуса телефона — в
// PowerShell от них оставалось `[O` перед командой пробуждения (жалоба
// владельца 29.09, скриншот). Alt-экран не трогаем — это сверка с зеркалом.
func (d *decTracker) dropInputModes() {
	for _, n := range []int{1000, 1002, 1003, 1004, 1005, 1006, 1015, 2004} {
		if d.modes[n] {
			delete(d.modes, n)
			d.dirty = true
		}
	}
}

func (d *decTracker) resetTracked() {
	if len(d.modes) > 0 {
		clear(d.modes)
		d.dirty = true
	}
	d.params = d.params[:0]
}

// apply выставляет/снимает отслеживаемые режимы из params вида "1049" или
// "1000;1006". dirty — только при реальном изменении набора.
func (d *decTracker) apply(params []byte, set bool) {
	start := 0
	for i := 0; i <= len(params); i++ {
		if i == len(params) || params[i] == ';' {
			if i > start {
				if n := atoiBytes(params[start:i]); n > 0 && decModesTracked[n] && !xtermIgnoresMode(n) {
					if set {
						// Mouse tracking and mouse encoding are enums in xterm,
						// despite being represented by separate DECSET numbers. A
						// later SET replaces its peer; retaining both would replay
						// static-order state rather than the last mode on the wire.
						for _, peer := range exclusiveModePeers(n) {
							if peer != n && d.modes[peer] {
								delete(d.modes, peer)
								d.dirty = true
							}
						}
						if !d.modes[n] {
							d.modes[n] = true
							d.dirty = true
						}
					} else {
						// xterm.js гасит трекинг мыши целиком на DECRST ЛЮБОГО
						// члена семейства (activeProtocol=NONE): после ?1002h
						// ?1000l кадр не должен снова включать drag-трекинг.
						for _, off := range resetModePeers(n) {
							if d.modes[off] {
								delete(d.modes, off)
								d.dirty = true
							}
						}
					}
				}
			}
			start = i + 1
		}
	}
}

func exclusiveModePeers(mode int) []int {
	switch mode {
	case 1000, 1002, 1003:
		return []int{1000, 1002, 1003}
	case 1005, 1006, 1015:
		return []int{1005, 1006, 1015}
	default:
		return nil
	}
}

// resetModePeers — что гасит DECRST n в xterm.js 6.0.0 (замер на
// @xterm/headless 14.09.2026: ?1002h ?1000l и ?1003h ?1002l дают трекинг none).
// Кодировка 1006 сбрасывается сама по себе.
func resetModePeers(mode int) []int {
	switch mode {
	case 1000, 1002, 1003:
		return []int{1000, 1002, 1003}
	default:
		return []int{mode}
	}
}

// xtermIgnoresMode — режимы, которые xterm.js 6.0.0 не поддерживает вовсе
// (кодировки мыши UTF-8 1005 и urxvt 1015, xterm.js #2507): и SET, и RESET у
// клиента ничего не меняют. Отслеживать их значило бы вытеснять из кадра
// действующую SGR-кодировку 1006 (?1006h ?1015h у xterm остаётся SGR).
func xtermIgnoresMode(mode int) bool {
	return mode == 1005 || mode == 1015
}

// altActive — рисует ли приложение в альтернативном экране (полноэкранный TUI:
// агенты, vim, less). У такого экрана нет прокручиваемой истории, и КАЖДАЯ смена
// размера заставляет его перерисовать себя целиком — см. targetSizeLocked, где
// от этого зависит, трогать ли размер осиротевшей сессии.
func (d *decTracker) altActive() bool {
	return d.modes[1049] || d.modes[1047] || d.modes[47]
}

// snapshot возвращает активные режимы в стабильном порядке (decModeReassertOrder).
func (d *decTracker) snapshot() []int {
	if len(d.modes) == 0 {
		return nil
	}
	out := make([]int, 0, len(d.modes))
	for _, n := range decModeReassertOrder {
		if d.modes[n] {
			out = append(out, n)
		}
	}
	return out
}

// seed выставляет набор режимов извне (host/pty.json на reattach), отфильтровав
// неотслеживаемые. Не трогает dirty — сидирование не «изменение».
func (d *decTracker) seed(modes []int) {
	for _, n := range modes {
		if decModesTracked[n] {
			d.modes[n] = true
		}
	}
}

// takeDirty отдаёт и сбрасывает флаг «набор менялся».
func (d *decTracker) takeDirty() bool {
	was := d.dirty
	d.dirty = false
	return was
}

// reassertSeq возвращает SET-последовательности всех активных режимов, чтобы
// клиент после reset-resync заново вошёл в alt-screen / mouse / bracketed-paste
// (вытесненные включающие байты этого уже не несут). Пусто для обычного shell.
func (d *decTracker) reassertSeq() string {
	if len(d.modes) == 0 {
		return ""
	}
	var sb strings.Builder
	for _, n := range decModeReassertOrder {
		if d.modes[n] {
			fmt.Fprintf(&sb, "\x1b[?%dh", n)
		}
	}
	return sb.String()
}

// decModeAltSwitches — переключатели альтернативного экрана. Гасить их можно
// ТОЛЬКО когда alt действительно выключен: у клиента, который сейчас в
// alt-screen, любой из этих RESET'ов уносит нарисованный кадр.
var decModeAltSwitches = []int{47, 1047, 1048, 1049}

// dropAlt вычищает alt-переключатели из набора. Нужен сверке с зеркалом
// (Session.ModeReassertSeq): смерть процесса без управляющего выхода не видна
// сканеру, поэтому TUI, вышедший без ?1049l/RIS, оставался «в альте» — и мусор
// реассертился клиенту на каждом маркере. Возвращает, было ли что чистить.
func (d *decTracker) dropAlt() bool {
	changed := false
	for _, n := range decModeAltSwitches {
		if d.modes[n] {
			delete(d.modes, n)
			d.dirty = true
			changed = true
		}
	}
	return changed
}

func isAltSwitch(n int) bool {
	for _, a := range decModeAltSwitches {
		if a == n {
			return true
		}
	}
	return false
}

// syncSeq возвращает полную синхронизацию режимов: SET для активных и RESET
// для неактивных из отслеживаемого набора. Применяется при gap-резюме, где
// пропущенная середина потока могла выключить режим (клиент застрял в
// alt-screen) так же легко, как и включить.
//
// ⚠ Порядок и состав здесь — не косметика, а сохранность картинки. Раньше
// последовательность шла строго по decModeReassertOrder и потому НАЧИНАЛАСЬ с
// `ESC[?47l ESC[?1047l ESC[?1048l` — а в xterm.js это activateNormalBuffer()
// с очисткой alt-буфера. Клиент, вернувшийся с потерянной серединой потока,
// имел на экране правильный кадр агента — и мы своей же синхронизацией стирали
// его дочиста, ещё до того как `ESC[?1049h` вернёт его в alt. Замер (проба на
// @xterm/headless 6.0.0, боевой набор Claude Code [1049,1000,1002,1003,1004,
// 1006,2004]): кадр из трёх строк → 0 строк после syncSeq и 3 строки после
// reassertSeq. Поэтому active alt включаем первым и никогда не гасим
// его peer-режимы; при normal гасим все alt-варианты. Неячеечные режимы
// синхронизирует syncNonAltSeq: inactive RESET перед active SET,
// иначе xterm-энумы mouse tracking/encoding сбросят только что выбранный режим.
func (d *decTracker) syncSeq() string {
	var sb strings.Builder
	alt := d.altActive()
	for _, n := range decModeReassertOrder {
		if isAltSwitch(n) && d.modes[n] {
			fmt.Fprintf(&sb, "\x1b[?%dh", n)
		}
	}
	if !alt {
		for _, n := range decModeReassertOrder {
			if isAltSwitch(n) {
				fmt.Fprintf(&sb, "\x1b[?%dl", n)
			}
		}
	}
	sb.WriteString(d.syncNonAltSeq())
	return sb.String()
}

// syncNonAltSeq serializes client modes that a screen grid cannot express.
// Alt-buffer selection is handled explicitly by screenMirror.frameLocked so a
// stale client is first unwound from every 47/1047/1049 variant and then put
// into one canonical buffer. Mouse/focus/encoding/bracketed-paste modes still
// need both SET and RESET commands to make a frame self-contained on clean and
// dirty clients alike. RESETs go first: xterm treats DECRST in the exclusive
// mouse tracking/encoding families as "none", so a later inactive reset would
// otherwise erase the active mode we had just set.
func (d *decTracker) syncNonAltSeq() string {
	var sb strings.Builder
	for _, n := range decModeReassertOrder {
		if isAltSwitch(n) || d.modes[n] {
			continue
		}
		fmt.Fprintf(&sb, "\x1b[?%dl", n)
	}
	for _, n := range decModeReassertOrder {
		if isAltSwitch(n) || !d.modes[n] {
			continue
		}
		fmt.Fprintf(&sb, "\x1b[?%dh", n)
	}
	return sb.String()
}

// atoiBytes парсит неотрицательное целое из байтов-цифр; -1 при мусоре.
func atoiBytes(b []byte) int {
	if len(b) == 0 || len(b) > 9 {
		return -1
	}
	n := 0
	for _, c := range b {
		if c < '0' || c > '9' {
			return -1
		}
		n = n*10 + int(c-'0')
	}
	return n
}
