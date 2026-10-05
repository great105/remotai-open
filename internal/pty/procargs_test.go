package pty

import (
	"encoding/binary"
	"testing"
)

// buildProcArgs2 собирает буфер в формате kern.procargs2: argc, exe\0, паддинг,
// argv по \0, потом окружение.
func buildProcArgs2(exe string, argv []string, env []string) []byte {
	buf := make([]byte, 4)
	binary.LittleEndian.PutUint32(buf, uint32(len(argv)))
	buf = append(buf, exe...)
	buf = append(buf, 0, 0, 0) // завершающий NUL + выравнивание
	for _, a := range argv {
		buf = append(buf, a...)
		buf = append(buf, 0)
	}
	for _, e := range env {
		buf = append(buf, e...)
		buf = append(buf, 0)
	}
	return buf
}

func TestParseProcArgs2(t *testing.T) {
	// Gemini на маке: node из PATH-симлинка npm, как на Linux (живой стенд 07.09.2026).
	buf := buildProcArgs2("/usr/local/bin/node", []string{"node", "/usr/local/bin/gemini"}, []string{"HOME=/Users/u", "PATH=/usr/bin"})
	got := parseProcArgs2(buf)
	want := "/usr/local/bin/node node /usr/local/bin/gemini"
	if got != want {
		t.Fatalf("parseProcArgs2 = %q, want %q", got, want)
	}
	if kind := matchAgentCmdline(got); kind != "gemini" {
		t.Fatalf("matchAgentCmdline(%q) = %q, want gemini", got, kind)
	}
	// Окружение не должно утечь в командную строку.
	if containsAny(got, "HOME=", "PATH=") {
		t.Fatalf("environment leaked into cmdline: %q", got)
	}
}

func TestParseProcArgs2Degenerate(t *testing.T) {
	if got := parseProcArgs2(nil); got != "" {
		t.Fatalf("nil buffer: %q", got)
	}
	if got := parseProcArgs2([]byte{1, 0, 0, 0}); got != "" {
		t.Fatalf("argc only: %q", got)
	}
	// argc больше, чем строк в буфере — берём сколько есть, не паникуем.
	buf := buildProcArgs2("/bin/sh", []string{"sh"}, nil)
	binary.LittleEndian.PutUint32(buf, 9)
	if got := parseProcArgs2(buf); got != "/bin/sh sh" {
		t.Fatalf("argc overflow: %q", got)
	}
	// Отрицательный argc — мусор, не команда.
	binary.LittleEndian.PutUint32(buf, 0xFFFFFFFF)
	if got := parseProcArgs2(buf); got != "" {
		t.Fatalf("negative argc: %q", got)
	}
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if len(sub) > 0 && indexString(s, sub) >= 0 {
			return true
		}
	}
	return false
}

func indexString(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
