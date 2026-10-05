package relayhub

import (
	"testing"
	"time"
)

// Присутствие считается по числу живых каналов: телефон может держать
// одновременно control-WS и открытый терминал, и уход одного из них не должен
// означать «человек вышел из приложения».
func TestClientPresenceCounts(t *testing.T) {
	h := NewHub()

	if h.ClientActive("dev1", 7) {
		t.Fatalf("свежий хаб не должен считать клиента активным")
	}
	if away, active := h.ClientAwayFor("dev1", 7); active || away != AwayForever {
		t.Fatalf("ни разу не виденный клиент: away=%v active=%v, ждали AwayForever/false", away, active)
	}

	detach1 := h.ClientAttach("dev1", 7)
	detach2 := h.ClientAttach("dev1", 7)
	if !h.ClientActive("dev1", 7) {
		t.Fatalf("после attach клиент должен быть активен")
	}

	detach1()
	if !h.ClientActive("dev1", 7) {
		t.Fatalf("второй канал ещё жив — клиент активен")
	}
	// Идемпотентность: повторный detach не должен уводить счётчик в минус.
	detach1()
	if !h.ClientActive("dev1", 7) {
		t.Fatalf("повторный detach уронил присутствие")
	}

	detach2()
	if h.ClientActive("dev1", 7) {
		t.Fatalf("после последнего detach клиента быть не должно")
	}
	away, active := h.ClientAwayFor("dev1", 7)
	if active {
		t.Fatalf("active=true после ухода")
	}
	if away > time.Second {
		t.Fatalf("away=%v — ждали «только что ушёл»", away)
	}
}

// Присутствие ключуется парой (device,user): к одному ПК допускают несколько
// аккаунтов, и открытое приложение одного не должно глушить уведомление другому.
func TestClientPresenceIsPerUser(t *testing.T) {
	h := NewHub()
	detach := h.ClientAttach("dev1", 7)
	defer detach()

	if h.ClientActive("dev1", 8) {
		t.Fatalf("присутствие пользователя 7 не должно засчитываться пользователю 8")
	}
	if h.ClientActive("dev2", 7) {
		t.Fatalf("присутствие на dev1 не должно засчитываться на dev2")
	}
}

func TestPrunePresence(t *testing.T) {
	h := NewHub()
	h.ClientAttach("dev1", 7)() // сразу ушёл
	h.ClientAttach("dev2", 7)()

	if n := h.PrunePresence(time.Hour); n != 0 {
		t.Fatalf("свежие отметки не должны чиститься, удалено %d", n)
	}
	// Сдвигаем «когда ушёл» в прошлое.
	h.presMu.Lock()
	for k := range h.lastGone {
		h.lastGone[k] = time.Now().Add(-2 * time.Hour)
	}
	h.presMu.Unlock()

	if n := h.PrunePresence(time.Hour); n != 2 {
		t.Fatalf("удалено %d записей, ждали 2", n)
	}
	if away, _ := h.ClientAwayFor("dev1", 7); away != AwayForever {
		t.Fatalf("после чистки пара должна выглядеть невиданной, away=%v", away)
	}
}

// Подписка на УЖЕ закрытое соединение не должна ронять процесс: Close()
// обнуляет карту подписчиков, и раньше запись в неё паниковала.
func TestSubscribeAfterCloseDoesNotPanic(t *testing.T) {
	a := newAgentConn("dev1", 7, "sess", nil)
	a.closeOnce.Do(func() {
		close(a.done)
		a.eventMu.Lock()
		a.eventSubs = nil
		a.eventMu.Unlock()
	})

	ch := a.SubscribeEvents(4)
	if _, ok := <-ch; ok {
		t.Fatalf("канал подписки на закрытом соединении должен быть закрыт")
	}
	a.UnsubscribeEvents(ch) // не должно паниковать двойным close
}
