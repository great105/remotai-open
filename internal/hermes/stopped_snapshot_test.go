package hermes

import (
	"context"
	"encoding/json"
	"testing"
)

func TestControlStoppedSnapshotDoesNotReportActiveTask(t *testing.T) {
	f := newFixture(t)
	m := f.manager(t, true)
	if err := m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	m.control.mu.Lock()
	m.control.Tasks = append(m.control.Tasks, TaskRecord{RunID: "stopped", SessionID: "fixture-session", State: "running", Generation: m.Status().BackendGeneration})
	m.control.mu.Unlock()
	if err := m.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	var s struct{ Tasks []TaskRecord }
	json.Unmarshal(m.ControlSnapshot(), &s)
	if s.Tasks[0].State != "interrupted" {
		t.Fatal("stopped task still reported active")
	}
}
