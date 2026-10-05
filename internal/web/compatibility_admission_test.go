package web

import (
	"encoding/json"
	"testing"
)

func TestCompatibilityCannotLaunchPromptOrCommandWork(t *testing.T) {
	for _, entry := range []struct{ method, params string }{
		{"prompt.submit", `{"text":"work","queued":true}`},
		{"command.dispatch", `{"name":"some-skill","arg":"work"}`},
		{"command.dispatch", `{"name":"goal","arg":"start work"}`},
		{"slash.exec", `{"command":"/refine work"}`},
		{"slash.exec", `{"command":"/review"}`},
		{"slash.exec", `{"command":"/prompt work"}`},
		{"slash.exec", `{"command":"/status","arg":"hidden work"}`},
	} {
		if hermesRPCAllowed(entry.method, json.RawMessage(entry.params)) {
			t.Errorf("work bypass: %s %s", entry.method, entry.params)
		}
	}
}

func TestCompatibilityPreservesNativeReadCommands(t *testing.T) {
	for _, entry := range []struct{ method, params string }{
		{"commands.catalog", `{}`},
		{"slash.exec", `{"command":"/status","session_id":"s"}`},
		{"slash.exec", `{"command":"/usage","session_id":"s"}`},
		{"command.dispatch", `{"name":"status","arg":"","session_id":"s"}`},
	} {
		if !hermesRPCAllowed(entry.method, json.RawMessage(entry.params)) {
			t.Errorf("read command refused: %s", entry.params)
		}
	}
}
