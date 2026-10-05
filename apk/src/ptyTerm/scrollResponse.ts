export interface ScreenScrollResponse {
  answered: boolean;
  changedRows: number;
  shiftedRows: number;
  /**
   * «shift» — строки сдвинулись: это похоже на прокрутку и доказывает ответ;
   * «repaint» — сменилось почти всё окно без сдвига: так выглядит и настоящая
   * смена страницы, и НЕЗАВИСИМЫЙ большой ответ агента во время пробы (аудит
   * 13.09, A03), поэтому одно такое наблюдение способность не доказывает
   * (navigationEvidence.ts); «none» — ответа нет.
   */
  kind: "shift" | "repaint" | "none";
}

type TerminalRowsSource = {
  rows: number;
  buffer: { active: {
    viewportY: number;
    baseY?: number;
    getLine(index: number): { translateToString(trimRight?: boolean): string } | undefined;
  } };
};

/**
 * Строки окна терминала. `from: "screen"` — живой экран (от baseY), а не то,
 * что человек сейчас читает: ответ приложения на PgUp появляется на живом
 * экране, и человек, отлиставший свою историю вверх, иначе видел бы «молчание»
 * там, где приложение ответило. Для якоря «значимого нового вывода» так же:
 * сравнивается живой экран, а не место чтения.
 */
export function captureTerminalRows(term: TerminalRowsSource, from: "viewport" | "screen" = "viewport"): string[] {
  try {
    const buffer = term.buffer.active;
    const top = from === "screen" ? (buffer.baseY ?? buffer.viewportY) : buffer.viewportY;
    const rows: string[] = [];
    for (let y = 0; y < term.rows; y++) {
      rows.push(buffer.getLine(top + y)?.translateToString(true) ?? "");
    }
    return rows;
  } catch {
    return [];
  }
}

/**
 * A scroll acknowledgement must look like scrolling, not like a live spinner.
 * Comparing one joined signature made a clock/spinner on one row count as a
 * successful PgUp. We compare rows and require either a substantial row delta
 * or a strong shifted-row overlap.
 *
 * `intent` — направление отправленного действия. PgUp/колесо вверх открывают
 * СТАРШИЙ текст: прежние строки уезжают ВНИЗ. Новый вывод приложения в обычном
 * буфере сдвигает строки ВВЕРХ — и без направления любой ответ агента во время
 * пробы засчитывался бы сдвигом-доказательством (аудит 13.09, A03: ответ
 * оценивается относительно отправленной команды). Сдвиг против намерения
 * доказательством не считается; при почти полной смене окна он остаётся
 * «repaint», то есть слабым наблюдением.
 */
export function screenScrollResponse(
  before: readonly string[],
  after: readonly string[],
  intent?: "up" | "down",
): ScreenScrollResponse {
  if (before.length < 3 || before.length !== after.length) {
    return { answered: false, changedRows: 0, shiftedRows: 0, kind: "none" };
  }
  const rows = before.length;
  let changedRows = 0;
  for (let i = 0; i < rows; i++) if (before[i] !== after[i]) changedRows++;

  const beforeCounts = new Map<string, number>();
  const afterCounts = new Map<string, number>();
  for (const row of before) if (row) beforeCounts.set(row, (beforeCounts.get(row) ?? 0) + 1);
  for (const row of after) if (row) afterCounts.set(row, (afterCounts.get(row) ?? 0) + 1);
  const uniqueInBoth = (row: string) => row !== ""
    && beforeCounts.get(row) === 1
    && afterCounts.get(row) === 1;

  let shiftedRows = 0;
  for (let shift = 1; shift < rows; shift++) {
    let up = 0;
    let down = 0;
    for (let i = 0; i < rows - shift; i++) {
      if (uniqueInBoth(before[i + shift]) && before[i + shift] === after[i]) up++;
      if (uniqueInBoth(before[i]) && before[i] === after[i + shift]) down++;
    }
    // Намерение «вверх» (к старому) двигает прежние строки вниз, и наоборот.
    if (intent === "up") shiftedRows = Math.max(shiftedRows, down);
    else if (intent === "down") shiftedRows = Math.max(shiftedRows, up);
    else shiftedRows = Math.max(shiftedRows, up, down);
  }

  // A busy TUI can repaint a status block without moving its history at all.
  // Treat a non-shifted response as a page only when it replaces nearly the
  // whole viewport; smaller deltas need strong row-shift evidence.
  //
  // «Окно» здесь — область, которая вообще менялась: неподвижные строки сверху
  // и снизу (поле ввода и строка состояния Claude Code) листаться не могут.
  // Лог владельца 23.09: на 48×25 PgUp Claude меняла 16–18 строк переписки,
  // семь строк подвала стояли, порог от всех 25 строк (19) не набирался —
  // «страница не ответила», «Авто» уходил в пустой «Вывод», человек
  // переключал «Агент» руками. Нижняя граница области — 60 % экрана, чтобы
  // перерисовка одного блока статуса не становилась «страницей».
  let firstChanged = -1;
  let lastChanged = -1;
  for (let i = 0; i < rows; i++) {
    if (before[i] === after[i]) continue;
    if (firstChanged < 0) firstChanged = i;
    lastChanged = i;
  }
  const span = firstChanged < 0 ? 0 : lastChanged - firstChanged + 1;
  const region = Math.max(span, Math.ceil(rows * 0.6));
  const repaintThreshold = Math.max(6, Math.ceil(region * 0.75));
  const shiftedThreshold = Math.max(3, Math.ceil(region * 0.45));
  const shifted = changedRows >= 3 && shiftedRows >= shiftedThreshold;
  const repainted = changedRows >= repaintThreshold;
  return {
    answered: shifted || repainted,
    changedRows,
    shiftedRows,
    kind: shifted ? "shift" : repainted ? "repaint" : "none",
  };
}
