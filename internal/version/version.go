// Package version provides build version information.
package version

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"time"

	"tgcontrol/internal/dnsfallback"
)

// Build-time variables set via -ldflags.
var (
	Version   = "dev"
	Commit    = "unknown"
	BuildDate = "unknown"
)

// Info returns version info as a map.
func Info() map[string]string {
	return map[string]string{
		"version":    Version,
		"commit":     Commit,
		"build_date": BuildDate,
	}
}

// String returns a human-readable version string.
func String() string {
	return fmt.Sprintf("Remotai v%s (commit: %s, built: %s)", Version, Commit, BuildDate)
}

// TargetInfo is a per-platform binary in a multi-target manifest.
type TargetInfo struct {
	DownloadURL string `json:"download_url"`
	Size        int64  `json:"size"`
	SHA256      string `json:"sha256"`
}

// UpdateInfo describes an available update. The flat DownloadURL/SHA256/Size
// fields are the legacy Windows manifest (kept for old clients); Targets is the
// multi-platform map keyed by "GOOS/GOARCH" (e.g. "linux/amd64").
type UpdateInfo struct {
	Available   bool                  `json:"available"`
	Version     string                `json:"version"`
	DownloadURL string                `json:"download_url"`
	ReleaseURL  string                `json:"release_url"`
	Changelog   string                `json:"changelog"`
	Size        int64                 `json:"size"`
	SHA256      string                `json:"sha256"`
	Targets     map[string]TargetInfo `json:"targets,omitempty"`
}

// DefaultUpdateURL — манифест публикуется scripts/publish-release.ps1.
const DefaultUpdateURL = "https://remotai.ru/download/latest.json"

// CheckForUpdate checks if a newer version is available.
// Сетевые/протокольные сбои возвращаются как error (раньше проглатывались в
// Available:false — UI из-за этого врал «у вас последняя версия» без сети).
func CheckForUpdate(updateURL string) (*UpdateInfo, error) {
	if updateURL == "" {
		updateURL = DefaultUpdateURL
	}

	// Запасной резолвер: сломанный системный DNS не должен отрезать машину ещё
	// и от обновлений — иначе она не сможет привезти себе исправление.
	client := &http.Client{Timeout: 10 * time.Second, Transport: dnsfallback.Transport()}
	resp, err := client.Get(updateURL)
	if err != nil {
		return nil, fmt.Errorf("fetch manifest: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("fetch manifest: HTTP %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read manifest: %w", err)
	}

	var info UpdateInfo
	if err := json.Unmarshal(body, &info); err != nil {
		return nil, fmt.Errorf("parse manifest: %w", err)
	}

	// Multi-target манифест: выбрать бинарь под текущий GOOS/GOARCH. Плоские
	// поля (download_url/sha256/size) — легаси-фолбэк ТОЛЬКО для Windows (там
	// исторически лежит remotai.exe). На любой другой платформе плоский или
	// неполный манифест НЕ должен привести к скачиванию чужого бинаря (напр.
	// применению win .exe как ELF) — зануляем URL, обновление не применится.
	if runtime.GOOS != "windows" {
		key := runtime.GOOS + "/" + runtime.GOARCH
		if tgt, ok := info.Targets[key]; ok && tgt.DownloadURL != "" {
			info.DownloadURL = tgt.DownloadURL
			info.SHA256 = tgt.SHA256
			info.Size = tgt.Size
		} else {
			info.DownloadURL = ""
			info.SHA256 = ""
			info.Size = 0
		}
	} else if tgt, ok := info.Targets["windows/amd64"]; ok && tgt.DownloadURL != "" {
		// Windows: предпочесть явный target, если он есть (иначе — плоские поля).
		info.DownloadURL = tgt.DownloadURL
		info.SHA256 = tgt.SHA256
		info.Size = tgt.Size
	}

	// Strict semver comparison: dev/local builds never count as outdated,
	// equal or older remote versions never trigger a downgrade.
	info.Available = IsNewer(info.Version, Version)

	return &info, nil
}

// IsNewer reports whether remote is a strictly newer semver than local.
// Unparseable versions (dev, local-YYYYMMDD…) are never upgraded from or to.
func IsNewer(remote, local string) bool {
	r, ok := parseSemver(remote)
	if !ok {
		return false
	}
	l, ok := parseSemver(local)
	if !ok {
		return false
	}
	for i := range 3 {
		if r[i] != l[i] {
			return r[i] > l[i]
		}
	}
	return false
}

func parseSemver(v string) ([3]int, bool) {
	var out [3]int
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	parts := strings.SplitN(v, ".", 3)
	if len(parts) != 3 {
		return out, false
	}
	for i, p := range parts {
		// допускаем суффиксы вида "1-rc1" только в патч-части
		if i == 2 {
			if dash := strings.IndexByte(p, '-'); dash >= 0 {
				p = p[:dash]
			}
		}
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return out, false
		}
		out[i] = n
	}
	return out, true
}
