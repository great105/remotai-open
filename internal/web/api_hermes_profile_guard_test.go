package web

import (
	"encoding/json"
	"testing"
)

func TestHermesNativeRPCAllowsOwnedDefaultProfile(t *testing.T) {
	for _, method := range []string{"session.create", "session.resume", "session.activate", "session.list", "session.interrupt", "session.cwd.set", "session.history", "commands.catalog", "model.options", "model.save_key", "setup.runtime_check"} {
		if !hermesRPCAllowed(method, json.RawMessage(`{"profile":"default"}`)) {
			t.Errorf("%s rejects owned default profile", method)
		}
	}
}

func TestHermesNativeRPCRejectsForeignProfileForEveryAllowedMethod(t *testing.T) {
	for _, method := range []string{"session.create", "session.resume", "session.activate", "session.list", "session.interrupt", "session.cwd.set", "session.history", "prompt.submit", "tools.show", "commands.catalog", "command.dispatch", "slash.exec", "config.set", "model.options", "model.save_key", "setup.runtime_check"} {
		params := json.RawMessage(`{"session_id":"fixture-session","profile":"foreign","key":"model","text":"fixture"}`)
		if hermesRPCAllowed(method, params) {
			t.Errorf("%s permits foreign profile override", method)
		}
	}
}
