package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Подсказка обязана соответствовать РЕЖИМУ УСТАНОВКИ, а не только системе.
//
// Живой Ubuntu 19.08.2026: человек поставил без sudo (`curl … | sh` от своего
// имени), установка и привязка прошли — «✅ Сервер привязан!». А компьютер
// остался «не в сети», и единственная подсказка на экране советовала
// `sudo systemctl enable --now remotai`. Такого юнита у него нет: служба лежит
// в его собственном менеджере, системный отвечает «Unit remotai.service not
// found». Совет уводил в тупик ровно там, где человек ждал решения.
func TestLinuxHintsFollowInstallMode(t *testing.T) {
	system := []string{linuxStartHint(false), linuxRestartHint(false), doctorLinuxFix(false)}
	user := []string{linuxStartHint(true), linuxRestartHint(true), doctorLinuxFix(true)}

	for _, hint := range system {
		if !strings.Contains(hint, "systemctl") {
			t.Fatalf("системный режим без systemctl: %q", hint)
		}
		if strings.Contains(hint, "--user") {
			t.Fatalf("системному юниту советуем --user: %q", hint)
		}
	}
	for _, hint := range user {
		if !strings.Contains(hint, "--user") {
			t.Fatalf("установка без sudo, а команда не про службу пользователя: %q", hint)
		}
		if strings.Contains(hint, "sudo") {
			t.Fatalf("ставили без sudo, а подсказка требует sudo: %q", hint)
		}
	}
}

// userUnitInstalled различает режимы по факту на диске, а не по догадке.
func TestUserUnitDetectedByFile(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("user-юнит systemd бывает только на Linux")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)

	if userUnitInstalled() {
		t.Fatal("юнита нет на диске, а функция сообщает об установке без sudo")
	}

	unitDir := filepath.Join(home, ".config", "systemd", "user")
	if err := os.MkdirAll(unitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(unitDir, "remotai.service"), []byte("[Unit]\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if !userUnitInstalled() {
		t.Fatal("юнит лежит в ~/.config/systemd/user, а функция его не видит")
	}
}
