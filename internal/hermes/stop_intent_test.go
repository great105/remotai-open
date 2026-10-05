package hermes

import (
	"context"
	"testing"
	"time"
)

func TestControlExplicitStopDisablesUnattendedRestart(t *testing.T) {
	f := newFixture(t)
	m := f.manager(t, true)
	if err := m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := m.SetAutoStart(true); err != nil {
		t.Fatal(err)
	}
	life, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.StartMaintenance(life)
	if err := m.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if m.Status().AutoStart {
		t.Fatal("explicit stop leaves unattended restart enabled")
	}
	time.Sleep(1200 * time.Millisecond)
	if m.Status().Running {
		t.Fatal("explicitly stopped fixture restarted")
	}
}
