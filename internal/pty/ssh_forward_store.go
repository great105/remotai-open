package pty

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"sort"
	"sync"
)

// SavedForwardSpec is the recoverable, secret-free part of an SSH forward.
// Active listeners still live only in memory; after an agent restart the UI
// can offer these specifications for explicit restoration.
type SavedForwardSpec struct {
	ID           string `json:"id"`
	HostID       string `json:"host_id,omitempty"`
	Host         string `json:"host"`
	Port         int    `json:"port,omitempty"`
	User         string `json:"user"`
	IdentityFile string `json:"identity_file,omitempty"`
	ProxyJump    string `json:"proxy_jump,omitempty"`
	Type         string `json:"type"`
	BindAddr     string `json:"bind_addr"`
	BindPort     int    `json:"bind_port"`
	TargetHost   string `json:"target_host,omitempty"`
	TargetPort   int    `json:"target_port,omitempty"`
	AllowLAN     bool   `json:"allow_lan,omitempty"`
}

type SSHForwardSpecStore struct {
	mu    sync.RWMutex
	path  string
	specs []SavedForwardSpec
}

func NewSSHForwardSpecStore(path string) *SSHForwardSpecStore {
	s := &SSHForwardSpecStore{path: path}
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, &s.specs)
	}
	return s
}

func (s *SSHForwardSpecStore) List() []SavedForwardSpec {
	s.mu.RLock()
	out := append([]SavedForwardSpec(nil), s.specs...)
	s.mu.RUnlock()
	sort.SliceStable(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (s *SSHForwardSpecStore) Upsert(spec SavedForwardSpec) (SavedForwardSpec, error) {
	spec.ID = forwardSpecID(spec)
	s.mu.Lock()
	found := false
	for i := range s.specs {
		if s.specs[i].ID == spec.ID {
			s.specs[i] = spec
			found = true
			break
		}
	}
	if !found {
		s.specs = append(s.specs, spec)
	}
	data, err := json.MarshalIndent(s.specs, "", "  ")
	s.mu.Unlock()
	if err != nil {
		return SavedForwardSpec{}, err
	}
	if err := writePrivateJSON(s.path, data); err != nil {
		return SavedForwardSpec{}, err
	}
	return spec, nil
}

func (s *SSHForwardSpecStore) Delete(id string) error {
	s.mu.Lock()
	for i := range s.specs {
		if s.specs[i].ID == id {
			s.specs = append(s.specs[:i:i], s.specs[i+1:]...)
			data, err := json.MarshalIndent(s.specs, "", "  ")
			s.mu.Unlock()
			if err != nil {
				return err
			}
			return writePrivateJSON(s.path, data)
		}
	}
	s.mu.Unlock()
	return ErrForwardNotFound
}

func forwardSpecID(spec SavedForwardSpec) string {
	spec.ID = ""
	data, _ := json.Marshal(spec)
	sum := sha256.Sum256(data)
	return "fws-" + hex.EncodeToString(sum[:8])
}

func writePrivateJSON(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
