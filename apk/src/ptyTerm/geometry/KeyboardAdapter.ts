import { Capacitor } from "@capacitor/core";
import { Keyboard } from "@capacitor/keyboard";
import { panAxis } from "./viewportPan";

/** Native events take precedence once observed; web keeps visualViewport.
 * Handles that arrive after disposal are removed too (StrictMode/unmount).
 */
export function observeNativeKeyboard(changed: (open: boolean) => void): () => void {
  if (!Capacitor.isNativePlatform() || !Capacitor.isPluginAvailable("Keyboard")) return () => {};
  let disposed = false;
  const handles: { remove(): Promise<void> }[] = [];
  const shown = () => { if (!disposed) changed(true); };
  const hidden = () => { if (!disposed) changed(false); };
  for (const pending of [Keyboard.addListener("keyboardWillShow", shown), Keyboard.addListener("keyboardDidShow", shown),
    Keyboard.addListener("keyboardWillHide", hidden), Keyboard.addListener("keyboardDidHide", hidden)]) {
    void pending
      .then(handle => { if (disposed) void handle.remove(); else handles.push(handle); })
      .catch(() => { /* Legacy APK: viewport remains the fallback. */ });
  }
  return () => { disposed = true; for (const handle of handles) void handle.remove(); };
}

/**
 * Keep the active cursor row visible with the smallest necessary pan.
 *
 * Частный случай двумерного viewportPan по вертикали (ST-10): то же правило
 * минимального сдвига, но контракт прежний — курсор вне сетки сбрасывает
 * сдвиг в 0, а высота ячейки берётся как drawn / rows.
 */
export function keyboardPeek(drawn: number, visible: number, rows: number,
  cursorRow: number, previous = 0): number {
  if (drawn <= visible || visible <= 0 || rows <= 0 || cursorRow < 0 || cursorRow >= rows) return 0;
  return panAxis(drawn, visible, drawn / rows, cursorRow, previous, true);
}

/**
 * Сдвиг под ПЕРЕКРЫТИЕМ (ST-08, I-09/I-10): выросло поле ввода или легла
 * плашка, сетка PTY прежняя, видимое окно меньше.
 *
 * Одного курсора мало. У TUI бывают строки ПОД строкой ввода (подвал Codex:
 * четыре строки подсказок под «> промпт»). Минимальный сдвиг к курсору их
 * срезал снизу. Замер скептика волны 4 (390×844, сетка 24, коробка 366 px):
 * курсор в строке 18 помещается без сдвига, а строки 19–22 срезаны все четыре.
 * PTY при этом о новом размере не узнаёт, поэтому приложение подвал не
 * переложит. Правило: наименьший сдвиг от прежнего, при котором видны И строка
 * курсора, И последняя непустая строка. Срезается верх, то есть история.
 * Если обе строки в окно не помещаются (курсор вверху, строка состояния vim
 * внизу), остаётся правило курсора (keyboardPeek): печатать надо туда, где
 * курсор.
 *
 * `lastTextRow` — индекс последней непустой строки видимой сетки, или -1.
 */
export function occlusionPeek(drawn: number, visible: number, rows: number,
  cursorRow: number, lastTextRow: number, previous = 0): number {
  const cursorOnly = keyboardPeek(drawn, visible, rows, cursorRow, previous);
  if (drawn <= visible || visible <= 0 || rows <= 0 || cursorRow < 0 || cursorRow >= rows) return cursorOnly;
  if (!Number.isInteger(lastTextRow) || lastTextRow < 0 || lastTextRow >= rows) return cursorOnly;
  const cell = drawn / rows;
  const maximum = drawn - visible;
  // Окно [pan, pan + visible] должно накрыть обе строки целиком.
  const low = Math.max(0, (Math.max(cursorRow, lastTextRow) + 1) * cell - visible);
  const high = Math.min(maximum, Math.min(cursorRow, lastTextRow) * cell);
  if (low > high) return cursorOnly;
  const from = Number.isFinite(previous) ? previous : 0;
  return Math.round(Math.max(low, Math.min(high, from)));
}

/**
 * Выдержка (мс): вывод программы тянет окно ВВЕРХ из режима «курсор и
 * последняя строка» — окно ждёт, не вернётся ли курсор (ST-06, скептик 15.09).
 *
 * Агент читает PTY кусками по 8192 байта, клиент спускает запись раз в кадр:
 * одна перерисовка TUI может разобраться ДВУМЯ записями, и между ними курсор
 * стоит там, куда его увела середина кадра (подвал Codex: строка 1 вместо 18).
 * Без выдержки окно уезжало к нему и следующей записью обратно: 100 смен
 * сдвига на 60 перерисовок (19↔71 px). Каждая смена ещё и гасит жест
 * (выделение долгим нажатием не начиналось). Перерисовка, пришедшая целиком,
 * выдержку не трогает; уход курсора, который держится дольше, — применяется.
 * Сдвиг ВНИЗ (к курсору ниже окна) не выжидает никогда.
 * Эхо ввода ЧЕЛОВЕКА не выжидает тоже (OCCLUSION_HUMAN_INPUT_MS).
 */
export const OCCLUSION_HOLD_MS = 100;
/**
 * Ввод человека (не автоответ терминала) не раньше стольких мс до пересчёта —
 * курсор ведёт его нажатие, а не середина чужой перерисовки: выдержка не
 * применяется (скептик 15.09, повторная проверка). Без этого k/↑ в vim через
 * верхнюю границу окна в режиме «оба» оставлял курсор вне коробки 116–122 мс
 * вместо одного кадра. Цена: перерисовка двумя записями БЕЗ ?2026 в эти
 * 300 мс снова может дёрнуть окно — ровно как до выдержки, и только сразу
 * после нажатия (транзакции 2026 ждут конца по-прежнему).
 */
export const OCCLUSION_HUMAN_INPUT_MS = 300;
/**
 * Назначение таймера повторного пересчёта: "sync" — страховка конца ?2026
 * (OCCLUSION_SYNC_RECHECK_MS), "hold" — конец выдержки (recheckMs). Таймер,
 * чья причина ушла, снимается и пересчёта не делает.
 */
export type OcclusionRecheck = "sync" | "hold";
/**
 * Таймер пересчёта сработал, пока человек листает историю, выделяет или ведёт
 * жест: окно не трогаем (смена сдвига гасит жест), спрашиваем снова через
 * столько мс.
 */
export const OCCLUSION_BUSY_RECHECK_MS = 250;
/**
 * Запас (в строках), с которым окно ВХОДИТ в режим «курсор и последняя
 * строка»; остаётся в нём, пока обе строки просто помещаются. Гистерезис:
 * vim у границы (строка состояния в 23, j/k по строкам 3–6) без него
 * переключался на каждой границе — 6 смен за два цикла, вниз 4:0 → 5:90 px на
 * одной строке. Подвал Codex (курсор 18, подвал до 22) входит с запасом
 * в 14 строк — ему запас не мешает.
 */
export const OCCLUSION_ENTER_ROOM_ROWS = 2;
/**
 * Приложение внутри ?2026h…?2026l — пересчёт ждёт конца транзакции; этот
 * срок — страховка, если конец не придёт разбором (сторож xterm снимает режим
 * через 1 с сам, без записи).
 */
export const OCCLUSION_SYNC_RECHECK_MS = 1050;

export interface OcclusionState {
  /** Окно держит видимыми и строку курсора, и последнюю непустую строку. */
  both: boolean;
  /** С какого момента вывод тянет окно вверх из режима «оба», или null. */
  holdSince: number | null;
}
export const OCCLUSION_IDLE: OcclusionState = Object.freeze({ both: false, holdSince: null });

export interface OcclusionInput {
  drawn: number;
  visible: number;
  rows: number;
  cursorRow: number;
  lastTextRow: number;
  previous: number;
  now: number;
  /** Пересчёт от вывода программы (курсор сдвинулся), а не от геометрии, клавиатуры, жеста. */
  fromOutput: boolean;
  /**
   * Сколько мс назад был последний ввод человека (не автоответ), в той же
   * шкале времени, что и сам ввод. Не дальше OCCLUSION_HUMAN_INPUT_MS —
   * вывод считается эхом нажатия, выдержка не применяется. Нет — ввода не было.
   */
  humanInputAgoMs?: number;
}

export interface OcclusionStep {
  peek: number;
  state: OcclusionState;
  /** Идёт выдержка — пересчитать через столько мс; иначе null. */
  recheckMs: number | null;
}

/**
 * Сдвиг под перекрытием с памятью (ST-08 × ST-06): то же правило occlusionPeek,
 * плюс два гистерезиса, чтобы окно не ходило за промежуточными положениями.
 *
 * 1. Вход в режим «оба» — только с запасом OCCLUSION_ENTER_ROOM_ROWS строк;
 *    выход — когда обе строки перестали помещаться. Режим «только курсор» —
 *    прежнее правило keyboardPeek без задержек.
 * 2. В режиме «оба» пересчёт ОТ ВЫВОДА, который поднял бы окно, выжидает
 *    OCCLUSION_HOLD_MS: сдвиг прежний, recheckMs — когда спросить снова. Вернулся
 *    курсор — выдержка снимается без единой смены. Геометрия, клавиатура, жест
 *    (fromOutput=false) применяются сразу.
 *
 * Курсор вне сетки (человек читает историю) или перекрытия нет — состояние
 * сбрасывается, результат как у keyboardPeek.
 */
export function occlusionStep(state: OcclusionState, input: OcclusionInput): OcclusionStep {
  const { drawn, visible, rows, cursorRow, previous, now } = input;
  const cursorOnly = keyboardPeek(drawn, visible, rows, cursorRow, previous);
  if (drawn <= visible || visible <= 0 || rows <= 0 || cursorRow < 0 || cursorRow >= rows) {
    return { peek: cursorOnly, state: OCCLUSION_IDLE, recheckMs: null };
  }
  // Пустой экран — последней строкой считается строка курсора.
  const last = Number.isInteger(input.lastTextRow) && input.lastTextRow >= 0 && input.lastTextRow < rows
    ? input.lastTextRow : cursorRow;
  const cell = drawn / rows;
  const room = visible - (Math.abs(last - cursorRow) + 1) * cell;
  const both = room >= 0 && (state.both || room >= OCCLUSION_ENTER_ROOM_ROWS * cell);
  const target = both ? occlusionPeek(drawn, visible, rows, cursorRow, last, previous) : cursorOnly;
  const held = Number.isFinite(previous) ? Math.round(Math.max(0, Math.min(drawn - visible, previous))) : 0;
  // Эхо нажатия человека — не середина чужой перерисовки: окно идёт к курсору сразу.
  const ago = input.humanInputAgoMs;
  const humanInput = typeof ago === "number" && Number.isFinite(ago) && ago >= 0 && ago <= OCCLUSION_HUMAN_INPUT_MS;
  if (input.fromOutput && state.both && target < held && !humanInput) {
    const since = state.holdSince ?? now;
    const waited = Number.isFinite(now - since) ? Math.max(0, now - since) : OCCLUSION_HOLD_MS;
    if (waited < OCCLUSION_HOLD_MS) {
      return { peek: held, state: { both: true, holdSince: since }, recheckMs: OCCLUSION_HOLD_MS - waited };
    }
  }
  return { peek: target, state: { both, holdSince: null }, recheckMs: null };
}

/** Ровно то из буфера xterm, что нужно скану (браузерный и headless совпадают). */
interface InkCell { isBgDefault(): boolean; isInverse(): number }
interface InkLine {
  readonly length: number;
  translateToString(trimRight?: boolean): string;
  getCell(x: number, cell?: InkCell): InkCell | undefined;
}
interface InkBuffer {
  readonly viewportY: number;
  getLine(y: number): InkLine | undefined;
  getNullCell(): InkCell;
}

/**
 * Последняя непустая строка видимой сетки, снизу вверх, или -1. Непустая —
 * есть текст ИЛИ хоть одна ячейка с цветным фоном или инверсией: строку
 * состояния без текста (один фон) человек видит так же, как текст, а скан по
 * тексту считал её пустой (скептик: сдвиг 14 px, синие строки целиком ниже
 * коробки). Ячейки перебираются только у строк без текста; скан
 * останавливается на первой непустой снизу.
 */
export function lastInkRow(buffer: InkBuffer, rows: number): number {
  const cell = buffer.getNullCell();
  for (let i = rows - 1; i >= 0; i--) {
    const line = buffer.getLine(buffer.viewportY + i);
    if (!line) continue;
    if (line.translateToString(true).trim().length > 0) return i;
    for (let x = 0; x < line.length; x++) {
      const c = line.getCell(x, cell);
      if (c && (!c.isBgDefault() || c.isInverse())) return i;
    }
  }
  return -1;
}
