package main

import "testing"

// Разбор флагов `remotai send`: позиционный текст, --file, --text, ошибки.
func TestParseSendArgs(t *testing.T) {
	cases := []struct {
		name     string
		args     []string
		wantText string
		wantFile string
		wantErr  bool
	}{
		{"текст позиционный", []string{"готово"}, "готово", "", false},
		{"файл", []string{"--file", "/tmp/a.pdf"}, "", "/tmp/a.pdf", false},
		{"файл и текст", []string{"--file", "x.log", "--text", "смотри"}, "смотри", "x.log", false},
		{"короткие флаги", []string{"-f", "x", "-t", "y"}, "y", "x", false},
		{"пусто — ошибка", nil, "", "", true},
		{"флаг без значения", []string{"--file"}, "", "", true},
		{"неизвестный флаг", []string{"--wat"}, "", "", true},
		{"два текста", []string{"a", "b"}, "", "", true},
	}
	for _, c := range cases {
		got, err := parseSendArgs(c.args)
		if (err != nil) != c.wantErr {
			t.Errorf("%s: err=%v, wantErr=%v", c.name, err, c.wantErr)
			continue
		}
		if err == nil && (got.text != c.wantText || got.file != c.wantFile) {
			t.Errorf("%s: got %+v, want text=%q file=%q", c.name, got, c.wantText, c.wantFile)
		}
	}
}
