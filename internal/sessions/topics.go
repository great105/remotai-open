package sessions

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"sync"

	"tgcontrol/internal/atomicfile"
	"tgcontrol/internal/paths"
)

// TopicBinding describes a Telegram topic → agent session binding.
type TopicBinding struct {
	AgentType      string `json:"agent_type"`
	SessionName    string `json:"session_name"`
	Cwd            string `json:"cwd"`
	Persistent     bool   `json:"persistent"`
	PermissionMode string `json:"permission_mode,omitempty"`
}

type topicStoreData struct {
	Bindings map[string]*TopicBinding `json:"bindings"` // "chatID:topicID" -> binding
}

// TopicStore manages topic-to-agent bindings with JSON persistence.
type TopicStore struct {
	mu   sync.RWMutex
	data topicStoreData
	path string
}

// NewTopicStore creates a TopicStore backed by a JSON file.
func NewTopicStore() *TopicStore {
	ts := &TopicStore{
		path: paths.StateFile("topics.json"),
		data: topicStoreData{
			Bindings: make(map[string]*TopicBinding),
		},
	}
	ts.load()
	return ts
}

func (ts *TopicStore) load() {
	data, err := os.ReadFile(ts.path)
	if err != nil {
		return
	}
	var d topicStoreData
	if err := json.Unmarshal(data, &d); err != nil {
		log.Printf("[TOPICS] %s is corrupt (%v) — quarantining", ts.path, err)
		_, _ = atomicfile.Quarantine(ts.path)
		return
	}
	if d.Bindings == nil {
		d.Bindings = make(map[string]*TopicBinding)
	}
	ts.data = d
}

func (ts *TopicStore) save() {
	data, err := json.MarshalIndent(ts.data, "", "  ")
	if err != nil {
		return
	}
	if err := atomicfile.WriteFile(ts.path, data, 0644); err != nil {
		log.Printf("[TOPICS] save failed: %v", err)
	}
}

// TopicKey builds the lookup key for a topic binding.
func TopicKey(chatID int64, threadID int) string {
	return fmt.Sprintf("%d:%d", chatID, threadID)
}

// Bind creates or updates a topic binding.
func (ts *TopicStore) Bind(key string, binding *TopicBinding) {
	ts.mu.Lock()
	defer ts.mu.Unlock()

	ts.data.Bindings[key] = binding
	ts.save()
}

// Unbind removes a topic binding.
func (ts *TopicStore) Unbind(key string) bool {
	ts.mu.Lock()
	defer ts.mu.Unlock()

	if _, ok := ts.data.Bindings[key]; !ok {
		return false
	}
	delete(ts.data.Bindings, key)
	ts.save()
	return true
}

// Get returns the binding for a topic key, or nil.
func (ts *TopicStore) Get(key string) *TopicBinding {
	ts.mu.RLock()
	defer ts.mu.RUnlock()

	return ts.data.Bindings[key]
}

// List returns all bindings.
func (ts *TopicStore) List() map[string]*TopicBinding {
	ts.mu.RLock()
	defer ts.mu.RUnlock()

	result := make(map[string]*TopicBinding, len(ts.data.Bindings))
	for k, v := range ts.data.Bindings {
		result[k] = v
	}
	return result
}
