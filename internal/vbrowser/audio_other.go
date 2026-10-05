//go:build !linux

package vbrowser

// AudioAvailable is always false off Linux (the capture rides PulseAudio).
func AudioAvailable() bool { return false }

// AudioActive is always false off Linux.
func AudioActive() bool { return false }

// SubscribeAudio is unsupported off Linux.
func SubscribeAudio() (<-chan []byte, func(), error) { return nil, nil, errNotLinux }
