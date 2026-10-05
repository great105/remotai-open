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

// Устройство одного аккаунта — соседу того же аккаунта.
//
// ЗАЧЕМ. Просьба владельца 07.08.2026: «надо дать серверу доступ к компьютеру —
// если что-то случилось, чтобы агент с сервера мог добраться и проверить, что
// там». До сих пор дотянуться до ПК мог только человек с телефона или из
// браузера: клиентские ручки требуют пользовательского JWT, а на сервере
// вводить его некому (и хранить там пропуск ко всему аккаунту — плохая идея).
//
// РЕШЕНИЕ: пропуском служит device-JWT самого сервера, который у него уже есть
// после `remotai pair`. Ничего нового заводить не нужно, а право доступа
// ограничено ровно тем, что оба устройства принадлежат одному пользователю.
//
// ГРАНИЦА ВЛАСТИ ЖИВЁТ НА ПРИНИМАЮЩЕЙ СТОРОНЕ, А НЕ ЗДЕСЬ. Релей помечает
// запрос origin=peer и передаёт имя инициатора; что именно позволено соседу,
// решает сам компьютер (настройка peer_access: off / diag / full). Так и должно
// быть: облако не может быть последней инстанцией в вопросе «пускать ли на мой
// компьютер».

// peerAuth опознаёт устройство-инициатора по его device-JWT.
func (s *Server) peerAuth(r *http.Request) (*db.Device, error) {
	tok := bearer(r.Header.Get("Authorization"))
	if tok == "" {
		return nil, errors.New("device token required")
	}
	claims, err := s.JWT.ParseDevice(tok)
	if err != nil {
		return nil, errors.New("invalid device token: " + err.Error())
	}
	dev, err := db.GetDeviceForAgentAuth(r.Context(), s.DB, claims.DeviceID, claims.UserID)
	if err != nil || dev.RevokedAt.Valid {
		return nil, errors.New("device token no longer owns this PC")
	}
	return dev, nil
}

type peerDevice struct {
	ID       string `json:"device_id"`
	Name     string `json:"name"`
	Hostname string `json:"hostname,omitempty"`
	Platform string `json:"platform,omitempty"`
	Version  string `json:"agent_version,omitempty"`
	Online   bool   `json:"online"`
	LastSeen int64  `json:"last_seen,omitempty"`
	Self     bool   `json:"self,omitempty"`
}

// handleAgentPeers — GET /v1/agent/peers: какие ещё компьютеры есть у этого
// аккаунта и кто из них на связи.
func (s *Server) handleAgentPeers(w http.ResponseWriter, r *http.Request) {
	me, err := s.peerAuth(r)
	if err != nil {
		writeErr(w, http.StatusUnauthorized, err.Error())
		return
	}
	list, err := db.ListDevicesByUser(r.Context(), s.DB, me.UserID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]peerDevice, 0, len(list))
	for _, d := range list {
		p := peerDevice{
			ID: d.ID, Name: d.Name, Hostname: d.Hostname, Platform: d.Platform,
			Version: d.AgentVersion, Self: d.ID == me.ID,
		}
		// «На связи» спрашиваем у хаба, а не у колонки в БД: колонка отражает
		// последнюю запись, хаб — живое соединение прямо сейчас.
		p.Online = s.Hub.Get(d.ID) != nil
		if d.LastSeenAt.Valid {
			p.LastSeen = d.LastSeenAt.Time.Unix()
		}
		out = append(out, p)
	}
	writeJSON(w, http.StatusOK, map[string]any{"devices": out})
}

// handleAgentPeerRequest — POST /v1/agent/peer/{deviceID}/request.
// Тело и ответ те же, что у клиентского /v1/client/{deviceID}/request.
func (s *Server) handleAgentPeerRequest(w http.ResponseWriter, r *http.Request) {
	me, err := s.peerAuth(r)
	if err != nil {
		writeErr(w, http.StatusUnauthorized, err.Error())
		return
	}
	target := chi.URLParam(r, "deviceID")
	if target == "" || target == me.ID {
		writeErr(w, http.StatusBadRequest, "target device required")
		return
	}
	dev, err := db.GetDevice(r.Context(), s.DB, target)
	if err != nil || dev.RevokedAt.Valid {
		writeErr(w, http.StatusForbidden, "no such device")
		return
	}
	// Одно и то же владение проверяем той же функцией, что и при авторизации
	// самого устройства: она учитывает слияние аккаунтов.
	if _, err := db.GetDeviceForAgentAuth(r.Context(), s.DB, target, me.UserID); err != nil {
		writeErr(w, http.StatusForbidden, "device belongs to another account")
		return
	}
	if !s.cloudGate(w, r, me.UserID) {
		return
	}

	var req struct {
		Method  string            `json:"method"`
		Path    string            `json:"path"`
		Query   map[string]string `json:"query,omitempty"`
		Body    []byte            `json:"body,omitempty"`
		Headers map[string]string `json:"headers,omitempty"`
	}
	// 1 МБ: это канал диагностики и управления, а не заливка файлов.
	body, _ := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
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

	agent := s.Hub.Get(target)
	if agent == nil {
		writeErrCode(w, http.StatusBadGateway, "pc_offline", relayhub.ErrAgentOffline.Error())
		return
	}
	name := me.Name
	if name == "" {
		name = me.Hostname
	}
	cmd := protocol.Cmd{
		Type:      protocol.MsgCmd,
		RequestID: uuid.NewString(),
		Method:    req.Method,
		Path:      req.Path,
		Query:     req.Query,
		Body:      req.Body,
		Headers:   req.Headers,
		Origin:    protocol.OriginPeer,
		From:      me.ID,
		FromName:  name,
	}
	res, err := agent.Send(r.Context(), cmd, 60*time.Second)
	if err != nil {
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
