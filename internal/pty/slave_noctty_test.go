package pty

import (
	"os"
	"strings"
	"testing"
)

// Ведомую сторону псевдотерминала открываем ТОЛЬКО с O_NOCTTY.
//
// ЖИВОЙ МАК 10.08.2026 — терминалы не открывались ни разу, в логе хоста:
//
//	[ptyhost …] PTY: fork/exec /bin/zsh: operation not permitted
//
// Одинаково на bash, zsh и sh, из /Users/andrei и из /tmp. Причина не в
// оболочке: pty-host запущен с Setsid, значит он лидер сессии без управляющего
// терминала, и открытие slave без O_NOCTTY делает терминал управляющим ДЛЯ
// ХОСТА. Оболочка после своего setsid просит TIOCSCTTY — терминал уже занят
// чужой сессией, ядро отвечает EPERM, а Go показывает это как «fork/exec».
//
// На Linux дефект прятался за root: привилегированному процессу ядро разрешает
// отобрать терминал, поэтому под обычным пользователем он тоже бы вылез.
//
// Проверяем исходник, а не поведение: на Windows псевдотерминал другой
// (ConPTY), на Linux под root дефект не воспроизводится, а цена ошибки —
// платформа без терминалов вообще.
func TestSlaveOpenedWithNoCtty(t *testing.T) {
	for _, name := range []string{"pty_darwin.go", "pty_linux.go"} {
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		text := string(src)
		if strings.Contains(text, "os.OpenFile(slavePath, os.O_RDWR, 0)") {
			t.Fatalf("%s: slave открыт без O_NOCTTY — оболочка не получит управляющий терминал (см. комментарий у теста)", name)
		}
		if !strings.Contains(text, "os.OpenFile(slavePath, os.O_RDWR|syscall.O_NOCTTY, 0)") {
			t.Fatalf("%s: не найдено открытие slave с O_NOCTTY", name)
		}
	}
}
