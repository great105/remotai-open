import { describe, expect, it } from "vitest";
import {
  nativeKeyboardOpen,
  OCCLUDER_SELECTORS, capacityWithOccluder, frameGeometryAction, frameIsStale, fullViewportHeight, inputGrowthPx,
  layoutChange, logicalRowsForKeyboard, reconcileAdoptedGrid, shouldExplainNarrowOutput, stateGridIsFresh,
} from "./geometry";

describe("U-ADOPTED-GRID: сетка из кадра сверяется с первым /state, спрошенным после приёма (ST-08, ревью)", () => {
  const grid = { gridCols: 47, gridRows: 15 };
  it("late state cannot replace a confirmed frame, while a later request and legacy state can", () => {
    expect(stateGridIsFresh(100, 99)).toBe(false);
    expect(stateGridIsFresh(100, 100)).toBe(false);
    expect(stateGridIsFresh(100, 101)).toBe(true);
    expect(stateGridIsFresh(100, NaN)).toBe(false);
    expect(stateGridIsFresh(null, 0)).toBe(true);
  });
  it("PTY вырос, пока вкладка была скрыта: /state возражает по ширине — проверить сетку кадром", () => {
    // Замер скептика: компьютер 47x15 при PTY 123x15, state.cols не менялся.
    expect(reconcileAdoptedGrid({ adoptedAt: 100, stateAskedAt: 250, stateCols: 123, stateRows: 15, ...grid }))
      .toEqual({ consumed: true, rows: false, cols: true });
  });
  it("то же по высоте: 123x15 при PTY 123x29 — проверить сетку кадром", () => {
    expect(reconcileAdoptedGrid({ adoptedAt: 100, stateAskedAt: 250, stateCols: 123, stateRows: 29, gridCols: 123, gridRows: 15 }))
      .toEqual({ consumed: true, rows: true, cols: false });
  });
  it("положительный контроль: /state согласен — пометка снимается, сетку не трогаем", () => {
    expect(reconcileAdoptedGrid({ adoptedAt: 100, stateAskedAt: 250, stateCols: 47, stateRows: 15, ...grid }))
      .toEqual({ consumed: true, rows: false, cols: false });
  });
  it("/state, спрошенный ДО приёма, ничего не доказывает — ни сверки, ни качелей", () => {
    expect(reconcileAdoptedGrid({ adoptedAt: 100, stateAskedAt: 99, stateCols: 123, stateRows: 29, ...grid }))
      .toEqual({ consumed: false, rows: false, cols: false });
    expect(reconcileAdoptedGrid({ adoptedAt: 100, stateAskedAt: 100, stateCols: 123, stateRows: 29, ...grid }))
      .toEqual({ consumed: false, rows: false, cols: false });
  });
  it("сетку кадр не принимал (или она уже сверена) — /state тут не решает", () => {
    expect(reconcileAdoptedGrid({ adoptedAt: null, stateAskedAt: 250, stateCols: 123, stateRows: 29, ...grid }))
      .toEqual({ consumed: false, rows: false, cols: false });
  });
  it("старый агент без cols/rows в /state — сверять нечем, пометка остаётся", () => {
    expect(reconcileAdoptedGrid({ adoptedAt: 100, stateAskedAt: 250, ...grid }))
      .toEqual({ consumed: false, rows: false, cols: false });
    expect(reconcileAdoptedGrid({ adoptedAt: 100, stateAskedAt: 250, stateCols: 0, stateRows: 1, ...grid }))
      .toEqual({ consumed: false, rows: false, cols: false });
  });
  it("известна одна ось — сверка по ней, вторая не трогается", () => {
    expect(reconcileAdoptedGrid({ adoptedAt: 100, stateAskedAt: 250, stateRows: 29, ...grid }))
      .toEqual({ consumed: true, rows: true, cols: false });
  });
  it("часы запроса /state неизвестны (NaN) — не сверяем", () => {
    expect(reconcileAdoptedGrid({ adoptedAt: 100, stateAskedAt: NaN, stateCols: 123, stateRows: 29, ...grid }))
      .toEqual({ consumed: false, rows: false, cols: false });
  });
});

describe("U-OCCLUDERS: что перекрывает коробку, а что меняет вместимость (T-31)", () => {
  it("временные плашки колонки — перекрытие; начальная и постоянная вёрстка — нет", () => {
    expect([...OCCLUDER_SELECTORS].sort()).toEqual([
      ".pty-agent-ask", ".pty-pair-offer", ".pty-update-bar", ".pty-upload-strip", ".pty-width-notice",
    ]);
    // SSH-баннер (QR install.sh под ним резать нельзя) и полоса завершённого
    // процесса решены как вместимость — см. комментарий OCCLUDER_SELECTORS.
    for (const layout of [".pty-agent-entry", ".pty-keys-bar", ".pty-header", ".pty-input-bar", ".pty-ssh-banner", ".pty-dead-bar"]) {
      expect(OCCLUDER_SELECTORS).not.toContain(layout);
    }
  });
  it("рост поля ввода: сверх высоты одной строки, и только когда строк больше одной (H4)", () => {
    expect(inputGrowthPx(5, 131, 51)).toBe(80);
    expect(inputGrowthPx(1, 131, 51)).toBe(0);
    expect(inputGrowthPx(3, 50, 51)).toBe(0);
  });
  it("база одной строки неизвестна — роста не видим (прежний путь вместимости)", () => {
    expect(inputGrowthPx(4, 111, 0)).toBe(0);
    expect(inputGrowthPx(4, NaN, 51)).toBe(0);
    expect(inputGrowthPx(NaN, 111, 51)).toBe(0);
  });
  it("рост поля + сжатие коробки на ту же высоту — перекрытие, а не resize", () => {
    expect(layoutChange({ windowChanged: false, widthChanged: false, boxDeltaPx: -80, occluderDeltaPx: inputGrowthPx(5, 131, 51) }))
      .toBe("occlusion");
  });
});

describe("подсказка узкого вывода на широком экране", () => {
  it("объясняет телефонную сетку на компьютере", () => {
    expect(shouldExplainNarrowOutput(48, 120)).toBe(true);
    expect(shouldExplainNarrowOutput(64, 80)).toBe(true);
  });
  it("не мешает телефону, нормальному размеру и небольшому округлению", () => {
    for (const [cols, available] of [[32, 48], [120, 120], [120, 100], [70, 80], [184, 200]]) {
      expect(shouldExplainNarrowOutput(cols, available)).toBe(false);
    }
  });
  it("ждёт настоящих размеров вместо начальных и некорректных значений", () => {
    for (const [cols, available] of [[0, 120], [48, 0], [NaN, 120], [48, Infinity]]) {
      expect(shouldExplainNarrowOutput(cols, available)).toBe(false);
    }
  });
});

// Боевые числа 13.08.2026: из 200 снимков экрана 24 пришли ВЫШЕ окна клиента,
// 12 из них сильно — кадр 30–31 строка в окно 9–11 строк. Это поднятая
// клавиатура: человек печатает запрос агенту, и ровно в этот момент строки
// 11–30 кадра складываются в последнюю видимую.
describe("кадр экрана в чужой геометрии", () => {
  it("совпало — пишем как есть", () => {
    expect(frameGeometryAction({
      snapCols: 48, snapRows: 30, termCols: 48, termRows: 30, keyboardOpen: false,
    })).toEqual({ kind: "apply" });
  });

  it("клавиатура: возвращаем СВОЮ логическую высоту, компьютер не трогаем", () => {
    expect(frameGeometryAction({
      snapCols: 48, snapRows: 31, termCols: 48, termRows: 11, keyboardOpen: true,
    })).toEqual({ kind: "restore", cols: 48, rows: 31 });
    expect(frameGeometryAction({
      snapCols: 48, snapRows: 30, termCols: 48, termRows: 9, keyboardOpen: true,
    })).toEqual({ kind: "restore", cols: 48, rows: 30 });
  });

  it("кадр выше, а клавиатуры нет — наш размер честный, просим свежий кадр", () => {
    expect(frameGeometryAction({
      snapCols: 48, snapRows: 30, termCols: 48, termRows: 28, keyboardOpen: false,
    })).toEqual({ kind: "resync", apply: true });
  });

  // ⚠ Подгонять ширину сдвигом и обрезкой нельзя: перенос длинных строк и вся
  // сетка кадра посчитаны под другую ширину.
  it("другая ширина — только честный resize и новый кадр", () => {
    expect(frameGeometryAction({
      snapCols: 80, snapRows: 24, termCols: 48, termRows: 24, keyboardOpen: true,
    })).toEqual({ kind: "resync", apply: false });
  });

  it("кадр ниже терминала — компьютер не знает нашего размера", () => {
    expect(frameGeometryAction({
      snapCols: 48, snapRows: 11, termCols: 48, termRows: 28, keyboardOpen: false,
    })).toEqual({ kind: "resync", apply: true });
  });

  // Боевой лог 06.09.2026: коробка выросла на 4 строки (ушёл блок «Запустить
  // ИИ-агента»), уточнённый размер не успел уйти, поднялась клавиатура — и
  // 12 секунд подряд `снапшот=48x24 логический=48x28 клавиатура=true
  // решение=resync`: размер под клавиатурой не уходит, кадр приходит в той же
  // высоте, круг не сходится. Правда под клавиатурой — высота кадра.
  it("кадр ниже, а клавиатура поднята — ужимаемся до кадра, свежий не просим", () => {
    expect(frameGeometryAction({
      snapCols: 48, snapRows: 24, termCols: 48, termRows: 28, keyboardOpen: true,
    })).toEqual({ kind: "restore", cols: 48, rows: 24 });
  });

  it("старый агент без полей геометрии — прежнее поведение", () => {
    expect(frameGeometryAction({
      termCols: 48, termRows: 30, keyboardOpen: true,
    })).toEqual({ kind: "apply" });
    expect(frameGeometryAction({
      snapCols: 48, snapRows: 0, termCols: 48, termRows: 30, keyboardOpen: false,
    })).toEqual({ kind: "apply" });
  });

  // ⚠ ВНЕШНИЙ АУДИТ 2.57.18, находка P0-05: два зрителя разной ширины (телефон
  // 48×30 + компьютер 232×40). Сервер держит PTY по самому УЗКОМУ, и кадр
  // приходит в 48 колонках ВСЕГДА — «отверг → попроси свежий» у широкого
  // зрителя не сходился никогда, лишь троттлился до раза в пять секунд.
  // Кадр в авторитетной ширине PTY (`state.cols`) принимаем: зажимаем СВОЮ
  // логическую сетку локально, сервер не дёргаем.
  describe("два зрителя разной ширины: авторитетная сетка PTY", () => {
    it("кадр в авторитетной ширине — принять сетку локально и писать кадр", () => {
      expect(frameGeometryAction({
        snapCols: 48, snapRows: 30, termCols: 232, termRows: 40,
        keyboardOpen: false, authoritativeCols: 48,
      })).toEqual({ kind: "adopt", cols: 48, rows: 30 });
    });

    it("без знания авторитетной ширины — прежний resync без записи кадра", () => {
      expect(frameGeometryAction({
        snapCols: 48, snapRows: 30, termCols: 232, termRows: 40,
        keyboardOpen: false,
      })).toEqual({ kind: "resync", apply: false });
      expect(frameGeometryAction({
        snapCols: 48, snapRows: 30, termCols: 232, termRows: 40,
        keyboardOpen: false, authoritativeCols: 0,
      })).toEqual({ kind: "resync", apply: false });
    });

    it("кадр НЕ в авторитетной ширине (переходный, PTY вырос) — resync", () => {
      // Узкий зритель ушёл, PTY вырос до 232, а наш зажатый терминал ещё 48.
      // Кадр 232 при зажатии 48: authoritative ещё старый (48) — кадр ему НЕ
      // равен, значит это не «узкий зритель рядом», а смена геометрии.
      expect(frameGeometryAction({
        snapCols: 232, snapRows: 40, termCols: 48, termRows: 30,
        keyboardOpen: false, authoritativeCols: 48,
      })).toEqual({ kind: "resync", apply: false });
    });
  });
});

// U-GEO-DELIVERED (ST-08, T-32). Кадр на запрос, отправленный после доставки
// нашей вместимости в этот же сокет, снят в сетке PTY: сервер читает resize и
// screen одной горутиной по порядку, кадр ждёт смены геометрии. Такой кадр
// принимаем по ОБЕИМ осям — иначе высокий зритель рядом с низким крутит
// «кадр → resync → запрос» раз в секунду (в скрытой вкладке вечно).
describe("U-GEO-DELIVERED: кадр после доставленной вместимости — сетка PTY", () => {
  it("расхождение по высоте — принять сетку кадра, а не просить снова", () => {
    expect(frameGeometryAction({
      snapCols: 48, snapRows: 20, termCols: 48, termRows: 40, keyboardOpen: false, capacityDelivered: true,
    })).toEqual({ kind: "adopt", cols: 48, rows: 20 });
  });

  it("расхождение по ширине без знания /state — тоже принять", () => {
    expect(frameGeometryAction({
      snapCols: 48, snapRows: 30, termCols: 232, termRows: 30, keyboardOpen: false, capacityDelivered: true,
    })).toEqual({ kind: "adopt", cols: 48, rows: 30 });
  });

  it("расхождение по обеим осям, /state устарел — принять кадр, а не /state", () => {
    expect(frameGeometryAction({
      snapCols: 90, snapRows: 10, termCols: 48, termRows: 20, keyboardOpen: false,
      authoritativeCols: 48, authoritativeRows: 20, capacityDelivered: true,
    })).toEqual({ kind: "adopt", cols: 90, rows: 10 });
  });

  it("совпало — как раньше, пишем", () => {
    expect(frameGeometryAction({
      snapCols: 48, snapRows: 20, termCols: 48, termRows: 20, keyboardOpen: false, capacityDelivered: true,
    })).toEqual({ kind: "apply" });
  });

  it("вместимость не доставлена — прежний resync с записью и без", () => {
    expect(frameGeometryAction({
      snapCols: 48, snapRows: 20, termCols: 48, termRows: 40, keyboardOpen: false, capacityDelivered: false,
    })).toEqual({ kind: "resync", apply: true });
    expect(frameGeometryAction({
      snapCols: 48, snapRows: 30, termCols: 232, termRows: 30, keyboardOpen: false, capacityDelivered: false,
    })).toEqual({ kind: "resync", apply: false });
  });

  it("клавиатура — restore по высоте в любом случае, размер под ней не уходит", () => {
    expect(frameGeometryAction({
      snapCols: 48, snapRows: 30, termCols: 48, termRows: 11, keyboardOpen: true, capacityDelivered: true,
    })).toEqual({ kind: "restore", cols: 48, rows: 30 });
    expect(frameGeometryAction({
      snapCols: 48, snapRows: 24, termCols: 48, termRows: 28, keyboardOpen: true, capacityDelivered: true,
    })).toEqual({ kind: "restore", cols: 48, rows: 24 });
    // Ширина под клавиатурой — прежнее правило (resync без записи).
    expect(frameGeometryAction({
      snapCols: 80, snapRows: 24, termCols: 48, termRows: 24, keyboardOpen: true, capacityDelivered: true,
    })).toEqual({ kind: "resync", apply: false });
  });

  it("старый агент без полей геометрии — прежнее apply", () => {
    expect(frameGeometryAction({
      termCols: 48, termRows: 30, keyboardOpen: false, capacityDelivered: true,
    })).toEqual({ kind: "apply" });
  });

  it("правило adopt по авторитетной сетке /state сохраняется без доставки", () => {
    expect(frameGeometryAction({
      snapCols: 80, snapRows: 24, termCols: 80, termRows: 40, keyboardOpen: false,
      authoritativeCols: 80, authoritativeRows: 24,
    })).toEqual({ kind: "adopt", cols: 80, rows: 24 });
  });

  it("capacityDelivered=false неотличим от прежнего вызова без поля", () => {
    const sizes = [0, 2, 10, 20, 24, 40, 48, 80, 232];
    let checked = 0;
    for (const snapCols of sizes) for (const snapRows of sizes) for (const termCols of [20, 48, 232])
      for (const termRows of [10, 20, 40]) for (const keyboardOpen of [false, true])
        for (const authoritativeCols of [undefined, 0, 48]) for (const authoritativeRows of [undefined, 20]) {
          const s = { snapCols, snapRows, termCols, termRows, keyboardOpen, authoritativeCols, authoritativeRows };
          expect(frameGeometryAction({ ...s, capacityDelivered: false })).toEqual(frameGeometryAction(s));
          checked++;
        }
    expect(checked).toBe(9 * 9 * 3 * 3 * 2 * 3 * 2);
  });
});

// U-OCCLUSION (ST-08, T-31). Плашка вопроса агента или загрузки сжимает
// коробку терминала; это локальное перекрытие, как клавиатура, а не новая
// вместимость — иначе каждое её появление и скрытие стоило TUI полной
// перерисовки через resize PTY.
describe("U-OCCLUSION: плашка — перекрытие, а не вместимость", () => {
  it("коробка сжалась ровно на высоту появившейся плашки — перекрытие", () => {
    expect(layoutChange({ windowChanged: false, widthChanged: false, boxDeltaPx: -40, occluderDeltaPx: 40 })).toBe("occlusion");
    expect(layoutChange({ windowChanged: false, widthChanged: false, boxDeltaPx: 40, occluderDeltaPx: -40 })).toBe("occlusion");
  });

  it("дробные пиксели вёрстки не мешают узнать плашку", () => {
    expect(layoutChange({ windowChanged: false, widthChanged: false, boxDeltaPx: -39.5, occluderDeltaPx: 40.25 })).toBe("occlusion");
  });

  it("окно или ширина изменились — вместимость, даже если плашка тоже", () => {
    expect(layoutChange({ windowChanged: true, widthChanged: false, boxDeltaPx: -40, occluderDeltaPx: 40 })).toBe("capacity");
    expect(layoutChange({ windowChanged: false, widthChanged: true, boxDeltaPx: -40, occluderDeltaPx: 40 })).toBe("capacity");
  });

  it("плашка не менялась — объяснять нечем, это вместимость", () => {
    expect(layoutChange({ windowChanged: false, widthChanged: false, boxDeltaPx: -40, occluderDeltaPx: 0 })).toBe("capacity");
    expect(layoutChange({ windowChanged: false, widthChanged: false, boxDeltaPx: 0, occluderDeltaPx: 0 })).toBe("capacity");
  });

  it("плашка объясняет изменение лишь частично — вместимость", () => {
    expect(layoutChange({ windowChanged: false, widthChanged: false, boxDeltaPx: -60, occluderDeltaPx: 40 })).toBe("capacity");
    expect(layoutChange({ windowChanged: false, widthChanged: false, boxDeltaPx: NaN, occluderDeltaPx: 40 })).toBe("capacity");
  });

  it("вместимость с плашкой = измеренные строки + целые строки плашки", () => {
    expect(capacityWithOccluder(15, 40, 20)).toBe(17);
    // Округление ВНИЗ: 1,5 строки плашки — это одна строка. К ближайшему
    // было бы 17 при настоящих floor(335/20) = 16, и нижняя строка ушла бы
    // под край (срез низа, жалоба 04.08).
    expect(capacityWithOccluder(15, 30, 20)).toBe(16);
  });

  it("высота коробки известна — считаем точно как fit", () => {
    expect(capacityWithOccluder(15, 30, 20, 305)).toBe(16);
    expect(capacityWithOccluder(15, 10, 20, 315)).toBe(16);
    expect(capacityWithOccluder(15, 10, 20, 300)).toBe(15);
  });

  it("нет плашки или неизвестная ячейка — измеренное как есть", () => {
    expect(capacityWithOccluder(15, 0, 20)).toBe(15);
    expect(capacityWithOccluder(15, 40, 0)).toBe(15);
    expect(capacityWithOccluder(15, NaN, 20)).toBe(15);
  });
});

// Кадр снимается на позиции потока: всё до неё он уже содержит. Пришёл кадр,
// снятый РАНЬШЕ показанного, — применить его значит откатить картинку, а
// откаченные байты второй раз не придут (повторный аудит, T259-07).
describe("свежесть кадра по позиции потока", () => {
  it("кадр старее показанного — не применяем", () => {
    expect(frameIsStale(2000, 2200)).toBe(true);
  });

  it("кадр ровно на нашей позиции или свежее — применяем", () => {
    expect(frameIsStale(2000, 2000)).toBe(false);
    expect(frameIsStale(2400, 2000)).toBe(false);
  });

  it("старый агент без позиции — решать нечем, поведение прежнее", () => {
    expect(frameIsStale(undefined, 2000)).toBe(false);
    expect(frameIsStale(0, 2000)).toBe(false);
  });
});

describe("логическая высота при клавиатуре", () => {
  it("берём известную логическую, а не измеренную клавиатурную", () => {
    expect(logicalRowsForKeyboard(10, 30)).toBe(30);
  });

  it("ничего не знаем — остаёмся на измеренной", () => {
    expect(logicalRowsForKeyboard(10, 0)).toBe(10);
    expect(logicalRowsForKeyboard(10, 1)).toBe(10);
  });

  it("измеренная больше известной (клавиатура ушла) — не занижаем", () => {
    expect(logicalRowsForKeyboard(31, 30)).toBe(31);
  });
});

const KEYBOARD_MIN_PX = 150; // тот же порог, что в PtyTermView
const keyboardSeen = (vpW: number, vpH: number, scrW: number, scrH: number, coarse = true) =>
  vpH < fullViewportHeight(vpW, vpH, scrW, scrH, coarse) - KEYBOARD_MIN_PX;

describe("полная высота экрана в ориентации окна", () => {
  it("НЕ считает клавиатуру поднятой на планшете в ландшафте", () => {
    // Живой случай владельца 10.09.2026: iPad лежит на боку, окно 1180x820,
    // а Safari отдаёт screen портретным (820x1180). Прямой screen.height
    // делал базу 1180 и запирал клиента в режиме клавиатуры навсегда.
    expect(fullViewportHeight(1180, 820, 820, 1180, true)).toBe(820);
    expect(keyboardSeen(1180, 820, 820, 1180)).toBe(false);
  });
  it("видит настоящую клавиатуру на планшете в ландшафте", () => {
    expect(keyboardSeen(1180, 480, 820, 1180)).toBe(true);
  });
  it("телефон: портрет без клавиатуры и с ней", () => {
    expect(keyboardSeen(390, 844, 390, 844)).toBe(false);
    expect(keyboardSeen(390, 500, 390, 844)).toBe(true);
  });
  it("телефон: ландшафт без клавиатуры и с ней", () => {
    expect(keyboardSeen(844, 390, 390, 844)).toBe(false);
    expect(keyboardSeen(844, 200, 390, 844)).toBe(true);
  });
  it("узкое окно с клавиатурой не принимается за ландшафт", () => {
    // Портрет 390 шириной: клавиатура сделала окно 390x380, и оно стало
    // «шире, чем выше». Ширина при клавиатуре не меняется — по ней и судим.
    expect(fullViewportHeight(390, 380, 390, 844, true)).toBe(844);
    expect(keyboardSeen(390, 380, 390, 844)).toBe(true);
  });
  it("стенд подменяет только высоту экрана — ответ всё равно верный", () => {
    // probe-keyboard-geometry фиксирует screen.height=844, ширину оставляет
    // Playwright. Раньше именно на этом падали три пробы клавиатуры.
    expect(keyboardSeen(390, 844, 390, 844)).toBe(false);
    expect(keyboardSeen(390, 380, 390, 844)).toBe(true);
  });
  it("устройству, которое само поворачивает стороны экрана, ответ тот же", () => {
    expect(fullViewportHeight(1180, 820, 1180, 820, true)).toBe(820);
    expect(fullViewportHeight(820, 1180, 1180, 820, true)).toBe(1180);
  });
  it("на десктопе базой остаётся окно, а не монитор", () => {
    expect(fullViewportHeight(1280, 700, 2560, 1440, false)).toBe(700);
    expect(keyboardSeen(1280, 700, 2560, 1440, false)).toBe(false);
  });
  it("без размеров экрана остаётся вьюпорт", () => {
    expect(fullViewportHeight(390, 844, 0, 0, true)).toBe(844);
  });
});

// Журнал 23–25.09.2026: возврат в приложение с поднятой клавиатурой, последний
// сигнал Android — «скрыта»; вьюпорт 561 при базе 933 слал компьютеру 48×10.
describe("nativeKeyboardOpen", () => {
  it("trusts a native 'shown' signal", () => {
    expect(nativeKeyboardOpen(true, 933, 420, 933, 420, 150)).toBe(true);
  });
  it("overrides a stale native 'hidden' when the viewport is keyboard-short", () => {
    expect(nativeKeyboardOpen(false, 561, 420, 933, 420, 150)).toBe(true);
  });
  it("keeps 'hidden' for a browser-bar sized change", () => {
    expect(nativeKeyboardOpen(false, 875, 420, 933, 420, 150)).toBe(false);
  });
  it("keeps 'hidden' when the width changed (rotation, other window)", () => {
    expect(nativeKeyboardOpen(false, 390, 844, 933, 420, 150)).toBe(false);
  });
  it("keeps 'hidden' without a known base", () => {
    expect(nativeKeyboardOpen(false, 561, 420, 0, 420, 150)).toBe(false);
  });
});
