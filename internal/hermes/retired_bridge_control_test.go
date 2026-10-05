package hermes

import (
	"context"
	"encoding/json"
	"testing"
)

func TestControlRetiredBridgeCannotPublishNewGenerationRequest(t *testing.T) {
	f := newFixture(t)
	m := f.manager(t, true)
	if err := m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := m.RPC(context.Background(), "ping", nil); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	old := m.bridge
	m.bridge = nil
	m.epoch++
	m.mu.Unlock()
	defer old.close()
	if _, err := old.call(context.Background(), "fixture.approval", nil); err != nil {
		t.Fatal(err)
	}
	var s struct{ Attention []AttentionRecord }
	json.Unmarshal(m.ControlSnapshot(), &s)
	if len(s.Attention) != 0 {
		t.Fatal("retired socket request rebound to current generation")
	}
}
