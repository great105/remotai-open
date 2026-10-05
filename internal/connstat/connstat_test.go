package connstat

import (
	"testing"
	"time"
)

func TestOpenCloseSnapshot(t *testing.T) {
	r := New(16)

	c := r.Open(KindPTY, "sess-1", 42)
	if c == nil {
		t.Fatal("Open returned nil")
	}
	c.RTT(50 * time.Millisecond)

	// Пока открыто: одно активное, один open, ноль закрытий.
	s := r.Snapshot(100)
	if len(s.Active) != 1 || s.OpensTotal != 1 || s.ClosesTotal != 0 {
		t.Fatalf("after open: active=%d opens=%d closes=%d", len(s.Active), s.OpensTotal, s.ClosesTotal)
	}
	if s.Active[0].RTTMs != 50 {
		t.Fatalf("active rtt = %d, want 50", s.Active[0].RTTMs)
	}

	c.Close("timeout")

	s = r.Snapshot(100)
	if len(s.Active) != 0 {
		t.Fatalf("after close: active=%d, want 0", len(s.Active))
	}
	if s.ClosesTotal != 1 || s.Closes5m != 1 {
		t.Fatalf("closes_total=%d closes_5m=%d, want 1/1", s.ClosesTotal, s.Closes5m)
	}
	if s.ReasonCounts["timeout"] != 1 {
		t.Fatalf("reason_counts[timeout]=%d, want 1", s.ReasonCounts["timeout"])
	}
	// Лента: новейшее событие — закрытие, с причиной и непустым RTT.
	if len(s.RecentEvents) != 2 || s.RecentEvents[0].Action != "close" {
		t.Fatalf("recent[0]=%+v", s.RecentEvents[0])
	}
	if s.RecentEvents[0].Reason != "timeout" || s.RecentEvents[0].RTTMs != 50 {
		t.Fatalf("close event = %+v", s.RecentEvents[0])
	}
}

func TestCloseIdempotent(t *testing.T) {
	r := New(16)
	c := r.Open(KindRemote, "screen", 1)
	c.Close("client-close")
	c.Close("client-close") // повторный вызов не должен задвоить счётчик
	if s := r.Snapshot(0); s.ClosesTotal != 1 {
		t.Fatalf("closes_total=%d, want 1 (idempotent)", s.ClosesTotal)
	}
}

func TestReconnectGap(t *testing.T) {
	r := New(16)
	c := r.Open(KindPTY, "s1", 1)
	c.Close("connection-lost")
	time.Sleep(3 * time.Millisecond)
	r.Open(KindPTY, "s1", 1) // реконнект той же сессии
	s := r.Snapshot(10)
	if s.Reconnects5m != 1 {
		t.Fatalf("reconnects_5m=%d, want 1", s.Reconnects5m)
	}
	if s.AvgReconnectMs <= 0 {
		t.Fatalf("avg_reconnect_ms=%d, want >0", s.AvgReconnectMs)
	}
	// Первый open другой сессии gap не считает.
	r.Open(KindPTY, "fresh", 1)
	if r.Snapshot(10).Reconnects5m != 1 {
		t.Fatal("fresh session must not count as reconnect")
	}
}

func TestNilSafe(t *testing.T) {
	var r *Registry
	c := r.Open(KindPTY, "x", 0) // выключенный реестр
	c.RTT(time.Second)           // не паникует на nil
	c.Close("x")                 // не паникует на nil
}

func TestRingEviction(t *testing.T) {
	r := New(16) // минимум
	for i := 0; i < 50; i++ {
		c := r.Open(KindPTY, "s", 1)
		c.Close("client-close")
	}
	s := r.Snapshot(0)
	// Кольцо хранит максимум cap событий, но кумулятивные счётчики — все.
	if len(s.RecentEvents) > 16 {
		t.Fatalf("ring kept %d events, cap 16", len(s.RecentEvents))
	}
	if s.OpensTotal != 50 || s.ClosesTotal != 50 {
		t.Fatalf("totals opens=%d closes=%d, want 50/50", s.OpensTotal, s.ClosesTotal)
	}
}

// TestReconnectGapThreshold: короткий гэп считается реконнектом (avg/max),
// длинный (юзер ушёл и вернулся) — нет, чтобы не портить метрику.
func TestReconnectGapThreshold(t *testing.T) {
	r := New(16)

	// Короткий гэп: close → open через ~2с (подделываем время закрытия).
	r.Open(KindPTY, "fast", 1).Close("client-gone")
	r.mu.Lock()
	r.lastClose[connKey(KindPTY, "fast")] = time.Now().Add(-2 * time.Second)
	r.mu.Unlock()
	r.Open(KindPTY, "fast", 1)

	// Длинный гэп: полчаса — это не реконнект.
	r.Open(KindPTY, "away", 1).Close("client-gone")
	r.mu.Lock()
	r.lastClose[connKey(KindPTY, "away")] = time.Now().Add(-30 * time.Minute)
	r.mu.Unlock()
	r.Open(KindPTY, "away", 1)

	s := r.Snapshot(100)
	if s.Reconnects5m != 1 {
		t.Fatalf("reconnects_5m=%d, want 1 (длинный гэп не считается)", s.Reconnects5m)
	}
	if s.AvgReconnectMs < 1500 || s.AvgReconnectMs > 3000 {
		t.Fatalf("avg_reconnect_ms=%d, want ~2000", s.AvgReconnectMs)
	}
	if s.MaxReconnectMs != s.AvgReconnectMs {
		t.Fatalf("max=%d avg=%d, want equal (один реконнект)", s.MaxReconnectMs, s.AvgReconnectMs)
	}
}
