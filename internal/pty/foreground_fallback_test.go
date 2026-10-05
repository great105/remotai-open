package pty

import (
	"errors"
	"testing"
)

func TestForegroundProcessOrShellDistinguishesIdleFromTransientFailure(t *testing.T) {
	agent := ProcessInfo{PID: 202, Name: "claude"}
	if got := foregroundProcessOrShell(101, "powershell.exe", agent, nil); got != agent {
		t.Fatalf("known foreground changed: got %+v want %+v", got, agent)
	}

	got := foregroundProcessOrShell(101, "powershell.exe", ProcessInfo{}, nil)
	if got.PID != 101 || got.Name != "powershell" {
		t.Fatalf("idle shell = %+v, want pid=101 name=powershell", got)
	}

	got = foregroundProcessOrShell(101, "bash", ProcessInfo{}, errors.New("snapshot failed"))
	if got.PID != 0 || got.Name != "" {
		t.Fatalf("transient scan failure announced a process: %+v", got)
	}
}
