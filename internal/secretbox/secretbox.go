// Package secretbox шифрует секреты, которые обязаны пережить перезапуск
// агента: пароли SSH-серверов и приватные ключи из хранилища приложения.
//
// Ключ шифрования принадлежит МАШИНЕ, а не приложению, и человек его нигде не
// вводит — иначе «запомнить пароль» превращается в «вводи мастер-пароль после
// каждой перезагрузки», то есть в ту же работу под другим именем:
//
//   - Windows — DPAPI (CryptProtectData): расшифровать может только та же
//     учётная запись Windows на этом же компьютере. Копия файла, унесённая на
//     чужую машину или открытая из-под другого пользователя, бесполезна.
//   - Остальные ОС — AES-256-GCM с ключом из secret.key рядом с конфигом
//     (права 0600, владелец — пользователь агента). Слабее DPAPI: кто прочитал
//     домашнюю папку целиком, прочитал и ключ. Зато утечка ОДНОГО файла с
//     паролями (бэкап, синхронизация папки) ничего не даёт.
//
// Формат контейнера самоописательный (v + alg), поэтому файл, зашифрованный
// другой учётной записью или перенесённый с другой ОС, распознаётся как чужой
// и даёт ErrForeign — а не «битый JSON», из-за которого хранилище молча
// стартовало бы пустым и человек решил бы, что пароли исчезли сами.
package secretbox

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"

	"tgcontrol/internal/paths"
)

// ErrForeign — контейнер цел, но расшифровать его на этой машине нечем:
// DPAPI-блоб другой учётной записи Windows, файл с другого компьютера или
// потерянный secret.key. Вызывающий обязан сказать об этом человеком, а не
// делать вид, что секретов никогда не было.
var ErrForeign = errors.New("secretbox: encrypted by another account or machine")

// errPlatformUnavailable — на этой ОС платформенного хранилища нет (см.
// dpapi_other.go); Seal молча уходит на AES-GCM. Настоящий сбой DPAPI под
// Windows этим маркером НЕ прикрывается: он всплывает наружу как ошибка.
var errPlatformUnavailable = errors.New("secretbox: platform keystore unavailable")

// Алгоритмы контейнера.
const (
	algDPAPI = "dpapi"
	algAES   = "aes-gcm"
)

// container — то, что реально лежит на диске.
type container struct {
	V    int    `json:"v"`
	Alg  string `json:"alg"`
	Data string `json:"data"` // base64
}

// Seal шифрует plain доступным на этой ОС способом.
func Seal(plain []byte) ([]byte, error) {
	if enc, err := protect(plain); err == nil {
		return marshal(algDPAPI, enc)
	} else if !errors.Is(err, errPlatformUnavailable) {
		return nil, err
	}
	enc, err := aesSeal(plain)
	if err != nil {
		return nil, err
	}
	return marshal(algAES, enc)
}

// Open расшифровывает контейнер. Чужой контейнер — ErrForeign.
func Open(blob []byte) ([]byte, error) {
	var c container
	if err := json.Unmarshal(blob, &c); err != nil {
		return nil, fmt.Errorf("secretbox: bad container: %w", err)
	}
	raw, err := base64.StdEncoding.DecodeString(c.Data)
	if err != nil {
		return nil, fmt.Errorf("secretbox: bad container payload: %w", err)
	}
	switch c.Alg {
	case algDPAPI:
		plain, err := unprotect(raw)
		if err != nil {
			// И «мы не на Windows», и «DPAPI отказал» значат для человека одно:
			// это секреты другой учётной записи.
			return nil, ErrForeign
		}
		return plain, nil
	case algAES:
		plain, err := aesOpen(raw)
		if err != nil {
			return nil, ErrForeign
		}
		return plain, nil
	default:
		return nil, fmt.Errorf("secretbox: unknown algorithm %q", c.Alg)
	}
}

// WriteFile шифрует и атомарно кладёт в path (0600).
func WriteFile(path string, plain []byte) error {
	blob, err := Seal(plain)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, blob, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ReadFile читает и расшифровывает path. Отсутствующий файл — (nil, nil):
// «хранилища ещё нет» — обычное состояние первого запуска, не ошибка.
func ReadFile(path string) ([]byte, error) {
	blob, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if len(blob) == 0 {
		return nil, nil
	}
	return Open(blob)
}

func marshal(alg string, enc []byte) ([]byte, error) {
	return json.Marshal(container{V: 1, Alg: alg, Data: base64.StdEncoding.EncodeToString(enc)})
}

// ── AES-GCM с машинным ключом (не-Windows и аварийный путь) ────────

var (
	keyOnce sync.Once
	keyData []byte
	keyErr  error
)

// machineKey читает (или создаёт при первом обращении) secret.key рядом с
// конфигом агента. 0600 и приватная папка — единственная защита на Unix.
func machineKey() ([]byte, error) {
	keyOnce.Do(func() {
		path := filepath.Join(paths.Base(), "secret.key")
		if b, err := os.ReadFile(path); err == nil && len(b) == 32 {
			keyData = b
			return
		}
		key := make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			keyErr = err
			return
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			keyErr = err
			return
		}
		// O_EXCL: два стартующих агента не должны затереть ключ друг друга —
		// проигравший перечитает уже записанный.
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			if b, rerr := os.ReadFile(path); rerr == nil && len(b) == 32 {
				keyData = b
				return
			}
			keyErr = err
			return
		}
		if _, err := f.Write(key); err != nil {
			f.Close()
			keyErr = err
			return
		}
		if err := f.Close(); err != nil {
			keyErr = err
			return
		}
		keyData = key
	})
	return keyData, keyErr
}

func aesSeal(plain []byte) ([]byte, error) {
	gcm, err := aesGCM()
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return gcm.Seal(nonce, nonce, plain, nil), nil
}

func aesOpen(enc []byte) ([]byte, error) {
	gcm, err := aesGCM()
	if err != nil {
		return nil, err
	}
	if len(enc) < gcm.NonceSize() {
		return nil, io.ErrUnexpectedEOF
	}
	nonce, body := enc[:gcm.NonceSize()], enc[gcm.NonceSize():]
	return gcm.Open(nil, nonce, body, nil)
}

func aesGCM() (cipher.AEAD, error) {
	key, err := machineKey()
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}
