package update

// Отложенный рестарт: новый бинарник уже на диске (Apply прошёл), но процесс
// ещё старый, потому что открыто окно панели — его не выдёргиваем из-под
// пользователя. Состояние живёт здесь, а не внутри горутины автообновления,
// чтобы панель могла показать «обновление готово» и перезапустить по кнопке:
// иначе о готовом обновлении знал только лог (в проде агент так простоял двое
// суток на v2.24.0 с открытым окном).

import (
	"sync"
	"time"
)

type pendingState struct {
	version string
	since   time.Time
	restart func()
}

var (
	pendingMu sync.Mutex
	pending   *pendingState
)

// SetPending запоминает готовое к применению обновление и способ перезапуска.
// Повторный вызов с более новой версией просто заменяет запись (время ожидания
// считается с первой отложки — обновление всё это время не применялось).
func SetPending(version string, restart func()) {
	pendingMu.Lock()
	defer pendingMu.Unlock()
	since := time.Now()
	if pending != nil {
		since = pending.since
	}
	pending = &pendingState{version: version, since: since, restart: restart}
}

// PendingInfo — версия готового обновления и момент первой отложки.
func PendingInfo() (version string, since time.Time, ok bool) {
	pendingMu.Lock()
	defer pendingMu.Unlock()
	if pending == nil {
		return "", time.Time{}, false
	}
	return pending.version, pending.since, true
}

// RestartPending применяет отложенный рестарт (кнопка в панели). Идемпотентно:
// повторный вызов ничего не делает — иначе два быстрых нажатия породили бы два
// процесса-наследника на одном порту. Сам рестарт — в отдельной горутине, чтобы
// HTTP-ответ успел уйти до os.Exit внутри Restart.
func RestartPending() bool {
	pendingMu.Lock()
	p := pending
	pending = nil
	pendingMu.Unlock()
	if p == nil {
		return false
	}
	go p.restart()
	return true
}
