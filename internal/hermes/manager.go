// Package hermes owns an isolated Hermes installation and a persistent local
// backend. Hermes stores credentials in its own profile; the local backend
// session token stays private to this transport.
package hermes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

var (
	ErrBusy         = errors.New("Hermes выполняет другую операцию")
	ErrNotInstalled = errors.New("Hermes ещё не установлен")
	ErrNotReady     = errors.New("Hermes ещё не готов к работе")
	ErrExternal     = errors.New("Обнаружена внешняя установка Hermes. Установите отдельную копию в Remotai для запуска и автоматических обновлений")
	ErrDeferred     = errors.New("Обновление Hermes ожидает завершения задач")
	ErrClosed       = errors.New("Hermes остановлен вместе с приложением")
)

type Options struct {
	// Root must be local application data, never a project or synchronised folder.
	// Empty uses the operating system's per-user application data directory.
	Root           string
	HTTPClient     *http.Client
	StartupTimeout time.Duration
	CheckInterval  time.Duration
	// Test seams keep process and network fixtures away from a real installation.
	command  func(context.Context, string, ...string) *exec.Cmd
	lookPath func(string) (string, error)
	goos     string
}

type Status struct {
	Installed           bool   `json:"installed"`
	Ownership           string `json:"ownership"`
	Running             bool   `json:"running"`
	Ready               bool   `json:"ready"`
	State               string `json:"state"`
	Operation           string `json:"operation,omitempty"`
	OperationDetail     string `json:"operation_detail,omitempty"`
	Version             string `json:"version,omitempty"`
	LatestVersion       string `json:"latest_version,omitempty"`
	AutoUpdate          bool   `json:"auto_update"`
	UpdateAvailable     bool   `json:"update_available"`
	UpdatePending       bool   `json:"update_pending"`
	LastCheckedAt       string `json:"last_checked_at,omitempty"`
	NextUpdateAttemptAt string `json:"next_update_attempt_at,omitempty"`
	LastError           string `json:"last_error,omitempty"`
	DataDir             string `json:"data_dir"`
	BinaryPath          string `json:"binary_path,omitempty"`
	UpdateChannel       string `json:"update_channel"`
	InstalledCommit     string `json:"installed_commit,omitempty"`
	LatestCommit        string `json:"latest_commit,omitempty"`
	BackendGeneration   uint64 `json:"backend_generation"`
}

type diskState struct {
	Schema          int    `json:"schema"`
	Managed         bool   `json:"managed"`
	AutoUpdate      bool   `json:"auto_update"`
	Version         string `json:"version,omitempty"`
	LatestVersion   string `json:"latest_version,omitempty"`
	UpdatePending   bool   `json:"update_pending,omitempty"`
	LastCheckedAt   string `json:"last_checked_at,omitempty"`
	LastError       string `json:"last_error,omitempty"`
	Operation       string `json:"operation,omitempty"`
	UpdateRetryAt   string `json:"update_retry_at,omitempty"`
	InstalledCommit string `json:"installed_commit,omitempty"`
	LatestCommit    string `json:"latest_commit,omitempty"`
}

type Manager struct {
	mu                           sync.Mutex
	opMu                         sync.Mutex
	opts                         Options
	root, home, checkout, binary string
	state                        diskState
	process                      *backendProcess
	baseURL, token               string
	ready                        bool
	bridge                       *rpcBridge
	events                       []Event
	seq                          uint64
	epoch                        uint64
	maintenanceOnce              sync.Once
	closing                      bool
	opCancel                     context.CancelFunc
	operationDetail              string
	epochStart                   uint64
}

func New(opts Options) (*Manager, error) {
	if opts.goos == "" {
		opts.goos = runtime.GOOS
	}
	if opts.command == nil {
		opts.command = exec.CommandContext
	}
	if opts.lookPath == nil {
		opts.lookPath = exec.LookPath
	}
	if opts.HTTPClient == nil {
		opts.HTTPClient = &http.Client{Timeout: 30 * time.Second}
	}
	if opts.StartupTimeout <= 0 {
		opts.StartupTimeout = 90 * time.Second
	}
	if opts.CheckInterval <= 0 {
		opts.CheckInterval = 6 * time.Hour
	}
	root := opts.Root
	if root == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		switch opts.goos {
		case "windows":
			root = filepath.Join(home, "AppData", "Local", "Remotai", "hermes")
		case "darwin":
			root = filepath.Join(home, "Library", "Application Support", "Remotai", "hermes")
		default:
			root = filepath.Join(home, ".local", "share", "remotai", "hermes")
		}
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(abs, 0700); err != nil {
		return nil, err
	}
	m := &Manager{opts: opts, root: abs, home: filepath.Join(abs, "home"), checkout: filepath.Join(abs, "runtime"), state: diskState{Schema: 1, AutoUpdate: true}}
	data, err := os.ReadFile(filepath.Join(abs, "manager.json"))
	if err == nil {
		if err := json.Unmarshal(data, &m.state); err != nil {
			return nil, fmt.Errorf("не читаются настройки Hermes: %w", err)
		}
		if m.state.Schema != 1 {
			return nil, errors.New("неподдерживаемая версия настроек Hermes")
		}
		if m.state.Operation != "" {
			m.state.LastError = "Предыдущая операция Hermes была прервана; повторите её"
			m.state.Operation = ""
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	m.detectLocked()
	if err := m.saveLocked(); err != nil {
		return nil, err
	}
	return m, nil
}

func (m *Manager) Status() Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := Status{Installed: m.binary != "", Ownership: "none", Running: m.process != nil, Ready: m.ready, State: "not_installed", Operation: m.state.Operation, Version: m.state.Version, LatestVersion: m.state.LatestVersion, AutoUpdate: m.state.AutoUpdate, UpdatePending: m.state.UpdatePending, UpdateAvailable: m.state.UpdatePending, LastCheckedAt: m.state.LastCheckedAt, LastError: m.state.LastError, DataDir: m.home, BinaryPath: m.binary, UpdateChannel: "main", BackendGeneration: m.epoch, InstalledCommit: m.state.InstalledCommit, LatestCommit: m.state.LatestCommit}
	s.OperationDetail = m.operationDetail
	s.NextUpdateAttemptAt = m.state.UpdateRetryAt
	if m.binary != "" {
		s.Ownership = "external"
		s.State = "installed"
		if m.isManagedLocked() {
			s.Ownership = "managed"
		}
	}
	if s.Running {
		s.State = "starting"
	}
	if s.Ready {
		s.State = "ready"
	}
	if s.Operation != "" {
		s.State = s.Operation
	}
	if s.LastError != "" && !s.Ready && s.Operation == "" {
		s.State = "error"
	}
	return s
}

func (m *Manager) SetAutoUpdate(enabled bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.state.AutoUpdate = enabled
	return m.saveLocked()
}

func (m *Manager) detectLocked() {
	m.binary = ""
	if m.state.Managed && isFile(filepath.Join(m.checkout, ".hermes-bootstrap-complete")) {
		for _, path := range m.managedCandidates() {
			if isFile(path) {
				m.binary = path
				return
			}
		}
	}
	if p, err := m.opts.lookPath("hermes"); err == nil && p != "" {
		// A half-installed owned launcher must not be misclassified as an
		// external install, and cannot silently bypass ownership checks.
		if !within(m.root, p) {
			m.binary = p
		}
	}
}

func (m *Manager) managedCandidates() []string {
	if m.opts.goos == "windows" {
		return []string{filepath.Join(m.checkout, ".hermes", "bin", "hermes.exe"), filepath.Join(m.home, "bin", "hermes.exe"), filepath.Join(m.checkout, ".venv", "Scripts", "hermes.exe"), filepath.Join(m.checkout, "venv", "Scripts", "hermes.exe"), filepath.Join(m.checkout, ".hermes", "bin", "hermes.cmd")}
	}
	return []string{filepath.Join(m.checkout, ".hermes", "bin", "hermes"), filepath.Join(m.checkout, ".venv", "bin", "hermes"), filepath.Join(m.checkout, "venv", "bin", "hermes")}
}

func isFile(path string) bool { st, err := os.Stat(path); return err == nil && !st.IsDir() }

func within(root, path string) bool {
	abs, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(root, abs)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

func (m *Manager) isManagedLocked() bool {
	return m.state.Managed && m.binary != "" && within(m.checkout, m.binary)
}

func (m *Manager) saveLocked() error {
	data, err := json.MarshalIndent(m.state, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(m.root, "manager.json")
	f, err := os.CreateTemp(m.root, ".manager-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err := f.Chmod(0600); err != nil {
		f.Close()
		return err
	}
	if _, err = f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (m *Manager) begin(operation string) error {
	if !m.opMu.TryLock() {
		return ErrBusy
	}
	m.mu.Lock()
	if m.closing {
		m.mu.Unlock()
		m.opMu.Unlock()
		return ErrClosed
	}
	m.state.Operation = operation
	m.operationDetail = ""
	m.state.LastError = ""
	err := m.saveLocked()
	m.mu.Unlock()
	if err != nil {
		m.opMu.Unlock()
	}
	return err
}

func (m *Manager) finish(err error) {
	m.mu.Lock()
	if m.opCancel != nil {
		m.opCancel()
		m.opCancel = nil
	}
	if m.state.Operation == "updating" && m.isManagedLocked() {
		if err != nil && !errors.Is(err, ErrDeferred) {
			m.state.UpdateRetryAt = time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
			m.state.UpdatePending = true
		} else if err == nil {
			m.state.UpdateRetryAt = ""
		}
	}
	m.state.Operation = ""
	m.operationDetail = ""
	if err != nil && !errors.Is(err, ErrDeferred) {
		m.state.LastError = m.scrub(err.Error())
	}
	_ = m.saveLocked()
	m.mu.Unlock()
	m.opMu.Unlock()
}

func (m *Manager) operationContext(ctx context.Context) context.Context {
	ctx, cancel := context.WithCancel(ctx)
	m.mu.Lock()
	m.opCancel = cancel
	if m.closing {
		cancel()
	}
	m.mu.Unlock()
	return ctx
}

func (m *Manager) progress(detail string) { m.mu.Lock(); m.operationDetail = detail; m.mu.Unlock() }

// Close takes priority over pending installation or update work. Once closed,
// this manager cannot launch again; a new application lifetime needs a new one.
func (m *Manager) Close(ctx context.Context) error {
	m.mu.Lock()
	m.closing = true
	if m.opCancel != nil {
		m.opCancel()
	}
	m.mu.Unlock()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for !m.opMu.TryLock() {
		select {
		case <-ctx.Done():
			// Still stop an already spawned backend even if the installer cannot
			// finish its cancelled child before the application's deadline.
			_ = m.stop(ctx)
			return ctx.Err()
		case <-ticker.C:
		}
	}
	defer m.opMu.Unlock()
	return m.stop(ctx)
}

func (m *Manager) scrub(text string) string {
	if m.token != "" {
		text = strings.ReplaceAll(text, m.token, "[скрыто]")
	}
	if len(text) > 1200 {
		text = text[:1200]
	}
	return text
}

// StartMaintenance is explicitly tied to the application's lifetime, not to an
// HTTP request. It never installs Hermes and never updates an external copy.
func (m *Manager) StartMaintenance(ctx context.Context) {
	m.maintenanceOnce.Do(func() {
		go func() {
			timer := time.NewTimer(time.Minute)
			defer timer.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-timer.C:
				}
				m.mu.Lock()
				enabled := !m.closing && m.state.AutoUpdate && m.isManagedLocked()
				last := m.state.LastCheckedAt
				m.mu.Unlock()
				if enabled {
					checked, _ := time.Parse(time.RFC3339, last)
					if checked.IsZero() || time.Since(checked) >= m.opts.CheckInterval {
						probe, cancel := context.WithTimeout(ctx, 2*time.Minute)
						_ = m.CheckUpdate(probe)
						cancel()
					}
					if m.maintenanceUpdateAllowed(time.Now()) {
						updateCtx, cancel := context.WithTimeout(ctx, 30*time.Minute)
						_ = m.Update(updateCtx)
						cancel()
					}
				}
				timer.Reset(time.Minute)
			}
		}()
	})
}

func (m *Manager) maintenanceUpdateAllowed(now time.Time) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closing || !m.state.AutoUpdate || !m.isManagedLocked() || !m.state.UpdatePending {
		return false
	}
	next, err := time.Parse(time.RFC3339, m.state.UpdateRetryAt)
	return err != nil || !now.Before(next)
}
