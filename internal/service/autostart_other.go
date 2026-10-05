//go:build !darwin

package service

import "errors"

// Other platforms keep their scheduler adapters in internal/web.
func SetAutostart(bool) error { return errors.New("launchd autostart requires macOS") }
func AutostartEnabled() bool  { return false }
