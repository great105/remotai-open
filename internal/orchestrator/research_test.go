package orchestrator

import (
	"context"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// ── Metric extraction / invariants / protected paths ──────────────────

func TestExtractMetric(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	t.Run("regex from stdout", func(t *testing.T) {
		cfg := ResearchConfig{EvalCommand: "echo score=42.5", MetricPattern: `score=([\d.]+)`}
		re, err := compileMetricRe(cfg)
		if err != nil {
			t.Fatal(err)
		}
		out, errMsg := extractMetric(ctx, cfg, re, dir)
		if errMsg != "" {
			t.Fatalf("errMsg = %q (output %q)", errMsg, out)
		}
		if out != "42.5" {
			t.Errorf("metric = %q, want 42.5", out)
		}
	})

	t.Run("metric file", func(t *testing.T) {
		if err := os.WriteFile(filepath.Join(dir, "metric.txt"), []byte("7.25\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		cfg := ResearchConfig{EvalCommand: "echo whatever", MetricFile: "metric.txt"}
		out, errMsg := extractMetric(ctx, cfg, nil, dir)
		if errMsg != "" || out != "7.25" {
			t.Errorf("out=%q err=%q", out, errMsg)
		}
	})

	t.Run("failing eval command", func(t *testing.T) {
		cfg := ResearchConfig{EvalCommand: "exit 3", MetricPattern: `([\d.]+)`}
		re, _ := compileMetricRe(cfg)
		if _, errMsg := extractMetric(ctx, cfg, re, dir); errMsg == "" {
			t.Error("non-zero eval exit must be an error")
		}
	})

	t.Run("pattern not found", func(t *testing.T) {
		cfg := ResearchConfig{EvalCommand: "echo no numbers here", MetricPattern: `score=([\d.]+)`}
		re, _ := compileMetricRe(cfg)
		if _, errMsg := extractMetric(ctx, cfg, re, dir); errMsg == "" {
			t.Error("missing metric in output must be an error")
		}
	})

	t.Run("unparsable metric", func(t *testing.T) {
		if err := os.WriteFile(filepath.Join(dir, "bad.txt"), []byte("abc"), 0o644); err != nil {
			t.Fatal(err)
		}
		cfg := ResearchConfig{EvalCommand: "echo x", MetricFile: "bad.txt"}
		if _, errMsg := extractMetric(ctx, cfg, nil, dir); errMsg == "" {
			t.Error("non-numeric metric file content must be an error")
		}
	})
}

// compileMetricRe mirrors the regex setup ExecuteResearch does.
func compileMetricRe(cfg ResearchConfig) (*regexp.Regexp, error) {
	if cfg.MetricPattern == "" {
		return nil, nil
	}
	return regexp.Compile(cfg.MetricPattern)
}

func TestRunInvariantsCheck(t *testing.T) {
	dir := t.TempDir()
	if _, errMsg := runInvariantsCheck(context.Background(), "echo ok", dir); errMsg != "" {
		t.Errorf("passing check: %q", errMsg)
	}
	if _, errMsg := runInvariantsCheck(context.Background(), "exit 1", dir); errMsg == "" {
		t.Error("failing check must return errMsg")
	}
}

func TestIsProtected(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		name     string
		path     string
		patterns []string
		want     bool
	}{
		{"no patterns protects nothing", "src/main.go", nil, false},
		{"exact file", "secrets.env", []string{"secrets.env"}, true},
		{"glob match", "config/prod.yaml", []string{"config/*.yaml"}, true},
		{"glob miss", "config/dev.json", []string{"config/*.yaml"}, false},
		{"directory prefix", "vendor/lib/x.go", []string{"vendor"}, true},
		{"dir itself protected", "vendor", []string{"vendor"}, true},
		{"sibling prefix is not inside", "vendor2/lib/x.go", []string{"vendor"}, false},
		{"dotdot sibling stays outside", "../vendor2/x.go", []string{"vendor"}, false},
		{"unrelated path", "src/main.go", []string{"vendor"}, false},
		{"absolute path inside cwd", filepath.Join(dir, "a.go"), []string{"a.go"}, true},
	}
	for _, c := range cases {
		if got := isProtected(c.path, dir, c.patterns); got != c.want {
			t.Errorf("%s: isProtected(%q) = %v, want %v", c.name, c.path, got, c.want)
		}
	}
}

// ── Summary / brief helpers ───────────────────────────────────────────

func TestFillResearchSummary(t *testing.T) {
	r := &ResearchResult{}
	fillResearchSummary(r, 100)
	if r.FinalMetric != 0 || r.Improvement != "" {
		t.Errorf("no experiments must stay empty: %+v", r)
	}

	r = &ResearchResult{Experiments: []Experiment{
		{Metric: 90, Kept: true},
		{Metric: 50, Kept: false}, // reverted — must not count
		{Metric: 80, Kept: true},
	}}
	fillResearchSummary(r, 100)
	if r.FinalMetric != 80 {
		t.Errorf("FinalMetric = %v, want 80 (last KEPT experiment)", r.FinalMetric)
	}
	if !strings.Contains(r.Improvement, "-20.0%") {
		t.Errorf("Improvement = %q, want -20.0%% vs baseline", r.Improvement)
	}
}

func TestBriefResearchTool(t *testing.T) {
	if got := briefResearchTool("git_revert", nil); got != "git_revert" {
		t.Errorf("git_revert brief = %q", got)
	}
	if got := briefResearchTool("experiment_done", map[string]any{"description": "tune x"}); got != "experiment_done: tune x" {
		t.Errorf("experiment_done brief = %q", got)
	}
	// Falls through to the generic brief for standard tools.
	if got := briefResearchTool("read_file", map[string]any{"path": "/x"}); got != "read_file: /x" {
		t.Errorf("read_file brief = %q", got)
	}
}

// ── recordExperiment: metric строкой ──────────────────────────────────

// Модель нередко шлёт metric строкой ("0.95") вместо number: раньше type
// assertion молча давал 0 и портил дельту/FinalMetric эксперимента.
func TestRecordExperimentMetricString(t *testing.T) {
	dir := t.TempDir()
	o := newTestOrchestrator(&queueTransport{}, nil)
	result := &ResearchResult{Experiments: []Experiment{}}
	expCount := 0
	logPath := filepath.Join(dir, ".research-log.jsonl")

	cases := []struct {
		name  string
		input any
		want  float64
	}{
		{"number", 0.95, 0.95},
		{"строка", "0.95", 0.95},
		{"строка с пробелами", " 1.25 ", 1.25},
		{"мусор строкой — 0, но без паники", "abc", 0},
		{"отсутствует", nil, 0},
	}
	for _, c := range cases {
		input := map[string]any{"description": c.name, "kept": true}
		if c.input != nil {
			input["metric"] = c.input
		}
		_, errMsg := o.recordExperiment(context.Background(), input, 0.9,
			ResearchConfig{}, true, &expCount, result, nil, dir, logPath)
		if errMsg != "" {
			t.Errorf("%s: errMsg = %q", c.name, errMsg)
		}
		got := result.Experiments[len(result.Experiments)-1].Metric
		if got != c.want {
			t.Errorf("%s: metric = %v, want %v", c.name, got, c.want)
		}
	}
	// kept=true с метрикой строкой: FinalMetric обязан увидеть реальное число.
	if result.Experiments[1].Metric != 0.95 {
		t.Errorf("string metric experiment = %v, want 0.95", result.Experiments[1].Metric)
	}
	// Дельта первого эксперимента: (0.95-0.9)/0.9 = +5.6%.
	if d := result.Experiments[0].Delta; d != "+5.6%" {
		t.Errorf("delta = %q, want +5.6%%", d)
	}
}

// ── Guard rails that need git ─────────────────────────────────────────

func initGitRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	run("init", "-b", "main")
	run("config", "user.email", "test@example.com")
	run("config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("one"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "-A")
	run("commit", "-m", "init")
	return dir
}

func TestExecuteResearchInvalidRegex(t *testing.T) {
	o := newTestOrchestrator(&queueTransport{}, nil)
	res := o.ExecuteResearch(context.Background(), "task", t.TempDir(),
		ResearchConfig{EvalCommand: "echo 1", MetricPattern: `[invalid`}, nil, nil, nil)
	if !res.IsError || !strings.Contains(res.Summary, "Invalid metric_pattern") {
		t.Fatalf("result: %+v", res)
	}
}

func TestExecuteResearchRequiresGitRepo(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	o := newTestOrchestrator(&queueTransport{}, nil)
	res := o.ExecuteResearch(context.Background(), "task", t.TempDir(),
		ResearchConfig{EvalCommand: "echo 1"}, nil, nil, nil)
	if !res.IsError || !strings.Contains(res.Summary, "needs a git repo") {
		t.Fatalf("result: %+v", res)
	}
}

func TestExecuteResearchRefusesDirtyTree(t *testing.T) {
	dir := initGitRepo(t)
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("dirty"), 0o644); err != nil {
		t.Fatal(err)
	}
	o := newTestOrchestrator(&queueTransport{}, nil)
	res := o.ExecuteResearch(context.Background(), "task", dir,
		ResearchConfig{EvalCommand: "echo 1"}, nil, nil, nil)
	if !res.IsError || !strings.Contains(res.Summary, "незакоммиченные изменения") {
		t.Fatalf("result: %+v", res)
	}
}

// TestExecuteResearchFullFlow drives the whole loop against a stubbed LLM:
// eval baseline → checkpoint → experiment_done → finish, then verifies the
// experiment was recorded and the original branch was restored.
func TestExecuteResearchFullFlow(t *testing.T) {
	dir := initGitRepo(t)

	qt := &queueTransport{responses: []*http.Response{
		apiResponse("", toolCallJSON("c1", "eval_metric", `{}`)),
		apiResponse("", toolCallJSON("c2", "git_checkpoint", `{"message":"baseline"}`)),
		apiResponse("", toolCallJSON("c3", "experiment_done", `{"description":"first try","metric":1.5,"kept":true}`)),
		apiResponse("", toolCallJSON("c4", "finish", `{"summary":"research complete"}`)),
	}}
	o := newTestOrchestrator(qt, nil)

	var experiments []Experiment
	res := o.ExecuteResearch(context.Background(), "optimize", dir,
		ResearchConfig{EvalCommand: "echo 1.5", MetricName: "score"},
		nil, func(e Experiment) { experiments = append(experiments, e) }, nil)

	if res.IsError {
		t.Fatalf("result: %+v", res)
	}
	if res.Summary != "research complete" {
		t.Errorf("Summary = %q", res.Summary)
	}
	if !strings.HasPrefix(res.Branch, "orch-research-") {
		t.Errorf("Branch = %q, want orch-research-*", res.Branch)
	}
	if res.BaselineMetric != 1.5 {
		t.Errorf("BaselineMetric = %v, want 1.5", res.BaselineMetric)
	}
	if len(experiments) != 1 || !experiments[0].Kept || experiments[0].Metric != 1.5 {
		t.Fatalf("experiments: %+v", experiments)
	}
	if res.FinalMetric != 1.5 {
		t.Errorf("FinalMetric = %v, want 1.5", res.FinalMetric)
	}

	// Cleanup: back on the original branch.
	out, err := exec.Command("git", "-C", dir, "rev-parse", "--abbrev-ref", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(out)); got != "main" {
		t.Errorf("after cleanup branch = %q, want main", got)
	}

	// JSONL experiment log written into the repo dir.
	if _, err := os.Stat(filepath.Join(dir, ".research-log.jsonl")); err != nil {
		t.Error(".research-log.jsonl should exist after a run")
	}
}

func TestLockResearchCwdSerializes(t *testing.T) {
	dir := t.TempDir()
	unlock := lockResearchCwd(dir)

	acquired := make(chan struct{})
	go func() {
		u2 := lockResearchCwd(dir)
		close(acquired)
		u2()
	}()

	select {
	case <-acquired:
		t.Fatal("second lock on the same cwd must block while the first is held")
	case <-time.After(200 * time.Millisecond):
	}

	unlock()
	select {
	case <-acquired:
	case <-time.After(2 * time.Second):
		t.Fatal("second lock must proceed after the first is released")
	}
}
