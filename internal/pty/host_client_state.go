package pty

import (
	"encoding/binary"
	"errors"
	"fmt"
	"sync"
	"time"
)

// resizeAckWait is a caller-facing soft deadline, not the lifetime of the
// protocol waiter. A local host may apply Resize while its ordered outbound
// lane is backed up; a late ACK must still resize the mirror synchronously
// before readDecoded is allowed to consume post-resize output.
const resizeAckWait = 2 * time.Second

var errResizeAckTimeout = errors.New("pty host resize ack timeout")

// resizeAckState связывает frResize с append-only frResizeAck. Ack читает
// обычный readLoop, поэтому параллельные resize разводятся seq.
type resizeAckState struct {
	mu      sync.Mutex
	next    uint32
	waiters map[uint32]*resizeWaiter
}

type resizeWaiter struct {
	done      chan error
	afterAck  func()
	afterDone func(error)
}

func (r *resizeAckState) request(send func(seq uint32) error) error {
	return r.requestAfter(send, nil)
}

func (r *resizeAckState) requestAfter(send func(seq uint32) error, afterAck func()) error {
	return r.requestWithinAfterDone(send, afterAck, nil, resizeAckWait)
}

func (r *resizeAckState) requestAfterDone(send func(seq uint32) error, afterAck func(), afterDone func(error)) error {
	return r.requestWithinAfterDone(send, afterAck, afterDone, resizeAckWait)
}

func (r *resizeAckState) requestWithin(send func(seq uint32) error, wait time.Duration) error {
	return r.requestWithinAfter(send, nil, wait)
}

func (r *resizeAckState) requestWithinAfter(send func(seq uint32) error, afterAck func(), wait time.Duration) error {
	return r.requestWithinAfterDone(send, afterAck, nil, wait)
}

func (r *resizeAckState) requestWithinAfterDone(send func(seq uint32) error, afterAck func(), afterDone func(error), wait time.Duration) error {
	r.mu.Lock()
	r.next++
	if r.next == 0 {
		r.next++
	}
	seq := r.next
	if r.waiters == nil {
		r.waiters = make(map[uint32]*resizeWaiter)
	}
	w := &resizeWaiter{done: make(chan error, 1), afterAck: afterAck, afterDone: afterDone}
	r.waiters[seq] = w
	r.mu.Unlock()

	if err := send(seq); err != nil {
		r.remove(seq)
		return err
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case err := <-w.done:
		return err
	case <-timer.C:
		// Do not remove the waiter. ACK is ordered with host output and may be
		// delayed by local backpressure; acknowledge must still run afterAck in
		// the reader before it advances to the next output frame.
		return errResizeAckTimeout
	}
}

func (r *resizeAckState) acknowledge(payload []byte) {
	if len(payload) < 4 {
		return
	}
	seq := binary.LittleEndian.Uint32(payload)
	r.mu.Lock()
	w := r.waiters[seq]
	delete(r.waiters, seq)
	r.mu.Unlock()
	if w != nil {
		if w.afterAck != nil {
			w.afterAck()
		}
		if w.afterDone != nil {
			w.afterDone(nil)
		}
		w.done <- nil
	}
}

func (r *resizeAckState) reject(payload []byte) {
	if len(payload) < 4 {
		return
	}
	seq := binary.LittleEndian.Uint32(payload)
	r.mu.Lock()
	w := r.waiters[seq]
	delete(r.waiters, seq)
	r.mu.Unlock()
	if w != nil {
		err := fmt.Errorf("pty host resize rejected")
		if w.afterDone != nil {
			w.afterDone(err)
		}
		w.done <- err
	}
}

// failAll releases requests when the transport ends. It deliberately never
// invokes afterAck: no successful host apply was observed.
func (r *resizeAckState) failAll(err error) {
	if err == nil {
		err = fmt.Errorf("pty host resize transport closed")
	}
	r.mu.Lock()
	waiters := r.waiters
	r.waiters = nil
	r.mu.Unlock()
	for _, w := range waiters {
		if w.afterDone != nil {
			w.afterDone(err)
		}
		w.done <- err
	}
}

func (r *resizeAckState) remove(seq uint32) {
	r.mu.Lock()
	delete(r.waiters, seq)
	r.mu.Unlock()
}

func resizePayload(cols, rows int, seq uint32, withAck bool) []byte {
	n := 4
	if withAck {
		n = 8
	}
	b := make([]byte, n)
	binary.LittleEndian.PutUint16(b[0:], uint16(cols))
	binary.LittleEndian.PutUint16(b[2:], uint16(rows))
	if withAck {
		binary.LittleEndian.PutUint32(b[4:], seq)
	}
	return b
}
