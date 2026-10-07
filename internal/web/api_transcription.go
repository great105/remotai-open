package web

import (
	"encoding/json"
	"errors"
	"net/http"

	"tgcontrol/internal/transcription"
)

func (s *Server) registerTranscriptionRoutes() {
	s.mux.HandleFunc("GET /api/transcription/status", s.authWrap(s.apiTranscriptionStatus))
	s.mux.HandleFunc("POST /api/transcription/connection", s.authWrap(s.apiTranscriptionConnection))
	s.mux.HandleFunc("POST /api/transcription/jobs", s.authWrap(s.apiTranscriptionStart))
	s.mux.HandleFunc("POST /api/transcription/actions", s.authWrap(s.apiTranscriptionAction))
	s.mux.HandleFunc("GET /api/transcription/jobs/{job}", s.authWrap(s.apiTranscriptionGet))
	s.mux.HandleFunc("DELETE /api/transcription/jobs/{job}", s.authWrap(s.apiTranscriptionCancel))
}

func (s *Server) transcriptionService() (*transcription.Service, error) {
	s.transcriptionMu.Lock()
	defer s.transcriptionMu.Unlock()
	if s.transcriptionClosing {
		return nil, errors.New("распознавание остановлено")
	}
	if s.transcription == nil {
		root, err := transcription.LocalRoot()
		if err != nil {
			return nil, err
		}
		s.transcription = transcription.New(root, uploadDestDir())
	}
	return s.transcription, nil
}

func (s *Server) recordTranscriptionUpload(uid int64, path string) {
	// Existing uploads do not initialize Whisper or create an extra process.
	s.transcriptionMu.Lock()
	service := s.transcription
	s.transcriptionMu.Unlock()
	if service != nil {
		service.RecordUpload(uid, path)
	}
}

func (s *Server) shutdownTranscription() {
	s.transcriptionMu.Lock()
	s.transcriptionClosing = true
	service := s.transcription
	s.transcriptionMu.Unlock()
	if service != nil {
		service.Close()
	}
}

func transcriptionError(w http.ResponseWriter, err error) {
	code := http.StatusBadRequest
	if errors.Is(err, transcription.ErrBusy) {
		code = http.StatusConflict
	}
	if errors.Is(err, transcription.ErrNotFound) {
		code = http.StatusNotFound
	}
	jsonError(w, err.Error(), code)
}

func (s *Server) apiTranscriptionStatus(w http.ResponseWriter, r *http.Request, uid int64) {
	service, err := s.transcriptionService()
	if err != nil {
		transcriptionError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	jsonResp(w, service.Status(uid))
}

func (s *Server) apiTranscriptionConnection(w http.ResponseWriter, r *http.Request, uid int64) {
	var input struct {
		Executable string `json:"executable"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192)).Decode(&input); err != nil {
		jsonError(w, "invalid request", 400)
		return
	}
	service, err := s.transcriptionService()
	if err == nil {
		err = service.Configure(input.Executable)
	}
	if err != nil {
		transcriptionError(w, err)
		return
	}
	jsonResp(w, service.Status(uid))
}

func (s *Server) apiTranscriptionStart(w http.ResponseWriter, r *http.Request, uid int64) {
	var input struct {
		Path     string `json:"path"`
		Model    string `json:"model"`
		Language string `json:"language"`
		ID       string `json:"id"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192)).Decode(&input); err != nil {
		jsonError(w, "invalid request", 400)
		return
	}
	service, err := s.transcriptionService()
	if err != nil {
		transcriptionError(w, err)
		return
	}
	if input.Model == "" {
		input.Model = "medium"
	}
	if input.Language == "" {
		input.Language = "ru"
	}
	if r.Context().Err() != nil {
		return
	}
	job, err := service.Start(uid, input.Path, input.Model, input.Language, input.ID)
	if err != nil {
		transcriptionError(w, err)
		return
	}
	jsonResp(w, job)
}

func (s *Server) apiTranscriptionGet(w http.ResponseWriter, r *http.Request, uid int64) {
	service, err := s.transcriptionService()
	if err != nil {
		transcriptionError(w, err)
		return
	}
	job, err := service.Get(uid, r.PathValue("job"))
	if err != nil {
		transcriptionError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	jsonResp(w, job)
}

func (s *Server) apiTranscriptionAction(w http.ResponseWriter, r *http.Request, uid int64) {
	var input transcription.Action
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 65536)).Decode(&input); err != nil {
		jsonError(w, "invalid request", 400)
		return
	}
	service, err := s.transcriptionService()
	if err != nil {
		transcriptionError(w, err)
		return
	}
	if r.Context().Err() != nil {
		return
	}
	job, err := service.StartAction(uid, input)
	if err != nil {
		transcriptionError(w, err)
		return
	}
	jsonResp(w, job)
}

func (s *Server) apiTranscriptionCancel(w http.ResponseWriter, r *http.Request, uid int64) {
	service, err := s.transcriptionService()
	if err == nil {
		err = service.Cancel(uid, r.PathValue("job"))
	}
	if err != nil {
		transcriptionError(w, err)
		return
	}
	jsonResp(w, map[string]bool{"ok": true})
}
