//go:build windows

package main

import (
	"debug/pe"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
	"tgcontrol/internal/procutil"
)

// A real GUI-subsystem process, launched like Explorer (no inherited console
// or std handles). The fixture uses the production console code, without
// config, server, scheduled tasks or access to the user's terminal sessions.
func TestWindowsConsoleLifecycle(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"console_windows.go", "console_mode.go"} {
		b, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(filepath.Join(dir, name), b, 0600); err != nil {
			t.Fatal(err)
		}
	}
	source := `package main
import ("encoding/json"; "os"; "time"; "unsafe"; "golang.org/x/sys/windows")
func main() {
 prepareConsole()
 // A service-based CI runner may have console IO without a desktop window.
 // Measure console attachment itself, not whether conhost exposes an HWND.
 var pids [16]uint32
 n,_,_ := windows.NewLazySystemDLL("kernel32.dll").NewProc("GetConsoleProcessList").Call(uintptr(unsafe.Pointer(&pids[0])), uintptr(len(pids)))
 b,_ := json.Marshal(map[string]any{"console":n != 0,"stdin":validConsoleIO(os.Stdin),"stdout":validConsoleIO(os.Stdout)})
 os.WriteFile(os.Getenv("REMOTAI_QA_REPORT"), b, 0600)
 os.Stdout.WriteString("stdout-ok\n"); os.Stderr.WriteString("stderr-ok\n")
 time.Sleep(150*time.Millisecond)
 if os.Getenv("REMOTAI_QA_EXIT") == "7" { os.Exit(7) }
}`
	mainFile := filepath.Join(dir, "probe.go")
	if err := os.WriteFile(mainFile, []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(dir, "remotai.exe")
	build := procutil.Hidden(exec.Command("go", "build", "-ldflags=-H windowsgui", "-o", exe, mainFile, filepath.Join(dir, "console_windows.go"), filepath.Join(dir, "console_mode.go")))
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v %s", err, out)
	}
	file, err := pe.Open(exe)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if file.OptionalHeader.(*pe.OptionalHeader64).Subsystem != pe.IMAGE_SUBSYSTEM_WINDOWS_GUI {
		t.Fatal("a console-subsystem executable flashes before main")
	}
	report := filepath.Join(dir, "report.json")
	t.Setenv("REMOTAI_QA_REPORT", report)
	for _, mode := range []string{"", "--background", "--pty-host", "attach"} {
		t.Run("Explorer_"+mode, func(t *testing.T) {
			if code := runWithoutConsole(t, exe, mode); code != 0 {
				t.Fatalf("exit=%d", code)
			}
			b, err := os.ReadFile(report)
			if err != nil {
				t.Fatal(err)
			}
			var state map[string]bool
			if err := json.Unmarshal(b, &state); err != nil {
				t.Fatal(err)
			}
			if state["console"] != (mode == "attach") {
				t.Fatalf("mode=%q: %s", mode, b)
			}
			if mode == "attach" && (!state["stdin"] || !state["stdout"]) {
				t.Fatalf("terminal IO missing: %s", b)
			}
		})
	}
	t.Run("redirected_IO", func(t *testing.T) {
		out, err := procutil.Hidden(exec.Command(exe, "--version")).CombinedOutput()
		if err != nil || !strings.Contains(string(out), "stdout-ok") || !strings.Contains(string(out), "stderr-ok") {
			t.Fatalf("redirected IO lost: %v %s", err, out)
		}
	})
	t.Run("console_launcher_waits_and_returns_exit_code", func(t *testing.T) {
		launcher := filepath.Join(dir, "remotai.com")
		if out, err := procutil.Hidden(exec.Command("go", "build", "-o", launcher, "../remotai-cli")).CombinedOutput(); err != nil {
			t.Fatalf("launcher: %v %s", err, out)
		}
		t.Setenv("REMOTAI_QA_EXIT", "7")
		out, err := procutil.Hidden(exec.Command(launcher, "--version")).CombinedOutput()
		if e, ok := err.(*exec.ExitError); !ok || e.ExitCode() != 7 {
			t.Fatalf("exit code lost: %v", err)
		}
		if !strings.Contains(string(out), "stdout-ok") || !strings.Contains(string(out), "stderr-ok") {
			t.Fatalf("launcher IO lost: %s", out)
		}
		// Without redirected handles, the wrapper owns a console and the GUI
		// child must attach to it; no second terminal is allocated.
		if code := runWithoutConsole(t, launcher, "--version"); code != 7 {
			t.Fatalf("console launcher exit=%d", code)
		}
		b, _ := os.ReadFile(report)
		var state map[string]bool
		_ = json.Unmarshal(b, &state)
		if !state["console"] || !state["stdin"] || !state["stdout"] {
			t.Fatalf("parent console not attached: %s", b)
		}
	})
}

func runWithoutConsole(t *testing.T, exe, arg string) uint32 {
	t.Helper()
	command, err := windows.UTF16PtrFromString(windows.ComposeCommandLine(append([]string{exe}, strings.Fields(arg)...)))
	if err != nil {
		t.Fatal(err)
	}
	si := windows.StartupInfo{Cb: uint32(unsafe.Sizeof(windows.StartupInfo{}))}
	var pi windows.ProcessInformation
	if err := windows.CreateProcess(nil, command, nil, nil, false, 0, nil, nil, &si, &pi); err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(pi.Process)
	defer windows.CloseHandle(pi.Thread)
	if wait, _ := windows.WaitForSingleObject(pi.Process, 15000); wait != windows.WAIT_OBJECT_0 {
		_ = windows.TerminateProcess(pi.Process, 1)
		t.Fatal("fixture timed out")
	}
	var code uint32
	_ = windows.GetExitCodeProcess(pi.Process, &code)
	return code
}
