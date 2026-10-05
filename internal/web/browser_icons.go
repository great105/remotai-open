package web

// Значки сайтов и превью вкладок — то, по чему человек узнаёт вкладку взглядом.
//
// В списке вкладок были буквы в цветных кружках: работает, но требует ЧТЕНИЯ.
// В настоящем браузере вкладка узнаётся значком сайта мгновенно, а на телефоне —
// ещё и картинкой страницы. Ни того, ни другого протокол не отдаёт готовым:
// список целей DevTools значков не содержит (проверено на живой машине), а кадр
// есть только у той вкладки, к которой мы подключены.
//
// Поэтому:
//   - значок берём у активной страницы (она знает свой путь) и кэшируем ПО
//     ДОМЕНУ: так он появляется и у остальных вкладок того же сайта, ровно как
//     в браузере, где значок кэшируется на домен;
//   - превью снимаем в момент УХОДА с вкладки: кадр этой страницы у нас уже
//     есть, и второй раз её открывать незачем. Картинка уменьшается до 240 px —
//     список вкладок не должен весить как сама страница.

import (
	"bytes"
	"context"
	"encoding/base64"
	"image"
	"image/jpeg"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/image/draw"
)

const (
	iconTTL       = 6 * time.Hour    // значок сайта меняется редко
	iconMissTTL   = 30 * time.Minute // «значка нет» тоже помним, но недолго
	iconMaxBytes  = 64 << 10         // больше — это не значок, а картинка
	previewWidth  = 240              // ширина превью в списке вкладок
	previewMaxAge = 30 * time.Minute // старше — уже не про эту страницу
	previewLimit  = 12               // сколько вкладок помним
)

type iconEntry struct {
	dataURL string
	at      time.Time
}

var icons struct {
	mu     sync.Mutex
	byHost map[string]iconEntry
}

// browserIconForHost отдаёт значок сайта как data-URL (пустая строка — нет).
func browserIconForHost(host string) string {
	host = normalizeVaultHost(host)
	if host == "" {
		return ""
	}
	icons.mu.Lock()
	defer icons.mu.Unlock()
	entry, ok := icons.byHost[host]
	if !ok {
		return ""
	}
	ttl := iconTTL
	if entry.dataURL == "" {
		ttl = iconMissTTL
	}
	if time.Since(entry.at) > ttl {
		delete(icons.byHost, host)
		return ""
	}
	return entry.dataURL
}

// rememberIcon кладёт значок в кэш домена (пустой — отрицательный результат).
func rememberIcon(host, dataURL string) {
	host = normalizeVaultHost(host)
	if host == "" {
		return
	}
	icons.mu.Lock()
	defer icons.mu.Unlock()
	if icons.byHost == nil {
		icons.byHost = map[string]iconEntry{}
	}
	icons.byHost[host] = iconEntry{dataURL: dataURL, at: time.Now()}
}

// fetchBrowserIcon качает значок с сайта силами агента.
//
// Именно агента, а не телефона: телефон, загружающий значок напрямую, ходит на
// сайт со своего адреса — это и лишний трафик, и след человека там, где он
// открывал страницу через сервер.
func fetchBrowserIcon(ctx context.Context, iconURL string) string {
	u, err := url.Parse(iconURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return ""
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, iconURL, nil)
	if err != nil {
		return ""
	}
	// Без имени клиента часть сайтов отвечает отказом (Википедия — живой
	// пример: curl с того же сервера значок отдаёт, безымянному клиенту — нет).
	req.Header.Set("User-Agent", "Mozilla/5.0 (Linux; Android 14; Pixel 7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/150.0.0.0 Mobile Safari/537.36")
	req.Header.Set("Accept", "image/avif,image/webp,image/png,image/svg+xml,image/*,*/*;q=0.8")
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, iconMaxBytes+1))
	if err != nil || len(data) == 0 || len(data) > iconMaxBytes {
		return ""
	}
	mime := resp.Header.Get("Content-Type")
	if i := strings.Index(mime, ";"); i > 0 {
		mime = mime[:i]
	}
	mime = strings.TrimSpace(strings.ToLower(mime))
	if !strings.HasPrefix(mime, "image/") {
		// Сайт вернул страницу вместо значка (частый ответ на отсутствующий
		// /favicon.ico) — значка нет.
		return ""
	}
	return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(data)
}

// refreshActiveTabIcon узнаёт значок открытой страницы и запоминает его для
// домена. Зовётся из фоновой задачи после загрузки страницы: держать её в
// обработчике списка вкладок нельзя — иначе каждый опрос списка ждал бы сети.
func refreshActiveTabIcon() {
	client := currentCDPClient()
	if client == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	pageURL := client.URL(ctx)
	host := normalizeVaultHost(pageURL)
	if host == "" {
		return
	}
	if browserIconForHost(host) != "" {
		return // уже знаем
	}
	// Первым делом просим значок у самой страницы: она качает его своим
	// User-Agent и со своими куками. Отдельный запрос с агента часть сайтов
	// отвергает (живой пример — Википедия), поэтому он остался запасным путём.
	data, err := client.IconData(ctx)
	if err == nil && data != "" {
		rememberIcon(host, data)
		return
	}
	iconURL, err := client.IconURL(ctx)
	if err != nil || iconURL == "" {
		rememberIcon(host, "")
		return
	}
	data = fetchBrowserIcon(ctx, iconURL)
	if data == "" {
		log.Printf("[VBROWSER] значок %s не загрузился (%s)", host, iconURL)
	}
	rememberIcon(host, data)
}

type previewEntry struct {
	dataURL string
	at      time.Time
}

var previews struct {
	mu    sync.Mutex
	byTab map[string]previewEntry
}

// rememberTabPreview делает превью из последнего кадра вкладки. Вызывается при
// уходе с неё: этот кадр — ровно то, что человек видел последним.
func rememberTabPreview(targetID string, jpegData []byte) {
	if targetID == "" || len(jpegData) == 0 {
		return
	}
	small, err := shrinkJPEG(jpegData, previewWidth)
	if err != nil {
		return
	}
	previews.mu.Lock()
	defer previews.mu.Unlock()
	if previews.byTab == nil {
		previews.byTab = map[string]previewEntry{}
	}
	previews.byTab[targetID] = previewEntry{
		dataURL: "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(small),
		at:      time.Now(),
	}
	// Держим только свежие и не больше горстки: список вкладок не должен
	// превратиться в хранилище картинок.
	if len(previews.byTab) > previewLimit {
		oldest, oldestAt := "", time.Now()
		for id, entry := range previews.byTab {
			if entry.at.Before(oldestAt) {
				oldest, oldestAt = id, entry.at
			}
		}
		delete(previews.byTab, oldest)
	}
}

// tabPreview отдаёт превью вкладки (пустая строка — нет).
func tabPreview(targetID string) string {
	previews.mu.Lock()
	defer previews.mu.Unlock()
	entry, ok := previews.byTab[targetID]
	if !ok {
		return ""
	}
	if time.Since(entry.at) > previewMaxAge {
		delete(previews.byTab, targetID)
		return ""
	}
	return entry.dataURL
}

// forgetTabPreview убирает превью закрытой вкладки.
func forgetTabPreview(targetID string) {
	previews.mu.Lock()
	defer previews.mu.Unlock()
	delete(previews.byTab, targetID)
}

// shrinkJPEG уменьшает кадр до ширины width. Кадр страницы — это сотни
// килобайт; в списке вкладок такому весу места нет.
func shrinkJPEG(data []byte, width int) ([]byte, error) {
	src, err := jpeg.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	bounds := src.Bounds()
	if bounds.Dx() <= 0 || bounds.Dy() <= 0 {
		return nil, io.ErrUnexpectedEOF
	}
	if bounds.Dx() <= width {
		return data, nil
	}
	height := bounds.Dy() * width / bounds.Dx()
	dst := image.NewRGBA(image.Rect(0, 0, width, height))
	draw.ApproxBiLinear.Scale(dst, dst.Bounds(), src, bounds, draw.Over, nil)
	var out bytes.Buffer
	if err := jpeg.Encode(&out, dst, &jpeg.Options{Quality: 60}); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}
