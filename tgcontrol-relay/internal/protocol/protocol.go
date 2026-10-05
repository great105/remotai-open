// Package protocol — wire-формат между relay и desktop agent.
// Зеркалит tgcontrol/internal/relay (P2.2). Хранится в обоих репах,
// чтобы каждый собирался независимо без shared module.
package protocol

const ProtocolVersion = 1

const (
	// Agent → Relay
	MsgHello     = "hello"
	MsgPong      = "pong"
	MsgCmdResult = "cmd_result"
	MsgEvent     = "event"

	// Relay → Agent
	MsgWelcome    = "welcome"
	MsgPing       = "ping"
	MsgCmd        = "cmd"
	MsgClose      = "close"
	MsgStreamOpen = "stream_open" // ask agent to bridge a streaming WS (PTY / Remote Desktop)
)

type Hello struct {
	Type            string `json:"type"`
	ProtocolVersion int    `json:"protocol_version"`
	DeviceID        string `json:"device_id"`
	JWT             string `json:"jwt"`
	AgentVersion    string `json:"agent_version"`
	Platform        string `json:"platform"`
	Hostname        string `json:"hostname"`
}

type Welcome struct {
	Type      string `json:"type"`
	SessionID string `json:"session_id"`
	IssuedAt  int64  `json:"issued_at"`
	UserID    int64  `json:"user_id"`

	// Refresh-on-connect: релей выдаёт свежий device JWT на каждом коннекте,
	// агент его сохраняет — привязка не протухает, пока ПК хоть иногда онлайн.
	// Опционально; старые агенты поле игнорируют.
	RefreshedJWT    string `json:"refreshed_jwt,omitempty"`
	RefreshedExpiry int64  `json:"refreshed_expiry,omitempty"`
}

type Cmd struct {
	Type      string            `json:"type"`
	RequestID string            `json:"request_id"`
	Method    string            `json:"method"`
	Path      string            `json:"path"`
	Query     map[string]string `json:"query,omitempty"`
	Body      []byte            `json:"body,omitempty"`
	Headers   map[string]string `json:"headers,omitempty"`

	// Origin — от кого запрос. Пусто (или OriginUser) — обычный клиент
	// пользователя: телефон, браузер, мини-апп. OriginPeer — ДРУГОЕ УСТРОЙСТВО
	// того же аккаунта (сервер, дотянувшийся до компьютера). Различать
	// обязательно: соседу компьютер даёт ровно столько прав, сколько разрешил
	// владелец настройкой peer_access, и решает это сам компьютер, а не релей.
	Origin   string `json:"origin,omitempty"`
	From     string `json:"from,omitempty"`      // device_id инициатора
	FromName string `json:"from_name,omitempty"` // как назвать его человеку
}

const (
	OriginUser = "user"
	OriginPeer = "peer"
)

type CmdResult struct {
	Type       string            `json:"type"`
	RequestID  string            `json:"request_id"`
	StatusCode int               `json:"status_code"`
	Body       []byte            `json:"body,omitempty"`
	Headers    map[string]string `json:"headers,omitempty"`
	Error      string            `json:"error,omitempty"`
}

type Event struct {
	Type      string `json:"type"`
	Channel   string `json:"channel"`
	Payload   any    `json:"payload"`
	CreatedAt int64  `json:"created_at"`
}

// StreamOpen — relay → agent: open a dedicated streaming bridge.
//
// The relay can't tunnel binary streams (JPEG frames, raw terminal I/O) over
// the request/response control channel, so for PTY and Remote Desktop it asks
// the agent to dial a fresh OUTBOUND WS back to the relay (carrying Token) and
// pipe it to the agent's own local WS endpoint at Path. The relay then becomes
// a transparent byte-pump between that agent stream and the waiting client WS.
type StreamOpen struct {
	Type     string `json:"type"`      // MsgStreamOpen
	StreamID string `json:"stream_id"` // correlation id (for logging)
	Token    string `json:"token"`     // one-time secret the agent echoes on /v1/agent/stream
	Path     string `json:"path"`      // local WS path, e.g. "/ws/pty/abc" or "/ws/screen"
}
