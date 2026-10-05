//go:build !darwin

package main

// restartOwnService — заглушка для не-macOS.
//
// На Linux служба перезапускается через systemctl прямо в
// restartServiceIfRunning, на Windows ею управляет само приложение. Функция
// существует только затем, чтобы darwin-ветка не ломала сборку под остальные
// платформы (см. pair_restart_darwin.go).
func restartOwnService() (running bool, err error) { return false, nil }
