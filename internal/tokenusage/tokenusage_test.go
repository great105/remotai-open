package tokenusage

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func claudeAssistant(ts, id, req, model string, in, out, cr, cw int64) string {
	return `{"type":"assistant","timestamp":"` + ts + `","cwd":"C:\\proj\\a","sessionId":"s1","requestId":"` + req +
		`","message":{"id":"` + id + `","model":"` + model + `","content":[{"type":"text","text":"hi"}],"usage":{"input_tokens":` +
		itoa(in) + `,"cache_creation_input_tokens":` + itoa(cw) + `,"cache_read_input_tokens":` + itoa(cr) +
		`,"output_tokens":` + itoa(out) + `}}}`
}

func itoa(v int64) string {
	b := []byte{}
	if v == 0 {
		return "0"
	}
	for v > 0 {
		b = append([]byte{byte('0' + v%10)}, b...)
		v /= 10
	}
	return string(b)
}

func writeLines(t *testing.T, path string, lines ...string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for _, l := range lines {
		if _, err := f.WriteString(l + "\n"); err != nil {
			t.Fatal(err)
		}
	}
}

func setup(t *testing.T) (*Tracker, string) {
	t.Helper()
	home := t.TempDir()
	data := t.TempDir()
	SetSources(func() []Source {
		return []Source{
			{AccountID: "default", Provider: "claude", Label: "основной"},
			{AccountID: "default", Provider: "codex", Label: "основной"},
		}
	})
	t.Cleanup(func() { SetSources(nil) })
	return NewTracker(data, home), home
}

func now() string { return time.Now().UTC().Format(time.RFC3339Nano) }

// Claude пишет один ответ несколькими строками с одинаковым usage (по блоку
// content на строку) — посчитать его надо один раз. <synthetic> — не вызов.
func TestClaudeDedupAndSynthetic(t *testing.T) {
	tr, home := setup(t)
	p := filepath.Join(home, ".claude", "projects", "C--proj-a", "s1.jsonl")
	ts := now()
	writeLines(t, p,
		`{"type":"user","message":{"content":"hi"}}`,
		claudeAssistant(ts, "m1", "r1", "claude-opus-5-5", 2, 300, 1000, 50),
		claudeAssistant(ts, "m1", "r1", "claude-opus-5-5", 2, 300, 1000, 50),
		claudeAssistant(ts, "m2", "r2", "claude-opus-5-5", 1, 100, 2000, 0),
		claudeAssistant(ts, "m3", "r3", "<synthetic>", 0, 0, 0, 0),
	)
	if err := tr.Scan(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
	rep := tr.Report(1, time.Now())
	want := Tokens{Input: 3, Output: 400, CacheRead: 3000, CacheWrite: 50, Calls: 2}
	if rep.Total.Tokens != want {
		t.Fatalf("total %+v, want %+v", rep.Total.Tokens, want)
	}
	if len(rep.ByProject) != 1 || rep.ByProject[0].Key != `C:\proj\a` || rep.ByProject[0].Sessions != 1 {
		t.Fatalf("by project %+v", rep.ByProject)
	}
	if len(rep.Top) != 1 || rep.Top[0].Model != "claude-opus-5-5" {
		t.Fatalf("top %+v", rep.Top)
	}
}

// Второй проход читает только дописанное, а незаконченную строку не трогает.
func TestIncrementalAndPartialLine(t *testing.T) {
	tr, home := setup(t)
	p := filepath.Join(home, ".claude", "projects", "C--proj-a", "s1.jsonl")
	ts := now()
	writeLines(t, p, claudeAssistant(ts, "m1", "r1", "claude-opus-5-5", 1, 10, 0, 0))
	partial := claudeAssistant(ts, "m2", "r2", "claude-opus-5-5", 1, 20, 0, 0)
	f, _ := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0o644)
	_, _ = f.WriteString(partial[:40])
	f.Close()
	if err := tr.Scan(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
	if got := tr.Report(1, time.Now()).Total.Output; got != 10 {
		t.Fatalf("after first scan output %d, want 10", got)
	}
	f, _ = os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0o644)
	_, _ = f.WriteString(partial[40:] + "\n")
	f.Close()
	if err := tr.Scan(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
	if got := tr.Report(1, time.Now()).Total.Output; got != 30 {
		t.Fatalf("after append output %d, want 30", got)
	}
	// Индекс переживает перезапуск: новый трекер не считает заново.
	tr2 := NewTracker(filepath.Dir(tr.path), home)
	if err := tr2.Scan(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
	if got := tr2.Report(1, time.Now()).Total.Output; got != 30 {
		t.Fatalf("after restart output %d, want 30", got)
	}
}

// Codex отдаёт накопленный итог; считаем разницу, кеш вычитаем из ввода.
func TestCodexCumulative(t *testing.T) {
	tr, home := setup(t)
	p := filepath.Join(home, ".codex", "sessions", "2026", "09", "24", "rollout-x.jsonl")
	ts := now()
	count := func(in, cached, out int64) string {
		return `{"timestamp":"` + ts + `","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":` +
			itoa(in) + `,"cached_input_tokens":` + itoa(cached) + `,"cache_write_input_tokens":0,"output_tokens":` + itoa(out) +
			`,"reasoning_output_tokens":0,"total_tokens":0}}}}`
	}
	writeLines(t, p,
		`{"timestamp":"`+ts+`","type":"session_meta","payload":{"id":"cx1","cwd":"C:\\proj\\b"}}`,
		`{"timestamp":"`+ts+`","type":"turn_context","payload":{"model":"gpt-6-astra","cwd":"C:\\proj\\b"}}`,
		count(1000, 400, 50),
		count(1000, 400, 50), // тот же итог — хода не было
		count(2500, 1400, 120),
	)
	if err := tr.Scan(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
	rep := tr.Report(7, time.Now())
	want := Tokens{Input: 1100, CacheRead: 1400, Output: 120, Calls: 2}
	if rep.Total.Tokens != want {
		t.Fatalf("codex total %+v, want %+v", rep.Total.Tokens, want)
	}
	if len(rep.ByModel) != 1 || rep.ByModel[0].Key != "gpt-6-astra" {
		t.Fatalf("by model %+v", rep.ByModel)
	}
	if len(rep.ByAccount) != 1 || rep.ByAccount[0].Label != "основной" || !strings.HasPrefix(rep.ByAccount[0].Key, "codex|") {
		t.Fatalf("by account %+v", rep.ByAccount)
	}
}

func TestReportDaysAndRetention(t *testing.T) {
	tr, home := setup(t)
	p := filepath.Join(home, ".claude", "projects", "x", "s.jsonl")
	old := time.Now().AddDate(0, 0, -3).UTC().Format(time.RFC3339Nano)
	ancient := time.Now().AddDate(0, 0, -(RetentionDays + 5)).UTC().Format(time.RFC3339Nano)
	writeLines(t, p,
		claudeAssistant(now(), "a", "1", "claude-opus-5-5", 0, 10, 0, 0),
		claudeAssistant(old, "b", "2", "claude-sonnet-5", 0, 20, 0, 0),
		claudeAssistant(ancient, "c", "3", "claude-sonnet-5", 0, 40, 0, 0),
	)
	if err := tr.Scan(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
	if got := tr.Report(1, time.Now()).Total.Output; got != 10 {
		t.Fatalf("today %d, want 10", got)
	}
	r7 := tr.Report(7, time.Now())
	if r7.Total.Output != 30 || len(r7.ByDay) != 7 {
		t.Fatalf("7 days output %d days %d", r7.Total.Output, len(r7.ByDay))
	}
	if got := tr.Report(RetentionDays, time.Now()).Total.Output; got != 30 {
		t.Fatalf("retention: ancient row kept, output %d", got)
	}
}
