import { describe, expect, it } from "vitest";
import { Terminal as HeadlessTerminal } from "@xterm/headless";
import { panAxis, panBy, panFollows, touchAxis, viewportPan, wheelPanDelta } from "./viewportPan";
import {
  keyboardPeek, lastInkRow, OCCLUSION_ENTER_ROOM_ROWS, OCCLUSION_HOLD_MS, OCCLUSION_HUMAN_INPUT_MS, OCCLUSION_IDLE,
  occlusionPeek, occlusionStep,
  type OcclusionState,
} from "./KeyboardAdapter";

// T-17 / T-VP1 (ST-10, ST-08). Сетка шире и выше коробки: курсор или
// совпадение поиска обязаны быть видны по ОБЕИМ осям, PTY при этом не
// меняется (сдвиг — чисто локальный transform).
const CELL_W = 8, CELL_H = 16;
const grid = (cols: number, rows: number, visCols: number, visRows: number) => ({
  drawnW: cols * CELL_W, drawnH: rows * CELL_H, visibleW: visCols * CELL_W, visibleH: visRows * CELL_H,
  cellW: CELL_W, cellH: CELL_H,
});
const cursorVisible = (g: ReturnType<typeof grid>, pan: { x: number; y: number }, col: number, row: number) =>
  col * g.cellW >= pan.x && (col + 1) * g.cellW <= pan.x + g.visibleW
  && row * g.cellH >= pan.y && (row + 1) * g.cellH <= pan.y + g.visibleH;

describe("viewportPan: минимальный сдвиг по двум осям", () => {
  it("курсор в колонке 200 при ширине экрана 48 колонок виден", () => {
    const g = grid(240, 40, 48, 30);
    const pan = viewportPan({ ...g, cursorCol: 200, cursorRow: 35, prev: { x: 0, y: 0 }, follow: true });
    expect(cursorVisible(g, pan, 200, 35)).toBe(true);
    // Минимальный: курсор у правого края окна, а не посередине.
    expect(pan).toEqual({ x: 201 * CELL_W - 48 * CELL_W, y: 36 * CELL_H - 30 * CELL_H });
  });

  it("всё помещается — сдвига нет, какой бы ни был прежний", () => {
    const g = grid(48, 30, 48, 30);
    expect(viewportPan({ ...g, cursorCol: 47, cursorRow: 29, prev: { x: 120, y: 64 }, follow: true })).toEqual({ x: 0, y: 0 });
    expect(viewportPan({ ...grid(40, 20, 48, 30), cursorCol: 39, cursorRow: 19, prev: { x: 5, y: 5 }, follow: false }))
      .toEqual({ x: 0, y: 0 });
  });

  it("зажим на краях: последняя колонка — ровно максимальный сдвиг, не дальше", () => {
    const g = grid(240, 40, 48, 30);
    const maxX = g.drawnW - g.visibleW, maxY = g.drawnH - g.visibleH;
    expect(viewportPan({ ...g, cursorCol: 239, cursorRow: 39, prev: { x: 0, y: 0 }, follow: true })).toEqual({ x: maxX, y: maxY });
    expect(viewportPan({ ...g, cursorCol: 0, cursorRow: 0, prev: { x: 99999, y: 99999 }, follow: false })).toEqual({ x: maxX, y: maxY });
    expect(viewportPan({ ...g, cursorCol: 10, cursorRow: 1, prev: { x: -50, y: -50 }, follow: false })).toEqual({ x: 0, y: 0 });
  });

  it("курсор уже виден — окно не двигается (без прыжков при каждом символе)", () => {
    const g = grid(240, 40, 48, 30);
    const prev = { x: 800, y: 64 };
    expect(viewportPan({ ...g, cursorCol: 110, cursorRow: 10, prev, follow: true })).toEqual(prev);
  });

  it("курсор левее окна — окно встаёт началом на курсор", () => {
    const g = grid(240, 40, 48, 30);
    expect(viewportPan({ ...g, cursorCol: 5, cursorRow: 2, prev: { x: 800, y: 160 }, follow: true }))
      .toEqual({ x: 5 * CELL_W, y: 2 * CELL_H });
  });

  it("follow=false — курсор не тянет окно, только зажим", () => {
    const g = grid(240, 40, 48, 30);
    expect(viewportPan({ ...g, cursorCol: 200, cursorRow: 39, prev: { x: 16, y: 0 }, follow: false })).toEqual({ x: 16, y: 0 });
  });

  it("курсор вне сетки или мусорные размеры — не следуем и не падаем", () => {
    const g = grid(240, 40, 48, 30);
    expect(viewportPan({ ...g, cursorCol: -1, cursorRow: 400, prev: { x: 16, y: 32 }, follow: true })).toEqual({ x: 16, y: 32 });
    expect(viewportPan({ ...g, cellW: 0, cellH: NaN, cursorCol: 200, cursorRow: 10, prev: { x: 16, y: NaN }, follow: true }))
      .toEqual({ x: 16, y: 0 });
    expect(viewportPan({ ...g, visibleW: 0, visibleH: -5, cursorCol: 200, cursorRow: 10, prev: { x: 16, y: 32 }, follow: true }))
      .toEqual({ x: 0, y: 0 });
  });

  it("по вертикали — ровно прежний keyboardPeek (частный случай сохранён)", () => {
    // Эталон — дословно прежняя реализация keyboardPeek (до ST-10). Сравнивать
    // с нынешней бессмысленно: она сама выражена через panAxis.
    const legacyPeek = (drawn: number, visible: number, rows: number, cursorRow: number, previous = 0) => {
      if (drawn <= visible || visible <= 0 || rows <= 0 || cursorRow < 0 || cursorRow >= rows) return 0;
      const height = drawn / rows;
      const top = cursorRow * height, bottom = top + height;
      const maximum = Math.max(0, drawn - visible);
      let peek = Math.max(0, Math.min(maximum, previous));
      if (top < peek) peek = top;
      if (bottom > peek + visible) peek = bottom - visible;
      return Math.round(Math.max(0, Math.min(maximum, peek)));
    };
    let checked = 0;
    for (const rows of [10, 24, 30, 31]) for (let cursor = -1; cursor <= rows; cursor++)
      for (const previous of [0, 16, 100, 320, 999, -5]) for (const drawn of [rows * 16, rows * 17.3, 150]) {
        const visible = 160;
        const expected = legacyPeek(drawn, visible, rows, cursor, previous);
        expect(keyboardPeek(drawn, visible, rows, cursor, previous)).toBe(expected);
        if (cursor >= 0 && cursor < rows && drawn > visible) {
          expect(panAxis(drawn, visible, drawn / rows, cursor, previous, true)).toBe(expected);
        }
        checked++;
      }
    expect(checked).toBe((12 + 26 + 32 + 33) * 6 * 3);
  });
});

describe("occlusionPeek: перекрытие не срезает строки под курсором (ST-08, подвал Codex)", () => {
  // Числа замера скептика волны 4: 390×844, сетка 24 строки по 19 px, коробка
  // после роста поля ввода 366 px (≈19,3 строки).
  const ROW = 19, ROWS = 24, DRAWN = ROWS * ROW, BOX = 366;
  const rowInside = (row: number, peek: number) => row * ROW >= peek - 0.5 && (row + 1) * ROW <= peek + BOX + 0.5;

  it("подвал под строкой ввода: курсор 18, подвал до 22 — сдвиг ровно до подвала, верх срезан", () => {
    // Правило одного курсора сдвига не даёт: 19 × 19 = 361 ≤ 366, и все
    // четыре строки подвала оказались бы ниже коробки.
    expect(keyboardPeek(DRAWN, BOX, ROWS, 18, 0)).toBe(0);
    const peek = occlusionPeek(DRAWN, BOX, ROWS, 18, 22, 0);
    expect(peek).toBe(23 * ROW - BOX);
    expect(rowInside(18, peek)).toBe(true);
    expect(rowInside(22, peek)).toBe(true);
    expect(rowInside(0, peek)).toBe(false);
  });

  it("совпадает с правилом курсора, когда под курсором текста нет", () => {
    for (const [cursor, last] of [[23, 23], [23, 22], [2, 2], [2, 1], [0, -1]]) {
      expect(occlusionPeek(DRAWN, BOX, ROWS, cursor, last, 0)).toBe(keyboardPeek(DRAWN, BOX, ROWS, cursor, 0));
    }
  });

  it("обе строки не помещаются (курсор вверху, строка состояния внизу) — держим курсор", () => {
    expect(occlusionPeek(DRAWN, BOX, ROWS, 0, 23, 0)).toBe(0);
    expect(occlusionPeek(DRAWN, BOX, ROWS, 0, 23, 90)).toBe(keyboardPeek(DRAWN, BOX, ROWS, 0, 90));
  });

  it("минимальный: прежний сдвиг, при котором обе видны, не меняется", () => {
    expect(occlusionPeek(DRAWN, BOX, ROWS, 18, 22, 80)).toBe(80);
    // Прежний сдвиг больше нужного: окно поднимается только до строки курсора.
    expect(occlusionPeek(DRAWN, BOX, ROWS, 5, 22, 90)).toBe(90);
    expect(occlusionPeek(DRAWN, BOX, ROWS, 2, 3, 400)).toBe(2 * ROW);
  });

  it("мусор и отсутствие перекрытия — как keyboardPeek", () => {
    expect(occlusionPeek(BOX, BOX, ROWS, 18, 22, 50)).toBe(0);
    expect(occlusionPeek(DRAWN, 0, ROWS, 18, 22, 50)).toBe(0);
    expect(occlusionPeek(DRAWN, BOX, ROWS, -1, 22, 50)).toBe(0);
    expect(occlusionPeek(DRAWN, BOX, ROWS, 18, 99, 0)).toBe(keyboardPeek(DRAWN, BOX, ROWS, 18, 0));
    expect(occlusionPeek(DRAWN, BOX, ROWS, 18, NaN, 0)).toBe(keyboardPeek(DRAWN, BOX, ROWS, 18, 0));
    expect(occlusionPeek(DRAWN, BOX, ROWS, 18, 22, NaN)).toBe(23 * ROW - BOX);
  });

  it("свойство: курсор виден всегда, а когда обе строки помещаются — видны обе", () => {
    let both = 0;
    for (const box of [120, 200, 300, 366, 440]) for (let cursor = 0; cursor < ROWS; cursor++)
      for (let last = -1; last < ROWS; last++) for (const previous of [0, 37, 90, 200, 999]) {
        const peek = occlusionPeek(DRAWN, box, ROWS, cursor, last, previous);
        const inside = (row: number) => row * ROW >= peek - 0.5 && (row + 1) * ROW <= peek + box + 0.5;
        expect(peek).toBeGreaterThanOrEqual(0);
        expect(peek).toBeLessThanOrEqual(DRAWN - box);
        expect(inside(cursor)).toBe(true);
        if (last >= 0 && (Math.abs(last - cursor) + 1) * ROW <= box) { expect(inside(last)).toBe(true); both++; }
      }
    expect(both).toBeGreaterThan(1000);
  });
});

describe("occlusionStep: окно не ходит за промежуточным курсором (ST-06 × ST-08, скептик 15.09)", () => {
  // Та же геометрия замера: 390×844, сетка 24 × 19 px, коробка 366 px.
  const ROW = 19, ROWS = 24, DRAWN = ROWS * ROW, BOX = 366;
  const inside = (row: number, peek: number, box = BOX) =>
    row * ROW >= peek - 0.5 && (row + 1) * ROW <= peek + box + 0.5;
  type Run = { peek: number; state: OcclusionState; changes: number; now: number };
  const input = (cursorRow: number, lastTextRow: number, previous: number, now: number, fromOutput: boolean, visible = BOX) =>
    ({ drawn: DRAWN, visible, rows: ROWS, cursorRow, lastTextRow, previous, now, fromOutput });
  /** Рост поля ввода (геометрия, не вывод) — первое решение без памяти. */
  const start = (cursorRow: number, lastTextRow: number): Run => {
    const s = occlusionStep(OCCLUSION_IDLE, input(cursorRow, lastTextRow, 0, 0, false));
    return { peek: s.peek, state: s.state, changes: 0, now: 0 };
  };
  /** Пересчёт от вывода через dt мс. */
  const output = (run: Run, cursorRow: number, lastTextRow: number, dt: number) => {
    run.now += dt;
    const s = occlusionStep(run.state, input(cursorRow, lastTextRow, run.peek, run.now, true));
    if (s.peek !== run.peek) run.changes++;
    run.peek = s.peek;
    run.state = s.state;
    return s;
  };

  it("перерисовка двумя записями, курсор между ними в строке 1: 0 смен на 60 перерисовок (без памяти — 120)", () => {
    const run = start(18, 22);
    expect(run.peek).toBe(23 * ROW - BOX);
    let stateless = run.peek;
    let statelessChanges = 0;
    for (let n = 0; n < 60; n++) {
      const mid = output(run, 1, 22, 16);
      expect(mid.recheckMs).not.toBeNull();
      output(run, 18, 22, 30);
      expect(run.state.holdSince).toBeNull();
      for (const cursor of [1, 18]) {
        const next = occlusionPeek(DRAWN, BOX, ROWS, cursor, 22, stateless);
        if (next !== stateless) statelessChanges++;
        stateless = next;
      }
    }
    expect(run.changes).toBe(0);
    expect(run.peek).toBe(23 * ROW - BOX);
    // Правило без памяти (сборка до исправления) — туда-обратно на каждой.
    expect(statelessChanges).toBe(120);
  });

  it("курсор ушёл надолго — после выдержки окно идёт к нему; вернулся — подвал снова виден", () => {
    const run = start(18, 22);
    const first = output(run, 1, 22, 0);
    expect(first.peek).toBe(71);
    expect(first.recheckMs).toBe(OCCLUSION_HOLD_MS);
    const still = output(run, 1, 22, OCCLUSION_HOLD_MS - 1);
    expect(still.peek).toBe(71);
    expect(still.recheckMs).toBe(1);
    const gone = output(run, 1, 22, 1);
    expect(gone.peek).toBe(keyboardPeek(DRAWN, BOX, ROWS, 1, 71));
    expect(gone.recheckMs).toBeNull();
    expect(gone.state.both).toBe(false);
    const back = output(run, 18, 22, 50);
    expect(back.peek).toBe(71);
    expect(back.state.both).toBe(true);
  });

  it("вниз — без выдержки; не от вывода (геометрия, клавиатура, жест) — сразу", () => {
    const run = start(2, 3);
    expect(run.peek).toBe(0);
    const down = output(run, 23, 23, 5);
    expect(down.peek).toBe(DRAWN - BOX);
    expect(down.recheckMs).toBeNull();
    const both = start(18, 22);
    const now = occlusionStep(both.state, input(1, 22, both.peek, 5, false));
    expect(now.peek).toBe(keyboardPeek(DRAWN, BOX, ROWS, 1, both.peek));
    expect(now.recheckMs).toBeNull();
  });

  it("vim j/k у границы (строка состояния 23, курсор 3↔6): смен не больше, чем у прежнего правила + 1", () => {
    const cycle = [4, 5, 6, 5, 4, 3];
    for (const first of [3, 6]) {
      const run = start(first, 23);
      let legacy = keyboardPeek(DRAWN, BOX, ROWS, first, 0);
      let legacyChanges = 0;
      let stateless = occlusionPeek(DRAWN, BOX, ROWS, first, 23, 0);
      let statelessChanges = 0;
      for (let c = 0; c < 2; c++) for (const row of cycle) {
        output(run, row, 23, 150);
        expect(inside(row, run.peek)).toBe(true);
        const l = keyboardPeek(DRAWN, BOX, ROWS, row, legacy);
        if (l !== legacy) legacyChanges++;
        legacy = l;
        const s = occlusionPeek(DRAWN, BOX, ROWS, row, 23, stateless);
        if (s !== stateless) statelessChanges++;
        stateless = s;
      }
      expect(run.changes).toBeLessThanOrEqual(legacyChanges + 1);
      // Замер скептика: правило без памяти — 6 смен за два цикла.
      if (first === 3) expect(statelessChanges).toBe(6);
    }
  });

  it("гистерезис: вход — с запасом в OCCLUSION_ENTER_ROOM_ROWS строк, остаётся — пока помещаются", () => {
    expect(OCCLUSION_ENTER_ROOM_ROWS).toBe(2);
    // Курсор 5 и строка 23 помещаются с запасом 5 px: без памяти не входит…
    const fresh = occlusionStep(OCCLUSION_IDLE, input(5, 23, 0, 0, true));
    expect(fresh.peek).toBe(0);
    expect(fresh.state.both).toBe(false);
    // …а из режима «оба» не выходит.
    const kept = occlusionStep({ both: true, holdSince: null }, input(5, 23, 90, 0, true));
    expect(kept.peek).toBe(90);
    expect(kept.state.both).toBe(true);
    // Запас 43 px (≥ 2 строк) — входит; 24 px — нет.
    expect(occlusionStep(OCCLUSION_IDLE, input(7, 23, 0, 0, true)).peek).toBe(DRAWN - BOX);
    expect(occlusionStep(OCCLUSION_IDLE, input(6, 23, 0, 0, true)).peek).toBe(0);
  });

  it("курсор вне сетки, нет перекрытия, мусор — как keyboardPeek, память сброшена", () => {
    const busy: OcclusionState = { both: true, holdSince: 5 };
    for (const [drawn, box, cursor] of [[DRAWN, BOX, -1], [DRAWN, BOX, 24], [BOX, BOX, 18], [DRAWN, 0, 18]]) {
      const s = occlusionStep(busy, { drawn, visible: box, rows: ROWS, cursorRow: cursor, lastTextRow: 22, previous: 50, now: 10, fromOutput: true });
      expect(s.peek).toBe(keyboardPeek(drawn, box, ROWS, cursor, 50));
      expect(s.state).toEqual(OCCLUSION_IDLE);
      expect(s.recheckMs).toBeNull();
    }
    // Пустой экран — как одна строка курсора.
    expect(occlusionStep(OCCLUSION_IDLE, input(23, -1, 0, 0, false)).peek).toBe(DRAWN - BOX);
    expect(occlusionStep(OCCLUSION_IDLE, input(18, NaN, NaN, NaN, true)).peek).toBe(keyboardPeek(DRAWN, BOX, ROWS, 18, 0));
    const nanHold = occlusionStep({ both: true, holdSince: NaN }, input(1, 22, 71, 10, true));
    expect(Number.isFinite(nanHold.peek)).toBe(true);
  });

  it("ввод человека (k/↑ через верх окна в режиме «оба»): выдержки нет, курсор в коробке сразу", () => {
    // vim: строка состояния в 23. Курсор 7 — вход в «оба» (запас ≥ 2 строк),
    // окно у низа (90 px). Курсор 5 — обе строки ещё помещаются. Курсор 4 —
    // уже нет: правило курсора тянет окно вверх, к строке 4.
    expect(OCCLUSION_HUMAN_INPUT_MS).toBe(300);
    const entered = occlusionStep(OCCLUSION_IDLE, input(7, 23, 0, 0, true));
    expect(entered.state.both).toBe(true);
    expect(entered.peek).toBe(DRAWN - BOX);
    const atEdge = occlusionStep(entered.state, input(5, 23, entered.peek, 150, true));
    expect(atEdge.state.both).toBe(true);
    expect(atEdge.peek).toBe(DRAWN - BOX);
    expect(inside(5, atEdge.peek)).toBe(true);
    // Нажатие человека 40 мс назад — эхо: окно идёт к курсору в этом же пересчёте.
    const human = occlusionStep(atEdge.state, { ...input(4, 23, atEdge.peek, 300, true), humanInputAgoMs: 40 });
    expect(human.recheckMs).toBeNull();
    expect(human.state.holdSince).toBeNull();
    expect(human.peek).toBe(keyboardPeek(DRAWN, BOX, ROWS, 4, atEdge.peek));
    expect(inside(4, human.peek)).toBe(true);
    // Контроль: тот же сдвиг курсора выводом программы (нажатия не было, или
    // оно было раньше окна) выжидает, курсор пока вне коробки.
    for (const humanInputAgoMs of [undefined, OCCLUSION_HUMAN_INPUT_MS + 1, -5, NaN]) {
      const output = occlusionStep(atEdge.state, { ...input(4, 23, atEdge.peek, 300, true), humanInputAgoMs });
      expect(output.recheckMs).toBe(OCCLUSION_HOLD_MS);
      expect(output.peek).toBe(atEdge.peek);
      expect(inside(4, output.peek)).toBe(false);
    }
    // Граница окна включительно.
    expect(occlusionStep(atEdge.state, { ...input(4, 23, atEdge.peek, 300, true), humanInputAgoMs: OCCLUSION_HUMAN_INPUT_MS }).recheckMs)
      .toBeNull();
    // Нажатие посреди уже идущей выдержки снимает её сразу.
    const holding = occlusionStep(atEdge.state, input(4, 23, atEdge.peek, 300, true));
    expect(holding.state.holdSince).toBe(300);
    const released = occlusionStep(holding.state, { ...input(3, 23, holding.peek, 320, true), humanInputAgoMs: 10 });
    expect(released.recheckMs).toBeNull();
    expect(released.state.holdSince).toBeNull();
    expect(inside(3, released.peek)).toBe(true);
  });

  it("свойство: вне выдержки курсор виден всегда, в режиме «оба» — и последняя строка", () => {
    let held = 0;
    let both = 0;
    for (const box of [120, 200, 300, 366, 440]) for (let cursor = 0; cursor < ROWS; cursor++)
      for (let last = -1; last < ROWS; last++) for (const previous of [0, 37, 90, 200, 999])
        for (const state of [OCCLUSION_IDLE, { both: true, holdSince: null }]) for (const fromOutput of [false, true]) {
          const s = occlusionStep(state, input(cursor, last, previous, 0, fromOutput, box));
          expect(s.peek).toBeGreaterThanOrEqual(0);
          expect(s.peek).toBeLessThanOrEqual(DRAWN - box);
          if (s.recheckMs !== null) {
            held++;
            expect(fromOutput && state.both).toBe(true);
            expect(s.peek).toBe(Math.round(Math.min(DRAWN - box, previous)));
            expect(s.recheckMs).toBe(OCCLUSION_HOLD_MS);
            continue;
          }
          expect(inside(cursor, s.peek, box)).toBe(true);
          if (s.state.both && last >= 0) { expect(inside(last, s.peek, box)).toBe(true); both++; }
        }
    expect(held).toBeGreaterThan(100);
    expect(both).toBeGreaterThan(1000);
  });
});

describe("lastInkRow: строка только с фоном — не пустая (скептик 15.09, находка 3)", () => {
  const write = (term: HeadlessTerminal, data: string) => new Promise<void>((resolve) => term.write(data, resolve));

  it("текст, фон без текста, инверсия, пробелы; строки — от видимого окна", async () => {
    const term = new HeadlessTerminal({ cols: 20, rows: 6, allowProposedApi: true });
    const buffer = () => term.buffer.active;
    expect(lastInkRow(buffer(), 6)).toBe(-1);
    // Скан по тексту видел бы строку 0; синяя строка 2 без текста — тоже вывод.
    await write(term, "текст\r\n\r\n\x1b[44m\x1b[K\x1b[0m");
    expect(lastInkRow(buffer(), 6)).toBe(2);
    await write(term, "\r\n\r\n\x1b[7m   \x1b[0m");
    expect(lastInkRow(buffer(), 6)).toBe(4);
    // Одни пробелы с фоном по умолчанию — пусто.
    await write(term, "\r\n      ");
    expect(lastInkRow(buffer(), 6)).toBe(4);
    await write(term, "\r\n" + Array.from({ length: 10 }, (_, i) => `a${i}`).join("\r\n"));
    expect(buffer().viewportY).toBeGreaterThan(0);
    expect(lastInkRow(buffer(), 6)).toBe(5);
    term.dispose();
  });
});

describe("panBy: жест и колесо", () => {
  const limits = { drawnW: 1920, drawnH: 640, visibleW: 384, visibleH: 480 };

  it("прибавляет и зажимает по краям", () => {
    expect(panBy({ x: 0, y: 0 }, 100, 50, limits)).toEqual({ x: 100, y: 50 });
    expect(panBy({ x: 1500, y: 150 }, 100, 50, limits)).toEqual({ x: 1536, y: 160 });
    expect(panBy({ x: 10, y: 10 }, -100, -100, limits)).toEqual({ x: 0, y: 0 });
  });

  it("копит дробные пиксели медленного жеста", () => {
    let pan = { x: 0, y: 0 };
    for (let i = 0; i < 10; i++) pan = panBy(pan, 0.4, 0, limits);
    expect(pan.x).toBeCloseTo(4, 6);
  });

  it("сетка помещается — жест не сдвигает", () => {
    expect(panBy({ x: 0, y: 0 }, 200, 200, { drawnW: 300, drawnH: 300, visibleW: 384, visibleH: 480 })).toEqual({ x: 0, y: 0 });
  });

  it("мусорные значения не дают NaN", () => {
    expect(panBy({ x: NaN, y: 5 }, Infinity, NaN, limits)).toEqual({ x: 0, y: 5 });
  });
});

describe("ST-10: кто ведёт окно по X, колесо и блокировка оси", () => {
  it("panFollows: до ручного сдвига — курсор; после — человек; ввод возвращает курсору", () => {
    expect(panFollows(0, 0)).toBe(true);
    expect(panFollows(100, 200)).toBe(false);
    expect(panFollows(300, 200)).toBe(true);
    expect(panFollows(200, 200)).toBe(true);
  });

  it("wheelPanDelta: только доминирующая горизонталь, без Ctrl/Meta; единицы deltaMode", () => {
    const e = { deltaX: 0, deltaY: 0, deltaMode: 0, ctrlKey: false, metaKey: false };
    expect(wheelPanDelta({ ...e, deltaX: 120, deltaY: 10 }, 9, 384)).toBe(120);
    expect(wheelPanDelta({ ...e, deltaX: -30, deltaY: 5 }, 9, 384)).toBe(-30);
    expect(wheelPanDelta({ ...e, deltaX: 3, deltaMode: 1 }, 9, 384)).toBe(27);
    expect(wheelPanDelta({ ...e, deltaX: 1, deltaMode: 2 }, 9, 384)).toBe(384);
    // Вертикаль и равенство — прокрутка маршрута навигации, не сдвиг.
    expect(wheelPanDelta({ ...e, deltaX: 10, deltaY: 40 }, 9, 384)).toBeNull();
    expect(wheelPanDelta({ ...e, deltaX: 10, deltaY: -10 }, 9, 384)).toBeNull();
    // Масштаб браузера не трогаем.
    expect(wheelPanDelta({ ...e, deltaX: 100, ctrlKey: true }, 9, 384)).toBeNull();
    expect(wheelPanDelta({ ...e, deltaX: 100, metaKey: true }, 9, 384)).toBeNull();
    expect(wheelPanDelta({ ...e, deltaX: NaN }, 9, 384)).toBeNull();
  });

  it("touchAxis: зона неуверенности, вертикаль — прокрутка, горизонталь — сдвиг только при запасе", () => {
    expect(touchAxis(5, -7, true)).toBeNull();
    expect(touchAxis(3, 20, true)).toBe("scroll");
    expect(touchAxis(-3, -20, false)).toBe("scroll");
    expect(touchAxis(20, 3, true)).toBe("pan");
    expect(touchAxis(-20, 3, true)).toBe("pan");
    // Сетка помещается — поведение прежнее: горизонталь не наш жест.
    expect(touchAxis(20, 3, false)).toBe("passthrough");
    // Диагональ 45° — вертикаль (прежнее правило |dx| > |dy| строгое).
    expect(touchAxis(12, 12, true)).toBe("scroll");
  });
});
