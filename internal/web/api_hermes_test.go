package web

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
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
	rpc          func(context.Context, string, any) (json.RawMessage, error)
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
func (f *hermesWebFixture) RPC(ctx context.Context, method string, params any) (json.RawMessage, error) {
	if f.rpc != nil {
		return f.rpc(ctx, method, params)
	}
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
	for _, method := range []string{"session.resume", "commands.catalog", "model.save_key"} {
		if !hermesRPCAllowed(method, json.RawMessage(`{}`)) {
			t.Errorf("native flow unreachable: %s", method)
		}
	}
	if !hermesRPCAllowed("config.set", json.RawMessage(`{"key":"model","value":"openai-codex:fixture"}`)) {
		t.Fatal("live model selection unreachable")
	}
}

func TestHermesSubagentRosterAllowsOnlyBoundedDefaultSessionRead(t *testing.T) {
	for _, params := range []string{`{"session_id":"live-A"}`, `{"profile":"default","session_id":"live-A"}`} {
		if !hermesRPCAllowed("subagent.list", json.RawMessage(params)) {
			t.Fatalf("owned roster read rejected: %s", params)
		}
	}
	for _, params := range []string{`{}`, `null`, `[]`, `{"session_id":null}`, `{"session_id":42}`,
		`{"session_id":""}`, `{"session_id":" live-A "}`, `{"session_id":"live-A","profile":"other"}`,
		`{"session_id":"live-A","profile":"all"}`, `{"session_id":"live-A","profile":null}`,
		`{"session_id":"live-A","owner":"other"}`, `{"session_id":"live-A","subagent_id":"child"}`,
		`{"session_id":"` + strings.Repeat("x", 257) + `"}`, `{"session_id":"live-A"} {}`} {
		if hermesRPCAllowed("subagent.list", json.RawMessage(params)) {
			t.Fatalf("unbounded or ambiguous roster read admitted: %s", params)
		}
	}
	for _, method := range []string{"subagent.interrupt", "subagent.steer", "subagent.spawn", "subagent.tail", "subagent.list_all", "spawn_tree.save"} {
		if hermesRPCAllowed(method, json.RawMessage(`{"profile":"default","session_id":"live-A"}`)) {
			t.Fatalf("new subagent control or sibling RPC admitted: %s", method)
		}
	}
}

func TestHermesSubagentRosterUsesAuthenticatedBackendUID(t *testing.T) {
	s := &Server{hermesManagers: map[int64]hermesRuntime{}}
	calls := map[int64]int{}
	for _, uid := range []int64{1, 2} {
		s.hermesManagers[uid] = &hermesWebFixture{rpc: func(_ context.Context, method string, params any) (json.RawMessage, error) {
			calls[uid]++
			if method != "subagent.list" {
				t.Fatalf("unexpected method %s", method)
			}
			var request map[string]string
			if raw, ok := params.(json.RawMessage); !ok || json.Unmarshal(raw, &request) != nil || request["session_id"] != "live-A" || request["profile"] != "default" {
				t.Fatal("roster request lost its live session/default profile")
			}
			return json.Marshal(map[string]any{"subagents": []map[string]any{{"subagent_id": strconv.FormatInt(uid, 10), "status": "running"}}})
		}}
	}
	for _, uid := range []int64{1, 2} {
		w := httptest.NewRecorder()
		s.apiHermesRPC(w, httptest.NewRequest("POST", "/api/hermes/rpc", strings.NewReader(`{"method":"subagent.list","params":{"profile":"default","session_id":"live-A"}}`)), uid)
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"subagent_id":"`+strconv.FormatInt(uid, 10)+`"`) {
			t.Fatalf("UID %d used another backend: %d %s", uid, w.Code, w.Body.String())
		}
	}
	if calls[1] != 1 || calls[2] != 1 {
		t.Fatalf("roster crossed backend namespaces: %v", calls)
	}
}

func TestHermesSubagentRosterPreservesUpstreamOwnershipFailure(t *testing.T) {
	f := &hermesWebFixture{rpc: func(_ context.Context, method string, _ any) (json.RawMessage, error) {
		return nil, &hermes.RPCError{Code: 4001, Message: "session not found or not owned by this transport"}
	}}
	s := &Server{hermesManagers: map[int64]hermesRuntime{1: f}}
	w := httptest.NewRecorder()
	s.apiHermesRPC(w, httptest.NewRequest("POST", "/api/hermes/rpc", strings.NewReader(`{"method":"subagent.list","params":{"session_id":"foreign-live"}}`)), 1)
	if w.Code != http.StatusBadGateway || !strings.Contains(w.Body.String(), "4001") || strings.Contains(w.Body.String(), `"subagents":[]`) {
		t.Fatalf("ownership failure became a confirmed empty roster: %d %s", w.Code, w.Body.String())
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

func TestHermesWorkBackendUsesExactRoutesAndVerbs(t *testing.T) {
	for _, route := range []struct {
		path    string
		methods []string
	}{
		{"cron/jobs", []string{"GET", "POST"}},
		{"cron/jobs/job-A_1", []string{"GET", "PUT", "DELETE"}},
		{"cron/jobs/job-A_1/pause", []string{"POST"}},
		{"cron/jobs/job-A_1/resume", []string{"POST"}},
		{"cron/jobs/job-A_1/runs", []string{"GET"}},
		{"memory", []string{"GET"}},
		{"skills", []string{"GET"}},
		{"skills/content?name=development%2Fgithub", []string{"GET"}},
		{"skills/toggle", []string{"PUT"}},
		{"profiles/default/soul", []string{"GET", "PUT"}},
	} {
		for _, method := range []string{"GET", "POST", "PUT", "PATCH", "DELETE", "HEAD"} {
			t.Run(method+" "+route.path, func(t *testing.T) {
				allowed := false
				for _, candidate := range route.methods {
					allowed = allowed || candidate == method
				}
				calls := 0
				catalogCalls := 0
				f := &hermesWebFixture{do: func(_ context.Context, gotMethod, path string, body io.Reader) (*http.Response, error) {
					if strings.HasPrefix(route.path, "cron/jobs/") && path == "/api/cron/jobs?profile=default" {
						catalogCalls++
						if gotMethod != "GET" || body != nil {
							t.Fatal("item membership must use a read-only catalog request")
						}
						return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`[{"id":"job-A_1"}]`))}, nil
					}
					calls++
					wanted := "/api/" + strings.Split(route.path, "?")[0] + "?profile=default"
					if strings.Contains(route.path, "?") {
						wanted = "/api/skills/content?name=development%2Fgithub&profile=default"
					}
					if route.path == "profiles/default/soul" {
						wanted = "/api/profiles/default/soul"
					}
					if gotMethod != method || path != wanted {
						t.Fatalf("unexpected upstream route: %s %s", gotMethod, path)
					}
					if route.path == "skills/toggle" {
						var value map[string]any
						if json.NewDecoder(body).Decode(&value) != nil || value["profile"] != "default" || value["name"] != "development/github" || value["enabled"] != false {
							t.Fatal("skill toggle did not preserve its explicit profile and values")
						}
					}
					if route.path == "profiles/default/soul" && method == "PUT" {
						var value map[string]any
						if json.NewDecoder(body).Decode(&value) != nil || len(value) != 1 || value["content"] != "Говори кратко." {
							t.Fatal("SOUL instruction changed or gained an unrelated field")
						}
					}
					return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}, "X-Hermes-Session-Token": {"host-only-fixture"}, "Set-Cookie": {"host-only=fixture"}}, Body: io.NopCloser(strings.NewReader(`{"ok":true}`))}, nil
				}}
				other := &hermesWebFixture{do: func(context.Context, string, string, io.Reader) (*http.Response, error) {
					t.Fatal("request reached a different user's backend")
					return nil, nil
				}}
				s := &Server{hermesManagers: map[int64]hermesRuntime{1: other, 2: f}}
				body := `{}`
				if route.path == "skills/toggle" {
					body = `{"name":"development/github","enabled":false}`
				}
				if route.path == "profiles/default/soul" {
					body = `{"content":"Говори кратко."}`
				}
				w := httptest.NewRecorder()
				s.apiHermesBackend(w, httptest.NewRequest(method, "/api/hermes/backend/"+route.path, strings.NewReader(body)), 2)
				if allowed {
					if w.Code != 200 || calls != 1 {
						t.Fatalf("accepted route failed: status=%d calls=%d body=%s", w.Code, calls, w.Body.String())
					}
					if w.Header().Get("X-Hermes-Session-Token") != "" || w.Header().Get("Set-Cookie") != "" || strings.Contains(w.Body.String(), "host-only") {
						t.Fatal("upstream identity escaped to the browser")
					}
					if strings.HasPrefix(route.path, "cron/jobs/") && catalogCalls != 1 {
						t.Fatal("item request bypassed the default catalog")
					}
				} else if w.Code != http.StatusMethodNotAllowed || calls != 0 || catalogCalls != 0 {
					t.Fatalf("forbidden verb reached upstream: status=%d calls=%d", w.Code, calls)
				}
			})
		}
	}
}

type hermesCountedRequestBody struct {
	io.Reader
	reads int
}

func (body *hermesCountedRequestBody) Read(buffer []byte) (int, error) {
	body.reads++
	return body.Reader.Read(buffer)
}

func (*hermesCountedRequestBody) Close() error { return nil }

func TestHermesCronItemsFailClosedBeforeReadingMutationBody(t *testing.T) {
	for _, request := range []struct{ method, path string }{
		{"GET", "foreign-id"}, {"PUT", "foreign-id"}, {"DELETE", "foreign-id"},
		{"POST", "foreign-id/pause"}, {"POST", "foreign-id/resume"}, {"GET", "foreign-id/runs?limit=20"},
	} {
		for _, catalog := range []struct {
			name, content string
			status        int
			want          int
		}{
			{"absent or name only", `[{"id":"default-id","name":"foreign-id"}]`, 200, 404},
			{"unavailable", `{"error":"private-catalog-fixture"}`, 500, 502},
			{"malformed", `{"private-catalog-fixture":true}`, 200, 502},
			{"oversize", strings.Repeat("x", (8<<20)+1), 200, 502},
			{"too many rows", `[` + strings.Repeat(`{"id":"default-id"},`, 4096) + `{"id":"foreign-id"}]`, 200, 502},
		} {
			t.Run(request.method+" "+request.path+" "+catalog.name, func(t *testing.T) {
				calls := 0
				f := &hermesWebFixture{do: func(_ context.Context, method, path string, body io.Reader) (*http.Response, error) {
					calls++
					if method != "GET" || path != "/api/cron/jobs?profile=default" || body != nil {
						t.Fatal("unverified item reached an upstream mutation or another profile")
					}
					return &http.Response{StatusCode: catalog.status, Header: http.Header{"X-Hermes-Session-Token": {"private-catalog-fixture"}}, Body: io.NopCloser(strings.NewReader(catalog.content))}, nil
				}}
				s := &Server{hermesManagers: map[int64]hermesRuntime{2: f}}
				body := &hermesCountedRequestBody{Reader: strings.NewReader(`{"updates":{"prompt":"fixture"}}`)}
				url := "/api/hermes/backend/cron/jobs/" + request.path
				w := httptest.NewRecorder()
				s.apiHermesBackend(w, httptest.NewRequest(request.method, url, body), 2)
				if w.Code != catalog.want || calls != 1 || body.reads != 0 {
					t.Fatalf("catalog failure did not fence the item: status=%d calls=%d reads=%d", w.Code, calls, body.reads)
				}
				if w.Header().Get("X-Hermes-Session-Token") != "" || strings.Contains(w.Body.String(), "private-catalog-fixture") {
					t.Fatal("private catalog diagnostic reached the browser")
				}
			})
		}
	}
}

func TestHermesWorkBackendRejectsUnknownPathsAndQueryBypasses(t *testing.T) {
	for _, path := range []string{
		"cron/jobs/", "cron/jobs/job.name", "cron/jobs/job/id/runs", "cron/jobs/job/trigger",
		"cron/jobs/job/unknown", "cron/jobs/job%2Fruns", "cron/jobs/%2e%2e/runs", "cron/jobs/job%252Fruns",
		"cron/jobs?profile=all", "cron/jobs?profile=other", "cron/jobs?profile=default&profile=all",
		"cron/jobs?limit=2", "cron/jobs?exec=fixture", "cron/jobs?reveal=true", "cron/jobs?session_token=fixture",
		"cron/jobs/job/runs?limit=0", "cron/jobs/job/runs?limit=101", "cron/jobs/job/runs?limit=NaN", "cron/jobs/job/runs?limit=1&limit=2",
		"memory/reset", "memory?provider=fixture", "skills/install", "skills/content",
		"skills/content?name=../private", "skills/content?name=%2Fprivate", "skills/content?name=folder%5Cprivate",
		"skills/content?name=one&name=two", "skills?name=private", "skills/toggle?enabled=true",
		"config", "config?key=full", "env/reveal", "health/retirement",
		"profiles/other/soul", "profiles/default/config", "profiles/default/soul?profile=default", "profiles/default/soul?profile=other",
	} {
		t.Run(path, func(t *testing.T) {
			calls := 0
			f := &hermesWebFixture{do: func(context.Context, string, string, io.Reader) (*http.Response, error) {
				calls++
				return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{}`))}, nil
			}}
			s := &Server{hermesManagers: map[int64]hermesRuntime{2: f}}
			w := httptest.NewRecorder()
			s.apiHermesBackend(w, httptest.NewRequest("GET", "/api/hermes/backend/"+path, nil), 2)
			if w.Code != http.StatusBadRequest || calls != 0 {
				t.Fatalf("invalid path/query reached upstream: status=%d calls=%d", w.Code, calls)
			}
		})
	}
	path, err := hermesBackendPath("/api/hermes/backend/cron/jobs/job/runs", "limit=100&profile=default")
	if err != nil || path != "/api/cron/jobs/job/runs?limit=100&profile=default" {
		t.Fatalf("bounded run history unavailable: %s %v", path, err)
	}
}

func TestHermesSkillToggleCannotOverrideProfileOrCallAnotherAction(t *testing.T) {
	for _, body := range []string{
		`{"name":"github","enabled":true,"profile":"other"}`,
		`{"name":"github","enabled":true,"profile":"all"}`,
		`{"name":"github","enabled":true,"script":"fixture"}`,
		`{"name":"github","enabled":"true"}`,
		`{"name":"github","enabled":null}`,
		`{"name":"../private","enabled":false}`,
		`{"name":"github","enabled":true} {"profile":"other"}`,
	} {
		calls := 0
		f := &hermesWebFixture{do: func(context.Context, string, string, io.Reader) (*http.Response, error) {
			calls++
			return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{}`))}, nil
		}}
		s := &Server{hermesManagers: map[int64]hermesRuntime{2: f}}
		w := httptest.NewRecorder()
		s.apiHermesBackend(w, httptest.NewRequest("PUT", "/api/hermes/backend/skills/toggle", strings.NewReader(body)), 2)
		if w.Code != http.StatusBadRequest || calls != 0 {
			t.Fatalf("invalid toggle reached upstream: status=%d calls=%d", w.Code, calls)
		}
	}
}

func TestHermesSoulWritesRequireOnlyBoundedStringContent(t *testing.T) {
	for _, body := range []string{
		"", `{}`, `{"content":null}`, `{"content":12}`, `{"content":false}`,
		`{"content":"fixture","profile":"other"}`, `{"content":"fixture","api_key":"fixture"}`,
		`{"content":"fixture"} {"content":"second"}`,
		`{"content":"` + strings.Repeat("x", 1<<20) + `"}`,
	} {
		calls := 0
		f := &hermesWebFixture{do: func(context.Context, string, string, io.Reader) (*http.Response, error) {
			calls++
			return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{}`))}, nil
		}}
		s := &Server{hermesManagers: map[int64]hermesRuntime{2: f}}
		w := httptest.NewRecorder()
		s.apiHermesBackend(w, httptest.NewRequest("PUT", "/api/hermes/backend/profiles/default/soul", strings.NewReader(body)), 2)
		if w.Code != http.StatusBadRequest || calls != 0 {
			t.Fatalf("invalid SOUL body reached upstream: status=%d calls=%d", w.Code, calls)
		}
	}
	called := false
	f := &hermesWebFixture{do: func(_ context.Context, method, path string, body io.Reader) (*http.Response, error) {
		called = true
		var value map[string]any
		if json.NewDecoder(body).Decode(&value) != nil || value["content"] != "" || method != "PUT" || path != "/api/profiles/default/soul" {
			t.Fatal("explicit instruction reset did not preserve empty string content")
		}
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"ok":true}`))}, nil
	}}
	s := &Server{hermesManagers: map[int64]hermesRuntime{2: f}}
	w := httptest.NewRecorder()
	s.apiHermesBackend(w, httptest.NewRequest("PUT", "/api/hermes/backend/profiles/default/soul", strings.NewReader(`{"content":""}`)), 2)
	if w.Code != 200 || !called {
		t.Fatalf("instruction reset unavailable: status=%d", w.Code)
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
