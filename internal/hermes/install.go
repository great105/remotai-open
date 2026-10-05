package hermes

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

//go:embed private_entry.py
var privateEntry []byte

func (m *Manager) Install(ctx context.Context) (err error) {
	if err = m.begin("installing"); err != nil {
		return err
	}
	defer func() { m.finish(err) }()
	ctx = m.operationContext(ctx)
	m.mu.Lock()
	running := m.process != nil
	owned := m.state.Managed
	m.mu.Unlock()
	if running {
		return ErrBusy
	}
	if !owned {
		if entries, readErr := os.ReadDir(m.checkout); readErr == nil && len(entries) > 0 {
			return errors.New("Каталог установки занят; Remotai не изменит чужие файлы")
		}
	}
	for _, dir := range []string{m.home, filepath.Join(m.root, "toolchain"), filepath.Join(m.root, "bootstrap")} {
		if err = os.MkdirAll(dir, 0700); err != nil {
			return err
		}
	}
	if err = m.ensurePrivateProfile(); err != nil {
		return err
	}
	m.mu.Lock()
	m.state.Managed = true
	err = m.saveLocked()
	m.mu.Unlock()
	if err != nil {
		return err
	}
	ext := "sh"
	url := "https://hermes-agent.nousresearch.com/install.sh"
	if m.opts.goos == "windows" {
		ext = "ps1"
		url = "https://hermes-agent.nousresearch.com/install.ps1"
	}
	script := filepath.Join(m.root, "bootstrap", "install."+ext)
	m.progress("Скачиваем официальный установщик Hermes")
	if err = m.download(ctx, url, script); err != nil {
		return fmt.Errorf("не удалось скачать официальный установщик Hermes: %w", err)
	}
	// A full official install publishes a user-wide launcher and edits PATH or
	// shell profiles. Execute its supported stages, keeping all dependencies,
	// and run its completion through the narrowly scoped private wrapper.
	for _, stage := range []string{"prerequisites", "repository", "venv", "python-deps"} {
		if err = m.installStage(ctx, script, stage); err != nil {
			return err
		}
	}
	if err = m.writePrivateEntry(); err != nil {
		return err
	}
	// Resolve the official main source commit with the upstream channel reader
	// reader BEFORE completing the install or opening/migrating a session DB.
	// The main checkout is only a bootstrap for this reader and its toolchain.
	var target sourceUpdateResult
	var result []byte
	if result, err = m.privateRun(ctx, "check"); err != nil {
		return err
	}
	if target, err = parseUpdateResult(string(result)); err != nil {
		return err
	}
	for _, stage := range []string{"repository", "venv", "python-deps", "config"} {
		if err = m.installStage(ctx, script, stage, target.Commit, target.Branch); err != nil {
			return err
		}
	}
	if _, err = m.privateRun(ctx, "complete"); err != nil {
		return fmt.Errorf("не удалось завершить установку Hermes: %w", err)
	}
	if _, err = m.privateRun(ctx, "cli", "update", "--set-channel", "main"); err != nil {
		return err
	}
	if err = m.installStage(ctx, script, "complete", target.Commit, target.Branch); err != nil {
		return err
	}
	m.mu.Lock()
	m.detectLocked()
	m.state.InstalledCommit = target.Commit
	m.state.LatestCommit = target.Commit
	m.state.LatestVersion = target.Version
	_ = m.saveLocked()
	found := m.isManagedLocked()
	m.mu.Unlock()
	if !found {
		return errors.New("Установщик завершился без команды Hermes в собственном каталоге")
	}
	m.refreshVersion(ctx)
	return nil
}

func (m *Manager) download(ctx context.Context, address, dst string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "Remotai-Hermes")
	client := *m.opts.HTTPClient
	client.CheckRedirect = func(next *http.Request, via []*http.Request) error {
		if next.URL.Scheme != "https" || next.URL.Host != req.URL.Host {
			return errors.New("официальный установщик перенаправлен на неподтверждённый сервер")
		}
		if len(via) >= 5 {
			return errors.New("слишком много перенаправлений установщика")
		}
		return nil
	}
	resp, err := client.Do(req)
	if err != nil {
		return errors.New("официальный сервер установки недоступен")
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("официальный сервер ответил HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 2*1024*1024+1))
	if err != nil {
		return err
	}
	if len(data) == 0 || len(data) > 2*1024*1024 {
		return errors.New("неверный размер установщика")
	}
	return os.WriteFile(dst, data, 0600)
}

func (m *Manager) installStage(ctx context.Context, script, stage string, commit ...string) error {
	m.progress(map[string]string{"prerequisites": "Проверяем компьютер", "repository": "Скачиваем Hermes", "venv": "Готовим Python", "python-deps": "Устанавливаем зависимости Hermes", "config": "Создаём отдельный профиль", "complete": "Проверяем установку"}[stage])
	var cmd *exec.Cmd
	if m.opts.goos == "windows" {
		shell := "powershell.exe"
		if p, err := m.opts.lookPath("pwsh.exe"); err == nil {
			shell = p
		}
		cmd = m.command(ctx, shell, "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", script, "-NonInteractive", "-HermesHome", m.home, "-InstallDir", m.checkout, "-Stage", stage, "-Json")
	} else {
		cmd = m.command(ctx, "bash", script, "--non-interactive", "--dir", m.checkout, "--stage", stage, "--json")
	}
	if len(commit) > 0 && commit[0] != "" {
		if m.opts.goos == "windows" {
			cmd.Args = append(cmd.Args, "-Commit", commit[0])
		} else {
			cmd.Args = append(cmd.Args, "--commit", commit[0])
		}
	}
	if len(commit) > 1 && commit[1] != "" {
		if m.opts.goos == "windows" {
			cmd.Args = append(cmd.Args, "-Branch", commit[1])
		} else {
			cmd.Args = append(cmd.Args, "--branch", commit[1])
		}
	}
	cmd.Dir = m.root
	cmd.Env = m.environment(true, "")
	if _, err := m.capture(cmd, "install-"+stage); err != nil {
		return fmt.Errorf("%s: этап установки не завершился; проверьте интернет и повторите установку (%w)", m.Status().OperationDetail, err)
	}
	return nil
}

func (m *Manager) writePrivateEntry() error {
	return os.WriteFile(filepath.Join(m.root, "private_entry.py"), privateEntry, 0600)
}

func (m *Manager) pythonPath() (string, error) {
	store := filepath.Join(m.root, "toolchain", "store")
	data, err := os.ReadFile(filepath.Join(store, "facts.json"))
	if err != nil {
		return "", errors.New("Hermes не записал свой интерпретатор Python; повторите установку")
	}
	var facts struct {
		Packages map[string]struct {
			Entry string `json:"entry"`
		} `json:"packages"`
	}
	if err = json.Unmarshal(data, &facts); err != nil {
		return "", errors.New("не читается запись Python Hermes")
	}
	entry := facts.Packages["python"].Entry
	if entry == "" {
		return "", errors.New("не найден собственный Python Hermes")
	}
	path := filepath.Join(store, entry, "bin", "python3")
	if m.opts.goos == "windows" {
		path = filepath.Join(store, entry, "python.exe")
	}
	if !within(store, path) || !isFile(path) {
		return "", errors.New("Python Hermes отсутствует или находится вне собственной установки")
	}
	return path, nil
}

func (m *Manager) privateCommand(ctx context.Context, mode string, args ...string) (*exec.Cmd, error) {
	python, err := m.pythonPath()
	if err != nil {
		return nil, err
	}
	if err = m.writePrivateEntry(); err != nil {
		return nil, err
	}
	argv := append([]string{"-I", "-B", "-u", filepath.Join(m.root, "private_entry.py"), m.checkout, mode}, args...)
	cmd := m.command(ctx, python, argv...)
	cmd.Env = m.environment(true, "")
	return cmd, nil
}

func (m *Manager) privateRun(ctx context.Context, mode string, args ...string) ([]byte, error) {
	if mode == "check" {
		m.progress("Проверяем основной канал Hermes")
	}
	if mode == "complete" {
		m.progress("Подготавливаем Hermes к первому запуску")
	}
	cmd, err := m.privateCommand(ctx, mode, args...)
	if err != nil {
		return nil, err
	}
	out, err := m.capture(cmd, "operation-"+mode)
	if err != nil {
		return nil, fmt.Errorf("официальная операция Hermes %s завершилась ошибкой: %w", mode, err)
	}
	return out, nil
}

func (m *Manager) refreshVersion(ctx context.Context) {
	out, err := m.run(ctx, "--version")
	if err != nil {
		return
	}
	version := strings.TrimSpace(string(out))
	if len(version) > 100 {
		return
	}
	m.mu.Lock()
	m.state.Version = version
	_ = m.saveLocked()
	m.mu.Unlock()
}
