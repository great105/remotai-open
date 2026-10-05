package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"tgcontrol-relay/internal/db"
	"tgcontrol-relay/internal/protocol"
	"tgcontrol-relay/internal/relayhub"
)

// handleClientRequest — POST /v1/client/{device_id}/request.
// Тело: JSON с полями { method, path, query, body (base64-decoded на стороне клиента) }.
// Удобнее для одноразовых REST-запросов где WS-канал избыточен.
func (s *Server) handleClientRequest(w http.ResponseWriter, r *http.Request) {
	deviceID := chi.URLParam(r, "deviceID")
	claims, err := s.requireUserAuth(r)
	if err != nil {
		writeErr(w, http.StatusUnauthorized, err.Error())
		return
	}
	dev, err := s.deviceForUser(r, deviceID, claims.UserID)
	if err != nil {
		writeErr(w, http.StatusForbidden, err.Error())
		return
	}
	role, err := db.DeviceRole(r.Context(), s.DB, deviceID, claims.UserID)
	if err != nil {
		writeErr(w, http.StatusForbidden, "device access required")
		return
	}
	// ⚠ ДЕНЕЖНАЯ ГРАНИЦА. Этот маршрут — полноценный доступ к компьютеру через
	// облако: им ходят файлы, настройки, список терминалов и почти весь REST
	// клиента. Гейт стоял только на сокетах (ws_client, ws_stream), поэтому
	// после конца пробы «удалённый доступ выключен» было неправдой: сокет
	// закрывался, а прокси продолжал работать (аудит онбординга 30.08.2026).
	if !s.cloudGate(w, r, dev.UserID) {
		return
	}

	var req struct {
		Method  string            `json:"method"`
		Path    string            `json:"path"`
		Query   map[string]string `json:"query,omitempty"`
		Body    []byte            `json:"body,omitempty"` // base64 JSON-decoded автоматически
		Headers map[string]string `json:"headers,omitempty"`
	}
	// 128 MB — large enough for proxied file/image uploads (multipart body is
	// base64-wrapped inside this JSON envelope, so ~95 MB raw file max; the
	// client in apk/src/api.ts fails fast at the same boundary).
	body, _ := io.ReadAll(http.MaxBytesReader(w, r.Body, 128<<20))
	if len(body) > 0 {
		if err := json.Unmarshal(body, &req); err != nil {
			writeErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return
		}
	}
	if req.Method == "" {
		req.Method = http.MethodGet
	}
	if req.Path == "" {
		writeErr(w, http.StatusBadRequest, "path required")
		return
	}
	if role == db.RoleViewer && req.Method != http.MethodGet && req.Method != http.MethodHead {
		writeErrCode(w, http.StatusForbidden, "viewer_read_only", "viewer access is read-only")
		return
	}

	agent := s.Hub.Get(deviceID)
	if agent == nil {
		// code=pc_offline: клиент покажет «Компьютер не в сети», а не «Ошибка сервера».
		writeErrCode(w, http.StatusBadGateway, "pc_offline", relayhub.ErrAgentOffline.Error())
		return
	}
	cmd := protocol.Cmd{
		Type:      protocol.MsgCmd,
		RequestID: uuid.NewString(),
		Method:    req.Method,
		Path:      req.Path,
		Query:     req.Query,
		Body:      req.Body,
		Headers:   req.Headers,
	}
	// Учёт использования фичи — дневной счётчик, в горутине (не блокируем запрос).
	if f := featureByPath(req.Path); f != "" {
		s.bumpFeature(claims.UserID, f)
	}
	res, err := agent.Send(r.Context(), cmd, 60*time.Second)
	if err != nil {
		// Агент был на связи, но не ответил или отвалился в процессе — для
		// пользователя это тот же «компьютер недоступен».
		code := "pc_timeout"
		if errors.Is(err, relayhub.ErrAgentDisconnect) {
			code = "pc_offline"
		}
		writeErrCode(w, http.StatusGatewayTimeout, code, err.Error())
		return
	}
	if res.Error != "" && res.StatusCode == 0 {
		res.StatusCode = http.StatusInternalServerError
	}
	for k, v := range res.Headers {
		w.Header().Set(k, v)
	}
	w.WriteHeader(res.StatusCode)
	_, _ = w.Write(res.Body)
}
