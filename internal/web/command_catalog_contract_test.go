package web

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestCommandCatalogReportsAuthoritativeHTTPAvailability(t *testing.T) {
	f := &hermesWebFixture{rpc: func(context.Context, string, any) (json.RawMessage, error) {
		return json.RawMessage(`{"pairs":[["status","Status"],["skills","Skills"],["context","Context"],["plugins","Plugins"],["model","Model"]],"remotai_commands":{"version":1,"without_arguments":["skills","plugins","context"]}}`), nil
	}}
	s := &Server{hermesManagers: map[int64]hermesRuntime{1: f}}
	w := httptest.NewRecorder()
	s.apiHermesRPC(w, httptest.NewRequest("POST", "/api/hermes/rpc", strings.NewReader(`{"method":"commands.catalog","params":{"profile":"default"}}`)), 1)
	var body struct {
		Pairs        [][2]string `json:"pairs"`
		Availability struct {
			Version int      `json:"version"`
			Names   []string `json:"without_arguments"`
		} `json:"remotai_commands"`
	}
	if json.Unmarshal(w.Body.Bytes(), &body) != nil || w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if body.Availability.Version != 1 {
		t.Fatal("HTTP capabilities missing")
	}
	allowed := map[string]bool{}
	for _, name := range body.Availability.Names {
		allowed[name] = true
	}
	if !allowed["status"] || !allowed["model"] || allowed["skills"] || allowed["plugins"] || allowed["context"] {
		t.Fatalf("catalog advertises bypass handlers: %s", w.Body.String())
	}
	if len(body.Pairs) != 5 {
		t.Fatal("native catalog lost")
	}
	if path := os.Getenv("HERMES_CATALOG_FIXTURE"); path != "" {
		os.WriteFile(path, w.Body.Bytes(), 0600)
	}
}
