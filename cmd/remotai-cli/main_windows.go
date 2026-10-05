//go:build windows

// remotai.com is the console entry point. Windows resolves .com before .exe,
// so interactive shells wait for CLI commands while Explorer opens the GUI exe.
// The launcher contains no product logic and always runs the adjacent current exe.
package main

import (
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"

	"tgcontrol/internal/procutil"
)

func main() {
	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	cmd := procutil.Hidden(exec.Command(filepath.Join(filepath.Dir(exe), "remotai.exe"), os.Args[1:]...))
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	// Ctrl+C belongs to the CLI child sharing this console, not its wrapper.
	signal.Ignore(os.Interrupt)
	if err := cmd.Run(); err != nil {
		if e, ok := err.(*exec.ExitError); ok {
			os.Exit(e.ExitCode())
		}
		fmt.Fprintln(os.Stderr, "Remotai:", err)
		os.Exit(1)
	}
}
