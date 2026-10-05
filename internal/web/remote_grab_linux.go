//go:build linux

package web

import (
	"image"

	"github.com/kbinani/screenshot"

	"tgcontrol/internal/xdamage"
)

// grabScreen — снимок области экрана.
func grabScreen(bounds image.Rectangle) (*image.RGBA, error) {
	return screenshot.CaptureRect(bounds)
}

// screenChanged спрашивает у X-сервера (расширение DAMAGE), перерисовывалось ли
// что-нибудь с прошлого кадра. Неподвижная страница при этом не стоит ничего:
// раньше каждый кадр тянул с экрана 4 МБ, чтобы выбросить их после сравнения.
//
// При любой неполадке слежение отвечает «менялось» — поведение возвращается к
// прежнему, захват на каждый кадр.
func screenChanged(image.Rectangle) bool { return xdamage.Changed() }
