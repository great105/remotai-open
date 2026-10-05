package main

import "testing"

func TestConsoleOnlyForExplicitCLI(t *testing.T) {
	for _, args := range [][]string{nil, {"--background"}, {"--minimized"}, {"--pty-host"}, {"--enable-autostart"}, {"--disable-autostart"}, {"--uninstall-cleanup"}} {
		if cliConsoleRequested(args) {
			t.Errorf("background launch requests console: %v", args)
		}
	}
	for _, command := range []string{"attach", "pair", "config", "doctor", "--version", "--help"} {
		if !cliConsoleRequested([]string{command}) {
			t.Errorf("CLI has no console: %s", command)
		}
	}
}
