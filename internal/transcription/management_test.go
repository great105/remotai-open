package transcription

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"tgcontrol/internal/agentdesk"
)

type managementWorker struct {
	fakeWorker
	legacy    bool
	installed atomic.Bool
}

func (w *managementWorker) Call(ctx context.Context, method string, input, output any) error {
	if method == "audio.transcribe" {
		return w.fakeWorker.Call(ctx, method, input, output)
	}
	w.calls.Add(1)
	if method == "local.inspect" && w.legacy {
		return &agentdesk.RemoteError{Code: "method_not_found", Message: "legacy"}
	}
	if method == "system.capabilities" {
		return json.Unmarshal([]byte(`{"models":["future-model"],"default_model":"future-model"}`), output)
	}
	if w.release != nil {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-w.release:
		}
	}
	if method == "local.model.install" {
		w.installed.Store(true)
	}
	return json.Unmarshal([]byte(`{"management":true,"module_version":"test","models":[{"id":"large-v3-turbo","ready":true}],"settings":{"model":"large-v3-turbo","language":"ru","dictionary":[]}}`), output)
}
func managerFixture(t *testing.T) (*Service, *managementWorker, string) {
	s, _, path := fixture(t)
	w := &managementWorker{}
	s.newWorker = func(agentdesk.Config) (Worker, error) { return w, nil }
	return s, w, path
}
func TestManagementSharesASRGateAndOwnership(t *testing.T) {
	s, w, path := managerFixture(t)
	w.release = make(chan struct{})
	id := "0123456789abcdef0123456789abcdef"
	job, err := s.StartAction(7, Action{Operation: "install", Model: "large-v3-turbo", ID: id})
	if err != nil {
		t.Fatal(err)
	}
	if s.Status(8).Active != nil || s.Status(7).Active == nil {
		t.Fatal("active task leaked or missing")
	}
	if _, err = s.Get(8, job.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("foreign management job readable")
	}
	s.RecordUpload(7, path)
	if _, err = s.Start(7, path, "large-v3-turbo", "ru", ""); !errors.Is(err, ErrBusy) {
		t.Fatal("ASR bypassed model installation gate")
	}
	if err = s.Cancel(8, id); !errors.Is(err, ErrNotFound) {
		t.Fatal("foreign model installation cancelled")
	}
	if err = s.Cancel(7, id); err != nil {
		t.Fatal(err)
	}
	if got := terminalJob(t, s, id); got.State != "cancelled" || w.installed.Load() {
		t.Fatal("cancelled installation completed")
	}
}
func TestManagementEarlyCancelAndDuplicateID(t *testing.T) {
	s, w, _ := managerFixture(t)
	id := "0123456789abcdef0123456789abcdef"
	if err := s.Cancel(7, id); err != nil {
		t.Fatal(err)
	}
	job, err := s.StartAction(7, Action{Operation: "install", Model: "small", ID: id})
	if err != nil || job.State != "cancelled" || w.calls.Load() != 0 {
		t.Fatal("early cancellation restarted installation")
	}
	if _, err = s.StartAction(8, Action{Operation: "inspect", ID: id}); !errors.Is(err, ErrNotFound) {
		t.Fatal("foreign ID reused")
	}
}
func TestManagementDynamicCatalogAndLegacyFallback(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(map[bool]string{false: "managed", true: "legacy"}[legacy], func(t *testing.T) {
			s, w, _ := managerFixture(t)
			w.legacy = legacy
			id := "0123456789abcdef0123456789abcdef"
			if _, err := s.StartAction(7, Action{Operation: "inspect", ID: id}); err != nil {
				t.Fatal(err)
			}
			job := terminalJob(t, s, id)
			var details Details
			if job.State != "done" || json.Unmarshal(job.Result, &details) != nil {
				t.Fatalf("inspection failed: %+v", job)
			}
			if details.Management == legacy || len(details.Models) != 1 {
				t.Fatalf("catalog: %+v", details)
			}
			if legacy && (details.Settings.Model != "future-model" || !details.Models[0].Supported) {
				t.Fatal("legacy model catalog lost")
			}
			if _, err := s.StartAction(7, Action{Operation: "inspect", ID: id}); err != nil {
				t.Fatal(err)
			}
			expected := int32(1)
			if legacy {
				expected = 2
			}
			if w.calls.Load() != expected {
				t.Fatal("duplicate task executed twice")
			}
		})
	}
}
func TestDynamicAudioModelValidation(t *testing.T) {
	s, _, path := fixture(t)
	s.RecordUpload(7, path)
	job, err := s.Start(7, path, "large-v3-turbo", "ru", "")
	if err != nil {
		t.Fatal(err)
	}
	if terminalJob(t, s, job.ID).State != "done" {
		t.Fatal("new model rejected")
	}
}

func TestRealManagedModule(t *testing.T) {
	exe := os.Getenv("AGENTDESK_MANAGED_TEST_EXE")
	if exe == "" {
		t.Skip("set AGENTDESK_MANAGED_TEST_EXE for portable module acceptance")
	}
	root := t.TempDir()
	s := New(filepath.Join(root, "history"), filepath.Join(root, "uploads"))
	defer s.Close()
	if err := s.Configure(exe); err != nil {
		t.Fatal(err)
	}
	actions := []Action{
		{Operation: "inspect", ID: "0123456789abcdef0123456789abcdef"},
		{Operation: "install", Model: "small", ID: "1123456789abcdef0123456789abcdef"},
		{Operation: "save", Settings: &Preferences{Model: "small", Language: "en", Dictionary: []DictionaryEntry{{Heard: "remotai", Written: "Remotai"}}}, ID: "2123456789abcdef0123456789abcdef"},
	}
	for _, action := range actions {
		if _, err := s.StartAction(7, action); err != nil {
			t.Fatal(err)
		}
		job := terminalJob(t, s, action.ID)
		var details Details
		if job.State != "done" || json.Unmarshal(job.Result, &details) != nil || !details.Management || len(details.Models) < 9 {
			t.Fatalf("module action %s: %+v", action.Operation, job)
		}
		if action.Operation == "save" && (details.Settings.Model != "small" || details.Settings.Language != "en" || len(details.Settings.Dictionary) != 1) {
			t.Fatalf("preferences not retained: %+v", details.Settings)
		}
		t.Logf("portable %s: %s, %d models", action.Operation, details.ModuleVersion, len(details.Models))
	}
}
