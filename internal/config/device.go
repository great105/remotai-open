package config

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"

	"tgcontrol/internal/procutil"
)

// DeviceInfo holds hardware-derived device identification.
type DeviceInfo struct {
	DeviceID   string `json:"device_id"`
	Hostname   string `json:"hostname"`
	OS         string `json:"os"`
	Arch       string `json:"arch"`
	MACAddress string `json:"mac_address"`
	Created    int64  `json:"created"`
}

var (
	_deviceInfo *DeviceInfo
	_deviceOnce sync.Once
)

// GetDeviceInfo returns cached device info singleton.
func GetDeviceInfo() *DeviceInfo {
	_deviceOnce.Do(func() {
		_deviceInfo = collectDeviceInfo()
	})
	return _deviceInfo
}

var _deviceIDOnce sync.Once

// GetOrCreateDeviceID returns device ID from config, or generates and saves one.
func GetOrCreateDeviceID() string {
	cfg := GetNoSetup()
	if cfg.DeviceID != "" {
		return cfg.DeviceID
	}
	_deviceIDOnce.Do(func() {
		info := GetDeviceInfo()
		_mu.Lock()
		if _config != nil && _config.DeviceID == "" {
			_config.DeviceID = info.DeviceID
			_config.Save()
		}
		_mu.Unlock()
	})
	return cfg.DeviceID
}

// GenerateDeviceID creates a deterministic device ID from hardware fingerprint.
func GenerateDeviceID() string {
	hostname, _ := os.Hostname()
	mac := getPrimaryMAC()
	serial := getMachineSerial()

	raw := fmt.Sprintf("%s|%s|%s", hostname, mac, serial)
	hash := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(hash[:])[:12]
}

func collectDeviceInfo() *DeviceInfo {
	hostname, _ := os.Hostname()
	mac := getPrimaryMAC()
	deviceID := GenerateDeviceID()

	return &DeviceInfo{
		DeviceID:   deviceID,
		Hostname:   hostname,
		OS:         runtime.GOOS,
		Arch:       runtime.GOARCH,
		MACAddress: mac,
		Created:    time.Now().Unix(),
	}
}

// getPrimaryMAC returns the MAC address of the first active non-loopback interface.
func getPrimaryMAC() string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return "unknown"
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		if len(iface.HardwareAddr) == 0 {
			continue
		}
		// Skip virtual adapters (common on Windows)
		name := strings.ToLower(iface.Name)
		if strings.Contains(name, "virtual") || strings.Contains(name, "vethernet") {
			continue
		}
		return iface.HardwareAddr.String()
	}
	// Fallback: return first non-empty MAC
	for _, iface := range ifaces {
		if len(iface.HardwareAddr) > 0 {
			return iface.HardwareAddr.String()
		}
	}
	return "unknown"
}

// getMachineSerial returns a machine-unique serial number.
func getMachineSerial() string {
	switch runtime.GOOS {
	case "windows":
		// Окно гасим: серийник машины собирается фоном (device_id), человеку
		// показывать нечего — а wmic без этого моргает чёрным окном.
		out, err := procutil.Hidden(exec.Command("wmic", "csproduct", "get", "uuid")).Output()
		if err == nil {
			lines := strings.Split(strings.TrimSpace(string(out)), "\n")
			if len(lines) >= 2 {
				serial := strings.TrimSpace(lines[len(lines)-1])
				if serial != "" && serial != "UUID" {
					return serial
				}
			}
		}
	case "linux":
		data, err := os.ReadFile("/etc/machine-id")
		if err == nil {
			return strings.TrimSpace(string(data))
		}
	case "darwin":
		// Та же фоновая проба на macOS; Hidden там пустышка, но код общий.
		out, err := procutil.Hidden(exec.Command("ioreg", "-rd1", "-c", "IOPlatformExpertDevice")).Output()
		if err == nil {
			for _, line := range strings.Split(string(out), "\n") {
				if strings.Contains(line, "IOPlatformSerialNumber") {
					parts := strings.SplitN(line, "=", 2)
					if len(parts) == 2 {
						return strings.Trim(strings.TrimSpace(parts[1]), "\"")
					}
				}
			}
		}
	}
	hostname, _ := os.Hostname()
	return "fallback-" + hostname
}
