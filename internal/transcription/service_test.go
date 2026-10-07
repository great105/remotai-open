package transcription

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"tgcontrol/internal/agentdesk"
)

type fakeWorker struct {
	calls, closed atomic.Int32
	release       chan struct{}
}

func (w *fakeWorker) Call(ctx context.Context, method string, input, output any) error {
	w.calls.Add(1)
	if w.release != nil {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-w.release:
		}
	}
	result := output.(*agentdesk.Transcript)
	result.Text, result.Duration, result.Device = "Проверь терминал", 2.5, "cpu"
	return nil
}
func (w *fakeWorker) Close() error { w.closed.Add(1); return nil }

func fixture(t *testing.T) (*Service, *fakeWorker, string) {
	t.Helper()
	root := t.TempDir()
	uploads := filepath.Join(root, "uploads")
	if err := os.Mkdir(uploads, 0o700); err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(root, "AgentDeskBridge.exe")
	if err := os.WriteFile(exe, []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := New(filepath.Join(root, "history"), uploads)
	s.executable = exe
	w := &fakeWorker{}
	s.newWorker = func(config agentdesk.Config) (Worker, error) {
		if !strings.HasSuffix(config.DataDir, filepath.Join("users", "7")) {
			t.Fatalf("wrong identity history: %s", config.DataDir)
		}
		if len(config.AllowedRoots) != 1 || config.AllowedRoots[0] != uploads {
			t.Fatal("wrong allowed roots")
		}
		return w, nil
	}
	path := filepath.Join(uploads, "voice.ogg")
	if err := os.WriteFile(path, []byte("synthetic audio"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s, w, path
}

func terminalJob(t *testing.T, s *Service, id string) Job {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		job, err := s.Get(7, id)
		if err != nil {
			t.Fatal(err)
		}
		if job.State != "running" {
			return job
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("job did not finish")
	return Job{}
}

func TestCompletedUploadOwnershipAndWarmWorker(t *testing.T) {
	s, worker, path := fixture(t)
	if _, err := s.Start(7, path, "medium", "ru", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unissued path accepted: %v", err)
	}
	s.RecordUpload(8, path)
	if _, err := s.Start(7, path, "medium", "ru", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign upload accepted: %v", err)
	}
	s.RecordUpload(7, path)
	job, err := s.Start(7, path, "medium", "ru", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(8, job.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("foreign job was readable")
	}
	if err := s.Cancel(8, job.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("foreign job was cancellable")
	}
	result := terminalJob(t, s, job.ID)
	if result.State != "done" || result.Text != "Проверь терминал" {
		t.Fatalf("result: %+v", result)
	}
	s.RecordUpload(7, path)
	second, err := s.Start(7, path, "small", "auto", "")
	if err != nil {
		t.Fatal(err)
	}
	terminalJob(t, s, second.ID)
	if worker.calls.Load() != 2 || worker.closed.Load() != 0 {
		t.Fatal("warm worker was reloaded")
	}
	s.Close()
	if worker.closed.Load() != 1 {
		t.Fatal("shutdown did not release worker")
	}
}

func TestCancellationBeforeStartAndDuringRecognition(t *testing.T) {
	s, worker, path := fixture(t)
	id := "0123456789abcdef0123456789abcdef"
	if err := s.Cancel(7, id); err != nil {
		t.Fatal(err)
	}
	s.RecordUpload(7, path)
	job, err := s.Start(7, path, "medium", "ru", id)
	if err != nil || job.State != "cancelled" || worker.calls.Load() != 0 {
		t.Fatalf("discarded recording ran: %+v %v", job, err)
	}
	worker.release = make(chan struct{})
	job, err = s.Start(7, path, "medium", "ru", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Start(7, path, "medium", "ru", ""); !errors.Is(err, ErrBusy) {
		t.Fatalf("unbounded work accepted: %v", err)
	}
	if err := s.Cancel(7, job.ID); err != nil {
		t.Fatal(err)
	}
	if terminalJob(t, s, job.ID).State != "cancelled" {
		t.Fatal("cancelled result was published")
	}
	s.Close()
}

func TestUploadBoundaryAndSize(t *testing.T) {
	s, _, path := fixture(t)
	outside := filepath.Join(t.TempDir(), "voice.ogg")
	if err := os.WriteFile(outside, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	s.RecordUpload(7, outside)
	if _, err := s.Start(7, outside, "medium", "ru", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("outside root accepted: %v", err)
	}
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(MaxAudioBytes + 1); err != nil {
		t.Fatal(err)
	}
	file.Close()
	s.RecordUpload(7, path)
	if _, err := s.Start(7, path, "medium", "ru", ""); err == nil {
		t.Fatal("oversized audio accepted")
	}
}

func TestShutdownCancelsInFlightRecognition(t *testing.T) {
	s, w, path := fixture(t)
	w.release = make(chan struct{})
	s.RecordUpload(7, path)
	job, err := s.Start(7, path, "medium", "ru", "")
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	result, err := s.Get(7, job.ID)
	if err != nil || result.State != "cancelled" || w.closed.Load() != 1 {
		t.Fatalf("shutdown: %+v %v", result, err)
	}
}

func TestLivePackagedSpeech(t *testing.T) {
	exe, audio := os.Getenv("REMOTAI_QA_TRANSCRIBER"), os.Getenv("REMOTAI_QA_AUDIO")
	if exe == "" || audio == "" {
		t.Skip("explicit packaged bridge and synthetic QA audio required")
	}
	folder := t.TempDir()
	uploads := filepath.Join(folder, "uploads")
	if err := os.Mkdir(uploads, 0o700); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(audio)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(uploads, filepath.Base(audio))
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	s := New(filepath.Join(folder, "history"), uploads)
	s.executable = exe
	defer s.Close()
	s.RecordUpload(7, path)
	job, err := s.Start(7, path, "small", "en", "")
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(deadline) {
		result, err := s.Get(7, job.ID)
		if err != nil {
			t.Fatal(err)
		}
		if result.State != "running" {
			if result.State != "done" || !strings.Contains(strings.ToLower(result.Text), "project") || !strings.Contains(strings.ToLower(result.Text), "terminal") {
				t.Fatalf("actual speech: %+v", result)
			}
			if expected := os.Getenv("REMOTAI_QA_EXPECT_DEVICE"); expected != "" && result.Device != expected {
				t.Fatalf("expected device %s, actual %s", expected, result.Device)
			}
			t.Logf("local recognition PASS: duration %.2fs, device %s", result.Duration, result.Device)
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("actual speech recognition timed out")
}
