package web

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"tgcontrol/internal/pty"
	"tgcontrol/internal/relay"
)

func serverAccessInitData() string {
	date := fmt.Sprint(time.Now().Unix())
	user := `{"id":42,"first_name":"Test"}`
	key := hmac.New(sha256.New, []byte("WebAppData"))
	key.Write([]byte("server-access-test"))
	mac := hmac.New(sha256.New, key.Sum(nil))
	mac.Write([]byte("auth_date=" + date + "\nuser=" + user))
	return url.Values{"auth_date": {date}, "user": {user}, "hash": {hex.EncodeToString(mac.Sum(nil))}}.Encode()
}

// Use the real route registration and authentication. No local configuration,
// live terminal manager, SSH credentials, or user's network is touched.
func TestServerOperationsAreGatedOnActualLocalRoutes(t *testing.T) {
	paths := []string{
		"POST /api/ssh/connect", "GET /api/ssh/sftp/list", "GET /api/ssh/sftp/preview",
		"GET /api/ssh/sftp/download", "POST /api/ssh/sftp/download", "POST /api/ssh/sftp/upload",
		"POST /api/ssh/sftp/mkdir", "POST /api/ssh/sftp/delete", "POST /api/ssh/sftp/rename",
		"POST /api/ssh/sftp/push", "POST /api/ssh/sftp/pull", "POST /api/ssh/keys/key/install", "POST /api/ssh/forwards",
	}
	for _, failure := range []bool{false, true} {
		s := &Server{mux: http.NewServeMux(), botToken: "server-access-test"}
		calls := 0
		s.serverAccessCheck = func(_ context.Context, start bool) (relay.ServerAccess, error) {
			calls++
			if !start {
				t.Error("operation did not request trial")
			}
			if failure {
				return relay.ServerAccess{}, errors.New("service down")
			}
			return relay.ServerAccess{Code: "server_subscription_required"}, nil
		}
		s.registerRoutes()
		for _, entry := range paths {
			method, path, _ := strings.Cut(entry, " ")
			req := httptest.NewRequest(method, path, nil)
			req.Header.Set("X-Telegram-Init-Data", serverAccessInitData())
			w := httptest.NewRecorder()
			s.mux.ServeHTTP(w, req)
			want := 402
			if failure {
				want = 503
			}
			if w.Code != want {
				t.Fatalf("%s: got %d, want %d: %s", entry, w.Code, want, w.Body.String())
			}
		}
		if calls != len(paths) {
			t.Fatalf("checks=%d routes=%d", calls, len(paths))
		}
		w := httptest.NewRecorder()
		s.mux.ServeHTTP(w, httptest.NewRequest("POST", "/api/ssh/connect", nil))
		if w.Code != 401 || calls != len(paths) {
			t.Fatal("unauthenticated request reached paid operation")
		}
	}
}

func TestLocalWorkAndServerCleanupNeverRequireSubscription(t *testing.T) {
	s := &Server{serverAccessCheck: func(context.Context, bool) (relay.ServerAccess, error) {
		t.Fatal("local work contacted billing")
		return relay.ServerAccess{}, nil
	}}
	for _, entry := range []string{
		"POST /api/pty", "GET /api/files", "GET /api/system", "POST /api/files/upload",
		"GET /api/ssh/hosts", "POST /api/ssh/hosts", "PATCH /api/ssh/hosts/id", "DELETE /api/ssh/hosts/id",
		"POST /api/ssh/hosts/id/unlock", "GET /api/ssh/keys", "POST /api/ssh/keys/generate",
		"DELETE /api/ssh/known-hosts", "GET /api/ssh/history", "GET /api/ssh/forwards",
		"DELETE /api/ssh/forwards/id", "DELETE /api/ssh/forward-specs/id", "POST /api/ssh/sftp/transfers/cancel",
		"GET /api/ssh/sftp/transfers",
	} {
		method, path, _ := strings.Cut(entry, " ")
		called := false
		s.serverAccessWrap(func(http.ResponseWriter, *http.Request, int64) { called = true })(httptest.NewRecorder(), httptest.NewRequest(method, path, nil), 42)
		if !called {
			t.Errorf("local work or cleanup blocked: %s", entry)
		}
	}
}

func TestServerStatusDoesNotStartTrialAndPaidOperationProceeds(t *testing.T) {
	s := &Server{serverAccessCheck: func(_ context.Context, start bool) (relay.ServerAccess, error) {
		if start {
			t.Fatal("status started trial")
		}
		return relay.ServerAccess{TrialAvailable: true}, nil
	}}
	w := httptest.NewRecorder()
	s.apiServerAccess(w, httptest.NewRequest("GET", "/api/server-access", nil), 42)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"trial_available":true`) {
		t.Fatalf("status %s", w.Body.String())
	}
	s.serverAccessCheck = func(_ context.Context, start bool) (relay.ServerAccess, error) {
		if !start {
			t.Fatal("operation skipped trial activation")
		}
		return relay.ServerAccess{Allowed: true}, nil
	}
	called := false
	s.serverAccessWrap(func(http.ResponseWriter, *http.Request, int64) { called = true })(w, httptest.NewRequest("POST", "/api/ssh/connect", nil), 42)
	if !called {
		t.Fatal("paid access denied")
	}
}

func TestServerSocketClosesAfterEntitlementExpires(t *testing.T) {
	for _, unavailable := range []bool{false, true} {
		s := &Server{serverAccessCheck: func(_ context.Context, start bool) (relay.ServerAccess, error) {
			if start {
				t.Error("watcher restarted trial")
			}
			if unavailable {
				return relay.ServerAccess{}, errors.New("offline")
			}
			return relay.ServerAccess{Allowed: false}, nil
		}}
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			upgrader := websocket.Upgrader{}
			conn, err := upgrader.Upgrade(w, r, nil)
			if err != nil {
				return
			}
			defer conn.Close()
			s.watchServerSocket(r.Context(), conn, time.Millisecond)
		}))
		conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(ts.URL, "http"), nil)
		if err != nil {
			t.Fatal(err)
		}
		conn.SetReadDeadline(time.Now().Add(time.Second))
		_, _, err = conn.ReadMessage()
		conn.Close()
		ts.Close()
		if !websocket.IsCloseError(err, websocket.ClosePolicyViolation) {
			t.Fatalf("expired socket not closed: %v", err)
		}
	}
}

func TestExistingServerTerminalRequiresAccessAfterRename(t *testing.T) {
	ssh := &pty.Session{ID: "renamed-session", CWD: "friendly name", Shell: "ssh"}
	local := &pty.Session{ID: "ssh-named-local", CWD: "ssh", Shell: "powershell"}
	for _, method := range []string{"GET", "POST", "PUT"} {
		if !serverTerminalAccessRequired(method, ssh) {
			t.Errorf("%s bypassed SSH terminal subscription", method)
		}
		if serverTerminalAccessRequired(method, local) || serverTerminalAccessRequired(method, nil) {
			t.Errorf("%s blocked local terminal", method)
		}
	}
	for _, method := range []string{"DELETE", "PATCH"} {
		if serverTerminalAccessRequired(method, ssh) {
			t.Errorf("%s blocked terminal cleanup", method)
		}
	}
}
