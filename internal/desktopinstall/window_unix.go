//go:build linux || darwin

package desktopinstall

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// WindowURL waits for a real local agent and obtains access from that agent,
// avoiding a second process writing a stale copy of its configuration.
func WindowURL(ctx context.Context, port int) (string, error) {
	if port < 1 || port > 65535 {
		return "", fmt.Errorf("invalid local port")
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	base := "http://localhost:" + strconv.Itoa(port)
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	read := func(path string, target any) error {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+path, nil)
		if err != nil {
			return err
		}
		resp, err := client.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("local agent not ready")
		}
		return json.NewDecoder(io.LimitReader(resp.Body, 64*1024)).Decode(target)
	}
	var state struct {
		Version    string `json:"version"`
		DeviceID   string `json:"device_id"`
		Configured bool   `json:"configured"`
	}
	if err := read("/api/setup/status", &state); err != nil {
		return "", err
	}
	if state.DeviceID == "" || !semverPattern.MatchString(state.Version) {
		return "", fmt.Errorf("local server is not Remotai")
	}
	if !state.Configured {
		return base + "/setup", nil
	}
	var access struct {
		Token string `json:"token"`
	}
	if err := read("/api/setup/local-access", &access); err != nil {
		return "", err
	}
	if access.Token == "" {
		return "", fmt.Errorf("local access is not ready")
	}
	return base + "/miniapp?token=" + url.QueryEscape(access.Token), nil
}
