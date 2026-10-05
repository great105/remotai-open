package web

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"tgcontrol/internal/hermes"
	"time"
)

type hermesControlRuntime interface {
	ControlSnapshot() json.RawMessage
	ControlCapabilities() json.RawMessage
	SubmitTask(context.Context, json.RawMessage) (json.RawMessage, error)
	ControlIntent(context.Context, json.RawMessage) (json.RawMessage, error)
	ControlReply(context.Context, json.RawMessage) error
}

func (s *Server) apiHermesControl(w http.ResponseWriter, r *http.Request, uid int64) {
	mgr := s.hermesForRequest(w, uid)
	if mgr == nil {
		return
	}
	c, ok := mgr.(hermesControlRuntime)
	if !ok {
		jsonErrorCode(w, 501, "hermes_control_unsupported", "Обновите Remotai на компьютере. Черновик сохранён.", nil)
		return
	}
	action := r.PathValue("action")
	w.Header().Set("Cache-Control", "no-store")
	if r.Method == http.MethodGet {
		switch action {
		case "snapshot":
			jsonResp(w, c.ControlSnapshot())
		case "capabilities":
			jsonResp(w, c.ControlCapabilities())
		default:
			jsonErrorCode(w, 400, "bad_request", "Неизвестный раздел Hermes.", nil)
		}
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 160<<10))
	if err != nil || !json.Valid(raw) {
		jsonErrorCode(w, 400, "bad_request", "Некорректный запрос Hermes.", nil)
		return
	}
	var result json.RawMessage
	switch action {
	case "submit":
		result, err = c.SubmitTask(r.Context(), raw)
	case "intent":
		result, err = c.ControlIntent(r.Context(), raw)
	case "readiness":
		reader, ok := mgr.(interface {
			ControlReadiness(context.Context, json.RawMessage) (json.RawMessage, error)
		})
		if !ok {
			jsonErrorCode(w, 501, "hermes_control_unsupported", "Обновите Remotai.", nil)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		result, err = reader.ControlReadiness(ctx, raw)
	case "read":
		var request struct {
			ID string `json:"id"`
		}
		if hermes.DecodeControlRequest(raw, &request) != nil || request.ID == "" || len(request.ID) > 512 {
			jsonErrorCode(w, 400, "bad_request", "Укажите ID уведомления.", nil)
			return
		}
		reader, ok := mgr.(interface{ ReadAttention(string) error })
		if !ok {
			jsonErrorCode(w, 501, "hermes_control_unsupported", "Обновите Remotai.", nil)
			return
		}
		err = reader.ReadAttention(request.ID)
		result = json.RawMessage(`{"ok":true}`)
	case "reply":
		err = c.ControlReply(r.Context(), raw)
		result = json.RawMessage(`{"ok":true}`)
	default:
		jsonErrorCode(w, 400, "bad_request", "Неизвестное действие Hermes.", nil)
		return
	}
	if err != nil {
		code := "hermes_control_failed"
		var rejected *hermes.AdmissionRejection
		if action == "submit" && errors.As(err, &rejected) {
			code = "hermes_admission_rejected"
		}
		jsonErrorCode(w, 409, code, err.Error(), nil)
		return
	}
	if action == "submit" {
		w.WriteHeader(http.StatusAccepted)
	}
	jsonResp(w, result)
}
func (s *Server) apiHermesArtifact(w http.ResponseWriter, r *http.Request, uid int64) {
	w.Header().Set("Cache-Control", "no-store")
	mgr := s.hermesForRequest(w, uid)
	if mgr == nil {
		return
	}
	reader, ok := mgr.(interface {
		ReadArtifact(string) ([]byte, string, error)
	})
	if !ok {
		jsonErrorCode(w, 501, "hermes_control_unsupported", "Обновите Remotai.", nil)
		return
	}
	id := r.URL.Query().Get("id")
	if id == "" || len(id) > 512 {
		jsonErrorCode(w, 400, "bad_request", "Укажите ID результата.", nil)
		return
	}
	data, name, err := reader.ReadArtifact(id)
	if err != nil {
		jsonErrorCode(w, 404, "hermes_artifact_unavailable", err.Error(), nil)
		return
	}
	if r.URL.Query().Get("format") == "json" {
		jsonResp(w, map[string]string{"name": name, "base64": base64.StdEncoding.EncodeToString(data)})
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%s", strconv.Quote(strings.ReplaceAll(name, "\"", ""))))
	w.Write(data)
}

func readHermesControlJSON(w http.ResponseWriter, r *http.Request, target any) error {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 40<<10))
	if err != nil {
		return err
	}
	return hermes.DecodeControlRequest(raw, target)
}
