package hermes

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestControlAdmissionRejectsQueueWithoutNativeExclusivity(t *testing.T) {
	f := newFixture(t)
	m := f.manager(t, true)
	if err := m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := m.SubmitTask(context.Background(), json.RawMessage(`{"client_request_id":"unsafe-queue","session_id":"fixture-session","text":"fixture only","queued":true}`)); err == nil {
		t.Fatal("old runtime admitted queue without exclusive submit capability")
	}
	var s struct{ Tasks []TaskRecord }
	json.Unmarshal(m.ControlSnapshot(), &s)
	if len(s.Tasks) != 0 {
		t.Fatal("unsupported queue left admitted work")
	}
}
func TestControlIntentRuntimeContractGate(t *testing.T) {
	f := newFixture(t)
	m := f.manager(t, true)
	if err := m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	runtime, ok := any(m).(interface {
		ControlIntent(context.Context, json.RawMessage) (json.RawMessage, error)
	})
	if !ok {
		t.Fatal("native intent negotiation unavailable")
	}
	req := json.RawMessage(`{"intent":"steer","session_id":"fixture-session","text":"fixture correction"}`)
	if _, err := runtime.ControlIntent(context.Background(), req); err == nil {
		t.Fatal("old runtime admitted unsupported steer")
	}
	dir := filepath.Join(m.checkout, "apps", "shared", "src")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "gateway-contract.openrpc.json"), []byte(`{"methods":[{"name":"session.steer"},{"name":"session.interrupt"},{"name":"prompt.submit"}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	var caps struct {
		Queue bool `json:"queue"`
	}
	json.Unmarshal(m.ControlCapabilities(), &caps)
	if caps.Queue {
		t.Fatal("method presence alone enabled exclusive queue")
	}
	if err := os.WriteFile(filepath.Join(dir, "gateway-contract.openrpc.json"), []byte(`{"methods":[{"name":"session.steer"},{"name":"session.interrupt"},{"name":"prompt.submit"},{"name":"gateway.capabilities"}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	json.Unmarshal(m.ControlCapabilities(), &caps)
	if !caps.Queue {
		t.Fatal("native exclusive submit capability not negotiated")
	}
	if _, err := runtime.ControlIntent(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.ControlIntent(context.Background(), json.RawMessage(`{"intent":"redirect","session_id":"fixture-session","text":"no"}`)); err == nil {
		t.Fatal("unrestricted correction allowed")
	}
}
