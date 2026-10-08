package transcription

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"tgcontrol/internal/agentdesk"
)

// Only the agent chooses this package. Requests cannot supply a URL, hash,
// destination or executable; the phone sends the same owned job ID as model tasks.
type moduleRelease struct {
	Version, URL, SHA256, Folder string
	Bytes, UnpackedBytes         int64
}

func officialModule() moduleRelease {
	return moduleRelease{Version: "1.0.0", URL: "https://github.com/great105/remotai-open/releases/download/v2.75.17/remotai-local-windows-amd64.zip", SHA256: "2c744c95f92244f963c5a704cfdff7b405573c014bdcaf1159d5914c35f8de4c", Folder: "Панель агента", Bytes: 1265740593, UnpackedBytes: 2070864158}
}

type ModulePackage struct {
	Supported     bool   `json:"supported"`
	Version       string `json:"version"`
	DownloadBytes int64  `json:"download_bytes"`
	RequiredBytes int64  `json:"required_bytes"`
	Managed       bool   `json:"managed"`
}

type ModuleProgress struct {
	Stage          string `json:"stage"`
	CompletedBytes int64  `json:"completed_bytes"`
	TotalBytes     int64  `json:"total_bytes"`
	ElapsedSeconds int64  `json:"elapsed_seconds,omitempty"`
}

func (s *Service) modulePath() string {
	return filepath.Join(s.root, "modules", "speech-"+s.moduleRelease.Version+"-"+s.moduleRelease.SHA256[:12])
}

func (s *Service) startModuleInstall(uid int64, id string) (Job, error) {
	if !validJobID(id) {
		return Job{}, errors.New("неверный идентификатор задачи")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked()
	if previous := s.jobs[id]; previous != nil {
		if previous.uid != uid {
			return Job{}, ErrNotFound
		}
		return *previous, nil
	}
	if s.closing {
		return Job{}, errors.New("распознавание остановлено")
	}
	if !s.moduleSupported {
		return Job{}, errors.New("автоматическая установка модуля доступна на Windows x64")
	}
	if s.active != nil {
		return Job{}, ErrBusy
	}
	s.releaseWorkerLocked()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	job := &Job{ID: id, Kind: "module.install", State: "running", uid: uid, cancel: cancel, Progress: &ModuleProgress{Stage: "downloading", TotalBytes: s.moduleRelease.Bytes}}
	s.jobs[id], s.active = job, job
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer cancel()
		err := s.installModule(ctx, job)
		s.mu.Lock()
		defer s.mu.Unlock()
		if job.State == "running" {
			job.State = "failed"
			if err != nil {
				job.Error = err.Error()
			} else {
				job.Error = "установка модуля не завершена"
			}
		}
		job.finished = time.Now()
		if s.active == job {
			s.active = nil
		}
	}()
	return *job, nil
}

func (s *Service) moduleProgress(job *Job, stage string, done, total int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if job.State == "running" {
		// Published copies stay immutable, including across concurrent GET/status.
		job.Progress = &ModuleProgress{Stage: stage, CompletedBytes: done, TotalBytes: total}
	}
}

func moduleCopy(ctx context.Context, dst io.Writer, src io.Reader, max int64, buf []byte, progress func(int64)) (int64, error) {
	var done int64
	for {
		if err := ctx.Err(); err != nil {
			return done, err
		}
		n, err := src.Read(buf)
		if n > 0 {
			if done+int64(n) > max {
				return done, errors.New("размер модуля не совпадает с официальной сборкой")
			}
			written, writeErr := dst.Write(buf[:n])
			done += int64(written)
			if writeErr != nil {
				return done, writeErr
			}
			if written != n {
				return done, io.ErrShortWrite
			}
			progress(done)
		}
		if err == io.EOF {
			return done, nil
		}
		if err != nil {
			return done, err
		}
	}
}

func downloadModule(ctx context.Context, spec moduleRelease, destination string, progress func(string, int64, int64)) error {
	progress("downloading", 0, spec.Bytes)
	request, err := http.NewRequestWithContext(ctx, "GET", spec.URL, nil)
	if err != nil {
		return err
	}
	request.Header.Set("User-Agent", "Remotai-local-speech-installer")
	client := &http.Client{Timeout: 30 * time.Minute, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		host := req.URL.Hostname()
		if len(via) > 5 || req.URL.Scheme != "https" || (host != "github.com" && host != "release-assets.githubusercontent.com" && host != "objects.githubusercontent.com") {
			return errors.New("официальный сервер перенаправил загрузку на неизвестный адрес")
		}
		return nil
	}}
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("не удалось скачать модуль: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("не удалось скачать модуль: HTTP %d; повторите установку", response.StatusCode)
	}
	if response.ContentLength >= 0 && response.ContentLength != spec.Bytes {
		return errors.New("размер загрузки не совпадает с официальным модулем")
	}
	file, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	hash := sha256.New()
	n, copyErr := moduleCopy(ctx, io.MultiWriter(file, hash), io.LimitReader(response.Body, spec.Bytes+1), spec.Bytes, make([]byte, 256<<10), func(done int64) { progress("downloading", done, spec.Bytes) })
	closeErr := file.Close()
	if copyErr != nil {
		return copyErr
	}
	if closeErr != nil {
		return closeErr
	}
	progress("verifying", n, spec.Bytes)
	if n != spec.Bytes || hex.EncodeToString(hash.Sum(nil)) != spec.SHA256 {
		return errors.New("проверка SHA256 модуля не прошла; повторите установку")
	}
	return ctx.Err()
}

func extractModule(ctx context.Context, archive, destination string, max int64, progress func(int64, int64)) error {
	reader, err := zip.OpenReader(archive)
	if err != nil {
		return err
	}
	defer reader.Close()
	var total, done int64
	if len(reader.File) == 0 || len(reader.File) > 10000 {
		return errors.New("некорректный архив модуля")
	}
	for _, entry := range reader.File {
		if entry.UncompressedSize64 > uint64(max-total) {
			return errors.New("распакованный модуль превышает ожидаемый размер")
		}
		total += int64(entry.UncompressedSize64)
	}
	progress(0, total)
	buffer := make([]byte, 256<<10)
	for _, entry := range reader.File {
		if err := ctx.Err(); err != nil {
			return err
		}
		name := strings.ReplaceAll(entry.Name, "\\", "/")
		target := filepath.Join(destination, filepath.FromSlash(name))
		if strings.Contains(name, ":") || strings.HasPrefix(name, "/") || !inside(destination, target) || entry.Mode()&os.ModeSymlink != 0 || (!entry.Mode().IsRegular() && !entry.FileInfo().IsDir()) {
			return errors.New("архив модуля содержит недопустимый путь")
		}
		if entry.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o700); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return err
		}
		src, err := entry.Open()
		if err != nil {
			return err
		}
		dst, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			src.Close()
			return err
		}
		base := done
		n, copyErr := moduleCopy(ctx, dst, io.LimitReader(src, int64(entry.UncompressedSize64)+1), int64(entry.UncompressedSize64), buffer, func(part int64) { progress(base+part, total) })
		closeErr := dst.Close()
		src.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
		if n != int64(entry.UncompressedSize64) {
			return errors.New("неполный файл в архиве модуля")
		}
		done += n
	}
	return nil
}

func (s *Service) probeModule(ctx context.Context, uid int64, folder string) (Details, error) {
	var result Details
	metadata, err := os.ReadFile(filepath.Join(folder, "_internal", "module-version.json"))
	var version struct {
		Version string `json:"version"`
	}
	if err != nil || json.Unmarshal(metadata, &version) != nil || version.Version != s.moduleRelease.Version || !regularFile(filepath.Join(folder, "AgentDeskBridge.exe")) {
		return result, errors.New("в сборке нет готового модуля распознавания")
	}
	worker, err := s.newWorker(agentdesk.Config{Executable: filepath.Join(folder, "AgentDeskBridge.exe"), DataDir: filepath.Join(s.root, "users", strconv.FormatInt(uid, 10)), AllowedRoots: []string{s.uploadsRoot}, RequestTimeout: 2 * time.Minute})
	if err != nil {
		return result, err
	}
	defer worker.Close()
	probeCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	if err := worker.Call(probeCtx, "local.inspect", nil, &result); err != nil {
		return result, fmt.Errorf("не удалось запустить модуль: %w", err)
	}
	if !result.Management || result.ModuleVersion != version.Version || len(result.Models) == 0 {
		return result, errors.New("модуль не подтвердил готовность; повторите установку")
	}
	return result, nil
}

func (s *Service) installModule(ctx context.Context, job *Job) error {
	modules := filepath.Join(s.root, "modules")
	if err := os.MkdirAll(modules, 0o700); err != nil {
		return err
	}
	stage, err := os.MkdirTemp(modules, ".install-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage) // created under this service's managed root only
	final := s.modulePath()
	module := final
	s.moduleProgress(job, "checking", 0, 0)
	details, cachedErr := s.probeModule(ctx, job.uid, final)
	if cachedErr != nil {
		if err := s.moduleSpace(modules, uint64(s.moduleRelease.Bytes+s.moduleRelease.UnpackedBytes+(256<<20))); err != nil {
			return err
		}
		archive := filepath.Join(stage, "module.zip")
		if err := downloadModule(ctx, s.moduleRelease, archive, func(phase string, n, total int64) { s.moduleProgress(job, phase, n, total) }); err != nil {
			return err
		}
		payload := filepath.Join(stage, "payload")
		if err := extractModule(ctx, archive, payload, s.moduleRelease.UnpackedBytes, func(n, total int64) { s.moduleProgress(job, "extracting", n, total) }); err != nil {
			return err
		}
		module = filepath.Join(payload, s.moduleRelease.Folder)
		s.moduleProgress(job, "checking", 0, 0)
		details, err = s.probeModule(ctx, job.uid, module)
		if err != nil {
			return err
		}
	}

	// Cancellation and publication share this lock. Once the new connection is
	// committed, the job is already done; an arriving Cancel cannot label it cancelled.
	s.mu.Lock()
	defer s.mu.Unlock()
	if ctx.Err() != nil || job.State != "running" || s.closing {
		return context.Canceled
	}
	job.Progress = &ModuleProgress{Stage: "connecting"}
	backup := filepath.Join(stage, "previous-module")
	previous := false
	if module != final {
		if _, err := os.Stat(final); err == nil {
			if err := os.Rename(final, backup); err != nil {
				return err
			}
			previous = true
		}
		if err := os.Rename(module, final); err != nil {
			if previous {
				_ = os.Rename(backup, final)
			}
			return err
		}
	}
	if err := s.configureLocked(filepath.Join(final, "AgentDeskBridge.exe")); err != nil {
		if module != final {
			_ = os.Rename(final, module)
			if previous {
				_ = os.Rename(backup, final)
			}
		}
		return err
	}
	job.State = "done"
	job.Result, _ = json.Marshal(details)
	job.finished = time.Now()
	s.active = nil
	return nil
}
