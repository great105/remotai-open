package pty

import (
	"bytes"
	"testing"
	"time"
)

// Снапшот больше предела кадра обязан доезжать до клиента.
//
// Живой случай владельца (2026-07-30): буфер терминала подняли до 4 МБ, а
// предел одного кадра остался 1 МБ. Хост не мог отправить снапшот, рвал
// соединение, агент переподключался — и так 252 тысячи раз за восемь минут,
// после чего терминал объявлялся завершённым при живом процессе. Тест держит
// оба числа в согласии: буфер вправе расти, кусок — обязан оставаться в пределе
// кадра.
func TestSnapshotChunkFitsFrameLimit(t *testing.T) {
	if snapshotChunk > maxFrame {
		t.Fatalf("кусок снапшота %d больше предела кадра %d — отправка упадёт", snapshotChunk, maxFrame)
	}
	if scrollbackSize <= maxFrame {
		// Не ошибка, но тогда чанкование не проверяется на живом размере: пусть
		// автор изменения увидит, что защита стала бессмысленной.
		t.Logf("буфер терминала %d не больше кадра %d — чанкование пока не нужно", scrollbackSize, maxFrame)
	}
}

// Сборку многокадрового снапшота проверяют платформенные файлы рядом
// (snapshot_chunk_windows_test.go / snapshot_chunk_unix_test.go): декодер живёт
// в клиенте транспорта, а он у Windows и Unix свой. Общая часть — ниже.

// chunkedSnapshotWire — тестовая «проволока»: снапшот, нарезанный на кадры
// ровно так, как это делает хост.
func chunkedSnapshotWire(t *testing.T) (*bytes.Buffer, []byte) {
	t.Helper()
	full := bytes.Repeat([]byte("A"), snapshotChunk+1024)
	var wire bytes.Buffer
	for rest := full; len(rest) > 0; {
		chunk := rest
		if len(chunk) > snapshotChunk {
			chunk = chunk[:snapshotChunk]
		}
		if err := writeFrame(&wire, frSnapshot, chunk); err != nil {
			t.Fatalf("запись куска: %v", err)
		}
		rest = rest[len(chunk):]
	}
	return &wire, full
}

// assertAssembled — общий приговор для обеих платформ.
func assertAssembled(t *testing.T, read func([]byte) (int, error), full []byte) {
	t.Helper()
	got := make([]byte, 0, len(full))
	buf := make([]byte, 4096)
	for len(got) < len(full) {
		n, err := read(buf)
		if err != nil {
			t.Fatalf("чтение снапшота: %v (собрано %d из %d)", err, len(got), len(full))
		}
		got = append(got, buf[:n]...)
	}
	if !bytes.Equal(got, full) {
		t.Fatalf("снапшот собран неверно: %d байт вместо %d", len(got), len(full))
	}
}

// Восстановление связи не должно превращаться в горячий цикл: если после
// каждого успешного переподключения связь рвётся сразу, серия обязана
// оборваться — иначе агент заваливает хост тысячами подключений (и убивает
// терминал, который пытался спасти).
func TestReattachBurstStopsHotLoop(t *testing.T) {
	s := &Session{ID: "burst"}
	allowed := 0
	for i := 0; i < reattachLiveBurst+3; i++ {
		if s.noteReattachAttempt() {
			allowed++
			continue
		}
		break
	}
	if allowed != reattachLiveBurst {
		t.Fatalf("разрешено %d восстановлений подряд, ожидалось %d", allowed, reattachLiveBurst)
	}
	if s.noteReattachAttempt() {
		t.Fatal("серия должна оставаться закрытой, пока связь не удержалась")
	}

	// МГНОВЕННЫЕ данные серию НЕ закрывают. Первое, что отдаёт pty-host любому
	// подключившемуся, — снапшот своего буфера: байты есть всегда, и пока
	// noteReattachProgress обнулял счётчик на любой из них, потолок не
	// достигался никогда — сторож горячего цикла был декоративным.
	s.noteReattachProgress()
	if s.noteReattachAttempt() {
		t.Fatal("снапшот, приехавший мгновенно, не должен считаться живой связью")
	}

	// А связь, прожившая дольше reattachBurstReset, — закрывает: это уже работа
	// терминала, а не круг цикла.
	s.reattachMu.Lock()
	s.reattachAt = time.Now().Add(-reattachBurstReset - time.Second)
	s.reattachMu.Unlock()
	s.noteReattachProgress()
	if !s.noteReattachAttempt() {
		t.Fatal("после удержавшейся связи восстановление обязано снова разрешаться")
	}
}

// Пауза внутри серии есть, но она не превращает первую попытку в ожидание:
// обычный одиночный обрыв должен восстанавливаться мгновенно.
func TestFirstReattachIsImmediate(t *testing.T) {
	s := &Session{ID: "fast"}
	start := time.Now()
	if !s.noteReattachAttempt() {
		t.Fatal("первая попытка обязана быть разрешена")
	}
	if elapsed := time.Since(start); elapsed > 200*time.Millisecond {
		t.Fatalf("первая попытка ждала %s — обрыв должен лечиться сразу", elapsed)
	}
}

// Клиенту при открытии терминала уходит ХВОСТ истории, а не все четыре
// мегабайта буфера.
//
// Живая жалоба (2026-07-30, второй пользователь): «терминал явно работает, но
// никакого вывода нет». Причина — 4 МБ ANSI, которые едут через облако и потом
// разбираются xterm.js на телефоне: пока это длится, экран пуст.
func TestClientGetsTailNotWholeBuffer(t *testing.T) {
	s := &Session{ID: "tail", epoch: "e1", subs: map[chan []byte]*subState{}}
	s.buf = make([]byte, scrollbackSize)
	for i := range s.buf {
		s.buf[i] = byte('a' + i%26)
	}
	s.totalBytes = uint64(len(s.buf))

	ch, payload, offset, epoch, isDelta, gap := s.SubscribeResume("", 0)
	defer s.Unsubscribe(ch)
	if len(payload) != clientReplayLimit {
		t.Fatalf("клиенту ушло %d байт, ожидался хвост %d", len(payload), clientReplayLimit)
	}
	if isDelta || gap {
		t.Fatalf("первое открытие — не дельта и не пропуск: isDelta=%v gap=%v", isDelta, gap)
	}
	if offset != s.totalBytes || epoch != "e1" {
		t.Fatalf("offset/epoch сбиты: %d %q", offset, epoch)
	}
	if want := s.buf[len(s.buf)-clientReplayLimit:]; string(payload) != string(want) {
		t.Fatal("ушёл не хвост буфера")
	}

	// Долгое отсутствие: клиент пропустил больше лимита — отдаём хвост и честно
	// помечаем пропуск, иначе человек получит «свежую» историю без предупреждения.
	_, big, _, _, isDelta2, gap2 := s.SubscribeResume("e1", 0)
	if len(big) != clientReplayLimit || !isDelta2 || !gap2 {
		t.Fatalf("дельта после долгого обрыва: %d байт, isDelta=%v gap=%v", len(big), isDelta2, gap2)
	}

	// Короткий обрыв: пропущенное меньше лимита — отдаём ровно его, без пометки.
	_, small, _, _, isDelta3, gap3 := s.SubscribeResume("e1", s.totalBytes-1000)
	if len(small) != 1000 || !isDelta3 || gap3 {
		t.Fatalf("короткая дельта: %d байт, isDelta=%v gap=%v", len(small), isDelta3, gap3)
	}
}
