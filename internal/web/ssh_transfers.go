package web

// Переносы файлов ПК ↔ SSH-сервер (#9).
//
// Смысл: байты идут ВНУТРИ ПК (агент ↔ сервер), телефон только запускает
// перенос и смотрит прогресс. Сегодня дамп на 200 МБ обязан прогуляться через
// телефон (сервер → релей → телефон → релей → сервер), хотя ПК и сервер могут
// стоять в одном ЦОДе.
//
// Почему асинхронно: облачный запрос через релей живёт максимум 60 с
// (tgcontrol-relay/internal/server/proxy.go), а клиентский AbortController —
// 30 с. Поэтому push/pull отвечают сразу 202 + id, а копирование идёт горутиной
// на ПК; приложение можно закрыть.
//
// Переносы живут ТОЛЬКО в памяти агента (как PTY-сессии): рестарт/автообновление
// агента = перенос умер. Это осознанно — персистентность потребовала бы
// докачки, а её нет. Клиент обязан трактовать исчезновение running-задания из
// GET /api/ssh/sftp/transfers как «прервано», а не как «готово».
//
// Прогресс уходит событием WS (Broadcast → и в облако через relayEventSink),
// НЕ ЧАЩЕ раза в секунду: без троттлинга событие ушло бы на каждый Read
// (сотни в секунду) — прямой рецидив грабли useLoadState.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"log"
	"sync"
	"sync/atomic"
	"time"
)

// newTransferID — короткий случайный id задания (в URL/JSON, не секрет).
func newTransferID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return hex.EncodeToString([]byte(time.Now().Format(time.RFC3339Nano)))
	}
	return hex.EncodeToString(b)
}

const (
	// maxConcurrentSSHTransfers — больше трёх параллельных переносов на ПК
	// смысла не имеют: они делят один канал и только мешают друг другу.
	maxConcurrentSSHTransfers = 3
	// sshTransferKeepFinished — сколько держим завершённое задание, чтобы
	// клиент, вернувшийся из фона, узнал исход, а не «оно исчезло».
	sshTransferKeepFinished = 10 * time.Minute
	// sshTransferTick — минимальный интервал между событиями прогресса.
	sshTransferTick = time.Second
)

type sshTransferState string

const (
	sshTransferRunning  sshTransferState = "running"
	sshTransferDone     sshTransferState = "done"
	sshTransferError    sshTransferState = "error"
	sshTransferCanceled sshTransferState = "canceled"
)

// errTooManySSHTransfers — превышен лимит одновременных переносов.
var errTooManySSHTransfers = errors.New("too many active transfers")

// sshTransfer — одно задание переноса.
type sshTransfer struct {
	id     string
	dir    string // "push" (ПК→сервер) | "pull" (сервер→ПК)
	name   string
	target string // user@host:port — подпись в UI
	remote string // полный путь на сервере
	local  string // полный путь на ПК
	total  int64
	uid    int64

	done   atomic.Int64
	ctx    context.Context
	cancel context.CancelFunc

	mu         sync.Mutex
	state      sshTransferState
	code       string // машинный код ошибки
	errMsg     string
	canceling  bool
	closers    []io.Closer
	lastTick   time.Time
	startedAt  time.Time
	finishedAt time.Time
}

// addClosers запоминает дескрипторы, которые надо закрыть при отмене: без
// этого горутина зависает в сетевом чтении до TCP-таймаута, а «Отмена» уже
// вернула ok:true.
func (j *sshTransfer) addClosers(cs ...io.Closer) {
	j.mu.Lock()
	j.closers = append(j.closers, cs...)
	j.mu.Unlock()
}

// snapshot — согласованный слепок для JSON/события.
func (j *sshTransfer) snapshot() map[string]any {
	j.mu.Lock()
	defer j.mu.Unlock()
	m := map[string]any{
		"id":          j.id,
		"dir":         j.dir,
		"name":        j.name,
		"target":      j.target,
		"state":       string(j.state),
		"done":        j.done.Load(),
		"total":       j.total,
		"remote_path": j.remote,
		"local_path":  j.local,
		"started_at":  j.startedAt.UTC().Format(time.RFC3339),
	}
	if j.code != "" {
		m["code"] = j.code
	}
	if j.errMsg != "" {
		m["error"] = j.errMsg
	}
	return m
}

// sshTransferManager — реестр переносов агента.
type sshTransferManager struct {
	mu   sync.Mutex
	jobs map[string]*sshTransfer
	srv  *Server
}

func newSSHTransferManager(s *Server) *sshTransferManager {
	return &sshTransferManager{jobs: make(map[string]*sshTransfer), srv: s}
}

// start регистрирует задание. Возвращает errTooManySSHTransfers, если активных
// уже maxConcurrentSSHTransfers.
func (m *sshTransferManager) start(uid int64, dir, name, target, remote, local string, total int64) (*sshTransfer, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.gcLocked()

	active := 0
	for _, j := range m.jobs {
		j.mu.Lock()
		if j.state == sshTransferRunning {
			active++
		}
		j.mu.Unlock()
	}
	if active >= maxConcurrentSSHTransfers {
		return nil, errTooManySSHTransfers
	}

	ctx, cancel := context.WithCancel(context.Background())
	j := &sshTransfer{
		id:        newTransferID(),
		dir:       dir,
		name:      name,
		target:    target,
		remote:    remote,
		local:     local,
		total:     total,
		uid:       uid,
		ctx:       ctx,
		cancel:    cancel,
		state:     sshTransferRunning,
		startedAt: time.Now(),
	}
	m.jobs[j.id] = j
	return j, nil
}

// run запускает копирование в фоне и по завершении фиксирует исход.
// cleanup(failed) вызывается ВСЕГДА и отвечает за недописанный файл.
func (m *sshTransferManager) run(j *sshTransfer, fn func(ctx context.Context) error, cleanup func(failed bool)) {
	go func() {
		defer j.cancel()
		err := fn(j.ctx)
		if cleanup != nil {
			cleanup(err != nil)
		}
		m.finish(j, err)
	}()
	m.tick(j, true) // «running» с нулевым прогрессом сразу, до первых байт
}

// finish переводит задание в финальное состояние и шлёт событие.
func (m *sshTransferManager) finish(j *sshTransfer, err error) {
	j.mu.Lock()
	canceling := j.canceling
	switch {
	case err == nil:
		j.state = sshTransferDone
	case canceling || errors.Is(err, context.Canceled):
		j.state = sshTransferCanceled
		j.code = "canceled"
	default:
		j.state = sshTransferError
		code, _ := transferErrCode(err)
		j.code = code
		j.errMsg = err.Error()
	}
	j.finishedAt = time.Now()
	name, dir, state := j.name, j.dir, j.state
	j.mu.Unlock()

	if state == sshTransferError {
		log.Printf("[SFTP-TRANSFER] %s %q failed: %v", dir, name, err)
	}
	m.tick(j, true)
}

// tick шлёт событие прогресса. force=true — всегда (смена состояния),
// иначе не чаще раза в секунду.
func (m *sshTransferManager) tick(j *sshTransfer, force bool) {
	j.mu.Lock()
	if !force && time.Since(j.lastTick) < sshTransferTick {
		j.mu.Unlock()
		return
	}
	j.lastTick = time.Now()
	j.mu.Unlock()

	if m.srv == nil {
		return
	}
	ev := j.snapshot()
	ev["type"] = "transfer"
	m.srv.Broadcast(j.uid, ev)
}

// cancel останавливает задание: сначала контекст (его видят обёртки прогресса),
// затем закрытие дескрипторов — иначе горутина висит в сетевом чтении.
// Идемпотентна: повторная отмена финального задания — тоже true.
func (m *sshTransferManager) cancel(uid int64, id string) bool {
	m.mu.Lock()
	j := m.jobs[id]
	m.mu.Unlock()
	if j == nil || j.uid != uid {
		return false
	}
	j.mu.Lock()
	if j.state != sshTransferRunning {
		j.mu.Unlock()
		return true
	}
	j.canceling = true
	closers := append([]io.Closer(nil), j.closers...)
	j.mu.Unlock()

	j.cancel()
	for _, c := range closers {
		_ = c.Close()
	}
	return true
}

// list — все известные задания пользователя (running + недавние финальные).
func (m *sshTransferManager) list(uid int64) []map[string]any {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.gcLocked()
	out := make([]map[string]any, 0, len(m.jobs))
	for _, j := range m.jobs {
		if j.uid != uid {
			continue
		}
		out = append(out, j.snapshot())
	}
	return out
}

// gcLocked убирает финальные задания старше sshTransferKeepFinished.
func (m *sshTransferManager) gcLocked() {
	cutoff := time.Now().Add(-sshTransferKeepFinished)
	for id, j := range m.jobs {
		j.mu.Lock()
		stale := j.state != sshTransferRunning && !j.finishedAt.IsZero() && j.finishedAt.Before(cutoff)
		j.mu.Unlock()
		if stale {
			delete(m.jobs, id)
		}
	}
}

// ── Счётчики байтов ────────────────────────────────────────────────
//
// ГРАБЛЯ ПРОИЗВОДИТЕЛЬНОСТИ: у *sftp.File реализованы и WriterTo, и ReaderFrom,
// внутри — конкурентные окна запросов. io.Copy пробует сначала src.(WriterTo),
// потом dst.(ReaderFrom). Поэтому счётчик вешаем на ЛОКАЛЬНЫЙ файл:
//   push (ПК→сервер): обёртка на ИСТОЧНИКЕ → срабатывает (*sftp.File).ReadFrom;
//   pull (сервер→ПК): обёртка на ПРИЁМНИКЕ → срабатывает (*sftp.File).WriteTo.
// Обернёшь не ту сторону — поедет по одному пакету за RTT.

// transferProgressReader — источник для push.
type transferProgressReader struct {
	r    io.Reader
	size int64 // остаток; ReadFrom по нему выбирает конкурентность (см. Size)
	ctx  context.Context
	j    *sshTransfer
	m    *sshTransferManager
}

func (p *transferProgressReader) Read(b []byte) (int, error) {
	if err := p.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := p.r.Read(b)
	if n > 0 {
		p.j.done.Add(int64(n))
		p.m.tick(p.j, false)
	}
	return n, err
}

// Size сообщает (*sftp.File).ReadFrom размер данных. БЕЗ ЭТОГО метода remain
// остаётся нулём и конкурентная запись не включается — файл поедет по одному
// 32-килобайтному пакету за круговую задержку.
func (p *transferProgressReader) Size() int64 { return p.size }

// transferProgressWriter — приёмник для pull.
type transferProgressWriter struct {
	w   io.Writer
	ctx context.Context
	j   *sshTransfer
	m   *sshTransferManager
}

func (p *transferProgressWriter) Write(b []byte) (int, error) {
	if err := p.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := p.w.Write(b)
	if n > 0 {
		p.j.done.Add(int64(n))
		p.m.tick(p.j, false)
	}
	return n, err
}
