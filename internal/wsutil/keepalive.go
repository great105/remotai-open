// Package wsutil holds shared WebSocket helpers.
//
// The Mini App's long-lived WebSockets (PTY terminal, remote-desktop screen, and
// the event hub) used to carry no application-level keepalive. An idle terminal
// produces no frames, so an intermediary (the Cloudflare tunnel, mobile NAT, a
// relay leg) silently drops the connection after a few seconds and the frontend
// reconnect-loops — the "terminal disconnects every 2-4s" symptom.
//
// Keepalive fixes this with the standard gorilla pattern: a periodic server ping
// (browsers auto-reply with pong) plus a read deadline that the pong handler
// extends, so a truly dead peer is detected deterministically instead of waiting
// on the OS TCP timeout.
package wsutil

import (
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const (
	// PongWait is how long we tolerate silence (no pong, no data) before the
	// read deadline fires and the handler tears the connection down.
	PongWait = 70 * time.Second
	// PingPeriod must be < PongWait so a missed pong is noticed promptly.
	PingPeriod = 25 * time.Second
	// WriteWait bounds a single control/data write so a stalled socket can't
	// wedge the writer goroutine forever.
	WriteWait = 10 * time.Second
)

// Keepalive drives server-initiated pings and read-deadline management for one
// gorilla connection. WriteControl (used for pings) is safe to call concurrently
// with WriteMessage per gorilla's contract, so no external write lock is needed.
type Keepalive struct {
	conn *websocket.Conn
	done chan struct{}
	once sync.Once
}

// Start sets the initial read deadline + pong handler and launches the ping
// ticker. Call Stop (defer) when the handler returns.
func Start(conn *websocket.Conn) *Keepalive {
	k := &Keepalive{conn: conn, done: make(chan struct{})}
	_ = conn.SetReadDeadline(time.Now().Add(PongWait))
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(PongWait))
	})
	go k.loop()
	return k
}

func (k *Keepalive) loop() {
	t := time.NewTicker(PingPeriod)
	defer t.Stop()
	for {
		select {
		case <-k.done:
			return
		case <-t.C:
			if err := k.conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(WriteWait)); err != nil {
				return
			}
		}
	}
}

// Touch extends the read deadline. Call it after every successful ReadMessage so
// an actively-used connection (which may not generate pongs) isn't killed.
func (k *Keepalive) Touch() {
	_ = k.conn.SetReadDeadline(time.Now().Add(PongWait))
}

// Stop halts the ping ticker. Idempotent.
func (k *Keepalive) Stop() {
	k.once.Do(func() { close(k.done) })
}
