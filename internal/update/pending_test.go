package update

import (
	"testing"
	"time"
)

func TestPendingLifecycle(t *testing.T) {
	t.Cleanup(func() { pendingMu.Lock(); pending = nil; pendingMu.Unlock() })

	if _, _, ok := PendingInfo(); ok {
		t.Fatal("на старте отложенного обновления быть не должно")
	}
	if RestartPending() {
		t.Fatal("RestartPending без pending должен возвращать false")
	}

	SetPending("2.26.0", func() {})
	v, since, ok := PendingInfo()
	if !ok || v != "2.26.0" {
		t.Fatalf("PendingInfo() = %q, %v; хотели 2.26.0", v, ok)
	}
	first := since

	// Версия поверх отложенной: время ожидания считается с ПЕРВОЙ отложки —
	// иначе новая версия каждые 6 часов обнуляла бы лимит и рестарт снова
	// не наступал бы никогда.
	time.Sleep(2 * time.Millisecond)
	SetPending("2.27.0", func() {})
	v, since, _ = PendingInfo()
	if v != "2.27.0" {
		t.Fatalf("версия после SetPending = %q; хотели 2.27.0", v)
	}
	if !since.Equal(first) {
		t.Fatalf("время ожидания сбросилось: было %v, стало %v", first, since)
	}

	// Рестарт одноразовый: два нажатия кнопки не должны родить два процесса.
	done := make(chan struct{})
	SetPending("2.27.0", func() { close(done) })
	if !RestartPending() {
		t.Fatal("первый RestartPending должен вернуть true")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("restart-колбэк не вызван")
	}
	if RestartPending() {
		t.Fatal("повторный RestartPending должен вернуть false")
	}
	if _, _, ok := PendingInfo(); ok {
		t.Fatal("после рестарта pending должен быть снят")
	}
}
