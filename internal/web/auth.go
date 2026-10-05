package web

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"tgcontrol/internal/config"
)

// ValidateInitData validates Telegram Mini App initData (HMAC-SHA256).
// Returns parsed data or nil if invalid.
func ValidateInitData(initData, botToken string, maxAge int64) map[string]any {
	if initData == "" || botToken == "" {
		return nil
	}

	parsed, err := url.ParseQuery(initData)
	if err != nil {
		return nil
	}

	data := make(map[string]string)
	for k, v := range parsed {
		if len(v) > 0 {
			data[k] = v[0]
		} else {
			data[k] = ""
		}
	}

	receivedHash := data["hash"]
	if receivedHash == "" {
		return nil
	}
	delete(data, "hash")

	// Freshness check
	authDateStr := data["auth_date"]
	if authDateStr == "" {
		return nil
	}
	authDate, err := strconv.ParseInt(authDateStr, 10, 64)
	if err != nil {
		return nil
	}
	if time.Now().Unix()-authDate > maxAge {
		return nil
	}

	// data-check-string: sorted key=value pairs joined by \n
	keys := make([]string, 0, len(data))
	for k := range data {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = fmt.Sprintf("%s=%s", k, data[k])
	}
	dataCheckString := strings.Join(parts, "\n")

	// HMAC-SHA256
	secret := hmacSHA256([]byte("WebAppData"), []byte(botToken))
	computed := hmacSHA256(secret, []byte(dataCheckString))
	computedHex := hex.EncodeToString(computed)

	if !hmac.Equal([]byte(computedHex), []byte(receivedHash)) {
		return nil
	}

	// Parse embedded JSON fields
	result := make(map[string]any)
	for k, v := range data {
		result[k] = v
	}
	result["hash"] = receivedHash

	for _, field := range []string{"user", "receiver", "chat"} {
		raw, ok := data[field]
		if !ok {
			continue
		}
		var parsed any
		if err := json.Unmarshal([]byte(raw), &parsed); err == nil {
			result[field] = parsed
		}
	}

	return result
}

// ValidateAPIToken checks a static API token from config. Returns mapped UID or 0.
func ValidateAPIToken(token string) int64 {
	cfg := config.Get()
	if cfg.APIToken == "" || token != cfg.APIToken {
		return 0
	}
	if cfg.APITokenUID != 0 {
		return cfg.APITokenUID
	}
	return 1
}

func hmacSHA256(key, message []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write(message)
	return h.Sum(nil)
}

// ExtractUID extracts user ID from validated initData.
func ExtractUID(data map[string]any) int64 {
	user, ok := data["user"]
	if !ok {
		return 0
	}
	userMap, ok := user.(map[string]any)
	if !ok {
		return 0
	}
	id, ok := userMap["id"]
	if !ok {
		return 0
	}
	switch v := id.(type) {
	case float64:
		return int64(v)
	case json.Number:
		n, _ := v.Int64()
		return n
	}
	return 0
}

// apiAuthToken returns the current API token for standalone APK auth.
// Auto-generates one if missing, using the requesting user's UID.
func (s *Server) apiAuthToken(w http.ResponseWriter, r *http.Request, uid int64) {
	cfg := config.Get()
	cfg.EnsureAPIToken(uid)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"token": cfg.APIToken,
		"uid":   cfg.APITokenUID,
	})
}

// apiAuthPair returns pairing info for the standalone APK:
// token, port, LAN IPs, hostname, and a compact pairing code.
// The pairing code prefers a tunnel URL over LAN-IP so the code keeps working
// when the phone is outside the home Wi-Fi.
func (s *Server) apiAuthPair(w http.ResponseWriter, r *http.Request, uid int64) {
	cfg := config.Get()
	cfg.EnsureAPIToken(uid)

	port := cfg.Port()
	hostname, _ := os.Hostname()
	lanIPs := getLocalIPs()

	// LAN URLs for "manual" fallback
	var urls []string
	for _, ip := range lanIPs {
		urls = append(urls, fmt.Sprintf("http://%s:%d", ip, port))
	}

	// Pick best URL: tunnel > LAN > localhost
	picked := s.pickServerURLInfo(cfg)
	pairingCode := buildPairingCode(picked.URL, cfg.APIToken)
	pairingURL := buildPairingURL(picked.URL, cfg.APIToken, cfg.DeviceID)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"token":        cfg.APIToken,
		"port":         port,
		"hostname":     hostname,
		"lan_ips":      lanIPs,
		"urls":         urls,
		"server_url":   picked.URL,
		"server_kind":  picked.Kind,
		"server_note":  picked.Note,
		"pairing_code": pairingCode,
		"pairing_url":  pairingURL,
	})
}

// getLocalIPs returns non-loopback IPv4 addresses.
func getLocalIPs() []string {
	var ips []string
	ifaces, err := net.Interfaces()
	if err != nil {
		return ips
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			var ip net.IP
			switch v := addr.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			if ip == nil || ip.IsLoopback() || ip.To4() == nil {
				continue
			}
			ips = append(ips, ip.String())
		}
	}
	// Prefer real home/office LAN ranges first so the pairing QR/code embeds an
	// address the phone can actually reach — not a Docker/WSL bridge (172.x) or
	// a Tailscale CGNAT (100.64/10) address.
	sort.SliceStable(ips, func(i, j int) bool { return lanRank(ips[i]) < lanRank(ips[j]) })
	return ips
}

// lanRank ranks an IPv4 by how likely a phone on the same network can reach it.
func lanRank(ip string) int {
	switch {
	case strings.HasPrefix(ip, "192.168."):
		return 0
	case strings.HasPrefix(ip, "10."):
		return 1
	case strings.HasPrefix(ip, "172."): // often Docker/WSL bridges
		return 3
	case strings.HasPrefix(ip, "100."): // CGNAT (Tailscale)
		return 4
	default:
		return 2
	}
}
