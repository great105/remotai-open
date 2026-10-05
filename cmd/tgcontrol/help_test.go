package main

import (
	"strings"
	"testing"
)

// Живой стенд 06.09.2026: `remotai --help` запускал приложение вместо справки.
func TestHelpAndVersionArgsAreRecognised(t *testing.T) {
	for _, a := range []string{"help", "HELP", "-h", "--help", "-help", "/?"} {
		if !isHelpArg(a) {
			t.Errorf("help form not recognised: %q", a)
		}
	}
	for _, a := range []string{"version", "-v", "-V", "--version"} {
		if !isVersionArg(a) {
			t.Errorf("version form not recognised: %q", a)
		}
	}
	for _, a := range []string{"", "attach", "--background", "--minimized", "install", "-help-me"} {
		if isHelpArg(a) || isVersionArg(a) {
			t.Errorf("ordinary argument treated as help/version: %q", a)
		}
	}
}

// Справка обязана называть каждую подкоманду, которую разбирает main: иначе
// человек узнаёт о команде только из исходников.
func TestUsageListsEveryDispatchedCommand(t *testing.T) {
	text := usageText()
	for _, name := range []string{"install", "uninstall", "pair", "unpair", "status", "doctor", "config", "attach", "send", "update", "vpn", "remote", "--version", "--background"} {
		if !strings.Contains(text, "  "+name) {
			t.Errorf("usage does not mention %q:\n%s", name, text)
		}
	}
	if !strings.Contains(text, "remotai.ru") || !strings.Contains(text, "--help") {
		t.Errorf("usage lacks the site link or the per-command help hint:\n%s", text)
	}
}

// Справка печатается в консоль — на Windows её надо сначала подключить,
// иначе GUI-сборка молча выйдет без единой строки.
func TestHelpFormsRequestConsole(t *testing.T) {
	for _, a := range []string{"help", "-h", "--help", "version", "--version"} {
		if !cliConsoleRequested([]string{a}) {
			t.Errorf("%q prints to the console but does not request one", a)
		}
	}
}

func TestCommandUsage(t *testing.T) {
	u, ok := commandUsage("uninstall")
	if !ok || !strings.Contains(u, "--purge") {
		t.Errorf("uninstall usage missing: ok=%v %q", ok, u)
	}
	if _, ok := commandUsage("send"); ok {
		t.Error("send has its own richer help and must not be shadowed by the table")
	}
	if _, ok := commandUsage("no-such-command"); ok {
		t.Error("unknown command must not get a usage line")
	}
}
