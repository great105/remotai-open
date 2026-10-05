package agentcheck

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const testKey = "sk-ant-api03-SECRETSECRETSECRET-a1b2"

func envMap(m map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) {
		v, ok := m[k]
		return v, ok
	}
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// ── Приоритеты источников ───────────────────────────────────────────

func TestClaudeSettingsEnvBeatsProcessEnv(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "settings.json"),
		`{"env":{"ANTHROPIC_BASE_URL":"https://openrouter.ai/api"},"model":"claude-from-settings"}`)
	c := Resolve(context.Background(), Input{
		AgentID: "claude", ConfigDir: dir,
		Account: Account{ID: "default", IsDefault: true},
		Getenv: envMap(map[string]string{
			"ANTHROPIC_BASE_URL":   "http://127.0.0.1:9999",
			"ANTHROPIC_AUTH_TOKEN": testKey,
			"ANTHROPIC_MODEL":      "claude-from-env",
		}),
	})
	if c.Endpoint.Value != "https://openrouter.ai/api" || c.Endpoint.Source.Kind != SrcCLISettings || c.Endpoint.Why != WhySettingsOverEnv {
		t.Fatalf("endpoint = %+v", c.Endpoint)
	}
	if len(c.Endpoint.Overridden) != 1 || c.Endpoint.Overridden[0].Source.Kind != SrcSystemEnv {
		t.Fatalf("проигравший адрес из окружения не показан: %+v", c.Endpoint.Overridden)
	}
	if c.Model.Value != "claude-from-env" || c.Model.Why != WhyEnvOverSettings {
		t.Fatalf("model = %+v", c.Model)
	}
	if c.Route != "openrouter" || c.Check != "http" || c.Auth.Kind != "bearer" {
		t.Fatalf("route=%s check=%s auth=%s", c.Route, c.Check, c.Auth.Kind)
	}
	if c.Auth.Value != "sk-…a1b2" {
		t.Fatalf("маска = %q", c.Auth.Value)
	}
}

func TestClaudeTokenBeatsKeyAndKeyBeatsLogin(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, ".credentials.json"), `{}`)
	c := Resolve(context.Background(), Input{
		AgentID: "claude", ConfigDir: dir,
		Getenv: envMap(map[string]string{"ANTHROPIC_API_KEY": testKey}),
	})
	if c.Auth.Kind != "api_key" || c.Auth.Why != WhyKeyOverLogin || c.Route != "api_key" {
		t.Fatalf("ключ должен победить вход по подписке: %+v route=%s", c.Auth, c.Route)
	}
	found := false
	for _, s := range c.Auth.Overridden {
		if s.Source.Kind == SrcCLILogin {
			found = true
		}
	}
	if !found {
		t.Fatalf("перекрытый вход по подписке не показан: %+v", c.Auth.Overridden)
	}

	c = Resolve(context.Background(), Input{
		AgentID: "claude", ConfigDir: dir,
		Getenv: envMap(map[string]string{"ANTHROPIC_API_KEY": testKey, "ANTHROPIC_AUTH_TOKEN": "sk-other-token-000011112222"}),
	})
	if c.Auth.Kind != "bearer" || c.Auth.Why != WhyTokenOverKey {
		t.Fatalf("токен должен победить ключ: %+v", c.Auth)
	}
}

func TestClaudeSubscriptionAndCLIReport(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, ".credentials.json"), `{"secret":"never-read"}`)
	probe := func(ctx context.Context, agentID, cli string, env []string) (CLIStatus, error) {
		return CLIStatus{LoggedIn: true, Method: "claude.ai", Plan: "max"}, nil
	}
	c := Resolve(context.Background(), Input{AgentID: "claude", ConfigDir: dir, CLI: "claude", Probe: probe})
	if c.Route != "subscription" || c.Check != "cli" || c.Auth.Value != "max" || !c.CLI.Asked {
		t.Fatalf("подписка: %+v", c)
	}
	if c.Endpoint.Source.Kind != SrcVendorDefault || c.Model.Source.Kind != SrcCLIDefault {
		t.Fatalf("умолчания: %+v %+v", c.Endpoint, c.Model)
	}

	// Файл входа есть, а CLI говорит «не вошли» — не рисуем подписку.
	out := func(ctx context.Context, agentID, cli string, env []string) (CLIStatus, error) {
		return CLIStatus{LoggedIn: false, Method: "none"}, nil
	}
	c = Resolve(context.Background(), Input{AgentID: "claude", ConfigDir: dir, CLI: "claude", Probe: out})
	if c.Auth.Kind != "none" || c.Route != "none" {
		t.Fatalf("протухший вход: %+v route=%s", c.Auth, c.Route)
	}
}

func TestAccountEnvBeatsProcessEnv(t *testing.T) {
	c := Resolve(context.Background(), Input{
		AgentID: "claude", ConfigDir: t.TempDir(),
		Account: Account{ID: "acc-1", Label: "Работа", Pairs: []EnvPair{{"ANTHROPIC_BASE_URL", "https://proxy.example.com/"}}},
		Getenv:  envMap(map[string]string{"ANTHROPIC_BASE_URL": "https://sys.example.com", "ANTHROPIC_API_KEY": testKey}),
	})
	if c.Endpoint.Source.Kind != SrcAccount || c.Endpoint.Source.Account != "Работа" || c.Endpoint.Why != WhyAccountOverEnv {
		t.Fatalf("endpoint = %+v", c.Endpoint)
	}
	if c.Route != "custom" {
		t.Fatalf("route = %s", c.Route)
	}
}

func TestEnvSourceNamesStoreAndEnvFile(t *testing.T) {
	in := Input{OpenRouterKey: "sk-or-v1-stored-key-0000", EnvFile: map[string]string{"ANTHROPIC_API_KEY": "from-file"}}
	if s := envSource(in, OpenRouterEnv, "sk-or-v1-stored-key-0000"); s.Kind != SrcOpenRouterStore {
		t.Fatalf("store: %+v", s)
	}
	if s := envSource(in, "ANTHROPIC_API_KEY", "from-file"); s.Kind != SrcEnvFile {
		t.Fatalf("env file: %+v", s)
	}
	if s := envSource(in, "ANTHROPIC_API_KEY", "other"); s.Kind != SrcSystemEnv {
		t.Fatalf("system: %+v", s)
	}
}

func TestCodexConfigProfileAndOpenRouterProvider(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "config.toml"), `
model = "gpt-top"
profile = "or"
# комментарий
[profiles.or]
model = "qwen/qwen3-coder:free"
model_provider = "openrouter"

[model_providers.openrouter]
name = "OpenRouter"
base_url = "https://openrouter.ai/api/v1"
env_key = "OPENROUTER_API_KEY"

[mcp_servers.x.env]
TOKEN = "must-not-matter"
`)
	c := Resolve(context.Background(), Input{
		AgentID: "codex", ConfigDir: dir,
		OpenRouterKey: "sk-or-v1-stored-key-9f9f",
		Getenv: envMap(map[string]string{
			"OPENROUTER_API_KEY": "sk-or-v1-stored-key-9f9f",
			"OPENAI_BASE_URL":    "http://127.0.0.1:1234/v1",
		}),
	})
	if c.Route != "openrouter" || c.Check != "http" || c.wire != "chat" {
		t.Fatalf("route=%s check=%s wire=%s", c.Route, c.Check, c.wire)
	}
	if c.Model.Value != "qwen/qwen3-coder:free" || c.Model.Why != WhyProfile || len(c.Model.Overridden) != 1 {
		t.Fatalf("model = %+v", c.Model)
	}
	if c.Auth.Source.Kind != SrcOpenRouterStore || c.Auth.Value != "sk-…9f9f" {
		t.Fatalf("auth = %+v", c.Auth)
	}
	if len(c.Endpoint.Overridden) != 1 || c.Endpoint.Overridden[0].Ignored != IgnNotReadByCLI {
		t.Fatalf("OPENAI_BASE_URL должен быть помечен как недействующий: %+v", c.Endpoint.Overridden)
	}
}

func TestCodexChatGPTLoginAsksCLI(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "auth.json"), `{"tokens":{"access_token":"SECRET"}}`)
	probe := func(ctx context.Context, agentID, cli string, env []string) (CLIStatus, error) {
		return parseCodexStatus([]byte("Logged in using ChatGPT\n"), nil)
	}
	c := Resolve(context.Background(), Input{AgentID: "codex", ConfigDir: dir, CLI: "codex", Probe: probe,
		Getenv: envMap(map[string]string{"OPENAI_API_KEY": "sk-proj-ignoredignored-7777"})})
	if c.Route != "subscription" || c.Check != "cli" || c.Endpoint.Value != ChatGPTBackendURL {
		t.Fatalf("codex chatgpt: route=%s check=%s endpoint=%s", c.Route, c.Check, c.Endpoint.Value)
	}
	if len(c.Auth.Overridden) != 1 || c.Auth.Overridden[0].Ignored != IgnNotReadByCLI {
		t.Fatalf("OPENAI_API_KEY должен быть недействующим: %+v", c.Auth.Overridden)
	}
	st, _ := parseCodexStatus([]byte("Logged in using an API key - sk-proj-***ABCDE\n"), nil)
	if st.Method != "api_key" || st.MaskedKey != "sk-…BCDE" {
		t.Fatalf("api key status: %+v", st)
	}
}

func TestOpenRouterAgentSystemBeatsStore(t *testing.T) {
	c := Resolve(context.Background(), Input{
		AgentID: "opencode", SupportsOpenRouterModel: true,
		OpenRouterKey: "sk-or-v1-stored-key-1111", OpenRouterModel: "openrouter/free",
		Getenv: envMap(map[string]string{OpenRouterEnv: "sk-or-v1-system-key-2222"}),
	})
	if c.Auth.Why != WhySystemOverStore || c.Auth.Value != "sk-…2222" || len(c.Auth.Overridden) != 1 {
		t.Fatalf("auth = %+v", c.Auth)
	}
	if c.Check != "http" || c.Model.Value != "openrouter/free" {
		t.Fatalf("check=%s model=%+v", c.Check, c.Model)
	}
	u := Resolve(context.Background(), Input{AgentID: "gemini"})
	if u.Supported || u.Check != "" {
		t.Fatalf("неизвестный агент не должен обещать проверку: %+v", u)
	}
}

func TestLaunchEnvDropsProxyAndAppliesAccount(t *testing.T) {
	env := LaunchEnv([]string{"PATH=/bin", "HTTPS_PROXY=http://sys:1", "CLAUDE_CONFIG_DIR=/old"},
		append([]string{"CLAUDE_CONFIG_DIR"}, ProxyEnvNames...),
		[]EnvPair{{"CLAUDE_CONFIG_DIR", "/acc"}})
	joined := strings.Join(env, "\n")
	if strings.Contains(joined, "sys:1") || strings.Contains(joined, "/old") || !strings.Contains(joined, "CLAUDE_CONFIG_DIR=/acc") {
		t.Fatalf("env = %v", env)
	}
}

// ── Секреты ─────────────────────────────────────────────────────────

// Падает, если ключ хоть где-то просочился в JSON подключения или результата
// — включая случай, когда провайдер вернул ключ эхом в тексте ошибки.
func TestConnectionJSONHasNoSecret(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "settings.json"), `{"env":{"ANTHROPIC_API_KEY":"`+testKey+`"}}`)
	c := Resolve(context.Background(), Input{
		AgentID: "claude", ConfigDir: dir,
		Getenv: envMap(map[string]string{"ANTHROPIC_API_KEY": "sk-ant-api03-SECONDSECONDSECOND-zz99"}),
	})
	if !c.hasSecret() {
		t.Fatal("ключ должен быть у проверки")
	}
	raw, _ := json.Marshal(c)
	for _, secret := range []string{testKey, "SECRETSECRET", "SECONDSECOND"} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("секрет %q в JSON подключения: %s", secret, raw)
		}
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key ` + r.Header.Get("x-api-key") + `"}}`))
	}))
	defer srv.Close()
	c.baseURL = srv.URL
	res := Run(context.Background(), c, Options{Client: srv.Client()})
	raw, _ = json.Marshal(res)
	if strings.Contains(string(raw), "SECRETSECRET") {
		t.Fatalf("секрет в JSON результата: %s", raw)
	}
	if res.Reason != ReasonKeyRejected || res.HTTPStatus != 401 || res.OK {
		t.Fatalf("401: %+v", res)
	}
}

// ── Настоящий запрос: ответы провайдера ─────────────────────────────

func anthropicConn(base string) *Connection {
	return &Connection{AgentID: "claude", Route: "api_key", Provider: "anthropic", Check: "http",
		secret: testKey, authHeader: "x-api-key", baseURL: base, wire: "anthropic", model: "claude-haiku-4-5"}
}

func TestHTTPCheckResponses(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		wire   string
		ok     bool
		reason string
		reply  string
	}{
		{"anthropic 200", 200, `{"model":"claude-haiku-4-5-20251001","content":[{"type":"text","text":"Ок"}]}`, "anthropic", true, "", "Ок"},
		{"openrouter 200", 200, `{"model":"qwen/qwen3:free","choices":[{"message":{"content":"ок"}}]}`, "chat", true, "", "ок"},
		{"responses 200", 200, `{"model":"gpt-x","output":[{"content":[{"text":"ок"}]}]}`, "responses", true, "", "ок"},
		{"401", 401, `{"error":{"message":"No auth credentials found","code":401}}`, "chat", false, ReasonKeyRejected, ""},
		{"402", 402, `{"error":{"message":"This request requires more credits","code":402}}`, "chat", false, ReasonNoFunds, ""},
		{"openai insufficient_quota", 429, `{"error":{"message":"You exceeded your current quota","type":"insufficient_quota"}}`, "chat", false, ReasonNoFunds, ""},
		{"anthropic credit", 400, `{"type":"error","error":{"type":"invalid_request_error","message":"Your credit balance is too low"}}`, "anthropic", false, ReasonNoFunds, ""},
		{"404 model", 404, `{"type":"error","error":{"type":"not_found_error","message":"model: nope"}}`, "anthropic", false, ReasonModelNotFound, ""},
		{"openrouter bad model", 400, `{"error":{"message":"nope/nope is not a valid model ID","code":400}}`, "chat", false, ReasonModelNotFound, ""},
		{"429", 429, `{"error":{"message":"Rate limit exceeded","code":429}}`, "chat", false, ReasonRateLimited, ""},
		{"200 with error", 200, `{"error":{"message":"No endpoints found for x","code":404}}`, "chat", false, ReasonModelNotFound, ""},
		{"503", 503, `upstream down`, "chat", false, ReasonServerError, ""},
		{"200 empty", 200, `{"choices":[{"message":{"content":""}}]}`, "chat", true, ReasonEmptyReply, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gotPath, gotAuth, gotKey string
			var gotBody map[string]any
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath, gotAuth, gotKey = r.URL.Path, r.Header.Get("Authorization"), r.Header.Get("x-api-key")
				_ = json.NewDecoder(r.Body).Decode(&gotBody)
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			c := anthropicConn(srv.URL)
			c.wire = tc.wire
			if tc.wire != "anthropic" {
				c.authHeader = "bearer"
			}
			res := Run(context.Background(), c, Options{Client: srv.Client()})
			if res.OK != tc.ok || res.Reason != tc.reason || res.HTTPStatus != tc.status {
				t.Fatalf("res = %+v", res)
			}
			if tc.reply != "" && res.Reply != tc.reply {
				t.Fatalf("reply = %q", res.Reply)
			}
			switch tc.wire {
			case "anthropic":
				if gotPath != "/v1/messages" || gotKey != testKey || gotBody["max_tokens"].(float64) > 16 {
					t.Fatalf("anthropic req: %s %q %v", gotPath, gotKey, gotBody)
				}
			case "chat":
				if gotPath != "/chat/completions" || gotAuth != "Bearer "+testKey {
					t.Fatalf("chat req: %s %q", gotPath, gotAuth)
				}
			case "responses":
				if gotPath != "/responses" {
					t.Fatalf("responses req: %s", gotPath)
				}
			}
			if res.LatencyMS < 0 {
				t.Fatalf("latency %d", res.LatencyMS)
			}
		})
	}
}

func TestHTTPCheckTimeoutAndNetwork(t *testing.T) {
	old := HTTPTimeout
	HTTPTimeout = 150 * time.Millisecond
	defer func() { HTTPTimeout = old }()
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	res := Run(context.Background(), anthropicConn(srv.URL), Options{Client: srv.Client()})
	close(release)
	srv.Close()
	if res.OK || res.Reason != ReasonTimeout {
		t.Fatalf("таймаут: %+v", res)
	}

	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := dead.URL
	dead.Close()
	res = Run(context.Background(), anthropicConn(url), Options{})
	if res.OK || res.Reason != ReasonNetwork || res.Host != "127.0.0.1" {
		t.Fatalf("нет сети: %+v", res)
	}
}

func TestHTTPCheckModelOverrideAndNoModel(t *testing.T) {
	var model string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b map[string]any
		_ = json.NewDecoder(r.Body).Decode(&b)
		model, _ = b["model"].(string)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ок"}}]}`))
	}))
	defer srv.Close()
	c := &Connection{Check: "http", Provider: "custom", wire: "chat", baseURL: srv.URL, secret: testKey}
	if res := Run(context.Background(), c, Options{Client: srv.Client()}); res.Reason != ReasonNoModel {
		t.Fatalf("без модели: %+v", res)
	}
	res := Run(context.Background(), c, Options{Client: srv.Client(), Model: "free/model:free"})
	if !res.OK || model != "free/model:free" || res.Model != "free/model:free" {
		t.Fatalf("override: %+v %q", res, model)
	}
}

// ── Разовый запуск CLI ──────────────────────────────────────────────

func fakeRun(stdout, stderr string, err error) Runner {
	return func(ctx context.Context, bin string, args, env []string, dir string) ([]byte, []byte, error) {
		return []byte(stdout), []byte(stderr), err
	}
}

func TestClaudeCLICheck(t *testing.T) {
	c := &Connection{AgentID: "claude", Route: "subscription", Check: "cli", baseURL: AnthropicDefaultURL}
	ok := `{"type":"result","is_error":false,"result":"Ок","modelUsage":{"claude-haiku-4-5":{"outputTokens":3},"claude-opus-5-5":{"outputTokens":9}}}`
	res := Run(context.Background(), c, Options{CLI: "claude", Run: fakeRun(ok, "", nil)})
	if !res.OK || res.Reply != "Ок" || res.Model != "claude-opus-5-5" || res.Via != "cli" {
		t.Fatalf("успех: %+v", res)
	}
	expired := `{"type":"result","is_error":true,"api_error_status":401,"result":"OAuth token has expired. Please obtain a new token or refresh your existing token."}`
	res = Run(context.Background(), c, Options{CLI: "claude", Run: fakeRun(expired, "", nil)})
	if res.OK || res.Reason != ReasonLoginExpired {
		t.Fatalf("протухший вход: %+v", res)
	}
	model := `{"type":"result","is_error":true,"api_error_status":404,"result":"There's an issue with the selected model (x). It may not exist or you may not have access to it."}`
	if res = Run(context.Background(), c, Options{CLI: "claude", Run: fakeRun(model, "", nil)}); res.Reason != ReasonModelNotFound {
		t.Fatalf("модель: %+v", res)
	}
	limit := `{"type":"result","is_error":true,"result":"Claude AI usage limit reached|1759999999"}`
	if res = Run(context.Background(), c, Options{CLI: "claude", Run: fakeRun(limit, "", nil)}); res.Reason != ReasonLimitReached {
		t.Fatalf("лимит: %+v", res)
	}
	if res = Run(context.Background(), c, Options{}); res.Reason != ReasonCLIMissing {
		t.Fatalf("нет CLI: %+v", res)
	}
}

func TestCLITimeout(t *testing.T) {
	old := CLITimeout
	CLITimeout = 50 * time.Millisecond
	defer func() { CLITimeout = old }()
	slow := func(ctx context.Context, bin string, args, env []string, dir string) ([]byte, []byte, error) {
		<-ctx.Done()
		return nil, nil, ctx.Err()
	}
	c := &Connection{AgentID: "claude", Route: "subscription", Check: "cli"}
	if res := Run(context.Background(), c, Options{CLI: "claude", Run: slow}); res.Reason != ReasonTimeout {
		t.Fatalf("таймаут CLI: %+v", res)
	}
}

func TestCodexCLICheck(t *testing.T) {
	c := &Connection{AgentID: "codex", Route: "subscription", Check: "cli", model: "gpt-6-sol", baseURL: ChatGPTBackendURL}
	ok := "{\"type\":\"thread.started\"}\n{\"type\":\"item.completed\",\"item\":{\"type\":\"agent_message\",\"text\":\"ок\"}}\n{\"type\":\"turn.completed\"}\n"
	res := Run(context.Background(), c, Options{CLI: "codex", Run: fakeRun(ok, "WARNING: proceeding", nil)})
	if !res.OK || res.Reply != "ок" || res.Model != "gpt-6-sol" {
		t.Fatalf("успех: %+v", res)
	}
	bad := "{\"type\":\"error\",\"message\":\"Reconnecting... 2/5 (unexpected status 401 Unauthorized: Missing bearer)\"}\n{\"type\":\"turn.failed\",\"error\":{\"message\":\"unexpected status 401 Unauthorized\"}}\n"
	res = Run(context.Background(), c, Options{CLI: "codex", Run: fakeRun(bad, "", nil)})
	if res.OK || res.Reason != ReasonLoginExpired {
		t.Fatalf("401 по подписке: %+v", res)
	}
	limit := "{\"type\":\"turn.failed\",\"error\":{\"message\":\"You've hit your usage limit. Try again later.\"}}\n"
	if res = Run(context.Background(), c, Options{CLI: "codex", Run: fakeRun(limit, "", nil)}); res.Reason != ReasonLimitReached {
		t.Fatalf("лимит: %+v", res)
	}
}

func TestClaudeStatusDropsPersonalData(t *testing.T) {
	st, err := parseClaudeStatus([]byte(`{"loggedIn":true,"authMethod":"claude.ai","email":"a@b.c","orgName":"X","subscriptionType":"max"}`), nil)
	if err != nil || !st.LoggedIn || st.Plan != "max" || st.Method != "claude.ai" {
		t.Fatalf("status = %+v %v", st, err)
	}
}

func TestMaskAndScrub(t *testing.T) {
	if Mask("short") != "…" || Mask("sk-or-v1-abcdefgh1234") != "sk-…1234" {
		t.Fatalf("mask: %q %q", Mask("short"), Mask("sk-or-v1-abcdefgh1234"))
	}
	s := Scrub("bad key sk-live-ABCDEFGHIJKLMNOP and Bearer abcdefghijklmnopqrst", nil, 0)
	if strings.Contains(s, "ABCDEFGH") || strings.Contains(s, "abcdefghijkl") {
		t.Fatalf("scrub: %q", s)
	}
	if DisplayURL("https://user:pass@host.example/api/?key=1#x") != "https://host.example/api" {
		t.Fatalf("display url: %q", DisplayURL("https://user:pass@host.example/api/?key=1#x"))
	}
}
