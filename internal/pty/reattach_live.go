package pty

import (
	"errors"
	"log"
	"time"
)

// Причины отказа «переподключить» — клиент по ним выбирает текст.
var (
	errReattachUnavailable = errors.New("переподключение недоступно")
	errNoHostRecord        = errors.New("этот терминал не жил в отдельном процессе — переподключаться не к чему")
	errHostGone            = errors.New("процесс терминала больше не отвечает")
	errHostProtoMismatch   = errors.New("процесс терминала от другой версии Remotai")
	// errHostBusy — хост ЖИВ, но сейчас не отдал трубу. Это не отказ, а
	// «повторите»: разводить его с errHostGone обязательно, иначе на живой
	// работающий терминал человек получает 404 и тост «Не найдено.» (боевой
	// случай 17.08.2026 — см. reattachLive ниже).
	errHostBusy = errors.New("терминал жив, но занят — повторите через несколько секунд")
)

// IsHostBusy отличает «хост жив, но занят» от «хоста нет». Нужно веб-слою:
// это разные ответы человеку и разные HTTP-коды (409 против 404).
func IsHostBusy(err error) bool { return errors.Is(err, errHostBusy) }

// hostAliveAfterFailedDial — есть ли ДОКАЗАТЕЛЬСТВО, что хост ещё жив, когда
// дозвон не удался. Вторым значением — чем именно доказано, для лога.
//
// ⚠ ГЛАВНОЕ ПРАВИЛО ЭТОГО ФАЙЛА: неудача дозвона НЕ является доказательством
// смерти. Доказательства жизни два, и оба дешёвые:
//
//   - занятая труба — сказать «занято» может только живой хост
//     (nMaxInstances=1, см. hostBusyErr);
//   - живой процесс по записанному host_pid (см. processAlive).
//
// Пока хоть одно держится, терминал хоронить нельзя: внутри него идёт работа.
func hostAliveAfterFailedDial(rec *hostRecord, dialErr error) (bool, string) {
	if hostBusyErr(dialErr) {
		return true, "труба занята"
	}
	if rec != nil && processAlive(rec.HostPID) {
		return true, "процесс жив"
	}
	return false, "процесса нет"
}

// Восстановление связи с ЖИВЫМ pty-host прямо во время работы.
//
// Терминал живёт в отдельном процессе (pty-host), а Remotai общается с ним по
// именованному каналу. Канал может оборваться сам по себе — и до этой правки
// такой обрыв означал смерть терминала: readLoop заканчивался, сессия
// становилась «завершённой», а хост со всей работой продолжал жить НЕВИДИМО.
//
// Живой случай (2026-07-27, лог владельца):
//
//	22:29:01 [PTY] reattached id=8dbf3fd97686dd7f host_pid=33436   ← после обновления
//	22:51:09 [PTY] readLoop ended id=8dbf3fd97686dd7f: EOF          ← канал оборвался
//
// В интерфейсе — «завершён · лог ещё 2 мин» и кнопка «Перезапустить в этой
// папке», а в это время процесс 33436 был жив, и внутри него работал Kimi Code
// (powershell → node). Перезапуск создал бы ВТОРОЙ терминал в той же папке,
// пока первый продолжает писать в никуда.
//
// Переподключение уже умело происходить, но только при СТАРТЕ агента
// (reattachHosts). Теперь то же самое делается на лету.
const (
	// Сколько раз пробуем дозвониться до хоста после обрыва. Хост мог быть занят
	// (перезапуск pipe-сервера) — одной попытки мало; но и висеть бесконечно на
	// мёртвом хосте нельзя, иначе «завершённый» терминал никогда не признают
	// завершённым.
	reattachLiveTries = 3
	// Пауза между попытками. Растёт линейно: 300, 600, 900 мс.
	reattachLiveBackoff = 300 * time.Millisecond
	// Таймаут одного дозвона.
	reattachLiveTimeout = 800 * time.Millisecond

	// ── Терпение сверх быстрой серии ──────────────────────────────────
	//
	// Быстрой серии хватает на обычную заминку, но она НЕ доказывает смерть:
	// весь её бюджет — около трёх секунд. Живой случай владельца (17.08.2026,
	// терминал «Клубная»):
	//
	//	11:55:27 [PTY-WS] closed reason=client-gone
	//	11:56:12 [PTY] reattach-live: хост не отозвался за 3 попыток
	//	11:56:12 [PTY] readLoop ended: EOF
	//
	// Приговор был окончательным, а host_pid=18052 жил дальше и держал свою
	// трубу; внутри работал Claude Code. Вернуть сессию можно было только
	// перезапуском приложения — при старте это делает reattachHosts.
	//
	// Поэтому после быстрой серии решает не счётчик попыток, а доказательство
	// жизни хоста (hostAliveAfterFailedDial). Потолок нужен ровно для одного:
	// не держать readLoop бесконечно на молчащем хосте. Когда он исчерпан,
	// терминал честно показывается как «связь потеряна», а возвращает его
	// фоновый relinkLoop — то есть терпение здесь про БЕСШОВНОСТЬ (человек не
	// замечает заминки), а не про сам факт возврата.
	reattachLivePatientEvery = 2 * time.Second
	reattachLivePatientFor   = 30 * time.Second

	// Сколько раз подряд разрешено «восстановить связь», не прочитав после
	// этого НИ ОДНОГО байта, и за какое время такая серия считается серией.
	//
	// Без этого предела восстановление превращается в горячий цикл: связь
	// поднимается, первое же чтение падает, снова поднимается — и так тысячи
	// раз в секунду. Живой случай владельца (2026-07-30): 252 тысячи строк в
	// логе за восемь минут, 35 МБ файла, процессор в потолок и «терминал
	// завершён» при живом хосте. Причина обрыва была своя (снапшот больше
	// кадра, см. host_common.go), но защиту всё равно надо держать здесь:
	// причин у обрыва много, а цикл без тормоза опаснее любой из них.
	reattachLiveBurst  = 4
	reattachBurstReset = 5 * time.Second
	// Пауза перед повторным восстановлением внутри серии: пока связь рвётся
	// сразу, дёргать хост чаще раза в секунду бессмысленно и вредно.
	reattachBurstPause = time.Second
)

// reattachLive пытается подменить оборвавшееся соединение сессии на новое к
// тому же pty-host. Возвращает true, если связь восстановлена и читать можно
// дальше — тогда терминал для человека вообще не «умирал».
func (m *Manager) reattachLive(s *Session) bool {
	if m == nil || s == nil || m.meta == nil {
		return false
	}
	// Закрыл человек — переподключаться некуда и незачем.
	if s.closedByUser.Load() {
		return false
	}
	// Шелл вышел САМ (`exit`, Ctrl+D, завершился скрипт) — это не обрыв связи, а
	// конец работы, и переподключаться нельзя. Хост после смерти шелла живёт ещё
	// deathGrace, и на КАЖДОЕ подключение честно отдаёт Hello + весь снапшот (до
	// 4 МБ) + frExit: без этой проверки терминал уходил в цикл, где вся история
	// заново проезжала по экрану сверху вниз до самого истечения отсрочки, а на
	// телефоне ещё и качалась через облако. Признак лежал рядом всё это время.
	if s.shouldForget() {
		return false
	}
	// Только персистентные терминалы: у SSH-сессии и локального ConPTY хоста
	// нет, и их обрыв — это действительно конец.
	rec := m.meta.Get(s.ID).Host
	if rec == nil {
		return false
	}
	// Серия обрывов без прогресса: связь поднимается и сразу рвётся. Дальше
	// пробовать нечего — и главное, нельзя делать это в горячем цикле.
	if !s.noteReattachAttempt() {
		log.Printf("[PTY] reattach-live: id=%s связь рвётся сразу после восстановления (%d раза подряд) — терминал считается завершённым",
			s.ID, reattachLiveBurst)
		return false
	}

	// Позиция, до которой сессия уже приняла вывод: хост до-шлёт только хвост
	// после неё вместо всего своего буфера (см. frClientHello.known). Именно
	// здесь это и нужно — связь рвётся на ЖИВОЙ сессии, у которой уже есть
	// история; при старте агента сессия пустая и просит всё.
	known, knownEpoch, absoluteHost := hostResumeCoordinates(s)

	patientUntil := time.Now().Add(reattachLivePatientFor)
	for attempt := 1; ; attempt++ {
		pc, err := dialHostFromEpoch(s.ID, reattachLiveTimeout, 0, 0, known, knownEpoch)
		if err != nil {
			alive, why := hostAliveAfterFailedDial(rec, err)
			// Причину пишем ВСЕГДА. Раньше её не писали нигде: в логе
			// оставалось только итоговое «хост не отозвался», и разобрать
			// боевой случай — занято, нет трубы или таймаут — было нечем.
			// Молчащая диагностика стоит дороже лишней строки.
			if attempt <= reattachLiveTries || attempt%5 == 0 {
				log.Printf("[PTY] reattach-live: id=%s попытка %d не прошла: %v (хост: %s)", s.ID, attempt, err, why)
			}
			if attempt < reattachLiveTries {
				if hostBusyErr(err) {
					// Хост жив, трубу держит кто-то другой — попытка потрачена
					// зря; ждём дольше обычного, вместо того чтобы сжигать бюджет.
					time.Sleep(reattachLiveBackoff * 3)
					continue
				}
				time.Sleep(time.Duration(attempt) * reattachLiveBackoff)
				continue
			}
			// Быстрая серия исчерпана. Дальше решает не счётчик, а хост.
			if !alive {
				log.Printf("[PTY] reattach-live: id=%s хост не отвечает, и его процесса нет — терминал действительно завершён", s.ID)
				return false
			}
			if time.Now().After(patientUntil) {
				log.Printf("[PTY] reattach-live: id=%s хост ЖИВ (host_pid=%d, %s), но связь не поднялась за %s — терминал уходит в «связь потеряна», возврат берёт на себя фоновый relink",
					s.ID, rec.HostPID, why, reattachLivePatientFor)
				return false
			}
			time.Sleep(reattachLivePatientEvery)
			continue
		}
		if pc.protoVersion() != ProtocolVersion {
			// Хост от другой версии бинарника: не усыновляем и не убиваем — как
			// и на старте (reattachHosts). Отцепляемся, хост живёт дальше.
			log.Printf("[PTY] reattach-live: id=%s proto mismatch (host=%d want=%d)", s.ID, pc.protoVersion(), ProtocolVersion)
			_ = pc.Close()
			return false
		}
		if !absoluteHost {
			// Старый живой host (2.57.20 и раньше) не называет
			// свою absolute-шкалу. Посылать ему relative Session.totalBytes
			// опасно: после restart он отрежет не то место кольца.
			// Мы уже запросили known=0; теперь честно сбрасываем
			// ring+DEC+mirror перед полным replay. Живой WS получит
			// новый epoch и reset marker, а не дубль поверх экрана.
			s.resetStreamContinuityForHostModesForConn("", "", 0, pc.hostModes(), pc)
			log.Printf("[PTY] reattach-live: id=%s legacy host — full replay в чистый epoch", s.ID)
		}
		// setConn atomically adopts the host stream and Hello modes before it
		// wakes lagged subscribers; otherwise reset-resync could emit empty modes
		// in the gap between reset and this assignment.
		s.setConn(pc)
		// Пишем только ПЕРВОЕ восстановление в серии: строка «связь
		// восстановлена» на каждый круг — это и есть те 252 тысячи строк из
		// живого случая, в которых утонула настоящая причина.
		if s.reattachBurst() <= 1 {
			log.Printf("[PTY] reattach-live: id=%s связь с host_pid=%d восстановлена (попытка %d)", s.ID, rec.HostPID, attempt)
		}
		return true
	}
}

// hostResumeCoordinates возвращает known только когда текущий
// host доказал абсолютную шкалу. Legacy host получает 0:
// лучше полный replay в чистую сессию, чем тихая дыра/дубль.
func hostResumeCoordinates(s *Session) (known uint64, epoch string, absolute bool) {
	if s == nil {
		return 0, "", false
	}
	state, ok := streamStateOf(s.conn())
	if !ok || state.Epoch == "" {
		return 0, "", false
	}
	s.bufMu.Lock()
	known, epoch = s.totalBytes, state.Epoch
	if s.hostEpoch != state.Epoch {
		// Крохотное окно adopt→setConn: новая baseline уже
		// принята, а conn ещё старый. Не склеиваем шкалы.
		known = 0
	}
	s.bufMu.Unlock()
	return known, epoch, true
}

// HostAlive — у сессии есть запись о pty-host, и он отзывается.
//
// Нужен списку терминалов: карточка «завершён» с живым хостом — это НЕ конец
// работы, а потерянная связь, и говорить о ней надо иначе (и предлагать
// «Переподключить», а не «Перезапустить»: перезапуск дал бы второй терминал в
// той же папке, пока первый продолжает работать).
func (m *Manager) HostAlive(id string) bool {
	if m == nil || m.meta == nil || m.meta.Get(id).Host == nil {
		return false
	}
	pc, err := dialHost(id, reattachLiveTimeout, 0, 0)
	if err != nil {
		// Занятая труба — доказательство ЖИЗНИ хоста, а не его смерти: она
		// однопользовательская, и «занято» означает, что с хостом уже говорит
		// другой экземпляр агента. Считать это смертью — значит нарисовать
		// карточку «завершён» с кнопкой «Перезапустить в этой папке», а она
		// заведёт ВТОРОЙ терминал поверх работающего первого (баг v2.42.4).
		return hostBusyErr(err)
	}
	_ = pc.Close() // Close отправляет Detach — хост остаётся жив
	return true
}

// ReattachSession поднимает связь с живым pty-host для сессии, чей readLoop уже
// закончился. Возвращает ожившую сессию (тот же id — к терминалу ведут ссылки
// из бота и открытые вкладки).
func (m *Manager) ReattachSession(id string) (*Session, error) {
	if m == nil || m.meta == nil {
		return nil, errReattachUnavailable
	}
	meta := m.meta.Get(id)
	if meta.Host == nil {
		return nil, errNoHostRecord
	}
	m.mu.RLock()
	old := m.sessions[id]
	m.mu.RUnlock()
	if old != nil && old.IsAlive() {
		return old, nil // уже живой — переподключать нечего
	}

	pc, err := dialHost(id, 2*time.Second, 0, 0)
	if err != nil {
		// «Не дозвонились» ≠ «его больше нет». Живой случай владельца
		// (17.08.2026, 11:58:07): на нажатие «Вернуть связь» продукт ответил
		// `процесс терминала больше не отвечает` → 404 → тост «Не найдено.»,
		// хотя host_pid=18052 был жив, держал трубу и внутри работал агент.
		// Человеку было предложено единственное оставшееся действие —
		// «Открыть в этой папке», то есть завести ВТОРОЙ терминал поверх
		// работающего первого. Ровно то, что этот файл и должен предотвращать.
		if alive, why := hostAliveAfterFailedDial(meta.Host, err); alive {
			log.Printf("[PTY] reattach by request: id=%s хост жив (host_pid=%d, %s), но не отозвался: %v",
				id, meta.Host.HostPID, why, err)
			return nil, errHostBusy
		}
		log.Printf("[PTY] reattach by request: id=%s дозвон не прошёл, процесса нет: %v", id, err)
		return nil, errHostGone
	}
	if pc.protoVersion() != ProtocolVersion {
		_ = pc.Close()
		return nil, errHostProtoMismatch
	}

	rec := *meta.Host
	sess := newSession(id, rec.CWD, rec.Shell, rec.UID, pc, time.UnixMilli(rec.Created))
	sess.setBornSize(rec.Cols, rec.Rows)
	modes := reattachModes(pc, meta.Modes)
	sess.seedModes(modes)
	// Зеркало обязано родиться в последнем применённом размере, а не в born —
	// PTY хоста так и живёт в нём (см. attachHost в persist_common.go).
	if meta.ViewCols > 0 && meta.ViewRows > 0 {
		sess.viewMu.Lock()
		sess.viewCols, sess.viewRows = meta.ViewCols, meta.ViewRows
		sess.viewMu.Unlock()
	}
	sess.persistView = func(c, r int) { _ = m.meta.SetViewSize(id, c, r) }

	m.mu.Lock()
	m.sessions[id] = sess
	m.mu.Unlock()
	go sess.readLoop(m)
	log.Printf("[PTY] reattach by request: id=%s host_pid=%d", id, rec.HostPID)
	return sess, nil
}
