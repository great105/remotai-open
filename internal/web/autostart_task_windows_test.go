//go:build windows

package web

import (
	"strings"
	"testing"
)

// Run real Windows PowerShell, replacing every scheduler cmdlet with a local
// function. USERPROFILE alone would NOT isolate the real TGControlUser task.
func TestAutostartRegistrationFailurePropagates(t *testing.T) {
	stubs := `
function New-ScheduledTaskAction { @{} }
function New-ScheduledTaskTrigger { @{} }
function New-ScheduledTaskPrincipal { @{} }
function New-ScheduledTaskSettingsSet { @{} }
function Register-ScheduledTask { Write-Error 'qa-registration-denied' }
`
	out, err := powershellCommand("-NoProfile", "-NonInteractive", "-Command", stubs+windowsAutostartScriptForTest()).CombinedOutput()
	if err == nil || !strings.Contains(string(out), "qa-registration-denied") {
		t.Fatalf("a registration failure must produce a failed process: %v %s", err, out)
	}
}

func TestDisabledAutostartIsNotReenabled(t *testing.T) {
	stub := `function Get-ScheduledTask { [pscustomobject]@{ State = 'Disabled' } }; `
	out, err := powershellCommand("-NoProfile", "-NonInteractive", "-Command", stub+windowsAutostartProbeForTest()).CombinedOutput()
	if err != nil || strings.TrimSpace(string(out)) != "absent" {
		t.Fatalf("disabled task must be left alone: %v %s", err, out)
	}
}
