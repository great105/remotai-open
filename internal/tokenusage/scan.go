package tokenusage

import (
	"bytes"
	"context"
	"io"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	// scanEvery — как часто дочитываем новое. Экран расхода смотрят не чаще.
	scanEvery = 2 * time.Minute
	// firstScanDelay — первый проход не мешает старту агента.
	firstScanDelay = 30 * time.Second
	// chunk и chunkPause — дочитываем порциями и отдыхаем между ними:
	// первый проход на ПК владельца — гигабайты, и ноутбук не должен
	// захлебнуться диском ради цифр, которые никто не ждёт срочно.
	chunk      = 4 << 20
	chunkPause = 15 * time.Millisecond
)

// Progress — где первый проход (для «считаем…» на экране).
type Progress struct {
	Running   bool  `json:"running"`
	Complete  bool  `json:"complete"` // хотя бы один полный проход был
	DoneBytes int64 `json:"done_bytes"`
	AllBytes  int64 `json:"all_bytes"`
	ScannedAt int64 `json:"scanned_at,omitempty"`
}

// Tracker держит индекс и дочитывает файлы сессий.
type Tracker struct {
	path string
	home string

	mu   sync.Mutex
	ix   *index
	prog Progress

	scanMu sync.Mutex // один проход за раз
}

// NewTracker — индекс в dataDir, домашний каталог для основных аккаунтов.
func NewTracker(dataDir, home string) *Tracker {
	p := filepath.Join(dataDir, "token-usage.json")
	ix := loadIndex(p)
	t := &Tracker{path: p, home: home, ix: ix}
	t.prog.Complete = len(ix.Files) > 0
	return t
}

var (
	defaultMu sync.Mutex
	defTr     *Tracker
)

// Start поднимает общий трекер агента: первый проход через firstScanDelay,
// дальше раз в scanEvery. Повторный вызов ничего не делает.
func Start(ctx context.Context, dataDir, home string) *Tracker {
	defaultMu.Lock()
	defer defaultMu.Unlock()
	if defTr != nil {
		return defTr
	}
	defTr = NewTracker(dataDir, home)
	go defTr.loop(ctx)
	return defTr
}

// Default — трекер, поднятый Start, или nil.
func Default() *Tracker {
	defaultMu.Lock()
	defer defaultMu.Unlock()
	return defTr
}

func (t *Tracker) loop(ctx context.Context) {
	select {
	case <-ctx.Done():
		return
	case <-time.After(firstScanDelay):
	}
	for {
		if err := t.Scan(ctx, chunkPause); err != nil && ctx.Err() == nil {
			log.Printf("[TOKENS] проход не удался: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(scanEvery):
		}
	}
}

type job struct {
	src  Source
	path string
	size int64
	mod  time.Time
}

func (t *Tracker) roots(src Source) (string, string) {
	switch src.Provider {
	case "claude":
		base := src.Dir
		if base == "" {
			base = filepath.Join(t.home, ".claude")
		}
		return filepath.Join(base, "projects"), "claude"
	case "codex":
		base := src.Dir
		if base == "" {
			base = filepath.Join(t.home, ".codex")
		}
		return filepath.Join(base, "sessions"), "codex"
	}
	return "", ""
}

// Scan — один проход: дочитать дописанное во всех файлах сессий. pause —
// отдых между порциями (0 в тестах).
func (t *Tracker) Scan(ctx context.Context, pause time.Duration) error {
	t.scanMu.Lock()
	defer t.scanMu.Unlock()

	now := time.Now()
	cutoff := now.AddDate(0, 0, -RetentionDays)
	var jobs []job
	var all, done int64
	seen := map[string]bool{}

	t.mu.Lock()
	for _, src := range sources() {
		root, kind := t.roots(src)
		if kind == "" {
			continue
		}
		for _, p := range sessionFiles(root, cutoff) {
			if seen[p] {
				continue // один каталог у двух записей аккаунтов
			}
			seen[p] = true
			info, err := os.Stat(p)
			if err != nil {
				continue
			}
			st := t.ix.Files[p]
			off := int64(0)
			if st != nil {
				off = st.Offset
				if info.Size() < off {
					off = 0 // файл переписали
				}
			}
			all += info.Size()
			done += off
			if info.Size() > off {
				jobs = append(jobs, job{src: src, path: p, size: info.Size(), mod: info.ModTime()})
			}
		}
	}
	t.prog.Running = true
	t.prog.AllBytes, t.prog.DoneBytes = all, done
	t.mu.Unlock()

	defer func() {
		t.mu.Lock()
		t.prog.Running = false
		t.mu.Unlock()
	}()

	for _, j := range jobs {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := t.readFile(ctx, j, pause); err != nil {
			log.Printf("[TOKENS] %s: %v", filepath.Base(j.path), err)
		}
	}

	t.mu.Lock()
	t.ix.prune(now)
	for p := range t.ix.Files {
		if !seen[p] {
			delete(t.ix.Files, p) // файл удалён или выпал за срок
		}
	}
	t.prog.Complete = true
	t.prog.ScannedAt = time.Now().Unix()
	t.prog.DoneBytes = t.prog.AllBytes
	err := saveIndex(t.path, t.ix)
	t.mu.Unlock()
	return err
}

func (t *Tracker) readFile(ctx context.Context, j job, pause time.Duration) error {
	f, err := os.Open(j.path)
	if err != nil {
		return err
	}
	defer f.Close()

	t.mu.Lock()
	st := t.ix.Files[j.path]
	if st == nil || j.size < st.Offset {
		st = &fileState{}
		t.ix.Files[j.path] = st
	}
	off := st.Offset
	t.mu.Unlock()

	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return err
	}
	buf := make([]byte, 0, chunk)
	tmp := make([]byte, chunk)
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		n, rerr := f.Read(tmp)
		buf = append(buf, tmp[:n]...)
		// Обрабатываем только полные строки: хвост без перевода строки агент
		// ещё дописывает, его дочитаем в следующий раз.
		if cut := bytes.LastIndexByte(buf, '\n'); cut >= 0 {
			whole := buf[:cut+1]
			t.mu.Lock()
			parse := parseClaude
			if j.src.Provider == "codex" {
				parse = parseCodex
			}
			parse(st, whole, j.src, j.mod, t.ix.addRow)
			st.Offset += int64(len(whole))
			t.prog.DoneBytes += int64(len(whole))
			t.mu.Unlock()
			buf = append(buf[:0], buf[cut+1:]...)
		}
		if rerr == io.EOF || n == 0 {
			break
		}
		if rerr != nil {
			return rerr
		}
		if len(buf) > 64<<20 {
			// Строка длиннее 64 МБ — это не сессия; пропускаем её целиком.
			t.mu.Lock()
			st.Offset += int64(len(buf))
			t.mu.Unlock()
			buf = buf[:0]
		}
		if pause > 0 {
			time.Sleep(pause)
		}
	}
	t.mu.Lock()
	st.Size = j.size
	st.ModTime = j.mod.Unix()
	t.mu.Unlock()
	return nil
}
