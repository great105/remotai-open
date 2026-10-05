//go:build !linux && !windows

package web

import (
	"image"

	"github.com/kbinani/screenshot"
)

// grabScreen — снимок области экрана.
func grabScreen(bounds image.Rectangle) (*image.RGBA, error) {
	return screenshot.CaptureRect(bounds)
}

// screenChanged — менялось ли изображение с прошлого кадра. Здесь спросить не у
// кого (на Linux отвечает X DAMAGE, на Windows — DXGI): отвечаем «да», то есть
// кадр захватывается как раньше и сравнивается по хэшу.
func screenChanged(image.Rectangle) bool { return true }
