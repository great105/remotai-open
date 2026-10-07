package hermes

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

func (m *Manager) CheckUpdate(ctx context.Context) (err error) {
	if err = m.begin("checking_update"); err != nil {
		return err
	}
	defer func() { m.finish(err) }()
	ctx = m.operationContext(ctx)
	m.mu.Lock()
	managed := m.isManagedLocked()
	m.mu.Unlock()
	if !managed {
		return ErrExternal
	}
	out, err := m.privateRun(ctx, "check")
	if err != nil {
		return err
	}
	result, err := parseUpdateResult(string(out))
	if err != nil {
		return err
	}
	m.mu.Lock()
	m.state.UpdatePending = *result.Available
	m.state.LatestVersion = result.Version
	m.state.LatestCommit = result.Commit
	if result.CurrentCommit != "" {
		m.state.InstalledCommit = result.CurrentCommit
	}
	m.state.LastCheckedAt = time.Now().UTC().Format(time.RFC3339)
	err = m.saveLocked()
	m.mu.Unlock()
	return err
}

func updateVerdict(output string) (bool, string, error) {
	result, err := parseUpdateResult(output)
	if err != nil {
		return false, "", err
	}
	return *result.Available, result.Version, nil
}

type sourceUpdateResult struct {
	Available     *bool  `json:"available"`
	Version       string `json:"version"`
	Commit        string `json:"commit"`
	Branch        string `json:"branch"`
	CurrentCommit string `json:"current_commit"`
}

func parseUpdateResult(output string) (sourceUpdateResult, error) {
	for _, line := range strings.Split(output, "\n") {
		if !strings.HasPrefix(line, "REMOTAI_HERMES_UPDATE ") {
			continue
		}
		var result sourceUpdateResult
		if json.Unmarshal([]byte(strings.TrimPrefix(line, "REMOTAI_HERMES_UPDATE ")), &result) != nil || result.Available == nil || len(result.Commit) != 40 {
			return result, errors.New("неверный ответ официального канала Hermes")
		}
		for _, r := range result.Commit {
			if !strings.ContainsRune("0123456789abcdef", r) {
				return result, errors.New("неверный commit официального канала Hermes")
			}
		}
		if result.CurrentCommit != "" && !validCommit(result.CurrentCommit) {
			return result, errors.New("неверная версия установленного Hermes")
		}
		return result, nil
	}
	// A branch delivery is resolved to an exact official commit too. A
	// successful exit without that verdict never permits a checkout change.
	return sourceUpdateResult{}, errors.New("Hermes не подтвердил точную официальную версию; проверка будет повторена")
}

func (m *Manager) Update(ctx context.Context) (err error) {
	if err = m.begin("updating"); err != nil {
		return err
	}
	defer func() { m.finish(err) }()
	ctx = m.operationContext(ctx)
	m.mu.Lock()
	managed := m.isManagedLocked()
	running := m.process != nil
	ready := m.ready
	m.mu.Unlock()
	if !managed {
		return ErrExternal
	}
	// Prepare the owned compilers before retiring a live backend: older Intel
	// Mac installs can acquire a wheel-less dependency on their next update.
	if err = m.prepareNativeBuildTools(ctx); err != nil {
		return err
	}
	if running {
		if !ready {
			return m.deferUpdate()
		}
		if err = m.retireForUpdate(ctx); err != nil {
			return err
		}
		if err = m.stop(ctx); err != nil {
			return err
		}
	}
	// Do not copy a live SQLite/WAL. The owned backend has been fenced against
	// new work and stopped before this local recovery snapshot is created.
	if _, err = m.snapshot(ctx); err != nil {
		return fmt.Errorf("обновление отменено: резервная копия Hermes не создана: %w", err)
	}
	if _, err = m.privateRun(ctx, "cli", "update", "--channel", "main", "--yes", "--backup", "--no-gateway-restart"); err != nil {
		// The official updater verifies migrations and new runtime. Never
		// silently restart old code against a possibly upgraded state schema.
		return fmt.Errorf("Hermes не завершил обновление; резервная копия сохранена, повторите установку для восстановления: %w", err)
	}
	revisionOut, revisionErr := m.privateRun(ctx, "revision")
	if revisionErr != nil {
		return revisionErr
	}
	commit := ""
	for _, line := range strings.Split(string(revisionOut), "\n") {
		if strings.HasPrefix(line, "REMOTAI_HERMES_REVISION ") {
			commit = strings.TrimSpace(strings.TrimPrefix(line, "REMOTAI_HERMES_REVISION "))
			break
		}
	}
	if !validCommit(commit) {
		return errors.New("Hermes не подтвердил commit после обновления; повторите установку")
	}
	m.mu.Lock()
	m.detectLocked()
	m.state.InstalledCommit = commit
	m.state.UpdatePending = false
	m.state.LastCheckedAt = time.Now().UTC().Format(time.RFC3339)
	err = m.saveLocked()
	m.mu.Unlock()
	if err != nil {
		return err
	}
	if running {
		return m.start(ctx)
	}
	m.refreshVersion(ctx)
	return nil
}

func validCommit(commit string) bool {
	if len(commit) != 40 {
		return false
	}
	for _, r := range commit {
		if !strings.ContainsRune("0123456789abcdef", r) {
			return false
		}
	}
	return true
}

func (m *Manager) deferUpdate() error {
	m.mu.Lock()
	m.state.UpdatePending = true
	_ = m.saveLocked()
	m.mu.Unlock()
	return ErrDeferred
}

func (m *Manager) retireForUpdate(ctx context.Context) error {
	request := func(action, token string) (map[string]json.RawMessage, error) {
		body, _ := json.Marshal(map[string]string{"action": action, "token": token})
		resp, err := m.doBackend(ctx, http.MethodPost, "/api/health/retirement", bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, ErrDeferred
		}
		var data map[string]json.RawMessage
		err = json.NewDecoder(io.LimitReader(resp.Body, 64*1024)).Decode(&data)
		return data, err
	}
	prepared, err := request("prepare", "")
	if err != nil {
		return m.deferUpdate()
	}
	var idle, ok bool
	var token string
	_ = json.Unmarshal(prepared["idle"], &idle)
	_ = json.Unmarshal(prepared["ok"], &ok)
	_ = json.Unmarshal(prepared["token"], &token)
	if !ok || !idle || token == "" {
		return m.deferUpdate()
	}
	committed := false
	defer func() {
		if !committed {
			cleanup, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			body, _ := json.Marshal(map[string]string{"action": "cancel", "token": token})
			resp, e := m.doBackend(cleanup, http.MethodPost, "/api/health/retirement", bytes.NewReader(body))
			if e == nil {
				resp.Body.Close()
			}
		}
	}()
	result, err := request("commit", token)
	if err != nil {
		return m.deferUpdate()
	}
	ok = false
	_ = json.Unmarshal(result["ok"], &ok)
	if !ok {
		return m.deferUpdate()
	}
	committed = true
	return nil
}

func (m *Manager) snapshot(ctx context.Context) (string, error) {
	base := filepath.Join(m.root, "backups")
	if err := os.MkdirAll(base, 0700); err != nil {
		return "", err
	}
	dst, err := os.MkdirTemp(base, "before-update-")
	if err != nil {
		return "", err
	}
	// PM tools and immutable dependency generations are not user state. Their
	// selection remains managed by PM and its own transactional update guard.
	skip := map[string]bool{"installs": true, "tools": true, "logs": true, "image_cache": true, "audio_cache": true, "backups": true}
	err = filepath.WalkDir(m.home, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			if errors.Is(walkErr, os.ErrNotExist) && path == m.home {
				return nil
			}
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		rel, err := filepath.Rel(m.home, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		parts := strings.Split(rel, string(filepath.Separator))
		if d.IsDir() && len(parts) == 1 && skip[d.Name()] {
			return filepath.SkipDir
		}
		target := filepath.Join(dst, "home", rel)
		if !within(dst, target) {
			return errors.New("неверный путь резервной копии")
		}
		if d.IsDir() {
			return os.MkdirAll(target, 0700)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
			return err
		}
		if d.Type()&os.ModeSymlink != 0 {
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			return os.Symlink(link, target)
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("неподдерживаемый файл резервной копии: %s", rel)
		}
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(out, in)
		closeErr := out.Close()
		if copyErr != nil {
			return copyErr
		}
		return closeErr
	})
	if err != nil {
		_ = removeOwnedBackup(base, dst)
		return dst, err
	}
	m.mu.Lock()
	version := m.state.Version
	m.mu.Unlock()
	meta, _ := json.Marshal(map[string]string{"version": version, "created_at": time.Now().UTC().Format(time.RFC3339), "recovery": "State snapshot only; old code must not open a newer schema without the official restore procedure."})
	if err = os.WriteFile(filepath.Join(dst, "snapshot.json"), meta, 0600); err != nil {
		_ = removeOwnedBackup(base, dst)
		return dst, err
	}
	if err = m.pruneSnapshots(base); err != nil {
		return dst, err
	}
	return dst, nil
}

func removeOwnedBackup(base, path string) error {
	baseReal, err := filepath.EvalSymlinks(base)
	if err != nil {
		return err
	}
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return err
	}
	if !within(baseReal, real) || filepath.Clean(real) == filepath.Clean(baseReal) || !strings.HasPrefix(filepath.Base(real), "before-update-") {
		return errors.New("путь резервной копии выходит за собственный каталог")
	}
	return os.RemoveAll(real)
}

func (m *Manager) pruneSnapshots(base string) error {
	entries, err := os.ReadDir(base)
	if err != nil {
		return err
	}
	type snapshot struct {
		path     string
		modified time.Time
	}
	var snapshots []snapshot
	for _, entry := range entries {
		if !entry.IsDir() || entry.Type()&os.ModeSymlink != 0 || !strings.HasPrefix(entry.Name(), "before-update-") {
			continue
		}
		path := filepath.Join(base, entry.Name())
		if !isFile(filepath.Join(path, "snapshot.json")) {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		snapshots = append(snapshots, snapshot{path: path, modified: info.ModTime()})
	}
	sort.Slice(snapshots, func(i, j int) bool { return snapshots[i].modified.After(snapshots[j].modified) })
	for i := 5; i < len(snapshots); i++ {
		if err := removeOwnedBackup(base, snapshots[i].path); err != nil {
			return err
		}
	}
	return nil
}
