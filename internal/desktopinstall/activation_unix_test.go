//go:build linux || darwin

package desktopinstall

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
)

func legacyExecutable(t *testing.T, path, release string) {
	t.Helper()
	// Old Remotai accepts --version only. Every other argument could launch
	// the application; make that a failure rather than running a real agent.
	if err := os.WriteFile(path, []byte(fmt.Sprintf("#!/bin/sh\n[ \"$1\" = --version ] || exit 42\nprintf 'Remotai v%s (test)\\n'\n", release)), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestInstallUpgradesLegacyExecutableWithoutLaunchingIt(t *testing.T) {
	source, home, target := desktopFixture(t)
	os.Chmod(source, 0o755)
	legacyExecutable(t, source, "2.66.8")
	legacyExecutable(t, target, "2.55.18")
	if _, err := Install(context.Background(), source, home, "2.66.8"); err != nil {
		t.Fatal(err)
	}
	if got, err := executableVersion(context.Background(), target); err != nil || got != "2.66.8" {
		t.Fatalf("installed version=%q error=%v", got, err)
	}
}

func TestPendingActivationRecognizesOldRunningAgent(t *testing.T) {
	exe := filepath.Join(t.TempDir(), "remotai")
	legacyExecutable(t, exe, "2.66.8")
	for _, tc := range []struct {
		name string
		body string
		code int
		want bool
	}{
		{"old running version", `{"version":"2.55.18","device_id":"qa"}`, 200, true},
		{"same version", `{"version":"2.66.8","device_id":"qa"}`, 200, false},
		{"newer running version", `{"version":"2.67.0","device_id":"qa"}`, 200, false},
		{"development version", `{"version":"dev","device_id":"qa"}`, 200, false},
		{"another app", `{"version":"2.55.18"}`, 200, false},
		{"SPA fallback", `<html>local dev server</html>`, 200, false},
		{"HTTP failure", `{"version":"2.55.18","device_id":"qa"}`, 503, false},
		{"redirect", ``, 302, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var requests atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if r.Method != http.MethodGet || r.URL.Path != "/api/setup/status" {
					t.Errorf("activation check must not mutate the running agent: %s %s", r.Method, r.URL.Path)
				}
				w.Header().Set("Location", "/must-not-follow")
				w.WriteHeader(tc.code)
				fmt.Fprint(w, tc.body)
			}))
			defer srv.Close()
			_, portText, _ := net.SplitHostPort(srv.Listener.Addr().String())
			port, _ := strconv.Atoi(portText)
			got := PendingActivation(context.Background(), exe, port)
			if (got != nil) != tc.want {
				t.Fatalf("notice=%+v want=%v", got, tc.want)
			}
			if requests.Load() != 1 {
				t.Fatalf("unexpected requests: %d", requests.Load())
			}
			if got != nil && (got.RunningVersion != "2.55.18" || got.InstalledVersion != "2.66.8" || got.PanelURL != srv.URL+"/setup?force=1") {
				t.Fatalf("wrong recovery panel or version: %+v", got)
			}
		})
	}
}
