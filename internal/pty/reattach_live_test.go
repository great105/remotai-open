package pty

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// storeManager — менеджер с хранилищем во временной папке и БЕЗ фоновых петель:
// тестам нужна только логика решения, а не reapLoop со scrollbackLoop.
func storeManager(t *testing.T) *Manager {
	t.Helper()
	return &Manager{
		sessions: make(map[string]*Session),
		meta:     NewMetaStoreAt(filepath.Join(t.TempDir(), "pty.json")),
	}
}

// ГЛАВНАЯ РЕГРЕССИЯ. Неудачный дозвон не имеет права означать «терминала нет».
//
// Живой случай владельца 17.08.2026, 11:58:07: человек нажал «Вернуть связь» на
// терминале «Клубная», продукт ответил `процесс терминала больше не отвечает`
// → HTTP 404 → тост «Не найдено.». В этот момент host_pid=18052 был жив, держал
// свою трубу, и внутри него работал Claude Code со всеми MCP-серверами. Из
// оставшихся на карточке действий человеку предлагалось «Открыть в этой папке»
// — то есть завести ВТОРОЙ терминал поверх работающего первого.
//
// На старом коде тест падает: там любая ошибка дозвона превращалась в errHostGone.
func TestReattachSessionDoesNotBuryLiveHost(t *testing.T) {
	m := storeManager(t)
	// Хост, до которого не дозвониться (трубы с таким именем нет), но чей
	// ПРОЦЕСС заведомо жив — берём процесс самого теста.
	if err := m.meta.PutHost("live", hostRecord{
		HostPID:  uint32(os.Getpid()),
		PipeName: pipeName("live"),
		CWD:      t.TempDir(),
		Shell:    "shell",
		Created:  time.Now().UnixMilli(),
	}); err != nil {
		t.Fatal(err)
	}

	_, err := m.ReattachSession("live")
	if err == nil {
		t.Fatal("дозваниваться было некуда — ошибка обязана быть")
	}
	if errors.Is(err, errHostGone) {
		t.Fatal("живой хост объявлен мёртвым: ровно так на «Вернуть связь» приходило 404 и «Не найдено.» о работающем терминале")
	}
	if !IsHostBusy(err) {
		t.Fatalf("ожидали errHostBusy (жив, но не отозвался), получили: %v", err)
	}
}

// Обратная сторона: починка не должна превращать ЛЮБОЙ отказ в «жив, повторите».
// Терминал, чьего процесса действительно нет, обязан честно называться мёртвым —
// иначе карточка вечно обещает возврат связи, которого не будет.
func TestReattachSessionStillReportsTrulyDeadHost(t *testing.T) {
	m := storeManager(t)
	// HostPID=0 — «процесса нет» по построению (см. processAlive).
	if err := m.meta.PutHost("dead", hostRecord{
		HostPID:  0,
		PipeName: pipeName("dead"),
		CWD:      t.TempDir(),
		Shell:    "shell",
		Created:  time.Now().UnixMilli(),
	}); err != nil {
		t.Fatal(err)
	}

	_, err := m.ReattachSession("dead")
	if !errors.Is(err, errHostGone) {
		t.Fatalf("мёртвый хост обязан называться мёртвым, получили: %v", err)
	}
	if IsHostBusy(err) {
		t.Fatal("мёртвый хост выдан за занятый — карточка будет вечно обещать возврат связи")
	}
}

// Живой процесс — доказательство жизни хоста, даже когда дозвон не прошёл.
// Это и есть разница между «не дозвонились» и «его нет».
func TestHostAliveAfterFailedDialTrustsLiveProcess(t *testing.T) {
	rec := &hostRecord{HostPID: uint32(os.Getpid())}
	alive, why := hostAliveAfterFailedDial(rec, errors.New("любая ошибка дозвона"))
	if !alive {
		t.Fatalf("процесс теста жив, но хост признан мёртвым (%s)", why)
	}
	if why != "процесс жив" {
		t.Fatalf("причина обязана попадать в лог понятной, получили %q", why)
	}

	dead, why := hostAliveAfterFailedDial(&hostRecord{HostPID: 0}, errors.New("любая ошибка дозвона"))
	if dead {
		t.Fatalf("хоста нет, но он признан живым (%s)", why)
	}
	// Отсутствие записи — не повод считать хост живым: у SSH и локального
	// ConPTY хоста нет вовсе, и их обрыв действительно конец.
	if alive, _ := hostAliveAfterFailedDial(nil, errors.New("любая ошибка дозвона")); alive {
		t.Fatal("сессия без host-записи признана живым хостом")
	}
}

// Фоновый возврат связи обязан обходить стороной три случая, и каждый из них
// стоил бы дорого: терминал после перезагрузки ПК ждёт Restore (нового
// процесса), а не связи со старым; живой сессии возвращать нечего; закрытую
// человеком возвращать нельзя.
func TestNeedsRelinkSkipsWhatMustNotBeRevived(t *testing.T) {
	m := storeManager(t)
	rec := hostRecord{HostPID: uint32(os.Getpid()), PipeName: pipeName("x"), CWD: t.TempDir(), Shell: "shell", Created: 1}

	if err := m.meta.PutHost("orphan", rec); err != nil {
		t.Fatal(err)
	}
	if !m.needsRelink("orphan") {
		t.Fatal("сессию прибрал reapLoop, а запись хоста осталась — это ровно тот случай, ради которого relink и заведён")
	}

	if err := m.meta.PutHost("rebooted", rec); err != nil {
		t.Fatal(err)
	}
	if err := m.meta.MarkLost("rebooted", time.Now()); err != nil {
		t.Fatal(err)
	}
	if m.needsRelink("rebooted") {
		t.Fatal("терминал после перезагрузки ПК ждёт Restore, а не возврата связи с несуществующим процессом")
	}
}
