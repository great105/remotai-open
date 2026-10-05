package agentupdate

import (
	"encoding/json"
	"os"
	"sync"

	"tgcontrol/internal/atomicfile"
)

// VersionEvent — одна смена версии агента, замеченная компьютером.
//
// Пишем не «когда обновили мы», а «когда увидели другую версию»: Claude Code
// обновляется сам и без спроса (09.09.2026 — шесть установок за двадцать
// минут), и вопрос «а когда он стал 2.1.284 и не с тех ли пор всё сломалось»
// иначе не на что опереть.
type VersionEvent struct {
	At    int64  `json:"at"`   // unix ms
	From  string `json:"from"` // "" — первый замер
	To    string `json:"to"`
	Owner string `json:"owner"`
}

type agentHistory struct {
	Last   string         `json:"last"`
	Owner  string         `json:"owner"`
	Events []VersionEvent `json:"events"`
}

type historyFile struct {
	Agents map[string]*agentHistory `json:"agents"`
}

// maxEvents — сколько смен помнить на агента: на год самообновлений хватает,
// файл остаётся килобайтами.
const maxEvents = 50

// History — файл agent-versions.json.
type History struct {
	path   string
	mu     sync.Mutex
	loaded bool
	data   historyFile
}

// NewHistory — история в файле path ("" = только в памяти, для проб).
func NewHistory(path string) *History {
	return &History{path: path}
}

func (h *History) load() {
	if h.loaded {
		return
	}
	h.loaded = true
	h.data = historyFile{Agents: map[string]*agentHistory{}}
	if h.path == "" {
		return
	}
	raw, err := os.ReadFile(h.path)
	if err != nil {
		return
	}
	var f historyFile
	if json.Unmarshal(raw, &f) == nil && f.Agents != nil {
		h.data = f
	}
}

func (h *History) save() error {
	if h.path == "" {
		return nil
	}
	raw, err := json.MarshalIndent(h.data, "", "  ")
	if err != nil {
		return err
	}
	return atomicfile.WriteFile(h.path, raw, 0o600)
}

// Record запоминает замер. Возвращает true, если версия сменилась (или это
// первый замер) и событие записано.
func (h *History) Record(id, version, owner string, atMs int64) (bool, error) {
	if id == "" || version == "" {
		return false, nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.load()
	a := h.data.Agents[id]
	if a == nil {
		a = &agentHistory{}
		h.data.Agents[id] = a
	}
	if a.Last == version {
		if a.Owner != owner && owner != "" {
			a.Owner = owner
			return false, h.save()
		}
		return false, nil
	}
	a.Events = append(a.Events, VersionEvent{At: atMs, From: a.Last, To: version, Owner: owner})
	if len(a.Events) > maxEvents {
		a.Events = a.Events[len(a.Events)-maxEvents:]
	}
	a.Last, a.Owner = version, owner
	return true, h.save()
}

// Events — смены версии агента, новые первыми.
func (h *History) Events(id string) []VersionEvent {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.load()
	a := h.data.Agents[id]
	out := []VersionEvent{}
	if a == nil {
		return out
	}
	for i := len(a.Events) - 1; i >= 0; i-- {
		out = append(out, a.Events[i])
	}
	return out
}
