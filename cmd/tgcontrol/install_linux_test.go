//go:build linux

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"tgcontrol/internal/procutil"
)

// Parse with the real systemd parser. verify never registers or starts a unit.
func TestSystemdUnitExecutablePath(t *testing.T) {
	analyze, err := exec.LookPath("systemd-analyze")
	if err != nil {
		t.Skip("systemd-analyze is not installed")
	}
	dir := t.TempDir()
	exe := filepath.Join(dir, `Remotai & Tools %h $HOME`)
	if err := os.WriteFile(exe, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, system := range []bool{false, true} {
		unit := systemdUnit(exe, "", system)
		if !strings.Contains(unit, "KillMode=process") || !strings.Contains(unit, "Restart=always") {
			t.Fatal("PTY preservation and automatic recovery must remain enabled")
		}
		path := filepath.Join(dir, "remotai-qa.service")
		if err := os.WriteFile(path, []byte(unit), 0o600); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(analyze, "verify", "--man=no", path)
		procutil.Hidden(cmd)
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("systemd rejected executable path: %v\n%s", err, output)
		}
	}
}
