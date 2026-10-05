/**
 * Сравнитель терминального состояния для сквозной проверки раздела 6 плана
 * (путь A — непрерывный поток, путь B — снимок Go-зеркала + тот же хвост).
 *
 * Зачем. Проба кадра (build/qa/probe-screen-frame.mjs) сверяла ячейки, стили,
 * тип буфера, курсор и mouse/focus/bracketed — и только в конце потока. Замер
 * карты 05 показал 15 классов расхождений, которые она не видит вовсе:
 * отложенный перенос у края, видимость курсора, перо SGR, scroll region,
 * сохранённый курсор, normal-буфер под alt, DECCKM/DECKPAM/DECOM/DECAWM/IRM,
 * charset, табуляции, мягкие переносы, DEC 2026… Проверка «картинка совпала»
 * здесь ничего не доказывает: различие проявляется на СЛЕДУЮЩИХ байтах.
 *
 * Что внутри:
 *   - captureTerminalState — полный снимок состояния: сетка обоих буферов с
 *     шириной, стилями, ссылками и isWrapped, scrollback, курсоры, все
 *     term.modes, кодировка мыши, перо, charset, табуляции, scroll region,
 *     saved cursor, заголовок, палитра. Часть полей xterm не открывает
 *     публично — их читает узкий приватный адаптер, который ПАДАЕТ ЯВНО, если
 *     обновление xterm их переместило (иначе контракт молча выпал бы из
 *     проверки — тот же приём, что в probe-screen-frame.mjs:158-161);
 *   - diffTerminalState — различия по путям полей; объяснёнными считаются
 *     только те, что лежат в полях ОБЪЯВЛЕННОГО фикстурой разрыва;
 *   - EXPECTED_MIRROR_GAPS — реестр известных разрывов зеркала с причиной и
 *     ссылкой на план. Исчезнувший объявленный разрыв роняет тест (строгий
 *     xfail: исправление обязано быть замечено и снято из реестра);
 *   - generateStream/generateChunkPlan — воспроизводимая генерация (mulberry32)
 *     для больших потоков (T-27, раздел 6.2).
 *
 * Чистый модуль: xterm передаётся снаружи (headless в тестах), DOM и React не
 * нужны. Диагностика не содержит ничего, кроме того, что уже есть в фикстуре
 * (I-15: стенд работает на синтетике, не на выводе пользователя).
 */

// ─── Структурные типы: ровно то, что читаем у xterm ──────────────────────────

interface CellLike {
  getChars(): string;
  getWidth(): number;
  isBold(): number | boolean;
  isItalic(): number | boolean;
  isDim(): number | boolean;
  isUnderline(): number | boolean;
  isBlink(): number | boolean;
  isInverse(): number | boolean;
  isInvisible(): number | boolean;
  isStrikethrough(): number | boolean;
  getFgColorMode(): number;
  getFgColor(): number;
  getBgColorMode(): number;
  getBgColor(): number;
}

interface LineLike {
  readonly isWrapped: boolean;
  getCell(x: number): CellLike | undefined;
  translateToString(trimRight?: boolean): string;
}

interface BufferLike {
  readonly type: string;
  readonly cursorX: number;
  readonly cursorY: number;
  readonly baseY: number;
  readonly length: number;
  getLine(y: number): LineLike | undefined;
}

/** Всё, что сравнитель читает у терминала публично (headless Terminal подходит). */
export interface ConformanceTerminal {
  readonly cols: number;
  readonly rows: number;
  readonly modes: object;
  readonly buffer: { readonly active: BufferLike; readonly normal: BufferLike; readonly alternate: BufferLike };
}

// ─── Приватный адаптер ───────────────────────────────────────────────────────

interface PrivBuffer {
  scrollTop: number;
  scrollBottom: number;
  savedX: number;
  savedY: number;
  savedCurAttrData: { fg: number; bg: number };
  tabs: Record<string, boolean | undefined>;
}

interface PrivAttr {
  fg: number;
  bg: number;
  extended: { underlineStyle?: number; underlineColor?: number; urlId?: number };
}

interface PrivCore {
  buffers: { normal: PrivBuffer; alt: PrivBuffer };
  coreService: { isCursorHidden: boolean };
  coreMouseService: { _activeEncoding: string };
  _inputHandler: {
    _curAttrData: PrivAttr;
    _windowTitle: string;
    _parser: { currentState: number };
    _utf8Decoder: { interim: Uint8Array };
    onColor?: (listener: (ev: unknown) => void) => unknown;
  };
  _charsetService: { glevel: number; _charsets: (Record<string, string> | undefined)[] };
  _oscLinkService: { getLinkData(id: number): { uri?: string } | undefined };
}

const ADAPTER_FIELDS: [string, string][] = [
  ["buffers.normal.scrollTop", "number"],
  ["buffers.normal.scrollBottom", "number"],
  ["buffers.normal.savedX", "number"],
  ["buffers.normal.savedY", "number"],
  ["buffers.normal.savedCurAttrData.fg", "number"],
  ["buffers.normal.tabs", "object"],
  ["buffers.alt.scrollTop", "number"],
  ["buffers.alt.savedX", "number"],
  ["coreService.isCursorHidden", "boolean"],
  ["coreMouseService._activeEncoding", "string"],
  ["_inputHandler._curAttrData.fg", "number"],
  ["_inputHandler._curAttrData.bg", "number"],
  ["_inputHandler._curAttrData.extended", "object"],
  ["_inputHandler._windowTitle", "string"],
  ["_inputHandler._parser.currentState", "number"],
  ["_inputHandler._utf8Decoder.interim", "object"],
  ["_charsetService.glevel", "number"],
  ["_charsetService._charsets", "object"],
  ["_oscLinkService.getLinkData", "function"],
];

function readPath(root: unknown, path: string): unknown {
  let cur: unknown = root;
  for (const k of path.split(".")) {
    if (cur === null || cur === undefined) return undefined;
    cur = (cur as Record<string, unknown>)[k];
  }
  return cur;
}

/**
 * Приватное ядро xterm с проверкой КАЖДОГО поля, которое мы читаем. Поля
 * сверены на @xterm/headless 6.0.0; переехавшее поле — явная ошибка с его
 * путём, а не тихо выпавший из сравнения контракт.
 */
export function privateCore(term: ConformanceTerminal): PrivCore {
  const core = (term as unknown as { _core?: unknown })._core;
  const missing = ADAPTER_FIELDS.filter(([p, t]) => typeof readPath(core, p) !== t).map(([p]) => `_core.${p}`);
  if (!core || missing.length > 0) {
    throw new Error(
      `xterm private state unavailable (${missing.join(", ") || "_core"}); ` +
      "update terminalConformance adapter — verified on xterm 6.0.0",
    );
  }
  return core as PrivCore;
}

/**
 * Стоит ли парсер xterm на границе: ни незавершённой ESC/CSI/OSC/DCS, ни
 * недобранного UTF-8. Независимый от Go оракул для T-27: на разрезе, где
 * xterm не на границе, снимок обязан быть withheld.
 */
export function xtermParserAtGround(term: ConformanceTerminal): boolean {
  const ih = privateCore(term)._inputHandler;
  return ih._parser.currentState === 0 && ih._utf8Decoder.interim[0] === 0;
}

// ─── Наблюдатель палитры ─────────────────────────────────────────────────────

const palettes = new WeakMap<object, Map<number, string>>();

/**
 * Палитру OSC 4/10/11 headless не хранит (нет theme service): запросы уходят
 * событием onColor. Подписываемся ДО записи потока; неподписанный терминал
 * отдаёт palette=null и в этом поле не сравнивается.
 */
export function observeTerminal(term: ConformanceTerminal): void {
  if (palettes.has(term)) return;
  const ih = privateCore(term)._inputHandler;
  if (typeof ih.onColor !== "function") {
    throw new Error("xterm private _inputHandler.onColor unavailable; update terminalConformance adapter");
  }
  const map = new Map<number, string>();
  palettes.set(term, map);
  ih.onColor((ev: unknown) => {
    for (const req of Array.isArray(ev) ? ev : []) {
      const r = req as { type?: number; index?: number; color?: number[] };
      if (r.type === 1 && typeof r.index === "number") map.set(r.index, (r.color ?? []).join(","));
      else if (r.type === 2) {
        if (typeof r.index === "number") map.delete(r.index);
        else map.clear();
      }
    }
  });
}

// ─── Снимок состояния ────────────────────────────────────────────────────────

export interface CellState {
  chars: string;
  width: number;
  style: string;
  link: string;
}

export interface ScreenLineState {
  wrapped: boolean;
  cells: CellState[];
}

export interface ScrollbackLineState {
  text: string;
  wrapped: boolean;
  style: string;
}

export interface BufferState {
  cursorX: number;
  cursorY: number;
  scrollRegion: string;
  saved: string;
  tabs: string;
  scrollback: ScrollbackLineState[];
  screen: ScreenLineState[];
}

export interface TerminalState {
  cols: number;
  rows: number;
  active: string;
  cursorHidden: boolean;
  modes: Record<string, unknown>;
  mouseEncoding: string;
  pen: string;
  penLink: boolean;
  charset: string;
  title: string;
  palette: string | null;
  normal: BufferState;
  alternate: BufferState;
}

function flag(v: number | boolean): number {
  return v ? 1 : 0;
}

/** Стиль ячейки только через публичный IBufferCell (как probe-screen-frame.mjs:122-138). */
function cellStyle(cell: CellLike): string {
  const c = cell as CellLike & {
    isOverline?: () => number | boolean;
    getUnderlineStyle?: () => number;
    getUnderlineColorMode?: () => number;
    getUnderlineColor?: () => number;
  };
  return [
    flag(c.isBold()), flag(c.isItalic()), flag(c.isDim()), flag(c.isUnderline()), flag(c.isBlink()),
    flag(c.isInverse()), flag(c.isInvisible()), flag(c.isStrikethrough()),
    typeof c.isOverline === "function" ? flag(c.isOverline()) : 0,
    c.getFgColorMode(), c.getFgColor(), c.getBgColorMode(), c.getBgColor(),
    typeof c.getUnderlineStyle === "function" ? c.getUnderlineStyle() : 0,
    typeof c.getUnderlineColorMode === "function" ? c.getUnderlineColorMode() : 0,
    typeof c.getUnderlineColor === "function" ? c.getUnderlineColor() : 0,
  ].join(".");
}

function cellLink(core: PrivCore, cell: CellLike): string {
  // getCell в xterm 6 отдаёт сам CellData: urlId лежит в extended.
  const ext = (cell as unknown as { extended?: { urlId?: number } }).extended;
  if (!ext || typeof ext !== "object") {
    throw new Error("xterm private cell.extended unavailable; update terminalConformance adapter");
  }
  const id = ext.urlId ?? 0;
  if (!id) return "";
  return core._oscLinkService.getLinkData(id)?.uri ?? `#${id}`;
}

function lineStyle(line: LineLike, width: number): string {
  // Сжатая запись стилей строки: сериями «стиль×n».
  const out: string[] = [];
  let prev = "";
  let n = 0;
  for (let x = 0; x < width; x++) {
    const cell = line.getCell(x);
    const s = cell ? cellStyle(cell) : "-";
    if (s === prev) { n++; continue; }
    if (n > 0) out.push(`${prev}×${n}`);
    prev = s;
    n = 1;
  }
  if (n > 0) out.push(`${prev}×${n}`);
  return out.join("|");
}

function charsetName(c: Record<string, string> | undefined): string {
  if (!c) return "B";
  return Object.keys(c).sort().map((k) => `${k}${c[k]}`).join("");
}

function captureBuffer(term: ConformanceTerminal, core: PrivCore, which: "normal" | "alternate"): BufferState {
  const pub = which === "normal" ? term.buffer.normal : term.buffer.alternate;
  const priv = which === "normal" ? core.buffers.normal : core.buffers.alt;
  const scrollback: ScrollbackLineState[] = [];
  for (let y = 0; y < pub.baseY; y++) {
    const line = pub.getLine(y);
    // Хвостовые пробелы срезаются по той же причине, что "" ≡ " " в сетке:
    // стёртая ячейка с фоном у xterm и напечатанный историей пробел с тем же
    // фоном неотличимы (стиль сравнивается отдельно, сериями).
    scrollback.push(line
      ? { text: line.translateToString(true).replace(/ +$/, "").normalize("NFC"), wrapped: line.isWrapped, style: lineStyle(line, term.cols) }
      : { text: "", wrapped: false, style: "" });
  }
  const screen: ScreenLineState[] = [];
  for (let y = 0; y < term.rows; y++) {
    const line = pub.getLine(pub.baseY + y);
    if (!line) { screen.push({ wrapped: false, cells: [] }); continue; }
    const cells: CellState[] = [];
    for (let x = 0; x < term.cols; x++) {
      const cell = line.getCell(x);
      // Нетронутая ячейка ("") и напечатанный пробел (" ") с тем же стилем
      // на экране неотличимы, и сборщик кадра их сознательно не различает
      // (хвостовые пробелы без фона не печатает, renderCells). Та же
      // нормализация, что в probe-screen-frame.mjs (`getChars() || " "`);
      // продолжение широкой ячейки (ширина 0) остаётся "".
      cells.push(cell
        ? {
          // NFC: канонически эквивалентные строки Unicode — одна строка. xterm
          // из потока хранит NFD «é» как пришло, кадр зеркала отдаёт NFC
          // (screen.go composeForMirror, 14.09); на экране глиф один.
          chars: (cell.getWidth() === 0 ? cell.getChars() : (cell.getChars() || " ")).normalize("NFC"),
          width: cell.getWidth(),
          style: cellStyle(cell),
          link: cellLink(core, cell),
        }
        : { chars: " ", width: 1, style: "-", link: "" });
    }
    screen.push({ wrapped: line.isWrapped, cells });
  }
  // Только табуляции ВНУТРИ ширины: после сужения xterm оставляет в карте
  // стопы за краем (Buffer.resize → setupTabStops(newCols) только добавляет),
  // а nextStop до них не доходит; при расширении обратно setupTabStops ставит
  // их же заново. Невидимое различие — как "" ≡ " " в сетке.
  const tabs = Object.keys(priv.tabs).filter((k) => priv.tabs[k]).map(Number).filter((x) => x < term.cols).sort((a, b) => a - b).join(",");
  return {
    cursorX: pub.cursorX,
    cursorY: pub.cursorY,
    scrollRegion: `${priv.scrollTop}-${priv.scrollBottom}`,
    // savedY у xterm АБСОЛЮТНЫЙ (ybase + y), а DECRC ставит курсор в
    // savedY − ybase (InputHandler.restoreCursor). Сравниваем то, что увидит
    // DECRC: иначе одинаковый сохранённый курсор при разной длине истории
    // (история зеркала, 500 строк, DL-утечка vt) читался бы как расхождение.
    saved: `${priv.savedX},${Math.max(priv.savedY - pub.baseY, 0)}:${priv.savedCurAttrData.fg}.${priv.savedCurAttrData.bg}`,
    tabs,
    scrollback,
    screen,
  };
}

export function captureTerminalState(term: ConformanceTerminal): TerminalState {
  const core = privateCore(term);
  const attr = core._inputHandler._curAttrData;
  const modes: Record<string, unknown> = {};
  for (const [k, v] of Object.entries(term.modes)) modes[k] = v;
  const cs = core._charsetService;
  const palette = palettes.get(term);
  return {
    cols: term.cols,
    rows: term.rows,
    active: term.buffer.active.type,
    cursorHidden: core.coreService.isCursorHidden,
    modes,
    mouseEncoding: core.coreMouseService._activeEncoding,
    pen: `${attr.fg}.${attr.bg}.${attr.extended.underlineStyle ?? 0}.${attr.extended.underlineColor ?? 0}`,
    penLink: (attr.extended.urlId ?? 0) > 0,
    charset: `gl${cs.glevel}:` + [0, 1, 2, 3].map((i) => charsetName(cs._charsets[i])).join("/"),
    title: core._inputHandler._windowTitle,
    palette: palette ? [...palette.entries()].sort((a, b) => a[0] - b[0]).map(([i, c]) => `${i}=${c}`).join(";") : null,
    normal: captureBuffer(term, core, "normal"),
    alternate: captureBuffer(term, core, "alternate"),
  };
}

// ─── Различия ────────────────────────────────────────────────────────────────

export interface StateDiff {
  path: string;
  a: unknown;
  b: unknown;
  /** Значение самого зеркала в этой клетке (строгая фаза снимка), для сообщения. */
  mirror?: unknown;
}

function pushIf(out: StateDiff[], path: string, a: unknown, b: unknown): void {
  if (a !== b) out.push({ path, a, b });
}

function diffBuffer(out: StateDiff[], name: string, a: BufferState, b: BufferState): void {
  pushIf(out, `${name}.cursor.x`, a.cursorX, b.cursorX);
  pushIf(out, `${name}.cursor.y`, a.cursorY, b.cursorY);
  pushIf(out, `${name}.scrollRegion`, a.scrollRegion, b.scrollRegion);
  pushIf(out, `${name}.saved`, a.saved, b.saved);
  pushIf(out, `${name}.tabs`, a.tabs, b.tabs);
  // Scrollback выравниваем по КОНЦУ: обе истории упираются в верх экрана, а
  // потолки у сторон разные (10000 у клиента, 500 у зеркала).
  pushIf(out, `${name}.scrollback.length`, a.scrollback.length, b.scrollback.length);
  const n = Math.min(a.scrollback.length, b.scrollback.length);
  for (let k = 1; k <= n; k++) {
    const la = a.scrollback[a.scrollback.length - k];
    const lb = b.scrollback[b.scrollback.length - k];
    pushIf(out, `${name}.scrollback[-${k}].text`, la.text, lb.text);
    pushIf(out, `${name}.scrollback[-${k}].wrapped`, la.wrapped, lb.wrapped);
    pushIf(out, `${name}.scrollback[-${k}].style`, la.style, lb.style);
  }
  const rows = Math.max(a.screen.length, b.screen.length);
  for (let y = 0; y < rows; y++) {
    const la = a.screen[y] ?? { wrapped: false, cells: [] };
    const lb = b.screen[y] ?? { wrapped: false, cells: [] };
    pushIf(out, `${name}.screen[${y}].wrapped`, la.wrapped, lb.wrapped);
    const cols = Math.max(la.cells.length, lb.cells.length);
    for (let x = 0; x < cols; x++) {
      const ca = la.cells[x];
      const cb = lb.cells[x];
      if (!ca || !cb) {
        pushIf(out, `${name}.screen[${y}][${x}].chars`, ca?.chars, cb?.chars);
        continue;
      }
      pushIf(out, `${name}.screen[${y}][${x}].chars`, ca.chars, cb.chars);
      pushIf(out, `${name}.screen[${y}][${x}].width`, ca.width, cb.width);
      pushIf(out, `${name}.screen[${y}][${x}].style`, ca.style, cb.style);
      pushIf(out, `${name}.screen[${y}][${x}].link`, ca.link, cb.link);
    }
  }
}

/** Все различия двух снимков, по путям полей. */
export function rawStateDiffs(a: TerminalState, b: TerminalState): StateDiff[] {
  const out: StateDiff[] = [];
  pushIf(out, "cols", a.cols, b.cols);
  pushIf(out, "rows", a.rows, b.rows);
  pushIf(out, "active", a.active, b.active);
  pushIf(out, "cursorHidden", a.cursorHidden, b.cursorHidden);
  for (const k of new Set([...Object.keys(a.modes), ...Object.keys(b.modes)])) {
    pushIf(out, `modes.${k}`, a.modes[k], b.modes[k]);
  }
  pushIf(out, "mouseEncoding", a.mouseEncoding, b.mouseEncoding);
  pushIf(out, "pen", a.pen, b.pen);
  pushIf(out, "penLink", a.penLink, b.penLink);
  pushIf(out, "charset", a.charset, b.charset);
  pushIf(out, "title", a.title, b.title);
  if (a.palette !== null && b.palette !== null) pushIf(out, "palette", a.palette, b.palette);
  diffBuffer(out, "normal", a.normal, b.normal);
  diffBuffer(out, "alternate", a.alternate, b.alternate);
  return out;
}

// ─── Реестр ожидаемых разрывов зеркала ───────────────────────────────────────

export interface MirrorGap {
  id: string;
  /** Поля-доказательства: различие здесь и есть сам разрыв. */
  fields: readonly string[];
  /**
   * Поля-последствия: после ХВОСТА разрыв законно проявляется и в них
   * (перенос не туда → другие ячейки). В момент снимка не допускаются, и
   * доказательством разрыва не считаются.
   */
  consequences: readonly string[];
  reason: string;
  planRef: string;
}

const SCREEN = ["*.screen*", "*.cursor.*", "*.scrollback*"];
// Последствия после хвоста: если в хвосте есть DECSC, сохранённый курсор
// запоминает уже разошедшуюся позицию (замер строгого C-03, seed 20260913).
const SCREEN_TAIL = [...SCREEN, "*.saved"];

/**
 * Известные разрывы: что кадр Go-зеркала НЕ переносит клиенту. Каждая запись
 * привязана к фикстуре-доказательству (snapshotConformance.test.ts) и к
 * синтетическому воспроизведению (terminalConformance.test.ts); исчезнувший
 * разрыв роняет тест, пока запись не снимут.
 *
 * СНЯТЫ 14.09.2026 (ST-05, «дёшево, силами зеркала»; трекер
 * internal/pty/screen_modes.go): cursor-visibility (?25), decckm (?1),
 * deckpam (?66/ESC =), decom (?6), decawm (?7), irm (флаг IRM). Кадр их теперь
 * переносит; строгий xfail это показал (шесть фикстур упали с «объявленный
 * разрыв исчез»), фикстуры остались положительными регрессиями без разрывов.
 */
export const EXPECTED_MIRROR_GAPS: readonly MirrorGap[] = [
  {
    id: "pending-wrap",
    fields: ["*.cursor.x"],
    consequences: SCREEN_TAIL,
    reason: "vt хранит отложенный перенос во внутреннем atPhantom без API; кадр ставит курсор абсолютным CUP в последнюю колонку, и следующий символ перезаписывает её вместо переноса",
    planRef: "ST-05 (дорого: API/форк vt); §6.1",
  },
  {
    id: "sgr-pen",
    fields: ["pen"],
    // *.saved — DECSC в хвосте запоминает перо (seed 20260914).
    consequences: ["*.screen*.style", "*.scrollback*.style", "*.saved"],
    reason: "кадр закрывается ESC[m; текущее перо приложения vt наружу не отдаёт (Screen.cur.Pen не экспортирован)",
    planRef: "ST-05 (дорого); §6.1",
  },
  {
    id: "scroll-region",
    fields: ["*.scrollRegion"],
    consequences: SCREEN_TAIL,
    reason: "DECSTBM живёт в Screen.scroll vt без публичного API; кадр region не восстанавливает",
    planRef: "ST-05 (дорого); §6.1",
  },
  {
    id: "saved-cursor",
    fields: ["*.saved"],
    consequences: SCREEN_TAIL,
    reason: "DECSC хранится в Screen.saved vt без API; после DECRC клиент встаёт в (0,0)",
    planRef: "ST-05 (дорого); §6.1",
  },
  {
    id: "alt-underlying-normal",
    fields: ["normal.*"],
    consequences: ["normal.*", "cursorHidden"],
    reason: "в alt-экране кадр рисует только alt, история пуста (historyLocked); normal-буфер и его курсор под alt у клиента пустые до выхода из alt",
    planRef: "ST-05 (дорого); §6.1",
  },
  {
    id: "charset",
    fields: ["charset"],
    consequences: ["*.screen*"],
    reason: "назначение G0–G3 (ESC ( 0 …) vt держит в неэкспортированных charsets; кадр печатает уже переведённые символы, но выбор набора не переносит",
    planRef: "ST-05 (дорого); §6.1",
  },
  {
    id: "tabstops",
    fields: ["*.tabs"],
    consequences: SCREEN_TAIL,
    reason: "HTS/TBC меняют табуляции vt без API; клиент после кадра на табуляциях по умолчанию",
    planRef: "ST-05 (дорого); §6.1",
  },
  {
    id: "soft-wrap-flags",
    fields: ["*.screen*.wrapped", "*.scrollback*.wrapped"],
    consequences: ["*.screen*.wrapped", "*.scrollback*.wrapped"],
    reason: "кадр адресует строки абсолютным CUP, история склеена \\r\\n: признак мягкого переноса (isWrapped) теряется — чтение и reflow видят две строки вместо одной",
    planRef: "ST-05; §6.1 (мягкие переносы)",
  },
  {
    id: "sync-2026",
    fields: ["modes.synchronizedOutputMode"],
    consequences: [],
    reason: "DEC 2026 vt не знает; снимок внутри открытого блока теряет флаг (конечный показ и таймаут 1 с — уровень L3)",
    planRef: "T-29; §6.2",
  },
  {
    id: "osc8",
    // style — потому что xterm отдаёт ячейку со ссылкой как подчёркнутую
    // (isUnderline() = 1 при urlId): замер C-05. pen — открытая ссылка
    // меняет перо xterm (extended-флаг и стиль подчёркивания): замер C-02.
    fields: ["*.screen*.link", "*.screen*.style", "penLink", "pen"],
    consequences: ["*.screen*.link", "*.screen*.style"],
    reason: "гиперссылки OSC 8 библиотека искажает, в кадр они не попадают (текст остаётся, ссылка теряется) — screen.go шапка",
    planRef: "§6.1; screen.go:39-42",
  },
  {
    id: "title-palette",
    fields: ["title", "palette"],
    consequences: [],
    reason: "заголовок (OSC 0/2) и палитра (OSC 4/10/11) в кадр не входят",
    planRef: "§6.1",
  },
  {
    id: "region-scrollback-leak",
    fields: ["normal.scrollback*"],
    consequences: ["normal.scrollback*"],
    reason: "vt кладёт в scrollback строки, ушедшие из области DECSTBM или удалённые DL; xterm — нет (TestScreenMirrorHistoryRegionScrollLeakIsKnown)",
    planRef: "§6.1; screen.go:617-624",
  },
  {
    id: "unicode-width-v6-vs-grapheme",
    fields: SCREEN,
    consequences: SCREEN_TAIL,
    reason: "клиент на встроенной таблице Unicode 6 (😀 шириной 1), vt — GraphemeWidth (шириной 2); сверяется только поклеточным правилом C==A или C==B",
    planRef: "§6.1; terminalEmulation.ts",
  },
  {
    id: "scrollback-cap-500",
    fields: ["normal.scrollback.length"],
    consequences: [],
    reason: "потолок истории зеркала 500 строк (screenMirrorScrollback), у клиента 10000",
    planRef: "§6.1; screen.go:44-61",
  },
  {
    id: "retention-ed3",
    fields: ["normal.scrollback*"],
    consequences: [],
    reason: "RetentionPolicy клиента (keepHistory) вырезает или откладывает ESC[3J — намеренно сохранённый архив отличается от чистого scrollback",
    planRef: "§6.1 (RetentionPolicy); ST-04",
  },
  // ── Расхождения самой библиотеки vt с xterm (найдены генератором C-03;
  // минимальные входы — в отчёте стенда). Не дефект сборщика кадра: зеркало
  // уже на префиксе держит другую сетку. Лечатся апстримом/форком vt или
  // подменой в зеркале (как rewriteEraseAll для ED2).
  {
    id: "alt-enter-cursor-home",
    fields: ["alternate.cursor.*", "alternate.screen*"],
    consequences: SCREEN_TAIL,
    reason: "vt при входе в alt (setAltScreenMode) ставит курсор в (0,0), xterm сохраняет позицию: текст после ?1049h без CUP ложится у зеркала в угол",
    planRef: "ST-05 (библиотека); найдено стендом C-02/C-03",
  },
  // ed3-vt-clears-screen снят 14.09: зеркало обрабатывает ED3 само, как xterm
  // (только история активного буфера; в alt — ничего), screen.go newScreenMirror.
  {
    id: "decom-vt-cursor",
    // *.scrollback* (SCREEN) с 15.09: текст не на своей строке и прокручивается
    // иначе — у зеркала в историю уходят строки, которых у xterm там нет
    // (C-03R seed 20261102: ?6h после CUP 5;1 и три строки — история 2 против 0).
    fields: SCREEN,
    consequences: SCREEN_TAIL,
    reason: "xterm на DECSET/DECRST 6 переводит курсор в начало области, vt — нет; текст после ?6h/?6l без CUP у зеркала в другом месте",
    planRef: "ST-05 (библиотека); найдено стендом C-03",
  },
  {
    id: "vt-combining-split",
    // Знак в своей клетке сдвигает и курсор vt на колонку (строгий C-03).
    fields: ["*.screen*", "*.cursor.*"],
    consequences: SCREEN_TAIL,
    reason: "vt пишет несамостоятельный знак (Mn) в клетку ПОД КУРСОРОМ и курсор не двигает, xterm приклеивает его к клетке слева: при печати следующий символ затирает знак, при затирании знак встаёт на место чужой буквы. Составимые пары (e + U+0301, й, ё) зеркало с 14.09 собирает NFC на входе (screen.go composeForMirror) — разрыв остался для знаков без составной формы и для разреза ровно между базой и знаком",
    planRef: "ST-05 (библиотека); найдено стендом C-03",
  },
  {
    id: "dec-graphics-glyphs",
    fields: ["*.screen*"],
    consequences: SCREEN_TAIL,
    reason: "таблицы DEC Special Graphics у vt и xterm расходятся в отдельных глифах (z: ⩾ у vt, ≥ у xterm)",
    planRef: "ST-05 (библиотека); найдено стендом C-03",
  },
  {
    id: "vt-ich-dch-region",
    fields: ["*.screen*"],
    consequences: SCREEN_TAIL,
    reason: "vt не выполняет ICH/DCH, если курсор вне области DECSTBM (InsertCellArea по s.scroll); xterm ограничивает их только левым/правым полем",
    planRef: "ST-05 (библиотека); найдено стендом C-03",
  },
  {
    id: "vt-phantom-after-edit",
    fields: SCREEN,
    consequences: SCREEN_TAIL,
    reason: "после печати до края vt сохраняет отложенный перенос через IL/DL/ICH/DCH, xterm его снимает: следующий символ у зеркала уходит на строку ниже",
    planRef: "ST-05 (библиотека); найдено стендом C-03",
  },
  // mouse-reset-enum и decstr-dec-tracker сняты 14.09: decTracker гасит
  // семейство трекинга мыши на DECRST любого члена, не отслеживает 1005/1015
  // (xterm их не поддерживает) и знает DECSTR (decmodes.go, decmode_test.go).
  {
    id: "xterm-ris-keeps-cursor-hidden",
    fields: ["cursorHidden"],
    consequences: [],
    reason: "xterm.js 6.0.0 после RIS (ESC c) оставляет курсор скрытым, если до сброса был ?25l; vt по RIS курсор показывает (как VT и настоящий xterm). Особенность клиента, не зеркала",
    planRef: "§6.1; найдено стендом C-03 (seed 20260919)",
  },
  {
    id: "irm-vt-print",
    fields: ["*.screen*"],
    consequences: SCREEN_TAIL,
    reason: "vt не реализует вставку IRM при печати (handleGrapheme перезаписывает): текст, напечатанный в режиме вставки ДО снимка, у зеркала другой",
    planRef: "ST-05 (библиотека); найдено стендом",
  },
  {
    id: "vt-decstr-ignored",
    // *.screen*.style отдельно от широкого *.screen*: в строгой фазе снимка
    // широкие поля сами не засчитываются (diffSnapshotAgainstMirror), а перо
    // vt после DECSTR красит клетки, СИМВОЛЫ которых совпадают с xterm, —
    // правило «клиент = зеркало» по символам такую клетку не оправдает.
    fields: [...SCREEN, "*.screen*.style", "*.scrollback*.style"],
    consequences: SCREEN_TAIL,
    reason: "vt не обрабатывает DECSTR вовсе: область DECSTBM, DECOM, DECAWM, перо, сохранённый курсор и charset у зеркала переживают мягкий сброс, и текст после него ложится и красится иначе, чем в xterm",
    planRef: "ST-05 (дёшево: подмена DECSTR в зеркале, как rewriteEraseAll); найдено ревью C-conformance",
  },
  // ── Найдены строгой фазой снимка C-03 (клиент = xterm ИЛИ зеркало) после
  // ревью: прежде их прятали широкие поля разрывов, объявленных по всему потоку.
  {
    id: "vt-ed1-whole-line",
    // *.screen*.style — лишние стёртые клетки получают фон пера (BCE): символ
    // тот же пробел, отличается только фон (seed 20260920: ESC[42m ESC[1J).
    fields: ["*.screen*", "*.screen*.style"],
    consequences: SCREEN_TAIL,
    reason: "vt на ED1 (ESC[1J) стирает строку курсора ЦЕЛИКОМ (FillArea по Rect(0,0,width,y+1)), xterm — только до курсора включительно: текст правее курсора у зеркала пропадает",
    planRef: "ST-05 (дёшево: подмена ED1 в зеркале, как rewriteEraseAll; или апстрим vt); найдено строгим C-03 (seed 20260920)",
  },
  {
    id: "vt-1049l-no-restore",
    fields: [...SCREEN, "*.screen*.style", "*.scrollback*.style"],
    consequences: SCREEN_TAIL,
    reason: "xterm на ?1049l выполняет DECRC (позиция, перо, charset) — и вне alt тоже; vt на сброс 1049 курсор не восстанавливает вовсе (csi_mode.go: saveCursor только на set, setAltScreenMode вне alt — выход): текст после ?1049l у зеркала другим пером и в другом месте",
    planRef: "ST-05 (библиотека); найдено строгим C-03 (seed 20260917)",
  },
  {
    id: "frame-orphan-combining",
    fields: ["*.screen*"],
    consequences: SCREEN_TAIL,
    reason: "ДЕФЕКТ СБОРЩИКА КАДРА, не библиотеки: клетку vt, которая начинается с комбинирующего знака (vt-combining-split; NFD «é» — так пишет имена файлов macOS), renderCells печатает как есть, xterm клиента приклеивает знак к клетке слева, и остаток строки у клиента съезжает на колонку влево. Строгая фаза снимка допускает это ТОЛЬКО в строке с такой клеткой зеркала и только от неё вправо",
    planRef: "ST-05; для составимых пар снято NFC на входе зеркала (14.09). Пробел или колонка после сироты в renderCells НЕ годятся: проверено, ломают последовательную печать (seed 20260919, история длиннее на две строки); найдено строгим C-03",
  },
  // ── Смена геометрии (resize) — §6, ST-05 после финального ревью: стенд не
  // менял геометрию ни до, ни во время, ни после снимка. Геометрия меняется у
  // всех сторон в одной точке потока (term.resize и Resize зеркала по маркеру
  // ACK); ниже — чем стороны расходятся. Фикстуры — resize-* и alt-resize-*
  // (snapshotConformance.test.ts), генерация — C-03R.
  // *.screen*.style и *.scrollback*.style отдельно от широкого *.screen* (как у
  // vt-decstr-ignored): фон пера (BCE) у xterm едет вместе с перенесённым
  // текстом, а у vt остаётся на обрезанной строке и на новых строках его нет —
  // различаются только стили ПРОБЕЛОВ, символы совпадают, и правило «клиент =
  // зеркало» по символам такую клетку не оправдает (seed 20261006, разрез 195:
  // ESC[42m, строки по 19 символов, resize 16×10).
  {
    id: "vt-resize-no-reflow",
    fields: [...SCREEN, "*.screen*.style", "*.scrollback*.style"],
    consequences: SCREEN_TAIL,
    reason: "xterm на смене ширины переносит (reflow) строки normal-буфера и его историю: при сужении режет длинную строку на мягко перенесённые, при расширении склеивает перенесённые обратно, и курсор едет вместе с текстом. vt строки обрезает по новой ширине (uv.Buffer.Resize: Lines[i][:width]), обрезанное не возвращается, перенесённые не склеиваются, история не трогается",
    planRef: "ST-05 (библиотека: reflow в vt/ultraviolet); найдено стендом resize (фикстуры resize-before-cols-*)",
  },
  {
    id: "vt-resize-height",
    fields: [...SCREEN, "*.screen*.style", "*.scrollback*.style"],
    consequences: SCREEN_TAIL,
    reason: "xterm при уменьшении высоты сначала убирает строки НИЖЕ курсора, остальное прокручивает вверх — в историю (в alt — отбрасывает верхние), курсор остаётся на своей строке текста; при увеличении с курсором на последней строке возвращает строки из истории. vt всегда отрезает НИЖНИЕ строки и прижимает курсор к новому низу, при увеличении добавляет пустые снизу; историю не трогает. Телефон: клавиатура поднялась — у зеркала верх экрана, у xterm низ с приглашением",
    planRef: "ST-05 (библиотека: uv.Buffer.Resize); найдено стендом resize (resize-before-rows-*, alt-resize-before-shrink)",
  },
  {
    id: "frame-reflow-without-wrap",
    fields: SCREEN,
    consequences: SCREEN_TAIL,
    reason: "resize ПОСЛЕ снимка: клиент перестраивает (reflow) экран, восстановленный из кадра, а в кадре нет признаков мягкого переноса (soft-wrap-flags: строки адресованы CUP, история склеена \\r\\n; у vt признака нет вовсе) — склеенная у непрерывного xterm строка у клиента остаётся двумя и режется иначе. Ограничение формата кадра, не библиотеки",
    planRef: "ST-05 (кадр: soft-wrap-flags; требует признака переноса в vt); найдено стендом resize (resize-between-*, resize-after-tail-*)",
  },
  {
    id: "frame-wide-last-column",
    fields: ["*.screen*"],
    consequences: ["*.screen*"],
    reason: "ДЕФЕКТ КАДРА, смягчён 15.09: после сужения vt и xterm оставляют широкую графему в последней колонке без второй половины; кадр напечатать её так не может (клиент переносит её на следующую строку — там оставался мусор, пустые строки кадр пропускает). Теперь кадр её не печатает (screen.go renderLine): у клиента в этой клетке пусто. Строгая фаза снимка допускает только эту клетку: последняя колонка, у xterm ширина 2, у зеркала та же графема, у клиента пробел",
    planRef: "ST-05; найдено стендом C-03R (seed 20261123), фикстура resize-wide-last-column, TestScreenFrameSkipsWideGraphemeCutAtRightEdge",
  },
  // vt-ris-keeps-scrollback снят 15.09: зеркало на RIS стирает и историю, как
  // xterm (internal/pty/screen_vtpanic.go, installEmulatorLocked); фикстура
  // ris-keeps-history — положительная регрессия.
  //
  // Отказ после паники vt (снимок untrusted до RIS, screen_vtpanic.go) —
  // НЕ разрыв этого реестра: снимка нет вовсе, сравнивать нечего. Стенд
  // считает такие отказы в сводке C-03 и проверяет сам путь в C-08.
  //
  // history-seam-wide-lines снят 15.09: цель досылки после истории — все её
  // РЯДЫ, а не число строк (snapshotApply.snapshotStepPayload), и строка
  // истории шире клиента больше не теряется на шве. Фикстура
  // resize-history-wider-than-client — положительная регрессия; любая
  // потеря истории на шве снова необъяснима.
  {
    id: "xterm-alt-resize-stale-length",
    fields: ["alternate.scrollback.length", "alternate.scrollback[*"],
    consequences: ["alternate.scrollback.length", "alternate.scrollback[*"],
    reason: "xterm.js 6.0.0: пустой alt-буфер при resize сохраняет прежнюю длину (Buffer.resize сокращает lines.maxLength только у непустого буфера), и после уменьшения высоты прокрутка в alt копит у непрерывного клиента «историю» alt до разницы высот; свежий клиент в той же геометрии её не копит. Особенность клиента, не зеркала",
    planRef: "§6.1; найдено стендом C-03R (seed 20261008)",
  },
];

const ORPHAN_GAP = "frame-orphan-combining";
const WIDE_GAP = "frame-wide-last-column";

const gapById = new Map(EXPECTED_MIRROR_GAPS.map((g) => [g.id, g]));

export function mirrorGap(id: string): MirrorGap {
  const g = gapById.get(id);
  if (!g) throw new Error(`неизвестный разрыв ${id}: нет в EXPECTED_MIRROR_GAPS`);
  return g;
}

function globToRegExp(pattern: string): RegExp {
  const esc = pattern.replace(/[.+?^${}()|[\]\\]/g, "\\$&").replace(/\*/g, ".*");
  return new RegExp(`^${esc}$`);
}

const globCache = new Map<string, RegExp>();
function matchField(pattern: string, path: string): boolean {
  let re = globCache.get(pattern);
  if (!re) {
    re = globToRegExp(pattern);
    globCache.set(pattern, re);
  }
  return re.test(path);
}

export type ConformancePhase = "snapshot" | "tail";

export interface ConformanceResult {
  /** Различия, которые не объясняет ни один объявленный разрыв. */
  unexplained: StateDiff[];
  /** Доказательства по разрывам: различия в полях `fields` каждого объявленного. */
  evidence: Map<string, StateDiff[]>;
  all: StateDiff[];
}

/**
 * Различия a и b с учётом объявленных фикстурой разрывов. Различие
 * объяснено, только если лежит в `fields` объявленного разрыва (или, после
 * хвоста, в его `consequences`). Необъявленный разрыв не объясняет ничего.
 */
export function diffTerminalState(
  a: TerminalState,
  b: TerminalState,
  allowedGaps: readonly string[] = [],
  phase: ConformancePhase = "snapshot",
): ConformanceResult {
  const gaps = allowedGaps.map(mirrorGap);
  const all = rawStateDiffs(a, b);
  const evidence = new Map<string, StateDiff[]>(gaps.map((g) => [g.id, []]));
  const unexplained: StateDiff[] = [];
  for (const d of all) {
    let explained = false;
    for (const g of gaps) {
      if (g.fields.some((f) => matchField(f, d.path))) {
        evidence.get(g.id)!.push(d);
        explained = true;
      }
    }
    if (!explained && phase === "tail") {
      explained = gaps.some((g) => g.consequences.some((f) => matchField(f, d.path)));
    }
    if (!explained) unexplained.push(d);
  }
  return { unexplained, evidence, all };
}

/**
 * Строгий xfail: объявленные разрывы, которые НЕ проявились ни в одной из
 * фаз. Непустой результат — разрыв исчез (починили зеркало или сломали
 * фикстуру), и запись обязана быть снята.
 */
export function vanishedGaps(declared: readonly string[], results: readonly ConformanceResult[]): string[] {
  return declared.filter((id) => results.every((r) => (r.evidence.get(id)?.length ?? 0) === 0));
}

/** Короткое описание различий для сообщения теста (без потока — только поля). */
export function describeDiffs(diffs: readonly StateDiff[], limit = 8): string {
  const head = diffs.slice(0, limit).map((d) => `${d.path}: ${JSON.stringify(d.a)} ≠ ${JSON.stringify(d.b)}` +
    (d.mirror === undefined ? "" : ` (зеркало ${JSON.stringify(d.mirror)})`));
  if (diffs.length > limit) head.push(`… ещё ${diffs.length - limit}`);
  return head.join("\n");
}

/**
 * Сетка активного экрана в представлении probe-screen-frame.mjs: продолжение
 * широкой ячейки — "", пустая — " ". Для правила C==A или C==B.
 */
export function activeGrid(s: TerminalState): string[][] {
  const buf = s.active === "alternate" ? s.alternate : s.normal;
  return buf.screen.map((l) => l.cells.map((c) => (c.width === 0 ? "" : (c.chars || " "))));
}

/**
 * Поклеточное правило Unicode (probe-screen-frame.mjs:109-120): клетка
 * восстановленного терминала обязана совпасть с эталоном xterm ИЛИ с сеткой
 * Go-зеркала. Новое третье значение — дефект сборщика кадра.
 */
export function cellRuleMismatches(raw: string[][], mirror: string[][], got: string[][]): { y: number; x: number; raw: string; mirror: string; got: string }[] {
  const bad: { y: number; x: number; raw: string; mirror: string; got: string }[] = [];
  for (let y = 0; y < got.length; y++) {
    for (let x = 0; x < got[y].length; x++) {
      const r = raw[y]?.[x] ?? " ";
      const m = mirror[y]?.[x] ?? " ";
      const g = got[y][x];
      if (g !== r && g !== m) bad.push({ y, x, raw: r, mirror: m, got: g });
    }
  }
  return bad;
}

// ─── Строгая фаза снимка: клиент против xterm И против самого зеркала ────────

/**
 * Состояние самого Go-зеркала на разрезе (StreamSnapshot: grid, cursorX,
 * cursorY, alt, scrollback) — третья сторона сравнения в момент снимка.
 */
export interface MirrorView {
  /** Активный экран vt в форме activeGrid: пустая клетка " ", продолжение широкой "". */
  grid: string[][];
  cursorX: number;
  cursorY: number;
  alt: boolean;
  /** Строки истории, ушедшие в снимок (старые → новые), без хвостовых пробелов. */
  scrollback: string[];
  /**
   * Ширина каждой строки истории в колонках, как её напечатает клиент (пробелы
   * с фоном — в счёт): строка шире клиента займёт у него несколько рядов.
   * Сравнитель её не читает с 15.09 (прогноз потери на шве снят вместе с
   * разрывом history-seam-wide-lines); поле оставлено как вывод инструмента.
   */
  scrollbackCells?: number[];
}

/**
 * Широкое поле закрывает целиком сетку, историю, курсор или буфер. В строгой
 * фазе снимка оно само ничего не объясняет: объявленный где-то в потоке
 * библиотечный разрыв с полем *.screen* иначе прятал бы любую клетку (ревью
 * C-conformance: разница в одну букву проходила C-03 в 8 seed из 8). Курсор
 * (включая одиночный *.cursor.x у pending-wrap) — тоже: WRAP объявлен почти
 * на каждом разрезе, и узкий курсор прятал бы любой промах кадра по колонке.
 */
export function isBroadField(pattern: string): boolean {
  return /^(\*|normal|alternate)\.(screen\*|scrollback\*|cursor\.[*xy]|\*)$/.test(pattern);
}

/**
 * Логические строки истории: мягко перенесённое продолжение (isWrapped)
 * приклеено к предыдущей строке, а та дополнена до ширины — пробелы на месте
 * переноса срезаны при снятии (captureBuffer). Потерянное или изменённое
 * продолжение даёт другую строку, то есть не совпадёт с зеркалом.
 */
export function logicalHistory(rows: readonly ScrollbackLineState[], cols: number): string[] {
  const groups: string[][] = [];
  for (const r of rows) {
    if (r.wrapped && groups.length > 0) groups[groups.length - 1].push(r.text);
    else groups.push([r.text]);
  }
  return groups.map((g) => g.map((t, i) => (i < g.length - 1 ? t.padEnd(cols) : t)).join("").replace(/ +$/, ""));
}

const CELL_PATH = /^(normal|alternate)\.screen\[(\d+)\]\[(\d+)\]\.(chars|width|style|link)$/;
const CURSOR_PATH = /^(normal|alternate)\.cursor\.[xy]$/;

/**
 * Различия в МОМЕНТ СНИМКА по строгому правилу: A — непрерывный xterm, B —
 * клиент после снимка, M — само зеркало (vt) на том же разрезе.
 *
 *   - узкие поля объявленных разрывов (pen, modes.*, *.scrollRegion,
 *     *.screen*.wrapped…) объясняют как в diffTerminalState;
 *   - широкие поля объясняют различие, только если B взял значение у
 *     зеркала: клетка активного экрана B == M ≠ A (тогда и её стиль, ширина и
 *     ссылка — от зеркала), курсор B == курсор M, история B == история M
 *     построчно. Значит, расходится сама библиотека, а кадр перенёс её
 *     состояние честно. Третье значение (B ≠ A и B ≠ M) — дефект сборщика
 *     кадра, и его не прячет никакой разрыв (правило C==A или C==B,
 *     probe-screen-frame.mjs:109-120, теперь на всём корпусе);
 *   - normal.* под alt-экраном объясняет только поле "normal.*" (разрыв
 *     alt-underlying-normal): normal-буфер клиенту не виден и кадром не
 *     переносится. На normal-экране то же поле не объясняет ничего;
 *   - единственное допущенное третье значение — известный дефект сборщика
 *     frame-orphan-combining: только в строке, где у зеркала клетка-сирота
 *     (начинается с комбинирующего знака), и только от неё вправо.
 *
 * После хвоста сравнение прежнее (diffTerminalState, фаза tail): там
 * последствия разрывов законно разбегаются по сетке.
 */
export function diffSnapshotAgainstMirror(
  a: TerminalState,
  b: TerminalState,
  mirror: MirrorView,
  allowedGaps: readonly string[] = [],
): ConformanceResult {
  const gaps = allowedGaps.map(mirrorGap);
  const all = rawStateDiffs(a, b);
  const evidence = new Map<string, StateDiff[]>(gaps.map((g) => [g.id, []]));
  const unexplained: StateDiff[] = [];
  const act = b.active === "alternate" ? "alternate" : "normal";
  const underAlt = a.active === "alternate" && b.active === "alternate";
  const ga = activeGrid(a);
  const gb = activeGrid(b);
  const mcell = (y: number, x: number) => mirror.grid[y]?.[x] ?? " ";
  const cellFromMirror = (y: number, x: number) => gb[y]?.[x] === mcell(y, x) && ga[y]?.[x] !== mcell(y, x);
  const cursorFromMirror = b[act].cursorX === mirror.cursorX && b[act].cursorY === mirror.cursorY;
  // История по ЛОГИЧЕСКИМ строкам: строка истории зеркала шире экрана клиента
  // (напечатана до сужения, vt её не переносит) у клиента сама переносится на
  // мягко перенесённые строки — клиент получил ровно её (seed 20261006).
  const bHist = logicalHistory(b.normal.scrollback, b.cols);
  const historyFromMirror = bHist.length === mirror.scrollback.length && bHist.every((t, i) => t === mirror.scrollback[i]);
  // Известный дефект кадра frame-wide-last-column: в последней колонке у xterm
  // и у зеркала одна и та же широкая графема (обрезана сужением), у клиента —
  // пустая клетка (кадр её не печатает). Только эта клетка.
  const wideCutAt = (y: number, x: number) => x === b.cols - 1 && a[act].screen[y]?.cells[x]?.width === 2
    && mcell(y, x) === ga[y]?.[x] && gb[y]?.[x] === " ";
  if (mirror.alt !== (b.active === "alternate")) {
    unexplained.push({ path: "mirror.alt", a: b.active, b: mirror.alt });
  }
  // Поле «весь буфер» (normal.*) — утверждение о СКРЫТОМ буфере под alt, а не
  // о библиотечной клетке: на видимом экране оно не оправдывает ничего.
  const notWholeBuffer = (f: string) => !/^(normal|alternate)\.\*$/.test(f);
  // Колонка первой клетки-сироты зеркала в строке: содержимое начинается с
  // комбинирующего знака. Правее неё строка B законно съезжает (дефект
  // сборщика кадра frame-orphan-combining) — и только правее, и только в ней.
  const orphanAt = (y: number) => (mirror.grid[y] ?? []).findIndex((c) => /^\p{M}/u.test(c));
  type Justify = (g: MirrorGap, f: string) => boolean;
  const fromMirror: Justify = (g, f) => g.id !== ORPHAN_GAP && g.id !== WIDE_GAP && notWholeBuffer(f);
  const justified = (path: string): Justify | null => {
    if (underAlt && path.startsWith("normal.")) return (_g, f) => f === "normal.*";
    const cell = CELL_PATH.exec(path);
    if (cell) {
      if (cell[1] !== act) return null;
      const y = Number(cell[2]);
      const x = Number(cell[3]);
      if (cellFromMirror(y, x)) return fromMirror;
      const o = orphanAt(y);
      if (o >= 0 && x >= o) return (g) => g.id === ORPHAN_GAP;
      // …и клетка СЛЕВА от сироты: клиент приклеивает к ней знак, напечатанный
      // кадром следом, — ровно «клетка зеркала + знак» (NFC). Прежде это прятало
      // совпадение с xterm, у которого там та же буква со знаком; resize
      // разводит сетки, и различие вышло наружу (C-03R seed 20261216, alt-экран).
      if (o > 0 && x === o - 1 && gb[y]?.[x] === (mcell(y, x) + mcell(y, o)).normalize("NFC")) return (g) => g.id === ORPHAN_GAP;
      return wideCutAt(y, x) ? (g) => g.id === WIDE_GAP : null;
    }
    const cur = CURSOR_PATH.exec(path);
    if (cur) return cur[1] === act && cursorFromMirror ? fromMirror : null;
    if (path.startsWith("normal.scrollback")) {
      if (underAlt) return null;
      return historyFromMirror ? fromMirror : null;
    }
    return null;
  };
  for (const d of all) {
    let hit = gaps.filter((g) => g.fields.some((f) => !isBroadField(f) && matchField(f, d.path)));
    if (hit.length === 0) {
      const ok = justified(d.path);
      if (ok) hit = gaps.filter((g) => g.fields.some((f) => isBroadField(f) && ok(g, f) && matchField(f, d.path)));
    }
    for (const g of hit) evidence.get(g.id)!.push(d);
    if (hit.length > 0) continue;
    const cell = CELL_PATH.exec(d.path);
    if (cell && cell[1] === act) d.mirror = mcell(Number(cell[2]), Number(cell[3]));
    unexplained.push(d);
  }
  return { unexplained, evidence, all };
}

// ─── Воспроизводимая генерация ───────────────────────────────────────────────

/** mulberry32: маленький детерминированный ГПСЧ (seed → поток в [0,1)). */
export function mulberry32(seed: number): () => number {
  let a = seed >>> 0;
  return () => {
    a = (a + 0x6d2b79f5) >>> 0;
    let t = a;
    t = Math.imul(t ^ (t >>> 15), t | 1);
    t ^= t + Math.imul(t ^ (t >>> 7), t | 61);
    return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
  };
}

/** Геометрия терминала (колонки × строки). */
export interface Geometry {
  cols: number;
  rows: number;
}

/** Одна операция генератора: байты и разрывы, которые она способна вызвать. */
export interface StreamOp {
  text: string;
  gaps: readonly string[];
  /**
   * Смена геометрии ВМЕСТО байтов (text пустой): непрерывный xterm получает
   * term.resize, Go-зеркало — производственный Resize в той же точке потока
   * (маркер ACK в одной FIFO с выводом: session_screen.go resizeScreen,
   * screen.go SnapshotsFromStreamResized). Разрывы `gaps` — то, чем vt и xterm
   * расходятся на самой смене геометрии (resize вошёл в префикс снимка).
   */
  resize?: Geometry;
  /**
   * Разрывы, которые операция вызывает, стоя В ХВОСТЕ (после разреза): их
   * допускает только сравнение после хвоста. Resize после снимка перестраивает
   * экран клиента, восстановленный из кадра, — а в нём нет того, чего кадр не
   * переносит (признаки мягкого переноса, история зеркала вместо истории xterm).
   */
  tailGaps?: readonly string[];
}

/**
 * Смена геометрии на позиции потока `off` (байты). afterCut решает только
 * ничью: при разрезе РОВНО на off снимок снят ДО resize (он первое событие
 * хвоста); без afterCut — после (последнее событие префикса). Та же семантика,
 * что у pty.StreamResize и флага -resizes инструмента screen-frame.
 */
export interface StreamResize extends Geometry {
  off: number;
  afterCut?: boolean;
}

function isResizeOp(op: StreamOp): boolean {
  return op.resize !== undefined && op.text === "";
}

/** Вошёл ли resize в префикс снимка на разрезе cut. */
export function resizeBeforeCut(r: StreamResize, cut: number): boolean {
  return r.off < cut || (r.off === cut && !r.afterCut);
}

/** Resize по позиции, при равной позиции — в исходном порядке. */
export function sortResizes(rs: readonly StreamResize[]): StreamResize[] {
  return rs.map((r, i) => ({ r, i })).sort((x, y) => x.r.off - y.r.off || x.i - y.i).map((x) => x.r);
}

/**
 * Смены геометрии операций с их байтовыми позициями. Операции с индексом ≥
 * splitAt — хвост: resize, стоящий ровно на границе, достаётся хвосту
 * (afterCut). По умолчанию хвоста нет: resize на разрезе — в префиксе, как в
 * gapsBeforeCut.
 */
export function resizesOf(ops: readonly StreamOp[], splitAt = ops.length): StreamResize[] {
  const enc = new TextEncoder();
  const out: StreamResize[] = [];
  let off = 0;
  ops.forEach((op, i) => {
    if (op.resize) out.push({ off, cols: op.resize.cols, rows: op.resize.rows, ...(i >= splitAt ? { afterCut: true } : {}) });
    off += enc.encode(op.text).byteLength;
  });
  return out;
}

/** Геометрия PTY на разрезе: последняя смена, вошедшая в префикс. */
export function geometryAt(initial: Geometry, resizes: readonly StreamResize[], cut: number): Geometry {
  let g = { cols: initial.cols, rows: initial.rows };
  for (const r of sortResizes(resizes)) if (resizeBeforeCut(r, cut)) g = { cols: r.cols, rows: r.rows };
  return g;
}

/** Значение флага -resizes для tools/screen-frame: off:COLSxROWS[:tail],… */
export function resizesArg(rs: readonly StreamResize[]): string {
  return rs.map((r) => `${r.off}:${r.cols}x${r.rows}${r.afterCut ? ":tail" : ""}`).join(",");
}

export interface GeneratedStream {
  seed: number;
  ops: StreamOp[];
  text: string;
  bytes: Uint8Array;
  /** Разрывы, которые поток способен вызвать (объединение по операциям). */
  gaps: string[];
}

export interface StreamOptions {
  ops?: number;
  cols?: number;
  rows?: number;
  /** Широкие графемы (эмодзи, ZWJ, флаги) — только для информационного корпуса. */
  wide?: boolean;
  /**
   * Смены геометрии (операции resize, RESIZE_COLS × RESIZE_ROWS: сужение и
   * расширение по обеим осям). Отдельный набор seed: без флага генератор и его
   * байты прежние (как wide).
   */
  resize?: boolean;
}

/** Колонки и строки операций resize генератора: вокруг исходных 20×6 в обе стороны. */
export const RESIZE_COLS: readonly number[] = [12, 16, 20, 26, 32];
export const RESIZE_ROWS: readonly number[] = [3, 4, 6, 8, 10];

export function joinOps(seed: number, ops: StreamOp[]): GeneratedStream {
  const text = ops.map((o) => o.text).join("");
  const gaps = [...new Set(ops.flatMap((o) => o.gaps))].sort();
  return { seed, ops, text, bytes: new TextEncoder().encode(text), gaps };
}

/**
 * Разрывы операций, НАЧАВШИХСЯ до разреза (байтовая позиция): состояние на
 * разрезе складывают только они, а хвост у A и B один и тот же. Объединение
 * по всему потоку (как было) давало разрез-в-начале разрывы из его конца —
 * и широкие поля библиотечных разрывов гасили сравнение сетки на всех
 * разрезах всех seed (ревью C-conformance).
 */
export function gapsBeforeCut(ops: readonly StreamOp[], cut: number, tie: ResizeTie = "prefix"): string[] {
  return [...new Set(ops.slice(0, prefixLength(ops, cut, tie)).flatMap((o) => o.gaps))].sort();
}

/**
 * Хвостовые разрывы (tailGaps) операций, НЕ вошедших в префикс разреза (то же
 * правило, что gapsBeforeCut). Их допускает только сравнение после хвоста.
 */
export function gapsAfterCut(ops: readonly StreamOp[], cut: number, tie: ResizeTie = "prefix"): string[] {
  return [...new Set(ops.slice(prefixLength(ops, cut, tie)).flatMap((o) => o.tailGaps ?? []))].sort();
}

/**
 * Кому достаётся resize, стоящий РОВНО на разрезе: префиксу (снимок после
 * смены геометрии) или хвосту (снимок до неё; resizesOf(ops, 0) — afterCut).
 */
export type ResizeTie = "prefix" | "tail";

/**
 * Сколько первых операций вошло в префикс разреза cut (байтовая позиция):
 * начавшиеся до него и — при ничьей «prefix» — resize ровно на нём. Префикс
 * всегда начальный отрезок: операция с байтами на разрезе кончается за ним.
 */
export function prefixLength(ops: readonly StreamOp[], cut: number, tie: ResizeTie = "prefix"): number {
  const enc = new TextEncoder();
  let off = 0;
  let n = 0;
  for (const op of ops) {
    if (off > cut || (off === cut && !(isResizeOp(op) && tie === "prefix"))) break;
    off += enc.encode(op.text).byteLength;
    n++;
  }
  return n;
}

const WRAP = ["pending-wrap", "soft-wrap-flags"];

/**
 * Разрывы resize, вошедшего в префикс снимка: расхождения самой библиотеки vt
 * с xterm на смене геометрии. soft-wrap-flags — reflow xterm сам создаёт
 * признаки мягкого переноса, которых у vt нет вовсе. saved-cursor — xterm
 * хранит сохранённую строку абсолютной, и возврат строк из истории при
 * увеличении высоты сдвигает цель DECRC относительно экрана (seed 20261005),
 * а кадр сохранённый курсор не переносит. xterm-alt-resize-stale-length —
 * особенность клиента на пустом alt-буфере.
 */
const RESIZE_GAPS = [
  "vt-resize-no-reflow", "vt-resize-height", "soft-wrap-flags", "saved-cursor", "xterm-alt-resize-stale-length",
  "frame-wide-last-column",
];
/** Разрывы resize в хвосте: клиент перестраивает экран, восстановленный из кадра. */
const RESIZE_TAIL_GAPS = ["frame-reflow-without-wrap"];

/**
 * Поток из смеси текста и управляющих последовательностей (раздел 6.2):
 * текст у края, узкий Unicode (кириллица, €, 𝐀, комбинирующий знак, CJK),
 * CUP/CUx, SGR, EL/ED/ECH/ICH/DCH/IL/DL, DECSTBM, DECSC/RC, ?1049, ?2004,
 * мышь, ?2026, ?25, ?1, DECKPAM, ?7, ?6, IRM, charset, ED2/ED3, OSC 0/8, DCS,
 * HTS/TBC, редкий RIS. Одинаковый seed — одинаковые байты.
 */
export function generateStream(seed: number, opts: StreamOptions = {}): GeneratedStream {
  const rnd = mulberry32(seed);
  const cols = opts.cols ?? 20;
  const rows = opts.rows ?? 6;
  const count = opts.ops ?? 60;
  const int = (lo: number, hi: number) => lo + Math.floor(rnd() * (hi - lo + 1));
  const pick = <T>(xs: readonly T[]): T => xs[Math.floor(rnd() * xs.length)];
  const word = () => {
    const alphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789 .,:;-_=+/";
    let s = "";
    for (let i = int(1, cols); i > 0; i--) s += alphabet[Math.floor(rnd() * alphabet.length)];
    return s;
  };
  // Текущая геометрия потока: её меняют только операции resize (флаг resize),
  // без него она всегда исходная — байты прежних seed те же. CUP берёт её, а
  // не исходную: приложение после SIGWINCH рисует в новой сетке. prev —
  // геометрия до последнего resize: низ DECSTBM в наборе resize берётся из
  // большей из двух — приложение, не успевшее перерисоваться, ставит область
  // под прежнюю высоту (ESC[1;30r на экране в 11). Такой вход ронял vt (P1);
  // с 15.09 зеркало зажимает низ, как xterm (internal/pty/screen_vtpanic.go,
  // TestScreenMirrorVtPanicsDoNotKillMirror), и обход снят.
  let cur: Geometry = { cols, rows };
  let prev: Geometry = cur;
  const makers: [number, () => StreamOp][] = [
    [18, () => ({ text: word(), gaps: WRAP })],
    [5, () => ({ text: pick(["ж", "€", "\u{1D400}", "中", "Щука"]), gaps: WRAP })],
    [1, () => ({ text: "é", gaps: ["vt-combining-split", "frame-orphan-combining", ...WRAP] })],
    [8, () => ({ text: pick(["\r\n", "\n", "\r", "\b", "\t"]), gaps: WRAP })],
    [3, () => {
      let s = "";
      const n = int(1, rows * 2);
      for (let i = 0; i < n; i++) s += `L${int(0, 999)} ${word().slice(0, cols - 6)}\r\n`;
      return { text: s, gaps: WRAP };
    }],
    [6, () => ({ text: `\x1b[${int(1, cur.rows)};${int(1, cur.cols)}H`, gaps: [] })],
    [4, () => ({ text: `\x1b[${int(1, 5)}${pick(["A", "B", "C", "D"])}`, gaps: [] })],
    [4, () => {
      // Тот же расход ГПСЧ, что раньше: байты seed не меняются.
      const text = pick([`\x1b[${int(0, 2)}K`, `\x1b[${int(0, 1)}J`, `\x1b[${int(1, 6)}X`]);
      return { text, gaps: text === "\x1b[1J" ? ["vt-ed1-whole-line"] : [] };
    }],
    [3, () => ({ text: pick([`\x1b[${int(1, 3)}@`, `\x1b[${int(1, 3)}P`, `\x1b[${int(1, 2)}L`]), gaps: ["vt-phantom-after-edit"] })],
    [1, () => ({ text: `\x1b[${int(1, 2)}M`, gaps: ["region-scrollback-leak", "vt-phantom-after-edit"] })],
    [6, () => ({ text: pick(["\x1b[m", "\x1b[1m", "\x1b[31m", "\x1b[42m", "\x1b[4;33m", "\x1b[38;5;200m", "\x1b[38;2;1;2;3m", "\x1b[7m", "\x1b[0m"]), gaps: ["sgr-pen"] })],
    [2, () => {
      const top = int(1, cur.rows - 1);
      const bottom = int(top + 1, opts.resize ? Math.max(cur.rows, prev.rows) : cur.rows);
      return { text: `\x1b[${top};${bottom}r`, gaps: ["scroll-region", "region-scrollback-leak", "vt-ich-dch-region"] };
    }],
    [1, () => ({ text: "\x1b[r", gaps: [] })],
    [2, () => ({ text: pick(["\x1b7", "\x1b8"]), gaps: ["saved-cursor"] })],
    [2, () => ({ text: pick(["\x1b[?1049h", "\x1b[?1049l"]), gaps: ["alt-underlying-normal", "alt-enter-cursor-home", "saved-cursor", "vt-1049l-no-restore"] })],
    [2, () => ({ text: pick(["\x1b[?2004h", "\x1b[?2004l", "\x1b[?1000h", "\x1b[?1002h", "\x1b[?1006h", "\x1b[?1004h"]), gaps: [] })],
    // Сброс члена семейства мыши: xterm гасит всё семейство, decTracker тоже
    // (разрыв mouse-reset-enum снят 14.09).
    [1, () => ({ text: pick(["\x1b[?1000l", "\x1b[?1002l", "\x1b[?1006l", "\x1b[?1015l"]), gaps: [] })],
    [1, () => ({ text: pick(["\x1b[?2026h", "\x1b[?2026l"]), gaps: ["sync-2026"] })],
    // ?25, ?1, DECKPAM, ?7, ?6 и флаг IRM кадр переносит (screen_modes.go):
    // своих разрывов у них нет, остаются только расхождения библиотеки.
    [1, () => ({ text: pick(["\x1b[?25l", "\x1b[?25h"]), gaps: [] })],
    [1, () => ({ text: pick(["\x1b[?1h", "\x1b[?1l"]), gaps: [] })],
    [1, () => ({ text: pick(["\x1b=", "\x1b>"]), gaps: [] })],
    [1, () => ({ text: pick(["\x1b[?7l", "\x1b[?7h"]), gaps: WRAP })],
    [1, () => ({ text: pick(["\x1b[?6h", "\x1b[?6l"]), gaps: ["decom-vt-cursor"] })],
    [1, () => ({ text: pick(["\x1b[4h", "\x1b[4l"]), gaps: ["irm-vt-print"] })],
    [1, () => ({ text: pick(["\x1b(0", "\x1b(B"]), gaps: ["charset", "dec-graphics-glyphs"] })],
    [1, () => ({ text: pick(["\x1b[2J", "\x1b[H\x1b[2J"]), gaps: [] })],
    [1, () => ({ text: "\x1b[3J", gaps: [] })],
    [1, () => ({ text: pick([`\x1b]0;t${int(0, 99)}\x07`, `\x1b]2;w${int(0, 99)}\x1b\\`]), gaps: ["title-palette"] })],
    [1, () => ({ text: `\x1b]8;;http://e/${int(0, 9)}\x07${word().slice(0, 5)}\x1b]8;;\x07`, gaps: ["osc8", ...WRAP] })],
    [1, () => ({ text: "\x1bP1$qm\x1b\\", gaps: [] })],
    // HTS и TBC 0 при курсоре за правым краем (resize в alt, затем ?1049l;
    // DECRC после сужения) роняли vt (P2/P3); с 15.09 зеркало их пропускает
    // как ненаблюдаемые (screen_vtpanic.go), и обход «HTS после CR» снят.
    [1, () => ({ text: pick(["\x1bH", "\x1b[3g", "\x1b[g"]), gaps: ["tabstops"] })],
    // DECSTR: xterm сбрасывает режимы, перо, область, charset и сохранённый
    // курсор (в начало экрана — как у клиента после кадра). Шесть режимов
    // трекера зеркала сбрасываются (screen_modes.go), остальное — разрывы.
    [1, () => ({ text: "\x1b[!p", gaps: ["vt-decstr-ignored"] })],
  ];
  if (opts.wide) {
    makers.push([6, () => ({
      text: pick(["\u{1F600}", "\u{1F468}‍\u{1F469}‍\u{1F467}", "☀️", "\u{1F1F7}\u{1F1FA}", "\u{1F44D}\u{1F3FD}"]),
      gaps: ["unicode-width-v6-vs-grapheme", ...WRAP],
    })]);
  }
  if (opts.resize) {
    makers.push([4, () => {
      prev = cur;
      cur = { cols: pick(RESIZE_COLS), rows: pick(RESIZE_ROWS) };
      return { text: "", resize: cur, gaps: RESIZE_GAPS, tailGaps: RESIZE_TAIL_GAPS };
    }]);
  }
  const total = makers.reduce((s, [w]) => s + w, 0);
  const ops: StreamOp[] = [];
  for (let i = 0; i < count; i++) {
    let r = rnd() * total;
    for (const [w, make] of makers) {
      r -= w;
      if (r < 0) { ops.push(make()); break; }
    }
    // Редкий полный сброс: проверяет, что трекеры режимов сбрасываются вместе с vt.
    if (rnd() < 0.01) ops.push({ text: "\x1bc", gaps: ["xterm-ris-keeps-cursor-hidden"] });
  }
  // Resize ПОСЛЕ хвоста: последнее событие потока — смена геометрии, и для
  // каждого разреза, кроме последнего, сравнение после хвоста видит клиента,
  // перестроенного resize'ом (без флага resize байты seed прежние).
  if (opts.resize) ops.push(makers[makers.length - 1][1]());
  return joinOps(seed, ops);
}

/** План кусков для зеркала: 1–8 размеров от 1 до maxChunk байт (по кругу). */
export function generateChunkPlan(seed: number, maxChunk = 64): number[] {
  const rnd = mulberry32(seed ^ 0x9e3779b9);
  const n = 1 + Math.floor(rnd() * 8);
  const out: number[] = [];
  for (let i = 0; i < n; i++) out.push(1 + Math.floor(rnd() * maxChunk));
  return out;
}
