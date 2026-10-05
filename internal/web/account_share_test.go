package web

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"tgcontrol/internal/agents"
)

// Главное свойство: у нового аккаунта СВОЙ вход, но ОБЩИЕ знания.
//
// Без этого человек переключается на вторую подписку и получает агента без
// единого скилла — формально верно, а выглядит как поломка (замер на живой
// машине: skills 112 МБ, plugins 84 МБ у основного профиля).
func TestSharedResourcesLinkKnowledgeAndCopySettings(t *testing.T) {
	main := t.TempDir()
	fresh := t.TempDir()

	// Основной профиль: папка знаний, файл настроек и — намеренно — вход.
	if err := os.MkdirAll(filepath.Join(main, "skills", "telegram"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(main, "skills", "telegram", "SKILL.md"), []byte("# скилл"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(main, "settings.json"), []byte(`{"theme":"dark"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(main, ".credentials.json"), []byte(`{"token":"секрет"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	d := agents.GetDescriptor("claude")
	if d == nil {
		t.Fatal("claude пропал из реестра")
	}
	result := shareAccountResources(d, main, fresh)

	// Скиллы видны в новом аккаунте — через связь, а не копию.
	skill := filepath.Join(fresh, "skills", "telegram", "SKILL.md")
	if _, err := os.Stat(skill); err != nil {
		t.Fatalf("скиллы не доехали до нового аккаунта: %v", err)
	}
	// Правка в основном профиле видна во втором — это и значит «общие».
	if err := os.WriteFile(filepath.Join(main, "skills", "telegram", "SKILL.md"), []byte("# правка"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(skill)
	if err != nil || string(got) != "# правка" {
		t.Fatalf("папка скопирована, а не связана: %q (%v)", got, err)
	}

	// Настройки — копия: связь порвал бы первый же «временный файл + rename».
	settings, err := os.ReadFile(filepath.Join(fresh, "settings.json"))
	if err != nil || string(settings) != `{"theme":"dark"}` {
		t.Fatalf("настройки не скопированы: %q (%v)", settings, err)
	}

	// ВХОД НЕ ПЕРЕНОСИТСЯ НИКОГДА — ради этого всё и затевалось.
	if _, err := os.Stat(filepath.Join(fresh, ".credentials.json")); !os.IsNotExist(err) {
		t.Fatal("вход основного аккаунта утёк в новый")
	}

	var linked, copied int
	for _, r := range result {
		if r.Linked {
			linked++
		}
		if r.Copied {
			copied++
		}
		if r.Error != "" {
			t.Fatalf("ресурс %q не сделан общим: %s", r.Name, r.Error)
		}
	}
	if linked != 1 || copied != 1 {
		t.Fatalf("связано %d, скопировано %d — ждали 1 и 1: %#v", linked, copied, result)
	}
}

// Повторный вызов ничего не ломает: у человека может быть уже связанный
// профиль, а кнопка «завести» нажимается и по второму разу.
func TestSharedResourcesAreIdempotent(t *testing.T) {
	main, fresh := t.TempDir(), t.TempDir()
	if err := os.MkdirAll(filepath.Join(main, "skills"), 0o700); err != nil {
		t.Fatal(err)
	}
	d := agents.GetDescriptor("claude")
	first := shareAccountResources(d, main, fresh)
	second := shareAccountResources(d, main, fresh)
	if len(first) == 0 {
		t.Fatal("первый проход ничего не сделал")
	}
	if len(second) != 0 {
		t.Fatalf("повтор снова что-то делает: %#v", second)
	}
}

// MCP-серверы лежат в одном файле со входом и историей проектов, поэтому
// переносим ровно секцию — и ничего больше.
func TestClaudeMCPCopiedWithoutCredentials(t *testing.T) {
	main, fresh := t.TempDir(), t.TempDir()
	content := `{
	  "oauthAccount": {"emailAddress": "main@example.com"},
	  "projects": {"C:/work": {"history": ["секрет"]}},
	  "mcpServers": {"telegram": {"command": "node", "args": ["bot.js"]}}
	}`
	if err := os.WriteFile(filepath.Join(main, ".claude.json"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	// Источник — путь к ФАЙЛУ: у основного аккаунта он лежит в корне домашней
	// папки, а не в каталоге конфига (живой прогон ловил на этом ноль серверов).
	n, err := copyClaudeMCP(filepath.Join(main, ".claude.json"), fresh)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("перенесено серверов: %d, ждали 1", n)
	}

	raw, err := os.ReadFile(filepath.Join(fresh, ".claude.json"))
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if _, ok := got["mcpServers"]; !ok {
		t.Fatal("MCP-серверы не доехали")
	}
	if _, leaked := got["oauthAccount"]; leaked {
		t.Fatal("вместе с MCP переехал ВХОД основного аккаунта")
	}
	if _, leaked := got["projects"]; leaked {
		t.Fatal("вместе с MCP переехала история проектов")
	}
}

// У kimi общих ресурсов нет намеренно: единственный похожий файл (config.toml)
// содержит вход, и копировать его нельзя.
func TestKimiSharesNothing(t *testing.T) {
	d := agents.GetDescriptor("kimi")
	if d == nil {
		t.Fatal("kimi пропал из реестра")
	}
	if len(d.AccountShared) != 0 {
		t.Fatalf("у kimi появились общие ресурсы: %#v", d.AccountShared)
	}
}
