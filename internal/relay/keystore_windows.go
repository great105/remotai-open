//go:build windows

package relay

import (
	"errors"
	"syscall"
	"unsafe"
)

// На Windows используем DPAPI (CryptProtectData / CryptUnprotectData):
// данные шифруются ключом, привязанным к учётной записи пользователя.
// Сохраняем зашифрованный blob в файл, как в fallback — но содержимое не plain text.

var (
	modCrypt32           = syscall.NewLazyDLL("crypt32.dll")
	modKernel32          = syscall.NewLazyDLL("kernel32.dll")
	procCryptProtectData = modCrypt32.NewProc("CryptProtectData")
	procCryptUnprotect   = modCrypt32.NewProc("CryptUnprotectData")
	procLocalFree        = modKernel32.NewProc("LocalFree")
)

type dataBlob struct {
	cbData uint32
	pbData *byte
}

func saveJWTPlatform(jwt string) error {
	enc, err := protect([]byte(jwt))
	if err != nil {
		return err
	}
	return saveJWTFile(string(enc))
}

func loadJWTPlatform() (string, error) {
	raw, err := loadJWTFile()
	if err != nil {
		return "", err
	}
	if raw == "" {
		return "", nil
	}
	dec, err := unprotect([]byte(raw))
	if err != nil {
		// Backward-compat: возможно файл содержит plaintext (старая версия).
		return raw, nil
	}
	return string(dec), nil
}

func clearJWTPlatform() error {
	return clearJWTFile()
}

func protect(plain []byte) ([]byte, error) {
	in := newBlob(plain)
	var out dataBlob
	ret, _, callErr := procCryptProtectData.Call(
		uintptr(unsafe.Pointer(in)),
		0,
		0, 0, 0, 0,
		uintptr(unsafe.Pointer(&out)),
	)
	if ret == 0 {
		return nil, errors.New("CryptProtectData failed: " + callErr.Error())
	}
	defer procLocalFree.Call(uintptr(unsafe.Pointer(out.pbData)))
	return blobToBytes(&out), nil
}

func unprotect(enc []byte) ([]byte, error) {
	in := newBlob(enc)
	var out dataBlob
	ret, _, callErr := procCryptUnprotect.Call(
		uintptr(unsafe.Pointer(in)),
		0, 0, 0, 0, 0,
		uintptr(unsafe.Pointer(&out)),
	)
	if ret == 0 {
		return nil, errors.New("CryptUnprotectData failed: " + callErr.Error())
	}
	defer procLocalFree.Call(uintptr(unsafe.Pointer(out.pbData)))
	return blobToBytes(&out), nil
}

func newBlob(b []byte) *dataBlob {
	if len(b) == 0 {
		return &dataBlob{}
	}
	return &dataBlob{cbData: uint32(len(b)), pbData: &b[0]}
}

func blobToBytes(b *dataBlob) []byte {
	out := make([]byte, b.cbData)
	for i := uint32(0); i < b.cbData; i++ {
		out[i] = *(*byte)(unsafe.Pointer(uintptr(unsafe.Pointer(b.pbData)) + uintptr(i)))
	}
	return out
}
