package web

// Пароли SSH-серверов: пароль сервера, пароль приватного ключа и пароль
// бастиона.
//
// До 2.41.0 они жили ТОЛЬКО в памяти агента, и «запомнить» означало «до
// первого перезапуска Remotai»: после перезагрузки компьютера человек вводил
// пароль каждого сервера заново. Владелец это правило отменил — раздел
// серверов работает как обычный SSH-клиент и помнит доступы.
//
// Секреты по-прежнему НЕ покидают ПК: они не отдаются клиенту ни одним
// эндпоинтом (наружу уходит только флаг unlocked), не попадают в логи и не
// синхронизируются через облако. На диске лежит контейнер secretbox —
// на Windows это DPAPI, то есть учётная запись Windows этого компьютера.
//
// Память и диск не одно и то же: секрет, сохранённый без persist, живёт до
// перезапуска, как раньше. Так остаётся способ подключиться, ничего не
// оставляя на машине.

import (
	"encoding/json"
	"log"
	"sync"

	"tgcontrol/internal/secretbox"
)

type sshSecret struct {
	password      string
	keyPassphrase string
	proxyPassword string
	// persist — секрет записан в vault и переживёт перезапуск агента.
	persist bool
}

// sshSecretJSON — форма записи в vault. Отдельный тип от sshSecret: поля
// последнего не экспортируются намеренно, чтобы секрет нельзя было случайно
// сериализовать в ответ API вместе с каким-нибудь хостом.
type sshSecretJSON struct {
	Password      string `json:"password,omitempty"`
	KeyPassphrase string `json:"key_passphrase,omitempty"`
	ProxyPassword string `json:"proxy_password,omitempty"`
}

type sshSecretVault struct {
	Version int                      `json:"version"`
	Hosts   map[string]sshSecretJSON `json:"hosts"`
}

type sshSecretStore struct {
	mu   sync.RWMutex
	path string
	data map[string]sshSecret
	// foreign — файл на диске цел, но зашифрован другой учётной записью
	// Windows (или перенесён с другой машины). Пароли не потеряны, просто
	// недоступны отсюда; интерфейс обязан сказать это словами, а не
	// притворяться, что сервер никогда не запоминали.
	foreign bool
}

// newSSHSecretStore поднимает хранилище и читает vault, если он есть.
// Ошибка чтения не мешает агенту стартовать: без сохранённых паролей продукт
// работает ровно как раньше — спрашивает их при подключении.
func newSSHSecretStore(path string) *sshSecretStore {
	s := &sshSecretStore{path: path, data: make(map[string]sshSecret)}
	if path == "" {
		return s
	}
	blob, err := secretbox.ReadFile(path)
	if err != nil {
		if err == secretbox.ErrForeign {
			s.foreign = true
			log.Printf("[SSH] saved passwords belong to another Windows account; asking for them again")
		} else {
			log.Printf("[SSH] cannot read saved passwords: %v", err)
		}
		return s
	}
	if len(blob) == 0 {
		return s
	}
	var vault sshSecretVault
	if err := json.Unmarshal(blob, &vault); err != nil {
		log.Printf("[SSH] saved passwords are unreadable: %v", err)
		return s
	}
	for id, rec := range vault.Hosts {
		s.data[id] = sshSecret{
			password:      rec.Password,
			keyPassphrase: rec.KeyPassphrase,
			proxyPassword: rec.ProxyPassword,
			persist:       true,
		}
	}
	return s
}

func (s *sshSecretStore) set(id string, secret sshSecret) {
	s.mu.Lock()
	s.data[id] = secret
	s.mu.Unlock()
	s.save()
}

func (s *sshSecretStore) get(id string) (sshSecret, bool) {
	s.mu.RLock()
	secret, ok := s.data[id]
	s.mu.RUnlock()
	return secret, ok
}

func (s *sshSecretStore) forget(id string) bool {
	s.mu.Lock()
	_, ok := s.data[id]
	delete(s.data, id)
	s.mu.Unlock()
	s.save()
	return ok
}

// foreignVault сообщает, что сохранённые пароли принадлежат другой учётной
// записи этого компьютера.
func (s *sshSecretStore) foreignVault() bool {
	if s == nil {
		return false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.foreign
}

// save переписывает vault целиком: записей единицы, а частичное обновление
// зашифрованного файла стоило бы формата с версионированием ради ничего.
// Секреты без persist в файл не попадают — это и есть «только до перезапуска».
func (s *sshSecretStore) save() {
	if s.path == "" {
		return
	}
	s.mu.RLock()
	vault := sshSecretVault{Version: 1, Hosts: make(map[string]sshSecretJSON, len(s.data))}
	for id, sec := range s.data {
		if !sec.persist {
			continue
		}
		vault.Hosts[id] = sshSecretJSON{
			Password:      sec.password,
			KeyPassphrase: sec.keyPassphrase,
			ProxyPassword: sec.proxyPassword,
		}
	}
	s.mu.RUnlock()

	blob, err := json.Marshal(vault)
	if err != nil {
		log.Printf("[SSH] cannot serialize saved passwords: %v", err)
		return
	}
	if err := secretbox.WriteFile(s.path, blob); err != nil {
		log.Printf("[SSH] cannot save passwords: %v", err)
		return
	}
	// Запись прошла — значит с этой учётной записи vault снова наш.
	s.mu.Lock()
	s.foreign = false
	s.mu.Unlock()
}

func (s *Server) sshSecretFor(id string) (sshSecret, bool) {
	if s == nil || s.sshSecrets == nil || id == "" {
		return sshSecret{}, false
	}
	return s.sshSecrets.get(id)
}
