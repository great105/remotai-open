//go:build !windows

package main

// enableVTOutput — на Unix терминалы понимают VT и так.
func enableVTOutput() {}

// enableVTInput — на Unix raw mode уже отдаёт VT-последовательности.
func enableVTInput() {}

// maybeFreeConsole — актуально только для Windows.
func maybeFreeConsole() {}

func prepareConsole() {}
