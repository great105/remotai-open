package pty

// Парсер ~/.ssh/config (OpenSSH) для списка «известных» SSH-хостов агента.
//
// Свой построчный парсер, без внешних зависимостей: поддерживает то, что
// реально встречается в пользовательских конфигах — блоки Host, поля
// HostName/User/Port/IdentityFile/ProxyJump, директиву Include (glob,
// рекурсивно, с защитой от циклов), дефолты из блоков с паттернами
// (Host * и опции до первого Host) и комментарий-теги вида
// "# Tags: prod, eu" перед блоком (конвенция sshm).
//
// Семантика OpenSSH «первое значение побеждает» сохранена: повторный блок с
// тем же именем и повторные поля внутри блока игнорируются.

import (
	"bufio"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// SSHConfigHost — один именованный блок Host из ssh-конфига.
type SSHConfigHost struct {
	Name         string   // алиас из строки "Host name"
	Host         string   // HostName; если не задан — совпадает с Name
	Port         int      // 0 = порт по умолчанию (22)
	User         string   // может быть пустым — клиент подставит свой
	IdentityFile string   // как в конфиге (~ не разворачиваем — путь показываем как есть)
	ProxyJump    string   // "user@host:port" или пусто ("none" трактуем как пусто)
	Tags         []string // из "# Tags: a, b" перед блоком
}

// sshConfigFields — поля, которые нас интересуют внутри блока.
type sshConfigFields struct {
	host         string
	user         string
	port         int
	identityFile string
	proxyJump    string
}

type sshConfigParser struct {
	visited  map[string]bool // абсолютные пути уже разобранных файлов (защита от циклов Include)
	seen     map[string]bool // имена хостов, уже попавшие в результат (first-wins)
	defaults sshConfigFields // дефолты: опции до первого Host + блоки Host */паттерны
	hosts    []SSHConfigHost

	cur        []*SSHConfigHost // записи текущего блока (nil = дефолтный блок)
	curDefault bool             // текущий блок — паттерн с wildcard (или до первого Host)
	pendingTag []string         // последний "# Tags:" комментарий, ждёт своего Host
}

// ParseSSHConfig разбирает ssh-конфиг по указанному пути (включая Include).
// Отсутствующий файл — не ошибка: хостов просто нет.
func ParseSSHConfig(path string) ([]SSHConfigHost, error) {
	p := &sshConfigParser{
		visited: make(map[string]bool),
		seen:    make(map[string]bool),
	}
	p.curDefault = true // опции до первого Host — глобальные дефолты
	if err := p.parseFile(path); err != nil {
		return nil, err
	}
	// Дефолты докладываем в конце: у OpenSSH Host * читается «отовсюду»,
	// а первое конкретное значение в блоке всё равно побеждает.
	out := make([]SSHConfigHost, 0, len(p.hosts))
	for _, h := range p.hosts {
		if h.Host == "" {
			h.Host = firstNonEmpty(p.defaults.host, h.Name)
		}
		if h.User == "" {
			h.User = p.defaults.user
		}
		if h.Port == 0 {
			h.Port = p.defaults.port
		}
		if h.IdentityFile == "" {
			h.IdentityFile = p.defaults.identityFile
		}
		if h.ProxyJump == "" {
			h.ProxyJump = p.defaults.proxyJump
		}
		out = append(out, h)
	}
	return out, nil
}

// LoadSSHConfigHosts читает ~/.ssh/config текущего пользователя. Ошибки чтения
// проглатываем — битый конфиг пользователя не должен ронять список хостов.
func LoadSSHConfigHosts() []SSHConfigHost {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return nil
	}
	hosts, err := ParseSSHConfig(filepath.Join(home, ".ssh", "config"))
	if err != nil {
		return nil
	}
	return hosts
}

func (p *sshConfigParser) parseFile(path string) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	if p.visited[abs] {
		return nil // цикл Include — выходим молча
	}
	p.visited[abs] = true

	f, err := os.Open(path)
	if err != nil {
		return nil // отсутствующий/недоступный файл — не ошибка (Include по glob тоже)
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "#") {
			if tags, ok := parseTagsComment(line); ok {
				p.pendingTag = tags
			}
			continue
		}
		key, val := splitSSHConfigLine(line)
		if key == "" {
			continue
		}
		lkey := strings.ToLower(key)
		if lkey != "host" {
			// Любая не-Host директива «отрывает» теги от предыдущего комментария:
			// они привязаны строго к блоку, который идёт сразу за ними.
			p.pendingTag = nil
		}
		switch lkey {
		case "host":
			p.beginBlock(strings.Fields(val))
		case "include":
			p.include(val, filepath.Dir(abs))
		default:
			p.applyOption(lkey, val)
		}
	}
	return sc.Err()
}

// beginBlock открывает новый Host-блок. Блоки, где ВСЕ паттерны содержат
// wildcard (* или ?), складываются в дефолты — именованными хостами в UI они
// быть не могут. Из смешанного блока берём только именованные паттерны.
func (p *sshConfigParser) beginBlock(patterns []string) {
	p.cur = nil
	p.curDefault = false
	tags := p.pendingTag
	p.pendingTag = nil

	var names []string
	for _, pat := range patterns {
		if strings.ContainsAny(pat, "*?") || strings.HasPrefix(pat, "!") {
			continue
		}
		names = append(names, pat)
	}
	if len(names) == 0 {
		p.curDefault = true // Host * и подобные — источник дефолтов
		return
	}
	for _, name := range names {
		if p.seen[name] {
			continue // first-wins: повторный блок с тем же именем игнорируем
		}
		p.seen[name] = true
		p.hosts = append(p.hosts, SSHConfigHost{Name: name, Tags: tags})
		p.cur = append(p.cur, &p.hosts[len(p.hosts)-1])
	}
}

// include разворачивает директиву Include: glob относительно папки текущего
// файла, рекурсия с защитой от циклов (см. visited).
func (p *sshConfigParser) include(val, baseDir string) {
	for _, pat := range strings.Fields(val) {
		pat = expandHomePath(pat)
		if !filepath.IsAbs(pat) {
			pat = filepath.Join(baseDir, pat)
		}
		matches, err := filepath.Glob(pat)
		if err != nil {
			continue // битый паттерн — пропускаем
		}
		sort.Strings(matches) // детерминированный порядок (OpenSSH — лексикографический)
		for _, m := range matches {
			_ = p.parseFile(m)
		}
	}
}

// applyOption раскладывает поле по записям текущего блока (first-wins) или в
// дефолты. Незнакомые директивы игнорируем — нас интересует только адрес.
func (p *sshConfigParser) applyOption(key, val string) {
	val = strings.Trim(val, `"'`)
	if p.curDefault {
		f := &p.defaults
		switch key {
		case "hostname":
			if f.host == "" {
				f.host = val
			}
		case "user":
			if f.user == "" {
				f.user = val
			}
		case "port":
			if f.port == 0 {
				f.port = parseSSHPort(val)
			}
		case "identityfile":
			if f.identityFile == "" {
				f.identityFile = val
			}
		case "proxyjump":
			if f.proxyJump == "" {
				f.proxyJump = normalizeProxyJump(val)
			}
		}
		return
	}
	for _, h := range p.cur {
		switch key {
		case "hostname":
			if h.Host == "" {
				h.Host = val
			}
		case "user":
			if h.User == "" {
				h.User = val
			}
		case "port":
			if h.Port == 0 {
				h.Port = parseSSHPort(val)
			}
		case "identityfile":
			if h.IdentityFile == "" {
				h.IdentityFile = val
			}
		case "proxyjump":
			if h.ProxyJump == "" {
				h.ProxyJump = normalizeProxyJump(val)
			}
		}
	}
}

// splitSSHConfigLine делит строку на ключ и значение: OpenSSH допускает и
// "Key value", и "Key = value" (равно может липнуть к любой стороне).
func splitSSHConfigLine(line string) (key, val string) {
	if i := strings.IndexAny(line, " \t"); i > 0 {
		key, val = line[:i], strings.TrimSpace(line[i+1:])
	} else {
		key = line
	}
	if i := strings.Index(key, "="); i > 0 {
		key, val = key[:i], strings.TrimSpace(key[i+1:]+val)
	} else if strings.HasPrefix(val, "=") {
		val = strings.TrimSpace(val[1:])
	}
	return key, val
}

// parseTagsComment вытаскивает теги из комментария "# Tags: a, b" (как у sshm).
func parseTagsComment(line string) ([]string, bool) {
	body := strings.TrimSpace(strings.TrimPrefix(line, "#"))
	if len(body) < 6 || !strings.EqualFold(body[:5], "tags:") {
		return nil, false
	}
	var tags []string
	for _, t := range strings.Split(body[5:], ",") {
		if t = strings.TrimSpace(t); t != "" {
			tags = append(tags, t)
		}
	}
	return tags, len(tags) > 0
}

func parseSSHPort(s string) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || n < 0 || n > 65535 {
		return 0
	}
	return n
}

// normalizeProxyJump — "none" у OpenSSH означает «без прыжка», храним пусто.
func normalizeProxyJump(s string) string {
	if strings.EqualFold(strings.TrimSpace(s), "none") {
		return ""
	}
	return s
}

func expandHomePath(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			return filepath.Join(home, strings.TrimPrefix(p, "~/"))
		}
	}
	return p
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
