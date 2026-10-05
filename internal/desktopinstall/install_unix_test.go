//go:build linux || darwin

package desktopinstall

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func testBinaryVersion(_ context.Context, path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	if !strings.HasPrefix(string(data), "binary:") {
		return "", errors.New("invalid binary")
	}
	return strings.TrimPrefix(string(data), "binary:"), nil
}

func desktopFixture(t *testing.T) (source, home, target string) {
	t.Helper()
	root := t.TempDir()
	home = filepath.Join(root, "Пользователь с пробелами")
	target = filepath.Join(home, ".local", "bin", "remotai")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	source = filepath.Join(root, "Remotai.app", "Contents", "MacOS", "Remotai")
	if err := os.MkdirAll(filepath.Dir(source), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, []byte("binary:2.66.0"), 0o555); err != nil {
		t.Fatal(err)
	}
	return
}

func TestDesktopPackageCopiesVerifiedBinaryAndPreservesBundle(t *testing.T) {
	source, home, target := desktopFixture(t)
	got, err := install(context.Background(), source, home, "2.66.0", testBinaryVersion)
	if err != nil || got != target {
		t.Fatalf("install: %s %v", got, err)
	}
	data, _ := os.ReadFile(target)
	st, _ := os.Stat(target)
	if string(data) != "binary:2.66.0" || st.Mode().Perm() != 0o755 {
		t.Fatal("incomplete installed binary")
	}
	data, _ = os.ReadFile(source)
	st, _ = os.Stat(source)
	if string(data) != "binary:2.66.0" || st.Mode().Perm() != 0o555 {
		t.Fatal("immutable app bundle was modified")
	}
}

func TestDesktopPackagePreservesNewerVersionAndRejectsForeignFile(t *testing.T) {
	for _, content := range []string{"binary:2.67.0", "foreign"} {
		t.Run(content, func(t *testing.T) {
			source, home, target := desktopFixture(t)
			os.WriteFile(target, []byte(content), 0o755)
			_, err := install(context.Background(), source, home, "2.66.0", testBinaryVersion)
			if (err != nil) != (content == "foreign") {
				t.Fatalf("error=%v", err)
			}
			data, _ := os.ReadFile(target)
			if string(data) != content {
				t.Fatal("existing file replaced")
			}
		})
	}
}

func TestDesktopPackageRejectsCorruptionAndSymlink(t *testing.T) {
	for _, symlink := range []bool{false, true} {
		t.Run(map[bool]string{false: "corrupt package", true: "symlink"}[symlink], func(t *testing.T) {
			source, home, target := desktopFixture(t)
			old := target
			if symlink {
				old += "-unrelated"
			}
			os.WriteFile(old, []byte("binary:2.65.8"), 0o755)
			if symlink {
				if err := os.Symlink(old, target); err != nil {
					t.Fatal(err)
				}
			}
			if !symlink {
				os.Chmod(source, 0o755)
				os.WriteFile(source, []byte("damaged"), 0o555)
			}
			if _, err := install(context.Background(), source, home, "2.66.0", testBinaryVersion); err == nil {
				t.Fatal("unsafe install accepted")
			}
			data, _ := os.ReadFile(old)
			if string(data) != "binary:2.65.8" {
				t.Fatal("previous data lost")
			}
		})
	}
}

func TestDesktopPackageConcurrentLaunchAndRunningInode(t *testing.T) {
	source, home, target := desktopFixture(t)
	os.WriteFile(target, []byte("binary:2.65.8"), 0o755)
	old, err := os.Open(target)
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var wait sync.WaitGroup
	for i := 0; i < 4; i++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			if _, err := install(ctx, source, home, "2.66.0", testBinaryVersion); err != nil {
				t.Error(err)
			}
		}()
	}
	wait.Wait()
	data, _ := os.ReadFile(target)
	if string(data) != "binary:2.66.0" {
		t.Fatal("new version missing")
	}
	buffer := make([]byte, 50)
	n, _ := old.Read(buffer)
	if string(buffer[:n]) != "binary:2.65.8" {
		t.Fatal("running inode was changed")
	}
}

func TestDesktopPackageLaunchDetection(t *testing.T) {
	for _, name := range []string{"Remotai.app", "Мой Remotai.app"} {
		path := "/Applications/" + name + "/Contents/MacOS/Remotai"
		if !IsPackageLaunch(path, nil, "darwin") || IsPackageLaunch(path, []string{"version"}, "darwin") {
			t.Fatal("app detection")
		}
	}
	if !IsPackageLaunch("/usr/lib/remotai/remotai", []string{"--desktop"}, "linux") || IsPackageLaunch("/home/user/.local/bin/remotai", nil, "linux") {
		t.Fatal("desktop/agent distinction")
	}
}
