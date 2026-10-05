//go:build !windows

package pty

// hostBusyErr — «хост жив, но канал занят другим клиентом». На unix-сокете
// такого состояния нет: сокет принимает сколько угодно подключений, поэтому
// ошибка дозвона там всегда означает, что хоста нет.
func hostBusyErr(error) bool { return false }
