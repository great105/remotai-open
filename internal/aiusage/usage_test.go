package aiusage

import (
	"encoding/json"
	"testing"
)

func TestCodexWindowsUsesAllLimitBuckets(t *testing.T) {
	duration := int64(10080)
	reset := int64(1_800_000_000)
	name := "Fast model"
	response := codexRateResponse{
		RateLimitsByLimitID: map[string]codexRateSnapshot{
			"codex": {
				Primary: &codexRateWindow{UsedPercent: 42, WindowDurationMin: &duration, ResetsAt: &reset},
			},
			"fast": {
				LimitName: &name,
				Primary:   &codexRateWindow{UsedPercent: 7},
			},
		},
	}

	windows := codexWindows(response)
	if len(windows) != 2 {
		t.Fatalf("windows=%d want=2", len(windows))
	}
	if windows[0].UsedPercent != 42 || windows[1].UsedPercent != 7 {
		t.Fatalf("unexpected windows: %#v", windows)
	}
	if windows[1].Label != "Fast model · основное окно" {
		t.Fatalf("label=%q", windows[1].Label)
	}
}

func TestClaudeWindowsAndExtraUsage(t *testing.T) {
	raw := `{
		"five_hour":{"utilization":12.5,"resets_at":"2026-07-26T15:00:00+03:00"},
		"seven_day":{"utilization":99.8,"resets_at":null},
		"extra_usage":{"is_enabled":true,"monthly_limit":19000,"used_credits":17039,
			"utilization":89.6789,"currency":"USD","decimal_places":2,"spend_limit_reached":false}
	}`
	var usage claudeUsageResponse
	if err := json.Unmarshal([]byte(raw), &usage); err != nil {
		t.Fatal(err)
	}
	windows := claudeWindows(usage)
	if len(windows) != 2 {
		t.Fatalf("windows=%d want=2", len(windows))
	}
	if windows[0].ResetsAt == nil {
		t.Fatal("expected parsed reset time")
	}
	if windows[1].UsedPercent != 99.8 {
		t.Fatalf("used=%v", windows[1].UsedPercent)
	}
}

func TestClampPercent(t *testing.T) {
	for _, test := range []struct {
		in, want float64
	}{
		{-1, 0},
		{50.5, 50.5},
		{101, 100},
	} {
		if got := clampPercent(test.in); got != test.want {
			t.Fatalf("clampPercent(%v)=%v want=%v", test.in, got, test.want)
		}
	}
}
