//go:build windows

package pty

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/windows"
)

// pipeClient is the remotai-side backend for a session whose ConPTY lives in a
// separate persistent host process. It implements the same method set as
// *conPTY (the ptyConn interface) so Session/readLoop are unchanged.
type pipeClient struct {
	conn            *pipeConn
	pid             uint32
	proto           uint16
	modes           []int // DEC-режимы из HelloMsg хоста (авторитетный источник на reattach)
	stream          hostStreamState
	resizeSupported bool
	resizeAcks      resizeAckState

	rbuf []byte // leftover decoded output not yet returned by Read

	// Граница переигровки буфера — см. replayWatch.
	replay replayWatch

	exitedMu sync.Mutex
	exited   bool
}

func (c *pipeClient) setReplayEnd(fn func()) { c.replay.set(fn) }

// dialHost connects to a session host, performs the handshake and returns a
// ready client. cols/rows of 0 leave the host's current size unchanged.
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
	conn, err := dialPipe(pipeName(id), timeout)
	if err != nil {
		return nil, err
	}

	hb := helloPayload(cols, rows, known, knownEpoch)
	if err := conn.writeFrame(frClientHello, hb); err != nil {
		conn.Close()
		return nil, err
	}

	// Read frames until Hello (skipping any unknowns the host might send first).
	deadline := time.Now().Add(timeout + 2*time.Second)
	for {
		t, p, err := conn.readFrame()
		if err != nil {
			conn.Close()
			return nil, err
		}
		if t == frHello {
			c := &pipeClient{conn: conn}
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
		if time.Now().After(deadline) {
			conn.Close()
			return nil, fmt.Errorf("pty host handshake timeout")
		}
	}
}

// Read returns raw terminal bytes decoded from Output/Snapshot frames. A clean
// shell exit (frExit) surfaces as io.EOF; a pipe error surfaces as that error
// (the distinction drives store cleanup — see exitedClean).
func (c *pipeClient) Read(p []byte) (int, error) {
	return c.readDecoded(p, c.conn.readFrame)
}

// readDecoded is the frame-source-agnostic decode loop (unit-tested directly).
func (c *pipeClient) readDecoded(p []byte, nextFrame func() (frameType, []byte, error)) (int, error) {
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

func (c *pipeClient) Write(p []byte) (int, error) {
	if err := c.conn.writeFrame(frInput, p); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (c *pipeClient) Resize(cols, rows int) error {
	return c.ResizeOrdered(cols, rows, nil)
}

func (c *pipeClient) ResizeOrdered(cols, rows int, afterApply func()) error {
	return c.ResizeOrderedComplete(cols, rows, afterApply, nil)
}

func (c *pipeClient) ResizeOrderedComplete(cols, rows int, afterApply func(), afterDone func(error)) error {
	if !c.resizeSupported {
		if err := c.conn.writeFrame(frResize, resizePayload(cols, rows, 0, false)); err != nil {
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
		return c.conn.writeFrame(frResize, resizePayload(cols, rows, seq, true))
	}, afterApply, afterDone)
}

// resizeOrderingSupported distinguishes a negotiated ACK barrier from the
// compatibility method on a new client talking to a pre-ACK host.
func (c *pipeClient) resizeOrderingSupported() bool { return c.resizeSupported }

// Close detaches WITHOUT killing the host — the shell keeps running. Used on
// reap/graceful-shutdown so terminals survive a remotai restart.
func (c *pipeClient) Close() error {
	c.resizeAcks.failAll(io.ErrClosedPipe)
	_ = c.conn.writeFrame(frDetach, nil) // best effort
	return c.conn.Close()
}

// kill terminates the host (and its shell) — used for explicit user close.
func (c *pipeClient) kill() error {
	c.resizeAcks.failAll(io.ErrClosedPipe)
	_ = c.conn.writeFrame(frKill, nil)
	return c.conn.Close()
}

func (c *pipeClient) shellPID() uint32 { return c.pid }

func (c *pipeClient) protoVersion() uint16 { return c.proto }

// hostModes возвращает DEC-режимы, которые хост наблюдал за всю жизнь сессии
// (пусто у старого хоста, оставшегося от прошлого бинаря через автообновление).
func (c *pipeClient) hostModes() []int { return c.modes }

func (c *pipeClient) streamState() (hostStreamState, bool) {
	return c.stream, c.stream.Epoch != ""
}

// currentCWD reads the shell's working directory directly via OpenProcess, so no
// extra pipe round-trip is needed. Works same-user and (with SeDebugPrivilege)
// for a LocalSystem remotai over a user-session shell.
func (c *pipeClient) currentCWD() (string, error) {
	if c.pid == 0 {
		return "", fmt.Errorf("no shell pid")
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_INFORMATION|windows.PROCESS_VM_READ, false, c.pid)
	if err != nil {
		return "", err
	}
	defer windows.CloseHandle(h)
	return readCWD(syscall.Handle(h))
}

func (c *pipeClient) exitedClean() bool {
	c.exitedMu.Lock()
	defer c.exitedMu.Unlock()
	return c.exited
}

func (c *pipeClient) setExited() {
	c.exitedMu.Lock()
	c.exited = true
	c.exitedMu.Unlock()
}
