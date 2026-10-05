package main

import (
	"runtime"
	"strings"
	"testing"
)

// Совет по службе обязан быть выполнимым НА ЭТОЙ системе.
//
// Живой первый мак 09.08.2026: установка прошла, «✅ Сервер привязан!», а
// следом — «sudo systemctl enable --now remotai». На macOS systemd нет вовсе:
// человек выполняет совет, получает command not found и остаётся с
// компьютером «не в сети» без единой рабочей подсказки.
func TestServiceHintsFitThePlatform(t *testing.T) {
	start, restart := serviceStartHint(), serviceRestartHint()
	if start == "" || restart == "" {
		t.Fatal("подсказка пустая — человеку нечего выполнить")
	}
	switch runtime.GOOS {
	case "darwin":
		for _, hint := range []string{start, restart} {
			if strings.Contains(hint, "systemctl") {
				t.Fatalf("на macOS советуем systemctl: %q", hint)
			}
		}
		if !strings.Contains(restart, "launchctl") {
			t.Fatalf("на macOS перезапуск не через launchctl: %q", restart)
		}
	case "linux":
		if !strings.Contains(start, "systemctl") {
			t.Fatalf("на Linux совет не про systemctl: %q", start)
		}
	case "windows":
		for _, hint := range []string{start, restart} {
			if strings.Contains(hint, "systemctl") || strings.Contains(hint, "launchctl") {
				t.Fatalf("на Windows советуем чужую команду: %q", hint)
			}
		}
	}
}
