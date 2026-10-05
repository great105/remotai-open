package pty

import (
	"strings"
	"testing"
)

// Уведомление «в терминале мелькнула ошибка» без самой строки неотличимо от
// красного вывода тестов (#60): событие обязано нести первую сработавшую
// строку — очищенную от ANSI и обрезанную.
func TestErrorHintFrom(t *testing.T) {
	tests := []struct {
		name  string
		chunk string
		want  string
	}{
		{
			name:  "цвет и перевод строки сняты",
			chunk: "building…\r\n\x1b[31mError: build failed\x1b[0m\r\nmore output\r\n",
			want:  "Error: build failed",
		},
		{
			name:  "берётся первая сработавшая строка",
			chunk: "go: downloading\r\nError: cannot find module 'foo'\r\npanic: later\r\n",
			want:  "Error: cannot find module 'foo'",
		},
		{
			name:  "отступ и лишние пробелы схлопнуты",
			chunk: "   FAIL   internal/web    0.2s\r\n",
			want:  "FAIL internal/web 0.2s",
		},
		{
			name:  "совпадений нет — показывать нечего",
			chunk: "ok  \tinternal/web\t0.2s\r\n",
			want:  "",
		},
	}
	for _, test := range tests {
		if got := errorHintFrom([]byte(test.chunk)); got != test.want {
			t.Errorf("%s: hint=%q, ожидали %q", test.name, got, test.want)
		}
	}
}

// Длинная строка (стек, простыня линкера) обрезается: в уведомлении читают
// первую фразу, а не килобайт вывода.
func TestErrorHintFromTruncates(t *testing.T) {
	long := "Error: " + strings.Repeat("очень длинная причина падения сборки ", 120)
	got := []rune(errorHintFrom([]byte(long + "\n")))
	if len(got) > errorHintLimit+1 || len(got) < errorHintLimit {
		t.Fatalf("длина=%d, ожидали около %d", len(got), errorHintLimit)
	}
	if got[len(got)-1] != '…' {
		t.Fatalf("обрезанная строка обязана заканчиваться многоточием: %q", string(got))
	}
}
