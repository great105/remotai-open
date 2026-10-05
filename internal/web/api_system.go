package web

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/kbinani/screenshot"
	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/disk"
	"github.com/shirou/gopsutil/v4/host"
	"github.com/shirou/gopsutil/v4/mem"
	"github.com/shirou/gopsutil/v4/net"
	"github.com/shirou/gopsutil/v4/process"

	"tgcontrol/internal/bundle"
	"tgcontrol/internal/procutil"
	"tgcontrol/internal/service"
)

func (s *Server) apiSystemStats(w http.ResponseWriter, r *http.Request, uid int64) {
	// CPU и тома — из общего кэша: оба замера дорогие, а сводку с одного ПК
	// одновременно тянут несколько экранов и несколько устройств (#122).
	snap := hostStats()
	cpuPercent := snap.cpuPercent
	cpuCores := runtime.NumCPU()

	// Memory
	memInfo, _ := mem.VirtualMemory()
	memResult := map[string]any{
		"total": 0, "used": 0, "available": 0, "percent": 0.0,
	}
	if memInfo != nil {
		memResult = map[string]any{
			"total":     memInfo.Total,
			"used":      memInfo.Used,
			"available": memInfo.Available,
			"percent":   memInfo.UsedPercent,
		}
	}

	// Disk (см. diskVolumes: служебные тома отфильтрованы, «главный» — рабочий).
	diskResult, partitionResults := snap.disk, snap.disks

	// Uptime
	uptimeSec := 0.0
	if bootTime, err := host.BootTime(); err == nil {
		uptimeSec = float64(time.Now().Unix()) - float64(bootTime)
	}

	// Network
	netResult := map[string]any{"bytes_sent": uint64(0), "bytes_recv": uint64(0)}
	if counters, err := net.IOCounters(false); err == nil && len(counters) > 0 {
		netResult = map[string]any{
			"bytes_sent": counters[0].BytesSent,
			"bytes_recv": counters[0].BytesRecv,
		}
	}

	jsonResp(w, map[string]any{
		"cpu":     map[string]any{"percent": cpuPercent, "cores": cpuCores},
		"memory":  memResult,
		"disk":    diskResult,
		"disks":   partitionResults,
		"uptime":  uptimeSec,
		"network": netResult,
	})
}

// ── Дорогая часть сводки: замер CPU и обход томов ────────────────────
//
// Замер загрузки процессора блокирует горутину на всё окно измерения, а обход
// томов на Windows опрашивает каждую букву диска (подвисшая сетевая шара
// отвечает не сразу). Раньше это делалось на КАЖДЫЙ запрос, а сводку одного ПК
// одновременно тянут «Устройства» на телефоне, открытый «Монитор» и вторая
// поверхность (окно exe, Telegram) — компьютер будили по кругу (#122).
//
// Кэш общий и короткий: «Монитор» опрашивает статистику раз в 5 секунд, так что
// цифры остаются живыми, а всплеск одновременных запросов стоит одного прохода.
const (
	hostStatsTTL  = 3 * time.Second
	hostCPUWindow = 200 * time.Millisecond
	minRealVolume = 2 << 30 // 2 ГиБ: меньше — служебный раздел (EFI, recovery, образ)
)

type hostStatsSnapshot struct {
	at         time.Time
	cpuPercent float64
	disk       map[string]any
	disks      []map[string]any
}

var (
	hostStatsMu   sync.Mutex
	hostStatsLast hostStatsSnapshot
)

// hostStats отдаёт снимок дорогой части сводки, пересчитывая его не чаще раза в
// hostStatsTTL. Замок держится и на время самого замера: пачка одновременных
// запросов обязана стоить одного прохода, а не четырёх.
func hostStats() hostStatsSnapshot {
	hostStatsMu.Lock()
	defer hostStatsMu.Unlock()
	if !hostStatsLast.at.IsZero() && time.Since(hostStatsLast.at) < hostStatsTTL {
		return hostStatsLast
	}
	snap := hostStatsSnapshot{at: time.Now()}
	if percents, err := cpu.Percent(hostCPUWindow, false); err == nil && len(percents) > 0 {
		snap.cpuPercent = percents[0]
	}
	snap.disk, snap.disks = diskVolumes()
	hostStatsLast = snap
	return snap
}

// serviceVolumeFstypes — файловые системы, которые на вопрос «сколько осталось
// места» не отвечают: read-only образы (snap, ISO), псевдо-ФС ядра и сетевые
// шары (там место чужой машины). На штатной Ubuntu самым заполненным томом
// всегда оказывается snap-образ — 100% по определению, — и карточка «Диск»
// пугала красным при трёхстах свободных гигабайтах на корне (#119).
var serviceVolumeFstypes = map[string]bool{
	"squashfs": true, "snapfuse": true, "fuse.snapfuse": true, "erofs": true,
	"iso9660": true, "cd9660": true, "udf": true, "cdfs": true,
	"overlay": true, "overlayfs": true, "aufs": true,
	"tmpfs": true, "devtmpfs": true, "ramfs": true, "efivarfs": true,
	"proc": true, "sysfs": true, "cgroup": true, "cgroup2": true,
	"debugfs": true, "tracefs": true, "securityfs": true, "pstore": true,
	"configfs": true, "fusectl": true, "mqueue": true, "hugetlbfs": true,
	"binfmt_misc": true, "autofs": true, "devpts": true, "nsfs": true,
	"nfs": true, "nfs4": true, "cifs": true, "smbfs": true, "smb2": true,
	"sshfs": true, "fuse.sshfs": true, "davfs": true, "davfs2": true,
	"9p": true, "fuse.rclone": true, "fuse.gvfsd-fuse": true,
}

// serviceMountPrefixes — куда система монтирует свои образы и служебные ветки.
var serviceMountPrefixes = []string{
	"/snap/", "/var/lib/snapd/", "/var/snap/", "/proc/", "/sys/", "/dev/",
	"/run/", "/var/lib/docker/", "/var/lib/kubelet/", "/boot/efi",
}

// isServiceVolume — том, который показывать как «диск» бессмысленно.
func isServiceVolume(part disk.PartitionStat) bool {
	if serviceVolumeFstypes[strings.ToLower(strings.TrimSpace(part.Fstype))] {
		return true
	}
	// /dev/loopN — тот же snap и смонтированные образы (на Windows устройство
	// выглядит как «C:», под это правило не попадает).
	if strings.HasPrefix(strings.ToLower(part.Device), "/dev/loop") {
		return true
	}
	mount := filepath.ToSlash(part.Mountpoint)
	for _, prefix := range serviceMountPrefixes {
		if strings.HasPrefix(mount, prefix) {
			return true
		}
	}
	// ro — образ, снапшот или защищённый от записи носитель: «свободно 0 B» там
	// не новость, а свойство тома.
	for _, opt := range part.Opts {
		if strings.EqualFold(strings.TrimSpace(opt), "ro") {
			return true
		}
	}
	return false
}

// normalizeVolumePath — путь в сравнимом виде: слеши вперёд, без хвостового,
// на Windows в нижнем регистре (том «C:» против дома «C:\Users\…»).
func normalizeVolumePath(path string) string {
	path = strings.TrimSuffix(filepath.ToSlash(path), "/")
	if runtime.GOOS == "windows" {
		path = strings.ToLower(path)
	}
	return path
}

// volumeOfPath — том, на котором лежит path: побеждает самый длинный
// подходящий mountpoint (корень «/» нормализуется в пустую строку и подходит
// всегда, но проигрывает любому вложенному).
func volumeOfPath(partitions []disk.PartitionStat, path string) string {
	target := normalizeVolumePath(path)
	best, bestLen := "", -1
	for _, part := range partitions {
		if part.Mountpoint == "" {
			continue
		}
		mount := normalizeVolumePath(part.Mountpoint)
		if mount != "" && target != mount && !strings.HasPrefix(target, mount+"/") {
			continue
		}
		if len(mount) > bestLen {
			best, bestLen = part.Mountpoint, len(mount)
		}
	}
	return best
}

// diskVolumes собирает тома компьютера: главным считается тот, где живут
// рабочие каталоги агента (домашняя папка), остальные идут списком по убыванию
// заполненности — чтобы «есть ещё заполненные: /var 94%» было чем показать.
// Служебные тома (см. isServiceVolume) и мелочь меньше minRealVolume в список
// не попадают; исключение — сам рабочий том: он информативен по определению.
func diskVolumes() (map[string]any, []map[string]any) {
	home, _ := os.UserHomeDir()
	if home == "" {
		home = "."
	}
	// Запасной вариант на случай, когда обход томов не удался (ограниченные
	// права, экзотическая ФС): честная сводка по домашней папке.
	fallback := map[string]any{
		"total": 0, "used": 0, "free": 0, "percent": 0.0,
		"mount": home, "device": "",
	}
	if usage, err := disk.Usage(home); err == nil && usage != nil {
		fallback = map[string]any{
			"total":   usage.Total,
			"used":    usage.Used,
			"free":    usage.Free,
			"percent": usage.UsedPercent,
			"mount":   home,
			"device":  "",
		}
	}

	volumes := make([]map[string]any, 0)
	partitions, err := disk.Partitions(false)
	if err != nil {
		return fallback, volumes
	}
	mainMount := volumeOfPath(partitions, home)
	seenMounts := make(map[string]bool)
	var mainVolume map[string]any
	for _, part := range partitions {
		if part.Mountpoint == "" || seenMounts[part.Mountpoint] {
			continue
		}
		seenMounts[part.Mountpoint] = true
		isMain := part.Mountpoint == mainMount
		if !isMain && isServiceVolume(part) {
			continue
		}
		usage, err := disk.Usage(part.Mountpoint)
		if err != nil || usage == nil || usage.Total == 0 {
			continue
		}
		if !isMain && usage.Total < minRealVolume {
			continue
		}
		volume := map[string]any{
			"total": usage.Total, "used": usage.Used, "free": usage.Free,
			"percent": usage.UsedPercent, "mount": part.Mountpoint,
			"device": part.Device, "fstype": part.Fstype,
			// main — том рабочих каталогов. Клиенту он нужен, чтобы «главным»
			// показывать системный диск, а не самый заполненный из всех.
			"main": isMain,
		}
		if isMain {
			mainVolume = volume
		}
		volumes = append(volumes, volume)
	}
	sort.Slice(volumes, func(i, j int) bool {
		return volumes[i]["percent"].(float64) > volumes[j]["percent"].(float64)
	})
	if mainVolume != nil {
		return mainVolume, volumes
	}
	if len(volumes) > 0 {
		return volumes[0], volumes
	}
	return fallback, volumes
}

// ── Защита процессов: self / critical ────────────────────────────────
//
// Список процессов и «✖» рядом с ним ничем не отличали свои процессы от чужих:
// сортировка по памяти регулярно выносила в топ окно панели (msedgewebview2) и
// сам агент, а удержания кнопки хватало, чтобы отрезать себе доступ к
// компьютеру, закрыть все терминалы или уронить сеанс Windows.

// criticalWindows / criticalLinux — процессы, чьё завершение ломает сеанс или
// саму ОС. Имена в нижнем регистре и без «.exe» (см. normalizeProcName).
var criticalWindows = map[string]bool{
	"system": true, "registry": true, "smss": true, "csrss": true,
	"wininit": true, "winlogon": true, "services": true, "lsass": true,
	"lsaiso": true, "svchost": true, "explorer": true, "dwm": true,
}

var criticalLinux = map[string]bool{
	"systemd": true, "init": true, "systemd-journald": true,
	"systemd-logind": true, "systemd-udevd": true, "systemd-resolved": true,
	"dbus-daemon": true, "dbus-broker": true, "sshd": true,
	"networkmanager": true, "xorg": true,
}

// webviewProcName — рантайм WebView2, на котором работает окно панели Remotai.
const webviewProcName = "msedgewebview2"

// normalizeProcName приводит имя процесса к виду ключей whitelist'а: нижний
// регистр без расширения .exe.
func normalizeProcName(name string) string {
	return strings.TrimSuffix(strings.ToLower(name), ".exe")
}

// isCriticalProc — системный процесс ОС, завершать который нельзя без явного
// предупреждения. Платформа определяется рантаймом: файл общий для всех ОС.
func isCriticalProc(pid int32, name string) bool {
	n := normalizeProcName(name)
	if runtime.GOOS == "windows" {
		return pid == 0 || pid == 4 || criticalWindows[n]
	}
	return pid == 1 || criticalLinux[n]
}

// selfGuard — снимок «что принадлежит самому Remotai», собирается один раз на
// HTTP-запрос.
type selfGuard struct {
	pid int32
	// base — имя нашего бинаря (нижний регистр, без .exe). По нему узнаются
	// персистентные pty-хосты (`remotai --pty-host`): это тот же самый бинарь,
	// но отдельный процесс — он намеренно детачится и уходит из Job Object
	// (internal/pty/persist_windows.go), поэтому нашим потомком не является и
	// обходом дерева не находится.
	base string
}

func newSelfGuard() selfGuard {
	g := selfGuard{pid: int32(os.Getpid())}
	if exe, err := os.Executable(); err == nil {
		g.base = normalizeProcName(filepath.Base(exe))
	}
	return g
}

// isSelfByName — быстрая проверка без обращения к дереву процессов: сам агент
// или любой другой экземпляр нашего бинаря (pty-хост, забытый хост от прежней
// версии после автообновления).
func (g selfGuard) isSelfByName(pid int32, name string) bool {
	if pid == g.pid {
		return true
	}
	return g.base != "" && normalizeProcName(name) == g.base
}

// isWebViewProc — процесс рантайма WebView2 (браузерный + renderer/GPU/crashpad).
func isWebViewProc(name string) bool {
	return normalizeProcName(name) == webviewProcName
}

// hasSelfAncestor поднимается по родителям от pid и ищет наш процесс.
//
// Зовётся ТОЛЬКО для WebView2: p.Ppid() на Windows делает собственный снимок
// дерева процессов на каждый вызов, на весь список это дорого.
//
// ВАЖНО: правило намеренно сужено до WebView2. Помечать self всех потомков
// подряд нельзя — если pty-хосту не удалось выйти из Job Object (ветка in-job
// в persist_windows.go), нашими потомками оказывается весь шелл пользователя
// вместе с claude/node/браузером.
func (g selfGuard) hasSelfAncestor(pid int32) bool {
	for i := 0; i < 6 && pid > 0; i++ {
		p, err := process.NewProcess(pid)
		if err != nil {
			return false
		}
		ppid, err := p.Ppid()
		if err != nil || ppid <= 0 || ppid == pid {
			return false
		}
		if ppid == g.pid {
			return true
		}
		pid = ppid
	}
	return false
}

// isSelf — полная проверка для одиночного PID (kill). В списке процессов
// родословная WebView2 разбирается пакетно, см. apiProcessesList.
func (g selfGuard) isSelf(pid int32, name string) bool {
	if g.isSelfByName(pid, name) {
		return true
	}
	return isWebViewProc(name) && g.hasSelfAncestor(pid)
}

// isPermissionErr — отказ по правам. Кроме os.ErrPermission ловим и сырой текст
// Windows: OpenProcess/TerminateProcess отдают «Access is denied», и он не
// всегда доезжает до errors.Is.
func isPermissionErr(err error) bool {
	if errors.Is(err, os.ErrPermission) {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "access is denied") || strings.Contains(msg, "permission denied")
}

// ── CPU процессов: дельта, а не среднее за жизнь ───────────────────
//
// gopsutil CPUPercent() делит ВСЁ процессорное время процесса на его возраст.
// Для браузера, открытого утром, это «2%», даже когда он прямо сейчас ест ядро
// целиком, а для только что запущенного компилятора — сотни процентов. Ровно
// поэтому сортировка «По CPU» не находила виновника нагрузки (#63).
//
// Считаем сами: разницу процессорного времени между двумя опросами, делённую
// на реальное время между ними и на число ядер. Первый опрос отдаёт 0 и флаг
// cpu_warmup — обещать точность, которой ещё нет, нельзя.
type cpuSample struct {
	total float64 // секунды CPU (user+system)
	at    time.Time
}

var (
	cpuSnapMu sync.Mutex
	cpuSnap   = map[int32]cpuSample{}
)

// cpuDelta — загрузка процесса между этим и прошлым опросом, 0..100.
// ok=false, когда предыдущего замера нет (первый показ списка).
func cpuDelta(pid int32, total float64, now time.Time, cores int) (float64, bool) {
	cpuSnapMu.Lock()
	prev, had := cpuSnap[pid]
	cpuSnap[pid] = cpuSample{total: total, at: now}
	cpuSnapMu.Unlock()

	if !had || !now.After(prev.at) {
		return 0, false
	}
	elapsed := now.Sub(prev.at).Seconds()
	if elapsed <= 0 {
		return 0, false
	}
	// PID переиспользуются: отрицательная разница значит «это уже другой
	// процесс», и врать про 0% честнее, чем показать мусор.
	if total < prev.total {
		return 0, false
	}
	pct := (total - prev.total) / elapsed * 100
	if cores > 1 {
		pct /= float64(cores)
	}
	return min(max(pct, 0), 100), true
}

// pruneCPUSnap выкидывает из кэша процессы, которых больше нет в снимке:
// иначе карта растёт по всем PID за время работы агента.
func pruneCPUSnap(alive map[int32]bool) {
	cpuSnapMu.Lock()
	defer cpuSnapMu.Unlock()
	for pid := range cpuSnap {
		if !alive[pid] {
			delete(cpuSnap, pid)
		}
	}
}

const (
	// cpuWarmupWindow — пауза внутреннего прогрева: за это время процессорное
	// время процессов успевает измениться заметно, а ответ остаётся быстрым.
	cpuWarmupWindow = 400 * time.Millisecond
	// cpuSnapMaxAge — предельный возраст базового замера. Дельта по замеру
	// десятиминутной давности — это «среднее за десять минут между двумя
	// нажатиями ↻», а человек читает её как «прямо сейчас».
	cpuSnapMaxAge = 10 * time.Second
)

// cpuSnapFresh — время самого свежего замера в кэше (нулевое, если кэш пуст).
func cpuSnapFresh() time.Time {
	cpuSnapMu.Lock()
	defer cpuSnapMu.Unlock()
	var fresh time.Time
	for _, s := range cpuSnap {
		if s.at.After(fresh) {
			fresh = s.at
		}
	}
	return fresh
}

// primeCPUSnap снимает базовый замер процессорного времени всех процессов, не
// выдавая чисел наружу: следующий замер (после cpuWarmupWindow) уже даёт
// настоящую дельту. Без этого первый показ вкладки «Процессы» писал «CPU 0.0%»
// у всех строк, и второй замер приходил только по ручному «↻».
func primeCPUSnap(procs []*process.Process, at time.Time) {
	samples := make(map[int32]cpuSample, len(procs))
	for _, p := range procs {
		if times, err := p.Times(); err == nil && times != nil {
			samples[p.Pid] = cpuSample{total: times.User + times.System, at: at}
		}
	}
	cpuSnapMu.Lock()
	defer cpuSnapMu.Unlock()
	for pid, s := range samples {
		cpuSnap[pid] = s
	}
}

func (s *Server) apiProcessesList(w http.ResponseWriter, r *http.Request, uid int64) {
	sortBy := r.URL.Query().Get("sort")
	if sortBy == "" {
		sortBy = "memory"
	}
	limitStr := r.URL.Query().Get("limit")
	limit := 50
	if n, err := strconv.Atoi(limitStr); err == nil && n > 0 {
		limit = n
	}

	procs, _ := process.Processes()
	type procInfo struct {
		PID    int32   `json:"pid"`
		Name   string  `json:"name"`
		CPU    float64 `json:"cpu"`
		Memory uint64  `json:"memory"`
		Status string  `json:"status"`
		// Self — процесс самого Remotai (агент, окно панели на WebView2,
		// pty-хост): «✖» здесь отрезает доступ к ПК или закрывает все терминалы.
		Self bool `json:"self"`
		// Critical — системный процесс ОС (whitelist).
		Critical bool `json:"critical"`
		// CPUWarmup — у ЭТОЙ строки дельты ещё нет (процесс появился после
		// базового замера): клиент рисует «—», а не «0.0%», иначе «нет данных»
		// читается как «процессор свободен».
		CPUWarmup bool `json:"cpu_warmup,omitempty"`
	}

	guard := newSelfGuard()
	// Родители процессов WebView2 — для второго прохода ниже. Ppid() на Windows
	// стоит снимка дерева процессов, поэтому зовём его только для них.
	webviewParents := make(map[int32]int32)

	now := time.Now()
	cores := runtime.NumCPU()
	alive := make(map[int32]bool, len(procs))

	// Прогрев ВНУТРИ запроса: дельта требует двух замеров, поэтому на холодном
	// (или устаревшем) кэше сначала снимаем базу, ждём cpuWarmupWindow и только
	// потом считаем загрузку. Раньше первый показ вкладки честно не мог ничего
	// измерить и писал «CPU 0.0%» у всех строк, а второй замер приходил лишь
	// после ручного «↻» — и означал среднее за промежуток между нажатиями.
	baseAt := cpuSnapFresh()
	if baseAt.IsZero() || now.Sub(baseAt) > cpuSnapMaxAge {
		primeCPUSnap(procs, now)
		baseAt = now
		time.Sleep(cpuWarmupWindow)
		now = time.Now()
	}

	// Имя и состояние — одним снимком там, где поштучный вопрос дорог (macOS:
	// каждый Name()/Status() у gopsutil — это запуск `ps`). Пусто — идём
	// прежним путём, см. procmeta_darwin.go / procmeta_other.go.
	meta := procMetaSnapshot()

	measured := 0
	result := make([]procInfo, 0, len(procs))
	for _, p := range procs {
		var name string
		if m, ok := meta[p.Pid]; ok {
			name = m.name
		} else {
			name, _ = p.Name()
		}
		alive[p.Pid] = true
		// Загрузка ЗА ПОСЛЕДНИЙ ИНТЕРВАЛ, а не за всю жизнь процесса.
		var cpuPct float64
		cpuWarm := true // пригодной дельты нет, пока не доказано обратное
		if times, err := p.Times(); err == nil && times != nil {
			pct, ok := cpuDelta(p.Pid, times.User+times.System, now, cores)
			cpuPct = pct
			cpuWarm = !ok
			if ok {
				measured++
			}
		}
		memInfo, _ := p.MemoryInfo()
		var rss uint64
		if memInfo != nil {
			rss = memInfo.RSS
		}
		statusStr := ""
		if m, ok := meta[p.Pid]; ok {
			statusStr = m.status
		} else if status, _ := p.Status(); len(status) > 0 {
			statusStr = status[0]
		}
		self := guard.isSelfByName(p.Pid, name)
		if !self && isWebViewProc(name) {
			if ppid, err := p.Ppid(); err == nil {
				webviewParents[p.Pid] = ppid
			}
		}
		result = append(result, procInfo{
			PID:       p.Pid,
			Name:      name,
			CPU:       cpuPct,
			Memory:    rss,
			Status:    statusStr,
			Self:      self,
			Critical:  isCriticalProc(p.Pid, name),
			CPUWarmup: cpuWarm,
		})
	}

	// Окно панели: браузерный процесс WebView2 — ребёнок агента, а
	// renderer/GPU/crashpad — уже его дети. Помечаем волнами, пока множество
	// растёт (на практике хватает двух проходов). Флаги считаем ДО обрезки по
	// limit — они дешёвые, а клиенту нужны именно у видимых строк.
	if len(webviewParents) > 0 {
		ours := map[int32]bool{guard.pid: true}
		for i := 0; i < 4; i++ {
			grew := false
			for pid, ppid := range webviewParents {
				if !ours[pid] && ours[ppid] {
					ours[pid] = true
					grew = true
				}
			}
			if !grew {
				break
			}
		}
		for i := range result {
			if ours[result[i].PID] {
				result[i].Self = true
			}
		}
	}

	if sortBy == "cpu" {
		sort.Slice(result, func(i, j int) bool { return result[i].CPU > result[j].CPU })
	} else {
		sort.Slice(result, func(i, j int) bool { return result[i].Memory > result[j].Memory })
	}

	if len(result) > limit {
		result = result[:limit]
	}

	pruneCPUSnap(alive)
	// cpu_warmup — не удалось измерить НИ ОДНУ строку (крайний случай: все
	// процессы появились только что). Клиент показывает «—» вместо 0%, чтобы не
	// выдавать «нет данных» за «процессор свободен»; у отдельных строк тот же
	// смысл несёт их собственный cpu_warmup.
	// cpu_window_ms — за какой интервал посчитана загрузка: цифру можно честно
	// подписать «за последние N с», а не оставлять «процент непонятно чего».
	jsonResp(w, map[string]any{
		"processes":     result,
		"cpu_warmup":    measured == 0,
		"cpu_window_ms": now.Sub(baseAt).Milliseconds(),
	})
}

// powerDelay — окно отмены выключения/перезагрузки. Клиент рисует плашку
// «Выключаю через N секунд · Отменить» ровно на это время.
const powerDelay = 5 * time.Second

// deferredPower — отложенная команда питания ЭТОГО агента.
//
// Окно отмены обязана обеспечивать та сторона, которая умеет ждать. На Windows
// паузу даёт сама ОС (`shutdown /s /t 5`, отмена — `shutdown /a`): она переживает
// даже падение агента. У Linux/macOS секундной гранулярности у `shutdown` нет
// (`+0` — немедленно, `+1` — уже целая минута), поэтому там паузу держит агент:
// таймер на powerDelay, action=cancel его снимает. Раньше не-Windows отправлял
// `shutdown -h +0` сразу, а клиент всё равно показывал «Отменить» — кнопку,
// которой нечего отменять.
var deferredPower struct {
	mu     sync.Mutex
	timer  *time.Timer
	action string
}

// schedulePower ставит команду питания на powerDelay, заменяя предыдущую:
// второе нажатие не должно выключать компьютер дважды.
func schedulePower(action string, run func()) {
	deferredPower.mu.Lock()
	defer deferredPower.mu.Unlock()
	if deferredPower.timer != nil {
		deferredPower.timer.Stop()
	}
	deferredPower.action = action
	deferredPower.timer = time.AfterFunc(powerDelay, func() {
		deferredPower.mu.Lock()
		deferredPower.timer = nil
		deferredPower.action = ""
		deferredPower.mu.Unlock()
		run()
	})
}

// cancelDeferredPower снимает отложенную команду. Второе значение false —
// отменять нечего: её либо не было, либо она уже ушла в ОС.
func cancelDeferredPower() (string, bool) {
	deferredPower.mu.Lock()
	defer deferredPower.mu.Unlock()
	if deferredPower.timer == nil {
		return "", false
	}
	stopped := deferredPower.timer.Stop()
	action := deferredPower.action
	deferredPower.timer = nil
	deferredPower.action = ""
	return action, stopped
}

// runPowerCmd выполняет команду питания шеллом платформы. Вывод возвращаем
// вместе с ошибкой: для отложенной команды он попадёт только в лог — клиенту
// отвечать уже нечем.
func runPowerCmd(cmdStr string) (string, error) {
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.Command("cmd", "/c", cmdStr)
	} else {
		cmd = exec.Command("sh", "-c", cmdStr)
	}
	// Без окна: команду питания заказали с телефона, вывод мы читаем сами и
	// возвращаем в ответе. Человеку за ПК тут показывать нечего — только чёрный
	// прямоугольник cmd.exe на долю секунды.
	procutil.Hidden(cmd)
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func (s *Server) apiPower(w http.ResponseWriter, r *http.Request, uid int64) {
	var body struct {
		Action string `json:"action"`
	}
	if err := readJSON(r, &body); err != nil {
		jsonError(w, "action required", 400)
		return
	}

	delaySec := int(powerDelay.Seconds())
	cmds := map[string]string{
		"shutdown": fmt.Sprintf("shutdown /s /t %d", delaySec),
		"restart":  fmt.Sprintf("shutdown /r /t %d", delaySec),
		"sleep":    "rundll32.exe powrprof.dll,SetSuspendState 0,1,0",
		"lock":     "rundll32.exe user32.dll,LockWorkStation",
		"cancel":   "shutdown /a",
	}
	if runtime.GOOS != "windows" {
		cmds = map[string]string{
			"shutdown": "shutdown -h +0",
			"restart":  "shutdown -r +0",
			"sleep":    "systemctl suspend",
			"lock":     "loginctl lock-session",
			"cancel":   "shutdown -c",
		}
	}

	cmdStr, ok := cmds[body.Action]
	if !ok {
		jsonError(w, fmt.Sprintf("Unknown action: %s", body.Action), 400)
		return
	}

	// Audit: power actions are destructive — always log who triggered them.
	log.Printf("[SYSTEM][AUDIT] power action=%q by uid=%d ip=%s", body.Action, uid, r.RemoteAddr)

	if body.Action == "cancel" {
		// Сначала снимаем СВОЙ таймер (не-Windows), потом просим отмену у ОС.
		if action, ok := cancelDeferredPower(); ok {
			log.Printf("[SYSTEM][AUDIT] power action=%q canceled by uid=%d", action, uid)
			jsonResp(w, map[string]any{"ok": true, "action": "cancel", "canceled": action})
			return
		}
		if out, err := runPowerCmd(cmdStr); err != nil {
			// Отменять уже нечего: команда ушла в ОС (или ОС отмену не умеет).
			// Машинный код и 409, а не 500 с сырым текстом и не 403: любой 403
			// от агента выкидывает облачный клиент на экран входа (см.
			// apiProcessKill), а по коду экран напишет человеческую фразу.
			log.Printf("[SYSTEM] power cancel failed: %v (%s)", err, truncateStr(out, 200))
			jsonErrorCode(w, 409, "power_not_pending", "nothing to cancel", nil)
			return
		}
		jsonResp(w, map[string]any{"ok": true, "action": "cancel", "canceled": "os"})
		return
	}

	// Не-Windows: задержку держим сами, поэтому окно отмены становится честным
	// и на headless-сервере.
	if runtime.GOOS != "windows" && (body.Action == "shutdown" || body.Action == "restart") {
		// Проверяем ДО обещания: без бинарника `shutdown` команда упала бы уже
		// после ответа, и клиент показал бы «Выключаю» вместо понятного отказа.
		if _, err := exec.LookPath("shutdown"); err != nil {
			log.Printf("[SYSTEM] power action=%q unsupported: %v", body.Action, err)
			jsonErrorCode(w, 409, "power_unsupported", "shutdown command not found", nil)
			return
		}
		action := body.Action
		schedulePower(action, func() {
			if out, err := runPowerCmd(cmdStr); err != nil {
				log.Printf("[SYSTEM] deferred power action=%q failed: %v (%s)", action, err, truncateStr(out, 200))
			}
		})
		jsonResp(w, map[string]any{
			"ok": true, "action": action,
			"delay_ms": powerDelay.Milliseconds(), "cancelable": true,
		})
		return
	}

	if out, err := runPowerCmd(cmdStr); err != nil {
		log.Printf("[SYSTEM] power action=%q failed: %v (%s)", body.Action, err, truncateStr(out, 200))
		jsonError(w, err.Error(), 500)
		return
	}
	resp := map[string]any{"ok": true, "action": body.Action}
	if body.Action == "shutdown" || body.Action == "restart" {
		// Windows: окно отмены дала сама ОС (`/t N`), снимается `shutdown /a`.
		resp["delay_ms"] = powerDelay.Milliseconds()
		resp["cancelable"] = true
	}
	jsonResp(w, resp)
}

// truncateStr clamps a string for log lines.
func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// apiProcessKill завершает процесс по PID.
//
// ГРАБЛЯ ПРО СТАТУСЫ: «этот процесс завершать нельзя» отдаём 409, а НЕ 403.
// В облаке ответ агента релей проксирует вербатим, а клиент считает ЛЮБОЙ
// 401/403 отказом релея в авторизации (cloudApi в apk/src/api.ts): рвёт
// events-WS и уводит на экран входа. Отказ в завершении процесса не должен
// выкидывать пользователя из аккаунта, поэтому все отказы этого обработчика —
// 409 (или 404/500) с машинным code, по которому и ветвится экран.
func (s *Server) apiProcessKill(w http.ResponseWriter, r *http.Request, uid int64) {
	var body struct {
		PID int `json:"pid"`
		// Confirm — клиент показал предупреждение о последствиях. Без него свои
		// (self) и системные (critical) процессы не убиваем: 409 confirm_required.
		Confirm bool `json:"confirm"`
	}
	if err := readJSON(r, &body); err != nil || body.PID == 0 {
		jsonError(w, "pid required", 400)
		return
	}
	// Не даём снести критичные системные процессы (крах ОС): отрицательные/группа,
	// PID 4 (Windows System), 1 (init/systemd на Linux). Подтверждением не снимается.
	if body.PID < 0 || body.PID == 4 || (runtime.GOOS != "windows" && body.PID == 1) {
		log.Printf("[SYSTEM] refused kill of system pid=%d uid=%d", body.PID, uid)
		jsonErrorCode(w, 409, "process_system", "refusing to kill a system process", nil)
		return
	}

	pid := int32(body.PID)
	name := ""
	if p, err := process.NewProcess(pid); err == nil {
		name, _ = p.Name()
	}
	guard := newSelfGuard()
	isSelf := guard.isSelf(pid, name)
	isCritical := isCriticalProc(pid, name)
	if (isSelf || isCritical) && !body.Confirm {
		reason := "critical"
		if isSelf {
			reason = "self"
		}
		log.Printf("[SYSTEM] kill needs confirm: uid=%d pid=%d name=%q reason=%s", uid, pid, name, reason)
		jsonErrorCode(w, 409, "confirm_required",
			"this process belongs to Remotai or to the operating system — confirm explicitly",
			map[string]string{"reason": reason, "process": name})
		return
	}

	log.Printf("[SYSTEM][AUDIT] process kill uid=%d pid=%d name=%q self=%v critical=%v", uid, pid, name, isSelf, isCritical)

	// Завершение САМОГО агента: сначала отвечаем клиенту, потом выходим — иначе
	// TerminateProcess обрывает соединение и телефон видит «нет связи» вместо
	// честного «агент остановлен». os.Exit(0) вместо самоубийства сигналом —
	// тот же путь, что у автообновления (cmd/tgcontrol/autoupdate.go).
	if pid == guard.pid {
		jsonResp(w, map[string]any{"ok": true, "self": true})
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		go func() {
			time.Sleep(500 * time.Millisecond)
			os.Exit(0)
		}()
		return
	}

	proc, err := os.FindProcess(body.PID)
	if err != nil {
		// На Windows os.FindProcess — это OpenProcess: для защищённого процесса
		// он падает с «Access is denied», а раньше это отдавалось как 404
		// «Process not found» и читалось как «процесса уже нет».
		if isPermissionErr(err) {
			jsonErrorCode(w, 409, "process_protected", "process is protected: administrator rights required", nil)
			return
		}
		jsonErrorCode(w, 404, "not_found", "process not found", nil)
		return
	}
	if err := proc.Kill(); err != nil {
		log.Printf("[SYSTEM] kill pid=%d failed: %v", body.PID, err)
		if isPermissionErr(err) {
			jsonErrorCode(w, 409, "process_protected", "process is protected: administrator rights required", nil)
			return
		}
		// Процесс уже завершился между списком и нажатием — это не отказ, а
		// гонка: 404, как и «не найден» выше.
		if errors.Is(err, os.ErrProcessDone) {
			jsonErrorCode(w, 404, "not_found", "process not found", nil)
			return
		}
		// Прочее (сбой TerminateProcess и т.п.) — сбой операции на стороне ПК.
		// Раньше здесь стоял 403: в облаке он уводил пользователя на экран входа.
		jsonErrorCode(w, 500, "kill_failed", err.Error(), nil)
		return
	}
	jsonResp(w, map[string]bool{"ok": true})
}

func (s *Server) apiTerminalExec(w http.ResponseWriter, r *http.Request, uid int64) {
	var body struct {
		Command string `json:"command"`
		Cwd     string `json:"cwd"`
		Timeout int    `json:"timeout"`
	}
	if err := readJSON(r, &body); err != nil || body.Command == "" {
		jsonError(w, "command required", 400)
		return
	}
	if body.Cwd == "" {
		body.Cwd, _ = os.Getwd()
	}
	if body.Timeout <= 0 || body.Timeout > 120 {
		body.Timeout = 30
	}

	// Audit: arbitrary command execution — always log who ran what.
	log.Printf("[SYSTEM][AUDIT] terminal exec by uid=%d ip=%s cwd=%q cmd=%q", uid, r.RemoteAddr, body.Cwd, truncateStr(body.Command, 200))

	ctx, cancel := context.WithTimeout(r.Context(), time.Duration(body.Timeout)*time.Second)
	defer cancel()

	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.CommandContext(ctx, "cmd", "/c", body.Command)
	} else {
		cmd = exec.CommandContext(ctx, "sh", "-c", body.Command)
	}
	procutil.Prepare(cmd) // kill the whole tree on timeout, not just cmd.exe
	// Без окна: это быстрый прогон команды из клиента, весь вывод уезжает в
	// ответ. Окно cmd.exe тут ничего не показывает — только мигает на ПК.
	procutil.Hidden(cmd)
	cmd.Dir = body.Cwd

	out, err := cmd.CombinedOutput()
	output := string(out)
	exitCode := 0
	if err != nil {
		if ctx.Err() != nil {
			output = "Command timed out"
			exitCode = -1
		} else {
			exitCode = -1
			if exitErr, ok := err.(*exec.ExitError); ok {
				exitCode = exitErr.ExitCode()
			}
		}
	}

	// Limit output size
	if len(output) > 50000 {
		output = output[len(output)-50000:]
	}

	jsonResp(w, map[string]any{
		"output":    output,
		"exit_code": exitCode,
	})
}

func (s *Server) apiCostStats(w http.ResponseWriter, r *http.Request, uid int64) {
	allSessions := s.store.List(int(uid))
	totalCost := 0.0
	todayCost := 0.0
	weekCost := 0.0
	totalMessages := 0

	now := time.Now()
	todayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	weekStart := todayStart.AddDate(0, 0, -int(todayStart.Weekday()))

	for _, sess := range allSessions {
		msgs := s.history.Get(int(uid), sess.Name)
		for _, msg := range msgs {
			totalCost += msg.CostUSD
			totalMessages++
			msgTime := time.Unix(int64(msg.Timestamp), 0)
			if msgTime.After(todayStart) {
				todayCost += msg.CostUSD
			}
			if msgTime.After(weekStart) {
				weekCost += msg.CostUSD
			}
		}
	}
	jsonResp(w, map[string]any{
		"total_cost":     totalCost,
		"today_cost":     todayCost,
		"week_cost":      weekCost,
		"total_messages": totalMessages,
		// Границу суммы обязан знать не только этот экран: считаются ТОЛЬКО
		// сессии агентов, запущенные через бота. PTY-терминалы (где и живут
		// Claude Code/Codex) сюда не попадают, а подписочные CLI цену не
		// присылают вовсе — поэтому «$0.00» здесь означает «не измерено», а не
		// «AI бесплатен» (принцип «unknown cost is never $0»).
		"scope":              "bot_sessions",
		"terminals_included": false,
	})
}

// ── Кадр превью для «Скачать оригинал» ───────────────────────────────
//
// Кнопка «Скачать оригинал» просила у ОС НОВЫЙ снимок: человек снимал экран,
// чтобы поймать всплывшую ошибку, а в галерею уезжал уже другой момент — и
// понять это было нельзя. Поэтому превью и оригинал берём из ОДНОГО захвата:
// полноразмерный кадр остаётся в памяти агента, а `original=1` забирает именно
// его.
//
// Кадр один на процесс (агент обслуживает один компьютер) и отдаётся ОДИН раз:
// второй «оригинал» подряд честнее сделать свежим снимком с пометкой
// `cached:false`, чем молча выдать старый кадр за текущий — этим же снимается
// риск, что снимок из «Системы» подменит кадр в удалёнке. Через shotKeepFor
// кадр исчезает сам: 4K RGBA — это ≈33 МБ, держать их дольше незачем.
var lastShot struct {
	sync.Mutex
	img     *image.RGBA
	uid     int64
	display int
	at      time.Time
	timer   *time.Timer
}

const shotKeepFor = 2 * time.Minute

func rememberShot(uid int64, display int, img *image.RGBA) {
	lastShot.Lock()
	defer lastShot.Unlock()
	if lastShot.timer != nil {
		lastShot.timer.Stop()
	}
	lastShot.img, lastShot.uid, lastShot.display, lastShot.at = img, uid, display, time.Now()
	lastShot.timer = time.AfterFunc(shotKeepFor, func() {
		lastShot.Lock()
		defer lastShot.Unlock()
		if time.Since(lastShot.at) >= shotKeepFor {
			lastShot.img = nil
		}
	})
}

// takeShot забирает показанный кадр, если он относится к тому же человеку и
// тому же монитору и ещё не протух. nil — значит показанного кадра больше нет,
// и оригинал придётся снимать заново.
func takeShot(uid int64, display int) *image.RGBA {
	lastShot.Lock()
	defer lastShot.Unlock()
	img := lastShot.img
	if img == nil || lastShot.uid != uid || lastShot.display != display || time.Since(lastShot.at) > shotKeepFor {
		return nil
	}
	lastShot.img = nil
	return img
}

func (s *Server) apiScreenshot(w http.ResponseWriter, r *http.Request, uid int64) {
	numDisplays := screenshot.NumActiveDisplays()
	if numDisplays == 0 {
		jsonErrorCode(w, http.StatusServiceUnavailable, "no_display", "no displays found", nil)
		return
	}
	display := 0
	if raw := r.URL.Query().Get("display"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 0 || parsed >= numDisplays {
			jsonErrorCode(w, http.StatusBadRequest, "bad_display", "invalid display", nil)
			return
		}
		display = parsed
	}

	bounds := screenshot.GetDisplayBounds(display)
	var buf bytes.Buffer
	mime := "image/jpeg"
	original := r.URL.Query().Get("original") == "1"
	// Отдали ли ТОТ САМЫЙ кадр, который человек видел в превью. Клиент говорит
	// вслух, когда пришлось снимать заново: «сохранён свежий снимок».
	cached := false
	if original {
		img := takeShot(uid, display)
		if img == nil {
			fresh, err := screenshot.CaptureRect(bounds)
			if err != nil {
				jsonErrorCode(w, http.StatusServiceUnavailable, "screenshot_failed", "screenshot failed", nil)
				return
			}
			img = fresh
		} else {
			cached = true
		}
		if err := png.Encode(&buf, img); err != nil {
			jsonErrorCode(w, http.StatusInternalServerError, "screenshot_failed", "screenshot encode failed", nil)
			return
		}
		mime = "image/png"
	} else {
		width := 1280
		if raw := r.URL.Query().Get("w"); raw != "" {
			width = remotePreviewWidth(raw)
		}
		// Снимаем сами (а не через captureJPEG): полноразмерный кадр нужен
		// целиком — из него делается и превью, и будущий «оригинал».
		img, err := screenshot.CaptureRect(bounds)
		if err != nil {
			jsonErrorCode(w, http.StatusServiceUnavailable, "screenshot_failed", "screenshot failed", nil)
			return
		}
		if err := jpeg.Encode(&buf, scaleDown(img, width), &jpeg.Options{Quality: 72}); err != nil {
			jsonErrorCode(w, http.StatusInternalServerError, "screenshot_failed", "screenshot encode failed", nil)
			return
		}
		rememberShot(uid, display, img)
	}

	displays := make([]map[string]int, 0, numDisplays)
	for i := 0; i < numDisplays; i++ {
		b := screenshot.GetDisplayBounds(i)
		displays = append(displays, map[string]int{"id": i, "w": b.Dx(), "h": b.Dy()})
	}
	jsonResp(w, map[string]any{
		"data":     base64.StdEncoding.EncodeToString(buf.Bytes()),
		"mime":     mime,
		"display":  display,
		"width":    bounds.Dx(),
		"height":   bounds.Dy(),
		"displays": displays,
		"original": original,
		"cached":   cached,
	})
}

// apiScreenshotSave — снимок экрана ФАЙЛОМ НА САМ КОМПЬЮТЕР, а не на телефон.
//
// Зачем отдельная ручка. `/api/system/screenshot` отдаёт кадр в ответе, и
// клиент кладёт его в галерею телефона — это путь «посмотреть». Здесь путь
// другой и он ради агента: картинка должна лежать НА ПК, потому что читает её
// Claude Code или Codex, а они видят только файловую систему той машины, где
// работают. Просьба владельца 08.09: «удобное приложение, которое делает
// скрины, чтобы сразу закидывать в агента было легко» — до этого он держал для
// снимков отдельную программу, а путь к файлу набирал руками.
//
// Кладём в `~/Remotai/files` — ту самую папку, куда уже падают загрузки из
// приложения и «файлы для агентов» (internal/bundle). Никакого нового
// соглашения о местах не заводим: агент и так знает это место, а телефон умеет
// его листать.
//
// Имя — латиницей и с секундами: путь уходит в командную строку агента, где
// кириллица и пробелы требуют кавычек, а снимки одной минуты не должны
// затирать друг друга.
func (s *Server) apiScreenshotSave(w http.ResponseWriter, r *http.Request, uid int64) {
	numDisplays := screenshot.NumActiveDisplays()
	if numDisplays == 0 {
		jsonErrorCode(w, http.StatusServiceUnavailable, "no_display", "no displays found", nil)
		return
	}
	display := 0
	if raw := r.URL.Query().Get("display"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 0 || parsed >= numDisplays {
			jsonErrorCode(w, http.StatusBadRequest, "bad_display", "invalid display", nil)
			return
		}
		display = parsed
	}

	shot, code, reason := saveScreenshotFile(uid, display)
	if reason != "" {
		jsonErrorCode(w, code, reason, reason, nil)
		return
	}
	jsonResp(w, shot)
}

// saveScreenshotFile — единственное место, где снимок превращается в файл.
//
// Зовут двое: HTTP-ручка (кнопка 📷 в приложении) и горячая клавиша PrtScr на
// самом компьютере. Один путь для обоих намеренно: имя файла, папка и защита
// от недописанного PNG не должны разъезжаться между двумя способами снять
// экран. Возвращает готовый ответ, HTTP-код и машинную причину отказа
// (пустая — успех).
func saveScreenshotFile(uid int64, display int) (map[string]any, int, string) {
	dir := bundle.FilesDir()
	if dir == "" {
		return nil, http.StatusInternalServerError, "no_home"
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, http.StatusInternalServerError, "mkdir_failed"
	}

	// Тот же кадр, что человек видел в превью, если он ещё свеж: иначе снимок
	// «по кнопке» показывал бы одно, а агенту доставалось другое.
	img := takeShot(uid, display)
	cached := img != nil
	if img == nil {
		fresh, err := screenshot.CaptureRect(screenshot.GetDisplayBounds(display))
		if err != nil {
			return nil, http.StatusServiceUnavailable, "screenshot_failed"
		}
		img = fresh
	}

	name := fmt.Sprintf("screen-%s.png", time.Now().Format("20060102-150405"))
	path := filepath.Join(dir, name)
	// Пишем во временный файл рядом и переименовываем: агент может читать папку
	// в тот же момент, и недописанный PNG он прочтёт как битый файл.
	tmp := path + ".part"
	file, err := os.Create(tmp)
	if err != nil {
		return nil, http.StatusInternalServerError, "write_failed"
	}
	if err := png.Encode(file, img); err != nil {
		file.Close()
		os.Remove(tmp)
		return nil, http.StatusInternalServerError, "screenshot_failed"
	}
	if err := file.Close(); err != nil {
		os.Remove(tmp)
		return nil, http.StatusInternalServerError, "write_failed"
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return nil, http.StatusInternalServerError, "write_failed"
	}

	bounds := img.Bounds()
	return map[string]any{
		"path":    path,
		"name":    name,
		"dir":     dir,
		"display": display,
		"width":   bounds.Dx(),
		"height":  bounds.Dy(),
		"cached":  cached,
	}, http.StatusOK, ""
}

// ── Autostart ────────────────────────────────────────────────────────

// autostartState уезжает наружу двумя путями: собранной вручную картой в
// /api/system/autostart и как есть — в /api/setup/status и /api/setup/autostart
// (панель окна exe). Теги ОБЯЗАТЕЛЬНЫ: без них панель получала «Enabled», а
// читала `autostart.enabled` — тумблер всегда рисовался выключенным, а после
// успешного включения тостил «Автозапуск выключен».
type autostartState struct {
	Enabled     bool   `json:"enabled"`
	Method      string `json:"method"`
	Recommended string `json:"recommended"`
	Legacy      bool   `json:"legacy"`
	// ManagedExternally — автозапуском управляет система (system-юнит systemd,
	// поставленный install.sh). Клиент не должен ни предлагать «Исправить», ни
	// показывать тумблер: создание user-юнита рядом породит ВТОРОЙ агент на том
	// же порту.
	ManagedExternally bool `json:"managed_externally"`
}

const windowsAutostartTaskName = "TGControlUser"

func (s *Server) apiAutostartStatus(w http.ResponseWriter, r *http.Request, uid int64) {
	status := getAutostartState()
	jsonResp(w, map[string]any{
		"enabled":            status.Enabled,
		"method":             status.Method,
		"recommended":        status.Recommended,
		"legacy":             status.Legacy,
		"managed_externally": status.ManagedExternally,
	})
}

func (s *Server) apiAutostartToggle(w http.ResponseWriter, r *http.Request, uid int64) {
	var body struct {
		Enable bool `json:"enable"`
	}
	if err := readJSON(r, &body); err != nil {
		jsonError(w, "enable field required", 400)
		return
	}

	var err error
	if body.Enable {
		err = enableAutostart()
	} else {
		err = disableAutostart()
	}
	if err != nil {
		// Автозапуском управляет система (system-юнит systemd от install.sh):
		// раньше «выключить» отвечало успехом, ничего не меняя, — человек не
		// знал, поднимется ли сервер после ребута. Машинный код, текст — на
		// клиенте (там же read-only строка вместо тумблера).
		if errors.Is(err, errAutostartManagedExternally) {
			jsonErrorCode(w, http.StatusConflict, "managed_externally", err.Error(),
				map[string]string{"method": getAutostartState().Method, "unit": systemUnitPath})
			return
		}
		jsonError(w, err.Error(), 500)
		return
	}
	status := getAutostartState()
	jsonResp(w, map[string]any{
		"ok":                 true,
		"enabled":            status.Enabled,
		"method":             status.Method,
		"recommended":        status.Recommended,
		"legacy":             status.Legacy,
		"managed_externally": status.ManagedExternally,
	})
}

func getAutostartState() autostartState {
	if runtime.GOOS == "darwin" {
		return autostartState{Enabled: service.AutostartEnabled(), Method: "launch_agent", Recommended: "launch_agent"}
	}
	exe, err := os.Executable()
	if err != nil {
		return autostartState{Method: "none"}
	}
	if runtime.GOOS == "windows" {
		if windowsScheduledTaskEnabled(windowsAutostartTaskName) {
			return autostartState{
				Enabled:     true,
				Method:      "scheduled_task",
				Recommended: "scheduled_task",
			}
		}
		if windowsStartupShortcutExists() {
			return autostartState{
				Enabled:     true,
				Method:      "startup_shortcut",
				Recommended: "scheduled_task",
				Legacy:      true,
			}
		}
		return autostartState{Method: "none", Recommended: "scheduled_task"}
	}
	// Linux, профиль headless-сервера: install.sh / `remotai install` ставит
	// SYSTEM-юнит. Его надо проверять ПЕРВЫМ: раньше про него никто не знал,
	// телефон показывал «Автозапуск выключен», а кнопка «Исправить» создавала
	// user-юнит — второй агент на том же порту 8080, то есть UI своими руками
	// ломал корректную установку.
	if data, err := os.ReadFile(systemUnitPath); err == nil {
		if strings.Contains(string(data), "remotai") {
			return autostartState{
				Enabled:           linuxAutostartEnabled(false),
				Method:            "systemd_system",
				Recommended:       "systemd_system",
				ManagedExternally: true,
			}
		}
	}
	// Linux: check systemd user service (десктоп-профиль).
	home, _ := os.UserHomeDir()
	servicePath := filepath.Join(home, ".config", "systemd", "user", "remotai.service")
	data, err := os.ReadFile(servicePath)
	if err != nil {
		return autostartState{Method: "none", Recommended: "systemd_user"}
	}
	owned := strings.Contains(string(data), exe) || strings.Contains(string(data), service.SystemdExecPath(exe))
	if owned {
		return autostartState{Enabled: linuxAutostartEnabled(true), Method: "systemd_user", Recommended: "systemd_user"}
	}
	return autostartState{Method: "none", Recommended: "systemd_user"}
}

// systemUnitPath — куда install.sh / `remotai install` кладут system-юнит.
const systemUnitPath = "/etc/systemd/system/remotai.service"

// errAutostartManagedExternally — автозапуском владеет система (system-юнит
// systemd). Менять его из приложения нельзя: включение создало бы ВТОРОЙ агент
// на порту 8080, а «выключение» user-юнита не трогает системный сервис и раньше
// молча отвечало успехом. HTTP-слой превращает эту ошибку в код
// managed_externally, CLI просто печатает текст.
var errAutostartManagedExternally = errors.New("автозапуском управляет системный сервис systemd")

// A unit file can remain installed while startup is disabled. Ask the manager
// about its persistent enablement, not the current PID or file presence.
func linuxAutostartEnabled(user bool) bool {
	args := []string{"is-enabled", "remotai.service"}
	if user {
		args = append([]string{"--user"}, args...)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	output, err := procutil.Hidden(exec.CommandContext(ctx, "systemctl", args...)).Output()
	return err == nil && strings.TrimSpace(string(output)) == "enabled"
}

func runUserAutostart(args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	output, err := procutil.Hidden(exec.CommandContext(ctx, "systemctl", append([]string{"--user"}, args...)...)).CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemctl %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(output)))
	}
	return nil
}

// EnableAutostart / DisableAutostart экспортированы для CLI-флагов
// --enable-autostart / --disable-autostart (их вызывает инсталлятор).
func EnableAutostart() error  { return enableAutostart() }
func DisableAutostart() error { return disableAutostart() }

// AutostartEnabled keeps CLI diagnostics consistent with the desktop panel.
func AutostartEnabled() bool { return getAutostartState().Enabled }

func enableAutostart() error {
	if runtime.GOOS == "darwin" {
		return service.SetAutostart(true)
	}
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("cannot find executable: %w", err)
	}
	exeDir := filepath.Dir(exe)

	if runtime.GOOS == "windows" {
		if service.RunAsService() {
			return fmt.Errorf("cannot configure user-session autostart while running as Windows Service")
		}
		if err := createWindowsUserAutostartTask(exe, exeDir); err != nil {
			return err
		}
		return removeWindowsStartupShortcut()
	}

	// Hard-guard: если автозапуском уже управляет system-юнит (install.sh /
	// `remotai install`), НЕ создаём user-юнит — иначе на сервере поднимется
	// второй агент и оба будут драться за порт 8080. Именно это делала кнопка
	// «Исправить» на корректно установленном сервере.
	if st := getAutostartState(); st.ManagedExternally {
		return fmt.Errorf("%w (%s) — вмешательство не нужно", errAutostartManagedExternally, systemUnitPath)
	}

	// Linux: systemd user service (десктоп-профиль A). Для headless-сервера
	// install.sh ставит system-unit — см. deploy/systemd/remotai.service.
	home, _ := os.UserHomeDir()
	serviceDir := filepath.Join(home, ".config", "systemd", "user")
	if err := os.MkdirAll(serviceDir, 0755); err != nil {
		return err
	}
	// Restart=always + KillMode=process зеркалят серверный профиль: автообновление
	// делает os.Exit(0) и полагается на рестарт юнита (см. autoupdate.go), а
	// KillMode=process не трогает персистентные PTY-хосты (systemd-run --scope)
	// при перезапуске сервиса.
	serviceContent := fmt.Sprintf(`[Unit]
Description=Remotai
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=%s --background
Restart=always
RestartSec=3
KillMode=process

[Install]
WantedBy=default.target
`, service.SystemdExecPath(exe))
	servicePath := filepath.Join(serviceDir, "remotai.service")
	if err := os.WriteFile(servicePath, []byte(serviceContent), 0644); err != nil {
		return err
	}
	// This desktop setting changes the next login only. Starting a service
	// here creates a second process beside the manually opened application.
	// Linger belongs to the explicit server installer, not this toggle.
	if err := runUserAutostart("daemon-reload"); err != nil {
		return err
	}
	return runUserAutostart("enable", "remotai.service")
}

func disableAutostart() error {
	if runtime.GOOS == "darwin" {
		return service.SetAutostart(false)
	}
	if runtime.GOOS == "windows" {
		var errs []string
		if err := deleteWindowsUserAutostartTask(); err != nil {
			errs = append(errs, err.Error())
		}
		if err := removeWindowsStartupShortcut(); err != nil {
			errs = append(errs, err.Error())
		}
		if len(errs) > 0 {
			return fmt.Errorf("%s", strings.Join(errs, "; "))
		}
		return nil
	}
	// Тот же hard-guard, что и на включении, но по обратной причине: у
	// system-юнита нет user-юнита, который можно выключить, — команда ниже
	// отработала бы «успешно», не изменив ничего, и человек считал бы
	// автозапуск отключённым. Отключать такой сервис надо на самой машине
	// (systemctl disable remotai).
	if st := getAutostartState(); st.ManagedExternally {
		return fmt.Errorf("%w (%s) — отключайте его на самом компьютере: systemctl disable remotai", errAutostartManagedExternally, systemUnitPath)
	}
	// Preserve the running agent and the loaded unit, including its restart
	// policy. Removing next-login links must not stop current work.
	return runUserAutostart("disable", "remotai.service")
}

func windowsScheduledTaskExists(taskName string) bool {
	if runtime.GOOS != "windows" {
		return false
	}
	// Без окна: это тихая проверка «есть ли задача автозапуска», её зовут при
	// каждом открытии настроек — иначе каждый раз мигало бы окно schtasks.
	return procutil.Hidden(exec.Command("schtasks", "/Query", "/TN", taskName)).Run() == nil
}

// Existence alone is not enough: Windows lets the user disable a task while
// retaining it. Report that as off, and never revive it during health checks.
func windowsScheduledTaskEnabled(taskName string) bool {
	if runtime.GOOS != "windows" {
		return false
	}
	out, err := powershellCommand("-NoProfile", "-Command",
		"$t = Get-ScheduledTask -TaskName "+psQuote(taskName)+" -ErrorAction SilentlyContinue; if ($t -and $t.State -ne 'Disabled') { 'enabled' }").Output()
	return err == nil && strings.TrimSpace(string(out)) == "enabled"
}

// autostartWatchdogMinutes — как часто задача проверяет, жив ли агент.
//
// Задача с MultipleInstances=IgnoreNew не поднимет второй экземпляр, пока
// работает первый: пока процесс жив, повторный тик ничего не делает. А если
// процесс закрылся — вылетел, человек снял его в диспетчере, убил антивирус, —
// следующий тик поднимет его обратно. Без этого триггера агент лежал до
// следующего входа в систему, то есть управление компьютером терялось на часы.
const autostartWatchdogMinutes = 5

// windowsAutostartScript собирает скрипт регистрации задачи. Вынесен отдельно
// от вызова, чтобы состав задачи (триггеры, батарея, экземпляры) проверялся
// тестом без Windows и без планировщика.
func windowsAutostartScript(exe, exeDir string) string {
	return fmt.Sprintf(`
$ErrorActionPreference = 'Stop'
$taskName = %s
$exePath = %s
$workDir = %s
$user = "$env:USERDOMAIN\$env:USERNAME"
if ([string]::IsNullOrWhiteSpace($env:USERDOMAIN)) { $user = $env:USERNAME }
# --background: запуск СТОРОЖЕМ, а не человеком. Без флага каждый его тик
# (раз в %d минут) поднимал окно приложения поверх работы — агент-то жив, и
# запущенный экземпляр «активировал существующее окно». Живая жалоба 31.07.
$action = New-ScheduledTaskAction -Execute $exePath -Argument '--background' -WorkingDirectory $workDir
# Два триггера: вход в систему (после перезагрузки) и повтор каждые %d минут —
# сторож на случай, если агент закрылся сам. Повтор бесконечный: длительность
# повтора не задаём вовсе (пустая и означает «навсегда»).
$atLogon = New-ScheduledTaskTrigger -AtLogOn
# Длительность повтора НЕ задаём: пустая означает «бесконечно». Любое явное
# значение (включая TimeSpan::MaxValue) планировщик отвергает —
# «XML содержит значение в неправильном формате» (проверено на живой машине).
$watchdog = New-ScheduledTaskTrigger -Once -At (Get-Date).AddMinutes(1) -RepetitionInterval (New-TimeSpan -Minutes %d)
$principal = New-ScheduledTaskPrincipal -UserId $user -LogonType Interactive -RunLevel Limited
# AllowStartIfOnBatteries + DontStopIfGoingOnBatteries: по умолчанию Windows НЕ
# запускает задачу на ноутбуке от батареи и останавливает уже запущенную. Для
# нас это значит «компьютер отключился от розетки — управление пропало».
$settings = New-ScheduledTaskSettingsSet -MultipleInstances IgnoreNew `+"`"+`
  -RestartCount 3 -RestartInterval (New-TimeSpan -Minutes 1) `+"`"+`
  -ExecutionTimeLimit (New-TimeSpan -Seconds 0) `+"`"+`
  -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries
Register-ScheduledTask -TaskName $taskName -Action $action -Trigger @($atLogon, $watchdog) -Principal $principal -Settings $settings -Description 'Remotai: автозапуск при входе и сторож (поднимает агент, если он закрылся)' -Force | Out-Null
`, psQuote(windowsAutostartTaskName), psQuote(exe), psQuote(exeDir),
		autostartWatchdogMinutes, autostartWatchdogMinutes, autostartWatchdogMinutes)
}

func createWindowsUserAutostartTask(exe, exeDir string) error {
	out, err := powershellCommand("-NoProfile", "-ExecutionPolicy", "Bypass", "-Command",
		windowsAutostartScript(exe, exeDir)).CombinedOutput()
	if err != nil {
		return fmt.Errorf("create Windows autostart task: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// windowsAutostartNeedsUpgrade — у задачи автозапуска нет сторожа или она не
// работает от батареи.
//
// Задачи, созданные прежними версиями, так и остались бы с одним логон-триггером:
// человек включил автозапуск однажды и больше в настройки не заходит. Поэтому
// проверяем это на каждом старте агента и молча дорегистрируем задачу.
func windowsAutostartNeedsUpgrade() bool {
	if runtime.GOOS != "windows" {
		return false
	}
	out, err := powershellCommand("-NoProfile", "-ExecutionPolicy", "Bypass", "-Command",
		windowsAutostartProbe()).Output()
	if err != nil {
		return false // не смогли спросить — не трогаем работающую задачу
	}
	return strings.Contains(strings.TrimSpace(string(out)), "upgrade")
}

// windowsAutostartProbe — скрипт, который отвечает absent | ok | upgrade.
func windowsAutostartProbe() string {
	return fmt.Sprintf(`
$t = Get-ScheduledTask -TaskName %s -ErrorAction SilentlyContinue
if (-not $t -or $t.State -eq 'Disabled') { 'absent'; exit }
$hasWatchdog = $false
foreach ($tr in $t.Triggers) { if ($tr.Repetition -and $tr.Repetition.Interval) { $hasWatchdog = $true } }
$battery = (-not $t.Settings.DisallowStartIfOnBatteries) -and (-not $t.Settings.StopIfGoingOnBatteries)
# Аргумент --background: без него каждый тик сторожа поднимает окно поверх
# работы человека (жалоба 31.07). Задачи прежних версий его не имеют.
$silent = $false
foreach ($a in $t.Actions) { if ($a.Arguments -and $a.Arguments.Contains('--background')) { $silent = $true } }
if ($hasWatchdog -and $battery -and $silent) { 'ok' } else { 'upgrade' }
`, psQuote(windowsAutostartTaskName))
}

// EnsureAutostartHealthy доводит задачу автозапуска до текущих требований:
// сторож раз в несколько минут и работа от батареи. Ничего не делает, если
// автозапуск выключен человеком (задачи нет) или уже всё в порядке.
func EnsureAutostartHealthy() {
	if runtime.GOOS != "windows" {
		return
	}
	if !windowsAutostartNeedsUpgrade() {
		return
	}
	exe, err := os.Executable()
	if err != nil {
		return
	}
	if err := createWindowsUserAutostartTask(exe, filepath.Dir(exe)); err != nil {
		log.Printf("[AUTOSTART] задачу не удалось обновить: %v", err)
		return
	}
	log.Printf("[AUTOSTART] задача обновлена: сторож раз в %d мин и работа от батареи", autostartWatchdogMinutes)
}

func deleteWindowsUserAutostartTask() error {
	if !windowsScheduledTaskExists(windowsAutostartTaskName) {
		return nil
	}
	// Без окна: удаление задачи автозапуска — фоновое действие по переключателю
	// в настройках, вывод schtasks мы читаем сами и кладём в текст ошибки.
	out, err := procutil.Hidden(exec.Command("schtasks", "/Delete", "/TN", windowsAutostartTaskName, "/F")).CombinedOutput()
	if err != nil {
		return fmt.Errorf("delete Windows autostart task: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func windowsStartupShortcutPath() string {
	home, _ := os.UserHomeDir()
	if home == "" {
		return ""
	}
	return filepath.Join(home, "AppData", "Roaming", "Microsoft", "Windows", "Start Menu", "Programs", "Startup", "tgcontrol.lnk")
}

func windowsStartupShortcutExists() bool {
	path := windowsStartupShortcutPath()
	if path == "" {
		return false
	}
	_, err := os.Stat(path)
	return err == nil
}

func removeWindowsStartupShortcut() error {
	path := windowsStartupShortcutPath()
	if path == "" {
		return nil
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove legacy startup shortcut: %w", err)
	}
	return nil
}

func psQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// powershellCommand / powershellCommandContext всегда запускают PowerShell БЕЗ
// окна. Через них идут только фоновые вещи (буфер обмена, регистрация задачи
// автозапуска, сбор сведений о системе) — показывать человеку там нечего, а
// powershell.exe консольный, и из оконного remotai.exe каждый вызов рисовал бы
// собственное чёрное окно. Если когда-нибудь понадобится PowerShell С окном —
// заводить отдельный конструктор, а не убирать Hidden отсюда.
func powershellCommand(args ...string) *exec.Cmd {
	return procutil.Hidden(exec.Command(powershellExecutable(), args...))
}

func powershellCommandContext(ctx context.Context, args ...string) *exec.Cmd {
	return procutil.Hidden(exec.CommandContext(ctx, powershellExecutable(), args...))
}

func powershellExecutable() string {
	if runtime.GOOS != "windows" {
		return "powershell"
	}
	for _, root := range []string{os.Getenv("SystemRoot"), os.Getenv("WINDIR"), `C:\Windows`} {
		if root == "" {
			continue
		}
		path := filepath.Join(root, "System32", "WindowsPowerShell", "v1.0", "powershell.exe")
		if _, err := os.Stat(path); err == nil {
			return path
		}
	}
	return "powershell.exe"
}

// windowsAutostartScriptForTest и windowsAutostartProbeForTest открывают
// содержимое скриптов тестам: состав задачи автозапуска — это контракт
// («агент возвращается сам»), и он должен проверяться, а не пересматриваться
// на живой машине после жалобы.
func windowsAutostartScriptForTest() string {
	return windowsAutostartScript(`C:\Remotai\remotai.exe`, `C:\Remotai`)
}

func windowsAutostartProbeForTest() string { return windowsAutostartProbe() }
