import { describe, expect, it } from "vitest";
import {
  chromeOnDrag, doubleTapScale, edgeSwipe, flingSteps, flingVelocity, isTap,
  pinchDistance, pinchScale, pullRefresh,
} from "./browserGestures";

describe("flingVelocity", () => {
  it("считает скорость по концу жеста, а не по всему пути", () => {
    // Палец полз, а в конце дёрнулся — инерция должна слушать дёрганье.
    const v = flingVelocity([
      { x: 0.5, y: 0.20, t: 0 },
      { x: 0.5, y: 0.22, t: 400 },
      { x: 0.5, y: 0.60, t: 480 },
    ]);
    expect(v).toBeGreaterThan(1);
  });

  it("одна точка — скорости нет", () => {
    expect(flingVelocity([{ x: 0.5, y: 0.5, t: 0 }])).toBe(0);
  });
});

describe("flingSteps", () => {
  it("медленное отпускание инерции не даёт (это был тап)", () => {
    expect(flingSteps(0.1, 1200)).toEqual([]);
  });

  it("бросок даёт затухающую серию в сторону, обратную движению пальца", () => {
    const steps = flingSteps(2, 1200);
    expect(steps.length).toBeGreaterThan(3);
    expect(steps[0].dy).toBeLessThan(0); // палец вниз → страница вверх
    expect(Math.abs(steps[steps.length - 1].dy)).toBeLessThan(Math.abs(steps[0].dy));
  });
});

describe("pinchScale", () => {
  it("разведённые пальцы увеличивают, сведённые уменьшают", () => {
    expect(pinchScale(1, 0.2, 0.4)).toBeCloseTo(2);
    expect(pinchScale(1, 0.4, 0.2)).toBeCloseTo(0.5);
  });

  it("держится в границах читаемости", () => {
    expect(pinchScale(1, 0.1, 10)).toBe(5);
    expect(pinchScale(1, 10, 0.01)).toBe(0.25);
  });

  it("нулевое стартовое расстояние не ломает масштаб", () => {
    expect(pinchScale(1.5, 0, 0.3)).toBe(1.5);
  });
});

describe("isTap", () => {
  it("короткое касание почти на месте — тап", () => {
    expect(isTap({ x: 0.5, y: 0.5 }, { x: 0.505, y: 0.503 }, 120)).toBe(true);
  });

  it("протяжка тапом не считается", () => {
    expect(isTap({ x: 0.5, y: 0.5 }, { x: 0.5, y: 0.8 }, 200)).toBe(false);
  });

  it("долгое удержание — не тап", () => {
    expect(isTap({ x: 0.5, y: 0.5 }, { x: 0.5, y: 0.5 }, 900)).toBe(false);
  });
});

describe("pinchDistance", () => {
  it("меряет расстояние между пальцами", () => {
    expect(pinchDistance({ x: 0, y: 0 }, { x: 0.3, y: 0.4 })).toBeCloseTo(0.5);
  });
});

describe("chromeOnDrag", () => {
  it("палец вверх (листаем дальше) прячет хром, но лишь после заметного хода", () => {
    // Дрожь пальца в пару пикселей панели не дёргает.
    expect(chromeOnDrag(0, -10).hidden).toBeNull();
    // ...но те же движения, сложенные подряд, уже решают.
    let acc = 0;
    let decision: boolean | null = null;
    for (const dy of [-10, -10, -10, -10]) {
      const r = chromeOnDrag(acc, dy);
      acc = r.acc;
      if (r.hidden !== null) decision = r.hidden;
    }
    expect(decision).toBe(true);
  });

  it("палец вниз (листаем обратно) возвращает хром", () => {
    expect(chromeOnDrag(0, 40).hidden).toBe(false);
  });

  it("качели вокруг нуля решения не дают", () => {
    let acc = 0;
    for (const dy of [20, -20, 20, -20, 15, -15]) {
      const r = chromeOnDrag(acc, dy);
      expect(r.hidden).toBeNull();
      acc = r.acc;
    }
  });

  it("после решения аккумулятор обнуляется: повторный перелом требует нового хода", () => {
    const first = chromeOnDrag(0, -40);
    expect(first.hidden).toBe(true);
    expect(first.acc).toBe(0);
    // Продолжение того же скролла мелкими шагами не спамит решениями.
    expect(chromeOnDrag(first.acc, -10).hidden).toBeNull();
  });
});

describe("edgeSwipe", () => {
  it("от левого края вправо — это «назад»", () => {
    expect(edgeSwipe(10, 120, 20, 400)).toBe("back");
  });

  it("от правого края влево — «вперёд»", () => {
    expect(edgeSwipe(392, -140, 10, 400)).toBe("forward");
  });

  it("свайп из середины экрана жестом навигации не считается", () => {
    // Иначе жест крал бы карусели и свайп по карточкам самой страницы.
    expect(edgeSwipe(200, 150, 10, 400)).toBeNull();
  });

  it("наклонное движение — это прокрутка, а не «назад»", () => {
    expect(edgeSwipe(10, 80, 200, 400)).toBeNull();
  });
});

describe("pullRefresh", () => {
  it("работает только у самого верха страницы", () => {
    expect(pullRefresh(300, 200).fire).toBe(false);
    expect(pullRefresh(0, 200).fire).toBe(true);
  });

  it("натяжение растёт медленнее пальца — это резинка, а не срыв", () => {
    const { progress, fire } = pullRefresh(0, 50);
    expect(fire).toBe(false);
    expect(progress).toBeGreaterThan(0);
    expect(progress).toBeLessThan(50 / 90 + 0.01);
  });

  it("движение вверх ничего не тянет", () => {
    expect(pullRefresh(0, -100)).toEqual({ progress: 0, fire: false });
  });
});

describe("doubleTapScale", () => {
  it("приближает и возвращает обратно", () => {
    expect(doubleTapScale(1)).toBeGreaterThan(1);
    expect(doubleTapScale(2.5)).toBe(1);
  });
});
