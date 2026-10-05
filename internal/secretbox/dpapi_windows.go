//go:build windows

package secretbox

import (
	"errors"
	"syscall"
	"unsafe"
)

// DPAPI: ключ шифрования выводится Windows из учётной записи пользователя,
// приложению его никто не показывает и хранить нечего. Дополнительная энтропия
// привязывает блоб к этому хранилищу: другая программа под тем же
// пользователем не расшифрует файл, просто позвав CryptUnprotectData.
var entropy = []byte("remotai/ssh-vault/v1")

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

func protect(plain []byte) ([]byte, error) {
	in := newBlob(plain)
	ent := newBlob(entropy)
	var out dataBlob
	ret, _, callErr := procCryptProtectData.Call(
		uintptr(unsafe.Pointer(in)),
		0,
		uintptr(unsafe.Pointer(ent)),
		0, 0, 0,
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
	ent := newBlob(entropy)
	var out dataBlob
	ret, _, callErr := procCryptUnprotect.Call(
		uintptr(unsafe.Pointer(in)),
		0,
		uintptr(unsafe.Pointer(ent)),
		0, 0, 0,
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
	if b.cbData == 0 || b.pbData == nil {
		return nil
	}
	out := make([]byte, b.cbData)
	copy(out, unsafe.Slice(b.pbData, b.cbData))
	return out
}
