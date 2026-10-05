package web

import "testing"

func TestAddAgentScrollModeDefaultUsesRegistryContract(t *testing.T) {
	want := map[string]string{
		"claude": "agent",
		"codex":  "terminal",
		"kimi":   "terminal",
	}
	for kind, mode := range want {
		resp := map[string]any{"agent_kind": kind}
		addAgentScrollModeDefault(resp, kind)
		if got := resp["scroll_mode_default"]; got != mode {
			t.Errorf("%s state default = %#v, want %q", kind, got, mode)
		}
	}

	resp := map[string]any{"agent_kind": "future-agent"}
	addAgentScrollModeDefault(resp, "future-agent")
	if _, ok := resp["scroll_mode_default"]; ok {
		t.Fatal("unknown agent received a forced scroll default")
	}
}

// ST-04: history_retention в /state — только объявленное реестром по
// доказательству. Хранение не выводится из прокрутки: у kimi прокрутка та же
// "terminal", что у codex, а политики нет.
func TestAddAgentHistoryRetentionOnlyWhenDeclared(t *testing.T) {
	resp := map[string]any{}
	addAgentHistoryRetention(resp, "codex")
	if got := resp["history_retention"]; got != "preserve" {
		t.Fatalf("codex history_retention = %#v, want preserve", got)
	}
	for _, kind := range []string{"claude", "kimi", "future-agent", ""} {
		resp := map[string]any{}
		addAgentHistoryRetention(resp, kind)
		if got, ok := resp["history_retention"]; ok {
			t.Errorf("%q received unproven history_retention %#v", kind, got)
		}
	}
}
