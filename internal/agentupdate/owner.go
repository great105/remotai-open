// Package agentupdate узнаёт, какой версии CLI-агент стоит на компьютере, КТО
// его поставил и какой командой его обновить тем же менеджером.
//
// Зачем «тем же менеджером». Агент, поставленный через npm, и тот же агент,
// поставленный нативным установщиком, обновляются разными командами, а чужая
// команда не обновляет, а СТАВИТ ВТОРУЮ КОПИЮ: `npm i -g` поверх нативного
// Claude даёт два claude в PATH, и какой из них запустится — решает порядок
// каталогов, а не человек. Поэтому владельца определяем по пути бинаря, а когда
// признаков нет — честно говорим «не знаем, обновить нельзя», а не угадываем.
package agentupdate

import (
	"path/filepath"
	"strings"
)

// Владельцы установки. Строки уходят клиенту как есть (поле owner).
const (
	OwnerNPM          = "npm"
	OwnerPNPM         = "pnpm"
	OwnerBun          = "bun"
	OwnerVolta        = "volta"
	OwnerClaudeNative = "claude-native"
	OwnerBrew         = "brew"
	OwnerWinget       = "winget"
	OwnerScoop        = "scoop"
	OwnerUnknown      = "unknown"
)

// OwnerTitle — как назвать владельца человеку (запасной текст: у клиента свои
// строки в i18n, это на случай старого клиента или нового владельца).
func OwnerTitle(owner string) string {
	switch owner {
	case OwnerNPM:
		return "npm"
	case OwnerPNPM:
		return "pnpm"
	case OwnerBun:
		return "Bun"
	case OwnerVolta:
		return "Volta"
	case OwnerClaudeNative:
		return "установщик Claude"
	case OwnerBrew:
		return "Homebrew"
	case OwnerWinget:
		return "winget"
	case OwnerScoop:
		return "Scoop"
	}
	return "неизвестно"
}

// Env — всё, что определение владельца знает о машине. Отдельной структурой,
// а не чтением os.Getenv по месту: так правило проверяется тестом и для
// Windows, и для POSIX на одной машине.
type Env struct {
	GOOS         string
	Home         string
	AppData      string // %APPDATA% (Windows)
	LocalAppData string // %LOCALAPPDATA% (Windows)
	PNPMHome     string // $PNPM_HOME
	VoltaHome    string // $VOLTA_HOME
	BunInstall   string // $BUN_INSTALL
	// NPMPrefix — ответ `npm prefix -g` (пусто = npm не ответил).
	NPMPrefix string
	// Resolve раскрывает симлинки (на POSIX обёртка npm — симлинк в
	// lib/node_modules). nil = путь как есть.
	Resolve func(string) string
	// Exists — есть ли файл. nil = считаем, что нет.
	Exists func(string) bool
}

// Install — что известно об установке одного агента.
type Install struct {
	Owner string
	// Prefix — каталог глобальных пакетов npm, куда поставлен агент (для npm).
	Prefix string
	// Package — npm-пакет (для npm/pnpm/bun/volta).
	Package string
	// Name — имя у системного менеджера: формула/cask Homebrew, id winget,
	// приложение Scoop.
	Name string
	Cask bool
	// Bin — базовое имя найденного бинаря (claude.exe, claude.cmd, claude).
	Bin string
}

// normPath приводит путь к виду для сравнения: прямые слэши, без хвостового.
// Регистр не трогаем — сравниваем сегменты через sameSeg.
func normPath(p string) string {
	p = strings.ReplaceAll(p, `\`, "/")
	for len(p) > 1 && strings.HasSuffix(p, "/") {
		p = strings.TrimSuffix(p, "/")
	}
	return p
}

func segments(p string) []string {
	var out []string
	for _, s := range strings.Split(normPath(p), "/") {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

func (e Env) sameSeg(a, b string) bool {
	if e.GOOS == "windows" || e.GOOS == "darwin" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

// under — лежит ли path внутри dir (по сегментам, без учёта регистра на
// Windows и macOS).
func (e Env) under(path, dir string) bool {
	if dir == "" {
		return false
	}
	ps, ds := segments(path), segments(dir)
	if len(ds) == 0 || len(ps) <= len(ds) {
		return false
	}
	for i := range ds {
		if !e.sameSeg(ps[i], ds[i]) {
			return false
		}
	}
	return true
}

// findSeg ищет подряд идущие сегменты want и возвращает индекс первого (-1 нет).
func (e Env) findSeg(segs []string, want ...string) int {
	for i := 0; i+len(want) <= len(segs); i++ {
		ok := true
		for j, w := range want {
			if !e.sameSeg(segs[i+j], w) {
				ok = false
				break
			}
		}
		if ok {
			return i
		}
	}
	return -1
}

func (e Env) join(parts ...string) string {
	return normPath(strings.Join(parts, "/"))
}

func (e Env) resolve(p string) string {
	if e.Resolve == nil {
		return p
	}
	if r := e.Resolve(p); r != "" {
		return r
	}
	return p
}

func (e Env) exists(p string) bool {
	return e.Exists != nil && e.Exists(p)
}

func baseName(p string) string {
	segs := segments(p)
	if len(segs) == 0 {
		return ""
	}
	return segs[len(segs)-1]
}

// pkgFromNodeModules достаёт имя пакета из пути …/node_modules/<pkg>/… (с
// учётом scope `@org/name`).
func pkgFromNodeModules(segs []string, i int) string {
	if i+1 >= len(segs) {
		return ""
	}
	name := segs[i+1]
	if strings.HasPrefix(name, "@") && i+2 < len(segs) {
		return name + "/" + segs[i+2]
	}
	return name
}

// Detect определяет владельца установки агента agentID по пути бинаря.
// pkg — npm-пакет из реестра (может быть пустым: aider, amazon-q).
//
// Порядок важен: pnpm, bun и Volta тоже держат внутри node_modules, поэтому
// общий признак npm проверяется последним.
func Detect(agentID, path, pkg string, env Env) Install {
	in := Install{Owner: OwnerUnknown, Package: pkg, Bin: baseName(path)}
	if path == "" || path == "built-in" {
		return in
	}
	real := env.resolve(path)
	home := env.Home
	win := env.GOOS == "windows"

	// Нативный установщик Claude: ~/.local/bin/claude(.exe) — ссылка или копия
	// из ~/.local/share/claude/versions/<версия>; старая «локальная» установка
	// жила в ~/.claude/local. Обе обновляются `claude update`. Только для
	// claude: в ~/.local/bin на Linux кладут и pipx, и установщик cursor-agent.
	if agentID == "claude" && home != "" {
		if env.under(path, env.join(home, ".local", "bin")) ||
			env.under(real, env.join(home, ".local", "share", "claude")) ||
			env.under(path, env.join(home, ".claude", "local")) ||
			env.under(real, env.join(home, ".claude", "local")) {
			in.Owner = OwnerClaudeNative
			return in
		}
	}

	voltaHome := env.VoltaHome
	if voltaHome == "" && home != "" {
		voltaHome = env.join(home, ".volta")
		if win && env.LocalAppData != "" {
			// Volta на Windows по умолчанию живёт в %LOCALAPPDATA%\Volta.
			if env.under(path, env.join(env.LocalAppData, "Volta")) || env.under(real, env.join(env.LocalAppData, "Volta")) {
				in.Owner = OwnerVolta
				return in
			}
		}
	}
	if env.under(path, voltaHome) || env.under(real, voltaHome) {
		in.Owner = OwnerVolta
		return in
	}

	bunHome := env.BunInstall
	if bunHome == "" && home != "" {
		bunHome = env.join(home, ".bun")
	}
	if env.under(path, bunHome) || env.under(real, bunHome) {
		in.Owner = OwnerBun
		return in
	}

	for _, dir := range env.pnpmDirs() {
		if env.under(path, dir) || env.under(real, dir) {
			in.Owner = OwnerPNPM
			return in
		}
	}
	realSegs := segments(real)
	if env.findSeg(realSegs, ".pnpm") >= 0 || env.findSeg(realSegs, "pnpm", "global") >= 0 {
		in.Owner = OwnerPNPM
		return in
	}

	// Scoop: ~/scoop/apps/<app>/current/… или ~/scoop/shims/<name>.exe.
	pathSegs := segments(path)
	for _, segs := range [][]string{realSegs, pathSegs} {
		if i := env.findSeg(segs, "scoop", "apps"); i >= 0 && i+2 < len(segs) {
			in.Owner, in.Name = OwnerScoop, segs[i+2]
			return in
		}
	}
	if i := env.findSeg(pathSegs, "scoop", "shims"); i >= 0 {
		in.Owner = OwnerScoop
		in.Name = strings.TrimSuffix(in.Bin, filepath.Ext(in.Bin))
		return in
	}

	// winget: …\WinGet\Packages\<Id>_Microsoft.Winget.Source_<хэш>\…; каталог
	// Links — ссылки туда же (Resolve раскроет).
	for _, segs := range [][]string{realSegs, pathSegs} {
		if i := env.findSeg(segs, "WinGet", "Packages"); i >= 0 && i+2 < len(segs) {
			in.Owner, in.Name = OwnerWinget, wingetID(segs[i+2])
			return in
		}
	}
	if env.findSeg(pathSegs, "WinGet", "Links") >= 0 {
		in.Owner = OwnerWinget // имя пакета из ссылки не видно — обновить не сможем
		return in
	}

	// Homebrew: /opt/homebrew/Cellar/<formula>/<версия>/…, Caskroom/<cask>/….
	for _, segs := range [][]string{realSegs, pathSegs} {
		if i := env.findSeg(segs, "Cellar"); i >= 0 && i+1 < len(segs) {
			in.Owner, in.Name = OwnerBrew, segs[i+1]
			return in
		}
		if i := env.findSeg(segs, "Caskroom"); i >= 0 && i+1 < len(segs) {
			in.Owner, in.Name, in.Cask = OwnerBrew, segs[i+1], true
			return in
		}
	}
	if !win && (env.under(path, "/opt/homebrew") || env.under(path, "/home/linuxbrew/.linuxbrew")) {
		in.Owner = OwnerBrew // формулу не узнали — обновить не сможем
		return in
	}

	// npm, POSIX: обёртка — симлинк в <prefix>/lib/node_modules/<pkg>/….
	if i := env.findSeg(realSegs, "lib", "node_modules"); i >= 0 {
		in.Owner = OwnerNPM
		in.Prefix = "/" + strings.Join(realSegs[:i], "/")
		if p := pkgFromNodeModules(realSegs, i+1); p != "" {
			in.Package = p
		}
		return in
	}
	// npm, Windows: обёртки лежат прямо в префиксе, пакет — в <prefix>\node_modules.
	dir := env.join(pathSegs[:max(len(pathSegs)-1, 0)]...)
	if !win && strings.HasPrefix(normPath(path), "/") {
		dir = "/" + dir
	}
	if pkg != "" && env.exists(env.join(dir, "node_modules", pkg, "package.json")) {
		in.Owner, in.Prefix = OwnerNPM, dir
		return in
	}
	if env.NPMPrefix != "" {
		prefixBin := env.NPMPrefix
		if !win {
			prefixBin = env.join(env.NPMPrefix, "bin")
		}
		if env.under(path, prefixBin) {
			in.Owner, in.Prefix = OwnerNPM, normPath(env.NPMPrefix)
			return in
		}
	}
	if win && env.AppData != "" && env.under(path, env.join(env.AppData, "npm")) {
		in.Owner, in.Prefix = OwnerNPM, env.join(env.AppData, "npm")
		return in
	}
	return in
}

func (e Env) pnpmDirs() []string {
	var dirs []string
	if e.PNPMHome != "" {
		dirs = append(dirs, e.PNPMHome)
	}
	if e.GOOS == "windows" && e.LocalAppData != "" {
		dirs = append(dirs, e.join(e.LocalAppData, "pnpm"))
	}
	if e.Home != "" {
		dirs = append(dirs, e.join(e.Home, ".local", "share", "pnpm"))
		if e.GOOS == "darwin" {
			dirs = append(dirs, e.join(e.Home, "Library", "pnpm"))
		}
	}
	return dirs
}

// wingetID: «Amazon.AmazonQ_Microsoft.Winget.Source_8wekyb3d8bbwe» → «Amazon.AmazonQ».
func wingetID(seg string) string {
	lower := strings.ToLower(seg)
	if i := strings.Index(lower, "_microsoft.winget.source"); i > 0 {
		return seg[:i]
	}
	if i := strings.LastIndex(seg, "_"); i > 0 {
		return seg[:i]
	}
	return seg
}

// PackageFromInstall достаёт npm-пакет из команды установки реестра
// («npm i -g @openai/codex» → «@openai/codex»). Пусто — установка не через npm.
func PackageFromInstall(install string) string {
	words := strings.Fields(install)
	if len(words) < 3 {
		return ""
	}
	if w := strings.ToLower(words[0]); w != "npm" && w != "npm.cmd" {
		return ""
	}
	if w := words[1]; w != "i" && w != "install" && w != "add" {
		return ""
	}
	global := false
	pkg := ""
	for _, w := range words[2:] {
		switch {
		case w == "-g" || w == "--global":
			global = true
		case strings.HasPrefix(w, "-"):
		default:
			if pkg != "" {
				return "" // несколько пакетов — не наш случай
			}
			pkg = w
		}
	}
	if !global {
		return ""
	}
	return stripVersion(pkg)
}

// stripVersion: «@scope/name@1.2» → «@scope/name», «name@latest» → «name».
func stripVersion(pkg string) string {
	at := strings.LastIndex(pkg, "@")
	if at > 0 {
		return pkg[:at]
	}
	return pkg
}
