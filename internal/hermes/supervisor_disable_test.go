package hermes

import (
	"context"
	"os/exec"
	"testing"
	"time"
)

func TestControlSupervisorCannotLaunchAfterDisableDuringSetup(t *testing.T) {
	f := newFixture(t)
	m := f.manager(t, true)
	original := m.opts.command
	entered, release := make(chan struct{}), make(chan struct{})
	m.opts.command = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		for _, arg := range args {
			if arg == "serve" {
				close(entered)
				<-release
				break
			}
		}
		return original(ctx, name, args...)
	}
	if err := m.SetAutoStart(true); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.StartMaintenance(ctx)
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("supervisor did not enter fixture setup")
	}
	if err := m.SetAutoStart(false); err != nil {
		t.Fatal(err)
	}
	close(release)
	deadline := time.Now().Add(4 * time.Second)
	for m.Status().Operation != "" && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if m.Status().Running {
		t.Fatal("disabled supervisor launched a stale attempt")
	}
}
