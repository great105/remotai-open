package web

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"tgcontrol/internal/procutil"
	"tgcontrol/internal/transcription"
)

// A fresh process owns an isolated Windows profile before config/path singletons
// initialize. Only the existing speech-model cache is reused, read-only.
func TestLiveTranscriptionHTTP(t *testing.T) {
	exe, audio := os.Getenv("REMOTAI_QA_TRANSCRIBER"), os.Getenv("REMOTAI_QA_AUDIO")
	if runtime.GOOS != "windows" || exe == "" || audio == "" {
		t.Skip("explicit packaged bridge and synthetic audio required")
	}
	if os.Getenv("REMOTAI_QA_HTTP_CHILD") != "1" {
		profile := t.TempDir()
		cache := os.Getenv("HF_HUB_CACHE")
		if cache == "" {
			current, _ := os.UserHomeDir()
			cache = filepath.Join(current, ".cache", "huggingface", "hub")
		}
		configDir := filepath.Join(profile, "AppData", "Local", "Remotai")
		if err := os.MkdirAll(configDir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(configDir, "config.json"), []byte(`{"mode":"central_bot","setup_complete":true,"api_token":"synthetic-voice-qa","api_token_uid":7}`), 0o600); err != nil {
			t.Fatal(err)
		}
		cmd := procutil.Hidden(exec.Command(os.Args[0], "-test.run=^TestLiveTranscriptionHTTP$", "-test.v", "-test.count=1"))
		cmd.Env = append(os.Environ(), "REMOTAI_QA_HTTP_CHILD=1", "USERPROFILE="+profile, "HF_HUB_CACHE="+cache)
		output, err := cmd.CombinedOutput()
		t.Log(string(output))
		if err != nil {
			t.Fatal(err)
		}
		return
	}
	s := &Server{mux: http.NewServeMux(), allowed: map[int64]bool{7: true}}
	s.registerTranscriptionRoutes()
	s.mux.HandleFunc("POST /api/pty/upload", s.authWrap(s.apiPtyUpload))
	stand := httptest.NewServer(s.mux)
	defer stand.Close()
	defer s.shutdownTranscription()
	request := func(method, path, contentType string, body io.Reader, result any) {
		t.Helper()
		req, err := http.NewRequest(method, stand.URL+path, body)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("X-API-Token", "synthetic-voice-qa")
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
		response, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		if response.StatusCode != 200 {
			data, _ := io.ReadAll(response.Body)
			t.Fatalf("%s %s: %d %s", method, path, response.StatusCode, data)
		}
		if result != nil {
			if err := json.NewDecoder(response.Body).Decode(result); err != nil {
				t.Fatal(err)
			}
		}
	}
	var status transcription.Status
	request("GET", "/api/transcription/status", "", nil, &status)
	connection, _ := json.Marshal(map[string]string{"executable": exe})
	request("POST", "/api/transcription/connection", "application/json", bytes.NewReader(connection), &status)
	if !status.Available {
		t.Fatal("bridge connection was not retained")
	}
	data, err := os.ReadFile(audio)
	if err != nil {
		t.Fatal(err)
	}
	var form bytes.Buffer
	writer := multipart.NewWriter(&form)
	part, err := writer.CreateFormFile("file", filepath.Base(audio))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(data); err != nil {
		t.Fatal(err)
	}
	writer.Close()
	var uploaded struct {
		Path string `json:"path"`
	}
	request("POST", "/api/pty/upload", writer.FormDataContentType(), &form, &uploaded)
	if !strings.HasPrefix(uploaded.Path, uploadDestDir()+string(filepath.Separator)) {
		t.Fatal("upload escaped the isolated profile")
	}
	body, _ := json.Marshal(map[string]string{"path": uploaded.Path, "model": "small", "language": "en", "id": "0123456789abcdef0123456789abcdef"})
	var job transcription.Job
	request("POST", "/api/transcription/jobs", "application/json", bytes.NewReader(body), &job)
	deadline := time.Now().Add(2 * time.Minute)
	for job.State == "running" && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
		request("GET", "/api/transcription/jobs/"+job.ID, "", nil, &job)
	}
	if job.State != "done" || !strings.Contains(strings.ToLower(job.Text), "project") || !strings.Contains(strings.ToLower(job.Text), "terminal") {
		t.Fatalf("HTTP recognition: %+v", job)
	}
	t.Logf("authenticated upload -> local bridge -> job polling PASS (%.2fs, %s)", job.Duration, job.Device)
}

func TestTranscriptionRoutesRequireAuthentication(t *testing.T) {
	s := &Server{mux: http.NewServeMux()}
	s.registerTranscriptionRoutes()
	for _, endpoint := range []struct{ method, path string }{
		{"GET", "/api/transcription/status"}, {"POST", "/api/transcription/connection"}, {"POST", "/api/transcription/jobs"},
		{"POST", "/api/transcription/actions"},
		{"GET", "/api/transcription/jobs/example"}, {"DELETE", "/api/transcription/jobs/example"},
	} {
		request := httptest.NewRequest(endpoint.method, endpoint.path, nil)
		response := httptest.NewRecorder()
		s.mux.ServeHTTP(response, request)
		if response.Code != http.StatusUnauthorized {
			t.Errorf("%s %s accepted without auth: %d", endpoint.method, endpoint.path, response.Code)
		}
	}
}

func TestTranscriptionAPIRejectsForeignJobAndArbitraryPath(t *testing.T) {
	service := transcription.New(t.TempDir(), t.TempDir())
	defer service.Close()
	s := &Server{transcription: service}
	id := "0123456789abcdef0123456789abcdef"
	if err := service.Cancel(7, id); err != nil {
		t.Fatal(err)
	}
	for _, action := range []struct {
		method  string
		handler func(http.ResponseWriter, *http.Request, int64)
	}{
		{"GET", s.apiTranscriptionGet}, {"DELETE", s.apiTranscriptionCancel},
	} {
		request := httptest.NewRequest(action.method, "/api/transcription/jobs/"+id, nil)
		request.SetPathValue("job", id)
		response := httptest.NewRecorder()
		action.handler(response, request, 8)
		if response.Code != http.StatusNotFound {
			t.Fatalf("foreign identity got %d", response.Code)
		}
	}
	request := httptest.NewRequest("POST", "/api/transcription/jobs", strings.NewReader(`{"path":"C:\\personal\\secret.wav"}`))
	response := httptest.NewRecorder()
	s.apiTranscriptionStart(response, request, 7)
	if response.Code != http.StatusNotFound {
		t.Fatalf("arbitrary path got %d", response.Code)
	}
}
