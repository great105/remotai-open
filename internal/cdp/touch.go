package cdp

// Касания вместо мыши.
//
// Страница, которой прислали mousedown, ведёт себя как на компьютере: нет ни
// свайпов, ни инерции, ни жестов самих сайтов, ни подсветки нажатия. Настоящие
// touch-события включают всё это разом — карты начинают тащиться пальцем,
// карусели листаться, а меню открываться так, как задумано на телефоне.
//
// Инерцию мы считаем на стороне клиента и досылаем как прокрутку: готовый жест
// CDP (synthesizeScrollGesture) на виртуальном экране не работает — он требует
// настоящего дисплея и на Xvfb молча ничего не делает.

import (
	"context"
	"fmt"
)

// TouchPoint — одна точка касания в координатах страницы (CSS-пиксели).
type TouchPoint struct {
	ID     int     `json:"id"`
	X      float64 `json:"x"`
	Y      float64 `json:"y"`
	Radius float64 `json:"radius,omitempty"`
	Force  float64 `json:"force,omitempty"`
}

// Touch отправляет касание. kind: "start" | "move" | "end" | "cancel".
// Для "end" точки передавать не нужно — палец уже оторван.
func (c *Client) Touch(ctx context.Context, kind string, points []TouchPoint) error {
	typeName := ""
	switch kind {
	case "start":
		typeName = "touchStart"
	case "move":
		typeName = "touchMove"
	case "end":
		typeName = "touchEnd"
	case "cancel":
		typeName = "touchCancel"
	default:
		return fmt.Errorf("неизвестный вид касания %q", kind)
	}

	list := make([]map[string]any, 0, len(points))
	for _, p := range points {
		radius := p.Radius
		if radius <= 0 {
			radius = 12 // палец, а не пиксель: сайты меряют площадь нажатия
		}
		force := p.Force
		if force <= 0 {
			force = 1
		}
		list = append(list, map[string]any{
			"x": p.X, "y": p.Y, "id": p.ID,
			"radiusX": radius, "radiusY": radius, "force": force,
		})
	}
	_, err := c.Call(ctx, "Input.dispatchTouchEvent", map[string]any{
		"type":        typeName,
		"touchPoints": list,
	})
	return err
}

// Wheel — прокрутка колесом в точке. Ею же досылается инерция после того, как
// палец оторвался: touchEnd страница уже получила, и продолжать движение
// касаниями нельзя.
func (c *Client) Wheel(ctx context.Context, x, y, dx, dy float64) error {
	_, err := c.Call(ctx, "Input.dispatchMouseEvent", map[string]any{
		"type": "mouseWheel", "x": x, "y": y,
		"deltaX": dx, "deltaY": dy,
	})
	return err
}
