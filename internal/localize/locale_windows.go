//go:build windows

package localize

import "syscall"

var userUILanguage = syscall.NewLazyDLL("kernel32.dll").NewProc("GetUserDefaultUILanguage")

func systemLanguage() string {
	language, _, _ := userUILanguage.Call()
	if language&0x3ff == 0x19 {
		return "ru"
	}
	return "en"
}
