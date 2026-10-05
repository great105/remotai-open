package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Preset is a quick-launch tile shown on the dashboard.
type Preset struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	Icon      string `json:"icon,omitempty"`
	Kind      string `json:"kind"` // "session" | "pty" | "url"
	Agent     string `json:"agent,omitempty"`
	CWD       string `json:"cwd,omitempty"`
	Shell     string `json:"shell,omitempty"`
	URL       string `json:"url,omitempty"`
	Pinned    bool   `json:"pinned"`
	LastUsed  int64  `json:"last_used,omitempty"`
	UsedCount int    `json:"used_count,omitempty"`
}

type presetsFile struct {
	Presets []Preset `json:"presets"`
}

var (
	presetsMu sync.Mutex
)

func presetsPath() string {
	home := realUserHome()
	return filepath.Join(home, ".tgcontrol-presets.json")
}

func loadPresets() []Preset {
	data, err := os.ReadFile(presetsPath())
	if err != nil {
		return nil
	}
	var f presetsFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil
	}
	return f.Presets
}

func savePresets(list []Preset) error {
	data, err := json.MarshalIndent(presetsFile{Presets: list}, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(presetsPath(), data, 0644)
}

// GET /api/presets — list. Returns built-in suggestions if user has none yet.
func (s *Server) apiPresetsList(w http.ResponseWriter, r *http.Request, uid int64) {
	presetsMu.Lock()
	list := loadPresets()
	presetsMu.Unlock()
	if list == nil {
		list = builtinPresets()
	}
	// Sort: pinned first, then by recent use desc, then alpha.
	sort.SliceStable(list, func(i, j int) bool {
		if list[i].Pinned != list[j].Pinned {
			return list[i].Pinned
		}
		if list[i].LastUsed != list[j].LastUsed {
			return list[i].LastUsed > list[j].LastUsed
		}
		return strings.ToLower(list[i].Title) < strings.ToLower(list[j].Title)
	})
	jsonResp(w, map[string]any{"presets": list})
}

// POST /api/presets — create or upsert.
func (s *Server) apiPresetsUpsert(w http.ResponseWriter, r *http.Request, uid int64) {
	var p Preset
	if err := readJSON(r, &p); err != nil {
		jsonError(w, "Bad JSON", 400)
		return
	}
	if p.Title == "" || p.Kind == "" {
		jsonError(w, "title and kind are required", 400)
		return
	}
	presetsMu.Lock()
	defer presetsMu.Unlock()
	list := loadPresets()
	if p.ID == "" {
		p.ID = fmt.Sprintf("p-%d", time.Now().UnixNano())
		list = append(list, p)
	} else {
		found := false
		for i := range list {
			if list[i].ID == p.ID {
				// Preserve usage stats unless explicitly set
				if p.LastUsed == 0 {
					p.LastUsed = list[i].LastUsed
				}
				if p.UsedCount == 0 {
					p.UsedCount = list[i].UsedCount
				}
				list[i] = p
				found = true
				break
			}
		}
		if !found {
			list = append(list, p)
		}
	}
	if err := savePresets(list); err != nil {
		jsonError(w, "Save failed: "+err.Error(), 500)
		return
	}
	jsonResp(w, map[string]any{"preset": p, "ok": true})
}

// DELETE /api/presets?id=...
func (s *Server) apiPresetsDelete(w http.ResponseWriter, r *http.Request, uid int64) {
	id := r.URL.Query().Get("id")
	if id == "" {
		jsonError(w, "id required", 400)
		return
	}
	presetsMu.Lock()
	defer presetsMu.Unlock()
	list := loadPresets()
	out := list[:0]
	for _, p := range list {
		if p.ID != id {
			out = append(out, p)
		}
	}
	if err := savePresets(out); err != nil {
		jsonError(w, "Save failed: "+err.Error(), 500)
		return
	}
	jsonResp(w, map[string]bool{"ok": true})
}

// POST /api/presets/launch?id=... — bumps usage stats; returns the preset so
// the client can dispatch it (create session / open PTY / open URL).
func (s *Server) apiPresetLaunch(w http.ResponseWriter, r *http.Request, uid int64) {
	id := r.URL.Query().Get("id")
	if id == "" {
		jsonError(w, "id required", 400)
		return
	}
	presetsMu.Lock()
	defer presetsMu.Unlock()
	list := loadPresets()
	var found *Preset
	for i := range list {
		if list[i].ID == id {
			list[i].LastUsed = time.Now().Unix()
			list[i].UsedCount++
			found = &list[i]
			break
		}
	}
	if found == nil {
		jsonError(w, "preset not found", 404)
		return
	}
	if err := savePresets(list); err != nil {
		// Non-fatal: still let the client dispatch
		fmt.Println("[presets] save failed:", err)
	}
	jsonResp(w, map[string]any{"preset": found, "ok": true})
}

// builtinPresets returns a curated starter set so a new user sees something
// useful on the Dashboard before they create any of their own.
//
// Чего здесь БОЛЬШЕ НЕТ и почему. Стартовый набор дублировал сам себя и
// навигацию: плитка шелла («PowerShell») стояла вплотную под главной кнопкой
// «+ Новый терминал» и делала почти то же самое — но молча, в домашней папке,
// тогда как кнопка спрашивает проект. Человек видел два соседних способа
// «запустить терминал» без видимой разницы и половину времени попадал не туда.
// Плитки «Рабочий стол» и «Файлы» вели на /remote и /files — разделы, у которых
// на телефоне есть постоянная вкладка внизу экрана (клиент их и так отфильтровывал
// как дубли навигации, PresetsTiles.NAV_URLS).
//
// Остаются только AI-пресеты: они запускают то, чего одной кнопкой не сделать —
// конкретного агента в конкретной папке. Свои плитки пользователя (~/.tgcontrol-
// presets.json) сюда не попадают вовсе и продолжают работать как раньше.
func builtinPresets() []Preset {
	home := realUserHome()
	list := make([]Preset, 0, 2)
	// A starter tile must be executable on this machine. Advertising missing
	// agents as installed turns the first click into a raw execve failure.
	if _, err := exec.LookPath("claude"); err == nil {
		list = append(list, Preset{ID: "builtin-claude", Title: "Claude в проекте", Icon: "🚀", Kind: "session", Agent: "claude", CWD: home, Pinned: true})
	}
	if _, err := exec.LookPath("codex"); err == nil {
		list = append(list, Preset{ID: "builtin-codex", Title: "Codex", Icon: "🤖", Kind: "session", Agent: "codex", CWD: home, Pinned: true})
	}
	return list
}
