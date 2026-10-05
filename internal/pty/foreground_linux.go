//go:build linux

package pty

import (
	"encoding/binary"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ProcessInfo describes a process detected inside a PTY session.
type ProcessInfo struct {
	PID     uint32
	Name    string
	ExePath string
	// StartMs — время создания процесса, unix мс; 0 = неизвестно. Поколение
	// процесса для ST-02: PID переиспользуется, пара (PID, StartMs) — нет.
	// Подробно — у Windows-версии ProcessInfo. При PID == 0 (передний план —
	// сам шелл) здесь время старта шелла для foregroundProcessOrShell.
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

const fgCacheTTL = 500 * time.Millisecond

// findForegroundProcess scans /proc/*/stat to build a ppid → children map
// and returns the deepest descendant of shellPID.
func findForegroundProcess(shellPID uint32) (ProcessInfo, error) {
	fgCache.mu.Lock()
	if hit, ok := fgCache.entries[shellPID]; ok && time.Since(hit.at) < fgCacheTTL {
		out := hit.info
		fgCache.mu.Unlock()
		return out, nil
	}
	fgCache.mu.Unlock()

	entries, err := os.ReadDir("/proc")
	if err != nil {
		return ProcessInfo{}, err
	}

	type procEntry struct {
		PID        uint32
		PPID       uint32
		Name       string
		StartTicks uint64
	}
	all := make([]procEntry, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		pid64, err := strconv.ParseUint(name, 10, 32)
		if err != nil {
			continue
		}
		stat, err := os.ReadFile("/proc/" + name + "/stat")
		if err != nil {
			continue
		}
		comm, ppid, startTicks, ok := parseProcStat(string(stat))
		if !ok {
			continue
		}
		all = append(all, procEntry{
			PID:        uint32(pid64),
			PPID:       ppid,
			Name:       comm,
			StartTicks: startTicks,
		})
	}

	children := map[uint32][]procEntry{}
	for _, p := range all {
		children[p.PPID] = append(children[p.PPID], p)
	}

	// Walk to deepest leaf (highest PID at each branch — usually most recent).
	var pick func(pid uint32) *procEntry
	pick = func(pid uint32) *procEntry {
		kids := children[pid]
		if len(kids) == 0 {
			return nil
		}
		// Prefer highest PID (proxy for most recent).
		best := kids[0]
		for _, k := range kids[1:] {
			if k.PID > best.PID {
				best = k
			}
		}
		if deeper := pick(best.PID); deeper != nil {
			return deeper
		}
		return &best
	}

	// Prefer a known AI agent anywhere in the subtree — an agent (claude/
	// codex/kimi/…) spawns long-lived MCP/tool children that would otherwise
	// become the deepest leaf and mask the agent (see isAgentProcess),
	// pinning the status to "working" forever. BFS finds the shallowest agent
	// (closest to the shell); fall back to the deepest leaf for plain commands.
	type agentHit struct {
		entry *procEntry
		kind  string
	}
	pickAgent := func(pid uint32) *agentHit {
		queue := append([]procEntry(nil), children[pid]...)
		for len(queue) > 0 {
			e := queue[0]
			queue = queue[1:]
			name := strings.ToLower(e.Name)
			if isAgentProcess(name) {
				ec := e
				return &agentHit{entry: &ec, kind: AgentKind(name)}
			}
			if isRuntimeExe(name) {
				// node/python hosting an npm/pip CLI — the command line names it.
				if kind := matchAgentCmdline(processCmdline(e.PID)); kind != "" {
					ec := e
					return &agentHit{entry: &ec, kind: kind}
				}
			}
			queue = append(queue, children[e.PID]...)
		}
		return nil
	}

	var leaf *procEntry
	agentKind := ""
	if hit := pickAgent(shellPID); hit != nil {
		leaf, agentKind = hit.entry, hit.kind
	} else {
		leaf = pick(shellPID)
	}
	var info ProcessInfo
	if leaf != nil {
		info.PID = leaf.PID
		info.Name = strings.ToLower(leaf.Name)
		if agentKind != "" && agentKind != info.Name {
			// npm-installed CLIs (kimi, gemini, …) run as "node" — surface
			// the resolved agent identity instead of the bare runtime.
			info.Name = agentKind
		}
		if exe, err := os.Readlink("/proc/" + strconv.FormatUint(uint64(leaf.PID), 10) + "/exe"); err == nil {
			info.ExePath = exe
		}
		// Время старта — из той же строки stat, что ppid и имя: пара
		// (PID, StartMs) согласована по построению, гонки с переиспользованием
		// номера между чтениями нет.
		info.StartMs = startTicksToUnixMs(leaf.StartTicks)
	} else {
		// На переднем плане сам шелл: его поколение тоже нужно клиенту, PID
		// подставит foregroundProcessOrShell.
		for i := range all {
			if all[i].PID == shellPID {
				info.StartMs = startTicksToUnixMs(all[i].StartTicks)
				break
			}
		}
	}

	fgCache.mu.Lock()
	if fgCache.entries == nil {
		fgCache.entries = make(map[uint32]foregroundCacheEntry)
	}
	now := time.Now()
	fgCache.entries[shellPID] = foregroundCacheEntry{at: now, info: info}
	if len(fgCache.entries) > 256 {
		for pid, cached := range fgCache.entries {
			if now.Sub(cached.at) > 2*fgCacheTTL {
				delete(fgCache.entries, pid)
			}
		}
	}
	fgCache.mu.Unlock()

	return info, nil
}

// shellPID returns the PID of the shell process spawned for this PTY.
func (p *conPTY) shellPID() uint32 {
	if p.cmd == nil || p.cmd.Process == nil {
		return 0
	}
	return uint32(p.cmd.Process.Pid)
}

func isShellExe(name string) bool {
	switch strings.ToLower(name) {
	case "bash", "sh", "zsh", "fish", "dash":
		return true
	}
	return false
}

// parseProcStat разбирает /proc/<pid>/stat: имя, ppid (поле 4) и starttime
// (поле 22, такты с загрузки). comm может содержать пробелы и скобки, поэтому
// режем по ПОСЛЕДНЕЙ ')': дальше идут поля с 3-го, и поле N лежит в rest[N-3].
func parseProcStat(s string) (comm string, ppid uint32, startTicks uint64, ok bool) {
	l := strings.IndexByte(s, '(')
	r := strings.LastIndexByte(s, ')')
	if l < 0 || r < 0 || r <= l {
		return "", 0, 0, false
	}
	comm = s[l+1 : r]
	rest := strings.Fields(s[r+1:])
	if len(rest) < 2 {
		return "", 0, 0, false
	}
	ppid64, err := strconv.ParseUint(rest[1], 10, 32)
	if err != nil {
		return "", 0, 0, false
	}
	// starttime необязателен для обхода дерева: без него процесс остаётся в
	// дереве, просто с неизвестным поколением (0).
	if len(rest) > 19 {
		startTicks, _ = strconv.ParseUint(rest[19], 10, 64)
	}
	return comm, uint32(ppid64), startTicks, true
}

// startTicksToUnixMs переводит starttime (такты с загрузки) в unix мс.
//
// Абсолютная точность ограничена btime (целые секунды), но для поколения важна
// СТАБИЛЬНОСТЬ, а не точность: btime читается один раз за жизнь агента (на
// некоторых ядрах /proc/stat btime дрожит на ±1 с после подводки часов, и
// перечитывание выдало бы тот же процесс за новое поколение), а starttime у
// процесса не меняется никогда.
func startTicksToUnixMs(ticks uint64) int64 {
	if ticks == 0 {
		return 0
	}
	boot := procBootTimeSec()
	if boot <= 0 {
		return 0
	}
	hz := procClockTicks()
	return boot*1000 + int64(ticks*1000/uint64(hz))
}

// processStartMs — время создания процесса pid, unix мс; 0 = неизвестно.
// Одинаковый контракт на всех ОС (см. foreground_windows.go).
func processStartMs(pid uint32) int64 {
	stat, err := os.ReadFile("/proc/" + strconv.FormatUint(uint64(pid), 10) + "/stat")
	if err != nil {
		return 0
	}
	_, _, ticks, ok := parseProcStat(string(stat))
	if !ok {
		return 0
	}
	return startTicksToUnixMs(ticks)
}

var procBoot struct {
	once sync.Once
	sec  int64
}

// procBootTimeSec — строка `btime` из /proc/stat (unix секунды загрузки).
func procBootTimeSec() int64 {
	procBoot.once.Do(func() {
		data, err := os.ReadFile("/proc/stat")
		if err != nil {
			return
		}
		for _, line := range strings.Split(string(data), "\n") {
			if v, ok := strings.CutPrefix(line, "btime "); ok {
				procBoot.sec, _ = strconv.ParseInt(strings.TrimSpace(v), 10, 64)
				return
			}
		}
	})
	return procBoot.sec
}

var procHZ struct {
	once sync.Once
	hz   int64
}

// _AT_CLKTCK — тип записи auxv с частотой times()/proc (USER_HZ).
const _AT_CLKTCK = 17

// procClockTicks — единица starttime в /proc (USER_HZ, она же sysconf(_SC_CLK_TCK)).
//
// Без cgo sysconf недоступен, поэтому берём то же, что берёт glibc: запись
// AT_CLKTCK из вектора auxv, который ядро кладёт процессу при exec
// (/proc/self/auxv — пары машинных слов «тип, значение»). Если его не прочесть
// (урезанный /proc в контейнере), берём 100: USER_HZ — часть ABI ядра, он
// равен 100 на x86, arm, arm64, riscv и прочих архитектурах, где мы работаем
// (исключение — alpha, 1024), и не зависит от CONFIG_HZ самого ядра.
func procClockTicks() int64 {
	procHZ.once.Do(func() {
		procHZ.hz = 100
		data, err := os.ReadFile("/proc/self/auxv")
		if err != nil {
			return
		}
		word := strconv.IntSize / 8
		for i := 0; i+2*word <= len(data); i += 2 * word {
			var typ, val uint64
			if word == 8 {
				typ = binary.NativeEndian.Uint64(data[i:])
				val = binary.NativeEndian.Uint64(data[i+word:])
			} else {
				typ = uint64(binary.NativeEndian.Uint32(data[i:]))
				val = uint64(binary.NativeEndian.Uint32(data[i+word:]))
			}
			if typ == 0 { // AT_NULL — конец вектора
				return
			}
			if typ == _AT_CLKTCK && val > 0 && val <= 100000 {
				procHZ.hz = int64(val)
				return
			}
		}
	})
	return procHZ.hz
}

// processCmdline returns the full command line of pid ("" on failure).
// /proc reads are cheap — no cache needed here (Windows has one).
func processCmdline(pid uint32) string {
	data, err := os.ReadFile("/proc/" + strconv.FormatUint(uint64(pid), 10) + "/cmdline")
	if err != nil {
		return ""
	}
	return strings.ReplaceAll(string(data), "\x00", " ")
}
