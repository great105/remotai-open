package pty

import "testing"

// drain снимает подписку, чтобы тестовый канал не повисал.
func drain(s *Session, ch chan []byte) { s.Unsubscribe(ch) }

func TestSubscribeResume_FirstConnect(t *testing.T) {
	s := &Session{epoch: "ep1", buf: []byte("hello"), totalBytes: 5, subs: map[chan []byte]*subState{}}
	ch, payload, offset, epoch, isDelta, gap := s.SubscribeResume("", 0)
	defer drain(s, ch)
	if isDelta || gap {
		t.Fatal("first connect must be full (isDelta=false, gap=false)")
	}
	if string(payload) != "hello" || offset != 5 || epoch != "ep1" {
		t.Fatalf("payload=%q offset=%d epoch=%q", payload, offset, epoch)
	}
}

func TestSubscribeResume_Delta(t *testing.T) {
	s := &Session{epoch: "ep1", buf: []byte("hello"), totalBytes: 5, subs: map[chan []byte]*subState{}}
	ch, payload, offset, _, isDelta, gap := s.SubscribeResume("ep1", 2)
	defer drain(s, ch)
	if !isDelta || gap {
		t.Fatalf("valid resume must be delta without gap; isDelta=%v gap=%v", isDelta, gap)
	}
	if string(payload) != "llo" || offset != 5 {
		t.Fatalf("delta payload=%q offset=%d, want llo/5", payload, offset)
	}
}

func TestSubscribeResume_NoNewData(t *testing.T) {
	// offset == total: клиент уже всё видел — отдаём пустую дельту, без редрава.
	s := &Session{epoch: "ep1", buf: []byte("hello"), totalBytes: 5, subs: map[chan []byte]*subState{}}
	ch, payload, _, _, isDelta, gap := s.SubscribeResume("ep1", 5)
	defer drain(s, ch)
	if !isDelta || gap || len(payload) != 0 {
		t.Fatalf("expected empty delta, got isDelta=%v gap=%v len=%d", isDelta, gap, len(payload))
	}
}

func TestSubscribeResume_EpochMismatch(t *testing.T) {
	// Чужой epoch (например после рестарта remotai) → полный буфер, не склейка.
	s := &Session{epoch: "ep2", buf: []byte("hello"), totalBytes: 5, subs: map[chan []byte]*subState{}}
	ch, payload, _, _, isDelta, gap := s.SubscribeResume("ep1", 2)
	defer drain(s, ch)
	if isDelta || gap || string(payload) != "hello" {
		t.Fatalf("epoch mismatch must be full; isDelta=%v gap=%v payload=%q", isDelta, gap, payload)
	}
}

func TestSubscribeResume_OffsetEvicted(t *testing.T) {
	// Буфер уже прокрутился: всего вывели 1000 байт, в кольце последние 5
	// (bufStart=995). Запрошенный offset=100 выпал из окна. Раньше это был
	// полный reset со стиранием экрана клиента; теперь — дельта с gap: клиент
	// сохраняет свою историю и дописывает кольцо с пометкой «пропуск»
	// (жалоба 2026-07-29: «в Codex скроллится только два экрана»).
	s := &Session{epoch: "ep1", buf: []byte("world"), totalBytes: 1000, subs: map[chan []byte]*subState{}}
	ch, payload, offset, _, isDelta, gap := s.SubscribeResume("ep1", 100)
	defer drain(s, ch)
	if !isDelta || !gap {
		t.Fatalf("evicted offset must be delta+gap; isDelta=%v gap=%v", isDelta, gap)
	}
	if string(payload) != "world" || offset != 1000 {
		t.Fatalf("payload=%q offset=%d, want world/1000", payload, offset)
	}
}

func TestSubscribeResume_OffsetBeyondTotal(t *testing.T) {
	// offset впереди потока — с клиентом что-то не так, честный полный редрав.
	s := &Session{epoch: "ep1", buf: []byte("hello"), totalBytes: 5, subs: map[chan []byte]*subState{}}
	ch, payload, _, _, isDelta, gap := s.SubscribeResume("ep1", 100)
	defer drain(s, ch)
	if isDelta || gap || string(payload) != "hello" {
		t.Fatalf("offset beyond total must be full; isDelta=%v gap=%v payload=%q", isDelta, gap, payload)
	}
}

// Боевой случай 23.09.2026: телефон ушёл, терминал 13 минут смотрели с ПК
// шириной 238, телефон вернулся с resume и проиграл байты, нарисованные под 238
// колонок, в свои 48 — Claude без строки ввода и с пропавшими строками. После
// смены размера resume обязан стать reset: клиент очистит экран и попросит кадр.
func TestSubscribeResume_ResizedSinceClientLeftIsReset(t *testing.T) {
	s, _ := newSizeSession(t, 48, 26)
	s.subs = map[chan []byte]*subState{}
	phone := s.AddViewer()
	if err := s.ResizeFor(phone, 48, 26); err != nil {
		t.Fatal(err)
	}
	s.bufMu.Lock()
	s.epoch, s.buf, s.totalBytes = "ep1", []byte("0123456789"), 10
	s.bufMu.Unlock()
	s.RemoveViewer(phone) // телефон ушёл, его позиция — 10

	pc := s.AddViewer()
	if err := s.ResizeFor(pc, 238, 34); err != nil {
		t.Fatal(err)
	}
	s.bufMu.Lock()
	s.buf, s.totalBytes = append(s.buf, "wide"...), 14
	s.bufMu.Unlock()
	s.RemoveViewer(pc)

	ch, payload, offset, _, isDelta, gap := s.SubscribeResume("ep1", 10)
	defer drain(s, ch)
	if isDelta || gap {
		t.Fatalf("resume across a resize must be reset; isDelta=%v gap=%v", isDelta, gap)
	}
	if string(payload) != "0123456789wide" || offset != 14 {
		t.Fatalf("reset payload=%q offset=%d", payload, offset)
	}

	// Клиент, ушедший ПОСЛЕ смены размера, по-прежнему получает дельту.
	ch2, payload2, _, _, isDelta2, _ := s.SubscribeResume("ep1", 12)
	defer drain(s, ch2)
	if !isDelta2 || string(payload2) != "de" {
		t.Fatalf("resume after the resize must stay delta; isDelta=%v payload=%q", isDelta2, payload2)
	}
}

// Тот же размер (телефон переподключился без смены сетки) reset не вызывает.
func TestSubscribeResume_SameSizeKeepsDelta(t *testing.T) {
	s, _ := newSizeSession(t, 48, 26)
	s.subs = map[chan []byte]*subState{}
	v := s.AddViewer()
	_ = s.ResizeFor(v, 48, 26)
	s.bufMu.Lock()
	s.epoch, s.buf, s.totalBytes = "ep1", []byte("0123456789"), 10
	s.bufMu.Unlock()
	_ = s.ResizeFor(v, 48, 26)
	ch, payload, _, _, isDelta, _ := s.SubscribeResume("ep1", 4)
	defer drain(s, ch)
	if !isDelta || string(payload) != "456789" {
		t.Fatalf("same-size resume must stay delta; isDelta=%v payload=%q", isDelta, payload)
	}
}
