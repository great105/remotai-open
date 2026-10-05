// Package tokenusage считает, сколько токенов сожгли ИИ-агенты на этой машине:
// по дням, аккаунтам, папкам проектов, моделям и сессиям.
//
// Источник — файлы сессий, которые агенты и так пишут на диск: Claude Code
// (`<каталог аккаунта>/projects/**/*.jsonl`) и Codex
// (`<каталог аккаунта>/sessions/**/*.jsonl`). Ни сети, ни ключей: читаем только
// цифры, текст разговоров никуда не уходит и в индекс не попадает.
//
// Файлов много и они большие (на ПК владельца 24.09.2026 — 2 458 файлов,
// около 13 ГБ), поэтому разбор инкрементальный: для каждого файла помним,
// до какого байта дочитали, и в следующий проход читаем только дописанное.
// Итоги лежат в `token-usage.json` рядом с журналом агента.
package tokenusage

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Source — один аккаунт агента на машине. Dir — каталог его кредов и сессий;
// пусто = основной (`~/.claude`, `~/.codex`).
type Source struct {
	AccountID string
	Provider  string // "claude" | "codex"
	Label     string
	Dir       string
}

var (
	sourcesMu sync.Mutex
	sourcesFn func() []Source
)

// SetSources подключает список аккаунтов машины (как aiusage.SetAccountsSource).
func SetSources(fn func() []Source) {
	sourcesMu.Lock()
	sourcesFn = fn
	sourcesMu.Unlock()
}

func sources() []Source {
	sourcesMu.Lock()
	fn := sourcesFn
	sourcesMu.Unlock()
	if fn == nil {
		return []Source{{AccountID: "default", Provider: "claude"}, {AccountID: "default", Provider: "codex"}}
	}
	return fn()
}

// Tokens — счётчики одной строки итогов.
//
// Input — свежий ввод без кеша; CacheRead — ввод, прочитанный из кеша
// (у Claude это основная масса); CacheWrite — запись в кеш; Output — вывод,
// включая рассуждения; Reasoning — из них рассуждения (справочно).
type Tokens struct {
	Input      int64 `json:"input"`
	Output     int64 `json:"output"`
	CacheRead  int64 `json:"cache_read"`
	CacheWrite int64 `json:"cache_write"`
	Reasoning  int64 `json:"reasoning"`
	Calls      int64 `json:"calls"`
}

// Total — всё, за что платят: ввод, кеш и вывод.
func (t Tokens) Total() int64 { return t.Input + t.Output + t.CacheRead + t.CacheWrite }

func (t *Tokens) add(o Tokens) {
	t.Input += o.Input
	t.Output += o.Output
	t.CacheRead += o.CacheRead
	t.CacheWrite += o.CacheWrite
	t.Reasoning += o.Reasoning
	t.Calls += o.Calls
}

// Row — итог по одному сочетанию «день × аккаунт × папка × модель × сессия».
type Row struct {
	Day      string `json:"d"` // локальная дата YYYY-MM-DD
	Provider string `json:"p"`
	Account  string `json:"a"`
	Project  string `json:"w"` // рабочая папка (cwd) сессии
	Model    string `json:"m"`
	Session  string `json:"s"`
	Last     int64  `json:"l"` // unix-время последнего вызова
	Tokens   Tokens `json:"t"`
}

func rowKey(r Row) string {
	return strings.Join([]string{r.Day, r.Provider, r.Account, r.Project, r.Model, r.Session}, "\x1f")
}

// fileState — где остановились в одном файле сессии.
type fileState struct {
	Size    int64  `json:"size"`
	ModTime int64  `json:"mtime"`
	Offset  int64  `json:"off"`
	Session string `json:"sess,omitempty"`
	Project string `json:"cwd,omitempty"`
	Model   string `json:"model,omitempty"`
	// Claude пишет один ответ несколькими строками с одинаковым usage — помним
	// последние ключи message.id+requestId, чтобы не посчитать его дважды.
	Seen []string `json:"seen,omitempty"`
	// Codex отдаёт НАКОПЛЕННЫЙ итог сессии; расход хода — разница с прошлым.
	CodexTotal Tokens `json:"ctot,omitempty"`
}

// index — всё, что переживает перезапуск агента.
type index struct {
	Version int                   `json:"v"`
	Files   map[string]*fileState `json:"files"`
	Rows    map[string]*Row       `json:"rows"`
}

const indexVersion = 1

// RetentionDays — сколько дней истории держим; первый проход не смотрит
// файлы старше этого срока.
const RetentionDays = 35

func newIndex() *index {
	return &index{Version: indexVersion, Files: map[string]*fileState{}, Rows: map[string]*Row{}}
}

func (ix *index) addRow(r Row) {
	if r.Tokens.Total() == 0 && r.Tokens.Calls == 0 {
		return
	}
	k := rowKey(r)
	cur, ok := ix.Rows[k]
	if !ok {
		cp := r
		ix.Rows[k] = &cp
		return
	}
	cur.Tokens.add(r.Tokens)
	if r.Last > cur.Last {
		cur.Last = r.Last
	}
}

// prune выбрасывает строки старше срока хранения.
func (ix *index) prune(now time.Time) {
	cutoff := now.AddDate(0, 0, -RetentionDays).Format("2006-01-02")
	for k, r := range ix.Rows {
		if r.Day < cutoff {
			delete(ix.Rows, k)
		}
	}
}

func loadIndex(path string) *index {
	b, err := os.ReadFile(path)
	if err != nil {
		return newIndex()
	}
	var ix index
	if json.Unmarshal(b, &ix) != nil || ix.Version != indexVersion || ix.Files == nil || ix.Rows == nil {
		return newIndex()
	}
	return &ix
}

func saveIndex(path string, ix *index) error {
	b, err := json.Marshal(ix)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// dayOf переводит метку времени строки в локальную дату и unix-время.
func dayOf(ts string, fallback time.Time) (string, int64) {
	t, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		t = fallback
	}
	t = t.Local()
	return t.Format("2006-01-02"), t.Unix()
}

// sessionFiles перечисляет .jsonl под root, изменённые не раньше cutoff.
func sessionFiles(root string, cutoff time.Time) []string {
	var out []string
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			if d != nil && d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".jsonl") {
			return nil
		}
		if info, err := d.Info(); err == nil && !info.ModTime().Before(cutoff) {
			out = append(out, p)
		}
		return nil
	})
	sort.Strings(out)
	return out
}
