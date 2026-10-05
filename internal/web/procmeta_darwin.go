//go:build darwin

// Имя и состояние всех процессов ОДНИМ снимком `ps` — вместо вызова на каждый.
//
// ЗАЧЕМ. gopsutil на macOS добывает состояние процесса внешней командой:
// `Status()` — это `ps -o state= -p <pid>` НА КАЖДЫЙ процесс
// (process_darwin.go), а `Name()` уходит в `ps` ещё раз, если имя длиннее 15
// символов. На живом маке 10.08.2026 это означало ~400 запусков `ps` на один
// GET /api/system/processes: запрос висел дольше двух минут и возвращал пустоту
// по таймауту, то есть раздел «Процессы» на маке не работал вовсе.
//
// Здесь тот же ответ берётся одним запуском `ps` на весь список. На Windows и
// Linux ничего не меняется: там gopsutil читает данные напрямую (WinAPI и
// /proc), внешних процессов не плодит — см. procmeta_other.go.
package web

import (
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"tgcontrol/internal/procutil"
)

// procMeta — то, что дорого спрашивать поштучно.
type procMeta struct {
	name   string
	status string
}

// procMetaSnapshot возвращает карту pid → имя и состояние. nil означает «снимок
// не удался» — вызывающий откатывается на поштучные вызовы gopsutil.
func procMetaSnapshot() map[int32]procMeta {
	// -c даёт короткое имя (accounting name) вместо полной командной строки:
	// именно его показывает клиент, и именно оно не ломает разбор по столбцам.
	cmd := exec.Command("ps", "-axco", "pid=,state=,comm=")
	procutil.Hidden(cmd)
	done := make(chan struct{})
	go func() {
		// Страховка от зависшего `ps`: раздел «Процессы» должен ответить хоть
		// чем-то, а не висеть, как это было до снимка.
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			if cmd.Process != nil {
				_ = cmd.Process.Kill()
			}
		}
	}()
	out, err := cmd.Output()
	close(done)
	if err != nil {
		return nil
	}

	res := make(map[int32]procMeta, 512)
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil {
			continue
		}
		// Состояние в стиле ps («S», «R+», «Ss») приводим к тем же словам, что
		// отдаёт gopsutil на других платформах, — клиент один на все ОС.
		res[int32(pid)] = procMeta{
			name:   filepath.Base(strings.Join(fields[2:], " ")),
			status: statusWord(fields[1]),
		}
	}
	if len(res) == 0 {
		return nil
	}
	return res
}

// statusWord переводит первую букву состояния BSD в слово gopsutil.
func statusWord(state string) string {
	if state == "" {
		return ""
	}
	switch state[0] {
	case 'R':
		return "running"
	case 'S':
		return "sleep"
	case 'T':
		return "stop"
	case 'I':
		return "idle"
	case 'Z':
		return "zombie"
	case 'U', 'D':
		return "blocked"
	default:
		return ""
	}
}
