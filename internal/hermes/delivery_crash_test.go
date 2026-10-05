package hermes

import (
	"context"
	"encoding/json"
	"testing"
)

func TestControlCrashMakesInflightDeliveryUncertainWithoutReplay(t *testing.T) {
	f := newFixture(t)
	m := f.manager(t, true)
	m.control.mu.Lock()
	m.control.Attention = append(m.control.Attention, AttentionRecord{ID: "delivery-crash", State: "unread", Delivery: "sending"})
	if err := m.saveControlLocked(); err != nil {
		t.Fatal(err)
	}
	m.control.mu.Unlock()
	if err := m.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	fresh, err := New(m.opts)
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close(context.Background())
	var s struct{ Attention []AttentionRecord }
	json.Unmarshal(fresh.ControlSnapshot(), &s)
	if s.Attention[0].Delivery != "uncertain" {
		t.Fatal("crashed delivery incorrectly remains in progress")
	}
}
