package web

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHermesAttachmentHandlerUploadBoundary(t *testing.T) {
	home := t.TempDir()
	t.Setenv("USERPROFILE", home)
	t.Setenv("HOME", home)
	root := uploadDestDir()
	write := func(path string) string {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("fixture"), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	outside := write(filepath.Join(home, "pty-upload-private.txt"))
	good := write(filepath.Join(root, "pty-upload-good.txt"))
	nested := write(filepath.Join(root, "nested", "pty-upload-nested.txt"))
	plain := write(filepath.Join(root, "plain.txt"))
	directory := filepath.Join(root, "pty-upload-directory")
	if err := os.Mkdir(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	relative, err := filepath.Rel(cwd, good)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, path string
		allowed    bool
	}{
		{"direct upload", good, true},
		{"outside root", outside, false},
		{"nested upload", nested, false},
		{"not an upload", plain, false},
		{"directory", directory, false},
		{"missing", filepath.Join(root, "pty-upload-missing"), false},
		{"relative", relative, false},
		{"traversal into root", root + string(filepath.Separator) + "nested" + string(filepath.Separator) + ".." + string(filepath.Separator) + filepath.Base(good), false},
	}
	link := filepath.Join(root, "pty-upload-link.txt")
	if err := os.Symlink(outside, link); err == nil {
		cases = append(cases, struct {
			name, path string
			allowed    bool
		}{"symlink", link, false})
	} else {
		t.Logf("symlink creation unavailable: %v", err)
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			f := &hermesWebFixture{rpc: func(_ context.Context, method string, params any) (json.RawMessage, error) {
				calls++
				var p map[string]string
				if method != "file.attach" || json.Unmarshal(params.(json.RawMessage), &p) != nil || p["path"] != good {
					t.Fatalf("unexpected forwarded request: %s %s", method, params)
				}
				return json.RawMessage(`{"attached":true,"ref_text":"@file:fixture"}`), nil
			}}
			s := &Server{hermesManagers: map[int64]hermesRuntime{1: f}}
			body, _ := json.Marshal(map[string]any{"method": "file.attach", "params": map[string]string{"session_id": "live-A", "path": tc.path}})
			w := httptest.NewRecorder()
			s.apiHermesRPC(w, httptest.NewRequest("POST", "/api/hermes/rpc", bytes.NewReader(body)), 1)
			if tc.allowed {
				if w.Code != http.StatusOK || calls != 1 {
					t.Fatalf("upload rejected: %d %s; calls=%d", w.Code, w.Body.String(), calls)
				}
			} else if w.Code != http.StatusBadRequest || calls != 0 {
				t.Fatalf("unsafe path forwarded: %d %s; calls=%d", w.Code, w.Body.String(), calls)
			}
		})
	}
}

func TestHermesAttachmentAcceptsActualPtyUpload(t *testing.T) {
	for _, fallback := range []bool{false, true} {
		for _, chunked := range []bool{false, true} {
			t.Run(fmt.Sprintf("fallback=%t/chunked=%t", fallback, chunked), func(t *testing.T) {
				testHermesActualPtyAttachment(t, fallback, chunked)
			})
		}
	}
}

func testHermesActualPtyAttachment(t *testing.T, fallback, chunked bool) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("USERPROFILE", home)
	t.Setenv("HOME", home)
	temp := t.TempDir()
	t.Setenv("TMP", temp)
	t.Setenv("TEMP", temp)
	t.Setenv("TMPDIR", temp)
	if fallback {
		// A file blocks the preferred directory without touching the real home.
		if err := os.WriteFile(filepath.Join(home, "Remotai"), []byte("fixture"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	file, err := writer.CreateFormFile("file", "report [1].txt")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = file.Write([]byte("uploaded bytes"))
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	url := "/api/pty/upload"
	if chunked {
		url += "?upload_id=attachment-fixture&chunk=0&chunks=1"
	}
	r := httptest.NewRequest("POST", url, &body)
	r.Header.Set("Content-Type", writer.FormDataContentType())
	s := &Server{}
	w := httptest.NewRecorder()
	s.apiPtyUpload(w, r, 1)
	var uploaded struct {
		Path string `json:"path"`
	}
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &uploaded) != nil || uploaded.Path == "" {
		t.Fatalf("upload failed: %d %s", w.Code, w.Body.String())
	}
	calls := 0
	s.hermesManagers = map[int64]hermesRuntime{1: &hermesWebFixture{rpc: func(_ context.Context, method string, params any) (json.RawMessage, error) {
		calls++
		if method != "file.attach" || !strings.Contains(string(params.(json.RawMessage)), "pty-upload-") {
			t.Fatalf("wrong forwarding: %s %s", method, params)
		}
		return json.RawMessage(`{"attached":true}`), nil
	}}}
	request, _ := json.Marshal(map[string]any{"method": "file.attach", "params": map[string]string{"session_id": "live-A", "path": uploaded.Path, "name": "report [1].txt"}})
	w = httptest.NewRecorder()
	s.apiHermesRPC(w, httptest.NewRequest("POST", "/api/hermes/rpc", bytes.NewReader(request)), 1)
	if w.Code != http.StatusOK || calls != 1 {
		t.Fatalf("actual upload rejected: %d %s; calls=%d", w.Code, w.Body.String(), calls)
	}
}

func TestHermesFileAttachmentPolicy(t *testing.T) {
	valid := `{"profile":"default","session_id":"live-chat","path":"C:/uploads/report.pdf","name":"report.pdf"}`
	if !hermesRPCAllowed("file.attach", json.RawMessage(valid)) {
		t.Fatal("uploaded host file cannot be staged in Hermes chat")
	}
	for _, raw := range []string{
		`{"session_id":"","path":"C:/uploads/report.pdf"}`,
		`{"session_id":"live-chat","path":""}`,
		`{"session_id":"live-chat","path":"C:/uploads/report.pdf","data_url":"secret"}`,
		`{"profile":"other","session_id":"live-chat","path":"C:/uploads/report.pdf"}`,
		`{"session_id":"live-chat","path":"C:/uploads/report.pdf","surface":"desktop"}`,
		`{"session_id":"live-chat","path":"C:/uploads/report.pdf","owner":1}`,
		`{"session_id":"live-chat","path":"C:/uploads/report.pdf","name":null}`,
		`{"session_id":"live-chat","path":"C:/uploads/a","path":"C:/uploads/b"}`,
	} {
		if hermesRPCAllowed("file.attach", json.RawMessage(raw)) {
			t.Errorf("unsafe attachment permitted: %s", raw)
		}
	}
}
