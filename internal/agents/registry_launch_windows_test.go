package agents

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf16"

	"tgcontrol/internal/procutil"
)

func TestAgentRegistryLaunchWithRestrictedPowerShell(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "npm wrappers with spaces")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	const cli = "remotai-test-agent"
	for name, contents := range map[string]string{
		cli + ".ps1": "Write-Output 'UNEXPECTED_PS1'\r\n",
		cli + ".cmd": "@echo off\r\necho CMD_OK %*\r\nexit /b 0\r\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(contents), 0600); err != nil {
			t.Fatal(err)
		}
	}
	d := AgentDescriptor{
		CLINames:     []string{cli},
		DetectedPath: filepath.Join(dir, cli+".cmd"),
		ResumeCLI:    cli + " resume --last",
	}
	item := d.ToMap()
	// Restrict only this child process. The control must reproduce the user's
	// PSSecurityException before commands from the API are allowed to pass.
	script := fmt.Sprintf(`
$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
$env:PATH = '%s;' + $env:PATH
try { %s --probe; throw 'The bare command unexpectedly ran' }
catch [System.Management.Automation.PSSecurityException] { Write-Output 'BARE_BLOCKED' }
%s --probe "one argument"
%s
Write-Output ('POLICY=' + $env:PSExecutionPolicyPreference)
`, strings.ReplaceAll(dir, "'", "''"), cli, item["cli"], item["resume_cli"])
	encoded := utf16.Encode([]rune(script))
	bytes := make([]byte, len(encoded)*2)
	for i, unit := range encoded {
		binary.LittleEndian.PutUint16(bytes[i*2:], unit)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "powershell.exe", "-NoLogo", "-NoProfile", "-NonInteractive",
		"-ExecutionPolicy", "Restricted", "-EncodedCommand", base64.StdEncoding.EncodeToString(bytes))
	procutil.Hidden(cmd)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("PowerShell registry launch failed: %v\n%s", err, output)
	}
	for _, want := range []string{"BARE_BLOCKED", `CMD_OK --probe "one argument"`, "CMD_OK resume --last", "POLICY=Restricted"} {
		if !strings.Contains(string(output), want) {
			t.Errorf("missing %q in PowerShell output:\n%s", want, output)
		}
	}
	if strings.Contains(string(output), "UNEXPECTED_PS1") {
		t.Errorf("PowerShell script ran instead of the CMD launcher:\n%s", output)
	}
}
