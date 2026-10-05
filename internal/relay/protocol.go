// Package relay implements the wire protocol between the desktop agent and
// the cloud relay server.
//
// The relay lets a desktop agent (this binary) accept commands from the
// hosted clients (APK / web app) without requiring its own bot token, public
// domain, or Cloudflare tunnel. The agent establishes an OUTBOUND WebSocket
// to the relay (production: remotai.ru) and serves commands forwarded by the
// relay over that channel. The client's Available() returns true once
// pairing is configured (relay URL + JWT).
//
// NOTE: keep in sync with tgcontrol-relay/internal/protocol/protocol.go —
// the relay keeps its own mirror of these types so it builds independently.
package relay

import "time"

// Protocol version. Bumped if the wire format changes incompatibly.
const ProtocolVersion = 1

// Message types exchanged between agent and relay.
const (
	// Agent → Relay
	MsgHello     = "hello"      // initial handshake with auth
	MsgPong      = "pong"       // keepalive response
	MsgCmdResult = "cmd_result" // result of a command from relay
	MsgEvent     = "event"      // out-of-band event (PTY waiting input etc.)

	// Relay → Agent
	MsgWelcome    = "welcome"     // handshake accepted
	MsgPing       = "ping"        // keepalive
	MsgCmd        = "cmd"         // command from a Mini App / APK client
	MsgClose      = "close"       // soft shutdown request
	MsgStreamOpen = "stream_open" // bridge a streaming WS (PTY / Remote Desktop)
)

// Hello is the first agent → relay message after the WS handshake.
type Hello struct {
	Type            string `json:"type"` // MsgHello
	ProtocolVersion int    `json:"protocol_version"`
	DeviceID        string `json:"device_id"`
	JWT             string `json:"jwt"` // issued during pairing
	AgentVersion    string `json:"agent_version"`
	Platform        string `json:"platform"` // "windows" | "linux" | "darwin"
	Hostname        string `json:"hostname"`
}

// Welcome is the relay's response to Hello.
type Welcome struct {
	Type      string `json:"type"` // MsgWelcome
	SessionID string `json:"session_id"`
	IssuedAt  int64  `json:"issued_at"`
	UserID    int64  `json:"user_id"`

	// Refresh-on-connect: a fresh device JWT issued on each connect. The agent
	// persists it so the pairing never expires while the PC comes online at
	// least once per JWT TTL. Optional — old relays omit it.
	RefreshedJWT    string `json:"refreshed_jwt,omitempty"`
	RefreshedExpiry int64  `json:"refreshed_expiry,omitempty"`
}

// Cmd represents a command forwarded from a Mini App / APK client.
// The agent dispatches it to the existing HTTP handler set so we don't need
// to duplicate logic.
type Cmd struct {
	Type      string            `json:"type"` // MsgCmd
	RequestID string            `json:"request_id"`
	Method    string            `json:"method"` // "GET" | "POST" | …
	Path      string            `json:"path"`   // "/api/sessions"
	Query     map[string]string `json:"query,omitempty"`
	Body      []byte            `json:"body,omitempty"`
	Headers   map[string]string `json:"headers,omitempty"`

	// Origin/From — кто спрашивает. OriginPeer означает «другое устройство
	// этого же аккаунта» (сервер, дотянувшийся до компьютера через облако);
	// пусто — обычный клиент владельца. Права соседа ограничивает САМ
	// компьютер (config.PeerAccess), см. peer_access.go.
	Origin   string `json:"origin,omitempty"`
	From     string `json:"from,omitempty"`
	FromName string `json:"from_name,omitempty"`
}

// Значения Cmd.Origin.
const (
	OriginUser = "user"
	OriginPeer = "peer"
)

// CmdResult is the agent's reply to a Cmd.
type CmdResult struct {
	Type       string            `json:"type"` // MsgCmdResult
	RequestID  string            `json:"request_id"`
	StatusCode int               `json:"status_code"`
	Body       []byte            `json:"body,omitempty"`
	Headers    map[string]string `json:"headers,omitempty"`
	Error      string            `json:"error,omitempty"`
}

// Event is an out-of-band message (PTY events, license changes, etc.) that
// should be fanned out to all of this user's connected clients via the relay.
type Event struct {
	Type      string `json:"type"`    // MsgEvent
	Channel   string `json:"channel"` // "pty" | "session" | "research" | …
	Payload   any    `json:"payload"`
	CreatedAt int64  `json:"created_at"`
}

// StreamOpen is the relay → agent request to bridge a streaming WebSocket
// (PTY I/O or Remote Desktop frames) that can't be tunneled over the
// request/response control channel. The agent dials a fresh outbound WS back to
// the relay carrying Token, dials its own local Path, and pipes between them.
type StreamOpen struct {
	Type     string `json:"type"`      // MsgStreamOpen
	StreamID string `json:"stream_id"` // correlation id
	Token    string `json:"token"`     // one-time secret echoed to /v1/agent/stream
	Path     string `json:"path"`      // local WS path, e.g. "/ws/pty/abc" | "/ws/screen"
}

// Defaults for the agent loop.
const (
	HelloDeadline = 10 * time.Second
	PingInterval  = 25 * time.Second
	ReadDeadline  = 60 * time.Second
	WriteDeadline = 10 * time.Second
	ReconnectMin  = 1 * time.Second
	ReconnectMax  = 60 * time.Second
)
