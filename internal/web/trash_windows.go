//go:build windows

package web

import (
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"strconv"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	trashShell32         = windows.NewLazySystemDLL("shell32.dll")
	procSHFileOperationW = trashShell32.NewProc("SHFileOperationW")
)

const (
	foDelete uint32 = 0x0003 // FO_DELETE

	fofSilent         uint16 = 0x0004 // без окна прогресса
	fofNoConfirmation uint16 = 0x0010 // без «Вы уверены?» — спрашивает клиент
	fofAllowUndo      uint16 = 0x0040 // ← собственно «в Корзину», а не насовсем
	fofNoErrorUI      uint16 = 0x0400 // молча вернуть код вместо модалки на ПК
)

// shFileOpStructW — SHFILEOPSTRUCTW из shellapi.h.
//
// ГРАБЛЯ РАСКЛАДКИ: в 32-битной сборке заголовок оборачивает структуру в
// pshpack1.h, то есть она байт-в-байт упакована, а в 64-битной действует
// обычное выравнивание. Здесь описана 64-битная раскладка (56 байт), поэтому
// moveToTrash на 32-битной цели сразу отвечает «корзины нет» — тихо испортить
// стек хуже, чем удалить без корзины и честно об этом сказать.
type shFileOpStructW struct {
	hwnd                  uintptr
	wFunc                 uint32
	_                     uint32 // выравнивание до указателя
	pFrom                 *uint16
	pTo                   *uint16
	fFlags                uint16
	_                     uint16 // выравнивание до BOOL
	fAnyOperationsAborted int32
	hNameMappings         uintptr
	lpszProgressTitle     *uint16
}

// hasRecycleBin — есть ли у тома корзина. Считаем, что она есть только у
// «жёстких» дисков: на сетевом диске и на флешке shell с FOF_ALLOWUNDO
// молча удаляет НАСОВСЕМ, и обещание «файл в Корзине» стало бы ложью.
func hasRecycleBin(path string) bool {
	vol := filepath.VolumeName(path)
	if vol == "" {
		return false
	}
	root, err := windows.UTF16PtrFromString(vol + `\`)
	if err != nil {
		return false
	}
	return windows.GetDriveType(root) == windows.DRIVE_FIXED
}

// moveToTrash отправляет файл или папку в «Корзину» Windows.
func moveToTrash(path string) (trashResult, error) {
	if strconv.IntSize != 64 {
		return trashResult{}, errTrashUnsupported
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return trashResult{}, err
	}
	if !hasRecycleBin(abs) {
		return trashResult{}, errTrashUnsupported
	}

	// pFrom — СПИСОК путей, а не строка: один \0 закрывает путь, второй —
	// список. Без второго нуля shell читает память за буфером.
	buf, err := windows.UTF16FromString(abs)
	if err != nil {
		return trashResult{}, err
	}
	buf = append(buf, 0)

	op := shFileOpStructW{
		wFunc:  foDelete,
		pFrom:  &buf[0],
		fFlags: fofAllowUndo | fofNoConfirmation | fofSilent | fofNoErrorUI,
	}

	// SHFileOperationW документирован как требующий инициализированного COM и
	// одного и того же потока. RPC_E_CHANGED_MODE (апартамент уже поднят
	// кем-то) фатальным не считаем — операция при этом проходит.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if err := windows.CoInitializeEx(0, windows.COINIT_APARTMENTTHREADED); err == nil {
		defer windows.CoUninitialize()
	}

	ret, _, _ := procSHFileOperationW.Call(uintptr(unsafe.Pointer(&op)))
	runtime.KeepAlive(buf)
	if ret != 0 {
		// Коды здесь НЕ Win32 (0x7C DE_INVALIDFILES и т.п.) — переводить их
		// нечем, поэтому наверх уходит просто «не вышло», и файл удаляется
		// обычным способом.
		return trashResult{}, fmt.Errorf("SHFileOperation: 0x%X", ret)
	}
	if op.fAnyOperationsAborted != 0 {
		return trashResult{}, errors.New("shell aborted the delete")
	}
	// RestorePath пуст: вернуть из «Корзины» можно только через оболочку на
	// самом ПК, поэтому кнопку «Отменить» клиенту не обещаем.
	return trashResult{Kind: "recycle_bin"}, nil
}

// forgetTrashEntry — на Windows делать нечего: запись о файле живёт внутри
// самой «Корзины», отдельного файла-описания у неё нет.
func forgetTrashEntry(string) {}
