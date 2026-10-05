package secretbox

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// isolateHome уводит paths.Base() в TempDir. Без этого AES-ветка создала бы
// secret.key в БОЕВОМ профиле (ровно так `go test ./...` однажды снёс живой
// relay_jwt — см. relay/keystore).
func isolateHome(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("USERPROFILE", dir)
	t.Setenv("HOME", dir)
}

func TestSealOpenRoundTrip(t *testing.T) {
	isolateHome(t)
	plain := []byte(`{"h-1":{"password":"пароль с пробелом"}}`)
	blob, err := Seal(plain)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	// Контейнер обязан быть непрозрачным: пароль не должен читаться глазами в
	// файле — ради этого всё и затевалось.
	if bytes.Contains(blob, []byte("пароль")) {
		t.Fatal("sealed container leaks the plaintext")
	}
	got, err := Open(blob)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if !bytes.Equal(got, plain) {
		t.Fatalf("round-trip mismatch: %q != %q", got, plain)
	}
}

func TestOpenForeignContainer(t *testing.T) {
	isolateHome(t)
	// Целый контейнер с чужой полезной нагрузкой = «зашифровано не здесь».
	// Вызывающий по этому признаку скажет человеку правду вместо «пароли
	// куда-то делись».
	blob, err := json.Marshal(container{V: 1, Alg: algAES, Data: "0YfRg9C20L7QuQ=="})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Open(blob); err != ErrForeign {
		t.Fatalf("want ErrForeign, got %v", err)
	}
}

func TestReadFileMissingIsNotAnError(t *testing.T) {
	isolateHome(t)
	got, err := ReadFile(filepath.Join(t.TempDir(), "nope.enc"))
	if err != nil || got != nil {
		t.Fatalf("want (nil, nil) for a missing vault, got (%v, %v)", got, err)
	}
}

func TestWriteFileRoundTrip(t *testing.T) {
	isolateHome(t)
	path := filepath.Join(t.TempDir(), "vault.enc")
	if err := WriteFile(path, []byte("s3cret")); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if info, err := os.Stat(path); err != nil {
		t.Fatalf("stat: %v", err)
	} else if info.Size() == 0 {
		t.Fatal("vault written empty")
	}
	got, err := ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != "s3cret" {
		t.Fatalf("got %q", got)
	}
}
