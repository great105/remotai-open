package pty

// SSH-ключи, которые живут внутри Remotai.
//
// Раньше ключ можно было только НАЗВАТЬ — полем identity_file, то есть путём к
// файлу на ПК. С телефона такой путь не наберёшь и файл туда не положишь,
// поэтому вход по ключу оставался привилегией того, кто уже сидит за
// компьютером. Здесь ключ становится обычным объектом продукта: его можно
// принести готовым, сгенерировать на месте, дать имя и назначить серверу.
//
// Разделение файлов не косметическое:
//
//	ssh_keys.json — открытая часть (имя, тип, ОТПЕЧАТОК, публичный ключ). По
//	                ней рисуется список даже тогда, когда расшифровать
//	                приватные части нечем;
//	ssh_keys.enc  — приватные ключи и их пароли, контейнер secretbox (на
//	                Windows — DPAPI, то есть учётная запись этого компьютера).
//
// Благодаря этому «ключи зашифрованы другой учётной записью Windows» —
// состояние, о котором можно рассказать словами и показать, КАКИЕ именно
// ключи затронуты, вместо молчаливо опустевшего списка.

import (
	"crypto"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"tgcontrol/internal/secretbox"
)

// Ошибки хранилища ключей. Каждая соответствует машинному code на API, потому
// что все три требуют от человека РАЗНОГО действия: найти ключ, ввести пароль
// ключа, принести другой файл.
var (
	ErrSSHKeyNotFound        = errors.New("ssh key not found")
	ErrSSHKeyNeedsPassphrase = errors.New("ssh key is encrypted: passphrase required")
	ErrSSHKeyBadPassphrase   = errors.New("ssh key passphrase does not match")
	ErrSSHKeyInvalid         = errors.New("not a valid ssh private key")
	// ErrSSHKeyLocked — приватная часть на диске зашифрована другой учётной
	// записью. Ключ виден в списке, но подключиться им отсюда нельзя.
	ErrSSHKeyLocked = errors.New("ssh key belongs to another account")
)

// SavedSSHKey — открытая часть ключа. Приватного материала здесь нет и быть не
// может: этот тип уходит в ответы API.
type SavedSSHKey struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Type        string    `json:"type"`        // ed25519 | rsa | ecdsa | …
	PublicKey   string    `json:"public_key"`  // строка формата authorized_keys
	Fingerprint string    `json:"fingerprint"` // SHA256:…
	CreatedAt   time.Time `json:"created_at"`
	// Encrypted — приватный ключ защищён собственным паролем. Пароль хранится
	// рядом, в том же зашифрованном файле, поэтому подключение всё равно идёт
	// без вопросов; флаг нужен, чтобы честно назвать это в интерфейсе.
	Encrypted bool `json:"encrypted"`
	// Generated — ключ создан здесь, а не принесён. Определяет подсказку «не
	// забудьте установить его на сервер»: у принесённого ключа он, скорее
	// всего, уже стоит.
	Generated bool `json:"generated"`
}

// sshKeySecret — приватная часть (только внутри ssh_keys.enc).
type sshKeySecret struct {
	PrivatePEM string `json:"private_pem"`
	Passphrase string `json:"passphrase,omitempty"`
}

type sshKeyVault struct {
	Version int                     `json:"version"`
	Keys    map[string]sshKeySecret `json:"keys"`
}

// SSHKeyStore — потокобезопасное хранилище ключей.
type SSHKeyStore struct {
	mu        sync.RWMutex
	path      string // ssh_keys.json
	vaultPath string // ssh_keys.enc
	keys      []SavedSSHKey
	secrets   map[string]sshKeySecret
	foreign   bool
}

// NewSSHKeyStore открывает хранилище. Нечитаемая приватная часть не мешает
// старту: список ключей остаётся видимым, а подключение таким ключом даст
// ErrSSHKeyLocked с внятным текстом.
func NewSSHKeyStore(path string) *SSHKeyStore {
	s := &SSHKeyStore{
		path:      path,
		vaultPath: strings.TrimSuffix(path, filepath.Ext(path)) + ".enc",
		secrets:   make(map[string]sshKeySecret),
	}
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, &s.keys)
	}
	blob, err := secretbox.ReadFile(s.vaultPath)
	if err != nil {
		s.foreign = errors.Is(err, secretbox.ErrForeign)
		return s
	}
	if len(blob) > 0 {
		var vault sshKeyVault
		if err := json.Unmarshal(blob, &vault); err == nil {
			for id, sec := range vault.Keys {
				s.secrets[id] = sec
			}
		}
	}
	return s
}

// Foreign сообщает, что приватные части принадлежат другой учётной записи.
func (s *SSHKeyStore) Foreign() bool {
	if s == nil {
		return false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.foreign
}

// List возвращает копию списка (никогда не nil), свежие сверху.
func (s *SSHKeyStore) List() []SavedSSHKey {
	if s == nil {
		return []SavedSSHKey{}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]SavedSSHKey, len(s.keys))
	copy(out, s.keys)
	return out
}

// Get возвращает открытую часть ключа.
func (s *SSHKeyStore) Get(id string) (SavedSSHKey, error) {
	if s == nil {
		return SavedSSHKey{}, ErrSSHKeyNotFound
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, k := range s.keys {
		if k.ID == id {
			return k, nil
		}
	}
	return SavedSSHKey{}, ErrSSHKeyNotFound
}

// Material отдаёт приватный ключ и его пароль для одного хендшейка.
// Наружу (в API) он не уходит никогда — только в dialSSH.
func (s *SSHKeyStore) Material(id string) (pemData, passphrase string, err error) {
	if s == nil || id == "" {
		return "", "", ErrSSHKeyNotFound
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	sec, ok := s.secrets[id]
	if !ok {
		// Открытая часть есть, приватной нет — значит именно она и не
		// расшифровалась (иначе ключа не было бы в списке вовсе).
		for _, k := range s.keys {
			if k.ID == id {
				if s.foreign {
					return "", "", ErrSSHKeyLocked
				}
				return "", "", ErrSSHKeyNotFound
			}
		}
		return "", "", ErrSSHKeyNotFound
	}
	return sec.PrivatePEM, sec.Passphrase, nil
}

// Import принимает готовый приватный ключ (содержимое файла id_ed25519 и т.п.).
// Зашифрованный ключ без пароля не берём: смысл хранилища в том, чтобы больше
// ничего не спрашивать при подключении.
func (s *SSHKeyStore) Import(name string, pemData []byte, passphrase string) (SavedSSHKey, error) {
	signer, encrypted, err := parsePrivateKey(pemData, passphrase)
	if err != nil {
		return SavedSSHKey{}, err
	}
	key := SavedSSHKey{
		ID:          "k-" + randomID(),
		Name:        strings.TrimSpace(name),
		Type:        signer.PublicKey().Type(),
		PublicKey:   strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey()))),
		Fingerprint: ssh.FingerprintSHA256(signer.PublicKey()),
		CreatedAt:   time.Now(),
		Encrypted:   encrypted,
	}
	if key.Name == "" {
		key.Name = defaultKeyName(key.Type)
	}
	return s.add(key, sshKeySecret{PrivatePEM: string(pemData), Passphrase: passphrase})
}

// Generate создаёт новую пару прямо на ПК. keyType: "ed25519" (по умолчанию)
// или "rsa" (4096 — размер, который принимают в том числе старые серверы).
func (s *SSHKeyStore) Generate(name, keyType, passphrase string) (SavedSSHKey, error) {
	var priv crypto.PrivateKey
	switch strings.ToLower(strings.TrimSpace(keyType)) {
	case "", "ed25519":
		_, key, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return SavedSSHKey{}, err
		}
		priv = key
	case "rsa":
		key, err := rsa.GenerateKey(rand.Reader, 4096)
		if err != nil {
			return SavedSSHKey{}, err
		}
		priv = key
	default:
		return SavedSSHKey{}, fmt.Errorf("unsupported key type %q", keyType)
	}

	comment := strings.TrimSpace(name)
	if comment == "" {
		comment = "remotai"
	}
	var block *pem.Block
	var err error
	if passphrase != "" {
		block, err = ssh.MarshalPrivateKeyWithPassphrase(priv, comment, []byte(passphrase))
	} else {
		block, err = ssh.MarshalPrivateKey(priv, comment)
	}
	if err != nil {
		return SavedSSHKey{}, err
	}
	pemData := pem.EncodeToMemory(block)

	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		return SavedSSHKey{}, err
	}
	key := SavedSSHKey{
		ID:          "k-" + randomID(),
		Name:        strings.TrimSpace(name),
		Type:        signer.PublicKey().Type(),
		PublicKey:   strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey()))),
		Fingerprint: ssh.FingerprintSHA256(signer.PublicKey()),
		CreatedAt:   time.Now(),
		Encrypted:   passphrase != "",
		Generated:   true,
	}
	if key.Name == "" {
		key.Name = defaultKeyName(key.Type)
	}
	return s.add(key, sshKeySecret{PrivatePEM: string(pemData), Passphrase: passphrase})
}

// Rename меняет только имя: остальное задано самим ключом.
func (s *SSHKeyStore) Rename(id, name string) (SavedSSHKey, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return SavedSSHKey{}, errors.New("name is required")
	}
	s.mu.Lock()
	idx := -1
	for i, k := range s.keys {
		if k.ID == id {
			idx = i
			break
		}
	}
	if idx < 0 {
		s.mu.Unlock()
		return SavedSSHKey{}, ErrSSHKeyNotFound
	}
	s.keys[idx].Name = name
	out := s.keys[idx]
	s.mu.Unlock()
	if err := s.saveMeta(); err != nil {
		return SavedSSHKey{}, err
	}
	return out, nil
}

// Delete убирает ключ вместе с приватной частью.
func (s *SSHKeyStore) Delete(id string) error {
	s.mu.Lock()
	found := false
	for i, k := range s.keys {
		if k.ID == id {
			s.keys = append(s.keys[:i:i], s.keys[i+1:]...)
			found = true
			break
		}
	}
	delete(s.secrets, id)
	s.mu.Unlock()
	if !found {
		return ErrSSHKeyNotFound
	}
	if err := s.saveMeta(); err != nil {
		return err
	}
	return s.saveVault()
}

func (s *SSHKeyStore) add(key SavedSSHKey, secret sshKeySecret) (SavedSSHKey, error) {
	s.mu.Lock()
	// Один и тот же ключ, добавленный дважды, — не два ключа: серверу он
	// предъявляется один, и в списке из двух одинаковых отпечатков человек
	// потом не разберёт, какой удалять.
	for _, existing := range s.keys {
		if existing.Fingerprint == key.Fingerprint {
			s.mu.Unlock()
			return existing, nil
		}
	}
	s.keys = append([]SavedSSHKey{key}, s.keys...)
	s.secrets[key.ID] = secret
	s.mu.Unlock()
	if err := s.saveMeta(); err != nil {
		return SavedSSHKey{}, err
	}
	if err := s.saveVault(); err != nil {
		return SavedSSHKey{}, err
	}
	return key, nil
}

func (s *SSHKeyStore) saveMeta() error {
	s.mu.RLock()
	b, err := json.MarshalIndent(s.keys, "", "  ")
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

func (s *SSHKeyStore) saveVault() error {
	s.mu.RLock()
	vault := sshKeyVault{Version: 1, Keys: make(map[string]sshKeySecret, len(s.secrets))}
	for id, sec := range s.secrets {
		vault.Keys[id] = sec
	}
	s.mu.RUnlock()
	b, err := json.Marshal(vault)
	if err != nil {
		return err
	}
	if err := secretbox.WriteFile(s.vaultPath, b); err != nil {
		return err
	}
	s.mu.Lock()
	s.foreign = false
	s.mu.Unlock()
	return nil
}

// parsePrivateKey разбирает ключ и заодно отвечает, был ли он зашифрован.
// Различать «нужен пароль» и «пароль не тот» обязательно: в первом случае
// человек его вводит, во втором — вспоминает другой.
func parsePrivateKey(pemData []byte, passphrase string) (ssh.Signer, bool, error) {
	if passphrase != "" {
		signer, err := ssh.ParsePrivateKeyWithPassphrase(pemData, []byte(passphrase))
		if err != nil {
			// Пароль дан для НЕзашифрованного ключа — не ошибка человека:
			// разбираем ключ как есть и просто не запоминаем лишний пароль.
			if signer, plainErr := ssh.ParsePrivateKey(pemData); plainErr == nil {
				return signer, false, nil
			}
			if strings.Contains(err.Error(), "decrypt") || strings.Contains(err.Error(), "passphrase") {
				return nil, false, ErrSSHKeyBadPassphrase
			}
			return nil, false, ErrSSHKeyInvalid
		}
		return signer, true, nil
	}
	signer, err := ssh.ParsePrivateKey(pemData)
	if err != nil {
		var missing *ssh.PassphraseMissingError
		if errors.As(err, &missing) {
			return nil, true, ErrSSHKeyNeedsPassphrase
		}
		return nil, false, ErrSSHKeyInvalid
	}
	return signer, false, nil
}

// defaultKeyName даёт ключу имя, если человек его не назвал: безымянная строка
// в списке хуже неточной.
func defaultKeyName(keyType string) string {
	switch {
	case strings.Contains(keyType, "ed25519"):
		return "Ключ ed25519"
	case strings.Contains(keyType, "rsa"):
		return "Ключ RSA"
	case strings.Contains(keyType, "ecdsa"):
		return "Ключ ECDSA"
	default:
		return "SSH-ключ"
	}
}
