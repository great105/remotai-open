// Package connstat — лёгкий in-process учёт стабильности WebSocket-соединений
// клиента (PTY-терминал, удалённый рабочий стол) на стороне ПК-бэкенда.
//
// Зачем: ПК видит свою сторону каждого клиентского WS (в облаке — через
// relay-бридж, в LAN — напрямую), поэтому именно здесь удобно фиксировать
// «опыт соединения»: когда подключился, когда и ПОЧЕМУ отвалился, сколько длился,
// какой был RTT. Раньше всё это терялось — connect/disconnect писались в текстовый
// лог без причины и длительности, а RTT удалёнки просто выбрасывался после показа.
//
// Данные живут в памяти (кольцевой журнал + карта активных) и отдаются срезом
// через loopback-эндпоинт /api/diag/connections — чтобы можно было постфактум
// увидеть «сколько раз отвалилось за час и почему».
package connstat

import (
	"sync"
	"sync/atomic"
	"time"
)

// Виды наблюдаемых соединений.
const (
	KindPTY    = "pty"
	KindRemote = "remote"
)

// Conn — дескриптор одного активного соединения. Возвращается Open; на Close
// журналируется закрытие с причиной и длительностью. Все методы безопасны на nil
// (Open у выключенного реестра вернёт nil), чтобы вызывающий код не городил проверки.
type Conn struct {
	reg    *Registry
	kind   string
	id     string
	uid    int64
	opened time.Time
	rttMs  atomic.Int64 // последний измеренный RTT, мс (0 = ещё не мерян)
	closed atomic.Bool
}

// RTT записывает свежий замер задержки (для удалёнки — из ACK кадров).
func (c *Conn) RTT(d time.Duration) {
	if c == nil {
		return
	}
	if ms := d.Milliseconds(); ms >= 0 {
		c.rttMs.Store(ms)
	}
}

// Close журналирует закрытие соединения. reason — классифицированная причина
// ("client-close", "timeout", "process-exit", "io-error: …"). Идемпотентно.
func (c *Conn) Close(reason string) {
	if c == nil || !c.closed.CompareAndSwap(false, true) {
		return
	}
	c.reg.closeConn(c, reason)
}

// Event — запись кольцевого журнала.
type Event struct {
	Time       time.Time `json:"time"`
	Kind       string    `json:"kind"`
	ID         string    `json:"id"`
	UID        int64     `json:"uid"`
	Action     string    `json:"action"` // "open" | "close"
	Reason     string    `json:"reason,omitempty"`
	DurationMs int64     `json:"duration_ms,omitempty"`
	RTTMs      int64     `json:"rtt_ms,omitempty"`
	// ReconnectGapMs — на событии "open": сколько мс прошло с прошлого закрытия
	// соединения той же сессии (time-to-reconnect). 0 = первый коннект сессии
	// ИЛИ гэп больше reconnectGapMax (юзер ушёл и вернулся — это не реконнект,
	// см. комментарий у константы).
	ReconnectGapMs int64 `json:"reconnect_gap_ms,omitempty"`
}

// reconnectGapMax — максимальный разрыв между close и open той же сессии,
// который ещё считается «переподключением». Клиент ретраит с backoff максимум
// ~2 минуты (10 попыток, кап 15с) — всё, что дольше, значит юзер закрыл
// страницу/ушёл и вернулся сам; мешать это в avg_reconnect_ms нельзя (после
// починки зомби-циклов 2.15.12 метрика показывала «31 минуту», честно усредняя
// получасовые отлучки юзера с секундными реконнектами).
const reconnectGapMax = 3 * time.Minute

// Registry — кольцевой журнал событий + карта активных соединений.
type Registry struct {
	mu     sync.Mutex
	active map[*Conn]struct{}
	ring   []Event
	head   int // индекс следующей записи
	count  int // сколько записей реально лежит (≤ cap)
	opens  uint64
	closes uint64
	// lastClose — время последнего закрытия по ключу kind|id, чтобы на следующем
	// open той же сессии посчитать time-to-reconnect.
	lastClose map[string]time.Time
}

// Default — общий реестр процесса. ПК-бэкенд — это один web-сервер, поэтому
// глобальный экземпляр избавляет от проводки реестра через все WS-хендлеры.
var Default = New(1000)

// New создаёт реестр с кольцом на capacity последних событий.
func New(capacity int) *Registry {
	if capacity < 16 {
		capacity = 16
	}
	return &Registry{
		active:    make(map[*Conn]struct{}),
		ring:      make([]Event, capacity),
		lastClose: make(map[string]time.Time),
	}
}

func connKey(kind, id string) string { return kind + "|" + id }

// Open регистрирует новое соединение и журналирует событие "open". Если у этой
// же сессии недавно было закрытие — считает time-to-reconnect (ReconnectGapMs).
func (r *Registry) Open(kind, id string, uid int64) *Conn {
	if r == nil {
		return nil
	}
	c := &Conn{reg: r, kind: kind, id: id, uid: uid, opened: time.Now()}
	r.mu.Lock()
	r.active[c] = struct{}{}
	r.opens++
	ev := Event{Time: c.opened, Kind: kind, ID: id, UID: uid, Action: "open"}
	if t, ok := r.lastClose[connKey(kind, id)]; ok {
		if gap := c.opened.Sub(t); gap <= reconnectGapMax {
			ev.ReconnectGapMs = gap.Milliseconds()
		}
		delete(r.lastClose, connKey(kind, id))
	}
	r.append(ev)
	r.mu.Unlock()
	return c
}

func (r *Registry) closeConn(c *Conn, reason string) {
	now := time.Now()
	r.mu.Lock()
	delete(r.active, c)
	r.closes++
	r.lastClose[connKey(c.kind, c.id)] = now
	r.append(Event{
		Time: now, Kind: c.kind, ID: c.id, UID: c.uid, Action: "close",
		Reason: reason, DurationMs: now.Sub(c.opened).Milliseconds(), RTTMs: c.rttMs.Load(),
	})
	r.mu.Unlock()
}

// append кладёт событие в кольцо. Вызывается под r.mu.
func (r *Registry) append(e Event) {
	r.ring[r.head] = e
	r.head = (r.head + 1) % len(r.ring)
	if r.count < len(r.ring) {
		r.count++
	}
}

// ── Снимок для диагностики ──────────────────────────────────────

type ActiveConn struct {
	Kind   string `json:"kind"`
	ID     string `json:"id"`
	UID    int64  `json:"uid"`
	Since  string `json:"since"`   // RFC3339
	AgeSec int64  `json:"age_sec"` // как долго держится
	RTTMs  int64  `json:"rtt_ms,omitempty"`
}

type Snapshot struct {
	Now            string         `json:"now"`
	Active         []ActiveConn   `json:"active"`
	OpensTotal     uint64         `json:"opens_total"`
	ClosesTotal    uint64         `json:"closes_total"`
	Closes5m       int            `json:"closes_5m"`        // разрывов за 5 мин — индикатор нестабильности
	ReasonCounts   map[string]int `json:"reason_counts_5m"` // причины разрывов за 5 мин
	AvgRTTMs       int64          `json:"avg_rtt_ms,omitempty"`
	Reconnects5m   int            `json:"reconnects_5m"`              // переподключений за 5 мин (гэп ≤ reconnectGapMax)
	AvgReconnectMs int64          `json:"avg_reconnect_ms,omitempty"` // среднее time-to-reconnect за 5 мин
	MaxReconnectMs int64          `json:"max_reconnect_ms,omitempty"` // худший time-to-reconnect за 5 мин
	RecentEvents   []Event        `json:"recent_events"`              // от новых к старым
}

// Snapshot собирает текущее состояние для отдачи в /api/diag/connections.
// limit — сколько последних событий вернуть (0 = все имеющиеся).
func (r *Registry) Snapshot(limit int) Snapshot {
	now := time.Now()
	r.mu.Lock()
	defer r.mu.Unlock()

	snap := Snapshot{
		Now:          now.Format(time.RFC3339),
		OpensTotal:   r.opens,
		ClosesTotal:  r.closes,
		ReasonCounts: map[string]int{},
		// Непустые срезы по умолчанию — чтобы JSON отдавал [] вместо null, когда
		// активных соединений / событий нет (иначе клиентские скрипты спотыкаются).
		Active:       []ActiveConn{},
		RecentEvents: []Event{},
	}

	// Активные соединения + средний RTT по ним.
	var rttSum, rttN int64
	for c := range r.active {
		ac := ActiveConn{
			Kind: c.kind, ID: c.id, UID: c.uid,
			Since:  c.opened.Format(time.RFC3339),
			AgeSec: int64(now.Sub(c.opened).Seconds()),
			RTTMs:  c.rttMs.Load(),
		}
		if ac.RTTMs > 0 {
			rttSum += ac.RTTMs
			rttN++
		}
		snap.Active = append(snap.Active, ac)
	}
	if rttN > 0 {
		snap.AvgRTTMs = rttSum / rttN
	}

	// Проход по кольцу от новых к старым: считаем разрывы за 5 мин и собираем
	// последние limit событий.
	cutoff := now.Add(-5 * time.Minute)
	var rcSum, rcN int64
	for i := 0; i < r.count; i++ {
		idx := (r.head - 1 - i + len(r.ring)) % len(r.ring)
		e := r.ring[idx]
		if e.Action == "close" && e.Time.After(cutoff) {
			snap.Closes5m++
			reason := e.Reason
			if reason == "" {
				reason = "unknown"
			}
			snap.ReasonCounts[reason]++
		}
		if e.Action == "open" && e.ReconnectGapMs > 0 && e.Time.After(cutoff) {
			snap.Reconnects5m++
			rcSum += e.ReconnectGapMs
			rcN++
			snap.MaxReconnectMs = max(snap.MaxReconnectMs, e.ReconnectGapMs)
		}
		if limit <= 0 || len(snap.RecentEvents) < limit {
			snap.RecentEvents = append(snap.RecentEvents, e)
		}
	}
	if rcN > 0 {
		snap.AvgReconnectMs = rcSum / rcN
	}
	return snap
}
