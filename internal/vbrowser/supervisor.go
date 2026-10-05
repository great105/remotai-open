package vbrowser

// Pure lifecycle decisions of the session supervisor, kept platform-neutral so
// unit tests exercise them without a real Xvfb. The Linux session wires these
// to live processes; tests inject times and durations directly.

import (
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Supervisor knobs (package vars so tests can shrink them). One budget covers
// both the browser and Xvfb: they are a single unit of health — if the pair
// cannot stay up, restarting pieces of it forever helps no one.
var (
	restartMaxFails    = 5
	restartStableAfter = 5 * time.Minute
	restartBackoffBase = time.Second

	// Idle stop: tick cadence and how long before the deadline the one-shot
	// warning goes out.
	idleTick       = 30 * time.Second
	idleWarnBefore = 5 * time.Minute
)

// defaultIdleMinutes — idle session lifetime without viewers and input.
const defaultIdleMinutes = 30

// restartDecision spends one unit of the restart budget. A process that
// survived restartStableAfter resets the budget first — an occasional crash
// is not a crash loop. ok=false means the budget is exhausted: delays were
// 1s, 2s, 4s, 8s, 16s across the five attempts.
func restartDecision(fails int, stable bool) (newFails int, delay time.Duration, ok bool) {
	if stable {
		fails = 0
	}
	if fails >= restartMaxFails {
		return fails, 0, false
	}
	return fails + 1, restartBackoffBase << uint(fails), true
}

// idleVerdict decides what the idle ticker should do: nothing, emit the
// one-shot warning (inside the warnBefore window before the deadline), or
// stop the session. Watching viewers and a disabled timeout suppress both.
func idleVerdict(now, lastActivity time.Time, viewers int, timeout, warnBefore time.Duration, warned bool) (warn, stop bool) {
	if timeout <= 0 || viewers > 0 {
		return false, false
	}
	left := timeout - now.Sub(lastActivity)
	if left <= 0 {
		return false, true
	}
	if !warned && left <= warnBefore {
		return true, false
	}
	return false, false
}

// idleTimeout — how long the browser may sit unwatched and untouched before
// the session stops itself. REMOTAI_VBROWSER_IDLE_MINUTES overrides the
// default without a rebuild (same pattern as REMOTAI_AI_USAGE_TTL); 0
// disables the idle stop, a broken value falls back to the default.
func idleTimeout() time.Duration {
	if v := strings.TrimSpace(os.Getenv("REMOTAI_VBROWSER_IDLE_MINUTES")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			return time.Duration(n) * time.Minute
		}
	}
	return defaultIdleMinutes * time.Minute
}

// errRing is a truncating writer for a child process's stderr. GPU/renderer
// crashes explain themselves in those last lines, but days of browser output
// must not grow memory, so only the tail survives.
type errRing struct {
	mu  sync.Mutex
	buf []byte
	cap int
}

func newErrRing(capacity int) *errRing { return &errRing{cap: capacity} }

func (r *errRing) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(p) >= r.cap {
		r.buf = append(r.buf[:0], p[len(p)-r.cap:]...)
		return len(p), nil
	}
	if len(r.buf)+len(p) > r.cap {
		r.buf = r.buf[len(r.buf)+len(p)-r.cap:]
	}
	r.buf = append(r.buf, p...)
	return len(p), nil
}

// Tail returns the buffered tail (may be empty).
func (r *errRing) Tail() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return string(r.buf)
}

// Reset drops the buffer (a restarted process starts with a clean slate).
func (r *errRing) Reset() {
	r.mu.Lock()
	r.buf = nil
	r.mu.Unlock()
}
