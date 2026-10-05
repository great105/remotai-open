//go:build !darwin

package main

// Заглушки мак-веток install/uninstall: сами ветки выбираются по runtime.GOOS,
// а компилятору нужны символы на всех платформах. Настоящая реализация —
// install_darwin.go.

func installDarwin(opts installOptions) int { return 1 }

func uninstallDarwin(purge bool) int { return 1 }
