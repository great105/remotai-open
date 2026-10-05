package openrouter

// Один короткий запрос к модели — ровно для служебных выжимок агента
// («что сделал», «о чём спрашивает»), а не для чата с человеком.
//
// ПОЧЕМУ ОТДЕЛЬНО ОТ orchestrator. Там свой клиент chat/completions, но он
// заточен под многошаговую работу с инструментами: десятиминутный таймаут,
// делегирование, счётчики стоимости. Уведомлению нужно обратное — ответить за
// секунды или не ответить вовсе, потому что человек ждёт сообщение в Telegram,
// а не отчёт.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// ErrRateLimited — модель отказала по частоте/квоте. Отдельная ошибка, потому
// что реакция на неё своя: молча обойтись без выжимки, а не считать это сбоем.
// У бесплатного роутера это штатное состояние (50 запросов в сутки на
// непополненном ключе, см. KeyInfo.FreeDailyQuota).
var ErrRateLimited = errors.New("openrouter: слишком часто")

// completeMaxTokens — потолок ответа. Выжимка обязана быть короткой: она едет в
// уведомление, а не в отчёт.
const completeMaxTokens = 220

// chatClient — клиент генерации: без своего таймаута, срок задаёт ctx
// вызывающего (см. Complete). Один на процесс, чтобы переиспользовались
// соединения: до OpenRouter каждый новый TLS-хендшейк стоит сотни миллисекунд,
// а через VPN владельца — заметно больше.
var chatClient = &http.Client{}

// Complete задаёт модели один вопрос и возвращает текст ответа.
//
// system — роль («ты пересказываешь вывод терминала»), user — сам материал.
// Пустой ответ ошибкой НЕ считается: вызывающий обязан уметь обойтись без
// выжимки, поэтому «модель промолчала» и «модель недоступна» для него одно и то
// же — оба случая дают пустую строку.
func (c *Client) Complete(ctx context.Context, model, system, user string) (string, error) {
	if strings.TrimSpace(c.key) == "" {
		return "", ErrUnauthorized
	}
	if model == "" {
		model = RouterModel
	}
	payload := map[string]any{
		"model": model,
		"messages": []map[string]string{
			{"role": "system", "content": system},
			{"role": "user", "content": user},
		},
		"max_tokens": completeMaxTokens,
		// Пересказ — не творчество: одинаковый вывод обязан давать одинаковый
		// текст, иначе одно и то же событие выглядит в чате каждый раз иначе.
		"temperature": 0,
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, BaseURL+"/chat/completions", bytes.NewReader(raw))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+c.key)
	req.Header.Set("Content-Type", "application/json")
	// Атрибуция запроса у OpenRouter: без неё трафик уходит в «безымянное»
	// и человек не поймёт в своей статистике, откуда взялись эти запросы.
	req.Header.Set("HTTP-Referer", "https://remotai.ru")
	req.Header.Set("X-Title", "Remotai")

	// СВОЙ клиент, а не c.http: у того таймаут 20 секунд на любой запрос, и
	// этого хватает справочным GET'ам (ключ, каталог), но НЕ генерации. Замер на
	// живом ключе: бесплатный роутер отвечает 5–10 секунд, а иногда дольше
	// двадцати — и такие ответы молча терялись как «модель не ответила», хотя
	// запрос из суточной квоты уже списался. Срок жизни запроса задаёт ctx
	// вызывающего: он один знает, сколько уведомление ещё имеет смысл ждать.
	resp, err := chatClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("openrouter: %w", err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return "", ErrUnauthorized
	case resp.StatusCode == http.StatusTooManyRequests:
		return "", ErrRateLimited
	case resp.StatusCode != http.StatusOK:
		// Тело в ошибку не тащим: там бывает эхо запроса вместе с ключом.
		return "", fmt.Errorf("openrouter: chat/completions ответил %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		// Ошибка приходит и с кодом 200 (роутер не нашёл свободной модели) —
		// разбираем её здесь же, иначе такой ответ выглядел бы как пустой.
		Error struct {
			Message string `json:"message"`
			Code    int    `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", err
	}
	if out.Error.Code == http.StatusTooManyRequests {
		return "", ErrRateLimited
	}
	if len(out.Choices) == 0 {
		if out.Error.Message != "" {
			return "", fmt.Errorf("openrouter: %s", out.Error.Message)
		}
		return "", nil
	}
	return strings.TrimSpace(out.Choices[0].Message.Content), nil
}
