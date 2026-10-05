//go:build windows

package pty

import (
	"log"
	"os"
	"strings"
	"time"

	"github.com/shirou/gopsutil/v4/process"
)

// killGraceTimeout — сколько ждём, пока хост уйдёт сам, получив кадр Kill.
// Добивание по PID — страховка на случай мёртвой трубы, а не замена штатному
// пути: хосту надо проснуться, прочитать кадр и закрыть ConPTY. Если снять его
// сразу, штатное закрытие не успеет отработать.
const killGraceTimeout = 700 * time.Millisecond

// killHostProcess убивает процесс хоста сессии id, если тот пережил штатное
// закрытие. Возвращает true, если процесс пришлось снимать силой.
//
// Три проверки перед выстрелом — PID переиспользуются, и убить чужую программу
// вместо давно вышедшего хоста было бы хуже утечки:
//   - процесс запущен из нашего же exe (с допуском на хвост автообновления);
//   - в его командной строке есть --pty-host;
//   - id в этой командной строке — именно наш.
//
// Дерево шелла добивать отдельно не нужно: шелл лежит в Job Object хоста с
// KILL_ON_JOB_CLOSE (makeKillOnCloseJob), и смерть хоста закрывает последний
// хэндл джоба — ОС снимает всё поддерево сама.
func killHostProcess(id string, pid uint32) bool {
	if pid == 0 || id == "" {
		return false
	}
	self, err := os.Executable()
	if err != nil {
		return false
	}
	deadline := time.Now().Add(killGraceTimeout)
	for {
		p, err := process.NewProcess(int32(pid))
		if err != nil {
			return false // процесса уже нет — хост вышел сам, штатным путём
		}
		cmdline, err := p.Cmdline()
		if err != nil || hostIDFromCmdline(cmdline) != strings.ToLower(id) {
			return false // чужой процесс на переиспользованном PID
		}
		if exe, err := p.Exe(); err != nil || !sameAgentExecutable(exe, self) {
			return false // не наш бинарь — не трогаем
		}
		if time.Now().After(deadline) {
			if err := p.Kill(); err != nil {
				log.Printf("[PTY] не удалось снять хост id=%s pid=%d: %v", id, pid, err)
				return false
			}
			log.Printf("[PTY] хост id=%s pid=%d снят по PID (труба не ответила)", id, pid)
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// reapOrphanHosts снимает pty-host'ы, которых нет ни в одной записи хранилища:
// терминала для них не существует ни на экране, ни на диске, вернуться в них
// нельзя, а память и процессы они держат.
//
// known — идентификаторы, известные хранилищу. Вызывающий обязан НЕ звать эту
// функцию, если хранилище не удалось прочитать: пустой список там означал бы
// «сироты все», и уборка снесла бы живые терминалы человека вместе с
// работающими в них агентами (см. MetaStore.Loaded).
func reapOrphanHosts(known map[string]struct{}) int {
	self, err := os.Executable()
	if err != nil {
		return 0
	}
	procs, err := process.Processes()
	if err != nil {
		return 0
	}
	selfPID := int32(os.Getpid())
	killed := 0
	for _, p := range procs {
		if p.Pid == selfPID {
			continue
		}
		cmdline, err := p.Cmdline()
		if err != nil {
			continue
		}
		id := hostIDFromCmdline(cmdline)
		if id == "" {
			continue
		}
		if _, ok := known[id]; ok {
			continue
		}
		exe, err := p.Exe()
		if err != nil || !sameAgentExecutable(exe, self) {
			continue // чужой бинарь: другой агент (dev-runtime) со своим хранилищем
		}
		if err := p.Kill(); err != nil {
			log.Printf("[PTY] сирота id=%s pid=%d не снялся: %v", id, p.Pid, err)
			continue
		}
		log.Printf("[PTY] снят осиротевший хост id=%s pid=%d (записи о терминале нет)", id, p.Pid)
		killed++
	}
	return killed
}
