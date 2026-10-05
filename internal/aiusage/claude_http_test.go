package aiusage

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func fakeClaudeClient(status int, body string, seen *http.Header) *http.Client {
	return &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if seen != nil {
			*seen = r.Header.Clone()
		}
		return &http.Response{
			StatusCode: status,
			Body:       io.NopCloser(strings.NewReader(body)),
			Header:     make(http.Header),
		}, nil
	})}
}

// Подмена пути к кредам через REMOTAI_CLAUDE_CREDENTIALS: реальный
// ~/.claude/.credentials.json тест не трогает.
func writeClaudeCredentials(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "credentials.json")
	content := `{"claudeAiOauth":{"accessToken":"test-token","subscriptionType":"pro"}}`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("REMOTAI_CLAUDE_CREDENTIALS", path)
}

func putFakeExecutableOnPath(t *testing.T, name string) {
	t.Helper()
	dir := t.TempDir()
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
}

func TestCollectClaudeParsesUsageResponse(t *testing.T) {
	writeClaudeCredentials(t)
	var seen http.Header
	client := fakeClaudeClient(200, `{
		"five_hour":{"utilization":12.5,"resets_at":"2026-07-28T17:00:00Z"},
		"seven_day":{"utilization":48.2,"resets_at":null},
		"seven_day_opus":{"utilization":5}
	}`, &seen)

	provider := collectClaude(context.Background(), client, Account{})

	if provider.Status != "available" {
		t.Fatalf("status=%q message=%q", provider.Status, provider.Message)
	}
	if provider.Plan != "pro" {
		t.Fatalf("plan=%q", provider.Plan)
	}
	if len(provider.Windows) != 3 {
		t.Fatalf("windows=%d want=3: %#v", len(provider.Windows), provider.Windows)
	}
	if provider.Windows[0].ID != "five_hour" || provider.Windows[0].UsedPercent != 12.5 {
		t.Fatalf("first window=%#v", provider.Windows[0])
	}
	if provider.Windows[0].ResetsAt == nil {
		t.Fatal("resets_at must be parsed")
	}
	// Авторизация — Bearer + beta-заголовок, иначе vendor endpoint молча 401.
	if got := seen.Get("Authorization"); got != "Bearer test-token" {
		t.Fatalf("Authorization=%q", got)
	}
	if got := seen.Get("anthropic-beta"); got != "oauth-2025-04-20" {
		t.Fatalf("anthropic-beta=%q", got)
	}
}

func TestCollectClaudeSessionExpired(t *testing.T) {
	writeClaudeCredentials(t)
	provider := collectClaude(context.Background(), fakeClaudeClient(401, `{"error":"unauthorized"}`, nil), Account{})
	if provider.Status != "signed_out" || provider.MessageCode != MsgSessionExpired {
		t.Fatalf("status=%q code=%q", provider.Status, provider.MessageCode)
	}
}

func TestCollectClaudeProviderErrorIsUnavailableNotZero(t *testing.T) {
	writeClaudeCredentials(t)
	provider := collectClaude(context.Background(), fakeClaudeClient(503, `oops`, nil), Account{})
	if provider.Status != "unavailable" || provider.MessageCode != MsgProviderError {
		t.Fatalf("status=%q code=%q", provider.Status, provider.MessageCode)
	}
	if len(provider.Windows) != 0 {
		t.Fatalf("windows=%#v want empty", provider.Windows)
	}
}

func TestCollectClaudeBadResponse(t *testing.T) {
	writeClaudeCredentials(t)
	provider := collectClaude(context.Background(), fakeClaudeClient(200, `not json`, nil), Account{})
	if provider.Status != "unavailable" || provider.MessageCode != MsgBadResponse {
		t.Fatalf("status=%q code=%q", provider.Status, provider.MessageCode)
	}
}

// Аккаунт с каталогом читает СВОИ креды, а не домашние.
//
// Это главное свойство менеджера аккаунтов: два профиля живут одновременно, и
// остаток лимита у каждого свой. Если бы каталог игнорировался, оба аккаунта
// показывали бы одно и то же число — и переключаться человек бы шёл вслепую.
func TestCollectClaudeReadsAccountDirectory(t *testing.T) {
	// Домашний путь намеренно ведёт в никуда: возьми его код — тест упал бы.
	t.Setenv("REMOTAI_CLAUDE_CREDENTIALS", filepath.Join(t.TempDir(), "missing.json"))
	dir := t.TempDir()
	content := `{"claudeAiOauth":{"accessToken":"second-account","subscriptionType":"max"}}`
	if err := os.WriteFile(filepath.Join(dir, ".credentials.json"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	profile := `{"oauthAccount":{"emailAddress":"second@example.com"}}`
	if err := os.WriteFile(filepath.Join(dir, ".claude.json"), []byte(profile), 0o600); err != nil {
		t.Fatal(err)
	}

	var seen http.Header
	client := fakeClaudeClient(200, `{"five_hour":{"utilization":7}}`, &seen)
	provider := collectClaude(context.Background(), client, Account{
		ID: "acc-2", Provider: "claude", Label: "личный", Dir: dir, Active: true,
	})

	if provider.Status != "available" {
		t.Fatalf("status=%q message=%q", provider.Status, provider.Message)
	}
	if got := seen.Get("Authorization"); got != "Bearer second-account" {
		t.Fatalf("ушёл токен другого аккаунта: %q", got)
	}
	if provider.Account != "second@example.com" || provider.Plan != "max" {
		t.Fatalf("account=%q plan=%q — профиль взят не из каталога аккаунта", provider.Account, provider.Plan)
	}
	if provider.AccountID != "acc-2" || provider.AccountLabel != "личный" || !provider.AccountActive {
		t.Fatalf("аккаунт не подписан: %#v", provider)
	}
}

// Заведённый, но ещё не залогиненный аккаунт — обычное состояние сразу после
// «+ Аккаунт». Он обязан говорить «войдите», а не показывать 0%.
func TestCollectClaudeEmptyAccountAsksToSignIn(t *testing.T) {
	putFakeExecutableOnPath(t, "claude")
	provider := collectClaude(context.Background(), fakeClaudeClient(200, `{}`, nil), Account{
		ID: "acc-3", Provider: "claude", Label: "новый", Dir: t.TempDir(),
	})
	if !provider.Installed {
		t.Fatal("provider must be installed for the empty-account sign-in check")
	}
	if provider.Status != "signed_out" || provider.MessageCode != MsgSignIn {
		t.Fatalf("status=%q code=%q", provider.Status, provider.MessageCode)
	}
	if len(provider.Windows) != 0 {
		t.Fatalf("windows=%#v want empty", provider.Windows)
	}
}

func TestCollectClaudeSignedOutWithoutCredentials(t *testing.T) {
	t.Setenv("REMOTAI_CLAUDE_CREDENTIALS", filepath.Join(t.TempDir(), "missing.json"))
	provider := collectClaude(context.Background(), fakeClaudeClient(200, `{}`, nil), Account{})
	// Креды не прочитались: либо клиент найден в PATH (тогда честный
	// signed_out), либо нет — тогда провайдер просто не установлен. 0% нет ни
	// в одном из исходов.
	if len(provider.Windows) != 0 {
		t.Fatalf("windows=%#v want empty", provider.Windows)
	}
	if provider.Installed && (provider.Status != "signed_out" || provider.MessageCode != MsgSignIn) {
		t.Fatalf("status=%q code=%q", provider.Status, provider.MessageCode)
	}
}
