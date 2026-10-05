// Package openrouter — доступ к OpenRouter по ключу человека.
//
// ЗАЧЕМ ЭТО ЕСТЬ. Чтобы Remotai что-то значил, у человека уже должен быть
// работающий CLI-агент с оплаченной подпиской. OpenRouter снимает это условие:
// один ключ — и любой из 400 моделей, включая бесплатные. Мы не становимся
// поставщиком интеллекта, мы делаем понятной чужую подписку: сколько потрачено,
// что доступно, какой моделью запускать агента.
//
// ЧТО ПРОВЕРЕНО ЖИВЫМ КЛЮЧОМ (07.08.2026), а не прочитано в доках:
//
//   - GET /api/v1/key и GET /api/v1/auth/key — ОДНО И ТО ЖЕ, работают обычным
//     ключом вывода (sk-or-v1-…). Отдают label (уже маскированный самим
//     OpenRouter), usage, usage_daily/weekly/monthly, limit, limit_remaining,
//     is_free_tier;
//   - GET /api/v1/credits отвечает 403 обычным ключом (нужен management-key), и
//     /me, /usage, /activity тоже закрыты. ЗНАЧИТ БАЛАНС АККАУНТА ЧЕРЕЗ API
//     НЕДОСТУПЕН — показываем расход и остаток ЛИМИТА КЛЮЧА, а не «денег на
//     счету». Обещать баланс в интерфейсе нельзя: его неоткуда взять;
//   - в ответе chat/completions есть usage.cost — стоимость каждого запроса;
//   - заголовков X-RateLimit-* в ответе НЕТ: остаток дневной квоты бесплатных
//     моделей узнать неоткуда, он только считается по документированным правилам
//     (см. FreeDailyQuota).
package openrouter

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// BaseURL — точка входа API. Переменной, а не константой, ради тестов.
var BaseURL = "https://openrouter.ai/api/v1"

const requestTimeout = 20 * time.Second

// KeyPrefix — с чего начинается ключ вывода OpenRouter. Проверяем его до
// сетевого запроса: человек, вставивший не тот ключ (например, от OpenAI),
// должен получить внятный ответ сразу, а не «неверный ключ» после ожидания.
const KeyPrefix = "sk-or-"

// KeyInfo — что OpenRouter рассказывает о ключе. Поля-указатели там, где null
// значит «ограничения нет»: 0 и «без лимита» — разные вещи, и путать их нельзя
// (это то же правило, что с квотами агентов: честный «неизвестно» вместо нуля).
type KeyInfo struct {
	// Label — метка ключа от самого OpenRouter, уже маскированная
	// («sk-or-v1-611...a5d»). Свою маскировку не делаем: она бы врала о длине.
	Label string `json:"label"`
	// Usage — потрачено этим ключом за всё время, в долларах.
	Usage        float64 `json:"usage"`
	UsageDaily   float64 `json:"usage_daily"`
	UsageWeekly  float64 `json:"usage_weekly"`
	UsageMonthly float64 `json:"usage_monthly"`
	// Limit — потолок трат ключа (null = без потолка), LimitRemaining — остаток.
	// Это ЕДИНСТВЕННЫЙ способ показать человеку «сколько осталось», потому что
	// баланс аккаунта закрыт: ключ с лимитом сам становится счётчиком.
	Limit          *float64 `json:"limit"`
	LimitRemaining *float64 `json:"limit_remaining"`
	// IsFreeTier — человек НИКОГДА не пополнял счёт. От этого зависит дневная
	// квота бесплатных моделей, и разница там двадцатикратная.
	IsFreeTier bool `json:"is_free_tier"`
	// ExpiresAt — срок ключа, если задан.
	ExpiresAt *string `json:"expires_at"`
}

// FreeDailyQuota — сколько запросов в сутки к бесплатным моделям (:free)
// разрешено этому ключу, по документации OpenRouter.
//
// ЧИСЛО, КОТОРОЕ РЕШАЕТ ВСЁ: пока человек не пополнял счёт, это 50 запросов в
// сутки — МЕНЬШЕ ОДНОЙ агентной задачи, потому что агент делает десятки вызовов
// на задачу. После разового пополнения на $10 — 1000. Поэтому интерфейс обязан
// называть это число прямо, а не писать «бесплатно» и оставлять человека
// упираться в стену посреди работы.
func (k KeyInfo) FreeDailyQuota() int {
	if k.IsFreeTier {
		return 50
	}
	return 1000
}

// FreeRequestsPerMinute — потолок в минуту у бесплатных моделей; от пополнения
// не зависит.
const FreeRequestsPerMinute = 20

// RouterModel — «Free Models Router» OpenRouter: он сам выбирает бесплатную
// модель под каждый запрос.
//
// ПОЧЕМУ ЭТО ВАЖНО ИМЕННО НАМ. Владелец хотел «смотреть, какая бесплатная модель
// сейчас доступна, и переключаться». Писать такую ротацию самим не нужно —
// OpenRouter её уже сделал, и это проверено запуском (09.08.2026):
//
//   - 15 запросов подряд → 15 ответов, ни одного отказа; отвечали семь РАЗНЫХ
//     моделей (gemma-4, laguna, nemotron, gpt-oss);
//   - 12 запросов С ИНСТРУМЕНТАМИ → 12 ответов от одиннадцати разных моделей, и
//     ни разу не выпала `nemotron-3.5-content-safety` (модель-классификатор без
//     поддержки инструментов), хотя без инструментов она попадалась. То есть
//     роутер отбирает модели под требования запроса, а не «любую бесплатную».
//
// Это же обходит и главную беду бесплатных моделей: 429 у них прилетает не от
// нашей квоты, а от общего пула стороннего провайдера («limit_source»:
// «upstream_provider_shared_pool») — то есть от чужой нагрузки. Роутер уводит
// запрос на другую модель, у которой пул свой.
const RouterModel = "openrouter/free"

// RouterModelID — тот же роутер без префикса провайдера, как он приходит в
// каталоге моделей.
const RouterModelID = "openrouter/free"

// Model — одна модель каталога в том виде, в каком её показываем человеку.
type Model struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Context — сколько токенов помещается в окно.
	Context int `json:"context"`
	// Free — цена нулевая по обоим направлениям.
	Free bool `json:"free"`
	// Tools — модель умеет вызывать инструменты. Для агента это не украшение,
	// а условие работы: без tools агент не прочитает файл и не запустит команду.
	Tools bool `json:"tools"`
	// PromptPrice/CompletionPrice — цена за МИЛЛИОН токенов, в долларах.
	// OpenRouter отдаёт цену за один токен строкой вида "0.0000004" — такое
	// число человеку показывать нельзя, поэтому приводим сразу здесь.
	PromptPrice     float64 `json:"prompt_price"`
	CompletionPrice float64 `json:"completion_price"`
	// Router — это не модель, а автоматический выбор модели (см. RouterModel).
	// Интерфейс обязан показывать её иначе: у неё нет ни своего качества, ни
	// своего контекста в привычном смысле — она про «просто работай».
	Router bool `json:"router,omitempty"`
}

// Client — тонкая обёртка над HTTP. Держит ключ, но НИКОГДА его не логирует и
// не возвращает наружу.
type Client struct {
	key  string
	http *http.Client
}

func New(key string) *Client {
	return &Client{key: strings.TrimSpace(key), http: &http.Client{Timeout: requestTimeout}}
}

// ErrUnauthorized — ключ отвергнут. Отдельная ошибка, потому что ответ человеку
// у неё свой: «ключ не подошёл», а не «OpenRouter недоступен». Первое чинит он,
// второе — время.
var ErrUnauthorized = fmt.Errorf("openrouter: ключ не принят")

// LooksLikeKey — грубая проверка формы до сети.
func LooksLikeKey(key string) bool {
	k := strings.TrimSpace(key)
	return strings.HasPrefix(k, KeyPrefix) && len(k) >= 24
}

// KeyInfo спрашивает OpenRouter о самом ключе.
func (c *Client) KeyInfo(ctx context.Context) (KeyInfo, error) {
	var wrap struct {
		Data KeyInfo `json:"data"`
	}
	if err := c.get(ctx, "/key", &wrap); err != nil {
		return KeyInfo{}, err
	}
	return wrap.Data, nil
}

// Models отдаёт каталог, приведённый к нашему виду и отсортированный так, как
// его читают: сначала бесплатные с инструментами (ими и запускают агента),
// потом остальные бесплатные, потом платные.
func (c *Client) Models(ctx context.Context) ([]Model, error) {
	var wrap struct {
		Data []struct {
			ID            string `json:"id"`
			Name          string `json:"name"`
			ContextLength int    `json:"context_length"`
			Pricing       struct {
				Prompt     string `json:"prompt"`
				Completion string `json:"completion"`
			} `json:"pricing"`
			SupportedParameters []string `json:"supported_parameters"`
		} `json:"data"`
	}
	if err := c.get(ctx, "/models", &wrap); err != nil {
		return nil, err
	}
	out := make([]Model, 0, len(wrap.Data))
	for _, m := range wrap.Data {
		prompt := parsePrice(m.Pricing.Prompt)
		completion := parsePrice(m.Pricing.Completion)
		tools := false
		for _, p := range m.SupportedParameters {
			if p == "tools" {
				tools = true
				break
			}
		}
		out = append(out, Model{
			ID:      m.ID,
			Name:    m.Name,
			Context: m.ContextLength,
			Free:    prompt == 0 && completion == 0,
			Tools:   tools,
			Router:  m.ID == RouterModelID,
			// Округление здесь не косметика: 0.0000004 × 1e6 в double даёт
			// 0.39999999999999997, и это число уехало бы человеку на экран
			// ценой за миллион токенов (поймано тестом).
			PromptPrice:     perMillion(prompt),
			CompletionPrice: perMillion(completion),
		})
	}
	sort.SliceStable(out, func(i, j int) bool {
		return modelRank(out[i]) < modelRank(out[j])
	})
	return out, nil
}

// modelRank — порядок показа. Человек берёт первую строку списка, поэтому
// первой обязана стоять та, что просто работает.
//
// Роутер выше всех именно поэтому: он сам выбирает доступную бесплатную модель
// под конкретный запрос, то есть у него нет главной беды бесплатных — «сегодня
// эта отвечает, завтра нет». Дальше бесплатные с инструментами; бесплатная БЕЗ
// инструментов агенту бесполезна (не прочитает файл, не запустит команду) и
// поэтому не должна опережать рабочую.
func modelRank(m Model) int {
	switch {
	case m.Router:
		return 0
	case m.Free && m.Tools:
		return 1
	case m.Free:
		return 2
	case m.Tools:
		return 3
	default:
		return 4
	}
}

// perMillion переводит цену за один токен в цену за миллион, оставляя четыре
// знака после запятой — этого хватает и самой дешёвой модели каталога.
func perMillion(perToken float64) float64 {
	return math.Round(perToken*1_000_000*10_000) / 10_000
}

func parsePrice(s string) float64 {
	v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return 0
	}
	return v
}

func (c *Client) get(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, BaseURL+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.key)
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("openrouter: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return ErrUnauthorized
	}
	if resp.StatusCode != http.StatusOK {
		// Тело ошибки в лог не тащим: там бывает эхо запроса вместе с ключом.
		return fmt.Errorf("openrouter: %s ответил %d", path, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return err
	}
	return json.Unmarshal(body, out)
}
