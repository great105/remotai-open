package web

import "testing"

// TestPtyLiveCapEviction: регистрация сверх капа вытесняет старейшие слоты,
// unregister идемпотентен и чистит ключ.
func TestPtyLiveCapEviction(t *testing.T) {
	s := &Server{ptyLive: make(map[string][]*ptyLiveSlot)}
	key := "1|sess"

	slots := make([]*ptyLiveSlot, 0, ptyLiveCap+2)
	for i := 0; i < ptyLiveCap; i++ {
		sl := &ptyLiveSlot{}
		if ev := s.registerPtyConn(key, sl); len(ev) != 0 {
			t.Fatalf("evicted %d under cap", len(ev))
		}
		slots = append(slots, sl)
	}

	// Кап+1: вытесняется ровно старейший (slots[0]).
	extra := &ptyLiveSlot{}
	ev := s.registerPtyConn(key, extra)
	if len(ev) != 1 || ev[0] != slots[0] {
		t.Fatalf("evicted=%v, want oldest slot", ev)
	}
	if n := len(s.ptyLive[key]); n != ptyLiveCap {
		t.Fatalf("live=%d, want %d", n, ptyLiveCap)
	}

	// Unregister вытесненного (его defer тоже отработает) — не ломает список.
	s.unregisterPtyConn(key, ev[0])
	if n := len(s.ptyLive[key]); n != ptyLiveCap {
		t.Fatalf("после unregister вытесненного live=%d, want %d", n, ptyLiveCap)
	}

	// Снять всех — ключ должен исчезнуть.
	s.unregisterPtyConn(key, extra)
	for _, sl := range slots[1:] {
		s.unregisterPtyConn(key, sl)
	}
	if _, ok := s.ptyLive[key]; ok {
		t.Fatal("ключ не удалён после снятия всех слотов")
	}

	// Другой uid на ту же сессию — свой ключ, не мешает.
	other := &ptyLiveSlot{}
	if ev := s.registerPtyConn("2|sess", other); len(ev) != 0 {
		t.Fatalf("чужой uid вытеснен: %v", ev)
	}
}
