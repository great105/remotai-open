//go:build linux

package web

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestLinuxAutostartPreservesRunningAgentAndReportsManagerState(t *testing.T) {
	if _, err := os.Stat(systemUnitPath); err == nil {
		t.Skip("system-managed installation must remain untouched")
	}
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("PATH", root) // Never invoke a real manager on the developer's machine.
	t.Setenv("QA_SYSTEMD_STATE", filepath.Join(root, "state"))
	t.Setenv("QA_SYSTEMD_CALLS", filepath.Join(root, "calls"))
	script := `#!/bin/sh
printf '%s\n' "$*" >> "$QA_SYSTEMD_CALLS"
if [ "$1" = --user ]; then shift; fi
if [ "${QA_SYSTEMD_FAIL:-}" = "$1" ]; then echo 'manager unavailable' >&2; exit 42; fi
case "$1" in
  daemon-reload) ;;
  enable) printf 'enabled\n' > "$QA_SYSTEMD_STATE" ;;
  disable) printf 'disabled\n' > "$QA_SYSTEMD_STATE" ;;
  is-enabled)
    state=disabled
    if [ -f "$QA_SYSTEMD_STATE" ]; then read -r state < "$QA_SYSTEMD_STATE"; fi
    printf '%s\n' "$state"
    [ "$state" = enabled ] ;;
  *) exit 91 ;;
esac
`
	if err := os.WriteFile(filepath.Join(root, "systemctl"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, enable := range []bool{true, false, true} {
		var err error
		if enable {
			err = enableAutostart()
		} else {
			err = disableAutostart()
		}
		if err != nil {
			t.Fatal(err)
		}
		st := getAutostartState()
		if st.Enabled != enable || st.Method != "systemd_user" {
			t.Fatalf("wrong manager state: %+v", st)
		}
	}
	// A present file is not proof of enabled startup; external changes count.
	if err := os.WriteFile(filepath.Join(root, "state"), []byte("disabled\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if getAutostartState().Enabled {
		t.Fatal("disabled unit reported enabled")
	}
	for _, operation := range []string{"daemon-reload", "enable", "disable"} {
		t.Setenv("QA_SYSTEMD_FAIL", operation)
		var err error
		if operation == "disable" {
			err = disableAutostart()
		} else {
			err = enableAutostart()
		}
		if err == nil || !strings.Contains(err.Error(), "manager unavailable") {
			t.Fatalf("%s failure swallowed: %v", operation, err)
		}
	}
	if _, err := os.Stat(filepath.Join(root, ".config/systemd/user/remotai.service")); err != nil {
		t.Fatal("running service definition removed:", err)
	}
	// Validate the actual UI-written unit, not only the separate CLI template.
	if parser := "/usr/bin/systemd-analyze"; fileExistsForAutostartTest(parser) {
		cmd := exec.Command(parser, "verify", "--man=no", filepath.Join(root, ".config/systemd/user/remotai.service"))
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("invalid UI startup unit: %v: %s", err, output)
		}
	}
	calls, err := os.ReadFile(filepath.Join(root, "calls"))
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"--now", " start ", " stop ", "restart", "kill", "loginctl"} {
		if strings.Contains(string(calls), forbidden) {
			t.Fatalf("toggle changes current process: %s", calls)
		}
	}
}

func fileExistsForAutostartTest(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
