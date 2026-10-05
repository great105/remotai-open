package web

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"tgcontrol/internal/hermes"
)

type hermesCursorFixture struct {
	*hermesWebFixture
	cursorCalls, eventsCalls int
}

func (f *hermesCursorFixture) EventCursor() hermes.EventBatch {
	f.cursorCalls++
	return hermes.EventBatch{Events: []hermes.Event{}, LatestSeq: 42}
}
func (f *hermesCursorFixture) Events(after uint64) hermes.EventBatch {
	f.eventsCalls++
	return f.hermesWebFixture.Events(after)
}

type hermesWaitingFixture struct {
	*hermesWebFixture
	wait func(context.Context, uint64, time.Duration) (hermes.EventBatch, error)
}

func (f *hermesWaitingFixture) WaitEvents(ctx context.Context, after uint64, wait time.Duration) (hermes.EventBatch, error) {
	return f.wait(ctx, after, wait)
}

func TestHermesEventsWaitUsesOptionalRuntimeAndBounds(t *testing.T) {
	for _, query := range []struct {
		raw      string
		duration time.Duration
	}{{"125", 125 * time.Millisecond}, {"20000", 20 * time.Second}, {"50000", 20 * time.Second}} {
		calls := 0
		f := &hermesWaitingFixture{hermesWebFixture: &hermesWebFixture{}, wait: func(ctx context.Context, after uint64, wait time.Duration) (hermes.EventBatch, error) {
			calls++
			if after != 42 || wait != query.duration || ctx.Err() != nil {
				t.Fatalf("wrong wait: after=%d wait=%v", after, wait)
			}
			return hermes.EventBatch{Events: []hermes.Event{}, LatestSeq: 42}, nil
		}}
		s := &Server{hermesManagers: map[int64]hermesRuntime{2: f}}
		w := httptest.NewRecorder()
		s.apiHermesEvents(w, httptest.NewRequest("GET", "/api/hermes/events?after=42&wait_ms="+query.raw, nil), 2)
		if calls != 1 || w.Code != 200 {
			t.Fatalf("did not wait: calls=%d code=%d", calls, w.Code)
		}
	}
}

func TestHermesEventsRejectsInvalidWait(t *testing.T) {
	for _, raw := range []string{"-1", "NaN", "1.5", "", "18446744073709551616"} {
		s := &Server{hermesManagers: map[int64]hermesRuntime{2: &hermesWebFixture{}}}
		w := httptest.NewRecorder()
		s.apiHermesEvents(w, httptest.NewRequest("GET", "/api/hermes/events?wait_ms="+raw, nil), 2)
		if w.Code != 400 {
			t.Fatalf("invalid wait %q accepted: %d", raw, w.Code)
		}
	}
}

func TestHermesEventsLegacyRuntimeKeepsPollingContract(t *testing.T) {
	f := &hermesWebFixture{frames: hermes.EventBatch{Events: []hermes.Event{{Seq: 42, Frame: json.RawMessage(`{}`)}}, LatestSeq: 42}}
	s := &Server{hermesManagers: map[int64]hermesRuntime{2: f}}
	for _, query := range []string{"after=41", "after=41&wait_ms=20000", "after=41&wait_ms=0"} {
		w := httptest.NewRecorder()
		s.apiHermesEvents(w, httptest.NewRequest("GET", "/api/hermes/events?"+query, nil), 2)
		var b hermes.EventBatch
		if json.Unmarshal(w.Body.Bytes(), &b) != nil || len(b.Events) != 1 || b.LatestSeq != 42 {
			t.Fatalf("legacy polling changed: %s", w.Body.String())
		}
	}
	w := httptest.NewRecorder()
	s.apiHermesEvents(w, httptest.NewRequest("GET", "/api/hermes/events?cursor=1&wait_ms=20000", nil), 2)
	var b hermes.EventBatch
	if json.Unmarshal(w.Body.Bytes(), &b) != nil || b.Events == nil || len(b.Events) != 0 || b.LatestSeq != 42 {
		t.Fatalf("legacy cursor leaked events: %s", w.Body.String())
	}
}

func TestHermesEventsWaitPropagatesRequestCancellation(t *testing.T) {
	entered := make(chan struct{})
	f := &hermesWaitingFixture{hermesWebFixture: &hermesWebFixture{}, wait: func(ctx context.Context, _ uint64, _ time.Duration) (hermes.EventBatch, error) {
		close(entered)
		<-ctx.Done()
		return hermes.EventBatch{}, ctx.Err()
	}}
	s := &Server{hermesManagers: map[int64]hermesRuntime{2: f}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.apiHermesEvents(w, httptest.NewRequest("GET", "/api/hermes/events?wait_ms=20000", nil).WithContext(ctx), 2)
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("runtime did not receive wait")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("cancelled HTTP handler leaked")
	}
	if w.Body.Len() != 0 {
		t.Fatalf("wrote to canceled request: %s", w.Body.String())
	}
}

func TestHermesEventsWaitClosedRuntimeReturnsUnavailable(t *testing.T) {
	f := &hermesWaitingFixture{hermesWebFixture: &hermesWebFixture{}, wait: func(context.Context, uint64, time.Duration) (hermes.EventBatch, error) {
		return hermes.EventBatch{}, hermes.ErrClosed
	}}
	s := &Server{hermesManagers: map[int64]hermesRuntime{2: f}}
	w := httptest.NewRecorder()
	s.apiHermesEvents(w, httptest.NewRequest("GET", "/api/hermes/events?wait_ms=1", nil), 2)
	if w.Code != 503 {
		t.Fatalf("closing returned success: %d %s", w.Code, w.Body.String())
	}
}

func TestHermesEventsCursorUsesCheapOptionalSnapshot(t *testing.T) {
	f := &hermesCursorFixture{hermesWebFixture: &hermesWebFixture{frames: hermes.EventBatch{Events: []hermes.Event{{Seq: 42, Frame: json.RawMessage(`{}`)}}, LatestSeq: 42}}}
	s := &Server{hermesManagers: map[int64]hermesRuntime{2: f}}
	w := httptest.NewRecorder()
	s.apiHermesEvents(w, httptest.NewRequest("GET", "/api/hermes/events?cursor=1", nil), 2)
	var b hermes.EventBatch
	if json.Unmarshal(w.Body.Bytes(), &b) != nil || w.Code != 200 || b.Events == nil || len(b.Events) != 0 || b.LatestSeq != 42 || f.cursorCalls != 1 || f.eventsCalls != 0 {
		t.Fatalf("cursor copied backlog: %s cursor=%d events=%d", w.Body.String(), f.cursorCalls, f.eventsCalls)
	}
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("cursor is cacheable")
	}
}
