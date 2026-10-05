package mcpmgr

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNormalizeValidation(t *testing.T) {
	cases := []struct {
		name string
		spec Spec
		ok   bool
	}{
		{"stdio ok", Spec{Name: "fs", Type: "stdio", Command: "npx", Args: []string{"-y", "", "pkg"}}, true},
		{"empty type = stdio", Spec{Name: "fs", Command: "npx"}, true},
		{"name with space", Spec{Name: "bad name", Command: "x"}, false},
		{"name with dot (claude отказывает)", Spec{Name: "a.b", Command: "x"}, false},
		{"name too long", Spec{Name: strings.Repeat("a", 65), Command: "x"}, false},
		{"no command", Spec{Name: "x", Type: "stdio"}, false},
		{"newline in arg", Spec{Name: "x", Command: "c", Args: []string{"a\nb"}}, false},
		{"bad env key", Spec{Name: "x", Command: "c", Env: map[string]string{"1BAD": "v"}}, false},
		{"env value newline", Spec{Name: "x", Command: "c", Env: map[string]string{"K": "a\r\nb"}}, false},
		{"http ok", Spec{Name: "w", Type: "http", URL: "https://ex.com/mcp", Headers: map[string]string{"Authorization": "Bearer x"}}, true},
		{"http bad scheme", Spec{Name: "w", Type: "http", URL: "ftp://ex.com"}, false},
		{"http userinfo", Spec{Name: "w", Type: "http", URL: "https://u:p@ex.com"}, false},
		{"http with env", Spec{Name: "w", Type: "http", URL: "https://ex.com", Env: map[string]string{"A": "b"}}, false},
		{"stdio with headers", Spec{Name: "w", Command: "x", Headers: map[string]string{"A": "b"}}, false},
		{"bad header name", Spec{Name: "w", Type: "sse", URL: "https://ex.com", Headers: map[string]string{"Bad Header": "b"}}, false},
		{"unknown type", Spec{Name: "w", Type: "ws", URL: "wss://x"}, false},
	}
	for _, c := range cases {
		got, err := c.spec.Normalize()
		if (err == nil) != c.ok {
			t.Errorf("%s: err=%v, want ok=%v", c.name, err, c.ok)
		}
		if err != nil && !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: kind %v, want ErrInvalid", c.name, err)
		}
		if c.name == "stdio ok" && len(got.Args) != 2 {
			t.Errorf("empty args must be dropped, got %v", got.Args)
		}
	}
}

func TestCheckCaps(t *testing.T) {
	sse := Spec{Name: "s", Type: TypeSSE, URL: "https://ex.com"}
	if err := sse.CheckCaps("codex"); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("codex must reject sse, got %v", err)
	}
	if err := sse.CheckCaps("claude"); err != nil {
		t.Fatalf("claude accepts sse: %v", err)
	}
	h := Spec{Name: "h", Type: TypeHTTP, URL: "https://ex.com", Headers: map[string]string{"X": "y"}}
	if err := h.CheckCaps("codex"); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("codex must reject headers, got %v", err)
	}
	if err := h.CheckCaps("gemini"); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("unknown agent must be unsupported")
	}
}

func TestMaskArgs(t *testing.T) {
	in := []string{"-y", "@mcp/server", "--api-key", "sk-live-1", "--token=abc", "API_KEY=zzz", "ghp_ABCDEFGHIJ1234567890abcdefXYZ", "/path/to/dir", "--path", "/x"}
	got := MaskArgs(in)
	want := []string{"-y", "@mcp/server", "--api-key", Mask, "--token=" + Mask, "API_KEY=" + Mask, Mask, "/path/to/dir", "--path", "/x"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("MaskArgs\n got %v\nwant %v", got, want)
	}
}

func TestMaskURL(t *testing.T) {
	got := MaskURL("https://user:pw@mcp.ex.com/v1/sse?api_key=SECRET&x=1#frag")
	if strings.Contains(got, "SECRET") || strings.Contains(got, "pw") || strings.Contains(got, "frag") {
		t.Fatalf("secret leaked: %s", got)
	}
	if !strings.Contains(got, "mcp.ex.com/v1/sse") || !strings.Contains(got, "api_key="+Mask) {
		t.Fatalf("unexpected: %s", got)
	}
	if MaskURL("https://ex.com/mcp") != "https://ex.com/mcp" {
		t.Fatalf("plain url must stay")
	}
}

func TestScrub(t *testing.T) {
	msg := scrub("WARNING: noise\nError: bad json {\"env\":{\"K\":\"topsecret\"}}\nstack", []string{"topsecret"})
	if strings.Contains(msg, "topsecret") || strings.Contains(msg, "WARNING") || strings.Contains(msg, "stack") {
		t.Fatalf("scrub: %q", msg)
	}
}

// Claude: из файла берутся имена ключей, но НЕ значения, а проектные — только
// для чтения.
func TestListClaudeHidesSecrets(t *testing.T) {
	dir := t.TempDir()
	cfg := `{
  "oauthAccount": {"emailAddress": "x@y"},
  "mcpServers": {
    "demo": {"type":"stdio","command":"npx","args":["-y","pkg","--api-key","sk-VALUE-1"],"env":{"API_KEY":"sekret-env"}},
    "web": {"type":"http","url":"https://ex.com/mcp?token=qtok","headers":{"Authorization":"Bearer hdr-secret"}},
    "legacy": {"command":"node","args":["s.js"]}
  },
  "projects": {"C:/work": {"mcpServers": {"local1": {"type":"stdio","command":"x","env":{"P":"proj-secret"}}}}}
}`
	if err := os.WriteFile(filepath.Join(dir, ".claude.json"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	list, err := listClaude(Target{Agent: "claude", ConfigDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 4 {
		t.Fatalf("want 4 servers, got %d", len(list))
	}
	blob, _ := json.Marshal(list)
	for _, secret := range []string{"sekret-env", "hdr-secret", "sk-VALUE-1", "qtok", "proj-secret", "x@y"} {
		if strings.Contains(string(blob), secret) {
			t.Fatalf("secret %q leaked in %s", secret, blob)
		}
	}
	byName := map[string]Server{}
	for _, s := range list {
		byName[s.Name] = s
	}
	if byName["demo"].EnvKeys[0] != "API_KEY" || !byName["demo"].CanToggle {
		t.Fatalf("demo: %+v", byName["demo"])
	}
	if byName["web"].HeaderKeys[0] != "Authorization" || byName["web"].Type != TypeHTTP {
		t.Fatalf("web: %+v", byName["web"])
	}
	if byName["legacy"].Type != TypeStdio {
		t.Fatalf("legacy type: %+v", byName["legacy"])
	}
	if l := byName["local1"]; !l.ReadOnly || l.Scope != "project" || l.CanToggle {
		t.Fatalf("project server must be read-only: %+v", l)
	}
}

// Основной аккаунт Claude — `~/.claude.json` в КОРНЕ home, а не в ~/.claude.
func TestClaudeConfigFileMainAccount(t *testing.T) {
	got := claudeConfigFile(Target{Agent: "claude", Home: filepath.Join("h", "user")})
	if got != filepath.Join("h", "user", ".claude.json") {
		t.Fatalf("main account file: %s", got)
	}
	got = claudeConfigFile(Target{Agent: "claude", Home: "h", ConfigDir: filepath.Join("acc", "a1")})
	if got != filepath.Join("acc", "a1", ".claude.json") {
		t.Fatalf("account file: %s", got)
	}
}

const codexListSample = `[
 {"name":"a","enabled":true,"disabled_reason":null,"transport":{"type":"stdio","command":"npx","args":["-y","p"],"env":{"K":"codex-secret"},"env_vars":[],"cwd":null},"startup_timeout_sec":null,"tool_timeout_sec":null,"auth_status":"unsupported"},
 {"name":"w","enabled":true,"disabled_reason":null,"transport":{"type":"streamable_http","url":"https://ex.com/mcp","bearer_token_env_var":"MY_TOKEN","http_headers":null,"env_http_headers":null,"http_headers_helper":null},"startup_timeout_sec":null,"tool_timeout_sec":null},
 {"name":"t","enabled":true,"disabled_reason":null,"transport":{"type":"stdio","command":"x","args":[],"env":null,"env_vars":["PATH_X"],"cwd":null},"startup_timeout_sec":30,"tool_timeout_sec":null},
 {"name":"off","enabled":false,"disabled_reason":null,"transport":{"type":"stdio","command":"x","args":[],"env":null,"env_vars":[],"cwd":null},"startup_timeout_sec":null,"tool_timeout_sec":null}
]`

func TestCodexParse(t *testing.T) {
	entries, err := parseCodexList([]byte(codexListSample))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]Server{}
	for _, e := range entries {
		s := codexServer(e)
		got[s.Name] = s
	}
	blob, _ := json.Marshal(got)
	if strings.Contains(string(blob), "codex-secret") {
		t.Fatalf("secret leaked: %s", blob)
	}
	if !got["a"].CanToggle || got["a"].EnvKeys[0] != "K" {
		t.Fatalf("a: %+v", got["a"])
	}
	if got["w"].Type != TypeHTTP || !got["w"].CanToggle || got["w"].HeaderKeys[0] != "Authorization" {
		t.Fatalf("w: %+v", got["w"])
	}
	if got["t"].CanToggle || got["t"].Note != "extra_settings" {
		t.Fatalf("t with timeout/env_vars must not be toggleable: %+v", got["t"])
	}
	if got["off"].Enabled || !got["off"].ReadOnly {
		t.Fatalf("off: %+v", got["off"])
	}
}

func TestCodexArgs(t *testing.T) {
	args := codexAddArgs(Spec{Name: "n", Type: TypeStdio, Command: "npx", Args: []string{"-y", "p"}, Env: map[string]string{"B": "2", "A": "1"}})
	want := "mcp add n --env A=1 --env B=2 -- npx -y p"
	if strings.Join(args, " ") != want {
		t.Fatalf("got %q", strings.Join(args, " "))
	}
	entries, _ := parseCodexList([]byte(codexListSample))
	raw, _ := json.Marshal(entries[1])
	args, _, err := codexRestoreArgs("w", raw)
	if err != nil || strings.Join(args, " ") != "mcp add w --url https://ex.com/mcp --bearer-token-env-var MY_TOKEN" {
		t.Fatalf("restore http: %v %v", args, err)
	}
}

// ── Логика выключения на поддельном CLI ─────────────────────────────────────

// fakeClaude ведёт себя как `claude mcp add-json/remove` над файлом.
func fakeClaude(t *testing.T) runner {
	return func(_ context.Context, tg Target, args ...string) ([]byte, []byte, error) {
		path := claudeConfigFile(tg)
		var f map[string]any
		if b, err := os.ReadFile(path); err == nil {
			_ = json.Unmarshal(b, &f)
		}
		if f == nil {
			f = map[string]any{}
		}
		servers, _ := f["mcpServers"].(map[string]any)
		if servers == nil {
			servers = map[string]any{}
		}
		switch {
		case len(args) == 6 && args[1] == "add-json":
			if _, ok := servers[args[4]]; ok {
				return nil, []byte("already exists"), errors.New("exit 1")
			}
			var def any
			if err := json.Unmarshal([]byte(args[5]), &def); err != nil {
				return nil, []byte("bad json"), err
			}
			servers[args[4]] = def
		case len(args) == 5 && args[1] == "remove":
			if _, ok := servers[args[4]]; !ok {
				return nil, []byte("No MCP server"), errors.New("exit 1")
			}
			delete(servers, args[4])
		default:
			t.Fatalf("unexpected args %v", args)
		}
		f["mcpServers"] = servers
		b, _ := json.Marshal(f)
		return nil, nil, os.WriteFile(path, b, 0o600)
	}
}

func TestToggleKeepsDefinition(t *testing.T) {
	dir := t.TempDir()
	m := &Manager{Store: &Store{Path: filepath.Join(dir, "mcp-disabled.json")}, run: fakeClaude(t)}
	tg := Target{Agent: "claude", AccountID: "default", Home: dir}
	ctx := context.Background()
	spec := Spec{Name: "demo", Command: "npx", Args: []string{"-y", "pkg"}, Env: map[string]string{"API_KEY": "sekret-1"}}
	if err := m.Add(ctx, tg, spec); err != nil {
		t.Fatal(err)
	}
	if err := m.Add(ctx, tg, spec); !errors.Is(err, ErrExists) {
		t.Fatalf("duplicate must be ErrExists, got %v", err)
	}
	if err := m.SetEnabled(ctx, tg, "demo", false); err != nil {
		t.Fatal(err)
	}
	// Из конфига агента ушёл…
	if raw, _ := claudeUserDef(tg, "demo"); raw != nil {
		t.Fatalf("still in agent config after disable")
	}
	// …а в списке остался выключенным и без секрета.
	list, err := m.List(ctx, tg)
	if err != nil || len(list) != 1 || list[0].Enabled || !list[0].CanToggle {
		t.Fatalf("list after disable: %+v %v", list, err)
	}
	blob, _ := json.Marshal(list)
	if strings.Contains(string(blob), "sekret-1") {
		t.Fatalf("secret leaked from store: %s", blob)
	}
	// Файл хранилища зашифрован: секрета в нём открытым текстом нет.
	if b, _ := os.ReadFile(m.Store.Path); strings.Contains(string(b), "sekret-1") || len(b) == 0 {
		t.Fatalf("store must be encrypted")
	}
	// Имя занято выключенным — добавить второй такой нельзя.
	if err := m.Add(ctx, tg, spec); !errors.Is(err, ErrExists) {
		t.Fatalf("name of disabled server must be taken, got %v", err)
	}
	if err := m.SetEnabled(ctx, tg, "demo", true); err != nil {
		t.Fatal(err)
	}
	raw, _ := claudeUserDef(tg, "demo")
	var back claudeEntry
	_ = json.Unmarshal(raw, &back)
	if back.Env["API_KEY"] != "sekret-1" || back.Command != "npx" || len(back.Args) != 2 {
		t.Fatalf("definition not restored: %+v", back)
	}
	if d, _ := m.Store.Get("claude", "default", "demo"); d != nil {
		t.Fatalf("store must forget after enable")
	}
	// Удаление выключенного — только из хранилища.
	_ = m.SetEnabled(ctx, tg, "demo", false)
	if err := m.Remove(ctx, tg, "demo"); err != nil {
		t.Fatal(err)
	}
	if list, _ := m.List(ctx, tg); len(list) != 0 {
		t.Fatalf("after remove: %+v", list)
	}
	// Другой аккаунт не видит выключенных чужого.
	_ = m.Add(ctx, tg, spec)
	_ = m.SetEnabled(ctx, tg, "demo", false)
	if l, _ := m.Store.List("claude", "acc-2"); len(l) != 0 {
		t.Fatalf("store leaks between accounts")
	}
}

func TestDisableRollsBackWhenCLIFails(t *testing.T) {
	dir := t.TempDir()
	base := fakeClaude(t)
	failRemove := func(ctx context.Context, tg Target, args ...string) ([]byte, []byte, error) {
		if args[1] == "remove" {
			return nil, []byte("boom"), errors.New("exit 1")
		}
		return base(ctx, tg, args...)
	}
	m := &Manager{Store: &Store{Path: filepath.Join(dir, "s.json")}, run: failRemove}
	tg := Target{Agent: "claude", AccountID: "default", Home: dir}
	if err := m.Add(context.Background(), tg, Spec{Name: "x", Command: "c"}); err != nil {
		t.Fatal(err)
	}
	if err := m.SetEnabled(context.Background(), tg, "x", false); err == nil {
		t.Fatalf("want error")
	}
	if d, _ := m.Store.Get("claude", "default", "x"); d != nil {
		t.Fatalf("store must be rolled back")
	}
}

func TestProjectServerIsReadOnly(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, ".claude.json"), []byte(`{"projects":{"C:/w":{"mcpServers":{"p":{"command":"x"}}}}}`), 0o600)
	m := &Manager{Store: &Store{Path: filepath.Join(dir, "s.json")}, run: fakeClaude(t)}
	tg := Target{Agent: "claude", AccountID: "default", Home: dir}
	if err := m.SetEnabled(context.Background(), tg, "p", false); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("want ErrReadOnly, got %v", err)
	}
	if err := m.Remove(context.Background(), tg, "p"); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("want ErrReadOnly, got %v", err)
	}
}

func TestChildEnvDropsInheritedAccount(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", "/somewhere/else")
	env := Target{Agent: "claude", Home: "/h"}.childEnv()
	for _, kv := range env {
		if strings.HasPrefix(kv, "CLAUDE_CONFIG_DIR=") {
			t.Fatalf("inherited account var must be dropped for main account: %s", kv)
		}
	}
	env = Target{Agent: "claude", Home: "/h", ConfigDir: "/acc"}.childEnv()
	found := false
	for _, kv := range env {
		if kv == "CLAUDE_CONFIG_DIR=/acc" {
			found = true
		}
	}
	if !found {
		t.Fatalf("account var missing")
	}
}

// Скептик 29.09: ключ в пути адреса, JWT и `user:token` в аргументах и имя,
// которое CLI принял бы за флаг.
func TestMaskingPathTokensJWTAndDashName(t *testing.T) {
	got := MaskURL("https://mcp.zapier.com/api/mcp/s/Zk9aXc12ab34CD56ef78GH90ij/mcp?x=1")
	if strings.Contains(got, "Zk9aXc12ab34CD56ef78GH90ij") || !strings.Contains(got, Mask) {
		t.Fatalf("ключ в пути не спрятан: %s", got)
	}
	if !strings.HasPrefix(got, "https://mcp.zapier.com/api/mcp/s/") {
		t.Fatalf("остальной адрес должен остаться читаемым: %s", got)
	}
	args := MaskArgs([]string{"eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.abcDEF123", "admin:s3cr3tT0kenValue1234567"})
	for _, a := range args {
		if a != Mask {
			t.Fatalf("JWT и user:token должны прятаться: %v", args)
		}
	}
	if nameRe.MatchString("-h") || nameRe.MatchString("--debug") || !nameRe.MatchString("github_1") {
		t.Fatal("имя MCP не должно начинаться с «-»")
	}
}
