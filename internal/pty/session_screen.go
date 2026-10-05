package pty

import (
	"log"
	"sync"
	"sync/atomic"
	"time"
)

// Подключение зеркала экрана к живой сессии.
//
// ⚠ КОРМИТЬ ЗЕРКАЛО ИЗ ГОРЯЧЕЙ СЕКЦИИ НЕЛЬЗЯ. Разбор потока в readLoop идёт
// под bufMu вместе с dec.scan и fanoutLocked, и это сделано намеренно (иначе
// подписчик получал вывод дважды). Пропускная способность эмулятора — около
// 3 МБ/с на обычном тексте: `cat` большого файла держал бы замок секундами,
// останавливая рассылку ВСЕМ подписчикам и чтение самого PTY. То есть мы
// своими руками добавили бы тормоз, который уже чинили в 2.48.3.
//
// Поэтому у зеркала своя горутина и свой канал. Канал не блокирует чтение PTY
// никогда: переполнился — чанк выбрасывается, а зеркало помечается
// «отставшим». Отставшее зеркало не врёт молча: кадр из него уже неточен, и
// клиенту в этом случае отдаётся привычный хвост потока.

// screenFeedDepth — сколько чанков помещается в очередь разбора.
//
// ⚠ ЗАПАС СЧИТАЕТСЯ БАЙТАМИ, А НЕ ЧАНКАМИ (screenFeedBytes) — на этих же
// граблях мы уже стояли с очередью подписчиков: ConPTY шлёт по ~70 байт, и
// «256 чанков» на таком потоке это 18 КБ, то есть зеркало объявляло бы себя
// отставшим на ровном месте. Глубина канала здесь — только защита от
// бесконечного роста самого канала.
const screenFeedDepth = 4096

// screenFeedBytes — сколько НЕРАЗОБРАННЫХ байт готовы держать в очереди.
// Больше этого — поток обгоняет эмулятор (около 3 МБ/с), и честнее признать
// зеркало отставшим, чем копить память.
const screenFeedBytes = 4 << 20

// screenChunk — кусок вывода вместе с позицией потока СРАЗУ ПОСЛЕ него.
// Позиция нужна, чтобы снятый кадр можно было отдать клиенту ровно на своём
// месте в потоке, а не «когда получится» (см. ScreenFrameAt).
type screenChunk struct {
	data []byte
	// off — Session.totalBytes после этого куска.
	off uint64
	// generation is captured before the blocking backend Read. It records the
	// ordering seam for tests and is an additional RIS guard, but it cannot by
	// itself prove provenance across kernel/SSH buffering (see
	// risReauthDisabled).
	generation uint64
	// dropScrollback — не байты, а МАРКЕР: «всё, что было до меня, — это
	// переигровка старого буфера, её scrollback выбросить». Едет ТЕМ ЖЕ
	// каналом, что и данные, иначе очистка обгонит ещё не разобранный реплей
	// и не сделает ничего (см. Session.dropReplayScrollback).
	dropScrollback bool
	// barrier закрывается worker'ом строго после всего, что
	// стояло в feed перед маркером. Это асинхронная очередь с
	// подтверждением, а не Sleep-догадка.
	barrier chan struct{}
	// beforeApply is a deterministic test hook for the dequeue/apply seam.
	// Production chunks always leave it nil.
	beforeApply func()
	// resize is an ordered geometry barrier. It travels through the same FIFO
	// as output so every pre-ACK byte is parsed in the old grid and every later
	// byte in the new one.
	resize     bool
	resizeCols int
	resizeRows int
}

type sessionScreen struct {
	mu     sync.Mutex
	mirror *screenMirror
	feed   chan screenChunk
	done   chan struct{}
	// authorityMu serializes all transitions that may make a mirror
	// authoritative again with a non-ACK resize invalidation. Independent
	// atomics are not enough here: a queued RIS worker could otherwise observe
	// the old policy, race invalidateScreenDelivery, and Store(true) last.
	authorityMu sync.Mutex
	// stale — зеркало пропустило часть потока (очередь переполнялась), и его
	// картинка больше не соответствует экрану. Снимается только полной
	// пересборкой зеркала.
	stale atomic.Bool
	// modeAuthoritative permits destructive reconciliation of DEC modes from
	// the mirror. A mirror rebuilt only from a truncated ring tail may have lost
	// ?1049h/RIS or start inside CSI/UTF-8, so it must never prune
	// host-authoritative active modes.
	modeAuthoritative atomic.Bool
	// deliveryAuthoritative permits publishing a frame over the raw client
	// screen. A mirror rebuilt from a truncated ring tail may be useful for
	// continued parsing, but its partial TUI diffs must never overwrite a client
	// that received the complete raw stream. RIS restores a mirror-only gap;
	// after a non-ACK resize only a full stream reset can restore this proof.
	deliveryAuthoritative atomic.Bool
	// minDeliveryGeneration records the successful non-ACK resize boundary. It
	// rejects already-started reads in addition to the permanent transport-safe
	// prohibition in risReauthDisabled.
	minDeliveryGeneration atomic.Uint64
	// risReauthDisabled is permanent for this sessionScreen after any resize
	// whose backend cannot ACK in the ordered output lane. Even a Read begun
	// after Resize returns may consume old-grid bytes already buffered by the
	// kernel/SSH transport, so generation alone cannot prove a later RIS was
	// produced in the new geometry. Only replacing the sessionScreen under an
	// ordered transport may restore RIS-based authorization; a non-ACK backend
	// initializes every replacement as raw-only too.
	risReauthDisabled atomic.Bool
	// queued — сколько байт ждут разбора. Именно по нему решается «зеркало
	// отстало», а не по числу чанков.
	queued atomic.Int64
}

// startScreen поднимает зеркало для сессии. Вызывается один раз при создании и
// при повторном присоединении к хосту: у зеркала нет способа узнать то, что
// было напечатано до его рождения, поэтому чем раньше оно стартует, тем полнее
// картинка.
func (s *Session) startScreen(cols, rows int) {
	sc := s.newSessionScreen(cols, rows)
	// Persistent-host Hello modes are seeded before readLoop starts. Replay may
	// be only a stateful tail without the old ?1049h, so publishing a blank
	// authoritative main-screen mirror would let ModeReassertSeq immediately
	// prune a genuinely active alt/mouse state. Snapshot DEC and seed the fresh
	// mirror under bufMu; this is the same bufMu→screenLifeMu order used by
	// stream reset and mode reconciliation.
	s.bufMu.Lock()
	modeSeed := ""
	if s.dec != nil {
		modeSeed = s.dec.reassertSeq()
	}
	if modeSeed != "" {
		sc.mu.Lock()
		mirror := sc.mirror
		sc.mu.Unlock()
		mirror.Write([]byte(modeSeed))
	}
	s.screenLifeMu.Lock()
	s.screen.Store(sc)
	s.screenLifeMu.Unlock()
	s.bufMu.Unlock()
}

func (s *Session) newSessionScreen(cols, rows int) *sessionScreen {
	return s.newSessionScreenWithFrames(cols, rows, screenFramesSupported(s.conn()))
}

func (s *Session) newSessionScreenWithFrames(cols, rows int, framesSupported bool) *sessionScreen {
	sc := &sessionScreen{
		mirror: newScreenMirror(cols, rows),
		feed:   make(chan screenChunk, screenFeedDepth),
		done:   make(chan struct{}),
	}
	sc.modeAuthoritative.Store(true)
	sc.deliveryAuthoritative.Store(true)
	if !framesSupported {
		// A pending frame captured before the first unordered resize could be
		// emitted afterwards, bypassing invalidateScreenDelivery. Do not create
		// such a frame at all for transports that cannot establish the output /
		// geometry boundary. Raw ring/replay remains fully available.
		sc.deliveryAuthoritative.Store(false)
		sc.risReauthDisabled.Store(true)
	}
	go func() {
		defer close(sc.done)
		for ch := range sc.feed {
			sc.mu.Lock()
			m := sc.mirror
			sc.mu.Unlock()
			if m == nil {
				if ch.barrier != nil {
					close(ch.barrier)
				}
				return
			}
			if ch.dropScrollback {
				// Переигровка кончилась: экран собран, а всё, что она нагнала
				// вверх, — не история человека, а повтор уже показанного.
				//
				// ⚠ РЕШЕНИЕ ПРИНИМАЕТСЯ ЗДЕСЬ, А НЕ ПРИ ОТПРАВКЕ МАРКЕРА.
				// Первая версия гейта спрашивала вид агента в момент границы —
				// и почти всегда получала пустоту: при перезапуске переигровка
				// приходит за миллисекунды, а вид агента заполняется только при
				// первом опросе состояния сессии. Живой замер 14.08.2026 после
				// выпуска: сигнал сработал у ОДНОЙ сессии из шести, дубли у
				// остальных остались. Здесь же маркер уже дождался своей
				// очереди — и вид агента можно спросить у самой системы.
				if kind, ok := s.agentLikeNow(); ok {
					m.DropScrollback()
					log.Printf("[PTY] переигровка буфера кончилась id=%s агент=%s — история зеркала начинается заново", s.ID, kind)
				}
				if ch.barrier != nil {
					close(ch.barrier)
				}
				continue
			}
			if ch.beforeApply != nil {
				ch.beforeApply()
			}
			if ch.resize {
				m.Resize(ch.resizeCols, ch.resizeRows)
				if ch.barrier != nil {
					close(ch.barrier)
				}
				continue
			}
			if len(ch.data) > 0 {
				m.mu.Lock()
				resetBefore := m.resetGeneration
				m.mu.Unlock()
				m.WriteAt(ch.data, ch.off)
				m.mu.Lock()
				resetAfter := m.resetGeneration
				m.mu.Unlock()
				if resetAfter > resetBefore {
					sc.authorityMu.Lock()
					if !sc.risReauthDisabled.Load() && ch.generation >= sc.minDeliveryGeneration.Load() {
						// RIS is a parser-proven full baseline after a mirror-only
						// continuity gap. Non-ACK resize disables this path because
						// the transport cannot prove the RIS used the new geometry.
						sc.deliveryAuthoritative.Store(true)
						sc.modeAuthoritative.Store(true)
					}
					sc.authorityMu.Unlock()
				}
				sc.queued.Add(-int64(len(ch.data)))
			}
			if ch.barrier != nil {
				close(ch.barrier)
			}
		}
	}()
	return sc
}

func (s *Session) stopScreen() {
	s.screenLifeMu.Lock()
	sc, _ := s.screen.Load().(*sessionScreen)
	if sc == nil {
		s.screenLifeMu.Unlock()
		return
	}
	s.screen.Store((*sessionScreen)(nil))
	m := detachSessionScreen(sc)
	s.screenLifeMu.Unlock()
	<-sc.done
	if m != nil {
		m.Close()
	}
}

// detachSessionScreen зовётся только под screenLifeMu.Lock:
// ни один feedScreenAt уже не держит ссылку на закрываемый feed.
func detachSessionScreen(sc *sessionScreen) *screenMirror {
	sc.mu.Lock()
	m := sc.mirror
	sc.mirror = nil
	sc.mu.Unlock()
	close(sc.feed)
	return m
}

// feedScreen отдаёт чанк зеркалу. НИКОГДА не блокирует: чтение PTY важнее
// точности картинки, и лучше признать зеркало отставшим, чем задержать вывод.
func (s *Session) feedScreen(data []byte) { s.feedScreenAt(data, 0) }

// feedScreenAt — то же плюс позиция потока сразу ПОСЛЕ этого куска
// (Session.totalBytes). Ноль означает «позиции нет» (тесты и пробы, где потока
// с позициями не существует) и кадр к потоку не привязывает.
func (s *Session) feedScreenAt(data []byte, off uint64) {
	s.feedScreenAtGeneration(data, off, s.outputGeneration.Load())
}

func (s *Session) feedScreenAtGeneration(data []byte, off, generation uint64) {
	s.screenLifeMu.RLock()
	defer s.screenLifeMu.RUnlock()
	sc, _ := s.screen.Load().(*sessionScreen)
	if sc == nil {
		return
	}
	// Once one chunk is lost, every later offset is non-contiguous. Enqueuing
	// it would advance mirror.appliedOff across the hole and make ring catch-up
	// falsely conclude that there is nothing left to recover.
	if sc.stale.Load() {
		return
	}
	if sc.queued.Load() > screenFeedBytes {
		sc.stale.Store(true)
		return
	}
	cp := make([]byte, len(data))
	copy(cp, data)
	sc.queued.Add(int64(len(cp)))
	select {
	case sc.feed <- screenChunk{data: cp, off: off, generation: generation}:
	default:
		sc.queued.Add(-int64(len(cp)))
		sc.stale.Store(true)
	}
}

// agentLikeNow — работает ли в сессии AI-агент ПРЯМО СЕЙЧАС. По ответу решается
// РАЗРУШИТЕЛЬНАЯ операция — очистка scrollback зеркала, поэтому порядок опроса
// здесь вопрос сохранности истории человека.
//
// ⚠ ПЕРВЫМ ИДЁТ ТЕКУЩИЙ FOREGROUND, А НЕ ЗАПОМНЕННЫЙ ВИД. Внешний аудит
// 2.57.18 (14.08.2026), находка P0-02: прежний порядок сначала доверял
// `lastAgentKind`, а он НЕ очищается при выходе агента. Сценарий: Claude
// поработал и завершился, человек вернулся в PowerShell и напечатал важную
// историю, агент переподключился — граница переигровки звала agentLikeNow,
// получала исторический "claude" и DropScrollback стирал НАСТОЯЩУЮ историю
// оболочки. Теперь: передний план определён — решает он; агент — очистка
// допустима, оболочка/другое — запрещена, что бы ни было запомнено раньше.
//
// ⚠ ЗАЧЕМ ТОГДА НУЖЕН FALLBACK. При перезапуске агента (автообновление)
// переигровка приходит за миллисекунды, а опрос foreground может быть
// недоступен (backend ещё не подключён, процесс не разрешился). Только тогда
// решает запомненный вид — и только если он АГЕНТСКИЙ: запомненная оболочка
// («shell»/«node»/«other») очистку не разрешает никогда.
//
// Чистое правило — cleanupAgentKind, здесь только сбор входов.
func (s *Session) agentLikeNow() (string, bool) {
	if s.conn() != nil {
		if fg := s.ForegroundProcess(); fg.Name != "" {
			kind, ok := cleanupAgentKind(AgentKind(fg.Name), true, "")
			if ok {
				s.rememberAgentKind(kind)
			}
			return kind, ok
		}
	}
	// Передний план неизвестен — единственный случай, когда решает история.
	return cleanupAgentKind("", false, s.lastAgentKind())
}

// cleanupAgentKind — кому разрешена разрушительная очистка истории зеркала.
//
//	fgKnown и fgKind — агентский  → очистка допустима;
//	fgKnown и fgKind — НЕ агентский (оболочка, node, other) → ЗАПРЕЩЕНА,
//	    что бы ни хранил historical: на переднем плане прямо сейчас работает
//	    человек, и его история настоящая;
//	foreground неизвестен → fallback на historical, и тоже только агентский.
func cleanupAgentKind(fgKind string, fgKnown bool, historical string) (string, bool) {
	if fgKnown {
		if IsAgentKind(fgKind) {
			return fgKind, true
		}
		return "", false
	}
	if IsAgentKind(historical) {
		return historical, true
	}
	return "", false
}

// dropReplayScrollback — переигровка старого буфера кончилась, живой поток
// начался. Всё, что переигровка нагнала в scrollback зеркала, выбрасываем.
//
// ⚠ ПОЧЕМУ ЭТО ВООБЩЕ НУЖНО. Зеркало исполняет переигровку как обычный поток:
// строки уходят вверх и оседают историей, которой у настоящего терминала в этом
// месте нет. Живой замер 14.08.2026 сразу после автообновления агента: в
// присланной телефону истории два повтора блока подряд (шапка `Claude Code
// v2.1.228` дважды и пара «цитата + пересылке.» дважды) — ровно то, что владелец
// увидел на экране. Тот же поток через настоящий @xterm/headless: scrollback
// четыре строки, повторов ноль.
//
// ⚠ ТОЛЬКО АГЕНТСКИЕ СЕССИИ. У обычной оболочки переигранная история НАСТОЯЩАЯ:
// человек печатал команды, они честно прокручивались вверх, и отнимать их
// нельзя. У агентских CLI история переигровки — это повтор экрана, который
// агент и так перерисует.
//
// ⚠ МАРКЕР ИДЁТ ЧЕРЕЗ ОЧЕРЕДЬ. Прямой вызов очистки здесь ничего бы не дал:
// реплейные байты в этот момент ещё лежат в канале неразобранными, и очистка
// прошла бы ДО них.
func (s *Session) dropReplayScrollback() {
	s.screenLifeMu.RLock()
	sc, _ := s.screen.Load().(*sessionScreen)
	if sc == nil {
		s.screenLifeMu.RUnlock()
		return
	}
	ack := make(chan struct{})
	select {
	case sc.feed <- screenChunk{dropScrollback: true, barrier: ack}:
		s.screenLifeMu.RUnlock()
	case <-sc.done:
		s.screenLifeMu.RUnlock()
		return
	}
	// Настоящий ordered barrier: readDecoded не вернётся к
	// следующему frOutput, пока marker не обработан.
	select {
	case <-ack:
	case <-sc.done:
	}
}

// screenBarrier дожидается всей очереди без polling/Sleep.
func (s *Session) screenBarrier(sc *sessionScreen) bool {
	ack := make(chan struct{})
	s.screenLifeMu.RLock()
	current, _ := s.screen.Load().(*sessionScreen)
	if current != sc {
		s.screenLifeMu.RUnlock()
		return false
	}
	select {
	case sc.feed <- screenChunk{barrier: ack}:
		s.screenLifeMu.RUnlock()
	case <-sc.done:
		s.screenLifeMu.RUnlock()
		return false
	}
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	select {
	case <-ack:
		return true
	case <-sc.done:
		return false
	case <-timer.C:
		return false
	}
}

// recoverScreen first tries a continuity-preserving in-place catch-up. After
// the first dropped chunk feedScreenAt stops enqueuing, so mirror.appliedOff is
// the exact last contiguous byte; if it is still in Session ring, replaying the
// missing suffix preserves the full VT/TUI state that no tail can recreate.
//
// If continuity has already fallen out of the ring, a best-effort fresh mirror
// may continue parsing future output, but it is non-authoritative for both
// delivery and destructive DEC-mode pruning. A truncated tail may omit the
// baseline, ?1049h/RIS or begin inside CSI/UTF-8; publishing it could overwrite
// a correct client that never missed the mirror-only queue gap.
func (s *Session) recoverScreen(stale *sessionScreen) bool {
	return s.recoverScreenWithHook(stale, nil)
}

// recoverScreenWithHook exposes the pre-swap seam to a deterministic geometry
// race test. Production always calls recoverScreen, which supplies nil.
func (s *Session) recoverScreenWithHook(stale *sessionScreen, beforeSwap func()) bool {
	if !s.screenBarrier(stale) {
		return false
	}
	stale.mu.Lock()
	oldMirror := stale.mirror
	stale.mu.Unlock()
	if oldMirror == nil {
		return false
	}
	cols, rows := oldMirror.Size()
	oldMirror.mu.Lock()
	applied := oldMirror.appliedOff
	oldMirror.mu.Unlock()

	s.bufMu.Lock()
	base := s.totalBytes
	start := bufStartOf(base, len(s.buf))
	seed := append([]byte(nil), s.buf...)
	modeSeed := s.dec.reassertSeq()
	s.bufMu.Unlock()

	// Preferred path: the ring still contains every byte after the last
	// contiguous mirror offset. Parse the potentially heavy suffix outside
	// bufMu, then catch the small concurrent delta while finalizing.
	if applied >= start && applied <= base {
		delta := append([]byte(nil), seed[applied-start:]...)
		if len(delta) > 0 {
			oldMirror.WriteAt(delta, base)
		}

		s.bufMu.Lock()
		current := s.totalBytes
		currentStart := bufStartOf(current, len(s.buf))
		if base >= currentStart && base <= current {
			catchUp := append([]byte(nil), s.buf[base-currentStart:]...)
			if beforeSwap != nil {
				beforeSwap()
			}
			s.screenLifeMu.Lock()
			active, _ := s.screen.Load().(*sessionScreen)
			if active == stale && stale.stale.Load() {
				if len(catchUp) > 0 {
					oldMirror.WriteAt(catchUp, current)
				}
				stale.stale.Store(false)
				s.screenLifeMu.Unlock()
				s.bufMu.Unlock()
				log.Printf("[PTY] зеркало id=%s догнано по непрерывному ring до базы %d", s.ID, current)
				return true
			}
			s.screenLifeMu.Unlock()
		}
		s.bufMu.Unlock()
	}

	fresh := s.newSessionScreen(cols, rows)
	fresh.modeAuthoritative.Store(false)
	fresh.deliveryAuthoritative.Store(false)
	minDeliveryGeneration := stale.minDeliveryGeneration.Load()
	fresh.minDeliveryGeneration.Store(minDeliveryGeneration)
	fresh.risReauthDisabled.Store(stale.risReauthDisabled.Load())
	fresh.mu.Lock()
	freshMirror := fresh.mirror
	fresh.mu.Unlock()
	if modeSeed != "" {
		// Improve the common stateful-tail case by entering the current active
		// modes before applying cell diffs. This is still best-effort; the flag
		// above is the safety boundary for destructive decisions.
		freshMirror.Write([]byte(modeSeed))
	}
	if len(seed) > 0 {
		freshMirror.WriteAt(seed, base)
	}
	freshMirror.mu.Lock()
	seedHasRIS := freshMirror.resetGeneration > 0
	freshMirror.mu.Unlock()
	if seedHasRIS && minDeliveryGeneration == 0 && !fresh.risReauthDisabled.Load() {
		fresh.modeAuthoritative.Store(true)
		fresh.deliveryAuthoritative.Store(true)
	}

	s.bufMu.Lock()
	current := s.totalBytes
	start = bufStartOf(current, len(s.buf))
	if base < start || base > current {
		s.bufMu.Unlock()
		fresh.mu.Lock()
		fresh.mirror = nil
		fresh.mu.Unlock()
		close(fresh.feed)
		<-fresh.done
		freshMirror.Close()
		return false
	}
	delta := append([]byte(nil), s.buf[base-start:]...)
	if beforeSwap != nil {
		beforeSwap()
	}

	s.screenLifeMu.Lock()
	active, _ := s.screen.Load().(*sessionScreen)
	if active != stale || !stale.stale.Load() {
		s.screenLifeMu.Unlock()
		s.bufMu.Unlock()
		fresh.mu.Lock()
		fresh.mirror = nil
		fresh.mu.Unlock()
		close(fresh.feed)
		<-fresh.done
		freshMirror.Close()
		return false
	}
	// За время тяжёлой сборки viewer мог поменять размер stale
	// mirror. screenLifeMu не пускает параллельный resize между
	// этой повторной сверкой и store.
	active.mu.Lock()
	activeMirror := active.mirror
	active.mu.Unlock()
	if activeMirror != nil {
		finalCols, finalRows := activeMirror.Size()
		freshMirror.Resize(finalCols, finalRows)
	}
	// A non-ACK resize can invalidate the active mirror while a heavy recovery
	// is being built outside screenLifeMu. Re-read the permanent authority bit
	// at the atomic swap boundary so recovery cannot replace an invalidated
	// screen with an accidentally authorizable one.
	active.authorityMu.Lock()
	if active.risReauthDisabled.Load() {
		fresh.risReauthDisabled.Store(true)
		fresh.deliveryAuthoritative.Store(false)
		fresh.modeAuthoritative.Store(false)
		activeMin := active.minDeliveryGeneration.Load()
		if activeMin > fresh.minDeliveryGeneration.Load() {
			fresh.minDeliveryGeneration.Store(activeMin)
		}
	}
	active.authorityMu.Unlock()
	if len(delta) > 0 {
		fresh.queued.Add(int64(len(delta)))
		fresh.feed <- screenChunk{data: delta, off: current}
	}
	s.screen.Store(fresh)
	retiredMirror := detachSessionScreen(stale)
	s.screenLifeMu.Unlock()
	s.bufMu.Unlock()
	<-stale.done
	if retiredMirror != nil {
		retiredMirror.Close()
	}
	log.Printf("[PTY] зеркало id=%s восстановлено из неполного ring на базе %d (DEC pruning disabled)", s.ID, current)
	return true
}

// resetScreenEmptyLocked swaps to an empty mirror while screenLifeMu is held.
// It intentionally does not wait for the retired worker: callers that also
// hold bufMu must release locks before waiting for the worker to drain.
// modeSeed is the authoritative Hello DEC state for the new stream and must be
// applied before bufMu is released/woken subscribers can call ModeReassertSeq.
func (s *Session) resetScreenEmptyLocked(modeSeed string, framesSupported bool) (*sessionScreen, *screenMirror) {
	old, _ := s.screen.Load().(*sessionScreen)
	if old == nil {
		return nil, nil
	}
	old.mu.Lock()
	oldMirror := old.mirror
	old.mu.Unlock()
	if oldMirror == nil {
		return nil, nil
	}
	cols, rows := oldMirror.Size()
	fresh := s.newSessionScreenWithFrames(cols, rows, framesSupported)
	if modeSeed != "" {
		fresh.mu.Lock()
		freshMirror := fresh.mirror
		fresh.mu.Unlock()
		freshMirror.Write([]byte(modeSeed))
	}
	s.screen.Store(fresh)
	retiredMirror := detachSessionScreen(old)
	return old, retiredMirror
}

func finishScreenRetire(old *sessionScreen, retiredMirror *screenMirror) {
	if old == nil {
		return
	}
	<-old.done
	if retiredMirror != nil {
		retiredMirror.Close()
	}
}

// resizeScreen is called at the backend's ordered resize completion (for a
// persistent host, synchronously while readDecoded handles frResizeAck).
// Geometry is a marker in the SAME FIFO as screen output: queued pre-ACK bytes
// must be parsed in the old grid before the marker, while readDecoded cannot
// deliver post-ACK output until the marker acknowledges completion.
//
// If the mirror already dropped a pre-ACK chunk, recover it from the Session
// ring before enqueuing the marker. Replaying that old output after resizing is
// observably different for CUP, wrapping and scroll regions.
func (s *Session) resizeScreen(cols, rows int) {
	for {
		s.screenLifeMu.RLock()
		sc, _ := s.screen.Load().(*sessionScreen)
		s.screenLifeMu.RUnlock()
		if sc == nil {
			return
		}
		if sc.stale.Load() {
			if !s.recoverScreen(sc) {
				if s.resetStaleScreenForResize(sc, cols, rows) {
					return
				}
			}
			continue // recovery may have swapped the active screen
		}

		ack := make(chan struct{})
		s.screenLifeMu.RLock()
		current, _ := s.screen.Load().(*sessionScreen)
		if current != sc || sc.stale.Load() {
			s.screenLifeMu.RUnlock()
			continue
		}
		select {
		case sc.feed <- screenChunk{resize: true, resizeCols: cols, resizeRows: rows, barrier: ack}:
			screenDone := sc.done
			s.screenLifeMu.RUnlock()
			select {
			case <-ack:
			case <-screenDone:
			}
			return
		case <-sc.done:
			s.screenLifeMu.RUnlock()
			return
		}
	}
}

// resetStaleScreenForResize is the conservative fallback when continuity
// cannot be recovered before the ACK barrier. It deliberately loses the old
// picture instead of parsing old-geometry bytes in the new grid. The fresh
// mirror starts at the current absolute offset and is non-authoritative for
// destructive DEC pruning; subsequent post-resize output remains contiguous.
func (s *Session) resetStaleScreenForResize(stale *sessionScreen, cols, rows int) bool {
	s.bufMu.Lock()
	baseline := s.totalBytes
	modeSeed := ""
	if s.dec != nil {
		modeSeed = s.dec.reassertSeq()
	}
	s.screenLifeMu.Lock()
	active, _ := s.screen.Load().(*sessionScreen)
	if active != stale || !stale.stale.Load() {
		s.screenLifeMu.Unlock()
		s.bufMu.Unlock()
		return false
	}

	fresh := s.newSessionScreen(cols, rows)
	fresh.modeAuthoritative.Store(false)
	fresh.deliveryAuthoritative.Store(false)
	fresh.minDeliveryGeneration.Store(stale.minDeliveryGeneration.Load())
	fresh.risReauthDisabled.Store(stale.risReauthDisabled.Load())
	fresh.mu.Lock()
	freshMirror := fresh.mirror
	fresh.mu.Unlock()
	if modeSeed != "" {
		freshMirror.Write([]byte(modeSeed))
	}
	freshMirror.mu.Lock()
	freshMirror.appliedOff = baseline
	freshMirror.mu.Unlock()
	s.screen.Store(fresh)
	retiredMirror := detachSessionScreen(stale)
	s.screenLifeMu.Unlock()
	s.bufMu.Unlock()

	// Do not let a wedged retired parser hold the protocol ACK callback and the
	// host output reader. It is unreachable after the atomic swap and can drain
	// and close independently.
	go finishScreenRetire(stale, retiredMirror)
	log.Printf("[PTY] stale mirror id=%s reset at resize barrier %dx%d base=%d", s.ID, cols, rows, baseline)
	return true
}

// ScreenFrame отдаёт самодостаточный кадр экрана, ИСТОРИЮ (scrollback + текущие
// видимые строки: клиент пишет её в чистый терминал перед кадром и получает
// точный шов, см. screenMirror.historyLocked) и геометрию, в которой они
// сняты. Пустой frame означает «снапшота нет» — зеркала не существует или оно
// отстало; тогда и history пуста, а вызывающий обязан в этом случае
// откатиться на хвост потока. У alt-screen истории нет по определению
// протокола: там history пуста и при живом кадре. histLines — общее число
// строк в history (клиенту для контроля приёма).
//
// Кадр и история снимаются ОДНИМ удержанием замка зеркала (см.
// screenMirror.Snapshot): между ними не проскочит запись, которая уронила бы
// строку или показала её дважды, а геометрия — ровно та, в которой снято
// содержимое. Клиент применяет снапшот именно в этой геометрии, а свой размер
// выставляет уже после (так же поступает VS Code — forceExactSize, и того же
// требует документация аддона сериализации xterm.js).
func (s *Session) ScreenFrame() (frame, history string, histLines, cols, rows int) {
	frame, history, histLines, cols, rows, _ = s.ScreenFrameAt()
	return
}

func (s *Session) beginScreenGeometryChange() {
	s.screenGeometryMu.Lock()
	s.screenGeometryRevision++
	s.screenResizePending++
	s.signalScreenGeometryLocked()
	s.screenGeometryMu.Unlock()
}

func (s *Session) finishScreenGeometryChange() {
	s.screenGeometryMu.Lock()
	if s.screenResizePending > 0 {
		s.screenResizePending--
	}
	s.screenGeometryRevision++
	s.signalScreenGeometryLocked()
	s.screenGeometryMu.Unlock()
}

func (s *Session) signalScreenGeometryLocked() {
	if s.screenGeometryWake != nil {
		close(s.screenGeometryWake)
	}
	s.screenGeometryWake = make(chan struct{})
}

func (s *Session) screenGeometryWakeLocked() <-chan struct{} {
	if s.screenGeometryWake == nil {
		s.screenGeometryWake = make(chan struct{})
	}
	return s.screenGeometryWake
}

// WithCurrentScreenFrameRevision validates a previously captured frame and
// holds a read lease through its actual transport write. A resize request or
// completion needs the write side of the same lock, so it is ordered wholly
// before or wholly after this send instead of overtaking the final check.
func (s *Session) WithCurrentScreenFrameRevision(revision uint64, send func() error) (bool, error) {
	s.screenGeometryMu.RLock()
	defer s.screenGeometryMu.RUnlock()
	if s.screenResizePending != 0 || revision != s.screenGeometryRevision {
		return false, nil
	}
	if send == nil {
		return true, nil
	}
	return true, send()
}

// ScreenFrameAt — то же самое плюс baseOff: позиция потока, до которой кадр
// УЧИТЫВАЕТ вывод. Всё, что ≤ baseOff, в кадре уже нарисовано.
//
// Зачем это нужно. Кадр снимается в горутине писателя, а байты для клиента
// лежат в его канале подписчика — и снимок обгонял их: зеркало применяло чанк
// Y, кадр включал Y, а следом писатель доставал Y из канала и отправлял как
// обычный вывод. Клиент исполнял те же VT-команды ВТОРОЙ раз. Для текста это
// видимый дубль, а для LF, относительных перемещений курсора, вставки/удаления
// строк и alt-screen повтор неидемпотентен — экран уезжает произвольно
// (внешний аудит 13.08.2026, находка T-003). Зная baseOff, писатель придержит
// кадр, пока не отдаст клиенту всё, что кадром уже покрыто.
//
// Инвариант baseOff ≥ sent соблюдается по построению: fanoutLocked (наполнение
// канала подписчика) стоит в readLoop ПЕРЕД feedScreen и в одной горутине с
// ним, а снимок ждёт разбора всей очереди зеркала.
func (s *Session) ScreenFrameAt() (frame, history string, histLines, cols, rows int, baseOff uint64) {
	frame, history, histLines, cols, rows, baseOff, _ = s.ScreenFrameAtRevision()
	return
}

// ScreenFrameAtRevision captures a geometry revision together with the frame.
// Capture is retried by the caller when a resize starts/completes across the
// snapshot. While any ACK may still arrive, frames are conservatively withheld.
func (s *Session) ScreenFrameAtRevision() (frame, history string, histLines, cols, rows int, baseOff, revision uint64) {
	frame, history, histLines, cols, rows, baseOff, revision, _, _ = s.ScreenFrameAtRevisionWait()
	return
}

// ScreenFrameAtRevisionWait atomically returns the retry decision and the
// exact geometry-transition channel together with an empty frame. An ACK
// cannot complete between the empty decision and subscription: both happen
// under screenGeometryMu. retry=true/wake=nil means the whole resize crossed
// capture and the caller should retry immediately.
//
// Прежняя сигнатура для прежних вызовов; причину пустого кадра отдаёт
// CaptureScreenFrame.
func (s *Session) ScreenFrameAtRevisionWait() (frame, history string, histLines, cols, rows int, baseOff, revision uint64, retry bool, wake <-chan struct{}) {
	c := s.CaptureScreenFrame()
	return c.Frame, c.History, c.HistLines, c.Cols, c.Rows, c.BaseOff, c.Revision, c.Retry, c.Wake
}

// Причины пустого кадра (ST-05, screen-request-v1). Строки уходят клиенту полем
// reason сообщения screen-none, то есть это часть протокола: значения только
// добавляются (I-14), смысл существующих не меняется.
const (
	// ScreenReasonNotReady — зеркало пока не может честно назвать базу: парсер
	// посреди UTF-8/CSI/OSC/DCS, придержан кластер графем, очередь разбора не
	// успела или зеркало подменили (reset потока, пересборка, resize) во время
	// барьера либо между барьером и снимком. Проходит само — повтор через паузу.
	ScreenReasonNotReady = "not-ready"
	// ScreenReasonUnavailable — кадра не будет, пока не сменится поток: зеркала
	// у сессии нет (старая сессия, остановленное зеркало), транспорт не
	// упорядочивает resize с выводом (!deliveryAuthoritative: SSH, прямой PTY,
	// старый pty-host), зеркало пересобрано из обрезанного кольца и не
	// авторитетно или эмулятор зеркала паниковал и кадр недостоверен до RIS
	// приложения (screen_vtpanic.go). Повторять бессмысленно.
	ScreenReasonUnavailable = "unavailable"
	// ScreenReasonResizePending — через снимок прошёл resize или ещё ждётся его
	// ACK. Сервер повторит снимок сам, когда геометрия устоится (retry/wake), и
	// кадр придёт с тем же req.
	ScreenReasonResizePending = "resize-pending"
)

// ScreenCapture — результат одной попытки снять кадр (см. CaptureScreenFrame).
// Пустой Frame всегда сопровождается Reason; непустой — пустым Reason.
type ScreenCapture struct {
	Frame, History        string
	HistLines, Cols, Rows int
	// BaseOff — позиция потока, которую кадр уже учитывает (см. ScreenFrameAt).
	BaseOff  uint64
	Revision uint64
	// Retry/Wake — прежний контракт ScreenFrameAtRevisionWait: resize пересёк
	// снимок; Wake != nil — ждать завершения, nil — повторить сразу.
	Retry  bool
	Wake   <-chan struct{}
	Reason string
}

// CaptureScreenFrame — ScreenFrameAtRevisionWait вместе с причиной пустого
// кадра. Раньше «кадра нет» было одной пустой строкой на три разные ситуации,
// и сервер молча не отвечал клиенту: тот не мог отличить «повтори позже» от
// «кадров на этом транспорте не будет» (карта ST-05, «молчаливый отказ»).
func (s *Session) CaptureScreenFrame() ScreenCapture {
	s.screenGeometryMu.Lock()
	revision := s.screenGeometryRevision
	if s.screenResizePending != 0 {
		wake := s.screenGeometryWakeLocked()
		if s.beforeScreenGeometryWaitReturn != nil {
			s.beforeScreenGeometryWaitReturn()
		}
		s.screenGeometryMu.Unlock()
		return ScreenCapture{Revision: revision, Retry: true, Wake: wake, Reason: ScreenReasonResizePending}
	}
	s.screenGeometryMu.Unlock()

	frame, history, histLines, cols, rows, baseOff, reason := s.screenFrameAtCurrentReason()

	s.screenGeometryMu.Lock()
	currentRevision := s.screenGeometryRevision
	if s.screenResizePending != 0 {
		wake := s.screenGeometryWakeLocked()
		if s.beforeScreenGeometryWaitReturn != nil {
			s.beforeScreenGeometryWaitReturn()
		}
		s.screenGeometryMu.Unlock()
		return ScreenCapture{Revision: currentRevision, Retry: true, Wake: wake, Reason: ScreenReasonResizePending}
	}
	changed := currentRevision != revision
	s.screenGeometryMu.Unlock()
	if changed {
		return ScreenCapture{Revision: currentRevision, Retry: true, Reason: ScreenReasonResizePending}
	}
	return ScreenCapture{
		Frame: frame, History: history, HistLines: histLines, Cols: cols, Rows: rows,
		BaseOff: baseOff, Revision: revision, Reason: reason,
	}
}

func (s *Session) screenFrameAtCurrent() (frame, history string, histLines, cols, rows int, baseOff uint64) {
	frame, history, histLines, cols, rows, baseOff, _ = s.screenFrameAtCurrentReason()
	return
}

// screenFrameAtCurrentReason — тело screenFrameAtCurrent с кодом причины на
// каждом пустом выходе. Решения те же, что и раньше; добавлена только метка.
func (s *Session) screenFrameAtCurrentReason() (frame, history string, histLines, cols, rows int, baseOff uint64, reason string) {
	none := func(why string) (string, string, int, int, int, uint64, string) {
		return "", "", 0, 0, 0, 0, why
	}
	// Барьер или пересборка не удались, и нужно назвать причину. Судим по
	// ТЕКУЩЕМУ зеркалу сессии, а не по sc.done: done закрывается не только при
	// остановке, но и при любой подмене — detachSessionScreen закрывает feed,
	// воркер на следующем чанке видит mirror==nil и выходит, не подтвердив наш
	// барьер. Подменяют зеркало reset потока (resetScreenEmptyLocked),
	// пересборка другого зрителя или resize (recoverScreen,
	// resetStaleScreenForResize). Новое зеркало уже есть, и следующий запрос
	// получит кадр; «unavailable» здесь увело бы клиента screen-request-v1 в
	// degraded до переподключения ровно в момент, когда кадры снова доступны
	// (замечание ревью B1, 14.09.2026). «Кадров не будет» — только когда зеркала
	// у сессии не осталось совсем (stopScreen кладёт nil). Неавторитетное новое
	// зеркало следующий запрос сам назовёт unavailable веткой deliveryAuthoritative.
	gone := func() (string, string, int, int, int, uint64, string) {
		if cur, _ := s.screen.Load().(*sessionScreen); cur == nil {
			return none(ScreenReasonUnavailable)
		}
		return none(ScreenReasonNotReady)
	}
	sc, _ := s.screen.Load().(*sessionScreen)
	if sc == nil {
		return none(ScreenReasonUnavailable)
	}
	if sc.stale.Load() {
		if !s.recoverScreen(sc) {
			// recoverScreen отказывает при подмене во время его барьера или
			// перед заменой (active != stale), при таймауте барьера и когда
			// кольцо ушло вперёд за время тяжёлой сборки. Всё это проходит само;
			// зеркало, пересобранное из обрезанного кольца, успехом не считается
			// и отказывает веткой deliveryAuthoritative ниже.
			return gone()
		}
		sc, _ = s.screen.Load().(*sessionScreen)
		if sc == nil {
			return none(ScreenReasonUnavailable)
		}
	}
	if !sc.deliveryAuthoritative.Load() {
		return none(ScreenReasonUnavailable)
	}
	// Точный barrier очереди вместо polling по queued + Sleep. Он
	// учитывает и небайтовые marker'ы, включая dropScrollback.
	if !s.screenBarrier(sc) {
		// Барьер не дождался: зеркало остановлено («не будет»), подменено или
		// очередь не разобралась за 2 с («позже»). Остановку от подмены
		// отличает только текущее зеркало сессии — см. gone выше.
		return gone()
	}
	if sc.stale.Load() {
		// Переполнение случилось уже во время барьера; следующий запрос сам
		// пересоберёт зеркало из кольца (ветка выше).
		return none(ScreenReasonNotReady)
	}
	if !sc.deliveryAuthoritative.Load() {
		return none(ScreenReasonUnavailable)
	}
	s.screenLifeMu.RLock()
	defer s.screenLifeMu.RUnlock()
	current, _ := s.screen.Load().(*sessionScreen)
	if current != sc {
		// Зеркало заменили (reset потока, пересборка) между барьером и снимком:
		// повтор увидит новое.
		return none(ScreenReasonNotReady)
	}
	if !sc.deliveryAuthoritative.Load() {
		return none(ScreenReasonUnavailable)
	}
	sc.mu.Lock()
	m := sc.mirror
	sc.mu.Unlock()
	if m == nil {
		return none(ScreenReasonUnavailable)
	}
	frame, history, histLines, cols, rows, baseOff = m.SnapshotAt(screenMirrorScrollback)
	if frame == "" {
		// SnapshotAt пуст ровно в трёх случаях: зеркало закрыто; эмулятор
		// паниковал, и зеркало недостоверно до RIS приложения (untrusted,
		// screen_vtpanic.go) — само это не проходит, поэтому «unavailable», как
		// у неавторитетного зеркала из обрезанного кольца; или
		// snapshotReadyLocked отказал (парсер не в Ground, придержан кластер,
		// хвост ED) — «ещё не готово», проходит само на следующей границе потока.
		m.mu.Lock()
		closed, untrusted := m.closed, m.untrusted
		m.mu.Unlock()
		if closed || untrusted {
			return none(ScreenReasonUnavailable)
		}
		return none(ScreenReasonNotReady)
	}
	return frame, history, histLines, cols, rows, baseOff, ""
}

// ScreenStale — правда ли, что зеркало пропустило часть потока. Наружу нужно
// для диагностики: молчаливое «кадра нет» иначе неотличимо от «нет зеркала».
func (s *Session) ScreenStale() bool {
	s.screenLifeMu.RLock()
	defer s.screenLifeMu.RUnlock()
	sc, _ := s.screen.Load().(*sessionScreen)
	return sc != nil && (sc.stale.Load() || !sc.deliveryAuthoritative.Load())
}

// invalidateScreenDelivery is the conservative boundary for a backend that
// cannot ACK resize in the same ordered lane as output (SSH, direct PTY, or a
// legacy persistent host). Raw bytes remain lossless, but their old/new-grid
// origin cannot be proven, so screen frames and destructive mode pruning stay
// disabled for the lifetime of this sessionScreen. A RIS is insufficient:
// bytes buffered before Resize may only be read afterwards, and generic
// SSH/local transports carry no output/resize ordering metadata. Replacements
// on the same non-ACK transport are initialized raw-only as well.
func (s *Session) invalidateScreenDelivery(minGeneration uint64) {
	s.screenLifeMu.RLock()
	defer s.screenLifeMu.RUnlock()
	sc, _ := s.screen.Load().(*sessionScreen)
	if sc == nil {
		return
	}
	sc.authorityMu.Lock()
	sc.risReauthDisabled.Store(true)
	sc.deliveryAuthoritative.Store(false)
	sc.modeAuthoritative.Store(false)
	sc.minDeliveryGeneration.Store(minGeneration)
	sc.authorityMu.Unlock()
}

// screenAltLiveAt is called with Session.bufMu held. A destructive DEC prune
// is valid only when the authoritative mirror has applied every byte through
// requiredOff. queued==0 alone is insufficient: readLoop scans/advances the
// stream under bufMu and feeds the mirror immediately after unlocking.
func (s *Session) screenAltLiveAt(requiredOff uint64) (alt, live bool) {
	s.screenLifeMu.RLock()
	defer s.screenLifeMu.RUnlock()
	sc, _ := s.screen.Load().(*sessionScreen)
	if sc == nil || sc.stale.Load() || sc.queued.Load() != 0 || !sc.modeAuthoritative.Load() {
		return false, false
	}
	sc.mu.Lock()
	m := sc.mirror
	sc.mu.Unlock()
	if m == nil {
		return false, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	// untrusted: эмулятор пересобран после паники и пуст — «не alt» из него не
	// опровергает живой alt-экран хоста (screen_vtpanic.go).
	if m.closed || m.untrusted || m.appliedOff < requiredOff {
		return false, false
	}
	return m.em.IsAltScreen(), true
}
