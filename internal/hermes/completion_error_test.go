package hermes

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"tgcontrol/internal/procutil"
)

func TestCompletionDetailsNeverExposeUnknownFieldsOrInstallerOutput(t *testing.T) {
	for _, report := range []string{
		`{"steps":["build"],"kind":"network","reason":"FIXTURE_SECRET"}`,
		`{"steps":["maintenance","build"],"kind":"disk","reason":"FIXTURE_SECRET"}`,
		`{"steps":["FIXTURE_SECRET"],"kind":"network"}`,
		`{"steps":["build"],"kind":"FIXTURE_SECRET"}`,
		`{"steps":["build"`,
	} {
		detail := completionFailureDetail([]byte("FIXTURE_SECRET\nREMOTAI_HERMES_COMPLETE " + report))
		if strings.Contains(detail, "FIXTURE_SECRET") {
			t.Fatal("private completion output escaped into the public error")
		}
		if strings.Contains(report, `"kind":"network","reason"`) && !strings.Contains(detail, "серверы загрузки") {
			t.Fatal("a bounded network failure was hidden behind the exit code")
		}
		if strings.Contains(report, `"kind":"disk"`) && (!strings.Contains(detail, "места") || !strings.Contains(detail, "компоненты")) {
			t.Fatal("the build/disk cause was not reported")
		}
	}
}

func TestPrivateCompletionFailureProducesSafeDiagnostic(t *testing.T) {
	python, err := exec.LookPath("python")
	if err != nil {
		python, err = exec.LookPath("python3")
	}
	if err != nil {
		t.Skip("Python unavailable for the private completion adapter")
	}
	root := t.TempDir()
	files := map[string]string{
		"entry.py":                        string(privateEntry),
		"pm/__init__.py":                  "",
		"pm/environments.py":              "def activate_dependencies(root):\n    pass\n",
		"hermes_cli/__init__.py":          "",
		"hermes_cli/_launchers.py":        "def expose_cli(*args, **kwargs):\n    raise RuntimeError('global exposure must be disabled')\n",
		"hermes_cli/source_completion.py": "def complete_source_checkout(root, **kwargs):\n    kwargs['followups'].append(('build', 'ENOTFOUND https://user:FIXTURE_SECRET@registry.example.invalid/private'))\n    return False\n",
	}
	for name, data := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, python, "-I", "-B", "-u", filepath.Join(root, "entry.py"), root, "complete")
	procutil.Hidden(cmd)
	procutil.Prepare(cmd)
	out, runErr := cmd.CombinedOutput()
	if runErr == nil {
		t.Fatal("the failed upstream completion was accepted")
	}
	if strings.Contains(string(out), "FIXTURE_SECRET") || strings.Contains(string(out), "registry.example.invalid") {
		t.Fatal("the adapter repeated a private upstream reason")
	}
	if detail := completionFailureDetail(out); !strings.Contains(detail, "серверы загрузки") {
		t.Fatalf("missing safe completion diagnosis: %s", out)
	}
}
