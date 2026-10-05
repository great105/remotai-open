package relay

import (
	"net/url"
	"strings"
	"testing"
)

// Облачный стрим приходит в локальный веб-сервер с 127.0.0.1 — мост открывает
// сам агент. Без метки транспорта отличить его от домашнего невозможно.
func TestLocalStreamURLMarksRelay(t *testing.T) {
	raw := localStreamURL(8080, "/ws/screen", "tok")
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got := u.Query().Get("transport"); got != "relay" {
		t.Errorf("transport = %q, want relay (иначе облако считается домом и идёт без лимитов)", got)
	}
	if !strings.HasPrefix(raw, "ws://127.0.0.1:8080/ws/screen?") {
		t.Errorf("сломан адрес локального обработчика: %s", raw)
	}
}

// Клиент не должен уметь объявить своё облачное подключение домашним:
// чужие параметры отбрасываются белым списком, метку ставим мы сами.
func TestLocalStreamURLIgnoresClientTransport(t *testing.T) {
	raw := localStreamURL(8080, "/ws/screen?transport=lan&resume=5:120", "tok")
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got := u.Query().Get("transport"); got != "relay" {
		t.Errorf("подмена метки клиентом прошла: transport = %q, want relay", got)
	}
	// resume в белом списке — он обязан дойти, иначе телефон при переподключении
	// снова потянет весь буфер и сотрёт прокрученную историю.
	if got := u.Query().Get("resume"); got != "5:120" {
		t.Errorf("resume = %q, want 5:120", got)
	}
}

// Тот же запрет для HTTP-запросов, идущих через туннель релея.
func TestBuildRequestMarksRelay(t *testing.T) {
	req, err := buildRequest(Cmd{
		Method: "POST",
		Path:   "/api/webrtc/offer",
		Query:  map[string]string{"transport": "lan", "display": "0"},
	})
	if err != nil {
		t.Fatalf("buildRequest: %v", err)
	}
	q := req.URL.Query()
	if got := q.Get("transport"); got != "relay" {
		t.Errorf("подмена метки клиентом прошла: transport = %q, want relay", got)
	}
	if got := q.Get("display"); got != "0" {
		t.Errorf("остальные параметры обязаны доходить: display = %q", got)
	}
}

// Запрос без своих параметров тоже обязан быть помечен — иначе облачный
// WebRTC-оффер уедет без лимитов тарифа.
func TestBuildRequestMarksRelayWithoutQuery(t *testing.T) {
	req, err := buildRequest(Cmd{Method: "GET", Path: "/api/pty"})
	if err != nil {
		t.Fatalf("buildRequest: %v", err)
	}
	if got := req.URL.Query().Get("transport"); got != "relay" {
		t.Errorf("transport = %q, want relay", got)
	}
}
