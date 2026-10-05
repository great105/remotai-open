package cdp

// Состояние страницы и вкладок — то, из чего собирается «оболочка браузера»:
// адресная строка, полоса загрузки, переключатель вкладок.
//
// Вкладками управляем через HTTP-ручки самого браузера (/json/new, /json/close,
// /json/activate), а не через домен Target: там пришлось бы держать вторую
// сессию к самому браузеру и разбирать плоские сессии, а результат тот же.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// PageInfo — что показать в адресной строке.
type PageInfo struct {
	URL       string  `json:"url"`
	Title     string  `json:"title"`
	CanBack   bool    `json:"can_back"`
	CanFwd    bool    `json:"can_forward"`
	Loading   bool    `json:"loading"`
	TargetID  string  `json:"target_id,omitempty"`
	Device    string  `json:"device,omitempty"`
	PageScale float64 `json:"page_scale,omitempty"`
}

// Info собирает состояние текущей вкладки.
func (c *Client) Info(ctx context.Context) PageInfo {
	info := PageInfo{TargetID: c.targetID, Device: c.CurrentDevice().Name, Loading: c.loading.Load()}
	raw, err := c.Call(ctx, "Page.getNavigationHistory", nil)
	if err != nil {
		return info
	}
	var h struct {
		CurrentIndex int `json:"currentIndex"`
		Entries      []struct {
			URL   string `json:"url"`
			Title string `json:"title"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(raw, &h); err != nil {
		return info
	}
	if h.CurrentIndex >= 0 && h.CurrentIndex < len(h.Entries) {
		info.URL = h.Entries[h.CurrentIndex].URL
		info.Title = h.Entries[h.CurrentIndex].Title
	}
	info.CanBack = h.CurrentIndex > 0
	info.CanFwd = h.CurrentIndex >= 0 && h.CurrentIndex < len(h.Entries)-1
	return info
}

// Tab — вкладка браузера.
type Tab struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	URL    string `json:"url"`
	Active bool   `json:"active"`
}

// Tabs перечисляет открытые вкладки.
func Tabs(ctx context.Context, endpoint, activeID string) ([]Tab, error) {
	body, err := devtoolsGET(ctx, endpoint, "/json/list")
	if err != nil {
		return nil, err
	}
	var raw []pageTarget
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("список вкладок: %w", err)
	}
	tabs := make([]Tab, 0, len(raw))
	for _, t := range raw {
		if t.Type != "page" || t.WSURL == "" {
			continue // служебные цели: расширения, фоновые страницы
		}
		title := t.Title
		if strings.TrimSpace(title) == "" {
			title = t.URL
		}
		tabs = append(tabs, Tab{ID: t.ID, Title: title, URL: t.URL, Active: t.ID == activeID})
	}
	return tabs, nil
}

// NewTab открывает вкладку и возвращает её идентификатор.
func NewTab(ctx context.Context, endpoint, target string) (string, error) {
	path := "/json/new"
	if target != "" {
		path += "?" + url.QueryEscape(target)
	}
	// PUT — так требует современный DevTools (GET оставлен для совместимости).
	body, err := devtoolsDo(ctx, http.MethodPut, endpoint, path)
	if err != nil {
		return "", err
	}
	var t pageTarget
	if err := json.Unmarshal(body, &t); err != nil {
		return "", fmt.Errorf("новая вкладка: %w", err)
	}
	return t.ID, nil
}

// CloseTab закрывает вкладку.
func CloseTab(ctx context.Context, endpoint, id string) error {
	_, err := devtoolsGET(ctx, endpoint, "/json/close/"+url.PathEscape(id))
	return err
}

// ActivateTab делает вкладку активной в самом браузере (страница получает
// focus — без этого таймеры и анимации на ней придушены).
func ActivateTab(ctx context.Context, endpoint, id string) error {
	_, err := devtoolsGET(ctx, endpoint, "/json/activate/"+url.PathEscape(id))
	return err
}

func devtoolsGET(ctx context.Context, endpoint, path string) ([]byte, error) {
	return devtoolsDo(ctx, http.MethodGet, endpoint, path)
}

func devtoolsDo(ctx context.Context, method, endpoint, path string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, "http://"+endpoint+path, nil)
	if err != nil {
		return nil, err
	}
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("devtools %s: %w", path, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("devtools %s: HTTP %d", path, resp.StatusCode)
	}
	return body, nil
}

// EnableDownloads разрешает скачивание файлов и говорит, куда их класть.
//
// По умолчанию headless-браузер скачивание запрещает: человек жмёт «Скачать» и
// не получает ничего, без единого сообщения. Файлы падают в папку на сервере —
// оттуда их видно в файловом менеджере приложения и можно забрать на телефон.
func (c *Client) EnableDownloads(ctx context.Context, dir string) error {
	_, err := c.Call(ctx, "Browser.setDownloadBehavior", map[string]any{
		"behavior":      "allow",
		"downloadPath":  dir,
		"eventsEnabled": true,
	})
	return err
}
