//go:build windows

package web

import (
	"errors"
	"syscall"
)

// Настоящие коды Win32. Сравнивать с syscall.ENOSPC/ETXTBSY на Windows НЕЛЬЗЯ:
// Go объявляет POSIX-errno здесь синтетическими значениями (APPLICATION_ERROR +
// iota), ОС такие никогда не возвращает. Матчить по тексту ошибки тоже нельзя —
// syscall.Errno.Error() идёт через FormatMessage с системной локалью, и на
// русской Windows «not enough space» не встретится ни разу.
const (
	winAccessDenied     syscall.Errno = 5   // ERROR_ACCESS_DENIED
	winWriteProtect     syscall.Errno = 19  // ERROR_WRITE_PROTECT
	winSharingViolation syscall.Errno = 32  // ERROR_SHARING_VIOLATION
	winLockViolation    syscall.Errno = 33  // ERROR_LOCK_VIOLATION
	winHandleDiskFull   syscall.Errno = 39  // ERROR_HANDLE_DISK_FULL
	winDiskFull         syscall.Errno = 112 // ERROR_DISK_FULL
	winDirNotEmpty      syscall.Errno = 145 // ERROR_DIR_NOT_EMPTY
)

func platformFSCode(err error) string {
	var e syscall.Errno
	if !errors.As(err, &e) {
		return ""
	}
	switch e {
	case winSharingViolation, winLockViolation:
		return fsCodeInUse
	case winDiskFull, winHandleDiskFull:
		return fsCodeDiskFull
	case winDirNotEmpty:
		return fsCodeNotEmpty
	case winWriteProtect:
		return fsCodeReadOnly
	case winAccessDenied:
		return fsCodePermission
	}
	return ""
}
