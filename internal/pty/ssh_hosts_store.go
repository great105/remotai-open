package pty

// Пользовательские SSH-хосты и история подключений.
//
// ssh_hosts.json — хосты, добавленные руками из приложения (CRUD).
// ssh_history.json — факты успешных подключений (last_at/count) для
// сортировки «недавние сверху».
//
// Паролей в ЭТИХ файлах нет: они лежат отдельно и зашифрованными
// (web/ssh_secrets.go), а сюда попадает только то, что не жалко показать —
// адрес, логин, теги, ссылка на ключ. Так список серверов остаётся читаемым
// файлом, который можно открыть и починить руками.

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"sync"
	"time"
)

// ErrSSHHostNotFound — хоста с таким id нет в ssh_hosts.json.
var ErrSSHHostNotFound = errors.New("ssh host not found")

// SavedSSHHost — пользовательский хост из ssh_hosts.json.
type SavedSSHHost struct {
	ID             string   `json:"id"`
	Name           string   `json:"name"`
	Host           string   `json:"host"`
	Port           int      `json:"port,omitempty"` // 0 = 22
	User           string   `json:"user"`
	IdentityFile   string   `json:"identity_file,omitempty"`
	ProxyJump      string   `json:"proxy_jump,omitempty"`
	Tags           []string `json:"tags,omitempty"`
	LinkedDeviceID string   `json:"linked_device_id,omitempty"`
	// KeyID — ключ из хранилища приложения (ssh_keys.json). В отличие от
	// IdentityFile его не надо искать в файловой системе ПК, поэтому сервер
	// можно завести с телефона целиком.
	KeyID string `json:"key_id,omitempty"`
}

// SSHHostPatch — точечное обновление SavedSSHHost (PATCH): меняются только
// non-nil поля. Слайс-теги обновляются целиком (включая сброс в пустой).
type SSHHostPatch struct {
	Name           *string   `json:"name"`
	Host           *string   `json:"host"`
	Port           *int      `json:"port"`
	User           *string   `json:"user"`
	IdentityFile   *string   `json:"identity_file"`
	ProxyJump      *string   `json:"proxy_jump"`
	Tags           *[]string `json:"tags"`
	LinkedDeviceID *string   `json:"linked_device_id"`
	KeyID          *string   `json:"key_id"`
}

// SSHHostStore — потокобезопасный CRUD над ssh_hosts.json.
type SSHHostStore struct {
	mu    sync.RWMutex
	path  string
	hosts []SavedSSHHost
}

// NewSSHHostStore открывает (или создаёт при первой записи) файл хостов.
// Битый/отсутствующий файл — стор стартует пустым (как bookmarkStore).
func NewSSHHostStore(path string) *SSHHostStore {
	s := &SSHHostStore{path: path}
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, &s.hosts)
	}
	return s
}

// List возвращает копию списка (никогда не nil).
func (s *SSHHostStore) List() []SavedSSHHost {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]SavedSSHHost, len(s.hosts))
	copy(out, s.hosts)
	return out
}

// Get возвращает хост по id.
func (s *SSHHostStore) Get(id string) (SavedSSHHost, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, h := range s.hosts {
		if h.ID == id {
			return h, nil
		}
	}
	return SavedSSHHost{}, ErrSSHHostNotFound
}

// Add сохраняет новый хост, присваивая id. Персистит немедленно.
func (s *SSHHostStore) Add(h SavedSSHHost) (SavedSSHHost, error) {
	h.ID = "h-" + randomID()
	s.mu.Lock()
	s.hosts = append(s.hosts, h)
	s.mu.Unlock()
	if err := s.save(); err != nil {
		return SavedSSHHost{}, err
	}
	return h, nil
}

// Update применяет патч к хосту. Персистит немедленно.
func (s *SSHHostStore) Update(id string, p SSHHostPatch) (SavedSSHHost, error) {
	s.mu.Lock()
	idx := -1
	for i, h := range s.hosts {
		if h.ID == id {
			idx = i
			break
		}
	}
	if idx < 0 {
		s.mu.Unlock()
		return SavedSSHHost{}, ErrSSHHostNotFound
	}
	h := &s.hosts[idx]
	if p.Name != nil {
		h.Name = *p.Name
	}
	if p.Host != nil {
		h.Host = *p.Host
	}
	if p.Port != nil {
		h.Port = *p.Port
	}
	if p.User != nil {
		h.User = *p.User
	}
	if p.IdentityFile != nil {
		h.IdentityFile = *p.IdentityFile
	}
	if p.ProxyJump != nil {
		h.ProxyJump = normalizeProxyJump(*p.ProxyJump)
	}
	if p.Tags != nil {
		h.Tags = *p.Tags
	}
	if p.LinkedDeviceID != nil {
		h.LinkedDeviceID = *p.LinkedDeviceID
	}
	if p.KeyID != nil {
		h.KeyID = *p.KeyID
	}
	out := *h
	s.mu.Unlock()
	if err := s.save(); err != nil {
		return SavedSSHHost{}, err
	}
	return out, nil
}

// Delete убирает хост по id. Персистит немедленно.
func (s *SSHHostStore) Delete(id string) error {
	s.mu.Lock()
	for i, h := range s.hosts {
		if h.ID == id {
			s.hosts = append(s.hosts[:i:i], s.hosts[i+1:]...)
			s.mu.Unlock()
			return s.save()
		}
	}
	s.mu.Unlock()
	return ErrSSHHostNotFound
}

func (s *SSHHostStore) save() error {
	s.mu.RLock()
	b, err := json.MarshalIndent(s.hosts, "", "  ")
	s.mu.RUnlock()
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// ── История подключений ────────────────────────────────────────────

// sshHistoryMax — потолок записей; дальше вытесняем самые старые по last_at.
const sshHistoryMax = 200

// SSHHistoryEntry — факт успешного подключения к user@host:port.
type SSHHistoryEntry struct {
	Host      string    `json:"host"`
	Port      int       `json:"port"`
	User      string    `json:"user"`
	ProxyJump string    `json:"proxy_jump,omitempty"`
	LastAt    time.Time `json:"last_at"`
	Count     int       `json:"count"`
}

// SSHHistoryKey — ключ мержа истории с хостами: "user@host:port" (порт уже
// нормализован, т.е. 22 вместо 0).
func SSHHistoryKey(user, host string, port int) string {
	return fmt.Sprintf("%s@%s:%d", user, host, port)
}

// SSHHistoryStore — потокобезопасная история в ssh_history.json.
type SSHHistoryStore struct {
	mu      sync.RWMutex
	path    string
	entries []SSHHistoryEntry
	now     func() time.Time // подменяется в тестах
}

// NewSSHHistoryStore открывает файл истории. Битый/отсутствующий — пустой стор.
func NewSSHHistoryStore(path string) *SSHHistoryStore {
	s := &SSHHistoryStore{path: path, now: time.Now}
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, &s.entries)
	}
	return s
}

// Record фиксирует успешное подключение: инкремент count и last_at=now для
// существующей записи user@host:port либо новая запись. Персистит немедленно.
func (s *SSHHistoryStore) Record(host string, port int, user, proxyJump string) error {
	if port <= 0 {
		port = 22
	}
	s.mu.Lock()
	for i, e := range s.entries {
		if e.Host == host && e.Port == port && e.User == user {
			s.entries[i].Count++
			s.entries[i].LastAt = s.now()
			s.entries[i].ProxyJump = proxyJump
			s.mu.Unlock()
			return s.save()
		}
	}
	s.entries = append(s.entries, SSHHistoryEntry{
		Host: host, Port: port, User: user, ProxyJump: proxyJump, LastAt: s.now(), Count: 1,
	})
	if len(s.entries) > sshHistoryMax {
		// Вытесняем самые старые: история — не архив, а помощник сортировки.
		sort.SliceStable(s.entries, func(i, j int) bool {
			return s.entries[i].LastAt.After(s.entries[j].LastAt)
		})
		s.entries = s.entries[:sshHistoryMax]
	}
	s.mu.Unlock()
	return s.save()
}

// List — до limit записей, свежие сверху (last_at desc).
func (s *SSHHistoryStore) List(limit int) []SSHHistoryEntry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]SSHHistoryEntry, len(s.entries))
	copy(out, s.entries)
	sort.SliceStable(out, func(i, j int) bool { return out[i].LastAt.After(out[j].LastAt) })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

// Snapshot — вся история как map[SSHHistoryKey]entry для мержа со списком хостов.
func (s *SSHHistoryStore) Snapshot() map[string]SSHHistoryEntry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]SSHHistoryEntry, len(s.entries))
	for _, e := range s.entries {
		out[SSHHistoryKey(e.User, e.Host, e.Port)] = e
	}
	return out
}

func (s *SSHHistoryStore) save() error {
	s.mu.RLock()
	b, err := json.MarshalIndent(s.entries, "", "  ")
	s.mu.RUnlock()
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}
