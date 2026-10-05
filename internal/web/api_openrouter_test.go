package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"tgcontrol/internal/openrouter"
)

// Стенд OpenRouter: отвечает так же, как живой сервис (форма ответа снята с
// настоящего ключа 07.08.2026), и запоминает, с каким ключом к нему пришли.
func fakeOpenRouter(t *testing.T, keySeen *string, keyStatus int) func() {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if keySeen != nil {
			*keySeen = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/key"):
			if keyStatus != 0 && keyStatus != http.StatusOK {
				w.WriteHeader(keyStatus)
				return
			}
			w.Write([]byte(`{"data":{"label":"sk-or-v1-611...a5d","usage":1.5,"usage_daily":0.31,
"usage_weekly":1.2,"usage_monthly":1.5,"limit":null,"limit_remaining":null,"is_free_tier":false}}`))
		case strings.HasSuffix(r.URL.Path, "/models"):
			w.Write([]byte(`{"data":[
{"id":"cohere/north-mini-code:free","name":"North Mini Code","context_length":256000,
 "pricing":{"prompt":"0","completion":"0"},"supported_parameters":["tools"]},
{"id":"openrouter/free","name":"Free Models Router","context_length":200000,
 "pricing":{"prompt":"0","completion":"0"},"supported_parameters":["tools"]},
{"id":"anthropic/claude-sonnet","name":"Claude Sonnet","context_length":200000,
 "pricing":{"prompt":"0.000003","completion":"0.000015"},"supported_parameters":["tools"]}]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	old := openrouter.BaseURL
	openrouter.BaseURL = srv.URL
	return func() {
		openrouter.BaseURL = old
		srv.Close()
	}
}

func newOpenRouterServer(t *testing.T) *Server {
	t.Helper()
	os.Unsetenv(openrouter.EnvName)
	t.Cleanup(func() { os.Unsetenv(openrouter.EnvName) })
	return &Server{openrouter: openrouter.NewStore(filepath.Join(t.TempDir(), "openrouter.enc"))}
}

func callJSON(t *testing.T, h func(http.ResponseWriter, *http.Request, int64), method, target, body string) (int, map[string]any) {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, target, nil)
	} else {
		r = httptest.NewRequest(method, target, strings.NewReader(body))
	}
	w := httptest.NewRecorder()
	h(w, r, 1)
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

// ГЛАВНОЕ СВОЙСТВО ВСЕГО РАЗДЕЛА: ключ не выходит наружу ни одним эндпоинтом.
// Команда запуска агента собирается на телефоне и ходит через облако — утёкший
// сюда ключ уехал бы с компьютера вместе с ней.
func TestOpenRouterNeverReturnsKey(t *testing.T) {
	restore := fakeOpenRouter(t, nil, 0)
	defer restore()
	s := newOpenRouterServer(t)
	const key = "sk-or-v1-0123456789abcdef0123456789"

	for _, tc := range []struct {
		name string
		call func() (int, map[string]any)
	}{
		{"сохранение", func() (int, map[string]any) {
			return callJSON(t, s.apiOpenRouterSetKey, "POST", "/api/openrouter/key", `{"key":"`+key+`"}`)
		}},
		{"состояние", func() (int, map[string]any) {
			return callJSON(t, s.apiOpenRouterStatus, "GET", "/api/openrouter", "")
		}},
		{"модели", func() (int, map[string]any) {
			return callJSON(t, s.apiOpenRouterModels, "GET", "/api/openrouter/models", "")
		}},
	} {
		code, out := tc.call()
		if code != http.StatusOK {
			t.Fatalf("%s: код %d", tc.name, code)
		}
		raw, _ := json.Marshal(out)
		if strings.Contains(string(raw), key) {
			t.Fatalf("%s: ключ уехал наружу в ответе API", tc.name)
		}
	}
}

func TestOpenRouterKeyCheckedBeforeSaving(t *testing.T) {
	// Ключ чужого сервиса отсекается ДО сети: человек узнаёт об ошибке сразу и
	// по делу, а не «ключ не принят» после ожидания.
	s := newOpenRouterServer(t)
	code, out := callJSON(t, s.apiOpenRouterSetKey, "POST", "/api/openrouter/key", `{"key":"sk-proj-openai"}`)
	if code != http.StatusBadRequest || out["code"] != "key_malformed" {
		t.Fatalf("форма ключа не проверена: код %d, ответ %v", code, out)
	}
	if s.openrouter.Configured() {
		t.Fatal("непроверенный ключ попал в хранилище")
	}

	// Ключ верной формы, но отвергнутый OpenRouter, тоже не сохраняется:
	// зелёная галочка на нерабочем ключе означала бы отказ агента через
	// полчаса, посреди работы.
	restore := fakeOpenRouter(t, nil, http.StatusUnauthorized)
	defer restore()
	code, out = callJSON(t, s.apiOpenRouterSetKey, "POST", "/api/openrouter/key", `{"key":"sk-or-v1-0123456789abcdef0123456789"}`)
	if code != http.StatusBadRequest || out["code"] != "key_rejected" {
		t.Fatalf("отвергнутый ключ принят: код %d, ответ %v", code, out)
	}
	if s.openrouter.Configured() {
		t.Fatal("отвергнутый ключ сохранён")
	}
	if os.Getenv(openrouter.EnvName) != "" {
		t.Fatal("отвергнутый ключ попал в окружение агента")
	}
}

func TestOpenRouterStatusTellsTheTruthAboutMoney(t *testing.T) {
	var keySeen string
	restore := fakeOpenRouter(t, &keySeen, 0)
	defer restore()
	s := newOpenRouterServer(t)
	const key = "sk-or-v1-0123456789abcdef0123456789"
	if code, _ := callJSON(t, s.apiOpenRouterSetKey, "POST", "/api/openrouter/key", `{"key":"`+key+`"}`); code != 200 {
		t.Fatalf("ключ не сохранён: %d", code)
	}
	if keySeen != key {
		t.Fatalf("к OpenRouter ушёл не тот ключ: %q", keySeen)
	}

	code, out := callJSON(t, s.apiOpenRouterStatus, "GET", "/api/openrouter", "")
	if code != 200 || out["configured"] != true || out["key_state"] != "ok" {
		t.Fatalf("состояние: код %d, ответ %v", code, out)
	}
	// Баланса счёта в ответе быть НЕ МОЖЕТ: обычным ключом он недоступен
	// (/api/v1/credits → 403). Показываем расход, а не выдуманный остаток.
	if _, has := out["balance"]; has {
		t.Error("в ответе появился баланс, которого OpenRouter нам не отдаёт")
	}
	if out["usage_daily"] != 0.31 {
		t.Errorf("расход за сегодня = %v", out["usage_daily"])
	}
	// limit=null в живом ответе значит «потолка нет» — ноль здесь был бы
	// прямой неправдой («осталось $0» тому, у кого потолка нет вовсе).
	if _, has := out["limit_remaining"]; has {
		t.Error("отсутствующий потолок ключа превратился в число")
	}
	// Платившему полагается 1000 запросов в сутки к бесплатным моделям.
	if out["free_daily_quota"] != float64(1000) {
		t.Errorf("дневная квота = %v", out["free_daily_quota"])
	}
	if out["label"] != "sk-or-v1-611...a5d" {
		t.Errorf("метка ключа = %v", out["label"])
	}
}

// Подключил ключ — и всё работает: выбирать модель из четырёхсот человек не
// должен. Но собственный выбор сильнее любого умолчания.
func TestOpenRouterPicksRouterByDefaultButNeverOverridesChoice(t *testing.T) {
	restore := fakeOpenRouter(t, nil, 0)
	defer restore()
	const key = `{"key":"sk-or-v1-0123456789abcdef0123456789"}`

	s := newOpenRouterServer(t)
	_, out := callJSON(t, s.apiOpenRouterSetKey, "POST", "/api/openrouter/key", key)
	if out["model"] != openrouter.RouterModel {
		t.Errorf("после подключения модель = %v, ожидали автоподбор", out["model"])
	}

	// А теперь человек выбрал сам — и переподключение ключа обязано это уважать.
	other := newOpenRouterServer(t)
	if err := other.openrouter.SetModel("openrouter/anthropic/claude-sonnet"); err != nil {
		t.Fatalf("SetModel: %v", err)
	}
	_, out = callJSON(t, other.apiOpenRouterSetKey, "POST", "/api/openrouter/key", key)
	if out["model"] != "openrouter/anthropic/claude-sonnet" {
		t.Errorf("выбор человека перебит на %v", out["model"])
	}
}

func TestOpenRouterModelRoundTripAndForget(t *testing.T) {
	restore := fakeOpenRouter(t, nil, 0)
	defer restore()
	s := newOpenRouterServer(t)
	if code, _ := callJSON(t, s.apiOpenRouterSetKey, "POST", "/api/openrouter/key",
		`{"key":"sk-or-v1-0123456789abcdef0123456789"}`); code != 200 {
		t.Fatal("ключ не сохранён")
	}

	// Каталог: бесплатная с инструментами обязана идти первой — человек берёт
	// первую строку списка.
	orModelsCache = orModelsCacheT{}
	code, out := callJSON(t, s.apiOpenRouterModels, "GET", "/api/openrouter/models", "")
	if code != 200 {
		t.Fatalf("модели: код %d", code)
	}
	list, _ := out["models"].([]any)
	if len(list) != 3 {
		t.Fatalf("моделей в ответе: %d", len(list))
	}
	// Человек берёт первую строку. Первой обязан идти роутер: он сам подбирает
	// доступную бесплатную модель, то есть у него нет беды «сегодня отвечает,
	// завтра нет».
	first, _ := list[0].(map[string]any)
	if first["id"] != "openrouter/free" || first["router"] != true {
		t.Errorf("первой идёт %v (router=%v), а не автоподбор модели", first["id"], first["router"])
	}
	second, _ := list[1].(map[string]any)
	if second["id"] != "cohere/north-mini-code:free" {
		t.Errorf("второй ожидали бесплатную с инструментами, получили %v", second["id"])
	}
	if out["free_total"] != float64(2) {
		t.Errorf("бесплатных насчитано %v", out["free_total"])
	}

	const model = "openrouter/cohere/north-mini-code:free"
	if code, _ := callJSON(t, s.apiOpenRouterSetModel, "POST", "/api/openrouter/model",
		`{"model":"`+model+`"}`); code != 200 {
		t.Fatal("модель не сохранена")
	}
	if _, st := callJSON(t, s.apiOpenRouterStatus, "GET", "/api/openrouter", ""); st["model"] != model {
		t.Errorf("модель в состоянии = %v", st["model"])
	}

	// «Отключить» обязано снимать ключ и с окружения: иначе терминалы
	// продолжали бы работать с ключом, который человек считает удалённым.
	if code, _ := callJSON(t, s.apiOpenRouterForgetKey, "DELETE", "/api/openrouter/key", ""); code != 200 {
		t.Fatal("ключ не удалён")
	}
	if os.Getenv(openrouter.EnvName) != "" {
		t.Error("после отключения ключ остался в окружении агента")
	}
	if _, st := callJSON(t, s.apiOpenRouterStatus, "GET", "/api/openrouter", ""); st["configured"] != false {
		t.Errorf("после отключения состояние = %v", st)
	}
}
