package pty

import (
	"hash/fnv"
	"io"
	"sync"
	"time"
)

// Codex repaints while idle. A Stop is authoritative until another submitted
// input; sessions without hooks fall back to changes in visible cells, never
// cursor/style redraw bytes. Keep this clock separate from transport activity.
type codexActivity struct {
	mu          sync.Mutex
	fingerprint uint64
	sampled     bool
	changedAt   time.Time
	submittedAt time.Time
	stoppedAt   time.Time
	hooked      bool // a verified foreground Stop has been observed
	inputCSI    string
	inputEscape bool
	inputPaste  bool
}

func (a *codexActivity) observe(fp uint64, valid bool, now time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !valid {
		a.sampled = false // a stale mirror must not prove idle
		return
	}
	if !a.sampled || fp != a.fingerprint {
		a.changedAt = now
	}
	a.fingerprint, a.sampled = fp, true
}

func (a *codexActivity) stop(at time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.hooked = true
	// The detector may drain the previous turn's Stop after the next Enter.
	if !at.Before(a.submittedAt) && at.After(a.stoppedAt) {
		a.stoppedAt = at
	}
}

func (a *codexActivity) status(now time.Time) (string, time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.stoppedAt.IsZero() {
		return "ready", a.stoppedAt
	}
	// Once this terminal has received a verified foreground notify, a silent
	// turn remains working until the next notify. Codex can think for much
	// longer than the screen-idle threshold without repainting.
	if a.hooked && !a.submittedAt.IsZero() {
		at := a.submittedAt
		if a.changedAt.After(at) {
			at = a.changedAt
		}
		return "working", at
	}
	if a.sampled {
		if now.Sub(a.changedAt) >= idleWaiting {
			return "ready", a.changedAt
		}
		return "working", a.changedAt
	}
	return "", time.Time{}
}

// Observe only bytes accepted by the backend. CSI may cross writes; Enter
// inside bracketed paste is a newline, while the Enter after 201~ submits it.
func (a *codexActivity) input(data []byte, now time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, b := range data {
		if a.inputEscape {
			a.inputEscape = false
			if b == '[' {
				a.inputCSI = "["
				continue
			}
		}
		if a.inputCSI != "" {
			a.inputCSI += string(b)
			if b >= 0x40 && b <= 0x7e {
				if a.inputCSI == "[200~" {
					a.inputPaste = true
				}
				if a.inputCSI == "[201~" {
					a.inputPaste = false
				}
				a.inputCSI = ""
			} else if len(a.inputCSI) > 64 {
				a.inputCSI = ""
			}
			continue
		}
		if b == 0x1b {
			a.inputEscape = true
			continue
		}
		if !a.inputPaste && (b == '\r' || b == '\n') {
			a.submittedAt, a.changedAt = now, now
			if !a.stoppedAt.After(now) {
				a.stoppedAt = time.Time{}
			}
		}
	}
}

type activityInputWriter struct {
	io.Writer
	activity *codexActivity
}

func (w activityInputWriter) Write(data []byte) (int, error) {
	// Timestamp before the write: a fast Stop can arrive before Write returns.
	at := time.Now()
	n, err := w.Writer.Write(data)
	if n > 0 {
		w.activity.input(data[:n], at)
	}
	return n, err
}

func (s *Session) observeCodexActivity(now time.Time) {
	s.screenLifeMu.RLock()
	defer s.screenLifeMu.RUnlock()
	sc, _ := s.screen.Load().(*sessionScreen)
	if sc == nil || sc.stale.Load() || sc.queued.Load() != 0 {
		s.codexActivity.observe(0, false, now)
		return
	}
	sc.mu.Lock()
	m := sc.mirror
	sc.mu.Unlock()
	if m == nil {
		s.codexActivity.observe(0, false, now)
		return
	}
	fp, valid := m.activityFingerprint()
	s.codexActivity.observe(fp, valid && !sc.stale.Load(), now)
}

// No transcript, history allocation, cursor position or styling in this hash.
// It is a conservative activity heuristic, not a frame sent to a client.
func (m *screenMirror) activityFingerprint() (uint64, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed || !m.snapshotReadyLocked() {
		return 0, false
	}
	h := fnv.New64a()
	for y := 0; y < m.rows; y++ {
		for x := 0; x < m.cols; x++ {
			if c := m.em.CellAt(x, y); c != nil {
				_, _ = io.WriteString(h, c.Content)
			}
			_, _ = h.Write([]byte{0})
		}
		_, _ = h.Write([]byte{'\n'})
	}
	return h.Sum64(), true
}
