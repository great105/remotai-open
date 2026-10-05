package agents

import (
	"path/filepath"
	"runtime"
	"testing"
)

func TestAgentRegistryTerminalCommands(t *testing.T) {
	for _, tc := range []struct {
		name       string
		names      []string
		file       string
		resume     string
		wantCLI    string
		wantResume string
	}{
		{"npm shim", []string{"kimi"}, "kimi.cmd", "", "kimi.cmd", ""},
		{"npm resume", []string{"codex"}, "codex.cmd", "codex resume --last", "codex.cmd", "codex.cmd resume --last"},
		{"native", []string{"claude"}, "claude.exe", "claude --continue", "claude.exe", "claude.exe --continue"},
		{"batch shim", []string{"kilo"}, "kilo.bat", "kilo --continue", "kilo.bat", "kilo.bat --continue"},
		{"detected alias", []string{"github-copilot-cli", "copilot"}, "copilot.cmd", "", "copilot.cmd", ""},
		{"explicit script", []string{"custom"}, "custom.ps1", "", "custom", ""},
		{"not installed", []string{"codex"}, "", "codex resume --last", "codex", "codex resume --last"},
		{"built in", nil, "", "", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := AgentDescriptor{CLINames: tc.names, ResumeCLI: tc.resume}
			if tc.file != "" {
				d.DetectedPath = filepath.Join(t.TempDir(), "bin with spaces", tc.file)
			}
			wantCLI, wantResume := tc.wantCLI, tc.wantResume
			if runtime.GOOS != "windows" {
				wantCLI = wantCLI[:len(wantCLI)-len(filepath.Ext(wantCLI))]
				wantResume = tc.resume
			}
			item := d.ToMap()
			if got := item["cli"]; got != wantCLI {
				t.Errorf("terminal CLI = %q, want %q", got, wantCLI)
			}
			if got := item["resume_cli"]; got != wantResume {
				t.Errorf("terminal resume = %q, want %q", got, wantResume)
			}
			// SSH still needs the extension-free registry names.
			names := item["cli_names"].([]string)
			for i, name := range names {
				if name != tc.names[i] {
					t.Errorf("SSH CLI name = %q, want %q", name, tc.names[i])
				}
			}
		})
	}
}

func TestAgentRegistryInstallCommands(t *testing.T) {
	for _, tc := range []struct {
		name    string
		install string
		windows string
		want    string
	}{
		{"npm", "npm i -g example-agent", "", "npm.cmd i -g example-agent"},
		{"pip", "pip install example-agent", "", "pip install example-agent"},
		{"platform override", "curl example.test/install.sh", "winget install Example.Agent", "winget install Example.Agent"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := AgentDescriptor{Install: tc.install, InstallWindows: tc.windows}
			want := tc.install
			if runtime.GOOS == "windows" {
				want = tc.want
			}
			item := d.ToMap()
			if got := item["install"]; got != want {
				t.Errorf("terminal install = %q, want %q", got, want)
			}
			if got := item["install_posix"]; got != tc.install {
				t.Errorf("SSH install changed: %q", got)
			}
		})
	}
}
