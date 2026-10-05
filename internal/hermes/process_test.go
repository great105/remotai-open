package hermes

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestEnvironmentOnlyOwnedBackendEnablesDesktopTicker(t *testing.T) {
	t.Setenv("HERMES_DESKTOP", "1")
	t.Setenv("HERMES_DASHBOARD_SESSION_TOKEN", "inherited-token")
	t.Setenv("HERMES_PARENT_PID", "inherited-parent")
	t.Setenv("PYTHONUNBUFFERED", "0")
	t.Setenv("PYTHONUTF8", "0")
	root := t.TempDir()
	m := &Manager{root: root, home: filepath.Join(root, "data")}
	for _, test := range []struct {
		name    string
		install bool
		token   string
	}{
		{"metadata", false, ""},
		{"installer", true, ""},
		{"owned backend", false, "owned-fixture-token"},
	} {
		t.Run(test.name, func(t *testing.T) {
			values := map[string]string{}
			for _, pair := range m.environment(test.install, test.token) {
				key, value, _ := strings.Cut(pair, "=")
				if _, duplicate := values[key]; duplicate {
					t.Fatalf("duplicate environment key: %s", key)
				}
				values[key] = value
			}
			if test.token == "" {
				for _, key := range []string{"HERMES_DESKTOP", "HERMES_DASHBOARD_SESSION_TOKEN", "HERMES_PARENT_PID"} {
					if _, exists := values[key]; exists {
						t.Fatalf("inherited backend identity reached %s subprocess: %s", test.name, key)
					}
				}
			} else if values["HERMES_DESKTOP"] != "1" || values["HERMES_DASHBOARD_SESSION_TOKEN"] != test.token || values["HERMES_PARENT_PID"] != strconv.Itoa(os.Getpid()) {
				t.Fatal("owned backend did not receive its authenticated desktop identity")
			}
			if values["HERMES_HOME"] != m.home {
				t.Fatal("dedicated data home was not preserved")
			}
		})
	}
}
