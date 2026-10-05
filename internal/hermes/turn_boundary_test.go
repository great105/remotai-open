package hermes

import "testing"

func TestTerminalFrameCannotCompleteUnstartedNextAdmission(t *testing.T) {
	m := regressionManager(t)
	m.control.Tasks = []TaskRecord{{RunID: "active", SessionID: "s", Generation: 7, State: "running"}, {RunID: "next", SessionID: "s", Generation: 7, State: "accepted", NativeStatus: "queued", Queued: true}}
	raw := []byte(`{"method":"event","params":{"type":"message.complete","session_id":"s","payload":{"status":"completed"}}}`)
	m.observeControl(raw, 1, 7)
	m.observeControl(raw, 2, 7)
	if m.control.Tasks[0].State != "completed" || m.control.Tasks[1].State != "accepted" {
		t.Fatalf("terminal frame completed a turn with no native start: %+v", m.control.Tasks)
	}
}
