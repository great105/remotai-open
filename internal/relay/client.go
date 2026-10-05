package relay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"tgcontrol/internal/config"
	"tgcontrol/internal/dnsfallback"
	"tgcontrol/internal/version"
)

// Client owns the outbound WebSocket connection to the TGControl Cloud relay.
//
// USAGE STATUS — this is a SKELETON for P2. ServeForever currently logs
// "relay not yet implemented" and returns; the relay server itself is not
// shipped. When the server is live the loop below should:
//  1. dial relayURL with the device JWT,
//  2. send Hello / receive Welcome,
//  3. read Cmd messages and dispatch them through the local httpHandler,
//  4. send CmdResult / Event messages back,
//  5. reconnect with exponential backoff on disconnect.
type Client struct {
	relayURL    string
	deviceID    string
	jwt         string
	httpHandler http.Handler // local handler whose responses get tunneled back

	mu        sync.Mutex
	conn      *websocket.Conn
	kick      chan struct{} // wakes ServeForever: config/JWT changed, reconnect now
	lastError ConnectionDetail
}

// ConnectionDetail is a compact, secret-free reason why the relay is offline.
type ConnectionDetail struct {
	Kind       string
	HTTPStatus int
	At         int64
	Message    string
}

type dialHTTPError struct {
	Status int
	Err    error
}

func (e *dialHTTPError) Error() string {
	return fmt.Sprintf("relay handshake failed (HTTP %d): %v", e.Status, e.Err)
}

func (e *dialHTTPError) Unwrap() error { return e.Err }

// New returns a Client wired to the given local handler. relayURL/jwt are
// re-read from config on every (re)connect, so pairing or re-pairing while
// the app is running picks up fresh credentials without a restart.
func New(httpHandler http.Handler) *Client {
	return &Client{httpHandler: httpHandler, kick: make(chan struct{}, 1)}
}

// Status reports the relay link state for the health dashboard.
// configured — relay base/device/JWT настроены (пайринг был);
// connected — control-WS к релею жив прямо сейчас.
func (c *Client) Status() (configured, connected bool) {
	configured = Available()
	c.mu.Lock()
	connected = c.conn != nil
	c.mu.Unlock()
	return configured, connected
}

// Detail returns the last classified connection failure. It intentionally
// contains neither the device JWT nor the full relay URL.
func (c *Client) Detail() ConnectionDetail {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastError
}

func (c *Client) setConnectionError(err error) {
	if err == nil {
		c.mu.Lock()
		c.lastError = ConnectionDetail{}
		c.mu.Unlock()
		return
	}
	d := ConnectionDetail{Kind: "network", At: time.Now().UnixMilli(), Message: err.Error()}
	if len(d.Message) > 240 {
		d.Message = d.Message[:240]
	}
	var httpErr *dialHTTPError
	if errors.As(err, &httpErr) {
		d.HTTPStatus = httpErr.Status
		switch {
		case httpErr.Status == http.StatusUnauthorized || httpErr.Status == http.StatusForbidden:
			d.Kind = "revoked"
		case httpErr.Status >= 500:
			d.Kind = "service"
		default:
			d.Kind = "http"
		}
	} else {
		msg := strings.ToLower(err.Error())
		switch {
		case strings.Contains(msg, "timeout") || strings.Contains(msg, "deadline exceeded"):
			d.Kind = "timeout"
		case strings.Contains(msg, "tls") || strings.Contains(msg, "certificate"):
			d.Kind = "tls"
		case strings.Contains(msg, "no such host") || strings.Contains(msg, "server misbehaving"):
			d.Kind = "dns"
		case strings.Contains(msg, "proxy"):
			d.Kind = "proxy"
		}
	}
	c.mu.Lock()
	c.lastError = d
	c.mu.Unlock()
}

// Kick asks the client to drop the current relay connection (if any) and
// reconnect immediately with freshly-loaded config/JWT. Called after pairing
// completes so the new device JWT takes effect right away. Safe from any
// goroutine; no-op if a kick is already pending.
func (c *Client) Kick() {
	c.mu.Lock()
	conn := c.conn
	c.mu.Unlock()
	if conn != nil {
		_ = conn.Close() // unblocks the read loop in runOnce
	}
	select {
	case c.kick <- struct{}{}:
	default:
	}
}

// Available reports whether the runtime build has a working relay backend.
// True если в config есть base URL + есть JWT в keystore или в config.RelayJWT.
func Available() bool {
	cfg := config.GetNoSetup()
	if cfg.RelayHTTPBase() == "" || cfg.DeviceID == "" {
		return false
	}
	if cfg.RelayJWT != "" {
		return true
	}
	tok, _ := LoadJWT()
	return tok != ""
}

// ServeForever runs the reconnect loop until ctx is cancelled.
//
// Safe to call when the relay isn't configured yet: it idles until pairing
// completes (Kick() or the periodic recheck picks the config up) instead of
// returning, so the relay link works without an app restart no matter when
// the user pairs. Config and JWT are re-read before every dial, so a re-pair
// that issues a fresh device JWT takes effect on the next (re)connect.
func (c *Client) ServeForever(ctx context.Context) {
	loggedWaiting := false
	// ceil — экспоненциально растущий ПОТОЛОК backoff; реальная задержка —
	// random(0, ceil) (см. jitteredBackoff). Без джиттера все агенты,
	// отвалившиеся в один момент (рестарт релея / деплой), реконнектятся
	// синхронно — thundering herd: на 4 ПК это ничто, на тысячах — TLS-лавина
	// в 1 vCPU релея. Случайная задержка размазывает залп во времени.
	ceil := ReconnectMin
	for {
		if ctx.Err() != nil {
			return
		}
		if !c.loadConfig() {
			if !loggedWaiting {
				log.Printf("[RELAY] not configured (relay_url/device_id/jwt missing) — standing by until pairing completes.")
				loggedWaiting = true
			}
			select {
			case <-ctx.Done():
				return
			case <-c.kick:
			case <-time.After(30 * time.Second):
			}
			continue
		}
		loggedWaiting = false

		started := time.Now()
		err := c.runOnce(ctx)
		c.setConnectionError(err)
		// Сессия, прожившая дольше минуты, — доказательство, что связь в порядке:
		// потолок задержки надо начинать заново. Иначе после любого шторма
		// (сутки без сети) потолок навсегда оставался 60 секунд, и следующий
		// одиночный блип — переключение Wi-Fi, сон роутера — стоил человеку до
		// минуты состояния «компьютер не в сети» при уже вернувшейся сети.
		// Сбрасываем не в самый минимум, а на ступень выше: при ceil ==
		// ReconnectMin джиттер вырождается в постоянную секунду (jitteredBackoff),
		// и парк агентов, у которых сессии стабильно рвутся чуть дольше минуты
		// (рестарт-петля релея, NAT-таймаут роутера), ломился бы синхронно —
		// ровно тот залп, ради которого джиттер и писался.
		if time.Since(started) > time.Minute && ceil > 2*ReconnectMin {
			ceil = 2 * ReconnectMin
		}
		wait := jitteredBackoff(ceil)
		if err != nil && ctx.Err() == nil {
			log.Printf("[RELAY] session ended: %v — reconnecting in %s (ceil %s)", err, wait.Round(time.Millisecond), ceil)
		}
		select {
		case <-ctx.Done():
			return
		case <-c.kick:
			ceil = ReconnectMin // explicit kick → fresh credentials, retry now
			continue
		case <-time.After(wait):
		}
		if ceil < ReconnectMax {
			ceil *= 2
			if ceil > ReconnectMax {
				ceil = ReconnectMax
			}
		}
	}
}

// jitteredBackoff реализует Full Jitter (AWS «Exponential Backoff And Jitter»):
// возвращает случайную задержку в [ReconnectMin, ceil]. Нижняя граница
// ReconnectMin held, чтобы не долбить релей нулевыми паузами при мелком ceil.
func jitteredBackoff(ceil time.Duration) time.Duration {
	if ceil <= ReconnectMin {
		return ReconnectMin
	}
	return ReconnectMin + time.Duration(rand.Int64N(int64(ceil-ReconnectMin)+1))
}

// loadConfig refreshes relayURL/deviceID/jwt from config + keystore.
// Returns false when the relay isn't configured (not paired yet).
func (c *Client) loadConfig() bool {
	cfg := config.GetNoSetup()
	jwt := cfg.RelayJWT
	if jwt == "" {
		if tok, err := LoadJWT(); err == nil {
			jwt = tok
		}
	}
	c.mu.Lock()
	c.relayURL = cfg.RelayAgentWS()
	c.deviceID = cfg.DeviceID
	c.jwt = jwt
	c.mu.Unlock()
	return c.relayURL != "" && c.deviceID != "" && jwt != ""
}

// currentJWT returns the most recently loaded device JWT. Safe for concurrent
// use (handleStreamOpen runs on its own goroutine while the reconnect loop may
// refresh credentials).
func (c *Client) currentJWT() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.jwt
}

// relayDialer — websocket.DefaultDialer плюс запасной резолвер: если системный
// DNS сломан (мёртвый VPN оставил в системе свой tun-интерфейс с нерабочим
// DNS-сервером), имя релея разрешается через публичные серверы напрямую. Без
// этого агент 01–02.08.2026 не мог найти remotai.ru двое суток, хотя маршрут
// наружу работал. См. internal/dnsfallback.
var relayDialer = &websocket.Dialer{
	Proxy:            http.ProxyFromEnvironment,
	HandshakeTimeout: 45 * time.Second,
	NetDialContext:   dnsfallback.DialContext,
}

// runOnce dials, handshakes, then services messages until the connection drops.
func (c *Client) runOnce(ctx context.Context) error {
	dialer := relayDialer
	hdr := http.Header{}
	hdr.Set("Authorization", "Bearer "+c.jwt)
	hdr.Set("User-Agent", "tgcontrol/"+version.Version)

	conn, resp, err := dialer.DialContext(ctx, c.relayURL, hdr)
	if err != nil {
		if resp != nil {
			return &dialHTTPError{Status: resp.StatusCode, Err: err}
		}
		return fmt.Errorf("dial: %w", err)
	}
	c.mu.Lock()
	c.conn = conn
	c.mu.Unlock()
	// Clear c.conn on exit so ForwardEvent/writeJSON treat the reconnect gap as
	// disconnected instead of writing to a dead socket.
	defer func() {
		c.mu.Lock()
		if c.conn == conn {
			c.conn = nil
		}
		c.mu.Unlock()
		conn.Close()
	}()

	hn, _ := os.Hostname()
	hello := Hello{
		Type:            MsgHello,
		ProtocolVersion: ProtocolVersion,
		DeviceID:        c.deviceID,
		JWT:             c.jwt,
		AgentVersion:    version.Version,
		Platform:        runtime.GOOS,
		Hostname:        hn,
	}
	if err := conn.WriteJSON(hello); err != nil {
		return fmt.Errorf("hello: %w", err)
	}

	conn.SetReadDeadline(time.Now().Add(HelloDeadline))
	var welcome Welcome
	if err := conn.ReadJSON(&welcome); err != nil {
		return fmt.Errorf("welcome: %w", err)
	}
	if welcome.Type != MsgWelcome {
		return fmt.Errorf("welcome: unexpected type %q", welcome.Type)
	}
	conn.SetReadDeadline(time.Time{})
	c.setConnectionError(nil)
	log.Printf("[RELAY] connected (session=%s, uid=%d)", welcome.SessionID, welcome.UserID)

	// Refresh-on-connect: persist the freshly issued device JWT so the pairing
	// never expires while the PC connects at least once per TTL — no manual
	// re-pairing needed after reboots/updates.
	if welcome.RefreshedJWT != "" {
		if err := SaveJWT(welcome.RefreshedJWT); err != nil {
			log.Printf("[RELAY] save refreshed jwt: %v", err)
		} else {
			c.mu.Lock()
			c.jwt = welcome.RefreshedJWT
			c.mu.Unlock()
			_ = config.Update(func(cfg *config.Config) {
				cfg.RelayJWT = welcome.RefreshedJWT
				if welcome.RefreshedExpiry > 0 {
					cfg.RelayJWTExpiry = welcome.RefreshedExpiry
				}
			})
		}
	}

	// Keepalive
	stopPing := make(chan struct{})
	go c.pingLoop(conn, stopPing)
	defer close(stopPing)

	for {
		conn.SetReadDeadline(time.Now().Add(ReadDeadline))
		var raw json.RawMessage
		if err := conn.ReadJSON(&raw); err != nil {
			return err
		}
		var head struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(raw, &head); err != nil {
			continue
		}
		switch head.Type {
		case MsgPing:
			c.writeJSON(map[string]string{"type": MsgPong})
		case MsgCmd:
			var cmd Cmd
			if err := json.Unmarshal(raw, &cmd); err != nil {
				continue
			}
			go c.handleCmd(cmd)
		case MsgStreamOpen:
			var so StreamOpen
			if err := json.Unmarshal(raw, &so); err != nil {
				continue
			}
			go c.handleStreamOpen(so)
		case MsgClose:
			return errors.New("server requested close")
		}
	}
}

func (c *Client) pingLoop(conn *websocket.Conn, stop <-chan struct{}) {
	t := time.NewTicker(PingInterval)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			c.mu.Lock()
			_ = conn.SetWriteDeadline(time.Now().Add(WriteDeadline))
			err := conn.WriteJSON(map[string]string{"type": MsgPing})
			c.mu.Unlock()
			if err != nil {
				return
			}
		}
	}
}

// handleCmd dispatches a relay-originated request through the local HTTP
// handler and ships the response back through the WS connection.
func (c *Client) handleCmd(cmd Cmd) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[RELAY] panic in handleCmd: %v", r)
		}
	}()

	req, err := buildRequest(cmd)
	if err != nil {
		c.writeResult(CmdResult{Type: MsgCmdResult, RequestID: cmd.RequestID, StatusCode: 400, Error: err.Error()})
		return
	}

	// Запрос от СОСЕДНЕГО УСТРОЙСТВА того же аккаунта (сервер дотянулся до
	// компьютера) приходит тем же каналом, что и запрос владельца с телефона, —
	// различает их только пометка релея. Права соседа решает этот компьютер, а
	// не облако: см. peer_access.go.
	if cmd.Origin == OriginPeer {
		mode := config.GetNoSetup().PeerAccess
		who := cmd.FromName
		if who == "" {
			who = cmd.From
		}
		if ok, why := PeerAllowed(mode, cmd.Method, cmd.Path); !ok {
			log.Printf("[PEER] отказано %s: %s %s (%s)", who, cmd.Method, cmd.Path, NormalizePeerAccess(mode))
			body, _ := json.Marshal(map[string]string{"error": why, "peer_access": NormalizePeerAccess(mode)})
			c.writeResult(CmdResult{
				Type: MsgCmdResult, RequestID: cmd.RequestID, StatusCode: 403,
				Body: body, Headers: map[string]string{"Content-Type": "application/json"},
			})
			return
		}
		log.Printf("[PEER] %s → %s %s", who, cmd.Method, cmd.Path)
	}
	// Relay-proxied requests are already authorized at the relay (the user's JWT
	// + device-ownership check). The remote client can't know this PC's local
	// api_token, so inject it here to satisfy the local web server's auth.
	if tok := config.Get().APIToken; tok != "" {
		req.Header.Set("X-API-Token", tok)
	}
	rec := httptest.NewRecorder()
	c.httpHandler.ServeHTTP(rec, req)

	headers := make(map[string]string, len(rec.Result().Header))
	for k, v := range rec.Result().Header {
		if len(v) > 0 {
			headers[k] = v[0]
		}
	}
	c.writeResult(CmdResult{
		Type:       MsgCmdResult,
		RequestID:  cmd.RequestID,
		StatusCode: rec.Code,
		Body:       rec.Body.Bytes(),
		Headers:    headers,
	})
}

// ForwardEvent ships a locally-broadcast event to the relay so remote cloud
// clients receive it live. No-op when not connected. Safe for concurrent use.
func (c *Client) ForwardEvent(data []byte) {
	c.mu.Lock()
	connected := c.conn != nil
	c.mu.Unlock()
	if !connected {
		return
	}
	_ = c.writeJSON(Event{
		Type:      MsgEvent,
		Channel:   "events",
		Payload:   json.RawMessage(data),
		CreatedAt: time.Now().Unix(),
	})
}

func (c *Client) writeResult(res CmdResult) {
	if err := c.writeJSON(res); err != nil {
		log.Printf("[RELAY] write result: %v", err)
	}
}

// writeJSON serializes all writes on c.conn — gorilla/websocket forbids
// concurrent writes, and handleCmd results, ping replies and the ping loop all
// write from different goroutines.
func (c *Client) writeJSON(v any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn == nil {
		return nil
	}
	// Write deadline: зависший relay-сокет иначе блокирует c.mu бессрочно, а под
	// ним сериализованы ping, CmdResult и ForwardEvent (последний — из веб-event
	// тракта), что застопорило бы и рассылку событий LAN-клиентам.
	_ = c.conn.SetWriteDeadline(time.Now().Add(WriteDeadline))
	return c.conn.WriteJSON(v)
}

func buildRequest(cmd Cmd) (*http.Request, error) {
	u, err := url.Parse(cmd.Path)
	if err != nil {
		return nil, err
	}
	q := u.Query()
	for k, v := range cmd.Query {
		q.Set(k, v)
	}
	// Метка транспорта ставится ПОСЛЕДНЕЙ, поверх всего, что прислал клиент:
	// иначе пульт мог бы объявить облачный запрос домашним и обойти лимиты
	// тарифа. Запрос, пришедший этим путём, по определению идёт через релей.
	q.Set("transport", "relay")
	u.RawQuery = q.Encode()
	method := strings.ToUpper(cmd.Method)
	if method == "" {
		method = http.MethodGet
	}
	body := strings.NewReader(string(cmd.Body))
	req, err := http.NewRequest(method, u.String(), body)
	if err != nil {
		return nil, err
	}
	for k, v := range cmd.Headers {
		req.Header.Set(k, v)
	}
	return req, nil
}
