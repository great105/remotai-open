// Package relayhub держит in-memory реестр подключённых агентов и роутит
// запросы клиентов к нужному агенту.
//
// Architecture:
//   - AgentConn — обёртка над gorilla/websocket для одного device.
//   - Hub — map[deviceID]*AgentConn, потокобезопасный.
//   - Pending — map[requestID]chan CmdResult для матчинга response → request.
//
// При горизонтальном масштабировании Hub нужно заменить на pub/sub через
// Redis или NATS, либо стики-сессии по device_id.
package relayhub

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"

	"tgcontrol-relay/internal/protocol"
)

var (
	ErrAgentOffline    = errors.New("agent offline")
	ErrRequestTimeout  = errors.New("agent did not respond in time")
	ErrAgentDuplicate  = errors.New("agent already connected from another session")
	ErrAgentDisconnect = errors.New("agent disconnected during request")
	ErrStreamUnknown   = errors.New("stream token unknown or expired")
)

// AgentConn — одно подключение десктоп-агента.
type AgentConn struct {
	DeviceID  string
	UserID    int64
	SessionID string
	conn      *websocket.Conn
	writeMu   sync.Mutex
	closeOnce sync.Once
	done      chan struct{}

	pendingMu sync.Mutex
	pending   map[string]chan protocol.CmdResult

	// fan-out для серверных событий — все клиенты данного user'а слушают канал
	// чтобы получать MsgEvent.
	eventMu   sync.RWMutex
	eventSubs map[chan protocol.Event]struct{}
}

func newAgentConn(deviceID string, userID int64, sessionID string, conn *websocket.Conn) *AgentConn {
	return &AgentConn{
		DeviceID:  deviceID,
		UserID:    userID,
		SessionID: sessionID,
		conn:      conn,
		done:      make(chan struct{}),
		pending:   make(map[string]chan protocol.CmdResult),
		eventSubs: make(map[chan protocol.Event]struct{}),
	}
}

func (a *AgentConn) writeJSON(v any) error {
	a.writeMu.Lock()
	defer a.writeMu.Unlock()
	a.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	return a.conn.WriteJSON(v)
}

// Done — закрывается когда соединение разорвано.
func (a *AgentConn) Done() <-chan struct{} { return a.done }

// Ping sends a keepalive ping through the SAME serialized writer as Send,
// so the periodic ping never races a proxied command write (gorilla/websocket
// forbids concurrent writes — racing corrupts frames and drops the agent).
func (a *AgentConn) Ping() error {
	return a.writeJSON(map[string]string{"type": protocol.MsgPing})
}

// SendStreamOpen asks the agent to bridge a streaming WS, going through the
// serialized writer so it never races a proxied command write.
func (a *AgentConn) SendStreamOpen(so protocol.StreamOpen) error {
	so.Type = protocol.MsgStreamOpen
	return a.writeJSON(so)
}

// SendWelcome отправляет приветствие — тоже через сериализованный writer.
//
// ГОНКА, ради которой этот метод и появился: обработчик подключения писал
// welcome прямо в conn, минуя writeMu, а Hub.Register к тому моменту уже
// положил агента в реестр — значит любой запрос (кнопка ответа из бота,
// команда с телефона) мог в ту же миллисекунду уйти агенту через Send. Две
// параллельные записи в один websocket дают панику gorilla «concurrent write
// to websocket connection»: net/http её перехватывает, но соединение рвётся, и
// для человека это выглядит как «ПК то в сети, то нет» сразу после
// переподключения. Поймано красным CI (TestSendPtyInputErrors), окно узкое —
// от регистрации в хабе до отправки приветствия.
func (a *AgentConn) SendWelcome(w protocol.Welcome) error {
	w.Type = protocol.MsgWelcome
	return a.writeJSON(w)
}

// Send отправляет cmd агенту и ждёт результат до timeout.
func (a *AgentConn) Send(ctx context.Context, cmd protocol.Cmd, timeout time.Duration) (*protocol.CmdResult, error) {
	ch := make(chan protocol.CmdResult, 1)
	a.pendingMu.Lock()
	a.pending[cmd.RequestID] = ch
	a.pendingMu.Unlock()

	defer func() {
		a.pendingMu.Lock()
		delete(a.pending, cmd.RequestID)
		a.pendingMu.Unlock()
	}()

	if err := a.writeJSON(cmd); err != nil {
		return nil, err
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case res := <-ch:
		return &res, nil
	case <-timer.C:
		return nil, ErrRequestTimeout
	case <-a.done:
		return nil, ErrAgentDisconnect
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Deliver вызывается ридер-горутиной при получении MsgCmdResult.
func (a *AgentConn) Deliver(res protocol.CmdResult) {
	a.pendingMu.Lock()
	ch, ok := a.pending[res.RequestID]
	a.pendingMu.Unlock()
	if !ok {
		return
	}
	select {
	case ch <- res:
	default:
	}
}

// SubscribeEvents возвращает канал на который будут поступать MsgEvent от агента.
// Unsubscribe вызывается отписаться.
func (a *AgentConn) SubscribeEvents(buf int) chan protocol.Event {
	ch := make(chan protocol.Event, buf)
	a.eventMu.Lock()
	if a.eventSubs == nil {
		// Соединение уже закрыто: Close() обнуляет eventSubs, и запись в nil-map
		// роняла бы весь релей паникой (гонка «агент отвалился ровно между
		// Hub.Get и подпиской»). Отдаём сразу закрытый канал — подписчик читает
		// его как «агент ушёл» и завершает цикл.
		a.eventMu.Unlock()
		close(ch)
		return ch
	}
	a.eventSubs[ch] = struct{}{}
	a.eventMu.Unlock()
	return ch
}

func (a *AgentConn) UnsubscribeEvents(ch chan protocol.Event) {
	a.eventMu.Lock()
	// Идемпотентно: если Close() уже закрыл каналы (eventSubs=nil) или подписку
	// сняли раньше, повторный close(ch) дал бы панику двойного закрытия.
	if _, ok := a.eventSubs[ch]; ok {
		delete(a.eventSubs, ch)
		close(ch)
	}
	a.eventMu.Unlock()
}

func (a *AgentConn) dispatchEvent(e protocol.Event) {
	a.eventMu.RLock()
	defer a.eventMu.RUnlock()
	for ch := range a.eventSubs {
		select {
		case ch <- e:
		default:
			// slow subscriber — пропускаем, чтобы не блокировать reader
		}
	}
}

// Close закрывает соединение и уведомляет всех ожидающих.
func (a *AgentConn) Close() {
	a.closeOnce.Do(func() {
		close(a.done)
		a.conn.Close()
		a.eventMu.Lock()
		for ch := range a.eventSubs {
			close(ch)
		}
		a.eventSubs = nil
		a.eventMu.Unlock()
	})
}

// ----- Hub -----

type Hub struct {
	mu     sync.RWMutex
	agents map[string]*AgentConn

	streamMu sync.Mutex
	streams  map[string]*PendingStream // keyed by one-time token

	// liveMu защищает live — реестр УЖЕ сшитых стрим-мостов (телефон↔ПК),
	// сгруппированных по deviceID. Нужен, чтобы отзыв устройства/гранта рубил
	// не только новые подключения и control-канал, но и активные стримы:
	// стрим-плечо агента — ОТДЕЛЬНЫЙ сокет от control-WS, поэтому ac.Close()
	// его не трогает, и открытый терминал отозванного клиента иначе тёк бы до
	// естественного обрыва.
	liveMu sync.Mutex
	live   map[string]map[*LiveBridge]struct{}

	// presMu защищает реестр присутствия клиента (см. presence.go): present —
	// счётчик живых клиентских каналов пары (device,user), lastGone — когда ушёл
	// последний. Нужен notifier'у, чтобы не слать в Telegram то, что человек и
	// так видит на экране приложения.
	presMu   sync.Mutex
	present  map[presenceKey]int
	lastGone map[presenceKey]time.Time
}

func NewHub() *Hub {
	return &Hub{
		agents:   make(map[string]*AgentConn),
		streams:  make(map[string]*PendingStream),
		live:     make(map[string]map[*LiveBridge]struct{}),
		present:  make(map[presenceKey]int),
		lastGone: make(map[presenceKey]time.Time),
	}
}

// LiveBridge — дескриптор одного сшитого стрим-моста. closer закрывает ОБА
// сокета (клиентский и агентский), разрывая pipeStreams.
type LiveBridge struct {
	DeviceID string
	UserID   int64
	closer   func()
}

// AddLiveBridge регистрирует активный мост и возвращает его дескриптор. Вызывать
// сразу после сшивки; RemoveLiveBridge — по завершении pipeStreams.
func (h *Hub) AddLiveBridge(deviceID string, userID int64, closer func()) *LiveBridge {
	lb := &LiveBridge{DeviceID: deviceID, UserID: userID, closer: closer}
	h.liveMu.Lock()
	set := h.live[deviceID]
	if set == nil {
		set = make(map[*LiveBridge]struct{})
		h.live[deviceID] = set
	}
	set[lb] = struct{}{}
	h.liveMu.Unlock()
	return lb
}

// RemoveLiveBridge снимает мост из реестра (idempotent).
func (h *Hub) RemoveLiveBridge(lb *LiveBridge) {
	if lb == nil {
		return
	}
	h.liveMu.Lock()
	if set := h.live[lb.DeviceID]; set != nil {
		delete(set, lb)
		if len(set) == 0 {
			delete(h.live, lb.DeviceID)
		}
	}
	h.liveMu.Unlock()
}

// CloseLiveBridges рвёт все активные стримы устройства. match==nil → все
// (отзыв всего устройства); иначе — только мосты этого пользователя (отзыв
// одного гранта), чтобы не ронять чужие живые сессии к тому же ПК. Возвращает
// число закрытых мостов.
func (h *Hub) CloseLiveBridges(deviceID string, match *int64) int {
	h.liveMu.Lock()
	set := h.live[deviceID]
	var victims []*LiveBridge
	for lb := range set {
		if match == nil || lb.UserID == *match {
			victims = append(victims, lb)
			delete(set, lb)
		}
	}
	if len(set) == 0 {
		delete(h.live, deviceID)
	}
	h.liveMu.Unlock()
	for _, lb := range victims {
		lb.closer()
	}
	return len(victims)
}

// PendingStream tracks a streaming bridge between the moment the relay tells the
// agent to open it and the moment the agent's outbound stream WS arrives.
//
//   - The CLIENT handler creates it, sends StreamOpen to the agent, then waits
//     on AgentCh for the agent's connection.
//   - The AGENT stream handler claims it by token, verifies ownership, and
//     delivers its connection on AgentCh, then waits on Taken (closed by the
//     client once it has taken ownership) so it can clean up if the client gave
//     up first.
type PendingStream struct {
	ID       string
	Token    string
	DeviceID string
	UserID   int64
	Path     string
	AgentCh  chan *websocket.Conn // buffered(1): agent delivers its conn here
	Taken    chan struct{}        // closed by client once it owns the agent conn
}

// RegisterStream creates a pending stream with a fresh id + token and stores it.
func (h *Hub) RegisterStream(deviceID string, userID int64, path string) *PendingStream {
	ps := &PendingStream{
		ID:       uuid.NewString(),
		Token:    randomToken(),
		DeviceID: deviceID,
		UserID:   userID,
		Path:     path,
		AgentCh:  make(chan *websocket.Conn, 1),
		Taken:    make(chan struct{}),
	}
	h.streamMu.Lock()
	h.streams[ps.Token] = ps
	h.streamMu.Unlock()
	return ps
}

// ClaimStream pops a pending stream by token and verifies it belongs to the
// given device. Single-use: a second claim returns ErrStreamUnknown.
//
// The user is NOT re-checked here on purpose: the client side was already
// authorized (deviceForUser) when the stream was registered, and the agent
// proves device identity via its device JWT + possession of the one-time
// token. The JWT's user_id claim can be stale after a re-pair (the agent
// only refreshes its JWT on reconnect), and rejecting on that mismatch used
// to break Remote Desktop / PTY until the desktop app restarted.
func (h *Hub) ClaimStream(token, deviceID string, userID int64) (*PendingStream, error) {
	h.streamMu.Lock()
	ps, ok := h.streams[token]
	if ok {
		delete(h.streams, token)
	}
	h.streamMu.Unlock()
	if !ok {
		return nil, ErrStreamUnknown
	}
	if ps.DeviceID != deviceID {
		return nil, ErrStreamUnknown
	}
	if ps.UserID != userID {
		log.Printf("[HUB] stream %s claimed with stale device JWT (jwt user=%d, stream user=%d) — allowing, agent should re-pair/reconnect", ps.ID, userID, ps.UserID)
	}
	return ps, nil
}

// RemoveStream drops a pending stream (client gave up before the agent dialled).
func (h *Hub) RemoveStream(token string) {
	h.streamMu.Lock()
	delete(h.streams, token)
	h.streamMu.Unlock()
}

func randomToken() string {
	var b [24]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// Register регистрирует агента. Если уже есть соединение с этим device_id —
// старое закрывается (LRU-замена), это позволяет переподключаться без ожидания.
func (h *Hub) Register(deviceID string, userID int64, sessionID string, conn *websocket.Conn) *AgentConn {
	h.mu.Lock()
	defer h.mu.Unlock()
	if old, ok := h.agents[deviceID]; ok {
		go old.Close()
	}
	ac := newAgentConn(deviceID, userID, sessionID, conn)
	h.agents[deviceID] = ac
	return ac
}

// Unregister удаляет агента из реестра, если это всё ещё та же сессия.
func (h *Hub) Unregister(deviceID, sessionID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if ac, ok := h.agents[deviceID]; ok && ac.SessionID == sessionID {
		delete(h.agents, deviceID)
	}
}

// Get возвращает агента для device_id или nil если offline.
func (h *Hub) Get(deviceID string) *AgentConn {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.agents[deviceID]
}

// IsOnline — быстрый чек для UI.
func (h *Hub) IsOnline(deviceID string) bool {
	return h.Get(deviceID) != nil
}

// OnlineDevices возвращает device_id всех онлайн-агентов (для health endpoint).
func (h *Hub) OnlineDevices() []string {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make([]string, 0, len(h.agents))
	for id := range h.agents {
		out = append(out, id)
	}
	return out
}

// DispatchRaw вызывается ридером WS-агента: разбирает входящий JSON и роутит
// в pending request или event subscribers.
func (a *AgentConn) DispatchRaw(raw json.RawMessage) {
	var head struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(raw, &head); err != nil {
		return
	}
	switch head.Type {
	case protocol.MsgCmdResult:
		var res protocol.CmdResult
		if err := json.Unmarshal(raw, &res); err == nil {
			a.Deliver(res)
		}
	case protocol.MsgEvent:
		var ev protocol.Event
		if err := json.Unmarshal(raw, &ev); err == nil {
			a.dispatchEvent(ev)
		}
	case protocol.MsgPong:
		// noop — ридер обновит deadline отдельно
	}
}
