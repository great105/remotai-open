//go:build linux

package pty

import (
	"fmt"
	"os/exec"
)

// terminalCandidates lists desktop terminal emulators in order of preference.
// Each candidate maps the binary name to the argv builder.
var terminalCandidates = []struct {
	bin  string
	args func(cwd, command string) []string
}{
	{"gnome-terminal", func(cwd, cmd string) []string {
		a := []string{"--working-directory=" + cwd}
		if cmd != "" {
			a = append(a, "--", "bash", "-c", cmd+"; exec bash")
		}
		return a
	}},
	{"konsole", func(cwd, cmd string) []string {
		a := []string{"--workdir", cwd}
		if cmd != "" {
			a = append(a, "-e", "bash", "-c", cmd+"; exec bash")
		}
		return a
	}},
	{"xfce4-terminal", func(cwd, cmd string) []string {
		a := []string{"--working-directory=" + cwd}
		if cmd != "" {
			a = append(a, "-e", "bash -c '"+cmd+"; exec bash'")
		}
		return a
	}},
	{"xterm", func(cwd, cmd string) []string {
		a := []string{}
		if cmd != "" {
			a = append(a, "-e", "cd "+cwd+" && "+cmd+"; exec bash")
		} else {
			a = append(a, "-e", "cd "+cwd+" && exec bash")
		}
		return a
	}},
}

// OpenOnHost launches a desktop terminal in the given cwd, falling back across
// gnome-terminal → konsole → xfce4-terminal → xterm.
func OpenOnHost(cwd, command string) error {
	for _, c := range terminalCandidates {
		bin, err := exec.LookPath(c.bin)
		if err != nil {
			continue
		}
		cmd := exec.Command(bin, c.args(cwd, command)...)
		if err := cmd.Start(); err != nil {
			continue
		}
		go cmd.Wait()
		return nil
	}
	return fmt.Errorf("no supported terminal emulator found (tried gnome-terminal, konsole, xfce4-terminal, xterm)")
}
