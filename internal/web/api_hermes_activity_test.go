package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"tgcontrol/internal/config"
	"tgcontrol/internal/hermes"
)

// A real registered HTTP/auth path; only the native RPC boundary is synthetic.
// The subprocess isolates configuration singletons from the installed host.
func TestHermesActivityRegistered(t *testing.T) {
	const env = "REMOTAI_ACTIVITY_FIXTURE_CHILD"
	if os.Getenv(env) != "1" {
		home := t.TempDir()
		dir := filepath.Join(home, ".config", "remotai")
		if runtime.GOOS == "windows" {
			dir = filepath.Join(home, "AppData", "Local", "Remotai")
		}
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"mode":"standalone","setup_complete":true,"api_token":"activity-fixture-token","api_token_uid":2}`), 0600); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestHermesActivityRegistered$", "-test.v", "-test.count=1")
		cmd.Env = append(os.Environ(), "USERPROFILE="+home, "HOME="+home, env+"=1")
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("registered activity regression: %v\n%s", err, output)
		}
		t.Log(string(output))
		return
	}
	calls := 0
	f := &hermesWebFixture{status: hermes.Status{BackendGeneration: 7}, rpc: func(_ context.Context, method string, params any) (json.RawMessage, error) {
		calls++
		raw, _ := json.Marshal(params)
		var request map[string]string
		if method != "subagent.tail" || json.Unmarshal(raw, &request) != nil || len(request) != 2 || request["session_id"] != "live-A" || request["subagent_id"] != "child-A" {
			t.Fatalf("incorrect internal native scope: %s %s", method, raw)
		}
		return json.Marshal(map[string]any{"subagent_id": "child-A", "available": true, "truncated": false, "text": "12:34:56 think    | REASONING_SECRET\n12:34:57 tool     | -> terminal(ARG_SECRET)\n12:34:58 result   | terminal ok 0.1s: OUTPUT_SECRET\n"})
	}}
	other := &hermesWebFixture{rpc: func(context.Context, string, any) (json.RawMessage, error) {
		t.Fatal("cross-user native lookup")
		return nil, nil
	}}
	s := &Server{mux: http.NewServeMux(), hermesManagers: map[int64]hermesRuntime{1: other, 2: f}, allowed: map[int64]bool{2: true}}
	s.registerHermesRoutes()
	invoke := func(method, path, token, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		if token != "" {
			r.Header.Set("X-API-Token", token)
		}
		w := httptest.NewRecorder()
		s.mux.ServeHTTP(w, r)
		return w
	}
	const path = "/api/hermes/subagents/activity?session_id=live-A&subagent_id=child-A"
	w := invoke("GET", path, "activity-fixture-token", "")
	if w.Code != 200 {
		t.Fatalf("registered activity route: %d %s", w.Code, w.Body.String())
	}
	var result struct {
		Supported, Available, Truncated, HistoryIncomplete bool
		Source                                             string `json:"source"`
		SourceTimeZone                                     string `json:"source_time_zone"`
		CapturedAtMS                                       int64  `json:"captured_at_ms"`
		Entries                                            []struct {
			ID, Kind string
			ToolName string `json:"tool_name"`
			Time     string `json:"source_time_text"`
			Status   string
			Duration *float64 `json:"duration_seconds"`
		}
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if !result.Supported || !result.Available || result.Truncated || result.Source != "native_live_log" || result.SourceTimeZone != "unknown" || result.CapturedAtMS < time.Now().Add(-time.Minute).UnixMilli() || len(result.Entries) != 2 {
		t.Fatalf("invalid safe snapshot: %s", w.Body.String())
	}
	if result.Entries[0].Kind != "tool_start" || result.Entries[0].ToolName != "terminal" || result.Entries[0].Time != "12:34:57" || result.Entries[0].ID == "" || result.Entries[1].Kind != "tool_result" || result.Entries[1].Status != "ok" || result.Entries[1].Duration == nil || *result.Entries[1].Duration != 0.1 {
		t.Fatalf("native logged callbacks lost: %s", w.Body.String())
	}
	for _, secret := range []string{"REASONING_SECRET", "ARG_SECRET", "OUTPUT_SECRET", `"text":`, "profile", "live-A", "child-A"} {
		if strings.Contains(w.Body.String(), secret) {
			t.Fatalf("private native data exposed: %s", secret)
		}
	}
	if w.Header().Get("Cache-Control") != "no-store" || calls != 1 {
		t.Fatalf("unsafe caching or RPC fanout: %s calls=%d", w.Header(), calls)
	}
	t.Run("authenticated UID namespace", func(t *testing.T) {
		cfg := config.GetNoSetup()
		originalUID := cfg.APITokenUID
		original := f.rpc
		defer func() {
			cfg.APITokenUID = originalUID
			f.rpc = original
			delete(s.hermesManagers, 3)
			s.allowed = map[int64]bool{2: true}
		}()
		f.rpc = func(context.Context, string, any) (json.RawMessage, error) {
			return json.RawMessage(`{"subagent_id":"child-A","available":true,"truncated":false,"text":"12:34:57 tool     | -> terminal(PRIVATE_ARGS)\n"}`), nil
		}
		get := func() hermesActivitySnapshot {
			t.Helper()
			got := invoke("GET", path, "activity-fixture-token", "")
			var snap hermesActivitySnapshot
			if got.Code != 200 || json.Unmarshal(got.Body.Bytes(), &snap) != nil {
				t.Fatalf("UID projection failed: %d %s", got.Code, got.Body.String())
			}
			return snap
		}
		first := get()
		s.hermesManagers[3] = f
		s.allowed = map[int64]bool{3: true}
		cfg.APITokenUID = 3
		second := get()
		if first.WindowID == second.WindowID || first.Entries[0].ID == second.Entries[0].ID {
			t.Fatal("authenticated UID crossed opaque row namespace")
		}
	})
	t.Run("registered relay transport", activityRelayFixture)
	t.Run("encoded path alias denied", func(t *testing.T) {
		before := calls
		got := invoke("GET", "/api/hermes/subagents/%61ctivity?session_id=live-A&subagent_id=child-A", "activity-fixture-token", "")
		if got.Code != 400 || calls != before {
			t.Fatalf("encoded path alias admitted: %d %s", got.Code, got.Body.String())
		}
	})
	t.Run("opaque snapshot identity and gaps", func(t *testing.T) {
		original := f.rpc
		defer func() { f.rpc = original; f.status.BackendGeneration = 7 }()
		text := "header\n12:34:57 tool     | -> terminal(ARG_SECRET)\n12:34:58 result   | terminal ok 0.1s: OUTPUT_SECRET\n"
		trunc := false
		f.rpc = func(_ context.Context, _ string, params any) (json.RawMessage, error) {
			raw, _ := json.Marshal(params)
			var request map[string]string
			_ = json.Unmarshal(raw, &request)
			return json.Marshal(map[string]any{"subagent_id": request["subagent_id"], "available": true, "truncated": trunc, "text": text})
		}
		read := func(p string) hermesActivitySnapshot {
			t.Helper()
			got := invoke("GET", p, "activity-fixture-token", "")
			var snap hermesActivitySnapshot
			if got.Code != 200 || json.Unmarshal(got.Body.Bytes(), &snap) != nil || snap.WindowID == "" || !snap.HistoryIncomplete {
				t.Fatalf("invalid snapshot identity: %d %s", got.Code, got.Body.String())
			}
			return snap
		}
		base := read(path)
		same := read(path)
		if base.WindowID != same.WindowID || base.Entries[0].ID != same.Entries[0].ID {
			t.Fatal("unchanged snapshot moved identity")
		}
		text = strings.ReplaceAll(strings.ReplaceAll(text, "ARG_SECRET", "CHANGED_PRIVATE_ARGS"), "OUTPUT_SECRET", "CHANGED_PRIVATE_OUTPUT") + "12:34:59 think    | REASONING_PRIVATE_SECRET\n"
		if got := read(path); got.WindowID != base.WindowID || got.Entries[0].ID != base.Entries[0].ID {
			t.Fatal("raw private suffix used in snapshot identity")
		}
		trunc = true // first non-event header dropped: same safe rows but a different native-window boundary
		if got := read(path); got.WindowID == base.WindowID {
			t.Fatal("changed truncation boundary reused native window identity")
		}
		trunc = false
		f.status.BackendGeneration = 8
		if got := read(path); got.WindowID == base.WindowID {
			t.Fatal("manager generation crossed identity")
		}
		f.status.BackendGeneration = 7
		for _, p := range []string{strings.Replace(path, "live-A", "live-B", 1), strings.Replace(path, "child-A", "child-B", 1)} {
			if got := read(p); got.WindowID == base.WindowID {
				t.Fatal("literal owner scope crossed identity")
			}
		}
		s.hermesManagers[2] = &hermesWebFixture{rpc: f.rpc, status: hermes.Status{BackendGeneration: 7}}
		if got := read(path); got.WindowID == base.WindowID {
			t.Fatal("manager instance crossed identity")
		}
		s.hermesManagers[2] = f
		text += "12:35:00 tool     | -> terminal(ARG_SECRET)\n"
		if got := read(path); got.WindowID == base.WindowID || got.Entries[0].ID == base.Entries[0].ID {
			t.Fatal("append falsely reused cross-window row identity")
		}
		text = "12:34:58 result   | terminal ok 0.1s: OUTPUT_SECRET\n12:35:00 tool     | -> terminal(ARG_SECRET)\n"
		if got := read(path); got.WindowID == base.WindowID {
			t.Fatal("shifted overlap falsely claimed stable native history")
		}
	})
	t.Run("bounded complete safe native window", func(t *testing.T) {
		original := f.rpc
		defer func() { f.rpc = original }()
		cases := []struct {
			name, text string
			truncated  bool
			names      []string
			statuses   []string
			durations  []*float64
		}{
			{name: "native registry names and no-duration errors", text: "00:00:00 tool     | -> read_file(ARG_SECRET)\n23:59:59 result   | read_file ERROR: OUTPUT_SECRET\n", names: []string{"read_file", "read_file"}, statuses: []string{"", "error"}},
			{name: "truncated first and incomplete last discarded", text: "12:34:57 tool     | -> terminal(ARG_SECRET)\n12:34:58 tool     | -> terminal(ARG_SECRET)\n12:34:59 result   | terminal ok 0.1s: OUTPUT_SECRET", truncated: true, names: []string{"terminal"}, statuses: []string{""}},
			{name: "malformed and unknown rows cannot publish private name", text: "24:00:00 tool     | -> terminal(ARG_SECRET)\n23:60:00 tool     | -> terminal(ARG_SECRET)\n23:59:60 tool     | -> terminal(ARG_SECRET)\n12:34:57 think    | REASONING_SECRET\n12:34:57 tool     | -> OUTPUT_SECRET(ARG_SECRET)\n12:34:57 tool     | -> terminal(ARG_SECRET\n12:34:58 result   | terminal ok 86400.1s: OUTPUT_SECRET\n12:34:58 result   | terminal ok NaNs: OUTPUT_SECRET\n12:34:58 result   | terminal ok -0.1s: OUTPUT_SECRET\n12:34:58 result   | terminal ok 1e9s: OUTPUT_SECRET\n12:34:58 result   | terminal ok [0.1s]: OUTPUT_SECRET\n12:34:58 result   | terminal ok 0.12s: OUTPUT_SECRET\nPREFIX 12:34:57 tool     | -> terminal(ARG_SECRET)\n12:34:57 tool     | -> terminal(ARGS\rSECRET)\n12:34:57 tool     | -> terminal(ARGS\x00SECRET)\n", names: []string{}},
			{name: "giant line ignored", text: "12:34:57 tool     | -> terminal(" + strings.Repeat("x", 5000) + ")\n", names: []string{}},
			{name: "newest two hundred", text: strings.Repeat("12:34:57 tool     | -> terminal(ARG_SECRET)\n", 201), names: make([]string, 200)},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				f.rpc = func(context.Context, string, any) (json.RawMessage, error) {
					return json.Marshal(map[string]any{"subagent_id": "child-A", "available": true, "truncated": tc.truncated, "text": tc.text})
				}
				got := invoke("GET", path, "activity-fixture-token", "")
				var snapshot hermesActivitySnapshot
				if got.Code != 200 || json.Unmarshal(got.Body.Bytes(), &snapshot) != nil || len(snapshot.Entries) != len(tc.names) || !snapshot.HistoryIncomplete || snapshot.Truncated != tc.truncated {
					t.Fatalf("unsafe or incomplete projection: %d %s", got.Code, got.Body.String())
				}
				ids := map[string]bool{}
				for i, entry := range snapshot.Entries {
					if tc.names[i] != "" && entry.ToolName != tc.names[i] || entry.ID == "" || ids[entry.ID] {
						t.Fatalf("wrong tool or duplicate identity: %+v", entry)
					}
					ids[entry.ID] = true
					if len(tc.statuses) > 0 && entry.Status != tc.statuses[i] {
						t.Fatalf("incorrect native status: %+v", entry)
					}
				}
				for _, secret := range []string{"ARG_SECRET", "OUTPUT_SECRET", "REASONING_SECRET", "PRIVATE_SECRET"} {
					if strings.Contains(got.Body.String(), secret) {
						t.Fatalf("private suffix leak: %s", got.Body.String())
					}
				}
			})
		}
	})
	t.Run("native errors and unavailable are not empty history", func(t *testing.T) {
		original := f.rpc
		defer func() { f.rpc = original }()
		for _, tc := range []struct {
			name     string
			code     int
			message  string
			status   int
			contains string
		}{
			{"unsupported", -32601, "Method not found", 200, `"supported":false`},
			{"foreign session", 4001, "session not found or not owned by this transport", 502, `"rpc_code":"4001"`},
			{"private upstream error", 4999, "ERROR_PRIVATE_SECRET", 502, `"rpc_code":"4999"`},
		} {
			t.Run(tc.name, func(t *testing.T) {
				f.rpc = func(context.Context, string, any) (json.RawMessage, error) {
					return nil, &hermes.RPCError{Code: tc.code, Message: tc.message}
				}
				got := invoke("GET", path, "activity-fixture-token", "")
				if got.Code != tc.status || !strings.Contains(got.Body.String(), tc.contains) || strings.Contains(got.Body.String(), "ERROR_PRIVATE_SECRET") {
					t.Fatalf("native error misrepresented or leaked: %d %s", got.Code, got.Body.String())
				}
				if tc.code == 4001 && !strings.Contains(got.Body.String(), tc.message) {
					t.Fatalf("native exact-owner error changed: %s", got.Body.String())
				}
			})
		}
		f.rpc = func(context.Context, string, any) (json.RawMessage, error) {
			return json.RawMessage(`{"subagent_id":"child-A","available":false,"truncated":false,"text":"UNAVAILABLE_PRIVATE_SECRET"}`), nil
		}
		got := invoke("GET", path, "activity-fixture-token", "")
		if got.Code != 200 || !strings.Contains(got.Body.String(), `"supported":true`) || !strings.Contains(got.Body.String(), `"available":false`) || !strings.Contains(got.Body.String(), `"entries":[]`) || strings.Contains(got.Body.String(), "UNAVAILABLE_PRIVATE_SECRET") {
			t.Fatalf("unavailable became empty known journal: %d %s", got.Code, got.Body.String())
		}
	})
	t.Run("malformed native responses fail closed", func(t *testing.T) {
		original := f.rpc
		defer func() { f.rpc = original }()
		for _, raw := range []string{`null`, `{}`, `{"subagent_id":"foreign-child","available":true,"truncated":false,"text":"12:34:57 tool     | -> terminal(ARG_SECRET)\n"}`, `{"subagent_id":"child-A","available":true,"available":false,"truncated":false,"text":"PRIVATE_SECRET"}`, `{"subagent_id":"child-A","available":"yes","truncated":false,"text":"PRIVATE_SECRET"}`, `{"subagent_id":"child-A","available":true,"truncated":false,"text":"` + strings.Repeat("x", 16385) + `"}`, `{"subagent_id":"child-A","available":true,"truncated":false,"text":"` + string([]byte{0xff}) + `"}`, strings.Repeat(" ", 131073), `{"subagent_id":"child-A","available":true,"truncated":false,"text":"PRIVATE_SECRET"} {}`} {
			f.rpc = func(context.Context, string, any) (json.RawMessage, error) { return json.RawMessage(raw), nil }
			got := invoke("GET", path, "activity-fixture-token", "")
			if got.Code != 502 || strings.Contains(got.Body.String(), "PRIVATE_SECRET") || strings.Contains(got.Body.String(), `"entries"`) {
				t.Fatalf("invalid native response became activity: len=%d %d %s", len(raw), got.Code, got.Body.String())
			}
		}
	})
	t.Run("generation change fails closed", func(t *testing.T) {
		original := f.rpc
		defer func() { f.rpc = original; f.status.BackendGeneration = 7 }()
		f.rpc = func(context.Context, string, any) (json.RawMessage, error) {
			f.status.BackendGeneration++
			return json.RawMessage(`{"subagent_id":"child-A","available":true,"truncated":false,"text":"12:34:57 tool     | -> terminal(PRIVATE_SECRET)\n"}`), nil
		}
		got := invoke("GET", path, "activity-fixture-token", "")
		if got.Code != 502 || !strings.Contains(got.Body.String(), "hermes_activity_generation_changed") || strings.Contains(got.Body.String(), `"entries"`) {
			t.Fatalf("retired manager generation exposed: %d %s", got.Code, got.Body.String())
		}
	})
	t.Run("strict scopes before native RPC", func(t *testing.T) {
		for _, query := range []string{"", "session_id=live-A", "session_id=live-A&subagent_id=", "session_id=+live-A&subagent_id=child-A", "session_id=live-A&subagent_id=child%0A", "session_id=live-A&subagent_id=child%2Fpath", "session_id=live-A&subagent_id=child%25", "session_id=live-A&subagent_id=child-A&profile=default", "session_id=live-A&subagent_id=child-A&uid=1", "session_id=live-A&subagent_id=child-A&hermes_home=secret", "session_id=live-A&subagent_id=child-A&cursor=1", "session_id=live-A&session_id=live-A&subagent_id=child-A", "session_id=live-A&subagent_id=child-A&transport=x&transport=y", "session_id=live-A&subagent_id=child-A&initData=x&initData=y", "session_id=%ZZ&subagent_id=child-A", "session_id=" + strings.Repeat("x", 257) + "&subagent_id=child-A"} {
			before := calls
			got := invoke("GET", "/api/hermes/subagents/activity?"+query, "activity-fixture-token", "")
			if got.Code != 400 || calls != before {
				t.Fatalf("scope admitted before validation: %q %d %s", query, got.Code, got.Body.String())
			}
		}
	})
	t.Run("authenticated metadata normalized", func(t *testing.T) {
		got := invoke("GET", path+"&transport=relay&initData=token:activity-fixture-token", "", "")
		if got.Code != 200 {
			t.Fatalf("authenticated transport metadata rejected: %d %s", got.Code, got.Body.String())
		}
	})
	t.Run("auth methods and raw lane denied", func(t *testing.T) {
		before := calls
		for _, token := range []string{"", "wrong-token"} {
			if got := invoke("GET", path, token, ""); got.Code != 401 {
				t.Fatalf("unauthenticated activity: %d", got.Code)
			}
		}
		s.allowed = map[int64]bool{1: true}
		got := invoke("GET", path, "activity-fixture-token", "")
		s.allowed = map[int64]bool{2: true}
		if got.Code != 403 {
			t.Fatalf("wrong allowed user: %d", got.Code)
		}
		for _, method := range []string{"POST", "PUT", "PATCH", "DELETE", "HEAD"} {
			if got := invoke(method, path, "activity-fixture-token", ""); got.Code != 405 {
				t.Fatalf("unexpected read-only verb: %s %d", method, got.Code)
			}
		}
		got = invoke("POST", "/api/hermes/rpc", "activity-fixture-token", `{"method":"subagent.tail","params":{"session_id":"live-A","subagent_id":"child-A"}}`)
		if got.Code != 400 || calls != before {
			t.Fatalf("raw tail or bad authority reached native: %d calls=%d->%d", got.Code, before, calls)
		}
		for _, sibling := range []string{"activity/extra", "tail", "trace", "activity_all"} {
			if got := invoke("GET", "/api/hermes/subagents/"+sibling, "activity-fixture-token", ""); got.Code != 404 {
				t.Fatalf("sibling became reachable: %s %d", sibling, got.Code)
			}
		}
	})
}
