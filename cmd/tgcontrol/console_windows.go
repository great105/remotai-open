//go:build windows

package main

import (
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

// enableVTOutput включает обработку ANSI-последовательностей на stdout —
// PTY шлёт VT-вывод, и без этого флага classic conhost печатает мусор.
func enableVTOutput() {
	h := windows.Handle(os.Stdout.Fd())
	var mode uint32
	if windows.GetConsoleMode(h, &mode) == nil {
		_ = windows.SetConsoleMode(h, mode|windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING)
	}
}

// enableVTInput переводит stdin в VT-режим: стрелки, F-клавиши и Ctrl-хорды
// приходят ANSI-последовательностями и доходят до удалённой TUI (Claude Code,
// vim, …) как есть. term.MakeRaw этот флаг не выставляет.
func enableVTInput() {
	h := windows.Handle(os.Stdin.Fd())
	var mode uint32
	if windows.GetConsoleMode(h, &mode) == nil {
		_ = windows.SetConsoleMode(h, mode|windows.ENABLE_VIRTUAL_TERMINAL_INPUT)
	}
}

// prepareConsole connects explicit CLI commands to the caller's console.
// The released exe uses the GUI subsystem: Explorer, autostart and PTY hosts
// never get a console, even for a frame. Redirected pipes must stay intact.
func prepareConsole() {
	if !cliConsoleRequested(os.Args[1:]) {
		return
	}
	k32 := windows.NewLazySystemDLL("kernel32.dll")
	ok, _, _ := k32.NewProc("AttachConsole").Call(^uintptr(0) & 0xffffffff)
	if ok == 0 {
		// Only the explicit "Open terminal" action may create a new console.
		// A CLI caller with redirected IO needs no window.
		var mode uint32
		if windows.GetConsoleMode(windows.Handle(os.Stdout.Fd()), &mode) != nil {
			if os.Args[1] != "attach" || validConsoleIO(os.Stdout) {
				return
			}
			if allocated, _, _ := k32.NewProc("AllocConsole").Call(); allocated == 0 {
				return
			}
		}
	}
	// Go initializes os.Stdin/Stdout/Stderr before main; AttachConsole does not
	// refresh these Go objects. Keep valid pipes/files, reopen missing handles.
	for _, stream := range []struct {
		file **os.File
		name string
		id   uint32
	}{
		{&os.Stdin, "CONIN$", windows.STD_INPUT_HANDLE},
		{&os.Stdout, "CONOUT$", windows.STD_OUTPUT_HANDLE},
		{&os.Stderr, "CONOUT$", windows.STD_ERROR_HANDLE},
	} {
		if validConsoleIO(*stream.file) {
			continue
		}
		f, err := os.OpenFile(stream.name, os.O_RDWR, 0)
		if err == nil {
			*stream.file = f
			_ = windows.SetStdHandle(stream.id, windows.Handle(f.Fd()))
		}
	}
}

func validConsoleIO(f *os.File) bool {
	if f == nil {
		return false
	}
	kind, err := windows.GetFileType(windows.Handle(f.Fd()))
	return err == nil && kind != windows.FILE_TYPE_UNKNOWN
}

// Defensive fallback for developer console-subsystem builds. Release builds
// have no console to free: hiding one after startup cannot prevent a flash.
func maybeFreeConsole() {
	k32 := windows.NewLazySystemDLL("kernel32.dll")
	pids := make([]uint32, 4)
	n, _, _ := k32.NewProc("GetConsoleProcessList").
		Call(uintptr(unsafe.Pointer(&pids[0])), uintptr(len(pids)))
	if n == 1 {
		k32.NewProc("FreeConsole").Call()
	}
}
