package pty

import (
	"fmt"
	"testing"
)

// Медленный клиент не теряет вывод.
//
// До 2.49.12 переполненный канал подписчика означал ВЫБРОС кадра: клиент его не
// видел никогда, а его позиция в потоке (он ведёт её инкрементами по длине
// принятых кадров) навсегда расходилась с серверной. Живой замер
// build/qa/probe-lost-lines.mjs: подтормозивший на 4 с клиент потерял 8 329
// строк из 20 000. Теперь подписчик помечается «отстал», и писатель до-сылает
// пропущенное из кольца — склейка «принятое + до-сланное» обязана дать поток
// байт в байт, без пропусков и без дублей.
func TestSlowSubscriberLosesNothing(t *testing.T) {
	s := &Session{epoch: "e1", subs: map[chan []byte]*subState{}, dec: newDecTracker()}
	ch, _, base, _, _, _ := s.SubscribeResume("", 0)
	defer s.Unsubscribe(ch)

	// Пишем заведомо больше, чем влезает в канал подписчика.
	var whole []byte
	const chunks = subChanFrames + 100
	for i := 0; i < chunks; i++ {
		data := []byte(fmt.Sprintf("<%04d>", i))
		whole = append(whole, data...)
		s.bufMu.Lock()
		s.buf = append(s.buf, data...)
		s.totalBytes += uint64(len(data))
		s.fanoutLocked(data)
		s.bufMu.Unlock()
	}

	st := s.subs[ch]
	if st == nil || !st.lagged.Load() {
		t.Fatal("подписчик обязан быть помечен «отстал»: канал переполнен заведомо")
	}

	// Писатель забирает то, что успело влезть в канал.
	var got []byte
drain:
	for {
		select {
		case d := <-ch:
			got = append(got, d...)
		default:
			break drain
		}
	}
	if len(got) == 0 {
		t.Fatal("канал пуст — тест не проверяет то, что должен")
	}
	sent := base + uint64(len(got))

	// До-сылка пропущенного.
	payload, offset, gap, epoch := s.ResyncFrom(ch, sent, "e1")
	if gap {
		t.Fatalf("кольцо вмещает весь поток (%d байт) — пропуска быть не должно", len(whole))
	}
	if offset != s.totalBytes {
		t.Fatalf("новая позиция %d, ожидалась %d", offset, s.totalBytes)
	}
	if epoch != "e1" {
		t.Fatalf("resync epoch=%q, want e1", epoch)
	}
	if st.lagged.Load() {
		t.Fatal("после до-сылки пометка «отстал» обязана сниматься")
	}

	// Главная проверка: человек увидел ровно то, что напечатал терминал.
	seen := append(append([]byte{}, got...), payload...)
	if string(seen) != string(whole) {
		t.Fatalf("поток на экране разошёлся с выводом терминала:\n  увидел %d байт\n  напечатано %d байт",
			len(seen), len(whole))
	}

	// И связь продолжает работать: после до-сылки кадры снова идут в канал.
	s.bufMu.Lock()
	s.totalBytes += 4
	s.buf = append(s.buf, []byte("tail")...)
	s.fanoutLocked([]byte("tail"))
	s.bufMu.Unlock()
	select {
	case d := <-ch:
		if string(d) != "tail" {
			t.Fatalf("после до-сылки пришло %q, ожидалось \"tail\"", d)
		}
	default:
		t.Fatal("после до-сылки живой вывод в канал не пошёл")
	}
}

// Кольцо провернулось за время затора: пропуск помечается честно, а не молча.
func TestSlowSubscriberBeyondRingIsMarkedGap(t *testing.T) {
	s := &Session{epoch: "e1", subs: map[chan []byte]*subState{}, dec: newDecTracker()}
	ch, _, _, _, _, _ := s.SubscribeResume("", 0)
	defer s.Unsubscribe(ch)

	s.bufMu.Lock()
	s.buf = make([]byte, liveResyncLimit*2)
	for i := range s.buf {
		s.buf[i] = byte('a' + i%26)
	}
	// Клиент отстал так, что его позиция уже вне кольца.
	s.totalBytes = uint64(len(s.buf)) + 10_000_000
	s.subs[ch].lagged.Store(true)
	s.bufMu.Unlock()

	payload, offset, gap, epoch := s.ResyncFrom(ch, 5, "e1")
	if !gap {
		t.Fatal("позиция клиента вытеснена из кольца — обязан быть признак пропуска")
	}
	if len(payload) != liveResyncLimit {
		t.Fatalf("до-слано %d байт, ожидался хвост в %d", len(payload), liveResyncLimit)
	}
	if offset != s.totalBytes {
		t.Fatalf("новая позиция %d, ожидалась %d", offset, s.totalBytes)
	}
	if epoch != "e1" {
		t.Fatalf("resync epoch=%q, want e1", epoch)
	}
}

// Догонять нечего: пометка снимается, лишних байт клиенту не уходит.
func TestResyncWhenAlreadyCaughtUp(t *testing.T) {
	s := &Session{epoch: "e1", subs: map[chan []byte]*subState{}, dec: newDecTracker()}
	ch, _, _, _, _, _ := s.SubscribeResume("", 0)
	defer s.Unsubscribe(ch)

	s.bufMu.Lock()
	s.buf = []byte("hello")
	s.totalBytes = 5
	s.subs[ch].lagged.Store(true)
	s.bufMu.Unlock()

	payload, offset, gap, epoch := s.ResyncFrom(ch, 5, "e1")
	if len(payload) != 0 || gap || offset != 5 {
		t.Fatalf("ожидалась пустая до-сылка, получено payload=%d gap=%v offset=%d", len(payload), gap, offset)
	}
	if epoch != "e1" {
		t.Fatalf("resync epoch=%q, want e1", epoch)
	}
	if s.subs[ch].lagged.Load() {
		t.Fatal("пометка «отстал» обязана сниматься и когда догонять нечего")
	}
}

// sent из старого epoch нельзя сравнивать с новой абсолютной шкалой: большое
// старое число иначе выглядит как «уже догнал» и весь новый replay теряется.
func TestResyncForeignEpochReturnsCleanNewStreamTail(t *testing.T) {
	s := &Session{epoch: "new-epoch", subs: map[chan []byte]*subState{}, dec: newDecTracker()}
	ch, _, _, _, _, _ := s.SubscribeResume("", 0)
	defer s.Unsubscribe(ch)
	s.bufMu.Lock()
	s.buf = []byte("NEW-STREAM")
	s.totalBytes = 1000 + uint64(len(s.buf))
	s.subs[ch].lagged.Store(true)
	s.bufMu.Unlock()

	payload, offset, gap, epoch := s.ResyncFrom(ch, 9_000_000, "old-epoch")
	if string(payload) != "NEW-STREAM" || offset != 1010 || gap || epoch != "new-epoch" {
		t.Fatalf("foreign epoch resync: payload=%q offset=%d gap=%v epoch=%q", payload, offset, gap, epoch)
	}
}
