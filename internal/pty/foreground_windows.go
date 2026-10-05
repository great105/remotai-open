//go:build windows

package pty

import (
	"fmt"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"github.com/shirou/gopsutil/v4/process"
)

var (
	procCreateToolhelp32Snapshot   = kernel32.NewProc("CreateToolhelp32Snapshot")
	procProcess32FirstW            = kernel32.NewProc("Process32FirstW")
	procProcess32NextW             = kernel32.NewProc("Process32NextW")
	procQueryFullProcessImageNameW = kernel32.NewProc("QueryFullProcessImageNameW")
	procOpenProcess                = kernel32.NewProc("OpenProcess")
)

const (
	_TH32CS_SNAPPROCESS                = 0x00000002
	_INVALID_HANDLE                    = ^uintptr(0)
	_PROCESS_QUERY_LIMITED_INFORMATION = 0x1000
)

// processEntry32W mirrors PROCESSENTRY32W (Windows x64).
type processEntry32W struct {
	Size              uint32
	Usage             uint32
	ProcessID         uint32
	DefaultHeapID     uintptr
	ModuleID          uint32
	Threads           uint32
	ParentProcessID   uint32
	PriorityClassBase int32
	Flags             uint32
	ExeFile           [260]uint16
}

// ProcessInfo describes a process detected inside a PTY session.
type ProcessInfo struct {
	PID     uint32
	Name    string // executable name without extension, e.g. "claude"
	ExePath string // full path if obtainable
	// StartMs — время создания процесса, unix мс; 0 = неизвестно.
	//
	// ⚠ ПОКОЛЕНИЕ ПРОЦЕССА (ST-02). Windows переиспользует PID охотно: агент
	// вышел, человек запустил его снова — и новый процесс легко получает тот же
	// номер. Для клиента это «тот же передний план», и вердикт прокрутки,
	// измеренный у прежнего экземпляра, молча переезжал на новый. Пара
	// (PID, StartMs) уникальна: время создания у переиспользованного номера
	// другое. План: «PID без поколения не является достаточным ключом».
	//
	// При PID == 0 (на переднем плане сам шелл) здесь может лежать время старта
	// шелла: его подставляет foregroundProcessOrShell вместе с PID шелла.
	StartMs int64
}

// foregroundCache reduces snapshot cost by reusing recent results.
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

// findForegroundProcess walks the process tree starting at shellPID and
// returns the deepest descendant — the actual command running in the PTY
// (e.g. "claude" inside "powershell.exe"). Falls back to the shell itself
// if there are no children.
func findForegroundProcess(shellPID uint32) (ProcessInfo, error) {
	fgCache.mu.Lock()
	if hit, ok := fgCache.entries[shellPID]; ok && time.Since(hit.at) < fgCacheTTL {
		out := hit.info
		fgCache.mu.Unlock()
		return out, nil
	}
	fgCache.mu.Unlock()

	var info ProcessInfo
	var snapAt time.Time
	for attempt := 0; ; attempt++ {
		children, at, snapDone, err := processChildren()
		if err != nil {
			return ProcessInfo{}, err
		}
		snapAt = at

		// Prefer a known AI agent anywhere in the subtree — an agent (claude/
		// codex/kimi/…) spawns long-lived MCP/tool children that would otherwise
		// become the "deepest leaf" and mask the agent (see isAgentProcess). For
		// ordinary commands there is no agent, so fall back to the deepest leaf.
		var leaf *processEntry32W
		agentKind := ""
		if hit := pickAgentLeaf(shellPID, children); hit != nil {
			leaf, agentKind = hit.entry, hit.kind
		} else {
			leaf = pickDeepestLeaf(shellPID, children)
		}

		info = ProcessInfo{}
		if leaf == nil {
			// На переднем плане сам шелл: его поколение тоже нужно клиенту, PID
			// подставит foregroundProcessOrShell.
			_, info.StartMs = queryProcessIdentity(shellPID, snapDone)
			break
		}
		info.PID = leaf.ProcessID
		exe := syscall.UTF16ToString(leaf.ExeFile[:])
		info.Name = strings.TrimSuffix(strings.ToLower(exe), ".exe")
		if agentKind != "" && agentKind != info.Name {
			// npm-installed CLIs (kimi, gemini, …) run as node.exe — surface
			// the resolved agent identity instead of the bare runtime.
			info.Name = agentKind
		}
		// Путь и время создания — одним OpenProcess, только для найденного
		// PID, а не для всей таблицы процессов (снимок Toolhelp времени
		// создания не содержит).
		var stale bool
		info.ExePath, info.StartMs, stale = queryProcessIdentityEx(leaf.ProcessID, snapDone)
		// ⚠ Запись снимка устарела: процесс уже вышел или его номер занят БОЛЕЕ
		// НОВЫМ процессом (агент вышел, его запустили снова, а снимок ещё жил в
		// кэше). Отдать такой PID — ровно та ошибка, ради которой заведено
		// поколение: тест TestForegroundProcessCarriesStartGeneration падал 9 раз
		// из 10 при -count=10. Один повтор со свежим снимком; отказ в доступе к
		// защищённому процессу повтора не вызывает — иначе детектор снимал бы
		// таблицу процессов дважды за тик (замер CPU у processChildren).
		if stale && attempt == 0 {
			dropProcSnapshot(at)
			continue
		}
		break
	}

	fgCache.mu.Lock()
	if fgCache.entries == nil {
		fgCache.entries = make(map[uint32]foregroundCacheEntry)
	}
	// Метка — время СНИМКА, а не текущий момент. Иначе возрасты складываются:
	// результат, посчитанный по снимку 900-миллисекундной давности, лежал бы в
	// этом кэше ещё 500 мс со свежей меткой, и детектор видел бы вышедшего
	// агента до полутора секунд после его выхода.
	fgCache.entries[shellPID] = foregroundCacheEntry{at: snapAt, info: info}
	if len(fgCache.entries) > 256 {
		now := time.Now()
		for pid, hit := range fgCache.entries {
			if now.Sub(hit.at) >= fgCacheTTL {
				delete(fgCache.entries, pid)
			}
		}
	}
	fgCache.mu.Unlock()

	return info, nil
}

// dropProcSnapshot выбрасывает снимок, если это всё ещё тот, по которому
// считали (другой обход мог уже взять свежий).
func dropProcSnapshot(at time.Time) {
	procSnapshot.Lock()
	if procSnapshot.at.Equal(at) {
		procSnapshot.children = nil
	}
	procSnapshot.Unlock()
}

// processChildren возвращает карту «родитель → дети» по всей таблице процессов,
// переиспользуя недавний снимок.
//
// Снимок один на ТИК, а не на сессию. Детектор (runDetector, раз в секунду)
// обходит все живые сессии и для каждой звал findForegroundProcess, а тот делал
// полный CreateToolhelp32Snapshot — при полутора десятках терминалов это
// пятнадцать снимков всей таблицы процессов КАЖДУЮ СЕКУНДУ. Прежний кэш не
// спасал: его ключом был shellPID, то есть у каждой сессии свой промах. Замер
// 02.08.2026 на машине владельца: 26 456 секунд CPU у агента за 2,6 суток
// (~12% ядра круглосуточно), 90,6% из них — системное время.
//
// TTL чуть меньше периода детектора: первая сессия в тике строит снимок, все
// остальные берут готовый, а к следующему тику он уже протух.
var procSnapshot = struct {
	sync.Mutex
	at time.Time
	// doneAt — когда обход таблицы закончился. Процесс, найденный в снимке,
	// создан не позже этого момента; более позднее время создания у того же PID
	// означает, что номер уже переиспользован (см. queryProcessIdentity).
	doneAt   time.Time
	children map[uint32][]processEntry32W
}{}

// procSnapshotTTL — половина периода детектора (detectorTick = 1 с). Ровно
// столько, чтобы за один тик все сессии обслужил один снимок, и при этом запас
// не съедала длительность самого обхода: TTL 900 мс при обходе в 150 мс на
// загруженной машине давал детектору свежие данные лишь через тик.
const procSnapshotTTL = 500 * time.Millisecond

func processChildren() (map[uint32][]processEntry32W, time.Time, time.Time, error) {
	procSnapshot.Lock()
	defer procSnapshot.Unlock()
	if procSnapshot.children != nil && time.Since(procSnapshot.at) < procSnapshotTTL {
		return procSnapshot.children, procSnapshot.at, procSnapshot.doneAt, nil
	}

	// Время фиксируем ДО обхода: метка должна означать «данные такой давности»,
	// а не «мы закончили считать в такой-то момент».
	takenAt := time.Now()

	snap, _, err := procCreateToolhelp32Snapshot.Call(_TH32CS_SNAPPROCESS, 0)
	if snap == _INVALID_HANDLE {
		return nil, time.Time{}, time.Time{}, fmt.Errorf("CreateToolhelp32Snapshot: %w", err)
	}
	defer syscall.CloseHandle(syscall.Handle(snap))

	var entry processEntry32W
	entry.Size = uint32(unsafe.Sizeof(entry))

	r, _, _ := procProcess32FirstW.Call(snap, uintptr(unsafe.Pointer(&entry)))
	if r == 0 {
		return nil, time.Time{}, time.Time{}, fmt.Errorf("Process32FirstW: empty")
	}

	// Build child map: parent → []child entries.
	children := map[uint32][]processEntry32W{}
	for {
		// Copy entry into the map (avoid keeping pointers into the loop var).
		e := entry
		children[e.ParentProcessID] = append(children[e.ParentProcessID], e)
		r, _, _ := procProcess32NextW.Call(snap, uintptr(unsafe.Pointer(&entry)))
		if r == 0 {
			break
		}
	}

	// Карта отдаётся наружу только на чтение (обходы её не меняют), поэтому
	// копию не делаем — иначе экономия снимка ушла бы в аллокации.
	doneAt := time.Now()
	procSnapshot.at = takenAt
	procSnapshot.doneAt = doneAt
	procSnapshot.children = children
	return children, takenAt, doneAt, nil
}

// agentLeaf is a BFS hit: the process entry plus the resolved agent kind
// (from the exe name, or from the command line for script runtimes).
type agentLeaf struct {
	entry *processEntry32W
	kind  string
}

// pickAgentLeaf breadth-first searches pid's descendants for the shallowest
// known AI agent (claude/codex/kimi/…) and returns it, or nil if none.
// Shallowest = closest to the shell, so we stop at the agent itself rather
// than recursing into its MCP/tool child processes.
func pickAgentLeaf(pid uint32, children map[uint32][]processEntry32W) *agentLeaf {
	queue := append([]processEntry32W(nil), children[pid]...)
	// Windows не проверяет ParentProcessID: если PID успел переиспользоваться,
	// в таблице попадается цикл (A → B → A). Без отметки посещённых обход в
	// таком снимке крутится вечно — и теперь снимок ОБЩИЙ, то есть один
	// невезучий кадр положил бы разбор сразу по всем терминалам.
	seen := map[uint32]struct{}{pid: {}}
	for len(queue) > 0 {
		e := queue[0]
		queue = queue[1:]
		if _, dup := seen[e.ProcessID]; dup {
			continue
		}
		seen[e.ProcessID] = struct{}{}
		name := strings.TrimSuffix(strings.ToLower(syscall.UTF16ToString(e.ExeFile[:])), ".exe")
		if isAgentProcess(name) {
			ec := e
			return &agentLeaf{entry: &ec, kind: AgentKind(name)}
		}
		if isRuntimeExe(name) {
			// node/python hosting an npm/pip CLI — the command line names it.
			if kind := matchAgentCmdline(processCmdline(e.ProcessID)); kind != "" {
				ec := e
				return &agentLeaf{entry: &ec, kind: kind}
			}
		}
		queue = append(queue, children[e.ProcessID]...)
	}
	return nil
}

// pickDeepestLeaf спускается по последним потомкам до листа. Итеративно и с
// отметкой посещённых: цикл в таблице процессов (см. pickAgentLeaf) на
// рекурсии означал бы переполнение стека, то есть падение всего агента.
func pickDeepestLeaf(pid uint32, children map[uint32][]processEntry32W) *processEntry32W {
	kids := children[pid]
	if len(kids) == 0 {
		return nil
	}
	seen := map[uint32]struct{}{pid: {}}
	// Use the last child as primary candidate (most recently spawned).
	candidate := kids[len(kids)-1]
	for {
		if _, dup := seen[candidate.ProcessID]; dup {
			break
		}
		seen[candidate.ProcessID] = struct{}{}
		deeper := children[candidate.ProcessID]
		if len(deeper) == 0 {
			break
		}
		candidate = deeper[len(deeper)-1]
	}
	return &candidate
}

// queryProcessIdentity — полный путь и время создания процесса (unix мс) одним
// OpenProcess. Любая часть может не получиться: чужой пользователь, процесс уже
// вышел. Тогда путь пуст, а startMs == 0 («поколение неизвестно»).
//
// seenBy — момент, когда снимок процессов, в котором найден pid, был готов.
// Процесс из снимка создан не позже; если время создания позже, pid успели
// переиспользовать между снимком и OpenProcess, и время чужого процесса выдало
// бы новое поколение за старое. Нулевой seenBy проверку отключает.
func queryProcessIdentity(pid uint32, seenBy time.Time) (path string, startMs int64) {
	path, startMs, _ = queryProcessIdentityEx(pid, seenBy)
	return path, startMs
}

// errInvalidParameter — ERROR_INVALID_PARAMETER от OpenProcess: процесса с таким
// номером нет. stillActive (STILL_ACTIVE) объявлен в persist_windows.go.
const errInvalidParameter = syscall.Errno(87)

// queryProcessIdentityEx — то же плюс признак УСТАРЕВШЕЙ записи снимка:
// процесса с таким номером больше нет, он уже завершился (объект живёт, пока
// кто-то держит дескриптор — Windows разрушает его асинхронно) или номер занят
// более новым процессом. У завершившегося процесса поколения нет: номер вот-вот
// уйдёт другому. Отказ в доступе к чужому или защищённому процессу устаревшей
// записью не считается — повторять обход ради него бессмысленно.
func queryProcessIdentityEx(pid uint32, seenBy time.Time) (path string, startMs int64, stale bool) {
	h, _, err := procOpenProcess.Call(_PROCESS_QUERY_LIMITED_INFORMATION, 0, uintptr(pid))
	if h == 0 {
		return "", 0, err == errInvalidParameter
	}
	defer syscall.CloseHandle(syscall.Handle(h))
	var code uint32
	if syscall.GetExitCodeProcess(syscall.Handle(h), &code) == nil && code != stillActive {
		return "", 0, true
	}
	buf := make([]uint16, 1024)
	size := uint32(len(buf))
	r, _, _ := procQueryFullProcessImageNameW.Call(
		h, 0,
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(unsafe.Pointer(&size)),
	)
	if r != 0 {
		path = syscall.UTF16ToString(buf[:size])
	}
	startMs = handleStartMs(syscall.Handle(h))
	if startMs > 0 && !seenBy.IsZero() && startMs > seenBy.UnixMilli() {
		return "", 0, true
	}
	return path, startMs, false
}

// handleStartMs — время создания процесса из GetProcessTimes (FILETIME, шаг
// 100 нс от 1601 года; Filetime.Nanoseconds уже переводит в эпоху Unix).
// Значение неизменно на всю жизнь процесса, поэтому годится как поколение.
func handleStartMs(h syscall.Handle) int64 {
	var creation, exit, kernel, user syscall.Filetime
	if err := syscall.GetProcessTimes(h, &creation, &exit, &kernel, &user); err != nil {
		return 0
	}
	ms := creation.Nanoseconds() / int64(time.Millisecond)
	if ms <= 0 {
		return 0
	}
	return ms
}

// processStartMs — время создания процесса pid, unix мс; 0 = неизвестно.
// Одинаковый контракт на всех ОС (см. foreground_linux.go, foreground_darwin.go).
func processStartMs(pid uint32) int64 {
	_, ms := queryProcessIdentity(pid, time.Time{})
	return ms
}

// shellPID returns the PID of the shell process spawned for this PTY.
func (p *conPTY) shellPID() uint32 {
	return uint32(p.procPID)
}

// isShellExe reports whether name is one of the well-known shell executables
// — used to decide whether to surface an interesting agent badge.
func isShellExe(name string) bool {
	switch strings.ToLower(name) {
	case "powershell", "pwsh", "cmd", "bash", "sh", "zsh", "fish":
		return true
	}
	return false
}

// cmdlineCache memoizes command-line lookups: processCmdline runs inside the
// BFS for every runtime (node/python) process of every session subtree, and
// the gopsutil query is not free. A process command line never changes; the
// TTL only guards against PID reuse.
type cmdlineCacheEntry struct {
	cmdline string
	at      time.Time
}

var cmdlineCache = struct {
	sync.Mutex
	m map[uint32]cmdlineCacheEntry
}{m: make(map[uint32]cmdlineCacheEntry)}

const cmdlineCacheTTL = 60 * time.Second

// processCmdline returns the full command line of pid ("" on failure).
func processCmdline(pid uint32) string {
	cmdlineCache.Lock()
	if e, ok := cmdlineCache.m[pid]; ok && time.Since(e.at) < cmdlineCacheTTL {
		s := e.cmdline
		cmdlineCache.Unlock()
		return s
	}
	cmdlineCache.Unlock()

	s := queryProcessCmdline(pid)

	cmdlineCache.Lock()
	cmdlineCache.m[pid] = cmdlineCacheEntry{cmdline: s, at: time.Now()}
	cmdlineCache.Unlock()
	return s
}

func queryProcessCmdline(pid uint32) string {
	p, err := process.NewProcess(int32(pid))
	if err != nil {
		return ""
	}
	cl, err := p.Cmdline()
	if err != nil {
		return ""
	}
	return cl
}
