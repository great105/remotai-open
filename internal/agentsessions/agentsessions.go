// Package agentsessions — список прошлых бесед Claude Code и Codex на этом
// компьютере по всем аккаунтам: экран «Беседы» (просьба владельца 29.09.2026).
//
// Беседы агенты пишут сами, и лежат они там, где их положил CLI:
//   - Claude: <каталог аккаунта>/projects/<папка>/<номер беседы>.jsonl;
//   - Codex: <CODEX_HOME>/sessions/ГГГГ/ММ/ДД/rollout-*.jsonl, номер и папка —
//     в первой строке session_meta.
//
// Сколько там всего, замерено на ПК владельца 29.09.2026: 322 беседы Claude на
// 2,8 ГБ и 889 файлов Codex на 10 ГБ, из которых 493 — субагенты. Поэтому:
//   - для заголовка читаем только ГОЛОВУ файла (первое настоящее сообщение
//     человека в среднем на 28 КБ у Claude и 76 КБ у Codex, потолок 1 МБ);
//   - что прочитано, помним по (путь, размер, время изменения) и на диске —
//     перезапуск Remotai не должен заново читать десятки мегабайт;
//   - сообщения считает фоновый проход, дочитывающий только дописанное:
//     первый проход — это гигабайты, и ответ списка их ждать не может;
//   - у запроса есть бюджет времени: не успели — отдаём, что успели, с
//     признаком partial, а свежие беседы разбираются первыми.
//
// Наружу уходят номер, папка, заголовок (~120 символов первого сообщения
// человека), время и число сообщений. Текст беседы целиком не уходит никогда.
package agentsessions

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"tgcontrol/internal/atomicfile"
)

const (
	// DefaultLimit / MaxLimit — размер страницы списка.
	DefaultLimit = 50
	MaxLimit     = 200
	// maxFiles — предохранитель от каталога-свалки: дальше не смотрим.
	maxFiles = 20000
	// listBudget — сколько запрос списка может разбирать новые файлы.
	listBudget = 5 * time.Second
)

// Root — каталог одного аккаунта одного агента.
type Root struct {
	Agent        string // "claude" | "codex"
	AccountID    string // "default" у основного
	AccountLabel string
	Dir          string // CLAUDE_CONFIG_DIR / CODEX_HOME (уже разрешённый)
}

// Open — беседа сейчас идёт (или спит) в терминале Remotai.
type Open struct {
	PtyID    string
	Sleeping bool
}

// Key — ключ беседы в карте открытых.
func Key(agent, sessionID string) string { return agent + "/" + sessionID }

// Item — одна беседа в ответе API.
type Item struct {
	Agent        string `json:"agent"`
	SessionID    string `json:"session_id"`
	AccountID    string `json:"account_id"`
	AccountLabel string `json:"account_label,omitempty"`
	CWD          string `json:"cwd"`
	Title        string `json:"title"`
	UpdatedAt    int64  `json:"updated_at"` // unix ms — последняя запись в беседу
	// Messages — сообщения человека и ответы агента текстом. Нет поля —
	// ещё не посчитано (фоновый проход не дошёл до файла).
	Messages *int `json:"messages,omitempty"`
	// OpenPtyID — беседа идёт или спит в этом терминале Remotai: открывать
	// надо его, второй запуск той же беседы — две копии, пишущие в один файл.
	OpenPtyID string `json:"open_pty_id,omitempty"`
	Sleeping  bool   `json:"sleeping,omitempty"`
	// Running — Claude держит эту беседу открытой вне Remotai (живой PID в
	// <каталог>/sessions). Продолжать можно, но человек должен знать.
	Running bool `json:"running,omitempty"`
}

// Query — параметры запроса списка.
type Query struct {
	Q      string
	Agent  string
	Limit  int
	Cursor string
}

// Page — ответ списка.
type Page struct {
	Sessions []Item         `json:"sessions"`
	Next     string         `json:"next,omitempty"`
	Total    int            `json:"total"`
	Agents   map[string]int `json:"agents"`
	// Partial — не все файлы успели разобрать за бюджет запроса.
	Partial bool `json:"partial,omitempty"`
	// Counting — фоновый подсчёт сообщений ещё идёт.
	Counting bool `json:"counting,omitempty"`
}

// Номер беседы уходит в командную строку шелла — те же правила, что у
// усыпления (internal/pty/agent_sleep.go).
var sessionIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{7,127}$`)

// ValidSessionID — номер годится для команды продолжения.
func ValidSessionID(id string) bool { return sessionIDPattern.MatchString(id) }

var codexFileID = regexp.MustCompile(`([0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12})\.jsonl$`)

// entry — что известно про один файл. Поля экспортированы ради JSON-кэша.
type entry struct {
	Agent     string `json:"a"`
	Account   string `json:"acc"`
	Label     string `json:"lbl,omitempty"`
	SessionID string `json:"sid"`
	CWD       string `json:"cwd,omitempty"`
	Title     string `json:"t,omitempty"`
	// TitleAI — заголовок взят из ai-title хвоста (человеческого сообщения в
	// голове нет): он меняется по ходу беседы, его перечитываем при росте.
	TitleAI bool `json:"tai,omitempty"`
	// HeadFull — голова прочитана до потолка или до конца файла, и
	// человеческого сообщения там уже не появится.
	HeadFull bool  `json:"hf,omitempty"`
	HeadSize int64 `json:"hs"`
	Hidden   bool  `json:"h,omitempty"` // субагент или файл без беседы
	Size     int64 `json:"sz"`
	ModNs    int64 `json:"m"`
	CountOff int64 `json:"co"`
	Messages int   `json:"n"`
}

type diskIndex struct {
	Version int               `json:"v"`
	Files   map[string]*entry `json:"files"`
}

const diskVersion = 1

// Index — разобранные файлы бесед. Один на процесс.
type Index struct {
	statePath string
	// PIDAlive — жив ли процесс (для «открыта вне Remotai»). nil — не проверяем.
	PIDAlive func(pid int) bool
	// CountPause — отдых между порциями фонового подсчёта.
	CountPause time.Duration

	mu     sync.Mutex
	files  map[string]*entry
	dirty  bool
	loaded bool

	workerOnce sync.Once
	// NoBackground — не поднимать фоновый подсчёт (тесты и замеры зовут
	// CountPass сами).
	NoBackground bool
	kick         chan struct{}
	counting     bool
	countMu      sync.Mutex // один проход подсчёта за раз
	// wanted — файлы последней отданной страницы: считаем только их (под mu).
	// nil — ещё ни одной страницы не отдавали, считать можно всё (тесты).
	wanted map[string]bool
}

// NewIndex — индекс с кэшем в statePath ("" — только в памяти).
func NewIndex(statePath string) *Index {
	return &Index{statePath: statePath, files: map[string]*entry{}, kick: make(chan struct{}, 1), CountPause: 10 * time.Millisecond}
}

func (ix *Index) loadLocked() {
	if ix.loaded {
		return
	}
	ix.loaded = true
	if ix.statePath == "" {
		return
	}
	data, err := os.ReadFile(ix.statePath)
	if err != nil {
		return
	}
	var d diskIndex
	if json.Unmarshal(data, &d) != nil || d.Version != diskVersion || d.Files == nil {
		return
	}
	ix.files = d.Files
}

// Save пишет кэш на диск, если он менялся.
func (ix *Index) Save() error {
	ix.mu.Lock()
	if !ix.dirty || ix.statePath == "" {
		ix.mu.Unlock()
		return nil
	}
	data, err := json.Marshal(diskIndex{Version: diskVersion, Files: ix.files})
	ix.dirty = false
	ix.mu.Unlock()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(ix.statePath), 0o700); err != nil {
		return err
	}
	return atomicfile.WriteFile(ix.statePath, data, 0o600)
}

type found struct {
	path string
	root Root
	size int64
	mod  time.Time
}

// scanRoots — все файлы бесед под каталогами аккаунтов. Один файл у двух
// записей аккаунтов (один каталог) считается один раз.
func scanRoots(roots []Root) []found {
	var out []found
	seen := map[string]bool{}
	add := func(p string, root Root, info os.FileInfo) {
		key := strings.ToLower(filepath.Clean(p))
		if seen[key] || len(out) >= maxFiles {
			return
		}
		seen[key] = true
		out = append(out, found{path: p, root: root, size: info.Size(), mod: info.ModTime()})
	}
	for _, root := range roots {
		if root.Dir == "" {
			continue
		}
		switch root.Agent {
		case "claude":
			base := filepath.Join(root.Dir, "projects")
			dirs, err := os.ReadDir(base)
			if err != nil {
				continue
			}
			for _, d := range dirs {
				if !d.IsDir() {
					continue
				}
				files, err := os.ReadDir(filepath.Join(base, d.Name()))
				if err != nil {
					continue
				}
				for _, f := range files {
					name := f.Name()
					// agent-*.jsonl — ветки субагентов старых версий Claude.
					if f.IsDir() || !strings.HasSuffix(name, ".jsonl") || strings.HasPrefix(name, "agent-") {
						continue
					}
					if !ValidSessionID(strings.TrimSuffix(name, ".jsonl")) {
						continue
					}
					if info, err := f.Info(); err == nil {
						add(filepath.Join(base, d.Name(), name), root, info)
					}
				}
			}
		case "codex":
			base := filepath.Join(root.Dir, "sessions")
			_ = filepath.WalkDir(base, func(p string, d os.DirEntry, err error) error {
				if err != nil {
					if d != nil && d.IsDir() {
						return filepath.SkipDir
					}
					return nil
				}
				if d.IsDir() || !strings.HasSuffix(d.Name(), ".jsonl") || !strings.HasPrefix(d.Name(), "rollout-") {
					return nil
				}
				if info, err := d.Info(); err == nil {
					add(p, root, info)
				}
				return nil
			})
		}
	}
	return out
}

// refresh приводит запись файла к его текущему виду. Возвращает false, если
// разбор головы не уложился в ctx.
func (ix *Index) refresh(ctx context.Context, f found) bool {
	ix.mu.Lock()
	e := ix.files[f.path]
	var prev entry
	if e != nil {
		prev = *e
	}
	ix.mu.Unlock()

	modNs := f.mod.UnixNano()
	if e != nil && prev.Size == f.size && prev.ModNs == modNs && prev.Account == f.root.AccountID && prev.Label == f.root.AccountLabel {
		return true
	}
	next := prev
	next.Agent, next.Account, next.Label = f.root.Agent, f.root.AccountID, f.root.AccountLabel
	grown := e != nil && f.size >= prev.Size && prev.Agent == f.root.Agent
	if !grown {
		// Новый или переписанный файл: всё с нуля.
		next = entry{Agent: f.root.Agent, Account: f.root.AccountID, Label: f.root.AccountLabel}
	}
	// Голова файла дописыванием не меняется: перечитываем её, только если
	// человеческого сообщения в ней ещё не было и она была не полной.
	needHead := !grown || (next.Title == "" && !next.HeadFull) || next.TitleAI || (next.Hidden && !next.HeadFull)
	if needHead {
		if ctx.Err() != nil {
			return false
		}
		h, err := readHead(f.path, f.root.Agent)
		if err != nil {
			return true // файл исчез или заперт — попробуем в следующий раз
		}
		next.SessionID, next.CWD, next.Title, next.TitleAI = h.sessionID, h.cwd, h.title, h.titleAI
		next.HeadFull, next.HeadSize, next.Hidden = h.full, h.scanned, h.hidden
		if f.root.Agent == "claude" {
			next.SessionID = strings.TrimSuffix(filepath.Base(f.path), ".jsonl")
		} else if next.SessionID == "" {
			if m := codexFileID.FindStringSubmatch(filepath.Base(f.path)); m != nil {
				next.SessionID = m[1]
			}
		}
	}
	next.Size, next.ModNs = f.size, modNs
	ix.mu.Lock()
	cp := next
	ix.files[f.path] = &cp
	ix.dirty = true
	ix.mu.Unlock()
	return true
}

// List — страница бесед: свежие сверху.
func (ix *Index) List(ctx context.Context, roots []Root, open map[string]Open, q Query) (Page, error) {
	ix.mu.Lock()
	ix.loadLocked()
	ix.mu.Unlock()

	files := scanRoots(roots)
	// Разбираем от свежих к старым: не уложились в бюджет — пострадает хвост
	// списка, а не его верх, который человек видит первым.
	sort.Slice(files, func(i, j int) bool { return files[i].mod.After(files[j].mod) })
	budget, cancel := context.WithTimeout(ctx, listBudget)
	defer cancel()
	partial := false
	present := make(map[string]bool, len(files))
	for _, f := range files {
		present[f.path] = true
		if !ix.refresh(budget, f) {
			partial = true
		}
	}

	running := ix.runningClaude(roots)

	ix.mu.Lock()
	for p := range ix.files {
		if !present[p] {
			delete(ix.files, p) // файл удалён или аккаунт убран
			ix.dirty = true
		}
	}
	all := make([]Item, 0, len(ix.files))
	agents := map[string]int{}
	words := strings.Fields(strings.ToLower(q.Q))
	pathOf := map[string]string{}
	for _, f := range files {
		e := ix.files[f.path]
		if e == nil || e.Hidden || !ValidSessionID(e.SessionID) || e.ModNs != f.mod.UnixNano() {
			continue // скрыт или не успели разобрать
		}
		if !matches(e, words) {
			continue
		}
		agents[e.Agent]++
		if q.Agent != "" && q.Agent != e.Agent {
			continue
		}
		it := Item{
			Agent: e.Agent, SessionID: e.SessionID, AccountID: e.Account, AccountLabel: e.Label,
			CWD: e.CWD, Title: e.Title, UpdatedAt: e.ModNs / int64(time.Millisecond),
		}
		if e.CountOff > 0 && e.CountOff >= e.Size-maxLine {
			n := e.Messages
			it.Messages = &n
		}
		if o, ok := open[Key(e.Agent, e.SessionID)]; ok {
			it.OpenPtyID, it.Sleeping = o.PtyID, o.Sleeping
		} else if e.Agent == "claude" && running[e.SessionID] {
			it.Running = true
		}
		all = append(all, it)
		pathOf[Key(e.Agent, e.SessionID)] = f.path
	}
	ix.mu.Unlock()

	sort.Slice(all, func(i, j int) bool { return less(all[i], all[j]) })
	start := 0
	if q.Cursor != "" {
		if c, ok := parseCursor(q.Cursor); ok {
			start = sort.Search(len(all), func(i int) bool { return less(c, all[i]) })
		}
	}
	limit := q.Limit
	if limit <= 0 {
		limit = DefaultLimit
	}
	if limit > MaxLimit {
		limit = MaxLimit
	}
	end := start + limit
	if end > len(all) {
		end = len(all)
	}
	// Сообщения считаем только у бесед, которые человек сейчас видит. Полный
	// проход по всем файлам — это ~13 ГБ чтения на ПК владельца (замер
	// 29.09.2026) после каждого запуска Remotai ради одной цифры в строке.
	wanted := make(map[string]bool, end-start)
	for _, it := range all[start:end] {
		if p := pathOf[Key(it.Agent, it.SessionID)]; p != "" {
			wanted[p] = true
		}
	}
	ix.mu.Lock()
	ix.wanted = wanted
	counting := ix.counting || ix.pendingCountLocked()
	ix.mu.Unlock()
	page := Page{Sessions: all[start:end], Total: len(all), Agents: agents, Partial: partial, Counting: counting}
	if page.Sessions == nil {
		page.Sessions = []Item{}
	}
	if end < len(all) {
		page.Next = cursorOf(all[end-1])
	}
	ix.startWorker()
	select {
	case ix.kick <- struct{}{}:
	default:
	}
	if err := ix.Save(); err != nil {
		log.Printf("[SESSIONS] кэш бесед не сохранён: %v", err)
	}
	return page, nil
}

func matches(e *entry, words []string) bool {
	if len(words) == 0 {
		return true
	}
	hay := strings.ToLower(e.Title + "\n" + e.CWD + "\n" + e.SessionID + "\n" + e.Label)
	for _, w := range words {
		if !strings.Contains(hay, w) {
			return false
		}
	}
	return true
}

// less — порядок списка: свежие сверху, при равенстве — по агенту и номеру.
func less(a, b Item) bool {
	if a.UpdatedAt != b.UpdatedAt {
		return a.UpdatedAt > b.UpdatedAt
	}
	if a.Agent != b.Agent {
		return a.Agent < b.Agent
	}
	return a.SessionID < b.SessionID
}

func cursorOf(it Item) string {
	return strconv.FormatInt(it.UpdatedAt, 10) + "~" + it.Agent + "~" + it.SessionID
}

func parseCursor(s string) (Item, bool) {
	parts := strings.SplitN(s, "~", 3)
	if len(parts) != 3 {
		return Item{}, false
	}
	ts, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return Item{}, false
	}
	return Item{UpdatedAt: ts, Agent: parts[1], SessionID: parts[2]}, true
}

// runningClaude — беседы Claude, которые держит живой процесс: Claude пишет
// <каталог>/sessions/<pid>.json с номером беседы. Читаем только *.json —
// рядом лежат ключи (*.key), их не открываем.
func (ix *Index) runningClaude(roots []Root) map[string]bool {
	out := map[string]bool{}
	if ix.PIDAlive == nil {
		return out
	}
	for _, root := range roots {
		if root.Agent != "claude" || root.Dir == "" {
			continue
		}
		dir := filepath.Join(root.Dir, "sessions")
		list, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, f := range list {
			name := f.Name()
			if f.IsDir() || !strings.HasSuffix(name, ".json") {
				continue
			}
			pid, err := strconv.Atoi(strings.TrimSuffix(name, ".json"))
			if err != nil || pid <= 0 {
				continue
			}
			data, err := os.ReadFile(filepath.Join(dir, name))
			if err != nil || len(data) > 64<<10 {
				continue
			}
			var st struct {
				SessionID string `json:"sessionId"`
			}
			if json.Unmarshal(data, &st) != nil || st.SessionID == "" {
				continue
			}
			if ix.PIDAlive(pid) {
				out[st.SessionID] = true
			}
		}
	}
	return out
}

// ── Фоновый подсчёт сообщений ─────────────────────────────────────────────

func (ix *Index) pendingCountLocked() bool {
	for p, e := range ix.files {
		if ix.wanted != nil && !ix.wanted[p] {
			continue
		}
		if !e.Hidden && e.CountOff < e.Size-maxLine {
			return true
		}
	}
	return false
}

func (ix *Index) startWorker() {
	if ix.NoBackground {
		return
	}
	ix.workerOnce.Do(func() {
		go func() {
			for {
				select {
				case <-ix.kick:
				case <-time.After(5 * time.Minute):
				}
				if err := ix.CountPass(context.Background()); err != nil {
					log.Printf("[SESSIONS] подсчёт сообщений: %v", err)
				}
			}
		}()
	})
}

// CountPass дочитывает дописанное во всех известных файлах и считает
// сообщения. Свежие файлы первыми: их человек видит вверху списка.
func (ix *Index) CountPass(ctx context.Context) error {
	ix.countMu.Lock()
	defer ix.countMu.Unlock()
	type job struct {
		path  string
		agent string
		from  int64
		size  int64
		mod   int64
	}
	ix.mu.Lock()
	var jobs []job
	for p, e := range ix.files {
		if e.Hidden || e.CountOff >= e.Size {
			continue
		}
		if ix.wanted != nil && !ix.wanted[p] {
			continue
		}
		jobs = append(jobs, job{p, e.Agent, e.CountOff, e.Size, e.ModNs})
	}
	ix.counting = len(jobs) > 0
	ix.mu.Unlock()
	defer func() {
		ix.mu.Lock()
		ix.counting = false
		ix.mu.Unlock()
	}()
	sort.Slice(jobs, func(i, j int) bool { return jobs[i].mod > jobs[j].mod })
	lastSave := time.Now()
	for _, j := range jobs {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		n, off, err := countMessages(ctx, j.path, j.agent, j.from, j.size, ix.CountPause)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			continue
		}
		ix.mu.Lock()
		if e := ix.files[j.path]; e != nil && e.CountOff == j.from && e.Agent == j.agent {
			e.CountOff = off
			e.Messages += n
			ix.dirty = true
		}
		ix.mu.Unlock()
		if time.Since(lastSave) > 30*time.Second {
			_ = ix.Save()
			lastSave = time.Now()
		}
	}
	return ix.Save()
}
