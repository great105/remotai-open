package server

import (
	"io"
	"net/http"
	"sync"
	"time"
)

const latestManifestURL = "https://remotai.ru/download/latest.json"

var latestManifestCache struct {
	sync.Mutex
	body    []byte
	expires time.Time
}

// GET /v1/latest proxies the public release manifest for Web/Telegram clients.
// A short cache prevents every settings screen from hitting the origin.
func (s *Server) handleLatest(w http.ResponseWriter, r *http.Request) {
	latestManifestCache.Lock()
	if len(latestManifestCache.body) > 0 && time.Now().Before(latestManifestCache.expires) {
		body := append([]byte(nil), latestManifestCache.body...)
		latestManifestCache.Unlock()
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "public, max-age=300")
		_, _ = w.Write(body)
		return
	}
	latestManifestCache.Unlock()

	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, latestManifestURL, nil)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "release manifest unavailable")
		return
	}
	client := &http.Client{Timeout: 8 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "release manifest unavailable")
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		writeErr(w, http.StatusBadGateway, "release manifest unavailable")
		return
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		writeErr(w, http.StatusBadGateway, "release manifest unavailable")
		return
	}
	latestManifestCache.Lock()
	latestManifestCache.body = append(latestManifestCache.body[:0], body...)
	latestManifestCache.expires = time.Now().Add(5 * time.Minute)
	latestManifestCache.Unlock()
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=300")
	_, _ = w.Write(body)
}
