package sessions

import (
	"encoding/json"
	"log"
	"os"
	"sync"

	"tgcontrol/internal/atomicfile"
	"tgcontrol/internal/paths"
)

// Message represents a chat message (user or agent).
type Message struct {
	Role      string   `json:"role"`
	Text      string   `json:"text"`
	Timestamp float64  `json:"timestamp"`
	CostUSD   float64  `json:"cost_usd,omitempty"`
	IsError   bool     `json:"is_error,omitempty"`
	Tools     []string `json:"tools,omitempty"`
}

// History stores per-session chat messages with JSON persistence.
type History struct {
	mu   sync.RWMutex
	data map[int]map[string][]Message // uid -> session_name -> messages
	path string
}

// NewHistory creates a History backed by JSON file.
func NewHistory() *History {
	h := &History{
		path: paths.StateFile("history.json"),
		data: make(map[int]map[string][]Message),
	}
	h.load()
	return h
}

func (h *History) load() {
	data, err := os.ReadFile(h.path)
	if err != nil {
		return
	}
	if err := json.Unmarshal(data, &h.data); err != nil {
		log.Printf("[HISTORY] %s is corrupt (%v) — quarantining", h.path, err)
		_, _ = atomicfile.Quarantine(h.path)
		h.data = make(map[int]map[string][]Message)
	}
}

func (h *History) save() {
	data, err := json.Marshal(h.data)
	if err != nil {
		return
	}
	if err := atomicfile.WriteFile(h.path, data, 0644); err != nil {
		log.Printf("[HISTORY] save failed: %v", err)
	}
}

// Add appends a message to a session's history.
func (h *History) Add(uid int, session string, msg Message) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.data[uid] == nil {
		h.data[uid] = make(map[string][]Message)
	}
	h.data[uid][session] = append(h.data[uid][session], msg)

	// Keep last 200 messages per session
	if len(h.data[uid][session]) > 200 {
		h.data[uid][session] = h.data[uid][session][len(h.data[uid][session])-200:]
	}
	h.save()
}

// Get returns messages for a session.
func (h *History) Get(uid int, session string) []Message {
	h.mu.RLock()
	defer h.mu.RUnlock()

	if h.data[uid] == nil {
		return []Message{}
	}
	msgs := h.data[uid][session]
	if msgs == nil {
		return []Message{}
	}
	// Return a copy: callers iterate/serialize this after the lock is released
	// while Add() may append/reslice the same backing array (data race).
	out := make([]Message, len(msgs))
	copy(out, msgs)
	return out
}

// Clear removes all messages for a session.
func (h *History) Clear(uid int, session string) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.data[uid] != nil {
		delete(h.data[uid], session)
		h.save()
	}
}

// GetSessionCost returns total accumulated cost for a session.
func (h *History) GetSessionCost(uid int, session string) float64 {
	h.mu.RLock()
	defer h.mu.RUnlock()

	total := 0.0
	for _, msg := range h.data[uid][session] {
		total += msg.CostUSD
	}
	return total
}

// GetLastUserMessage returns the text of the last user message.
func (h *History) GetLastUserMessage(uid int, session string) string {
	h.mu.RLock()
	defer h.mu.RUnlock()

	msgs := h.data[uid][session]
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "user" {
			return msgs[i].Text
		}
	}
	return ""
}

// MessageCount returns the number of messages in a session.
func (h *History) MessageCount(uid int, session string) int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.data[uid][session])
}

// Rename moves history from old session name to new.
func (h *History) Rename(uid int, oldName, newName string) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.data[uid] != nil {
		h.data[uid][newName] = h.data[uid][oldName]
		delete(h.data[uid], oldName)
		h.save()
	}
}
