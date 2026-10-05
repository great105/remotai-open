package server

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"

	"tgcontrol-relay/internal/db"
	"tgcontrol-relay/internal/protocol"
	"tgcontrol-relay/internal/relayhub"
)

// BotAgentRequest is the narrow relay bridge behind Telegram /term and /run.
// Authorization is checked again here even though the bot selected a device
// from ListDevicesForUser: the grant may have been revoked between both calls.
func (s *Server) BotAgentRequest(ctx context.Context, userID int64, deviceID, method, path string, body []byte) (int, []byte, error) {
	device, err := db.GetDevice(ctx, s.DB, deviceID)
	if err != nil || device.RevokedAt.Valid {
		return http.StatusForbidden, nil, errors.New("device not available")
	}
	if device.UserID != userID {
		allowed, accessErr := db.UserCanAccessDevice(ctx, s.DB, deviceID, userID)
		if accessErr != nil {
			return http.StatusInternalServerError, nil, accessErr
		}
		if !allowed {
			return http.StatusForbidden, nil, errors.New("device not available")
		}
	}
	agent := s.Hub.Get(deviceID)
	if agent == nil {
		return http.StatusBadGateway, nil, relayhub.ErrAgentOffline
	}
	if method == "" {
		method = http.MethodGet
	}
	result, err := agent.Send(ctx, protocol.Cmd{
		Type: protocol.MsgCmd, RequestID: uuid.NewString(),
		Method: method, Path: path, Body: body,
		Headers: map[string]string{"Content-Type": "application/json"},
	}, 20*time.Second)
	if err != nil {
		return http.StatusGatewayTimeout, nil, err
	}
	status := result.StatusCode
	if status == 0 {
		status = http.StatusInternalServerError
	}
	return status, result.Body, nil
}
