package hermes

import (
	"context"
	"encoding/json"
	"testing"
)

func TestControlResultContradictoryFailureNotVerified(t *testing.T) {
	f := newFixture(t)
	m := f.manager(t, true)
	if err := m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	m.control.mu.Lock()
	m.control.Tasks = append(m.control.Tasks, TaskRecord{RunID: "failed-tool", SessionID: "fixture-session", Generation: m.Status().BackendGeneration, State: "running"})
	m.control.mu.Unlock()
	m.addEvent([]byte(`{"method":"event","params":{"session_id":"fixture-session","type":"tool.complete","payload":{"tool_id":"contradiction","name":"terminal","result":{"exit_code":0,"error":"fixture error","success":false}}}}`))
	var s struct{ Results []ResultRecord }
	json.Unmarshal(m.ControlSnapshot(), &s)
	if s.Results[0].Verified && s.Results[0].Outcome == "completed" {
		t.Fatal("contradictory failed tool declared verified completion")
	}
}
