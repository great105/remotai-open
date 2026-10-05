package web

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"tgcontrol/internal/config"
	"tgcontrol/internal/hermes"
)

func TestHermesBackendUIDPreservesOtherUsers(t *testing.T) {
	cases := []struct {
		name      string
		cfg       config.Config
		uid, want int64
	}{
		{"legacy paired owner", config.Config{Mode: config.ModeCentralBot, TelegramUserID: "42"}, 42, 1},
		{"explicit fallback owner", config.Config{Mode: config.ModeCentralBot, TelegramUserID: "42", APITokenUID: 1}, 42, 1},
		{"other allowed user", config.Config{Mode: config.ModeCentralBot, TelegramUserID: "42"}, 43, 43},
		{"conflicting token owner", config.Config{Mode: config.ModeCentralBot, TelegramUserID: "42", APITokenUID: 99}, 42, 42},
		{"own bot has no proven owner", config.Config{Mode: config.ModeOwnBot, TelegramUserID: "42"}, 42, 42},
		{"malformed owner", config.Config{Mode: config.ModeCentralBot, TelegramUserID: "invalid"}, 42, 42},
		{"standalone", config.Config{Mode: config.ModeStandalone}, 1, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := hermesBackendUID(&tc.cfg, tc.uid); got != tc.want {
				t.Fatalf("identity %d mapped to %d, want %d", tc.uid, got, tc.want)
			}
		})
	}
}

type hermesWebFixture struct {
	mu           sync.Mutex
	status       hermes.Status
	start        func(context.Context) error
	closeRuntime func(context.Context) error
	do           func(context.Context, string, string, io.Reader) (*http.Response, error)
	frames       hermes.EventBatch
}

func (f *hermesWebFixture) Status() hermes.Status         { f.mu.Lock(); defer f.mu.Unlock(); return f.status }
func (f *hermesWebFixture) Install(context.Context) error { return nil }
func (f *hermesWebFixture) Start(ctx context.Context) error {
	if f.start != nil {
		return f.start(ctx)
	}
	return nil
}
func (f *hermesWebFixture) Stop(context.Context) error { return nil }
func (f *hermesWebFixture) Close(ctx context.Context) error {
	if f.closeRuntime != nil {
		return f.closeRuntime(ctx)
	}
	return nil
}
func (f *hermesWebFixture) Update(context.Context) error      { return nil }
func (f *hermesWebFixture) CheckUpdate(context.Context) error { return nil }
func (f *hermesWebFixture) SetAutoUpdate(v bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status.AutoUpdate = v
	return nil
}
func (f *hermesWebFixture) StartMaintenance(context.Context) {}
func (f *hermesWebFixture) Do(ctx context.Context, method, path string, body io.Reader) (*http.Response, error) {
	return f.do(ctx, method, path, body)
}
func (f *hermesWebFixture) RPC(context.Context, string, any) (json.RawMessage, error) {
	return json.RawMessage(`{"session_id":"fixture"}`), nil
}
func (f *hermesWebFixture) Reply(context.Context, string, json.RawMessage) error { return nil }
func (f *hermesWebFixture) Events(uint64) hermes.EventBatch                      { return f.frames }

func TestHermesBackendPathCannotReachTokenOrRevealRoutes(t *testing.T) {
	for _, test := range []struct{ path, query string }{
		{"/api/hermes/backend/auth/token", ""},
		{"/api/hermes/backend/env/reveal", ""},
		{"/api/hermes/backend/../auth", ""},
		{"/api/hermes/backend/sessions/%2e%2e/auth", ""},
		{"/api/hermes/backend/sessions\\auth", ""},
		{"/api/hermes/backend/config", "session_token=fixture"},
		{"/api/hermes/backend/env", "reveal=true"},
		{"/api/hermes/backend/config", ""},
		{"/api/hermes/backend/health/retirement", ""},
		{"/api/hermes/backend/mcp/servers/private", ""},
	} {
		if _, err := hermesBackendPath(test.path, test.query); err == nil {
			t.Errorf("accepted %s?%s", test.path, test.query)
		}
	}
	got, err := hermesBackendPath("/api/hermes/backend/providers/oauth/openai-codex/poll/fixture", "profile=default")
	if err != nil || got != "/api/providers/oauth/openai-codex/poll/fixture?profile=default" {
		t.Fatalf("OAuth polling unreachable: %q %v", got, err)
	}
}

func TestHermesGatewayRejectsCredentialReadBypasses(t *testing.T) {
	for _, method := range []string{"cli.exec", "config.get", "auth.get", "unknown.future.method"} {
		if hermesRPCAllowed(method, json.RawMessage(`{"key":"full"}`)) {
			t.Errorf("unsafe gateway method admitted: %s", method)
		}
	}
	if hermesRPCAllowed("config.set", json.RawMessage(`{"key":"api_key"}`)) {
		t.Fatal("credential-bearing configuration write admitted")
	}
	for _, method := range []string{"prompt.submit", "session.resume", "commands.catalog", "model.save_key"} {
		if !hermesRPCAllowed(method, json.RawMessage(`{}`)) {
			t.Errorf("native flow unreachable: %s", method)
		}
	}
	if !hermesRPCAllowed("config.set", json.RawMessage(`{"key":"model","value":"openai-codex:fixture"}`)) {
		t.Fatal("live model selection unreachable")
	}
}

func TestHermesBackendKeepsRuntimeHeadersOnDevice(t *testing.T) {
	f := &hermesWebFixture{do: func(ctx context.Context, method, path string, body io.Reader) (*http.Response, error) {
		if method != "POST" || path != "/api/providers/oauth/openai-codex/start" {
			t.Fatalf("unexpected proxy %s %s", method, path)
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}, "X-Hermes-Session-Token": {"private-fixture"}, "Set-Cookie": {"hermes=private-fixture"}}, Body: io.NopCloser(strings.NewReader(`{"session_id":"device-login","user_code":"1234-5678"}`))}, nil
	}}
	s := &Server{hermesManagers: map[int64]hermesRuntime{1: f}}
	r := httptest.NewRequest("POST", "/api/hermes/backend/providers/oauth/openai-codex/start", strings.NewReader(`{}`))
	r.Header.Set("Authorization", "Bearer browser-fixture")
	w := httptest.NewRecorder()
	s.apiHermesBackend(w, r, 1)
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if w.Header().Get("X-Hermes-Session-Token") != "" || w.Header().Get("Set-Cookie") != "" || strings.Contains(w.Body.String(), "private-fixture") {
		t.Fatal("upstream token left the device")
	}
}

func TestHermesEventsStayScopedToRemotaiIdentity(t *testing.T) {
	a := &hermesWebFixture{frames: hermes.EventBatch{Events: []hermes.Event{{Seq: 1, Frame: json.RawMessage(`{"method":"approval","id":"owner-a"}`)}}}}
	b := &hermesWebFixture{frames: hermes.EventBatch{Events: []hermes.Event{{Seq: 1, Frame: json.RawMessage(`{"method":"approval","id":"owner-b"}`)}}}}
	s := &Server{hermesManagers: map[int64]hermesRuntime{1: a, 2: b}}
	w := httptest.NewRecorder()
	s.apiHermesEvents(w, httptest.NewRequest("GET", "/api/hermes/events?after=0", nil), 2)
	if strings.Contains(w.Body.String(), "owner-a") || !strings.Contains(w.Body.String(), "owner-b") {
		t.Fatalf("cross-user event leak: %s", w.Body.String())
	}
	w = httptest.NewRecorder()
	s.apiHermesEvents(w, httptest.NewRequest("GET", "/api/hermes/events?after=-1", nil), 2)
	if w.Code != 400 {
		t.Fatalf("invalid cursor admitted: %d", w.Code)
	}
}

func TestHermesProviderErrorKeepsActionableDetailWithoutSigningOutRemotai(t *testing.T) {
	f := &hermesWebFixture{do: func(context.Context, string, string, io.Reader) (*http.Response, error) {
		return &http.Response{StatusCode: 401, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"detail":"Enable device code login in account security settings."}`))}, nil
	}}
	s := &Server{hermesManagers: map[int64]hermesRuntime{1: f}}
	w := httptest.NewRecorder()
	s.apiHermesBackend(w, httptest.NewRequest("POST", "/api/hermes/backend/providers/oauth/openai-codex/start", strings.NewReader(`{}`)), 1)
	if w.Code != http.StatusBadGateway || !strings.Contains(w.Body.String(), "hermes_backend_error") || !strings.Contains(w.Body.String(), "Enable device code login") {
		t.Fatalf("provider guidance lost: %d %s", w.Code, w.Body.String())
	}
}

func TestHermesPreparationSurvivesPhoneDisconnectAndRejectsDuplicate(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	finished := make(chan error, 1)
	f := &hermesWebFixture{start: func(ctx context.Context) error {
		close(started)
		select {
		case <-release:
			finished <- nil
		case <-ctx.Done():
			finished <- ctx.Err()
		}
		return nil
	}}
	s := &Server{hermesManagers: map[int64]hermesRuntime{1: f}, eventBuf: newEventBuffer()}
	requestCtx, cancel := context.WithCancel(context.Background())
	r := httptest.NewRequest("POST", "/api/hermes/start", nil).WithContext(requestCtx)
	w := httptest.NewRecorder()
	s.apiHermesOperation("start")(w, r, 1)
	if w.Code != 202 {
		t.Fatalf("start %d %s", w.Code, w.Body.String())
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("preparation never started")
	}
	cancel()
	w = httptest.NewRecorder()
	s.apiHermesOperation("start")(w, httptest.NewRequest("POST", "/api/hermes/start", nil), 1)
	if w.Code != 409 {
		t.Fatalf("duplicate operation %d", w.Code)
	}
	close(release)
	select {
	case err := <-finished:
		if err != nil {
			t.Fatalf("phone disconnected and killed preparation: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("preparation stalled")
	}
}

func TestHermesShutdownRejectsFirstLateRequest(t *testing.T) {
	s := &Server{}
	s.shutdownHermes(context.Background())
	// The closing check precedes config.GetNoSetup and filesystem discovery.
	// This probe must never create a profile or even load the host config.
	if mgr, err := s.hermesForUser(99); mgr != nil || !errors.Is(err, hermes.ErrClosed) {
		t.Fatalf("late manager=%v err=%v", mgr, err)
	}
	w := httptest.NewRecorder()
	s.apiHermesOperation("install")(w, httptest.NewRequest("POST", "/api/hermes/install", nil), 99)
	if w.Code < 400 {
		t.Fatalf("late installation accepted: %d", w.Code)
	}
	s.hermesMu.Lock()
	defer s.hermesMu.Unlock()
	if s.hermesManagers != nil || s.hermesCtx != nil || s.hermesJobs != nil {
		t.Fatal("shutdown request created manager/context/background job")
	}
}

func TestHermesShutdownBlocksPreviouslyReturnedManager(t *testing.T) {
	starts, closes := 0, 0
	f := &hermesWebFixture{start: func(context.Context) error { starts++; return nil }, closeRuntime: func(context.Context) error { closes++; return nil }}
	s := &Server{hermesManagers: map[int64]hermesRuntime{1: f}, eventBuf: newEventBuffer()}
	// Simulate the exact admission gap: lookup returned f, then shutdown took
	// its snapshot before the handler acquired hermesMu to enqueue the job.
	previouslyReturned := hermesRuntime(f)
	s.shutdownHermes(context.Background())
	w := httptest.NewRecorder()
	s.enqueueHermesOperation(w, "start", 1, previouslyReturned)
	if w.Code != http.StatusServiceUnavailable || starts != 0 || closes != 1 {
		t.Fatalf("late operation code=%d starts=%d closes=%d", w.Code, starts, closes)
	}
	s.hermesMu.Lock()
	defer s.hermesMu.Unlock()
	if s.hermesCtx != nil || s.hermesJobs != nil {
		t.Fatal("late operation created lifetime/context/job")
	}
}

func TestHermesShutdownCancelsAlreadyAdmittedPreparation(t *testing.T) {
	started := make(chan struct{})
	finished := make(chan struct{})
	f := &hermesWebFixture{}
	f.start = func(ctx context.Context) error {
		f.mu.Lock()
		f.status.Running = true
		f.mu.Unlock()
		close(started)
		<-ctx.Done()
		f.mu.Lock()
		f.status.Running = false
		f.mu.Unlock()
		close(finished)
		return ctx.Err()
	}
	f.closeRuntime = func(ctx context.Context) error {
		select {
		case <-finished:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	s := &Server{hermesManagers: map[int64]hermesRuntime{1: f}, eventBuf: newEventBuffer()}
	w := httptest.NewRecorder()
	s.enqueueHermesOperation(w, "start", 1, f)
	if w.Code != http.StatusAccepted {
		t.Fatalf("admission code=%d", w.Code)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("fixture did not start")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	s.shutdownHermes(ctx)
	if f.Status().Running {
		t.Fatal("owned preparation survived server shutdown")
	}
	select {
	case <-finished:
	default:
		t.Fatal("preparation context not cancelled")
	}
	deadline := time.Now().Add(time.Second)
	for {
		s.hermesMu.Lock()
		pending := s.hermesJobs[1]
		s.hermesMu.Unlock()
		if !pending {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("completed shutdown left job reservation")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestHermesSubscriptionLinksUseSystemBrowserAllowlist(t *testing.T) {
	for _, raw := range []string{"https://auth.openai.com/codex/device", "https://portal.nousresearch.com/auth/device"} {
		if got, err := allowedExternalURL(raw); err != nil || got != raw {
			t.Errorf("login unreachable: %q %v", got, err)
		}
	}
	for _, raw := range []string{"http://auth.openai.com/codex/device", "https://auth.openai.com:8443/device", "https://user@auth.openai.com/device", "https://auth.openai.com.evil.example/device"} {
		if _, err := allowedExternalURL(raw); err == nil {
			t.Errorf("untrusted login accepted: %s", raw)
		}
	}
}
