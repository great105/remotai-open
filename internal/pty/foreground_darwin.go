//go:build darwin

// Кто сейчас работает в терминале — на macOS.
//
// На Linux дерево процессов читается из /proc/*/stat. На маке /proc нет, но
// есть sysctl KERN_PROC_ALL: одним вызовом получаем СНИМОК всех процессов с
// pid, ppid и коротким именем. Дальше логика ровно та же, что в Linux-версии:
// спускаемся от шелла к самому глубокому потомку и ОСТАНАВЛИВАЕМСЯ на агенте
// (claude/codex/…), иначе «передним планом» вечно оказывался бы его
// MCP-сервер на node, а статусы waiting/ready не наступали бы никогда.
package pty

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// ProcessInfo describes a process detected inside a PTY session.
type ProcessInfo struct {
	PID     uint32
	Name    string
	ExePath string
	// StartMs — время создания процесса, unix мс; 0 = неизвестно. Поколение
	// процесса для ST-02: PID переиспользуется, пара (PID, StartMs) — нет.
	// Подробно — у Windows-версии ProcessInfo.
	StartMs int64
}

type foregroundCache struct {
	mu      sync.Mutex
	entries map[uint32]foregroundCacheEntry
}

type foregroundCacheEntry struct {
	at   time.Time
	info ProcessInfo
}

var fgCache foregroundCache

// Тот же TTL, что на Linux: детектор тикает раз в секунду на каждую сессию, а
// снимок процессов на маке заметно дороже чтения одного каталога.
const fgCacheTTL = 500 * time.Millisecond

type darwinProc struct {
	pid     uint32
	ppid    uint32
	name    string
	startMs int64
}

// kinfoStartMs — p_starttime из kinfo_proc (timeval), unix мс. Берётся из того
// же снимка, что pid и ppid, поэтому пара (PID, StartMs) согласована по
// построению.
func kinfoStartMs(tv unix.Timeval) int64 {
	ms := int64(tv.Sec)*1000 + int64(tv.Usec)/1000
	if ms <= 0 {
		return 0
	}
	return ms
}

// snapshotProcs — один sysctl вместо обхода /proc.
func snapshotProcs() ([]darwinProc, error) {
	procs, err := unix.SysctlKinfoProcSlice("kern.proc.all")
	if err != nil {
		return nil, err
	}
	out := make([]darwinProc, 0, len(procs))
	for i := range procs {
		p := &procs[i].Proc.P_comm
		name := make([]byte, 0, len(p))
		for _, c := range p {
			if c == 0 {
				break
			}
			name = append(name, byte(c))
		}
		out = append(out, darwinProc{
			pid:     uint32(procs[i].Proc.P_pid),
			ppid:    uint32(procs[i].Eproc.Ppid),
			name:    string(name),
			startMs: kinfoStartMs(procs[i].Proc.P_starttime),
		})
	}
	return out, nil
}

// processStartMs — время создания процесса pid, unix мс; 0 = неизвестно.
// Одинаковый контракт на всех ОС (см. foreground_windows.go).
func processStartMs(pid uint32) int64 {
	k, err := unix.SysctlKinfoProc("kern.proc.pid", int(pid))
	if err != nil || k == nil || uint32(k.Proc.P_pid) != pid {
		return 0
	}
	return kinfoStartMs(k.Proc.P_starttime)
}

// findForegroundProcess возвращает самого глубокого потомка shellPID, но не
// заходит внутрь AI-агента (см. isAgentProcess в events.go — там же объяснение,
// почему это принципиально).
func findForegroundProcess(shellPID uint32) (ProcessInfo, error) {
	fgCache.mu.Lock()
	if hit, ok := fgCache.entries[shellPID]; ok && time.Since(hit.at) < fgCacheTTL {
		out := hit.info
		fgCache.mu.Unlock()
		return out, nil
	}
	fgCache.mu.Unlock()

	procs, err := snapshotProcs()
	if err != nil {
		return ProcessInfo{}, err
	}
	children := make(map[uint32][]darwinProc, len(procs))
	byPID := make(map[uint32]darwinProc, len(procs))
	for _, p := range procs {
		byPID[p.pid] = p
		children[p.ppid] = append(children[p.ppid], p)
	}
	if _, ok := byPID[shellPID]; !ok {
		return ProcessInfo{}, fmt.Errorf("process %d not found", shellPID)
	}

	// Спуск к самому глубокому потомку: у шелла обычно одна живая ветка (то,
	// что человек запустил), у неё — свои дети.
	//
	// npm-агенты (Gemini CLI, часть Kimi) живут как `node …/bin/gemini`: имя
	// процесса — runtime, агента выдаёт только командная строка. На Linux это
	// делает matchAgentCmdline по /proc/<pid>/cmdline (живой стенд 07.09.2026:
	// без неё `fg=node kind=node` при открытом Gemini); здесь то же самое через
	// sysctl kern.procargs2 — без /proc и без `ps`.
	cur := byPID[shellPID]
	agentKind := ""
	for {
		kids := children[cur.pid]
		if len(kids) == 0 {
			break
		}
		// Останов на агенте: его дочерние процессы (MCP-серверы, воркеры) живут
		// столько же, сколько сам агент, и «глубочайший потомок» всегда был бы
		// node/python, а не claude.
		next := kids[0]
		for _, k := range kids {
			if isAgentProcess(k.name) {
				next = k
				break
			}
			if isRuntimeExe(strings.ToLower(k.name)) {
				if kind := matchAgentCmdline(processCmdline(k.pid)); kind != "" {
					next, agentKind = k, kind
					break
				}
			}
		}
		cur = next
		if agentKind != "" || isAgentProcess(cur.name) {
			break
		}
	}

	info := ProcessInfo{PID: cur.pid, Name: cur.name, StartMs: cur.startMs}
	if agentKind != "" && agentKind != info.Name {
		// node/python на переднем плане — показываем распознанного агента, как
		// на Linux и Windows, иначе клиент держит Gemini за обычный процесс.
		info.Name = agentKind
	}
	fgCache.mu.Lock()
	if fgCache.entries == nil {
		fgCache.entries = make(map[uint32]foregroundCacheEntry)
	}
	fgCache.entries[shellPID] = foregroundCacheEntry{at: time.Now(), info: info}
	fgCache.mu.Unlock()
	return info, nil
}

// processCmdline — командная строка процесса из sysctl kern.procargs2 (""
// при отказе: чужой пользователь, процесс уже умер). Разбор буфера — в
// procargs.go, он покрыт тестом на любой ОС.
func processCmdline(pid uint32) string {
	buf, err := unix.SysctlRaw("kern.procargs2", int(pid))
	if err != nil {
		return ""
	}
	return parseProcArgs2(buf)
}

// shellPID — pid шелла этой сессии (тот же контракт, что у Linux и Windows).
func (p *conPTY) shellPID() uint32 {
	if p.cmd == nil || p.cmd.Process == nil {
		return 0
	}
	return uint32(p.cmd.Process.Pid)
}
