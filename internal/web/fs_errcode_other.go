//go:build !windows

package web

import (
	"errors"
	"syscall"
)

func platformFSCode(err error) string {
	var e syscall.Errno
	if !errors.As(err, &e) {
		return ""
	}
	switch e {
	case syscall.ENOSPC, syscall.EDQUOT:
		return fsCodeDiskFull
	case syscall.ETXTBSY, syscall.EBUSY:
		return fsCodeInUse
	case syscall.ENOTEMPTY:
		return fsCodeNotEmpty
	case syscall.EROFS:
		return fsCodeReadOnly
	case syscall.EACCES, syscall.EPERM:
		return fsCodePermission
	}
	return ""
}
