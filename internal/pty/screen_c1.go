package pty

import (
	"bytes"
	"unicode/utf8"
)

// dropStrayC1 выбрасывает одиночные байты 0x80–0x9F — C1 в восьмибитной
// форме вне UTF-8 — ровно как xterm клиента.
//
// ⚠ ЗАЧЕМ. xterm декодирует поток как UTF-8, и байт, не образующий
// допустимой последовательности, просто пропадает (замер @xterm/headless
// 6.0.0, 15.09: «A\x88B» → «AB», «A\x9b2CX» → «A2CX», «A\x90xyz\x9cB» →
// «AxyzB»). vt те же байты ИСПОЛНЯЕТ как C1: 0x9B — CSI, 0x90 — DCS, 0x88 —
// HTS. Последнее — ещё и обход предохранителя P2/P3 (screen_vtpanic.go):
// HTS у vt зовётся через закрытый обработчик управляющего символа, свой туда
// не поставить, и при курсоре за правым краем после сужения (DECRC, ?1049l)
// TabStops.Set падал за концом массива (скептик волны 4). Откуда байт в
// жизни: «€» в cp1251, `cat` двоичного файла. 0xA0–0xFF vt выбрасывает сам,
// как xterm, — их не трогаем. U+0088 в UTF-8 (C2 88) — допустимая руна, её
// не трогаем тоже.
//
// owed — сколько байт продолжения ещё должна последовательность UTF-8,
// оборванная концом прошлого куска (vt склеит её сам — его разборщик держит
// начало между записями); возвращается то же для конца этого куска. Обычно
// ноль: splitAtClusterBoundary придерживает оборванный хвост до продолжения.
//
// Нечего выбрасывать — возвращается сам b, без копии: это горячий путь всего
// вывода терминала.
func dropStrayC1(b []byte, owed int) ([]byte, int) {
	start := 0
	for start < len(b) && owed > 0 && !utf8.RuneStart(b[start]) {
		start++
		owed--
	}
	if start == len(b) {
		// Кусок целиком ушёл в продолжение прошлой руны, и она всё ещё должна
		// байты: остаток переносится дальше, иначе следующее продолжение той
		// же руны выброшено как одиночный C1 (ревью волны 5: «∈» = E2 88 88
		// тремя кусками по байту).
		return b, owed
	}
	stray := false
	for _, c := range b[start:] {
		if c >= 0x80 && c < 0xa0 {
			stray = true
			break
		}
	}
	if !stray {
		return b, openUTF8Tail(b[start:])
	}
	var out []byte
	i := start
	for i < len(b) {
		c := b[i]
		if c < utf8.RuneSelf {
			if out != nil {
				out = append(out, c)
			}
			i++
			continue
		}
		if !utf8.FullRune(b[i:]) {
			// Допустимое начало, оборванное концом куска: продолжение придёт.
			if out != nil {
				out = append(out, b[i:]...)
			}
			return keptOrCopy(b, out), utf8SeqLen(c) - (len(b) - i)
		}
		r, size := utf8.DecodeRune(b[i:])
		if r == utf8.RuneError && size == 1 && c < 0xa0 {
			if out == nil {
				out = append(make([]byte, 0, len(b)), b[:i]...)
			}
			i++
			continue
		}
		if out != nil {
			out = append(out, b[i:i+size]...)
		}
		i += size
	}
	return keptOrCopy(b, out), 0
}

func keptOrCopy(b, out []byte) []byte {
	if out == nil {
		return b
	}
	return out
}

// openUTF8Tail — сколько байт продолжения недостаёт последовательности,
// оборванной концом b (0 — конец цельный).
func openUTF8Tail(b []byte) int {
	for k := len(b) - 1; k >= 0 && k >= len(b)-utf8.UTFMax; k-- {
		if !utf8.RuneStart(b[k]) {
			continue
		}
		if utf8.FullRune(b[k:]) {
			return 0
		}
		return utf8SeqLen(b[k]) - (len(b) - k)
	}
	return 0
}

// utf8SeqLen — длина последовательности по ведущему байту.
func utf8SeqLen(lead byte) int {
	switch {
	case lead >= 0xf0:
		return 4
	case lead >= 0xe0:
		return 3
	case lead >= 0xc0:
		return 2
	}
	return 1
}

// risSeq — RIS (полный сброс). Восьмибитной формы у него нет.
var risSeq = []byte("\x1bc")

// lastRIS — начало последнего RIS в b или -1. ESC внутри строки (OSC, DCS)
// у vt, xterm и boundaryParser одинаково обрывает строку, так что найденный
// RIS исполнится и там (разбор скептика волны 4).
func lastRIS(b []byte) int { return bytes.LastIndex(b, risSeq) }
