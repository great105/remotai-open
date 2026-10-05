package main

import (
	"strconv"

	"tgcontrol/internal/pty"
)

// runPtyHost runs the detached PTY host process: it owns a ConPTY + shell and
// serves remotai over a per-session named pipe, surviving remotai restarts.
// Invoked as `remotai --pty-host --id <id> --cwd <cwd> --shell <shell> --cols N --rows N`.
func runPtyHost(args []string) int {
	var id, shell, cwd string
	cols, rows := 80, 24

	next := func(i *int) string {
		*i++
		if *i < len(args) {
			return args[*i]
		}
		return ""
	}
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--id":
			id = next(&i)
		case "--shell":
			shell = next(&i)
		case "--cwd":
			cwd = next(&i)
		case "--cols":
			if v, err := strconv.Atoi(next(&i)); err == nil {
				cols = v
			}
		case "--rows":
			if v, err := strconv.Atoi(next(&i)); err == nil {
				rows = v
			}
		}
	}
	if id == "" {
		return 2
	}
	return pty.RunHost(id, shell, cwd, cols, rows)
}
