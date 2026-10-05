package relayhub

import (
	"sync/atomic"
	"testing"
)

// TestLiveBridges: CloseLiveBridges рвёт нужные мосты (все или конкретного
// пользователя), не трогает чужие, и реестр чистится.
func TestLiveBridges(t *testing.T) {
	h := NewHub()
	var a, b, c atomic.Bool
	_ = h.AddLiveBridge("dev1", 10, func() { a.Store(true) })
	_ = h.AddLiveBridge("dev1", 20, func() { b.Store(true) })
	_ = h.AddLiveBridge("dev2", 10, func() { c.Store(true) })

	// Отзыв одного гранта (user 20) на dev1 — рвёт только его мост.
	u20 := int64(20)
	if n := h.CloseLiveBridges("dev1", &u20); n != 1 {
		t.Fatalf("closed %d, want 1", n)
	}
	if a.Load() || c.Load() || !b.Load() {
		t.Fatalf("closed wrong bridge: a=%v b=%v c=%v", a.Load(), b.Load(), c.Load())
	}

	// Отзыв всего dev1 (match nil) — рвёт оставшийся мост user 10.
	if n := h.CloseLiveBridges("dev1", nil); n != 1 {
		t.Fatalf("closed %d on full revoke, want 1", n)
	}
	if !a.Load() {
		t.Fatal("user10 bridge on dev1 not closed by full revoke")
	}
	// dev2 не тронут.
	if c.Load() {
		t.Fatal("dev2 bridge closed by dev1 revoke")
	}
	// Реестр dev1 очищен.
	h.liveMu.Lock()
	_, ok := h.live["dev1"]
	h.liveMu.Unlock()
	if ok {
		t.Fatal("dev1 не удалён из реестра после закрытия всех мостов")
	}
}

// TestRemoveLiveBridge: нормальное завершение стрима снимает мост из реестра,
// повторный/нулевой вызов безопасен.
func TestRemoveLiveBridge(t *testing.T) {
	h := NewHub()
	var closed atomic.Bool
	lb := h.AddLiveBridge("dev1", 1, func() { closed.Store(true) })
	h.RemoveLiveBridge(lb)
	h.RemoveLiveBridge(lb)  // idempotent
	h.RemoveLiveBridge(nil) // nil-safe

	// После снятия отзыв ничего не находит и closer НЕ зовётся.
	if n := h.CloseLiveBridges("dev1", nil); n != 0 {
		t.Fatalf("closed %d after remove, want 0", n)
	}
	if closed.Load() {
		t.Fatal("closer вызван для снятого моста")
	}
}
