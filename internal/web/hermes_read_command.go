package web

import (
	"encoding/json"
	"strings"
)

// Only unconditional live read/display handlers. In particular /review,
// /refine, /compress, skills, quick commands and worker fallbacks are not safe.
func hermesReadCommand(method string, scope map[string]json.RawMessage) (string, bool) {
	if scope == nil {
		return "", false
	}
	var command string
	for key, raw := range scope {
		switch key {
		case "session_id", "profile":
		case "name":
			if method != "command.dispatch" || json.Unmarshal(raw, &command) != nil {
				return "", false
			}
		case "arg":
			var arg string
			if method != "command.dispatch" || json.Unmarshal(raw, &arg) != nil || arg != "" {
				return "", false
			}
		case "command":
			if method != "slash.exec" || json.Unmarshal(raw, &command) != nil {
				return "", false
			}
		default:
			return "", false
		}
	}
	command = strings.TrimPrefix(command, "/")
	switch command {
	case "status", "usage", "history", "clear", "models", "rename", "effort", "model":
		return command, true
	}
	return "", false
}
