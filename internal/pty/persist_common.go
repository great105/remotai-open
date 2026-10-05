package pty

import (
	"log"
	"time"
)

// hostClient is a connected persistent-host client (remotai side): a ptyConn
// plus the protocol version and host-observed DEC modes needed at reattach.
// Implemented by *pipeClient (Windows) and *sockClient (Linux); returned by the
// per-platform dialHost.
type hostClient interface {
	ptyConn
	protoVersion() uint16
	hostModes() []int
}

func reattachModes(c ptyConn, fallback []int) []int {
	modes := hostModesOf(c)
	if state, ok := streamStateOf(c); ok && state.Epoch != "" && state.ReplayStart <= state.Produced {
		// Stream-aware Hello is a current, atomic host snapshot. Its empty list
		// is authoritative "no active modes" (for example ?1049l happened while
		// the agent was offline), not absence of the optional capability.
		return modes
	}
	if len(modes) == 0 {
		// A legacy host cannot distinguish unsupported modes from an empty mode
		// set, so preserve the old pty.json fallback during rolling upgrades.
		return fallback
	}
	return modes
}

// busyRetryDelays — когда труба хоста занята другим экземпляром агента, ждать
// его ухода стоит: чаще всего это накладка при автообновлении (новый процесс
// стартовал, старый ещё не отпустил трубы) и длится секунды.
var busyRetryDelays = []time.Duration{3 * time.Second, 10 * time.Second, 30 * time.Second}

// reattachHosts reconnects to every live host recorded in the store. Dead
// records are forgotten; protocol-incompatible hosts (left by a previous version
// across an auto-update) are left running and untouched. Platform-neutral — the
// transport lives behind dialHost.
func reattachHosts(m *Manager) {
	if m == nil || m.meta == nil {
		return
	}
	var busy []string
	for id, rec := range m.meta.AllHosts() {
		switch attachHost(m, id, rec) {
		case attachBusy:
			busy = append(busy, id)
		case attachGone:
			// Хоста нет — компьютер перезагрузили (или процесс убили). Запись НЕ
			// удаляем: в ней имя, папка и рабочий каталог, а рядом на диске лежит
			// хвост вывода. Раньше здесь стоял Forget, и человек, включив
			// компьютер, встречал пустой список — работа исчезала молча. Теперь
			// терминал ждёт в списке потерянных и продолжается одним нажатием
			// (см. restore.go).
			_ = m.meta.MarkLost(id, time.Now())
		}
	}

	if len(busy) > 0 {
		// Повторяем в фоне: молча пропустить такой терминал нельзя — он не
		// попадёт ни в живые, ни в потерянные, то есть исчезнет с экрана совсем,
		// хотя работает.
		go retryBusyHosts(m, busy)
		// И уборку сирот в этот раз не делаем вовсе: занятая труба означает, что
		// рядом работает ВТОРОЙ экземпляр агента, а у него может быть своё
		// представление о том, какие терминалы существуют.
		return
	}

	// Уборка за прошлыми жизнями: хосты, которых нет ни в одной записи, вернуть
	// в интерфейс нечем — терминала для них не существует ни на экране, ни на
	// диске, а память и процессы они держат (замер 02.08.2026: 4,2 ГБ в пяти
	// таких хостах). Делаем это ПОСЛЕ реаттача, когда все известные id уже
	// разобраны, и только если хранилище действительно прочитано: пустая карта
	// из-за битого файла означала бы «сироты все».
	if !m.meta.Loaded() {
		return
	}
	if n := reapOrphanHosts(m.meta.KnownIDs()); n > 0 {
		log.Printf("[PTY] уборка: снято осиротевших хостов — %d", n)
	}
}

type attachResult int

const (
	attachOK attachResult = iota
	attachGone
	attachBusy
	attachIncompatible
)

// attachHost подключается к одному живому хосту и публикует сессию.
func attachHost(m *Manager, id string, rec hostRecord) attachResult {
	pc, err := dialHost(id, 800*time.Millisecond, 0, 0)
	if err != nil {
		if hostBusyErr(err) {
			// Труба занята — значит хост ЖИВ, просто с ним уже разговаривает
			// другой процесс агента. Помечать такой терминал потерянным нельзя:
			// он работает, и «Продолжить работу» на нём отвечало бы «этот
			// терминал не ждёт восстановления» (живой случай 31.07.2026 — второй
			// экземпляр агента за 23 секунды похоронил все 16 терминалов).
			log.Printf("[PTY] reattach: id=%s канал занят другим экземпляром — хост жив, пробуем позже", id)
			return attachBusy
		}
		log.Printf("[PTY] reattach: id=%s host gone (%v) — терминал ждёт восстановления", id, err)
		return attachGone
	}
	if pc.protoVersion() != ProtocolVersion {
		// Incompatible host from an older/newer binary — don't adopt and
		// don't kill the user's running shell; just detach and keep the
		// record. A future matching restart can re-attach.
		log.Printf("[PTY] reattach: id=%s proto mismatch (host=%d want=%d) — leaving host running", id, pc.protoVersion(), ProtocolVersion)
		_ = pc.Close() // sends Detach, host stays alive
		return attachIncompatible
	}
	sess := newSession(id, rec.CWD, rec.Shell, rec.UID, pc, time.UnixMilli(rec.Created))
	sess.setBornSize(rec.Cols, rec.Rows)
	// Восстановить DEC-режимы, наблюдавшиеся ДО рестарта: включающие байты
	// (?1049h alt-screen и т.п.) видел только прошлый процесс — без этого
	// живой TUI (Claude Code) после автообновления оставался для клиента
	// «обычным буфером» и скролл пальцем молча ломался. Приоритет — набор от
	// самого хоста (HelloMsg.Modes: он видел ВСЕ байты, включая период, пока
	// сервер был мёртв); фолбэк — pty.json ТОЛЬКО для legacy host без
	// абсолютного stream-контракта. Пустой Modes нового Hello означает, что
	// режимы выключены. Сессия ещё не опубликована и readLoop не
	// запущен — лок не нужен.
	modes := reattachModes(pc, m.meta.Get(id).Modes)
	sess.seedModes(modes)
	// Последний применённый размер пережил рестарт в pty.json — зеркало экрана
	// обязано родиться В НЁМ, а не в born: сам PTY у хоста так и остался в этом
	// размере (dialHost с 0,0 его не трогает), и реплей кольца печатался под
	// него. Зеркало born-геометрии переваривало 48-колоночный реплей в сетке
	// 80x24 — история приезжала на телефон рваной («кодекс и кими дублируют и
	// рвут», 13.08.2026; в логе — кадры 80x24 при живом PTY 48x31).
	if mt := m.meta.Get(id); mt.ViewCols > 0 && mt.ViewRows > 0 {
		sess.viewMu.Lock()
		sess.viewCols, sess.viewRows = mt.ViewCols, mt.ViewRows
		sess.viewMu.Unlock()
	}
	sess.persistView = func(c, r int) { _ = m.meta.SetViewSize(id, c, r) }
	m.mu.Lock()
	m.sessions[id] = sess
	m.mu.Unlock()
	go sess.readLoop(m)
	log.Printf("[PTY] reattached id=%s host_pid=%d", id, rec.HostPID)
	return attachOK
}

// retryBusyHosts дозванивается до хостов, чья труба была занята, пока они не
// освободятся или пока не кончатся попытки. Терминал, за который так и не
// удалось взяться, помечается потерянным — тогда у человека хотя бы есть
// кнопка «Продолжить работу», а не пустое место.
func retryBusyHosts(m *Manager, ids []string) {
	for _, delay := range busyRetryDelays {
		time.Sleep(delay)
		var still []string
		for _, id := range ids {
			m.mu.Lock()
			_, live := m.sessions[id]
			m.mu.Unlock()
			if live {
				continue // успел подключиться другим путём
			}
			rec, ok := m.meta.AllHosts()[id]
			if !ok {
				continue // запись исчезла (терминал закрыли) — больше не наше дело
			}
			if attachHost(m, id, rec) == attachBusy {
				still = append(still, id)
			}
		}
		if len(still) == 0 {
			return
		}
		ids = still
	}
	for _, id := range ids {
		log.Printf("[PTY] reattach: id=%s канал так и не освободился — терминал ждёт восстановления", id)
		_ = m.meta.MarkLost(id, time.Now())
	}
}
