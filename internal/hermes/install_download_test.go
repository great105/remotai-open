package hermes

import (
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

type installerTransportFunc func(*http.Request) (*http.Response, error)

func (f installerTransportFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func installerResponse(req *http.Request, status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"text/plain"}}, Body: io.NopCloser(strings.NewReader(body)), Request: req}
}

func TestInstallFallsBackToOfficialRepository(t *testing.T) {
	f := newFixture(t)
	m := f.manager(t, false)
	name := "install.sh"
	if m.opts.goos == "windows" {
		name = "install.ps1"
	}
	primary := "https://hermes-agent.nousresearch.com/" + name
	fallback := "https://raw.githubusercontent.com/NousResearch/hermes-agent/main/scripts/" + name
	const script = "# official repository installer fixture\n"
	var requested []string
	m.opts.HTTPClient.Transport = installerTransportFunc(func(req *http.Request) (*http.Response, error) {
		requested = append(requested, req.URL.String())
		switch req.URL.String() {
		case primary:
			return installerResponse(req, http.StatusForbidden, "Access denied"), nil
		case fallback:
			return installerResponse(req, http.StatusOK, script), nil
		default:
			t.Fatalf("unexpected request: %s", req.URL)
			return nil, nil
		}
	})
	if err := m.Install(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(requested, []string{primary, fallback}) {
		t.Fatalf("installer sources: %v", requested)
	}
	data, err := os.ReadFile(filepath.Join(m.root, "bootstrap", name))
	if err != nil || string(data) != script {
		t.Fatalf("bootstrap script: %q, %v", data, err)
	}
	if status := m.Status(); !status.Installed || status.Ownership != "managed" || status.InstalledCommit != fixtureCommit || status.LastError != "" {
		t.Fatalf("fallback did not complete the managed install: %+v", status)
	}
}

func installerManager(t *testing.T, goos string, transport http.RoundTripper) *Manager {
	t.Helper()
	m, err := New(Options{Root: t.TempDir(), goos: goos, HTTPClient: &http.Client{Transport: transport, Timeout: time.Second}, lookPath: func(string) (string, error) { return "", errors.New("fixture: no external Hermes") }})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestDownloadInstallerPrimarySourcePerPlatform(t *testing.T) {
	for _, goos := range []string{"windows", "linux", "darwin"} {
		t.Run(goos, func(t *testing.T) {
			name := "install.sh"
			if goos == "windows" {
				name = "install.ps1"
			}
			var requested []string
			m := installerManager(t, goos, installerTransportFunc(func(req *http.Request) (*http.Response, error) {
				if req.Header.Get("User-Agent") != "Remotai-Hermes" {
					t.Fatal("missing installer user agent")
				}
				requested = append(requested, req.URL.String())
				return installerResponse(req, http.StatusOK, "# official-script fixture"), nil
			}))
			if err := m.downloadInstaller(context.Background(), filepath.Join(m.root, name)); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(requested, []string{"https://hermes-agent.nousresearch.com/" + name}) {
				t.Fatalf("unnecessary fallback or wrong platform: %v", requested)
			}
		})
	}
}

type brokenInstallerBody struct{}

func (brokenInstallerBody) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }
func (brokenInstallerBody) Close() error             { return nil }

func TestDownloadInstallerFallbackOnUnavailableOrInvalidResponse(t *testing.T) {
	for _, failure := range []string{"403", "429", "503", "network", "empty", "oversize", "html-header", "html-body", "read-error"} {
		t.Run(failure, func(t *testing.T) {
			const primary = "https://hermes-agent.nousresearch.com/install.sh"
			const fallback = "https://raw.githubusercontent.com/NousResearch/hermes-agent/main/scripts/install.sh"
			var requested []string
			m := installerManager(t, "linux", installerTransportFunc(func(req *http.Request) (*http.Response, error) {
				requested = append(requested, req.URL.String())
				if req.URL.String() == fallback {
					return installerResponse(req, http.StatusOK, "#!/usr/bin/env bash\n# repository fixture\n"), nil
				}
				if req.URL.String() != primary {
					t.Fatalf("unexpected source: %s", req.URL)
				}
				response := installerResponse(req, http.StatusOK, "")
				switch failure {
				case "403":
					response.StatusCode = http.StatusForbidden
				case "429":
					response.StatusCode = http.StatusTooManyRequests
				case "503":
					response.StatusCode = http.StatusServiceUnavailable
				case "network":
					return nil, errors.New("fixture: unreachable site")
				case "oversize":
					response.Body = io.NopCloser(strings.NewReader(strings.Repeat("#", 2*1024*1024+1)))
				case "html-header":
					response.Header.Set("Content-Type", "text/html; charset=utf-8")
					response.Body = io.NopCloser(strings.NewReader("Access denied"))
				case "html-body":
					response.Body = io.NopCloser(strings.NewReader("\xef\xbb\xbf \n<!DOCTYPE HTML><html>Access denied</html>"))
				case "read-error":
					response.Body = brokenInstallerBody{}
				}
				return response, nil
			}))
			dst := filepath.Join(m.root, "install.sh")
			if err := m.downloadInstaller(context.Background(), dst); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(requested, []string{primary, fallback}) {
				t.Fatalf("fallback sequence: %v", requested)
			}
			if data, err := os.ReadFile(dst); err != nil || !strings.Contains(string(data), "repository fixture") {
				t.Fatalf("saved failed response instead of installer: %q, %v", data, err)
			}
		})
	}
}

func TestDownloadInstallerBothSourcesFailPreservesBootstrap(t *testing.T) {
	requests := 0
	m := installerManager(t, "windows", installerTransportFunc(func(req *http.Request) (*http.Response, error) {
		requests++
		return installerResponse(req, http.StatusForbidden, "Access denied"), nil
	}))
	dst := filepath.Join(m.root, "install.ps1")
	if err := os.WriteFile(dst, []byte("# previous valid installer"), 0600); err != nil {
		t.Fatal(err)
	}
	err := m.downloadInstaller(context.Background(), dst)
	if err == nil || !strings.Contains(err.Error(), "официальный сайт Hermes") || !strings.Contains(err.Error(), "официальный GitHub Hermes") || strings.Count(err.Error(), "HTTP 403") != 2 || requests != 2 {
		t.Fatalf("missing failure diagnostics: %v, requests=%d", err, requests)
	}
	if data, err := os.ReadFile(dst); err != nil || string(data) != "# previous valid installer" {
		t.Fatalf("failed download overwrote bootstrap: %q, %v", data, err)
	}
}

func TestDownloadInstallerCancellationDoesNotFallBack(t *testing.T) {
	for _, alreadyCancelled := range []bool{false, true} {
		t.Run(map[bool]string{false: "during request", true: "before request"}[alreadyCancelled], func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if alreadyCancelled {
				cancel()
			}
			requests := 0
			m := installerManager(t, "windows", installerTransportFunc(func(req *http.Request) (*http.Response, error) {
				requests++
				cancel()
				return nil, context.Canceled
			}))
			err := m.downloadInstaller(ctx, filepath.Join(m.root, "install.ps1"))
			expected := 1
			if alreadyCancelled {
				expected = 0
			}
			if !errors.Is(err, context.Canceled) || requests != expected {
				t.Fatalf("cancellation ignored: %v, requests=%d", err, requests)
			}
		})
	}
}

func TestDownloadInstallerDoesNotRetryLocalWriteFailure(t *testing.T) {
	requests := 0
	m := installerManager(t, "windows", installerTransportFunc(func(req *http.Request) (*http.Response, error) {
		requests++
		return installerResponse(req, http.StatusOK, "# valid installer"), nil
	}))
	if err := m.downloadInstaller(context.Background(), m.root); err == nil || requests != 1 {
		t.Fatalf("local write failure triggered network retry: %v, requests=%d", err, requests)
	}
}

func TestDownloadInstallerRejectsUntrustedRedirects(t *testing.T) {
	for _, target := range []string{"https://untrusted.invalid/install.ps1", "http://hermes-agent.nousresearch.com/install.ps1"} {
		t.Run(target, func(t *testing.T) {
			var requested []string
			m := installerManager(t, "windows", installerTransportFunc(func(req *http.Request) (*http.Response, error) {
				requested = append(requested, req.URL.String())
				if req.URL.Host == "raw.githubusercontent.com" {
					return installerResponse(req, http.StatusOK, "# official repository fixture"), nil
				}
				if req.URL.String() != "https://hermes-agent.nousresearch.com/install.ps1" {
					t.Fatalf("followed untrusted redirect: %s", req.URL)
				}
				response := installerResponse(req, http.StatusFound, "")
				response.Header.Set("Location", target)
				return response, nil
			}))
			if err := m.downloadInstaller(context.Background(), filepath.Join(m.root, "install.ps1")); err != nil {
				t.Fatal(err)
			}
			if len(requested) != 2 {
				t.Fatalf("unexpected redirect requests: %v", requested)
			}
		})
	}
}

func TestDownloadInstallerRejectsUntrustedFallbackRedirect(t *testing.T) {
	requests := 0
	m := installerManager(t, "windows", installerTransportFunc(func(req *http.Request) (*http.Response, error) {
		requests++
		switch req.URL.Host {
		case "hermes-agent.nousresearch.com":
			return installerResponse(req, http.StatusForbidden, ""), nil
		case "raw.githubusercontent.com":
			response := installerResponse(req, http.StatusFound, "")
			response.Header.Set("Location", "https://untrusted.invalid/install.ps1")
			return response, nil
		default:
			t.Fatalf("followed untrusted fallback redirect: %s", req.URL)
			return nil, nil
		}
	}))
	dst := filepath.Join(m.root, "install.ps1")
	if err := m.downloadInstaller(context.Background(), dst); err == nil || requests != 2 {
		t.Fatalf("untrusted fallback accepted: %v, requests=%d", err, requests)
	}
	if _, err := os.Stat(dst); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("saved untrusted response: %v", err)
	}
}

// Download-only acceptance: no upstream script or managed runtime is executed.
func TestLiveHermesInstallerDownload(t *testing.T) {
	if os.Getenv("REMOTAI_HERMES_DOWNLOAD_LIVE") != "1" {
		t.Skip("set REMOTAI_HERMES_DOWNLOAD_LIVE=1 for download-only upstream acceptance")
	}
	for _, goos := range []string{"windows", "linux"} {
		t.Run(goos, func(t *testing.T) {
			m := installerManager(t, goos, installerTransportFunc(func(req *http.Request) (*http.Response, error) {
				if req.URL.Host == "hermes-agent.nousresearch.com" {
					return installerResponse(req, http.StatusForbidden, "fixture: reproduce the reported HTTP 403"), nil
				}
				return http.DefaultTransport.RoundTrip(req)
			}))
			m.opts.HTTPClient.Timeout = 30 * time.Second
			ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
			defer cancel()
			dst := filepath.Join(m.root, "installer-download-only")
			if err := m.downloadInstaller(ctx, dst); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(dst)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("official fallback downloaded: bytes=%d sha256=%x", len(data), sha256.Sum256(data))
		})
	}
}
