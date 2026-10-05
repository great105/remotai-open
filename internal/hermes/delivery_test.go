package hermes

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"
)

func TestControlPrivateDeliveryOptInAndDedup(t *testing.T) {
	f := newFixture(t)
	m := f.manager(t, true)
	var mu sync.Mutex
	ids := []string{}
	setter, ok := any(m).(interface{ SetDelivery(bool) error })
	if !ok {
		t.Fatal("background delivery adapter unavailable")
	}
	// Fixture transport is installed without owner accounts or any public request.
	adapter, ok := any(m).(interface {
		SetDeliveryTransport(func(context.Context, AttentionRecord) error, func() bool)
	})
	if !ok {
		t.Fatal("fixture delivery transport unavailable")
	}
	adapter.SetDeliveryTransport(func(_ context.Context, r AttentionRecord) error {
		mu.Lock()
		defer mu.Unlock()
		if len(r.Params) != 0 {
			t.Error("notification leaked request content")
		}
		ids = append(ids, r.ID)
		return nil
	}, func() bool { return true })
	if err := m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	life, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.StartMaintenance(life)
	_, err := m.RPC(context.Background(), "fixture.approval", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	n := len(ids)
	mu.Unlock()
	if n != 0 {
		t.Fatal("delivered before opt-in")
	}
	if err := setter.SetDelivery(true); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n = len(ids)
		mu.Unlock()
		if n == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if n != 1 {
		t.Fatal("background notification not delivered")
	}
	time.Sleep(1200 * time.Millisecond)
	mu.Lock()
	n = len(ids)
	mu.Unlock()
	if n != 1 {
		t.Fatal("duplicate delivery")
	}
	var snapshot map[string]any
	_ = json.Unmarshal(control(t, m).ControlSnapshot(), &snapshot)
}
