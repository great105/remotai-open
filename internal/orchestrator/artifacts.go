package orchestrator

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"tgcontrol/internal/paths"
)

// ── Артефакты запуска (source of truth для UI) ────────────────────────
//
// Каждый запуск оркестратора — папка runs/<runID>/:
//
//	events.jsonl — те же события, что пишет RunLogger (run_start, task,
//	               thinking, api_resp, tool_call, tool_result, run_end, error)
//	summary.md   — человекочитаемый итог (задача, модель, стоимость, шаги)
//	patch.diff   — git diff рабочей директории по завершении; создаётся только
//	               если cwd — git-репозиторий и есть изменения
//
// Веб (/api/orch/runs) читает эти файлы сканированием, а не память процесса,
// поэтому история переживает перезапуск агента. Старый orchestrator.log
// продолжает писаться параллельно (обратная совместимость).

// Ограничения на чтение артефактов — защита от гигантских файлов.
const (
	maxEventsFileRead = 16 << 20 // events.jsonl сверх этого — хвост отбрасывается
	maxSummaryRead    = 256 << 10
	maxPatchRead      = 1 << 20
	defaultEventLimit = 500
	maxEventLimit     = 2000
)

// Ошибки чтения артефактов — веб мапит их на HTTP-коды.
var (
	ErrRunNotFound  = errors.New("run not found")
	ErrInvalidRunID = errors.New("invalid run id")
)

var runsRootOverride atomic.Value // string

// RunsRoot возвращает корень папок запусков (по умолчанию paths.Base()/runs).
func RunsRoot() string {
	if v, ok := runsRootOverride.Load().(string); ok && v != "" {
		return v
	}
	return filepath.Join(paths.Base(), "runs")
}

// SetRunsRoot переопределяет корень артефактов (для тестов); "" — сброс на default.
func SetRunsRoot(dir string) {
	runsRootOverride.Store(dir)
}

// newRunID — общий генератор идентификатора запуска (миллисекунды).
func newRunID() string {
	return fmt.Sprintf("%d", time.Now().UnixMilli())
}

// validRunID — runID становится частью пути и URL, поэтому только безопасные символы.
func validRunID(id string) bool {
	if id == "" || len(id) > 64 {
		return false
	}
	for _, c := range id {
		if (c < '0' || c > '9') && (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && c != '-' && c != '_' {
			return false
		}
	}
	return true
}

// ── Писатель ──────────────────────────────────────────────────────────

// ArtifactWriter пишет events.jsonl одного запуска и по завершении — summary.md
// и patch.diff. Все методы безопасны на nil/disabled-писателе: артефакты — это
// наблюдаемость, они не имеют права ронять сам запуск.
type ArtifactWriter struct {
	mu       sync.Mutex
	dir      string
	runID    string
	model    string
	task     string
	cwd      string
	start    time.Time
	events   *os.File
	steps    int // tool_result-события (или явный override через setSteps)
	sawEnd   bool
	patch    string // явный override вместо git diff (research: diff между ветками)
	patchSet bool   // override задан (даже пустой) — git-fallback не нужен
	finished bool
	disabled bool
}

// newArtifactWriter создаёт папку runs/<runID>/ и открывает events.jsonl.
// При любой ошибке файловой системы возвращает disabled-писатель (no-op).
func newArtifactWriter(runID, model, task, cwd string) *ArtifactWriter {
	w := &ArtifactWriter{runID: runID, model: model, task: task, cwd: cwd, start: time.Now()}
	if !validRunID(runID) {
		w.disabled = true
		return w
	}
	w.dir = filepath.Join(RunsRoot(), runID)
	if err := os.MkdirAll(w.dir, 0o755); err != nil {
		log.Printf("[Orchestrator] artifacts: cannot create %s: %v", w.dir, err)
		w.disabled = true
		return w
	}
	f, err := os.OpenFile(filepath.Join(w.dir, "events.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		log.Printf("[Orchestrator] artifacts: cannot open events.jsonl in %s: %v", w.dir, err)
		w.disabled = true
		return w
	}
	w.events = f
	return w
}

// Dir — папка артефактов запуска (для тестов/диагностики).
func (w *ArtifactWriter) Dir() string {
	if w == nil {
		return ""
	}
	return w.dir
}

// LogEvent добавляет событие в events.jsonl. Незаполненные Time/RunID/Model
// достампливаются — так же, как это делает RunLogger.Log.
func (w *ArtifactWriter) LogEvent(entry LogEntry) {
	if w == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.disabled || w.events == nil {
		return
	}
	if entry.Time == "" {
		entry.Time = time.Now().Format("2006-01-02 15:04:05.000")
	}
	if entry.RunID == "" {
		entry.RunID = w.runID
	}
	if entry.Model == "" {
		entry.Model = w.model
	}
	if entry.Event == "tool_result" {
		w.steps++
	}
	if entry.Event == "run_end" {
		w.sawEnd = true
	}
	w.appendEventLocked(entry)
}

func (w *ArtifactWriter) appendEventLocked(entry LogEntry) {
	data, err := json.Marshal(entry)
	if err != nil {
		return
	}
	w.events.Write(data)
	w.events.Write([]byte("\n"))
}

// setPatch задаёт готовый diff вместо авто-снятия git diff (research считает
// diff между ветками до возврата на исходную). Даже пустой override отключает
// git-fallback: research отказался от запуска — чужие незакоммиченные изменения
// в patch.diff попасть не должны.
func (w *ArtifactWriter) setPatch(diff string) {
	if w == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.patch = diff
	w.patchSet = true
}

// setSteps задаёт число шагов явно (research не гоняет события через LogEvent).
func (w *ArtifactWriter) setSteps(n int) {
	if w == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.steps = n
}

// finish закрывает запуск: гарантирует run_end в events.jsonl, пишет summary.md
// и (если есть изменения) patch.diff. Идемпотентен — повторный вызов no-op.
func (w *ArtifactWriter) finish(summary string, cost float64, isError bool) {
	if w == nil {
		return
	}
	w.mu.Lock()
	if w.finished || w.disabled {
		w.mu.Unlock()
		return
	}
	w.finished = true

	if !w.sawEnd && w.events != nil {
		errFlag := ""
		if isError {
			errFlag = "true"
		}
		w.appendEventLocked(LogEntry{
			Event:   "run_end",
			Time:    time.Now().Format("2006-01-02 15:04:05.000"),
			RunID:   w.runID,
			Model:   w.model,
			Summary: truncateLog(summary, 2000),
			CostUSD: cost,
			Error:   errFlag,
		})
	}
	if w.events != nil {
		w.events.Close()
		w.events = nil
	}
	steps := w.steps
	patch := w.patch
	patchSet := w.patchSet
	start := w.start
	dir := w.dir
	w.mu.Unlock()

	writeSummaryMD(dir, summaryMDParams{
		runID: w.runID, model: w.model, task: w.task, cwd: w.cwd,
		start: start, end: time.Now(), steps: steps,
		summary: summary, cost: cost, isError: isError,
	})

	// patch.diff: явный override либо git diff по cwd. Пустой diff / не git —
	// файл не создаём, чтобы «есть patch.diff» честно значило «были изменения».
	if !patchSet && strings.TrimSpace(patch) == "" {
		patch = gitDiffCapture(w.cwd)
	}
	if strings.TrimSpace(patch) != "" {
		if err := os.WriteFile(filepath.Join(dir, "patch.diff"), []byte(patch), 0o644); err != nil {
			log.Printf("[Orchestrator] artifacts: cannot write patch.diff in %s: %v", dir, err)
		}
	}
}

type summaryMDParams struct {
	runID, model, task, cwd string
	start, end              time.Time
	steps                   int
	summary                 string
	cost                    float64
	isError                 bool
}

func writeSummaryMD(dir string, p summaryMDParams) {
	status := "завершён"
	if p.isError {
		status = "ОШИБКА"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# Запуск оркестратора %s\n\n", p.runID)
	fmt.Fprintf(&b, "- Задача: %s\n", p.task)
	fmt.Fprintf(&b, "- Модель: %s\n", p.model)
	if p.cwd != "" {
		fmt.Fprintf(&b, "- Директория: %s\n", p.cwd)
	}
	fmt.Fprintf(&b, "- Начат: %s\n", p.start.Format("2006-01-02 15:04:05"))
	fmt.Fprintf(&b, "- Длительность: %s\n", p.end.Sub(p.start).Truncate(time.Second))
	fmt.Fprintf(&b, "- Статус: %s\n", status)
	fmt.Fprintf(&b, "- Стоимость: $%.4f\n", p.cost)
	fmt.Fprintf(&b, "- Шагов: %d\n\n", p.steps)
	b.WriteString("## Итог\n\n")
	b.WriteString(p.summary)
	b.WriteString("\n")
	if err := os.WriteFile(filepath.Join(dir, "summary.md"), []byte(b.String()), 0o644); err != nil {
		log.Printf("[Orchestrator] artifacts: cannot write summary.md in %s: %v", dir, err)
	}
}

// gitDiffCapture снимает `git diff HEAD` по cwd (tracked-изменения, staged и
// unstaged). Не git-репозиторий, нет коммитов, таймаут — возвращает "".
// Untracked-файлы в diff не попадают (известное ограничение).
func gitDiffCapture(cwd string) string {
	if cwd == "" {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := gitCmd(ctx, cwd, "diff", "HEAD").Output()
	if err != nil {
		return ""
	}
	return string(out)
}

// ── Чтение (проекция файлов для API) ──────────────────────────────────

// RunInfo — строка списка запусков, проекция папки runs/<runID>/.
type RunInfo struct {
	RunID     string  `json:"run_id"`
	Model     string  `json:"model,omitempty"`
	Task      string  `json:"task,omitempty"`
	Cwd       string  `json:"cwd,omitempty"`
	StartedAt string  `json:"started_at"`
	Summary   string  `json:"summary,omitempty"`
	CostUSD   float64 `json:"cost_usd"`
	IsError   bool    `json:"is_error"`
	Finished  bool    `json:"finished"`
	HasPatch  bool    `json:"has_patch"`
}

// RunDetail — детали запуска: события (с пагинацией) + summary.md + patch.diff.
type RunDetail struct {
	RunInfo
	Events         []LogEntry `json:"events"`
	TotalEvents    int        `json:"total_events"`
	Offset         int        `json:"offset"`
	Limit          int        `json:"limit"`
	SummaryMD      string     `json:"summary_md"`
	Patch          string     `json:"patch,omitempty"`
	PatchTruncated bool       `json:"patch_truncated,omitempty"`
}

// ListRuns сканирует папку runs/ и возвращает проекции, новые первыми.
// Битые/чужие папки пропускаются молча — список не должен падать из-за одной.
func ListRuns(root string) []RunInfo {
	entries, err := os.ReadDir(root)
	if err != nil {
		return []RunInfo{}
	}
	runs := make([]RunInfo, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() || !validRunID(e.Name()) {
			continue
		}
		runs = append(runs, runInfoFromDir(filepath.Join(root, e.Name()), e.Name(), e))
	}
	sort.Slice(runs, func(i, j int) bool { return runs[i].RunID > runs[j].RunID })
	return runs
}

func runInfoFromDir(dir, runID string, e os.DirEntry) RunInfo {
	info := RunInfo{RunID: runID}
	if fi, err := e.Info(); err == nil {
		info.StartedAt = fi.ModTime().Format("2006-01-02 15:04:05")
	}
	if _, err := os.Stat(filepath.Join(dir, "patch.diff")); err == nil {
		info.HasPatch = true
	}
	events, _ := readEventsFile(filepath.Join(dir, "events.jsonl"))
	for _, ev := range events {
		switch ev.Event {
		case "run_start":
			if ev.Time != "" {
				info.StartedAt = ev.Time
			}
			info.Model = ev.Model
		case "task":
			info.Task = ev.Input
			info.Cwd = ev.Summary
		case "run_end":
			info.Summary = ev.Summary
			info.CostUSD = ev.CostUSD
			info.IsError = ev.Error == "true"
			info.Finished = true
		}
	}
	return info
}

// ReadRun читает детали одного запуска. offset/limit — пагинация событий
// (limit <= 0 → 500, максимум 2000).
func ReadRun(root, runID string, offset, limit int) (*RunDetail, error) {
	if !validRunID(runID) {
		return nil, ErrInvalidRunID
	}
	dir := filepath.Join(root, runID)
	fi, err := os.Stat(dir)
	if err != nil || !fi.IsDir() {
		return nil, ErrRunNotFound
	}
	if offset < 0 {
		offset = 0
	}
	if limit <= 0 {
		limit = defaultEventLimit
	}
	if limit > maxEventLimit {
		limit = maxEventLimit
	}

	detail := &RunDetail{Offset: offset, Limit: limit}
	detail.RunInfo = runInfoFromDir(dir, runID, dirEntry{fi})

	events, _ := readEventsFile(filepath.Join(dir, "events.jsonl"))
	detail.TotalEvents = len(events)
	if offset > len(events) {
		offset = len(events)
	}
	end := offset + limit
	if end > len(events) {
		end = len(events)
	}
	detail.Events = events[offset:end]
	if detail.Events == nil {
		detail.Events = []LogEntry{}
	}

	if data, err := readCapped(filepath.Join(dir, "summary.md"), maxSummaryRead); err == nil {
		detail.SummaryMD = string(data)
	}
	if data, truncated, err := readCappedTrunc(filepath.Join(dir, "patch.diff"), maxPatchRead); err == nil {
		detail.Patch = string(data)
		detail.PatchTruncated = truncated
	}
	return detail, nil
}

// dirEntry — минимальный адаптер os.DirEntry для уже известной папки.
type dirEntry struct{ fi os.FileInfo }

func (d dirEntry) Name() string               { return d.fi.Name() }
func (d dirEntry) IsDir() bool                { return true }
func (d dirEntry) Type() os.FileMode          { return os.ModeDir }
func (d dirEntry) Info() (os.FileInfo, error) { return d.fi, nil }

// readEventsFile парсит events.jsonl (кап по размеру). Битые строки пропускаются.
func readEventsFile(path string) ([]LogEntry, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	events := []LogEntry{}
	sc := bufio.NewScanner(io.LimitReader(f, maxEventsFileRead))
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var ev LogEntry
		if json.Unmarshal([]byte(line), &ev) == nil {
			events = append(events, ev)
		}
	}
	return events, nil
}

func readCapped(path string, cap int64) ([]byte, error) {
	data, _, err := readCappedTrunc(path, cap)
	return data, err
}

func readCappedTrunc(path string, cap int64) ([]byte, bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, false, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, cap+1))
	if err != nil {
		return nil, false, err
	}
	if int64(len(data)) > cap {
		return data[:cap], true, nil
	}
	return data, false, nil
}
