//go:build linux || darwin

package pty

import (
	"io"
	"net"
	"os"
	"sync"
)

// unixHost is the server-side transport for a pty-host: a unix-socket listener
// serving one client at a time. Go's netpoller handles concurrent read/write on
// the accepted conn out of the box, so none of the overlapped-I/O dance the
// Windows named-pipe path needs is required here. Implements hostTransport.
type unixHost struct {
	ln   net.Listener
	path string

	mu   sync.Mutex
	conn net.Conn // current client, nil when none
	wmu  sync.Mutex
}

// createUnixServer creates the listening unix socket for a host, replacing any
// stale socket file left by a previous host at the same path.
func createUnixServer(path string) (*unixHost, error) {
	if err := preparePrivateSocketDir(path); err != nil {
		return nil, err
	}
	_ = os.Remove(path) // stale socket from a dead host
	ln, err := net.Listen("unix", path)
	if err != nil {
		if dir := privateSocketDir(path); dir != "" {
			_ = os.Remove(dir) // our empty directory only
		}
		return nil, err
	}
	// Owner-only: other users on a shared box must not reach the shell.
	_ = os.Chmod(path, 0o600)
	return &unixHost{ln: ln, path: path}, nil
}

// connect waits for the next client. Blocks in Accept until a client dials or
// wake()/Close() tears the listener down.
func (u *unixHost) connect() error {
	c, err := u.ln.Accept()
	if err != nil {
		return err
	}
	u.mu.Lock()
	u.conn = c
	u.mu.Unlock()
	return nil
}

func (u *unixHost) current() net.Conn {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.conn
}

func (u *unixHost) readFrame() (frameType, []byte, error) {
	c := u.current()
	if c == nil {
		return 0, nil, io.EOF
	}
	return readFrame(c)
}

func (u *unixHost) writeFrame(t frameType, payload []byte) error {
	c := u.current()
	if c == nil {
		return io.EOF
	}
	u.wmu.Lock()
	defer u.wmu.Unlock()
	return writeFrame(c, t, payload)
}

// disconnect closes the current client connection so the next connect() can
// accept a fresh one (and unblocks a reader parked in readFrame).
func (u *unixHost) disconnect() {
	u.mu.Lock()
	c := u.conn
	u.conn = nil
	u.mu.Unlock()
	if c != nil {
		_ = c.Close()
	}
}

// wake tears down the listener so a parked connect() returns — used from
// host.markDead after the grace window (analogue of CancelIoEx on Windows).
func (u *unixHost) wake() {
	_ = u.ln.Close()
}

func (u *unixHost) Close() error {
	u.disconnect()
	err := u.ln.Close()
	_ = os.Remove(u.path)
	if dir := privateSocketDir(u.path); dir != "" {
		_ = os.Remove(dir) // never recursive; unrelated files prevent removal
	}
	return err
}
