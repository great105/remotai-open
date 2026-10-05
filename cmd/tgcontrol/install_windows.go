//go:build windows

package main

import (
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows/registry"

	"tgcontrol/internal/bundle"
	"tgcontrol/internal/paths"
	"tgcontrol/internal/wincli"
)

// ensureInstalled — лёгкая самоустановка: копирует exe в папку данных
// (%LOCALAPPDATA%\Remotai) и добавляет её в user PATH, чтобы `remotai attach`
// работал из любого терминала. У portable-раскладки сохраняем профиль/PATH,
// добавляем только CLI launcher рядом с remotai.exe. Ошибки пишем в лог.
func ensureInstalled() {
	exe, err := os.Executable()
	if err != nil {
		return
	}
	if paths.Portable() {
		if strings.EqualFold(filepath.Base(exe), "remotai.exe") {
			if err := wincli.Ensure(filepath.Dir(exe)); err != nil {
				log.Printf("[INSTALL] CLI launcher: %v", err)
			}
		}
		return
	}
	// Куда указывать PATH и нужна ли вторая копия.
	//
	// ⚠ Раньше exe копировался в папку данных ВСЕГДА — и у человека, поставившего
	// Remotai установщиком, на диске лежали две копии по 25,7 МБ: одна в
	// %LOCALAPPDATA%\Programs\Remotai (установщик), вторая в
	// %LOCALAPPDATA%\Remotai (эта функция). Про обе никто не говорил, и вторая
	// не давала ничего: PATH может указывать прямо на папку установки
	// (аудит онбординга 30.08.2026).
	//
	// Копию по-прежнему делаем для запуска «откуда попало» (portable-файл в
	// Загрузках): PATH на Загрузки — не то, что человек имел в виду.
	pathDir := paths.Base()
	if installedDir := stableInstallDir(exe); installedDir != "" {
		pathDir = installedDir
	} else {
		dst := filepath.Join(paths.Base(), "remotai.exe")
		if !strings.EqualFold(filepath.Clean(exe), filepath.Clean(dst)) {
			// Копия может быть занята другим работающим экземпляром — тогда просто
			// пропускаем, обновится при следующем старте.
			if err := copyFile(exe, dst); err != nil {
				log.Printf("[INSTALL] copy to %s: %v", dst, err)
			} else {
				log.Printf("[INSTALL] exe copied to %s", dst)
			}
		}
	}
	if err := addUserPath(pathDir); err != nil {
		log.Printf("[INSTALL] PATH: %v", err)
	}
	if err := wincli.Ensure(pathDir); err != nil {
		log.Printf("[INSTALL] CLI launcher: %v", err)
	}
	// Папка ~/Remotai (files + встроенные скиллы) — при каждом старте, чтобы
	// она появилась и у давних установок после автообновления.
	if err := bundle.Ensure(); err != nil {
		log.Printf("[INSTALL] папка ~/Remotai: %v", err)
	}
}

// stableInstallDir — каталог, в котором exe стоит ПОСТОЯННО (его поставил
// установщик), или "" для запуска «откуда попало».
//
// Признак — деинсталлятор Inno рядом с exe: он появляется только у настоящей
// установки и переживает автообновление (оно меняет сам exe, а не соседей).
// Проверка по файлу, а не по пути: папку установки человек выбирает сам.
func stableInstallDir(exe string) string {
	dir := filepath.Dir(exe)
	for _, name := range []string{"unins000.exe", "unins001.exe"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			return dir
		}
	}
	return ""
}

// isRoot на Windows не используется: `remotai install` там сразу выходит
// с подсказкой (автозапуск — из панели/инсталлятора). Заглушка для install.go.
func isRoot() bool { return false }

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dst + ".tmp"
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// addUserPath дописывает dir в HKCU\Environment\Path (если его там нет) и
// рассылает WM_SETTINGCHANGE, чтобы новые терминалы увидели изменение.
func addUserPath(dir string) error {
	k, err := registry.OpenKey(registry.CURRENT_USER, "Environment", registry.QUERY_VALUE|registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()

	cur, typ, err := k.GetStringValue("Path")
	if err != nil && err != registry.ErrNotExist {
		return err
	}
	if typ == 0 {
		typ = registry.EXPAND_SZ
	}
	for _, p := range strings.Split(cur, ";") {
		if strings.EqualFold(strings.TrimSpace(p), dir) {
			return nil // уже есть
		}
	}
	next := dir
	if cur != "" {
		next = strings.TrimRight(cur, ";") + ";" + dir
	}
	if typ == registry.EXPAND_SZ {
		err = k.SetExpandStringValue("Path", next)
	} else {
		err = k.SetStringValue("Path", next)
	}
	if err != nil {
		return err
	}

	// Уведомить систему (новые процессы Explorer'а подхватят PATH).
	user32 := syscall.NewLazyDLL("user32.dll")
	env, _ := syscall.UTF16PtrFromString("Environment")
	const HWND_BROADCAST, WM_SETTINGCHANGE, SMTO_ABORTIFHUNG = 0xffff, 0x001A, 0x0002
	user32.NewProc("SendMessageTimeoutW").Call(
		HWND_BROADCAST, WM_SETTINGCHANGE, 0,
		uintptr(unsafe.Pointer(env)), SMTO_ABORTIFHUNG, 3000, 0)
	log.Printf("[INSTALL] %s добавлен в user PATH", dir)
	return nil
}
