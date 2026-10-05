// Package paths centralizes where Remotai stores its files (config, .env,
// logs, state JSONs), so the exe can live anywhere (e.g. Downloads) without
// littering that folder.
//
// Two layouts:
//
//   - Portable/legacy: config.json lives next to the exe → everything keeps
//     working exactly as before (existing installs, dev runtimes with a
//     swapped USERPROFILE, the prod dog-food in Downloads). State files stay
//     as ~/.tgcontrol-*.json dotfiles.
//
//   - Installed (default for fresh runs): %LOCALAPPDATA%\Remotai on Windows,
//     ~/.config/remotai elsewhere. State files live in the same folder, and
//     legacy home dotfiles are migrated in on first access.
package paths

import (
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sync"
)

var (
	once     sync.Once
	baseDir  string
	portable bool
)

func resolve() {
	exeDir := ""
	if exe, err := os.Executable(); err == nil {
		exeDir = filepath.Dir(exe)
	}
	if exeDir != "" && fileExists(filepath.Join(exeDir, "config.json")) {
		baseDir, portable = exeDir, true
		return
	}
	baseDir = defaultDir()
	_ = os.MkdirAll(baseDir, 0o755)
}

// defaultDir uses os.UserHomeDir (USERPROFILE) rather than %LOCALAPPDATA%
// directly, so the dev runtime's profile isolation keeps working.
func defaultDir() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		if exe, err := os.Executable(); err == nil {
			return filepath.Dir(exe)
		}
		return "."
	}
	if runtime.GOOS == "windows" {
		return filepath.Join(home, "AppData", "Local", "Remotai")
	}
	return filepath.Join(home, ".config", "remotai")
}

// Base returns the directory for config.json, .env and the log file.
func Base() string {
	once.Do(resolve)
	return baseDir
}

// Portable reports whether we run in portable/legacy layout (config next to exe).
func Portable() bool {
	once.Do(resolve)
	return portable
}

// StateFile resolves the path for a state file (e.g. "sessions.json").
// Portable layout keeps the legacy ~/.tgcontrol-<name> dotfiles untouched;
// installed layout uses Base()/<name>, adopting a legacy dotfile on first use.
func StateFile(name string) string {
	once.Do(resolve)
	home, herr := os.UserHomeDir()
	legacy := ""
	if herr == nil && home != "" {
		legacy = filepath.Join(home, ".tgcontrol-"+name)
	}
	if portable {
		if legacy != "" {
			return legacy
		}
		return filepath.Join(baseDir, name)
	}
	dst := filepath.Join(baseDir, name)
	if !fileExists(dst) && legacy != "" && fileExists(legacy) {
		migrate(legacy, dst)
	}
	return dst
}

// migrate moves a legacy file into the new location; falls back to copy when
// rename fails (file locked by an old running instance, cross-volume, …).
func migrate(src, dst string) {
	if os.Rename(src, dst) == nil {
		return
	}
	in, err := os.Open(src)
	if err != nil {
		return
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	_, cerr := io.Copy(out, in)
	if err := out.Close(); err == nil && cerr == nil {
		return
	}
	os.Remove(dst) // partial copy — better to retry next start
}

func fileExists(p string) bool {
	info, err := os.Stat(p)
	return err == nil && !info.IsDir()
}
