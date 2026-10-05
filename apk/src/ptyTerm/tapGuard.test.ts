import { describe, expect, it } from "vitest";
import {
  emptyTapGuard, noteDown, noteMove, noteScroll, noteExpanded, tapVerdict,
  MOVE_TOLERANCE_PX, SCROLL_COOLDOWN_MS, EXPAND_COOLDOWN_MS,
} from "./tapGuard";

describe("случайное нажатие в прокручиваемом ряду", () => {
  it("обычный тап проходит: ради него ряд и существует", () => {
    let s = emptyTapGuard();
    s = noteDown(s, 100, 500);
    s = noteMove(s, 101, 501); // палец всегда чуть дрожит
    expect(tapVerdict(s, 1000)).toEqual({ ok: true });
  });

  it("палец уехал вбок — это прокрутка, а не команда", () => {
    let s = emptyTapGuard();
    s = noteDown(s, 200, 500);
    s = noteMove(s, 200 - (MOVE_TOLERANCE_PX + 1), 500);
    expect(tapVerdict(s, 1000)).toEqual({ ok: false, why: "moved" });
  });

  it("палец уехал вниз — человек листает страницу", () => {
    let s = emptyTapGuard();
    s = noteDown(s, 200, 500);
    s = noteMove(s, 200, 500 + MOVE_TOLERANCE_PX + 1);
    expect(tapVerdict(s, 1000)).toEqual({ ok: false, why: "moved" });
  });

  it("уехавший палец не «возвращается»: вернулся на место — всё равно прокрутка", () => {
    // Иначе быстрый свайп туда-обратно отправил бы команду.
    let s = emptyTapGuard();
    s = noteDown(s, 200, 500);
    s = noteMove(s, 260, 500);
    s = noteMove(s, 200, 500);
    expect(tapVerdict(s, 1000)).toEqual({ ok: false, why: "moved" });
  });

  it("лента ещё едет по инерции — следующее касание её ОСТАНАВЛИВАЕТ", () => {
    let s = emptyTapGuard();
    s = noteScroll(s, 1000);
    s = noteDown(s, 100, 500);
    expect(tapVerdict(s, 1000 + SCROLL_COOLDOWN_MS - 50)).toEqual({ ok: false, why: "scrolling" });
    // Остановилась — снова обычный ряд.
    expect(tapVerdict(s, 1000 + SCROLL_COOLDOWN_MS + 1)).toEqual({ ok: true });
  });

  it("ряд только что развернули — команда под пальцем ещё не была командой", () => {
    let s = emptyTapGuard();
    s = noteExpanded(s, 5000);
    s = noteDown(s, 100, 500);
    expect(tapVerdict(s, 5000 + EXPAND_COOLDOWN_MS - 50)).toEqual({ ok: false, why: "just-expanded" });
    expect(tapVerdict(s, 5000 + EXPAND_COOLDOWN_MS + 1)).toEqual({ ok: true });
  });

  it("мышь и клавиатура правилами движения не связаны", () => {
    // pointerdown с мышью мы не пишем: там промахнуться прокруткой нечем,
    // а клавиатурный Enter вообще приходит без координат.
    const s = emptyTapGuard();
    expect(tapVerdict(s, 10_000)).toEqual({ ok: true });
  });

  it("новое касание сбрасывает прошлый свайп", () => {
    let s = emptyTapGuard();
    s = noteDown(s, 200, 500);
    s = noteMove(s, 300, 500);
    expect(tapVerdict(s, 1000).ok).toBe(false);
    s = noteDown(s, 200, 500); // человек убрал палец и нажал заново
    expect(tapVerdict(s, 2000)).toEqual({ ok: true });
  });
});
