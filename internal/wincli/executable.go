package wincli

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// Executable selects the synchronous console entry point for commands run
// inside PowerShell/Windows Terminal. GUI launches keep using remotai.exe.
func Executable(exe string) string {
	if runtime.GOOS == "windows" && strings.EqualFold(filepath.Base(exe), "remotai.exe") {
		launcher := filepath.Join(filepath.Dir(exe), "remotai.com")
		if st, err := os.Stat(launcher); err == nil && !st.IsDir() {
			return launcher
		}
	}
	return exe
}
