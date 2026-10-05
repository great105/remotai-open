// Package billing — приём денег через ЮKassa (магазин 1451249, remotai.ru).
//
// Контракт API выяснен ЖИВЫМИ запросами 01.09.2026, а не по документации:
// страницы developers отдают навигацию, а не поля. Что показал боевой магазин:
//
//   - `receipt` ОБЯЗАТЕЛЕН. Без него ответ 400 «Receipt is missing or illegal»:
//     у магазина подключены «Чеки от ЮKassa», и данные чека приходят с каждым
//     платежом. Отсюда customer.email и items в каждом запросе.
//   - `save_payment_method: true` пока даёт 403 «This store can't make
//     recurring payments» — автоплатежи боевому магазину включает менеджер по
//     отдельному запросу. Поэтому запрос умеет понижаться: получив этот отказ,
//     повторяет платёж БЕЗ сохранения карты, чтобы разовая оплата работала уже
//     сегодня, и говорит об этом в лог.
//   - Ответ содержит confirmation.confirmation_url — туда и отправляем человека.
//
// Секретный ключ живёт в переменных окружения релея и НИКОГДА в репозитории.
package billing

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
)

const apiBase = "https://api.yookassa.ru/v3"

// ErrNotConfigured — магазин не настроен: ключей нет, платить нечем.
var ErrNotConfigured = errors.New("yookassa: не настроен shop_id/secret_key")

// Client — доступ к API ЮKassa одного магазина.
type Client struct {
	ShopID    string
	SecretKey string
	HTTP      *http.Client

	// Recurring — умеет ли магазин автоплатежи. Начинается с того, что скажет
	// конфигурация, и САМ выключается, если ЮKassa ответила отказом: живой
	// магазин — источник правды достовернее переменной окружения.
	recurringOff bool
}

func New(shopID, secretKey string) *Client {
	return &Client{
		ShopID:    strings.TrimSpace(shopID),
		SecretKey: strings.TrimSpace(secretKey),
		HTTP:      &http.Client{Timeout: 30 * time.Second},
	}
}

func (c *Client) Configured() bool {
	return c != nil && c.ShopID != "" && c.SecretKey != ""
}

// Money — сумма в минорных единицах (копейках): дробей в расчётах не бывает.
type Money int64

func (m Money) String() string { return fmt.Sprintf("%d.%02d", int64(m)/100, int64(m)%100) }

// Payment — то, что вернула ЮKassa. Поля только те, что нам нужны.
type Payment struct {
	ID     string `json:"id"`
	Status string `json:"status"` // pending | waiting_for_capture | succeeded | canceled
	Paid   bool   `json:"paid"`
	Amount struct {
		Value    string `json:"value"`
		Currency string `json:"currency"`
	} `json:"amount"`
	Confirmation struct {
		Type            string `json:"type"`
		ConfirmationURL string `json:"confirmation_url"`
	} `json:"confirmation"`
	PaymentMethod struct {
		ID    string `json:"id"`
		Type  string `json:"type"`
		Saved bool   `json:"saved"`
		Card  struct {
			Last4    string `json:"last4"`
			CardType string `json:"card_type"`
			ExpiryY  string `json:"expiry_year"`
			ExpiryM  string `json:"expiry_month"`
		} `json:"card"`
	} `json:"payment_method"`
	Metadata map[string]string `json:"metadata"`
	Test     bool              `json:"test"`
}

// ReceiptItem — строка чека. 54-ФЗ закрывается на стороне ЮKassa, но состав
// чека обязаны прислать мы.
type ReceiptItem struct {
	Description    string `json:"description"`
	Quantity       string `json:"quantity"`
	Amount         amount `json:"amount"`
	VATCode        int    `json:"vat_code"`
	PaymentSubject string `json:"payment_subject"`
	PaymentMode    string `json:"payment_mode"`
}

type amount struct {
	Value    string `json:"value"`
	Currency string `json:"currency"`
}

// CreateRequest — что именно оплачивают.
type CreateRequest struct {
	Amount      Money
	Description string
	ReturnURL   string
	Email       string            // для чека; ЮKassa шлёт его покупателю
	Metadata    map[string]string // сюда кладём user_id и полку
	SaveCard    bool              // первый платёж подписки: запомнить способ оплаты
	Method      string            // "sbp" | "bank_card" | "" (человек выберет сам)
	VATCode     int               // 1 = без НДС (УСН)
}

// Create создаёт платёж и возвращает его вместе со ссылкой на оплату.
func (c *Client) Create(ctx context.Context, req CreateRequest) (*Payment, error) {
	if !c.Configured() {
		return nil, ErrNotConfigured
	}
	vat := req.VATCode
	if vat == 0 {
		vat = 1 // без НДС: ИП на УСН
	}
	sum := amount{Value: req.Amount.String(), Currency: "RUB"}

	body := map[string]any{
		"amount":       sum,
		"capture":      true,
		"description":  req.Description,
		"confirmation": map[string]any{"type": "redirect", "return_url": req.ReturnURL},
		"metadata":     req.Metadata,
		// ⚠ Без receipt боевой магазин отвечает 400 «Receipt is missing or
		// illegal» — проверено запросом.
		"receipt": map[string]any{
			"customer": map[string]any{"email": req.Email},
			"items": []ReceiptItem{{
				Description:    req.Description,
				Quantity:       "1.00",
				Amount:         sum,
				VATCode:        vat,
				PaymentSubject: "service",
				PaymentMode:    "full_payment",
			}},
		},
	}
	if req.Method != "" {
		body["payment_method_data"] = map[string]any{"type": req.Method}
	}
	if req.SaveCard && !c.recurringOff {
		body["save_payment_method"] = true
	}

	pay, err := c.post(ctx, "/payments", body)
	if err == nil {
		return pay, nil
	}

	// Магазину ещё не включили автоплатежи. Разовая оплата от этого страдать не
	// должна: повторяем без сохранения карты и запоминаем отказ, чтобы не
	// биться в него на каждом платеже.
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.IsRecurringForbidden() {
		c.recurringOff = true
		log.Printf("[BILLING] ЮKassa: автоплатежи магазину не включены (%s) — платёж без сохранения карты", apiErr.Description)
		delete(body, "save_payment_method")
		return c.post(ctx, "/payments", body)
	}
	return nil, err
}

// Charge списывает по сохранённому способу оплаты — это и есть автоплатёж.
// Человек в нём не участвует, поэтому confirmation не нужен.
func (c *Client) Charge(ctx context.Context, methodID string, req CreateRequest) (*Payment, error) {
	if !c.Configured() {
		return nil, ErrNotConfigured
	}
	if methodID == "" {
		return nil, errors.New("yookassa: нет сохранённого способа оплаты")
	}
	vat := req.VATCode
	if vat == 0 {
		vat = 1
	}
	sum := amount{Value: req.Amount.String(), Currency: "RUB"}
	body := map[string]any{
		"amount":            sum,
		"capture":           true,
		"description":       req.Description,
		"payment_method_id": methodID,
		"metadata":          req.Metadata,
		"receipt": map[string]any{
			"customer": map[string]any{"email": req.Email},
			"items": []ReceiptItem{{
				Description:    req.Description,
				Quantity:       "1.00",
				Amount:         sum,
				VATCode:        vat,
				PaymentSubject: "service",
				PaymentMode:    "full_payment",
			}},
		},
	}
	return c.post(ctx, "/payments", body)
}

// Get спрашивает состояние платежа. Нужен там, где уведомлению верить нельзя:
// webhook приходит по открытой ручке, а этот ответ — от самой ЮKassa.
func (c *Client) Get(ctx context.Context, paymentID string) (*Payment, error) {
	if !c.Configured() {
		return nil, ErrNotConfigured
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiBase+"/payments/"+paymentID, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Basic "+c.basic())
	return c.do(req)
}

func (c *Client) post(ctx context.Context, path string, body map[string]any) (*Payment, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiBase+path, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Basic "+c.basic())
	req.Header.Set("Content-Type", "application/json")
	// Ключ идемпотентности: повтор запроса с тем же ключом не создаёт второй
	// платёж. Свежий на каждую попытку — повторяем мы осознанно и хотим новый.
	req.Header.Set("Idempotence-Key", uuid.NewString())
	return c.do(req)
}

func (c *Client) do(req *http.Request) (*Payment, error) {
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("yookassa: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		e := &APIError{HTTPStatus: resp.StatusCode}
		_ = json.Unmarshal(data, e)
		return nil, e
	}
	var p Payment
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, fmt.Errorf("yookassa: неразбираемый ответ: %w", err)
	}
	return &p, nil
}

func (c *Client) basic() string {
	return base64.StdEncoding.EncodeToString([]byte(c.ShopID + ":" + c.SecretKey))
}

// APIError — отказ ЮKassa вместе с её объяснением.
type APIError struct {
	HTTPStatus  int    `json:"-"`
	Type        string `json:"type"`
	ID          string `json:"id"`
	Code        string `json:"code"`
	Description string `json:"description"`
	Parameter   string `json:"parameter"`
}

func (e *APIError) Error() string {
	return fmt.Sprintf("yookassa %d %s: %s", e.HTTPStatus, e.Code, e.Description)
}

// IsRecurringForbidden — тот самый отказ «магазину не включены автоплатежи».
// Проверено живым запросом 01.09.2026: 403 + «This store can't make recurring
// payments. Contact the YooMoney manager to learn more».
func (e *APIError) IsRecurringForbidden() bool {
	return e.HTTPStatus == http.StatusForbidden &&
		strings.Contains(strings.ToLower(e.Description), "recurring payments")
}

// RecurringDisabled — узнали ли мы от самой ЮKassa, что автоплатежей нет.
func (c *Client) RecurringDisabled() bool { return c != nil && c.recurringOff }
