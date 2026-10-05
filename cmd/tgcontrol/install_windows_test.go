//go:build windows

package main

import (
	"os"
	"path/filepath"
	"testing"
)

// Постоянная установка узнаётся по деинсталлятору рядом с exe — тогда вторая
// копия на 25,7 МБ не нужна, а PATH указывает прямо на папку установки
// (аудит онбординга 30.08.2026).
func TestStableInstallDir(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "remotai.exe")
	if err := os.WriteFile(exe, []byte("exe"), 0o600); err != nil {
		t.Fatal(err)
	}

	if got := stableInstallDir(exe); got != "" {
		t.Fatalf("без деинсталлятора это запуск «откуда попало», получили %q", got)
	}

	if err := os.WriteFile(filepath.Join(dir, "unins000.exe"), []byte("uninst"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := stableInstallDir(exe); got != dir {
		t.Fatalf("установленная копия: ждали %q, получили %q", dir, got)
	}
}
