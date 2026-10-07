package hermes

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

func TestObserverRetriesFailedSubscriptionWithoutClientRPC(t *testing.T) {
	f := newFixture(t)
	m := f.manager(t, true)
	var attempts atomic.Int32
	f.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/ws" && attempts.Add(1) <= 2 {
			http.Error(w, "fixture temporary refusal", http.StatusServiceUnavailable)
			return
		}
		f.serveHTTP(w, r)
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := m.SetAutoStart(true); err != nil {
		t.Fatal(err)
	}
	m.StartMaintenance(ctx)
	deadline := time.Now().Add(6 * time.Second)
	for {
		f.mu.Lock()
		n := f.connections
		f.mu.Unlock()
		// The server has accepted a socket before its first event reaches the
		// observer. Wait for the promised event, not only the handshake count.
		if n == 1 && m.EventCursor().LatestSeq > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("observer did not retry failed subscriptions: %d", attempts.Load())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if attempts.Load() != 3 {
		t.Fatalf("duplicate subscription: %d", attempts.Load())
	}
	cursor := m.EventCursor().LatestSeq
	if cursor == 0 {
		t.Fatal("retry did not receive gateway event")
	}
	cancel()
	time.Sleep(500 * time.Millisecond)
	if attempts.Load() != 3 {
		t.Fatal("observer reconnected after application lifetime cancellation")
	}
}
