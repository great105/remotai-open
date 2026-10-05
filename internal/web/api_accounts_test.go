package web

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"tgcontrol/internal/agents"
)

// Дом на время теста подменяет общий withTempHome (api_commands_test.go):
// хранилище аккаунтов лежит в домашнем каталоге, и трогать боевой
// ~/.tgcontrol-accounts.json тест не вправе.

func TestAccountsDefaultAlwaysFirstAndActive(t *testing.T) {
	withTempHome(t)
	f := loadAccountsFile()

	list := accountsForAgent(f, "claude")
	if len(list) != 1 || list[0].ID != DefaultAccountID {
		t.Fatalf("основной аккаунт обязан существовать всегда: %#v", list)
	}
	if active := activeAccount(f, "claude"); active.ID != DefaultAccountID {
		t.Fatalf("без выбора активен основной, получили %q", active.ID)
	}
}

func TestAccountsActivateAndForget(t *testing.T) {
	withTempHome(t)
	home := realUserHome()

	account := AgentAccount{ID: "acc-1", AgentID: "claude", Label: "рабочий"}
	account.Dir = filepath.Join(accountsRoot(), "claude", account.ID)
	if err := os.MkdirAll(account.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	f := accountsFile{Accounts: []AgentAccount{account}, Active: map[string]string{"claude": "acc-1"}}
	if err := saveAccountsFile(f); err != nil {
		t.Fatal(err)
	}

	f = loadAccountsFile()
	if active := activeAccount(f, "claude"); active.ID != "acc-1" || active.Dir == "" {
		t.Fatalf("выбранный аккаунт не вернулся: %#v", active)
	}
	// Файл живёт в домашнем каталоге, а не где-то в профиле процесса.
	if _, err := os.Stat(filepath.Join(home, ".tgcontrol-accounts.json")); err != nil {
		t.Fatalf("список аккаунтов не сохранён в доме: %v", err)
	}

	// Каталог аккаунта — наш, значит удаляется вместе с записью: иначе на диске
	// остался бы живой токен от «удалённого» аккаунта.
	if !withinAccountsRoot(account.Dir) {
		t.Fatal("свой каталог не опознан как свой")
	}
	if withinAccountsRoot(filepath.Join(home, "Documents")) {
		t.Fatal("чужой каталог опознан как наш — его стирать нельзя")
	}
}

// Исчезнувший каталог не должен оставлять висящий выбор: запуск ушёл бы в
// несуществующий профиль, и агент встретил бы человека требованием войти.
func TestActiveAccountFallsBackWhenDirectoryGone(t *testing.T) {
	withTempHome(t)
	f := accountsFile{
		Accounts: []AgentAccount{{ID: "acc-9", AgentID: "codex", Label: "личный", Dir: filepath.Join(t.TempDir(), "gone")}},
		Active:   map[string]string{"codex": "acc-9"},
	}
	if active := activeAccount(f, "codex"); active.ID != DefaultAccountID {
		t.Fatalf("ожидали откат на основной, получили %#v", active)
	}
}

// Каталог КРЕДОВ зависит от того, что агент кладёт в переменную: у gemini это
// домашний каталог, и креды лежат внутри, в `.gemini`.
func TestUsageAccountsResolveCredentialsDirectory(t *testing.T) {
	withTempHome(t)
	dir := filepath.Join(accountsRoot(), "gemini", "acc-g")
	f := accountsFile{Accounts: []AgentAccount{{ID: "acc-g", AgentID: "gemini", Label: "второй", Dir: dir}}}
	if err := saveAccountsFile(f); err != nil {
		t.Fatal(err)
	}

	var found bool
	for _, a := range usageAccounts() {
		if a.ID != "acc-g" {
			continue
		}
		found = true
		if !strings.HasSuffix(a.Dir, filepath.Join("acc-g", ".gemini")) {
			t.Fatalf("креды gemini ищутся не там: %q", a.Dir)
		}
	}
	if !found {
		t.Fatal("аккаунт не попал в список для сбора лимитов")
	}

	// У основного аккаунта каталога нет вовсе — читать его надо там, где он и
	// лежал, иначе первое же переключение «потеряло» бы рабочий вход.
	for _, a := range usageAccounts() {
		if a.ID == DefaultAccountID && a.Dir != "" {
			t.Fatalf("основному аккаунту подставили каталог: %q", a.Dir)
		}
	}
}

// Закрепление за папкой сильнее общего выбора и действует на вложенные папки:
// агента запускают из любого места дерева проекта, а подписка у проекта одна.
func TestFolderPinBeatsGlobalChoice(t *testing.T) {
	withTempHome(t)
	work := filepath.Join(t.TempDir(), "work")
	if err := os.MkdirAll(filepath.Join(work, "service", "internal"), 0o700); err != nil {
		t.Fatal(err)
	}
	f := accountsFile{
		Accounts: []AgentAccount{
			{ID: "acc-personal", AgentID: "claude", Label: "личный"},
			{ID: "acc-work", AgentID: "claude", Label: "рабочий"},
		},
		Active:  map[string]string{"claude": "acc-personal"},
		Folders: map[string]string{folderKey("claude", work): "acc-work"},
	}

	// Вне закреплённой папки — общий выбор.
	if got := activeAccountIn(f, "claude", t.TempDir()).ID; got != "acc-personal" {
		t.Fatalf("вне проекта ожидали общий выбор, получили %q", got)
	}
	// В самой папке и во вложенной — закреплённый.
	for _, dir := range []string{work, filepath.Join(work, "service"), filepath.Join(work, "service", "internal")} {
		if got := activeAccountIn(f, "claude", dir).ID; got != "acc-work" {
			t.Fatalf("в %q ожидали закреплённый аккаунт, получили %q", dir, got)
		}
	}
	// Регистр и слеши на Windows пишут как придётся — это одна и та же папка.
	mixed := strings.ReplaceAll(work, `\`, "/")
	if got := activeAccountIn(f, "claude", mixed).ID; runtime.GOOS == "windows" && got != "acc-work" {
		t.Fatalf("путь через прямые слеши не опознан: %q", got)
	}
	// Снятое закрепление возвращает общий выбор.
	delete(f.Folders, folderKey("claude", work))
	if got := activeAccountIn(f, "claude", work).ID; got != "acc-personal" {
		t.Fatalf("после снятия закрепления ожидали общий выбор, получили %q", got)
	}
}

// Реестр обязан называть переменную для каждого агента, у которого мы обещаем
// аккаунты, и молчать у остальных: гадать нельзя — неверная переменная просто
// не сработает, а человек увидит «войдите заново».
func TestRegistryAccountEnvIsVerified(t *testing.T) {
	want := map[string]string{
		"claude": "CLAUDE_CONFIG_DIR",
		"codex":  "CODEX_HOME",
		"gemini": "GEMINI_CLI_HOME",
		"kimi":   "KIMI_CODE_HOME",
	}
	for id, env := range want {
		d := agents.GetDescriptor(id)
		if d == nil {
			t.Fatalf("агент %q пропал из реестра", id)
		}
		if d.AccountEnv != env {
			t.Fatalf("%s: переменная аккаунта %q, ожидалась %q", id, d.AccountEnv, env)
		}
		if !d.SupportsAccounts() {
			t.Fatalf("%s: аккаунты обязаны поддерживаться", id)
		}
	}
	// Shell и оркестратор — не подписки: у них аккаунтов нет по построению.
	for _, id := range []string{"shell", "orchestrator"} {
		if d := agents.GetDescriptor(id); d != nil && d.SupportsAccounts() {
			t.Fatalf("%s: аккаунтов быть не должно", id)
		}
	}
}
