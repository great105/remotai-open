package mcpmgr

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Живой прогон НАСТОЯЩИХ CLI во временном каталоге:
//
//	MCPMGR_LIVE=1 go test ./internal/mcpmgr -run Live -v
//
// Выключен по умолчанию: гоняет claude и codex, а на CI их нет, и чужой
// машине незачем запускать агентов из тестов. Конфиг владельца здесь только
// ЧИТАЕТСЯ — ради хэша до/после, который и доказывает, что его не задели.
func liveOnly(t *testing.T) {
	if os.Getenv("MCPMGR_LIVE") != "1" {
		t.Skip("MCPMGR_LIVE=1 — живой прогон claude/codex")
	}
}

func liveExe(t *testing.T, agent string) string {
	p, err := exec.LookPath(agent)
	if err != nil {
		t.Skipf("%s не установлен", agent)
	}
	exe := NativeExe(agent, p)
	if exe == "" {
		t.Skipf("%s: родной исполняемый файл не найден рядом с %s", agent, p)
	}
	return exe
}

// ownerFingerprint — хэш MCP-разделов НАСТОЯЩИХ конфигов владельца. Целиком
// `.claude.json` хэшировать бессмысленно: работающий рядом Claude Code
// переписывает его каждые несколько секунд (история, счётчики).
func ownerFingerprint(t *testing.T) string {
	home, _ := os.UserHomeDir()
	h := sha256.New()
	if raw, err := os.ReadFile(filepath.Join(home, ".claude.json")); err == nil {
		var f claudeFile
		if json.Unmarshal(raw, &f) == nil {
			b, _ := json.Marshal(f) // только mcpServers и projects[*].mcpServers
			h.Write(b)
		}
	}
	if raw, err := os.ReadFile(filepath.Join(home, ".codex", "config.toml")); err == nil {
		h.Write(raw)
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

func liveCycle(t *testing.T, tg Target, spec Spec, secret string) {
	ctx := context.Background()
	m := New(&Store{Path: filepath.Join(t.TempDir(), "mcp-disabled.json")})
	names := func() map[string]Server {
		list, err := m.List(ctx, tg)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		blob, _ := json.Marshal(list)
		if secret != "" && strings.Contains(string(blob), secret) {
			t.Fatalf("секрет попал в список: %s", blob)
		}
		out := map[string]Server{}
		for _, s := range list {
			out[s.Name] = s
		}
		return out
	}
	if err := m.Add(ctx, tg, spec); err != nil {
		t.Fatalf("add: %v", err)
	}
	if s, ok := names()[spec.Name]; !ok || !s.Enabled {
		t.Fatalf("после add сервера нет: %+v", s)
	}
	if err := m.Add(ctx, tg, spec); err == nil {
		t.Fatalf("повтор имени обязан отказать")
	}
	if err := m.SetEnabled(ctx, tg, spec.Name, false); err != nil {
		t.Fatalf("off: %v", err)
	}
	if s := names()[spec.Name]; s.Enabled {
		t.Fatalf("после off сервер включён: %+v", s)
	}
	// Сверяем САМ конфиг агента: сервера в нём нет.
	live, _, err := m.listLive(ctx, tg)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range live {
		if s.Name == spec.Name {
			t.Fatalf("после off сервер остался в конфиге агента")
		}
	}
	if err := m.SetEnabled(ctx, tg, spec.Name, true); err != nil {
		t.Fatalf("on: %v", err)
	}
	if s := names()[spec.Name]; !s.Enabled {
		t.Fatalf("после on сервер выключен: %+v", s)
	}
	if err := m.Remove(ctx, tg, spec.Name); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if _, ok := names()[spec.Name]; ok {
		t.Fatalf("после remove сервер остался")
	}
}

func TestLiveClaude(t *testing.T) {
	liveOnly(t)
	exe := liveExe(t, "claude")
	before := ownerFingerprint(t)
	home := t.TempDir()
	secret := "live-secret-" + filepath.Base(home)
	spec := Spec{Name: "remotai-live", Type: TypeStdio, Command: "npx", Args: []string{"-y", "some-mcp", "a b"}, Env: map[string]string{"API_KEY": secret}}
	// Основной аккаунт (переменной нет, HOME подменён) и отдельный каталог.
	liveCycle(t, Target{Agent: "claude", AccountID: "default", Exe: exe, Home: home}, spec, secret)
	if _, err := os.Stat(filepath.Join(home, ".claude.json")); err != nil {
		t.Fatalf("основной аккаунт обязан писать в <home>/.claude.json: %v", err)
	}
	acc := filepath.Join(home, "acc")
	_ = os.MkdirAll(acc, 0o700)
	web := Spec{Name: "remotai-web", Type: TypeSSE, URL: "https://example.com/sse?k=1", Headers: map[string]string{"Authorization": "Bearer " + secret}}
	liveCycle(t, Target{Agent: "claude", AccountID: "acc-1", Exe: exe, Home: home, ConfigDir: acc}, web, secret)
	if _, err := os.Stat(filepath.Join(acc, ".claude.json")); err != nil {
		t.Fatalf("аккаунт обязан писать в <каталог>/.claude.json: %v", err)
	}
	if after := ownerFingerprint(t); after != before {
		t.Fatalf("КОНФИГ ВЛАДЕЛЬЦА ИЗМЕНИЛСЯ: %s → %s", before, after)
	}
	t.Logf("конфиг владельца не тронут: %s", before)
}

func TestLiveCodex(t *testing.T) {
	liveOnly(t)
	exe := liveExe(t, "codex")
	before := ownerFingerprint(t)
	home := t.TempDir()
	codexHome := filepath.Join(home, ".codex")
	_ = os.MkdirAll(codexHome, 0o700)
	secret := "live-secret-" + filepath.Base(home)
	spec := Spec{Name: "remotai-live", Type: TypeStdio, Command: "npx", Args: []string{"-y", "some-mcp", "a b"}, Env: map[string]string{"API_KEY": secret}}
	liveCycle(t, Target{Agent: "codex", AccountID: "acc-1", Exe: exe, Home: home, ConfigDir: codexHome}, spec, secret)
	web := Spec{Name: "remotai-web", Type: TypeHTTP, URL: "https://example.com/mcp"}
	// Основной аккаунт: CODEX_HOME не задан, HOME подменён → <home>/.codex.
	liveCycle(t, Target{Agent: "codex", AccountID: "default", Exe: exe, Home: home}, web, "")
	if after := ownerFingerprint(t); after != before {
		t.Fatalf("КОНФИГ ВЛАДЕЛЬЦА ИЗМЕНИЛСЯ: %s → %s", before, after)
	}
	t.Logf("конфиг владельца не тронут: %s", before)
}

// Живое ЧТЕНИЕ настоящего списка владельца: только числа, без значений.
func TestLiveReadOwner(t *testing.T) {
	liveOnly(t)
	home, _ := os.UserHomeDir()
	m := New(&Store{Path: filepath.Join(t.TempDir(), "s.json")})
	for _, agent := range SupportedIDs {
		p, err := exec.LookPath(agent)
		if err != nil {
			continue
		}
		tg := Target{Agent: agent, AccountID: "default", Exe: NativeExe(agent, p), Home: home}
		list, err := m.List(context.Background(), tg)
		if err != nil {
			t.Fatalf("%s: %v", agent, err)
		}
		user, project, toggle, envKeys := 0, 0, 0, 0
		for _, s := range list {
			if s.Scope == "user" {
				user++
			} else {
				project++
			}
			if s.CanToggle {
				toggle++
			}
			envKeys += len(s.EnvKeys) + len(s.HeaderKeys)
		}
		t.Logf("%s: общих %d, проектных %d, можно выключать %d, ключей env/заголовков (имена) %d", agent, user, project, toggle, envKeys)
	}
}
