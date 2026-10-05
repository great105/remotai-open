package agentupdate

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"tgcontrol/internal/procutil"
)

// Runner запускает команду и отдаёт её вывод. Подменяется в тестах: настоящие
// CLI владельца в пробах НЕ запускаются (у него в это время живые сессии).
type Runner func(ctx context.Context, name string, args ...string) ([]byte, error)

// ExecRunner — настоящий запуск. Фоновый, поэтому через procutil.Hidden:
// иначе на Windows каждый опрос версии мигал бы чёрным окном.
func ExecRunner(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	procutil.Hidden(cmd)
	// npm и так любит печатать «доступна новая версия npm» — в разбор версии
	// это не попадёт, но лишний поход в сеть на каждый замер не нужен.
	cmd.Env = append(os.Environ(), "NO_UPDATE_NOTIFIER=1", "npm_config_update_notifier=false")
	// .cmd-обёртка на Windows — это cmd.exe, под которым живёт node. По
	// таймауту снимается cmd.exe, а node держит трубы; без WaitDelay ожидание
	// вывода висело бы до его естественного конца.
	cmd.WaitDelay = 2 * time.Second
	// По таймауту снимаем всё дерево, а не одну обёртку cmd.exe: иначе node,
	// спрашивающий `--version` или реестр npm, оставался жить (скептик 29.09).
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return procutil.KillPIDTree(cmd.Process.Pid)
	}
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		return out, ctx.Err()
	}
	return out, err
}

// Agent — вход проверки: обнаруженный агент из реестра.
type Agent struct {
	ID      string
	Name    string
	Path    string // найденный бинарь
	Install string // команда установки из реестра (из неё берём npm-пакет)
}

// Item — ответ GET /api/agents/updates для одного агента.
type Item struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Path       string `json:"path"`
	Version    string `json:"version"`
	Latest     string `json:"latest"`
	Owner      string `json:"owner"`
	OwnerTitle string `json:"owner_title"`
	Package    string `json:"package,omitempty"`
	// UpdateCommand — что напечатать в терминал; CanUpdate=false — нечем.
	UpdateCommand string `json:"update_command"`
	CanUpdate     bool   `json:"can_update"`
	Reason        string `json:"reason,omitempty"`
	// LatestKnown — последняя версия узнаётся без обновления. false у
	// нативного Claude: «проверит сам установщик».
	LatestKnown     bool   `json:"latest_known"`
	LatestError     string `json:"latest_error,omitempty"`
	VersionError    string `json:"version_error,omitempty"`
	UpdateAvailable bool   `json:"update_available"`
	// RunningSessions — живые терминалы с этим агентом (заполняет web).
	RunningSessions int   `json:"running_sessions"`
	CheckedAt       int64 `json:"checked_at"`
}

type versionEntry struct {
	version string
	err     string
	mtime   int64
	at      time.Time
}

type latestEntry struct {
	version string
	err     string
	at      time.Time
}

// Checker — замер версий с кэшем. Один на процесс.
type Checker struct {
	Run     Runner
	Env     func() Env
	Look    LookPath
	History *History
	Now     func() time.Time
	// Mtime — время изменения бинаря (0 = не знаем). Смена mtime сбрасывает
	// кэш версии: npm при переустановке переписывает обёртки.
	Mtime func(path string) int64
	// ReadFile — чтение package.json, когда --version не ответил (nil = не читать).
	ReadFile func(path string) ([]byte, error)

	VersionTimeout time.Duration
	LatestTimeout  time.Duration
	PrefixTimeout  time.Duration
	VersionTTL     time.Duration
	LatestTTL      time.Duration
	LatestFailTTL  time.Duration
	PrefixTTL      time.Duration

	checkMu  sync.Mutex // одна проверка за раз: вторая ждёт и берёт кэш
	mu       sync.Mutex
	versions map[string]versionEntry
	latest   map[string]latestEntry
	prefix   string
	prefixAt time.Time
}

// NewLive — проверка на настоящей машине с историей в historyPath.
func NewLive(historyPath string) *Checker {
	return &Checker{
		Run:      ExecRunner,
		Env:      LiveEnv,
		Look:     func(name string) bool { _, err := exec.LookPath(name); return err == nil },
		History:  NewHistory(historyPath),
		ReadFile: os.ReadFile,
		Mtime: func(p string) int64 {
			if st, err := os.Stat(p); err == nil {
				return st.ModTime().UnixNano()
			}
			return 0
		},
	}
}

// LiveEnv собирает Env текущего процесса (NPMPrefix заполняет Checker).
func LiveEnv() Env {
	home, _ := os.UserHomeDir()
	return Env{
		GOOS:         runtime.GOOS,
		Home:         home,
		AppData:      os.Getenv("APPDATA"),
		LocalAppData: os.Getenv("LOCALAPPDATA"),
		PNPMHome:     os.Getenv("PNPM_HOME"),
		VoltaHome:    os.Getenv("VOLTA_HOME"),
		BunInstall:   os.Getenv("BUN_INSTALL"),
		Resolve: func(p string) string {
			r, err := filepath.EvalSymlinks(p)
			if err != nil {
				return ""
			}
			return r
		},
		Exists: func(p string) bool { _, err := os.Stat(p); return err == nil },
	}
}

func dur(v, def time.Duration) time.Duration {
	if v > 0 {
		return v
	}
	return def
}

func (c *Checker) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *Checker) npmName(goos string) string {
	if goos == "windows" {
		return "npm.cmd"
	}
	return "npm"
}

// npmPrefix — `npm prefix -g`, кэш на час.
func (c *Checker) npmPrefix(ctx context.Context, goos string) string {
	c.mu.Lock()
	if !c.prefixAt.IsZero() && c.now().Sub(c.prefixAt) < dur(c.PrefixTTL, time.Hour) {
		p := c.prefix
		c.mu.Unlock()
		return p
	}
	c.mu.Unlock()
	cctx, cancel := context.WithTimeout(ctx, dur(c.PrefixTimeout, 10*time.Second))
	defer cancel()
	out, err := c.Run(cctx, c.npmName(goos), "prefix", "-g")
	p := ""
	if err == nil {
		p = lastLine(string(out))
	}
	c.mu.Lock()
	c.prefix, c.prefixAt = p, c.now()
	c.mu.Unlock()
	return p
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

// version — `<бинарь> --version` с кэшем по пути и mtime.
func (c *Checker) version(ctx context.Context, path string, refresh bool) (string, string) {
	var mtime int64
	if c.Mtime != nil {
		mtime = c.Mtime(path)
	}
	c.mu.Lock()
	if e, ok := c.versions[path]; ok && !refresh && e.mtime == mtime &&
		c.now().Sub(e.at) < dur(c.VersionTTL, 30*time.Minute) {
		c.mu.Unlock()
		return e.version, e.err
	}
	c.mu.Unlock()
	cctx, cancel := context.WithTimeout(ctx, dur(c.VersionTimeout, 15*time.Second))
	defer cancel()
	out, err := c.Run(cctx, path, "--version")
	e := versionEntry{mtime: mtime, at: c.now()}
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		e.err = "не ответил на --version вовремя"
	case err != nil && ParseVersion(string(out)) == "":
		e.err = "не ответил на --version"
	default:
		e.version = ParseVersion(string(out))
		if e.version == "" {
			e.err = "версия не распознана"
		}
	}
	c.mu.Lock()
	if c.versions == nil {
		c.versions = map[string]versionEntry{}
	}
	c.versions[path] = e
	c.mu.Unlock()
	return e.version, e.err
}

// latestFor — `npm view <pkg> version`, кэш 6 часов (неудача — 10 минут).
func (c *Checker) latestFor(ctx context.Context, pkg, goos string, refresh bool) (string, string) {
	c.mu.Lock()
	if e, ok := c.latest[pkg]; ok {
		ttl := dur(c.LatestTTL, 6*time.Hour)
		if e.err != "" {
			ttl = dur(c.LatestFailTTL, 10*time.Minute)
		}
		age := c.now().Sub(e.at)
		// «Проверить сейчас» идёт в реестр заново, но не чаще раза в минуту:
		// кнопку жмут подряд, а ответ реестра за минуту не меняется.
		if age < ttl && !(refresh && age >= time.Minute) {
			c.mu.Unlock()
			return e.version, e.err
		}
	}
	c.mu.Unlock()
	cctx, cancel := context.WithTimeout(ctx, dur(c.LatestTimeout, 20*time.Second))
	defer cancel()
	out, err := c.Run(cctx, c.npmName(goos), "view", pkg, "version")
	e := latestEntry{at: c.now()}
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		e.err = "реестр npm не ответил вовремя"
	case err != nil:
		e.err = "не удалось спросить реестр npm"
	default:
		e.version = ParseVersion(lastLine(string(out)))
		if e.version == "" {
			e.err = "реестр npm ответил без версии"
		}
	}
	c.mu.Lock()
	if c.latest == nil {
		c.latest = map[string]latestEntry{}
	}
	c.latest[pkg] = e
	c.mu.Unlock()
	return e.version, e.err
}

// Check замеряет всех агентов параллельно. refresh — «проверить сейчас»:
// версию спрашиваем заново, реестр — если ответу больше минуты.
func (c *Checker) Check(ctx context.Context, list []Agent, refresh bool) []Item {
	c.checkMu.Lock()
	defer c.checkMu.Unlock()
	env := Env{GOOS: runtime.GOOS}
	if c.Env != nil {
		env = c.Env()
	}
	env.NPMPrefix = c.npmPrefix(ctx, env.GOOS)

	items := make([]Item, len(list))
	var wg sync.WaitGroup
	for i, a := range list {
		wg.Add(1)
		go func(i int, a Agent) {
			defer wg.Done()
			items[i] = c.checkOne(ctx, a, env, refresh)
		}(i, a)
	}
	wg.Wait()
	return items
}

func (c *Checker) checkOne(ctx context.Context, a Agent, env Env, refresh bool) Item {
	in := Detect(a.ID, a.Path, PackageFromInstall(a.Install), env)
	cmd := BuildCommand(in, env, c.Look)
	it := Item{
		ID: a.ID, Name: a.Name, Path: a.Path,
		Owner: in.Owner, OwnerTitle: OwnerTitle(in.Owner), Package: in.Package,
		UpdateCommand: cmd.Text, CanUpdate: cmd.CanUpdate, Reason: cmd.Reason,
		LatestKnown: cmd.LatestKnown, CheckedAt: c.now().UnixMilli(),
	}
	it.Version, it.VersionError = c.version(ctx, a.Path, refresh)
	if it.Version == "" && in.Owner == OwnerNPM {
		// --version не ответил (агент занят обновлением, завис) — у npm-пакета
		// версия лежит в его package.json, это тот же факт без запуска.
		if v := packageJSONVersion(in, env, c.ReadFile); v != "" {
			it.Version, it.VersionError = v, ""
		}
	}
	if cmd.LatestKnown && in.Package != "" {
		it.Latest, it.LatestError = c.latestFor(ctx, in.Package, env.GOOS, refresh)
	}
	it.UpdateAvailable = it.CanUpdate && it.Version != "" && it.Latest != "" &&
		CompareVersions(it.Version, it.Latest) < 0
	if c.History != nil && it.Version != "" {
		_, _ = c.History.Record(a.ID, it.Version, in.Owner, c.now().UnixMilli())
	}
	return it
}

func packageJSONVersion(in Install, env Env, read func(string) ([]byte, error)) string {
	if read == nil || in.Prefix == "" || in.Package == "" {
		return ""
	}
	p := env.join(in.Prefix, "node_modules", in.Package, "package.json")
	if env.GOOS != "windows" {
		p = env.join(in.Prefix, "lib", "node_modules", in.Package, "package.json")
		if !strings.HasPrefix(p, "/") {
			p = "/" + p
		}
	}
	raw, err := read(p)
	if err != nil {
		return ""
	}
	var pj struct {
		Version string `json:"version"`
	}
	if json.Unmarshal(raw, &pj) != nil {
		return ""
	}
	return ParseVersion(pj.Version)
}

// Forget сбрасывает кэш версий (после «перепроверить» и после обновления).
func (c *Checker) Forget() {
	c.mu.Lock()
	c.versions = nil
	c.mu.Unlock()
}
