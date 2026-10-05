// Package update handles binary self-updates.
package update

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"tgcontrol/internal/dnsfallback"
	"tgcontrol/internal/procutil"
)

// HandoffFlag is appended to the restarted process args after a self-update:
// the new process waits ~2.5s on start so the old one releases the listening
// port first (see cmd/tgcontrol/autoupdate.go waitUpdateHandoff).
const HandoffFlag = "--update-handoff"

// Apply downloads a new binary from the given URL and replaces the current one.
// wantSHA256 (hex, optional) is verified before the swap — на пол-пути
// оборванная загрузка не должна превратиться в «обновление».
// On Windows, the running binary can't be replaced directly,
// so we download to a .new file and swap via rename.
func Apply(downloadURL, wantSHA256 string) error {
	if downloadURL == "" {
		return fmt.Errorf("no download URL")
	}

	client := &http.Client{Timeout: 5 * time.Minute, Transport: dnsfallback.Transport()}
	resp, err := client.Get(downloadURL)
	if err != nil {
		return fmt.Errorf("download: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return fmt.Errorf("download failed: HTTP %d", resp.StatusCode)
	}

	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("get executable path: %w", err)
	}
	exe, err = filepath.EvalSymlinks(exe)
	if err != nil {
		return fmt.Errorf("resolve symlinks: %w", err)
	}

	// Download to temp file
	newPath := exe + ".new"
	f, err := os.Create(newPath)
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}

	hasher := sha256.New()
	_, err = io.Copy(io.MultiWriter(f, hasher), resp.Body)
	f.Close()
	if err != nil {
		os.Remove(newPath)
		return fmt.Errorf("write temp file: %w", err)
	}

	if wantSHA256 != "" {
		got := hex.EncodeToString(hasher.Sum(nil))
		if !strings.EqualFold(got, wantSHA256) {
			os.Remove(newPath)
			return fmt.Errorf("sha256 mismatch: got %s, want %s", got, wantSHA256)
		}
	}

	// Make executable on Unix
	if runtime.GOOS != "windows" {
		os.Chmod(newPath, 0755)
	}

	// On Windows: the running binary can be RENAMED (just not deleted/overwritten).
	// Strategy: rename running exe -> backup, rename .new -> exe.
	//
	// ВАЖНО: имя бэкапа УНИКАЛЬНО (`.old-<ms>`), а не фиксированное `.old`.
	// Персистентный pty-host — это тот же remotai.exe в отдельном процессе, и он
	// переживает рестарт сервера ⇒ держит образ открытым и лочит ЛЮБОЙ ранее
	// созданный `.old`. С фиксированным именем `rename(exe→.old)` падал на занятом
	// бэкапе → batch-фолбэк, который потом не мог перезаписать работающий exe
	// (а pty-host его не отпускал и после рестарта) → «binary locked» навсегда.
	// Свежее имя коллизий не даёт: rename работающего exe в новый путь Windows
	// разрешает всегда. Выживший pty-host продолжает работать из переименованного
	// файла; бэкап подметается CleanupOldBinary, когда процесс наконец выйдет.
	if runtime.GOOS == "windows" {
		oldPath := fmt.Sprintf("%s.old-%d", exe, time.Now().UnixMilli())

		if err := os.Rename(exe, oldPath); err != nil {
			// Даже rename в свежее имя не прошёл (редко) — отложенная замена batch'ем.
			batchPath := exe + ".update.bat"
			batch := fmt.Sprintf("@echo off\r\n:retry\r\ntimeout /t 2 /nobreak >nul\r\nmove /y \"%s\" \"%s\" >nul 2>&1\r\nif errorlevel 1 goto retry\r\ndel \"%%~f0\"\r\n", newPath, exe)
			if writeErr := os.WriteFile(batchPath, []byte(batch), 0755); writeErr != nil {
				os.Remove(newPath)
				return fmt.Errorf("backup current binary: %w (batch fallback also failed: %v)", err, writeErr)
			}
			// Launch batch script detached.
			// Окно гасим у самой обёртки cmd.exe: обновление — фоновая работа,
			// человек её не запрашивал и смотреть в неё не должен (сам батник
			// уходит свёрнутым — за это отвечает `start /min`).
			procutil.Hidden(exec.Command("cmd", "/c", "start", "/min", batchPath)).Start()
			return fmt.Errorf("binary locked — scheduled replacement via batch script, restart required")
		}
		if err := os.Rename(newPath, exe); err != nil {
			os.Rename(oldPath, exe) // restore
			return fmt.Errorf("replace binary: %w", err)
		}
		// oldPath (и старые `.old-*`) подметаются на следующем старте.
	} else {
		// On Unix: atomic rename
		if err := os.Rename(newPath, exe); err != nil {
			os.Remove(newPath)
			return fmt.Errorf("replace binary: %w", err)
		}
	}

	return nil
}

// CleanupOldBinary removes leftover post-update backups on startup. Sweeps the
// legacy fixed-name `.old`/`.prev*` and the new unique `.old-<ms>` backups;
// those still locked by a surviving pty-host fail silently and get swept on a
// later start once that process exits.
func CleanupOldBinary() {
	exe, err := os.Executable()
	if err != nil {
		return
	}
	os.Remove(exe + ".old") // legacy fixed-name backup
	for _, pat := range []string{exe + ".old-*", exe + ".prev*"} {
		if matches, err := filepath.Glob(pat); err == nil {
			for _, m := range matches {
				os.Remove(m) // ignore errors (locked backups swept later)
			}
		}
	}
}

// Restart запускает свежезаписанный бинарник с указанными аргументами и
// завершает текущий процесс. Вызывать только после успешного Apply.
// Платформенный запуск — startDetached (см. restart_windows.go / _unix.go).
func Restart(args []string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if err := startDetached(exe, args); err != nil {
		return err
	}
	os.Exit(0)
	return nil // unreachable
}
