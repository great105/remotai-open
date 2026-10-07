package hermes

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"tgcontrol/internal/procutil"
)

func TestPrivateEntryKeepsCompletionAndRuntimeHandoffsPrivate(t *testing.T) {
	python, err := exec.LookPath("python")
	if err != nil {
		python, err = exec.LookPath("python3")
	}
	if err != nil {
		t.Skip("Python unavailable for adapter fixture")
	}
	root := t.TempDir()
	wrapper := filepath.Join(root, "private_entry.py")
	files := map[string]string{
		"private_entry.py":         string(privateEntry),
		"hermes_cli/__init__.py":   "",
		"pm/__init__.py":           "",
		"pm/environments.py":       "def activate_dependencies(root):\n    pass\n",
		"hermes_cli/_launchers.py": "from pathlib import Path\nimport os\ndef expose_cli(*a, **kw):\n    Path(os.environ['REMOTAI_PRIVATE_MARKER']).write_text('global publication')\n",
		"hermes_cli/venv_sync.py":  "def relaunch_command(*args):\n    raise RuntimeError('unpatched handoff')\n",
		"hermes_bootstrap.py":      "import sys\nassert sys.argv[1] == 'serve', sys.argv\n",
		"hermes_cli/main.py": `from pathlib import Path
import sys
from hermes_cli import _launchers, venv_sync
_launchers.expose_cli(Path(__file__).parent.parent)
root = Path(__file__).parent.parent
command = venv_sync.relaunch_command(Path(sys.executable), root, ['launcher', 'serve'], [], None)
assert command[command.index('cli')+1:] == ['serve'], command
assert 'private_entry.py' in command[command.index('cli')-2], command
print('runtime-private')
`,
		"hermes_cli/source_completion.py": `from pathlib import Path
import subprocess
import sys
from hermes_cli import _launchers
def complete_source_checkout(root, **kw):
    _launchers.expose_cli(root)
    subprocess.run([sys.executable, '-I', '-S', str(root / 'hermes_cli/update_completion.py'), 'receipt'], check=True)
    return True
`,
		"hermes_cli/update_completion.py": `from pathlib import Path
import subprocess
import sys
from hermes_cli import _launchers
root = Path(__file__).parent.parent
_launchers.expose_cli(root)
if sys.argv[-1] != '--prepared':
    subprocess.run([sys.executable, '-I', '-S', str(Path(__file__)), sys.argv[1], '--prepared'], check=True)
else:
    assert sys.argv[1] == 'receipt', sys.argv
    (root / 'receipt-ok').write_text('official receipt shape preserved')
`,
	}
	for name, data := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	marker := filepath.Join(root, "global-publication")
	for _, args := range [][]string{{"complete"}, {"cli", "serve"}} {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		cmd := exec.CommandContext(ctx, python, append([]string{"-I", "-B", "-u", wrapper, root}, args...)...)
		procutil.Hidden(cmd)
		procutil.Prepare(cmd)
		cmd.Env = append(os.Environ(), "REMOTAI_PRIVATE_MARKER="+marker)
		out, runErr := cmd.CombinedOutput()
		cancel()
		if runErr != nil {
			t.Fatalf("private wrapper %v: %v\n%s", args, runErr, out)
		}
	}
	if isFile(marker) {
		t.Fatal("managed lifecycle published a global command")
	}
	if !isFile(filepath.Join(root, "receipt-ok")) {
		t.Fatal("completion handoff/receipt was lost")
	}
}

// Opt-in only: install official native packages in an explicit disposable local
// appdata directory. No model calls, owner credentials or global config changes.
func TestLiveHermesSmoke(t *testing.T) {
	root := os.Getenv("REMOTAI_HERMES_LIVE_ROOT")
	if root == "" {
		t.Skip("set REMOTAI_HERMES_LIVE_ROOT to a disposable OS-local directory")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	// Inspecting CLI login availability can itself read host profiles. The live
	// probe uses its own OS home as well as its own Hermes home, before any code.
	osHome := filepath.Join(root, "os-home")
	if err := os.MkdirAll(osHome, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", osHome)
	t.Setenv("USERPROFILE", osHome)
	t.Setenv("APPDATA", filepath.Join(osHome, "AppData", "Roaming"))
	t.Setenv("LOCALAPPDATA", filepath.Join(osHome, "AppData", "Local"))
	command := func(ctx context.Context, name string, args ...string) *exec.Cmd {
		for i, arg := range args {
			if (arg == "--stage" || arg == "-Stage") && i+1 < len(args) {
				t.Logf("Official installer stage: %s", args[i+1])
			}
			if filepath.Base(arg) == "native_build_tools.sh" {
				t.Log("Preparing owned Intel macOS build tools")
			}
			if filepath.Base(arg) == "private_entry.py" && i+2 < len(args) {
				t.Logf("Private Hermes operation: %s", args[i+2])
			}
		}
		return exec.CommandContext(ctx, name, args...)
	}
	m, err := New(Options{Root: root, StartupTimeout: 3 * time.Minute, command: command})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		stop, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if err := m.Close(stop); err != nil {
			t.Error(err)
		}
	})
	if m.Status().Ownership != "managed" || !isFile(filepath.Join(root, "runtime", ".hermes-bootstrap-complete")) {
		t.Log("Installing official main Hermes source pinned to its exact commit in disposable local appdata")
		if err = m.Install(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if err = m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Logf("Hermes native backend ready: version=%s generation=%d", m.Status().Version, m.Status().BackendGeneration)
	for _, method := range []string{"gateway.capabilities", "model.options", "commands.catalog"} {
		call, stop := context.WithTimeout(ctx, 45*time.Second)
		raw, e := m.RPC(call, method, map[string]any{})
		stop()
		if e != nil {
			t.Fatalf("%s: %v", method, e)
		}
		if !json.Valid(raw) {
			t.Fatalf("%s invalid JSON", method)
		}
		t.Logf("%s: valid JSON (%d bytes)", method, len(raw))
	}
	for _, path := range []string{"/api/health", "/api/config/schema", "/api/providers/oauth?profile=default"} {
		resp, e := m.Do(ctx, "GET", path, nil)
		if e != nil {
			t.Fatal(e)
		}
		raw, e := io.ReadAll(io.LimitReader(resp.Body, 2*1024*1024))
		resp.Body.Close()
		if e != nil || resp.StatusCode != 200 || !json.Valid(raw) {
			t.Fatalf("%s: HTTP %d error=%v body=%s", path, resp.StatusCode, e, strings.TrimSpace(string(raw)))
		}
		t.Logf("%s: HTTP 200 valid JSON (%d bytes)", path, len(raw))
	}
	if err = m.CheckUpdate(ctx); err != nil {
		t.Fatal(err)
	}
	t.Logf("Official main channel verified, commit=%s update_available=%t", m.Status().InstalledCommit, m.Status().UpdateAvailable)
	if os.Getenv("REMOTAI_HERMES_LIVE_UPDATE") == "1" {
		t.Log("Verifying real idle retirement, official update and backend restart")
		if err = m.Update(ctx); err != nil {
			t.Fatalf("real lifecycle update: %v", err)
		}
		if !m.Status().Ready || m.Status().BackendGeneration < 2 || !validCommit(m.Status().InstalledCommit) {
			t.Fatal(m.Status())
		}
		if _, err = m.RPC(ctx, "gateway.capabilities", map[string]any{}); err != nil {
			t.Fatal(err)
		}
		t.Logf("Real official update/restart passed: commit=%s generation=%d", m.Status().InstalledCommit, m.Status().BackendGeneration)
	}
}
