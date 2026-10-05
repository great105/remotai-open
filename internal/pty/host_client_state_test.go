package pty

import (
	"encoding/binary"
	"io"
	"strings"
	"testing"
	"time"
)

func TestResizePayloadKeepsLegacyPrefix(t *testing.T) {
	legacy := resizePayload(120, 33, 0, false)
	modern := resizePayload(120, 33, 42, true)
	if len(legacy) != 4 || len(modern) != 8 {
		t.Fatalf("resize payload sizes: legacy=%d modern=%d", len(legacy), len(modern))
	}
	if string(legacy) != string(modern[:4]) || binary.LittleEndian.Uint32(modern[4:]) != 42 {
		t.Fatalf("append-only resize payload несовместим: legacy=%v modern=%v", legacy, modern)
	}
}

func TestResizeAckStateWaitsForMatchingAck(t *testing.T) {
	var state resizeAckState
	sent := make(chan uint32, 1)
	done := make(chan error, 1)
	go func() {
		done <- state.requestWithin(func(seq uint32) error {
			sent <- seq
			return nil
		}, time.Second)
	}()
	seq := <-sent

	wrong := make([]byte, 4)
	binary.LittleEndian.PutUint32(wrong, seq+1)
	state.acknowledge(wrong)
	select {
	case err := <-done:
		t.Fatalf("чужой ack разблокировал resize: %v", err)
	case <-time.After(20 * time.Millisecond):
	}

	ack := make([]byte, 4)
	binary.LittleEndian.PutUint32(ack, seq)
	state.acknowledge(ack)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("свой ack не разблокировал resize")
	}
}

func TestResizeAckStateLateAckStillAppliesMirrorAfterTimeout(t *testing.T) {
	var state resizeAckState
	var seq uint32
	applied := make(chan struct{})
	err := state.requestWithinAfter(func(sent uint32) error {
		seq = sent
		return nil
	}, func() { close(applied) }, 20*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "resize ack timeout") {
		t.Fatalf("soft timeout не вернул ошибку: %v", err)
	}

	ack := make([]byte, 4)
	binary.LittleEndian.PutUint32(ack, seq)
	state.acknowledge(ack)
	select {
	case <-applied:
	case <-time.After(time.Second):
		t.Fatal("late ACK не применил mirror callback")
	}
	state.mu.Lock()
	pending := len(state.waiters)
	state.mu.Unlock()
	if pending != 0 {
		t.Fatalf("late ACK оставил waiter: %d", pending)
	}
}

func TestResizeAckStateLateTerminalOutcomeRunsCompletion(t *testing.T) {
	for _, tc := range []struct {
		name   string
		finish func(*resizeAckState, uint32)
	}{
		{"nack", func(state *resizeAckState, seq uint32) {
			payload := make([]byte, 4)
			binary.LittleEndian.PutUint32(payload, seq)
			state.reject(payload)
		}},
		{"transport close", func(state *resizeAckState, _ uint32) {
			state.failAll(io.EOF)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var state resizeAckState
			var seq uint32
			completed := make(chan error, 1)
			err := state.requestWithinAfterDone(func(sent uint32) error {
				seq = sent
				return nil
			}, func() { t.Error("terminal failure applied successful resize callback") }, func(err error) {
				completed <- err
			}, 10*time.Millisecond)
			if err == nil || !strings.Contains(err.Error(), "resize ack timeout") {
				t.Fatalf("soft timeout not returned: %v", err)
			}

			tc.finish(&state, seq)
			select {
			case completionErr := <-completed:
				if completionErr == nil {
					t.Fatal("terminal outcome reported successful completion")
				}
			case <-time.After(time.Second):
				t.Fatal("late terminal outcome did not run completion callback")
			}
		})
	}
}

func TestResizeNackRejectsWithoutApplyingMirrorOrLeakingWaiter(t *testing.T) {
	var state resizeAckState
	sent := make(chan uint32, 1)
	applied := make(chan struct{}, 1)
	done := make(chan error, 1)
	go func() {
		done <- state.requestWithinAfter(func(seq uint32) error {
			sent <- seq
			return nil
		}, func() { applied <- struct{}{} }, time.Second)
	}()
	seq := <-sent
	nack := make([]byte, 4)
	binary.LittleEndian.PutUint32(nack, seq)
	state.reject(nack)
	if err := <-done; err == nil || !strings.Contains(err.Error(), "resize rejected") {
		t.Fatalf("NACK did not reject resize: %v", err)
	}
	select {
	case <-applied:
		t.Fatal("NACK applied mirror callback")
	default:
	}
	state.mu.Lock()
	pending := len(state.waiters)
	state.mu.Unlock()
	if pending != 0 {
		t.Fatalf("NACK left waiter: %d", pending)
	}
}

// readDecoded обрабатывает ACK в своей (output) горутине. Она не должна читать
// следующий post-resize frame, пока Resize-goroutine не перестроила mirror.
func TestResizeAckBlocksReaderUntilMirrorCallbackCompletes(t *testing.T) {
	var state resizeAckState
	sent := make(chan uint32, 1)
	callbackEntered := make(chan struct{})
	releaseCallback := make(chan struct{})
	requestDone := make(chan error, 1)
	go func() {
		requestDone <- state.requestAfter(func(seq uint32) error {
			sent <- seq
			return nil
		}, func() {
			close(callbackEntered)
			<-releaseCallback
		})
	}()
	seq := <-sent
	ack := make([]byte, 4)
	binary.LittleEndian.PutUint32(ack, seq)
	readerReleased := make(chan struct{})
	go func() {
		state.acknowledge(ack)
		close(readerReleased)
	}()
	<-callbackEntered
	select {
	case <-readerReleased:
		t.Fatal("reader crossed resize ACK before mirror callback completed")
	case <-time.After(20 * time.Millisecond):
	}
	close(releaseCallback)
	if err := <-requestDone; err != nil {
		t.Fatal(err)
	}
	select {
	case <-readerReleased:
	case <-time.After(time.Second):
		t.Fatal("reader remained blocked after mirror callback")
	}
}
