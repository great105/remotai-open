package mcpmgr

import (
	"encoding/json"
	"errors"
	"sync"
	"time"

	"tgcontrol/internal/secretbox"
)

// Disabled — выключенный сервер: определение целиком, С СЕКРЕТАМИ.
//
// Ни у Claude, ни у Codex нет команды «выключить сервер для всего аккаунта»:
// у Claude есть только выключение в конкретном проекте, у Codex — ручное
// `enabled = false` в TOML, которое его CLI не ставит. Поэтому «выключить» =
// убрать из конфига агента, а определение запомнить у нас, чтобы «включить»
// вернуло сервер тем же CLI без повторного ввода ключей.
type Disabled struct {
	Agent     string `json:"agent"`
	AccountID string `json:"account"`
	Name      string `json:"name"`
	// Def — как агент сам описал сервер: для Claude — объект из
	// `.claude.json` (его и примет `add-json`), для Codex — ответ
	// `codex mcp get --json`.
	Def        json.RawMessage `json:"def"`
	DisabledAt int64           `json:"disabled_at"`
}

// Store — выключенные серверы. Файл зашифрован secretbox (DPAPI на Windows,
// AES с ключом 0600 на остальных) и лежит с правами 0600: внутри ключи API.
type Store struct {
	Path string
	mu   sync.Mutex
}

type storeFile struct {
	Items []Disabled `json:"items"`
}

// ErrStoreForeign — файл зашифрован другой учётной записью: читать нечем, но
// и перезаписывать нельзя — там чужие ключи, которые человек потеряет.
var ErrStoreForeign = errors.New("mcp store: encrypted by another account")

func (s *Store) load() (storeFile, error) {
	var f storeFile
	plain, err := secretbox.ReadFile(s.Path)
	if err != nil {
		if errors.Is(err, secretbox.ErrForeign) {
			return f, ErrStoreForeign
		}
		return f, err
	}
	if len(plain) == 0 {
		return f, nil
	}
	if err := json.Unmarshal(plain, &f); err != nil {
		return storeFile{}, err
	}
	return f, nil
}

func (s *Store) save(f storeFile) error {
	data, err := json.Marshal(f)
	if err != nil {
		return err
	}
	return secretbox.WriteFile(s.Path, data)
}

// List — выключенные серверы агента в аккаунте.
func (s *Store) List(agent, account string) ([]Disabled, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := s.load()
	if err != nil {
		return nil, err
	}
	out := []Disabled{}
	for _, it := range f.Items {
		if it.Agent == agent && it.AccountID == account {
			out = append(out, it)
		}
	}
	return out, nil
}

// Get — одна запись или nil.
func (s *Store) Get(agent, account, name string) (*Disabled, error) {
	list, err := s.List(agent, account)
	if err != nil {
		return nil, err
	}
	for i := range list {
		if list[i].Name == name {
			return &list[i], nil
		}
	}
	return nil, nil
}

// Put кладёт запись (заменяя одноимённую).
func (s *Store) Put(d Disabled) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := s.load()
	if err != nil {
		return err
	}
	if d.DisabledAt == 0 {
		d.DisabledAt = time.Now().Unix()
	}
	items := f.Items[:0]
	for _, it := range f.Items {
		if !(it.Agent == d.Agent && it.AccountID == d.AccountID && it.Name == d.Name) {
			items = append(items, it)
		}
	}
	f.Items = append(items, d)
	return s.save(f)
}

// Delete убирает запись. Отсутствие — не ошибка.
func (s *Store) Delete(agent, account, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := s.load()
	if err != nil {
		return err
	}
	items := f.Items[:0]
	for _, it := range f.Items {
		if !(it.Agent == agent && it.AccountID == account && it.Name == name) {
			items = append(items, it)
		}
	}
	f.Items = items
	return s.save(f)
}
