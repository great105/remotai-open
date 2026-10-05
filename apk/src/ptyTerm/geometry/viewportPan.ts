/**
 * ДВУМЕРНЫЙ СДВИГ ВИДИМОГО ОКНА ПО ЛОГИЧЕСКОЙ СЕТКЕ (ST-10 «полный двумерный
 * viewport», ST-08 «локально видимый прямоугольник», T-17, T-31, T-32).
 *
 * Логическая сетка (PTY) может быть больше коробки по обеим осям:
 *   • по высоте — под клавиатурой и плашками (I-09: сетку они не меняют);
 *   • по ширине — когда рядом более широкий владелец размера или когда
 *     принята сетка кадра шире нашего экрана (adopt при полу 20×10).
 * До этого модуля сдвиг был только вертикальным и только под клавиатурой
 * (keyboardPeek), а правая часть широкой сетки просто обрезалась
 * `overflow: hidden` — курсор в колонке 200 при экране в 48 колонок был
 * невидим, и печатать туда было вслепую.
 *
 * Правило одно на обе оси: МИНИМАЛЬНЫЙ сдвиг, при котором ячейка курсора (или
 * найденного совпадения) видна, с зажимом по краям сетки. Минимальный — чтобы
 * окно не прыгало при каждом движении курсора: пока курсор внутри, сдвиг
 * остаётся прежним. Всё помещается — сдвига нет вовсе.
 *
 * Модуль чистый: ни DOM, ни React. Пиксели меряет вызывающий код (ширина
 * `.xterm-screen` против коробки), а попадание касаний и так учитывает
 * transform (TerminalCoordinates.cellAt меряет прямоугольник после сдвига).
 */

/** Сдвиг видимого окна в пикселях: насколько окно уехало вправо и вниз. */
export interface PanOffset { x: number; y: number }

/** Размер нарисованной сетки и видимой коробки, пиксели. */
export interface PanLimits { drawnW: number; drawnH: number; visibleW: number; visibleH: number }

export interface ViewportPanInput extends PanLimits {
  /** Размер ячейки, пиксели. */
  cellW: number;
  cellH: number;
  /** Ячейка, которую надо держать видимой (курсор, совпадение поиска). */
  cursorCol: number;
  cursorRow: number;
  /** Текущий сдвиг — от него считается минимальное изменение. */
  prev: PanOffset;
  /**
   * Следовать ли за ячейкой. false — только зажать прежний сдвиг по новым
   * краям (человек сам двигает окно пальцем, колесом или читает).
   */
  follow: boolean;
}

/**
 * Сдвиг по одной оси. Экспортирован для keyboardPeek, который остаётся частным
 * случаем по вертикали.
 *
 * `index` вне сетки или неизвестный размер ячейки — следовать не за чем, прежний
 * сдвиг только зажимается.
 */
export function panAxis(drawn: number, visible: number, cell: number, index: number,
  previous: number, follow: boolean): number {
  if (!(visible > 0) || !(drawn > visible)) return 0;
  const maximum = drawn - visible;
  let pan = clamp(Number.isFinite(previous) ? previous : 0, 0, maximum);
  if (follow && cell > 0 && Number.isFinite(cell) && index >= 0 && index * cell < drawn) {
    const start = index * cell, end = start + cell;
    if (start < pan) pan = start;
    if (end > pan + visible) pan = end - visible;
  }
  return Math.round(clamp(pan, 0, maximum));
}

/** Минимальный сдвиг по обеим осям, чтобы ячейка курсора была видна. */
export function viewportPan(p: ViewportPanInput): PanOffset {
  return {
    x: panAxis(p.drawnW, p.visibleW, p.cellW, p.cursorCol, p.prev.x, p.follow),
    y: panAxis(p.drawnH, p.visibleH, p.cellH, p.cursorRow, p.prev.y, p.follow),
  };
}

/**
 * Сдвиг жестом или колесом: прибавить и зажать по краям.
 *
 * `dx`, `dy` — насколько сдвинуть ОКНО (положительное — вправо и вниз).
 * Палец тянет содержимое, поэтому для касания это минус движение пальца.
 * Результат НЕ округляется: жест копит дробные пиксели, иначе медленное
 * движение по 0,4 px за событие не сдвинуло бы окно никогда. Округлять — при
 * записи в CSS.
 */
export function panBy(prev: PanOffset, dx: number, dy: number, limits: PanLimits): PanOffset {
  const maxX = Math.max(0, finite(limits.drawnW) - finite(limits.visibleW));
  const maxY = Math.max(0, finite(limits.drawnH) - finite(limits.visibleH));
  return {
    x: clamp(finite(prev.x) + finite(dx), 0, maxX),
    y: clamp(finite(prev.y) + finite(dy), 0, maxY),
  };
}

/**
 * Кто ведёт окно по X (ST-10): курсор или человек.
 *
 * Человек сдвинул окно пальцем или колесом — он читает правую часть, и курсор,
 * бегающий по строке состояния, окно у него не отбирает. Следующий ввод
 * человека (клавиша, отправка поля) снова отдаёт окно курсору: печатать надо
 * туда, где курсор. До первого ручного сдвига окно следует за курсором.
 */
export function panFollows(lastInputAt: number, lastManualPanAt: number): boolean {
  return !(lastManualPanAt > lastInputAt);
}

/**
 * Колесо и трекпад: горизонтальный сдвиг окна в пикселях или null.
 *
 * Только доминирующий deltaX без Ctrl/Meta (масштаб браузера не трогаем).
 * Вертикаль (null) остаётся прокруткой единого маршрута навигации ST-02/03 —
 * в приложение горизонталь не уходит никогда. deltaMode: 0 — пиксели,
 * 1 — строки (ширина ячейки), 2 — страницы (ширина окна).
 */
export function wheelPanDelta(e: { deltaX: number; deltaY: number; deltaMode: number; ctrlKey: boolean; metaKey: boolean },
  cellW: number, pageW: number): number | null {
  if (e.ctrlKey || e.metaKey) return null;
  if (!Number.isFinite(e.deltaX) || !(Math.abs(e.deltaX) > Math.abs(finite(e.deltaY)))) return null;
  const unit = e.deltaMode === 1 ? finite(cellW) : e.deltaMode === 2 ? finite(pageW) : 1;
  return e.deltaX * unit;
}

/** Зона неуверенности пальца, px: меньше — ещё не жест (как прежде в PtyTermView). */
export const TOUCH_SLOP_PX = 8;

/**
 * Блокировка оси касания: первое уверенное движение решает на весь жест.
 *   • null — палец ещё в зоне неуверенности;
 *   • "scroll" — вертикаль: прежний маршрут прокрутки ST-02/03, без изменений;
 *   • "pan" — горизонталь, и сетка шире коробки: сдвиг окна, в PTY ничего;
 *   • "passthrough" — горизонталь, сдвигать нечего: как раньше, жест не наш.
 */
export function touchAxis(dx: number, dy: number, canPanX: boolean): "scroll" | "pan" | "passthrough" | null {
  if (Math.abs(dy) < TOUCH_SLOP_PX && Math.abs(dx) < TOUCH_SLOP_PX) return null;
  if (Math.abs(dx) > Math.abs(dy)) return canPanX ? "pan" : "passthrough";
  return "scroll";
}

function clamp(v: number, lo: number, hi: number): number {
  return Math.max(lo, Math.min(hi, v));
}

function finite(v: number): number {
  return Number.isFinite(v) ? v : 0;
}
