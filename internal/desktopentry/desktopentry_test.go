package desktopentry

import (
	"encoding/binary"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"tgcontrol/internal/procutil"
)

func TestInstallAndRemovePreservesUnrelatedFiles(t *testing.T) {
	for _, platform := range []string{"darwin", "linux"} {
		t.Run(platform, func(t *testing.T) {
			home := filepath.Join(t.TempDir(), "home with spaces")
			exe := filepath.Join(home, "Apps", "remotai")
			entry, err := installAt(home, exe, platform)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := installAt(home, exe, platform); err != nil {
				t.Fatal("reinstall:", err)
			}
			if platform == "darwin" {
				image, err := os.ReadFile(filepath.Join(entry, "Contents/Resources/remotai.icns"))
				if err != nil {
					t.Fatal(err)
				}
				if string(image[:4]) != "icns" || int(binary.BigEndian.Uint32(image[4:8])) != len(image) {
					t.Fatal("invalid icon container")
				}
				file := filepath.Join(entry, "personal-note.txt")
				if err := os.WriteFile(file, []byte("keep"), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := removeAt(home, platform); err != nil {
					t.Fatal(err)
				}
				if _, err := os.Stat(file); err != nil {
					t.Fatal("unrelated file removed")
				}
				if _, err := installAt(home, exe, platform); err == nil {
					t.Fatal("existing unowned application overwritten")
				}
			} else {
				data, _ := os.ReadFile(entry)
				if !strings.Contains(string(data), "Terminal=false") {
					t.Fatal("launcher would create a terminal")
				}
				if err := removeAt(home, platform); err != nil {
					t.Fatal(err)
				}
				if _, err := os.Stat(entry); !os.IsNotExist(err) {
					t.Fatal("owned launcher left behind")
				}
				if err := os.WriteFile(entry, []byte("personal desktop entry"), 0o600); err != nil {
					t.Fatal(err)
				}
				if _, err := installAt(home, exe, platform); err == nil {
					t.Fatal("unrelated shortcut overwritten")
				}
			}
		})
	}
}

func TestShellLauncherPreservesExecutablePathAndArguments(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell execution requires a Unix host")
	}
	home := t.TempDir()
	exe := filepath.Join(home, "Remotai ' $literal `name` & tool")
	receipt := filepath.Join(home, "receipt")
	payload := "#!/bin/sh\nprintf '%s\\n' \"$@\" > '" + receipt + "'\n"
	if err := os.WriteFile(exe, []byte(payload), 0o755); err != nil {
		t.Fatal(err)
	}
	wrapper := filepath.Join(home, "launch.sh")
	if err := os.WriteFile(wrapper, []byte(shellLauncher(exe)), 0o755); err != nil {
		t.Fatal(err)
	}
	args := []string{"first argument", "literal $value", "$(touch NOT_EXECUTED)"}
	cmd := procutil.Hidden(exec.Command("/bin/sh", append([]string{wrapper}, args...)...))
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("launcher failed: %v %s", err, output)
	}
	data, err := os.ReadFile(receipt)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != strings.Join(args, "\n")+"\n" {
		t.Fatalf("arguments changed: %s", data)
	}
}
