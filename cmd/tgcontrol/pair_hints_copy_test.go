package main

import (
	"runtime"
	"strings"
	"testing"
)

// Команда в подсказке обязана КОПИРОВАТЬСЯ и работать как есть.
//
// Живой мак 10.08.2026: человек получил `launchctl kickstart -k
// gui/$(id -u)/ru.remotai.agent`, набрал руками и промахнулся дважды —
// `launch` вместо `launchctl` и `id-u` без пробела. Два «command not found»
// подряд, компьютер так и не вышел на связь. Подстановки и скобки в тексте для
// человека — это приглашение к опечатке.
func TestHintsAreCopyPastable(t *testing.T) {
	for _, hint := range []string{serviceStartHint(), serviceRestartHint()} {
		if strings.Contains(hint, "$(") || strings.Contains(hint, "${") {
			t.Fatalf("в подсказке осталась подстановка, её придётся набирать руками: %q", hint)
		}
	}
	if runtime.GOOS == "darwin" && !strings.Contains(serviceRestartHint(), "gui/") {
		t.Fatalf("на macOS в перезапуске нет цели gui/<uid>: %q", serviceRestartHint())
	}
}

// Диагностика тоже обязана советовать команду ЭТОЙ системы.
//
// Живой мак 10.08.2026: `remotai doctor` написал «запустите приложение Remotai
// (на сервере: systemctl start remotai)». Человек выполнил — «command not
// found». Диагностика отправила его в тупик вместо починки.
func TestDoctorFixFitsThePlatform(t *testing.T) {
	fix := doctorStartFix()
	if fix == "" {
		t.Fatal("у проблемы «агент не отвечает» нет действия")
	}
	if runtime.GOOS == "darwin" && strings.Contains(fix, "systemctl") {
		t.Fatalf("на macOS doctor советует systemctl: %q", fix)
	}
	if runtime.GOOS == "linux" && !strings.Contains(fix, "systemctl") {
		t.Fatalf("на Linux doctor не называет systemctl: %q", fix)
	}
}
