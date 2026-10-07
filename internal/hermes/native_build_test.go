package hermes

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestMacIntelPreparesOwnedCompilersBeforePythonDependencies(t *testing.T) {
	f := newFixture(t)
	m := f.manager(t, false)
	m.opts.goos, m.opts.goarch = "darwin", "amd64"
	original := m.opts.command
	prepared, used := false, false
	m.opts.command = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		if len(args) > 0 && filepath.Base(args[0]) == "native_build_tools.sh" {
			if name != "bash" || len(args) != 2 || args[1] != filepath.Join(m.root, "toolchain") {
				t.Fatalf("unscoped compiler bootstrap: %s %v", name, args)
			}
			prepared = true
			return original(ctx, name, "--version")
		}
		for i, arg := range args {
			if arg == "--stage" && args[i+1] == "python-deps" {
				if !prepared {
					t.Fatal("Mac Intel dependencies ran before the owned compiler bootstrap")
				}
				used = true
			}
		}
		return original(ctx, name, args...)
	}
	// The fixture's Python path follows the host OS, while the installation
	// commands below exercise Darwin. Keep that local test seam consistent.
	if filepath.Separator == '\\' {
		originalPython := filepath.Join(m.root, "toolchain", "store", "python-fixture", "python.exe")
		m.opts.command = fixtureDarwinPython(t, m, m.opts.command, originalPython)
	}
	if err := m.Install(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !prepared || !used {
		t.Fatal("Mac Intel compiler preparation was bypassed")
	}
}

func fixtureDarwinPython(t *testing.T, m *Manager, command func(context.Context, string, ...string) *exec.Cmd, hostPython string) func(context.Context, string, ...string) *exec.Cmd {
	return func(ctx context.Context, name string, args ...string) *exec.Cmd {
		cmd := command(ctx, name, args...)
		for i, arg := range args {
			if arg == "--stage" && args[i+1] == "python-deps" {
				data, err := os.ReadFile(hostPython)
				if err != nil {
					t.Fatal(err)
				}
				path := filepath.Join(filepath.Dir(hostPython), "bin", "python3")
				if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, data, 0600); err != nil {
					t.Fatal(err)
				}
				launcher := filepath.Join(m.checkout, ".hermes", "bin", "hermes")
				if err := os.WriteFile(launcher, []byte("fixture"), 0600); err != nil {
					t.Fatal(err)
				}
			}
		}
		return cmd
	}
}

func TestNativeCompilerEnvironmentStaysInsideManagedInstall(t *testing.T) {
	m, err := New(Options{Root: t.TempDir(), goos: "darwin", goarch: "amd64", lookPath: func(string) (string, error) { return "", exec.ErrNotFound }})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("CARGO_HOME", filepath.Join(t.TempDir(), "foreign-cargo"))
	t.Setenv("RUSTUP_HOME", filepath.Join(t.TempDir(), "foreign-rustup"))
	t.Setenv("OPENSSL_DIR", filepath.Join(t.TempDir(), "foreign-openssl"))
	before := os.Getenv("CARGO_HOME")
	values := map[string]string{}
	for _, pair := range m.environment(true, "") {
		key, value, _ := strings.Cut(pair, "=")
		values[key] = value
	}
	for _, key := range []string{"CARGO_HOME", "RUSTUP_HOME", "OPENSSL_DIR"} {
		if !within(filepath.Join(m.root, "toolchain"), values[key]) {
			t.Fatalf("%s escapes owned toolchain: %s", key, values[key])
		}
	}
	if os.Getenv("CARGO_HOME") != before {
		t.Fatal("compiler preparation changed the parent environment")
	}
}

func TestOtherPlatformsDoNotPrepareNativeBuildTools(t *testing.T) {
	for _, platform := range []struct{ os, arch string }{{"windows", "amd64"}, {"linux", "amd64"}, {"linux", "arm64"}, {"darwin", "arm64"}} {
		t.Run(platform.os+"/"+platform.arch, func(t *testing.T) {
			m, err := New(Options{Root: t.TempDir(), goos: platform.os, goarch: platform.arch, lookPath: func(string) (string, error) { return "", exec.ErrNotFound }, command: func(context.Context, string, ...string) *exec.Cmd {
				t.Fatal("compiler preparation ran on an unaffected platform")
				return nil
			}})
			if err != nil {
				t.Fatal(err)
			}
			if err := m.prepareNativeBuildTools(context.Background()); err != nil {
				t.Fatal(err)
			}
		})
	}
}
