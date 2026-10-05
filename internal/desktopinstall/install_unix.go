//go:build linux || darwin

// Package desktopinstall stages a packaged desktop agent in the user's stable
// update location. It never starts/stops services or changes an existing session.
package desktopinstall

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
	"tgcontrol/internal/procutil"
	"tgcontrol/internal/version"
)

var releasePattern = regexp.MustCompile(`^Remotai v(\d+\.\d+\.\d+)(?: |$)`)
var semverPattern = regexp.MustCompile(`^\d+\.\d+\.\d+$`)

func IsPackageLaunch(exe string, args []string, platform string) bool {
	if len(args) == 1 && args[0] == "--desktop" {
		return true
	}
	bundle := filepath.Dir(filepath.Dir(filepath.Dir(exe)))
	return platform == "darwin" && len(args) == 0 &&
		strings.EqualFold(filepath.Ext(bundle), ".app") &&
		strings.HasSuffix(filepath.ToSlash(exe), "/Contents/MacOS/Remotai")
}

// Install preserves an equal/newer installed version, including self-updates.
// An unknown executable or symlink is never overwritten.
func Install(ctx context.Context, source, home, bundledVersion string) (string, error) {
	return install(ctx, source, home, bundledVersion, executableVersion)
}

func install(ctx context.Context, source, home, bundledVersion string, readVersion func(context.Context, string) (string, error)) (string, error) {
	if !filepath.IsAbs(source) || !filepath.IsAbs(home) || !semverPattern.MatchString(bundledVersion) {
		return "", errors.New("Установочный пакет повреждён: проверьте версию и скачайте его заново")
	}
	dir := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	if st, err := os.Lstat(dir); err != nil || !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("Папка установки Remotai недоступна или заменена ссылкой")
	}
	fd, err := unix.Open(filepath.Join(dir, ".remotai-desktop.lock"), unix.O_RDWR|unix.O_CREAT|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return "", err
	}
	defer unix.Close(fd)
	for {
		err = unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			break
		}
		if err != unix.EWOULDBLOCK && err != unix.EAGAIN {
			return "", err
		}
		select {
		case <-ctx.Done():
			return "", errors.New("Remotai уже устанавливается. Подождите и откройте приложение ещё раз")
		case <-time.After(50 * time.Millisecond):
		}
	}
	defer unix.Flock(fd, unix.LOCK_UN)
	target := filepath.Join(dir, "remotai")
	if st, err := os.Lstat(target); err == nil {
		if !st.Mode().IsRegular() {
			return "", fmt.Errorf("Не заменяю посторонний файл или ссылку: %s", target)
		}
		installedVersion, err := readVersion(ctx, target)
		if err != nil {
			return "", fmt.Errorf("Не удалось проверить установленный Remotai; прежний файл сохранён: %w", err)
		}
		if !version.IsNewer(bundledVersion, installedVersion) {
			return target, nil
		}
	} else if !os.IsNotExist(err) {
		return "", err
	}
	input, err := os.Open(source)
	if err != nil {
		return "", err
	}
	defer input.Close()
	st, err := input.Stat()
	if err != nil || !st.Mode().IsRegular() || st.Size() == 0 {
		return "", errors.New("В установочном пакете нет готового приложения")
	}
	staged, err := os.CreateTemp(dir, ".remotai-desktop-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(staged.Name())
	defer staged.Close()
	if _, err := io.Copy(staged, input); err != nil {
		return "", err
	}
	if err := staged.Chmod(0o755); err != nil {
		return "", err
	}
	if err := staged.Sync(); err != nil {
		return "", err
	}
	if err := staged.Close(); err != nil {
		return "", err
	}
	if got, err := readVersion(ctx, staged.Name()); err != nil || got != bundledVersion {
		return "", errors.New("Проверка установочного пакета не прошла; прежний Remotai сохранён")
	}
	if err := os.Rename(staged.Name(), target); err != nil {
		return "", err
	}
	return target, nil
}

func executableVersion(ctx context.Context, path string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	// Releases before 2.65.3 only understand --version. Passing "version" to
	// them starts the application instead of inspecting the installed binary.
	cmd := exec.CommandContext(ctx, path, "--version")
	procutil.Hidden(cmd)
	output, err := cmd.Output()
	if err != nil {
		return "", errors.New("файл не отвечает на проверку версии")
	}
	match := releasePattern.FindStringSubmatch(strings.TrimSpace(string(output)))
	if len(match) != 2 {
		return "", errors.New("файл не распознан как Remotai")
	}
	return match[1], nil
}

func Start(exe string, args ...string) error {
	cmd := exec.Command(exe, args...)
	procutil.Hidden(cmd)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("Не удалось запустить Remotai: %w", err)
	}
	return cmd.Process.Release()
}
