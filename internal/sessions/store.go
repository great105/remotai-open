// Package sessions manages named sessions with JSON persistence.
package sessions

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"sync"
	"time"

	"tgcontrol/internal/atomicfile"
	"tgcontrol/internal/config"
	"tgcontrol/internal/paths"
)

// cloneSession returns a deep copy so callers can read fields without holding
// the store lock while another goroutine mutates the stored session (the source
// of the IsBusy / field-aliasing data races).
func cloneSession(s *Session) *Session {
	if s == nil {
		return nil
	}
	c := *s
	if s.AgentConfig != nil {
		c.AgentConfig = make(map[string]string, len(s.AgentConfig))
		for k, v := range s.AgentConfig {
			c.AgentConfig[k] = v
		}
	}
	return &c
}

// Session mode constants.
const (
	ModePersistent = "persistent"
	ModeOneshot    = "oneshot"
)

// Session status constants.
const (
	StatusAlive    = "alive"
	StatusDead     = "dead"
	StatusNotReady = "not-ready"
)

// Permission mode constants.
const (
	PermApproveAll   = "approve-all"
	PermApproveReads = "approve-reads"
	PermDenyAll      = "deny-all"
)

var (
	ErrSessionBusy     = errors.New("session is busy")
	ErrConcurrentLimit = errors.New("max concurrent session limit reached")
)

// Session represents an active agent session.
type Session struct {
	Name           string            `json:"name"`
	AgentType      string            `json:"agent_type"`
	Cwd            string            `json:"cwd"`
	AgentSessionID string            `json:"agent_session_id,omitempty"`
	AgentConfig    map[string]string `json:"agent_config,omitempty"`

	// Session management (ACP-inspired)
	Mode           string  `json:"mode,omitempty"`            // "persistent" (default) or "oneshot"
	Status         string  `json:"status,omitempty"`          // "alive", "dead", "not-ready"
	PermissionMode string  `json:"permission_mode,omitempty"` // per-session: "approve-all", "approve-reads", "deny-all"
	CreatedAt      float64 `json:"created_at,omitempty"`      // unix timestamp
	LastActiveAt   float64 `json:"last_active_at,omitempty"`  // unix timestamp
	TopicID        int     `json:"topic_id,omitempty"`        // bound Telegram topic thread ID
	TTLMinutes     int     `json:"ttl_minutes,omitempty"`     // auto-close after inactivity (0=disabled)

	// Transient (not persisted)
	IsBusy bool `json:"-"`
}

// storeData is the JSON structure on disk.
type storeData struct {
	Sessions map[int]map[string]*Session `json:"sessions"` // uid -> name -> session
	Active   map[int]string              `json:"active"`   // uid -> active session name
	Previous map[int]string              `json:"previous"` // uid -> previous session name
}

// Store manages sessions with thread-safe access and JSON persistence.
type Store struct {
	mu   sync.RWMutex
	data storeData
	path string
}

// NewStore creates a Store backed by a JSON file in the app data dir.
func NewStore() *Store {
	s := &Store{
		path: paths.StateFile("sessions.json"),
		data: storeData{
			Sessions: make(map[int]map[string]*Session),
			Active:   make(map[int]string),
			Previous: make(map[int]string),
		},
	}
	s.load()
	return s
}

func (s *Store) load() {
	data, err := os.ReadFile(s.path)
	if err != nil {
		return
	}
	var d storeData
	if err := json.Unmarshal(data, &d); err != nil {
		// Don't silently start empty over a corrupt file — quarantine it so the
		// bad copy survives for inspection and isn't overwritten by the next save.
		log.Printf("[SESSIONS] %s is corrupt (%v) — quarantining", s.path, err)
		if bad, qerr := atomicfile.Quarantine(s.path); qerr == nil {
			log.Printf("[SESSIONS] moved corrupt file to %s", bad)
		}
		return
	}
	if d.Sessions == nil {
		d.Sessions = make(map[int]map[string]*Session)
	}
	if d.Active == nil {
		d.Active = make(map[int]string)
	}
	if d.Previous == nil {
		d.Previous = make(map[int]string)
	}
	s.data = d
}

func (s *Store) save() {
	data, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return
	}
	if err := atomicfile.WriteFile(s.path, data, 0644); err != nil {
		log.Printf("[SESSIONS] save failed: %v", err)
	}
}

// Create creates a new session.
func (s *Store) Create(uid int, name, agentType, cwd string) (*Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.data.Sessions[uid] == nil {
		s.data.Sessions[uid] = make(map[string]*Session)
	}
	if _, exists := s.data.Sessions[uid][name]; exists {
		return nil, fmt.Errorf("session %q already exists", name)
	}

	now := float64(time.Now().UnixMilli()) / 1000
	defaultTTL := config.Get().DefaultTTLMinutes
	sess := &Session{
		Name:         name,
		AgentType:    agentType,
		Cwd:          cwd,
		AgentConfig:  make(map[string]string),
		Mode:         ModePersistent,
		Status:       StatusAlive,
		CreatedAt:    now,
		LastActiveAt: now,
		TTLMinutes:   defaultTTL,
	}
	s.data.Sessions[uid][name] = sess
	s.data.Active[uid] = name
	s.save()
	return sess, nil
}

// List returns all sessions for a user as (name, session) pairs.
func (s *Store) List(uid int) []*Session {
	s.mu.RLock()
	defer s.mu.RUnlock()

	sessions := s.data.Sessions[uid]
	result := make([]*Session, 0, len(sessions))
	for _, sess := range sessions {
		result = append(result, cloneSession(sess))
	}
	return result
}

// Get returns a specific session.
func (s *Store) Get(uid int, name string) *Session {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.data.Sessions[uid] == nil {
		return nil
	}
	return cloneSession(s.data.Sessions[uid][name])
}

// GetActive returns the active session for a user.
func (s *Store) GetActive(uid int) *Session {
	s.mu.RLock()
	defer s.mu.RUnlock()

	name := s.data.Active[uid]
	if name == "" {
		return nil
	}
	if s.data.Sessions[uid] == nil {
		return nil
	}
	return cloneSession(s.data.Sessions[uid][name])
}

// GetActiveName returns the active session name.
func (s *Store) GetActiveName(uid int) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.data.Active[uid]
}

// Switch sets the active session, tracking the previous one.
func (s *Store) Switch(uid int, name string) (*Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.data.Sessions[uid] == nil || s.data.Sessions[uid][name] == nil {
		return nil, fmt.Errorf("session %q not found", name)
	}
	prev := s.data.Active[uid]
	if prev != "" && prev != name {
		s.data.Previous[uid] = prev
	}
	s.data.Active[uid] = name
	s.save()
	return cloneSession(s.data.Sessions[uid][name]), nil
}

// GetPreviousName returns the previously active session name.
func (s *Store) GetPreviousName(uid int) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	prev := s.data.Previous[uid]
	// Only return if the session still exists
	if prev != "" && s.data.Sessions[uid] != nil && s.data.Sessions[uid][prev] != nil {
		return prev
	}
	return ""
}

// Clone duplicates a session with a new name.
func (s *Store) Clone(uid int, srcName, newName string) (*Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	sessions := s.data.Sessions[uid]
	if sessions == nil || sessions[srcName] == nil {
		return nil, fmt.Errorf("session %q not found", srcName)
	}
	if sessions[newName] != nil {
		return nil, fmt.Errorf("session %q already exists", newName)
	}
	src := sessions[srcName]
	clone := &Session{
		Name:        newName,
		AgentType:   src.AgentType,
		Cwd:         src.Cwd,
		AgentConfig: make(map[string]string),
	}
	for k, v := range src.AgentConfig {
		clone.AgentConfig[k] = v
	}
	sessions[newName] = clone
	s.save()
	return cloneSession(clone), nil
}

// SetAgent changes the agent type of a session and resets its session ID.
func (s *Store) SetAgent(uid int, name, agentType string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	sess := s.data.Sessions[uid][name]
	if sess == nil {
		return fmt.Errorf("session %q not found", name)
	}
	sess.AgentType = agentType
	sess.AgentSessionID = "" // reset context
	s.save()
	return nil
}

// Close removes a session.
func (s *Store) Close(uid int, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.data.Sessions[uid] == nil || s.data.Sessions[uid][name] == nil {
		return fmt.Errorf("session %q not found", name)
	}
	delete(s.data.Sessions[uid], name)
	if s.data.Active[uid] == name {
		s.data.Active[uid] = ""
		// Switch to another session if available
		for n := range s.data.Sessions[uid] {
			s.data.Active[uid] = n
			break
		}
	}
	s.save()
	return nil
}

// Rename renames a session.
func (s *Store) Rename(uid int, oldName, newName string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	sessions := s.data.Sessions[uid]
	if sessions == nil || sessions[oldName] == nil {
		return fmt.Errorf("session %q not found", oldName)
	}
	if sessions[newName] != nil {
		return fmt.Errorf("session %q already exists", newName)
	}
	sess := sessions[oldName]
	sess.Name = newName
	sessions[newName] = sess
	delete(sessions, oldName)
	if s.data.Active[uid] == oldName {
		s.data.Active[uid] = newName
	}
	s.save()
	return nil
}

// SetCwd changes the working directory of a session.
func (s *Store) SetCwd(uid int, name, cwd string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	sess := s.data.Sessions[uid][name]
	if sess == nil {
		return fmt.Errorf("session %q not found", name)
	}
	sess.Cwd = cwd
	s.save()
	return nil
}

// SetBusy sets the busy state (transient, not persisted).
func (s *Store) SetBusy(uid int, name string, busy bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if sess := s.data.Sessions[uid][name]; sess != nil {
		sess.IsBusy = busy
	}
}

// StartRun atomically marks a session busy after checking the per-user
// concurrent session limit.
func (s *Store) StartRun(uid int, name string, maxConcurrent int) (*Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	sess := s.data.Sessions[uid][name]
	if sess == nil {
		return nil, fmt.Errorf("session %q not found", name)
	}
	if sess.IsBusy {
		return nil, ErrSessionBusy
	}
	if maxConcurrent > 0 {
		busy := 0
		for _, candidate := range s.data.Sessions[uid] {
			if candidate.IsBusy {
				busy++
			}
		}
		if busy >= maxConcurrent {
			return nil, ErrConcurrentLimit
		}
	}

	sess.IsBusy = true
	sess.LastActiveAt = float64(time.Now().UnixMilli()) / 1000
	s.save()
	return cloneSession(sess), nil
}

// UpdateAgentSessionID stores the agent's session ID for resume.
func (s *Store) UpdateAgentSessionID(uid int, name, sessionID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	sess := s.data.Sessions[uid][name]
	if sess == nil {
		return fmt.Errorf("session %q not found", name)
	}
	sess.AgentSessionID = sessionID
	s.save()
	return nil
}

// UpdateConfig merges new config values into a session's agent_config.
func (s *Store) UpdateConfig(uid int, name string, updates map[string]string) (*Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	sess := s.data.Sessions[uid][name]
	if sess == nil {
		return nil, fmt.Errorf("session %q not found", name)
	}
	if sess.AgentConfig == nil {
		sess.AgentConfig = make(map[string]string)
	}
	for k, v := range updates {
		sess.AgentConfig[k] = v
	}
	s.save()
	return cloneSession(sess), nil
}

// ── ACP-inspired session management ──────────────────────────────────

// Touch updates the LastActiveAt timestamp.
func (s *Store) Touch(uid int, name string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if sess := s.data.Sessions[uid][name]; sess != nil {
		sess.LastActiveAt = float64(time.Now().UnixMilli()) / 1000
		s.save()
	}
}

// SetStatus sets the session status (alive, dead, not-ready).
func (s *Store) SetStatus(uid int, name, status string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if sess := s.data.Sessions[uid][name]; sess != nil {
		sess.Status = status
		s.save()
	}
}

// SetMode sets the session mode (persistent or oneshot).
func (s *Store) SetMode(uid int, name, mode string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if sess := s.data.Sessions[uid][name]; sess != nil {
		sess.Mode = mode
		s.save()
	}
}

// SetPermissionMode sets per-session permission mode.
func (s *Store) SetPermissionMode(uid int, name, mode string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if sess := s.data.Sessions[uid][name]; sess != nil {
		sess.PermissionMode = mode
		s.save()
	}
}

// SetTopicID binds a session to a Telegram topic.
func (s *Store) SetTopicID(uid int, name string, topicID int) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if sess := s.data.Sessions[uid][name]; sess != nil {
		sess.TopicID = topicID
		s.save()
	}
}

// CleanExpired closes sessions that exceeded their TTL. Returns closed names.
func (s *Store) CleanExpired() []string {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := float64(time.Now().UnixMilli()) / 1000
	var closed []string

	for uid, sessions := range s.data.Sessions {
		for name, sess := range sessions {
			if sess.TTLMinutes <= 0 || sess.IsBusy {
				continue
			}
			deadline := sess.LastActiveAt + float64(sess.TTLMinutes)*60
			if now > deadline {
				closed = append(closed, fmt.Sprintf("%d:%s", uid, name))
				delete(sessions, name)
				// Don't auto-pick a random replacement (non-deterministic, jumps
				// the user to an unrelated session). Prefer the previous session
				// if it still exists, else clear the active slot.
				if s.data.Active[uid] == name {
					prev := s.data.Previous[uid]
					if prev != "" && prev != name && sessions[prev] != nil {
						s.data.Active[uid] = prev
					} else {
						s.data.Active[uid] = ""
					}
				}
			}
		}
	}

	if len(closed) > 0 {
		s.save()
	}
	return closed
}
