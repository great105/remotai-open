import { describe, expect, it } from "vitest";
import {
  TrackpadGesture, LONG_PRESS_MS, PINCH_RATIO, SCROLL_PX_PER_NOTCH, TAP_MS, THREE_SWIPE_PX,
} from "./trackpad";
import type { PadAction, PadSurface } from "./trackpad";

const stage: PadSurface = { width: 400, height: 800, sensitivity: 1.5, allowZoom: true };
const strip: PadSurface = { width: 400, height: 800, sensitivity: 1.5, allowZoom: false };
const kinds = (a: PadAction[]) => a.map((x) => x.kind);

describe("тачпад: один палец", () => {
  it("короткий тап — левый клик под курсором, без движения", () => {
    const g = new TrackpadGesture();
    const a0 = g.start([{ x: 100, y: 100 }], 1000, stage);
    expect(kinds(a0)).toContain("armLongPress");
    const a1 = g.end(0, 1100);
    expect(a1).toContainEqual({ kind: "click", button: "l", second: false });
    expect(kinds(a1)).not.toContain("move");
  });

  it("второй тап за 300 мс — ВТОРОЙ обычный клик, а не dblclick", () => {
    const g = new TrackpadGesture();
    g.start([{ x: 100, y: 100 }], 1000, stage);
    g.end(0, 1080);
    g.start([{ x: 103, y: 101 }], 1200, stage);
    const a = g.end(0, 1280);
    expect(a).toContainEqual({ kind: "click", button: "l", second: true });
    // третий тап подряд снова «первый» — пара закрыта
    g.start([{ x: 100, y: 100 }], 1400, stage);
    expect(g.end(0, 1480)).toContainEqual({ kind: "click", button: "l", second: false });
  });

  it("движение — сдвиг курсора в долях кадра с чувствительностью, тапа нет", () => {
    const g = new TrackpadGesture();
    g.start([{ x: 100, y: 100 }], 1000, stage);
    const a = g.move([{ x: 140, y: 120 }], 1016);
    expect(kinds(a)).toContain("disarmLongPress");
    const mv = a.find((x) => x.kind === "move");
    expect(mv).toBeDefined();
    if (mv && mv.kind === "move") {
      expect(mv.dx).toBeCloseTo((40 * 1.5) / 400, 6);
      expect(mv.dy).toBeCloseTo((20 * 1.5) / 800, 6);
    }
    const end = g.end(0, 1100);
    expect(kinds(end)).not.toContain("click");
  });

  it("быстрый свайп даёт инерцию, медленный — нет", () => {
    const g = new TrackpadGesture();
    g.start([{ x: 100, y: 100 }], 1000, stage);
    for (let i = 1; i <= 5; i++) g.move([{ x: 100 + i * 30, y: 100 }], 1000 + i * 16);
    expect(kinds(g.end(0, 1090))).toContain("fling");
    const slow = new TrackpadGesture();
    slow.start([{ x: 100, y: 100 }], 1000, stage);
    for (let i = 1; i <= 5; i++) slow.move([{ x: 100 + i * 1, y: 100 }], 1000 + i * 90);
    expect(kinds(slow.end(0, 1500))).not.toContain("fling");
  });

  it("долгое нажатие без движения — перетаскивание до отрыва", () => {
    const g = new TrackpadGesture();
    const a0 = g.start([{ x: 100, y: 100 }], 1000, stage);
    expect(a0).toContainEqual({ kind: "armLongPress", ms: LONG_PRESS_MS });
    expect(g.longPress()).toEqual([{ kind: "dragStart" }]);
    expect(g.dragging).toBe(true);
    const mv = g.move([{ x: 160, y: 100 }], 1600);
    expect(kinds(mv)).toContain("move");
    const end = g.end(0, 1700);
    expect(kinds(end)).toContain("dragEnd");
    expect(kinds(end)).not.toContain("click");
    expect(g.dragging).toBe(false);
  });

  it("долгое нажание не срабатывает, если палец уже двигался", () => {
    const g = new TrackpadGesture();
    g.start([{ x: 100, y: 100 }], 1000, stage);
    g.move([{ x: 130, y: 100 }], 1050);
    expect(g.longPress()).toEqual([]);
  });
});

describe("тачпад: два и три пальца", () => {
  it("тап двумя пальцами — правый клик", () => {
    const g = new TrackpadGesture();
    g.start([{ x: 100, y: 100 }], 1000, stage);
    g.start([{ x: 100, y: 100 }, { x: 150, y: 100 }], 1010, stage);
    g.end(1, 1100);
    const a = g.end(0, 1110);
    expect(a).toContainEqual({ kind: "click", button: "r", second: false });
    expect(a.filter((x) => x.kind === "click")).toHaveLength(1);
  });

  it("два пальца вертикально — щелчки колеса по 8 px, без клика", () => {
    const g = new TrackpadGesture();
    g.start([{ x: 100, y: 100 }], 1000, stage);
    g.start([{ x: 100, y: 100 }, { x: 150, y: 100 }], 1010, stage);
    g.move([{ x: 100, y: 120 }, { x: 150, y: 120 }], 1030); // взводит порог
    const a = g.move([{ x: 100, y: 136 }, { x: 150, y: 136 }], 1050);
    const sc = a.find((x) => x.kind === "scroll");
    expect(sc).toBeDefined();
    if (sc && sc.kind === "scroll") {
      expect(sc.dy).toBeCloseTo(16 / SCROLL_PX_PER_NOTCH, 6);
      expect(sc.dx).toBe(0);
    }
    g.end(1, 1100);
    expect(kinds(g.end(0, 1110))).not.toContain("click");
  });

  it("два пальца по горизонтали — горизонтальная прокрутка", () => {
    const g = new TrackpadGesture();
    g.start([{ x: 100, y: 100 }, { x: 150, y: 100 }], 1000, stage);
    g.move([{ x: 120, y: 100 }, { x: 170, y: 100 }], 1020);
    const a = g.move([{ x: 144, y: 100 }, { x: 194, y: 100 }], 1040);
    const sc = a.find((x) => x.kind === "scroll");
    expect(sc && sc.kind === "scroll" ? sc.dx : 0).toBeCloseTo(24 / SCROLL_PX_PER_NOTCH, 6);
  });

  it("щипок — масштаб там, где разрешён; на полосе — нет", () => {
    const g = new TrackpadGesture();
    g.start([{ x: 100, y: 100 }, { x: 200, y: 100 }], 1000, stage);
    const a = g.move([{ x: 80, y: 100 }, { x: 240, y: 100 }], 1030); // 100 → 160 px
    const z = a.find((x) => x.kind === "zoom");
    expect(z && z.kind === "zoom" ? z.ratio : 0).toBeCloseTo(1.6, 6);
    // обратно в прокрутку не превращается
    const b = g.move([{ x: 80, y: 130 }, { x: 240, y: 130 }], 1060);
    expect(kinds(b)).not.toContain("scroll");
    const p = new TrackpadGesture();
    p.start([{ x: 100, y: 100 }, { x: 200, y: 100 }], 1000, strip);
    const c = p.move([{ x: 80, y: 100 }, { x: 240, y: 100 }], 1030);
    expect(kinds(c)).not.toContain("zoom");
    expect(PINCH_RATIO).toBeLessThan(0.6);
  });

  it("три пальца вправо — Alt+Tab, один раз за жест", () => {
    const g = new TrackpadGesture();
    const three = (x: number) => [{ x, y: 100 }, { x: x + 40, y: 100 }, { x: x + 80, y: 100 }];
    g.start(three(100), 1000, stage);
    const a = g.move(three(100 + THREE_SWIPE_PX + 5), 1050);
    expect(a).toContainEqual({ kind: "altTab", dir: 1 });
    expect(kinds(g.move(three(100 + THREE_SWIPE_PX + 60), 1080))).not.toContain("altTab");
  });

  it("удержание дольше TAP_MS без движения — не тап", () => {
    const g = new TrackpadGesture();
    g.start([{ x: 100, y: 100 }], 1000, stage);
    expect(kinds(g.end(0, 1000 + TAP_MS + 50))).not.toContain("click");
  });
});
