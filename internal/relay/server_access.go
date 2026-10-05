package relay

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"tgcontrol/internal/config"
)

// ServerAccess is a fresh decision from the account that owns this device.
// It is never persisted as a user-editable entitlement.
type ServerAccess struct {
	Allowed        bool   `json:"allowed"`
	TrialAvailable bool   `json:"trial_available"`
	TrialDays      int    `json:"trial_days"`
	Tier           string `json:"tier"`
	Feature        string `json:"feature"`
	DeviceID       string `json:"device_id"`
	TrialEnd       string `json:"trial_end,omitempty"`
	Code           string `json:"code,omitempty"`
	Message        string `json:"message,omitempty"`
}

var serverAccessHTTPClient = defaultHTTPClient()

func CheckServerAccess(ctx context.Context, startTrial bool) (ServerAccess, error) {
	cfg := config.GetNoSetup()
	token, _ := LoadJWT()
	if token == "" {
		token = cfg.RelayJWT
	}
	if token == "" || cfg.DeviceID == "" || cfg.RelayHTTPBase() == "" {
		return ServerAccess{Code: "server_account_required", Message: "Подключите компьютер к аккаунту Remotai для работы с серверами."}, nil
	}
	return requestServerAccess(ctx, serverAccessHTTPClient, cfg.RelayHTTPBase(), cfg.DeviceID, token, startTrial)
}

func requestServerAccess(ctx context.Context, client *http.Client, base, deviceID, token string, startTrial bool) (ServerAccess, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	method := http.MethodGet
	if startTrial {
		method = http.MethodPost
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(base, "/")+"/v1/agent/server-access", nil)
	if err != nil {
		return ServerAccess{}, errors.New("server access request unavailable")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := client.Do(req)
	if err != nil {
		return ServerAccess{}, errors.New("server access service unavailable")
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return ServerAccess{Code: "server_account_required", Message: "Подключите компьютер к аккаунту Remotai для работы с серверами."}, nil
	}
	if resp.StatusCode != http.StatusOK {
		return ServerAccess{}, errors.New("server access service unavailable")
	}
	var access ServerAccess
	if err := json.NewDecoder(io.LimitReader(resp.Body, 16<<10)).Decode(&access); err != nil {
		return ServerAccess{}, errors.New("invalid server access response")
	}
	if access.DeviceID != deviceID || access.Feature != "servers" {
		return ServerAccess{}, errors.New("server access response does not match device")
	}
	return access, nil
}
