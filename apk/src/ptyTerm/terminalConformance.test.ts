import { describe, expect, it } from "vitest";
import { Terminal as HeadlessTerminal } from "@xterm/headless";
import { Unicode11Addon } from "@xterm/addon-unicode11";
import {
  activeGrid,
  captureTerminalState,
  diffSnapshotAgainstMirror,
  diffTerminalState,
  EXPECTED_MIRROR_GAPS,
  gapsAfterCut,
  gapsBeforeCut,
  generateChunkPlan,
  generateStream,
  geometryAt,
  isBroadField,
  mulberry32,
  observeTerminal,
  prefixLength,
  privateCore,
  rawStateDiffs,
  resizesArg,
  resizesOf,
  sortResizes,
  vanishedGaps,
  xtermParserAtGround,
} from "./terminalConformance";
import type { ConformanceTerminal, MirrorView, StreamOp, TerminalState } from "./terminalConformance";
import { TERMINAL_EMULATION } from "./terminalEmulation";

const COLS = 20;
const ROWS = 6;

interface TermOpts { scrollback?: number; unicode11?: boolean; cols?: number; rows?: number }

function fresh(opts: TermOpts = {}): HeadlessTerminal {
  const term = new HeadlessTerminal({
    cols: opts.cols ?? COLS,
    rows: opts.rows ?? ROWS,
    scrollback: opts.scrollback ?? TERMINAL_EMULATION.scrollback,
    allowProposedApi: true,
  });
  if (opts.unicode11) {
    term.loadAddon(new Unicode11Addon());
    term.unicode.activeVersion = "11";
  }
  observeTerminal(term as unknown as ConformanceTerminal);
  return term;
}

/** Кусок потока: байты или смена геометрии на этом месте (term.resize). */
type Seg = string | { cols: number; rows: number };

async function stateOf(stream: string | readonly Seg[], opts: TermOpts = {}): Promise<TerminalState> {
  const term = fresh(opts);
  for (const seg of typeof stream === "string" ? [stream] : stream) {
    if (typeof seg === "string") await new Promise<void>((r) => term.write(seg, r));
    else term.resize(seg.cols, seg.rows);
  }
  const s = captureTerminalState(term as unknown as ConformanceTerminal);
  term.dispose();
  return s;
}

const lines = (n: number, from = 1) => Array.from({ length: n }, (_, i) => `L${i + from}`).join("\r\n");

// Синтетическое воспроизведение каждого класса разрыва на двух headless:
// a — что даёт непрерывный поток, b — что даёт кадр зеркала. Сравнитель
// обязан поймать различие, объяснить его ровно этим разрывом и не объяснить
// посторонним (иначе реестр превращается в свалку, риск карты 05).
interface GapCase {
  a: string | readonly Seg[];
  b: string | readonly Seg[];
  aOpts?: TermOpts;
  bOpts?: TermOpts;
}

// Режимы, которые кадр теперь переносит (разрывы сняты): сравнитель обязан
// по-прежнему видеть их различие как НЕОБЪЯСНЁННОЕ — регресс зеркала не
// спрячется за реестром.
const MODE_CASES: Record<string, GapCase> = {
  "cursor-visibility": { a: "\x1b[?25l", b: "" },
  decckm: { a: "\x1b[?1h", b: "" },
  deckpam: { a: "\x1b=", b: "" },
  decom: { a: "\x1b[?6h", b: "" },
  decawm: { a: "\x1b[?7l", b: "" },
  irm: { a: "\x1b[4h", b: "" },
  // Сняты 14.09 правкой decTracker: DECRST любого члена гасит трекинг мыши,
  // DECSTR гасит ?2004 и ?1004.
  "mouse-reset-enum": { a: "\x1b[?1002h\x1b[?1000l", b: "\x1b[?1002h" },
  "decstr-dec-tracker": { a: "\x1b[?2004h\x1b[?1004h\x1b[!p", b: "\x1b[?2004h\x1b[?1004h" },
};

const GAP_CASES: Record<string, GapCase> = {
  "pending-wrap": { a: "x".repeat(COLS), b: "x".repeat(COLS) + `\x1b[1;${COLS}H` },
  "sgr-pen": { a: "\x1b[31m", b: "" },
  "scroll-region": { a: "\x1b[2;4r", b: "" },
  "saved-cursor": { a: "\x1b[3;5H\x1b7\x1b[H", b: "" },
  // Кадр входит в alt с чистого терминала и ставит курсор абсолютно: alt
  // совпадает, расходится только normal под ним (текст и курсор входа).
  "alt-underlying-normal": { a: "shell\x1b[?1049h", b: "\x1b[?1049h\x1b[1;6H" },
  // Расхождения библиотеки vt: b моделирует поведение vt средствами xterm.
  // vt ставит курсор в угол при входе в alt, xterm — нет.
  "alt-enter-cursor-home": { a: "shell\x1b[?1049h", b: "shell\x1b[?1049h\x1b[H" },
  // vt на ED3 стирает и экран.
  // xterm на ?6h уводит курсор в начало, vt оставляет на месте.
  "decom-vt-cursor": { a: "abc\x1b[?6h", b: "abc\x1b[?6h\x1b[1;4H" },
  // vt отрывает комбинирующий знак от ASCII-буквы: DCH по его колонке.
  "vt-combining-split": { a: "é\x1b[1;2H\x1b[P", b: "e\x1b[1;2H\x1b[P" },
  "dec-graphics-glyphs": { a: "\x1b(0z\x1b(B", b: "⩾" },
  // xterm.js держит курсор скрытым через RIS; кадр зеркала (как vt) — показывает.
  "xterm-ris-keeps-cursor-hidden": { a: "\x1b[?25l\x1bc", b: "\x1b[?25l\x1bc\x1b[?25h" },
  // vt не вставляет ячейки вне области прокрутки.
  "vt-ich-dch-region": { a: "abc\x1b[3;5r\x1b[1;1H\x1b[2@", b: "abc\x1b[3;5r\x1b[1;1H" },
  // vt держит отложенный перенос через DL: следующий символ — на строку ниже.
  "vt-phantom-after-edit": { a: `${"x".repeat(COLS)}\x1b[Mv`, b: `${"x".repeat(COLS)}\x1b[M\x1b[2;1Hv` },
  charset: { a: "\x1b(0", b: "" },
  tabstops: { a: "\x1b[3g", b: "" },
  "soft-wrap-flags": { a: "x".repeat(COLS + 5), b: "x".repeat(COLS) + "\x1b[2;1Hxxxxx" },
  "sync-2026": { a: "\x1b[?2026h", b: "" },
  osc8: { a: "\x1b]8;;http://x\x07L\x1b]8;;\x07", b: "L" },
  "title-palette": { a: "\x1b]0;t\x07\x1b]4;1;rgb:ff/00/00\x07", b: "" },
  "region-scrollback-leak": { a: lines(8), b: `L0\r\n${lines(8)}` },
  "unicode-width-v6-vs-grapheme": { a: "\u{1F600}b", b: "\u{1F600}b", bOpts: { unicode11: true } },
  "scrollback-cap-500": { a: lines(520), b: lines(520), bOpts: { scrollback: 500 } },
  "retention-ed3": { a: `${lines(12)}\x1b[3J`, b: lines(12) },
  "irm-vt-print": { a: "ab\x1b[1;1H\x1b[4hX", b: "ab\x1b[1;1HX\x1b[4h" },
  // vt DECSTR не видит: перо переживает сброс, буква у зеркала красная.
  "vt-decstr-ignored": { a: "\x1b[31m\x1b[!pX", b: "\x1b[31mX\x1b[m" },
  // vt на ED1 стирает строку курсора целиком.
  "vt-ed1-whole-line": { a: "abcdefgh\x1b[1;4H\x1b[1J", b: "abcdefgh\x1b[1;4H\x1b[2K" },
  // vt на ?1049l вне alt не делает DECRC: перо живёт дальше.
  "vt-1049l-no-restore": { a: "\x1b[1m\x1b[?1049lX", b: "\x1b[1mX\x1b[m" },
  // Смена геометрии. b моделирует vt средствами xterm: строки не склеены.
  // xterm при расширении склеивает мягко перенесённую строку, vt — нет.
  "vt-resize-no-reflow": { a: ["x".repeat(25), { cols: 30, rows: ROWS }], b: [`${"x".repeat(20)}\r\n${"x".repeat(5)}`, { cols: 30, rows: ROWS }] },
  // xterm при уменьшении высоты с курсором внизу уводит верх в историю, vt
  // отрезает низ (у зеркала остаются L1–L3 и нет истории).
  "vt-resize-height": { a: [lines(6), { cols: COLS, rows: 3 }], b: ["L1\r\nL2\r\nL3", { cols: COLS, rows: 3 }] },
  // Кадр адресует строки CUP: признака переноса нет, resize после снимка их не склеивает.
  "frame-reflow-without-wrap": { a: ["x".repeat(25), { cols: 30, rows: ROWS }], b: [`${"x".repeat(20)}\x1b[2;1H${"x".repeat(5)}`, { cols: 30, rows: ROWS }] },
  // Сужение оставило 中 в последней колонке у xterm; кадр её не печатает.
  "frame-wide-last-column": { a: ["abcdefghijk中", { cols: 12, rows: ROWS }], b: "abcdefghijk", bOpts: { cols: 12 } },
  // Пустой alt-буфер пережил уменьшение высоты с прежней длиной: прокрутка в
  // alt копит «историю»; свежий клиент 20×3 — нет.
  "xterm-alt-resize-stale-length": { a: [{ cols: COLS, rows: 3 }, `\x1b[?1049h${"\n".repeat(5)}`], b: `\x1b[?1049h${"\n".repeat(5)}`, bOpts: { rows: 3 } },
  // Кадр печатает клетку-сироту зеркала (e | U+0301) как есть: xterm склеивает
  // знак с «e», и строка клиента съезжает на колонку влево.
  "frame-orphan-combining": { a: "L313 J\x1b[1;1Hé", b: "é13 J\x1b[1;2H" },
};

describe("сравнитель терминального состояния (C-05)", () => {
  it("у каждого разрыва реестра есть синтетическое воспроизведение", () => {
    const missing = EXPECTED_MIRROR_GAPS.map((g) => g.id).filter((id) => !(id in GAP_CASES));
    expect(missing).toEqual([]);
  });

  it("одинаковые потоки — ноль различий (сравнитель не «всегда разный»)", async () => {
    const stream = "\x1b[?1049h\x1b[31mtext\x1b[2;4r\x1b7\x1b(0q\x1b]0;t\x07";
    expect(rawStateDiffs(await stateOf(stream), await stateOf(stream))).toEqual([]);
  });

  for (const gap of EXPECTED_MIRROR_GAPS) {
    it(`ловит и объясняет ровно разрыв ${gap.id}`, async () => {
      const c = GAP_CASES[gap.id];
      const a = await stateOf(c.a, c.aOpts);
      const b = await stateOf(c.b, c.bOpts);
      // 1. Без объявления — различие необъяснено: сравнитель его видит.
      const bare = diffTerminalState(a, b, []);
      expect(bare.unexplained.length, `сравнитель не увидел ${gap.id}`).toBeGreaterThan(0);
      // 2. С объявлением — объяснено целиком и есть доказательство.
      const declared = diffTerminalState(a, b, [gap.id]);
      expect(declared.unexplained, `различие ${gap.id} вышло за его поля`).toEqual([]);
      expect(declared.evidence.get(gap.id)!.length).toBeGreaterThan(0);
      expect(vanishedGaps([gap.id], [declared])).toEqual([]);
      // 3. Посторонний разрыв это различие не объясняет.
      const other = gap.id === "title-palette" ? "sync-2026" : "title-palette";
      expect(diffTerminalState(a, b, [other]).unexplained.length).toBeGreaterThan(0);
    });
  }

  for (const [id, c] of Object.entries(MODE_CASES)) {
    it(`снятый разрыв ${id}: различие режима необъяснимо ни одним разрывом реестра`, async () => {
      expect(EXPECTED_MIRROR_GAPS.some((g) => g.id === id)).toBe(false);
      const a = await stateOf(c.a);
      const b = await stateOf(c.b);
      const everything = EXPECTED_MIRROR_GAPS.filter((g) => !g.fields.some((f) => f.includes("screen") || f.includes("cursor"))).map((g) => g.id);
      expect(diffTerminalState(a, b, everything).unexplained.length).toBeGreaterThan(0);
    });
  }

  it("строгий xfail: объявленный разрыв без проявления — исчез", async () => {
    const s = await stateOf("same");
    const r = diffTerminalState(s, s, ["pending-wrap", "sgr-pen"]);
    expect(vanishedGaps(["pending-wrap", "sgr-pen"], [r])).toEqual(["pending-wrap", "sgr-pen"]);
  });

  it("последствия разрыва допускаются только после хвоста, и не считаются доказательством", async () => {
    // Перенос у края: после хвоста "BC" ячейки разные, но в момент снимка
    // допустим только курсор.
    const a = await stateOf("x".repeat(COLS) + "BC");
    const b = await stateOf("x".repeat(COLS) + `\x1b[1;${COLS}H` + "BC");
    expect(diffTerminalState(a, b, ["pending-wrap"], "snapshot").unexplained.length).toBeGreaterThan(0);
    const tail = diffTerminalState(a, b, ["pending-wrap"], "tail");
    expect(tail.unexplained).toEqual([]);
  });

  it("неизвестный разрыв — ошибка, а не молчаливое «объяснено»", async () => {
    const s = await stateOf("");
    expect(() => diffTerminalState(s, s, ["no-such-gap"])).toThrow(/EXPECTED_MIRROR_GAPS/);
  });

  it("приватный адаптер падает явно, если поля xterm переехали", () => {
    const term = fresh();
    expect(() => privateCore({ cols: 1, rows: 1, modes: {}, buffer: term.buffer } as unknown as ConformanceTerminal))
      .toThrow(/_core/);
    const core = (term as unknown as { _core: { coreService: unknown } })._core;
    const saved = core.coreService;
    core.coreService = {};
    try {
      expect(() => captureTerminalState(term as unknown as ConformanceTerminal))
        .toThrow(/_core\.coreService\.isCursorHidden/);
    } finally {
      core.coreService = saved;
      term.dispose();
    }
  });

  it("оракул границы парсера xterm: незавершённые CSI/OSC/DCS/UTF-8 — не граница", async () => {
    const term = fresh();
    const t = term as unknown as ConformanceTerminal;
    const w = (d: string | Uint8Array) => new Promise<void>((r) => term.write(d, r));
    expect(xtermParserAtGround(t)).toBe(true);
    for (const [partial, rest] of [
      ["\x1b[38;5", "m"],
      ["\x1b]0;x", "\x07"],
      ["\x1bP1|x", "\x1b\\"],
      ["\x1b", "7"],
    ]) {
      await w(partial);
      expect(xtermParserAtGround(t), JSON.stringify(partial)).toBe(false);
      await w(rest);
      expect(xtermParserAtGround(t)).toBe(true);
    }
    await w(new Uint8Array([0xf0, 0x9f]));
    expect(xtermParserAtGround(t)).toBe(false);
    await w(new Uint8Array([0x98, 0x80]));
    expect(xtermParserAtGround(t)).toBe(true);
    term.dispose();
  });

  it("эмуляция стенда совпадает с объявленной (Unicode 6, версии xterm)", async () => {
    const term = fresh();
    expect(term.unicode.activeVersion).toBe(TERMINAL_EMULATION.unicodeVersion);
    term.dispose();
  });
});

// Строгая фаза снимка (C-03 после ревью): третья сторона — само зеркало.
// mirrorFrom моделирует зеркало средствами xterm: его сетка, курсор и история.
function mirrorFrom(s: TerminalState): MirrorView {
  const buf = s.active === "alternate" ? s.alternate : s.normal;
  return {
    grid: activeGrid(s),
    cursorX: buf.cursorX,
    cursorY: buf.cursorY,
    alt: s.active === "alternate",
    scrollback: s.active === "alternate" ? [] : s.normal.scrollback.map((l) => l.text),
  };
}

const ALL_GAPS = EXPECTED_MIRROR_GAPS.map((g) => g.id);

describe("строгая фаза снимка: клиент = xterm ИЛИ зеркало (C-03, ревью C-conformance)", () => {
  it("буква, которой нет ни у xterm, ни у зеркала, не прячется даже за ВСЕМИ разрывами реестра", async () => {
    const a = await stateOf("line one\r\nline two");
    const b = await stateOf("line one\r\nline twX");
    // Прежнее сравнение: широкие поля объясняли дефект сборщика кадра.
    expect(diffTerminalState(a, b, ALL_GAPS, "snapshot").unexplained).toEqual([]);
    const r = diffSnapshotAgainstMirror(a, b, mirrorFrom(a), ALL_GAPS);
    expect(r.unexplained.map((d) => d.path)).toEqual(["normal.screen[1][7].chars"]);
    expect(r.unexplained[0].mirror).toBe("o");
    // Лишняя строка — тоже.
    const extra = await stateOf("line one\r\nline two\r\nEXTRA\x1b[2;9H");
    expect(diffSnapshotAgainstMirror(a, extra, mirrorFrom(a), ALL_GAPS).unexplained.length).toBeGreaterThan(0);
  });

  it("клетка B == M ≠ A объяснена только объявленным библиотечным разрывом с широким полем", async () => {
    // xterm рисует DEC Special Graphics «z» как ≥, vt — как U+2A7E: клиент взял
    // глиф у зеркала (B == M ≠ A).
    const a = await stateOf("\x1b(0z\x1b(B");
    const b = await stateOf("⩾");
    const m = mirrorFrom(b);
    const ok = diffSnapshotAgainstMirror(a, b, m, ["dec-graphics-glyphs"]);
    expect(ok.unexplained).toEqual([]);
    expect(ok.evidence.get("dec-graphics-glyphs")!.length).toBeGreaterThan(0);
    expect(diffSnapshotAgainstMirror(a, b, m, []).unexplained.length).toBeGreaterThan(0);
    expect(diffSnapshotAgainstMirror(a, b, m, ["title-palette"]).unexplained.length).toBeGreaterThan(0);
    // normal.* — утверждение о скрытом буфере под alt, клетку видимого экрана не оправдывает.
    expect(diffSnapshotAgainstMirror(a, b, m, ["alt-underlying-normal"]).unexplained.length).toBeGreaterThan(0);
  });

  it("курсор: B == M объясняет pending-wrap; курсор B, которого нет у зеркала, — нет", async () => {
    const a = await stateOf("x".repeat(COLS));
    const b = await stateOf("x".repeat(COLS) + `\x1b[1;${COLS}H`);
    expect(diffSnapshotAgainstMirror(a, b, mirrorFrom(b), ["pending-wrap"]).unexplained).toEqual([]);
    expect(diffSnapshotAgainstMirror(a, b, mirrorFrom(a), ["pending-wrap"]).unexplained.map((d) => d.path)).toEqual(["normal.cursor.x"]);
  });

  it("normal.* под alt объясняет alt-underlying-normal; на normal-экране — нет", async () => {
    const a = await stateOf("shell\x1b[?1049h");
    const b = await stateOf("\x1b[?1049h\x1b[1;6H");
    expect(diffSnapshotAgainstMirror(a, b, mirrorFrom(b), ["alt-underlying-normal"]).unexplained).toEqual([]);
    const na = await stateOf("shell");
    const nb = await stateOf("");
    expect(diffSnapshotAgainstMirror(na, nb, mirrorFrom(nb), ["alt-underlying-normal"]).unexplained.length).toBeGreaterThan(0);
  });

  it("история: широкий разрыв объясняет различие с xterm, только если клиент получил ровно историю зеркала", async () => {
    const a = await stateOf(lines(8));
    const b = await stateOf(`L0\r\n${lines(8)}`);
    expect(diffSnapshotAgainstMirror(a, b, mirrorFrom(b), ["region-scrollback-leak"]).unexplained).toEqual([]);
    const lost = { ...mirrorFrom(b), scrollback: ["L0", "L1", "LOST"] };
    expect(diffSnapshotAgainstMirror(a, b, lost, ["region-scrollback-leak"]).unexplained.length).toBeGreaterThan(0);
  });

  it("история зеркала шире экрана клиента: сравнение по логическим строкам (мягкий перенос склеен)", async () => {
    // Зеркало держит строку прежней ширины 20; клиент в 12 колонках пишет её,
    // и она сама переносится на две строки истории.
    const long = "L109 =KMGK SRjIVh-";
    const b = await stateOf(`${long}\r\n${lines(12)}`, { cols: 12 });
    const a = await stateOf(lines(12), { cols: 12 });
    const m = { ...mirrorFrom(b), scrollback: [long, ...lines(6).split("\r\n")] };
    expect(b.normal.scrollback.slice(0, 2).map((l) => [l.text, l.wrapped])).toEqual([["L109 =KMGK S", false], ["RjIVh-", true]]);
    expect(diffSnapshotAgainstMirror(a, b, m, ["vt-resize-no-reflow"]).unexplained).toEqual([]);
    // Продолжение потеряно — другая логическая строка, не история зеркала.
    const cut = await stateOf(`${long.slice(0, 12)}\r\n${lines(12)}`, { cols: 12 });
    expect(diffSnapshotAgainstMirror(a, cut, m, ["vt-resize-no-reflow"]).unexplained.length).toBeGreaterThan(0);
  });

  // Разрыв history-seam-wide-lines снят 15.09 (цель досылки — все ряды истории,
  // snapshotApply): потеря строки истории на шве больше не объясняется ничем.
  it("шов истории: потеря строки шире клиента необъяснима, целая история — от зеркала", async () => {
    const w1 = "W1".padEnd(19, "=");
    const w2 = "W2".padEnd(19, "=");
    // Экраны у сторон одинаковы (стёрты): различие только в истории.
    const a = await stateOf(`${lines(14)}\x1b[H\x1b[2J`, { cols: 16 });
    const lost = await stateOf(`${w1}\r\n${lines(6)}\x1b[H\x1b[2J`, { cols: 16 });
    expect(lost.normal.scrollback.map((l) => l.text)).toEqual(["W1==============", "==="]);
    const m = { ...mirrorFrom(lost), scrollback: [w1, w2] };
    // Прежний дефект — W2 стёрта кадром: ни один разрыв resize его не прячет.
    for (const gaps of [["vt-resize-no-reflow"], ["vt-resize-no-reflow", "soft-wrap-flags", "frame-wide-last-column"]]) {
      expect(diffSnapshotAgainstMirror(a, lost, m, gaps).unexplained.length).toBeGreaterThan(0);
    }
    // Клиент получил ровно историю зеркала (переносы — его, по его ширине).
    const whole = await stateOf(`${w1}\r\n${w2}\r\n${lines(6)}\x1b[H\x1b[2J`, { cols: 16 });
    expect(diffSnapshotAgainstMirror(a, whole, { ...mirrorFrom(whole), scrollback: [w1, w2] }, ["vt-resize-no-reflow"]).unexplained).toEqual([]);
  });

  it("клетка-сирота зеркала: сдвиг строки B правее неё объясняет только frame-orphan-combining", async () => {
    const a = await stateOf("L313 J\x1b[1;1Hé");
    const b = await stateOf("é13 J\x1b[1;3H");
    const m = mirrorFrom(a);
    m.grid = m.grid.map((row) => [...row]);
    m.grid[0].splice(0, 6, "e", "́", "1", "3", " ", "J");
    m.cursorX = 2;
    const both = diffSnapshotAgainstMirror(a, b, m, ["vt-combining-split", "frame-orphan-combining"]);
    expect(both.unexplained).toEqual([]);
    expect(both.evidence.get("frame-orphan-combining")!.length).toBeGreaterThan(0);
    // Одной библиотеки мало: третье значение — дефект сборщика кадра.
    expect(diffSnapshotAgainstMirror(a, b, m, ["vt-combining-split"]).unexplained.map((d) => d.path)).toContain("normal.screen[0][1].chars");
    // Клетка слева от сироты: у xterm там другое (сетки развёл resize), у
    // клиента — «клетка зеркала + знак». Оправдана только frame-orphan-combining
    // и только ровно этим значением.
    const shifted = await stateOf("X313 J\x1b[1;3H");
    const left = diffSnapshotAgainstMirror(shifted, b, m, ["vt-combining-split", "frame-orphan-combining"]);
    expect(left.unexplained.map((d) => d.path)).not.toContain("normal.screen[0][0].chars");
    expect(diffSnapshotAgainstMirror(shifted, b, m, ["vt-combining-split"]).unexplained.map((d) => d.path)).toContain("normal.screen[0][0].chars");
    const wrongLeft = await stateOf("ê13 J\x1b[1;3H");
    expect(diffSnapshotAgainstMirror(shifted, wrongLeft, m, ["vt-combining-split", "frame-orphan-combining"]).unexplained.map((d) => d.path))
      .toContain("normal.screen[0][0].chars");
    // Другая строка сиротой не оправдана.
    const other = await stateOf("é13 J\r\nQ\x1b[1;3H");
    expect(diffSnapshotAgainstMirror(a, other, m, ["vt-combining-split", "frame-orphan-combining"]).unexplained.map((d) => d.path))
      .toEqual(["normal.screen[1][0].chars"]);
  });

  it("широкая графема в последней колонке после сужения: пустая клетка клиента оправдана только frame-wide-last-column", async () => {
    const a = await stateOf(["abcdefghijk中", { cols: 12, rows: ROWS }]);
    expect(a.normal.screen[0].cells[11]).toMatchObject({ chars: "中", width: 2 });
    const b = await stateOf("abcdefghijk", { cols: 12 });
    const m = mirrorFrom(a); // зеркало держит ту же графему
    const r = diffSnapshotAgainstMirror(a, b, m, ["frame-wide-last-column"]);
    expect(r.unexplained).toEqual([]);
    expect(r.evidence.get("frame-wide-last-column")!.length).toBeGreaterThan(0);
    // Библиотечный разрыв с широким полем такую клетку не оправдывает.
    expect(diffSnapshotAgainstMirror(a, b, m, ["vt-resize-no-reflow"]).unexplained.length).toBeGreaterThan(0);
    // Не последняя колонка — не оправдана.
    const short = await stateOf("abcdefghij\x1b[1;12H", { cols: 12 });
    expect(diffSnapshotAgainstMirror(a, short, m, ["frame-wide-last-column"]).unexplained.map((d) => d.path)).toContain("normal.screen[0][10].chars");
  });

  it("сохранённый курсор — относительно экрана: xterm хранит savedY абсолютным (ybase + y)", async () => {
    const withHistory = await stateOf(`${lines(8)}\x1b[4;3H\x1b7`);
    const bare = await stateOf("\x1b[4;3H\x1b7");
    expect(withHistory.normal.saved).toBe(bare.normal.saved);
    expect(bare.normal.saved.startsWith("2,3:")).toBe(true);
  });

  it("зеркало в alt, а клиент нет — необъяснимо", async () => {
    const s = await stateOf("same");
    const r = diffSnapshotAgainstMirror(s, s, { ...mirrorFrom(s), alt: true }, ALL_GAPS);
    expect(r.unexplained.map((d) => d.path)).toEqual(["mirror.alt"]);
  });

  it("узкие поля работают как раньше, широкие — не сами по себе", () => {
    for (const f of ["*.screen*", "*.cursor.*", "*.cursor.x", "*.scrollback*", "normal.*", "alternate.cursor.*", "alternate.screen*", "normal.scrollback*"]) {
      expect(isBroadField(f), f).toBe(true);
    }
    for (const f of ["pen", "modes.synchronizedOutputMode", "*.scrollRegion", "*.screen*.wrapped", "*.screen*.style", "normal.scrollback.length", "cursorHidden"]) {
      expect(isBroadField(f), f).toBe(false);
    }
  });
});

describe("воспроизводимая генерация (T-27)", () => {
  it("gapsBeforeCut: разрывы только операций, начавшихся до разреза (байты, не символы)", () => {
    const ops = [
      { text: "ab", gaps: ["pending-wrap"] },
      { text: "\x1b[31m", gaps: ["sgr-pen"] },
      { text: "ж", gaps: ["soft-wrap-flags"] },
      { text: "\x1b[!p", gaps: ["vt-decstr-ignored"] },
    ];
    expect(gapsBeforeCut(ops, 0)).toEqual([]);
    expect(gapsBeforeCut(ops, 1)).toEqual(["pending-wrap"]);
    expect(gapsBeforeCut(ops, 2)).toEqual(["pending-wrap"]);
    expect(gapsBeforeCut(ops, 3)).toEqual(["pending-wrap", "sgr-pen"]);
    expect(gapsBeforeCut(ops, 7)).toEqual(["pending-wrap", "sgr-pen"]);
    expect(gapsBeforeCut(ops, 8)).toEqual(["pending-wrap", "sgr-pen", "soft-wrap-flags"]);
    expect(gapsBeforeCut(ops, 10)).toEqual(["pending-wrap", "sgr-pen", "soft-wrap-flags", "vt-decstr-ignored"]);
  });

  it("mulberry32 детерминирован", () => {
    const a = mulberry32(7);
    const b = mulberry32(7);
    const xs = [a(), a(), a()];
    expect([b(), b(), b()]).toEqual(xs);
    expect(xs.every((x) => x >= 0 && x < 1)).toBe(true);
  });

  it("без флага resize байты прежних seed C-03 не изменились (отпечаток генератора HEAD 1ddff81)", () => {
    const fnv = (s: string) => {
      let h = 0x811c9dc5;
      for (let i = 0; i < s.length; i++) {
        h ^= s.charCodeAt(i);
        h = Math.imul(h, 0x01000193) >>> 0;
      }
      return h.toString(16);
    };
    const got = [];
    for (let s = 20260913; s <= 20260920; s++) {
      const t = generateStream(s, { cols: COLS, rows: ROWS, ops: 50 }).text;
      got.push(`${s}:${t.length}:${fnv(t)}`);
    }
    got.push(`d42:${fnv(generateStream(42).text)}`);
    expect(got).toEqual([
      "20260913:505:d07205b1", "20260914:536:abd8e9cc", "20260915:302:e2fd7abb", "20260916:554:59f4866e",
      "20260917:307:e3885b5a", "20260918:271:e102a209", "20260919:717:585f42f1", "20260920:553:20995c56", "d42:c09783f6",
    ]);
  });

  it("один seed — одни байты и один план кусков; другой seed — другие", () => {
    const s1 = generateStream(42);
    expect(generateStream(42).text).toBe(s1.text);
    expect(generateStream(43).text).not.toBe(s1.text);
    expect(generateChunkPlan(42)).toEqual(generateChunkPlan(42));
    expect(s1.bytes.byteLength).toBeGreaterThan(0);
    // Разрывы потока — только из реестра.
    const known = new Set(EXPECTED_MIRROR_GAPS.map((g) => g.id));
    expect(s1.gaps.filter((g) => !known.has(g))).toEqual([]);
  });
});

describe("смена геометрии в модели стенда (§6, resize)", () => {
  const ops: StreamOp[] = [
    { text: "ab", gaps: ["pending-wrap"] },
    { text: "", gaps: ["vt-resize-height"], resize: { cols: 12, rows: 4 }, tailGaps: ["frame-reflow-without-wrap"] },
    { text: "ж", gaps: ["soft-wrap-flags"] },
    { text: "", gaps: ["vt-resize-no-reflow"], resize: { cols: 30, rows: 8 }, tailGaps: ["xterm-alt-resize-stale-length"] },
  ];

  it("resizesOf: байтовые позиции, хвост — со splitAt (afterCut)", () => {
    expect(resizesOf(ops)).toEqual([{ off: 2, cols: 12, rows: 4 }, { off: 4, cols: 30, rows: 8 }]);
    expect(resizesOf(ops, 2)).toEqual([{ off: 2, cols: 12, rows: 4 }, { off: 4, cols: 30, rows: 8, afterCut: true }]);
    expect(resizesOf(ops, 0).every((r) => r.afterCut)).toBe(true);
  });

  it("ничья: resize ровно на разрезе — префиксу или хвосту, разрывы вслед за ним", () => {
    expect(prefixLength(ops, 2)).toBe(2);
    expect(gapsBeforeCut(ops, 2)).toEqual(["pending-wrap", "vt-resize-height"]);
    expect(gapsAfterCut(ops, 2)).toEqual(["xterm-alt-resize-stale-length"]);
    expect(prefixLength(ops, 2, "tail")).toBe(1);
    expect(gapsBeforeCut(ops, 2, "tail")).toEqual(["pending-wrap"]);
    expect(gapsAfterCut(ops, 2, "tail")).toEqual(["frame-reflow-without-wrap", "xterm-alt-resize-stale-length"]);
    // Разрез посреди «ж» (байты 2..4): буква начата до разреза, resize на 4 — нет.
    expect(prefixLength(ops, 3)).toBe(3);
    expect(prefixLength(ops, 4)).toBe(4);
    expect(gapsAfterCut(ops, 4)).toEqual([]);
    expect(gapsAfterCut(ops, 4, "tail")).toEqual(["xterm-alt-resize-stale-length"]);
  });

  it("geometryAt: resize на разрезе входит в снимок, если он не afterCut; порядок при равной позиции сохраняется", () => {
    const rs = [{ off: 5, cols: 12, rows: 4 }, { off: 5, cols: 30, rows: 8, afterCut: true }, { off: 9, cols: 16, rows: 3 }];
    const g0 = { cols: COLS, rows: ROWS };
    expect(geometryAt(g0, rs, 4)).toEqual(g0);
    expect(geometryAt(g0, rs, 5)).toEqual({ cols: 12, rows: 4 });
    expect(geometryAt(g0, rs, 6)).toEqual({ cols: 30, rows: 8 });
    expect(geometryAt(g0, rs, 9)).toEqual({ cols: 16, rows: 3 });
    expect(sortResizes([rs[2], rs[0], rs[1]]).map((r) => r.cols)).toEqual([12, 30, 16]);
    expect(resizesArg(rs)).toBe("5:12x4,5:30x8:tail,9:16x3");
  });

  it("табуляции за краем после сужения не сравниваются (xterm их не удаляет, nextStop до них не доходит)", async () => {
    const shrunk = await stateOf([{ cols: 12, rows: ROWS }]);
    const born = await stateOf("", { cols: 12 });
    expect(shrunk.normal.tabs).toBe("0,8");
    expect(rawStateDiffs(shrunk, born)).toEqual([]);
  });

  it("генератор resize: смены геометрии, последняя — после хвоста; детерминирован; входы-убийцы vt не обходятся", () => {
    const known = new Set(EXPECTED_MIRROR_GAPS.map((g) => g.id));
    for (let seed = 20261001; seed <= 20261008; seed++) {
      const g = generateStream(seed, { cols: COLS, rows: ROWS, ops: 50, resize: true });
      expect(generateStream(seed, { cols: COLS, rows: ROWS, ops: 50, resize: true }).text).toBe(g.text);
      const rs = g.ops.filter((o) => o.resize);
      expect(rs.length).toBeGreaterThan(0);
      expect(g.ops[g.ops.length - 1].resize, `seed ${seed}: последним событием должна быть смена геометрии`).toBeDefined();
      expect(rs.every((o) => o.text === "")).toBe(true);
      expect(g.ops.flatMap((o) => [...o.gaps, ...(o.tailGaps ?? [])]).filter((x) => !known.has(x))).toEqual([]);
    }
    // P1/P2 (internal/pty/screen_vtpanic.go): генератор снова даёт низ DECSTBM
    // под прежнюю высоту и HTS без подготовки курсора — зеркало держит их
    // предохранителями и страховочной сеткой
    // (TestScreenMirrorVtPanicsDoNotKillMirror), обходов больше нет.
    let overBottom = 0;
    let bareHts = 0;
    for (let seed = 20261001; seed <= 20261100; seed++) {
      let rows = ROWS;
      for (const o of generateStream(seed, { cols: COLS, rows: ROWS, ops: 50, resize: true }).ops) {
        if (o.resize) rows = o.resize.rows;
        for (const m of o.text.matchAll(/\x1b\[(\d+);(\d+)r/g)) if (Number(m[2]) > rows) overBottom++;
        if (o.text === "\x1bH") bareHts++;
      }
    }
    expect(overBottom, "DECSTBM с низом за высотой").toBeGreaterThan(0);
    expect(bareHts, "HTS без CR и CUF").toBeGreaterThan(0);
    expect(generateStream(42).ops.some((o) => o.resize)).toBe(false);
  });
});
