package hermes

import (
	"context"
	"testing"
	"time"
)

func TestControlSupervisorDefaultCrashRecovery(t *testing.T) {
	f := newFixture(t)
	m := f.manager(t, true)
	if !m.Status().AutoStart {
		t.Fatal("default unattended startup disabled")
	}
	life, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.StartMaintenance(life)
	deadline := time.Now().Add(4 * time.Second)
	for !m.Status().Ready && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !m.Status().Ready {
		t.Fatal("enabled manager did not start unattended")
	}
	before := m.Status().BackendGeneration
	m.mu.Lock()
	process := m.process
	m.mu.Unlock()
	if err := process.cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if m.Status().Ready && m.Status().BackendGeneration > before {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !m.Status().Ready || m.Status().BackendGeneration <= before {
		t.Fatal("fixture crash not recovered")
	}
	if err := m.SetAutoStart(false); err != nil {
		t.Fatal(err)
	}
	if err := m.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1200 * time.Millisecond)
	if m.Status().Running {
		t.Fatal("disabled manager restarted")
	}
}
