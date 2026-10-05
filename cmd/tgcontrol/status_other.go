//go:build !windows && !darwin

package main

import (
	"os"
	"path/filepath"
)

func cliAutostartState() (bool, string) {
	home, _ := os.UserHomeDir()
	if _, err := os.Stat("/etc/systemd/system/remotai.service"); err == nil {
		return true, "systemd system-unit"
	}
	if _, err := os.Stat(filepath.Join(home, ".config", "systemd", "user", "remotai.service")); err == nil {
		return true, "systemd user-unit"
	}
	return false, "не настроен"
}

func cleanupInstalledArtifacts() error { return nil }
