package vbrowser

import (
	"bytes"
	"testing"
	"time"
)

// TestFrameChunker проверяет склейку потока байт в кадры по 20 мс: точные
// границы, полтора кадра за чтение, побайтная подача и неполный хвост.
func TestFrameChunker(t *testing.T) {
	collect := func(chunks [][]byte) [][]byte {
		var c frameChunker
		var out [][]byte
		for _, ch := range chunks {
			c.feed(ch, func(f []byte) {
				cp := make([]byte, len(f))
				copy(cp, f)
				out = append(out, cp)
			})
		}
		return out
	}

	frame := func(fill byte) []byte { return bytes.Repeat([]byte{fill}, audioFrameBytes) }

	// Ровно два кадра одним куском.
	if got := collect([][]byte{append(frame(1), frame(2)...)}); len(got) != 2 {
		t.Fatalf("two exact frames: got %d", len(got))
	}
	// Полтора кадра: второй доклеивается следующим чтением.
	oneAndHalf := append(frame(3), frame(4)[:100]...)
	got := collect([][]byte{oneAndHalf, frame(4)[100:]})
	if len(got) != 2 || got[0][0] != 3 || got[1][0] != 4 {
		t.Fatalf("1.5-frame split: got %d frames", len(got))
	}
	// Побайтная подача трёх кадров.
	var stream []byte
	for i := byte(0); i < 3; i++ {
		stream = append(stream, frame(i+10)...)
	}
	var trickle [][]byte
	for i := range stream {
		trickle = append(trickle, stream[i:i+1])
	}
	if got := collect(trickle); len(got) != 3 {
		t.Fatalf("byte-wise feed: got %d frames", len(got))
	}
	// Неполный хвост молча ждёт продолжения (и не теряется в середине).
	got = collect([][]byte{frame(5)[:audioFrameBytes-7]})
	if len(got) != 0 {
		t.Fatalf("incomplete tail must not emit: got %d", len(got))
	}
}

// TestAudioRestartDecision — бюджет перезапусков parec: 3 попытки с backoff
// 1s→2s→4s, устойчивая жизнь сбрасывает счётчик.
func TestAudioRestartDecision(t *testing.T) {
	fails, delay, ok := audioRestartDecision(0, false)
	if fails != 1 || delay != time.Second || !ok {
		t.Fatalf("first: %d %s %v", fails, delay, ok)
	}
	if _, delay, ok = audioRestartDecision(1, false); delay != 2*time.Second || !ok {
		t.Fatalf("second: %s %v", delay, ok)
	}
	if fails, delay, ok = audioRestartDecision(2, false); fails != 3 || delay != 4*time.Second || !ok {
		t.Fatalf("third: %d %s %v", fails, delay, ok)
	}
	if _, _, ok = audioRestartDecision(3, false); ok {
		t.Fatalf("budget of %d must be exhausted", audioRestartMaxFails)
	}
	if fails, _, ok = audioRestartDecision(3, true); fails != 1 || !ok {
		t.Fatalf("stable process resets the budget: %d %v", fails, ok)
	}
}

// hubTestPump — фейковый захват: сообщает о старте/остановке и гонит пакеты,
// которые тест шлёт в emit. Каждый emit подтверждается ПОСЛЕ broadcast —
// иначе тест не знает, когда буферы слушателей устаканились (гонка на дропах).
type hubTestPump struct {
	started chan struct{}
	stopped chan struct{}
	emit    chan emitReq
}

type emitReq struct {
	pkt []byte
	ack chan struct{}
}

func newHubTestPump() *hubTestPump {
	return &hubTestPump{
		started: make(chan struct{}, 4),
		stopped: make(chan struct{}, 4),
		emit:    make(chan emitReq),
	}
}

func (p *hubTestPump) run(broadcast func([]byte), stop <-chan struct{}) {
	p.started <- struct{}{}
	for {
		select {
		case req := <-p.emit:
			broadcast(req.pkt)
			close(req.ack)
		case <-stop:
			p.stopped <- struct{}{}
			return
		}
	}
}

// emitSync прогоняет пакет через broadcast и ждёт завершения раздачи.
func (p *hubTestPump) emitSync(pkt []byte) {
	ack := make(chan struct{})
	p.emit <- emitReq{pkt: pkt, ack: ack}
	<-ack
}

func recvPkt(t *testing.T, ch <-chan []byte) []byte {
	t.Helper()
	select {
	case pkt := <-ch:
		return pkt
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for packet")
		return nil
	}
}

// TestAudioHubFanout — подписка/отписка, ленивый старт на первом слушателе,
// остановка на последнем и повторный старт на новом.
func TestAudioHubFanout(t *testing.T) {
	p := newHubTestPump()
	h := newAudioHub(p.run)

	// Без слушателей захват не работает.
	select {
	case <-p.started:
		t.Fatal("pump must not run without subscribers")
	case <-time.After(50 * time.Millisecond):
	}

	ch1, un1 := h.subscribe()
	<-p.started
	if !h.active() {
		t.Fatal("hub must be active with a subscriber")
	}
	ch2, un2 := h.subscribe()

	p.emitSync([]byte{0xAA})
	if pkt := recvPkt(t, ch1); !bytes.Equal(pkt, []byte{0xAA}) {
		t.Fatalf("sub1 got %x", pkt)
	}
	if pkt := recvPkt(t, ch2); !bytes.Equal(pkt, []byte{0xAA}) {
		t.Fatalf("sub2 got %x", pkt)
	}

	// Первый ушёл — захват жив ради второго.
	un1()
	select {
	case <-p.stopped:
		t.Fatal("pump must survive while a subscriber remains")
	case <-time.After(50 * time.Millisecond):
	}
	p.emitSync([]byte{0xBB})
	recvPkt(t, ch2)

	// Последний ушёл — захват гаснет.
	un2()
	<-p.stopped
	if h.active() {
		t.Fatal("hub must be idle after the last unsubscribe")
	}

	// Новый слушатель поднимает захват заново.
	ch3, un3 := h.subscribe()
	<-p.started
	p.emitSync([]byte{0xCC})
	recvPkt(t, ch3)
	un3()
	<-p.stopped

	// Двойная отписка безопасна.
	un3()
}

// TestAudioHubOverflow — буфер слушателя на ~1с; медленный зритель получает
// дропы, а не блокировку всего потока.
func TestAudioHubOverflow(t *testing.T) {
	p := newHubTestPump()
	h := newAudioHub(p.run)
	ch, un := h.subscribe()
	defer un()
	<-p.started

	total := audioSubBufferSize + 25
	for i := 0; i < total; i++ {
		p.emitSync([]byte{byte(i)})
	}
	// Раздача завершена синхронно: в буфере ровно audioSubBufferSize пакетов,
	// остальные дропнулись, не заблокировав pump.
	got := 0
	for {
		select {
		case <-ch:
			got++
		case <-time.After(200 * time.Millisecond):
			if got != audioSubBufferSize {
				t.Fatalf("got %d packets, want buffer-full %d", got, audioSubBufferSize)
			}
			return
		}
	}
}

// TestAudioHubStopAll — Stop сессии снимает всех слушателей и гасит захват.
func TestAudioHubStopAll(t *testing.T) {
	p := newHubTestPump()
	h := newAudioHub(p.run)
	_, un1 := h.subscribe()
	_, un2 := h.subscribe()
	<-p.started
	_ = un1
	_ = un2 // отписок после stopAll не ломаются

	h.stopAll()
	<-p.stopped
	if h.active() {
		t.Fatal("stopAll must detach everyone")
	}
	un1()
	un2()
}

// TestAudioHubPumpDeathRestart — померший сам по себе захват (бюджет parec
// исчерпан) помечается мёртвым: следующий слушатель поднимает его заново,
// а не присоединяется к тишине.
func TestAudioHubPumpDeathRestart(t *testing.T) {
	started := make(chan struct{}, 4)
	release := make(chan struct{})
	h := newAudioHub(func(broadcast func([]byte), stop <-chan struct{}) {
		started <- struct{}{}
		<-release // живёт, пока тест не «убьёт» его
	})
	_, un1 := h.subscribe()
	<-started
	close(release)

	deadline := time.Now().Add(2 * time.Second)
	for {
		h.mu.Lock()
		running := h.running
		h.mu.Unlock()
		if !running {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("a dead pump must flip running=false")
		}
		time.Sleep(5 * time.Millisecond)
	}

	_, un2 := h.subscribe()
	<-started // захват поднялся заново
	un1()
	un2()
}
