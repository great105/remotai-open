package hermes

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"tgcontrol/internal/procutil"
)

type backendProcess struct {
	cmd    *exec.Cmd
	cancel context.CancelFunc
	done   chan struct{}
}

func (m *Manager) command(ctx context.Context, name string, args ...string) *exec.Cmd {
	cmd := m.opts.command(ctx, name, args...)
	procutil.Hidden(cmd)
	procutil.Prepare(cmd)
	cmd.Dir = m.checkout
	cmd.Env = m.environment(false, "")
	return cmd
}

func (m *Manager) environment(install bool, token string) []string {
	// Hermes reads the dedicated profile's credentials itself. Inherited
	// provider keys, alternate profiles and install overrides would silently
	// adopt another user's account or code, so do not forward them.
	out := make([]string, 0, len(os.Environ())+10)
	for _, pair := range os.Environ() {
		key, _, _ := strings.Cut(pair, "=")
		upper := strings.ToUpper(key)
		if strings.HasPrefix(upper, "HERMES_") || strings.HasPrefix(upper, "GIT_CONFIG_") || upper == "GIT_ASKPASS" || upper == "GIT_SSH_COMMAND" || upper == "GIT_TERMINAL_PROMPT" || strings.HasSuffix(upper, "_API_KEY") || strings.HasSuffix(upper, "_TOKEN") || upper == "API_KEY" || upper == "GOOGLE_APPLICATION_CREDENTIALS" || upper == "PYTHONPATH" || upper == "PYTHONHOME" || upper == "PYTHONUNBUFFERED" || upper == "PYTHONUTF8" || upper == "VIRTUAL_ENV" {
			continue
		}
		out = append(out, pair)
	}
	out = append(out, "HERMES_HOME="+m.home, "HERMES_RUNTIME_DIR="+filepath.Join(m.root, "toolchain", "store"), "PYTHONUNBUFFERED=1", "PYTHONUTF8=1")
	// Windows' Git defaults still reject long upstream documentation filenames.
	// This override belongs to the child process, never the user's Git config.
	out = append(out, "GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=core.longpaths", "GIT_CONFIG_VALUE_0=true", "GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+filepath.Join(m.root, "toolchain", "gitconfig"))
	if token != "" {
		// Upstream starts its owned-backend cron ticker only for this authenticated
		// desktop identity. Installer and metadata subprocesses never receive it.
		out = append(out, "HERMES_DESKTOP=1", "HERMES_DASHBOARD_SESSION_TOKEN="+token, "HERMES_PARENT_PID="+strconv.Itoa(os.Getpid()))
	}
	if install {
		out = append(out, "UV_CACHE_DIR="+filepath.Join(m.root, "toolchain", "uv-cache"), "UV_PYTHON_INSTALL_DIR="+filepath.Join(m.root, "toolchain", "python"))
	}
	return out
}

func (m *Manager) run(ctx context.Context, args ...string) ([]byte, error) {
	m.mu.Lock()
	binary := m.binary
	m.mu.Unlock()
	if binary == "" {
		return nil, ErrNotInstalled
	}
	cmd, err := m.binaryCommand(ctx, binary, args...)
	if err != nil {
		return nil, err
	}
	out, err := m.capture(cmd, "command")
	if err != nil {
		return nil, fmt.Errorf("команда Hermes не выполнена (%w)", err)
	}
	return out, nil
}

func (m *Manager) binaryCommand(ctx context.Context, binary string, args ...string) (*exec.Cmd, error) {
	m.mu.Lock()
	owned := m.state.Managed && within(m.checkout, binary)
	m.mu.Unlock()
	if owned {
		return m.privateCommand(ctx, "cli", args...)
	}
	return nil, ErrExternal
}

func (m *Manager) Start(ctx context.Context) (err error) {
	if err = m.begin("starting"); err != nil {
		return err
	}
	defer func() { m.finish(err) }()
	ctx = m.operationContext(ctx)
	return m.start(ctx)
}

func (m *Manager) start(ctx context.Context) error {
	m.mu.Lock()
	if m.closing {
		m.mu.Unlock()
		return ErrClosed
	}
	if m.ready && m.process != nil {
		m.mu.Unlock()
		return nil
	}
	if m.process != nil {
		m.mu.Unlock()
		return ErrBusy
	}
	m.detectLocked()
	binary := m.binary
	managed := m.isManagedLocked()
	m.mu.Unlock()
	if binary == "" {
		return ErrNotInstalled
	}
	if !managed {
		return ErrExternal
	}
	if err := m.ensurePrivateProfile(); err != nil {
		return err
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return err
	}
	token := base64.RawURLEncoding.EncodeToString(secret)
	// The request deadline governs readiness, not the lifespan of a successfully
	// started backend. The application owns Stop; losing a client leaves work alive.
	life, cancel := context.WithCancel(context.Background())
	cmd, err := m.binaryCommand(life, binary, "serve", "--host", "127.0.0.1", "--port", "0")
	if err != nil {
		cancel()
		return err
	}
	cmd.Env = m.environment(false, token)
	if !isFile(filepath.Join(m.checkout, "pyproject.toml")) {
		cmd.Dir = m.home
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		cancel()
		return err
	}
	// Fence the actual spawn, not just the supervisor's earlier policy read.
	// Disable/Close cannot return between this check and publishing the process.
	m.mu.Lock()
	supervised, _ := ctx.Value(supervisedStartKey{}).(bool)
	if m.closing || ctx.Err() != nil || supervised && !m.state.AutoStart {
		m.mu.Unlock()
		stdout.Close()
		stderr.Close()
		cancel()
		return ErrDeferred
	}
	if err = cmd.Start(); err != nil {
		m.mu.Unlock()
		cancel()
		return fmt.Errorf("не удалось запустить Hermes: %w", err)
	}
	p := &backendProcess{cmd: cmd, cancel: cancel, done: make(chan struct{})}
	m.process = p
	m.token = token
	m.ready = false
	m.epoch++
	m.epochStart = m.seq
	m.resetEventsLocked()
	closing := m.closing
	m.mu.Unlock()
	if closing {
		cancel()
	}
	ports := make(chan int, 1)
	failures := make(chan error, 2)
	scan := func(r io.Reader) {
		s := bufio.NewScanner(r)
		s.Buffer(make([]byte, 4096), 1024*1024)
		for s.Scan() {
			line := strings.TrimSpace(s.Text())
			if port, matched, parseErr := readinessPort(line); matched {
				if parseErr != nil {
					select {
					case failures <- parseErr:
					default:
					}
				} else {
					select {
					case ports <- port:
					default:
					}
				}
			}
		}
	}
	go scan(stdout)
	go scan(stderr)
	go func() {
		defer close(p.done)
		waitErr := cmd.Wait()
		m.mu.Lock()
		if m.process == p {
			m.process = nil
			m.ready = false
			m.baseURL = ""
			m.resetEventsLocked()
			if waitErr != nil && life.Err() == nil {
				m.state.LastError = "Процесс Hermes завершился; запустите его снова"
				_ = m.saveLocked()
			}
			bridge := m.bridge
			m.bridge = nil
			m.mu.Unlock()
			if bridge != nil {
				bridge.close()
			}
		} else {
			m.mu.Unlock()
		}
	}()
	wait, cancelWait := context.WithTimeout(ctx, m.opts.StartupTimeout)
	defer cancelWait()
	var port int
	select {
	case port = <-ports:
	case err = <-failures:
		cancel()
		<-p.done
		return err
	case <-p.done:
		return errors.New("Hermes завершился до готовности; проверьте установку")
	case <-wait.Done():
		cancel()
		<-p.done
		return fmt.Errorf("Hermes не успел запуститься: %w", wait.Err())
	}
	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	for {
		probe, probeCancel := context.WithTimeout(wait, 3*time.Second)
		req, _ := http.NewRequestWithContext(probe, http.MethodGet, base+"/api/health", nil)
		req.Header.Set("X-Hermes-Session-Token", token)
		resp, probeErr := m.httpClient().Do(req)
		var health struct {
			OK             bool   `json:"ok"`
			Version        string `json:"version"`
			DisplayVersion string `json:"displayVersion"`
		}
		if probeErr == nil {
			probeErr = json.NewDecoder(io.LimitReader(resp.Body, 64*1024)).Decode(&health)
			resp.Body.Close()
			if resp.StatusCode != 200 || !health.OK {
				probeErr = ErrNotReady
			}
		}
		probeCancel()
		if probeErr == nil {
			m.mu.Lock()
			if m.process != p {
				m.mu.Unlock()
				return errors.New("Hermes завершился во время проверки")
			}
			m.baseURL = base
			m.ready = true
			m.state.Version = health.Version
			_ = m.saveLocked()
			m.mu.Unlock()
			return nil
		}
		select {
		case <-wait.Done():
			cancel()
			<-p.done
			return fmt.Errorf("Hermes не прошёл проверку готовности: %w", wait.Err())
		case <-p.done:
			return errors.New("Hermes завершился во время проверки")
		case <-time.After(150 * time.Millisecond):
		}
	}
}

func (m *Manager) ensurePrivateProfile() error {
	if err := os.MkdirAll(m.home, 0700); err != nil {
		return err
	}
	path := filepath.Join(m.home, "config.yaml")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if errors.Is(err, os.ErrExist) {
		return nil
	}
	if err != nil {
		return err
	}
	_, writeErr := f.WriteString("# Remotai owns this isolated profile. External CLI logins require explicit adoption.\nauth:\n  adopt_external_logins: false\n")
	closeErr := f.Close()
	if writeErr != nil {
		return writeErr
	}
	return closeErr
}

func readinessPort(line string) (int, bool, error) {
	for _, prefix := range []string{"HERMES_BACKEND_READY port=", "HERMES_DASHBOARD_READY port="} {
		if strings.HasPrefix(line, prefix) {
			p, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, prefix)))
			if err != nil || p < 1 || p > 65535 {
				return 0, true, errors.New("Hermes сообщил некорректный порт")
			}
			return p, true, nil
		}
	}
	if strings.HasPrefix(line, "BACKEND_PORT_IN_USE port=") {
		return 0, true, errors.New("Порт backend Hermes занят")
	}
	return 0, false, nil
}

func (m *Manager) Stop(ctx context.Context) (err error) {
	// An explicit stop is intentional, unlike an internal update/crash stop.
	if err = m.SetAutoStart(false); err != nil {
		return err
	}
	if err = m.begin("stopping"); err != nil {
		return err
	}
	defer func() { m.finish(err) }()
	ctx = m.operationContext(ctx)
	return m.stop(ctx)
}

func (m *Manager) stop(ctx context.Context) error {
	m.mu.Lock()
	p := m.process
	bridge := m.bridge
	m.bridge = nil
	m.ready = false
	m.baseURL = ""
	m.resetEventsLocked()
	m.mu.Unlock()
	if bridge != nil {
		bridge.close()
	}
	if p == nil {
		return nil
	}
	p.cancel()
	select {
	case <-p.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (m *Manager) httpClient() *http.Client {
	client := *m.opts.HTTPClient
	// A local backend redirect must never take its private token to another host.
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &client
}

func (m *Manager) Do(ctx context.Context, method, path string, body io.Reader) (*http.Response, error) {
	m.mu.Lock()
	busy := m.state.Operation == "updating" || m.state.Operation == "installing"
	m.mu.Unlock()
	if busy {
		return nil, ErrBusy
	}
	return m.doBackend(ctx, method, path, body)
}

func (m *Manager) doBackend(ctx context.Context, method, path string, body io.Reader) (*http.Response, error) {
	u, err := url.Parse(path)
	if err != nil || u.IsAbs() || u.Host != "" || !strings.HasPrefix(u.Path, "/api/") {
		return nil, errors.New("неверный путь Hermes API")
	}
	for _, part := range strings.Split(u.Path, "/") {
		if part == ".." {
			return nil, errors.New("неверный путь Hermes API")
		}
	}
	m.mu.Lock()
	base, token, ready := m.baseURL, m.token, m.ready
	m.mu.Unlock()
	if !ready || base == "" {
		return nil, ErrNotReady
	}
	req, err := http.NewRequestWithContext(ctx, method, base+u.String(), body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Hermes-Session-Token", token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return m.httpClient().Do(req)
}
