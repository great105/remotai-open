package pty

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

// isolateHome уводит хранилище (и secret.key из secretbox) в TempDir.
func isolateHome(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("USERPROFILE", dir)
	t.Setenv("HOME", dir)
}

func TestSSHKeyGenerateAndUse(t *testing.T) {
	isolateHome(t)
	store := NewSSHKeyStore(filepath.Join(t.TempDir(), "ssh_keys.json"))

	key, err := store.Generate("Прод", "ed25519", "")
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if key.Type != ssh.KeyAlgoED25519 {
		t.Fatalf("unexpected type %q", key.Type)
	}
	if !strings.HasPrefix(key.Fingerprint, "SHA256:") {
		t.Fatalf("fingerprint looks wrong: %q", key.Fingerprint)
	}
	// Публичный ключ обязан быть готовой строкой authorized_keys: его копируют
	// на сервер как есть.
	if _, _, _, _, err := ssh.ParseAuthorizedKey([]byte(key.PublicKey)); err != nil {
		t.Fatalf("public key is not an authorized_keys line: %v", err)
	}
	// Открытая часть не должна тащить за собой приватную ни при каких условиях.
	if strings.Contains(key.PublicKey, "PRIVATE") {
		t.Fatal("public key leaks private material")
	}

	pemData, pass, err := store.Material(key.ID)
	if err != nil {
		t.Fatalf("Material: %v", err)
	}
	if pass != "" {
		t.Fatalf("unexpected passphrase %q", pass)
	}
	if _, err := parseSignerPEM([]byte(pemData), ""); err != nil {
		t.Fatalf("stored key does not parse: %v", err)
	}
}

// Главное обещание раздела: ключ переживает перезапуск агента. Без этого
// «хранилище ключей» ничем не отличается от прежней памяти процесса.
func TestSSHKeySurvivesRestart(t *testing.T) {
	isolateHome(t)
	path := filepath.Join(t.TempDir(), "ssh_keys.json")

	first := NewSSHKeyStore(path)
	key, err := first.Generate("Прод", "ed25519", "секрет")
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	reopened := NewSSHKeyStore(path)
	list := reopened.List()
	if len(list) != 1 || list[0].ID != key.ID {
		t.Fatalf("key list after restart: %+v", list)
	}
	if !list[0].Encrypted {
		t.Fatal("encrypted flag lost after restart")
	}
	pemData, pass, err := reopened.Material(key.ID)
	if err != nil {
		t.Fatalf("Material after restart: %v", err)
	}
	// Пароль ключа хранится рядом — иначе после перезагрузки его снова
	// спрашивали бы, и смысл «запомнить» терялся.
	if pass != "секрет" {
		t.Fatalf("passphrase after restart = %q", pass)
	}
	if _, err := parseSignerPEM([]byte(pemData), pass); err != nil {
		t.Fatalf("encrypted key does not unlock after restart: %v", err)
	}
}

func TestSSHKeyImportValidation(t *testing.T) {
	isolateHome(t)
	store := NewSSHKeyStore(filepath.Join(t.TempDir(), "ssh_keys.json"))

	if _, err := store.Import("мусор", []byte("не ключ вовсе"), ""); !errors.Is(err, ErrSSHKeyInvalid) {
		t.Fatalf("want ErrSSHKeyInvalid, got %v", err)
	}

	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKeyWithPassphrase(priv, "test", []byte("пароль"))
	if err != nil {
		t.Fatal(err)
	}
	encrypted := pem.EncodeToMemory(block)

	// Зашифрованный ключ без пароля брать нельзя: хранилище обещает подключать
	// без вопросов, а без пароля вопрос всплыл бы при первом же подключении.
	if _, err := store.Import("ключ", encrypted, ""); !errors.Is(err, ErrSSHKeyNeedsPassphrase) {
		t.Fatalf("want ErrSSHKeyNeedsPassphrase, got %v", err)
	}
	if _, err := store.Import("ключ", encrypted, "не тот"); !errors.Is(err, ErrSSHKeyBadPassphrase) {
		t.Fatalf("want ErrSSHKeyBadPassphrase, got %v", err)
	}
	key, err := store.Import("ключ", encrypted, "пароль")
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if !key.Encrypted {
		t.Fatal("imported key must be marked encrypted")
	}
}

func TestSSHKeyImportSameKeyTwice(t *testing.T) {
	isolateHome(t)
	store := NewSSHKeyStore(filepath.Join(t.TempDir(), "ssh_keys.json"))

	first, err := store.Generate("Прод", "ed25519", "")
	if err != nil {
		t.Fatal(err)
	}
	pemData, _, err := store.Material(first.ID)
	if err != nil {
		t.Fatal(err)
	}
	// Тот же ключ, принесённый повторно, — один объект: два одинаковых
	// отпечатка в списке потом не различить.
	again, err := store.Import("Он же", []byte(pemData), "")
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if again.ID != first.ID {
		t.Fatalf("duplicate key stored as %q (original %q)", again.ID, first.ID)
	}
	if got := len(store.List()); got != 1 {
		t.Fatalf("want 1 key, got %d", got)
	}
}

func TestSSHKeyDelete(t *testing.T) {
	isolateHome(t)
	path := filepath.Join(t.TempDir(), "ssh_keys.json")
	store := NewSSHKeyStore(path)
	key, err := store.Generate("Прод", "ed25519", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(key.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := store.Delete(key.ID); !errors.Is(err, ErrSSHKeyNotFound) {
		t.Fatalf("want ErrSSHKeyNotFound, got %v", err)
	}
	// Приватная часть тоже должна уйти — иначе удаление косметическое.
	if _, _, err := NewSSHKeyStore(path).Material(key.ID); !errors.Is(err, ErrSSHKeyNotFound) {
		t.Fatalf("private material survived deletion: %v", err)
	}
}

func TestAuthorizedKeyCommandQuoting(t *testing.T) {
	// Комментарий ключа пишет человек, и апостроф в нём не должен разваливать
	// команду на сервере (одинарные кавычки sh закрываются по правилу '\'').
	if got, want := shellSingleQuote("Женин 'ноутбук'"), `'Женин '\''ноутбук'\'''`; got != want {
		t.Fatalf("shellSingleQuote = %s, want %s", got, want)
	}
	cmd := authorizedKeyCommand("ssh-ed25519 AAAA Женин 'ноутбук'")
	if !strings.Contains(cmd, `'\''ноутбук'\''`) {
		t.Fatalf("unexpected quoting: %s", cmd)
	}
	// Права выставляем явно: sshd молча игнорирует слишком открытый файл.
	for _, want := range []string{"chmod 700 ~/.ssh", "chmod 600 ~/.ssh/authorized_keys", authorizedKeyExists} {
		if !strings.Contains(cmd, want) {
			t.Fatalf("command misses %q: %s", want, cmd)
		}
	}
}
