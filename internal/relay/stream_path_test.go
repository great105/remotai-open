package relay

import (
	"net/url"
	"strings"
	"testing"
)

// localStreamURL собирает адрес, по которому агент дозванивается до СВОЕГО
// обработчика, из пути, пришедшего снаружи. Здесь сходятся две вещи, каждая из
// которых уже ломалась: пронос resume (без него телефон перекачивает буфер
// заново на каждом переподключении) и авторизация (initData обязан быть наш).
func TestLocalStreamURL(t *testing.T) {
	const token = "s3cret-token"

	t.Run("простой путь получает наш initData", func(t *testing.T) {
		got := localStreamURL(8080, "/ws/pty/abc123", token)
		u, err := url.Parse(got)
		if err != nil {
			t.Fatalf("получился неразбираемый адрес %q: %v", got, err)
		}
		if u.Path != "/ws/pty/abc123" {
			t.Errorf("путь потерян: %q", u.Path)
		}
		if u.Query().Get("initData") != "token:"+token {
			t.Errorf("initData не наш: %q", u.Query().Get("initData"))
		}
	})

	t.Run("resume доезжает до обработчика", func(t *testing.T) {
		got := localStreamURL(8080, "/ws/pty/abc123?resume=6f1a2b3c:918273", token)
		u, err := url.Parse(got)
		if err != nil {
			t.Fatalf("неразбираемый адрес %q: %v", got, err)
		}
		if u.Path != "/ws/pty/abc123" {
			t.Errorf("путь потерян: %q", u.Path)
		}
		if got, want := u.Query().Get("resume"), "6f1a2b3c:918273"; got != want {
			t.Errorf("resume = %q, ожидалось %q", got, want)
		}
		if u.Query().Get("initData") != "token:"+token {
			t.Errorf("initData потерялся при наличии resume: %q", u.Query().Get("initData"))
		}
		// Ровно тот дефект, из-за которого resume не работал через облако:
		// склейка давала два знака вопроса, и initData уезжал внутрь resume.
		if strings.Count(got, "?") != 1 {
			t.Errorf("в адресе должен быть один знак вопроса: %q", got)
		}
	})

	t.Run("посторонние параметры вырезаются", func(t *testing.T) {
		got := localStreamURL(8080, "/ws/pty/abc?initData=token:foreign&jwt=stolen&resume=a:1", token)
		u, err := url.Parse(got)
		if err != nil {
			t.Fatalf("неразбираемый адрес %q: %v", got, err)
		}
		if v := u.Query()["initData"]; len(v) != 1 || v[0] != "token:"+token {
			t.Errorf("чужой initData просочился: %v", v)
		}
		if u.Query().Get("jwt") != "" {
			t.Error("посторонний параметр jwt должен быть отброшен")
		}
		if u.Query().Get("resume") != "a:1" {
			t.Errorf("разрешённый resume потерян: %q", u.Query().Get("resume"))
		}
	})

	t.Run("порт подставляется", func(t *testing.T) {
		if got := localStreamURL(18080, "/ws/screen", token); !strings.HasPrefix(got, "ws://127.0.0.1:18080/ws/screen?") {
			t.Errorf("адрес не тот: %q", got)
		}
	})
}

func TestValidStreamPathAgentSide(t *testing.T) {
	good := []string{"/ws/pty/abc", "/ws/screen", "/ws/pty/abc?resume=a:1"}
	for _, p := range good {
		if !validStreamPath(p) {
			t.Errorf("путь должен приниматься: %q", p)
		}
	}
	bad := []string{"/api/files", "/ws/../api/files", "ws://evil/ws/pty/x", "/ws/pty/x\ttab"}
	for _, p := range bad {
		if validStreamPath(p) {
			t.Errorf("путь должен отвергаться: %q", p)
		}
	}
}
