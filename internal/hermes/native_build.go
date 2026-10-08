package hermes

import (
	"bytes"
	"context"
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
)

//go:embed native_build_tools.sh
var nativeBuildTools []byte

func (m *Manager) needsNativeBuildTools() bool {
	return m.opts.goos == "darwin" && m.opts.goarch == "amd64"
}

func (m *Manager) prepareNativeBuildTools(ctx context.Context) error {
	if !m.needsNativeBuildTools() {
		return nil
	}
	m.progress("Готовим инструменты сборки зависимостей Hermes для Mac Intel")
	script := filepath.Join(m.root, "bootstrap", "native_build_tools.sh")
	if err := os.MkdirAll(filepath.Dir(script), 0700); err != nil {
		return err
	}
	if err := os.WriteFile(script, bytes.ReplaceAll(nativeBuildTools, []byte("\r\n"), []byte("\n")), 0600); err != nil {
		return err
	}
	cmd := m.command(ctx, "bash", script, filepath.Join(m.root, "toolchain"))
	cmd.Dir = m.root
	cmd.Env = m.environment(true, "")
	if _, err := m.capture(cmd, "native-build-tools"); err != nil {
		return fmt.Errorf("не удалось подготовить инструменты сборки Hermes для Mac Intel; нужны инструменты разработчика macOS и доступ к серверам Rust/OpenSSL (%w)", err)
	}
	return nil
}
