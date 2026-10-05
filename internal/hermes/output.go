package hermes

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Dependency installers may print megabytes. Keep a bounded diagnostic tail
// in private application data on failure; never return provider output to UI.
type outputTail struct{ data []byte }

func (b *outputTail) Write(p []byte) (int, error) {
	n := len(p)
	const limit = 256 * 1024
	if len(p) >= limit {
		b.data = append(b.data[:0], p[len(p)-limit:]...)
		return n, nil
	}
	if len(b.data)+len(p) > limit {
		b.data = append(b.data[:0], b.data[len(b.data)+len(p)-limit:]...)
	}
	b.data = append(b.data, p...)
	return n, nil
}

func (m *Manager) capture(cmd *exec.Cmd, label string) ([]byte, error) {
	b := &outputTail{}
	cmd.Stdout = b
	cmd.Stderr = b
	err := cmd.Run()
	if err != nil {
		m.mu.Lock()
		token := m.token
		m.mu.Unlock()
		tail := string(b.data)
		if token != "" {
			tail = strings.ReplaceAll(tail, token, "[скрыто]")
		}
		logs := filepath.Join(m.root, "logs")
		if os.MkdirAll(logs, 0700) == nil {
			_ = os.WriteFile(filepath.Join(logs, label+".log"), []byte(tail), 0600)
		}
	}
	return b.data, err
}
