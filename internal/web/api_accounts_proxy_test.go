package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"tgcontrol/internal/agents"
)

func envMap(pairs []map[string]string) map[string]string {
	out := map[string]string{}
	for _, p := range pairs {
		out[p["name"]] = p["value"]
	}
	return out
}

// Прокси уходит только для transport-а, который подтверждён у самого CLI.
// Claude Code официально поддерживает HTTP(S)_PROXY, но не SOCKS.
func TestAccountProxyGivesVerifiedEnv(t *testing.T) {
	d := agents.GetDescriptor("claude")
	if d == nil {
		t.Skip("в реестре нет claude")
	}
	acc := AgentAccount{ID: "a1", AgentID: "claude", Dir: "C:\\acc", Proxy: "http://127.0.0.1:8080"}
	env := envMap(accountEnvPairs(acc, d))

	if env[d.AccountEnv] != "C:\\acc" {
		t.Fatalf("каталог аккаунта потерялся: %v", env)
	}
	for _, name := range []string{"HTTPS_PROXY", "HTTP_PROXY", "ALL_PROXY", "https_proxy", "http_proxy", "all_proxy"} {
		if env[name] != "http://127.0.0.1:8080" {
			t.Fatalf("%s = %q, ожидался адрес прокси", name, env[name])
		}
	}
	if env["NO_PROXY"] == "" || env["no_proxy"] != env["NO_PROXY"] {
		t.Fatal("нет best-effort NO_PROXY для transport-ов, которые его поддерживают")
	}
	if _, has := env["NODE_USE_ENV_PROXY"]; has {
		t.Fatal("NODE_USE_ENV_PROXY не является контрактом native Claude/Codex и не должен обещаться")
	}
}

// Без прокси лишних переменных не появляется: это самый частый случай, и
// окружение агента не должно обрастать пустышками.
func TestAccountWithoutProxyKeepsEnvClean(t *testing.T) {
	d := agents.GetDescriptor("claude")
	if d == nil {
		t.Skip("в реестре нет claude")
	}
	env := envMap(accountEnvPairs(AgentAccount{ID: "a1", AgentID: "claude", Dir: "C:\\acc"}, d))
	if len(env) != 1 || env[d.AccountEnv] == "" {
		t.Fatalf("ожидался только каталог аккаунта, получено %v", env)
	}
}

// У ОСНОВНОГО аккаунта каталога нет, а прокси быть должен: у большинства людей
// аккаунт ровно один — основной, и без этого функция им недоступна вовсе.
func TestDefaultAccountCanHaveProxy(t *testing.T) {
	d := agents.GetDescriptor("claude")
	if d == nil {
		t.Skip("в реестре нет claude")
	}
	f := accountsFile{DefaultProxy: map[string]string{"claude": "http://10.0.0.1:3128"}}
	acc := defaultAccount(f, "claude")
	env := envMap(accountEnvPairs(acc, d))
	if _, has := env[d.AccountEnv]; has {
		t.Fatal("основному аккаунту подставили каталог — это сломало бы его рабочий вход")
	}
	if env["HTTPS_PROXY"] != "http://10.0.0.1:3128" {
		t.Fatalf("прокси основного аккаунта не доехал: %v", env)
	}
}

// Адрес без схемы CLI молча игнорируют — значит его нельзя принимать молча и нам.
func TestProxyValidation(t *testing.T) {
	bad := []string{
		"127.0.0.1:1080", "ftp://host:21", "просто текст",
		"http://proxy.example:8080/private",
		"http://proxy.example:8080?token=secret",
		"http://proxy.example:8080#secret",
	}
	for _, raw := range bad {
		if err := validateProxyURLForAgent("claude", raw); err == nil {
			t.Fatalf("адрес %q принят, а он не работает", raw)
		}
	}
	for _, raw := range []string{"", "http://10.0.0.1:3128", "https://p.example.com:8443", "http://proxy.example:8080/"} {
		if err := validateProxyURLForAgent("claude", raw); err != nil {
			t.Fatalf("адрес %q отвергнут: %v", raw, err)
		}
	}
	if err := validateProxyURLForAgent("claude", "socks5://127.0.0.1:1080"); err == nil {
		t.Fatal("Claude получил SOCKS, который официальный transport не поддерживает")
	}
	if err := validateProxyURLForAgent("gemini", "http://127.0.0.1:8080"); err == nil {
		t.Fatal("Gemini получил прокси без доказанного transport-контракта")
	}
	if err := validateProxyURLForAgent("codex", "http://127.0.0.1:8080"); err != nil {
		t.Fatalf("подтверждённый HTTP proxy Codex отвергнут: %v", err)
	}
	if err := validateProxyURLForAgent("claude", "http://alice:secret@proxy.example:8080"); err == nil {
		t.Fatal("proxy credentials попали бы в shell preview, но адрес принят")
	}
	if err := validateProxyURLForAgent("claude", "http://secret-token@proxy.example:8080"); err == nil {
		t.Fatal("proxy username тоже может быть токеном, но адрес принят")
	}
}

func TestLegacyUnsupportedProxyIsNotInjected(t *testing.T) {
	for _, tc := range []AgentAccount{
		{ID: "a1", AgentID: "claude", Dir: "C:\\acc", Proxy: "socks5://127.0.0.1:1080"},
		{ID: "a2", AgentID: "gemini", Dir: "C:\\gemini", Proxy: "http://127.0.0.1:8080"},
	} {
		d := agents.GetDescriptor(tc.AgentID)
		env := envMap(accountEnvPairs(tc, d))
		if env["HTTP_PROXY"] != "" || env["HTTPS_PROXY"] != "" || env["ALL_PROXY"] != "" || env["http_proxy"] != "" {
			t.Fatalf("неподтверждённый proxy %s/%s попал в launch env: %v", tc.AgentID, tc.Proxy, env)
		}
	}
}

func TestAccountNoProxyCannotBypassProvider(t *testing.T) {
	t.Setenv("NO_PROXY", "*,.openai.com")
	t.Setenv("no_proxy", "api.anthropic.com")
	d := agents.GetDescriptor("codex")
	env := envMap(accountEnvPairs(AgentAccount{ID: "a1", AgentID: "codex", Proxy: "http://127.0.0.1:8080"}, d))
	const want = "localhost,127.0.0.1,::1"
	if env["NO_PROXY"] != want || env["no_proxy"] != want {
		t.Fatalf("host bypass протёк в аккаунт: NO_PROXY=%q no_proxy=%q", env["NO_PROXY"], env["no_proxy"])
	}
}

// Прокси основного аккаунта сохраняется и снимается через ту же ручку.
func TestUpsertDefaultAccountProxyRoundTrip(t *testing.T) {
	withTempHome(t)
	s := &Server{}
	post := func(body string) map[string]any {
		t.Helper()
		rec := httptest.NewRecorder()
		s.apiAccountsUpsert(rec, httptest.NewRequest("POST", "/api/accounts", strings.NewReader(body)), 1)
		if rec.Code != 200 {
			t.Fatalf("код %d, тело %s", rec.Code, rec.Body.String())
		}
		var out map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}

	out := post(`{"id":"default","agent_id":"claude","proxy":"http://127.0.0.1:1"}`)
	acc, _ := out["account"].(map[string]any)
	if acc["proxy"] != "http://127.0.0.1:1" {
		t.Fatalf("прокси не сохранился: %v", out)
	}
	// Список переменных для запуска приезжает тем же ответом.
	names := []string{}
	for _, raw := range acc["env"].([]any) {
		names = append(names, raw.(map[string]any)["name"].(string))
	}
	if !slices.Contains(names, "HTTP_PROXY") || slices.Contains(names, "NODE_USE_ENV_PROXY") {
		t.Fatalf("launch env не соответствует подтверждённому native transport: %v", names)
	}

	// Пустая строка — это «сними прокси», а не «не трогай». Отличить одно от
	// другого позволяет указатель в запросе; без этого снять было бы нечем.
	out = post(`{"id":"default","agent_id":"claude","proxy":""}`)
	acc, _ = out["account"].(map[string]any)
	if _, has := acc["proxy"]; has {
		t.Fatalf("прокси не снялся: %v", out)
	}
}

// Кривой адрес отвергается с внятным текстом, а не записывается молча.
func TestUpsertRejectsBadProxy(t *testing.T) {
	withTempHome(t)
	s := &Server{}
	rec := httptest.NewRecorder()
	s.apiAccountsUpsert(rec, httptest.NewRequest("POST", "/api/accounts",
		strings.NewReader(`{"id":"default","agent_id":"claude","proxy":"127.0.0.1:1080"}`)), 1)
	if rec.Code != 400 {
		t.Fatalf("код %d — адрес без схемы принят", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "http://") {
		t.Fatalf("в отказе нет подсказки, как надо: %s", rec.Body.String())
	}
}

// Живая проверка прокси: запрос ЧЕРЕЗ него на эхо-адрес, ответ — выходной IP.
// В CI не ходит: сети и прокси там нет. Прогон вручную:
//
//	TGCONTROL_LIVE_PROXY=http://127.0.0.1:12334 go test ./internal/web/ -run TestProbeProxyLive -v
//
// Мёртвый порт обязан давать ошибку, а не «отвечает»: боевой случай
// 14.08.2026 — человек вписал socks5://127.0.0.1:1080, которого на машине
// не было вовсе, и агент ушёл в бесконечные ретраи «Unable to connect».
func TestProbeProxyLive(t *testing.T) {
	raw := os.Getenv("TGCONTROL_LIVE_PROXY")
	if raw == "" {
		t.Skip("нет TGCONTROL_LIVE_PROXY — живая сеть в тестах не нужна")
	}
	ip, err := probeProxy(context.Background(), raw)
	if err != nil {
		t.Fatalf("живой прокси %s не прошёл проверку: %v", raw, err)
	}
	t.Logf("выходной IP через %s: %s", raw, ip)
}

// Мёртвый прокси — ошибка, а не молчание. Сети не надо: 127.0.0.1:1 слушать
// некому, отказ придёт мгновенно.
func TestProbeProxyDeadPortFails(t *testing.T) {
	if _, err := probeProxy(context.Background(), "http://127.0.0.1:1"); err == nil {
		t.Fatal("мёртвый прокси прошёл проверку — это и есть слепое пятно старого TCP-стука")
	}
	if _, err := probeProxy(context.Background(), "socks5://127.0.0.1:1"); err == nil {
		t.Fatal("неподдержанный socks5 прошёл проверку")
	}
}

func TestProbeProxyHonorsCancellation(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	proxyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
	}))
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
		proxyServer.CloseClientConnections()
		proxyServer.Close()
	}()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := probeProxy(ctx, proxyServer.URL)
		done <- err
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("probe не дошёл до локального proxy")
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("отменённый probe завершился успехом")
		}
		close(release)
	case <-time.After(time.Second):
		t.Fatal("probe проигнорировал отмену контекста")
	}
}

func TestProxyProbeDoesNotHoldAccountsLock(t *testing.T) {
	withTempHome(t)
	entered := make(chan struct{})
	release := make(chan struct{})
	proxyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
		http.Error(w, "proxy unavailable", http.StatusBadGateway)
	}))
	defer proxyServer.Close()

	s := &Server{}
	saveDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		rec := httptest.NewRecorder()
		body := `{"id":"default","agent_id":"claude","proxy":"` + proxyServer.URL + `"}`
		s.apiAccountsUpsert(rec, httptest.NewRequest("POST", "/api/accounts", strings.NewReader(body)), 1)
		saveDone <- rec
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("save не дошёл до локального proxy")
	}

	listDone := make(chan struct{})
	go func() {
		rec := httptest.NewRecorder()
		s.apiAccountsList(rec, httptest.NewRequest("GET", "/api/accounts", nil), 1)
		close(listDone)
	}()
	select {
	case <-listDone:
		// accountsMu освобождён до сети — остальные операции живы.
	case <-time.After(300 * time.Millisecond):
		close(release)
		t.Fatal("медленный proxy держит accountsMu и блокирует список")
	}
	close(release)
	select {
	case rec := <-saveDone:
		if rec.Code != http.StatusOK {
			t.Fatalf("save завершился кодом %d: %s", rec.Code, rec.Body.String())
		}
	case <-time.After(time.Second):
		t.Fatal("save не завершился после освобождения proxy")
	}
}

func TestProxyLabelRedactsAllCredentialSurfaces(t *testing.T) {
	tests := map[string]string{
		"http://alice:secret@proxy.example:8080/private?token=also-secret": "http://***@proxy.example:8080",
		"http://secret-token@proxy.example:8080/private":                   "http://***@proxy.example:8080",
		"http://proxy.example:8080/private?token=also-secret#fragment":     "http://proxy.example:8080",
	}
	for raw, want := range tests {
		if got := proxyLabel(raw); got != want {
			t.Fatalf("credential surface proxy показана небезопасно: %q -> %q, want %q", raw, got, want)
		}
	}
}

func TestLegacyCredentialProxyIsRedactedAndNotLaunched(t *testing.T) {
	d := agents.GetDescriptor("claude")
	account := AgentAccount{ID: "a1", AgentID: "claude", Proxy: "http://alice:secret@proxy.example:8080"}
	payload := accountPayload(account, d, false)
	if _, ok := payload["proxy"]; ok {
		t.Fatalf("invalid legacy proxy попал в совместимое старое поле: %v", payload)
	}
	shown, _ := payload["proxy_legacy"].(string)
	if shown != "http://***@proxy.example:8080" {
		t.Fatalf("credential proxy показан без редактирования: %q", shown)
	}
	if _, ok := payload["proxy_warning"]; !ok {
		t.Fatalf("legacy credential proxy не помечен: %v", payload)
	}
	if blocked, _ := payload["proxy_blocked"].(bool); !blocked {
		t.Fatalf("legacy credential proxy не блокирует запуск: %v", payload)
	}
	for _, pair := range accountEnvPairs(account, d) {
		if strings.Contains(strings.ToLower(pair["name"]), "proxy") {
			t.Fatalf("credential proxy попал в launch env: %v", pair)
		}
	}
}

func TestAccountsListAdvertisesProxyContract(t *testing.T) {
	withTempHome(t)
	rec := httptest.NewRecorder()
	(&Server{}).apiAccountsList(rec, httptest.NewRequest(http.MethodGet, "/api/accounts", nil), 1)
	if rec.Code != http.StatusOK {
		t.Fatalf("код %d: %s", rec.Code, rec.Body.String())
	}
	var payload map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload["proxy_contract_version"] != float64(1) {
		t.Fatalf("клиент не может отличить проверенный proxy contract: %v", payload)
	}
}

func TestLegacyClientCannotLaunchAgentWithBlockedProxy(t *testing.T) {
	find := func(items []map[string]any, id string) map[string]any {
		t.Helper()
		for _, item := range items {
			if item["id"] == id {
				return item
			}
		}
		t.Fatalf("нет агента %s", id)
		return nil
	}

	for name, proxyURL := range map[string]string{
		"unsupported SOCKS": "socks5://127.0.0.1:1080",
		"supported HTTP":    "http://127.0.0.1:8080",
	} {
		t.Run(name, func(t *testing.T) {
			withTempHome(t)
			if err := saveAccountsFile(accountsFile{
				DefaultProxy: map[string]string{"claude": proxyURL},
			}); err != nil {
				t.Fatal(err)
			}
			legacy := find(applyLegacyProxyGuard([]map[string]any{{
				"id": "claude", "detected": true, "cli": "claude", "resume_cli": "claude --continue",
				"install": "install", "install_posix": "install-posix",
			}}, blockedLegacyProxyAgents()), "claude")
			if legacy["detected"] != false || legacy["cli"] != "" || legacy["proxy_upgrade_required"] != true {
				t.Fatalf("старый client всё ещё может запустить proxy напрямую: %v", legacy)
			}
		})
	}
}
