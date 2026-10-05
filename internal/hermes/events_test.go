package hermes

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"testing"
	"time"
)

type eventWaiter interface {
	WaitEvents(context.Context, uint64, time.Duration) (EventBatch, error)
}

func waiter(t *testing.T, m *Manager) eventWaiter {
	t.Helper()
	w, ok := any(m).(eventWaiter)
	if !ok {
		t.Fatal("manager has no cancellable event wait")
	}
	return w
}

func TestWaitEventsBroadcastsNewFrames(t *testing.T) {
	m := &Manager{}
	w := waiter(t, m)
	const n = 16
	done := make(chan EventBatch, n)
	for i := 0; i < n; i++ {
		go func() {
			b, err := w.WaitEvents(context.Background(), 0, time.Second)
			if err != nil {
				done <- EventBatch{}
			} else {
				done <- b
			}
		}()
	}
	// Also covers an event arriving before a waiter registers (no lost wakeup).
	time.Sleep(25 * time.Millisecond)
	m.addEvent([]byte(`{"delta":"one"}`))
	for i := 0; i < n; i++ {
		select {
		case b := <-done:
			if b.LatestSeq != 1 || len(b.Events) != 1 {
				t.Fatalf("lost broadcast: %+v", b)
			}
		case <-time.After(300 * time.Millisecond):
			t.Fatal("waiter did not wake immediately")
		}
	}
	// Returned frame ownership remains independent for callers.
	b, err := w.WaitEvents(context.Background(), 0, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	b.Events[0].Frame[0] = 'x'
	if m.Events(0).Events[0].Frame[0] != '{' {
		t.Fatal("caller changed retained frame")
	}
}

func TestWaitEventsCancellationAndTimeout(t *testing.T) {
	m := &Manager{}
	w := waiter(t, m)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := w.WaitEvents(ctx, 0, time.Minute); done <- err }()
	time.Sleep(10 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancel: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancel leaked waiter")
	}
	start := time.Now()
	b, err := w.WaitEvents(context.Background(), 0, 25*time.Millisecond)
	if err != nil || b.Events == nil || len(b.Events) != 0 || time.Since(start) < 20*time.Millisecond || time.Since(start) > time.Second {
		t.Fatalf("timeout: %+v %v %v", b, err, time.Since(start))
	}
}

func TestWaitEventsCloseWakesWaiters(t *testing.T) {
	m := &Manager{}
	w := waiter(t, m)
	done := make(chan error, 1)
	go func() { _, err := w.WaitEvents(context.Background(), 0, time.Minute); done <- err }()
	time.Sleep(10 * time.Millisecond)
	if err := m.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, ErrClosed) {
			t.Fatalf("close: %v", err)
		}
	case <-time.After(300 * time.Millisecond):
		t.Fatal("closing failed to wake waiter")
	}
}

func TestWaitEventsResetWakesAtUnchangedCursor(t *testing.T) {
	m := &Manager{seq: 1, epoch: 1}
	w := waiter(t, m)
	reset, ok := any(m).(interface{ resetEventsLocked() })
	if !ok {
		t.Fatal("event resets cannot notify waiting callers")
	}
	done := make(chan EventBatch, 1)
	go func() { b, _ := w.WaitEvents(context.Background(), 1, time.Second); done <- b }()
	waitForEventSubscription(t, m)
	m.mu.Lock()
	m.epoch++
	m.epochStart = m.seq
	m.events = nil
	reset.resetEventsLocked()
	m.mu.Unlock()
	select {
	case b := <-done:
		if !b.Reset || b.LatestSeq != 1 || len(b.Events) != 0 {
			t.Fatalf("reset lost: %+v", b)
		}
	case <-time.After(300 * time.Millisecond):
		t.Fatal("reset did not wake waiter")
	}
}

func TestWaitEventsStopWakesWithoutNewFrames(t *testing.T) {
	m := &Manager{ready: true, seq: 1, epoch: 1}
	done := make(chan EventBatch, 1)
	go func() { b, _ := waiter(t, m).WaitEvents(context.Background(), 1, time.Second); done <- b }()
	waitForEventSubscription(t, m)
	if err := m.stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case b := <-done:
		if !b.Reset {
			t.Fatalf("stop did not invalidate cursor: %+v", b)
		}
	case <-time.After(300 * time.Millisecond):
		t.Fatal("stop stranded long poll")
	}
}

func waitForEventSubscription(t *testing.T, m *Manager) {
	t.Helper()
	deadline := time.After(time.Second)
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		m.mu.Lock()
		subscribed := m.eventChanged != nil
		m.mu.Unlock()
		if subscribed {
			return
		}
		select {
		case <-deadline:
			t.Fatal("waiter did not subscribe")
		case <-ticker.C:
		}
	}
}

type eventObservedContext struct {
	context.Context
	checked       chan struct{}
	once          sync.Once
	calls, target int
}

func (ctx *eventObservedContext) Err() error {
	err := ctx.Context.Err()
	ctx.calls++
	if ctx.calls == max(ctx.target, 1) {
		ctx.once.Do(func() { close(ctx.checked) })
	}
	return err
}

func TestWaitEventsCancelWhileReacquiringLock(t *testing.T) {
	m := &Manager{}
	base, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx := &eventObservedContext{Context: base, checked: make(chan struct{}), target: 3}
	done := make(chan error, 1)
	go func() { _, err := m.WaitEvents(ctx, 0, time.Second); done <- err }()
	waitForEventSubscription(t, m)
	m.mu.Lock()
	m.seq = 1
	m.events = []Event{{Seq: 1, Frame: []byte(`{}`)}}
	m.notifyEventsLocked()
	<-ctx.checked
	cancel()
	m.mu.Unlock()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("returned events after cancellation at final lock: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("final lock cancellation stranded waiter")
	}
}

func TestWaitEventsCancelWhileAcquiringLock(t *testing.T) {
	m := &Manager{seq: 1, events: []Event{{Seq: 1, Frame: []byte(`{}`)}}}
	w := waiter(t, m)
	base, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx := &eventObservedContext{Context: base, checked: make(chan struct{})}
	done := make(chan error, 1)
	m.mu.Lock()
	go func() { _, err := w.WaitEvents(ctx, 0, time.Second); done <- err }()
	<-ctx.checked
	cancel()
	m.mu.Unlock()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("returned events after cancellation at lock: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("lock cancellation stranded waiter")
	}
}

func TestWaitEventsRepeatedCancellationDoesNotSpawnGoroutines(t *testing.T) {
	m := &Manager{}
	w := waiter(t, m)
	before := runtime.NumGoroutine()
	for i := 0; i < 64; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { _, err := w.WaitEvents(ctx, 0, time.Second); done <- err }()
		waitForEventSubscription(t, m)
		cancel()
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("cancel %d: %v", i, err)
			}
		case <-time.After(time.Second):
			t.Fatal("leaked cancellation")
		}
	}
	// WaitEvents may retain one shared channel, never a worker goroutine.
	time.Sleep(10 * time.Millisecond)
	if after := runtime.NumGoroutine(); after > before {
		t.Fatalf("event waits leaked goroutines: before=%d after=%d", before, after)
	}
}

func TestEventCursorDoesNotCopyBacklog(t *testing.T) {
	m := &Manager{seq: 42, events: []Event{{Seq: 42, Frame: []byte(`{"large":"backlog"}`)}}}
	cursor, ok := any(m).(interface{ EventCursor() EventBatch })
	if !ok {
		t.Fatal("manager has no cheap cursor-only snapshot")
	}
	b := cursor.EventCursor()
	if b.LatestSeq != 42 || b.Events == nil || len(b.Events) != 0 || b.Reset {
		t.Fatalf("unexpected cursor: %+v", b)
	}
	if allocs := testing.AllocsPerRun(20, func() { cursor.EventCursor() }); allocs != 0 {
		t.Fatalf("cursor copied/allocated backlog: %v allocations", allocs)
	}
}
