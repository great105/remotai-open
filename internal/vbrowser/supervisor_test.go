package vbrowser

import (
	"testing"
	"time"
)

// The restart budget must spend exactly five attempts with a 1s→16s backoff
// ladder and then refuse more.
func TestRestartDecisionBackoffLadder(t *testing.T) {
	want := []time.Duration{1, 2, 4, 8, 16}
	fails := 0
	for i, secs := range want {
		var delay time.Duration
		var ok bool
		fails, delay, ok = restartDecision(fails, false)
		if !ok {
			t.Fatalf("attempt %d: refused too early (fails=%d)", i+1, fails)
		}
		if delay != secs*time.Second {
			t.Fatalf("attempt %d: delay=%s, want %ds", i+1, delay, secs)
		}
	}
	if fails != restartMaxFails {
		t.Fatalf("fails=%d after ladder, want %d", fails, restartMaxFails)
	}
	if _, _, ok := restartDecision(fails, false); ok {
		t.Fatal("sixth restart allowed — budget must be exhausted")
	}
}

// A process that survived long enough resets the budget: an occasional crash
// after hours of uptime is not a crash loop.
func TestRestartDecisionStableResetsBudget(t *testing.T) {
	fails := 0
	for i := 0; i < 4; i++ {
		fails, _, _ = restartDecision(fails, false)
	}
	if fails != 4 {
		t.Fatalf("fails=%d, want 4", fails)
	}
	fails, delay, ok := restartDecision(fails, true)
	if !ok {
		t.Fatal("stable process refused a restart")
	}
	if fails != 1 || delay != time.Second {
		t.Fatalf("after stable reset: fails=%d delay=%s, want 1/1s", fails, delay)
	}
}

// Even a stable process is denied when the budget is already exhausted — the
// reset applies before the check, so only a non-exhausted budget benefits.
func TestRestartDecisionStableAtLimit(t *testing.T) {
	_, delay, ok := restartDecision(restartMaxFails, false)
	if ok || delay != 0 {
		t.Fatalf("exhausted budget: ok=%v delay=%s, want refused", ok, delay)
	}
}

func TestIdleVerdict(t *testing.T) {
	now := time.Now()
	timeout := 30 * time.Minute
	warnBefore := 5 * time.Minute

	cases := []struct {
		name         string
		lastActivity time.Time
		viewers      int
		timeout      time.Duration
		warned       bool
		wantWarn     bool
		wantStop     bool
	}{
		{"disabled timeout", now.Add(-2 * time.Hour), 0, 0, false, false, false},
		{"viewer watching past deadline", now.Add(-2 * time.Hour), 1, timeout, false, false, false},
		{"recent activity", now.Add(-time.Minute), 0, timeout, false, false, false},
		{"inside warn window", now.Add(-26 * time.Minute), 0, timeout, false, true, false},
		{"warn fires once", now.Add(-26 * time.Minute), 0, timeout, true, false, false},
		{"past deadline", now.Add(-31 * time.Minute), 0, timeout, true, false, true},
		{"stop beats warn", now.Add(-31 * time.Minute), 0, timeout, false, false, true},
	}
	for _, tc := range cases {
		warn, stop := idleVerdict(now, tc.lastActivity, tc.viewers, tc.timeout, warnBefore, tc.warned)
		if warn != tc.wantWarn || stop != tc.wantStop {
			t.Errorf("%s: warn=%v stop=%v, want %v/%v", tc.name, warn, stop, tc.wantWarn, tc.wantStop)
		}
	}
}

// REMOTAI_VBROWSER_IDLE_MINUTES overrides the default without a rebuild;
// 0 disables the idle stop, garbage falls back to the default.
func TestIdleTimeoutEnv(t *testing.T) {
	t.Run("default", func(t *testing.T) {
		if d := idleTimeout(); d != defaultIdleMinutes*time.Minute {
			t.Fatalf("idleTimeout()=%s, want %dm", d, defaultIdleMinutes)
		}
	})
	t.Run("override", func(t *testing.T) {
		t.Setenv("REMOTAI_VBROWSER_IDLE_MINUTES", "45")
		if d := idleTimeout(); d != 45*time.Minute {
			t.Fatalf("idleTimeout()=%s, want 45m", d)
		}
	})
	t.Run("zero disables", func(t *testing.T) {
		t.Setenv("REMOTAI_VBROWSER_IDLE_MINUTES", "0")
		if d := idleTimeout(); d != 0 {
			t.Fatalf("idleTimeout()=%s, want 0 (disabled)", d)
		}
	})
	t.Run("garbage falls back", func(t *testing.T) {
		t.Setenv("REMOTAI_VBROWSER_IDLE_MINUTES", "soon")
		if d := idleTimeout(); d != defaultIdleMinutes*time.Minute {
			t.Fatalf("idleTimeout()=%s, want %dm", d, defaultIdleMinutes)
		}
	})
}

// The stderr ring keeps exactly the last cap bytes across writes, survives a
// write larger than the cap itself, and forgets everything after Reset.
func TestErrRing(t *testing.T) {
	r := newErrRing(10)
	if _, err := r.Write([]byte("0123456789")); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Write([]byte("abc")); err != nil {
		t.Fatal(err)
	}
	if got := r.Tail(); got != "3456789abc" {
		t.Fatalf("tail=%q, want %q", got, "3456789abc")
	}
	if _, err := r.Write([]byte("0123456789ABCDEF")); err != nil {
		t.Fatal(err)
	}
	if got := r.Tail(); got != "6789ABCDEF" {
		t.Fatalf("oversized write: tail=%q, want %q", got, "6789ABCDEF")
	}
	r.Reset()
	if got := r.Tail(); got != "" {
		t.Fatalf("after Reset tail=%q, want empty", got)
	}
}
