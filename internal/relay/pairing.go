package relay

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"runtime"
	"strings"
	"time"

	"tgcontrol/internal/dnsfallback"
	"tgcontrol/internal/version"
)

// PairingResponse — ответ /v1/pair/request.
type PairingResponse struct {
	Code      string    `json:"code"`
	ExpiresAt time.Time `json:"expires_at"`
	BotLink   string    `json:"bot_link"`
}

// PairingStatus — ответ /v1/pair/status.
type PairingStatus struct {
	Confirmed bool      `json:"confirmed"`
	Expired   bool      `json:"expired"`
	JWT       string    `json:"jwt,omitempty"`
	DeviceID  string    `json:"device_id,omitempty"`
	ExpiresAt time.Time `json:"expires_at,omitempty"`
}

// RequestPairingCode — POST /v1/pair/request — десктоп получает короткий код.
// deviceJWT: если ПК УЖЕ привязан, передаём его device-JWT — релей требует его,
// чтобы выпустить код на добавление управляющих устройств (иначе чужой, зная
// device_id, выпустил бы код и получил грант = захват ПК). Для первого пейринга
// JWT ещё нет — пустая строка, релей пускает (устройства на сервере нет).
func RequestPairingCode(ctx context.Context, relayBase, deviceID, hostname, deviceJWT string) (*PairingResponse, error) {
	if relayBase == "" {
		return nil, errors.New("relay base URL empty")
	}
	if deviceID == "" {
		return nil, errors.New("device_id empty")
	}
	payload := map[string]string{
		"device_id":     deviceID,
		"hostname":      hostname,
		"platform":      runtime.GOOS,
		"agent_version": version.Version,
	}
	body, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(relayBase, "/")+"/v1/pair/request",
		bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "tgcontrol/"+version.Version)
	if deviceJWT != "" {
		req.Header.Set("Authorization", "Bearer "+deviceJWT)
	}

	resp, err := defaultHTTPClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
		return nil, fmt.Errorf("pair_request: %s — %s", resp.Status, strings.TrimSpace(string(b)))
	}
	var out PairingResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return &out, nil
}

// CheckPairingStatus — GET /v1/pair/status?code=...
func CheckPairingStatus(ctx context.Context, relayBase, code string) (*PairingStatus, error) {
	if relayBase == "" {
		return nil, errors.New("relay base URL empty")
	}
	u, err := url.Parse(strings.TrimRight(relayBase, "/") + "/v1/pair/status")
	if err != nil {
		return nil, err
	}
	q := u.Query()
	q.Set("code", code)
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "tgcontrol/"+version.Version)

	resp, err := defaultHTTPClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out PairingStatus
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusNotFound {
		out.Expired = true
	}
	return &out, nil
}

// RevokeSelf — POST /v1/device/revoke-self — десктоп отзывает СВОЁ устройство на
// релее, авторизуясь своим device-JWT. Зовётся при «Отвязать все телефоны», чтобы
// телефоны реально потеряли доступ (а не только локально на ПК сбросился JWT).
// Идемпотентно на сервере (200 даже если уже отозвано).
func RevokeSelf(ctx context.Context, relayBase, deviceJWT string) error {
	if relayBase == "" {
		return errors.New("relay base URL empty")
	}
	if deviceJWT == "" {
		return errors.New("device jwt empty")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(relayBase, "/")+"/v1/device/revoke-self", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+deviceJWT)
	req.Header.Set("User-Agent", "tgcontrol/"+version.Version)

	resp, err := defaultHTTPClient().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
		return fmt.Errorf("revoke_self: %s — %s", resp.Status, strings.TrimSpace(string(b)))
	}
	return nil
}

func defaultHTTPClient() *http.Client {
	return &http.Client{Timeout: 20 * time.Second, Transport: dnsfallback.Transport()}
}
