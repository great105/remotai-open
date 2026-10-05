//go:build linux || darwin

package desktopinstall

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"time"

	"tgcontrol/internal/version"
)

type UpgradeNotice struct {
	InstalledVersion string
	RunningVersion   string
	PanelURL         string
}

// PendingActivation distinguishes a staged update from the old running agent.
// Installing a package replaces a file, not the process using its old inode.
// Only inspect the local agent: never stop it or apply an update from here.
func PendingActivation(ctx context.Context, exe string, port int) *UpgradeNotice {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	base := "http://127.0.0.1:" + strconv.Itoa(port)
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/setup/status", nil)
	if err != nil {
		return nil
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	var running struct {
		Version  string `json:"version"`
		DeviceID string `json:"device_id"`
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(resp.Body, 64*1024)).Decode(&running) != nil ||
		running.DeviceID == "" || !semverPattern.MatchString(running.Version) {
		return nil
	}
	installed, err := executableVersion(ctx, exe)
	if err != nil || !version.IsNewer(installed, running.Version) {
		return nil
	}
	return &UpgradeNotice{InstalledVersion: installed, RunningVersion: running.Version, PanelURL: base + "/setup?force=1"}
}
