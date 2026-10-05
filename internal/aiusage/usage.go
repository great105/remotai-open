// Package aiusage collects a privacy-bounded snapshot of AI subscription
// limits from provider clients installed on the same machine as Remotai.
//
// Credentials are used only on the managed machine to query the provider
// itself. They never enter the Remotai response or relay; the public response
// contains only an account label, plan, percentage/reset windows and optional
// spend totals.
package aiusage

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"tgcontrol/internal/procutil"
)

const (
	providerTimeout  = 12 * time.Second
	maxResponseBytes = 1 << 20
)

// Window is one provider quota window. UsedPercent is always 0..100.
type Window struct {
	LimitID         string  `json:"limit_id,omitempty"`
	ID              string  `json:"id"`
	Label           string  `json:"label"`
	UsedPercent     float64 `json:"used_percent"`
	DurationMinutes *int64  `json:"duration_minutes,omitempty"`
	ResetsAt        *int64  `json:"resets_at,omitempty"`
}

// ExtraUsage describes an optional money/credit ceiling. Values are kept in
// the provider's minor units; DecimalPlaces tells the client how to format them.
type ExtraUsage struct {
	Enabled       bool    `json:"enabled"`
	Used          float64 `json:"used"`
	Limit         float64 `json:"limit"`
	UsedPercent   float64 `json:"used_percent"`
	Currency      string  `json:"currency,omitempty"`
	DecimalPlaces int     `json:"decimal_places,omitempty"`
	Reached       bool    `json:"reached,omitempty"`
}

// Machine codes for Provider.MessageCode. Карточка лимитов сейчас печатает
// готовый русский Message, а код нужен клиенту, чтобы решать, что предлагать
// делать (войти, обновить клиент, повторить позже), не разбирая текст. Коды
// стабильные — сравниваются строкой.
const (
	MsgSignIn         = "sign_in_required"     // вход в провайдера не выполнен
	MsgSessionExpired = "session_expired"      // токен есть, но протух
	MsgClientStart    = "client_start_failed"  // локальный клиент не запустился
	MsgClientTalk     = "client_talk_failed"   // клиент запустился, но не отвечает
	MsgClientOld      = "client_outdated"      // версия клиента не умеет сводку лимитов
	MsgNoWindows      = "no_windows"           // клиент не назвал ни одного окна лимита
	MsgNoPercent      = "no_percent_for_login" // для этого типа входа процента нет
	MsgRequestFailed  = "request_failed"       // запрос к провайдеру не собрался
	MsgTimeout        = "provider_timeout"     // провайдер не ответил вовремя
	MsgUnreachable    = "provider_unreachable" // до провайдера не достучались
	MsgProviderError  = "provider_error"       // провайдер ответил ошибкой (любой код HTTP)
	MsgRateLimited    = "rate_limited"
	MsgBadResponse    = "provider_bad_response" // ответ есть, формат незнакомый
	MsgUnsupported    = "client_unsupported"    // клиент вообще не отдаёт лимиты
)

// Account — один аккаунт провайдера на этой машине.
//
// У человека бывает несколько подписок на одного вендора (две Claude, две
// ChatGPT) — их держат ровно затем, чтобы не упираться в лимит. Тогда и
// смотреть остаток надо у КАЖДОЙ: «сколько осталось» без ответа «у кого» не
// помогает выбрать, чем работать дальше.
//
// Dir — каталог, где лежат креды этого аккаунта; пусто = основной, то есть то,
// что настроено на машине по умолчанию. Список приходит снаружи (см.
// SetAccountsSource): здесь мы не знаем и не должны знать, где он хранится.
type Account struct {
	ID       string
	Provider string
	Label    string
	Dir      string
	Active   bool
}

// accountsSource — откуда берётся список аккаунтов. Пусто = только основной:
// пакет обязан работать сам по себе (его дёргают и тесты, и старые сборки).
var accountsSource func() []Account

// SetAccountsSource подключает список аккаунтов машины. Зовётся один раз при
// старте агента; aiusage не знает про хранилище, а хранилище — про сбор лимитов.
func SetAccountsSource(fn func() []Account) { accountsSource = fn }

// accountsFor отдаёт аккаунты одного провайдера, всегда как минимум основной.
func accountsFor(providerID string) []Account {
	if accountsSource == nil {
		return []Account{{ID: "default", Provider: providerID, Active: true}}
	}
	var out []Account
	for _, a := range accountsSource() {
		if a.Provider == providerID {
			out = append(out, a)
		}
	}
	if len(out) == 0 {
		return []Account{{ID: "default", Provider: providerID, Active: true}}
	}
	return out
}

// Provider is a sanitized provider snapshot.
type Provider struct {
	CapturedAt  time.Time  `json:"captured_at,omitempty"`
	CheckedAt   time.Time  `json:"checked_at,omitempty"`
	NextRetryAt *time.Time `json:"next_retry_at,omitempty"`
	Stale       bool       `json:"stale,omitempty"`
	failures    int
	ID          string `json:"id"`
	Name        string `json:"name"`
	Installed   bool   `json:"installed"`
	Status      string `json:"status"` // available | signed_out | unavailable | unsupported
	Account     string `json:"account,omitempty"`
	Plan        string `json:"plan,omitempty"`
	// AccountID/AccountLabel — какой из аккаунтов машины это. Ими клиент
	// подписывает строку («Claude · рабочий — 82%»); Account выше — это почта,
	// которую назвал сам вендор, и она может отсутствовать.
	AccountID     string      `json:"account_id,omitempty"`
	AccountLabel  string      `json:"account_label,omitempty"`
	AccountActive bool        `json:"account_active,omitempty"`
	Windows       []Window    `json:"windows"`
	Extra         *ExtraUsage `json:"extra_usage,omitempty"`
	// Message — человеческий текст для клиентов, которые ещё не знают кодов.
	// В нём не бывает ни кодов HTTP, ни английских строк провайдера: это то,
	// что человек читает в карточке лимитов.
	Message string `json:"message,omitempty"`
	// MessageCode — машинная причина (Msg* выше); по ней клиент выбирает свой
	// текст и решает, что предлагать делать.
	MessageCode string `json:"message_code,omitempty"`
	Source      string `json:"source,omitempty"`
}

// fail записывает причину сразу в двух видах: машинный код (по нему клиент
// выбирает текст) и запасной русский текст для клиентов, кодов не знающих.
func (p *Provider) fail(code, text string) {
	p.MessageCode, p.Message = code, text
}

// Snapshot is returned by GET /api/ai-usage.
type Snapshot struct {
	CapturedAt time.Time  `json:"captured_at"`
	Providers  []Provider `json:"providers"`
}

// Collect probes the provider clients independently. A broken or signed-out
// provider never makes the whole endpoint fail. The snapshot is remembered in
// the process cache (see cache.go) so panel views can render it cheaply.
func Collect(ctx context.Context) Snapshot {
	return CollectCachedWithRefresh(ctx, true)
}

// collectAll performs the actual probing; kept separate from Collect so tests
// can substitute it and exercise the cache without touching the network.
func collectAll(ctx context.Context) Snapshot {
	providers := make([]Provider, 0, 6)

	// Каждый аккаунт опрашивается отдельно и параллельно: у Codex это свой
	// app-server процесс, у Claude — свой запрос с его токеном. Аккаунтов
	// обычно два-три, поэтому городить пул незачем, а вот делать это
	// последовательно нельзя — два аккаунта удвоили бы ожидание карточки.
	type probe struct {
		account Account
		run     func(context.Context, Account) Provider
	}
	var probes []probe
	for _, a := range accountsFor("codex") {
		probes = append(probes, probe{a, collectCodex})
	}
	for _, a := range accountsFor("claude") {
		probes = append(probes, probe{a, func(c context.Context, acc Account) Provider {
			return collectClaude(c, http.DefaultClient, acc)
		}})
	}

	results := make([]Provider, len(probes))
	var wait sync.WaitGroup
	wait.Add(len(probes))
	for i, p := range probes {
		go func(i int, p probe) {
			defer wait.Done()
			probeCtx, cancel := context.WithTimeout(ctx, providerTimeout)
			defer cancel()
			results[i] = collectAccount(probeCtx, p.account, p.run)
		}(i, p)
	}
	wait.Wait()

	for _, p := range results {
		if p.Installed {
			providers = append(providers, p)
		}
	}

	// These clients do not currently expose a stable, credential-safe local
	// subscription-quota API. Showing them explicitly is more honest than
	// silently pretending the list is complete.
	for _, item := range []struct {
		ID, Name, Command string
	}{
		{ID: "gemini", Name: "Gemini CLI", Command: "gemini"},
		{ID: "kimi", Name: "Kimi CLI", Command: "kimi"},
		{ID: "opencode", Name: "OpenCode", Command: "opencode"},
		{ID: "aider", Name: "Aider", Command: "aider"},
		{ID: "grok", Name: "Grok", Command: "grok"},
	} {
		if executableExists(item.Command) {
			providers = append(providers, Provider{
				ID:          item.ID,
				Name:        item.Name,
				Installed:   true,
				Status:      "unsupported",
				Windows:     []Window{},
				MessageCode: MsgUnsupported,
				Message:     "Клиент пока не отдаёт надёжную сводку лимитов без доступа к секретам.",
			})
		}
	}

	return Snapshot{
		CapturedAt: time.Now().UTC(),
		Providers:  providers,
	}
}

func executableExists(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

type rpcResponse struct {
	ID     json.RawMessage `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Message string `json:"message"`
	} `json:"error"`
}

type codexAccountResponse struct {
	Account *struct {
		Type     string  `json:"type"`
		Email    *string `json:"email"`
		PlanType string  `json:"planType"`
	} `json:"account"`
	RequiresOpenAIAuth bool `json:"requiresOpenaiAuth"`
}

type codexRateWindow struct {
	UsedPercent       int    `json:"usedPercent"`
	WindowDurationMin *int64 `json:"windowDurationMins"`
	ResetsAt          *int64 `json:"resetsAt"`
}

type codexRateSnapshot struct {
	LimitID   *string          `json:"limitId"`
	LimitName *string          `json:"limitName"`
	Primary   *codexRateWindow `json:"primary"`
	Secondary *codexRateWindow `json:"secondary"`
	PlanType  *string          `json:"planType"`
}

type codexRateResponse struct {
	RateLimits          codexRateSnapshot            `json:"rateLimits"`
	RateLimitsByLimitID map[string]codexRateSnapshot `json:"rateLimitsByLimitId"`
}

func collectCodex(ctx context.Context, acc Account) Provider {
	provider := Provider{
		ID:            "codex",
		Name:          "OpenAI Codex",
		Status:        "unavailable",
		Windows:       []Window{},
		AccountID:     acc.ID,
		AccountLabel:  acc.Label,
		AccountActive: acc.Active,
	}
	executable, ok := findCodexExecutable()
	if !ok {
		return provider
	}
	provider.Installed = true

	// Фоновый опрос лимитов подписки: общение идёт по stdio, показывать человеку
	// нечего. Экран «Подписки и лимиты» дёргает это регулярно — без Hidden каждый
	// заход открывал бы пустое консольное окно codex.
	cmd := procutil.Hidden(exec.CommandContext(ctx, executable, "app-server", "--stdio"))
	// Аккаунт Codex — это каталог CODEX_HOME (так его называет и сам
	// `codex --help`). Спрашиваем лимиты у ТОГО аккаунта, под которым агент и
	// будет запущен, а не у того, что лежит в домашнем каталоге.
	if acc.Dir != "" {
		cmd.Env = append(os.Environ(), "CODEX_HOME="+acc.Dir)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		provider.fail(MsgClientStart, "Не удалось открыть локальный интерфейс Codex.")
		return provider
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		provider.fail(MsgClientStart, "Не удалось прочитать ответ Codex.")
		return provider
	}
	// App-server diagnostics may contain machine-local paths. They are not part
	// of the product response and are intentionally discarded.
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		provider.fail(MsgClientStart, "Codex установлен, но локальный интерфейс не запустился.")
		return provider
	}
	defer func() {
		_ = stdin.Close()
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		_ = cmd.Wait()
	}()

	enc := json.NewEncoder(stdin)
	if err := enc.Encode(map[string]any{
		"id":     1,
		"method": "initialize",
		"params": map[string]any{
			"clientInfo": map[string]string{
				"name":    "remotai",
				"title":   "Remotai",
				"version": "1",
			},
			"capabilities": map[string]bool{"experimentalApi": true},
		},
	}); err != nil {
		provider.fail(MsgClientTalk, "Codex не принял запрос лимитов.")
		return provider
	}

	var account codexAccountResponse
	var rates codexRateResponse
	gotAccount, gotRates := false, false
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64*1024), maxResponseBytes)
	for scanner.Scan() {
		var response rpcResponse
		if json.Unmarshal(scanner.Bytes(), &response) != nil {
			continue
		}
		switch strings.TrimSpace(string(response.ID)) {
		case "1":
			if response.Error != nil {
				provider.fail(MsgClientOld, "Версия Codex не поддерживает сводку лимитов.")
				return provider
			}
			if err := enc.Encode(map[string]any{
				"id": 2, "method": "account/read",
				"params": map[string]bool{"refreshToken": false},
			}); err != nil {
				provider.fail(MsgClientTalk, "Codex не принял запрос аккаунта.")
				return provider
			}
			if err := enc.Encode(map[string]any{
				"id": 3, "method": "account/rateLimits/read", "params": map[string]any{},
			}); err != nil {
				provider.fail(MsgClientTalk, "Codex не принял запрос лимитов.")
				return provider
			}
		case "2":
			if response.Error != nil {
				provider.fail(MsgClientTalk, "Не удалось проверить аккаунт Codex. Повторим автоматически.")
				return provider
			}
			if response.Error == nil && json.Unmarshal(response.Result, &account) == nil {
				gotAccount = true
			}
		case "3":
			if response.Error != nil {
				provider.fail(MsgProviderError, "Codex временно не отдаёт лимиты. Повторим автоматически.")
				if strings.Contains(response.Error.Message, "429") {
					provider.MessageCode = MsgRateLimited
				}
				return provider
			}
			if response.Error == nil && json.Unmarshal(response.Result, &rates) == nil {
				gotRates = true
			}
		}
		if gotAccount && gotRates {
			break
		}
	}

	if ctx.Err() != nil || !gotAccount {
		provider.fail(MsgTimeout, "Codex не ответил вовремя. Повторим автоматически.")
		return provider
	}
	if account.Account == nil {
		provider.Status = "signed_out"
		provider.fail(MsgSignIn, "Войдите в Codex на этом устройстве.")
		return provider
	}
	if account.Account.Email != nil {
		provider.Account = strings.TrimSpace(*account.Account.Email)
	}
	provider.Plan = account.Account.PlanType
	if !gotRates {
		provider.fail(MsgNoWindows, "Codex не вернул текущие окна лимитов.")
		return provider
	}

	provider.Status = "available"
	provider.Source = "Codex app-server"
	provider.Windows = codexWindows(rates)
	if provider.Plan == "" {
		provider.Plan = firstCodexPlan(rates)
	}
	if len(provider.Windows) == 0 {
		provider.fail(MsgNoPercent, "Для этого типа входа Codex не сообщает процент лимита.")
	}
	return provider
}

func codexWindows(response codexRateResponse) []Window {
	snapshots := response.RateLimitsByLimitID
	if len(snapshots) == 0 {
		key := "codex"
		if response.RateLimits.LimitID != nil && *response.RateLimits.LimitID != "" {
			key = *response.RateLimits.LimitID
		}
		snapshots = map[string]codexRateSnapshot{key: response.RateLimits}
	}

	keys := make([]string, 0, len(snapshots))
	for key := range snapshots {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	var result []Window
	for _, key := range keys {
		snapshot := snapshots[key]
		name := strings.TrimSpace(valueOrEmpty(snapshot.LimitName))
		if name == "" {
			if key == "codex" {
				name = "Основной лимит"
			} else {
				name = key
			}
		}
		if snapshot.Primary != nil {
			result = append(result, codexWindow(key+":primary", name, "основное окно", snapshot.Primary))
		}
		if snapshot.Secondary != nil {
			result = append(result, codexWindow(key+":secondary", name, "дополнительное окно", snapshot.Secondary))
		}
	}
	return result
}

func codexWindow(id, name, kind string, source *codexRateWindow) Window {
	label := name
	if kind != "" {
		label += " · " + kind
	}
	return Window{
		LimitID:         strings.SplitN(id, ":", 2)[0],
		ID:              id,
		Label:           label,
		UsedPercent:     clampPercent(float64(source.UsedPercent)),
		DurationMinutes: source.WindowDurationMin,
		ResetsAt:        source.ResetsAt,
	}
}

func firstCodexPlan(response codexRateResponse) string {
	if response.RateLimits.PlanType != nil {
		return *response.RateLimits.PlanType
	}
	for _, snapshot := range response.RateLimitsByLimitID {
		if snapshot.PlanType != nil {
			return *snapshot.PlanType
		}
	}
	return ""
}

func valueOrEmpty(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func findCodexExecutable() (string, bool) {
	path, err := exec.LookPath("codex")
	if err != nil {
		return "", false
	}
	if runtime.GOOS != "windows" || strings.EqualFold(filepath.Ext(path), ".exe") {
		return path, true
	}

	// npm exposes codex.cmd/codex.ps1, while os/exec needs the native binary.
	// Resolve the package-local executable without invoking a shell.
	base := filepath.Dir(path)
	patterns := []string{
		filepath.Join(base, "node_modules", "@openai", "codex", "node_modules", "@openai", "codex-win32-*", "vendor", "*", "bin", "codex.exe"),
		filepath.Join(base, "node_modules", "@openai", "codex", "vendor", "*", "bin", "codex.exe"),
	}
	for _, pattern := range patterns {
		if matches, _ := filepath.Glob(pattern); len(matches) > 0 {
			sort.Strings(matches)
			return matches[0], true
		}
	}
	return "", false
}

type claudeCredentials struct {
	ClaudeOAuth struct {
		AccessToken      string `json:"accessToken"`
		SubscriptionType string `json:"subscriptionType"`
	} `json:"claudeAiOauth"`
}

type claudeProfile struct {
	OAuthAccount struct {
		EmailAddress string `json:"emailAddress"`
		BillingType  string `json:"billingType"`
		Organization string `json:"organizationName"`
		DisplayName  string `json:"displayName"`
	} `json:"oauthAccount"`
}

type claudeUsageWindow struct {
	Utilization *float64 `json:"utilization"`
	ResetsAt    *string  `json:"resets_at"`
}

type claudeExtraUsage struct {
	IsEnabled     bool     `json:"is_enabled"`
	MonthlyLimit  *float64 `json:"monthly_limit"`
	UsedCredits   *float64 `json:"used_credits"`
	Utilization   *float64 `json:"utilization"`
	Currency      string   `json:"currency"`
	DecimalPlaces int      `json:"decimal_places"`
	SpendLimitHit bool     `json:"spend_limit_reached"`
}

type claudeUsageResponse struct {
	FiveHour          *claudeUsageWindow `json:"five_hour"`
	SevenDay          *claudeUsageWindow `json:"seven_day"`
	SevenDayOpus      *claudeUsageWindow `json:"seven_day_opus"`
	SevenDaySonnet    *claudeUsageWindow `json:"seven_day_sonnet"`
	SevenDayOAuthApps *claudeUsageWindow `json:"seven_day_oauth_apps"`
	Extra             *claudeExtraUsage  `json:"extra_usage"`
}

func collectClaude(ctx context.Context, client *http.Client, acc Account) Provider {
	provider := Provider{
		ID:            "claude",
		Name:          "Claude",
		Status:        "unavailable",
		Windows:       []Window{},
		AccountID:     acc.ID,
		AccountLabel:  acc.Label,
		AccountActive: acc.Active,
	}
	home, _ := os.UserHomeDir()
	credentialPath := claudeCredentialsPath(acc.Dir)
	_, credentialErr := os.Stat(credentialPath)
	provider.Installed = executableExists("claude") || credentialErr == nil
	if !provider.Installed {
		return provider
	}

	var credentials claudeCredentials
	if err := readJSONFile(credentialPath, &credentials); err != nil ||
		strings.TrimSpace(credentials.ClaudeOAuth.AccessToken) == "" {
		provider.Status = "signed_out"
		if acc.Dir != "" {
			// Заведённый, но ещё не залогиненный аккаунт — обычное состояние
			// сразу после «+ Аккаунт»: каталог наш, а токен выдаёт вендор.
			provider.fail(MsgSignIn, "В этот аккаунт Claude ещё не входили.")
		} else {
			provider.fail(MsgSignIn, "Войдите в Claude Code на этом устройстве.")
		}
		return provider
	}

	// Профиль (почта, тариф) лежит рядом с кредами: у своего каталога — внутри
	// него, у основного — исторически в корне домашнего каталога, а НЕ в
	// `~/.claude`.
	var profile claudeProfile
	if acc.Dir != "" {
		_ = readJSONFile(filepath.Join(acc.Dir, ".claude.json"), &profile)
	} else if home != "" {
		_ = readJSONFile(filepath.Join(home, ".claude.json"), &profile)
	}
	provider.Account = strings.TrimSpace(profile.OAuthAccount.EmailAddress)
	provider.Plan = strings.TrimSpace(credentials.ClaudeOAuth.SubscriptionType)
	if provider.Plan == "" {
		provider.Plan = strings.TrimSpace(profile.OAuthAccount.BillingType)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.anthropic.com/api/oauth/usage", nil)
	if err != nil {
		provider.fail(MsgRequestFailed, "Не удалось подготовить запрос Claude.")
		return provider
	}
	req.Header.Set("Authorization", "Bearer "+credentials.ClaudeOAuth.AccessToken)
	req.Header.Set("anthropic-beta", "oauth-2025-04-20")
	req.Header.Set("User-Agent", "remotai-ai-usage/1")

	response, err := client.Do(req)
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			provider.fail(MsgTimeout, "Claude не ответил вовремя.")
		} else {
			provider.fail(MsgUnreachable, "Не удалось получить лимиты Claude.")
		}
		return provider
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
		provider.Status = "signed_out"
		provider.fail(MsgSessionExpired, "Сессия Claude Code истекла — войдите заново.")
		return provider
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		provider.NextRetryAt = retryAfter(response.Header.Get("Retry-After"), time.Now())
		if response.StatusCode == http.StatusTooManyRequests {
			provider.fail(MsgRateLimited, "Claude просит подождать перед обновлением лимитов. Повторим автоматически.")
			return provider
		}
		// Код ответа — в лог агента, а не в карточку лимитов: «HTTP 503» человеку
		// не говорит ничего, а решение у него одно — повторить позже.
		log.Printf("[AI-USAGE] claude usage http=%d", response.StatusCode)
		provider.fail(MsgProviderError, "Claude временно не отдаёт лимиты — попробуйте позже.")
		return provider
	}

	var usage claudeUsageResponse
	decoder := json.NewDecoder(io.LimitReader(response.Body, maxResponseBytes))
	if err := decoder.Decode(&usage); err != nil {
		provider.fail(MsgBadResponse, "Claude вернул незнакомый формат лимитов.")
		return provider
	}
	provider.Status = "available"
	provider.Source = "Anthropic account usage"
	provider.Windows = claudeWindows(usage)
	if usage.Extra != nil && usage.Extra.IsEnabled {
		used := numberOrZero(usage.Extra.UsedCredits)
		limit := numberOrZero(usage.Extra.MonthlyLimit)
		percent := numberOrZero(usage.Extra.Utilization)
		if percent == 0 && limit > 0 {
			percent = used / limit * 100
		}
		provider.Extra = &ExtraUsage{
			Enabled:       true,
			Used:          used,
			Limit:         limit,
			UsedPercent:   clampPercent(percent),
			Currency:      usage.Extra.Currency,
			DecimalPlaces: usage.Extra.DecimalPlaces,
			Reached:       usage.Extra.SpendLimitHit,
		}
	}
	if len(provider.Windows) == 0 && provider.Extra == nil {
		provider.fail(MsgNoWindows, "Claude не сообщил активные окна лимитов.")
	}
	return provider
}

func claudeWindows(usage claudeUsageResponse) []Window {
	items := []struct {
		ID, Label string
		Window    *claudeUsageWindow
	}{
		{ID: "five_hour", Label: "Текущее 5-часовое окно", Window: usage.FiveHour},
		{ID: "seven_day", Label: "Недельный лимит", Window: usage.SevenDay},
		{ID: "seven_day_opus", Label: "Недельный лимит Opus", Window: usage.SevenDayOpus},
		{ID: "seven_day_sonnet", Label: "Недельный лимит Sonnet", Window: usage.SevenDaySonnet},
		{ID: "seven_day_oauth_apps", Label: "Недельный лимит OAuth-приложений", Window: usage.SevenDayOAuthApps},
	}
	result := make([]Window, 0, len(items))
	for _, item := range items {
		if item.Window == nil || item.Window.Utilization == nil {
			continue
		}
		var reset *int64
		if item.Window.ResetsAt != nil {
			if parsed, err := time.Parse(time.RFC3339Nano, *item.Window.ResetsAt); err == nil {
				value := parsed.Unix()
				reset = &value
			}
		}
		result = append(result, Window{
			ID:          item.ID,
			Label:       item.Label,
			UsedPercent: clampPercent(*item.Window.Utilization),
			ResetsAt:    reset,
		})
	}
	return result
}

// claudeCredentialsPath — путь к OAuth-кредам Claude Code. Используется
// только для чтения; REMOTAI_CLAUDE_CREDENTIALS переопределяет его для
// диагностики и тестов (токен наружу не попадает никогда).
//
// dir — каталог аккаунта (CLAUDE_CONFIG_DIR). Пусто = основной, у него креды
// лежат в `~/.claude`. Проверено запуском 04.08.2026: с CLAUDE_CONFIG_DIR
// Claude Code держит ВСЁ своё внутри указанного каталога.
func claudeCredentialsPath(dir string) string {
	if dir != "" {
		return filepath.Join(dir, ".credentials.json")
	}
	if p := strings.TrimSpace(os.Getenv("REMOTAI_CLAUDE_CREDENTIALS")); p != "" {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".claude", ".credentials.json")
}

func readJSONFile(path string, target any) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	return json.NewDecoder(io.LimitReader(file, maxResponseBytes)).Decode(target)
}

func numberOrZero(value *float64) float64 {
	if value == nil {
		return 0
	}
	return *value
}

func clampPercent(value float64) float64 {
	if value < 0 {
		return 0
	}
	if value > 100 {
		return 100
	}
	return value
}
