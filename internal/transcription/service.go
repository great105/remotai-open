// Package transcription runs local ASR outside the lifetime of relay requests.
package transcription

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"

	"tgcontrol/internal/agentdesk"
)

const MaxAudioBytes int64 = 25 << 20

var ErrBusy = errors.New("распознавание уже выполняется на этом компьютере")
var ErrNotFound = errors.New("запись или задача не найдена")
var modelIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{0,63}$`)

type Worker interface {
	Call(context.Context, string, any, any) error
	Close() error
}

type Status struct {
	Available  bool   `json:"available"`
	Executable string `json:"executable,omitempty"`
	Platform   string `json:"platform"`
	MaxBytes   int64  `json:"max_bytes"`
	Active     *Job   `json:"active,omitempty"`
}

type Job struct {
	ID       string          `json:"id"`
	State    string          `json:"state"`
	Text     string          `json:"text,omitempty"`
	Error    string          `json:"error,omitempty"`
	Duration float64         `json:"duration,omitempty"`
	Device   string          `json:"device,omitempty"`
	Kind     string          `json:"kind,omitempty"`
	Result   json.RawMessage `json:"result,omitempty"`
	uid      int64
	cancel   context.CancelFunc
	finished time.Time
}

type upload struct {
	uid int64
	at  time.Time
}

// Only one worker stays warm, even if several identities use this computer.
// Different identities have different SQLite histories and allowed roots.
type Service struct {
	mu                            sync.Mutex
	root, uploadsRoot, executable string
	jobs                          map[string]*Job
	uploads                       map[string]upload
	active                        *Job
	worker                        Worker
	workerUID                     int64
	idle                          *time.Timer
	warmRevision                  uint64
	closing                       bool
	wg                            sync.WaitGroup
	newWorker                     func(agentdesk.Config) (Worker, error)
}

func LocalRoot() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "", errors.New("не удалось определить папку локального распознавания")
	}
	switch runtime.GOOS {
	case "windows":
		return filepath.Join(home, "AppData", "Local", "Remotai", "transcription"), nil
	case "darwin":
		return filepath.Join(home, "Library", "Application Support", "Remotai", "transcription"), nil
	default:
		return filepath.Join(home, ".local", "share", "remotai", "transcription"), nil
	}
}

func New(root, uploadsRoot string) *Service {
	s := &Service{root: root, uploadsRoot: uploadsRoot, jobs: make(map[string]*Job), uploads: make(map[string]upload),
		newWorker: func(c agentdesk.Config) (Worker, error) { return agentdesk.New(c) }}
	// This file contains only the user-selected bridge path, never API credentials.
	var saved struct {
		Executable string `json:"executable"`
	}
	if data, err := os.ReadFile(filepath.Join(root, "connection.json")); err == nil && json.Unmarshal(data, &saved) == nil {
		s.executable = saved.Executable
	}
	return s
}

func regularFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

func (s *Service) executableLocked() string {
	if s.executable != "" {
		if regularFile(s.executable) {
			return s.executable
		}
		return ""
	}
	if path := os.Getenv("REMOTAI_AGENTDESK_EXE"); filepath.IsAbs(path) && regularFile(path) {
		return path
	}
	if runtime.GOOS != "windows" {
		return ""
	}
	home, _ := os.UserHomeDir()
	candidates := []string{filepath.Join(s.root, "AgentDeskBridge.exe"), filepath.Join(home, "AppData", "Local", "AgentDesk", "AgentDeskBridge.exe")}
	if exe, err := os.Executable(); err == nil {
		candidates = append(candidates, filepath.Join(filepath.Dir(exe), "AgentDeskBridge.exe"))
	}
	for _, path := range candidates {
		if regularFile(path) {
			return path
		}
	}
	return ""
}

func (s *Service) Status(uid ...int64) Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	exe := s.executableLocked()
	status := Status{Available: exe != "", Executable: exe, Platform: runtime.GOOS, MaxBytes: MaxAudioBytes}
	if len(uid) > 0 && s.active != nil && s.active.uid == uid[0] {
		job := *s.active
		status.Active = &job
	}
	return status
}

// Configure is explicitly selected by the owner. No shell strings or arguments
// come from the phone; only the prepared Windows bridge executable is accepted.
func (s *Service) Configure(path string) error {
	path = strings.TrimSpace(path)
	if runtime.GOOS != "windows" {
		return errors.New("готовый модуль AgentDeskBridge поддерживает Windows")
	}
	if !filepath.IsAbs(path) || !strings.EqualFold(filepath.Base(path), "AgentDeskBridge.exe") || !regularFile(path) {
		return errors.New("выберите существующий AgentDeskBridge.exe на этом компьютере")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closing {
		return errors.New("распознавание остановлено")
	}
	if s.active != nil {
		return ErrBusy
	}
	if err := os.MkdirAll(s.root, 0o700); err != nil {
		return err
	}
	data, _ := json.Marshal(struct {
		Executable string `json:"executable"`
	}{path})
	temp := filepath.Join(s.root, "connection.json.tmp")
	if err := os.WriteFile(temp, data, 0o600); err != nil {
		return err
	}
	if err := os.Rename(temp, filepath.Join(s.root, "connection.json")); err != nil {
		return err
	}
	s.releaseWorkerLocked()
	s.executable = path
	return nil
}

// Upload paths are issued by the existing authenticated, completed upload.
// Arbitrary paths, other identities' files and late/incomplete uploads are denied.
func (s *Service) RecordUpload(uid int64, path string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked()
	if !s.closing {
		s.uploads[filepath.Clean(path)] = upload{uid, time.Now()}
	}
}

func (s *Service) pruneLocked() {
	cutoff := time.Now().Add(-15 * time.Minute)
	for path, file := range s.uploads {
		if file.at.Before(cutoff) {
			delete(s.uploads, path)
		}
	}
	for id, job := range s.jobs {
		if !job.finished.IsZero() && job.finished.Before(cutoff) {
			delete(s.jobs, id)
		}
	}
}

func inside(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

func (s *Service) Start(uid int64, path, model, language, id string) (Job, error) {
	if !modelIDPattern.MatchString(model) {
		return Job{}, errors.New("неизвестная речевая модель")
	}
	if language != "ru" && language != "en" && language != "auto" {
		return Job{}, errors.New("неизвестный язык записи")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked()
	if id != "" {
		decoded, err := hex.DecodeString(id)
		if err != nil || len(decoded) != 16 {
			return Job{}, errors.New("неверный идентификатор записи")
		}
		if existing := s.jobs[id]; existing != nil {
			if existing.uid != uid {
				return Job{}, ErrNotFound
			}
			return *existing, nil
		}
	}
	if s.closing {
		return Job{}, errors.New("распознавание остановлено")
	}
	if s.active != nil {
		return Job{}, ErrBusy
	}
	path = filepath.Clean(path)
	file, owned := s.uploads[path]
	if !owned || file.uid != uid {
		return Job{}, ErrNotFound
	}
	resolved, err := filepath.EvalSymlinks(path)
	root, rootErr := filepath.EvalSymlinks(s.uploadsRoot)
	if err != nil || rootErr != nil || !inside(root, resolved) {
		return Job{}, ErrNotFound
	}
	info, err := os.Stat(resolved)
	if err != nil || !info.Mode().IsRegular() || info.Size() == 0 || info.Size() > MaxAudioBytes {
		return Job{}, errors.New("запись должна быть от 1 байта до 25 МБ")
	}
	exe := s.executableLocked()
	if exe == "" {
		return Job{}, errors.New("подключите локальный модуль AgentDeskBridge на выбранном компьютере")
	}
	if id == "" {
		var random [16]byte
		if _, err := rand.Read(random[:]); err != nil {
			return Job{}, err
		}
		id = hex.EncodeToString(random[:])
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	job := &Job{ID: id, State: "running", uid: uid, cancel: cancel}
	worker, err := s.workerLocked(uid, exe)
	if err != nil {
		cancel()
		return Job{}, err
	}
	s.jobs[id], s.active = job, job
	delete(s.uploads, path) // An uploaded recording starts once, even on a retried POST.
	s.wg.Add(1)
	go s.run(ctx, worker, job, resolved, model, language)
	return *job, nil
}

func (s *Service) run(ctx context.Context, worker Worker, job *Job, path, model, language string) {
	defer s.wg.Done()
	defer job.cancel()
	var result agentdesk.Transcript
	err := worker.Call(ctx, "audio.transcribe", map[string]any{"path": path, "model": model, "language": language}, &result)
	s.mu.Lock()
	defer s.mu.Unlock()
	if job.State != "cancelled" {
		if err != nil {
			job.State, job.Error = "failed", fmt.Sprintf("%v", err)
		} else {
			job.State, job.Text, job.Duration, job.Device = "done", result.Text, result.Duration, result.Device
		}
	}
	job.finished = time.Now()
	s.active = nil
	s.armIdleLocked()
}

func (s *Service) armIdleLocked() {
	if !s.closing {
		revision := s.warmRevision
		s.idle = time.AfterFunc(5*time.Minute, func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			if s.active == nil && s.warmRevision == revision {
				s.releaseWorkerLocked()
			}
		})
	}
}

func (s *Service) Get(uid int64, id string) (Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked()
	job := s.jobs[id]
	if job == nil || job.uid != uid {
		return Job{}, ErrNotFound
	}
	return *job, nil
}

func (s *Service) Cancel(uid int64, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	job := s.jobs[id]
	if job == nil {
		decoded, err := hex.DecodeString(id)
		if err != nil || len(decoded) != 16 {
			return ErrNotFound
		}
		s.pruneLocked()
		if len(s.jobs) >= 100 {
			return ErrNotFound
		}
		// Cancellation may reach the server before the short start response.
		// A late POST for this identity must not start a discarded recording.
		s.jobs[id] = &Job{ID: id, uid: uid, State: "cancelled", finished: time.Now()}
		return nil
	}
	if job.uid != uid {
		return ErrNotFound
	}
	if job.State == "running" {
		job.State = "cancelled"
		job.cancel()
	}
	return nil
}

func (s *Service) releaseWorkerLocked() {
	if s.idle != nil {
		s.idle.Stop()
		s.idle = nil
	}
	if s.worker != nil {
		_ = s.worker.Close()
		s.worker = nil
	}
}

func (s *Service) Close() {
	s.mu.Lock()
	s.closing = true
	if s.active != nil {
		s.active.State = "cancelled"
		s.active.cancel()
	}
	s.mu.Unlock()
	s.wg.Wait()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.releaseWorkerLocked()
}
