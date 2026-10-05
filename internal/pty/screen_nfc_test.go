package pty

import (
	"strings"
	"testing"
)

// NFD-пары (так пишет имена файлов macOS) зеркало собирает до эмулятора: vt
// пишет несамостоятельный знак в клетку под курсором, и без композиции акцент
// пропадал из кадра после переподключения (замер 14.09, см. composeForMirror).
// Нарезки выбраны так, чтобы база и знак приходили одним куском, в том числе
// когда кусок кончается знаком (путь придержанного хвоста). Разрез ровно между
// базой и знаком — известный остаток (разрыв vt-combining-split стенда).
func TestScreenMirrorComposesNFDLikeXterm(t *testing.T) {
	acute := string(rune(0x301))
	breve := string(rune(0x306))
	diaer := string(rune(0x308))
	eAcute := string(rune(0xE9))
	cases := []struct {
		name, stream, row, cursor string
		chunks                    []int
	}{
		// Последовательная печать: «é» в одной клетке, курсор за «dc». Кусок 10
		// кончается ровно после знака.
		{"sequential", "L243 7e" + "e" + acute + "dc", "L243 7e" + eAcute + "dc", "\x1b[1;11H", []int{0, 10}},
		// Затирание: «é» ложится в колонку 0, «313 J» на месте, как у xterm.
		// Поток кончается знаком — база обязана ждать вместе с ним.
		{"overwrite", "L313 J\x1b[1;1He" + acute, eAcute + "313 J", "\x1b[1;2H", []int{0, 12}},
		// Русские «й» и «ё» в NFD; кусок 4 кончается после каждого знака.
		{"cyrillic", "и" + breve + "е" + diaer + "ж", "йёж", "\x1b[1;4H", []int{0, 4}},
	}
	for _, c := range cases {
		for _, chunk := range c.chunks {
			frame := FrameFromStream([]byte(c.stream), 40, 3, chunk)
			if !strings.Contains(frame, "\x1b[1;1H"+c.row) || !strings.HasSuffix(frame, c.cursor) {
				t.Errorf("%s chunk=%d: кадр %q, want строку %q и курсор %q", c.name, chunk, frame, c.row, c.cursor)
			}
		}
	}
}

func TestSplitAtClusterBoundaryHoldsBaseWithMark(t *testing.T) {
	acute := string(rune(0x301))
	for _, c := range []struct{ in, tail string }{
		{"abc e" + acute, "e" + acute}, // база ждёт вместе со знаком
		{"abc\x07" + acute, acute},     // управляющий байт базой не бывает
		{"abc e", ""},                  // обычная буква без знака не ждёт
	} {
		if _, tail := splitAtClusterBoundary([]byte(c.in)); string(tail) != c.tail {
			t.Errorf("splitAtClusterBoundary(%q) tail=%q, want %q", c.in, tail, c.tail)
		}
	}
}

func TestComposeForMirrorLeavesOtherBytesAlone(t *testing.T) {
	for _, s := range []string{
		"plain ascii\r\n",
		"\x1b[31mred\x1b[m \x1b]0;title\x07",
		"жук € 中文",
		"\xff\xfe broken utf-8",
	} {
		if got := string(composeForMirror([]byte(s))); got != s {
			t.Errorf("composeForMirror(%q)=%q, want без изменений", s, got)
		}
	}
	// Несоставимая пара остаётся как есть: у «q» нет формы с акутом.
	q := "q" + string(rune(0x301))
	if got := string(composeForMirror([]byte(q))); got != q {
		t.Errorf("composeForMirror(%q)=%q", q, got)
	}
}
