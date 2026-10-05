package web

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// Проверяем ровно ту жалобу, ради которой это написано: «зашёл во второй
// аккаунт, а он опять просит войти». Claude Code гоняет мастер первого запуска
// (с экраном входа внутри), пока в конфиге каталога нет hasCompletedOnboarding.

func writeAccountCreds(t *testing.T, dir, token string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	body := `{"claudeAiOauth":{"accessToken":"` + token + `","subscriptionType":"max"}}`
	if err := os.WriteFile(filepath.Join(dir, ".credentials.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func onboardingDone(t *testing.T, dir string) bool {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, ".claude.json"))
	if err != nil {
		return false
	}
	var config map[string]any
	if err := json.Unmarshal(raw, &config); err != nil {
		t.Fatalf("конфиг аккаунта испорчен: %v", err)
	}
	done, _ := config[claudeOnboardingKey].(bool)
	return done
}

func TestOnboardingHealedOnlyWhenSignedIn(t *testing.T) {
	withTempHome(t)
	dir := filepath.Join(accountsRoot(), "claude", "acc-1")

	// Аккаунт заведён, но входа ещё нет: отметку не ставим НИКОГДА — мастер
	// как раз и ведёт человека ко входу, без него он упрётся в отказ провайдера
	// уже после первого запроса.
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if healClaudeOnboarding(dir) || onboardingDone(t, dir) {
		t.Fatal("мастер помечен пройденным у аккаунта без входа")
	}

	// Пустой токен — это тоже «не входили»: файл кредов у Claude появляется и
	// в середине неудачной попытки входа.
	writeAccountCreds(t, dir, "")
	if healClaudeOnboarding(dir) || onboardingDone(t, dir) {
		t.Fatal("пустой токен принят за живой вход")
	}

	// Вход есть — отметка появляется, и второй запуск агента больше не увидит
	// «Select login method».
	writeAccountCreds(t, dir, "sk-test")
	if !healClaudeOnboarding(dir) {
		t.Fatal("вход есть, а мастер не починен")
	}
	if !onboardingDone(t, dir) {
		t.Fatalf("отметка %s не записана", claudeOnboardingKey)
	}
	// Повтор ничего не переписывает: файл принадлежит агенту, и лишняя запись —
	// это шанс встретиться с ним на одном и том же конфиге.
	if healClaudeOnboarding(dir) {
		t.Fatal("повторный вызов переписал конфиг заново")
	}
}

func TestOnboardingHealKeepsOtherSettings(t *testing.T) {
	withTempHome(t)
	dir := filepath.Join(accountsRoot(), "claude", "acc-2")
	writeAccountCreds(t, dir, "sk-test")
	// В этом же файле лежат MCP-серверы, которые мы туда сами и положили при
	// заведении аккаунта: потерять их починкой мастера было бы обменом одной
	// поломки на другую.
	seed := `{"mcpServers":{"stitch":{"type":"stdio"}},"theme":"dark"}`
	if err := os.WriteFile(filepath.Join(dir, ".claude.json"), []byte(seed), 0o600); err != nil {
		t.Fatal(err)
	}

	if !healClaudeOnboarding(dir) {
		t.Fatal("вход есть, а мастер не починен")
	}
	raw, err := os.ReadFile(filepath.Join(dir, ".claude.json"))
	if err != nil {
		t.Fatal(err)
	}
	var config map[string]any
	if err := json.Unmarshal(raw, &config); err != nil {
		t.Fatal(err)
	}
	if done, _ := config[claudeOnboardingKey].(bool); !done {
		t.Fatal("отметка не записана")
	}
	if _, ok := config["mcpServers"]; !ok {
		t.Fatal("MCP-серверы затёрты починкой")
	}
	if config["theme"] != "dark" {
		t.Fatalf("чужие настройки затёрты: %#v", config)
	}

	// Битый конфиг не трогаем вовсе: перезапись стоила бы человеку настроек.
	broken := filepath.Join(accountsRoot(), "claude", "acc-3")
	writeAccountCreds(t, broken, "sk-test")
	if err := os.WriteFile(filepath.Join(broken, ".claude.json"), []byte("{не json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if healClaudeOnboarding(broken) {
		t.Fatal("испорченный конфиг переписан")
	}
	raw, err = os.ReadFile(filepath.Join(broken, ".claude.json"))
	if err != nil || string(raw) != "{не json" {
		t.Fatalf("испорченный конфиг изменён: %q, %v", string(raw), err)
	}
}

func TestOnboardingHealSkipsForeignDirs(t *testing.T) {
	withTempHome(t)
	// Каталог не наш (человек указал свой путь руками) — не лезем: мы его не
	// заводили, и правила его конфига не наши.
	foreign := filepath.Join(realUserHome(), "my-claude-profile")
	writeAccountCreds(t, foreign, "sk-test")

	ours := filepath.Join(accountsRoot(), "claude", "acc-4")
	writeAccountCreds(t, ours, "sk-test")

	healAccountsOnboarding(accountsFile{Accounts: []AgentAccount{
		{ID: "acc-x", AgentID: "claude", Dir: foreign},
		{ID: "acc-4", AgentID: "claude", Dir: ours},
		// Чужой агент со своим каталогом: у него другой конфиг и другие правила.
		{ID: "acc-5", AgentID: "codex", Dir: filepath.Join(accountsRoot(), "codex", "acc-5")},
	}})

	if onboardingDone(t, foreign) {
		t.Fatal("залезли в чужой каталог аккаунта")
	}
	if !onboardingDone(t, ours) {
		t.Fatal("свой каталог не вылечен")
	}
}
