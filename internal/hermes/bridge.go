package hermes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

// Event frames retain the upstream protocol, including server requests. There
// is no browser-owned gateway: polling/reopening the UI leaves this socket alive.
type Event struct {
	Seq   uint64          `json:"seq"`
	Frame json.RawMessage `json:"frame"`
}
type EventBatch struct {
	Events    []Event `json:"events"`
	LatestSeq uint64  `json:"latest_seq"`
	Reset     bool    `json:"reset"`
}
type RPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *RPCError) Error() string { return e.Message }

type rpcFrame struct {
	JSONRPC string          `json:"jsonrpc,omitempty"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

type rpcBridge struct {
	manager *Manager
	mu      sync.Mutex
	writeMu sync.Mutex
	conn    *websocket.Conn
	ready   chan struct{}
	done    chan struct{}
	cancel  context.CancelFunc
	pending map[string]chan rpcFrame
	methods map[string]string
	ids     atomic.Uint64
	closed  bool
}

func (m *Manager) Events(after uint64) EventBatch {
	m.mu.Lock()
	defer m.mu.Unlock()
	b := EventBatch{Events: []Event{}, LatestSeq: m.seq, Reset: after > m.seq || (m.epoch > 1 && after <= m.epochStart) || (len(m.events) == 0 && after < m.seq)}
	if len(m.events) > 0 && after+1 < m.events[0].Seq {
		b.Reset = true
	}
	for _, e := range m.events {
		if e.Seq > after {
			b.Events = append(b.Events, Event{Seq: e.Seq, Frame: append(json.RawMessage(nil), e.Frame...)})
		}
	}
	return b
}

func (m *Manager) addEvent(raw []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.seq++
	m.events = append(m.events, Event{Seq: m.seq, Frame: append(json.RawMessage(nil), raw...)})
	// Bound both count and bytes: token-heavy turns must not retain an unlimited
	// transcript in Go; Hermes owns the durable history for reset/reconnect.
	size := 0
	start := len(m.events)
	for start > 0 {
		n := len(m.events[start-1].Frame)
		if len(m.events)-start >= 512 || size+n > 8*1024*1024 {
			break
		}
		size += n
		start--
	}
	if start > 0 {
		m.events = append([]Event(nil), m.events[start:]...)
	}
}

func (m *Manager) ensureBridge(ctx context.Context) (*rpcBridge, error) {
	m.mu.Lock()
	if !m.ready || m.baseURL == "" {
		m.mu.Unlock()
		return nil, ErrNotReady
	}
	if m.state.Operation == "updating" || m.state.Operation == "installing" {
		m.mu.Unlock()
		return nil, ErrBusy
	}
	b := m.bridge
	if b == nil {
		life, cancel := context.WithCancel(context.Background())
		b = &rpcBridge{manager: m, ready: make(chan struct{}), done: make(chan struct{}), cancel: cancel, pending: map[string]chan rpcFrame{}, methods: map[string]string{}}
		m.bridge = b
		base, token := m.baseURL, m.token
		go b.connect(life, base, token)
	}
	m.mu.Unlock()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-b.done:
		return nil, ErrNotReady
	case <-b.ready:
		return b, nil
	}
}

func (b *rpcBridge) connect(ctx context.Context, base, token string) {
	defer close(b.done)
	u, err := url.Parse(base)
	if err != nil {
		return
	}
	u.Scheme = "ws"
	u.Path = "/api/ws"
	q := url.Values{}
	q.Set("token", token)
	u.RawQuery = q.Encode()
	dialer := websocket.Dialer{HandshakeTimeout: 10 * time.Second, Proxy: nil}
	header := http.Header{}
	header.Set("X-Hermes-Session-Token", token)
	conn, _, err := dialer.DialContext(ctx, u.String(), header)
	if err != nil {
		b.manager.mu.Lock()
		if b.manager.bridge == b {
			b.manager.bridge = nil
			b.manager.state.LastError = "Не удалось подключить чат Hermes; повторите действие"
		}
		b.manager.mu.Unlock()
		return
	}
	conn.SetReadLimit(32 * 1024 * 1024)
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		conn.Close()
		return
	}
	b.conn = conn
	b.mu.Unlock()
	defer func() {
		conn.Close()
		b.failPending()
		b.manager.mu.Lock()
		reconnect := false
		if b.manager.bridge == b {
			b.manager.bridge = nil
			reconnect = b.manager.ready && !b.manager.closing
			if reconnect {
				// The fresh gateway needs a durable session activation even if
				// the serve process itself stayed healthy during a socket drop.
				b.manager.epoch++
				b.manager.epochStart = b.manager.seq
				b.manager.events = nil
			}
		}
		b.manager.mu.Unlock()
		if reconnect {
			go b.manager.reconnectBridge()
		}
	}()
	go func() {
		select {
		case <-ctx.Done():
			conn.Close()
		case <-b.done:
		}
	}()
	capabilitiesSent := false
	for {
		_, raw, readErr := conn.ReadMessage()
		if readErr != nil {
			return
		}
		var frame rpcFrame
		if json.Unmarshal(raw, &frame) != nil {
			continue
		}
		if frame.Method != "" {
			if len(frame.ID) > 0 && rendererOnlyRequest(frame.Method) {
				value, _ := json.Marshal(map[string]string{"error": "This Remotai surface has no Hermes Desktop renderer; use available host tools."})
				if b.write(ctx, map[string]any{"jsonrpc": "2.0", "id": frame.ID, "result": map[string]string{"value": string(value)}}) != nil {
					return
				}
				continue
			}
			b.manager.addEvent(raw)
			var event struct {
				Type string `json:"type"`
			}
			_ = json.Unmarshal(frame.Params, &event)
			if !capabilitiesSent && (frame.Method == "gateway.ready" || event.Type == "gateway.ready") {
				capabilitiesSent = true
				if b.write(ctx, map[string]any{"jsonrpc": "2.0", "id": "remotai-capabilities", "method": "client.capabilities", "params": map[string]bool{"server_requests": true}}) != nil {
					return
				}
				close(b.ready)
			}
			continue
		}
		key := rpcID(frame.ID)
		if key == "remotai-capabilities" {
			continue
		}
		b.mu.Lock()
		ch := b.pending[key]
		method := b.methods[key]
		delete(b.pending, key)
		delete(b.methods, key)
		b.mu.Unlock()
		if ch != nil {
			// The reader has consumed all events preceding this response. Bind
			// history hydration to that exact replay cursor before delivering it.
			if method == "session.create" || method == "session.resume" || method == "session.activate" {
				var obj map[string]json.RawMessage
				if frame.Error == nil && json.Unmarshal(frame.Result, &obj) == nil && obj != nil {
					b.manager.mu.Lock()
					seq := b.manager.seq
					b.manager.mu.Unlock()
					cursor, _ := json.Marshal(seq)
					obj["_remotai_event_seq"] = cursor
					frame.Result, _ = json.Marshal(obj)
				}
			}
			ch <- frame
		}
	}
}

func rendererOnlyRequest(method string) bool {
	switch method {
	case "terminal.read", "preview.read", "window.read", "preview.act", "tour":
		return true
	}
	return false
}

func (m *Manager) reconnectBridge() {
	for {
		timer := time.NewTimer(2 * time.Second)
		<-timer.C
		m.mu.Lock()
		ready := m.ready && !m.closing
		m.mu.Unlock()
		if !ready {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		_, err := m.ensureBridge(ctx)
		cancel()
		if err == nil {
			return
		}
	}
}

func rpcID(raw json.RawMessage) string {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	return string(raw)
}

func (b *rpcBridge) write(ctx context.Context, obj any) error {
	b.writeMu.Lock()
	defer b.writeMu.Unlock()
	b.mu.Lock()
	conn := b.conn
	closed := b.closed
	b.mu.Unlock()
	if conn == nil || closed {
		return ErrNotReady
	}
	deadline := time.Now().Add(10 * time.Second)
	if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
		deadline = dl
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	_ = conn.SetWriteDeadline(deadline)
	return conn.WriteJSON(obj)
}

func (b *rpcBridge) failPending() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closed = true
	for key, ch := range b.pending {
		ch <- rpcFrame{Error: &RPCError{Code: -32001, Message: "Соединение с Hermes прервалось; снова откройте беседу"}}
		delete(b.pending, key)
		delete(b.methods, key)
	}
}

func (b *rpcBridge) close() {
	b.cancel()
	b.mu.Lock()
	b.closed = true
	conn := b.conn
	b.mu.Unlock()
	if conn != nil {
		conn.Close()
	}
}

func (m *Manager) RPC(ctx context.Context, method string, params any) (json.RawMessage, error) {
	if strings.TrimSpace(method) == "" {
		return nil, errors.New("не задан метод Hermes")
	}
	b, err := m.ensureBridge(ctx)
	if err != nil {
		return nil, err
	}
	id := fmt.Sprintf("remotai-%d", b.ids.Add(1))
	ch := make(chan rpcFrame, 1)
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return nil, ErrNotReady
	}
	b.pending[id] = ch
	b.methods[id] = method
	b.mu.Unlock()
	defer func() { b.mu.Lock(); delete(b.pending, id); delete(b.methods, id); b.mu.Unlock() }()
	if err := b.write(ctx, map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
		return nil, err
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-b.done:
		return nil, ErrNotReady
	case result := <-ch:
		if result.Error != nil {
			return nil, result.Error
		}
		return result.Result, nil
	}
}

func (m *Manager) Reply(ctx context.Context, id string, result json.RawMessage) error {
	if id == "" || !json.Valid(result) {
		return errors.New("некорректный ответ Hermes")
	}
	b, err := m.ensureBridge(ctx)
	if err != nil {
		return err
	}
	return b.write(ctx, map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}
