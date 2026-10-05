//go:build !linux

package vbrowser

import "fmt"

// Status is the JSON view of the feature state (always unavailable off Linux).
type Status struct {
	Available bool     `json:"available"`
	XvfbPath  string   `json:"xvfb_path,omitempty"`
	Browsers  []string `json:"browsers,omitempty"`
	Running   bool     `json:"running"`
	Display   string   `json:"display,omitempty"`
	Browser   string   `json:"browser,omitempty"`
	Since     int64    `json:"since,omitempty"`
	Hint      string   `json:"hint,omitempty"`

	// Ввод: вне Linux он встроен в ОС (SendInput/CGEvent), доустанавливать
	// нечего — поэтому всегда готов.
	InputReady      bool   `json:"input_ready"`
	InputHint       string `json:"input_hint,omitempty"`
	CanInstallInput bool   `json:"can_install_input,omitempty"`

	// Supervisor bookkeeping (Linux-only feature; always zero here).
	Restarts  int    `json:"restarts"`
	LastError string `json:"last_error,omitempty"`

	// Звук виртуального браузера — фича Linux (PulseAudio); здесь всегда нули.
	Audio     bool   `json:"audio"`
	AudioHint string `json:"audio_hint,omitempty"`
}

var errNotLinux = fmt.Errorf("virtual browser is only supported on Linux agents")

// GetStatus reports the current feature state.
func GetStatus() Status { return Status{InputReady: true} }

// StartInstallInput is unsupported off Linux (input tooling is built into the OS).
func StartInstallInput() error { return errNotLinux }

// Running reports whether a virtual display session is active.
func Running() bool { return false }

// Start is unsupported off Linux.
func Start(_ string, _, _ int, _ string) (Status, error) { return Status{}, errNotLinux }

// Stop is a no-op off Linux.
func Stop() {}

// DebugEndpoint is empty off Linux (no virtual browser to talk to).
func DebugEndpoint() string { return "" }

// ScreenSize is zero off Linux (no virtual screen).
func ScreenSize() (int, int) { return 0, 0 }

// NoteActivity is a no-op off Linux (no session to keep alive).
func NoteActivity() {}

// SetViewers is a no-op off Linux (no idle timer without a session).
func SetViewers(_ int) {}

// SetEventCallback is a no-op off Linux (no lifecycle events without a session).
func SetEventCallback(_ func(kind string, fields map[string]any)) {}

// PrepareUploads is unavailable without the Linux virtual browser.
func PrepareUploads(_ []UploadFile) ([]string, error) { return nil, errNotLinux }

// DiscardUploads is a no-op off Linux.
func DiscardUploads(_ []string) {}
