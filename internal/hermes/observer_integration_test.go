package hermes

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/gorilla/websocket"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

func TestObserverReceivesAndDeliversAfterOwnedProcessRestartWithoutRPC(t *testing.T) {
	f := newFixture(t)
	m := f.manager(t, true)
	delivered := make(chan AttentionRecord, 8)
	m.SetDeliveryTransport(func(ctx context.Context, r AttentionRecord) error { delivered <- r; return nil }, func() bool { return true })
	if err := m.SetDelivery(true); err != nil {
		t.Fatal(err)
	}
	var connections atomic.Int32
	f.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/ws" {
			f.serveHTTP(w, r)
			return
		}
		c, err := (&websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		n := connections.Add(1)
		c.WriteJSON(map[string]any{"method": "event", "params": map[string]any{"type": "gateway.ready"}})
		var frame rpcFrame
		if c.ReadJSON(&frame) != nil {
			return
		}
		if frame.Method != "client.capabilities" {
			return
		}
		c.WriteJSON(map[string]any{"id": frame.ID, "result": map[string]bool{"ok": true}})
		epoch := m.Status().BackendGeneration
		m.control.mu.Lock()
		m.control.Tasks = append(m.control.Tasks, TaskRecord{RunID: fmt.Sprintf("fixture-turn-%d", n), SessionID: "fixture-background", Generation: epoch, State: "accepted"})
		m.control.mu.Unlock()
		for _, kind := range []string{"message.start", "message.complete"} {
			c.WriteJSON(map[string]any{"method": "event", "params": map[string]any{"session_id": "fixture-background", "type": kind, "payload": map[string]any{"status": "completed"}}})
		}
		c.WriteJSON(map[string]any{"id": fmt.Sprintf("fixture-question-%d", n), "method": "clarify", "params": map[string]any{"session_id": "fixture-question", "questions": []any{map[string]string{"qid": "one", "question": "Fixture only"}}}})
		for {
			if c.ReadJSON(&frame) != nil {
				return
			}
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := m.SetAutoStart(true); err != nil {
		t.Fatal(err)
	}
	m.StartMaintenance(ctx)
	for round := 0; round < 2; round++ {
		seen := map[string]bool{}
		deadline := time.NewTimer(9 * time.Second)
		for !seen["question"] || !seen["completed"] {
			select {
			case r := <-delivered:
				seen[r.Kind] = true
			case <-deadline.C:
				t.Fatalf("round %d background delivery missing: %v", round, seen)
			}
		}
		deadline.Stop()
		var journal struct {
			Tasks     []TaskRecord
			Attention []AttentionRecord
		}
		json.Unmarshal(m.ControlSnapshot(), &journal)
		if len(journal.Tasks) != round+1 || journal.Tasks[round].State != "completed" || journal.Tasks[round].NativeTurnSeq == 0 {
			t.Fatalf("background native turn missing: %+v", journal.Tasks)
		}
		if round == 0 {
			m.mu.Lock()
			p := m.process
			m.mu.Unlock()
			if p == nil {
				t.Fatal("no owned fixture process")
			}
			p.cancel()
		}
	}
	if connections.Load() != 2 {
		t.Fatalf("duplicate or missing generation subscriptions: %d", connections.Load())
	}
	if err := m.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	time.Sleep(350 * time.Millisecond)
	if connections.Load() != 2 || m.Status().Running {
		t.Fatal("observer reconnected after intentional stop")
	}
}
