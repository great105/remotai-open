// Package desktopentry installs a user-owned launcher for the browser-backed
// desktop interface. The agent binary stays at its stable, updatable location.
package desktopentry

import (
	_ "embed"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"tgcontrol/internal/localize"
)

//go:embed icon.png
var icon []byte

const marker = "Remotai desktop launcher v1\n"
const desktopHeader = "# Remotai desktop launcher v1\n"

func Install(exe string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return installAt(home, exe, runtime.GOOS)
}

func shellLauncher(exe string) string {
	quoted := "'" + strings.ReplaceAll(exe, "'", "'\"'\"'") + "'"
	return "#!/bin/sh\n# " + marker + "exec " + quoted + " \"$@\"\n"
}

// Desktop entries unescape string values before interpreting Exec quoting.
func desktopArg(value string) string {
	value = strings.NewReplacer(`\`, `\\\\`, `"`, `\\"`, "$", `\\$`, "`", "\\\\`", "%", "%%").Replace(value)
	return `"` + value + `"`
}

func desktopString(value string) string {
	return strings.NewReplacer(`\`, `\\`, "\n", `\n`, "\r", `\r`, "\t", `\t`).Replace(value)
}

func installAt(home, exe, platform string) (string, error) {
	if platform != "linux" && platform != "darwin" {
		return "", nil
	}
	if !filepath.IsAbs(home) || !filepath.IsAbs(exe) || strings.ContainsAny(exe, "\r\n\x00") {
		return "", fmt.Errorf("%s", localize.Text("путь программы должен быть абсолютным и не содержать переводов строки"))
	}
	root := filepath.Join(home, ".local", "share", "remotai", "desktop")
	if platform == "darwin" {
		root = filepath.Join(home, "Applications", "Remotai.app")
	}
	if st, err := os.Lstat(root); err == nil {
		if !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf(localize.Text("папка ярлыка занята: %s"), root)
		}
		data, err := os.ReadFile(filepath.Join(root, ".remotai-launcher"))
		if err != nil || string(data) != marker {
			return "", fmt.Errorf(localize.Text("не заменяю существующее приложение: %s"), root)
		}
	} else if !os.IsNotExist(err) {
		return "", err
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return "", err
	}
	if err := write(filepath.Join(root, ".remotai-launcher"), []byte(marker), 0o644); err != nil {
		return "", err
	}
	if platform == "darwin" {
		plist := `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
<key>CFBundleIdentifier</key><string>ru.remotai.launcher</string>
<key>CFBundleName</key><string>Remotai</string>
<key>CFBundleDisplayName</key><string>Remotai</string>
<key>CFBundleExecutable</key><string>Remotai</string>
<key>CFBundlePackageType</key><string>APPL</string>
<key>CFBundleVersion</key><string>1</string>
<key>CFBundleShortVersionString</key><string>1.0</string>
<key>CFBundleIconFile</key><string>remotai.icns</string>
<key>LSMinimumSystemVersion</key><string>12.0</string>
<key>LSUIElement</key><true/>
</dict></plist>
`
		// The existing 512px PNG can be stored losslessly in an ic09 ICNS chunk.
		icns := make([]byte, 16+len(icon))
		copy(icns, "icns")
		binary.BigEndian.PutUint32(icns[4:8], uint32(len(icns)))
		copy(icns[8:12], "ic09")
		binary.BigEndian.PutUint32(icns[12:16], uint32(8+len(icon)))
		copy(icns[16:], icon)
		for _, file := range []struct {
			name string
			data []byte
			mode os.FileMode
		}{
			{"Contents/Info.plist", []byte(plist), 0o644},
			{"Contents/Resources/remotai.icns", icns, 0o644},
			{"Contents/MacOS/Remotai", []byte(shellLauncher(exe)), 0o755},
		} {
			if err := write(filepath.Join(root, file.name), file.data, file.mode); err != nil {
				return "", err
			}
		}
		return root, nil
	}
	launcher := filepath.Join(root, "launch.sh")
	if err := write(launcher, []byte(shellLauncher(exe)), 0o755); err != nil {
		return "", err
	}
	iconPath := filepath.Join(root, "icon.png")
	if err := write(iconPath, icon, 0o644); err != nil {
		return "", err
	}
	entry := filepath.Join(home, ".local", "share", "applications", "ru.remotai.desktop")
	if data, err := os.ReadFile(entry); err == nil && !strings.HasPrefix(string(data), desktopHeader) {
		return "", fmt.Errorf(localize.Text("не заменяю существующий ярлык: %s"), entry)
	}
	content := desktopHeader + "[Desktop Entry]\nType=Application\nName=Remotai\nComment=Access your AI agents\nComment[ru]=Удалённый доступ к вашим ИИ-агентам\nExec=/bin/sh " + desktopArg(launcher) + "\nIcon=" + desktopString(iconPath) + "\nTerminal=false\nStartupNotify=false\nCategories=Development;Network;\n"
	if err := write(entry, []byte(content), 0o644); err != nil {
		return "", err
	}
	return entry, nil
}

func write(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".remotai-launcher-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Chmod(mode); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

// Remove deletes only known generated files. Unknown files and unrelated apps
// are preserved; no recursive directory removal is used.
func Remove() error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	return removeAt(home, runtime.GOOS)
}

func removeAt(home, platform string) error {
	if platform != "linux" && platform != "darwin" {
		return nil
	}
	root := filepath.Join(home, ".local", "share", "remotai", "desktop")
	files := []string{"launch.sh", "icon.png", ".remotai-launcher"}
	dirs := []string{""}
	if platform == "darwin" {
		root = filepath.Join(home, "Applications", "Remotai.app")
		files = []string{"Contents/Info.plist", "Contents/Resources/remotai.icns", "Contents/MacOS/Remotai", ".remotai-launcher"}
		dirs = []string{"Contents/Resources", "Contents/MacOS", "Contents", ""}
	}
	if !filepath.IsAbs(home) {
		return fmt.Errorf("%s", localize.Text("домашний каталог должен быть абсолютным"))
	}
	if st, err := os.Lstat(root); err == nil && (!st.IsDir() || st.Mode()&os.ModeSymlink != 0) {
		return fmt.Errorf(localize.Text("папка ярлыка заменена: %s"), root)
	}
	data, err := os.ReadFile(filepath.Join(root, ".remotai-launcher"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if string(data) != marker {
		return fmt.Errorf(localize.Text("ярлык не принадлежит установщику Remotai: %s"), root)
	}
	if platform == "linux" {
		entry := filepath.Join(home, ".local", "share", "applications", "ru.remotai.desktop")
		if data, err := os.ReadFile(entry); err == nil && strings.HasPrefix(string(data), desktopHeader) {
			if err := os.Remove(entry); err != nil {
				return err
			}
		}
	}
	for _, name := range files {
		if err := os.Remove(filepath.Join(root, name)); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	for _, name := range dirs {
		_ = os.Remove(filepath.Join(root, name))
	}
	return nil
}
