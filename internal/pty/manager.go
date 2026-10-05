// Package pty manages interactive pseudo-terminal sessions.
package pty

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"

	"tgcontrol/internal/agenthistory"
)

// 4 MB per session. Codex-подобные TUI перерисовывают экран непрерывно и
// выносят 512 КБ за ~25 с активной работы: блип сети на телефоне дольше этого
// означал, что offset клиента уже вытеснен из кольца → полный reset вместо
// дельты → человек видит, как экран стирается и стробит переигровкой (живая
// жалоба 2026-07-29: «вывод Codex пропадает прямо во время работы»). 4 МБ — это
// ~2-3 минуты такого потока, дельта-резюме переживает обычные мобильные блипы.
const scrollbackSize = 4 * 1024 * 1024

// ptyConn is the backend of a single PTY session: either a local ConPTY
// (*conPTY) or a client to a persistent host process over a named pipe
// (*pipeClient, Windows). Session/readLoop talk only to this interface.
type ptyConn interface {
	Read(p []byte) (int, error)
	Write(p []byte) (int, error)
	Resize(cols, rows int) error
	Close() error
	shellPID() uint32
	currentCWD() (string, error)
}

// orderedResizeConn ACKs a resize in the same decoded lane as PTY output and
// runs afterApply before that lane can deliver the next output frame.
type orderedResizeConn interface {
	ResizeOrdered(cols, rows int, afterApply func()) error
}

type orderedResizeCompletionConn interface {
	ResizeOrderedComplete(cols, rows int, afterApply func(), afterDone func(error)) error
}

// resizeOrderingCapability distinguishes a negotiated ACK barrier from a
// compatibility method on a new client attached to a legacy host.
type resizeOrderingCapability interface {
	resizeOrderingSupported() bool
}

func orderedResizeFor(conn ptyConn) (orderedResizeConn, bool) {
	ordered, ok := conn.(orderedResizeConn)
	if !ok {
		return nil, false
	}
	if capability, ok := conn.(resizeOrderingCapability); ok && !capability.resizeOrderingSupported() {
		return nil, false
	}
	return ordered, true
}

// screenFramesSupported is deliberately stricter than raw terminal delivery.
// With no ordered resize ACK (SSH, local direct PTY, legacy host), output that
// was buffered before a geometry change cannot be assigned to either grid.
// Those transports stay on lossless raw ring/replay and never publish a
// synthetic screen frame that could overwrite the client's correct state.
// nil is allowed for isolated mirror unit tests.
func screenFramesSupported(conn ptyConn) bool {
	if conn == nil {
		return true
	}
	_, ok := orderedResizeFor(conn)
	return ok
}

// hostRecord persists what's needed to re-attach to a live pty-host after a
// remotai restart. Stored per-session in pty.json (Meta.Host). HostPID == 0
// means "no persistent host" (Linux / local backend) — nothing is persisted.
type hostRecord struct {
	HostPID  uint32 `json:"host_pid"`
	PipeName string `json:"pipe"`
	CWD      string `json:"cwd"`
	Shell    string `json:"shell"`
	Created  int64  `json:"created"` // unix ms
	Proto    int    `json:"proto"`
	UID      int64  `json:"uid"`
	// Cols/Rows — размер, с которым терминал РОДИЛСЯ. Переживает перезапуск
	// агента вместе с записью: без него восстановленная сессия не знала бы, к
	// чему возвращаться, когда смотреть перестали, и навсегда оставалась бы в
	// размере последнего зрителя (обычно телефона).
	Cols int `json:"cols,omitempty"`
	Rows int `json:"rows,omitempty"`
}

// SessionInfo is the JSON-safe view of a PTY session.
type SessionInfo struct {
	ID         string `json:"id"`
	CWD        string `json:"cwd"`
	Shell      string `json:"shell"`
	Created    int64  `json:"created"`     // unix ms
	LastActive int64  `json:"last_active"` // unix ms (last output or input)
	Alive      bool   `json:"alive"`
	Name       string `json:"name,omitempty"` // user-set friendly name
	// Group/Sort без omitempty намеренно: по наличию ключей в JSON клиент
	// отличает нового агента (папки поддерживаются) от старого.
	Group     string  `json:"group"`                // user folder in the terminal list ("" = ungrouped)
	Sort      float64 `json:"sort"`                 // manual order in the list (0 = fall back to -created)
	FgProcess string  `json:"fg_process,omitempty"` // foreground process name
	AgentKind string  `json:"agent_kind,omitempty"` // normalized kind for UI
	// AccountID/AccountLabel — аккаунт нейросети, под которым запущен агент
	// ЭТОГО терминала (см. Meta). У соседнего терминала он может быть другим:
	// окружение выдаётся процессу при запуске и дальше не меняется.
	AccountID    string `json:"account_id,omitempty"`
	AccountLabel string `json:"account_label,omitempty"`
	// Status summarises what the terminal is doing for the list UI:
	// "working" (a command/agent is running), "idle" (done, back at prompt),
	// "waiting" (agent asked a recognised question — Hint непустой),
	// "ready" (агент жив и молчит, но вопрос не распознан),
	// "error" (last output had an error), "dead" (process exited).
	// StatusAt (unix ms) — начало этого состояния (когда завершилось / упало /
	// начали ждать). Hint описывает вопрос («Подтвердите: y/n»).
	Status   string `json:"status,omitempty"`
	StatusAt int64  `json:"status_at,omitempty"`
	// DiedAt lets clients show the honest five-minute scrollback window and
	// offer restart/export actions before the reaper removes the session.
	DiedAt int64 `json:"died_at,omitempty"`
	// HostAlive — терминал числится завершённым, но его процесс ЖИВ: оборвалась
	// только связь с ним (см. reattach_live.go). Для человека это разные новости:
	// «работа закончилась» и «я потерял с ней связь». Предлагать перезапуск во
	// втором случае нельзя — получится второй терминал в той же папке, пока
	// первый продолжает работать.
	HostAlive bool   `json:"host_alive,omitempty"`
	Hint      string `json:"hint,omitempty"`
	// HintKind — тип вопроса (yes_no / choice / enter / text), по нему клиент
	// выбирает кнопки ответа. Без omitempty намеренно, как Group/Sort: наличие
	// ключа = агент новый и умеет типы, пустая строка = тип не распознан.
	HintKind string `json:"hint_kind"`
	// HintOptions — подписи пунктов распознанного меню в порядке цифр
	// (индекс 0 = «1»). Без них кнопки ответа были голыми цифрами: что выберет
	// «2», человек мог узнать, только прокрутив вывод, а на телефоне меню к
	// этому моменту обычно уже под клавиатурой.
	HintOptions []string `json:"hint_options,omitempty"`
	// Viewers — сколько экранов сейчас смотрит в этот терминал (телефон + окно
	// на ПК + Telegram — это три). Нужно клиенту, чтобы объяснить, почему TUI
	// агента вдруг перерисовался под чужую ширину: размер PTY общий на всех и
	// держится по САМОМУ УЗКОМУ зрителю (см. Session.ResizeFor).
	Viewers int `json:"viewers"`
	// Kind — «где живёт» сессия: "" (локальный шелл ПК) или "ssh" (удалённый
	// сервер через агента-бастион). Раньше SSH-сессия узнавалась только по
	// тексту заголовка, а после переименования — вообще никак, поэтому половина
	// действий экрана (открыть папку ПК, «Открыть на ПК», загрузка файла в TEMP
	// компьютера) молча уходила не на тот хост.
	Kind         string `json:"kind"`
	SSHHost      string `json:"ssh_host,omitempty"`
	SSHHostID    string `json:"ssh_host_id,omitempty"`
	SSHUser      string `json:"ssh_user,omitempty"`
	SSHPort      int    `json:"ssh_port,omitempty"`
	SSHProxyJump string `json:"ssh_proxy_jump,omitempty"`
	Remote       bool   `json:"remote,omitempty"`
	// Sleep — агент этого терминала усыплён человеком (agent_sleep.go).
	Sleep *SleepRecord `json:"sleep,omitempty"`
}

// Session is a single PTY instance with scrollback and fan-out.
type Session struct {
	ID           string
	CWD          string
	Shell        string
	Created      time.Time
	UID          int64 // owning user (for routing events)
	SSHPort      int
	SSHHostID    string
	SSHProxyJump string

	// pty — backend сессии. Под ptyMu, потому что при обрыве связи с живым
	// pty-host его ПОДМЕНЯЮТ на новое соединение прямо во время работы (см.
	// Manager.reattachLive), а читают/пишут в него параллельно readLoop, WS-
	// хендлеры (Write/Resize) и опрос состояния.
	ptyMu sync.RWMutex
	pty   ptyConn
	// Serializes complete paste operations with keys from every viewer/API.
	inputMu       sync.Mutex
	codexActivity codexActivity
	historyMu     sync.Mutex
	historySource agenthistory.Source
	// historyAt — когда пришёл хук с historySource (см. sleepSessionID).
	historyAt time.Time

	// Scrollback buffer — circular, keeps last scrollbackSize bytes.
	bufMu sync.Mutex
	buf   []byte
	// totalBytes — сколько байт сессия вывела ВСЕГО (монотонно растёт, под bufMu).
	// Это «offset» для resume: клиент помнит, до какого offset он применил вывод;
	// на реконнекте сервер отдаёт только хвост buf после этого offset.
	// Считаются СЫРЫЕ байты потока PTY, включая служебные последовательности:
	// маркеры разметки команд OSC 133 (ST-10) занимают offset наравне с
	// выводом и досылаются в resume, но в кадр восстановления из зеркала экрана
	// (screen.go) не попадают — блоки до снапшота клиент честно помечает
	// недоступными (I-11).
	totalBytes uint64
	// epoch — клиентский идентификатор непрерывности потока. Он обычно
	// равен hostEpoch, но меняется и при прыжке replay_start внутри того
	// же host: мобильному xterm нужен reset, а не resumed поверх старого
	// экрана. Под bufMu.
	epoch string
	// geometryAt/geometryEpoch — позиция потока (totalBytes) и epoch последней
	// СМЕНЫ размера PTY. Байты после неё нарисованы под другую сетку, и клиент,
	// ушедший раньше, дописать их поверх своего экрана не может: 23.09.2026
	// телефон вернулся с resume после того, как терминал 13 минут смотрели с ПК
	// шириной 238, проиграл эти байты в 48 колонок и показал Claude без строки
	// ввода и с пропавшими строками, пока человек не вышел и не зашёл заново.
	// Под bufMu.
	geometryAt    uint64
	geometryEpoch string
	// hostEpoch — стабильный stream_epoch из Hello host. Он отделён от
	// client epoch: после ring gap клиентская continuity меняется, но
	// следующий ClientHello.known всё ещё должен назвать родной host epoch.
	hostEpoch string

	// DEC private-mode state (см. decTracker в decmodes.go): на полном
	// reset-resync включающая последовательность (alt-screen ?1049h, мышь
	// ?1006h) обычно уже вытеснена из 512 КБ буфера — клиент после term.reset()
	// остаётся в обычном буфере без mouse-tracking, и проброс прокрутки в
	// full-screen TUI (Claude Code) молча ломается. Храним текущие режимы и
	// до-сылаем их в голове reset-payload. Под bufMu (как buf). Изменения
	// write-through персистятся в pty.json (см. readLoop), а на reattach набор
	// сидируется от pty-host (HelloMsg.Modes) или из pty.json.
	dec *decTracker
	// altScreen — зеркало dec.altActive() СНАРУЖИ bufMu. Размер считается под
	// viewMu, и лезть из-под него в bufMu значило бы завести второй порядок
	// захвата замков рядом с уже существующим bufMu→subMu. Атомик читается без
	// замка вовсе, поэтому порядок остаётся один.
	altScreen atomic.Bool

	// screen — зеркало экрана (*sessionScreen, см. session_screen.go). Именно
	// оно отвечает на вопрос «что человек увидит, открыв терминал сейчас»:
	// восстановить экран агента из хвоста сырого потока невозможно, потому что
	// агенты рисуют диффом по ячейкам и полного кадра в потоке нет вовсе.
	screen atomic.Value
	// screenLifeMu защищает swap/close зеркала от параллельного
	// feedScreenAt. Нужен для безопасной пересборки stale mirror.
	screenLifeMu sync.RWMutex
	// screenGeometryMu is the capture/send lease for synthetic screen frames.
	// Every resize request and completion advances screenGeometryRevision;
	// pending requests suppress capture. A WS writer holds RLock across the
	// final revision check and frame write, so a resize cannot overtake it.
	screenGeometryMu       sync.RWMutex
	screenGeometryRevision uint64
	screenResizePending    int
	screenGeometryWake     chan struct{}
	// beforeScreenGeometryWaitReturn is a deterministic test hook for the
	// pending-capture / ACK-completion seam. Production leaves it nil.
	beforeScreenGeometryWaitReturn func()
	// outputApplyMu defines the observable output/resize boundary for backends
	// without a protocol ACK (SSH, direct local PTY and legacy hosts). Read is
	// intentionally outside this lock; once it returns, ring+fanout+screen feed
	// are one side of the same lane as Resize+ordered screen marker.
	outputApplyMu sync.Mutex
	// outputGeneration records the non-ACK resize boundary on queued mirror
	// chunks. It is only an additional guard: transport/kernel buffering means
	// even a Read begun later may return old-grid bytes, so non-ACK invalidation
	// remains permanent until the whole screen mirror is replaced.
	outputGeneration atomic.Uint64
	// beforeOutputApply is a deterministic test hook for the Read-returned /
	// observable-commit seam. Production always leaves it nil.
	beforeOutputApply func()

	// Subscribers (connected WebSocket clients).
	subMu sync.RWMutex
	subs  map[chan []byte]*subState

	// Зрители терминала: по одному на открытый экран (телефон, окно на ПК,
	// Telegram). Терминалы персистентны, и к одной сессии подключаются сколько
	// угодно клиентов, а PTY у неё ОДИН — раньше каждый клиент слал свой resize,
	// и последний пришедший переставлял общий размер: открыл на телефоне тот же
	// терминал, где на ПК работает Claude Code в 200 колонок, — TUI перерисовался
	// в 60 и на большом экране остался узкой колонкой. Держим размер по самому
	// узкому зрителю (см. resizeToViewers) и отдаём их число наружу.
	viewMu sync.Mutex
	// viewApplyMu serializes aggregate recompute → backend/mirror apply →
	// viewCols/viewRows commit. Without it two viewers could finish in reverse
	// order and leave the real geometry different from the recorded aggregate.
	viewApplyMu sync.Mutex
	// viewCommitMu also covers callbacks that arrive after ResizeOrdered already
	// returned a soft timeout. Sequence numbers make a genuinely stale callback
	// unable to overwrite a newer successful geometry.
	viewCommitMu   sync.Mutex
	viewNextSeq    uint64 // under viewMu
	viewAppliedSeq uint64 // under viewMu
	viewers        map[*Viewer]struct{}
	viewerSequence uint64
	sizeRevision   uint64
	sizeOwner      *Viewer
	sizeLeaseUntil time.Time
	sizeLeaseTimer *time.Timer
	viewCols       int // последний ПРИМЕНЁННЫЙ к PTY размер (чтобы не дёргать зря)
	viewRows       int
	// bornCols/bornRows — размер, с которым сессия РОДИЛАСЬ. К нему возвращаемся,
	// когда смотреть перестали совсем: иначе терминал навсегда оставался в
	// размере последнего зрителя. Живой случай владельца (04.08.2026): 402 раза
	// из 408 агент получал от клиентов 48×33 — телефон, — и сессия Kimi жила в
	// 48 колонках даже когда человек сидел за компьютером.
	bornCols int
	bornRows int
	// growTimer — отложенное УВЕЛИЧЕНИЕ размера (см. resizeToViewers).
	growTimer *time.Timer
	// persistView — записать применённый размер в pty.json (Meta.ViewCols).
	// Устанавливается создателем сессии ДО readLoop; зовётся ВНЕ viewMu
	// (дисковый I/O). Без персиста рестарт агента забывал размер, зеркало
	// рождалось в born-геометрии и переваривало реплей чужой ширины в кашу.
	persistView func(cols, rows int)

	// Heuristic event-detector state (lazily allocated).
	evMu sync.Mutex
	ev   *detectorState

	// waitAt — начало ОТКРЫТОГО эпизода ожидания (нулевое время — вопроса на
	// экране нет). Зеркало detectorState.episode/episodeAt под evMu: поля
	// детектора пишет только его горутина и мьютексом не защищены, а сверять
	// «отвечаю на тот вопрос, который видел» приходится из HTTP-горутины
	// (Manager.CheckPrompt).
	waitAt time.Time

	// Closed when the PTY read loop ends (process exited / pipe broken).
	done     chan struct{}
	doneOnce sync.Once

	// diedAt records when the read loop ended, so the reaper can drop dead
	// sessions after a grace period (keeping scrollback briefly for export).
	diedMu sync.Mutex
	diedAt time.Time

	// closedByUser — терминал закрыл человек (Manager.Close). Такая смерть НЕ
	// исход: в журнал «что случилось без меня» она бы попала как «терминал
	// завершился», хотя человек закрыл его сам.
	closedByUser atomic.Bool

	// agentKind — последний известный вид агента (claude/codex/…) под evMu.
	// У мёртвой сессии foreground-процесса уже не спросить, а журналу исходов
	// надо сказать, ЧТО именно упало. Обновляется в infoOf.
	agentKind string

	// Серия восстановлений связи без прогресса: сколько раз подряд связь с
	// pty-host поднялась и сразу оборвалась, не дав ни байта. Нужна, чтобы
	// восстановление не превращалось в горячий цикл (см. reattach_live.go).
	reattachMu    sync.Mutex
	reattachCount int
	reattachAt    time.Time
}

// noteReattachAttempt отмечает очередную попытку восстановить связь и говорит,
// стоит ли её делать. false — связь рвётся сразу после каждого восстановления,
// и продолжать бессмысленно.
//
// Внутри серии выдерживается пауза: пока обрыв мгновенный, дёргать хост чаще
// раза в секунду вредно (именно так лог и вырос до 35 МБ за восемь минут).
func (s *Session) noteReattachAttempt() bool {
	s.reattachMu.Lock()
	last := s.reattachAt
	s.reattachAt = time.Now()
	if !last.IsZero() && time.Since(last) > reattachBurstReset {
		s.reattachCount = 0 // связь держалась долго — серия закончилась
	}
	s.reattachCount++
	count := s.reattachCount
	s.reattachMu.Unlock()

	if count > reattachLiveBurst {
		return false
	}
	if count > 1 {
		time.Sleep(reattachBurstPause)
	}
	return true
}

// reattachBurst — какой по счёту раз подряд мы восстанавливаем связь (1 —
// первый). По нему решается, писать ли строку в лог.
func (s *Session) reattachBurst() int {
	s.reattachMu.Lock()
	defer s.reattachMu.Unlock()
	return s.reattachCount
}

// noteReattachProgress сбрасывает серию: после восстановления связь ДЕРЖИТСЯ и
// приносит данные, значит следующий обрыв — новая история.
//
// «Пришли данные» само по себе доказательством не является, и это стоило нам
// сторожа. Первое, что отдаёт pty-host любому подключившемуся, — снапшот всего
// своего буфера; байты есть всегда, счётчик обнулялся мгновенно, потолок
// reattachLiveBurst не достигался никогда, паузы не было — то есть защита от
// горячего цикла, написанная в 2.48.3 ровно против залива логов, была
// декоративной. Поэтому серию закрывает только связь, прожившая
// reattachBurstReset: снапшот приезжает за миллисекунды и под условие не
// попадает, а реальная работа терминала — попадает сразу.
func (s *Session) noteReattachProgress() {
	s.reattachMu.Lock()
	if s.reattachCount > 0 && !s.reattachAt.IsZero() && time.Since(s.reattachAt) > reattachBurstReset {
		s.reattachCount = 0
	}
	s.reattachMu.Unlock()
}

// conn возвращает текущий backend сессии. Через него ходят ВСЕ: соединение с
// pty-host может смениться на живой сессии (см. Manager.reattachLive).
func (s *Session) conn() ptyConn {
	s.ptyMu.RLock()
	defer s.ptyMu.RUnlock()
	return s.pty
}

// setConn подменяет backend сессии и ЗАКРЫВАЕТ прежний.
//
// Закрывать обязательно: подмена происходит только в reattachLive, после
// ошибки чтения, и прежняя труба остаётся открытым дескриптором. В горячем
// цикле переподключений это утечка на каждый круг — при типичном ulimit 1024
// падает не терминал, а весь агент.
func (s *Session) setConn(c ptyConn) {
	reset := s.adoptHostStream(c)
	s.ptyMu.Lock()
	prev := s.pty
	s.pty = c
	s.ptyMu.Unlock()
	if prev != nil && prev != c {
		_ = prev.Close()
	}
	if reset {
		log.Printf("[PTY] id=%s host stream/baseline changed — ring reset to absolute host scale", s.ID)
	}
	// Новый backend — новая переигровка буфера: сигнал о её границе нужен и
	// здесь, иначе после reattachLive (автообновление агента) зеркало снова
	// запишет чужое прошлое себе в историю.
	s.watchReplayEnd()
}

// adoptHostStream переводит Session offset в абсолютную шкалу host.
// Legacy host без optional-контракта сбрасывается отдельно в
// reattachLive: known=0 и full replay в чистые ring/DEC/mirror.
func (s *Session) adoptHostStream(c ptyConn) (reset bool) {
	state, ok := streamStateOf(c)
	if !ok || state.Epoch == "" || state.ReplayStart > state.Produced {
		// A legacy host has no absolute stream contract, but if it does expose
		// modes through a transitional Hello, merge them before any later wake.
		s.seedModes(hostModesOf(c))
		return false
	}
	modes := hostModesOf(c)
	s.bufMu.Lock()
	hostChanged := s.hostEpoch != state.Epoch
	baselineChanged := s.totalBytes != state.ReplayStart
	reset = hostChanged || baselineChanged
	clientEpoch := state.Epoch
	if !hostChanged && baselineChanged {
		// Host тот же, но между sent и replay_start нет байтов.
		// Для WS это новая continuity; для host known — прежний epoch.
		clientEpoch = randomID()
	}
	if !reset {
		s.dec.seed(modes)
		s.altScreen.Store(s.dec.altActive())
	}
	s.bufMu.Unlock()
	if reset {
		s.resetStreamContinuityForHostModesForConn(clientEpoch, state.Epoch, state.ReplayStart, modes, c)
	}
	return reset
}

func hostModesOf(c ptyConn) []int {
	type modeAware interface{ hostModes() []int }
	if aware, ok := c.(modeAware); ok {
		return aware.hostModes()
	}
	return nil
}

func streamStateOf(c ptyConn) (hostStreamState, bool) {
	type streamAware interface {
		streamState() (hostStreamState, bool)
	}
	a, ok := c.(streamAware)
	if !ok {
		return hostStreamState{}, false
	}
	return a.streamState()
}

// resetStreamContinuity — честный reset при смене epoch, прыжке
// replay_start или legacy-host reconnect. Старые ring, DEC и mirror не
// имеют права склеиваться с новым replay. Живые WS-подписчики
// будятся и получают marker с новым epoch до байтов replay.
func (s *Session) resetStreamContinuity(epoch string, baseline uint64) {
	s.resetStreamContinuityForHost(epoch, epoch, baseline)
}

func (s *Session) resetStreamContinuityForHost(epoch, hostEpoch string, baseline uint64) {
	s.resetStreamContinuityForHostModes(epoch, hostEpoch, baseline, nil)
}

func (s *Session) resetStreamContinuityForHostModes(epoch, hostEpoch string, baseline uint64, modes []int) {
	s.resetStreamContinuityForHostModesHook(epoch, hostEpoch, baseline, modes, nil)
}

// resetStreamContinuityForHostModesForConn is used during setConn before the
// incoming backend has replaced s.pty. Screen authority must follow that new
// backend's negotiated resize capability, not the dead connection's concrete
// type/capability.
func (s *Session) resetStreamContinuityForHostModesForConn(epoch, hostEpoch string, baseline uint64, modes []int, conn ptyConn) {
	s.resetStreamContinuityForHostModesHookWithFrames(epoch, hostEpoch, baseline, modes, nil, screenFramesSupported(conn))
}

// resetStreamContinuityForHostHook exists so the lock seam has a deterministic
// regression test. beforeScreen runs under bufMu immediately before the screen
// swap; production always passes nil via resetStreamContinuityForHost.
func (s *Session) resetStreamContinuityForHostHook(epoch, hostEpoch string, baseline uint64, beforeScreen func()) {
	s.resetStreamContinuityForHostModesHook(epoch, hostEpoch, baseline, nil, beforeScreen)
}

func (s *Session) resetStreamContinuityForHostModesHook(epoch, hostEpoch string, baseline uint64, modes []int, beforeScreen func()) {
	s.resetStreamContinuityForHostModesHookWithFrames(epoch, hostEpoch, baseline, modes, beforeScreen, screenFramesSupported(s.conn()))
}

func (s *Session) resetStreamContinuityForHostModesHookWithFrames(epoch, hostEpoch string, baseline uint64, modes []int, beforeScreen func(), framesSupported bool) {
	if epoch == "" {
		epoch = randomID()
	}
	s.bufMu.Lock()
	s.subMu.RLock()
	s.buf = s.buf[:0]
	s.totalBytes = baseline
	s.epoch = epoch
	s.hostEpoch = hostEpoch
	s.dec = newDecTracker()
	// Seed authoritative Hello modes before waking subscribers. ResyncFrom and
	// ModeReassertSeq both take bufMu, so a writer woken below can only observe
	// the new epoch together with its current alt/mouse state.
	s.dec.seed(modes)
	s.altScreen.Store(s.dec.altActive())
	modeSeed := s.dec.reassertSeq()
	for _, st := range s.subs {
		st.markLagged()
	}
	s.subMu.RUnlock()
	if beforeScreen != nil {
		beforeScreen()
	}
	// bufMu→screenLifeMu is the same order as recoverScreen. The swap must
	// happen before bufMu is released: otherwise readLoop can append/feed the
	// first bytes of the NEW replay into the old mirror, after which a blank
	// replacement would silently discard them while still claiming fresh.
	s.screenLifeMu.Lock()
	retired, retiredMirror := s.resetScreenEmptyLocked(modeSeed, framesSupported)
	s.screenLifeMu.Unlock()
	s.bufMu.Unlock()
	finishScreenRetire(retired, retiredMirror)
}

// watchReplayEnd подписывает сессию на границу переигровки буфера, если
// транспорт умеет её различать (труба/сокет хоста умеют; локальный PTY и SSH
// переигровки не имеют вовсе).
func (s *Session) watchReplayEnd() {
	type replayAware interface{ setReplayEnd(func()) }
	if c, ok := s.conn().(replayAware); ok {
		c.setReplayEnd(func() { s.dropReplayScrollback() })
	}
}

// rememberAgentKind запоминает непустой вид агента для журнала исходов.
func (s *Session) rememberAgentKind(kind string) {
	if kind == "" {
		return
	}
	s.evMu.Lock()
	s.agentKind = kind
	s.evMu.Unlock()
}

// lastAgentKind — последний известный вид агента. Нужен и журналу исходов, и
// карточке терминала, потерянного при перезагрузке («здесь работал Claude Code»).
func (s *Session) lastAgentKind() string {
	s.evMu.Lock()
	defer s.evMu.Unlock()
	return s.agentKind
}

// deadSince reports when the session died and whether it is dead at all.
func (s *Session) deadSince() (time.Time, bool) {
	if s.IsAlive() {
		return time.Time{}, false
	}
	s.diedMu.Lock()
	defer s.diedMu.Unlock()
	return s.diedAt, !s.diedAt.IsZero()
}

// evState returns the (lazy) detector state for this session.
func (s *Session) evState() *detectorState {
	s.evMu.Lock()
	defer s.evMu.Unlock()
	if s.ev == nil {
		s.ev = newDetectorState()
	}
	return s.ev
}

// setEvent records the latest heuristic event so the PTY list can render a
// status badge. Guarded by evMu (consistent with List's reads of lastEvt).
//
// options — подписи пунктов распознанного меню (см. SessionInfo.HintOptions).
// Аргумент вариативный намеренно: у остальных событий меню не бывает, и все
// прежние вызовы остались как были.
func (s *Session) setEvent(kind EventKind, at time.Time, hint, hintKind string, options ...string) {
	s.evMu.Lock()
	if s.ev == nil {
		s.ev = newDetectorState()
	}
	s.ev.lastEvt = sessEvent{kind: kind, at: at, hint: hint, hintKind: hintKind, options: options}
	s.evMu.Unlock()
}

// errorHintLimit — сколько символов строки ошибки уезжает в событие. Строка
// печатается в уведомлении Telegram: одной хватает, чтобы отличить «npm ERR!
// code E404» от красного FAIL в тестах, а весь стек там всё равно не читают.
const errorHintLimit = 160

// errorHintFrom — первая ЗНАЧИМАЯ строка чанка, на которую сработал детектор
// ошибок (errorRe), очищенная от ANSI и управляющих символов и обрезанная до
// errorHintLimit. Пустая строка = показывать нечего.
func errorHintFrom(chunk []byte) string {
	return errorHintIn(ansiRe.ReplaceAll(chunk, nil))
}

// errorHintIn — то же по УЖЕ очищенному от ANSI куску вывода (горячий путь
// readLoop снимает ANSI один раз на чанк, второй проход не нужен).
//
// Шумовые строки интерфейса агентов (errorNoiseRe) пропускаются, а не
// возвращаются: сработка на них — не новость, и если в чанке нет ничего
// другого, детектор обязан промолчать вовсе.
func errorHintIn(clean []byte) string {
	if hint := firstMatchingLine(clean, errorRe); hint != "" {
		return hint
	}
	// Смерть от нехватки памяти слова «error» не содержит вовсе (см. oomRe):
	// ищем её вторым проходом, чтобы обычные ошибки по-прежнему были первыми.
	return firstMatchingLine(clean, oomRe)
}

// firstMatchingLine — первая значимая строка, на которую сработало правило.
func firstMatchingLine(clean []byte, re *regexp.Regexp) string {
	for _, loc := range re.FindAllIndex(clean, -1) {
		line := clean[loc[0]:]
		if end := bytes.IndexAny(line, "\r\n"); end >= 0 {
			line = line[:end]
		}
		text := normalizeLine(string(line))
		if text == "" || errorNoiseRe.MatchString(text) {
			continue
		}
		if runes := []rune(text); len(runes) > errorHintLimit {
			text = strings.TrimRight(string(runes[:errorHintLimit]), " ") + "…"
		}
		return text
	}
	return ""
}

// normalizeLine приводит строку вывода к виду, в котором её можно показать
// человеку и сравнить с другой такой же: битый UTF-8 (чанк рвётся по байтам —
// иначе в Telegram уедет «�») выброшен, табуляция стала пробелом, прочие
// управляющие (\b и компания от перерисовки прогресс-баров) убраны, пробелы
// схлопнуты.
func normalizeLine(line string) string {
	text := strings.ToValidUTF8(line, "")
	text = strings.Map(func(r rune) rune {
		if r == '\t' {
			return ' '
		}
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, text)
	return strings.Join(strings.Fields(text), " ")
}

// syncWaitEpisode зеркалит состояние эпизода ожидания в сессию (см. waitAt).
// Зовётся детектором на КАЖДОМ тике: эпизод закрывается не мгновенно, а после
// нескольких спокойных тиков, и всё это время вопрос ещё на экране.
func (s *Session) syncWaitEpisode(d *detectorState) {
	var at time.Time
	if strings.HasPrefix(d.episode, waitEpisodePrefix) {
		at = d.episodeAt
	}
	s.evMu.Lock()
	s.waitAt = at
	s.evMu.Unlock()
}

// PromptVerdict — результат сверки «человек отвечает на ТОТ вопрос, который
// видел» (Manager.CheckPrompt).
type PromptVerdict int

const (
	// PromptUnknown — убедиться нечем: открытого эпизода ожидания у сессии нет
	// (агент печатает дольше эпизода, освободился, remotai перезапускался).
	// Это НЕ «спрашивает о другом» — доказательства смены вопроса нет.
	PromptUnknown PromptVerdict = iota
	// PromptSame — на экране тот самый вопрос, штамп эпизода совпал.
	PromptSame
	// PromptChanged — доказанная смена: эпизод ожидания открыт, но начался он в
	// другой момент, то есть вопрос уже другой.
	PromptChanged
)

// CheckPrompt сверяет штамп эпизода, который человек видел (expectAt, unix ms),
// с текущим эпизодом ожидания сессии. hint/hintKind — текущий вопрос (для
// сообщения об отказе), best-effort.
//
// Почему не info.Status/StatusAt из GET /api/pty: статус уходит в "working" на
// любые ~3 секунды после ЛЮБОГО байта вывода — включая перерисовку того же
// самого вопроса, — а StatusAt там 0. Сравнение с нулём давало 409
// prompt_changed («агент уже спрашивает о другом») на вопрос, который не
// менялся. Эпизод же держится, пока вопрос на экране.
func (m *Manager) CheckPrompt(id string, expectAt int64) (v PromptVerdict, hint, hintKind string) {
	m.mu.RLock()
	s := m.sessions[id]
	m.mu.RUnlock()
	if s == nil {
		return PromptUnknown, "", ""
	}
	s.evMu.Lock()
	waitAt := s.waitAt
	var le sessEvent
	if s.ev != nil {
		le = s.ev.lastEvt
	}
	s.evMu.Unlock()

	if waitAt.IsZero() {
		return PromptUnknown, "", ""
	}
	if le.kind == EventWaitingInput {
		hint, hintKind = le.hint, le.hintKind
	}
	if waitAt.UnixMilli() != expectAt {
		return PromptChanged, hint, hintKind
	}
	return PromptSame, hint, hintKind
}

// ── Журнал исходов: «что случилось без меня» ──────────────────────
//
// Статус "error" гаснет через 5 минут (statusFrom), а сама мёртвая сессия
// удаляется reapLoop'ом тоже через 5 минут — то есть человек, вернувшийся через
// полчаса, не мог узнать НИЧЕГО о том, чем закончилась ночная сборка: главная
// показывала мирную плитку «Продолжить с того места». Журнал живёт отдельно от
// сессий и переживает их удаление: последние outcomeLimit исходов за outcomeTTL.
//
// Только в оперативной памяти (как и сами PTY-сессии): рестарт агента журнал
// теряет — врать про «исход», не зная, что случилось после рестарта, хуже.
const (
	outcomeLimit = 30
	outcomeTTL   = 12 * time.Hour
)

// Машинные коды причин исхода — тексты подставляет клиент.
const (
	// OutcomeReasonOutputError — в выводе распознана ошибка (детектор событий).
	OutcomeReasonOutputError = "output_error"
	// OutcomeReasonExited — шелл вышел сам, терминала больше нет.
	OutcomeReasonExited = "exited"
	// OutcomeReasonDetached — связь с pty-хостом потеряна, шелл мог остаться
	// жив (перезапуск remotai, сломанный pipe): «терминал отвалился», а не
	// «команда закончилась».
	OutcomeReasonDetached = "detached"
)

// Outcome — один исход: чем закончилась сессия или последняя команда в ней.
// Отдаётся в GET /api/pty (поле outcomes) для сводки главной.
type Outcome struct {
	ID        string `json:"id"`
	Name      string `json:"name,omitempty"`  // имя из pty.json на момент исхода
	CWD       string `json:"cwd,omitempty"`   // папка (или ярлык ssh:user@host)
	Shell     string `json:"shell,omitempty"` // "ssh" у SSH-сессии
	Group     string `json:"group,omitempty"` // папка списка терминалов
	AgentKind string `json:"agent_kind,omitempty"`
	// Status — "error" (команда/агент отчитались об ошибке, терминал мог
	// остаться жив) или "dead" (терминал закрылся сам).
	Status string `json:"status"`
	Reason string `json:"reason,omitempty"` // машинный код: см. OutcomeReason*
	At     int64  `json:"at"`               // unix ms — когда исход случился
	// Routine — штатное событие, а не беда: терминал вышел сам (набрали `exit`,
	// доработал скрипт, докер закрыл сессию). Такому не место под заголовком
	// «Требует внимания» — за рабочий день таких карточек набегает пачка, и
	// каждую приходилось гасить руками, а живой вопрос агента тонул между ними.
	// Клиент показывает их нейтральной строкой и гасит сам.
	Routine bool `json:"routine,omitempty"`
	// Hint — сама строка, на которую сработал детектор ошибок. Без неё карточка
	// на главной умела сказать лишь «в выводе мелькнула строка, похожая на
	// ошибку», и проверить это утверждение человеку было нечем: живое событие
	// строку несло, а запись журнала — нет.
	Hint string `json:"hint,omitempty"`
}

// Manager tracks all active PTY sessions.
type Manager struct {
	mu       sync.RWMutex
	sessions map[string]*Session
	meta     *MetaStore
	// sleepMu — выдача и стирание записи сна (agent_sleep.go) одним шагом.
	sleepMu sync.Mutex
	// scrollbacks — хвосты вывода на диске: единственное, что остаётся от
	// работы, когда компьютер перезагрузили (см. restore.go). nil у локального
	// менеджера тестов: там сессии и так живут только в памяти.
	scrollbacks *scrollbackStore

	// outMu/outcomes — журнал исходов (см. выше). Отдельный мьютекс: пишется из
	// readLoop каждой сессии, читается из HTTP-горутины.
	outMu    sync.Mutex
	outcomes []Outcome

	// persistent spawns each session in a separate host process (survives a
	// remotai restart). Disabled for unit tests / smoke (local ConPTY only).
	persistent bool

	bcMu sync.RWMutex
	bc   Broadcaster

	// detectQuestions — распознавать ли вопросы агента по экрану («ждёт ответа»
	// + кнопки 1/2/3). ВЫКЛЮЧЕНО по умолчанию: см. config.DetectAgentQuestions —
	// текст на экране не отличает рассказ о меню от самого меню, и терминал
	// объявлял вопрос там, где агент просто печатал ответ про вопросы.
	detectQuestions atomic.Bool

	// shellIntegration — запускать ли НОВЫЕ терминалы с разметкой команд
	// OSC 133 (ST-10, см. shell_integration.go). ВЫКЛЮЧЕНО по умолчанию
	// (config.ShellIntegration): владелец включает после проверки на своём
	// устройстве. Уже запущенные шеллы переключатель не трогает.
	shellIntegration atomic.Bool

	// sumHook — движок понятных уведомлений (см. SetSummaryHook). Опционален:
	// без ключа OpenRouter и с выключенной настройкой его просто нет.
	sumMu   sync.RWMutex
	sumHook func(SummaryEvent)
}

// SetQuestionDetection включает/выключает распознавание вопросов агента.
// Зовётся из web-сервера при старте и при смене настройки: перезапуск агента
// ради переключателя человеку не нужен.
func (m *Manager) SetQuestionDetection(on bool) {
	m.detectQuestions.Store(on)
}

// QuestionDetection — текущее состояние (для отдачи клиенту в настройках).
func (m *Manager) QuestionDetection() bool {
	return m.detectQuestions.Load()
}

// SetShellIntegration включает/выключает разметку команд OSC 133 для новых
// терминалов (ST-10). Зовётся из web-сервера при старте и при смене
// настройки, как SetQuestionDetection.
func (m *Manager) SetShellIntegration(on bool) {
	m.shellIntegration.Store(on)
}

// ShellIntegration — текущее состояние переключателя.
func (m *Manager) ShellIntegration() bool {
	return m.shellIntegration.Load()
}

// SummaryKind — повод для понятного уведомления (см. SummaryEvent).
type SummaryKind string

const (
	// SummaryFinished — агент отработал эпизод и затих.
	SummaryFinished SummaryKind = "finished"
	// SummaryQuestion — на экране КАНДИДАТ в вопрос. Именно кандидат: по тексту
	// экрана рассказ о меню от самого меню не отличить (2.49.4), и решать,
	// вопрос это или нет, будет модель — здесь мы только замечаем повод.
	SummaryQuestion SummaryKind = "question"
)

// SummaryEvent — материал для выжимки: что за терминал и что в нём было.
// Отдаётся хуку, который ставит web-сервер; сам pty про сеть, ключи и настройки
// ничего не знает.
type SummaryEvent struct {
	Kind      SummaryKind
	PtyID     string
	UID       int64
	Name      string // имя терминала, как его назвал человек
	CWD       string // папка — это и есть «по какому проекту»
	AgentKind string
	Duration  time.Duration // длительность эпизода (только у finished)
	Hint      string        // распознанный кандидат в вопрос (только у question)
	// Tail — СЫРОЙ хвост вывода: чистит и маскирует его получатель
	// (internal/agentsummary), потому что это его ответственность целиком.
	Tail []byte
}

// SetSummaryHook подключает движок понятных уведомлений. nil — отключить.
//
// Хук обязан возвращаться немедленно: его зовут из горутины детектора, которая
// тикает раз в секунду по ВСЕМ сессиям, и любое ожидание там останавливает
// детекцию на всех терминалах сразу.
func (m *Manager) SetSummaryHook(fn func(SummaryEvent)) {
	m.sumMu.Lock()
	m.sumHook = fn
	m.sumMu.Unlock()
}

// fireSummary отдаёт повод хуку, если он есть.
func (m *Manager) fireSummary(ev SummaryEvent) {
	m.sumMu.RLock()
	fn := m.sumHook
	m.sumMu.RUnlock()
	if fn != nil {
		fn(ev)
	}
}

// NewManager creates an empty manager with a persistent metadata store. PTY
// sessions are spawned in persistent host processes (see Reattach).
func NewManager() *Manager {
	m := &Manager{
		sessions:    make(map[string]*Session),
		meta:        NewMetaStore(),
		scrollbacks: newScrollbackStore(),
		persistent:  true,
	}
	go m.reapLoop()
	go m.scrollbackLoop()
	// Только у персистентного менеджера: у локального ConPTY хоста нет, и
	// возвращать связь не к чему (см. relinkLoop).
	go m.relinkLoop()
	return m
}

// NewLocalManager builds a manager that creates sessions with a local
// in-process ConPTY (no host process / no persistence) — used by unit tests and
// pty-smoke so they don't spawn detached host processes.
func NewLocalManager() *Manager {
	m := &Manager{
		sessions: make(map[string]*Session),
		meta:     NewMetaStore(),
	}
	go m.reapLoop()
	return m
}

// newSession allocates a Session around a ready backend. Used by Create and by
// the platform reattach path.
func newSession(id, cwd, shell string, uid int64, conn ptyConn, created time.Time) *Session {
	s := &Session{
		ID:      id,
		CWD:     cwd,
		Shell:   shell,
		Created: created,
		UID:     uid,
		pty:     conn,
		buf:     make([]byte, 0, scrollbackSize),
		epoch:   randomID(),
		dec:     newDecTracker(),
		subs:    make(map[chan []byte]*subState),
		viewers: make(map[*Viewer]struct{}),
		done:    make(chan struct{}),
	}
	// Персистентный host живёт дольше Session. Сидируем absolute
	// baseline до первого Read, чтобы snapshot не начинался с нуля.
	s.adoptHostStream(conn)
	return s
}

// Reattach reconnects to live pty-host processes recorded in the store. Called
// once at startup (web server). No-op on platforms without persistent hosts.
func (m *Manager) Reattach() { reattachHosts(m) }

// reapLoop periodically drops sessions that have been dead for longer than the
// grace period, so Manager.sessions doesn't grow for the life of the process.
// Dead sessions are kept briefly so their scrollback can still be exported.
func (m *Manager) reapLoop() {
	const grace = 5 * time.Minute
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		var toClose []*Session
		m.mu.Lock()
		for id, sess := range m.sessions {
			if t, dead := sess.deadSince(); dead && time.Since(t) > grace {
				toClose = append(toClose, sess)
				delete(m.sessions, id)
			}
		}
		m.mu.Unlock()
		for _, sess := range toClose {
			log.Printf("[PTY] reaping dead id=%s", sess.ID)
			// Forget the store entry only if the shell truly exited. If the
			// pipe merely broke while the host is still alive, keep the record
			// so the next restart can re-attach.
			if m.meta != nil && sess.shouldForget() {
				m.meta.Forget(sess.ID)
				if m.scrollbacks != nil {
					m.scrollbacks.forget(sess.ID)
				}
			}
			sess.close()
		}
	}
}

// Как часто проверяем, не ждёт ли живой хост возврата связи. 15 секунд —
// компромисс: человек не успевает решить, что терминал потерян навсегда, а
// стоимость круга близка к нулю (обращение к процессу по PID; дозвон делается
// только к тем, у кого сессии нет).
const relinkEvery = 15 * time.Second

// relinkLoop возвращает связь с терминалами, чей ХОСТ ЖИВ, а сессия умерла.
//
// ⚠ ЗАЧЕМ ОТДЕЛЬНАЯ ПЕТЛЯ. reattachLive работает только пока жив readLoop, и
// его терпение ограничено (reattach_live.go): не поднялась связь за отведённое
// время — сессия становится мёртвой. Дальше до этой правки не происходило
// НИЧЕГО: единственным способом вернуть работающий терминал был перезапуск
// приложения, потому что реаттач по записям делался ровно один раз, на старте
// (reattachHosts). Живой случай владельца (17.08.2026): сессия «Клубная» с
// работающим внутри Claude Code была недостижима больше пяти часов, хотя её
// host_pid=18052 всё это время жил и держал свою трубу.
//
// Тот же круг чинит и вторую половину случая: пока связь не вернулась,
// терминал честно показан как «связь потеряна» (host_alive), а не выдан за
// работающий. Врать интерфейсом ради бесшовности нельзя — на такой карточке
// человек нажимает «Открыть в этой папке» и заводит ВТОРОЙ терминал поверх
// работающего первого.
func (m *Manager) relinkLoop() {
	ticker := time.NewTicker(relinkEvery)
	defer ticker.Stop()
	for range ticker.C {
		m.relinkOnce()
	}
}

func (m *Manager) relinkOnce() {
	if m == nil || m.meta == nil {
		return
	}
	for id, rec := range m.meta.AllHosts() {
		if !m.needsRelink(id) {
			continue
		}
		// Сначала спрашиваем процесс, и только потом дозваниваемся: дозвон к
		// мёртвому хосту стоит секунды ожидания на каждом круге и каждом
		// терминале, а вопрос про PID — микросекунды.
		if !processAlive(rec.HostPID) {
			continue
		}
		if _, err := m.ReattachSession(id); err != nil {
			// Занято или не отозвался — повторим на следующем круге. Молчим:
			// иначе строка «не вышло» писалась бы каждые 15 секунд на каждый
			// потерянный терминал и утопила бы лог (грабля 2026-07-30).
			continue
		}
		log.Printf("[PTY] relink: связь с терминалом id=%s вернулась сама (host_pid=%d)", id, rec.HostPID)
	}
}

// needsRelink — этому терминалу действительно нужно вернуть связь.
//
// Три случая, когда НЕ нужно, и каждый стоил бы дорого:
//   - терминал не пережил перезагрузку ПК: он ждёт Restore (нового процесса), а
//     не связи со старым;
//   - сессия жива — возвращать нечего;
//   - шелл вышел САМ либо терминал закрыл человек: хост доживает свой
//     deathGrace, и «возврат» поднял бы карточку завершённой работы обратно.
func (m *Manager) needsRelink(id string) bool {
	if m.meta.Get(id).Lost != nil {
		return false
	}
	m.mu.RLock()
	sess := m.sessions[id]
	m.mu.RUnlock()
	if sess == nil {
		// Сессию уже прибрал reapLoop, а запись хоста осталась — он и оставляет
		// её ровно для этого случая (см. reapLoop выше).
		return true
	}
	if sess.IsAlive() {
		return false
	}
	return !sess.shouldForget() && !sess.closedByUser.Load()
}

// Как часто хвост вывода уезжает на диск. Реже — потеряется последняя минута
// работы агента; чаще — лишняя запись на диск в терминале, где никто не
// печатает. Пишется только то, что изменилось (см. scrollbackStore.save).
const scrollbackFlushEvery = 15 * time.Second

// scrollbackLoop сохраняет хвосты живых терминалов, чтобы они пережили
// выключение компьютера (обычное или внезапное — ждать graceful shutdown
// нельзя, его может не случиться).
func (m *Manager) scrollbackLoop() {
	if m.scrollbacks == nil {
		return
	}
	ticker := time.NewTicker(scrollbackFlushEvery)
	defer ticker.Stop()
	for range ticker.C {
		m.flushScrollbacks()
	}
}

func (m *Manager) flushScrollbacks() {
	if m.scrollbacks == nil {
		return
	}
	m.mu.RLock()
	sessions := make([]*Session, 0, len(m.sessions))
	for _, sess := range m.sessions {
		sessions = append(sessions, sess)
	}
	m.mu.RUnlock()

	for _, sess := range sessions {
		// Только терминалы, которые ВООБЩЕ могут быть восстановлены: у SSH-сессии
		// нет своего хоста на этом компьютере, у мёртвой — нечего продолжать.
		if m.meta == nil || m.meta.Get(sess.ID).Host == nil {
			continue
		}
		tail, total := sess.tailForPersist(scrollbackKeepBytes)
		if len(tail) == 0 {
			continue
		}
		if err := m.scrollbacks.save(sess.ID, tail, total); err != nil {
			log.Printf("[PTY] scrollback save id=%s: %v", sess.ID, err)
		}
		// Имя агента запоминаем рядом: карточка потерянного терминала должна
		// сказать «здесь работал Claude Code», а не «сессия».
		if kind := sess.lastAgentKind(); kind != "" {
			_ = m.meta.SetAgent(sess.ID, kind)
		}
	}
}

// tailForPersist отдаёт последние n байт вывода и общий счётчик (по нему
// хранилище понимает, изменилось ли что-нибудь с прошлой записи).
func (s *Session) tailForPersist(n int) ([]byte, uint64) {
	s.bufMu.Lock()
	defer s.bufMu.Unlock()
	buf := s.buf
	if len(buf) > n {
		buf = buf[len(buf)-n:]
	}
	out := make([]byte, len(buf))
	copy(out, buf)
	return out, s.totalBytes
}

// recordOutcome кладёт исход в журнал. На сессию хранится по одной записи
// каждого типа: повторная ошибка в том же терминале обновляет время, а не
// плодит строки — главной нужно «что случилось», а не полный лог.
func (m *Manager) recordOutcome(s *Session, status, reason string, at time.Time, hint string) {
	if s == nil || status == "" {
		return
	}
	o := Outcome{
		ID:     s.ID,
		CWD:    s.CWD,
		Shell:  s.Shell,
		Status: status,
		Reason: reason,
		At:     at.UnixMilli(),
		Hint:   hint,
		// Штатный выход шелла — новость, но НЕ беда (см. Outcome.Routine).
		// «Связь с pty-хостом потеряна» (detached) и ошибка в выводе остаются
		// поводом позвать человека.
		Routine: status == "dead" && reason == OutcomeReasonExited,
	}
	if m.meta != nil {
		meta := m.meta.Get(s.ID)
		o.Name, o.Group = meta.Name, meta.Group
	}
	s.evMu.Lock()
	o.AgentKind = s.agentKind
	s.evMu.Unlock()

	m.outMu.Lock()
	defer m.outMu.Unlock()
	for i := range m.outcomes {
		if m.outcomes[i].ID == o.ID && m.outcomes[i].Status == o.Status {
			m.outcomes[i] = o
			return
		}
	}
	m.outcomes = append(m.outcomes, o)
	if len(m.outcomes) > outcomeLimit {
		// Режем самые старые ПО ВРЕМЕНИ исхода: порядок вставки после
		// перезаписи записей уже не совпадает с хронологией.
		sort.Slice(m.outcomes, func(i, j int) bool { return m.outcomes[i].At < m.outcomes[j].At })
		m.outcomes = m.outcomes[len(m.outcomes)-outcomeLimit:]
	}
}

// Outcomes возвращает журнал исходов (свежие первыми), попутно выбрасывая
// записи старше outcomeTTL. Копия: вызывающий её сериализует.
func (m *Manager) Outcomes() []Outcome {
	cutoff := time.Now().Add(-outcomeTTL).UnixMilli()
	m.outMu.Lock()
	kept := make([]Outcome, 0, len(m.outcomes))
	for _, o := range m.outcomes {
		if o.At >= cutoff {
			kept = append(kept, o)
		}
	}
	m.outcomes = kept
	out := make([]Outcome, len(kept))
	copy(out, kept)
	m.outMu.Unlock()

	sort.Slice(out, func(i, j int) bool {
		if out[i].At != out[j].At {
			return out[i].At > out[j].At
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// MetaName returns the user-set friendly name for a PTY (empty if none).
func (m *Manager) MetaName(id string) string {
	if m.meta == nil {
		return ""
	}
	return m.meta.Get(id).Name
}

// SetMetaName updates the friendly name for a PTY.
func (m *Manager) SetMetaName(id, name string) error {
	if m.meta == nil {
		return nil
	}
	return m.meta.SetName(id, name)
}

// SetMetaPlacement updates the folder (group) and manual sort order for a PTY.
func (m *Manager) SetMetaPlacement(id, group string, sort float64) error {
	if m.meta == nil {
		return nil
	}
	return m.meta.SetPlacement(id, group, sort)
}

// SetMetaAccount запоминает аккаунт нейросети, под которым запущен агент этого
// терминала (клиент зовёт это в момент отправки команды запуска).
func (m *Manager) SetMetaAccount(id, accountID, label string) error {
	if m.meta == nil {
		return nil
	}
	return m.meta.SetAccount(id, accountID, label)
}

func (m *Manager) MetaFolders() []string {
	if m.meta == nil {
		return nil
	}
	return m.meta.Folders()
}

func (m *Manager) SetMetaFolders(folders []string) error {
	if m.meta == nil {
		return nil
	}
	return m.meta.SetFolders(folders)
}

// Create spawns a new PTY session owned by the given uid (0 = no owner,
// events from this session won't be broadcast).
func (m *Manager) Create(uid int64, cwd, shell string, cols, rows int) (*Session, error) {
	return m.createWithID(randomID(), uid, cwd, shell, cols, rows)
}

// createWithID — Create с заданным идентификатором. Свой id нужен ровно одному
// сценарию: восстановлению терминала после перезагрузки компьютера (restore.go).
// Ссылки из бота, закладки экрана и открытые вкладки ведут к КОНКРЕТНОМУ
// терминалу — «продолжить работу» не должно подсовывать другой.
func (m *Manager) createWithID(id string, uid int64, cwd, shell string, cols, rows int) (*Session, error) {
	if shell == "" {
		shell = defaultShell()
	} else {
		// Resolve short name to full path (e.g. "powershell.exe" → full path).
		p, err := exec.LookPath(shell)
		if err != nil {
			return nil, fmt.Errorf("шелл %q не найден на этом компьютере", shell)
		}
		shell = p
	}
	if cwd == "" {
		cwd = "."
	}

	var conn ptyConn
	var rec hostRecord
	// Разметка команд (ST-10) решается в момент рождения терминала: живые
	// шеллы переключатель не трогает.
	integ := m.shellIntegration.Load()
	if m.persistent {
		c, r, err := newPersistentPTYWith(id, cols, rows, cwd, shell, integ)
		if err != nil {
			return nil, err
		}
		conn, rec = c, r
		rec.UID = uid
	} else {
		argv, extraEnv := localShellLaunch(shell, integ)
		p, err := newPlatformPTY(cols, rows, cwd, argv, extraEnv)
		if err != nil {
			return nil, err
		}
		conn = p
	}

	sess := newSession(id, cwd, shell, uid, conn, time.Now())
	sess.setBornSize(cols, rows)
	rec.Cols, rec.Rows = cols, rows

	// Persist the host record only after a successful handshake (rec.HostPID set).
	if m.meta != nil && rec.HostPID != 0 {
		_ = m.meta.PutHost(id, rec)
	}
	if m.meta != nil {
		sess.persistView = func(c, r int) { _ = m.meta.SetViewSize(id, c, r) }
	}

	go sess.readLoop(m)

	m.mu.Lock()
	m.sessions[id] = sess
	m.mu.Unlock()

	log.Printf("[PTY] created id=%s shell=%s cwd=%s host_pid=%d", id, shell, cwd, rec.HostPID)
	return sess, nil
}

// CreateSSH открывает SSH-сессию к удалённому серверу (агент — бастион) и
// показывает её как обычный PTY-терминал: тот же Session, readLoop, WS
// /ws/pty/{id}. Без hostRecord/persist — SSH-сессия живёт только в памяти
// агента и рестарт не переживает (см. ssh_conn.go).
func (m *Manager) CreateSSH(uid int64, cfg SSHConfig) (*Session, error) {
	conn, err := ConnectSSH(cfg)
	if err != nil {
		return nil, err
	}

	id := randomID()
	label := "ssh:" + cfg.User + "@" + cfg.Host
	sess := newSession(id, label, "ssh", uid, conn, time.Now())
	sess.setBornSize(cfg.Cols, cfg.Rows)
	sess.SSHPort = cfg.Port
	sess.SSHHostID = cfg.HostID
	if sess.SSHPort <= 0 {
		sess.SSHPort = 22
	}
	sess.SSHProxyJump = cfg.ProxyJump
	group := "SSH · " + cfg.User + "@" + cfg.Host
	if m.meta != nil {
		_ = m.meta.SetPlacement(id, group, 0)
		folders := m.meta.Folders()
		found := false
		for _, folder := range folders {
			if folder == group {
				found = true
				break
			}
		}
		if !found {
			_ = m.meta.SetFolders(append(folders, group))
		}
	}

	go sess.readLoop(m)

	m.mu.Lock()
	m.sessions[id] = sess
	m.mu.Unlock()

	log.Printf("[PTY] created ssh id=%s %s", id, label)
	return sess, nil
}

// Get returns a session by ID (nil if not found).
func (m *Manager) Get(id string) *Session {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.sessions[id]
}

// infoOf собирает JSON-вид одной сессии вместе со статусом. Единственное место
// расчёта: List() (весь список) и Info() (экран терминала) обязаны показывать
// одно и то же — раньше /api/pty/{id}/state вообще не знал про status/hint, и
// экран терминала не мог показать, чего ждёт агент.
func (m *Manager) infoOf(s *Session, now time.Time) SessionInfo {
	info := SessionInfo{
		ID:      s.ID,
		CWD:     s.CWD,
		Shell:   s.Shell,
		Created: s.Created.UnixMilli(),
		Alive:   s.IsAlive(),
	}
	// LastActive = max(Created, detector lastOutputAt). Falls back to
	// Created if the detector hasn't observed any output yet.
	la := s.Created
	var le sessEvent
	var lastOut time.Time
	s.evMu.Lock()
	if s.ev != nil {
		lastOut = s.ev.lastOutputAt
		le = s.ev.lastEvt
		if !lastOut.IsZero() && lastOut.After(la) {
			la = lastOut
		}
	}
	s.evMu.Unlock()
	info.LastActive = la.UnixMilli()
	// Зрителей считаем и у мёртвой сессии: её вывод ещё пять минут доступен на
	// экране, и «смотрят двое» там такая же правда.
	info.Viewers = s.ViewerCount()
	if s.Shell == "ssh" {
		info.Kind = "ssh"
		info.SSHUser, info.SSHHost = parseSSHLabel(s.CWD)
		info.SSHPort = s.SSHPort
		info.SSHHostID = s.SSHHostID
		info.SSHProxyJump = s.SSHProxyJump
		info.Remote = true
	}
	if m.meta != nil {
		meta := m.meta.Get(s.ID)
		info.Name = meta.Name
		info.Group = meta.Group
		info.Sort = meta.Sort
		// Под каким аккаунтом нейросети живёт агент этого терминала: у соседнего
		// он может быть другим, и человек имеет право видеть, чьи лимиты тратит.
		info.AccountID = meta.AccountID
		info.AccountLabel = meta.AccountLabel
	}

	// Status for the list badge.
	if !info.Alive {
		info.Status = "dead"
		if diedAt, ok := s.deadSince(); ok {
			info.DiedAt = diedAt.UnixMilli()
			info.StatusAt = info.DiedAt
			// Жив ли процесс терминала — спрашиваем только у мёртвых сессий и
			// только пока их видно (5 минут до реапера): дозвон до хоста стоит
			// одного соединения, и делать его на каждый живой терминал в списке
			// незачем.
			info.HostAlive = m.HostAlive(s.ID)
		}
		return info
	}
	fg := s.ForegroundProcess()
	if fg.Name != "" {
		info.FgProcess = fg.Name
		info.AgentKind = AgentKind(fg.Name)
	}
	if info.Kind == "ssh" {
		if remoteKind := remoteAgentKind(s); remoteKind != "" {
			info.AgentKind = remoteKind
		}
	}
	// agentBusy — сессией распоряжается AI-агент: его процесс в фокусе, и пока
	// он не отчитался о готовности, «работает» — правда.
	agentBusy := info.FgProcess != "" && info.AgentKind != "" && info.AgentKind != "shell"
	stillIdle := lastOut.IsZero() || now.Sub(lastOut) >= idleWaiting
	// У SSH-сессии foreground-процесса на нашей машине нет (shellPID()==0), из-за
	// чего waiting/working для неё не срабатывали никогда: состояние считаем по
	// выводу самой сессии. Но признак «работает» для неё честен, только пока
	// вывод свежий: раньше busy у kind=="ssh" был включён БЕЗУСЛОВНО, и сессия,
	// в которой человек утром посмотрел логи и ушёл, вечно числилась «Сейчас
	// работает» с пульсирующей точкой, пряча за собой настоящую работу.
	busy := agentBusy || info.Kind == "ssh"
	working := agentBusy || (info.Kind == "ssh" && !stillIdle)
	// Вопрос на экране прямо сейчас (детектор подтвердил его в этом или
	// прошлом тике). Без этого признака кнопки ответа жили только в паузах
	// вывода и пропадали, стоило агенту перерисовать собственное меню.
	questionFresh := false
	if state := s.evState(); state != nil {
		questionFresh = state.questionOnScreen(now)
	}
	info.Status, info.StatusAt, info.Hint, info.HintKind = statusFrom(le, busy, working, stillIdle, questionFresh, now, info.LastActive)
	if info.AgentKind == "codex" && info.Status != "waiting" {
		if status, at := s.codexActivity.status(now); status != "" {
			info.Status, info.StatusAt = status, at.UnixMilli()
			info.LastActive = at.UnixMilli()
			info.Hint, info.HintKind = "", ""
		}
	}
	if info.Status == "waiting" {
		info.HintOptions = le.options
	}
	// Claude Code writes a precise busy/idle status keyed by its foreground
	// PID. Prefer it over a heuristic when present, while retaining PTY hint
	// detection for the actual question text.
	if info.AgentKind == "claude" && fg.PID > 0 {
		if runtimeStatus, ok := s.claudeRuntimeStatus(int(fg.PID)); ok {
			if IsAgentKind(runtimeStatus.Kind) {
				info.AgentKind = runtimeStatus.Kind
			}
			switch strings.ToLower(runtimeStatus.Status) {
			case "busy", "working", "running":
				info.Status = "working"
				info.StatusAt = runtimeStatus.UpdatedAt
				if !lastOut.IsZero() && now.Sub(lastOut) > 2*time.Minute {
					info.Status = "stalled"
					info.StatusAt = lastOut.Add(2 * time.Minute).UnixMilli()
				}
			case "waiting":
				// Claude сам пишет "waiting", пока на экране его вопрос или запрос
				// разрешения (живой прогон 11.09.2026, claude 2.1.268). Раньше
				// ветки не было, и статус падал в эвристику: в том же прогоне
				// продукт показывал «работает» и «свободен» вперемешку.
				info.Status = "waiting"
				info.StatusAt = runtimeStatus.UpdatedAt
				if le.kind == EventWaitingInput && le.hint != "" {
					info.StatusAt = le.at.UnixMilli()
					info.Hint = le.hint
					info.HintKind = le.hintKind
					info.HintOptions = le.options
				} else {
					info.Hint, info.HintKind, info.HintOptions = claudeWaitingHint, "", nil
				}
			case "idle", "ready":
				// Именно ВОПРОС агента, а не любое последнее событие с текстом:
				// у события error hint теперь тоже непустой (там строка ошибки,
				// #60), и без проверки вида терминал показывал бы «ждёт ответа»
				// с текстом упавшей команды.
				if le.kind == EventWaitingInput && le.hint != "" {
					info.Status = "waiting"
					info.StatusAt = le.at.UnixMilli()
					info.Hint = le.hint
					info.HintKind = le.hintKind
					info.HintOptions = le.options
				} else {
					info.Status = "ready"
					info.StatusAt = runtimeStatus.UpdatedAt
				}
			}
		}
	}
	// Пункты меню имеют смысл ТОЛЬКО пока вопрос на экране: статус мог быть
	// перебит ниже (Claude отчитался «работаю»), и тогда подписи с прошлого
	// вопроса ушли бы на кнопки к следующему.
	if info.Status != "waiting" {
		info.HintOptions = nil
	}
	// Запоминаем вид агента для журнала исходов: у мёртвой сессии foreground
	// уже не спросить, а сказать «упал claude», а не «упал терминал», надо.
	s.rememberAgentKind(info.AgentKind)
	if info.Kind != "ssh" {
		info.Sleep = m.sleepInfoFor(s, info.AgentKind, fg)
	}
	return info
}

// statusFrom — чистое правило «состояние терминала» (отдельно от сборки
// SessionInfo, чтобы его можно было проверить тестом):
//
//	waiting — агент задал РАСПОЗНАННЫЙ вопрос (hint непустой);
//	ready   — агент жив и молчит, но вопрос не распознан;
//	working — агент/команда выводит что-то прямо сейчас;
//	error   — последняя команда упала (не старше 5 минут);
//	idle    — шелл у промпта.
//
// Разделение waiting/ready и есть суть правки: раньше оба состояния были
// waiting, и каждый закончивший агент навсегда оставался в «Требует внимания»
// с текстом «ждёт вашего ответа», из-за чего настоящие вопросы терялись.
//
// busy    — сессией распоряжается агент (или это SSH): только для таких сессий
// имеют смысл waiting/ready, у голого шелла «ждёт ответа» не бывает;
// working — она ПРЯМО СЕЙЧАС работает. Раньше это был один флаг, и «работает»
// у SSH-сессии не выключалось никогда;
// questionFresh — вопрос ВИДЕН на экране прямо сейчас (детектор подтвердил его
// в последние секунды). Живая жалоба: «выбери варианты — пропадает сразу».
// Полноэкранный агент, стоя на собственном меню, продолжает печатать
// (подсветка пункта, таймер, курсор), поэтому условия «молчит три секунды» для
// вопроса недостаточно: кнопки ответа исчезали через долю секунды после
// появления. Вопрос на экране — состояние ЭКРАНА, а не потока.
func statusFrom(le sessEvent, busy, working, stillIdle, questionFresh bool, now time.Time, lastActive int64) (status string, at int64, hint, hintKind string) {
	errFresh := le.kind == EventError && now.Sub(le.at) < errorStatusTTL
	switch {
	// Вопрос, распознанный ПРЯМО СЕЙЧАС, — это «ждёт ответа» и в обычном
	// терминале. Прежнее условие требовало busy («сессией распоряжается агент»),
	// и терминал, где `npm install` спросил «Ok to proceed?», `git add -p` —
	// «[y,n,q,a,d,?]», а ssh — «continue connecting (yes/no)?», числился
	// «свободен»: на телефоне ни подсказки, ни кнопок, и работа стояла, пока
	// человек не откроет сам терминал. Спрашивает не шелл, а команда в нём.
	case le.kind == EventWaitingInput && le.hint != "" && (busy && (stillIdle || questionFresh) || questionFresh):
		return "waiting", le.at.UnixMilli(), le.hint, le.hintKind
	case (le.kind == EventAgentReady || le.kind == EventWaitingInput) && busy && stillIdle:
		// Ветка EventWaitingInput — для состояний, взведённых агентом до
		// обновления: там hint пустой, но означает ровно «молчит».
		return "ready", le.at.UnixMilli(), "", ""
	// Ошибка идёт ПЕРЕД «работает», когда вывод уже остановился: раньше ветка
	// busy перехватывала её первой, и у агентских и SSH-сессий статус error не
	// доходил до клиента вовсе. Пока вывод идёт, честнее «работает» — агент мог
	// напечатать ошибку и продолжить работу.
	case errFresh && (stillIdle || !working):
		// Сама сработавшая строка (см. errorHintFrom): и карточке «Требует
		// внимания», и доборному уведомлению есть что показать вместо «что-то
		// упало». HintKind пуст — кнопок ответа у ошибки не бывает.
		return "error", le.at.UnixMilli(), le.hint, ""
	case working:
		return "working", 0, "", ""
	default:
		return "idle", lastActive, "", ""
	}
}

// parseSSHLabel разбирает ярлык сессии «ssh:user@host» (см. CreateSSH).
func parseSSHLabel(label string) (user, host string) {
	rest := strings.TrimPrefix(label, "ssh:")
	if at := strings.LastIndex(rest, "@"); at >= 0 {
		return rest[:at], rest[at+1:]
	}
	return "", rest
}

// Info возвращает вид одной сессии (для GET /api/pty/{id}/state).
func (m *Manager) Info(id string) (SessionInfo, bool) {
	m.mu.RLock()
	s := m.sessions[id]
	m.mu.RUnlock()
	if s == nil {
		return SessionInfo{}, false
	}
	return m.infoOf(s, time.Now()), true
}

// List returns info about all sessions.
func (m *Manager) List() []SessionInfo {
	m.mu.RLock()
	defer m.mu.RUnlock()

	now := time.Now()
	out := make([]SessionInfo, 0, len(m.sessions))
	for _, s := range m.sessions {
		out = append(out, m.infoOf(s, now))
	}
	// Stable, deterministic order (newest first) — Go map iteration is
	// randomized, which made the list cards shuffle on every refresh.
	sort.Slice(out, func(i, j int) bool {
		if out[i].Created != out[j].Created {
			return out[i].Created > out[j].Created
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// CloseDead terminates and removes all dead (exited) sessions.
// Returns the number of sessions closed.
func (m *Manager) CloseDead() int {
	m.mu.Lock()
	var toClose []*Session
	for id, sess := range m.sessions {
		if !sess.IsAlive() {
			toClose = append(toClose, sess)
			delete(m.sessions, id)
		}
	}
	m.mu.Unlock()

	for _, sess := range toClose {
		log.Printf("[PTY] closing dead id=%s", sess.ID)
		if m.meta != nil && sess.shouldForget() {
			m.meta.Forget(sess.ID)
		}
		sess.close() // detach — a dead session's host has already exited
	}
	return len(toClose)
}

// Close terminates and removes a session.
func (m *Manager) Close(id string) error {
	m.mu.Lock()
	sess, ok := m.sessions[id]
	if ok {
		delete(m.sessions, id)
	}
	m.mu.Unlock()

	if !ok {
		return fmt.Errorf("session %q not found", id)
	}

	// Метка ДО kill: смерть от нашей же руки не должна попасть в журнал исходов
	// как «терминал завершился» (см. readLoop).
	sess.closedByUser.Store(true)
	log.Printf("[PTY] closing id=%s", id)

	// PID хоста читаем ДО того, как что-либо стирать: kill() шлёт кадр в ту же
	// трубу, и если она мертва (а это ровно тот случай, когда readLoop упал по
	// ошибке трубы), хост о своём закрытии не узнает. Раньше запись и хвост
	// вывода стирались ПЕРВЫМИ, поэтому после такого провала о процессе не
	// оставалось ни следа: он жил невидимо, пока его не снимали Диспетчером
	// задач. Живой замер 02.08.2026: 5 сирот, 80 процессов, 4,2 ГБ.
	var hostPID uint32
	if m.meta != nil {
		if h := m.meta.Get(id).Host; h != nil {
			hostPID = h.HostPID
		}
	}

	err := sess.kill() // explicit user close — terminate the host + shell
	// Труба могла не донести Kill — добиваем по PID. Внутри есть пауза на
	// штатный уход и проверки «это наш хост именно этой сессии», так что
	// нормальный случай (хост вышел сам) сюда не попадает.
	forced := killHostProcess(id, hostPID)

	// Запись и хвост убираем ПОСЛЕ убийства — и только если уверены, что хоста
	// больше нет. Иначе оставляем запись: терминал, который не удалось снять,
	// должен остаться видимым, а не превратиться в невидимый процесс с чужой
	// работой внутри (ровно так и накопились 5 сирот к 02.08.2026).
	if err != nil && !forced && hostStillAlive(id, hostPID) {
		log.Printf("[PTY] хост id=%s pid=%d снять не удалось (%v) — запись оставлена, терминал виден в списке", id, hostPID, err)
		return err
	}
	if m.meta != nil {
		m.meta.Forget(id)
	}
	// Терминал закрыл человек — сохранённый хвост вывода больше не нужен:
	// восстанавливать нечего, а лишний файл с содержимым чужой работы на диске
	// не нужен никому.
	if m.scrollbacks != nil {
		m.scrollbacks.forget(id)
	}
	return err
}

// ── Session methods ──────────────────────────────────────────────

// readLoop reads PTY output, appends to scrollback, fans out to subscribers,
// and feeds the heuristic event detector.
func (s *Session) readLoop(m *Manager) {
	// Зеркало экрана поднимаем здесь, а не в четырёх местах запуска чтения
	// (создание, восстановление, повторное присоединение к живому хосту): у
	// зеркала ровно один источник — этот цикл, и живёт оно ровно столько же.
	// Всё, что напечатано ДО его рождения, оно не увидит: на переподключении к
	// многочасовой сессии кадр соберётся по мере того, как агент перерисует
	// экран. Это честнее, чем показывать обрывки, но не волшебство.
	s.viewMu.Lock()
	mcols, mrows := s.viewCols, s.viewRows
	if mcols <= 0 || mrows <= 0 {
		mcols, mrows = s.bornCols, s.bornRows
	}
	s.viewMu.Unlock()
	s.startScreen(mcols, mrows)
	defer s.stopScreen()
	// Граница переигровки буфера хоста: всё до неё — прошлое, которое зеркало
	// не должно записывать себе в историю (см. dropReplayScrollback). Ставим
	// ЗДЕСЬ и в setConn: reattachLive подменяет backend на живой сессии.
	s.watchReplayEnd()

	defer s.doneOnce.Do(func() {
		s.viewMu.Lock()
		if s.sizeOwner != nil {
			s.clearSizeOwnerLocked()
			s.notifySizeControlsLocked()
		}
		s.viewMu.Unlock()
		died := time.Now()
		s.diedMu.Lock()
		s.diedAt = died
		s.diedMu.Unlock()
		close(s.done)
		// Журнал исходов: терминал закрылся сам — это ровно то, о чём главная
		// должна сказать вернувшемуся человеку (сессию через 5 минут съест
		// reapLoop). Закрытие руками исходом не считается — человек и так знает.
		if m != nil && !s.closedByUser.Load() {
			reason := OutcomeReasonDetached
			if s.shouldForget() {
				reason = OutcomeReasonExited
			}
			m.recordOutcome(s, "dead", reason, died, "")
		}
	})

	buf := make([]byte, 8192)
	for {
		readGeneration := s.outputGeneration.Load()
		n, err := s.conn().Read(buf)
		if n > 0 {
			// Данные пришли — значит связь живая, и серия обрывов (если была)
			// закончилась: следующий обрыв начнёт свой счёт заново.
			s.noteReattachProgress()
			data := make([]byte, n)
			copy(data, buf[:n])
			if s.beforeOutputApply != nil {
				s.beforeOutputApply()
			}
			s.outputApplyMu.Lock()

			// Append to scrollback.
			s.bufMu.Lock()
			s.buf = append(s.buf, data...)
			if len(s.buf) > scrollbackSize {
				s.buf = s.buf[len(s.buf)-scrollbackSize:]
			}
			s.totalBytes += uint64(n)
			streamOff := s.totalBytes // позиция сразу ПОСЛЕ этого куска — для зеркала
			s.dec.scan(data)          // отследить alt-screen/mouse/bracketed-paste для reset-resync
			s.altScreen.Store(s.dec.altActive())
			// Снапшот изменившихся режимов под тем же bufMu; персист — после
			// Unlock (дисковый I/O вне горячей секции). Порядок гарантирован:
			// SetModes зовётся только из этой горутины, последовательно.
			var modesSnap []int
			modesChanged := s.dec.takeDirty()
			if modesChanged {
				modesSnap = s.dec.snapshot()
			}

			s.fanoutLocked(data)
			s.bufMu.Unlock()

			// Зеркало экрана — ВНЕ горячей секции и без блокировки: разбор
			// потока эмулятором идёт со скоростью около 3 МБ/с, и под bufMu он
			// останавливал бы и рассылку подписчикам, и чтение PTY.
			//
			// Позиция потока едет вместе с байтами: по ней снятый кадр знает,
			// какой вывод он уже покрывает, и писатель не отдаёт кадр раньше
			// самих этих байт (см. ScreenFrameAt). fanoutLocked выше стоит в
			// той же горутине ПЕРЕД этой строкой — отсюда и инвариант
			// «кадр не может опередить канал подписчика».
			s.feedScreenAtGeneration(data, streamOff, readGeneration)
			s.outputApplyMu.Unlock()

			// Персист режимов — вне горячей секции (дисковый I/O). Порядок
			// гарантирован: SetModes зовётся только из этой горутины.
			if modesChanged && m != nil && m.meta != nil {
				_ = m.meta.SetModes(s.ID, modesSnap)
			}

			// Feed the heuristic detector. Строка, похожая на ошибку, здесь
			// только ПОМЕЧАЕТСЯ: станет ли она новостью «упало», решает
			// settleError, когда терминал затихнет. Пока исход писался прямо
			// отсюда, на любой подходящий чанк, «Требует внимания» на 12 часов
			// занимали строки, которые агент прочитал и пошёл дальше.
			if m != nil && s.UID != 0 {
				if state := s.evState(); state != nil {
					now := time.Now()
					if hint := state.observeChunk(data, now); hint != "" {
						state.noteError(hint, now)
					}
				}
			}
		}
		if err != nil {
			// Канал до pty-host мог оборваться, пока сам хост жив и работа в нём
			// продолжается: тогда «терминал завершился» — неправда, и человеку
			// нельзя предлагать перезапуск (получил бы второй терминал в той же
			// папке). Сначала пробуем восстановить связь (см. reattach_live.go).
			if m != nil && m.reattachLive(s) {
				continue
			}
			log.Printf("[PTY] readLoop ended id=%s: %v", s.ID, err)
			return
		}
	}
}

// fanoutLocked рассылает свежий кадр подписчикам. Зовётся ПОД bufMu — тем же,
// под которым кадр только что дописан в кольцо.
//
// Почему под bufMu, а не после него. Иначе между «дописали в buf» и «разослали»
// успевает влезть SubscribeResume: он берёт эти байты в свой снимок И получает
// их же следующим кадром в канал — человек видит кусок вывода дважды. Порядок
// замков bufMu→subMu совпадает с SubscribeResume и ResyncFrom, поэтому взаимной
// блокировки нет, а сама рассылка — неблокирующие записи в буферизованные
// каналы, то есть в горячей секции она не задерживается.
//
// Переполненный канал больше НЕ означает выброс байтов. Раньше здесь стоял
// `default:` с комментарием «scrollback covers the gap» — не покрывал: клиент
// ведёт свою позицию в потоке инкрементами по длине ПРИНЯТЫХ кадров, поэтому
// выброшенное он не видел никогда, а его offset навсегда расходился с сервером
// (следующее резюме отдавало хвост с отставшей позиции — уже показанное
// печаталось второй раз). Замер 2026-08-04, build/qa/probe-lost-lines.mjs:
// клиент, подтормозивший на 4 с, потерял 8 329 строк из 20 000 — 42% вывода.
// Теперь подписчик помечается «отстал», в канал ему не пишут, а его писатель
// до-сылает пропущенное из кольца (см. ResyncFrom).
func (s *Session) fanoutLocked(data []byte) {
	s.subMu.RLock()
	for ch, st := range s.subs {
		if st.paused.Load() {
			continue // клиент на паузе: его байты копятся только в кольце
		}
		if st.lagged.Load() {
			continue // ждёт до-сылки из кольца — в канал не пишем
		}
		if st.queued.Load()+int64(len(data)) > subQueueBytes {
			st.markLagged()
			continue
		}
		select {
		case ch <- data:
			st.queued.Add(int64(len(data)))
		default:
			st.markLagged()
		}
	}
	s.subMu.RUnlock()
}

// Запас, который держится для подписчика, пока он не забрал вывод. Это буфер на
// ЗАМИРАНИЕ клиента: пока телефон не читает сокет (метро, переход Wi-Fi↔LTE,
// просто медленный канал), кадры копятся здесь.
//
// Меряем БАЙТАМИ, а не кадрами, и это главное. Ёмкость канала — это число
// элементов, а размер кадра диктует не наш буфер чтения (8 КБ), а ConPTY:
// замер 04.08.2026 показал 20 119 кадров на 1,44 МБ, то есть ~72 байта на кадр.
// При счёте кадрами «запас 512» превращался в 37 КБ, и залповый вывод
// переполнял его за доли секунды — сколько ни увеличивай счётчик, он мерит не
// то. Отсюда два числа: канал заведомо длинный (это всего лишь указатели), а
// настоящий предел — subQueueBytes.
//
// 4 МБ выбраны по кольцу сессии (scrollbackSize): больше держать бессмысленно —
// то, что вытеснено из кольца, до-слать всё равно неоткуда. Замер: залп 1,8 МБ
// при паузе клиента 4 с проходит целиком, без пометки пропуска; на выводе
// обычного агента (10–20 КБ/с) очередь не наполняется никогда. Память тратится
// только на реально непрочитанное и только у замершего клиента.
const (
	subChanFrames = 8192
	subQueueBytes = 4 << 20
)

// bufStartOf — позиция в потоке, которой соответствует первый байт кольца.
//
// Считается вычитанием в БЕЗЗНАКОВОЙ арифметике, поэтому нарушенный инвариант
// (кольцо длиннее счётчика выведенного) давал не отрицательное число, а число
// под 2^64 — и резюме ломалось молча, навсегда и без единого сообщения в лог.
// Одна такая дыра уже была реальной (seedScrollback у восстановленного после
// перезагрузки терминала), поэтому страховка стоит в самой арифметике, а не в
// вызывающих: цена ошибки — потерянная история у человека, цена проверки — ноль.
func bufStartOf(total uint64, bufLen int) uint64 {
	if uint64(bufLen) > total {
		return 0
	}
	return total - uint64(bufLen)
}

// subState — состояние одного подписчика рядом с его каналом.
//
// lagged взводится, когда канал переполнен: это НЕ повод выбросить вывод (так
// было до 2.49.12 — человек терял куски экрана безвозвратно), а повод до-слать
// пропущенное из кольцевого буфера. Пока флаг взведён, в канал не пишут: иначе
// свежие байты обогнали бы до-сылку и легли на экран не в том порядке.
//
// wake — будильник писателя (ёмкость 1): его select висит на канале вывода и
// на нём, так что «ты отстал» доезжает мгновенно, а не следующим heartbeat.
//
// paused взводит сам клиент (ctrl «pause» в /ws/pty): его xterm не успевает
// разбирать поток. Пауза — на ПОДПИСЧИКА, не на сессию: readLoop читает PTY
// дальше, кольцо (4 МБ) и screen-модель наполняются, потерь нет. Глушить чтение
// PTY было бы ударом по остальным зрителям сессии, а всплеск поглощает кольцо.
// На «resume» пропущенное до-сылается из кольца тем же путём, что у отставшего
// подписчика (см. Session.SetFlowPaused).
type subState struct {
	lagged atomic.Bool
	paused atomic.Bool
	// queued — сколько байт лежит в канале непрочитанными. Именно оно, а не
	// длина канала, ограничивает запас: см. subQueueBytes.
	queued atomic.Int64
	wake   chan struct{}
}

func (st *subState) markLagged() {
	st.lagged.Store(true)
	select {
	case st.wake <- struct{}{}:
	default: // будильник уже взведён
	}
}

// SubHandle — ручка подписчика для его писателя: будильник «ты отстал» и учёт
// вычитанного. Отдельный тип, чтобы писатель не лазил в карту подписчиков под
// замком на КАЖДЫЙ кадр (их бывают десятки тысяч в секунду).
type SubHandle struct{ st *subState }

// Wake — канал сигнала «отстал, забери пропущенное через ResyncFrom».
// Nil-ручка отдаёт nil: select на nil-канале просто никогда не срабатывает.
func (h SubHandle) Wake() <-chan struct{} {
	if h.st == nil {
		return nil
	}
	return h.st.wake
}

// Took — писатель забрал n байт из канала: место в запасе освободилось.
func (h SubHandle) Took(n int) {
	if h.st == nil {
		return
	}
	h.st.queued.Add(-int64(n))
}

// Handle возвращает ручку подписчика по его каналу.
func (s *Session) Handle(ch chan []byte) SubHandle {
	s.subMu.RLock()
	defer s.subMu.RUnlock()
	return SubHandle{st: s.subs[ch]}
}

// Paused — клиент просил паузу потока (см. Session.SetFlowPaused). Писатель
// смотрит её в будильнике отставания: до-сылка на паузе откладывается, её
// взведёт сама разпауза.
func (h SubHandle) Paused() bool {
	return h.st != nil && h.st.paused.Load()
}

// SetFlowPaused переводит подписчика на паузу и обратно по просьбе клиента
// (ctrl «pause»/«resume» в /ws/pty): клиент сообщает, что его xterm не успевает
// разбирать поток. Пороги (256 КБ неподтверждённых байт → pause, спад ниже
// 64 КБ → resume) живут на клиенте, агенту важно лишь само состояние.
//
// Пауза: fanoutLocked перестаёт писать в канал подписчика, его вывод с этого
// момента лежит ТОЛЬКО в кольце сессии — потерь нет, а PTY читается дальше
// ради остальных зрителей.
//
// Снятие паузы: взводим тот же будильник, что у отставшего подписчика
// (markLagged), и писатель до-сылает пропущенное со своей позиции sent
// существующим путём ResyncFrom — параллельного механизма догона у паузы нет.
// «resume» без предшествующей паузы холостой: CompareAndSwap не проходит, и
// лишний маркер resumed клиенту не уезжает.
//
// Отписка в состоянии паузы ничем не отличается от обычной: Unsubscribe
// удаляет subState целиком, флагу некуда течь.
func (s *Session) SetFlowPaused(ch chan []byte, paused bool) {
	s.subMu.RLock()
	st := s.subs[ch]
	s.subMu.RUnlock()
	if st == nil {
		return // подписчика уже нет (отписался или сессия закрылась)
	}
	if paused {
		st.paused.Store(true)
		return
	}
	if st.paused.CompareAndSwap(true, false) {
		st.markLagged()
	}
}

// ResyncFrom до-сылает подписчику всё, что он пропустил, начиная с sent —
// позиции в потоке, до которой писатель РЕАЛЬНО отдал байты в сокет.
//
// Возвращает payload (кусок кольца после sent), новую позицию потока и признак
// gap: пропущенного оказалось больше, чем стоит слать (клиент отсутствовал так
// долго, что кольцо провернулось, либо хвост длиннее clientReplayLimit) —
// тогда честная пометка «пропуск» вместо молчаливой потери.
//
// Очередь канала при этом ВЫБРАСЫВАЕТСЯ: всё, что в ней лежало, находится
// после sent и входит в payload, поэтому доставка остаётся ровно однократной.
// Флаг lagged снимается под теми же замками, в которых снят снимок буфера, —
// иначе readLoop успел бы дописать байт между снимком и снятием флага, и этот
// байт уехал бы клиенту дважды.
func (s *Session) ResyncFrom(ch chan []byte, sent uint64, sentEpoch string) (payload []byte, offset uint64, gap bool, epoch string) {
	s.bufMu.Lock()
	s.subMu.Lock()
	total := s.totalBytes
	bufStart := bufStartOf(total, len(s.buf))
	switch {
	case sentEpoch != "" && sentEpoch != s.epoch:
		// sent живёт в чужой шкале. Числово оно может быть даже
		// больше total, но это не «уже догнал»: отдаём хвост нового
		// потока, а web пошлёт reset marker до payload.
		payload = clientReplayTail(s.buf)
	case sent >= total:
		// Догонять нечего (пометили «отстал», но писатель успел всё отдать).
	case sent >= bufStart:
		tail := s.buf[sent-bufStart:]
		if len(tail) > liveResyncLimit {
			tail = tail[len(tail)-liveResyncLimit:]
			gap = true
		}
		payload = make([]byte, len(tail))
		copy(payload, tail)
	default:
		// Пропущенное вытеснено из кольца — отдаём хвост с пометкой пропуска.
		payload = tailOf(s.buf, liveResyncLimit)
		gap = true
	}
	offset, epoch = total, s.epoch
	if st := s.subs[ch]; st != nil {
		// Порядок: сначала опустошить очередь, потом снять флаг. Иначе новые
		// байты легли бы в канал ПЕРЕД тем, как мы выбросим старые.
		for {
			select {
			case <-ch:
				continue
			default:
			}
			break
		}
		st.queued.Store(0)
		st.lagged.Store(false)
	}
	s.subMu.Unlock()
	s.bufMu.Unlock()
	return
}

// Subscribe returns a channel for live output and the current scrollback.
// Caller must call Unsubscribe when done.
func (s *Session) Subscribe() (chan []byte, []byte) {
	ch := make(chan []byte, subChanFrames)

	// Snapshot scrollback under lock, then register subscriber. Отдаём ХВОСТ:
	// целиком буфер (4 МБ) телефон разбирает секундами, и всё это время экран
	// пустой — см. clientReplayLimit.
	s.bufMu.Lock()
	scrollback := clientReplayTail(s.buf)
	s.bufMu.Unlock()

	s.subMu.Lock()
	s.subs[ch] = &subState{wake: make(chan struct{}, 1)}
	s.subMu.Unlock()

	return ch, scrollback
}

// SubscribeResume регистрирует подписчика и возвращает данные для (ре)синхронизации
// по модели offset+epoch (SOTA resumable subscription, как Centrifugo/Ably):
//   - resume succeeds (isDelta=true) если epoch совпал И запрошенный offset ещё
//     присутствует в кольцевом буфере → отдаём ТОЛЬКО хвост после offset;
//   - epoch совпал, но offset уже ВЫТЕСНЕН из кольца (долгий обрыв при болтливом
//     TUI) → gap=true: отдаём весь текущий буфер, а клиент ДОПИСЫВАЕТ его к своей
//     истории с пометкой «пропуск», НЕ стирая экран. Раньше это был полный
//     reset, и история, которую клиент уже имел, пропадала ни за что — живая
//     жалоба 2026-07-29: «в Codex скроллится только два экрана» (репейнтовый
//     поток ConPTY выносил 512 КБ кольца за минуты работы агента);
//   - иначе (первый коннект, чужой/устаревший epoch, offset впереди потока) →
//     отдаём весь буфер (isDelta=false), клиент делает чистый полный редрав.
//
// epoch и offset возвращаются всегда — клиент их запоминает для следующего резюме.
//
// Снимок буфера и регистрация канала атомарны (оба замка в порядке bufMu→subMu,
// как в readLoop), чтобы между снимком и подпиской не потерять и не задвоить байты.
// clientReplayLimit — сколько истории уходит КЛИЕНТУ при открытии терминала.
//
// Буфер сессии держит 4 МБ (scrollbackSize, поднят в 2.46.4 ради «Codex: два
// экрана скролла»), но отдавать их телефону целиком нельзя: живая жалоба
// (2026-07-30) — «терминал явно работает, а вывода нет вообще». Четыре мегабайта
// ANSI едут через облако и потом разбираются xterm.js на телефоне: пока это
// длится, человек смотрит в пустой экран. Хвоста хватает на несколько экранов
// TUI-агента, а вся история остаётся у агента (и выгружается кнопкой «Сохранить
// лог»).
const clientReplayLimit = 512 << 10

// liveResyncLimit — сколько до-сылается клиенту, который УЖЕ смотрит терминал и
// отстал (см. ResyncFrom). Это другой случай, чем открытие терминала, и лимит у
// него свой, больше.
//
// На ОТКРЫТИИ важно, как быстро появится первая картинка: клиент пуст, и лишний
// мегабайт — это секунды пустого экрана (живая жалоба 2026-07-30). На ЖИВОЙ
// до-сылке экран уже нарисован, и лишний мегабайт — это, наоборот, ровно тот
// вывод, который человек иначе не увидит вовсе. Замер 04.08.2026: залп 1,8 МБ
// при паузе клиента 4 с оставлял позади 1,1 МБ — под 512 КБ это был честный,
// но обидный «пропуск», под 2 МБ пропуска нет вовсе.
//
// Потолок в 2 МБ, а не всё кольцо: больше не нужно — у xterm на клиенте
// история 10 000 строк (~1,5 МБ), остальное всё равно вытеснится, а платить за
// это пришлось бы одним крупным кадром на медленном канале.
const liveResyncLimit = 2 << 20

// tailOf копирует последние limit байт буфера.
func tailOf(buf []byte, limit int) []byte {
	if len(buf) > limit {
		buf = buf[len(buf)-limit:]
	}
	out := make([]byte, len(buf))
	copy(out, buf)
	return out
}

// clientReplayTail копирует хвост для ОТКРЫТИЯ терминала (см. clientReplayLimit).
func clientReplayTail(buf []byte) []byte { return tailOf(buf, clientReplayLimit) }

func (s *Session) SubscribeResume(resumeEpoch string, resumeOffset uint64) (ch chan []byte, payload []byte, offset uint64, epoch string, isDelta bool, gap bool) {
	ch = make(chan []byte, subChanFrames)
	s.bufMu.Lock()
	s.subMu.Lock()
	total := s.totalBytes
	bufStart := bufStartOf(total, len(s.buf))
	// Размер менялся после того, как клиент ушёл: его экран и пропущенные байты
	// в разных сетках. Честный reset — клиент очистит экран и попросит кадр.
	// Смена ровно на позиции клиента тоже считается: всё после неё — уже новая
	// сетка. Без вывода после смены досылать нечего, и reset не нужен.
	resizedSince := s.geometryEpoch == s.epoch && resumeOffset <= s.geometryAt && total > s.geometryAt
	switch {
	case resizedSince:
		payload = clientReplayTail(s.buf)
	case resumeEpoch == s.epoch && resumeOffset >= bufStart && resumeOffset <= total:
		tail := s.buf[resumeOffset-bufStart:]
		if len(tail) > clientReplayLimit {
			// Клиент отсутствовал так долго, что пропустил больше, чем ему стоит
			// присылать: отдаём хвост и честно помечаем пропуск.
			tail = tail[len(tail)-clientReplayLimit:]
			gap = true
		}
		payload = make([]byte, len(tail))
		copy(payload, tail)
		isDelta = true
	case resumeEpoch == s.epoch && resumeOffset < bufStart:
		// Середина потока потеряна для клиента, но его СТАРАЯ история валидна:
		// отдаём хвост кольца как дельту с пропуском (см. шапку).
		payload = clientReplayTail(s.buf)
		isDelta, gap = true, true
	default:
		payload = clientReplayTail(s.buf)
	}
	offset, epoch = total, s.epoch
	s.subs[ch] = &subState{wake: make(chan struct{}, 1)}
	s.subMu.Unlock()
	s.bufMu.Unlock()
	return
}

// seedModes сидирует DEC-режимы, пришедшие от pty-host или из pty.json, и
// обновляет зеркало alt-screen.
//
// Отдельным методом, потому что мест сидирования три (attach к живому хосту,
// reattach-live, reattach по запросу), и зеркало, обновляемое «руками» рядом с
// каждым, однажды забудут обновить. Тогда осиротевшая TUI-сессия молча вернётся
// к born-размеру и сотрёт экран — ровно тот дефект, ради которого зеркало и
// заведено.
func (s *Session) seedModes(modes []int) {
	s.bufMu.Lock()
	s.dec.seed(modes)
	s.altScreen.Store(s.dec.altActive())
	s.bufMu.Unlock()
}

// dropStaleAltModes сверяет alt-screen трекера с зеркалом и вычищает мусор.
//
// Сканер трекера понимает ?1049l и RIS, но не может увидеть смерть процесса:
// TUI, исчезнувший без управляющего выхода, числится «в альте» у pty-host и
// сидируется агенту при каждом attach. Дальше он реассертился клиенту на
// КАЖДОМ маркере: телефон входил в alt-screen с mouse-tracking, жест прокрутки
// уходил приложению SGR-колесом (которое Claude Code игнорирует), и человек
// видел «не скроллится» — при том что зеркало отдавало полноценную историю
// ОСНОВНОГО буфера. Боевой случай 13.08.2026, сессия «Отчет»: modes маркера
// несли ?1049h+мышь при НУЛЕ переключений режимов в 512 КБ хвоста кольца.
//
// Правду знает зеркало: vt исполняет весь поток, включая RIS. Чистим только
// при живом зеркале с разобранной очередью; свежий вход в альт клиент и так
// получит сырым потоком — реассерт существует лишь для ВЫТЕСНЕННЫХ байтов, а
// вытесненный вход в альт при «vt в основном буфере» и есть мусор.
func (s *Session) dropStaleAltModes() {
	s.bufMu.Lock()
	defer s.bufMu.Unlock()
	if !s.dec.altActive() {
		return
	}
	// bufMu freezes totalBytes/DEC between the proof and the prune. The mirror
	// is allowed to disprove alt only after it has applied that exact absolute
	// offset; scan-before-feed therefore returns live=false instead of deleting
	// a freshly observed ?1049h.
	alt, live := s.screenAltLiveAt(s.totalBytes)
	if !live || alt {
		return
	}
	if s.dec.altActive() && s.dec.dropAlt() {
		s.altScreen.Store(false)
	}
}

// ModeReassertSeq возвращает SET-последовательности всех активных отслеживаемых
// DEC-режимов (см. decTracker), чтобы клиент после reset-resync заново вошёл в
// alt-screen / mouse / bracketed-paste. Пусто для обычного shell.
func (s *Session) ModeReassertSeq() string {
	s.dropStaleAltModes()
	s.bufMu.Lock()
	defer s.bufMu.Unlock()
	return s.dec.reassertSeq()
}

// ModeSyncSeq возвращает ПОЛНУЮ синхронизацию отслеживаемых DEC-режимов: SET
// для активных и RESET для неактивных. Нужна при gap-резюме: в потерянной
// середине потока режимы могли переключиться в ЛЮБУЮ сторону, и одним
// reassert (только SET) клиент, застрявший в alt-screen, из него не вытащишь.
func (s *Session) ModeSyncSeq() string {
	s.dropStaleAltModes()
	s.bufMu.Lock()
	defer s.bufMu.Unlock()
	return s.dec.syncSeq()
}

// Unsubscribe removes a subscriber channel.
func (s *Session) Unsubscribe(ch chan []byte) {
	s.subMu.Lock()
	delete(s.subs, ch)
	s.subMu.Unlock()
}

// Write sends raw input to the PTY.
func (s *Session) Write(data []byte) (int, error) {
	s.inputMu.Lock()
	defer s.inputMu.Unlock()
	return (activityInputWriter{Writer: s.conn(), activity: &s.codexActivity}).Write(data)
}

// WritePaste owns input until the last chunk (including optional Enter).
// Capture one backend: after a reconnect an uncertain paste is never resumed.
func (s *Session) WritePaste(data []byte) error {
	s.inputMu.Lock()
	defer s.inputMu.Unlock()
	return WriteInputChunks(activityInputWriter{Writer: s.conn(), activity: &s.codexActivity}, data)
}

// Resize changes the PTY dimensions.
func (s *Session) Resize(cols, rows int) error {
	if cols <= 0 || rows <= 0 {
		return nil
	}
	// REST/direct resize shares the same ordered apply lane and sequence as
	// viewer aggregation. Otherwise a concurrent direct call could finish last
	// while AppliedSize still described the viewer request it overtook.
	s.viewApplyMu.Lock()
	defer s.viewApplyMu.Unlock()
	s.viewMu.Lock()
	s.stopGrowLocked()
	s.viewNextSeq++
	seq := s.viewNextSeq
	s.viewMu.Unlock()
	return s.applyViewerGeometry(seq, cols, rows, false)
}

// applyBackendGeometry — низкоуровневый ЕДИНСТВЕННЫЙ путь применения размера:
// PTY и callback состояния/зеркала меняются только на одном completion-barrier.
//
// Путей смены размера три (Resize снаружи, resizeToViewers, отложенный
// applyGrow), и зеркало ресайзил ровно один из них. Отложенный рост — самый
// частый случай: телефон ушёл, через четыре секунды оставшийся зритель вернул
// размер побольше, PTY стал новым, а зеркало продолжило разбирать тот же поток в
// старой сетке. Дальше ScreenFrame сообщал СТАРЫЕ screen_cols/screen_rows и
// собирал строки, курсор и scroll-регионы не по своим местам — то самое «кадр
// применился, а экран разъехался». Внешний аудит 13.08.2026, находка T-006.
// applyBackendGeometry runs afterApply at the protocol's ordered completion
// boundary. On a new host this is inside readDecoded while handling ACK, so the
// callback finishes before the next post-resize output frame can be read.
func (s *Session) applyBackendGeometry(cols, rows int, afterApply func()) error {
	s.beginScreenGeometryChange()
	var geometryDone sync.Once
	finishGeometry := func() {
		geometryDone.Do(s.finishScreenGeometryChange)
	}
	finishSuccess := func() {
		geometryDone.Do(func() {
			if afterApply != nil {
				afterApply()
			}
			s.finishScreenGeometryChange()
		})
	}

	// Снача host/PTY, затем зеркало. Новый host возвращает
	// frResizeAck только после применения; старый сохраняет прежнюю
	// семантику «кадр записан». При ошибке зеркало не расходится с PTY.
	conn := s.conn()
	// Pipe transport additionally holds its output reader on frResizeAck until
	// this callback completes. Therefore no post-resize frOutput can be parsed
	// by the mirror in its old geometry. A new client attached to a pre-ACK host
	// reports the compatibility method as unsupported.
	if ordered, ok := orderedResizeFor(conn); ok {
		if completion, ok := conn.(orderedResizeCompletionConn); ok {
			err := completion.ResizeOrderedComplete(cols, rows, afterApply, func(error) {
				finishGeometry()
			})
			if err == nil || !errors.Is(err, errResizeAckTimeout) {
				// Send failures have no waiter callback; synchronous ACK/NACK
				// already finished through afterDone and sync.Once makes this safe.
				finishGeometry()
			}
			return err
		}
		err := ordered.ResizeOrdered(cols, rows, finishSuccess)
		if err == nil || !errors.Is(err, errResizeAckTimeout) {
			// nil normally means finishSuccess already ran. For a broken custom
			// implementation, do not leave screen capture disabled forever.
			finishGeometry()
		}
		// A soft timeout deliberately keeps the request pending: its waiter and
		// callback remain live, and a late ACK may still apply geometry.
		return err
	}

	// SSH/direct PTY/legacy-host resize has no transport ACK that can share the
	// output reader's FIFO. Serialize the already-returned output commit and
	// the resize+screen marker instead. Read itself stays outside this mutex, so
	// an idle terminal can always be resized; whichever operation acquires the
	// lane first defines the observable order seen by ring, subscribers and the
	// mirror together.
	s.outputApplyMu.Lock()
	defer s.outputApplyMu.Unlock()
	if err := conn.Resize(cols, rows); err != nil {
		finishGeometry()
		return err
	}
	// There is no protocol proof assigning bytes already returned by Read to
	// the old or new PTY geometry. Keep raw delivery flowing, but never publish
	// a potentially misparsed mirror frame until RIS/full stream reset restores
	// an authoritative baseline. This must happen before resizeScreen callback.
	newGeneration := s.outputGeneration.Add(1)
	s.invalidateScreenDelivery(newGeneration)
	finishSuccess()
	return nil
}

// applyViewerGeometry commits the aggregate from the actual completion
// callback, not from the request return. Thus a late ACK after the soft timeout
// still updates mirror, AppliedSize and persistence before post-output. A lower
// sequence that somehow completes after a newer one is ignored entirely.
func (s *Session) applyViewerGeometry(seq uint64, cols, rows int, delayed bool) error {
	return s.applyBackendGeometry(cols, rows, func() {
		s.viewCommitMu.Lock()
		defer s.viewCommitMu.Unlock()

		s.viewMu.Lock()
		if seq <= s.viewAppliedSeq {
			s.viewMu.Unlock()
			return
		}
		fromCols, fromRows := s.viewCols, s.viewRows
		s.viewMu.Unlock()

		// Must precede the state commit and return from the ACK callback: the
		// output reader is held here and cannot parse future-geometry bytes yet.
		s.resizeScreen(cols, rows)
		if fromCols != cols || fromRows != rows {
			s.bufMu.Lock()
			s.geometryAt, s.geometryEpoch = s.totalBytes, s.epoch
			s.bufMu.Unlock()
		}

		s.viewMu.Lock()
		s.viewCols, s.viewRows = cols, rows
		s.viewAppliedSeq = seq
		watchers := len(s.viewers)
		persist := s.persistView
		s.viewMu.Unlock()

		suffix := ""
		if delayed {
			suffix = ", отложенный рост"
		}
		log.Printf("[PTY] размер id=%s %dx%d → %dx%d (зрителей %d%s)", s.ID, fromCols, fromRows, cols, rows, watchers, suffix)
		if persist != nil {
			persist(cols, rows)
		}
	})
}

// ── Зрители терминала (см. Session.viewers) ──────────────────────

// Viewer — один открытый экран терминала. Хранит размер ИМЕННО этого окна,
// чтобы общий PTY можно было держать по самому узкому из них.
type Viewer struct {
	cols         int
	rows         int
	id           string
	controls     bool
	controlsWake chan struct{}
	controlError string
}

// AddViewer регистрирует открытый экран. Размера у него сначала нет — он
// приедет первым же resize от клиента, и до тех пор зритель на размер PTY не
// влияет (иначе новая вкладка мгновенно сжимала бы терминал в нули).
func (s *Session) AddViewer() *Viewer {
	v := &Viewer{controlsWake: make(chan struct{}, 1)}
	s.viewMu.Lock()
	v.id = s.nextViewerIDLocked()
	if s.viewers == nil {
		s.viewers = make(map[*Viewer]struct{})
	}
	s.viewers[v] = struct{}{}
	s.sizeRevision++
	s.notifySizeControlsLocked()
	s.viewMu.Unlock()
	return v
}

// RemoveViewer снимает экран с учёта и возвращает PTY размер по оставшимся:
// телефон закрыли — окно на ПК получает обратно свои 200 колонок само, без
// «подёргайте окно, чтобы починилось».
func (s *Session) RemoveViewer(v *Viewer) {
	if v == nil {
		return
	}
	s.viewMu.Lock()
	delete(s.viewers, v)
	if s.sizeOwner == v {
		s.clearSizeOwnerLocked()
	}
	s.sizeRevision++
	s.notifySizeControlsLocked()
	s.viewMu.Unlock()
	// Зритель УШЁЛ — вот здесь рост и надо откладывать: телефон уходит и
	// возвращается постоянно, и каждый круг стоил TUI полной перерисовки.
	_ = s.resizeToViewers(true)
}

// ViewerCount — сколько экранов смотрит в терминал прямо сейчас.
func (s *Session) ViewerCount() int {
	s.viewMu.Lock()
	defer s.viewMu.Unlock()
	return len(s.viewers)
}

// ResizeFor запоминает размер окна конкретного зрителя и применяет к PTY
// наименьший среди всех. Без этого последний пришедший resize переставлял общий
// PTY: телефон, открытый рядом с окном на ПК, ужимал работающий TUI агента в
// свои 60 колонок, а на большом экране тот оставался узкой колонкой.
func (s *Session) ResizeFor(v *Viewer, cols, rows int) error {
	if cols <= 0 || rows <= 0 {
		return nil
	}
	if v == nil {
		return s.Resize(cols, rows)
	}
	s.viewMu.Lock()
	if _, exists := s.viewers[v]; !exists {
		s.viewMu.Unlock()
		return nil
	}
	if v.cols != cols || v.rows != rows {
		s.sizeRevision++
		s.notifySizeControlsLocked()
	}
	v.cols, v.rows = cols, rows
	s.viewMu.Unlock()
	// Зритель СМОТРИТ ПРЯМО СЕЙЧАС и сообщил свой размер — применяем сразу, в
	// обе стороны. Откладывать рост здесь нельзя: человек открывает терминал,
	// у которого PTY меньше его экрана (например 33 строки от прошлого захода
	// с клавиатурой), и до применения видит содержимое в ВЕРХНЕЙ ПОЛОВИНЕ, а
	// ниже — пустоту. Живая жалоба владельца 04.08: «иногда вот так
	// открывается в пол-экрана».
	return s.resizeToViewers(false)
}

// Каждая СМЕНА размера PTY заставляет полноэкранное приложение перерисовать
// себя целиком, и для TUI с длинной перепиской это не «кадр», а вся переписка
// заново. Живой разбор 04.08.2026 (жалоба «вывод зациклился»): в буфере сессии
// Kimi нашлось 143 копии её приветственного блока через ровные ~418 строк, при
// ОДНОМ И ТОМ ЖЕ session_id — то есть агент не перезапускался, он 143 раза
// печатал себя заново. Кадр в 418 строк против терминала в 33 строки стереть
// нечем, поэтому TUI печатает его целиком.
//
// Откуда 143 перерисовки. Размер общий и считается по самому узкому зрителю,
// поэтому он ПРЫГАЛ туда-сюда: телефон подключился → 48 колонок, телефон ушёл
// → обратно 138, вернулся через секунду → снова 48. А телефон уходит и
// возвращается постоянно (карман, смена сети, засыпание вкладки).
//
// Отсюда два правила ниже:
//   - УМЕНЬШЕНИЕ применяем сразу: зрителю, который смотрит СЕЙЧАС, иначе рвёт
//     строки;
//   - УВЕЛИЧЕНИЕ откладываем на viewerGrowQuiet. Растить размер спешить некуда:
//     тот, кому нужно больше, и так видит меньше строк, зато вернувшийся через
//     секунду телефон отменяет отложенный рост — и PTY не меняется ВООБЩЕ.
const (
	viewerGrowQuiet = 4 * time.Second
	// Пол размера: вырожденный зритель (свёрнутое окно, вкладка в фоне) не
	// вправе ужать общий терминал до нечитаемого.
	minPtyCols = 20
	minPtyRows = 10
)

// setBornSize запоминает размер, с которым сессия родилась. Ноль означает
// «неизвестен» (сессия, поднятая старой записью без размера) — тогда к
// исходному размеру не возвращаемся вовсе, то есть остаётся прежнее поведение.
func (s *Session) setBornSize(cols, rows int) {
	if cols <= 0 || rows <= 0 {
		return
	}
	s.viewMu.Lock()
	s.bornCols, s.bornRows = cols, rows
	s.viewMu.Unlock()
}

// AppliedSize — размер, ПРИМЕНЁННЫЙ к PTY прямо сейчас (0,0 — ещё ни разу).
//
// Нужен экрану терминала как ЛОГИЧЕСКАЯ геометрия. На телефоне видимая область
// и размер терминала — разные вещи: поднятая клавиатура забирает две трети
// экрана, но PTY при этом не меняется (иначе TUI перерисовывает всю переписку,
// см. targetSizeLocked). Клиенту, открытому с уже поднятой клавиатурой, неоткуда
// узнать настоящую высоту — и он писал кадр в 30 строк в терминал, ужатый до
// десяти: строки 11–30 адресуются за край и складываются в последнюю
// (боевые снимки 13.08.2026: `снапшот=48x31 клиент=48x11`).
func (s *Session) AppliedSize() (int, int) {
	s.viewMu.Lock()
	defer s.viewMu.Unlock()
	return s.viewCols, s.viewRows
}

// targetSizeLocked — какой размер PTY нужен прямо сейчас. Зовётся под viewMu.
func (s *Session) targetSizeLocked() (int, int) {
	if owner := s.sizeOwner; owner != nil && time.Now().Before(s.sizeLeaseUntil) {
		if _, exists := s.viewers[owner]; exists && owner.cols >= 2 && owner.rows >= 2 {
			// Until every client can pan an oversized live VT grid, ownership
			// cannot place input outside another viewer's accessible screen.
			// This server-side guard includes legacy/unnegotiated viewers.
			cols, rows := owner.cols, owner.rows
			for other := range s.viewers {
				if other.cols > 0 && other.rows > 0 {
					cols = min(cols, other.cols)
					rows = min(rows, other.rows)
				}
			}
			return max(minPtyCols, cols), max(minPtyRows, rows)
		}
	}
	cols, rows := 0, 0
	for other := range s.viewers {
		if other.cols <= 0 || other.rows <= 0 {
			continue // зритель ещё не сообщил свой размер
		}
		if cols == 0 || other.cols < cols {
			cols = other.cols
		}
		if rows == 0 || other.rows < rows {
			rows = other.rows
		}
	}
	if cols <= 0 || rows <= 0 {
		// Не смотрит никто.
		//
		// Полноэкранный TUI не трогаем ВОВСЕ: у alt-screen нет прокручиваемой
		// истории, портить которой нечего, зато каждая смена размера — это
		// полная перерисовка, и приложение в простое может стереть экран, не
		// нарисовав ничего взамен. Живой случай владельца 09.08.2026, сессия
		// c70c4bb42accd938: телефон отвалился на полторы минуты, PTY отскочил
		// 48x30 → 80x24, телефон вернулся — 80x24 → 48x30, и Claude Code, уже
		// закончивший работу, выдал последним в буфер `ESC[H` и двадцать два
		// `ESC[K` подряд. Чёрный экран при живой сессии и целом ответе агента.
		// За сутки таких отскоков набралось 335.
		//
		// Возврат к born нужен обычному шеллу: у него история ОСТАЁТСЯ, и
		// напечатанная в 48 колонок телефона она такой навсегда и останется,
		// даже когда человек откроет терминал за компьютером (жалоба
		// 04.08.2026 — сессия Kimi жила в 48 колонках).
		//
		// Агентская сессия без alt-screen (Kimi: режимы [1004, 2004]) правилом
		// выше НЕ покрывалась, и отскок к born ломал ей экран иначе
		// (жалоба владельца 11.08.2026 «у Kimi вывод странный», разбор —
		// Контекст/Журнал/2026-08-11_kimi-strannyy-vyvod-na-telefone.md):
		// на каждый уход/возврат телефона PTY прыгал 48x30 → 80x24 → 48x30,
		// а Kimi рисует строками по АБСОЛЮТНЫМ адресам без полного стирания —
		// кадры, напечатанные под одну ширину и высоту, навсегда оседали в
		// прокрутке зрителя другого размера («Working…» посреди текста).
		// Без зрителей размер агенту не нужен никому: следующий зритель всё
		// равно пришлёт свой, и TUI перерисуется под него один раз.
		if s.altScreen.Load() || s.lastAgentKind() != "" {
			return s.viewCols, s.viewRows
		}
		cols, rows = s.bornCols, s.bornRows
	}
	if cols > 0 && cols < minPtyCols {
		cols = minPtyCols
	}
	if rows > 0 && rows < minPtyRows {
		rows = minPtyRows
	}
	return cols, rows
}

func (s *Session) stopGrowLocked() {
	if s.growTimer != nil {
		s.growTimer.Stop()
		s.growTimer = nil
	}
}

// applyGrow — отложенный рост. К этому моменту состав зрителей мог измениться
// ещё раз, поэтому размер считается ЗАНОВО, а не берётся из замыкания.
func (s *Session) applyGrow() {
	s.viewApplyMu.Lock()
	defer s.viewApplyMu.Unlock()
	s.viewMu.Lock()
	s.growTimer = nil
	cols, rows := s.targetSizeLocked()
	settled := s.viewNextSeq == s.viewAppliedSeq
	if cols <= 0 || rows <= 0 || (cols == s.viewCols && rows == s.viewRows && settled) {
		s.viewMu.Unlock()
		return
	}
	s.viewNextSeq++
	seq := s.viewNextSeq
	s.viewMu.Unlock()
	if err := s.applyViewerGeometry(seq, cols, rows, true); err != nil {
		log.Printf("[PTY] отложенный resize id=%s → %dx%d отклонён: %v", s.ID, cols, rows, err)
		return
	}
}

// resizeToViewers применяет размер по живым зрителям. Уменьшение — сразу,
// увеличение — после паузы (см. комментарий выше).
// deferGrow — откладывать ли УВЕЛИЧЕНИЕ. Истина только там, где зритель ушёл:
// расти для того, кто уже не смотрит, спешить некуда, а вернувшийся через
// секунду телефон отменит отложенный рост, и PTY не дёрнется вовсе. Когда
// зритель, наоборот, ПРИШЁЛ или сообщил новый размер, ждать нельзя: он смотрит
// в экран прямо сейчас.
func (s *Session) resizeToViewers(deferGrow bool) error {
	s.viewApplyMu.Lock()
	defer s.viewApplyMu.Unlock()
	s.viewMu.Lock()
	cols, rows := s.targetSizeLocked()
	settled := s.viewNextSeq == s.viewAppliedSeq
	if cols <= 0 || rows <= 0 || (cols == s.viewCols && rows == s.viewRows && settled) {
		// Размер уже правильный — отменяем и отложенный рост, если он был
		// назначен предыдущим уходом зрителя: тот вернулся, менять нечего.
		s.stopGrowLocked()
		s.viewMu.Unlock()
		return nil
	}
	// Первый размер сессии ставим сразу: терминал должен открыться правильным.
	//
	// ⚠ Сравнение размеров здесь СТОЯЛО и не работало (найдено 04.08.2026 при
	// разборе жалобы «у Кими прокрутка вверх перескакивает»). Условие требовало
	// роста по ОБЕИМ осям, а возврат к born — это 48x31 → 80x24: шире, но НИЖЕ.
	// По строкам это уменьшение, поэтому отсрочка пропускалась, и каждый уход
	// телефона бил полной перерисовкой немедленно. Боевой лог за сутки: 13 строк
	// «зрителей 0» — все мгновенные, ни одной «отложенный рост».
	// Сравнивать размеры тут не нужно вовсе: deferGrow=true бывает ТОЛЬКО в
	// RemoveViewer, а уход зрителя может лишь ПОДНЯТЬ минимум по оставшимся
	// (targetSizeLocked) — уменьшения в этой ветке не бывает по построению.
	// Правило v2.49.14 «уменьшение живому зрителю — сразу» не задето: оно идёт
	// через ResizeFor с deferGrow=false.
	if deferGrow && s.viewCols > 0 {
		if s.growTimer == nil {
			s.growTimer = time.AfterFunc(viewerGrowQuiet, s.applyGrow)
		}
		s.viewMu.Unlock()
		return nil
	}
	s.stopGrowLocked()
	s.viewNextSeq++
	seq := s.viewNextSeq
	s.viewMu.Unlock()
	return s.applyViewerGeometry(seq, cols, rows, false)
}

// CurrentCWD returns the live working directory of the shell process,
// falling back to the initial cwd if the OS query fails.
func (s *Session) CurrentCWD() string {
	if !s.IsAlive() {
		return s.CWD
	}
	if cwd, err := s.conn().currentCWD(); err == nil && cwd != "" {
		return cwd
	}
	return s.CWD
}

// ForegroundProcess returns information about the deepest descendant of the
// shell process — i.e. what's actually running in the terminal right now.
// If only the shell itself is running (no children), Name == shell basename.
func (s *Session) ForegroundProcess() ProcessInfo {
	if !s.IsAlive() {
		return ProcessInfo{}
	}
	pid := s.conn().shellPID()
	if pid == 0 {
		return ProcessInfo{}
	}
	info, err := findForegroundProcess(pid)
	return foregroundProcessOrShell(pid, s.Shell, info, err)
}

func foregroundProcessOrShell(shellPID uint32, shell string, info ProcessInfo, err error) ProcessInfo {
	if err != nil {
		// Process enumeration can fail transiently. Returning an unknown process
		// preserves the current client generation instead of falsely announcing
		// that the agent exited.
		return ProcessInfo{}
	}
	if info.PID != 0 {
		return info
	}
	// A successful scan with no child means the shell itself is foreground.
	// Its stable PID lets clients leave an agent-specific scroll default when
	// Claude/Codex/Kimi exits, while a genuine scan failure above remains
	// transient and does not erase a person's manual choice.
	name := strings.TrimSuffix(strings.ToLower(filepath.Base(shell)), ".exe")
	return ProcessInfo{PID: shellPID, Name: name}
}

// Scrollback returns a snapshot of the current scrollback buffer.
func (s *Session) Scrollback() []byte {
	s.bufMu.Lock()
	defer s.bufMu.Unlock()
	out := make([]byte, len(s.buf))
	copy(out, s.buf)
	return out
}

// IsAlive returns true if the read loop hasn't ended yet.
func (s *Session) IsAlive() bool {
	select {
	case <-s.done:
		return false
	default:
		return true
	}
}

// Done returns a channel that is closed when the session process exits.
func (s *Session) Done() <-chan struct{} {
	return s.done
}

// close detaches the backend. For a persistent host this leaves the shell
// running (the host survives); for a local ConPTY it terminates the shell.
func (s *Session) close() error {
	return s.conn().Close()
}

// kill terminates the backend's shell unconditionally. For a persistent host
// it sends Kill so the host process exits too; for a local ConPTY it is the
// same as close (TerminateProcess).
func (s *Session) kill() error {
	if k, ok := s.conn().(interface{ kill() error }); ok {
		return k.kill()
	}
	return s.conn().Close()
}

// shouldForget reports whether the store entry may be dropped when this session
// dies. A persistent host backend says no while its host is still alive (pipe
// broke but shell lives) so a restart can re-attach; a clean shell exit says
// yes. A local ConPTY always says yes (no persistence).
func (s *Session) shouldForget() bool {
	if e, ok := s.conn().(interface{ exitedClean() bool }); ok {
		return e.exitedClean()
	}
	return true
}

// ── Helpers ──────────────────────────────────────────────────────

func randomID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}
