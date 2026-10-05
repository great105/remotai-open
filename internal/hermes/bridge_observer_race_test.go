package hermes

import (
	"testing"
	"time"
)

func TestRetiredBridgeCannotPublishAcrossJournalLockWait(t *testing.T) {
	m := regressionManager(t)
	old := &rpcBridge{}
	m.bridge = old
	m.control.mu.Lock()
	done := make(chan struct{})
	go func() {
		m.addBridgeEvent([]byte(`{"id":"old-question","method":"clarify","params":{"session_id":"s"}}`), old)
		close(done)
	}()
	deadline := time.Now().Add(time.Second)
	for m.EventCursor().LatestSeq == 0 {
		if time.Now().After(deadline) {
			m.control.mu.Unlock()
			t.Fatal("event never reached journal seam")
		}
		time.Sleep(time.Millisecond)
	}
	m.mu.Lock()
	m.bridge = &rpcBridge{}
	m.epoch++
	m.mu.Unlock()
	m.control.mu.Unlock()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("journal observer stranded")
	}
	if len(m.control.Attention) != 0 {
		t.Fatal("retired bridge published after new generation acquired journal")
	}
}
