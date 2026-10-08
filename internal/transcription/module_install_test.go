package transcription

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"tgcontrol/internal/agentdesk"
)

func moduleArchive(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	for path, body := range files {
		file, err := writer.Create(path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := file.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

type installedModuleWorker struct{ managementWorker }

func (w *installedModuleWorker) Call(ctx context.Context, method string, input, output any) error {
	if w.release != nil {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-w.release:
		}
	}
	return json.Unmarshal([]byte(`{"management":true,"module_version":"1.0.0","models":[{"id":"small","ready":false}],"settings":{"model":"small","language":"ru","dictionary":[]}}`), output)
}

func moduleFixture(t *testing.T, body []byte, handler http.HandlerFunc) (*Service, *atomic.Int32) {
	t.Helper()
	var downloads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		downloads.Add(1)
		if handler != nil {
			handler(w, r)
			return
		}
		w.Write(body)
	}))
	t.Cleanup(server.Close)
	s, _, _ := fixture(t)
	hash := sha256.Sum256(body)
	s.moduleRelease = moduleRelease{Version: "1.0.0", URL: server.URL, SHA256: hex.EncodeToString(hash[:]), Folder: "module", Bytes: int64(len(body)), UnpackedBytes: 100000}
	s.moduleSupported = true
	s.newWorker = func(config agentdesk.Config) (Worker, error) { return &installedModuleWorker{}, nil }
	return s, &downloads
}
func completeArchive(t *testing.T) []byte {
	return moduleArchive(t, map[string]string{"module/AgentDeskBridge.exe": "trusted fixture", "module/_internal/module-version.json": `{"version":"1.0.0"}`, "module/_internal/library.dll": "synthetic library"})
}

func TestModuleInstallConnectsVerifiedPackageAndReusesIt(t *testing.T) {
	s, downloads := moduleFixture(t, completeArchive(t), nil)
	id := "11111111111111111111111111111111"
	if _, err := s.StartAction(7, Action{Operation: "module.install", ID: id}); err != nil {
		t.Fatal(err)
	}
	job := terminalJob(t, s, id)
	if job.State != "done" || !s.Status(7).Available || !s.Status(7).Module.Managed {
		t.Fatalf("not connected: %+v", job)
	}
	var result Details
	if json.Unmarshal(job.Result, &result) != nil || len(result.Models) != 1 {
		t.Fatal("missing installed catalog")
	}
	saved := New(s.root, s.uploadsRoot)
	defer saved.Close()
	if saved.Status(7).Executable != s.Status(7).Executable {
		t.Fatal("connection lost after agent restart")
	}
	if _, err := s.StartAction(7, Action{Operation: "module.install", ID: id}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.StartAction(7, Action{Operation: "module.install", ID: "22222222222222222222222222222222"}); err != nil {
		t.Fatal(err)
	}
	if terminalJob(t, s, "22222222222222222222222222222222").State != "done" || downloads.Load() != 1 {
		t.Fatal("existing module downloaded again")
	}
}

func TestModuleFailurePreservesConnectionAndCleansStaging(t *testing.T) {
	for _, kind := range []string{"hash", "traversal", "http", "probe", "commit", "space"} {
		t.Run(kind, func(t *testing.T) {
			body := completeArchive(t)
			if kind == "traversal" {
				body = moduleArchive(t, map[string]string{"../outside": "forbidden"})
			}
			var handler http.HandlerFunc
			if kind == "http" {
				handler = func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(403) }
			}
			s, _ := moduleFixture(t, body, handler)
			old := s.Status(7).Executable
			if kind == "space" {
				s.moduleSpace = func(string, uint64) error { return errors.New("insufficient disk space") }
			}
			if kind == "hash" {
				s.moduleRelease.SHA256 = "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
			}
			if kind == "probe" {
				s.newWorker = func(agentdesk.Config) (Worker, error) { return nil, errors.New("probe failed") }
			}
			if kind == "commit" {
				if err := os.MkdirAll(filepath.Join(s.root, "connection.json"), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			id := "11111111111111111111111111111111"
			if _, err := s.StartAction(7, Action{Operation: "module.install", ID: id}); err != nil {
				t.Fatal(err)
			}
			if job := terminalJob(t, s, id); job.State != "failed" || job.Error == "" {
				t.Fatalf("failure not visible: %+v", job)
			}
			if s.Status(7).Executable != old || !regularFile(old) {
				t.Fatal("old module lost on failed install")
			}
			s.wg.Wait()
			entries, err := os.ReadDir(filepath.Join(s.root, "modules"))
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 0 {
				t.Fatalf("partial module remains: %v", entries)
			}
		})
	}
}

func TestModuleInstallOwnershipCancelAndSharedGate(t *testing.T) {
	started := make(chan struct{})
	body := completeArchive(t)
	s, _ := moduleFixture(t, body, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.Write(body[:1])
		w.(http.Flusher).Flush()
		close(started)
		<-r.Context().Done()
	})
	id := "11111111111111111111111111111111"
	old := s.Status(7).Executable
	if _, err := s.StartAction(7, Action{Operation: "module.install", ID: id}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("download not started")
	}
	if s.Status(8).Active != nil {
		t.Fatal("foreign install exposed")
	}
	if _, err := s.Get(8, id); !errors.Is(err, ErrNotFound) {
		t.Fatal("foreign install readable")
	}
	if err := s.Cancel(8, id); !errors.Is(err, ErrNotFound) {
		t.Fatal("foreign install cancellable")
	}
	if _, err := s.StartAction(7, Action{Operation: "inspect", ID: "22222222222222222222222222222222"}); !errors.Is(err, ErrBusy) {
		t.Fatal("model task bypassed install gate")
	}
	if progress := s.Status(7).Active.Progress; progress == nil || progress.TotalBytes != int64(len(body)) {
		t.Fatal("download progress absent")
	}
	if err := s.Cancel(7, id); err != nil {
		t.Fatal(err)
	}
	s.wg.Wait()
	if job := terminalJob(t, s, id); job.State != "cancelled" || s.Status(7).Executable != old {
		t.Fatal("cancellation replaced old connection")
	}
	if _, err := s.StartAction(7, Action{Operation: "module.install", ID: id}); err != nil {
		t.Fatal(err)
	}
	if job, _ := s.Get(7, id); job.State != "cancelled" {
		t.Fatal("duplicate cancelled install restarted")
	}
}

func TestModuleEarlyCancelUnsupportedAndTruncatedDownload(t *testing.T) {
	body := completeArchive(t)
	s, _ := moduleFixture(t, body, nil)
	s.moduleSupported = false
	if _, err := s.StartAction(7, Action{Operation: "module.install", ID: "11111111111111111111111111111111"}); err == nil {
		t.Fatal("unsupported install started")
	}
	s.moduleSupported = true
	id := "22222222222222222222222222222222"
	if err := s.Cancel(7, id); err != nil {
		t.Fatal(err)
	}
	job, err := s.StartAction(7, Action{Operation: "module.install", ID: id})
	if err != nil || job.State != "cancelled" {
		t.Fatal("late install ignored early cancel")
	}
	s, _ = moduleFixture(t, body, func(w http.ResponseWriter, r *http.Request) { w.Write(body[:len(body)/2]) })
	id = "33333333333333333333333333333333"
	if _, err := s.StartAction(7, Action{Operation: "module.install", ID: id}); err != nil {
		t.Fatal(err)
	}
	if terminalJob(t, s, id).State != "failed" {
		t.Fatal("truncated package accepted")
	}
}

func TestRealModuleInstallFromOfficialDownload(t *testing.T) {
	if runtime.GOOS != "windows" || os.Getenv("REMOTAI_QA_MODULE_INSTALL") != "1" {
		t.Skip("explicit real Windows module-download acceptance required")
	}
	root := t.TempDir()
	s := New(filepath.Join(root, "managed"), filepath.Join(root, "uploads"))
	defer s.Close()
	id := "11111111111111111111111111111111"
	if _, err := s.StartAction(7, Action{Operation: "module.install", ID: id}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Minute)
	stages := map[string]bool{}
	for time.Now().Before(deadline) {
		job, err := s.Get(7, id)
		if err != nil {
			t.Fatal(err)
		}
		if job.Progress != nil {
			stages[job.Progress.Stage] = true
		}
		if job.State != "running" {
			if job.State != "done" || !s.Status(7).Module.Managed {
				t.Fatalf("real install failed: %+v", job)
			}
			var details Details
			if json.Unmarshal(job.Result, &details) != nil || len(details.Models) != 9 {
				t.Fatal("real module catalog missing")
			}
			t.Logf("official download installed and connected; version=%s, models=%d, stages=%v", details.ModuleVersion, len(details.Models), stages)
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("real module installation timed out")
}
