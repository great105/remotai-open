package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"tgcontrol-relay/internal/config"
)

func TestHealthReportsBuildVersion(t *testing.T) {
	previousVersion := Version
	Version = "release-test"
	t.Cleanup(func() { Version = previousVersion })

	d := initSQLite(t)
	t.Cleanup(func() { _ = d.Close() })
	srv := New(&config.Config{
		JWTSecret: "test-secret-please-change-1234567890",
		JWTTTL:    time.Hour,
	}, d)

	rec := httptest.NewRecorder()
	srv.handleHealth(rec, httptest.NewRequest(http.MethodGet, "/health", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("health status = %d, want %d", rec.Code, http.StatusOK)
	}
	var got healthResponse
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode health: %v", err)
	}
	if !got.OK || !got.DBOk {
		t.Fatalf("health not ready: %+v", got)
	}
	if got.Version != Version {
		t.Fatalf("health version = %q, want %q", got.Version, Version)
	}
}
