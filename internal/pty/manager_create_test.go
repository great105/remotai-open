package pty

import (
	"strings"
	"testing"
)

func TestManagerCreateReportsMissingShell(t *testing.T) {
	m := NewLocalManager()
	_, err := m.Create(0, ".", "tgcontrol-shell-that-does-not-exist", 80, 24)
	if err == nil {
		t.Fatal("Create unexpectedly accepted a missing shell")
	}
	if !strings.Contains(err.Error(), "не найден") {
		t.Fatalf("Create error = %q, want friendly missing-shell message", err)
	}
}
