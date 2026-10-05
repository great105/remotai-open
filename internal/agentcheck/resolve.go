// Package agentcheck отвечает на два вопроса владельца: «КУДА и ЧЕМ на самом
// деле ходит мой агент» и «работает ли это прямо сейчас».
//
// ЗАЧЕМ. Подключение CLI-агента складывается из четырёх мест, и ни одно из них
// человек не видит целиком: окружение процесса Remotai (системные переменные и
// .env), аккаунт (каталог профиля и прокси), настройки Remotai (ключ OpenRouter)
// и конфиг самого CLI (settings.json у Claude Code, config.toml у Codex). Когда
// что-то из этого перекрывает другое, агент молча уходит не туда — а на экране
// по-прежнему «подписка Max».
//
// ГЛАВНОЕ ПРАВИЛО: СЕКРЕТ НЕ ВЫХОДИТ НАРУЖУ. Ключ живёт в неэкспортируемых
// полях Connection и нужен только самой проверке. В JSON уходят источник
// («переменная окружения системы», «аккаунт Работа») и маска «sk-…a1b2».
// Тест TestConnectionJSONHasNoSecret падает, если ключ просочился.
//
// ПОРЯДОК ИСТОЧНИКОВ — НЕ ИЗ ДОКУМЕНТАЦИИ, А ЗАМЕРЕН (29.09.2026):
//   - Claude Code 2.1.284: `env` из settings.json каталога аккаунта СИЛЬНЕЕ
//     окружения процесса (ANTHROPIC_BASE_URL из settings.json победил такой же
//     из окружения — запросы пришли на адрес из файла); ANTHROPIC_MODEL из
//     окружения сильнее `model` из settings.json;
//   - codex-cli 0.158.0: OPENAI_BASE_URL и OPENAI_API_KEY из окружения НЕ
//     действуют вовсе (запрос ушёл на api.openai.com без ключа), адрес задаёт
//     `openai_base_url` в config.toml, свой провайдер — `[model_providers.*]`.
//
// Поэтому порядок у каждого CLI свой, и каждое значение в ответе несёт, откуда
// оно и почему победило (`why`), а проигравшие лежат рядом (`overridden`).
package agentcheck

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// Коды источников. Человеческие слова к ним — в клиенте (i18n agentCheck.src.*).
const (
	SrcSystemEnv       = "system_env"       // переменная окружения системы (унаследована Remotai)
	SrcEnvFile         = "env_file"         // файл .env Remotai
	SrcOpenRouterStore = "openrouter_store" // ключ, сохранённый в Remotai («OpenRouter в Remotai»)
	SrcAccount         = "account"          // аккаунт Remotai (каталог профиля, прокси)
	SrcCLISettings     = "cli_settings"     // settings.json Claude Code
	SrcCLIConfig       = "cli_config"       // config.toml Codex
	SrcCLILogin        = "cli_login"        // вход по подписке, сделанный в самом CLI
	SrcVendorDefault   = "vendor_default"   // адрес вендора по умолчанию
	SrcCLIDefault      = "cli_default"      // CLI выбирает сам
	SrcRemotaiSettings = "remotai_settings" // настройки Remotai (модель OpenRouter)
)

// Коды «почему победило».
const (
	WhyOnly            = "only"              // других источников нет
	WhyDefault         = "default"           // ничего не задано — работает умолчание
	WhySettingsOverEnv = "settings_over_env" // Claude подставляет env из settings.json поверх окружения
	WhyAccountOverEnv  = "account_over_env"  // запуск аккаунта ставит переменную сам
	WhyEnvOverSettings = "env_over_settings" // ANTHROPIC_MODEL сильнее model в settings.json
	WhyTokenOverKey    = "token_over_key"    // ANTHROPIC_AUTH_TOKEN сильнее ANTHROPIC_API_KEY
	WhyKeyOverLogin    = "key_over_login"    // ключ сильнее входа по подписке
	WhyProfile         = "profile"           // выбран профиль config.toml
	WhySystemOverStore = "system_over_store" // системная переменная сильнее ключа из Remotai
	WhyCLIReported     = "cli_reported"      // так ответил сам CLI (auth status / login status)
)

// Почему проигравшее значение не действует вовсе (а не просто перекрыто).
const (
	IgnNotReadByCLI = "not_read_by_cli" // CLI эту переменную не читает (замерено)
	IgnExecOnly     = "exec_only"       // читает только неинтерактивный запуск
	IgnDroppedByRun = "dropped_by_launch"
)

// Source — откуда значение. Пути и имена переменных — не секрет.
type Source struct {
	Kind    string `json:"kind"`
	Name    string `json:"name,omitempty"`
	Path    string `json:"path,omitempty"`
	Account string `json:"account,omitempty"`
}

// Shadow — значение, которое тоже задано, но НЕ действует.
type Shadow struct {
	Value   string `json:"value,omitempty"`
	Source  Source `json:"source"`
	Ignored string `json:"ignored,omitempty"`
}

// Item — одно действующее значение подключения.
type Item struct {
	Value      string   `json:"value"`
	Kind       string   `json:"kind,omitempty"`
	Source     Source   `json:"source"`
	Why        string   `json:"why,omitempty"`
	Overridden []Shadow `json:"overridden,omitempty"`
}

// AccountRef — под каким аккаунтом Remotai смотрим.
type AccountRef struct {
	ID        string `json:"id"`
	Label     string `json:"label,omitempty"`
	IsDefault bool   `json:"is_default"`
}

// CLIReport — что про вход сказал САМ CLI (`claude auth status`,
// `codex login status`). Секретов там нет: способ, тариф, маска.
type CLIReport struct {
	Asked    bool   `json:"asked"`
	LoggedIn *bool  `json:"logged_in,omitempty"`
	Method   string `json:"method,omitempty"`
	Plan     string `json:"plan,omitempty"`
}

// Connection — действующее подключение агента, как он будет запущен.
type Connection struct {
	AgentID   string     `json:"agent_id"`
	AgentName string     `json:"agent_name,omitempty"`
	Supported bool       `json:"supported"`
	Account   AccountRef `json:"account"`
	// Route — «subscription» | «api_key» | «openrouter» | «custom» | «cloud» |
	// «local» | «none» | «unknown».
	Route     string    `json:"route"`
	Provider  string    `json:"provider,omitempty"`
	Endpoint  Item      `json:"endpoint"`
	Auth      Item      `json:"auth"`
	Model     Item      `json:"model"`
	Proxy     string    `json:"proxy,omitempty"`
	LoginFile *bool     `json:"login_file,omitempty"`
	CLI       CLIReport `json:"cli"`
	// Check — чем проверим: «http» (прямой запрос с ключом), «cli» (разовый
	// запуск агента), «» (проверить нельзя).
	Check string   `json:"check"`
	Notes []string `json:"notes,omitempty"`

	// Всё ниже — для самой проверки и НИКОГДА не сериализуется.
	secret     string // ключ/токен
	authHeader string // "x-api-key" | "bearer"
	baseURL    string // полный адрес, без маскировки
	wire       string // "anthropic" | "chat" | "responses"
	model      string // модель для проверки ("" = CLI решает сам)
}

// Secret — есть ли у подключения ключ (для тестов и web-слоя; значение не
// отдаётся никогда).
func (c *Connection) hasSecret() bool { return c != nil && c.secret != "" }

// EnvPair — переменная, которую ставит запуск аккаунта.
type EnvPair struct {
	Name  string
	Value string
}

// Account — аккаунт Remotai, под которым запускают агента.
type Account struct {
	ID        string
	Label     string
	IsDefault bool
	// Pairs — ровно то, что ставит запуск (accountEnvPairs в web): каталог
	// профиля и переменные прокси.
	Pairs      []EnvPair
	ProxyLabel string
}

// CLIStatus — разобранный ответ CLI о входе.
type CLIStatus struct {
	LoggedIn     bool
	Method       string // claude: claude.ai | api_key | oauth_token | none; codex: chatgpt | api_key | none
	APIKeySource string // claude: откуда ключ (имя переменной / apiKeyHelper)
	Plan         string // claude: max | pro | team …
	MaskedKey    string // codex: маска из «Logged in using an API key - …»
}

// Prober спрашивает сам CLI о входе с окружением запуска. nil — не спрашивать.
type Prober func(ctx context.Context, agentID, cli string, env []string) (CLIStatus, error)

// Input — всё, из чего складывается подключение.
type Input struct {
	AgentID   string
	AgentName string
	// ConfigDir — каталог конфига CLI под этим аккаунтом (каталог аккаунта или
	// основной ~/.claude, ~/.codex).
	ConfigDir string
	Account   Account
	// Getenv — окружение процесса Remotai (его унаследует терминал).
	Getenv func(string) (string, bool)
	// EnvFile — содержимое .env Remotai: только чтобы назвать источник.
	EnvFile map[string]string
	// OpenRouterKey / OpenRouterModel — настройки OpenRouter в Remotai.
	OpenRouterKey   string
	OpenRouterModel string
	// CLI — путь к бинарю агента ("" = не установлен).
	CLI string
	// LaunchEnv — окружение, с которым агент будет запущен (для Prober).
	LaunchEnv []string
	Probe     Prober
	// SupportsOpenRouterModel — у агента есть проверенный флаг модели, и
	// Remotai подставляет ему модель OpenRouter (opencode, grok).
	SupportsOpenRouterModel bool
}

// Адреса вендоров по умолчанию. Переменные — ради тестов.
var (
	AnthropicDefaultURL  = "https://api.anthropic.com"
	OpenAIDefaultURL     = "https://api.openai.com/v1"
	ChatGPTBackendURL    = "https://chatgpt.com/backend-api/codex"
	OpenRouterDefaultURL = "https://openrouter.ai/api/v1"
)

// OpenRouterEnv — переменная ключа OpenRouter (internal/openrouter.EnvName).
const OpenRouterEnv = "OPENROUTER_API_KEY"

// ProxyEnvNames — что запуск Remotai СНИМАЕТ перед агентом (agentLaunch.ts:
// WINDOWS_ACCOUNT_ENV_SCOPE / POSIX_ACCOUNT_ENV_SCOPE). Системный прокси до
// агента не доходит: ходит он напрямую или через прокси аккаунта.
var ProxyEnvNames = []string{
	"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY",
	"SOCKS_PROXY", "SOCKS5_PROXY", "NODE_USE_ENV_PROXY",
	"http_proxy", "https_proxy", "all_proxy", "no_proxy",
	"socks_proxy", "socks5_proxy", "node_use_env_proxy",
}

// LaunchEnv — окружение, с которым Remotai запустит агента: окружение процесса
// без снимаемых имён (каталог профиля, прокси) плюс пары аккаунта.
func LaunchEnv(base []string, remove []string, pairs []EnvPair) []string {
	drop := map[string]bool{}
	norm := func(s string) string {
		if runtime.GOOS == "windows" {
			return strings.ToUpper(s)
		}
		return s
	}
	for _, n := range remove {
		if n != "" {
			drop[norm(n)] = true
		}
	}
	for _, p := range pairs {
		drop[norm(p.Name)] = true
	}
	out := make([]string, 0, len(base)+len(pairs))
	for _, kv := range base {
		eq := strings.Index(kv, "=")
		if eq <= 0 {
			out = append(out, kv)
			continue
		}
		if drop[norm(kv[:eq])] {
			continue
		}
		out = append(out, kv)
	}
	for _, p := range pairs {
		out = append(out, p.Name+"="+p.Value)
	}
	return out
}

// Resolve складывает действующее подключение.
func Resolve(ctx context.Context, in Input) *Connection {
	c := &Connection{
		AgentID:   in.AgentID,
		AgentName: in.AgentName,
		Account:   AccountRef{ID: in.Account.ID, Label: in.Account.Label, IsDefault: in.Account.IsDefault},
		Proxy:     in.Account.ProxyLabel,
		Route:     "unknown",
	}
	if in.Getenv == nil {
		in.Getenv = func(string) (string, bool) { return "", false }
	}
	// Системный прокси запуск снимает — если он есть, человек должен знать,
	// что агент им НЕ пользуется (VPN всей машины, конечно, остаётся).
	if in.Account.ProxyLabel == "" {
		for _, n := range []string{"HTTPS_PROXY", "HTTP_PROXY", "ALL_PROXY"} {
			if v, ok := in.Getenv(n); ok && strings.TrimSpace(v) != "" {
				c.Notes = append(c.Notes, "system_proxy_dropped")
				break
			}
		}
	}
	switch in.AgentID {
	case "claude":
		resolveClaude(ctx, in, c)
	case "codex":
		resolveCodex(ctx, in, c)
	default:
		if in.SupportsOpenRouterModel {
			resolveOpenRouterAgent(in, c)
		} else {
			c.Supported = false
			c.Notes = append(c.Notes, "agent_not_supported")
		}
	}
	if in.CLI == "" && c.Check == "cli" {
		c.Check = ""
		c.Notes = append(c.Notes, "cli_missing")
	}
	return c
}

// envSource называет, откуда в окружении процесса значение: сохранённый в
// Remotai ключ OpenRouter, файл .env Remotai или система.
func envSource(in Input, name, value string) Source {
	if name == OpenRouterEnv && in.OpenRouterKey != "" && value == in.OpenRouterKey {
		return Source{Kind: SrcOpenRouterStore, Name: name}
	}
	if fv, ok := in.EnvFile[name]; ok && fv == value {
		return Source{Kind: SrcEnvFile, Name: name}
	}
	return Source{Kind: SrcSystemEnv, Name: name}
}

func accountPair(in Input, name string) (string, bool) {
	for _, p := range in.Account.Pairs {
		if strings.EqualFold(p.Name, name) || p.Name == name {
			return p.Value, true
		}
	}
	return "", false
}

// layered — значение переменной по слоям, от сильного к слабому, и все, кто
// проиграл. `settings` — env из конфига CLI (nil, если CLI такого не умеет).
type layerHit struct {
	value string
	src   Source
}

func layered(in Input, name string, settings map[string]string, settingsPath string) []layerHit {
	var hits []layerHit
	if v, ok := settings[name]; ok && strings.TrimSpace(v) != "" {
		hits = append(hits, layerHit{v, Source{Kind: SrcCLISettings, Name: name, Path: settingsPath}})
	}
	if v, ok := accountPair(in, name); ok && strings.TrimSpace(v) != "" {
		hits = append(hits, layerHit{v, Source{Kind: SrcAccount, Name: name, Account: in.Account.Label}})
	}
	if v, ok := in.Getenv(name); ok && strings.TrimSpace(v) != "" {
		hits = append(hits, layerHit{v, envSource(in, name, v)})
	}
	return hits
}

func whyFor(hits []layerHit) string {
	if len(hits) <= 1 {
		return WhyOnly
	}
	switch hits[0].src.Kind {
	case SrcCLISettings:
		return WhySettingsOverEnv
	case SrcAccount:
		return WhyAccountOverEnv
	}
	return WhyOnly
}

func shadowsOf(hits []layerHit, secret bool) []Shadow {
	var out []Shadow
	for _, h := range hits {
		v := h.value
		if secret {
			v = Mask(v)
		} else if strings.Contains(v, "://") {
			v = DisplayURL(v)
		}
		out = append(out, Shadow{Value: v, Source: h.src})
	}
	return out
}

func boolPtr(b bool) *bool { return &b }

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

// ── Claude Code ─────────────────────────────────────────────────────

type claudeSettings struct {
	Env          map[string]string
	Model        string
	APIKeyHelper bool
}

func readClaudeSettings(path string) claudeSettings {
	var raw struct {
		Env          map[string]any `json:"env"`
		Model        string         `json:"model"`
		APIKeyHelper string         `json:"apiKeyHelper"`
	}
	out := claudeSettings{Env: map[string]string{}}
	data, err := os.ReadFile(path)
	if err != nil || json.Unmarshal(data, &raw) != nil {
		return out
	}
	for k, v := range raw.Env {
		if s, ok := v.(string); ok {
			out.Env[k] = s
		}
	}
	out.Model = raw.Model
	out.APIKeyHelper = strings.TrimSpace(raw.APIKeyHelper) != ""
	return out
}

func resolveClaude(ctx context.Context, in Input, c *Connection) {
	c.Supported = true
	c.Provider = "anthropic"
	settingsPath := filepath.Join(in.ConfigDir, "settings.json")
	st := readClaudeSettings(settingsPath)

	// Облачные провайдеры — отдельный мир со своими ключами: адрес и вход
	// там не наши, проверяем только запуском.
	for _, flag := range []string{"CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX", "CLAUDE_CODE_USE_FOUNDRY"} {
		if hits := layered(in, flag, st.Env, settingsPath); len(hits) > 0 && hits[0].value != "0" && !strings.EqualFold(hits[0].value, "false") {
			c.Provider = strings.ToLower(strings.TrimPrefix(flag, "CLAUDE_CODE_USE_"))
			c.Route = "cloud"
			c.Endpoint = Item{Value: c.Provider, Source: hits[0].src, Why: whyFor(hits)}
		}
	}

	// Адрес.
	base := AnthropicDefaultURL
	if c.Route != "cloud" {
		if hits := layered(in, "ANTHROPIC_BASE_URL", st.Env, settingsPath); len(hits) > 0 {
			base = strings.TrimSpace(hits[0].value)
			c.Endpoint = Item{Value: DisplayURL(base), Source: hits[0].src, Why: whyFor(hits), Overridden: shadowsOf(hits[1:], false)}
		} else {
			c.Endpoint = Item{Value: AnthropicDefaultURL, Source: Source{Kind: SrcVendorDefault}, Why: WhyDefault}
		}
		if Host(base) == "openrouter.ai" {
			c.Provider = "openrouter"
		} else if !strings.EqualFold(Host(base), Host(AnthropicDefaultURL)) {
			c.Provider = "custom"
		}
	}
	c.baseURL = base
	c.wire = "anthropic"

	// Вход. Порядок Claude Code: токен (Bearer) → ключ → помощник ключа →
	// долгоживущий токен подписки → вход по подписке.
	tokenHits := layered(in, "ANTHROPIC_AUTH_TOKEN", st.Env, settingsPath)
	keyHits := layered(in, "ANTHROPIC_API_KEY", st.Env, settingsPath)
	oauthHits := layered(in, "CLAUDE_CODE_OAUTH_TOKEN", st.Env, settingsPath)
	credFile := filepath.Join(in.ConfigDir, ".credentials.json")
	hasCred := fileExists(credFile)
	c.LoginFile = boolPtr(hasCred)

	var losers []Shadow
	switch {
	case len(tokenHits) > 0:
		c.secret, c.authHeader = strings.TrimSpace(tokenHits[0].value), "bearer"
		why := whyFor(tokenHits)
		if len(keyHits) > 0 {
			why = WhyTokenOverKey
			losers = append(losers, shadowsOf(keyHits, true)...)
		}
		losers = append(shadowsOf(tokenHits[1:], true), losers...)
		c.Auth = Item{Value: Mask(c.secret), Kind: "bearer", Source: tokenHits[0].src, Why: why}
	case len(keyHits) > 0:
		c.secret, c.authHeader = strings.TrimSpace(keyHits[0].value), "x-api-key"
		losers = shadowsOf(keyHits[1:], true)
		c.Auth = Item{Value: Mask(c.secret), Kind: "api_key", Source: keyHits[0].src, Why: whyFor(keyHits)}
	case st.APIKeyHelper:
		c.Auth = Item{Kind: "helper", Source: Source{Kind: SrcCLISettings, Name: "apiKeyHelper", Path: settingsPath}, Why: WhyOnly}
	case len(oauthHits) > 0:
		c.Auth = Item{Value: Mask(oauthHits[0].value), Kind: "token", Source: oauthHits[0].src, Why: whyFor(oauthHits)}
	case hasCred:
		c.Auth = Item{Kind: "subscription", Source: Source{Kind: SrcCLILogin, Path: credFile}, Why: WhyOnly}
	default:
		c.Auth = Item{Kind: "none", Source: Source{Kind: SrcCLILogin, Path: credFile}, Why: WhyDefault}
	}
	if (c.Auth.Kind == "bearer" || c.Auth.Kind == "api_key") && hasCred {
		// Ключ есть, а рядом лежит и вход по подписке: подписка НЕ действует.
		// Самая дорогая путаница — человек платит за Max, а агент тратит ключ.
		losers = append(losers, Shadow{Source: Source{Kind: SrcCLILogin, Path: credFile}})
		if c.Auth.Why == WhyOnly {
			c.Auth.Why = WhyKeyOverLogin
		}
	}
	c.Auth.Overridden = losers

	// Что говорит сам CLI.
	if in.Probe != nil && in.CLI != "" {
		st2, err := in.Probe(ctx, "claude", in.CLI, in.LaunchEnv)
		if err == nil {
			c.CLI = CLIReport{Asked: true, LoggedIn: boolPtr(st2.LoggedIn), Method: st2.Method, Plan: st2.Plan}
			switch {
			case !st2.LoggedIn && c.Auth.Kind == "subscription":
				// Файл входа есть, а CLI говорит «не вошли» — вход протух.
				c.Auth.Kind = "none"
				c.Notes = append(c.Notes, "login_file_but_logged_out")
			case st2.LoggedIn && st2.Method == "claude.ai" && c.Auth.Kind == "none":
				// Вход лежит не в файле (связка ключей macOS) — верим CLI.
				c.Auth = Item{Kind: "subscription", Source: Source{Kind: SrcCLILogin}, Why: WhyCLIReported}
			}
			if c.Auth.Kind == "subscription" {
				c.Auth.Value = st2.Plan
			}
		}
	}

	// Модель: env из settings.json → аккаунт → окружение → model в settings.json.
	if hits := layered(in, "ANTHROPIC_MODEL", st.Env, settingsPath); len(hits) > 0 {
		c.model = strings.TrimSpace(hits[0].value)
		why := whyFor(hits)
		over := shadowsOf(hits[1:], false)
		if st.Model != "" {
			if why == WhyOnly {
				why = WhyEnvOverSettings
			}
			over = append(over, Shadow{Value: st.Model, Source: Source{Kind: SrcCLISettings, Name: "model", Path: settingsPath}})
		}
		c.Model = Item{Value: c.model, Source: hits[0].src, Why: why, Overridden: over}
	} else if st.Model != "" {
		c.model = st.Model
		c.Model = Item{Value: st.Model, Source: Source{Kind: SrcCLISettings, Name: "model", Path: settingsPath}, Why: WhyOnly}
	} else {
		c.Model = Item{Source: Source{Kind: SrcCLIDefault}, Why: WhyDefault}
	}

	// Маршрут и способ проверки.
	switch {
	case c.Route == "cloud":
		c.Check = "cli"
	case c.Auth.Kind == "bearer" || c.Auth.Kind == "api_key":
		switch c.Provider {
		case "openrouter":
			c.Route = "openrouter"
		case "custom":
			c.Route = "custom"
		default:
			c.Route = "api_key"
		}
		c.Check = "http"
	case c.Auth.Kind == "helper":
		c.Route = "api_key"
		c.Check = "cli"
	case c.Auth.Kind == "subscription" || c.Auth.Kind == "token":
		c.Route = "subscription"
		c.Check = "cli"
	default:
		c.Route = "none"
		c.Check = "cli"
	}
}

// ── Codex ───────────────────────────────────────────────────────────

func resolveCodex(ctx context.Context, in Input, c *Connection) {
	c.Supported = true
	cfgPath := filepath.Join(in.ConfigDir, "config.toml")
	cfg := readCodexConfig(cfgPath)
	cfgSrc := func(name string) Source { return Source{Kind: SrcCLIConfig, Name: name, Path: cfgPath} }

	// Профиль config.toml.
	prof := codexProfile{}
	profName := cfg.Profile
	if profName != "" {
		prof = cfg.Profiles[profName]
	}

	providerID, providerWhy, providerSrc := "openai", WhyDefault, Source{Kind: SrcCLIDefault}
	if prof.ModelProvider != "" {
		providerID, providerWhy, providerSrc = prof.ModelProvider, WhyProfile, cfgSrc("profiles."+profName+".model_provider")
	} else if cfg.ModelProvider != "" {
		providerID, providerWhy, providerSrc = cfg.ModelProvider, WhyOnly, cfgSrc("model_provider")
	}

	// Модель.
	switch {
	case prof.Model != "":
		c.model = prof.Model
		var over []Shadow
		if cfg.Model != "" {
			over = []Shadow{{Value: cfg.Model, Source: cfgSrc("model")}}
		}
		c.Model = Item{Value: prof.Model, Source: cfgSrc("profiles." + profName + ".model"), Why: WhyProfile, Overridden: over}
	case cfg.Model != "":
		c.model = cfg.Model
		c.Model = Item{Value: cfg.Model, Source: cfgSrc("model"), Why: WhyOnly}
	default:
		c.Model = Item{Source: Source{Kind: SrcCLIDefault}, Why: WhyDefault}
	}

	// Переменные, которые Codex НЕ читает (замер 29.09.2026) — показываем как
	// недействующие: человек, выставивший их, уверен, что они работают.
	ignored := func(name string, secret bool) []Shadow {
		v, ok := in.Getenv(name)
		if !ok || strings.TrimSpace(v) == "" {
			return nil
		}
		shown := DisplayURL(v)
		if secret {
			shown = Mask(v)
		}
		return []Shadow{{Value: shown, Source: envSource(in, name, v), Ignored: IgnNotReadByCLI}}
	}

	authFile := filepath.Join(in.ConfigDir, "auth.json")
	hasAuth := fileExists(authFile)
	c.LoginFile = boolPtr(hasAuth)

	switch providerID {
	case "openai":
		c.Provider = "openai"
		base, baseSrc, why := "", Source{Kind: SrcVendorDefault}, WhyDefault
		if cfg.OpenAIBaseURL != "" {
			base, baseSrc, why = cfg.OpenAIBaseURL, cfgSrc("openai_base_url"), WhyOnly
			c.Provider = "custom"
			if Host(base) == "openrouter.ai" {
				c.Provider = "openrouter"
			}
		}
		// Вход спрашиваем у самого Codex: auth.json не читаем вовсе.
		c.Auth = Item{Kind: "unknown", Source: Source{Kind: SrcCLILogin, Path: authFile}, Why: WhyOnly}
		if !hasAuth {
			c.Auth.Kind = "none"
		}
		if in.Probe != nil && in.CLI != "" {
			if st, err := in.Probe(ctx, "codex", in.CLI, in.LaunchEnv); err == nil {
				c.CLI = CLIReport{Asked: true, LoggedIn: boolPtr(st.LoggedIn), Method: st.Method}
				switch {
				case !st.LoggedIn:
					c.Auth.Kind = "none"
				case st.Method == "chatgpt":
					c.Auth.Kind, c.Auth.Why = "subscription", WhyCLIReported
				case st.Method == "api_key":
					c.Auth.Kind, c.Auth.Value, c.Auth.Why = "api_key", st.MaskedKey, WhyCLIReported
				}
			}
		}
		c.Auth.Overridden = ignored("OPENAI_API_KEY", true)
		if v, ok := in.Getenv("CODEX_API_KEY"); ok && strings.TrimSpace(v) != "" {
			c.Auth.Overridden = append(c.Auth.Overridden, Shadow{Value: Mask(v), Source: envSource(in, "CODEX_API_KEY", v), Ignored: IgnExecOnly})
			c.Notes = append(c.Notes, "codex_api_key_exec_only")
		}
		if base == "" {
			if c.Auth.Kind == "subscription" {
				base = ChatGPTBackendURL
			} else {
				base = OpenAIDefaultURL
			}
		}
		c.Endpoint = Item{Value: DisplayURL(base), Source: baseSrc, Why: why, Overridden: ignored("OPENAI_BASE_URL", false)}
		c.baseURL = base
		switch c.Auth.Kind {
		case "subscription":
			c.Route = "subscription"
		case "api_key":
			c.Route = "api_key"
			if c.Provider != "openai" {
				c.Route = c.Provider
			}
		case "none":
			c.Route = "none"
		}
		// Ключ из auth.json мы не читаем — значит и прямой запрос сделать
		// нечем. Проверка — разовым `codex exec` под этим аккаунтом.
		c.Check = "cli"

	case "oss", "ollama", "lmstudio":
		c.Provider = "local"
		c.Route = "local"
		c.Endpoint = Item{Value: providerID, Source: providerSrc, Why: providerWhy}
		c.Auth = Item{Kind: "none", Source: Source{Kind: SrcCLIDefault}, Why: WhyDefault}
		c.Check = "cli"

	default:
		p, ok := cfg.Providers[providerID]
		if !ok || p.BaseURL == "" {
			c.Provider = "custom"
			c.Route = "unknown"
			c.Endpoint = Item{Value: providerID, Source: providerSrc, Why: providerWhy}
			c.Auth = Item{Kind: "unknown", Source: Source{Kind: SrcCLIConfig, Path: cfgPath}}
			c.Notes = append(c.Notes, "codex_provider_unknown")
			c.Check = "cli"
			return
		}
		c.baseURL = p.BaseURL
		c.Provider = "custom"
		if Host(p.BaseURL) == "openrouter.ai" {
			c.Provider = "openrouter"
		}
		c.Endpoint = Item{Value: DisplayURL(p.BaseURL), Source: cfgSrc("model_providers." + providerID + ".base_url"), Why: providerWhy,
			Overridden: ignored("OPENAI_BASE_URL", false)}
		c.wire = "chat"
		if strings.EqualFold(p.WireAPI, "responses") {
			c.wire = "responses"
		}
		if p.EnvKey == "" {
			c.Auth = Item{Kind: "none", Source: cfgSrc("model_providers." + providerID + ".env_key"), Why: WhyDefault}
			c.Route = c.Provider
			c.Check = "cli"
			return
		}
		hits := layered(in, p.EnvKey, nil, "")
		if len(hits) == 0 {
			c.Auth = Item{Kind: "none", Source: Source{Kind: SrcSystemEnv, Name: p.EnvKey}, Why: WhyDefault}
			c.Route = c.Provider
			c.Notes = append(c.Notes, "env_key_missing")
			c.Check = "cli"
			return
		}
		c.secret, c.authHeader = strings.TrimSpace(hits[0].value), "bearer"
		c.Auth = Item{Value: Mask(c.secret), Kind: "api_key", Source: hits[0].src, Why: whyFor(hits), Overridden: shadowsOf(hits[1:], true)}
		c.Route = c.Provider
		if c.model == "" {
			// Модели нет — прямой запрос делать не к чему, Codex выберет сам.
			c.Check = "cli"
			return
		}
		c.Check = "http"
	}
}

// ── Агенты с моделью OpenRouter (opencode, grok) ────────────────────

func resolveOpenRouterAgent(in Input, c *Connection) {
	c.Supported = true
	c.Provider = "openrouter"
	hits := layered(in, OpenRouterEnv, nil, "")
	if len(hits) == 0 {
		c.Route = "unknown"
		c.Endpoint = Item{Source: Source{Kind: SrcCLIDefault}, Why: WhyDefault}
		c.Auth = Item{Kind: "unknown", Source: Source{Kind: SrcCLIDefault}}
		c.Model = Item{Source: Source{Kind: SrcCLIDefault}, Why: WhyDefault}
		c.Notes = append(c.Notes, "openrouter_not_configured")
		return
	}
	c.Route = "openrouter"
	c.baseURL = OpenRouterDefaultURL
	c.wire = "chat"
	c.Endpoint = Item{Value: DisplayURL(OpenRouterDefaultURL), Source: Source{Kind: SrcRemotaiSettings}, Why: WhyOnly}
	c.secret, c.authHeader = strings.TrimSpace(hits[0].value), "bearer"
	why := whyFor(hits)
	var over []Shadow
	if hits[0].src.Kind != SrcOpenRouterStore && in.OpenRouterKey != "" && in.OpenRouterKey != hits[0].value {
		// Store.applyEnv уважает системную переменную: сохранённый ключ
		// в этом случае не действует.
		why = WhySystemOverStore
		over = append(over, Shadow{Value: Mask(in.OpenRouterKey), Source: Source{Kind: SrcOpenRouterStore, Name: OpenRouterEnv}})
	}
	c.Auth = Item{Value: Mask(c.secret), Kind: "api_key", Source: hits[0].src, Why: why, Overridden: over}
	if in.OpenRouterModel != "" {
		c.model = in.OpenRouterModel
		c.Model = Item{Value: in.OpenRouterModel, Source: Source{Kind: SrcRemotaiSettings, Name: "model"}, Why: WhyOnly}
	} else {
		c.Model = Item{Source: Source{Kind: SrcCLIDefault}, Why: WhyDefault}
		c.Notes = append(c.Notes, "openrouter_model_unset")
	}
	c.Check = "http"
}
