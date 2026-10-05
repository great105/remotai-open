package pty

import (
	"strings"
	"testing"
)

// ED3 (ESC[3J) у xterm стирает только историю активного буфера. До 14.09 vt
// гасил ещё и видимый экран зеркала, и после переподключения человек видел
// пустоту там, где у xterm из потока остался текст (стенд эквивалентности,
// фикстуры ed3-alone и ed3-in-alt).
func TestScreenMirrorED3KeepsScreenLikeXterm(t *testing.T) {
	stream := "L1\r\nL2\r\nL3\r\nL4\r\nL5\x1b[3J"
	if frame := FrameFromStream([]byte(stream), 20, 3, 0); !strings.Contains(frame, "L5") || !strings.Contains(frame, "L3") {
		t.Fatalf("ED3 погасил экран зеркала: %q", frame)
	}
	// В alt-экране истории нет, и xterm на ED3 не делает ничего.
	alt := "shell\x1b[?1049h\x1b[HALT\x1b[3J"
	if frame := FrameFromStream([]byte(alt), 20, 3, 0); !strings.Contains(frame, "ALT") {
		t.Fatalf("ED3 в alt-экране стёр экран: %q", frame)
	}
}

// RIS (ESC c) у xterm стирает и историю прокрутки; vt — только экраны, и после
// `reset` снимок возвращал клиенту историю, которую приложение только что
// сбросило (разрыв vt-ris-keeps-scrollback стенда, снят 15.09; фикстура
// ris-keeps-history в snapshotConformance.test.ts).
func TestScreenMirrorRISClearsHistoryLikeXterm(t *testing.T) {
	m := newScreenMirror(20, 3)
	defer m.Close()
	m.Write([]byte("L1\r\nL2\r\nL3\r\nL4\r\nL5"))
	if _, n := m.History(100); n <= 3 {
		t.Fatalf("до RIS история должна быть глубже экрана, строк %d", n)
	}
	m.Write([]byte("\x1bc$ "))
	history, n := m.History(100)
	if n != 3 || strings.Contains(history, "L") || !strings.Contains(history, "$") {
		t.Fatalf("после RIS: строк %d, история %q — want только видимые строки с приглашением", n, history)
	}
}

func TestScreenMirrorED3ClearsOnlyHistory(t *testing.T) {
	m := newScreenMirror(20, 3)
	m.Write([]byte("L1\r\nL2\r\nL3\r\nL4\r\nL5"))
	if _, n := m.History(100); n <= 3 {
		t.Fatalf("до ED3 история должна быть глубже экрана, строк %d", n)
	}
	m.Write([]byte("\x1b[3J"))
	history, n := m.History(100)
	if n != 3 || strings.Contains(history, "L1") || !strings.Contains(history, "L5") {
		t.Fatalf("после ED3: строк %d, история %q — want только видимые L3..L5", n, history)
	}
}
