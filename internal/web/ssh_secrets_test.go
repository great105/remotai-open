package web

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// isolateHome уводит secret.key (AES-ветка secretbox на не-Windows) в TempDir,
// чтобы тест не трогал боевой профиль.
func isolateHome(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("USERPROFILE", dir)
	t.Setenv("HOME", dir)
}

// Ради этого всё и делалось: пароль, сохранённый с persist, открывает сервер
// после перезапуска агента.
func TestSSHSecretSurvivesRestart(t *testing.T) {
	isolateHome(t)
	path := filepath.Join(t.TempDir(), "ssh_secrets.enc")

	first := newSSHSecretStore(path)
	first.set("h-1", sshSecret{password: "пароль прода", persist: true})

	reopened := newSSHSecretStore(path)
	got, ok := reopened.get("h-1")
	if !ok {
		t.Fatal("saved password is gone after restart")
	}
	if got.password != "пароль прода" {
		t.Fatalf("password = %q", got.password)
	}
	if !got.persist {
		t.Fatal("restored secret must stay persistent")
	}
}

// Секрет без persist — прежнее поведение «до перезапуска»: способ подключиться,
// ничего не оставив на компьютере.
func TestSSHSecretWithoutPersistIsNotWritten(t *testing.T) {
	isolateHome(t)
	path := filepath.Join(t.TempDir(), "ssh_secrets.enc")

	store := newSSHSecretStore(path)
	store.set("h-1", sshSecret{password: "разовый", persist: false})
	if _, ok := store.get("h-1"); !ok {
		t.Fatal("in-memory secret must be readable right away")
	}
	if _, ok := newSSHSecretStore(path).get("h-1"); ok {
		t.Fatal("secret without persist must not survive a restart")
	}
}

func TestSSHSecretForgetRemovesFromDisk(t *testing.T) {
	isolateHome(t)
	path := filepath.Join(t.TempDir(), "ssh_secrets.enc")

	store := newSSHSecretStore(path)
	store.set("h-1", sshSecret{password: "пароль", persist: true})
	if !store.forget("h-1") {
		t.Fatal("forget reported nothing to forget")
	}
	if _, ok := newSSHSecretStore(path).get("h-1"); ok {
		t.Fatal("forgotten password came back after restart")
	}
}

// Файл с паролями не должен читаться глазами: смысл secretbox в этом.
func TestSSHSecretFileIsEncrypted(t *testing.T) {
	isolateHome(t)
	path := filepath.Join(t.TempDir(), "ssh_secrets.enc")

	newSSHSecretStore(path).set("h-1", sshSecret{password: "сверхсекрет", persist: true})
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("vault not written: %v", err)
	}
	if strings.Contains(string(raw), "сверхсекрет") {
		t.Fatal("vault stores the password in plain text")
	}
}
