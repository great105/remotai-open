//go:build linux || darwin

package desktopinstall

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
)

func TestWindowURL(t *testing.T) {
	for _, tc := range []struct {
		name, status, access, suffix string
		wantAccess                   bool
	}{
		{"first launch", `{"version":"2.66.9","device_id":"local","configured":false}`, ``, "/setup", false},
		{"configured", `{"version":"2.66.9","device_id":"local","configured":true}`, `{"token":"a+b&c"}`, "/miniapp?token=a%2Bb%26c", true},
		{"foreign local server", `{"configured":true}`, ``, "", false},
		{"missing identity", `{"version":"2.66.9","configured":true}`, ``, "", false},
		{"invalid JSON", `<html>server</html>`, ``, "", false},
		{"access not ready", `{"version":"2.66.9","device_id":"local","configured":true}`, `{}`, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			accessed := false
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					t.Errorf("unexpected mutation: %s", r.Method)
				}
				switch r.URL.Path {
				case "/api/setup/status":
					fmt.Fprint(w, tc.status)
				case "/api/setup/local-access":
					accessed = true
					fmt.Fprint(w, tc.access)
				default:
					t.Errorf("unexpected URL: %s", r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer srv.Close()
			u, _ := url.Parse(srv.URL)
			port, _ := strconv.Atoi(u.Port())
			got, err := WindowURL(context.Background(), port)
			if tc.suffix == "" {
				if err == nil || got != "" {
					t.Fatalf("foreign/unready agent accepted")
				}
			} else if err != nil || got != "http://localhost:"+u.Port()+tc.suffix {
				t.Fatalf("wrong window URL or error: %v", err)
			}
			if accessed != tc.wantAccess {
				t.Errorf("local access requested=%t, want=%t", accessed, tc.wantAccess)
			}
		})
	}
}

func TestWindowURLDoesNotFollowRedirect(t *testing.T) {
	called := false
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true }))
	defer target.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, 302) }))
	defer srv.Close()
	port, _ := strconv.Atoi(srv.URL[strings.LastIndex(srv.URL, ":")+1:])
	if _, err := WindowURL(context.Background(), port); err == nil || called {
		t.Fatal("redirect escaped loopback agent check")
	}
}
