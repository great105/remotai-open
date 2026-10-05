//go:build linux || darwin

package pty

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"sync"
	"time"
)

// sockClient is the remotai-side backend for a session whose PTY lives in a
// separate persistent host process, reached over a unix socket. Mirrors
// *pipeClient on Windows; implements ptyConn + hostClient.
type sockClient struct {
	conn            net.Conn
	pid             uint32
	proto           uint16
	modes           []int // DEC-режимы из HelloMsg хоста (авторитетный источник на reattach)
	stream          hostStreamState
	resizeSupported bool
	resizeAcks      resizeAckState

	wmu  sync.Mutex
	rbuf []byte // leftover decoded output not yet returned by Read

	// Граница переигровки буфера — см. replayWatch.
	replay replayWatch

	exitedMu sync.Mutex
	exited   bool
}

func (c *sockClient) setReplayEnd(fn func()) { c.replay.set(fn) }

// dialHost connects to a session host over its unix socket, performs the
// handshake and returns a ready client. cols/rows of 0 leave the host's current
// size unchanged. Retries the dial until the deadline (the socket may not exist
// yet right after spawn).
func dialHost(id string, timeout time.Duration, cols, rows int) (hostClient, error) {
	return dialHostFrom(id, timeout, cols, rows, 0)
}

// dialHostFrom — то же, но говорит хосту, сколько байт сессии клиент УЖЕ имеет:
// тогда вместо всего буфера приедет только хвост после этой позиции. known=0 —
// «у меня ничего нет», прежнее поведение.
func dialHostFrom(id string, timeout time.Duration, cols, rows int, known uint64) (hostClient, error) {
	return dialHostFromEpoch(id, timeout, cols, rows, known, "")
}

func dialHostFromEpoch(id string, timeout time.Duration, cols, rows int, known uint64, knownEpoch string) (hostClient, error) {
	deadline := time.Now().Add(timeout)
	var conn net.Conn
	var err error
	for {
		conn, err = net.DialTimeout("unix", pipeName(id), 2*time.Second)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			return nil, err
		}
		time.Sleep(25 * time.Millisecond)
	}

	hb := helloPayload(cols, rows, known, knownEpoch)
	c := &sockClient{conn: conn}
	if err := c.writeFrame(frClientHello, hb); err != nil {
		conn.Close()
		return nil, err
	}

	// Read frames until Hello (skipping any unknowns the host might send first).
	handshakeDeadline := time.Now().Add(timeout + 2*time.Second)
	for {
		t, p, err := readFrame(conn)
		if err != nil {
			conn.Close()
			return nil, err
		}
		if t == frHello {
			if len(p) >= 2 {
				c.proto = binary.LittleEndian.Uint16(p[0:])
				var hm HelloMsg
				if json.Unmarshal(p[2:], &hm) == nil {
					c.pid = hm.ShellPID
					c.modes = hm.Modes
					if hm.StreamEpoch != "" && hm.ReplayStart <= hm.Produced {
						c.stream = hostStreamState{Epoch: hm.StreamEpoch, ReplayStart: hm.ReplayStart, Produced: hm.Produced}
					}
					c.resizeSupported = hm.ResizeAck
				}
			}
			return c, nil
		}
		if time.Now().After(handshakeDeadline) {
			conn.Close()
			return nil, fmt.Errorf("pty host handshake timeout")
		}
	}
}

func (c *sockClient) writeFrame(t frameType, payload []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	return writeFrame(c.conn, t, payload)
}

// Read returns raw terminal bytes decoded from Output/Snapshot frames. A clean
// shell exit (frExit) surfaces as io.EOF; a socket error surfaces as that error.
func (c *sockClient) Read(p []byte) (int, error) {
	return c.readDecoded(p, func() (frameType, []byte, error) { return readFrame(c.conn) })
}

// readDecoded is the frame-source-agnostic decode loop (mirrors pipeClient).
func (c *sockClient) readDecoded(p []byte, nextFrame func() (frameType, []byte, error)) (int, error) {
	if len(c.rbuf) > 0 {
		n := copy(p, c.rbuf)
		c.rbuf = c.rbuf[n:]
		return n, nil
	}
	for {
		t, payload, err := nextFrame()
		if err != nil {
			c.resizeAcks.failAll(err)
			return 0, err
		}
		switch t {
		case frOutput, frSnapshot:
			// Граница переигровки: снапшот — это прошлое, output — живой поток
			// (см. replayWatch). Отметку ставим ДО отдачи байтов, чтобы сигнал
			// встал в очередь зеркала строго между ними.
			if t == frSnapshot {
				c.replay.noteSnapshot()
			} else {
				c.replay.noteOutput()
			}
			if len(payload) == 0 {
				continue
			}
			n := copy(p, payload)
			if n < len(payload) {
				c.rbuf = payload[n:]
			}
			return n, nil
		case frReplayEnd:
			// Явная граница от нового хоста: переигровка кончилась СЕЙЧАС, а не
			// «когда сессия что-то напечатает» (тихая сессия может молчать
			// часами — внешний аудит 2.57.18, P0-03).
			c.replay.noteReplayEnd()
			continue
		case frResizeAck:
			c.resizeAcks.acknowledge(payload)
			continue
		case frResizeNack:
			c.resizeAcks.reject(payload)
			continue
		case frExit:
			c.setExited()
			c.resizeAcks.failAll(io.EOF)
			return 0, io.EOF
		case frPong:
			continue
		default: // ignore unknown frame types (forward-compat)
			continue
		}
	}
}

func (c *sockClient) Write(p []byte) (int, error) {
	if err := c.writeFrame(frInput, p); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (c *sockClient) Resize(cols, rows int) error {
	return c.ResizeOrdered(cols, rows, nil)
}

func (c *sockClient) ResizeOrdered(cols, rows int, afterApply func()) error {
	return c.ResizeOrderedComplete(cols, rows, afterApply, nil)
}

func (c *sockClient) ResizeOrderedComplete(cols, rows int, afterApply func(), afterDone func(error)) error {
	if !c.resizeSupported {
		if err := c.writeFrame(frResize, resizePayload(cols, rows, 0, false)); err != nil {
			return err
		}
		if afterApply != nil {
			afterApply()
		}
		if afterDone != nil {
			afterDone(nil)
		}
		return nil
	}
	return c.resizeAcks.requestAfterDone(func(seq uint32) error {
		return c.writeFrame(frResize, resizePayload(cols, rows, seq, true))
	}, afterApply, afterDone)
}

// resizeOrderingSupported distinguishes a negotiated ACK barrier from the
// compatibility method on a new client talking to a pre-ACK host.
func (c *sockClient) resizeOrderingSupported() bool { return c.resizeSupported }

// Close detaches WITHOUT killing the host — the shell keeps running. Used on
// reap/graceful-shutdown so terminals survive a remotai restart.
func (c *sockClient) Close() error {
	c.resizeAcks.failAll(io.ErrClosedPipe)
	_ = c.writeFrame(frDetach, nil) // best effort
	return c.conn.Close()
}

// kill terminates the host (and its shell) — used for explicit user close.
func (c *sockClient) kill() error {
	c.resizeAcks.failAll(io.ErrClosedPipe)
	_ = c.writeFrame(frKill, nil)
	return c.conn.Close()
}

func (c *sockClient) shellPID() uint32     { return c.pid }
func (c *sockClient) protoVersion() uint16 { return c.proto }

// hostModes возвращает DEC-режимы, наблюдавшиеся хостом за всю жизнь сессии
// (пусто у старого хоста, оставшегося от прошлого бинаря через автообновление).
func (c *sockClient) hostModes() []int { return c.modes }

func (c *sockClient) streamState() (hostStreamState, bool) {
	return c.stream, c.stream.Epoch != ""
}

// currentCWD reads /proc/<pid>/cwd for the shell. Host and client share the box
// and user, so the symlink is readable directly — no pipe round-trip.
func (c *sockClient) currentCWD() (string, error) {
	if c.pid == 0 {
		return "", fmt.Errorf("no shell pid")
	}
	return os.Readlink("/proc/" + strconv.FormatUint(uint64(c.pid), 10) + "/cwd")
}

func (c *sockClient) exitedClean() bool {
	c.exitedMu.Lock()
	defer c.exitedMu.Unlock()
	return c.exited
}

func (c *sockClient) setExited() {
	c.exitedMu.Lock()
	c.exited = true
	c.exitedMu.Unlock()
}
