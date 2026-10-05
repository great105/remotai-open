//go:build windows

package codec

// Загрузка openh264 и вызовы его методов на Windows.
//
// Этот слой существует ради второй платформы: сам кодировщик (h264_openh264.go)
// одинаков везде — структуры openh264, порядок методов в таблице, разбор
// битстрима, — а различаются только «открыть библиотеку», «найти функцию» и
// «позвать по адресу». Пока различий не было, весь файл был помечен windows, и
// Linux-агент оставался без H.264 вовсе.

import (
	"fmt"
	"syscall"
)

// dynLib — загруженная динамическая библиотека.
type dynLib struct{ dll *syscall.DLL }

func openDynLib(path string) (*dynLib, error) {
	dll, err := syscall.LoadDLL(path)
	if err != nil {
		return nil, fmt.Errorf("load %s: %w", path, err)
	}
	return &dynLib{dll: dll}, nil
}

// proc возвращает адрес экспортируемой функции.
func (l *dynLib) proc(name string) (uintptr, error) {
	p, err := l.dll.FindProc(name)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", name, err)
	}
	return p.Addr(), nil
}

func (l *dynLib) close() {
	if l.dll != nil {
		l.dll.Release()
		l.dll = nil
	}
}

// callAddr вызывает функцию по адресу (обычная функция или метод из таблицы
// C++-объекта — у обеих одинаковое соглашение для целых и указателей).
func callAddr(fn uintptr, args ...uintptr) uintptr {
	r, _, _ := syscall.SyscallN(fn, args...)
	return r
}

// openh264Names — файлы, среди которых ищем библиотеку рядом с программой.
var openh264Names = []string{
	"openh264.dll",
	"openh264-2.4.1-win64.dll",
	"openh264-2.5.0-win64.dll",
	"openh264-2.3.1-win64.dll",
}

// systemLibDirs пуст на Windows: openh264.dll живёт рядом с программой и
// скачивается при первом включении H.264 (см. EnsureOpenH264DLL).
var systemLibDirs []string

// OpenH264InstallHint — на Windows библиотека скачивается сама.
const OpenH264InstallHint = ""
