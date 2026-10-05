import { describe, expect, it } from "vitest";
import { compatMouseAfterTap, edgeDirection, TAP_COMPAT_MOUSE_MS } from "./GestureController";

// ST-10 (tapClickOnce): одно касание — один клик приложению.
describe("compatMouseAfterTap — совместимые события мыши после касания", () => {
  it("гасит mousedown/mouseup только в окне сразу после нашего клика", () => {
    expect(compatMouseAfterTap(1000, 1000)).toBe(true);
    expect(compatMouseAfterTap(1000 + TAP_COMPAT_MOUSE_MS - 1, 1000)).toBe(true);
    expect(compatMouseAfterTap(1000 + TAP_COMPAT_MOUSE_MS, 1000)).toBe(false);
  });

  it("настоящая мышь без касания и клик до касания не гасятся", () => {
    expect(compatMouseAfterTap(5000, -Infinity)).toBe(false);
    expect(compatMouseAfterTap(900, 1000)).toBe(false);
    expect(compatMouseAfterTap(NaN, 1000)).toBe(false);
  });
});

// T-12: ручка выделения у края — управляемая автопрокрутка по ОБЕИМ осям.
describe("edgeDirection — автопрокрутка ручки у края", () => {
  const rect = { left: 0, top: 100, right: 390, bottom: 700 };

  it("вертикаль как раньше: верх/низ в поле 36 px, середина стоит", () => {
    expect(edgeDirection({ x: 200, y: 110 }, rect)).toEqual({ dx: 0, dy: -1 });
    expect(edgeDirection({ x: 200, y: 690 }, rect)).toEqual({ dx: 0, dy: 1 });
    expect(edgeDirection({ x: 200, y: 400 }, rect)).toEqual({ dx: 0, dy: 0 });
  });

  it("горизонталь: документ на 240 колонок едет вбок за ручкой у левого и правого края", () => {
    expect(edgeDirection({ x: 380, y: 400 }, rect)).toEqual({ dx: 1, dy: 0 });
    expect(edgeDirection({ x: 10, y: 400 }, rect)).toEqual({ dx: -1, dy: 0 });
    // Угол — обе оси сразу.
    expect(edgeDirection({ x: 385, y: 695 }, rect)).toEqual({ dx: 1, dy: 1 });
  });

  it("за пределами области направление сохраняется (палец ушёл за край экрана)", () => {
    expect(edgeDirection({ x: 500, y: 50 }, rect)).toEqual({ dx: 1, dy: -1 });
  });

  it("на узкой области поля краёв не перекрываются: в центре направления нет", () => {
    const narrow = { left: 0, top: 0, right: 50, bottom: 50 };
    expect(edgeDirection({ x: 25, y: 25 }, narrow)).toEqual({ dx: 0, dy: 0 });
    expect(edgeDirection({ x: 5, y: 45 }, narrow)).toEqual({ dx: -1, dy: 1 });
  });

  it("поле края настраивается", () => {
    expect(edgeDirection({ x: 200, y: 150 }, rect, 60)).toEqual({ dx: 0, dy: -1 });
    expect(edgeDirection({ x: 200, y: 150 }, rect, 36)).toEqual({ dx: 0, dy: 0 });
  });
});
