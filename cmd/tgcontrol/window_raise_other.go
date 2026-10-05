//go:build !windows

package main

// Нативного окна вне Windows нет: поднимать нечего, вызывающий откроет
// адрес обычным путём.
func raiseAppWindow() bool { return false }
