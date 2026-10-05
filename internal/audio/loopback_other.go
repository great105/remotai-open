//go:build !windows

package audio

import "errors"

// Системный звук пока берём только на Windows. На Linux звук виртуального
// браузера идёт своим путём (internal/vbrowser/audio_linux.go, Opus), на маке
// захват требует отдельного разрешения и своей реализации.

// Available — можно ли брать звук с этой машины.
func Available() bool { return false }

// Subscribe — подписка на куски звука. Здесь её нет.
func Subscribe() (<-chan []byte, func(), error) {
	return nil, nil, errors.New("системный звук на этой системе пока не поддерживается")
}
