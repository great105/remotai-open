package hermes

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestNativeNormalizedDuplicateRejectedBeforeAdmission(t *testing.T) {
	for _, boundary := range []string{"", " \t\n", "\u001c\u001d\u001e\u001f", "\u0085\u00a0\u2007\u202f\u3000"} {
		t.Run(boundary, func(t *testing.T) {
			f := newFixture(t)
			m := f.manager(t, true)
			if err := m.Start(context.Background()); err != nil {
				t.Fatal(err)
			}
			if _, err := m.RPC(context.Background(), "session.create", nil); err != nil {
				t.Fatal(err)
			}
			dir := filepath.Join(m.checkout, "apps", "shared", "src")
			os.MkdirAll(dir, 0700)
			os.WriteFile(filepath.Join(dir, "gateway-contract.openrpc.json"), []byte(`{"methods":[{"name":"prompt.submit"},{"name":"gateway.capabilities"}]}`), 0600)
			req := func(id, text string, q bool) json.RawMessage {
				b, _ := json.Marshal(map[string]any{"client_request_id": id, "session_id": "fixture-session", "text": text, "queued": q})
				return b
			}
			if _, err := m.SubmitTask(context.Background(), req("first", boundary+"same"+boundary, false)); err != nil {
				t.Fatal(err)
			}
			if _, err := m.SubmitTask(context.Background(), req("duplicate", "same", true)); err == nil {
				t.Fatal("native self duplicate accepted")
			} else if refusal, ok := err.(interface{ AdmissionRejected() bool }); !ok || !refusal.AdmissionRejected() {
				t.Fatalf("duplicate is not a safe typed refusal: %T", err)
			}
			m.control.mu.Lock()
			n := len(m.control.Tasks)
			m.control.mu.Unlock()
			if n != 1 {
				t.Fatal("refused duplicate persisted")
			}
			m.control.mu.Lock()
			hash := m.control.Tasks[0].NativeTextHash
			m.control.Tasks[0].NativeTextHash = ""
			m.control.mu.Unlock()
			if _, err := m.SubmitTask(context.Background(), req("legacy-unknown", "different legacy", true)); err == nil {
				t.Fatal("legacy unknown active content admitted")
			}
			m.control.mu.Lock()
			m.control.Tasks[0].NativeTextHash = hash
			m.control.mu.Unlock()
			if _, err := m.SubmitTask(context.Background(), req("different", "other", true)); err != nil {
				t.Fatal(err)
			}
			deadline := time.Now().Add(time.Second)
			for {
				f.mu.Lock()
				n = len(f.promptTexts)
				f.mu.Unlock()
				if n == 2 {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("different text not delivered")
				}
				time.Sleep(time.Millisecond)
			}
		})
	}
}
