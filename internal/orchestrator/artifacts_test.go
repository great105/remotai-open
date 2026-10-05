package orchestrator

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestMain уводит корень артефактов во временную папку: Execute/ExecuteResearch
// в тестах теперь пишут runs/<runID>/, и без этого они мусорили бы в реальный
// профиль (paths.Base()/runs).
func TestMain(m *testing.M) {
	SetRunsRoot(filepath.Join(os.TempDir(), "orchestrator-test-runs"))
	os.Exit(m.Run())
}

// freshRunsRoot подменяет корень артефактов на per-test TempDir.
func freshRunsRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	SetRunsRoot(root)
	t.Cleanup(func() { SetRunsRoot(filepath.Join(os.TempDir(), "orchestrator-test-runs")) })
	return root
}

func readEvents(t *testing.T, dir string) []LogEntry {
	t.Helper()
	events, err := readEventsFile(filepath.Join(dir, "events.jsonl"))
	if err != nil {
		t.Fatalf("readEventsFile: %v", err)
	}
	return events
}

func countEvent(events []LogEntry, name string) int {
	n := 0
	for _, e := range events {
		if e.Event == name {
			n++
		}
	}
	return n
}

// ── Писатель ──────────────────────────────────────────────────────────

func TestArtifactWriterCreatesRunFolder(t *testing.T) {
	root := freshRunsRoot(t)
	cwd := t.TempDir() // не git → patch.diff быть не должно

	w := newArtifactWriter("test-run-1", "model-x", "сделай задачу", cwd)
	w.LogEvent(LogEntry{Event: "run_start"})
	w.LogEvent(LogEntry{Event: "task", Input: "сделай задачу", Summary: cwd})
	w.LogEvent(LogEntry{Event: "tool_call", Tool: "run_command"})
	w.LogEvent(LogEntry{Event: "tool_result", Tool: "run_command", Output: "ok"})
	w.finish("всё готово", 0.42, false)

	dir := filepath.Join(root, "test-run-1")
	events := readEvents(t, dir)
	if len(events) != 5 { // 4 записанных + синтезированный run_end
		t.Fatalf("events = %d, want 5", len(events))
	}
	if countEvent(events, "run_end") != 1 {
		t.Error("finish должен синтезировать run_end")
	}
	last := events[len(events)-1]
	if last.Event != "run_end" || last.Summary != "всё готово" || last.CostUSD != 0.42 || last.Error != "" {
		t.Errorf("run_end: %+v", last)
	}
	// Пустые поля достампливаются из писателя.
	if events[0].RunID != "test-run-1" || events[0].Model != "model-x" || events[0].Time == "" {
		t.Errorf("stamping: %+v", events[0])
	}

	md, err := os.ReadFile(filepath.Join(dir, "summary.md"))
	if err != nil {
		t.Fatalf("summary.md: %v", err)
	}
	for _, want := range []string{"сделай задачу", "model-x", "всё готово", "$0.4200", "Шагов: 1", "завершён"} {
		if !strings.Contains(string(md), want) {
			t.Errorf("summary.md не содержит %q:\n%s", want, md)
		}
	}

	if _, err := os.Stat(filepath.Join(dir, "patch.diff")); !os.IsNotExist(err) {
		t.Error("patch.diff не должен создаваться вне git-репозитория")
	}

	// Повторный finish — no-op (идемпотентность).
	w.finish("другой итог", 9, true)
	if events := readEvents(t, dir); len(events) != 5 {
		t.Errorf("после повторного finish events = %d, want 5", len(events))
	}
}

func TestArtifactWriterRunEndNotDuplicated(t *testing.T) {
	freshRunsRoot(t)
	w := newArtifactWriter("run-dup", "m", "t", t.TempDir())
	w.LogEvent(LogEntry{Event: "run_end", Summary: "финиш", CostUSD: 0.1})
	w.finish("финиш", 0.1, false)

	if events := readEvents(t, w.Dir()); countEvent(events, "run_end") != 1 {
		t.Errorf("run_end продублирован: %+v", events)
	}
}

func TestArtifactWriterErrorRun(t *testing.T) {
	freshRunsRoot(t)
	w := newArtifactWriter("run-err", "m", "t", t.TempDir())
	w.LogEvent(LogEntry{Event: "error", Error: "boom"})
	w.finish("Ошибка API: boom", 0, true)

	md, _ := os.ReadFile(filepath.Join(w.Dir(), "summary.md"))
	if !strings.Contains(string(md), "ОШИБКА") {
		t.Errorf("summary.md должен помечать ошибку:\n%s", md)
	}
	events := readEvents(t, w.Dir())
	last := events[len(events)-1]
	if last.Event != "run_end" || last.Error != "true" {
		t.Errorf("run_end с ошибкой: %+v", last)
	}
}

func TestArtifactNilAndDisabledSafe(t *testing.T) {
	var w *ArtifactWriter
	w.LogEvent(LogEntry{Event: "run_start"}) // не должно паниковать
	w.setPatch("x")
	w.setSteps(1)
	w.finish("s", 0, false)
	if w.Dir() != "" {
		t.Error("Dir nil-писателя должен быть пустым")
	}

	// Невалидный runID → disabled, все операции no-op.
	freshRunsRoot(t)
	d := newArtifactWriter("../evil", "m", "t", t.TempDir())
	d.LogEvent(LogEntry{Event: "run_start"})
	d.finish("s", 0, false)
	if d.Dir() != "" {
		t.Error("disabled-писатель не должен получать папку")
	}
}

// ── patch.diff ────────────────────────────────────────────────────────

func TestArtifactPatchDiffAppearsOnChanges(t *testing.T) {
	repo := initGitRepo(t)

	// Чистое дерево → patch.diff нет.
	freshRunsRoot(t)
	wClean := newArtifactWriter("run-clean", "m", "t", repo)
	wClean.finish("ok", 0, false)
	if _, err := os.Stat(filepath.Join(wClean.Dir(), "patch.diff")); !os.IsNotExist(err) {
		t.Error("чистое дерево: patch.diff быть не должно")
	}

	// Изменили tracked-файл → patch.diff с diff'ом.
	if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("changed"), 0o644); err != nil {
		t.Fatal(err)
	}
	wDirty := newArtifactWriter("run-dirty", "m", "t", repo)
	wDirty.finish("ok", 0, false)
	patch, err := os.ReadFile(filepath.Join(wDirty.Dir(), "patch.diff"))
	if err != nil {
		t.Fatalf("patch.diff: %v", err)
	}
	if !strings.Contains(string(patch), "a.txt") || !strings.Contains(string(patch), "+changed") {
		t.Errorf("patch.diff должен содержать diff по a.txt:\n%s", patch)
	}
}

func TestArtifactSetPatchEmptyDisablesGitFallback(t *testing.T) {
	repo := initGitRepo(t)
	// Дерево грязное, но research отказался от запуска: явный пустой patch
	// обязан отключить git-fallback — чужие изменения не наша заслуга.
	if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("not ours"), 0o644); err != nil {
		t.Fatal(err)
	}
	freshRunsRoot(t)
	w := newArtifactWriter("run-refused", "m", "t", repo)
	w.setPatch("")
	w.finish("refused", 0, true)
	if _, err := os.Stat(filepath.Join(w.Dir(), "patch.diff")); !os.IsNotExist(err) {
		t.Error("пустой явный patch: patch.diff создаваться не должен")
	}
}

// ── Проекция (чтение) ─────────────────────────────────────────────────

func TestListRunsProjection(t *testing.T) {
	root := freshRunsRoot(t)

	for _, id := range []string{"100", "200"} {
		w := newArtifactWriter(id, "model-"+id, "task-"+id, t.TempDir())
		w.LogEvent(LogEntry{Event: "run_start"})
		w.LogEvent(LogEntry{Event: "task", Input: "task-" + id, Summary: "/cwd/" + id})
		w.finish("summary-"+id, 0.5, id == "100")
	}

	runs := ListRuns(root)
	if len(runs) != 2 {
		t.Fatalf("runs = %d, want 2", len(runs))
	}
	// Новые первыми.
	if runs[0].RunID != "200" || runs[1].RunID != "100" {
		t.Errorf("порядок: %s, %s", runs[0].RunID, runs[1].RunID)
	}
	r := runs[0]
	if r.Model != "model-200" || r.Task != "task-200" || r.Cwd != "/cwd/200" {
		t.Errorf("проекция task/model: %+v", r)
	}
	if r.Summary != "summary-200" || r.CostUSD != 0.5 || r.IsError || !r.Finished || r.HasPatch {
		t.Errorf("проекция run_end: %+v", r)
	}
	if !runs[1].IsError {
		t.Errorf("run 100 должен быть с ошибкой: %+v", runs[1])
	}

	// Пустой/несуществующий корень — пустой список, а не nil/паника.
	if got := ListRuns(filepath.Join(root, "no-such")); len(got) != 0 {
		t.Errorf("ListRuns несуществующего корня: %+v", got)
	}
}

func TestReadRunDetail(t *testing.T) {
	root := freshRunsRoot(t)
	w := newArtifactWriter("run-detail", "model-x", "задача", t.TempDir())
	for range 4 {
		w.LogEvent(LogEntry{Event: "thinking", Output: "думаю"})
	}
	w.finish("готово", 0.1, false)
	// summary.md написан finish'ем; patch.diff подложим вручную (как сделал бы git capture).
	if err := os.WriteFile(filepath.Join(w.Dir(), "patch.diff"), []byte("diff --git a/x b/x\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	d, err := ReadRun(root, "run-detail", 0, 0)
	if err != nil {
		t.Fatalf("ReadRun: %v", err)
	}
	if d.TotalEvents != 5 || len(d.Events) != 5 { // 4 thinking + run_end; limit default 500
		t.Errorf("events: total=%d len=%d", d.TotalEvents, len(d.Events))
	}
	if !strings.Contains(d.SummaryMD, "готово") {
		t.Errorf("summary_md: %q", d.SummaryMD)
	}
	if d.Patch != "diff --git a/x b/x\n" || d.PatchTruncated {
		t.Errorf("patch: %q truncated=%v", d.Patch, d.PatchTruncated)
	}
	if !d.HasPatch {
		t.Error("HasPatch должен быть true")
	}

	// Пагинация.
	d, err = ReadRun(root, "run-detail", 3, 10)
	if err != nil {
		t.Fatalf("ReadRun page: %v", err)
	}
	if len(d.Events) != 2 || d.TotalEvents != 5 || d.Offset != 3 || d.Limit != 10 {
		t.Errorf("пагинация: len=%d total=%d offset=%d limit=%d", len(d.Events), d.TotalEvents, d.Offset, d.Limit)
	}
	// Offset за пределами — пустой срез, не паника.
	d, _ = ReadRun(root, "run-detail", 99, 10)
	if len(d.Events) != 0 {
		t.Errorf("offset за пределами: len=%d", len(d.Events))
	}
}

func TestReadRunErrors(t *testing.T) {
	root := freshRunsRoot(t)
	if _, err := ReadRun(root, "../evil", 0, 0); err != ErrInvalidRunID {
		t.Errorf("traversal: %v, want ErrInvalidRunID", err)
	}
	if _, err := ReadRun(root, "no-such-run", 0, 0); err != ErrRunNotFound {
		t.Errorf("missing: %v, want ErrRunNotFound", err)
	}
}

// ── Интеграция с Execute ──────────────────────────────────────────────

func TestExecuteWithStepsWritesArtifacts(t *testing.T) {
	root := freshRunsRoot(t)
	qt := &queueTransport{responses: []*http.Response{
		apiResponse("", toolCallJSON("c1", "finish", `{"summary":"интеграция ок"}`)),
	}}
	o := newTestOrchestrator(qt, nil)

	res := o.ExecuteWithSteps(context.Background(), "тестовая задача", t.TempDir(), nil, nil, nil)
	if res.IsError {
		t.Fatalf("result: %+v", res)
	}

	runs := ListRuns(root)
	if len(runs) != 1 {
		t.Fatalf("runs = %d, want 1", len(runs))
	}
	r := runs[0]
	if r.Summary != "интеграция ок" || !r.Finished || r.IsError {
		t.Errorf("проекция запуска: %+v", r)
	}
	if r.Task != "тестовая задача" || r.Model != "anthropic/claude-sonnet-4.6" {
		t.Errorf("task/model в проекции: %+v", r)
	}

	events := readEvents(t, filepath.Join(root, r.RunID))
	for _, want := range []string{"run_start", "task", "api_resp", "tool_call", "run_end"} {
		if countEvent(events, want) == 0 {
			t.Errorf("в events.jsonl нет события %q: %+v", want, events)
		}
	}
	if _, err := os.Stat(filepath.Join(root, r.RunID, "summary.md")); err != nil {
		t.Error("summary.md должен существовать после Execute")
	}
}

func TestExecuteResearchWritesArtifacts(t *testing.T) {
	root := freshRunsRoot(t)
	dir := initGitRepo(t)

	qt := &queueTransport{responses: []*http.Response{
		apiResponse("", toolCallJSON("c1", "finish", `{"summary":"research ok"}`)),
	}}
	o := newTestOrchestrator(qt, nil)

	res := o.ExecuteResearch(context.Background(), "optimize", dir,
		ResearchConfig{EvalCommand: "echo 1"}, nil, nil, nil)
	if res.IsError {
		t.Fatalf("result: %+v", res)
	}

	runs := ListRuns(root)
	if len(runs) != 1 {
		t.Fatalf("runs = %d, want 1", len(runs))
	}
	if runs[0].Summary != "research ok" || !runs[0].Finished {
		t.Errorf("проекция research-запуска: %+v", runs[0])
	}
	// События писались напрямую (без RunLogger): run_start/task должны быть.
	events := readEvents(t, filepath.Join(root, runs[0].RunID))
	if countEvent(events, "run_start") == 0 || countEvent(events, "task") == 0 || countEvent(events, "run_end") == 0 {
		t.Errorf("events research-запуска: %+v", events)
	}
}

// events.jsonl должен оставаться валидным JSONL построчно (его парсит API).
func TestEventsFileIsValidJSONL(t *testing.T) {
	freshRunsRoot(t)
	w := newArtifactWriter("run-jsonl", "m", "t", t.TempDir())
	w.LogEvent(LogEntry{Event: "thinking", Output: "текст с \"кавычками\" и \n переносом"})
	w.finish("ok", 0, false)

	data, err := os.ReadFile(filepath.Join(w.Dir(), "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	for i, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var ev LogEntry
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Errorf("строка %d невалидна: %v", i, err)
		}
	}
}
