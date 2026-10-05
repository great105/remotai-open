import { describe, expect, it } from "vitest";
import { firstStepsGoesOnTop, homeOnboardingDone, homeStepIds } from "./homeProgress";

/**
 * Где стоит чеклист «Первые шаги» на главной.
 *
 * Дефект, ради которого правило появилось (замер 04.09.2026): блок стоял
 * последним — его заголовок начинался на 745 px телефона 390×844, то есть под
 * нижней панелью навигации. Для того, кто уже работает, это верное место; для
 * открывшего приложение впервые обучение просто не существовало.
 */
describe("firstStepsGoesOnTop", () => {
  it("новичку — наверх: шагов нет вовсе", () => {
    expect(firstStepsGoesOnTop({}, false)).toBe(true);
  });

  it("наверх и после первого шага — человек только начал", () => {
    expect(firstStepsGoesOnTop({ terminal: true }, false)).toBe(true);
  });

  it("освоился (два шага из трёх) — чеклист уходит вниз и не занимает первый экран", () => {
    expect(firstStepsGoesOnTop({ terminal: true, agent: true }, false)).toBe(false);
  });

  it("«Скрыть» сильнее прогресса: скрытый чеклист наверх не всплывает", () => {
    expect(firstStepsGoesOnTop({}, true)).toBe(false);
    expect(firstStepsGoesOnTop({ terminal: true }, true)).toBe(false);
  });

  // Связь с соседним правилом: пройденный онбординг и «наверх» не пересекаются
  // ни при каком наборе шагов — иначе главная показывала бы новичку блок,
  // который сама же считает завершённым.
  it("пройденный онбординг никогда не поднимается наверх", () => {
    const full = Object.fromEntries(homeStepIds(true).map((s) => [s, true]));
    expect(homeOnboardingDone(full, true)).toBe(true);
    expect(firstStepsGoesOnTop(full, false)).toBe(false);
  });
});
