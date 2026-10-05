package web

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestSanitizePtyName(t *testing.T) {
	long := strings.Repeat("я", 100)
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain", "релиз 2.18", "релиз 2.18"},
		{"trims spaces", "  фикс бота  ", "фикс бота"},
		{"strips newline", "фикс\nбота", "фиксбота"},
		{"strips ansi escape", "\x1b[31mкрасный\x1b[0m", "[31mкрасный[0m"},
		{"strips invalid utf8", "имя\xff\xfe", "имя"},
		{"empty stays empty", "   ", ""},
		{"clamps by runes", long, strings.Repeat("я", ptyNameMaxRunes)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := sanitizePtyName(c.in)
			if got != c.want {
				t.Fatalf("sanitizePtyName(%q) = %q, want %q", c.in, got, c.want)
			}
			if !utf8.ValidString(got) {
				t.Fatalf("sanitizePtyName(%q) produced invalid UTF-8: %q", c.in, got)
			}
		})
	}
}

// Регрессия: обрезка шла по БАЙТАМ — кириллическое имя длиннее 40 символов
// рвалось посреди UTF-8-последовательности и уходило битым в pty.json.
func TestSanitizePtyNameCyrillicNotCorrupted(t *testing.T) {
	in := strings.Repeat("тест", 30) // 120 рун, 240 байт
	got := sanitizePtyName(in)

	if !utf8.ValidString(got) {
		t.Fatalf("битый UTF-8 после обрезки: %q", got)
	}
	if n := utf8.RuneCountInString(got); n != ptyNameMaxRunes {
		t.Fatalf("обрезано до %d рун, ожидалось %d", n, ptyNameMaxRunes)
	}
	if want := strings.Repeat("тест", 20); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	// Старое поведение: in[:80] дало бы ровно 80 байт = 40 рун — вдвое короче.
	if utf8.RuneCountInString(in[:80]) == utf8.RuneCountInString(got) {
		t.Fatal("обрезка всё ещё считает байты, а не руны")
	}
}
