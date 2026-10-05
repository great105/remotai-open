//go:build !windows

package web

import "image"

// screenSource — то, чем цикл зрителя берёт кадры. Вне Windows источник
// прежний: захват на каждый кадр и вопрос «менялось ли» тому, кто может
// ответить (X DAMAGE на Linux, иначе — «да, снимай и сравнивай по хэшу»).
type screenSource struct{}

func newScreenSource(image.Rectangle) *screenSource { return &screenSource{} }

func (s *screenSource) Changed(bounds image.Rectangle) bool { return screenChanged(bounds) }

func (s *screenSource) Grab(bounds image.Rectangle) (*image.RGBA, error) { return grabScreen(bounds) }
