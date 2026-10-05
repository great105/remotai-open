package web

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"net/url"

	qrcode "github.com/skip2/go-qrcode"

	"tgcontrol/internal/config"
)

// generateQRCode creates a PNG-encoded QR code image.
func generateQRCode(data string, size int) ([]byte, error) {
	return qrcode.Encode(data, qrcode.Medium, size)
}

// buildPairingURL creates a tgcontrol:// deep link for mobile pairing.
// All values are URL-encoded so tokens containing '&', '=' or '+' survive
// transport through QR codes / clipboard / Telegram.
func buildPairingURL(serverURL, token, deviceID string) string {
	q := url.Values{}
	q.Set("url", serverURL)
	q.Set("token", token)
	if deviceID != "" {
		q.Set("device", deviceID)
	}
	return "tgcontrol://pair?" + q.Encode()
}

// buildPairingCode creates a compact base64-encoded pairing code.
// Format: base64("url|token") — deliberately omits deviceID so the APK
// (which parses only the first two fields) and the Mini App use the same
// format. The APK generates its own deviceID on first connect.
func buildPairingCode(serverURL, token string) string {
	raw := fmt.Sprintf("%s|%s", serverURL, token)
	return base64.StdEncoding.EncodeToString([]byte(raw))
}

// apiSetupQR serves a QR code PNG image for the setup wizard.
// GET /api/setup/qr?data=...&size=256
func (s *Server) apiSetupQR(w http.ResponseWriter, r *http.Request) {
	data := r.URL.Query().Get("data")
	if data == "" {
		// Generate default pairing QR using best-available server URL
		cfg := config.GetNoSetup()
		defaultUID := cfg.APITokenUID
		if defaultUID == 0 {
			defaultUID = 1
		}
		cfg.EnsureAPIToken(defaultUID)
		deviceID := config.GetOrCreateDeviceID()
		serverURL := s.pickServerURL(cfg)
		data = buildPairingURL(serverURL, cfg.APIToken, deviceID)
	}

	size := 256
	png, err := generateQRCode(data, size)
	if err != nil {
		http.Error(w, "QR generation failed", 500)
		return
	}

	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-cache")
	w.Write(png)
}

// PickedURL describes the chosen pairing URL and its kind, for UI display.
type PickedURL struct {
	URL  string `json:"url"`
	Kind string `json:"kind"`           // "tunnel_running" | "tunnel_configured" | "lan" | "localhost"
	Note string `json:"note,omitempty"` // human-readable hint
}

// pickServerURL chooses the best server URL for pairing.
// Priority: 1) running tunnel, 2) configured tunnel URL, 3) LAN IP, 4) localhost.
func (s *Server) pickServerURL(cfg *config.Config) string {
	picked := s.pickServerURLInfo(cfg)
	return picked.URL
}

// pickServerURLInfo returns the picked URL together with metadata about
// which source it came from so the UI can label it explicitly.
func (s *Server) pickServerURLInfo(cfg *config.Config) PickedURL {
	// 1) running tunnel — most reliable: we know it's up right now
	if s.tunnelManager != nil {
		info := s.tunnelManager.Info()
		if info.URL != "" && string(info.Status) == "running" {
			return PickedURL{
				URL:  info.URL,
				Kind: "tunnel_running",
				Note: "Публичный URL — доступен из любой точки мира",
			}
		}
	}
	// 2) configured tunnel URL (even if not currently up)
	if cfg.TunnelURL != "" {
		return PickedURL{
			URL:  cfg.TunnelURL,
			Kind: "tunnel_configured",
			Note: "Настроенный публичный URL (туннель сейчас не запущен)",
		}
	}
	// 3) LAN IP — works inside the same Wi-Fi network
	if ips := getLocalIPs(); len(ips) > 0 {
		return PickedURL{
			URL:  fmt.Sprintf("http://%s:%d", ips[0], cfg.Port()),
			Kind: "lan",
			Note: "Локальная сеть — телефон должен быть в той же Wi-Fi",
		}
	}
	// 4) Fallback — same machine only
	return PickedURL{
		URL:  fmt.Sprintf("http://localhost:%d", cfg.Port()),
		Kind: "localhost",
		Note: "Только для этого компьютера",
	}
}
