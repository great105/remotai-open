// Package tunnel manages a cloudflared subprocess for exposing TGControl to the internet.
package tunnel

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"

	"tgcontrol/internal/procutil"
)

// Status represents the tunnel lifecycle state.
type Status string

const (
	StatusStopped  Status = "stopped"
	StatusStarting Status = "starting"
	StatusRunning  Status = "running"
	StatusError    Status = "error"
)

// Mode represents the tunnel mode.
type Mode string

const (
	ModeDisabled Mode = "disabled"
	ModeQuick    Mode = "quick"
	ModeNamed    Mode = "named"
)

// Info is the snapshot returned by Manager.Info().
type Info struct {
	Mode    Mode     `json:"mode"`
	Status  Status   `json:"status"`
	URL     string   `json:"url"`
	Error   string   `json:"error,omitempty"`
	PID     int      `json:"pid,omitempty"`
	Uptime  int64    `json:"uptime,omitempty"` // seconds
	LogTail []string `json:"log_tail,omitempty"`
}

// Config holds tunnel settings (stored in config.json).
type Config struct {
	Mode            Mode   `json:"tunnel_mode,omitempty"`
	CloudflaredPath string `json:"cloudflared_path,omitempty"`
	TunnelName      string `json:"tunnel_name,omitempty"`
	TunnelURL       string `json:"tunnel_url,omitempty"`
	AutoStart       bool   `json:"tunnel_autostart,omitempty"`
}

// Manager owns the cloudflared subprocess lifecycle.
type Manager struct {
	mu          sync.RWMutex
	cfg         Config
	localPort   int
	status      Status
	url         string
	err         error
	startedAt   time.Time
	cmd         *exec.Cmd
	cancel      context.CancelFunc
	logBuf      *ringBuffer
	onURLChange func(string)

	// exited is closed by the Wait goroutine when the current cloudflared
	// process ends — used by stopLocked (instead of a second cmd.Wait) and by
	// the supervisor to know when to restart.
	exited chan struct{}
	// connCount tracks live edge connections for a named tunnel so status can
	// revert when they all drop.
	connCount int
	// stopRequested is set by Stop() so the supervisor doesn't resurrect a
	// tunnel the user deliberately stopped.
	stopRequested bool
}

// NewManager creates a tunnel manager.
func NewManager(localPort int) *Manager {
	return &Manager{
		localPort: localPort,
		status:    StatusStopped,
		logBuf:    newRingBuffer(200),
	}
}

// SetConfig updates the tunnel configuration.
func (m *Manager) SetConfig(cfg Config) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cfg = cfg
}

// OnURLChange sets a callback invoked when the tunnel URL becomes available.
func (m *Manager) OnURLChange(fn func(string)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.onURLChange = fn
}

// Info returns a snapshot of the tunnel state.
func (m *Manager) Info() Info {
	m.mu.RLock()
	defer m.mu.RUnlock()
	info := Info{
		Mode:    m.cfg.Mode,
		Status:  m.status,
		URL:     m.url,
		LogTail: m.logBuf.Lines(),
	}
	if m.err != nil {
		info.Error = m.err.Error()
	}
	if m.cmd != nil && m.cmd.Process != nil {
		info.PID = m.cmd.Process.Pid
	}
	if !m.startedAt.IsZero() && m.status == StatusRunning {
		info.Uptime = int64(time.Since(m.startedAt).Seconds())
	}
	return info
}

// Start launches cloudflared. Safe to call if already running (no-op).
func (m *Manager) Start(ctx context.Context) error {
	m.mu.Lock()
	if m.status == StatusRunning || m.status == StatusStarting {
		m.mu.Unlock()
		return nil
	}
	m.mu.Unlock()

	return m.start(ctx)
}

// Stop gracefully terminates cloudflared and tells the supervisor to leave it
// stopped.
func (m *Manager) Stop() error {
	m.mu.Lock()
	m.stopRequested = true
	m.mu.Unlock()
	return m.stop()
}

// Restart stops then starts the tunnel.
func (m *Manager) Restart(ctx context.Context) error {
	m.stop()
	time.Sleep(500 * time.Millisecond)
	return m.start(ctx)
}

// Supervise keeps the tunnel up: it (re)starts cloudflared and, whenever the
// process exits unexpectedly or fails to launch, retries with capped
// exponential backoff until ctx is cancelled or the user calls Stop(). This is
// the fix for "the Mini App goes permanently unreachable when cloudflared dies".
func (m *Manager) Supervise(ctx context.Context) {
	backoff := time.Second
	const maxBackoff = 60 * time.Second
	for {
		if ctx.Err() != nil {
			return
		}
		if err := m.Start(ctx); err != nil {
			log.Printf("[TUNNEL] start failed: %v — retrying in %s", err, backoff)
			if !sleepCtx(ctx, backoff) {
				return
			}
			backoff = min(backoff*2, maxBackoff)
			continue
		}

		m.mu.RLock()
		exited := m.exited
		startedAt := m.startedAt
		m.mu.RUnlock()
		if exited != nil {
			select {
			case <-ctx.Done():
				return
			case <-exited:
			}
		}

		m.mu.RLock()
		stopReq := m.stopRequested
		m.mu.RUnlock()
		if ctx.Err() != nil || stopReq {
			return
		}

		// A tunnel that ran healthy for a while gets a fresh backoff budget.
		if time.Since(startedAt) > 2*time.Minute {
			backoff = time.Second
		}
		log.Printf("[TUNNEL] cloudflared down — restarting in %s", backoff)
		if !sleepCtx(ctx, backoff) {
			return
		}
		backoff = min(backoff*2, maxBackoff)
	}
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// ── internal ────────────────────────────────────────────────────────

var quickURLRe = regexp.MustCompile(`https://[a-z0-9-]+\.trycloudflare\.com`)

func (m *Manager) start(ctx context.Context) error {
	m.mu.Lock()
	path := m.cfg.CloudflaredPath
	mode := m.cfg.Mode
	name := m.cfg.TunnelName
	port := m.localPort
	m.mu.Unlock()

	if path == "" {
		path = findCloudflared()
	}
	if path == "" {
		return fmt.Errorf("cloudflared not found")
	}

	var args []string
	switch mode {
	case ModeQuick:
		args = []string{"tunnel", "--url", fmt.Sprintf("http://localhost:%d", port)}
	case ModeNamed:
		if name == "" {
			return fmt.Errorf("tunnel name required for named mode")
		}
		args = []string{"tunnel", "run", name}
	default:
		return fmt.Errorf("tunnel mode is disabled")
	}

	tunnelCtx, cancel := context.WithCancel(ctx)
	cmd := exec.CommandContext(tunnelCtx, path, args...)
	cmd.Env = os.Environ()

	// Окно гасим: cloudflared — фоновая служба, её вывод мы и так читаем в лог
	// через пайп. Общий помощник вместо локального: он не затирает уже
	// выставленный SysProcAttr и ставит оба признака (HideWindow + без консоли).
	procutil.Hidden(cmd)

	// Combine stdout+stderr for parsing
	pr, pw, err := os.Pipe()
	if err != nil {
		cancel()
		return fmt.Errorf("pipe: %w", err)
	}
	cmd.Stdout = pw
	cmd.Stderr = pw

	if err := cmd.Start(); err != nil {
		cancel()
		pw.Close()
		pr.Close()
		return fmt.Errorf("start cloudflared: %w", err)
	}

	exited := make(chan struct{})
	m.mu.Lock()
	m.cmd = cmd
	m.cancel = cancel
	m.status = StatusStarting
	m.err = nil
	m.url = ""
	m.startedAt = time.Now()
	m.connCount = 0
	m.stopRequested = false
	m.exited = exited
	m.logBuf.Reset()
	m.mu.Unlock()

	log.Printf("[TUNNEL] Started cloudflared (PID %d) mode=%s", cmd.Process.Pid, mode)

	// Read output in background
	go func() {
		pw.Close() // close write end in reader goroutine
		scanner := bufio.NewScanner(pr)
		for scanner.Scan() {
			line := scanner.Text()
			m.logBuf.Add(line)

			// Detect quick tunnel URL
			if mode == ModeQuick {
				if match := quickURLRe.FindString(line); match != "" {
					m.mu.Lock()
					if m.url == "" {
						m.url = match
						m.status = StatusRunning
						log.Printf("[TUNNEL] Quick tunnel URL: %s", match)
						if m.onURLChange != nil {
							go m.onURLChange(match)
						}
					}
					m.mu.Unlock()
				}
			}

			// Named tunnel: track edge-connection registrations/drops so status
			// reflects reality (a "running" tunnel with 0 live connections is
			// actually down, and the supervisor/health must see that).
			if mode == ModeNamed {
				switch {
				case strings.Contains(line, "Registered tunnel connection"):
					m.mu.Lock()
					m.connCount++
					if m.status != StatusRunning {
						m.status = StatusRunning
						if m.cfg.TunnelURL != "" {
							m.url = m.cfg.TunnelURL
						}
						log.Printf("[TUNNEL] Named tunnel connected (%d conn)", m.connCount)
						if m.onURLChange != nil && m.url != "" {
							go m.onURLChange(m.url)
						}
					}
					m.mu.Unlock()
				case strings.Contains(line, "Unregistered tunnel connection"),
					strings.Contains(line, "Lost connection with the edge"),
					strings.Contains(line, "Register tunnel error"):
					m.mu.Lock()
					if m.connCount > 0 {
						m.connCount--
					}
					if m.connCount <= 0 && m.status == StatusRunning {
						m.status = StatusStarting // all edges gone — reconnecting
						log.Printf("[TUNNEL] All edge connections dropped — reconnecting")
					}
					m.mu.Unlock()
				}
			}
		}
		pr.Close()
	}()

	// Wait for process exit in background. This is the ONLY caller of cmd.Wait()
	// for this process; stopLocked waits on `exited` instead of calling Wait
	// again (a double Wait returns immediately and defeats the grace period).
	go func() {
		exitErr := cmd.Wait()
		m.mu.Lock()
		if m.cmd == cmd { // only if this is still the current process
			if exitErr != nil && tunnelCtx.Err() == nil {
				// Unexpected exit
				m.status = StatusError
				m.err = fmt.Errorf("cloudflared exited: %v", exitErr)
				log.Printf("[TUNNEL] cloudflared exited unexpectedly: %v", exitErr)
			} else {
				m.status = StatusStopped
				log.Printf("[TUNNEL] cloudflared stopped")
			}
			m.cmd = nil
			m.cancel = nil
		}
		m.mu.Unlock()
		close(exited)
	}()

	// Timeout: if quick tunnel doesn't produce URL in 30s, mark as error
	if mode == ModeQuick {
		go func() {
			time.Sleep(30 * time.Second)
			m.mu.Lock()
			defer m.mu.Unlock()
			if m.status == StatusStarting && m.cmd == cmd {
				m.status = StatusError
				m.err = fmt.Errorf("timeout waiting for tunnel URL")
				log.Printf("[TUNNEL] Timeout waiting for quick tunnel URL")
			}
		}()
	}

	return nil
}

// stop terminates cloudflared gracefully. It must NOT hold m.mu while waiting,
// because the Wait goroutine needs the lock to finalize status before closing
// `exited`. It waits on that channel instead of issuing a second cmd.Wait().
func (m *Manager) stop() error {
	m.mu.Lock()
	cancel := m.cancel
	cmd := m.cmd
	exited := m.exited
	m.cancel = nil
	m.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	if cmd != nil && cmd.Process != nil && exited != nil {
		select {
		case <-exited:
		case <-time.After(3 * time.Second):
			_ = cmd.Process.Kill()
			<-exited // the Wait goroutine will now return and close exited
		}
	}

	m.mu.Lock()
	m.status = StatusStopped
	m.url = ""
	m.err = nil
	m.cmd = nil
	m.mu.Unlock()
	return nil
}

// findCloudflared searches for cloudflared binary.
func findCloudflared() string {
	// 1. Check next to our executable
	exe, _ := os.Executable()
	if exe != "" {
		name := "cloudflared"
		if runtime.GOOS == "windows" {
			name = "cloudflared.exe"
		}
		local := filepath.Join(filepath.Dir(exe), name)
		if _, err := os.Stat(local); err == nil {
			return local
		}
	}
	// 2. Check PATH
	path, err := exec.LookPath("cloudflared")
	if err == nil {
		return path
	}
	if runtime.GOOS == "windows" {
		path, err = exec.LookPath("cloudflared.exe")
		if err == nil {
			return path
		}
	}
	return ""
}

// ── Ring buffer for log lines ───────────────────────────────────────

type ringBuffer struct {
	mu    sync.Mutex
	lines []string
	max   int
}

func newRingBuffer(max int) *ringBuffer {
	return &ringBuffer{lines: make([]string, 0, max), max: max}
}

func (rb *ringBuffer) Add(line string) {
	rb.mu.Lock()
	defer rb.mu.Unlock()
	if len(rb.lines) >= rb.max {
		rb.lines = rb.lines[1:]
	}
	rb.lines = append(rb.lines, line)
}

func (rb *ringBuffer) Lines() []string {
	rb.mu.Lock()
	defer rb.mu.Unlock()
	out := make([]string, len(rb.lines))
	copy(out, rb.lines)
	return out
}

func (rb *ringBuffer) Reset() {
	rb.mu.Lock()
	defer rb.mu.Unlock()
	rb.lines = rb.lines[:0]
}
