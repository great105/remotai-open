package pty

import "testing"

// Пауза подписчика (ctrl «pause» от клиента) — это НЕ остановка сессии:
// readLoop пишет в кольцо дальше, а в канал поставленного на паузу кадры не
// идут. На «resume» пропущенное до-сылается из кольца тем же путём, что у
// отставшего подписчика (markLagged → ResyncFrom), и склейка «принятое до
// паузы + до-сланное» обязана дать поток байт в байт, без пропусков и дублей.
func TestFlowPauseResumeLosesNothing(t *testing.T) {
	s := &Session{epoch: "e1", subs: map[chan []byte]*subState{}, dec: newDecTracker()}
	ch, _, base, _, _, _ := s.SubscribeResume("", 0)
	defer s.Unsubscribe(ch)

	write := func(text string) {
		s.bufMu.Lock()
		s.buf = append(s.buf, text...)
		s.totalBytes += uint64(len(text))
		s.fanoutLocked([]byte(text))
		s.bufMu.Unlock()
	}

	// Живой кадр до паузы доходит как обычно.
	write("до паузы;")
	d := <-ch
	sent := base + uint64(len(d))

	// На паузе в канал не пишут, но кольцо наполняется.
	s.SetFlowPaused(ch, true)
	write("на паузе;")
	select {
	case d := <-ch:
		t.Fatalf("подписчик на паузе, а в канал пишут: %q", d)
	default:
	}
	if s.subs[ch].lagged.Load() {
		t.Fatal("пауза — не отставание: будильник до-сылки обязан молчать до resume")
	}

	// Второй зритель той же сессии паузой не задет: пауза per-subscriber.
	ch2, _, _, _, _, _ := s.SubscribeResume("", 0)
	defer s.Unsubscribe(ch2)
	write("ещё;")
	select {
	case <-ch2:
	default:
		t.Fatal("пауза одного подписчика остановила вывод другому")
	}
	select {
	case d := <-ch:
		t.Fatalf("подписчик всё ещё на паузе, а в канал пишут: %q", d)
	default:
	}

	// Resume: писатель будится существующим будильником отставания и до-сылает
	// пропущенное из кольца с позиции sent.
	s.SetFlowPaused(ch, false)
	if !s.subs[ch].lagged.Load() {
		t.Fatal("разпауза обязана вести в существующий путь до-сылки (lagged)")
	}
	payload, offset, gap, epoch := s.ResyncFrom(ch, sent, "e1")
	if gap {
		t.Fatal("кольцо вмещает всё пропущенное — пропуска быть не должно")
	}
	if got, want := string(payload), "на паузе;ещё;"; got != want {
		t.Fatalf("до-сылка после паузы: получено %q, ожидалось %q", got, want)
	}
	if offset != s.totalBytes {
		t.Fatalf("позиция после до-сылки %d, ожидалась %d", offset, s.totalBytes)
	}
	if epoch != "e1" {
		t.Fatalf("resync epoch=%q, want e1", epoch)
	}

	// И связь продолжает работать: после разпаузы кадры снова идут в канал.
	write("после;")
	select {
	case d := <-ch:
		if string(d) != "после;" {
			t.Fatalf("после разпаузы пришло %q, ожидалось \"после;\"", d)
		}
	default:
		t.Fatal("после разпаузы живой вывод в канал не пошёл")
	}
}

// Холостые и повторные сигналы: «resume» без паузы не будит писателя (лишний
// маркер resumed клиенту не уезжает), повторный «pause» идемпотентен, отписка
// на паузе — обычная уборка без утечек, сигнал в никуда безопасен.
func TestFlowPauseSpuriousSignals(t *testing.T) {
	s := &Session{epoch: "e1", subs: map[chan []byte]*subState{}, dec: newDecTracker()}
	ch, _, _, _, _, _ := s.SubscribeResume("", 0)

	st := s.subs[ch]
	s.SetFlowPaused(ch, false)
	if st.lagged.Load() {
		t.Fatal("resume без паузы взвёл до-сылку")
	}

	s.SetFlowPaused(ch, true)
	s.SetFlowPaused(ch, true)
	if !st.paused.Load() {
		t.Fatal("пауза не поставлена")
	}

	// Подписчик отвалился в состоянии paused: обычная уборка, а запоздавший
	// resume в пустоту не паникует.
	s.Unsubscribe(ch)
	s.SetFlowPaused(ch, false)
	if _, ok := s.subs[ch]; ok {
		t.Fatal("подписчик на паузе не убран")
	}
}
