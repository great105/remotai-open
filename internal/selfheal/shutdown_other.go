//go:build !windows

package selfheal

import "time"

// lastShutdown на Linux/macOS причину не выдумывает.
//
// ПОЧЕМУ ПУСТО, А НЕ «РАЗБОР last/journalctl»: единственные доступные источники —
// `last -x` (локализованный текстовый вывод, разный в разных дистрибутивах) и
// journalctl (есть не везде, читается только с правами). Отчёт всё равно
// называет главное — что система перезагружалась и сколько её не было, — а
// придуманная причина хуже отсутствующей. На сервере под systemd падение
// агента и так лечится Restart=always, и разбор нужен реже.
func lastShutdown(since, boot time.Time) *ShutdownInfo { return nil }
