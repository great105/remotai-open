//go:build windows

package pty

import (
	"fmt"
	"log"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
)

// OpenOnHost spawns a user-facing terminal window on the local machine in the
// given working directory. Tries Windows Terminal first (wt.exe), falls back
// to a detached PowerShell window. The optional command is appended to run
// after the shell starts.
//
// Critical detail: when tgcontrol runs as a Windows Service (LocalSystem,
// Session 0), exec.Command launches GUI processes in the isolated Session 0
// where the user can't see them. We detect that case and re-route the launch
// through the active console session via WTSQueryUserToken +
// CreateProcessAsUserW so the window appears on the user's desktop.
func OpenOnHost(cwd, command string) error {
	// Resolve the executable & build the command line. Both the in-session
	// and service paths need this.
	exe, args, err := resolveTerminal(cwd, command)
	if err != nil {
		return err
	}

	if isService, _ := svc.IsWindowsService(); isService {
		if err := launchInActiveSession(exe, args, cwd); err != nil {
			log.Printf("[handoff] launchInActiveSession failed: %v — fallback to direct CreateProcess", err)
		} else {
			return nil
		}
	}

	// Direct path: not a service (or fallback). Same behavior as before.
	cmd := exec.Command(exe, args...)
	cmd.Dir = cwd
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: false}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start handoff: %w", err)
	}
	go cmd.Wait()
	return nil
}

// resolveTerminal picks Windows Terminal if available, else falls back to
// powershell.exe. Returns the absolute exe path and full argument list
// (without the exe itself, as exec.Command expects).
func resolveTerminal(cwd, command string) (string, []string, error) {
	if wt, err := exec.LookPath("wt.exe"); err == nil {
		args := []string{"-w", "0", "nt", "-d", cwd}
		if command != "" {
			args = append(args, "powershell.exe", "-NoExit", "-Command", command)
		}
		return wt, args, nil
	}

	psPath, err := exec.LookPath("powershell.exe")
	if err != nil {
		// LocalSystem service has a stripped PATH — try the well-known location.
		fallback := filepath.Join(`C:\Windows\System32\WindowsPowerShell\v1.0`, "powershell.exe")
		if _, err2 := windows.UTF16PtrFromString(fallback); err2 == nil {
			psPath = fallback
		} else {
			return "", nil, fmt.Errorf("locate powershell.exe: %w", err)
		}
	}
	args := []string{"-NoExit", "-WorkingDirectory", cwd}
	if command != "" {
		args = append(args, "-Command", command)
	}
	return psPath, args, nil
}

// ── Session 0 → active console session bridge ──────────────────────────

const (
	tokenAdjustDefault = 0x0080
	tokenAssignPrimary = 0x0001
	tokenDuplicate     = 0x0002
	tokenQuery         = 0x0008

	createUnicodeEnv      = 0x00000400
	createNewConsole      = 0x00000010
	detachedProcess       = 0x00000008
	createNoWindow        = 0x08000000
	createNewProcessGroup = 0x00000200

	tokenPrimary = 1
)

var (
	wtsapi32                  = windows.NewLazySystemDLL("wtsapi32.dll")
	procWTSGetActiveConsoleID = windows.NewLazySystemDLL("kernel32.dll").NewProc("WTSGetActiveConsoleSessionId")
	procWTSQueryUserToken     = wtsapi32.NewProc("WTSQueryUserToken")

	userenv                     = windows.NewLazySystemDLL("userenv.dll")
	procCreateEnvironmentBlock  = userenv.NewProc("CreateEnvironmentBlock")
	procDestroyEnvironmentBlock = userenv.NewProc("DestroyEnvironmentBlock")

	advapi32                 = windows.NewLazySystemDLL("advapi32.dll")
	procDuplicateTokenEx     = advapi32.NewProc("DuplicateTokenEx")
	procCreateProcessAsUserW = advapi32.NewProc("CreateProcessAsUserW")
)

// launchInActiveSession runs `exe args` with cwd as the active console user,
// inheriting that user's environment. The new process appears on the user's
// desktop.
func launchInActiveSession(exe string, args []string, cwd string) error {
	// Find the active console session (the user logged into the physical
	// machine; for RDP scenarios this returns 0xFFFFFFFF).
	sessRet, _, _ := procWTSGetActiveConsoleID.Call()
	sessID := uint32(sessRet)
	if sessID == 0xFFFFFFFF {
		return fmt.Errorf("no active console session")
	}

	// Get a primary token for that session.
	var userToken windows.Token
	r1, _, e1 := procWTSQueryUserToken.Call(uintptr(sessID), uintptr(unsafe.Pointer(&userToken)))
	if r1 == 0 {
		return fmt.Errorf("WTSQueryUserToken: %w", e1)
	}
	defer userToken.Close()

	// Duplicate as a primary token suitable for CreateProcessAsUser.
	var dupToken windows.Token
	r2, _, e2 := procDuplicateTokenEx.Call(
		uintptr(userToken),
		tokenAssignPrimary|tokenDuplicate|tokenQuery|tokenAdjustDefault,
		0,
		uintptr(windows.SecurityImpersonation),
		tokenPrimary,
		uintptr(unsafe.Pointer(&dupToken)),
	)
	if r2 == 0 {
		return fmt.Errorf("DuplicateTokenEx: %w", e2)
	}
	defer dupToken.Close()

	// Build a Unicode environment block for the user.
	var envBlock unsafe.Pointer
	r3, _, e3 := procCreateEnvironmentBlock.Call(
		uintptr(unsafe.Pointer(&envBlock)),
		uintptr(dupToken),
		0,
	)
	if r3 == 0 {
		return fmt.Errorf("CreateEnvironmentBlock: %w", e3)
	}
	defer procDestroyEnvironmentBlock.Call(uintptr(envBlock))

	// Build the command line. Quote every arg so paths with spaces survive.
	cmdLine := windows.EscapeArg(exe)
	for _, a := range args {
		cmdLine += " " + windows.EscapeArg(a)
	}
	cmdLineW, err := windows.UTF16PtrFromString(cmdLine)
	if err != nil {
		return fmt.Errorf("encode cmdline: %w", err)
	}

	cwdW, err := windows.UTF16PtrFromString(cwd)
	if err != nil {
		// Empty string → CreateProcessAsUser uses the user profile dir.
		cwdW = nil
	}

	// We don't pass the exe explicitly — first token of cmdLineW is parsed.
	si := &windows.StartupInfo{
		Cb:         uint32(unsafe.Sizeof(windows.StartupInfo{})),
		Flags:      windows.STARTF_USESHOWWINDOW,
		ShowWindow: uint16(windows.SW_SHOW),
		Desktop:    windows.StringToUTF16Ptr(`winsta0\default`),
	}
	pi := &windows.ProcessInformation{}

	r4, _, e4 := procCreateProcessAsUserW.Call(
		uintptr(dupToken),
		0, // lpApplicationName
		uintptr(unsafe.Pointer(cmdLineW)),
		0, // lpProcessAttributes
		0, // lpThreadAttributes
		0, // bInheritHandles = FALSE
		uintptr(createUnicodeEnv|createNewConsole),
		uintptr(envBlock),
		uintptr(unsafe.Pointer(cwdW)),
		uintptr(unsafe.Pointer(si)),
		uintptr(unsafe.Pointer(pi)),
	)
	if r4 == 0 {
		return fmt.Errorf("CreateProcessAsUserW(%s): %w", trimForLog(cmdLine), e4)
	}

	// We don't need the handles — the spawned terminal is detached.
	_ = windows.CloseHandle(pi.Process)
	_ = windows.CloseHandle(pi.Thread)
	return nil
}

func trimForLog(s string) string {
	if len(s) > 200 {
		return s[:200] + "…"
	}
	return strings.TrimSpace(s)
}
