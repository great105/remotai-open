package pty

import (
	"testing"

	"tgcontrol/internal/agents"
)

func TestCLIRegistryHasInstallAndDetectionMarker(t *testing.T) {
	markers := make(map[string]bool, len(agentCmdlineMarkers))
	for _, item := range agentCmdlineMarkers {
		markers[item.kind] = len(item.markers) > 0
	}
	for _, desc := range agents.Registry {
		if len(desc.CLINames) == 0 {
			continue
		}
		if desc.InstallCommand() == "" {
			t.Errorf("%s: CLI agent has no install command", desc.ID)
		}
		if !markers[desc.ID] {
			t.Errorf("%s: CLI agent has no command-line detection marker", desc.ID)
		}
		if got := AgentKind(desc.CLINames[0]); got != desc.ID {
			t.Errorf("%s: primary CLI %q resolves to %q", desc.ID, desc.CLINames[0], got)
		}
	}
}
