package web

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func (f *controlWebFixture) ReadArtifact(id string) ([]byte, string, error) {
	if id != "verified-fixture" {
		return nil, "", errors.New("missing")
	}
	return []byte("fixture artifact"), "report.txt", nil
}
func TestHermesControlHTTPRejectsUnknownKeys(t *testing.T) {
	f := &controlWebFixture{}
	s := &Server{hermesManagers: map[int64]hermesRuntime{1: f}}
	for _, action := range []string{"read", "settings", "reply"} {
		t.Run(action, func(t *testing.T) {
			body := `{"id":"notice","profile":"foreign"}`
			if action == "settings" {
				body = `{"auto_start":true,"profile":"foreign"}`
			}
			if action == "reply" {
				body = `{"id":"notice","result":{"value":"fixture"},"profile":"foreign"}`
			}
			r := httptest.NewRequest(http.MethodPost, "/api/hermes/"+action, strings.NewReader(body))
			r.SetPathValue("action", action)
			w := httptest.NewRecorder()
			switch action {
			case "settings":
				s.apiHermesSettings(w, r, 1)
			case "reply":
				s.apiHermesReply(w, r, 1)
			default:
				s.apiHermesControl(w, r, 1)
			}
			if w.Code != 400 {
				t.Fatalf("unknown HTTP key accepted: %d", w.Code)
			}
		})
	}
}
func TestHermesArtifactJSONIsNotCacheable(t *testing.T) {
	f := &controlWebFixture{}
	s := &Server{hermesManagers: map[int64]hermesRuntime{1: f}}
	r := httptest.NewRequest(http.MethodGet, "/api/hermes/artifact?id=verified-fixture&format=json", nil)
	w := httptest.NewRecorder()
	s.apiHermesArtifact(w, r, 1)
	if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("private artifact JSON can be cached")
	}
}
