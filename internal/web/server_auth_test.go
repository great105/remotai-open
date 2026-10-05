package web

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

func TestNewServerInitializesAuthManagerWithAPITokenOnly(t *testing.T) {
	const childEnv = "REMOTAI_TEST_AUTH_MANAGER_CHILD"
	if os.Getenv(childEnv) == "1" {
		server := NewServer(nil, nil, "", nil)
		if server.authManager == nil {
			t.Fatal("auth manager should be initialized when api_token is configured")
		}
		return
	}

	home := t.TempDir()
	t.Setenv("USERPROFILE", home)
	t.Setenv("HOME", home)

	configDir := filepath.Join(home, ".config", "remotai")
	if runtime.GOOS == "windows" {
		configDir = filepath.Join(home, "AppData", "Local", "Remotai")
	}
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatal(err)
	}
	configJSON := `{
		"mode": "central_bot",
		"setup_complete": true,
		"web_port": "18080",
		"api_token": "test-api-token",
		"api_token_uid": 12345
	}`
	if err := os.WriteFile(filepath.Join(configDir, "config.json"), []byte(configJSON), 0o600); err != nil {
		t.Fatal(err)
	}

	// paths.Base and config.GetNoSetup are process singletons. Start the exact
	// test in a fresh process so HOME/USERPROFILE are effective before either
	// singleton can be initialized by another web-package test.
	t.Setenv(childEnv, "1")
	cmd := exec.Command(os.Args[0], "-test.run=^TestNewServerInitializesAuthManagerWithAPITokenOnly$", "-test.count=1")
	cmd.Env = os.Environ()
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("isolated auth-manager check failed: %v\n%s", err, output)
	}
}
