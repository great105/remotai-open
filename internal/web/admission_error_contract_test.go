package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"tgcontrol/internal/hermes"
)

type admissionErrorWebFixture struct {
	controlWebFixture
	err error
}

func (f *admissionErrorWebFixture) SubmitTask(context.Context, json.RawMessage) (json.RawMessage, error) {
	return nil, f.err
}
func TestDefinitiveAdmissionHTTPErrorContract(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		code string
	}{{"definitive", &hermes.AdmissionRejection{Reason: "fixture queue full"}, "hermes_admission_rejected"}, {"unknown", errors.New("fixture queue full"), "hermes_control_failed"}} {
		t.Run(tc.name, func(t *testing.T) {
			f := &admissionErrorWebFixture{err: tc.err}
			s := &Server{hermesManagers: map[int64]hermesRuntime{1: f}}
			w := httptest.NewRecorder()
			r := httptest.NewRequest("POST", "/api/hermes/control/submit", strings.NewReader(`{"client_request_id":"fixture-id","session_id":"s","text":"fixture"}`))
			r.SetPathValue("action", "submit")
			s.apiHermesControl(w, r, 1)
			var body map[string]any
			json.Unmarshal(w.Body.Bytes(), &body)
			if w.Code != 409 || body["code"] != tc.code {
				t.Fatalf("wrong safe-release contract: %d %s", w.Code, w.Body.String())
			}
			if tc.name == "definitive" {
				if path := os.Getenv("HERMES_REFUSAL_FIXTURE"); path != "" {
					if err := os.WriteFile(path, w.Body.Bytes(), 0600); err != nil {
						t.Fatal(err)
					}
				}
			}
		})
	}
}
