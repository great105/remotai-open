//go:build !windows && !linux

package codec

import "errors"

// FindOpenH264DLL is Windows-only; elsewhere there's no DLL to find.
func FindOpenH264DLL(dirs ...string) string { return "" }

// NewH264 is not supported off Windows yet: the openh264 binding uses the
// Windows syscall/DLL API. Callers fall back to the JPEG-over-DataChannel path,
// which is fully functional on X11 (capture = kbinani pure-Go X11, transport =
// pion). A Linux H.264 encoder is a future opt-in: load openh264 .so via
// purego.Dlopen and reuse the ABI/structs/EncodeRGBA from h264_windows.go
// (deferred — it touches the raw-memory ABI shared with the shipping Windows
// encoder and needs a real X11 encode+browser-decode loop to validate).
func NewH264(dllPath string, w, h, fps, bitrate int) (Encoder, error) {
	return nil, errors.New("h264 encoder not supported on this platform")
}

// OpenH264InstallHint — ставить нечего: H.264 на этой платформе не поддержан.
const OpenH264InstallHint = ""
