package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"tgcontrol/internal/agentcheck"
)

const agentCheckTestKey = "sk-ant-api03-WEBSECRETWEBSECRET-q7q7"

// Стенд: основной аккаунт Claude Code с ключом и адресом в settings.json —
// ровно так люди подключают Claude к OpenRouter или своему шлюзу.
func agentCheckStand(t *testing.T, handler http.HandlerFunc) *Server {
	t.Helper()
	isolateHome(t)
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	home, _ := os.UserHomeDir()
	settings := `{"env":{"ANTHROPIC_BASE_URL":"` + srv.URL + `","ANTHROPIC_API_KEY":"` + agentCheckTestKey + `","ANTHROPIC_MODEL":"claude-test"}}`
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".claude", "settings.json"), []byte(settings), 0o600); err != nil {
		t.Fatal(err)
	}
	oldProber := agentCheckProber
	agentCheckProber = nil
	t.Cleanup(func() { agentCheckProber = oldProber })
	return newOpenRouterServer(t)
}

func agentCall(t *testing.T, h func(http.ResponseWriter, *http.Request, int64), method, target, body string) (int, string) {
	t.Helper()
	return agentCallID(t, h, method, target, body, "claude")
}

func agentCallID(t *testing.T, h func(http.ResponseWriter, *http.Request, int64), method, target, body, id string) (int, string) {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, target, nil)
	} else {
		r = httptest.NewRequest(method, target, strings.NewReader(body))
	}
	r.SetPathValue("id", id)
	w := httptest.NewRecorder()
	h(w, r, 1)
	return w.Code, w.Body.String()
}

func TestAgentCheckNeverReturnsKey(t *testing.T) {
	var seenKey string
	s := agentCheckStand(t, func(w http.ResponseWriter, r *http.Request) {
		seenKey = r.Header.Get("x-api-key")
		_, _ = w.Write([]byte(`{"model":"claude-test","content":[{"type":"text","text":"ок"}]}`))
	})

	code, body := agentCall(t, s.apiAgentConnection, "GET", "/api/agents/claude/connection", "")
	if code != 200 {
		t.Fatalf("connection %d %s", code, body)
	}
	if strings.Contains(body, "WEBSECRET") {
		t.Fatalf("ключ в ответе connection: %s", body)
	}
	var conn map[string]any
	_ = json.Unmarshal([]byte(body), &conn)
	auth, _ := conn["auth"].(map[string]any)
	if auth["value"] != "sk-…q7q7" || conn["check"] != "http" || conn["route"] != "custom" {
		t.Fatalf("connection = %s", body)
	}

	code, body = agentCall(t, s.apiAgentCheck, "POST", "/api/agents/claude/check", `{"account":""}`)
	if code != 200 || strings.Contains(body, "WEBSECRET") {
		t.Fatalf("check %d: %s", code, body)
	}
	var out struct {
		State  string            `json:"state"`
		Result agentcheck.Result `json:"result"`
	}
	_ = json.Unmarshal([]byte(body), &out)
	if out.State != "done" || !out.Result.OK || out.Result.Reply != "ок" || out.Result.Model != "claude-test" {
		t.Fatalf("check = %s", body)
	}
	if seenKey != agentCheckTestKey {
		t.Fatalf("проверка должна идти тем же ключом, что и агент")
	}
}

// Отказ ключа не превращается в «адрес отвечает», а эхо ключа в тексте
// ошибки провайдера до клиента не доходит.
func TestAgentCheckRejectedKeyIsNotGreen(t *testing.T) {
	s := agentCheckStand(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key ` + r.Header.Get("x-api-key") + `"}}`))
	})
	_, body := agentCall(t, s.apiAgentCheck, "POST", "/api/agents/claude/check", `{}`)
	if strings.Contains(body, "WEBSECRET") {
		t.Fatalf("ключ в ответе check: %s", body)
	}
	var out struct {
		Result agentcheck.Result `json:"result"`
	}
	_ = json.Unmarshal([]byte(body), &out)
	if out.Result.OK || out.Result.Reason != agentcheck.ReasonKeyRejected || out.Result.HTTPStatus != 401 {
		t.Fatalf("check = %s", body)
	}
}

// Долгая проверка: первый POST возвращает номер, второй с номером ждёт ТУ ЖЕ
// проверку — модель спрашивают один раз.
func TestAgentCheckRunningThenJoin(t *testing.T) {
	release := make(chan struct{})
	var calls atomic.Int32
	s := agentCheckStand(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		<-release
		_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"ок"}]}`))
	})
	old := agentCheckWait
	agentCheckWait = 50 * time.Millisecond
	defer func() { agentCheckWait = old }()

	_, body := agentCall(t, s.apiAgentCheck, "POST", "/api/agents/claude/check", `{"model":"claude-slow"}`)
	var first struct{ State, ID string }
	_ = json.Unmarshal([]byte(body), &first)
	if first.State != "running" || first.ID == "" {
		t.Fatalf("первый ответ: %s", body)
	}
	close(release)
	agentCheckWait = 5 * time.Second
	_, body = agentCall(t, s.apiAgentCheck, "POST", "/api/agents/claude/check", `{"id":"`+first.ID+`"}`)
	var second struct {
		State  string
		Result agentcheck.Result
	}
	_ = json.Unmarshal([]byte(body), &second)
	if second.State != "done" || !second.Result.OK || calls.Load() != 1 {
		t.Fatalf("второй ответ: %s (запросов к модели: %d)", body, calls.Load())
	}

	code, _ := agentCall(t, s.apiAgentCheck, "POST", "/api/agents/claude/check", `{"id":"chk-nope"}`)
	if code != 404 {
		t.Fatalf("неизвестный номер: %d", code)
	}
	code, _ = agentCall(t, s.apiAgentCheck, "POST", "/api/agents/claude/check", `{"model":"x; rm -rf /"}`)
	if code != 400 {
		t.Fatalf("мусор вместо модели: %d", code)
	}
}
