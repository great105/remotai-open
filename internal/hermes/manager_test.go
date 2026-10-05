package hermes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

const fixtureCommit = "0123456789abcdef0123456789abcdef01234567"

// This executable is also a child-process fixture: cancellation and independent
// backend lifespan are exercised across the same process boundary as Hermes.
func TestHermesHelperProcess(t *testing.T) {
	if os.Getenv("REMOTAI_HERMES_HELPER") != "1" {
		return
	}
	args := os.Args
	for len(args) > 0 && args[0] != "--" {
		args = args[1:]
	}
	if len(args) < 2 {
		os.Exit(80)
	}
	args = args[2:]
	home := os.Getenv("HERMES_HOME")
	if home == "" || os.Getenv("OPENAI_API_KEY") != "" || os.Getenv("HERMES_API_KEY") != "" || os.Getenv("GITHUB_TOKEN") != "" {
		os.Exit(81)
	}
	root := filepath.Dir(home)
	for i, a := range args {
		if a == "-Stage" || a == "--stage" {
			stage := args[i+1]
			if stage == "products" || stage == "setup" || stage == "gateway" {
				os.Exit(82)
			}
			if isFile(filepath.Join(root, "slow-install")) {
				for {
					time.Sleep(time.Second)
				}
			}
			if stage == "repository" {
				path := filepath.Join(root, "runtime", ".hermes", "bin", "hermes")
				if runtime.GOOS == "windows" {
					path += ".exe"
				}
				_ = os.MkdirAll(filepath.Dir(path), 0700)
				_ = os.WriteFile(path, []byte("fixture"), 0600)
				_ = os.WriteFile(filepath.Join(root, "runtime", "pyproject.toml"), []byte("fixture"), 0600)
			}
			if stage == "venv" {
				store := filepath.Join(root, "toolchain", "store")
				path := filepath.Join(store, "python-fixture", "bin", "python3")
				if runtime.GOOS == "windows" {
					path = filepath.Join(store, "python-fixture", "python.exe")
				}
				_ = os.MkdirAll(filepath.Dir(path), 0700)
				_ = os.WriteFile(path, []byte("fixture"), 0600)
				_ = os.WriteFile(filepath.Join(store, "facts.json"), []byte(`{"packages":{"python":{"entry":"python-fixture"}}}`), 0600)
			}
			if stage == "complete" {
				_ = os.WriteFile(filepath.Join(root, "runtime", ".hermes-bootstrap-complete"), []byte("fixture"), 0600)
			}
			fmt.Println(`{"ok":true}`)
			os.Exit(0)
		}
	}
	for i, a := range args {
		if filepath.Base(a) == "private_entry.py" && i+2 < len(args) {
			mode := args[i+2]
			args = args[i+3:]
			if mode == "check" {
				fmt.Printf("REMOTAI_HERMES_UPDATE {\"available\":true,\"version\":\"fixture-2\",\"commit\":\"%s\"}\n", fixtureCommit)
				os.Exit(0)
			}
			if mode == "revision" {
				fmt.Println("REMOTAI_HERMES_REVISION " + fixtureCommit)
				os.Exit(0)
			}
			if mode == "complete" {
				os.Exit(0)
			}
			if mode != "cli" {
				os.Exit(83)
			}
			break
		}
	}
	if len(args) > 0 && args[0] == "serve" {
		if isFile(filepath.Join(root, "slow-start")) {
			for {
				time.Sleep(time.Second)
			}
		}
		fmt.Printf("HERMES_BACKEND_READY port=%s\n", os.Getenv("REMOTAI_HERMES_FIXTURE_PORT"))
		for {
			time.Sleep(time.Second)
		}
	}
	if len(args) > 0 && args[0] == "--version" {
		fmt.Println("fixture-1")
		os.Exit(0)
	}
	if len(args) > 0 && args[0] == "update" {
		if isFile(filepath.Join(root, "update-fails")) {
			os.Exit(84)
		}
		os.Exit(0)
	}
	os.Exit(85)
}

type testTransport struct{}

func (testTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Host == "hermes-agent.nousresearch.com" {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("# official-script fixture")), Header: make(http.Header), Request: req}, nil
	}
	return http.DefaultTransport.RoundTrip(req)
}

type fixture struct {
	t            *testing.T
	server       *httptest.Server
	mu           sync.Mutex
	token        string
	retirement   string
	actions      []string
	commands     [][]string
	connections  int
	capabilities bool
	promptTexts  []string
	replies      chan rpcFrame
}

func newFixture(t *testing.T) *fixture {
	f := &fixture{t: t, retirement: "idle", replies: make(chan rpcFrame, 4)}
	f.server = httptest.NewServer(http.HandlerFunc(f.serveHTTP))
	t.Cleanup(f.server.Close)
	return f
}
func (f *fixture) serveHTTP(w http.ResponseWriter, r *http.Request) {
	token := r.Header.Get("X-Hermes-Session-Token")
	f.mu.Lock()
	if f.token == "" {
		f.token = token
	}
	valid := len(token) >= 40 && token == f.token
	// A new owned process generates a different token after each restart.
	if r.URL.Path == "/api/health" && len(token) >= 40 {
		f.token = token
		valid = true
	}
	f.mu.Unlock()
	if !valid {
		http.Error(w, "token required", 401)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	switch r.URL.Path {
	case "/api/health":
		fmt.Fprint(w, `{"ok":true,"version":"fixture-1","auth_required":true}`)
	case "/api/redirect":
		http.Redirect(w, r, "http://example.invalid/leak", 302)
	case "/api/health/retirement":
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		mode := f.retirement
		f.actions = append(f.actions, body["action"])
		f.mu.Unlock()
		if mode == "missing" {
			http.NotFound(w, r)
			return
		}
		if body["action"] == "prepare" {
			switch mode {
			case "busy":
				fmt.Fprint(w, `{"ok":true,"idle":false}`)
			case "unknown":
				fmt.Fprint(w, `{"ok":true,"idle":null}`)
			default:
				fmt.Fprint(w, `{"ok":true,"idle":true,"token":"retirement-fixture"}`)
			}
		} else if body["token"] != "retirement-fixture" {
			http.Error(w, "bad retirement token", 400)
		} else if mode == "commit-fails" && body["action"] == "commit" {
			fmt.Fprint(w, `{"ok":false}`)
		} else {
			fmt.Fprint(w, `{"ok":true}`)
		}
	case "/api/ws":
		f.websocket(w, r)
	default:
		fmt.Fprint(w, `{"ok":true}`)
	}
}

func (f *fixture) websocket(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	valid := r.URL.Query().Get("token") == f.token
	f.mu.Unlock()
	if !valid {
		http.Error(w, "WS token required", 401)
		return
	}
	u := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	c, err := u.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer c.Close()
	f.mu.Lock()
	f.connections++
	f.mu.Unlock()
	var writeMu sync.Mutex
	send := func(v any) { writeMu.Lock(); defer writeMu.Unlock(); _ = c.WriteJSON(v) }
	send(map[string]any{"jsonrpc": "2.0", "method": "event", "params": map[string]any{"type": "gateway.ready"}})
	for {
		var frame rpcFrame
		if c.ReadJSON(&frame) != nil {
			return
		}
		if frame.Method == "" {
			select {
			case f.replies <- frame:
			default:
			}
			continue
		}
		id := rpcID(frame.ID)
		response := func(result any) { send(map[string]any{"jsonrpc": "2.0", "id": id, "result": result}) }
		switch frame.Method {
		case "client.capabilities":
			var flags map[string]bool
			_ = json.Unmarshal(frame.Params, &flags)
			f.mu.Lock()
			f.capabilities = flags["server_requests"]
			f.mu.Unlock()
			response(map[string]bool{"ok": true})
		case "tools.show":
			response(map[string]any{"sections": []any{map[string]any{"name": "fixture", "tools": []any{map[string]string{"name": "fixture_tool", "description": "Isolated fixture"}}}}, "total": 1})
		case "prompt.submit":
			var request struct {
				Text   string `json:"text"`
				Queued bool   `json:"queued"`
			}
			_ = json.Unmarshal(frame.Params, &request)
			f.mu.Lock()
			f.promptTexts = append(f.promptTexts, request.Text)
			f.mu.Unlock()
			status := "streaming"
			if request.Queued {
				status = "queued"
			}
			response(map[string]string{"status": status})
		case "session.steer":
			response(map[string]string{"status": "queued"})
		case "gateway.capabilities":
			response(map[string]bool{"per_session_exclusive_submit": true})
		case "fixture.error":
			send(map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": -32012, "message": "fixture-error", "data": map[string]string{"reason": "approval"}}})
		case "fixture.slow": // Request cancellation must leave the process and gateway alive.
		case "fixture.disconnect":
			return
		case "fixture.echo":
			var p struct {
				Value int `json:"value"`
				Delay int `json:"delay"`
			}
			_ = json.Unmarshal(frame.Params, &p)
			go func() {
				time.Sleep(time.Duration(p.Delay) * time.Millisecond)
				response(map[string]int{"value": p.Value})
			}()
		case "session.activate", "session.create", "session.resume":
			send(map[string]any{"jsonrpc": "2.0", "method": "event", "params": map[string]string{"type": "message.delta", "payload": "before-snapshot"}})
			response(map[string]any{"session_id": "fixture-session", "stored_session_id": "stored-fixture", "info": map[string]any{"cwd": ""}, "messages": []any{}})
			send(map[string]any{"jsonrpc": "2.0", "method": "event", "params": map[string]string{"type": "message.delta", "payload": "after-snapshot"}})
		case "fixture.approval":
			send(map[string]any{"jsonrpc": "2.0", "id": "approval-fixture", "method": "approval", "params": map[string]any{"session_id": "fixture-session", "request_id": "approval-fixture", "choices": []string{"once", "deny"}, "allow_session": false, "allow_permanent": false}})
			response(map[string]bool{"ok": true})
		case "fixture.renderer":
			send(map[string]any{"jsonrpc": "2.0", "id": "renderer-fixture", "method": "preview.read", "params": map[string]string{"session_id": "fixture-session"}})
			response(map[string]bool{"ok": true})
		default:
			response(map[string]bool{"ok": true})
		}
	}
}

func (f *fixture) manager(t *testing.T, installed bool) *Manager {
	t.Setenv("REMOTAI_HERMES_HELPER", "1")
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(f.server.URL, "http://"))
	t.Setenv("REMOTAI_HERMES_FIXTURE_PORT", port)
	t.Setenv("OPENAI_API_KEY", "owner-key-must-not-inherit")
	t.Setenv("HERMES_API_KEY", "owner-key-must-not-inherit")
	t.Setenv("GITHUB_TOKEN", "owner-token-must-not-inherit")
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	m, err := New(Options{Root: t.TempDir(), StartupTimeout: 3 * time.Second, HTTPClient: &http.Client{Transport: testTransport{}, Timeout: 3 * time.Second}, lookPath: func(string) (string, error) { return "", exec.ErrNotFound }, command: func(ctx context.Context, name string, args ...string) *exec.Cmd {
		f.mu.Lock()
		f.commands = append(f.commands, append([]string{name}, args...))
		f.mu.Unlock()
		return exec.CommandContext(ctx, exe, append([]string{"-test.run=^TestHermesHelperProcess$", "--", name}, args...)...)
	}})
	if err != nil {
		t.Fatal(err)
	}
	if installed {
		if err = m.Install(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := m.Close(ctx); err != nil {
			t.Errorf("close: %v", err)
		}
	})
	return m
}

func TestInstallIsolationAndPersistedPolicy(t *testing.T) {
	f := newFixture(t)
	m := f.manager(t, false)
	if !m.Status().AutoUpdate || m.Status().Installed {
		t.Fatal(m.Status())
	}
	if err := m.Install(context.Background()); err != nil {
		t.Fatal(err)
	}
	s := m.Status()
	if s.Ownership != "managed" || !s.Installed || s.Version != "fixture-1" {
		t.Fatal(s)
	}
	f.mu.Lock()
	commands := append([][]string(nil), f.commands...)
	f.mu.Unlock()
	stages := []string{}
	for _, c := range commands {
		for i, a := range c {
			if a == "-Stage" || a == "--stage" {
				stages = append(stages, c[i+1])
			}
		}
	}
	if strings.Join(stages, ",") != "prerequisites,repository,venv,python-deps,repository,venv,python-deps,config,complete" {
		t.Fatal(stages)
	}
	if err := m.SetAutoUpdate(false); err != nil {
		t.Fatal(err)
	}
	other, err := New(Options{Root: m.root, lookPath: func(string) (string, error) { return "", exec.ErrNotFound }})
	if err != nil {
		t.Fatal(err)
	}
	if other.Status().AutoUpdate {
		t.Fatal("auto update preference lost")
	}
	if _, err := os.Stat(filepath.Join(m.home, ".env")); !os.IsNotExist(err) {
		t.Fatal("fixture unexpectedly adopted credentials")
	}
}

func TestPersistentRPCAndSnapshotCursor(t *testing.T) {
	f := newFixture(t)
	m := f.manager(t, true)
	start, cancel := context.WithCancel(context.Background())
	initialGeneration := m.Status().BackendGeneration
	if err := m.Start(start); err != nil {
		t.Fatal(err)
	}
	cancel()
	if !m.Status().Ready || m.Status().BackendGeneration != initialGeneration+1 {
		t.Fatal(m.Status())
	}
	ctx, end := context.WithTimeout(context.Background(), 5*time.Second)
	defer end()
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			raw, err := m.RPC(ctx, "fixture.echo", map[string]int{"value": i, "delay": 12 - i})
			if err != nil {
				t.Error(err)
				return
			}
			var got struct{ Value int }
			_ = json.Unmarshal(raw, &got)
			if got.Value != i {
				t.Errorf("mismatched concurrent response: %s", raw)
			}
		}(i)
	}
	wg.Wait()
	raw, err := m.RPC(ctx, "session.activate", map[string]string{"session_id": "fixture-session"})
	if err != nil {
		t.Fatal(err)
	}
	var snapshot map[string]json.RawMessage
	_ = json.Unmarshal(raw, &snapshot)
	var cursor uint64
	if json.Unmarshal(snapshot["_remotai_event_seq"], &cursor) != nil || cursor < 2 {
		t.Fatal(string(raw))
	}
	deadline := time.Now().Add(time.Second)
	for m.Events(cursor).LatestSeq == cursor && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	b := m.Events(cursor)
	if b.Reset || len(b.Events) != 1 || !strings.Contains(string(b.Events[0].Frame), "after-snapshot") {
		t.Fatalf("cursor=%d batch=%+v", cursor, b)
	}
	_, err = m.RPC(ctx, "fixture.error", nil)
	var rpcErr *RPCError
	if !errors.As(err, &rpcErr) || rpcErr.Code != -32012 || !strings.Contains(string(rpcErr.Data), "approval") {
		t.Fatal(err)
	}
	_, err = m.RPC(ctx, "fixture.approval", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = m.Reply(ctx, "approval-fixture", json.RawMessage(`{"choice":"once"}`)); err != nil {
		t.Fatal(err)
	}
	select {
	case reply := <-f.replies:
		if rpcID(reply.ID) != "approval-fixture" {
			t.Fatal(reply)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	_, err = m.RPC(ctx, "fixture.renderer", nil)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case reply := <-f.replies:
		var result map[string]string
		_ = json.Unmarshal(reply.Result, &result)
		if rpcID(reply.ID) != "renderer-fixture" || !json.Valid([]byte(result["value"])) || !strings.Contains(result["value"], "no Hermes Desktop renderer") {
			t.Fatal(reply)
		}
	case <-ctx.Done():
		t.Fatal("renderer-only request was left hanging")
	}
	for _, event := range m.Events(cursor).Events {
		if strings.Contains(string(event.Frame), "renderer-fixture") {
			t.Fatal("renderer request leaked into human prompt flow")
		}
	}
	slow, cancelSlow := context.WithTimeout(ctx, 15*time.Millisecond)
	_, err = m.RPC(slow, "fixture.slow", nil)
	cancelSlow()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	if _, err = m.RPC(ctx, "fixture.echo", map[string]int{"value": 99}); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	connections, capabilities := f.connections, f.capabilities
	f.mu.Unlock()
	if connections != 1 || !capabilities {
		t.Fatalf("connections=%d capabilities=%t", connections, capabilities)
	}
	for _, path := range []string{"https://example.invalid/api/a", "//example.invalid/api/a", "/api/../secret", "/health"} {
		if _, err = m.Do(ctx, "GET", path, nil); err == nil {
			t.Fatal("accepted", path)
		}
	}
	response, err := m.Do(ctx, "GET", "/api/redirect", nil)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 302 {
		t.Fatal("redirect followed")
	}
}

func TestGatewayReconnectKeepsOwnedProcess(t *testing.T) {
	f := newFixture(t)
	m := f.manager(t, true)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	if err := m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := m.RPC(ctx, "fixture.echo", nil); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	process := m.process
	m.mu.Unlock()
	connectedGeneration := m.Status().BackendGeneration
	if _, err := m.RPC(ctx, "fixture.disconnect", nil); err == nil {
		t.Fatal("disconnected request succeeded")
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		f.mu.Lock()
		connections := f.connections
		f.mu.Unlock()
		if connections >= 2 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	f.mu.Lock()
	connections := f.connections
	f.mu.Unlock()
	if connections != 2 {
		t.Fatal("gateway did not reconnect without a new client RPC", connections)
	}
	m.mu.Lock()
	same := m.process == process
	m.mu.Unlock()
	if !same || !m.Status().Ready || m.Status().BackendGeneration != connectedGeneration+1 {
		t.Fatal(m.Status())
	}
	if !m.Events(0).Reset {
		t.Fatal("stale session cursor survived gateway restart")
	}
	if _, err := m.RPC(ctx, "session.activate", map[string]string{"session_id": "fixture-session"}); err != nil {
		t.Fatal(err)
	}
}

func TestRetirementFailsClosedAndUpdateRestarts(t *testing.T) {
	for _, mode := range []string{"busy", "unknown", "missing", "commit-fails", "idle"} {
		t.Run(mode, func(t *testing.T) {
			f := newFixture(t)
			f.retirement = mode
			m := f.manager(t, true)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := m.Start(ctx); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(m.home, "state.db"), []byte("durable-state"), 0600); err != nil {
				t.Fatal(err)
			}
			connectedGeneration := m.Status().BackendGeneration
			err := m.Update(ctx)
			if mode != "idle" {
				if !errors.Is(err, ErrDeferred) || !m.Status().Ready || m.Status().BackendGeneration != connectedGeneration || !m.Status().UpdatePending {
					t.Fatalf("err=%v status=%+v", err, m.Status())
				}
				if matches, _ := filepath.Glob(filepath.Join(m.root, "backups", "*")); len(matches) > 0 {
					t.Fatal("busy backend was snapshotted")
				}
			} else {
				if err != nil || !m.Status().Ready || m.Status().BackendGeneration != connectedGeneration+1 {
					t.Fatalf("err=%v status=%+v", err, m.Status())
				}
				matches, _ := filepath.Glob(filepath.Join(m.root, "backups", "*", "home", "state.db"))
				if len(matches) != 1 {
					t.Fatal(matches)
				}
				data, _ := os.ReadFile(matches[0])
				if string(data) != "durable-state" {
					t.Fatal(string(data))
				}
				if !m.Events(0).Reset {
					t.Fatal("restart must invalidate event cursor")
				}
			}
			f.mu.Lock()
			actions := strings.Join(f.actions, ",")
			commands := append([][]string(nil), f.commands...)
			f.mu.Unlock()
			if mode == "idle" && actions != "prepare,commit" {
				t.Fatal(actions)
			}
			if mode == "commit-fails" && actions != "prepare,commit,cancel" {
				t.Fatal(actions)
			}
			updated := false
			for _, c := range commands {
				joined := strings.Join(c, " ")
				if strings.Contains(joined, "--no-gateway-restart") {
					updated = true
					if !strings.Contains(joined, "--channel main --yes --backup") {
						t.Fatal(joined)
					}
				}
			}
			if updated != (mode == "idle") {
				t.Fatal("unsafe update executed", mode)
			}
		})
	}
}

func TestCloseCancelsStartingAndInstalling(t *testing.T) {
	for _, operation := range []string{"start", "install"} {
		t.Run(operation, func(t *testing.T) {
			f := newFixture(t)
			m := f.manager(t, operation == "start")
			marker := "slow-install"
			if operation == "start" {
				marker = "slow-start"
			}
			_ = os.WriteFile(filepath.Join(m.root, marker), []byte("1"), 0600)
			done := make(chan error, 1)
			go func() {
				if operation == "start" {
					done <- m.Start(context.Background())
				} else {
					done <- m.Install(context.Background())
				}
			}()
			deadline := time.Now().Add(3 * time.Second)
			for m.Status().Operation == "" && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := m.Close(ctx); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("cancelled operation succeeded")
				}
			case <-ctx.Done():
				t.Fatal("operation leaked")
			}
			if m.Status().Running || m.Status().Ready {
				t.Fatal(m.Status())
			}
			if err := m.Start(ctx); !errors.Is(err, ErrClosed) {
				t.Fatal("closed manager starts again", err)
			}
		})
	}
}

func TestExternalInstallationCannotBeUpdated(t *testing.T) {
	root := t.TempDir()
	external := filepath.Join(t.TempDir(), "hermes")
	_ = os.WriteFile(external, []byte("external"), 0600)
	spawns := 0
	m, err := New(Options{Root: root, command: func(ctx context.Context, name string, args ...string) *exec.Cmd {
		spawns++
		return exec.CommandContext(ctx, name, args...)
	}, lookPath: func(name string) (string, error) {
		if name == "hermes" {
			return external, nil
		}
		return "", exec.ErrNotFound
	}})
	if err != nil {
		t.Fatal(err)
	}
	if m.Status().Ownership != "external" {
		t.Fatal(m.Status())
	}
	if err = m.Start(context.Background()); !errors.Is(err, ErrExternal) {
		t.Fatal("unowned binary started", err)
	}
	if spawns != 0 {
		t.Fatal("external discovery or startup executed a command", spawns)
	}
	if _, err = os.Stat(m.home); !os.IsNotExist(err) {
		t.Fatal("external startup wrote a profile", err)
	}
	if err = m.Update(context.Background()); !errors.Is(err, ErrExternal) {
		t.Fatal(err)
	}
	if err = m.CheckUpdate(context.Background()); !errors.Is(err, ErrExternal) {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(external)
	if string(data) != "external" {
		t.Fatal("external installation mutated")
	}
}

func TestUpdateFailureCooldownAndBackupRetention(t *testing.T) {
	f := newFixture(t)
	m := f.manager(t, true)
	_ = os.WriteFile(filepath.Join(m.root, "update-fails"), []byte("1"), 0600)
	if err := m.Update(context.Background()); err == nil {
		t.Fatal("failed updater succeeded")
	}
	s := m.Status()
	if s.NextUpdateAttemptAt == "" || !s.UpdatePending || s.LastError == "" {
		t.Fatal(s)
	}
	if m.maintenanceUpdateAllowed(time.Now()) || !m.maintenanceUpdateAllowed(time.Now().Add(2*time.Hour)) {
		t.Fatal("automatic update cooldown not enforced")
	}
	other, err := New(Options{Root: m.root, lookPath: func(string) (string, error) { return "", exec.ErrNotFound }})
	if err != nil {
		t.Fatal(err)
	}
	if other.maintenanceUpdateAllowed(time.Now()) {
		t.Fatal("restart lost update failure cooldown")
	}
	if err = os.Remove(filepath.Join(m.root, "update-fails")); err != nil {
		t.Fatal(err)
	}
	if err = m.Update(context.Background()); err != nil {
		t.Fatal("manual recovery blocked by auto cooldown", err)
	}
	if m.Status().NextUpdateAttemptAt != "" {
		t.Fatal("successful update retained failure cooldown")
	}
	for i := 0; i < 7; i++ {
		if _, err = m.snapshot(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := os.ReadDir(filepath.Join(m.root, "backups"))
	if err != nil || len(entries) != 5 {
		t.Fatalf("backups=%d err=%v", len(entries), err)
	}
	if err = removeOwnedBackup(filepath.Join(m.root, "backups"), m.root); err == nil {
		t.Fatal("backup retention accepted deletion outside backup directory")
	}
}

func TestReplayBufferAndProtocolFailure(t *testing.T) {
	m, err := New(Options{Root: t.TempDir(), lookPath: func(string) (string, error) { return "", exec.ErrNotFound }})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 600; i++ {
		m.addEvent([]byte(fmt.Sprintf(`{"i":%d}`, i)))
	}
	b := m.Events(1)
	if !b.Reset || len(b.Events) != 512 || b.LatestSeq != 600 {
		t.Fatalf("%+v", b)
	}
	b.Events[0].Frame[0] = 'x'
	if !json.Valid(m.Events(1).Events[0].Frame) {
		t.Fatal("caller mutated replay")
	}
	if !m.Events(601).Reset {
		t.Fatal("future cursor accepted")
	}
	for _, output := range []string{"Up to date", `REMOTAI_HERMES_UPDATE {"available":true,"commit":"main"}`, `REMOTAI_HERMES_UPDATE {"commit":"` + fixtureCommit + `"}`} {
		if _, _, err := updateVerdict(output); err == nil {
			t.Fatal("unknown update protocol accepted", output)
		}
	}
	for _, line := range []string{"HERMES_BACKEND_READY port=0", "HERMES_BACKEND_READY port=65536", "BACKEND_PORT_IN_USE port=9119"} {
		if _, matched, err := readinessPort(line); !matched || err == nil {
			t.Fatal(line)
		}
	}
	if port, matched, err := readinessPort("HERMES_BACKEND_READY port=" + strconv.Itoa(12345)); err != nil || !matched || port != 12345 {
		t.Fatal(port, matched, err)
	}
}
