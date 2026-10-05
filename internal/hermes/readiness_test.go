package hermes

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestControlReadinessUsesRuntimeToolsNotFakeCatalog(t *testing.T) {
	f := newFixture(t)
	m := f.manager(t, true)
	if err := m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	reader, ok := any(m).(interface {
		ControlReadiness(context.Context, json.RawMessage) (json.RawMessage, error)
	})
	if !ok {
		t.Fatal("runtime readiness unavailable")
	}
	req := json.RawMessage(`{"session_id":"fixture-session"}`)
	raw, err := reader.ControlReadiness(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	var s struct {
		Tools struct {
			Status string
			Total  int
		}
		ModelQuota string `json:"model_quota"`
	}
	json.Unmarshal(raw, &s)
	if s.Tools.Status != "unsupported" || s.ModelQuota != "unknown" {
		t.Fatalf("invented readiness: %s", raw)
	}
	dir := filepath.Join(m.checkout, "apps", "shared", "src")
	os.MkdirAll(dir, 0700)
	os.WriteFile(filepath.Join(dir, "gateway-contract.openrpc.json"), []byte(`{"methods":[{"name":"tools.show"}]}`), 0600)
	raw, err = reader.ControlReadiness(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	json.Unmarshal(raw, &s)
	if s.Tools.Status != "ready" || s.Tools.Total != 1 {
		t.Fatalf("native catalog unused: %s", raw)
	}
}
