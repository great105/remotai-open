package openrouter

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// Ответ снят с ЖИВОГО ключа 07.08.2026 — в том числе поля-null, из-за которых
// нельзя писать нули: limit=null значит «потолка нет», а не «денег нет».
const liveKeyResponse = `{"data":{"label":"sk-or-v1-611...a5d","is_management_key":false,
"limit":null,"limit_reset":null,"limit_remaining":null,"usage":0,"usage_daily":0,
"usage_weekly":0,"usage_monthly":0,"is_free_tier":false,"expires_at":null,
"rate_limit":{"requests":-1,"interval":"10s","note":"This field is deprecated and safe to ignore."}}}`

func TestKeyInfoParsesLiveShape(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer sk-or-test" {
			t.Errorf("ключ не ушёл в заголовок: %q", got)
		}
		w.Write([]byte(liveKeyResponse))
	}))
	defer srv.Close()
	old := BaseURL
	BaseURL = srv.URL
	defer func() { BaseURL = old }()

	info, err := New("sk-or-test").KeyInfo(context.Background())
	if err != nil {
		t.Fatalf("KeyInfo: %v", err)
	}
	if info.Label != "sk-or-v1-611...a5d" {
		t.Errorf("label = %q", info.Label)
	}
	if info.Limit != nil || info.LimitRemaining != nil {
		t.Error("limit=null обязан остаться nil: иначе интерфейс покажет «осталось $0» тому, у кого потолка нет вовсе")
	}
	if info.IsFreeTier {
		t.Error("is_free_tier=false прочитан как true")
	}
	if got := info.FreeDailyQuota(); got != 1000 {
		t.Errorf("оплативший счёт получает 1000 запросов в сутки, а не %d", got)
	}
	if got := (KeyInfo{IsFreeTier: true}).FreeDailyQuota(); got != 50 {
		t.Errorf("никогда не плативший получает 50 запросов в сутки, а не %d", got)
	}
}

func TestKeyInfoUnauthorizedIsItsOwnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	old := BaseURL
	BaseURL = srv.URL
	defer func() { BaseURL = old }()

	// «Ключ не приняли» человек чинит сам, «сервис недоступен» проходит само —
	// значит это разные ошибки, а не одна «не получилось».
	if _, err := New("sk-or-test").KeyInfo(context.Background()); err != ErrUnauthorized {
		t.Fatalf("ожидали ErrUnauthorized, получили %v", err)
	}
}

func TestModelsPricingAndOrder(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data":[
{"id":"paid/with-tools","name":"Paid","context_length":200000,
 "pricing":{"prompt":"0.0000004","completion":"0.0000016"},"supported_parameters":["tools"]},
{"id":"free/no-tools","name":"FreeNoTools","context_length":128000,
 "pricing":{"prompt":"0","completion":"0"},"supported_parameters":["temperature"]},
{"id":"free/with-tools","name":"FreeTools","context_length":256000,
 "pricing":{"prompt":"0","completion":"0"},"supported_parameters":["tools"]}]}`))
	}))
	defer srv.Close()
	old := BaseURL
	BaseURL = srv.URL
	defer func() { BaseURL = old }()

	models, err := New("sk-or-test").Models(context.Background())
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	// Человек берёт первую строку списка. Бесплатная модель БЕЗ инструментов
	// агенту бесполезна — она не должна стоять выше бесплатной с инструментами.
	if models[0].ID != "free/with-tools" {
		t.Errorf("первой обязана идти бесплатная с инструментами, а идёт %q", models[0].ID)
	}
	if models[1].ID != "free/no-tools" {
		t.Errorf("второй ожидали бесплатную без инструментов, получили %q", models[1].ID)
	}
	// Цена за токен («0.0000004») человеку ничего не говорит — приводим к
	// миллиону токенов ещё в агенте, чтобы клиент не считал сам.
	paid := models[2]
	if paid.PromptPrice != 0.4 || paid.CompletionPrice != 1.6 {
		t.Errorf("цена за миллион токенов = %v/%v, ожидали 0.4/1.6", paid.PromptPrice, paid.CompletionPrice)
	}
	if !models[0].Free || paid.Free {
		t.Error("бесплатность определяется нулевой ценой по обоим направлениям")
	}
}

// Живая проверка настоящим ключом: стенд отвечает так, как МЫ решили, а сервис —
// так, как есть. Запускается только когда ключ передан снаружи:
//
//	OPENROUTER_LIVE_KEY=sk-or-… go test ./internal/openrouter/ -run Live -v
//
// Ключ в репозиторий не кладём никогда — он живёт у владельца и в хранилище на
// его компьютере.
func TestLiveKeyAgainstRealService(t *testing.T) {
	key := os.Getenv("OPENROUTER_LIVE_KEY")
	if key == "" {
		t.Skip("нет OPENROUTER_LIVE_KEY — живая проверка пропущена")
	}
	c := New(key)

	info, err := c.KeyInfo(context.Background())
	if err != nil {
		t.Fatalf("живой KeyInfo: %v", err)
	}
	if info.Label == "" {
		t.Error("живой сервис не вернул метку ключа")
	}
	t.Logf("метка=%s платил=%v расход сегодня=%v квота бесплатных=%d/сутки",
		info.Label, !info.IsFreeTier, info.UsageDaily, info.FreeDailyQuota())

	models, err := c.Models(context.Background())
	if err != nil {
		t.Fatalf("живой Models: %v", err)
	}
	free, freeWithTools := 0, 0
	for _, m := range models {
		if m.Free {
			free++
			if m.Tools {
				freeWithTools++
			}
		}
	}
	if len(models) < 50 {
		t.Errorf("каталог подозрительно мал: %d моделей", len(models))
	}
	if freeWithTools == 0 {
		t.Error("ни одной бесплатной модели с инструментами — агенту не на чем работать")
	}
	// Первая строка списка — то, что человек возьмёт не глядя.
	if !models[0].Free || !models[0].Tools {
		t.Errorf("первой в живом каталоге идёт %q (free=%v tools=%v)", models[0].ID, models[0].Free, models[0].Tools)
	}
	t.Logf("моделей=%d бесплатных=%d из них с инструментами=%d, первая=%s",
		len(models), free, freeWithTools, models[0].ID)
}

func TestLooksLikeKey(t *testing.T) {
	if LooksLikeKey("sk-proj-something-from-openai") {
		t.Error("ключ другого сервиса обязан отсеиваться до сетевого запроса")
	}
	if LooksLikeKey("sk-or-") {
		t.Error("один префикс — не ключ")
	}
	if !LooksLikeKey("  sk-or-v1-0123456789abcdef0123456789  ") {
		t.Error("пробелы по краям вставленного ключа не должны мешать")
	}
}

func TestStoreKeepsKeyOutOfClientAndAppliesEnv(t *testing.T) {
	os.Unsetenv(EnvName)
	path := filepath.Join(t.TempDir(), "openrouter.enc")

	s := NewStore(path)
	if s.Configured() {
		t.Fatal("пустое хранилище не может быть настроенным")
	}
	if err := s.Set("sk-or-v1-secret"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	// Ключ обязан оказаться в окружении АГЕНТА: отсюда его унаследуют терминалы,
	// и только так он не попадает в текст команды, уезжающий на телефон.
	if got := os.Getenv(EnvName); got != "sk-or-v1-secret" {
		t.Errorf("%s = %q, ожидали ключ", EnvName, got)
	}

	// Файл на диске не должен содержать ключ открытым текстом.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("файл не записан: %v", err)
	}
	if containsPlain(raw, "sk-or-v1-secret") {
		t.Error("ключ лежит на диске открытым текстом — хранилище обязано шифровать")
	}

	// Перечитывание с диска возвращает ключ и снова применяет окружение.
	os.Unsetenv(EnvName)
	again := NewStore(path)
	if again.Key() != "sk-or-v1-secret" {
		t.Error("ключ не пережил перезапуск агента")
	}
	if os.Getenv(EnvName) == "" {
		t.Error("после перезапуска окружение не применено — новые терминалы остались бы без ключа")
	}

	// Забыли — значит забыли и в окружении: иначе «удалил» означало бы «не
	// показываю», а терминалы продолжали бы работать с этим ключом.
	if err := again.Clear(); err != nil {
		t.Fatalf("Clear: %v", err)
	}
	if os.Getenv(EnvName) != "" {
		t.Error("после удаления ключ остался в окружении агента")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("после удаления файл хранилища остался на диске")
	}
}

func TestStoreRespectsExternalEnv(t *testing.T) {
	// Ключ, заданный человеком в системе, был раньше нас и мог быть выставлен
	// намеренно — перебивать его сохранённым нельзя.
	os.Setenv(EnvName, "sk-or-v1-from-system")
	defer os.Unsetenv(EnvName)

	s := NewStore(filepath.Join(t.TempDir(), "openrouter.enc"))
	if err := s.Set("sk-or-v1-ours"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if got := os.Getenv(EnvName); got != "sk-or-v1-from-system" {
		t.Errorf("системный ключ перебит на %q", got)
	}
}

func TestStoreModelSurvivesRestart(t *testing.T) {
	os.Unsetenv(EnvName)
	path := filepath.Join(t.TempDir(), "openrouter.enc")
	s := NewStore(path)
	if err := s.Set("sk-or-v1-secret"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := s.SetModel("openrouter/cohere/north-mini-code:free"); err != nil {
		t.Fatalf("SetModel: %v", err)
	}
	if got := NewStore(path).Model(); got != "openrouter/cohere/north-mini-code:free" {
		t.Errorf("модель не пережила перезапуск: %q", got)
	}
	os.Unsetenv(EnvName)
}

func containsPlain(hay []byte, needle string) bool {
	return len(needle) > 0 && len(hay) > 0 &&
		bytesIndex(hay, []byte(needle)) >= 0
}

func bytesIndex(h, n []byte) int {
outer:
	for i := 0; i+len(n) <= len(h); i++ {
		for j := range n {
			if h[i+j] != n[j] {
				continue outer
			}
		}
		return i
	}
	return -1
}
