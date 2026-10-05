// Package relay — keystore хранит выданный relay JWT в OS-secure
// storage. Если secure-API недоступно (Linux без keyring, тест-окружение),
// используется fallback ~/.tgcontrol/relay_jwt с правами 0600.
package relay

import (
	"errors"
	"os"
	"path/filepath"
)

// SaveJWT записывает JWT в безопасное хранилище.
// На Windows — DPAPI (CryptProtectData), на Linux/macOS — fallback файл.
func SaveJWT(jwt string) error {
	if err := saveJWTPlatform(jwt); err == nil {
		return nil
	} else if !errors.Is(err, errPlatformNotImplemented) {
		return err
	}
	return saveJWTFile(jwt)
}

// LoadJWT возвращает JWT из хранилища, либо пустую строку (без ошибки) если ничего нет.
func LoadJWT() (string, error) {
	if tok, err := loadJWTPlatform(); err == nil {
		return tok, nil
	} else if !errors.Is(err, errPlatformNotImplemented) {
		return "", err
	}
	return loadJWTFile()
}

// ClearJWT удаляет хранящийся JWT.
func ClearJWT() error {
	if err := clearJWTPlatform(); err == nil {
		return nil
	} else if !errors.Is(err, errPlatformNotImplemented) {
		return err
	}
	return clearJWTFile()
}

var errPlatformNotImplemented = errors.New("platform keystore not implemented")

// ------ fallback (file 0600 в %USERPROFILE%/.tgcontrol/relay_jwt) ------

func jwtFilePath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		exe, _ := os.Executable()
		home = filepath.Dir(exe)
	}
	dir := filepath.Join(home, ".tgcontrol")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return filepath.Join(dir, "relay_jwt"), nil
}

func saveJWTFile(jwt string) error {
	p, err := jwtFilePath()
	if err != nil {
		return err
	}
	return os.WriteFile(p, []byte(jwt), 0o600)
}

func loadJWTFile() (string, error) {
	p, err := jwtFilePath()
	if err != nil {
		return "", err
	}
	b, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	return string(b), nil
}

func clearJWTFile() error {
	p, err := jwtFilePath()
	if err != nil {
		return err
	}
	err = os.Remove(p)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
