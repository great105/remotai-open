//go:build !windows

package desktopui

// Show is a no-op on non-Windows platforms; returning false tells the caller to
// fall back to opening the system browser.
func Show(title, url string, width, height int) bool { return false }

// WindowOpen always reports false off Windows — нативного окна там нет.
func WindowOpen() bool { return false }
