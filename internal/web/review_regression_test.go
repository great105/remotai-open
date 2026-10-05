package web

import (
	"encoding/json"
	"testing"
)

func TestReviewRegressionLegacyPromptBypassesAdmission(t *testing.T) {
	if hermesRPCAllowed("prompt.submit", json.RawMessage(`{"session_id":"live","text":"execute this twice","queued":true}`)) {
		t.Fatal("legacy prompt.submit allowed without client_request_id, generation, durable admission or native capability gate")
	}
}
