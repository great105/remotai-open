package pty

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"tgcontrol/internal/agenthooks"
)

// deathGrace is how long a host lingers after its shell exits, so a reconnecting
// client (e.g. remotai restarting) can still pull the final scrollback + exit.
const deathGrace = 30 * time.Second

// snapshotChunk — размер куска снапшота при передаче клиенту. Половина предела
// кадра (maxFrame): запас на заголовок и на то, что предел когда-нибудь снизят,
// а буфер терминала (scrollbackSize) вырастет — именно расхождение этих двух
// чисел один раз уже стоило владельцу «отваливающихся» терминалов.
const snapshotChunk = maxFrame / 2

// hostSlowClientWait — сколько хост ждёт своего (локального) читателя, прежде
// чем признать кадр потерянным. Подпор на пару секунд — нормальное поведение
// терминала: шелл притормаживает, пока читатель разбирает вывод. Потолок нужен
// только чтобы заклинивший агент не подвесил шелл насовсем.
const hostSlowClientWait = 2 * time.Second

// Production host PTYs expose a non-blocking availability probe. Keeping the
// actual read and FIFO publication under bufMu closes the seam where Read had
// already obtained old-geometry bytes but the goroutine was descheduled before
// publishOutput while Resize+ACK passed it. Polling only happens while idle.
const hostReadPoll = 5 * time.Millisecond

// hostPTY is what a pty-host owns: a ConPTY (Windows) or a POSIX PTY (Linux).
// Both conPTY types already satisfy it (see foreground_*.go, cwd_*.go, and the
// exitCode() method in host_windows.go / host_linux.go).
type hostPTY interface {
	Read([]byte) (int, error)
	Write([]byte) (int, error)
	Resize(cols, rows int) error
	Close() error
	shellPID() uint32
	currentCWD() (string, error)
	exitCode() int
}

type availableHostPTY interface {
	readAvailable([]byte) (int, error)
}

// hostTransport is the server side of the host↔remotai IPC channel: an
// overlapped named pipe on Windows, a unix socket on Linux. connect() waits for
// a client; wake() aborts a parked connect() after the grace window (Windows:
// CancelIoEx; Linux: close the listener). Writes are serialized by the impl.
type hostTransport interface {
	connect() error
	disconnect()
	readFrame() (frameType, []byte, error)
	writeFrame(t frameType, payload []byte) error
	wake()
	Close() error
}

// hostOutbound is the single ordered host→client lane. Resize ACK shares
// this FIFO with output so it cannot overtake bytes queued before Resize, and
// output queued after Resize cannot overtake the ACK.
type hostOutbound struct {
	typ     frameType
	payload []byte
}

type hostSubscription struct {
	out       chan hostOutbound
	broken    chan struct{}
	breakOnce sync.Once
}

func newHostSubscription(depth int) *hostSubscription {
	return &hostSubscription{out: make(chan hostOutbound, depth), broken: make(chan struct{})}
}

func (s *hostSubscription) breakContinuity() {
	if s != nil {
		s.breakOnce.Do(func() { close(s.broken) })
	}
}

// host is the pty-host process state: it owns the PTY, keeps scrollback, and
// serves one client (remotai) at a time over the transport.
type host struct {
	pty     hostPTY
	srv     hostTransport
	shell   string
	cwd     string
	created int64
	// streamEpoch живёт ровно столько же, сколько этот host/поток.
	streamEpoch string

	bufMu sync.Mutex
	buf   []byte
	// produced — сколько байт хост выдал ВСЕГО с рождения сессии (кольцо buf
	// хранит только хвост). По нему считается, что именно до-слать клиенту,
	// который вернулся с известной позицией (frClientHello.known). Под bufMu.
	produced uint64
	dec      *decTracker // DEC-режимы по всему потоку с рождения сессии (под bufMu)

	subMu sync.Mutex
	sub   *hostSubscription // current client's ordered output/control lane

	dead     chan struct{}
	deadOnce sync.Once
	exitCode int
}

// HostLogPath — куда пишет отвязанный pty-host. Один источник правды для самого
// хоста и для сообщений об ошибках: диагностика бесполезна, если указывает не на
// тот файл.
func HostLogPath(id string) string {
	return filepath.Join(os.TempDir(), "remotai-ptyhost-"+id+".log")
}

// RunHost is the entry point for `remotai --pty-host`. It blocks until the shell
// is killed (frKill) or exits and the grace period elapses. Returns a process
// exit code. The transport is created per-platform via newHostServer.
func RunHost(id, shell, cwd string, cols, rows int) int {
	// Dedicated log file — the host is detached, stderr is dead.
	// Путь обязан совпадать с HostLogPath: на него ссылается ошибка запуска, и
	// расхождение уже стоило двух заходов в тупик (10.08.2026 — сообщение вело
	// в ~/Library/Logs/Remotai/pty-host.log, а лог всё это время писался сюда).
	if lf, err := os.OpenFile(HostLogPath(id),
		os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600); err == nil {
		log.SetOutput(lf)
		defer lf.Close()
	}

	if shell == "" {
		shell = defaultShell()
	} else if p, err := exec.LookPath(shell); err == nil {
		shell = p
	}
	if cwd == "" {
		cwd = "."
	}
	log.Printf("[ptyhost %s] start pid=%d shell=%s cwd=%s %dx%d", id, os.Getpid(), shell, cwd, cols, rows)

	// Хук ИИ-агента, запущенного в этом терминале, узнаёт из окружения, куда
	// класть события (internal/agenthooks). Ставим в окружение самого хоста:
	// шелл наследует его на всех трёх платформах (buildEnvBlock и
	// build*PTYEnv начинают с os.Environ).
	spool := agenthooks.SpoolPath(id)
	_ = os.Setenv(agenthooks.EnvFile, spool)
	_ = os.Setenv(agenthooks.EnvPTY, id)
	defer func() {
		_ = os.Remove(spool)
		_ = os.Remove(spool + ".old")
	}()

	// Разметку команд (OSC 133, ST-10) агент заказывает переменной окружения,
	// а не флагом командной строки: живой хост старой версии флага не знает, а
	// переменная новому хосту не мешает (I-14). Снимаем её сразу — шелл и всё,
	// что в нём запустят (в том числе вложенный remotai), её не наследуют.
	integ := os.Getenv(EnvShellIntegration) == "1"
	_ = os.Unsetenv(EnvShellIntegration)
	argv, extraEnv := prepareShellLaunch(shell, integ, os.Environ())
	if len(argv) > 1 || len(extraEnv) > 0 {
		log.Printf("[ptyhost %s] разметка команд включена (%s)", id, shellIntegrationKind(shell))
	}

	p, err := newPlatformPTY(cols, rows, cwd, argv, extraEnv)
	if err != nil {
		log.Printf("[ptyhost %s] PTY: %v", id, err)
		return 1
	}

	srv, err := newHostServer(id)
	if err != nil {
		log.Printf("[ptyhost %s] transport: %v", id, err)
		p.Close()
		return 1
	}
	defer srv.Close()

	h := &host{
		pty:         p,
		srv:         srv,
		shell:       shell,
		cwd:         cwd,
		created:     time.Now().UnixMilli(),
		streamEpoch: randomID(),
		buf:         make([]byte, 0, scrollbackSize),
		dec:         newDecTracker(),
		dead:        make(chan struct{}),
	}

	go h.drainLoop()
	h.acceptLoop()
	log.Printf("[ptyhost %s] exit code=%d", id, h.exitCode)
	return h.exitCode
}

// drainLoop continuously reads PTY output into the scrollback buffer and fans
// out to the active client. Runs even with no client connected so the PTY pipe
// never backs up (which would block the shell on write).
func (h *host) drainLoop() {
	defer h.markDead()
	buf := make([]byte, 8192)
	if _, ok := h.pty.(availableHostPTY); ok {
		for {
			h.bufMu.Lock()
			n, err := h.readAvailableLocked(buf)
			h.bufMu.Unlock()
			if err != nil {
				return
			}
			if n == 0 {
				time.Sleep(hostReadPoll)
			}
		}
	}
	// Defensive fallback for non-platform test/third-party implementations.
	// Every production conPTY implements availableHostPTY.
	for {
		n, err := h.pty.Read(buf)
		if n > 0 {
			data := make([]byte, n)
			copy(data, buf[:n])
			h.publishOutput(data)
		}
		if err != nil {
			return
		}
	}
}

func (h *host) publishOutput(data []byte) { h.publishOutputWithHook(data, nil) }

// publishOutputWithHook keeps ring mutation and ordered delivery in one bufMu
// critical section. The bounded slow send also stays under bufMu: otherwise a
// concurrent resize ACK could race that already-observed pre-resize frame for
// the first free channel slot and overtake it.
func (h *host) publishOutputWithHook(data []byte, beforeSlow func()) {
	h.publishOutputWithHookWait(data, beforeSlow, hostSlowClientWait)
}

func (h *host) publishOutputWithHookWait(data []byte, beforeSlow func(), wait time.Duration) {
	h.bufMu.Lock()
	defer h.bufMu.Unlock()
	h.publishOutputLockedWait(data, beforeSlow, wait)
}

func (h *host) publishOutputLocked(data []byte, beforeSlow func()) {
	h.publishOutputLockedWait(data, beforeSlow, hostSlowClientWait)
}

func (h *host) publishOutputLockedWait(data []byte, beforeSlow func(), wait time.Duration) {
	h.buf = append(h.buf, data...)
	h.produced += uint64(len(data))
	if len(h.buf) > scrollbackSize {
		h.buf = h.buf[len(h.buf)-scrollbackSize:]
	}
	if h.dec != nil {
		h.dec.scan(data)
	}
	h.subMu.Lock()
	sub := h.sub
	h.subMu.Unlock()
	if sub == nil {
		return
	}
	out := sub.out
	event := hostOutbound{typ: frOutput, payload: data}
	select {
	case out <- event:
		return
	default:
	}
	if beforeSlow != nil {
		beforeSlow()
	}
	// Local agent backpressure is preferable to a reconnect. The wait is
	// bounded; on timeout continuity is explicitly broken rather than silently
	// dropping a range. Holding bufMu also preserves the resize barrier order.
	select {
	case out <- event:
	case <-time.After(wait):
		// Ring/produced already include this byte range, but this client did not
		// receive it. Continuing the same connection would make Session offsets
		// diverge from the host absolute scale. Break only this subscription;
		// reconnect known=last actually received will replay the exact suffix.
		h.breakSubscription(sub)
		log.Printf("[ptyhost] клиент не читает дольше %s — continuity разорвана, требуется replay (%d байт)", wait, len(data))
	case <-h.dead:
	}
}

func (h *host) breakSubscription(sub *hostSubscription) {
	h.subMu.Lock()
	if h.sub == sub {
		h.sub = nil
	}
	sub.breakContinuity()
	h.subMu.Unlock()
}

// readAvailableLocked reads at most one already-readable chunk and publishes
// it before releasing bufMu. It never waits for future PTY output.
func (h *host) readAvailableLocked(buf []byte) (int, error) {
	p, ok := h.pty.(availableHostPTY)
	if !ok {
		return 0, nil
	}
	for {
		n, err := p.readAvailable(buf)
		// Unix poll can be interrupted by a signal (including Go preemption).
		// That does not mean the shell exited. Returning EINTR stopped the
		// drain loop forever while markDead waited for a still-running shell:
		// input worked, but every later reconnect showed frozen output.
		if n == 0 && errors.Is(err, syscall.EINTR) {
			continue
		}
		if n > 0 {
			data := append([]byte(nil), buf[:n]...)
			h.publishOutputLocked(data, nil)
		}
		return n, err
	}
}

// drainAvailableLocked establishes the resize boundary: every byte already
// readable from the PTY is appended to the ring and the ordered outbound lane
// before Resize is applied and its ACK is queued.
func (h *host) drainAvailableLocked() error {
	if _, ok := h.pty.(availableHostPTY); !ok {
		return nil
	}
	buf := make([]byte, 8192)
	for {
		n, err := h.readAvailableLocked(buf)
		if err != nil {
			return err
		}
		if n == 0 {
			return nil
		}
	}
}

func (h *host) markDead() {
	h.deadOnce.Do(func() {
		h.exitCode = h.pty.exitCode()
		close(h.dead)
		// Wake a parked connect()/serveClient after the grace window so the
		// process can exit if no client comes back.
		time.AfterFunc(deathGrace, func() { h.srv.wake() })
	})
}

func (h *host) isDead() bool {
	select {
	case <-h.dead:
		return true
	default:
		return false
	}
}

// acceptLoop serves clients one at a time, reconnecting after each disconnect.
func (h *host) acceptLoop() {
	for {
		if err := h.srv.connect(); err != nil {
			if h.isDead() {
				return // grace timer woke the pending connect
			}
			log.Printf("[ptyhost] connect: %v", err)
			time.Sleep(100 * time.Millisecond)
			continue
		}
		killed := h.serveClient()
		if killed {
			h.pty.Close()
			return
		}
		// Client detached (likely remotai restart). Keep the shell alive and
		// wait for the next connection — unless the shell already exited and the
		// grace timer is running.
	}
}

// snapshotTail выбирает, какой хвост буфера до-слать клиенту, вернувшемуся с
// позиции known (сколько байт этой сессии он уже видел).
//
// Позиция ВНУТРИ кольца — точный хвост после неё, сколько бы он ни занял: это
// живой путь reattach, пропущенный вывод цел и обязан доехать полностью.
//
// Позиция ВЫТЕСНЕНА из кольца (known < start) — середина (known, start)
// потеряна безвозвратно, и отдавать ВЕСЬ буфер (до 4 МБ) нельзя: клиент трубы
// не отличает снапшот от живого потока (readDecoded), и лавина уходила живым
// подписчикам мидстримом — медленное мобильное плечо рвалось на write-дедлайне,
// дальше молчаливый реконнект и переигровка, отсюда видимое мельтешение
// «туда-сюда» (2026-08-07). Досылаем свежий хвост в пределах
// clientReplayLimit — ровно столько уходит клиенту при gap-резюме
// (SubscribeResume), то есть плечо такой объём уже переваривает. Дубля в кольце
// сессии при этом нет: весь отсылаемый хвост новее known.
//
// Непонятная позиция (ноль, из будущего) — прежнее «отдать всё»: для свежей
// сессии (ReattachSession, known=0) это единственный источник истории, и
// ошибиться здесь можно только в безопасную сторону.
func snapshotTail(buf []byte, produced, known uint64) []byte {
	if known > 0 && known <= produced {
		start := produced - uint64(len(buf))
		if known >= start {
			return buf[known-start:]
		}
		if len(buf) > clientReplayLimit {
			return buf[len(buf)-clientReplayLimit:]
		}
	}
	return buf
}

type snapshotSlice struct {
	Data  []byte
	Start uint64
}

func snapshotWindow(buf []byte, produced, known uint64) snapshotSlice {
	tail := snapshotTail(buf, produced, known)
	return snapshotSlice{Data: tail, Start: produced - uint64(len(tail))}
}

// Epoch от другого host делает known невалидным. Пустой epoch —
// legacy-клиент: для него сохраняем прежнюю семантику known.
func snapshotWindowForStream(buf []byte, produced, known uint64, knownEpoch, hostEpoch string) snapshotSlice {
	if knownEpoch != "" && knownEpoch != hostEpoch {
		known = 0
	}
	return snapshotWindow(buf, produced, known)
}

// applyHostResize applies the first four legacy-compatible bytes and returns
// the optional request sequence only after the PTY accepted the size. An ack
// on error would let the client resize its mirror to geometry the PTY rejected.
func applyHostResize(payload []byte, resize func(cols, rows int) error) ([]byte, error) {
	if len(payload) < 4 {
		return nil, nil
	}
	cols := int(binary.LittleEndian.Uint16(payload))
	rows := int(binary.LittleEndian.Uint16(payload[2:]))
	if err := resize(cols, rows); err != nil {
		return nil, err
	}
	if len(payload) < 8 {
		return nil, nil
	}
	return append([]byte(nil), payload[4:8]...), nil
}

func (h *host) queueHostResizeWith(out chan<- hostOutbound, writerDone <-chan struct{}, payload []byte, resize func(cols, rows int) error) error {
	// drainLoop uses the same bufMu while reading and appending each frOutput.
	// Drain already-readable old-geometry bytes, then hold the lock across
	// Resize+completion insertion to linearize the boundary in the FIFO.
	h.bufMu.Lock()
	defer h.bufMu.Unlock()
	h.subMu.Lock()
	activeBeforeDrain := h.sub
	h.subMu.Unlock()
	if err := h.drainAvailableLocked(); err != nil {
		return err
	}
	if activeBeforeDrain != nil {
		h.subMu.Lock()
		stillCurrent := h.sub == activeBeforeDrain && activeBeforeDrain.out == out
		h.subMu.Unlock()
		if !stillCurrent {
			return fmt.Errorf("pty host output continuity broke before resize")
		}
	}
	ack, err := applyHostResize(payload, resize)
	if err != nil {
		if seq := resizeRequestSeq(payload); seq != nil {
			if sendErr := queueHostControl(out, writerDone, frResizeNack, seq); sendErr != nil {
				return fmt.Errorf("%w (resize nack: %v)", err, sendErr)
			}
		}
		return err
	}
	if ack == nil {
		return nil
	}
	return queueHostControl(out, writerDone, frResizeAck, ack)
}

func resizeRequestSeq(payload []byte) []byte {
	if len(payload) < 8 {
		return nil
	}
	return append([]byte(nil), payload[4:8]...)
}

func queueHostControl(out chan<- hostOutbound, writerDone <-chan struct{}, typ frameType, payload []byte) error {
	select {
	case out <- hostOutbound{typ: typ, payload: payload}:
		return nil
	case <-writerDone:
		return fmt.Errorf("pty host writer stopped before resize response")
	}
}

func (h *host) queueHostResize(out chan<- hostOutbound, writerDone <-chan struct{}, payload []byte) error {
	return h.queueHostResizeWith(out, writerDone, payload, h.pty.Resize)
}

// serveClient handshakes and proxies one client connection. Returns true if the
// client asked to kill the session.
func (h *host) serveClient() bool {
	srv := h.srv
	defer srv.disconnect()

	// Handshake: expect ClientHello first.
	t, payload, err := srv.readFrame()
	if err != nil {
		return false
	}
	if t != frClientHello {
		return false
	}
	if len(payload) >= 6 {
		cols := int(binary.LittleEndian.Uint16(payload[2:]))
		rows := int(binary.LittleEndian.Uint16(payload[4:]))
		if cols > 0 && rows > 0 {
			_ = h.pty.Resize(cols, rows)
		}
	}
	// Необязательный хвост hello: «столько байт этой сессии у меня уже есть».
	// Короткий hello (старый клиент) — known остаётся нулём, то есть прежнее
	// поведение «отдать весь буфер».
	var known uint64
	if len(payload) >= 14 {
		known = binary.LittleEndian.Uint64(payload[6:])
	}
	knownEpoch := clientHelloKnownEpoch(payload)

	hm := HelloMsg{
		ShellPID:  h.pty.shellPID(),
		Shell:     h.shell,
		CWD:       h.cwd,
		Created:   h.created,
		ResizeAck: true,
	}
	if cwd, err := h.pty.currentCWD(); err == nil && cwd != "" {
		hm.CWD = cwd
	}

	// Snapshot, его absolute-границы и регистрация живого
	// подписчика снимаются под ОДНИМ bufMu. Иначе Hello мог бы
	// объявить Produced=N, а snapshot уже оказывался до N+K — та же
	// плавающая шкала, только спрятанная в handshake.
	sub := newHostSubscription(256)
	ch := sub.out
	h.bufMu.Lock()
	window := snapshotWindowForStream(h.buf, h.produced, known, knownEpoch, h.streamEpoch)
	snap := append([]byte(nil), window.Data...)
	produced := h.produced
	buffered := len(h.buf)
	hm.Modes = h.dec.snapshot()
	hm.StreamEpoch = h.streamEpoch
	hm.ReplayStart = window.Start
	hm.Produced = produced
	h.subMu.Lock()
	h.sub = sub
	h.subMu.Unlock()
	h.bufMu.Unlock()
	defer func() {
		h.subMu.Lock()
		if h.sub == sub {
			h.sub = nil
		}
		h.subMu.Unlock()
	}()
	abortWatchDone := make(chan struct{})
	defer close(abortWatchDone)
	go func() {
		select {
		case <-sub.broken:
			// Interrupt a blocked direct snapshot/write as well as the writer
			// loop. serveClient's deferred disconnect is intentionally idempotent.
			srv.disconnect()
		case <-abortWatchDone:
		}
	}()

	if err := srv.writeFrame(frHello, encodeHello(hm)); err != nil {
		return false
	}
	log.Printf("[ptyhost] client connected, hello sent pid=%d stream=%s replay=%d..%d", hm.ShellPID, hm.StreamEpoch, hm.ReplayStart, hm.Produced)
	if known > 0 {
		log.Printf("[ptyhost] клиент вернулся с позиции %d из %d — до-сылаем %d байт вместо %d",
			known, produced, len(snap), buffered)
	}
	// Снапшот уходит КУСКАМИ, а не одним кадром.
	//
	// Живой случай владельца (2026-07-30): буфер терминала в 2.46.4 подняли до
	// 4 МБ, а предел одного кадра протокола остался 1 МБ. У любого терминала,
	// где агент успел напечатать больше мегабайта, отправка снапшота падала с
	// «pty frame too large», хост рвал соединение — и агент, честно пытаясь
	// восстановить связь, получал обрыв снова и снова: 252 тысячи строк
	// «reattach-live … восстановлена» в логе за восемь минут, 35 МБ файла, и в
	// конце «терминал завершён» при живом процессе и работающем внутри агенте.
	// Клиент читает frSnapshot тем же путём, что frOutput (см. readDecoded),
	// поэтому несколько кадров он собирает сам — и старые версии тоже.
	for len(snap) > 0 {
		chunk := snap
		if len(chunk) > snapshotChunk {
			chunk = chunk[:snapshotChunk]
		}
		if err := srv.writeFrame(frSnapshot, chunk); err != nil {
			// История не отправилась — это НЕ повод рвать связь. Терминал важнее
			// своего прошлого: человек хотя бы продолжит работу, а хвост вывода у
			// него уже есть на клиенте. Раньше здесь стоял выход, и одна
			// неудачная отправка означала «терминал недоступен» навсегда.
			log.Printf("[ptyhost] снапшот не отправлен (%v) — продолжаем без истории", err)
			break
		}
		snap = snap[len(chunk):]
	}
	// ЯВНАЯ ГРАНИЦА ПЕРЕИГРОВКИ — сразу после последнего куска, даже пустого.
	// Без неё клиент трубы узнавал о конце переигровки только по первому ЖИВОМУ
	// кадру, и тихая сессия (агент закончил и молчит) оставляла мусор
	// переигровки в scrollback зеркала до первого вывода — или навсегда
	// (внешний аудит 2.57.18, P0-03). Здесь кадр ещё гарантированно обгоняет
	// любой frOutput: писатель живого потока стартует ниже.
	if err := srv.writeFrame(frReplayEnd, nil); err != nil {
		log.Printf("[ptyhost] граница переигровки не отправлена (%v) — клиент дождётся по первому выводу", err)
	}

	// Reader goroutine: client → PTY.
	readerDone := make(chan struct{})
	writerDone := make(chan struct{})
	var killed bool
	go func() {
		defer close(readerDone)
		for {
			ft, p, err := srv.readFrame()
			if err != nil {
				return
			}
			switch ft {
			case frInput:
				_, _ = h.pty.Write(p)
			case frResize:
				resizeErr := h.queueHostResize(ch, writerDone, p)
				if resizeErr != nil {
					// Новый клиент получает ordered NACK и не перестраивает
					// mirror. Legacy payload без seq ответа по-прежнему не ждёт.
					log.Printf("[ptyhost] resize rejected: %v", resizeErr)
					continue
				}
			case frPing:
				_ = srv.writeFrame(frPong, nil)
			case frKill:
				killed = true
				return
			case frDetach:
				return
			default: // ignore unknown frame types (forward-compat)
			}
		}
	}()

	// Writer loop: PTY → client.
writer:
	for {
		select {
		case event := <-ch:
			if err := srv.writeFrame(event.typ, event.payload); err != nil {
				break writer
			}
		case <-h.dead:
			_ = writeHostExitAfterDrain(ch, h.exitCode, srv.writeFrame)
			break writer
		case <-readerDone:
			break writer
		case <-sub.broken:
			break writer
		}
	}

	close(writerDone)
	srv.disconnect() // unblock the reader if it is still parked in a read
	<-readerDone
	return killed
}

// writeHostExitAfterDrain is called only after h.dead closes: drainLoop has
// finished publishing, so the current channel contents are a finite FIFO.
// frExit is a terminal marker and must follow every already-published output
// frame; a select between dead and a ready out channel could otherwise choose
// dead first and discard the visible tail.
func writeHostExitAfterDrain(out <-chan hostOutbound, exitCode int, write func(frameType, []byte) error) error {
	for {
		select {
		case event, ok := <-out:
			if !ok {
				out = nil
				continue
			}
			if err := write(event.typ, event.payload); err != nil {
				return err
			}
		default:
			var code [4]byte
			binary.LittleEndian.PutUint32(code[:], uint32(exitCode))
			return write(frExit, code[:])
		}
	}
}

func encodeHello(h HelloMsg) []byte {
	j, _ := json.Marshal(h)
	out := make([]byte, 2+len(j))
	binary.LittleEndian.PutUint16(out[0:], ProtocolVersion)
	copy(out[2:], j)
	return out
}
