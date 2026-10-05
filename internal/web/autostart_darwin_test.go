//go:build darwin

package web

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMacAutostartUsesLaunchdAndKeepsCurrentAgentRunning(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("PATH", root) // only the fake launchctl can be invoked
	t.Setenv("QA_LAUNCH_STATE", filepath.Join(root, "disabled"))
	t.Setenv("QA_LAUNCH_CALLS", filepath.Join(root, "calls"))
	script := `#!/bin/sh
printf '%s\n' "$*" >> "$QA_LAUNCH_CALLS"
case "$1" in
  enable) printf false > "$QA_LAUNCH_STATE" ;;
  disable) printf true > "$QA_LAUNCH_STATE" ;;
  print-disabled)
    state=false
    if [ -f "$QA_LAUNCH_STATE" ]; then read -r state < "$QA_LAUNCH_STATE" || true; fi
    printf 'disabled services = {\n "ru.remotai.agent" => %s\n}\n' "$state"
    ;;
  *) exit 91 ;;
esac
`
	if err := os.WriteFile(filepath.Join(root, "launchctl"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if getAutostartState().Enabled {
		t.Fatal("empty profile must start with autostart off")
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
		state := getAutostartState()
		if state.Enabled != enable || state.Method != "launch_agent" || state.Recommended != "launch_agent" {
			t.Fatalf("panel received the wrong macOS state: %+v", state)
		}
	}
	calls, err := os.ReadFile(filepath.Join(root, "calls"))
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"bootout", "bootstrap", "kickstart", "kill", "systemctl"} {
		if strings.Contains(string(calls), forbidden) {
			t.Fatalf("changing next-login startup must not stop or duplicate the current agent: %s", calls)
		}
	}
}
