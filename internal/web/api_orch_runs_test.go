package web

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"tgcontrol/internal/orchestrator"
)

// setupOrchRunsRoot подменяет корень артефактов оркестратора на TempDir.
func setupOrchRunsRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	orchestrator.SetRunsRoot(root)
	t.Cleanup(func() { orchestrator.SetRunsRoot("") })
	return root
}

// writeFakeRun создаёт папку запуска руками — хендлеры проверяются как чистая
// проекция файлов, без участия писателя артефактов.
func writeFakeRun(t *testing.T, root, id string, events []string, summaryMD, patch string) {
	t.Helper()
	dir := filepath.Join(root, id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	content := strings.Join(events, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(dir, "events.jsonl"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if summaryMD != "" {
		if err := os.WriteFile(filepath.Join(dir, "summary.md"), []byte(summaryMD), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if patch != "" {
		if err := os.WriteFile(filepath.Join(dir, "patch.diff"), []byte(patch), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestOrchRunsList(t *testing.T) {
	root := setupOrchRunsRoot(t)
	s := &Server{}

	// Пустой корень — пустой список.
	req := httptest.NewRequest("GET", "/api/orch/runs", nil)
	rec := httptest.NewRecorder()
	s.apiOrchRunsList(rec, req, 1)
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var empty struct {
		Runs []any `json:"runs"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &empty); err != nil {
		t.Fatal(err)
	}
	if len(empty.Runs) != 0 {
		t.Fatalf("runs = %v, want empty", empty.Runs)
	}

	writeFakeRun(t, root, "100", []string{
		`{"time":"2026-07-28 10:00:00.000","run_id":"100","event":"run_start","model":"sonnet"}`,
		`{"time":"2026-07-28 10:00:01.000","run_id":"100","event":"task","input":"почини баг","summary":"/repo"}`,
		`{"time":"2026-07-28 10:01:00.000","run_id":"100","event":"run_end","summary":"готово","cost_usd":0.12}`,
	}, "# Run 100", "diff --git a/f.go b/f.go\n")
	writeFakeRun(t, root, "200", []string{
		`{"time":"2026-07-28 11:00:00.000","run_id":"200","event":"run_start","model":"opus"}`,
		`{"time":"2026-07-28 11:00:01.000","run_id":"200","event":"task","input":"обнови deps","summary":"/repo2"}`,
		`{"time":"2026-07-28 11:05:00.000","run_id":"200","event":"run_end","summary":"упало","cost_usd":1.5,"error":"true"}`,
	}, "# Run 200", "")

	rec = httptest.NewRecorder()
	s.apiOrchRunsList(rec, req, 1)
	var resp struct {
		Runs []struct {
			RunID     string  `json:"run_id"`
			Model     string  `json:"model"`
			Task      string  `json:"task"`
			Cwd       string  `json:"cwd"`
			StartedAt string  `json:"started_at"`
			Summary   string  `json:"summary"`
			CostUSD   float64 `json:"cost_usd"`
			IsError   bool    `json:"is_error"`
			Finished  bool    `json:"finished"`
			HasPatch  bool    `json:"has_patch"`
		} `json:"runs"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Runs) != 2 {
		t.Fatalf("runs = %d, want 2", len(resp.Runs))
	}
	// Новые первыми.
	newer, older := resp.Runs[0], resp.Runs[1]
	if newer.RunID != "200" || older.RunID != "100" {
		t.Fatalf("порядок: %s, %s", newer.RunID, older.RunID)
	}
	if newer.Model != "opus" || newer.Task != "обнови deps" || newer.Cwd != "/repo2" {
		t.Errorf("проекция task/model/cwd: %+v", newer)
	}
	if newer.Summary != "упало" || newer.CostUSD != 1.5 || !newer.IsError || !newer.Finished {
		t.Errorf("проекция run_end: %+v", newer)
	}
	if newer.HasPatch {
		t.Error("run 200 без patch.diff: HasPatch должен быть false")
	}
	if !older.HasPatch || older.StartedAt != "2026-07-28 10:00:00.000" {
		t.Errorf("run 100: %+v", older)
	}
}

func TestOrchRunDetail(t *testing.T) {
	root := setupOrchRunsRoot(t)
	s := &Server{}

	writeFakeRun(t, root, "300", []string{
		`{"run_id":"300","event":"run_start","model":"sonnet"}`,
		`{"run_id":"300","event":"thinking","output":"д1"}`,
		`{"run_id":"300","event":"thinking","output":"д2"}`,
		`{"run_id":"300","event":"run_end","summary":"фин"}`,
	}, "# Итог\n\nвсё хорошо", "diff --git a/x b/x\n+line\n")

	get := func(url string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", url, nil)
		req.SetPathValue("id", "300")
		rec := httptest.NewRecorder()
		s.apiOrchRunDetail(rec, req, 1)
		if rec.Code != 200 {
			t.Fatalf("%s: status %d: %s", url, rec.Code, rec.Body.String())
		}
		return rec
	}

	var detail struct {
		RunID       string `json:"run_id"`
		TotalEvents int    `json:"total_events"`
		Offset      int    `json:"offset"`
		Limit       int    `json:"limit"`
		Events      []struct {
			Event  string `json:"event"`
			Output string `json:"output"`
		} `json:"events"`
		SummaryMD string `json:"summary_md"`
		Patch     string `json:"patch"`
	}

	if err := json.Unmarshal(get("/api/orch/runs/300").Body.Bytes(), &detail); err != nil {
		t.Fatal(err)
	}
	if detail.RunID != "300" || detail.TotalEvents != 4 || len(detail.Events) != 4 {
		t.Errorf("events: total=%d len=%d", detail.TotalEvents, len(detail.Events))
	}
	if !strings.Contains(detail.SummaryMD, "всё хорошо") {
		t.Errorf("summary_md: %q", detail.SummaryMD)
	}
	if !strings.Contains(detail.Patch, "+line") {
		t.Errorf("patch: %q", detail.Patch)
	}

	// Пагинация: limit=2.
	if err := json.Unmarshal(get("/api/orch/runs/300?limit=2").Body.Bytes(), &detail); err != nil {
		t.Fatal(err)
	}
	if len(detail.Events) != 2 || detail.TotalEvents != 4 || detail.Limit != 2 {
		t.Errorf("limit=2: len=%d total=%d limit=%d", len(detail.Events), detail.TotalEvents, detail.Limit)
	}
	// offset=3 — последнее событие.
	if err := json.Unmarshal(get("/api/orch/runs/300?offset=3&limit=10").Body.Bytes(), &detail); err != nil {
		t.Fatal(err)
	}
	if len(detail.Events) != 1 || detail.Events[0].Event != "run_end" {
		t.Errorf("offset=3: %+v", detail.Events)
	}
}

func TestOrchRunDetailErrors(t *testing.T) {
	setupOrchRunsRoot(t)
	s := &Server{}

	call := func(id string) int {
		req := httptest.NewRequest("GET", "/api/orch/runs/"+id, nil)
		req.SetPathValue("id", id)
		rec := httptest.NewRecorder()
		s.apiOrchRunDetail(rec, req, 1)
		return rec.Code
	}

	if code := call("../evil"); code != 400 {
		t.Errorf("traversal id: status %d, want 400", code)
	}
	if code := call("no-such-run"); code != 404 {
		t.Errorf("missing run: status %d, want 404", code)
	}
}
