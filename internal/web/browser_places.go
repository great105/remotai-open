package web

// Закладки и «часто открываю» виртуального браузера.
//
// Пустая вкладка сейчас показывает внутреннюю страницу Chrome — десктопную,
// чужую и бесполезную с телефона. У телефонного браузера на её месте своё:
// строка поиска и плитки сайтов, куда человек ходит. Список должен жить на
// агенте, а не на телефоне: браузер один, а пультов у него несколько (телефон,
// окно на компьютере, планшет), и закладка, сделанная с одного, обязана быть
// видна с остальных.
//
// Файл открытый (JSON рядом с остальным состоянием): здесь нет секретов —
// пароли лежат отдельно и зашифрованы (browser_vault.go).

import (
	"encoding/json"
	"log"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"tgcontrol/internal/paths"
)

// Bookmark — сохранённый сайт.
type Bookmark struct {
	URL   string `json:"url"`
	Title string `json:"title"`
	Host  string `json:"host"`
	Added int64  `json:"added"`
}

// TopSite — сайт, куда человек заходит часто (счётчик посещений).
type TopSite struct {
	Host  string `json:"host"`
	URL   string `json:"url"`
	Title string `json:"title,omitempty"`
	Count int    `json:"count"`
	Last  int64  `json:"last"`
}

// HistoryEntry — страница, на которой человек был. Отдельно от «часто
// открываю»: то — про сайты, а история нужна, чтобы найти КОНКРЕТНУЮ страницу,
// которую видел вчера и не сохранил.
type HistoryEntry struct {
	URL   string `json:"url"`
	Title string `json:"title"`
	Host  string `json:"host"`
	At    int64  `json:"at"`
}

type placesFile struct {
	Version   int            `json:"version"`
	Bookmarks []Bookmark     `json:"bookmarks"`
	Top       []TopSite      `json:"top"`
	History   []HistoryEntry `json:"history"`
}

type placesStore struct {
	mu        sync.Mutex
	path      string
	loaded    bool
	dirty     bool
	bookmarks []Bookmark
	top       []TopSite
	history   []HistoryEntry
}

var places = &placesStore{}

// maxTopSites — сколько сайтов помним. Больше на стартовом экране всё равно не
// показать, а файл незачем растить историей годичной давности.
const maxTopSites = 40

// maxHistory — сколько страниц помним. Хватает на месяцы обычного чтения, а
// файл остаётся размером с текстовую заметку.
const maxHistory = 500

func (p *placesStore) ensureLoaded() {
	if p.loaded {
		return
	}
	p.loaded = true
	p.path = paths.StateFile("browser-places.json")
	raw, err := os.ReadFile(p.path)
	if err != nil {
		return
	}
	var file placesFile
	if err := json.Unmarshal(raw, &file); err != nil {
		log.Printf("[VBROWSER] закладки не прочитаны: %v", err)
		return
	}
	p.bookmarks = file.Bookmarks
	p.top = file.Top
	p.history = file.History
}

func (p *placesStore) saveLocked() {
	data, err := json.MarshalIndent(placesFile{
		Version: 1, Bookmarks: p.bookmarks, Top: p.top, History: p.history,
	}, "", "  ")
	if err != nil {
		return
	}
	if err := os.WriteFile(p.path, data, 0o600); err != nil {
		log.Printf("[VBROWSER] закладки не сохранены: %v", err)
	}
}

// Note учитывает посещение страницы. Служебные адреса (пустая вкладка,
// настройки браузера) в список не попадают: человек их не выбирал.
func (p *placesStore) Note(pageURL, title string) {
	host := normalizeVaultHost(pageURL)
	if host == "" || !strings.HasPrefix(strings.ToLower(pageURL), "http") {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ensureLoaded()
	now := time.Now().Unix()
	p.noteHistoryLocked(pageURL, title, host, now)
	for i := range p.top {
		if p.top[i].Host == host {
			p.top[i].Count++
			p.top[i].Last = now
			if title != "" {
				p.top[i].Title = title
			}
			p.top[i].URL = pageURL
			p.saveLocked()
			return
		}
	}
	p.top = append(p.top, TopSite{Host: host, URL: pageURL, Title: title, Count: 1, Last: now})
	if len(p.top) > maxTopSites {
		sort.Slice(p.top, func(i, j int) bool { return placeRank(p.top[i]) > placeRank(p.top[j]) })
		p.top = p.top[:maxTopSites]
	}
	p.saveLocked()
}

// placeRank — насколько сайт «свой»: чаще заходят и заходили недавно. Только
// счётчик посещений оставлял бы на виду сайт, открытый сто раз год назад.
func placeRank(t TopSite) float64 {
	ageDays := float64(time.Now().Unix()-t.Last) / 86400
	if ageDays < 0 {
		ageDays = 0
	}
	return float64(t.Count) / (1 + ageDays/14)
}

// Top отдаёт самые нужные сайты (для плиток стартовой страницы).
func (p *placesStore) Top(limit int) []TopSite {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ensureLoaded()
	out := append([]TopSite(nil), p.top...)
	sort.Slice(out, func(i, j int) bool { return placeRank(out[i]) > placeRank(out[j]) })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

// Bookmarks отдаёт закладки, свежие сверху.
func (p *placesStore) Bookmarks() []Bookmark {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ensureLoaded()
	out := append([]Bookmark(nil), p.bookmarks...)
	sort.Slice(out, func(i, j int) bool { return out[i].Added > out[j].Added })
	return out
}

// AddBookmark сохраняет сайт. Повторное сохранение того же адреса обновляет
// заголовок, а не плодит двойников.
func (p *placesStore) AddBookmark(pageURL, title string) (Bookmark, error) {
	pageURL = strings.TrimSpace(pageURL)
	if pageURL == "" {
		return Bookmark{}, os.ErrInvalid
	}
	if u, err := url.Parse(pageURL); err != nil || u.Host == "" {
		return Bookmark{}, os.ErrInvalid
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ensureLoaded()
	host := normalizeVaultHost(pageURL)
	for i := range p.bookmarks {
		if p.bookmarks[i].URL == pageURL {
			if title != "" {
				p.bookmarks[i].Title = title
			}
			p.saveLocked()
			return p.bookmarks[i], nil
		}
	}
	mark := Bookmark{URL: pageURL, Title: title, Host: host, Added: time.Now().Unix()}
	p.bookmarks = append(p.bookmarks, mark)
	p.saveLocked()
	return mark, nil
}

// RemoveBookmark убирает закладку по адресу.
func (p *placesStore) RemoveBookmark(pageURL string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ensureLoaded()
	for i := range p.bookmarks {
		if p.bookmarks[i].URL == pageURL {
			p.bookmarks = append(p.bookmarks[:i], p.bookmarks[i+1:]...)
			p.saveLocked()
			return true
		}
	}
	return false
}

// noteHistoryLocked пишет страницу в историю. Вызывать под взятым замком.
//
// Повторное открытие того же адреса поднимает запись вверх, а не добавляет
// вторую: иначе перезагрузка страницы засоряла бы историю десятком одинаковых
// строк, между которыми не найти нужную.
func (p *placesStore) noteHistoryLocked(pageURL, title, host string, now int64) {
	for i := range p.history {
		if p.history[i].URL == pageURL {
			p.history[i].At = now
			if title != "" {
				p.history[i].Title = title
			}
			entry := p.history[i]
			p.history = append(p.history[:i], p.history[i+1:]...)
			p.history = append([]HistoryEntry{entry}, p.history...)
			return
		}
	}
	p.history = append([]HistoryEntry{{URL: pageURL, Title: title, Host: host, At: now}}, p.history...)
	if len(p.history) > maxHistory {
		p.history = p.history[:maxHistory]
	}
}

// History отдаёт историю, свежее сверху. query фильтрует по адресу и заголовку
// — искать в истории надо словами, а не прокруткой пятисот строк.
func (p *placesStore) History(query string, limit int) []HistoryEntry {
	needle := strings.ToLower(strings.TrimSpace(query))
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ensureLoaded()
	out := make([]HistoryEntry, 0, min(limit, len(p.history)))
	for _, entry := range p.history {
		if needle != "" {
			hay := strings.ToLower(entry.Title + " " + entry.URL)
			if !strings.Contains(hay, needle) {
				continue
			}
		}
		out = append(out, entry)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out
}

// ForgetHistory убирает одну страницу (url) или всю историю (url == "").
func (p *placesStore) ForgetHistory(pageURL string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ensureLoaded()
	if strings.TrimSpace(pageURL) == "" {
		removed := len(p.history)
		p.history = nil
		p.saveLocked()
		return removed
	}
	for i := range p.history {
		if p.history[i].URL == pageURL {
			p.history = append(p.history[:i], p.history[i+1:]...)
			p.saveLocked()
			return 1
		}
	}
	return 0
}

// ForgetSite убирает сайт из «часто открываю» — то же, что «удалить плитку» в
// телефонном браузере: попал случайно, видеть не хочу.
func (p *placesStore) ForgetSite(host string) bool {
	host = normalizeVaultHost(host)
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ensureLoaded()
	for i := range p.top {
		if p.top[i].Host == host {
			p.top = append(p.top[:i], p.top[i+1:]...)
			p.saveLocked()
			return true
		}
	}
	return false
}
