//go:build windows

package pty

import (
	"fmt"
	"syscall"
	"unsafe"
)

var (
	ntdll                         = syscall.NewLazyDLL("ntdll.dll")
	procNtQueryInformationProcess = ntdll.NewProc("NtQueryInformationProcess")
	procReadProcessMemory         = kernel32.NewProc("ReadProcessMemory")
)

// Offsets for 64-bit PEB / RTL_USER_PROCESS_PARAMETERS on Windows x64.
// These are stable across Windows 10 / 11; see public PEB layout docs.
const (
	pebOffsetProcessParameters          = 0x20
	processParamsOffsetCurrentDirectory = 0x38
)

// processBasicInformation mirrors PROCESS_BASIC_INFORMATION (x64).
type processBasicInformation struct {
	Reserved1       uintptr
	PebBaseAddress  uintptr
	Reserved2       [2]uintptr
	UniqueProcessID uintptr
	Reserved3       uintptr
}

// readCWD returns the current working directory of the shell process
// by walking the PEB via NtQueryInformationProcess + ReadProcessMemory.
// Works on x64 Windows 10/11.
func readCWD(hProc syscall.Handle) (string, error) {
	var pbi processBasicInformation
	var retLen uint32
	status, _, _ := procNtQueryInformationProcess.Call(
		uintptr(hProc),
		0, // ProcessBasicInformation
		uintptr(unsafe.Pointer(&pbi)),
		unsafe.Sizeof(pbi),
		uintptr(unsafe.Pointer(&retLen)),
	)
	if status != 0 {
		return "", fmt.Errorf("NtQueryInformationProcess: 0x%08x", status)
	}
	if pbi.PebBaseAddress == 0 {
		return "", fmt.Errorf("PEB address is null")
	}

	// Read ProcessParameters pointer from PEB.
	var ppAddr uintptr
	if err := readMem(hProc, pbi.PebBaseAddress+pebOffsetProcessParameters, unsafe.Pointer(&ppAddr), unsafe.Sizeof(ppAddr)); err != nil {
		return "", fmt.Errorf("read PEB.ProcessParameters: %w", err)
	}
	if ppAddr == 0 {
		return "", fmt.Errorf("ProcessParameters is null")
	}

	// Read CurrentDirectory UNICODE_STRING { USHORT Length; USHORT MaxLength; PWSTR Buffer; }
	// On x64 the struct is padded: [Length u16][MaxLength u16][pad u32][Buffer u64]
	type unicodeString struct {
		Length    uint16
		MaxLength uint16
		_         uint32 // padding
		Buffer    uintptr
	}
	var us unicodeString
	if err := readMem(hProc, ppAddr+processParamsOffsetCurrentDirectory, unsafe.Pointer(&us), unsafe.Sizeof(us)); err != nil {
		return "", fmt.Errorf("read CurrentDirectory: %w", err)
	}
	if us.Buffer == 0 || us.Length == 0 {
		return "", fmt.Errorf("CurrentDirectory buffer empty")
	}

	// Read the UTF-16 path bytes.
	pathLen := int(us.Length) / 2 // length is in bytes; convert to uint16 count
	if pathLen > 32768 {
		pathLen = 32768
	}
	buf := make([]uint16, pathLen)
	if err := readMem(hProc, us.Buffer, unsafe.Pointer(&buf[0]), uintptr(us.Length)); err != nil {
		return "", fmt.Errorf("read path buffer: %w", err)
	}

	s := syscall.UTF16ToString(buf)
	// Windows CWD often ends with a trailing backslash; trim for display.
	if len(s) > 3 && (s[len(s)-1] == '\\' || s[len(s)-1] == '/') {
		s = s[:len(s)-1]
	}
	return s, nil
}

func readMem(h syscall.Handle, addr uintptr, dst unsafe.Pointer, size uintptr) error {
	var read uintptr
	r, _, err := procReadProcessMemory.Call(
		uintptr(h),
		addr,
		uintptr(dst),
		size,
		uintptr(unsafe.Pointer(&read)),
	)
	if r == 0 {
		return err
	}
	return nil
}

// currentCWD is the Windows implementation of Session.CurrentCWD().
func (p *conPTY) currentCWD() (string, error) {
	return readCWD(p.procHandle)
}
