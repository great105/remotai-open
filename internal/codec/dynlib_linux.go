//go:build linux

package codec

// Загрузка openh264 и вызовы его методов на Linux — без cgo.
//
// purego делает то же, что syscall.LoadDLL/SyscallN на Windows: открывает
// разделяемую библиотеку и зовёт функции по адресу, соблюдая соглашение
// вызовов. Для openh264 этого достаточно: все аргументы, которые мы передаём, —
// целые и указатели (сами структуры лежат в памяти Go и передаются по ссылке).
// Сборка при этом остаётся статической и кросс-компилируемой, что для агента,
// который раскладывается одним файлом, важнее удобства cgo.

import (
	"fmt"

	"github.com/ebitengine/purego"
)

type dynLib struct{ handle uintptr }

func openDynLib(path string) (*dynLib, error) {
	// RTLD_NOW: пусть все символы разрешатся сразу — падение на первом же
	// вызове посреди кадра диагностировать куда труднее, чем отказ при загрузке.
	h, err := purego.Dlopen(path, purego.RTLD_NOW|purego.RTLD_LOCAL)
	if err != nil {
		return nil, fmt.Errorf("load %s: %w", path, err)
	}
	return &dynLib{handle: h}, nil
}

func (l *dynLib) proc(name string) (uintptr, error) {
	p, err := purego.Dlsym(l.handle, name)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", name, err)
	}
	if p == 0 {
		return 0, fmt.Errorf("%s: символ не найден", name)
	}
	return p, nil
}

func (l *dynLib) close() {
	if l.handle != 0 {
		_ = purego.Dlclose(l.handle)
		l.handle = 0
	}
}

func callAddr(fn uintptr, args ...uintptr) uintptr {
	r, _, _ := purego.SyscallN(fn, args...)
	return r
}

// openh264Names — как Cisco называет свои сборки для Linux плюс имя из
// репозиториев дистрибутивов (Debian/Ubuntu ставят libopenh264.so.N).
var openh264Names = []string{
	"libopenh264.so",
	"openh264.so",
	// Пакеты дистрибутивов кладут библиотеку с номером ABI в имени, и симлинка
	// без номера в рантайм-пакете нет (он в -dev). Проверено на Ubuntu 26.04:
	// apt ставит libopenh264.so.8, и без этой строки кодек «не находился».
	"libopenh264.so.8",
	"libopenh264.so.7",
	"libopenh264.so.6",
	"openh264-2.4.1-linux64.7.so",
}

// systemLibDirs — где дистрибутивы держат разделяемые библиотеки.
var systemLibDirs = []string{
	"/usr/lib/x86_64-linux-gnu",
	"/usr/lib/aarch64-linux-gnu",
	"/usr/lib64",
	"/usr/lib",
	"/usr/local/lib",
	"/lib/x86_64-linux-gnu",
}

// OpenH264InstallHint — команда, которой человек доставит кодек. Своей загрузки
// для Linux нет (см. FindOpenH264DLL), поэтому подсказка ведёт в пакеты.
const OpenH264InstallHint = "sudo apt install libopenh264-8 || sudo apt install libopenh264-7"
