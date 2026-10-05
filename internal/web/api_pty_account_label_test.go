package web

import (
	"os"
	"path/filepath"
	"testing"

	"tgcontrol/internal/pty"
)

// Два терминала одного и того же Codex («дом» и «работа») в списке выглядели
// одинаково: у именованного аккаунта метка есть, у основного она пуста по
// конструкции. Различить их было нечем.
func TestLabelDefaultAccounts(t *testing.T) {
	home := t.TempDir()
	t.Setenv("USERPROFILE", home)
	t.Setenv("HOME", home)

	// У codex два аккаунта (основной + «работа»), у claude — только основной.
	if err := os.MkdirAll(filepath.Dir(accountsPath()), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := saveAccountsFile(accountsFile{
		Accounts: []AgentAccount{
			{ID: "work", AgentID: "codex", Label: "работа", Dir: filepath.Join(home, "codex-work")},
		},
	}); err != nil {
		t.Fatalf("saveAccountsFile: %v", err)
	}

	list := []pty.SessionInfo{
		{ID: "1", AgentKind: "codex"},                         // основной аккаунт codex
		{ID: "2", AgentKind: "codex", AccountLabel: "работа"}, // именованный — не трогаем
		{ID: "3", AgentKind: "claude"},                        // единственный аккаунт — подпись была бы шумом
		{ID: "4", AgentKind: "shell"},                         // не агент
		{ID: "5", AgentKind: ""},                              // вид неизвестен
	}

	got := labelDefaultAccounts(list)

	if got[0].AccountLabel != defaultAccountName {
		t.Errorf("сессия на основном аккаунте codex осталась без подписи: %q", got[0].AccountLabel)
	}
	if got[1].AccountLabel != "работа" {
		t.Errorf("именованный аккаунт перезаписан: %q", got[1].AccountLabel)
	}
	if got[2].AccountLabel != "" {
		t.Errorf("у claude один аккаунт — подпись это шум, получили %q", got[2].AccountLabel)
	}
	if got[3].AccountLabel != "" || got[4].AccountLabel != "" {
		t.Errorf("не-агентские сессии подписывать нечем: %q / %q", got[3].AccountLabel, got[4].AccountLabel)
	}
}
