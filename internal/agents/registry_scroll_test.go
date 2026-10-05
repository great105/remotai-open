package agents

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestAgentScrollModeDefaults(t *testing.T) {
	want := map[string]string{
		"claude": ScrollModeAgent,
		"codex":  ScrollModeTerminal,
		"kimi":   ScrollModeTerminal,
	}
	for id, mode := range want {
		desc := GetDescriptor(id)
		if desc == nil {
			t.Fatalf("missing descriptor %q", id)
		}
		if got := desc.ScrollModeDefault(); got != mode {
			t.Errorf("%s scroll default = %q, want %q", id, got, mode)
		}
		if got := desc.ToMap()["scroll_mode_default"]; got != mode {
			t.Errorf("%s wire scroll default = %#v, want %q", id, got, mode)
		}
	}
}

func TestAgentScrollModeDefaultsAreWireSafe(t *testing.T) {
	for _, desc := range Registry {
		switch desc.DefaultScrollMode {
		case "", ScrollModeAuto, ScrollModeTerminal, ScrollModeAgent:
		default:
			t.Errorf("%s has invalid scroll default %q", desc.ID, desc.DefaultScrollMode)
		}
		switch got := desc.ScrollModeDefault(); got {
		case ScrollModeAuto, ScrollModeTerminal, ScrollModeAgent:
		default:
			t.Errorf("%s normalized to invalid scroll mode %q", desc.ID, got)
		}
	}

	for _, raw := range []string{"", ScrollModeAuto, "future-mode"} {
		desc := AgentDescriptor{DefaultScrollMode: raw}
		if got := desc.ScrollModeDefault(); got != ScrollModeAuto {
			t.Errorf("unforced scroll default %q normalized to %q, want %q", raw, got, ScrollModeAuto)
		}
	}
}

// ST-04: политика хранения истории — отдельное от прокрутки свойство профиля.
// Допустимы только "honor", "preserve" и пусто; заполняется ТОЛЬКО по
// доказательству, поэтому ожидаемые значения перечислены явно: появление нового
// значения в реестре без правки этого теста — сигнал проверить доказательство.
func TestAgentHistoryRetentionContract(t *testing.T) {
	want := map[string]string{
		"claude": "",
		"codex":  HistoryRetentionPreserve,
		"kimi":   "",
	}
	for id, retention := range want {
		desc := GetDescriptor(id)
		if desc == nil {
			t.Fatalf("missing descriptor %q", id)
		}
		if desc.HistoryRetention != retention {
			t.Errorf("%s history retention = %q, want %q", id, desc.HistoryRetention, retention)
		}
	}
	for _, desc := range Registry {
		switch desc.HistoryRetention {
		case "", HistoryRetentionHonor, HistoryRetentionPreserve:
		default:
			t.Errorf("%s has invalid history retention %q", desc.ID, desc.HistoryRetention)
		}
		if _, listed := want[desc.ID]; !listed && desc.HistoryRetention != "" {
			t.Errorf("%s declares history retention %q without an entry in this contract", desc.ID, desc.HistoryRetention)
		}
	}

	// Проводной вид: поле необязательное, у агента без доказательства его нет
	// вовсе (старый клиент и неизвестный агент видят прежний JSON).
	codex, _ := json.Marshal(GetDescriptor("codex"))
	if !strings.Contains(string(codex), `"history_retention":"preserve"`) {
		t.Errorf("codex wire descriptor lacks history_retention: %s", codex)
	}
	claude, _ := json.Marshal(GetDescriptor("claude"))
	if strings.Contains(string(claude), "history_retention") {
		t.Errorf("claude wire descriptor has unproven history_retention: %s", claude)
	}
}
