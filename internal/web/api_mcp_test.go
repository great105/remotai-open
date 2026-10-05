package web

import (
	"errors"
	"testing"

	"tgcontrol/internal/mcpmgr"
)

// Аккаунт для MCP: присланный id своего агента → он; чужой или никакой →
// выбранный в «Аккаунтах»; исчезнувший каталог → отказ, а не основной молча.
func TestMCPAccountSelection(t *testing.T) {
	dir := t.TempDir()
	f := accountsFile{
		Accounts: []AgentAccount{
			{ID: "acc-c", AgentID: "claude", Dir: dir},
			{ID: "acc-x", AgentID: "codex", Dir: dir},
			{ID: "acc-gone", AgentID: "claude", Dir: dir + "/missing"},
		},
		Active: map[string]string{"codex": "acc-x"},
	}
	if a, ok := mcpAccount(f, "claude", []string{"acc-x", "acc-c"}); !ok || a.ID != "acc-c" {
		t.Fatalf("claude: %+v %v", a, ok)
	}
	if a, ok := mcpAccount(f, "codex", []string{"acc-c"}); !ok || a.ID != "acc-x" {
		t.Fatalf("codex must fall back to active: %+v", a)
	}
	if a, ok := mcpAccount(f, "claude", nil); !ok || a.ID != DefaultAccountID {
		t.Fatalf("claude default: %+v", a)
	}
	if _, ok := mcpAccount(f, "claude", []string{"acc-gone"}); ok {
		t.Fatalf("missing dir must not silently fall back")
	}
}

func TestMCPErrorStatus(t *testing.T) {
	cases := map[error]int{
		&mcpmgr.UserError{Kind: mcpmgr.ErrInvalid}:     400,
		&mcpmgr.UserError{Kind: mcpmgr.ErrExists}:      409,
		&mcpmgr.UserError{Kind: mcpmgr.ErrNotFound}:    404,
		&mcpmgr.UserError{Kind: mcpmgr.ErrUnsupported}: 422,
		errors.New("cli"): 424, // 502 клиент считает обрывом связи
	}
	for err, want := range cases {
		if got, _ := mcpErrorStatus(err); got != want {
			t.Errorf("%v: %d want %d", err, got, want)
		}
	}
}
