package agentcheck

// Проверка — ОДИН настоящий минимальный запрос тем путём, каким ходит агент.
//
// Почему не «адрес отвечает». Стук в порт или GET на корень зелёный и при
// отозванном ключе, и при нулевом балансе, и при опечатке в имени модели — то
// есть ровно тогда, когда агент не работает. Поэтому спрашиваем модель
// по-настоящему: «Ответь одним словом: ок», потолок в несколько токенов.
//
//   - маршрут с ключом (Anthropic-/OpenAI-совместимый адрес, OpenRouter) —
//     прямой HTTP-запрос с тем же ключом, адресом, моделью и прокси аккаунта;
//   - вход по подписке — разовый безинтерактивный запуск самого CLI с
//     окружением аккаунта (`claude -p`, `codex exec`): токен подписки лежит в
//     каталоге CLI, и честно проверить его может только он сам.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"sort"
	"strings"
	"time"

	"tgcontrol/internal/procutil"
)

// Prompt — что спрашиваем. Короткий ответ — дёшево и сразу видно, что модель
// поняла вопрос.
const Prompt = "Ответь одним словом: ок"

// Коды причин. Человеческие слова — в клиенте (agentCheck.reason.*).
const (
	ReasonKeyRejected   = "key_rejected"
	ReasonLoginExpired  = "login_expired"
	ReasonNotLoggedIn   = "not_logged_in"
	ReasonNoFunds       = "no_funds"
	ReasonLimitReached  = "limit_reached"
	ReasonRateLimited   = "rate_limited"
	ReasonModelNotFound = "model_not_found"
	ReasonNetwork       = "network"
	ReasonTimeout       = "timeout"
	ReasonServerError   = "server_error"
	ReasonNoModel       = "no_model"
	ReasonCLIMissing    = "cli_missing"
	ReasonUnsupported   = "unsupported"
	ReasonEmptyReply    = "empty_reply"
	ReasonUnknown       = "unknown"
)

// Сроки. Переменные — ради тестов.
var (
	HTTPTimeout = 30 * time.Second
	CLITimeout  = 60 * time.Second
)

// Result — итог проверки.
type Result struct {
	OK         bool   `json:"ok"`
	Via        string `json:"via"`
	Route      string `json:"route,omitempty"`
	Reply      string `json:"reply,omitempty"`
	Model      string `json:"model,omitempty"`
	LatencyMS  int64  `json:"latency_ms"`
	HTTPStatus int    `json:"http_status,omitempty"`
	Reason     string `json:"reason,omitempty"`
	// Detail — короткий текст от провайдера или CLI, вычищенный от ключей.
	Detail    string `json:"detail,omitempty"`
	Host      string `json:"host,omitempty"`
	CheckedAt int64  `json:"checked_at"`
}

// Runner запускает CLI. Подменяется в тестах.
type Runner func(ctx context.Context, bin string, args, env []string, dir string) (stdout, stderr []byte, err error)

// Options — чем проверять.
type Options struct {
	// Model — модель вместо действующей (например, бесплатная у OpenRouter).
	Model string
	// ProxyURL — прокси аккаунта. Пусто = напрямую: системный прокси запуск
	// Remotai снимает (ProxyEnvNames), и проверка обязана идти тем же путём.
	ProxyURL string
	// Client — HTTP-клиент (тесты). nil = свой, с ProxyURL.
	Client *http.Client
	CLI    string
	Env    []string
	Dir    string
	Run    Runner
}

// DefaultCheckModel — чем проверять, если модель не задана нигде: CLI сам
// выберет свою, а прямому запросу модель нужна обязательно.
func DefaultCheckModel(c *Connection, openRouterModel string) string {
	switch {
	case c.Provider == "openrouter":
		if openRouterModel != "" {
			return openRouterModel
		}
		return "openrouter/free"
	case c.wire == "anthropic" && c.Provider == "anthropic":
		return "claude-haiku-4-5"
	}
	return ""
}

// Run выполняет проверку по плану подключения.
func Run(ctx context.Context, c *Connection, opt Options) Result {
	res := Result{Via: c.Check, Route: c.Route, CheckedAt: time.Now().Unix()}
	switch c.Check {
	case "http":
		return runHTTP(ctx, c, opt, res)
	case "cli":
		if opt.CLI == "" {
			res.Reason = ReasonCLIMissing
			return res
		}
		switch c.AgentID {
		case "claude":
			return runClaudeCLI(ctx, c, opt, res)
		case "codex":
			return runCodexCLI(ctx, c, opt, res)
		}
	}
	res.Reason = ReasonUnsupported
	for _, n := range c.Notes {
		if n == "cli_missing" {
			res.Reason = ReasonCLIMissing
		}
	}
	return res
}

func httpClient(opt Options) *http.Client {
	if opt.Client != nil {
		return opt.Client
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.DisableKeepAlives = true
	tr.Proxy = nil
	if opt.ProxyURL != "" {
		if u, err := url.Parse(opt.ProxyURL); err == nil {
			tr.Proxy = http.ProxyURL(u)
		}
	}
	return &http.Client{Transport: tr}
}

func runHTTP(ctx context.Context, c *Connection, opt Options, res Result) Result {
	model := strings.TrimSpace(opt.Model)
	if model == "" {
		model = c.model
	}
	if model == "" {
		model = DefaultCheckModel(c, "")
	}
	if model == "" {
		res.Reason = ReasonNoModel
		return res
	}
	res.Model = model
	res.Host = Host(c.baseURL)

	var endpoint string
	var body map[string]any
	base := strings.TrimRight(c.baseURL, "/")
	switch c.wire {
	case "anthropic":
		endpoint = base + "/v1/messages"
		body = map[string]any{"model": model, "max_tokens": 5,
			"messages": []map[string]string{{"role": "user", "content": Prompt}}}
	case "responses":
		// У Responses API нижняя граница потолка — 16 токенов.
		endpoint = base + "/responses"
		body = map[string]any{"model": model, "input": Prompt, "max_output_tokens": 16}
	default:
		endpoint = base + "/chat/completions"
		body = map[string]any{"model": model, "max_tokens": 5,
			"messages": []map[string]string{{"role": "user", "content": Prompt}}}
	}
	raw, _ := json.Marshal(body)
	cctx, cancel := context.WithTimeout(ctx, HTTPTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodPost, endpoint, bytes.NewReader(raw))
	if err != nil {
		res.Reason = ReasonUnknown
		res.Detail = Scrub(err.Error(), []string{c.secret}, 160)
		return res
	}
	req.Header.Set("Content-Type", "application/json")
	if c.wire == "anthropic" {
		req.Header.Set("anthropic-version", "2023-06-01")
	}
	if c.authHeader == "x-api-key" {
		req.Header.Set("x-api-key", c.secret)
	} else {
		req.Header.Set("Authorization", "Bearer "+c.secret)
	}
	if c.Provider == "openrouter" {
		req.Header.Set("HTTP-Referer", "https://remotai.ru")
		req.Header.Set("X-Title", "Remotai")
	}

	start := time.Now()
	resp, err := httpClient(opt).Do(req)
	res.LatencyMS = time.Since(start).Milliseconds()
	if err != nil {
		res.Reason = netReason(cctx, err)
		res.Detail = Scrub(err.Error(), []string{c.secret}, 160)
		return res
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	res.LatencyMS = time.Since(start).Milliseconds()
	res.HTTPStatus = resp.StatusCode

	text, gotModel, errMsg := parseReply(c.wire, data)
	if gotModel != "" {
		res.Model = gotModel
	}
	if resp.StatusCode != http.StatusOK || errMsg != "" {
		msg := errMsg
		if msg == "" {
			msg = string(data)
		}
		res.Reason = Classify(resp.StatusCode, msg, false)
		res.Detail = Scrub(msg, []string{c.secret}, 200)
		return res
	}
	res.Reply = Scrub(text, []string{c.secret}, 120)
	if strings.TrimSpace(res.Reply) == "" {
		// Ответ 200 без текста — ключ, баланс и модель в порядке (иначе был бы
		// отказ), но «ок» мы не увидели. Не прячем это под зелёное.
		res.OK = true
		res.Reason = ReasonEmptyReply
		return res
	}
	res.OK = true
	return res
}

// parseReply достаёт текст, модель и ошибку из ответа любого из трёх форматов.
// Ошибка бывает и с кодом 200 (OpenRouter: роутер не нашёл свободной модели).
func parseReply(wire string, data []byte) (text, model, errMsg string) {
	var p struct {
		Model   string `json:"model"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		Choices []struct {
			Message struct {
				Content any `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Output []struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
		OutputText string          `json:"output_text"`
		Error      json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(data, &p); err != nil {
		return "", "", ""
	}
	model = p.Model
	if len(p.Error) > 0 && string(p.Error) != "null" {
		var e struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    any    `json:"code"`
		}
		if json.Unmarshal(p.Error, &e) == nil {
			errMsg = strings.TrimSpace(fmt.Sprintf("%s %s %v", e.Type, e.Message, codeStr(e.Code)))
		} else {
			var s string
			if json.Unmarshal(p.Error, &s) == nil {
				errMsg = s
			}
		}
	}
	for _, c := range p.Content {
		text += c.Text
	}
	for _, ch := range p.Choices {
		if s, ok := ch.Message.Content.(string); ok {
			text += s
		}
	}
	if p.OutputText != "" {
		text += p.OutputText
	} else {
		for _, o := range p.Output {
			for _, c := range o.Content {
				text += c.Text
			}
		}
	}
	return strings.TrimSpace(text), model, errMsg
}

func codeStr(v any) string {
	if v == nil {
		return ""
	}
	return fmt.Sprint(v)
}

func netReason(ctx context.Context, err error) string {
	if errors.Is(err, context.DeadlineExceeded) || ctx.Err() == context.DeadlineExceeded {
		return ReasonTimeout
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return ReasonTimeout
	}
	return ReasonNetwork
}

// Classify — причина отказа по коду ответа и тексту. subscription = проверяли
// вход по подписке: там «401» значит «вход протух», а не «ключ отклонён».
func Classify(status int, msg string, subscription bool) string {
	m := strings.ToLower(msg)
	has := func(words ...string) bool {
		for _, w := range words {
			if strings.Contains(m, w) {
				return true
			}
		}
		return false
	}
	switch {
	case has("not logged in", "please run /login", "run codex login", "not signed in"):
		if subscription && has("expired", "revoked") {
			return ReasonLoginExpired
		}
		return ReasonNotLoggedIn
	case status == http.StatusPaymentRequired,
		has("insufficient_quota", "insufficient credits", "insufficient funds", "credit balance is too low",
			"requires more credits", "payment required", "billing", "out of credits"):
		return ReasonNoFunds
	case has("usage limit", "limit reached", "hit your limit", "you've hit your", "quota exceeded for your plan"):
		return ReasonLimitReached
	case status == http.StatusUnauthorized, status == http.StatusForbidden,
		has("invalid api key", "invalid x-api-key", "invalid_api_key", "authentication_error", "unauthorized",
			"oauth token has expired", "token expired", "incorrect api key", "401"):
		if subscription {
			return ReasonLoginExpired
		}
		return ReasonKeyRejected
	case status == http.StatusNotFound,
		has("model_not_found", "not_found_error", "no endpoints found", "is not a valid model", "unknown model",
			"model not found", "does not exist", "issue with the selected model", "not supported when using", "invalid model"):
		return ReasonModelNotFound
	case status == http.StatusTooManyRequests, has("rate limit", "rate_limit", "too many requests", "overloaded", "429"):
		return ReasonRateLimited
	case status >= 500, has("internal server error", "bad gateway", "service unavailable"):
		return ReasonServerError
	case has("error sending request", "dns", "connection refused", "no such host", "stream disconnected",
		"connection reset", "network", "tls handshake", "failed to connect", "econnrefused", "enotfound", "etimedout"):
		return ReasonNetwork
	}
	return ReasonUnknown
}

// ── Разовый запуск CLI ──────────────────────────────────────────────

// ExecRunner — настоящий запуск: без окна (procutil.Hidden), с убийством всего
// дерева по сроку (procutil.Prepare) и пустым stdin — `codex exec` иначе ждёт
// дополнительный ввод.
func ExecRunner(ctx context.Context, bin string, args, env []string, dir string) ([]byte, []byte, error) {
	cmd := exec.CommandContext(ctx, bin, args...)
	procutil.Prepare(cmd)
	procutil.Hidden(cmd)
	cmd.Env = env
	cmd.Dir = dir
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	err := cmd.Run()
	return out.Bytes(), errb.Bytes(), err
}

func runCLI(ctx context.Context, opt Options, args []string) (stdout, stderr []byte, ms int64, timedOut bool, err error) {
	run := opt.Run
	if run == nil {
		run = ExecRunner
	}
	cctx, cancel := context.WithTimeout(ctx, CLITimeout)
	defer cancel()
	start := time.Now()
	stdout, stderr, err = run(cctx, opt.CLI, args, opt.Env, opt.Dir)
	ms = time.Since(start).Milliseconds()
	timedOut = cctx.Err() == context.DeadlineExceeded
	return
}

func runClaudeCLI(ctx context.Context, c *Connection, opt Options, res Result) Result {
	// --tools "" — модели нечем что-то сделать на компьютере; --no-session-
	// persistence — проверка не оседает в истории бесед человека.
	args := []string{"-p", Prompt, "--output-format", "json", "--no-session-persistence", "--tools", ""}
	if m := strings.TrimSpace(opt.Model); m != "" {
		args = append(args, "--model", m)
	}
	stdout, stderr, ms, timedOut, err := runCLI(ctx, opt, args)
	res.LatencyMS = ms
	res.Host = Host(c.baseURL)
	if timedOut {
		res.Reason = ReasonTimeout
		return res
	}
	sub := c.Route == "subscription" || c.Route == "none"
	var out struct {
		Type           string                    `json:"type"`
		IsError        bool                      `json:"is_error"`
		Result         string                    `json:"result"`
		APIErrorStatus int                       `json:"api_error_status"`
		ModelUsage     map[string]map[string]any `json:"modelUsage"`
	}
	line := lastJSONLine(stdout)
	if line == nil || json.Unmarshal(line, &out) != nil || out.Type != "result" {
		msg := strings.TrimSpace(string(stdout) + " " + string(stderr))
		if err != nil && msg == "" {
			msg = err.Error()
		}
		res.Reason = Classify(0, msg, sub)
		res.Detail = Scrub(msg, []string{c.secret}, 200)
		return res
	}
	res.HTTPStatus = out.APIErrorStatus
	res.Model = pickModel(out.ModelUsage)
	if out.IsError {
		res.Reason = Classify(out.APIErrorStatus, out.Result, sub)
		if c.Route == "none" && res.Reason == ReasonLoginExpired {
			res.Reason = ReasonNotLoggedIn
		}
		res.Detail = Scrub(out.Result, []string{c.secret}, 200)
		return res
	}
	res.Reply = Scrub(out.Result, []string{c.secret}, 120)
	res.OK = true
	if strings.TrimSpace(res.Reply) == "" {
		res.Reason = ReasonEmptyReply
	}
	return res
}

// pickModel — модель, которая выдала больше всего токенов: Claude Code
// дёргает и служебные маленькие модели, а человеку важна та, что отвечала.
func pickModel(usage map[string]map[string]any) string {
	type kv struct {
		name string
		out  float64
	}
	var list []kv
	for name, u := range usage {
		f, _ := u["outputTokens"].(float64)
		list = append(list, kv{name, f})
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].out != list[j].out {
			return list[i].out > list[j].out
		}
		return list[i].name < list[j].name
	})
	if len(list) == 0 {
		return ""
	}
	return list[0].name
}

func lastJSONLine(b []byte) []byte {
	lines := bytes.Split(bytes.TrimSpace(b), []byte("\n"))
	for i := len(lines) - 1; i >= 0; i-- {
		l := bytes.TrimSpace(lines[i])
		if len(l) > 0 && l[0] == '{' {
			return l
		}
	}
	return nil
}

func runCodexCLI(ctx context.Context, c *Connection, opt Options, res Result) Result {
	args := []string{"exec", "--skip-git-repo-check", "--ephemeral", "--json", "-s", "read-only", "--ignore-rules"}
	model := strings.TrimSpace(opt.Model)
	if model != "" {
		args = append(args, "-m", model)
	}
	args = append(args, Prompt)
	stdout, stderr, ms, timedOut, err := runCLI(ctx, opt, args)
	res.LatencyMS = ms
	res.Host = Host(c.baseURL)
	res.Model = c.model
	if model != "" {
		res.Model = model
	}
	var reply string
	var errs []string
	sc := bufio.NewScanner(bytes.NewReader(stdout))
	sc.Buffer(make([]byte, 64*1024), 4<<20)
	for sc.Scan() {
		l := bytes.TrimSpace(sc.Bytes())
		if len(l) == 0 || l[0] != '{' {
			continue
		}
		var ev struct {
			Type    string `json:"type"`
			Message string `json:"message"`
			Item    struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"item"`
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(l, &ev) != nil {
			continue
		}
		switch {
		case ev.Type == "item.completed" && ev.Item.Type == "agent_message":
			reply = ev.Item.Text
		case ev.Type == "error" && ev.Message != "":
			errs = append(errs, ev.Message)
		case ev.Type == "turn.failed" && ev.Error.Message != "":
			errs = append(errs, ev.Error.Message)
		}
	}
	sub := c.Route == "subscription"
	if strings.TrimSpace(reply) != "" {
		res.OK = true
		res.Reply = Scrub(reply, []string{c.secret}, 120)
		return res
	}
	if timedOut {
		res.Reason = ReasonTimeout
		if len(errs) > 0 {
			res.Detail = Scrub(errs[len(errs)-1], []string{c.secret}, 200)
		}
		return res
	}
	msg := strings.Join(errs, " | ")
	if msg == "" {
		msg = strings.TrimSpace(string(stderr))
		if msg == "" && err != nil {
			msg = err.Error()
		}
	}
	res.Reason = Classify(0, msg, sub)
	if c.Route == "none" && (res.Reason == ReasonLoginExpired || res.Reason == ReasonKeyRejected) {
		res.Reason = ReasonNotLoggedIn
	}
	res.Detail = Scrub(lastUseful(msg), []string{c.secret}, 200)
	return res
}

// lastUseful — последняя осмысленная строка: у Codex перед ошибкой идут
// предупреждения о псевдонимах PATH, человеку они ничего не говорят.
func lastUseful(msg string) string {
	parts := strings.Split(msg, "\n")
	for i := len(parts) - 1; i >= 0; i-- {
		p := strings.TrimSpace(parts[i])
		if p != "" && !strings.HasPrefix(p, "WARNING:") && !strings.HasPrefix(p, "Reading additional input") {
			return p
		}
	}
	return strings.TrimSpace(msg)
}

// ── Что говорит сам CLI о входе ─────────────────────────────────────

// ExecProber — `claude auth status --json` / `codex login status` с
// окружением запуска. Вывод целиком наружу не уходит: разбираем в CLIStatus
// (почта и организация из ответа Claude отбрасываются).
func ExecProber(run Runner) Prober {
	if run == nil {
		run = ExecRunner
	}
	return func(ctx context.Context, agentID, cli string, env []string) (CLIStatus, error) {
		cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		switch agentID {
		case "claude":
			out, _, err := run(cctx, cli, []string{"auth", "status", "--json"}, env, "")
			return parseClaudeStatus(out, err)
		case "codex":
			out, errb, err := run(cctx, cli, []string{"login", "status"}, env, "")
			return parseCodexStatus(append(out, errb...), err)
		}
		return CLIStatus{}, errors.New("unsupported")
	}
}

func parseClaudeStatus(out []byte, runErr error) (CLIStatus, error) {
	var raw struct {
		LoggedIn         bool   `json:"loggedIn"`
		AuthMethod       string `json:"authMethod"`
		APIKeySource     string `json:"apiKeySource"`
		SubscriptionType string `json:"subscriptionType"`
	}
	start := bytes.IndexByte(out, '{')
	if start < 0 || json.Unmarshal(out[start:], &raw) != nil {
		if runErr != nil {
			return CLIStatus{}, runErr
		}
		return CLIStatus{}, errors.New("claude auth status: не разобрать")
	}
	return CLIStatus{LoggedIn: raw.LoggedIn, Method: raw.AuthMethod, APIKeySource: raw.APIKeySource, Plan: raw.SubscriptionType}, nil
}

func parseCodexStatus(out []byte, runErr error) (CLIStatus, error) {
	text := string(out)
	low := strings.ToLower(text)
	switch {
	case strings.Contains(low, "not logged in"):
		return CLIStatus{LoggedIn: false, Method: "none"}, nil
	case strings.Contains(low, "logged in using chatgpt"):
		return CLIStatus{LoggedIn: true, Method: "chatgpt"}, nil
	case strings.Contains(low, "logged in using an api key"):
		st := CLIStatus{LoggedIn: true, Method: "api_key"}
		// «… - sk-proj-***ABCDE»: хвост уже маскирован самим Codex, берём
		// из него только последние четыре знака.
		for _, line := range strings.Split(text, "\n") {
			if i := strings.LastIndex(line, " - "); i >= 0 && strings.Contains(strings.ToLower(line), "api key") {
				tail := strings.TrimSpace(line[i+3:])
				if len(tail) >= 4 {
					st.MaskedKey = "sk-…" + tail[len(tail)-4:]
				}
			}
		}
		return st, nil
	}
	if runErr != nil {
		return CLIStatus{}, runErr
	}
	return CLIStatus{}, errors.New("codex login status: не разобрать")
}
