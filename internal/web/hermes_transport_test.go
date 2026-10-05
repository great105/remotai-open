package web

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"tgcontrol/internal/config"
	"tgcontrol/internal/relay"
)

// Use a fresh process: both paths and config are singletons. No host credentials,
// runtime, cron or memory are read or changed. Only the upstream runtime is a fixture;
// relay framing/buildRequest/authentication/routing/path and body guards are real.
func TestHermesAuthenticatedTransport(t *testing.T) {
	const childEnv = "REMOTAI_HERMES_TRANSPORT_CHILD"
	if os.Getenv(childEnv) != "1" {
		home := t.TempDir()
		dir := filepath.Join(home, ".config", "remotai")
		if runtime.GOOS == "windows" {
			dir = filepath.Join(home, "AppData", "Local", "Remotai")
		}
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"mode":"standalone","setup_complete":true,"api_token":"isolated-transport-fixture","api_token_uid":2}`), 0600); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestHermesAuthenticatedTransport$", "-test.v", "-test.count=1")
		cmd.Env = append(os.Environ(), "USERPROFILE="+home, "HOME="+home, childEnv+"=1")
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("isolated transport regression: %v\n%s", err, output)
		}
		t.Log(string(output))
		return
	}
	cfg := config.GetNoSetup()
	var calls []string
	var callMu sync.Mutex
	count := func() int { callMu.Lock(); defer callMu.Unlock(); return len(calls) }
	f := &hermesWebFixture{do: func(_ context.Context, method, path string, body io.Reader) (*http.Response, error) {
		callMu.Lock()
		defer callMu.Unlock()
		calls = append(calls, method+" "+path)
		if strings.Contains(path, "transport") || strings.Contains(path, "initData") {
			t.Error("transport metadata leaked to upstream")
		}
		content := `{}`
		switch {
		case method == "GET" && path == "/api/cron/jobs?profile=default":
			content = `[{"id":"fixture-job","name":"Изолированная задача","prompt":"Fixture only","enabled":true}]`
		case method == "GET" && path == "/api/memory?profile=default":
			content = `{"active":"builtin","builtin_files":{"memory":1,"user":1}}`
		case method == "GET" && path == "/api/skills?profile=default":
			content = `[{"name":"github","description":"Изолированный навык","enabled":true}]`
		case method == "GET" && strings.HasPrefix(path, "/api/skills/content?"):
			content = `{"name":"github","content":"Инструкции изолированного навыка"}`
		case method == "GET" && path == "/api/profiles/default/soul":
			content = `{"content":"Изолированные инструкции"}`
		case method == "GET" && strings.HasPrefix(path, "/api/cron/jobs/fixture-job/runs?"):
			content = `{"runs":[]}`
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(content))}, nil
	}}
	s := &Server{mux: http.NewServeMux(), hermesManagers: map[int64]hermesRuntime{2: f}}
	s.registerHermesRoutes()
	// The relay fixture speaks the actual agent protocol. It has no external sockets.
	peers := make(chan *websocket.Conn, 1)
	finish := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		var hello relay.Hello
		if conn.ReadJSON(&hello) != nil {
			return
		}
		if conn.WriteJSON(relay.Welcome{Type: relay.MsgWelcome, SessionID: "fixture", UserID: 2}) != nil {
			return
		}
		peers <- conn
		<-finish
	}))
	defer upstream.Close()
	cfg.RelayURL = upstream.URL
	cfg.RelayBaseURL = upstream.URL
	cfg.DeviceID = "fixture-device"
	cfg.RelayJWT = "fixture-device-jwt"
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); relay.New(s.mux).ServeForever(ctx) }()
	defer func() {
		cancel()
		close(finish)
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("relay fixture failed to stop")
		}
	}()
	var peer *websocket.Conn
	select {
	case peer = <-peers:
	case <-time.After(3 * time.Second):
		t.Fatal("relay handshake timed out")
	}
	invoke := func(method, path, body string) relay.CmdResult {
		t.Helper()
		peer.SetWriteDeadline(time.Now().Add(3 * time.Second))
		if err := peer.WriteJSON(relay.Cmd{Type: relay.MsgCmd, RequestID: "fixture", Method: method, Path: "/api/hermes/backend/" + path, Body: []byte(body)}); err != nil {
			t.Fatal(err)
		}
		peer.SetReadDeadline(time.Now().Add(3 * time.Second))
		var result relay.CmdResult
		if err := peer.ReadJSON(&result); err != nil {
			t.Fatal(err)
		}
		return result
	}
	result := invoke("GET", "cron/jobs?profile=default", "")
	if result.StatusCode != 200 {
		t.Fatalf("production relay/auth/proxy returned %d: %s", result.StatusCode, result.Body)
	}
	if count() != 1 {
		t.Fatalf("incorrect forwarding count: %d", count())
	}
	for _, tc := range []struct{ method, path, body, want string }{
		{"GET", "memory?profile=default", "", "/api/memory?profile=default"},
		{"GET", "skills?profile=default", "", "/api/skills?profile=default"},
		{"GET", "skills/content?profile=default&name=github", "", "/api/skills/content?name=github&profile=default"},
		{"PUT", "skills/toggle?profile=default", `{"name":"github","enabled":true}`, "/api/skills/toggle?profile=default"},
		{"GET", "profiles/default/soul", "", "/api/profiles/default/soul"},
		{"PUT", "profiles/default/soul", `{"content":"fixture"}`, "/api/profiles/default/soul"},
		{"POST", "cron/jobs?profile=default", `{"prompt":"fixture","profile":"default"}`, "/api/cron/jobs?profile=default"},
		{"GET", "cron/jobs/fixture-job?profile=default", "", "/api/cron/jobs/fixture-job?profile=default"},
		{"PUT", "cron/jobs/fixture-job?profile=default", `{"updates":{"name":"fixture"}}`, "/api/cron/jobs/fixture-job?profile=default"},
		{"DELETE", "cron/jobs/fixture-job?profile=default", "", "/api/cron/jobs/fixture-job?profile=default"},
		{"POST", "cron/jobs/fixture-job/pause?profile=default", `{}`, "/api/cron/jobs/fixture-job/pause?profile=default"},
		{"POST", "cron/jobs/fixture-job/resume?profile=default", `{}`, "/api/cron/jobs/fixture-job/resume?profile=default"},
		{"GET", "cron/jobs/fixture-job/runs?profile=default&limit=20", "", "/api/cron/jobs/fixture-job/runs?limit=20&profile=default"},
	} {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			result := invoke(tc.method, tc.path, tc.body)
			if result.StatusCode != 200 {
				t.Fatalf("relay status %d: %s", result.StatusCode, result.Body)
			}
			callMu.Lock()
			last := calls[len(calls)-1]
			callMu.Unlock()
			if last != tc.method+" "+tc.want {
				t.Fatalf("forwarded %s", last)
			}
		})
	}
	for _, path := range []string{
		"memory?extra=true", "memory?profile=other", "memory?profile=all",
		"memory?profile=default&profile=other", "memory?session_token=fixture", "memory?reveal=true",
		"memory?initData=one&initData=two", "memory?InitData=fixture", "memory?transportation=relay",
		"skills/content?name=../private", "cron/jobs/fixture-job/runs?limit=101",
		"auth/token", "env/reveal", "config", "cron/jobs/fixture-job/unknown",
	} {
		before := count()
		result := invoke("GET", path, "")
		if result.StatusCode != 400 || count() != before {
			t.Errorf("unsafe relay request %s: status %d calls %d", path, result.StatusCode, count()-before)
		}
	}
	before := count()
	if result := invoke("GET", "cron/jobs/other-job", ""); result.StatusCode != 404 || count() != before+1 {
		t.Errorf("cross-profile item admitted: %d", result.StatusCode)
	}
	if result := invoke("POST", "memory", `{}`); result.StatusCode != 405 {
		t.Errorf("method bypass: %d", result.StatusCode)
	}

	// Direct query-auth and headers both traverse the same registered production gate.
	for _, query := range []string{"profile=default", "profile=default&transport=relay"} {
		before := count()
		r := httptest.NewRequest("GET", "/api/hermes/backend/memory?"+query+"&initData=token:isolated-transport-fixture", nil)
		w := httptest.NewRecorder()
		s.mux.ServeHTTP(w, r)
		if w.Code != 200 || count() != before+1 {
			t.Fatalf("query-auth failed: %d %s", w.Code, w.Body.String())
		}
		if r.URL.Query().Get("initData") == "" {
			t.Fatal("original request metadata mutated")
		}
	}
	for _, auth := range []string{"", "token:invalid"} {
		before := count()
		r := httptest.NewRequest("GET", "/api/hermes/backend/memory?transport=relay&initData="+auth, nil)
		w := httptest.NewRecorder()
		s.mux.ServeHTTP(w, r)
		if w.Code != 401 || count() != before {
			t.Errorf("auth bypass: %d", w.Code)
		}
	}
	for _, query := range []string{"transport=relay&transport=lan", "initData=one&initData=two", "profile=default&profile=default", "bad=%ZZ"} {
		before := count()
		r := httptest.NewRequest("GET", "/api/hermes/backend/memory?"+query, nil)
		r.Header.Set("X-API-Token", "isolated-transport-fixture")
		w := httptest.NewRecorder()
		s.mux.ServeHTTP(w, r)
		if w.Code != 400 || count() != before {
			t.Errorf("duplicate or malformed query accepted: %s status %d", query, w.Code)
		}
	}
	if script := os.Getenv("REMOTAI_HERMES_BROWSER_SCRIPT"); script != "" {
		server := httptest.NewServer(s.mux)
		defer server.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, "node", script)
		cmd.Dir = filepath.Dir(filepath.Dir(filepath.Dir(script)))
		cmd.Env = append(os.Environ(), "QA_HERMES_BACKEND="+server.URL)
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("real browser/auth/proxy probe: %v\n%s", err, output)
		}
		t.Log(string(output))
	}
}
