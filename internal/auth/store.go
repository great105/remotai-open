package auth

import (
	"encoding/json"
	"log"
	"os"
	"sync"
	"time"

	"tgcontrol/internal/atomicfile"
	"tgcontrol/internal/paths"
)

// SessionStore persists auth sessions (token families) to disk.
type SessionStore struct {
	mu       sync.RWMutex
	sessions map[string]*UserSession // family -> session
	path     string
}

// UserSession represents an authenticated session (tied to a refresh token family).
type UserSession struct {
	UID        int64     `json:"uid"`
	Family     string    `json:"family"`
	Method     string    `json:"method"` // "initdata", "oidc", "api_token"
	CreatedAt  time.Time `json:"created_at"`
	LastUsedAt time.Time `json:"last_used_at"`
	UserAgent  string    `json:"user_agent"`
	IP         string    `json:"ip"`
	Revoked    bool      `json:"revoked"`
}

// NewSessionStore creates a session store, loading from disk if available.
func NewSessionStore() *SessionStore {
	path := paths.StateFile("auth-sessions.json")

	s := &SessionStore{
		sessions: make(map[string]*UserSession),
		path:     path,
	}
	s.load()
	return s
}

// Create creates a new auth session.
func (s *SessionStore) Create(uid int64, family, method, ua, ip string) *UserSession {
	session := &UserSession{
		UID:        uid,
		Family:     family,
		Method:     method,
		CreatedAt:  time.Now(),
		LastUsedAt: time.Now(),
		UserAgent:  ua,
		IP:         ip,
	}

	s.mu.Lock()
	s.sessions[family] = session
	s.mu.Unlock()

	s.save()
	return session
}

// Get returns a session by family ID.
func (s *SessionStore) Get(family string) *UserSession {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.sessions[family]
}

// Touch updates the last-used timestamp and persists it, so Cleanup() (which
// evicts by LastUsedAt) doesn't drop an actively-used session after a restart.
func (s *SessionStore) Touch(family string) {
	s.mu.Lock()
	sess, ok := s.sessions[family]
	if ok {
		sess.LastUsedAt = time.Now()
	}
	s.mu.Unlock()
	if ok {
		s.save()
	}
}

// RevokeAll revokes all sessions for a user.
func (s *SessionStore) RevokeAll(uid int64) {
	s.mu.Lock()
	for _, sess := range s.sessions {
		if sess.UID == uid {
			sess.Revoked = true
		}
	}
	s.mu.Unlock()
	s.save()
}

// ListActive returns all non-revoked sessions for a user.
func (s *SessionStore) ListActive(uid int64) []*UserSession {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var result []*UserSession
	for _, sess := range s.sessions {
		if sess.UID == uid && !sess.Revoked {
			result = append(result, sess)
		}
	}
	return result
}

// Cleanup removes expired and revoked sessions.
func (s *SessionStore) Cleanup() {
	s.mu.Lock()
	cutoff := time.Now().Add(-RefreshTokenTTL - 24*time.Hour) // grace period
	for family, sess := range s.sessions {
		if sess.Revoked || sess.LastUsedAt.Before(cutoff) {
			delete(s.sessions, family)
		}
	}
	s.mu.Unlock()
	s.save()
}

func (s *SessionStore) load() {
	data, err := os.ReadFile(s.path)
	if err != nil {
		return
	}
	var sessions map[string]*UserSession
	if err := json.Unmarshal(data, &sessions); err != nil {
		log.Printf("[AUTH] %s is corrupt (%v) — quarantining", s.path, err)
		_, _ = atomicfile.Quarantine(s.path)
		return
	}
	s.sessions = sessions
}

func (s *SessionStore) save() {
	s.mu.RLock()
	data, err := json.MarshalIndent(s.sessions, "", "  ")
	s.mu.RUnlock()
	if err != nil {
		return
	}
	if err := atomicfile.WriteFile(s.path, data, 0600); err != nil {
		log.Printf("[AUTH] save failed: %v", err)
	}
}
