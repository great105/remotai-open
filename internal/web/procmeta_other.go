//go:build !darwin

package web

// procMeta / procMetaSnapshot — см. procmeta_darwin.go. На Windows и Linux
// gopsutil берёт имя и состояние процесса напрямую (WinAPI, /proc), внешних
// команд не запускает, и обходной снимок только добавил бы работы. nil здесь
// означает «идём обычным путём».
type procMeta struct {
	name   string
	status string
}

func procMetaSnapshot() map[int32]procMeta { return nil }
