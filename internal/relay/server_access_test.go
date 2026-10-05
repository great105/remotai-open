package relay

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRequestServerAccessRequiresFreshMatchingDecision(t *testing.T) {
	for _, tc := range []struct {
		name, body       string
		status           int
		allowed, wantErr bool
		code             string
	}{
		{"paid", `{"allowed":true,"tier":"pro","feature":"servers","device_id":"pc"}`, 200, true, false, ""},
		{"expired", `{"allowed":false,"tier":"free","feature":"servers","device_id":"pc","code":"server_subscription_required"}`, 200, false, false, "server_subscription_required"},
		{"wrong device", `{"allowed":true,"tier":"pro","feature":"servers","device_id":"other"}`, 200, false, true, ""},
		{"wrong feature", `{"allowed":true,"tier":"pro","feature":"local","device_id":"pc"}`, 200, false, true, ""},
		{"old relay", `not found`, 404, false, true, ""},
		{"unavailable", `failure containing a-private-token`, 503, false, true, ""},
		{"html fallback", `<html>index</html>`, 200, false, true, ""},
		{"revoked", `{}`, 401, false, false, "server_account_required"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				wantMethod := "GET"
				if calls == 2 {
					wantMethod = "POST"
				}
				if r.Method != wantMethod || r.URL.Path != "/v1/agent/server-access" || r.Header.Get("Authorization") != "Bearer a-private-token" {
					t.Errorf("wrong request method, path or authorization")
				}
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			}))
			defer srv.Close()
			for _, startTrial := range []bool{false, true} {
				access, err := requestServerAccess(t.Context(), srv.Client(), srv.URL, "pc", "a-private-token", startTrial)
				if (err != nil) != tc.wantErr || access.Allowed != tc.allowed || access.Code != tc.code {
					t.Fatalf("access=%+v err=%v", access, err)
				}
				if err != nil && strings.Contains(err.Error(), "a-private-token") {
					t.Fatal("error exposed credentials")
				}
			}
			if calls != 2 {
				t.Fatal("decision was cached")
			}
		})
	}
}

func TestRequestServerAccessCancellationFailsClosed(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	access, err := requestServerAccess(ctx, http.DefaultClient, "http://127.0.0.1:1", "pc", "test", true)
	if err == nil || access.Allowed {
		t.Fatal("unavailable service granted access")
	}
}
