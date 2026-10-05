import type { IBuffer } from "@xterm/xterm";
import type { EraseScrollback } from "../keepHistory";
import { logicalRowText } from "./bufferText";

export interface ReadLine {
  readonly text: string;
  readonly start: number;
  readonly end: number;
  readonly wrapped: boolean;
  /** UTF-16 offsets for column boundaries; wide-cell continuations share an offset. */
  readonly columns: readonly number[];
}
export interface ReadDocument {
  /**
   * Идентичность ВЕРСИИ документа (ST-04, I-07): `session:epoch:version`.
   * Раньше её роль играл React-ключ capturedAt:firstBufferRow, а firstBufferRow
   * относителен буферу xterm и устаревает при обрезке scrollback.
   */
  readonly id: string;
  /** Монотонный номер снятия: новый снимок того же места — новая версия. */
  readonly version: number;
  readonly session: string;
  readonly epoch: string;
  readonly offset: number;
  readonly geometryRevision: number;
  readonly capturedAt: number;
  readonly source: "terminal" | "screen" | "agent";
  readonly sourceDetail?: string;
  readonly partial?: boolean;
  readonly streamGap?: boolean;
  /** Политика хранения поколения на момент снятия (keepHistory.retentionFor); нет — не передали. */
  readonly retention?: EraseScrollback;
  /**
   * Счётчик событий, реально уносивших историю (исполненный CSI 3 J, RIS,
   * ручная очистка), на момент снятия. Листание Older/Newer сверяет его, а не
   * эпоху writer, которая меняется на каждом sync-маркере (ST-04).
   */
  readonly historySeq: number;
  /** В этом поколении история уже стиралась приложением/RIS/вручную: начало буфера не начало вывода (I-11). */
  readonly erasedBefore: boolean;
  /** Буфер был на пределе scrollback: старший вывод вытеснен лимитом, а не отсутствовал (I-11). */
  readonly evictedBefore: boolean;
  /**
   * Буфер собран нашим RIS из ХВОСТА потока эпохи (маркер reset с базой > 0 или
   * кадр с историей зеркала на её пределе): более ранний вывод этой эпохи на
   * устройстве не хранится и с компьютера не пришёл (I-11, волна 4). Нет поля
   * (документ истории агента, старый вызов) — отметки нет.
   */
  readonly ringTruncatedBefore?: boolean;
  readonly firstBufferRow: number;
  readonly truncatedBefore: boolean;
  readonly truncatedAfter: boolean;
  readonly cols: number;
  readonly lines: readonly ReadLine[];
  readonly text: string;
}
export interface ReadStamp {
  session: string; epoch: string; offset: number; geometryRevision: number; streamGap?: boolean;
  // Всё ниже необязательно: старые вызовы captureReadDocument продолжают работать (ST-04, этап 2).
  /** Номер версии от вызывающего (монотонный счётчик экрана); нет — внутренний счётчик модуля. */
  version?: number;
  historySeq?: number;
  erasedBefore?: boolean;
  retention?: EraseScrollback;
  /** Ёмкость буфера xterm: по ней определяется evictedBefore (length >= scrollback + rows). */
  capacity?: { scrollback: number; rows: number };
  /** Буфер начат с хвоста потока эпохи (см. ReadDocument.ringTruncatedBefore). */
  ringTruncatedBefore?: boolean;
}

/**
 * Сколько строк истории держит зеркало на компьютере — копия
 * internal/pty/screen.go screenMirrorScrollback. Кадр, чья scrollback-часть
 * истории (hist_lines − screen_rows) дошла до предела, отдал не всё.
 */
export const MIRROR_HISTORY_LINES = 500;

/**
 * Начат ли буфер с хвоста потока эпохи после нашего RIS (I-11, волна 4).
 * Маркер reset: база > 0 — пришёл только хвост кольца. Кадр с заменой: история
 * зеркала на пределе, или истории нет вовсе, а поток эпохи до кадра был.
 * ⚠ hist_lines 0 у живого агента бывает только в alt-screen (screen.go
 * historyLocked): нормальный буфер после такой замены действительно пуст, а
 * документ самого alt-экрана отметку не несёт (captureReadDocument).
 */
export function ringCutAfterRis(src: { kind: "marker"; base: number | undefined }
  | { kind: "frame"; histLines: number | undefined; screenRows: number | undefined; base: number | undefined }): boolean {
  if (src.kind === "marker") return typeof src.base === "number" && src.base > 0;
  const hist = typeof src.histLines === "number" && src.histLines > 0 ? src.histLines : 0;
  const rows = typeof src.screenRows === "number" && src.screenRows > 0 ? src.screenRows : 0;
  if (hist === 0) return typeof src.base !== "number" || src.base > 0;
  return hist - rows >= MIRROR_HISTORY_LINES;
}

let documentVersion = 0;
/** Следующий номер версии документа; общий для терминального и агентского чтения. */
export function nextDocumentVersion(): number {
  return ++documentVersion;
}

/**
 * Какое уведомление о полноте показать над документом (I-11). Разрыв связи и
 * частичный источник важнее всего; у самого начала доступного текста честно
 * различаем «стёрто приложением» и «вытеснено лимитом» — раньше оба случая
 * выглядели как полный документ или безликое «есть ещё текст».
 */
export function completenessNotice(doc: Pick<ReadDocument, "streamGap" | "partial" | "truncatedBefore"
  | "truncatedAfter" | "erasedBefore" | "evictedBefore"> & { ringTruncatedBefore?: boolean }): string {
  if (doc.streamGap) return "pty.historyGap";
  if (doc.partial) return "pty.readPartial";
  if (!doc.truncatedBefore && doc.erasedBefore) return "pty.readErased";
  if (!doc.truncatedBefore && doc.ringTruncatedBefore) return "pty.readRingLimited";
  if (!doc.truncatedBefore && doc.evictedBefore) return "pty.readEvicted";
  if (doc.truncatedBefore || doc.truncatedAfter) return "pty.readLimited";
  return "pty.readFrozen";
}

/**
 * Можно ли продолжать листать от этого документа (ST-04, T-14). История, из
 * которой он снят, должна быть той же: ни одного стирания/RIS/ручной очистки
 * после снятия и та же геометрия. Эпоха writer сюда намеренно не входит —
 * resumed без пропуска меняет её, не трогая буфер, и раньше блокировал
 * Older/Newer уведомлением «Источник истории изменился» без причины.
 * Маркер-якорь (isDisposed) и тип буфера по-прежнему проверяет вызывающий.
 */
export function continuationValid(doc: Pick<ReadDocument, "historySeq" | "geometryRevision">,
  live: { historySeq: number; geometryRevision: number }): boolean {
  return doc.historySeq === live.historySeq && doc.geometryRevision === live.geometryRevision;
}
export interface ReadLimits {
  rows: number;
  chars: number;
  /** A forward page starts here; an older page ends immediately before endRow. */
  firstRow?: number;
  endRow?: number;
  /** Mandatory row for the initial window (e.g. the touched visible row). */
  anchorRow?: number;
}

/** Call only at a TerminalWriter barrier. Snapshot data never points into xterm. */
export function captureReadDocument(buffer: IBuffer, cols: number, stamp: ReadStamp,
  limits: ReadLimits = { rows: 5000, chars: 1024 * 1024 }): ReadDocument {
  const count = Math.max(1, Math.floor(limits.rows)), budget = Math.max(0, Math.floor(limits.chars));
  const boundedRow = (row: number) => Math.max(0, Math.min(buffer.length, Math.floor(row)));
  const cache = new Map<number, { text: string; wrapped: boolean }>();
  const rowAt = (row: number) => {
    let value = cache.get(row);
    if (!value) {
      const line = buffer.getLine(row);
      // T-15: пустая ячейка перед переносом широкого символа — не пробел команды.
      value = { text: line ? logicalRowText(line, !!buffer.getLine(row + 1)?.isWrapped, cols) : "", wrapped: !!line?.isWrapped };
      cache.set(row, value);
    }
    return value;
  };
  let first = boundedRow(limits.endRow ?? limits.firstRow ?? Math.min(buffer.length - 1, limits.anchorRow ?? buffer.viewportY));
  let end = first, chars = 0, partial = false;
  const add = (direction: -1 | 1) => {
    const row = direction < 0 ? first - 1 : end;
    if (row < 0 || row >= buffer.length || end - first >= count) return false;
    const value = rowAt(row);
    const separator = end > first && !(direction < 0 ? rowAt(first) : value).wrapped ? 1 : 0;
    if (chars + separator + value.text.length > budget) {
      if (end > first) return false;
      // An exceptional single row can itself exceed the budget. Keep its
      // identity, include only complete cells and disclose the partial row.
      const line = buffer.getLine(row);
      let text = "";
      for (let col = 0; col < cols;) {
        const cell = line?.getCell(col), cellText = cell?.getChars() || " ";
        if (text.length + cellText.length > budget) break;
        text += cellText;
        col += cell?.getWidth() || 1;
      }
      value.text = text.slice(0, Math.min(text.length, value.text.length));
      partial = true;
    }
    chars += separator + value.text.length;
    if (direction < 0) first--; else end++;
    return true;
  };
  if (limits.endRow != null) {
    while (add(-1)) { /* The nearest older row is mandatory, then grow backwards. */ }
  } else if (limits.firstRow != null) {
    while (add(1)) { /* Forward continuation uses the actual previous end. */ }
  } else {
    const middleEnd = Math.min(buffer.length, first + Math.ceil(count / 2));
    while (end < middleEnd && add(1)) { /* Include the anchor before spending budget on older text. */ }
    while (add(-1)) { /* Balance around it, including near the end of scrollback. */ }
    while (add(1)) { /* Fill unused room when close to the beginning. */ }
  }
  const lines: ReadLine[] = [];
  let text = "";
  for (let row = first; row < end; row++) {
    const line = buffer.getLine(row);
    if (!line) break;
    const wrapped = row > first && line.isWrapped;
    const lineText = rowAt(row).text;
    const separator = lines.length && !wrapped ? "\n" : "";
    text += separator;
    const start = text.length;
    const columns: number[] = [];
    let columnOffset = 0;
    for (let col = 0; col < cols;) {
      const cell = line.getCell(col);
      const width = cell?.getWidth() || 1;
      for (let part = 0; part < width; part++) columns[col + part] = start + Math.min(lineText.length, columnOffset);
      columnOffset += (cell?.getChars() || " ").length;
      col += width;
    }
    columns[cols] = start + lineText.length;
    text += lineText;
    lines.push(Object.freeze({ text: lineText, start, end: text.length, wrapped, columns: Object.freeze(columns) }));
  }
  const { version: requestedVersion, historySeq, erasedBefore, retention, capacity, ringTruncatedBefore, ...base } = stamp;
  let version: number;
  if (requestedVersion != null && Number.isFinite(requestedVersion)) {
    version = requestedVersion;
    documentVersion = Math.max(documentVersion, requestedVersion);
  } else version = nextDocumentVersion();
  // Предел проверяем у нормального буфера: у alt-экрана scrollback нет вовсе.
  const evictedBefore = !!capacity && buffer.type !== "alternate"
    && buffer.length >= Math.max(0, capacity.scrollback) + Math.max(0, capacity.rows);
  // Тоже только у нормального буфера (волна 6): кадр без истории (hist_lines 0)
  // шлёт лишь зеркало в alt-screen, и чтение vim/htop/less показывало «более
  // ранний вывод сюда не пришёл» вместо «текст закреплён». Alt-экран своей
  // истории не имеет — ему нечего недополучить.
  return Object.freeze({ ...base, id: `${base.session}:${base.epoch}:${version}`, version,
    retention, historySeq: historySeq ?? 0, erasedBefore: !!erasedBefore, evictedBefore,
    ringTruncatedBefore: !!ringTruncatedBefore && buffer.type !== "alternate",
    partial: partial || undefined, capturedAt: Date.now(), source: buffer.type === "alternate" ? "screen" : "terminal",
    firstBufferRow: first, truncatedBefore: first > 0, truncatedAfter: first + lines.length < buffer.length,
    cols, text, lines: Object.freeze(lines) });
}

/** Visible glyph boxes use exactly the same columns as hit testing. */
export function documentRuns(line: ReadLine, firstColumn = 0, lastColumn = line.columns.length - 1) {
  const runs: { col: number; width: number; start: number; end: number; text: string }[] = [];
  let col = Math.max(0, Math.floor(firstColumn));
  while (col > 0 && line.columns[col] === line.columns[col - 1]) col--;
  while (col < Math.min(lastColumn, line.columns.length - 1)) {
    const start = line.columns[col];
    if (start >= line.end) break;
    let next = col + 1;
    while (next < line.columns.length - 1 && line.columns[next] === start) next++;
    const end = line.columns[next];
    runs.push({ col, width: next - col, start, end, text: line.text.slice(start - line.start, end - line.start) });
    col = next;
  }
  return runs;
}

export function documentCell(doc: ReadDocument, row: number, col: number): number {
  const line = doc.lines[Math.max(0, Math.min(doc.lines.length - 1, row))];
  return line ? line.columns[Math.max(0, Math.min(doc.cols, Math.floor(col)))] : 0;
}

export function documentPosition(doc: ReadDocument, offset: number): { row: number; col: number } {
  let lo = 0, hi = doc.lines.length - 1;
  while (lo < hi) {
    const mid = Math.ceil((lo + hi) / 2);
    if (doc.lines[mid].start <= offset) lo = mid; else hi = mid - 1;
  }
  const line = doc.lines[lo];
  if (!line) return { row: 0, col: 0 };
  const col = line.columns.findIndex(value => value >= offset);
  return { row: lo, col: col < 0 ? doc.cols : col };
}
