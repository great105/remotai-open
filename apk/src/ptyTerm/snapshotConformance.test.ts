/**
 * Сквозная проверка терминального состояния (раздел 6 плана; T-26, T-27,
 * часть T-29; C-01…C-04, C-07 карты 05).
 *
 *   A  — весь поток целиком в headless xterm с параметрами клиента
 *        (TERMINAL_EMULATION);
 *   B  — префикс обрабатывает НАСТОЯЩЕЕ Go-зеркало (tools/screen-frame
 *        -snapshots-json → SnapshotsFromStream → производственные WriteAt и
 *        SnapshotAt), его снимок применяется к свежему headless по тому же
 *        плану, что исполняет клиент (planSnapshotApply), затем тот же хвост;
 *   A' — поток через RetentionPolicy клиента (keepHistory): допустимы только
 *        отличия retention-ed3.
 *
 * Сравнение — в момент снимка и после хвоста, по всему состоянию
 * (terminalConformance.ts). Любое различие обязано быть объявленным разрывом
 * из EXPECTED_MIRROR_GAPS, а объявленный разрыв обязан проявиться (строгий
 * xfail на назначенном разрезе фикстуры).
 *
 * Без Go тест ПРОПУСКАЕТСЯ со статусом not run — не pass (раздел 8).
 * Seed: CONFORMANCE_SEED (по умолчанию 20260913), число потоков
 * CONFORMANCE_STREAMS, разрезов на поток CONFORMANCE_CUTS.
 */
// @ts-ignore — runtime Vitest всегда Node, но browser tsconfig APK не подключает @types/node (как agentLaunch.test.ts).
import { execFileSync } from "node:child_process";
// @ts-ignore — см. выше.
import { existsSync, mkdtempSync, readdirSync, readFileSync, rmSync, statSync, writeFileSync } from "node:fs";
// @ts-ignore — см. выше.
import { tmpdir } from "node:os";
// @ts-ignore — см. выше.
import { join } from "node:path";
// @ts-ignore — см. выше.
import { fileURLToPath } from "node:url";
import { afterAll, beforeAll, describe, expect, it } from "vitest";
import { Terminal as HeadlessTerminal } from "@xterm/headless";
import { EMPTY_BYTES, SCROLLBACK_ERASE, splitScrollbackErase, stripScrollbackErase } from "./keepHistory";
import { planSnapshotApply, snapshotStepPayload } from "./snapshotApply";
import { noteSyncMarker } from "./snapshotHistory";
import {
  activeGrid,
  captureTerminalState,
  cellRuleMismatches,
  describeDiffs,
  diffSnapshotAgainstMirror,
  diffTerminalState,
  gapsAfterCut,
  gapsBeforeCut,
  generateChunkPlan,
  generateStream,
  geometryAt,
  joinOps,
  mulberry32,
  observeTerminal,
  prefixLength,
  resizeBeforeCut,
  resizesArg,
  resizesOf,
  sortResizes,
  vanishedGaps,
  xtermParserAtGround,
} from "./terminalConformance";
import type { ConformanceResult, ConformanceTerminal, Geometry, MirrorView, ResizeTie, StreamOp, StreamResize, TerminalState } from "./terminalConformance";
import { emulationMismatches, TERMINAL_EMULATION } from "./terminalEmulation";

declare const process: { env: Record<string, string | undefined>; platform: string };

const REPO = fileURLToPath(new URL("../../..", import.meta.url));
const COLS = 20;
const ROWS = 6;

function goVersion(): string | null {
  try {
    return execFileSync("go", ["version"], { encoding: "utf8", stdio: ["ignore", "pipe", "ignore"] }).trim();
  } catch {
    return null;
  }
}
const GO = goVersion();
if (!GO) console.warn("[conformance] Go toolchain не найден — snapshotConformance: NOT RUN (skip, это не pass)");

interface StreamSnapshot {
  cut: number;
  ready: boolean;
  frame: string;
  history: string;
  histLines: number;
  cols: number;
  rows: number;
  appliedOff: number;
  /**
   * Отказ после паники vt (internal/pty/screen_vtpanic.go): снимок withheld до
   * RIS. Не различие состояния — сравнивать нечего; C-03 считает, C-08 проверяет.
   */
  untrusted?: boolean;
  /** Состояние самого зеркала на разрезе (только при ready) — третья сторона строгой фазы снимка. */
  grid?: string[][];
  cursorX: number;
  cursorY: number;
  alt: boolean;
  scrollback?: string[];
  /** Ширина строк истории в колонках у клиента (вывод инструмента; сравнитель с 15.09 её не читает). */
  scrollbackCells?: number[];
}

/**
 * Зеркало на разрезе для diffSnapshotAgainstMirror; без сетки — инструмент
 * устарел, это ошибка.
 */
function mirrorOf(s: StreamSnapshot): MirrorView {
  if (!s.grid) throw new Error(`снимок на разрезе ${s.cut} без сетки зеркала — пересоберите tools/screen-frame`);
  return {
    grid: s.grid, cursorX: s.cursorX, cursorY: s.cursorY, alt: s.alt, scrollback: s.scrollback ?? [],
    scrollbackCells: s.scrollbackCells,
  };
}

const enc = (s: string) => new TextEncoder().encode(s);

function concat(a: Uint8Array, b: Uint8Array): Uint8Array {
  const out = new Uint8Array(a.byteLength + b.byteLength);
  out.set(a, 0);
  out.set(b, a.byteLength);
  return out;
}

function newTerm(cols = COLS, rows = ROWS): HeadlessTerminal {
  const t = new HeadlessTerminal({ cols, rows, scrollback: TERMINAL_EMULATION.scrollback, allowProposedApi: true });
  observeTerminal(t as unknown as ConformanceTerminal);
  return t;
}

const write = (t: HeadlessTerminal, d: string | Uint8Array) => new Promise<void>((r) => t.write(d, r));
const capture = (t: HeadlessTerminal) => captureTerminalState(t as unknown as ConformanceTerminal);
const atGround = (t: HeadlessTerminal) => xtermParserAtGround(t as unknown as ConformanceTerminal);

// ─── Go-инструмент ───────────────────────────────────────────────────────────

// tmp — рабочий каталог (инструмент ~7 МБ и вход на каждый вызов), удаляется
// в afterAll. Минимальные входы C-03 — в ОТДЕЛЬНОМ keptDir, который остаётся:
// по нему перезапуск CONFORMANCE_REPRO=<папка|файл>.
let tmp = "";
let tool = "";
let inputs = 0;
let keptDir = "";
const reproDir = () => keptDir || (keptDir = mkdtempSync(join(tmpdir(), "remotai-conformance-repro-")));

function snapshots(
  stream: Uint8Array,
  opts: {
    cuts: number[] | "all"; chunks?: number[]; idle?: boolean; cols?: number; rows?: number; resizes?: readonly StreamResize[];
    /** Снять предохранители от паник vt: проверка страховочной сетки (C-08). */
    noVtGuards?: boolean;
  },
): StreamSnapshot[] {
  const f = join(tmp, `in-${inputs++}.bin`);
  writeFileSync(f, stream);
  const args = [
    "-in", f, "-cols", String(opts.cols ?? COLS), "-rows", String(opts.rows ?? ROWS), "-snapshots-json",
    "-cuts", opts.cuts === "all" ? "all" : opts.cuts.join(","),
  ];
  if (opts.chunks && opts.chunks.length > 0) args.push("-chunks", opts.chunks.join(","));
  if (opts.idle) args.push("-idle");
  // Зеркало получает производственный Resize в той же точке потока
  // (SnapshotsFromStreamResized), что и term.resize у пути A.
  if (opts.resizes && opts.resizes.length > 0) args.push("-resizes", resizesArg(opts.resizes));
  if (opts.noVtGuards) args.push("-no-vt-guards");
  return JSON.parse(execFileSync(tool, args, { encoding: "utf8", maxBuffer: 256 << 20 })) as StreamSnapshot[];
}

// ─── Пути A и B ──────────────────────────────────────────────────────────────

/**
 * Байты потока вперемешку со сменами геометрии — так их видит клиент: resize
 * исполняется ровно на своей позиции, после записи всех байтов до неё (write
 * дожидается разбора, term.resize синхронный). Возвращает шаг «доиграть до
 * позиции to»; includeAt решает, брать ли resize, стоящий ровно на to.
 */
function player(t: HeadlessTerminal, stream: Uint8Array, resizes: readonly StreamResize[], from = 0) {
  const rs = sortResizes(resizes);
  let pos = from;
  let ri = 0;
  return async (to: number, includeAt: (r: StreamResize) => boolean): Promise<void> => {
    while (ri < rs.length && (rs[ri].off < to || (rs[ri].off === to && includeAt(rs[ri])))) {
      const r = rs[ri++];
      if (r.off > pos) {
        await write(t, stream.subarray(pos, r.off));
        pos = r.off;
      }
      t.resize(r.cols, r.rows);
    }
    if (to > pos) {
      await write(t, stream.subarray(pos, to));
      pos = to;
    }
  };
}

/**
 * Путь A на заданных разрезах: состояние и граница парсера xterm в каждом.
 * Смены геометрии — в тех же точках потока, что у зеркала (resizeBeforeCut:
 * resize ровно на разрезе входит в снимок, если он не afterCut).
 */
async function continuous(stream: Uint8Array, cuts: number[], cols = COLS, rows = ROWS, resizes: readonly StreamResize[] = []) {
  const t = newTerm(cols, rows);
  const states = new Map<number, TerminalState>();
  const ground = new Map<number, boolean>();
  const play = player(t, stream, resizes);
  for (const c of [...new Set(cuts)].sort((a, b) => a - b)) {
    await play(c, (r) => !r.afterCut);
    states.set(c, capture(t));
    ground.set(c, atGround(t));
  }
  await play(stream.byteLength, () => true);
  const end = capture(t);
  t.dispose();
  return { states, ground, end };
}

/**
 * Путь B: снимок зеркала → план клиента в свежем терминале → тот же хвост со
 * своими сменами геометрии. Модель одного зрителя: клиент живёт в геометрии
 * PTY на разрезе (initial + resize префикса), и кадр обязан быть снят в ней же —
 * зеркало, не получившее Resize, отдало бы кадр в прежней сетке.
 */
async function restored(snap: StreamSnapshot, stream: Uint8Array, cut: number, resizes: readonly StreamResize[] = [], initial: Geometry = { cols: COLS, rows: ROWS }) {
  const g = geometryAt(initial, resizes, cut);
  expect(`${snap.cols}x${snap.rows}`, `кадр на разрезе ${cut} снят не в геометрии PTY — зеркало не получило resize`).toBe(`${g.cols}x${g.rows}`);
  const t = newTerm(g.cols, g.rows);
  const plan = planSnapshotApply({
    history: snap.history,
    histLines: snap.histLines,
    screenRows: snap.rows,
    snapCols: snap.cols,
    termCols: t.cols,
    localScrollback: t.buffer.normal.baseY,
    historyState: noteSyncMarker("reset"),
  });
  for (const step of plan.steps) {
    await write(t, snapshotStepPayload(step, snap.frame, () => ({
      baseY: t.buffer.active.baseY,
      cursorY: t.buffer.active.cursorY,
      rows: t.rows,
    })));
  }
  const atCut = capture(t);
  const play = player(t, stream, resizes.filter((r) => !resizeBeforeCut(r, cut)), cut);
  await play(stream.byteLength, () => true);
  const end = capture(t);
  t.dispose();
  return { atCut, end, replace: plan.replace };
}

function assertConforms(label: string, snap: ConformanceResult, tail: ConformanceResult): void {
  expect(snap.unexplained, `${label}: необъяснённые различия В МОМЕНТ СНИМКА:\n${describeDiffs(snap.unexplained)}`).toEqual([]);
  expect(tail.unexplained, `${label}: необъяснённые различия ПОСЛЕ ХВОСТА:\n${describeDiffs(tail.unexplained)}`).toEqual([]);
}

// ─── Фикстуры ────────────────────────────────────────────────────────────────

interface Fixture {
  name: string;
  prefix: string;
  tail: string;
  /** Разрывы на назначенном разрезе (конец prefix): строгий xfail. */
  gaps: string[];
  /** Дополнительно допустимые на ПРОИЗВОЛЬНЫХ разрезах перебора (C-02). */
  cutGaps?: string[];
  /** false — только назначенный разрез (большая фикстура). */
  allCuts?: boolean;
  /** Смены геометрии в потоке (байтовые позиции prefix+tail). */
  resizes?: StreamResize[];
}

/** Кусок фикстуры с resize: текст или смена геометрии на этом месте. */
type Seg = string | Geometry;

/**
 * Фикстура со сменами геометрии: resize в prefix — ДО снимка (последний на
 * самом разрезе тоже входит в снимок), в tail — после него (первый в tail —
 * «между снимком и хвостом», последний — «после хвоста»; afterCut).
 */
function withResizes(f: { name: string; prefix: Seg[]; tail: Seg[]; gaps: string[]; cutGaps?: string[] }): Fixture {
  let prefix = "";
  let tail = "";
  const resizes: StreamResize[] = [];
  const len = (s: string) => enc(s).byteLength;
  for (const s of f.prefix) {
    if (typeof s === "string") prefix += s;
    else resizes.push({ off: len(prefix), cols: s.cols, rows: s.rows });
  }
  for (const s of f.tail) {
    if (typeof s === "string") tail += s;
    else resizes.push({ off: len(prefix) + len(tail), cols: s.cols, rows: s.rows, afterCut: true });
  }
  return { name: f.name, prefix, tail, gaps: f.gaps, cutGaps: f.cutGaps, resizes };
}

const lines = (n: number, from = 1) => Array.from({ length: n }, (_, i) => `L${i + from}`).join("\r\n");

// Строгий корпус: ASCII и узкий Unicode. Хвосты продолжения из раздела 6.1:
// перенос у края, DECRC, ?1049l, DECSTBM+LF, следующая CSI, вставка.
const FIXTURES: Fixture[] = [
  { name: "plain-shell", prefix: "PS> ls\r\nfile1 file2\r\nPS> ", tail: "echo hi\r\nhi\r\nPS> ", gaps: [] },
  { name: "history-scroll", prefix: `${lines(14)}\r\n$ `, tail: "cmd\r\nout1\r\nout2\r\n$ ", gaps: [] },
  { name: "wrap-at-edge", prefix: `\x1b[2;1H${"x".repeat(COLS)}`, tail: "BC", gaps: ["pending-wrap"], cutGaps: ["soft-wrap-flags"] },
  { name: "soft-wrap", prefix: `${"y".repeat(COLS + 5)}\r\n$ `, tail: "z", gaps: ["soft-wrap-flags"], cutGaps: ["pending-wrap"] },
  // Разрывы cursor-visibility, decckm, deckpam, decawm, decom, irm сняты
  // (screen_modes.go): эти фикстуры — положительные регрессии, различий ноль.
  { name: "cursor-hidden", prefix: "\x1b[?25lbusy", tail: " more\x1b[?25h", gaps: [] },
  { name: "sgr-pen", prefix: "\x1b[31mred", tail: " tail\x1b[m", gaps: ["sgr-pen"] },
  { name: "decrc", prefix: "\x1b[5;7H\x1b7\x1b[1;1Htop", tail: "\x1b8X", gaps: ["saved-cursor"] },
  { name: "alt-exit", prefix: "shell1\r\nshell2\x1b[?1049h\x1b[H\x1b[2Jalt screen", tail: "\x1b[?1049lback", gaps: ["alt-underlying-normal"], cutGaps: ["alt-enter-cursor-home", "saved-cursor"] },
  { name: "decstbm-lf", prefix: `${lines(6)}\x1b[2;4r\x1b[4;1H`, tail: "\nX\nY", gaps: ["scroll-region"], cutGaps: ["region-scrollback-leak"] },
  { name: "next-csi", prefix: "abc\x1b[1;1H", tail: "\x1b[2CZ\x1b[K\x1b[3;3H\x1b[1mB\x1b[m", gaps: [], cutGaps: ["sgr-pen"] },
  { name: "bracketed-paste-mouse", prefix: "\x1b[?2004h\x1b[?1002h\x1b[?1006h\x1b[?1004h$ ", tail: "pasted\x1b[?2004l", gaps: [] },
  // Найдено генератором (seed 31002): сброс ДРУГОГО члена семейства мыши.
  // decTracker исправлен 14.09 — положительная регрессия, различий ноль.
  { name: "mouse-cross-reset", prefix: "\x1b[?1002h\x1b[?1006h\x1b[?1000l\x1b[?1015l$ ", tail: "x", gaps: [] },
  // ?1015h у xterm ничего не меняет: SGR-кодировка обязана пережить его и в кадре.
  { name: "mouse-urxvt-ignored", prefix: "\x1b[?1000h\x1b[?1006h\x1b[?1015h$ ", tail: "x", gaps: [] },
  { name: "decckm", prefix: "\x1b[?1h$ ", tail: "x", gaps: [] },
  { name: "deckpam", prefix: "\x1b=$ ", tail: "x", gaps: [] },
  // У края без переноса xterm держит cursorX = cols, vt — cols−1: тот же
  // класс, что отложенный перенос (следующий символ у обоих перезаписывает край).
  { name: "decawm-off", prefix: "\x1b[?7l$ ", tail: "0123456789ABCDEFGHIJKLMNOP", gaps: [], cutGaps: ["pending-wrap"] },
  { name: "decom", prefix: "\x1b[?6htext", tail: "\x1b[2;2HX", gaps: [] },
  { name: "irm", prefix: "ab\x1b[1;1H\x1b[4h", tail: "X\x1b[4l", gaps: [], cutGaps: ["irm-vt-print"] },
  { name: "charset", prefix: "\x1b(0", tail: "qqq\x1b(B", gaps: ["charset"] },
  { name: "tabstops", prefix: "\x1b[3g\x1b[1;13H\x1bH\x1b[1;1H", tail: "\tT", gaps: ["tabstops"] },
  { name: "osc8", prefix: "\x1b]8;;http://e/1\x07link\x1b]8;;\x07 rest", tail: "!", gaps: ["osc8"] },
  { name: "title-palette", prefix: "\x1b]0;agent\x07\x1b]4;1;rgb:ff/00/00\x07$ ", tail: "x", gaps: ["title-palette"] },
  { name: "region-history", prefix: `${lines(8)}\x1b[2;5r\x1b[5;1H\r\nIN\r\nNEXT\x1b[r\x1b[6;1H`, tail: "\r\nafter", gaps: ["region-scrollback-leak"], cutGaps: ["scroll-region"] },
  { name: "ed2-ed3", prefix: `${lines(12)}\x1b[2J\x1b[3J\x1b[Hfresh`, tail: "\r\nnext", gaps: [] },
  // Одиночный ED3: xterm стирает только историю, экран остаётся. До 14.09
  // зеркало гасило и экран — после переподключения человек видел пустоту.
  { name: "ed3-alone", prefix: `${lines(8)}\x1b[3J`, tail: "\r\nnext", gaps: [] },
  // ED3 в alt-экране у xterm не делает ничего; vt стирал alt-экран.
  { name: "ed3-in-alt", prefix: "shell\x1b[?1049h\x1b[HALT\x1b[3J", tail: "x", gaps: ["alt-underlying-normal"], cutGaps: ["alt-enter-cursor-home", "saved-cursor"] },
  { name: "narrow-unicode", prefix: "жук € \u{1D400} é 中文\r\n", tail: "ok ж", gaps: [] },
  { name: "scrollback-cap", prefix: lines(520), tail: "\r\nend", gaps: ["scrollback-cap-500"], allCuts: false },
  // C-04 (T-29, L2): DEC 2026 — флаг и буфер; конечный показ и таймаут 1 с — L3.
  { name: "2026-open", prefix: "\x1b[?2026hframe", tail: " more\x1b[?2026l", gaps: ["sync-2026"] },
  { name: "2026-ed2-ed3", prefix: `${lines(10)}\x1b[?2026h\x1b[2J\x1b[3J\x1b[Hnew`, tail: "\x1b[?2026l", gaps: ["sync-2026"] },
  { name: "2026-ris", prefix: "\x1b[?2026htext\x1bcafter", tail: "!", gaps: [], cutGaps: ["sync-2026"] },
  { name: "2026-no-end", prefix: "\x1b[?2026hA", tail: "B", gaps: ["sync-2026"] },
  { name: "2026-closed", prefix: "\x1b[?2026hA\x1b[?2026l", tail: "B", gaps: [], cutGaps: ["sync-2026"] },
  // DECSTR (CSI ! p): xterm.js softReset сбрасывает все шесть режимов трекера
  // зеркала — кадр обязан их НЕ воскрешать (ревью C-conformance: курсор скрыт
  // навсегда, вставка портит ввод; is2 у `tput init`). Хвост проверяет перенос
  // у края и вставку уже после сброса. На назначенном разрезе — положительная
  // регрессия без разрывов; на разрезах внутри хвоста — отложенный перенос и
  // DECAWM, который vt после DECSTR так и держит выключенным.
  { name: "decstr", prefix: "\x1b[?1h\x1b[4h\x1b[?25l\x1b[?7l\x1b=\x1b[?6h\x1b[!ptext", tail: "\x1b[2;1H0123456789ABCDEFGHIJKLM", gaps: [], cutGaps: ["pending-wrap", "soft-wrap-flags", "vt-decstr-ignored"] },
  // decTracker (decmodes.go) знает DECSTR с 14.09: ?2004 и ?1004 кадр больше не
  // воскрешает — положительная регрессия, различий ноль.
  { name: "decstr-dec-tracker", prefix: "\x1b[?2004h\x1b[?1004h$ \x1b[!p", tail: "x", gaps: [] },
  // vt DECSTR игнорирует: перо переживает сброс — клетка той же буквы у
  // зеркала красная (узкое поле *.screen*.style).
  { name: "decstr-vt-pen", prefix: "\x1b[31m\x1b[!pX", tail: "Y", gaps: ["vt-decstr-ignored"], cutGaps: ["sgr-pen"] },
  // …и область DECSTBM тоже: LF у низа области прокручивает у зеркала регион,
  // у xterm уходит ниже. Сетка B == сетке зеркала ≠ A — строгое правило
  // «клиент = xterm ИЛИ зеркало» оправдывает её только объявленным разрывом.
  { name: "decstr-vt-region", prefix: `${lines(6)}\x1b[2;4r\x1b[!p\x1b[4;1H\nX`, tail: "\r\nZ", gaps: ["vt-decstr-ignored"], cutGaps: ["scroll-region", "region-scrollback-leak"] },
  // Найдены строгим C-03 (клиент = xterm ИЛИ зеркало) после ревью.
  // vt на ED1 стирает строку курсора целиком (seed 20260920).
  { name: "ed1", prefix: "vuYn+;V4qcaL3vC\x1b[1;8H\x1b[1J", tail: "Z", gaps: ["vt-ed1-whole-line"] },
  // …и лишние стёртые клетки красит фоном пера (BCE): символ тот же пробел.
  { name: "ed1-bce", prefix: "\x1b[42m\x1b[1J\x1b[m", tail: "Z", gaps: ["vt-ed1-whole-line"], cutGaps: ["sgr-pen"] },
  // vt на ?1049l вне alt не делает DECRC: перо живёт дальше (seed 20260917).
  { name: "1049l-outside-alt", prefix: "\x1b[1m\x1b[?1049lGb8=", tail: "x", gaps: ["vt-1049l-no-restore"], cutGaps: ["sgr-pen"] },
  // NFD «é» (e + U+0301, так пишет имена файлов macOS): vt кладёт знак в свою
  // клетку, кадр печатает её как есть, строка клиента съезжает (seed 20260919).
  { name: "nfd-combining", prefix: "L313 J\x1b[1;1Hé", tail: "x", gaps: [], cutGaps: ["vt-combining-split", "frame-orphan-combining"] },
  // Составимую пару (e + U+0301 → é) зеркало собирает NFC на входе
  // (screen.go composeForMirror, 14.09): выше — положительная регрессия. У «q»
  // составной формы нет — знак по-прежнему ложится в свою клетку vt.
  { name: "nfd-noncomposable", prefix: "L313 J\x1b[1;1Hq́", tail: "x", gaps: ["vt-combining-split", "frame-orphan-combining"] },
  // vt на RIS историю не стирал (найдено C-03R, seed 20261118): после `reset`
  // клиент получал историю, которой у xterm уже нет. С 15.09 зеркало стирает
  // её само (screen_vtpanic.go) — положительная регрессия, различий ноль.
  { name: "ris-keeps-history", prefix: `${lines(10)}\x1bc$ `, tail: "x", gaps: [] },
];

// Смена геометрии (§6 после финального ревью: resize до, во время и после
// снимка). Геометрия в продукте меняется у всех сторон в одной точке потока:
// term.resize у непрерывного xterm и у восстановленного клиента, Resize у
// зеркала (маркер ACK в FIFO). HIST — история (10 строк в 6-строчном экране) и
// строка шире экрана (мягкий перенос); ALT — полноэкранное приложение поверх
// оболочки.
const HIST = `${lines(10)}\r\n${"w".repeat(COLS + 6)}\r\n$ `;
const ALT = `shell1\r\nshell2\x1b[?1049h\x1b[H\x1b[2JA1 alt line one\r\nA2\r\nA3\r\nA4\r\nA5\r\nA6 bottom`;
const WIDE_HIST = Array.from({ length: 8 }, (_, i) => `W${i + 1}`.padEnd(COLS - 1, "=")).join("\r\n");
// Разрезы внутри HIST — перенос у края строки «w…» (как wrap-at-edge и
// soft-wrap); внутри ALT — вход в alt (как alt-exit).
const HIST_CUTS = ["pending-wrap", "soft-wrap-flags"];
const ALT_CUTS = ["alt-underlying-normal", "alt-enter-cursor-home", "saved-cursor"];
const RESIZE_FIXTURES: Fixture[] = [
  // ДО снимка: зеркало и xterm расходятся на самой смене геометрии.
  withResizes({ name: "resize-before-cols-shrink", prefix: [HIST, { cols: 12, rows: ROWS }], tail: ["ls\r\nout"], gaps: ["vt-resize-no-reflow", "soft-wrap-flags"], cutGaps: HIST_CUTS }),
  withResizes({ name: "resize-before-cols-grow", prefix: [HIST, { cols: 32, rows: ROWS }], tail: ["x"], gaps: ["vt-resize-no-reflow"], cutGaps: HIST_CUTS }),
  withResizes({ name: "resize-before-rows-shrink", prefix: [HIST, { cols: COLS, rows: 3 }], tail: ["x"], gaps: ["vt-resize-height", "soft-wrap-flags"], cutGaps: HIST_CUTS }),
  withResizes({ name: "resize-before-rows-grow", prefix: [HIST, { cols: COLS, rows: 10 }], tail: ["x"], gaps: ["vt-resize-height", "soft-wrap-flags"], cutGaps: HIST_CUTS }),
  // МЕЖДУ снимком и хвостом: снимок в старой геометрии, клиент меняет её сам.
  withResizes({ name: "resize-between-shrink", prefix: [HIST], tail: [{ cols: 12, rows: 4 }, "ls\r\nout"], gaps: ["soft-wrap-flags", "frame-reflow-without-wrap"], cutGaps: HIST_CUTS }),
  withResizes({ name: "resize-between-grow", prefix: [HIST], tail: [{ cols: 32, rows: 10 }, "x"], gaps: ["soft-wrap-flags", "frame-reflow-without-wrap"], cutGaps: HIST_CUTS }),
  // ПОСЛЕ хвоста.
  withResizes({ name: "resize-after-tail-shrink", prefix: [HIST], tail: ["ls\r\nout", { cols: 12, rows: 3 }], gaps: ["soft-wrap-flags", "frame-reflow-without-wrap"], cutGaps: HIST_CUTS }),
  withResizes({ name: "resize-after-tail-grow", prefix: [HIST], tail: ["x", { cols: 32, rows: 10 }], gaps: ["soft-wrap-flags", "frame-reflow-without-wrap"], cutGaps: HIST_CUTS }),
  // alt-экран: normal под ним кадром не переносится (alt-underlying-normal).
  withResizes({ name: "alt-resize-before-shrink", prefix: [ALT, { cols: 12, rows: 3 }], tail: ["Z"], gaps: ["alt-underlying-normal", "vt-resize-height"], cutGaps: ALT_CUTS }),
  withResizes({ name: "alt-resize-before-grow", prefix: [ALT, { cols: 32, rows: 10 }], tail: ["Z"], gaps: ["alt-underlying-normal"], cutGaps: ALT_CUTS }),
  withResizes({ name: "alt-resize-between-exit", prefix: [ALT], tail: [{ cols: 12, rows: 3 }, "Z\x1b[?1049lback"], gaps: ["alt-underlying-normal"], cutGaps: ALT_CUTS }),
  withResizes({ name: "alt-resize-after-tail", prefix: [ALT], tail: ["Z\x1b[?1049lback", { cols: 32, rows: 10 }], gaps: ["alt-underlying-normal"], cutGaps: ALT_CUTS }),
  // Бывший дефект клиента history-seam-wide-lines (seed 20261006): история
  // зеркала напечатана в 20 колонках, клиент в 16 — строки W1…W8 по 19
  // символов занимают по два ряда. Досылка «по строкам» уводила в scrollback
  // вдвое меньше рядов, и свежие строки стирались кадром. С 15.09 цель досылки
  // — все ряды истории (snapshotApply): положительная регрессия, шов без потерь.
  withResizes({ name: "resize-history-wider-than-client", prefix: [WIDE_HIST, { cols: 16, rows: ROWS }], tail: ["\r\nx"], gaps: ["vt-resize-no-reflow", "soft-wrap-flags"] }),
  // ДЕФЕКТ КАДРА frame-wide-last-column (seed 20261123): сужение обрезает
  // строку по «中», графема остаётся в последней колонке у xterm и у зеркала;
  // кадр её не печатает (иначе клиент перенёс бы её на следующую строку).
  // Разрез после «x» — буква у края (отложенный перенос, как wrap-at-edge).
  withResizes({ name: "resize-wide-last-column", prefix: ["abcdefghijk中", { cols: 12, rows: ROWS }], tail: ["x"], gaps: ["frame-wide-last-column"], cutGaps: ["pending-wrap"] }),
  // Без расхождений по построению.
  withResizes({ name: "resize-pending-wrap-grow", prefix: [`\x1b[2;1H${"x".repeat(COLS)}`, { cols: 30, rows: ROWS }], tail: ["BC"], gaps: [] }),
  withResizes({ name: "resize-rows-shrink-cursor-top", prefix: ["top\r\nmid\r\nbottom\x1b[1;1H", { cols: COLS, rows: 2 }], tail: ["X"], gaps: [] }),
  withResizes({ name: "resize-shell-grow-both", prefix: ["PS> ls\r\nfile1 file2\r\nPS> ", { cols: 32, rows: 10 }], tail: ["echo hi\r\nhi\r\nPS> "], gaps: [] }),
];

const ALL_FIXTURES: Fixture[] = [...FIXTURES, ...RESIZE_FIXTURES];

const summary: string[] = [];

describe("эмуляция стенда совпадает с клиентом (§6.1)", () => {
  it("версии browser xterm и headless совпадают с объявленной", () => {
    const ver = (p: string) => (JSON.parse(readFileSync(join(REPO, p), "utf8")) as { version: string }).version;
    const browser = ver("apk/node_modules/@xterm/xterm/package.json");
    const headless = ver("node_modules/@xterm/headless/package.json");
    expect(emulationMismatches({ xtermVersion: browser })).toEqual([]);
    expect(emulationMismatches({ xtermVersion: headless })).toEqual([]);
    const t = newTerm();
    expect(emulationMismatches({ unicodeVersion: t.unicode.activeVersion, scrollback: t.options.scrollback })).toEqual([]);
    t.dispose();
  });
});

// C-07: RetentionPolicy — отдельно от live-семантики. Go не нужен.
describe("RetentionPolicy против чистого потока (C-07)", () => {
  const plan = [3, 7];
  const chunksOf = (b: Uint8Array) => {
    const out: Uint8Array[] = [];
    for (let off = 0, i = 0; off < b.byteLength; i++) {
      const n = Math.min(plan[i % plan.length], b.byteLength - off);
      out.push(b.subarray(off, off + n));
      off += n;
    }
    return out;
  };
  async function strip(stream: Uint8Array): Promise<TerminalState> {
    const t = newTerm();
    let tail: Uint8Array = EMPTY_BYTES;
    for (const chunk of chunksOf(stream)) {
      const r = stripScrollbackErase(concat(tail, chunk));
      tail = r.tail;
      if (r.data.byteLength > 0) await write(t, r.data);
    }
    expect(tail.byteLength).toBe(0);
    const s = capture(t);
    t.dispose();
    return s;
  }
  async function splitWrite(stream: Uint8Array): Promise<TerminalState> {
    const t = newTerm();
    let tail: Uint8Array = EMPTY_BYTES;
    for (const chunk of chunksOf(stream)) {
      const r = splitScrollbackErase(concat(tail, chunk));
      tail = r.tail;
      for (const it of r.items) await write(t, it.kind === "data" ? it.data : SCROLLBACK_ERASE);
    }
    const s = capture(t);
    t.dispose();
    return s;
  }
  const kimi = enc(`${lines(10)}\x1b[2J\x1b[3J\x1b[H${lines(8, 100)}\x1b[2J\x1b[3J\x1b[H${lines(9, 200)}`);

  it("вырезание ESC[3J меняет только scrollback; экран, курсор и режимы совпадают", async () => {
    const a = (await continuous(kimi, [])).end;
    const r = diffTerminalState(a, await strip(kimi), ["retention-ed3"], "snapshot");
    expect(r.unexplained, describeDiffs(r.unexplained)).toEqual([]);
    expect(vanishedGaps(["retention-ed3"], [r])).toEqual([]);
  });

  it("стирание, досланное по ходу разбора (у низа), даёт ровно чистый поток", async () => {
    const a = (await continuous(kimi, [])).end;
    const r = diffTerminalState(a, await splitWrite(kimi), [], "snapshot");
    expect(r.all, describeDiffs(r.all)).toEqual([]);
  });

  it("без ESC[3J политика ничего не меняет — объявленный retention-ed3 исчезает", async () => {
    const plain = enc(`${lines(15)}\x1b[2J\x1b[Hx`);
    const a = (await continuous(plain, [])).end;
    const r = diffTerminalState(a, await strip(plain), ["retention-ed3"], "snapshot");
    expect(r.all).toEqual([]);
    expect(vanishedGaps(["retention-ed3"], [r])).toEqual(["retention-ed3"]);
  });
});

describe.skipIf(!GO)("снимок Go-зеркала + хвост против непрерывного потока (§6, T-26/T-27/T-29)", () => {
  beforeAll(() => {
    tmp = mkdtempSync(join(tmpdir(), "remotai-conformance-"));
    tool = join(tmp, process.platform === "win32" ? "screen-frame.exe" : "screen-frame");
    execFileSync("go", ["build", "-o", tool, "./tools/screen-frame"], { cwd: REPO, stdio: "pipe", timeout: 300_000 });
    summary.push(`build: ${GO}; tool ${tool}; xterm ${TERMINAL_EMULATION.xtermVersion}; unicode ${TERMINAL_EMULATION.unicodeVersion}`);
  }, 320_000);

  afterAll(() => {
    if (summary.length > 0) console.log(`[conformance]\n${summary.join("\n")}`);
    if (keptDir) console.log(`[conformance] минимальные входы C-03 сохранены: ${keptDir}`);
    // Рабочий каталог — удалить: без этого каждый прогон (и каждый
    // qa:terminal после регистрации файла) оставлял в TEMP 7–8 МБ (ревью
    // C-conformance). CONFORMANCE_KEEP=1 — оставить для разбора.
    if (!tmp) return;
    if (process.env.CONFORMANCE_KEEP) {
      console.log(`[conformance] рабочий каталог сохранён (CONFORMANCE_KEEP): ${tmp}`);
      return;
    }
    try {
      rmSync(tmp, { recursive: true, force: true, maxRetries: 5, retryDelay: 200 });
    } catch (e) {
      console.warn(`[conformance] не удалось удалить ${tmp}: ${String(e)}`);
    }
  });

  describe("C-01: назначенный разрез фикстуры, в момент снимка и после хвоста", () => {
    for (const fx of ALL_FIXTURES) {
      it(fx.name, async () => {
        const prefix = enc(fx.prefix);
        const stream = concat(prefix, enc(fx.tail));
        const cut = prefix.byteLength;
        const resizes = fx.resizes ?? [];
        const [snap] = snapshots(stream, { cuts: [cut], idle: true, resizes });
        expect(snap.ready, `${fx.name}: снимок на назначенном разрезе withheld`).toBe(true);
        expect(snap.appliedOff).toBe(cut);
        const a = await continuous(stream, [cut], COLS, ROWS, resizes);
        expect(a.ground.get(cut)).toBe(true);
        const b = await restored(snap, stream, cut, resizes);
        const rSnap = diffSnapshotAgainstMirror(a.states.get(cut)!, b.atCut, mirrorOf(snap), fx.gaps);
        const rTail = diffTerminalState(a.end, b.end, fx.gaps, "tail");
        assertConforms(fx.name, rSnap, rTail);
        expect(vanishedGaps(fx.gaps, [rSnap, rTail]), `${fx.name}: объявленный разрыв исчез — снимите его из фикстуры и реестра`).toEqual([]);
        summary.push(`C-01 ${fx.name}: replace=${b.replace}; разрывы ${fx.gaps.map((g) => `${g}×${(rSnap.evidence.get(g)?.length ?? 0) + (rTail.evidence.get(g)?.length ?? 0)}`).join(", ") || "нет"}`);
      }, 60_000);
    }
  });

  describe("C-02: все разрезы малых фикстур, планы кусков, idle и не idle", () => {
    const PLANS: number[][] = [[], [1], [3, 5]];
    for (const fx of ALL_FIXTURES.filter((f) => f.allCuts !== false)) {
      it(fx.name, async () => {
        const stream = concat(enc(fx.prefix), enc(fx.tail));
        const cuts = Array.from({ length: stream.byteLength + 1 }, (_, i) => i);
        const resizes = fx.resizes ?? [];
        const a = await continuous(stream, cuts, COLS, ROWS, resizes);
        const allowed = [...new Set([...fx.gaps, ...(fx.cutGaps ?? [])])];
        const cache = new Map<string, Awaited<ReturnType<typeof restored>>>();
        let ready = 0;
        let withheld = 0;
        for (const chunks of PLANS) {
          for (const idle of [false, true]) {
            for (const s of snapshots(stream, { cuts: "all", chunks, idle, resizes })) {
              const label = `${fx.name} cut=${s.cut} plan=[${chunks}] idle=${idle}`;
              const ground = a.ground.get(s.cut)!;
              // T-27: разрез вне границы парсера xterm — кадр обязан быть withheld.
              if (!ground) expect(s.ready, `${label}: кадр выдан посреди последовательности`).toBe(false);
              // Живость: при тишине граница парсера — всегда готовый снимок.
              if (idle && ground) expect(s.ready, `${label}: снимок на границе withheld`).toBe(true);
              if (!s.ready) { withheld++; continue; }
              ready++;
              expect(s.appliedOff, label).toBe(s.cut);
              const key = `${s.cut} ${s.frame} ${s.history} ${s.histLines}`;
              let b = cache.get(key);
              if (!b) {
                b = await restored(s, stream, s.cut, resizes);
                cache.set(key, b);
              }
              assertConforms(label,
                diffSnapshotAgainstMirror(a.states.get(s.cut)!, b.atCut, mirrorOf(s), allowed),
                diffTerminalState(a.end, b.end, allowed, "tail"));
            }
          }
        }
        expect(ready).toBeGreaterThan(0);
        summary.push(`C-02 ${fx.name}: ${stream.byteLength} Б, готово ${ready}, withheld ${withheld}, уникальных восстановлений ${cache.size}`);
      }, 120_000);
    }
  });

  describe("C-03: seed-генерация, выборка разрезов, случайные планы кусков", () => {
    const baseSeed = Number(process.env.CONFORMANCE_SEED || 20260913);
    const streams = Number(process.env.CONFORMANCE_STREAMS || 8);
    const cutsPer = Number(process.env.CONFORMANCE_CUTS || 16);

    /**
     * Проверка одного входа; вернёт текст дефекта или null. allowed — разрывы
     * операций ДО разреза (gapsBeforeCut). Момент снимка — строгое правило
     * против самого зеркала (diffSnapshotAgainstMirror): широкие поля
     * библиотечных разрывов сетку, курсор и историю не прячут.
     */
    async function check(
      stream: Uint8Array, cut: number, chunks: number[], idle: boolean, allowed: string[],
      resizes: readonly StreamResize[] = [], allowedTail: string[] = allowed,
    ): Promise<string | null> {
      const a = await continuous(stream, [cut], COLS, ROWS, resizes);
      const [s] = snapshots(stream, { cuts: [cut], chunks, idle, resizes });
      if (!a.ground.get(cut) && s.ready) return "кадр выдан посреди последовательности";
      if (!s.ready) return null;
      if (s.appliedOff !== cut) return `база ${s.appliedOff} != разрез ${cut}`;
      const b = await restored(s, stream, cut, resizes);
      const r1 = diffSnapshotAgainstMirror(a.states.get(cut)!, b.atCut, mirrorOf(s), allowed);
      const r2 = diffTerminalState(a.end, b.end, allowedTail, "tail");
      if (r1.unexplained.length) return `в момент снимка:\n${describeDiffs(r1.unexplained)}`;
      if (r2.unexplained.length) return `после хвоста:\n${describeDiffs(r2.unexplained)}`;
      return null;
    }

    /**
     * Жадное сокращение по операциям генератора при сохранении дефекта.
     * Разрешённые разрывы пересчитываются по оставшимся операциям ДО разреза:
     * выброшенная операция уносит и своё право на расхождение.
     */
    async function shrink(pre: StreamOp[], post: StreamOp[], chunks: number[], idle: boolean) {
      const bytesOf = (xs: StreamOp[]) => enc(xs.map((o) => o.text).join(""));
      // Хвостовые разрывы — у resize, оставшихся в хвосте; resize на границе
      // достаётся той стороне, где он лежит (resizesOf со splitAt).
      const tailOf = (p: StreamOp[], q: StreamOp[]) => [...new Set([...joinOps(0, p).gaps, ...q.flatMap((o) => o.tailGaps ?? [])])].sort();
      const fails = async (p: StreamOp[], q: StreamOp[]) => {
        const head = bytesOf(p);
        return (await check(concat(head, bytesOf(q)), head.byteLength, chunks, idle, joinOps(0, p).gaps,
          resizesOf([...p, ...q], p.length), tailOf(p, q))) !== null;
      };
      for (let i = post.length - 1; i >= 0; i--) {
        const q = post.filter((_, k) => k !== i);
        if (await fails(pre, q)) post = q;
      }
      for (let i = pre.length - 2; i >= 0; i--) {
        const p = pre.filter((_, k) => k !== i);
        if (await fails(p, post)) pre = p;
      }
      const head = bytesOf(pre);
      return {
        stream: concat(head, bytesOf(post)), cut: head.byteLength, allowed: joinOps(0, pre).gaps,
        allowedTail: tailOf(pre, post), resizes: resizesOf([...pre, ...post], pre.length),
      };
    }

    // Перезапуск сохранённого минимального входа (или папки с ними): печатает
    // различия, ничего не разрешая сверх записанного в .json.
    const repro = process.env.CONFORMANCE_REPRO;
    if (repro) {
      it(`repro ${repro}`, async () => {
        // Только сохранённые входы (у них есть .json), не рабочие файлы инструмента.
        const files: string[] = statSync(repro).isDirectory()
          ? (readdirSync(repro) as string[]).map((f: string) => join(repro, f) as string)
            .filter((f: string) => f.endsWith(".bin") && existsSync(`${f}.json`))
          : [repro];
        const out: string[] = [];
        for (const f of files) {
          const meta = JSON.parse(readFileSync(`${f}.json`, "utf8")) as {
            cut: number; chunks: number[]; idle: boolean; allowed: string[]; allowedTail?: string[]; resizes?: StreamResize[];
          };
          const stream = new Uint8Array(readFileSync(f));
          const td = new TextDecoder();
          const why = await check(stream, meta.cut, meta.chunks, meta.idle, meta.allowed, meta.resizes ?? [], meta.allowedTail ?? meta.allowed);
          out.push(`${f}\n  prefix ${JSON.stringify(td.decode(stream.subarray(0, meta.cut)))}\n  tail ${JSON.stringify(td.decode(stream.subarray(meta.cut)).slice(0, 120))}\n  → ${why ?? "совпало"}`);
        }
        console.log(out.join("\n"));
      }, 600_000);
      return;
    }

    /**
     * Один seed. resize — отдельный набор (C-03R): генератор со сменами
     * геометрии; без него поток, план кусков и разрезы seed прежние. Ничья на
     * resize ровно на разрезе — по чётности seed: нечётный отдаёт его хвосту
     * («между снимком и хвостом»), чётный — префиксу («до снимка»).
     */
    async function runSeed(seed: number, resize: boolean): Promise<void> {
      const g = generateStream(seed, { cols: COLS, rows: ROWS, ops: 50, resize });
      const tie: ResizeTie = resize && seed % 2 === 1 ? "tail" : "prefix";
      const resizes = resizesOf(g.ops, tie === "tail" ? 0 : g.ops.length);
      const chunks = generateChunkPlan(seed);
      const rnd = mulberry32(seed ^ 0x5bd1e995);
      const idle = rnd() < 0.5;
      const bytes = g.bytes;
      // Разрезы: половина на границах операций, половина — любой байт
      // (в том числе посреди CSI/OSC/UTF-8).
      const bounds: number[] = [];
      let acc = 0;
      for (const op of g.ops) { acc += enc(op.text).byteLength; bounds.push(acc); }
      const cuts = new Set<number>([bytes.byteLength]);
      while (cuts.size < cutsPer) {
        cuts.add(rnd() < 0.5 ? bounds[Math.floor(rnd() * bounds.length)] : Math.floor(rnd() * bytes.byteLength));
      }
      // C-03R: и разрезы ровно на каждом resize — ничья должна быть проверена.
      for (const r of resizes) cuts.add(r.off);
      const sorted = [...cuts].sort((x, y) => x - y);
      const a = await continuous(bytes, sorted, COLS, ROWS, resizes);
      let ready = 0;
      let refused = 0;
      let libraryCells = 0;
      let tailExplained = 0;
      const tag = resize ? "C-03R" : "C-03";
      for (const s of snapshots(bytes, { cuts: sorted, chunks, idle, resizes })) {
        const label = `${tag} seed=${seed} cut=${s.cut}/${bytes.byteLength} plan=[${chunks}] idle=${idle}${resize ? ` ничья=${tie}` : ""}`;
        if (!a.ground.get(s.cut)) expect(s.ready, `${label}: кадр выдан посреди последовательности`).toBe(false);
        // Отказ после паники vt — честный разрыв иного рода: снимка нет до RIS,
        // сравнивать нечего. Не прячется: считается в сводке.
        if (s.untrusted) {
          expect(s.ready, `${label}: кадр из недостоверного зеркала`).toBe(false);
          refused++;
          continue;
        }
        if (!s.ready) continue;
        ready++;
        // Разрывы — только операций ДО разреза: хвост у A и B общий. После
        // хвоста ещё и хвостовые разрывы resize, оставшихся в хвосте.
        const allowed = gapsBeforeCut(g.ops, s.cut, tie);
        const allowedTail = [...new Set([...allowed, ...gapsAfterCut(g.ops, s.cut, tie)])].sort();
        const b = await restored(s, bytes, s.cut, resizes);
        const r1 = diffSnapshotAgainstMirror(a.states.get(s.cut)!, b.atCut, mirrorOf(s), allowed);
        const r2 = diffTerminalState(a.end, b.end, allowedTail, "tail");
        libraryCells += r1.all.length - r1.unexplained.length;
        tailExplained += r2.all.length - r2.unexplained.length;
        if (r1.unexplained.length || r2.unexplained.length) {
          // Минимальный вход — на диск (отдельный keptDir, afterAll его не
          // удаляет); в сообщение seed, путь и различия именно минимального
          // входа (перезапуск: CONFORMANCE_REPRO=<файл|папка>). Сокращать
          // можно только разрез на границе операций.
          const n = prefixLength(g.ops, s.cut, tie);
          const atBoundary = n > 0 && enc(g.ops.slice(0, n).map((o) => o.text).join("")).byteLength === s.cut;
          let min = { stream: bytes, cut: s.cut, allowed, allowedTail, resizes };
          if (atBoundary) min = await shrink(g.ops.slice(0, n), g.ops.slice(n), chunks, idle);
          const file = join(reproDir(), `seed-${seed}-cut-${min.cut}.bin`);
          writeFileSync(file, min.stream);
          writeFileSync(`${file}.json`, JSON.stringify({ seed, cut: min.cut, chunks, idle, allowed: min.allowed, allowedTail: min.allowedTail, resizes: min.resizes }, null, 1));
          const why = (await check(min.stream, min.cut, chunks, idle, min.allowed, min.resizes, min.allowedTail)) ?? "(на минимальном входе не воспроизвелось)";
          throw new Error(`${label}: необъяснённые различия.\nМинимальный вход ${file} (разрез ${min.cut}, resize ${resizesArg(min.resizes) || "нет"}): ${why}\nИсходно:\n${describeDiffs([...r1.unexplained, ...r2.unexplained])}`);
        }
      }
      const geo = resize ? `, resize ${resizesArg(resizes)} (ничья ${tie}), объяснённых после хвоста ${tailExplained}` : "";
      const refusals = refused > 0 ? `, ОТКАЗОВ после паники vt ${refused} (снимка нет до RIS)` : "";
      summary.push(`${tag} seed ${seed}: ${bytes.byteLength} Б, разрезов ${sorted.length}, готово ${ready}${refusals}, объяснённых различий в момент снимка ${libraryCells}${geo}, plan [${chunks}], idle ${idle}, разрывы потока: ${g.gaps.join(",")}`);
    }

    for (let n = 0; n < streams; n++) {
      const seed = baseSeed + n;
      it(`seed ${seed}`, () => runSeed(seed, false), 120_000);
    }

    // C-03R: отдельный набор seed со сменами геометрии (resize до снимка, на
    // самом разрезе с обеими ничьими, в хвосте и последним событием потока).
    // Прежние seed C-03 не меняются: без флага resize генератор тот же.
    describe("C-03R: seed-генерация со сменами геометрии (resize)", () => {
      const resizeSeed = Number(process.env.CONFORMANCE_RESIZE_SEED || 20261001);
      const resizeStreams = Number(process.env.CONFORMANCE_RESIZE_STREAMS || 8);
      for (let n = 0; n < resizeStreams; n++) {
        const seed = resizeSeed + n;
        it(`seed ${seed}`, () => runSeed(seed, true), 180_000);
      }
    });
  });

  // C-08: страховочная сетка на НАСТОЯЩЕЙ панике библиотеки. Предохранители
  // (internal/pty/screen_vtpanic.go) не дают vt паниковать на известных входах,
  // поэтому здесь они сняты (-no-vt-guards), и P1 (низ DECSTBM за высотой + DL)
  // паникует. Отказ — не различие состояния: снимок withheld с untrusted, пока
  // RIS не разобран пересобранным эмулятором; до паники и после RIS — обычная
  // сверка с непрерывным xterm (у которого низ области зажат по высоте).
  describe("C-08: паника vt без предохранителей — честный отказ до RIS", () => {
    const PRE = `${lines(3)}\r\n\x1b[2;9r\x1b[4;1H`;
    const text = `${PRE}\x1b[M zz\x1bcafter\r\nreset$ `;
    const stream = enc(text);
    // Разрез после финального «M» — DL разобран, зеркало паникует; после «c»
    // RIS разобран новым эмулятором.
    const panicAt = enc(PRE).byteLength + 3;
    const risAt = enc(`${PRE}\x1b[M zz\x1bc`).byteLength;
    const allowed = ["scroll-region", "region-scrollback-leak"];
    it("P1 на всех разрезах, планы кусков, idle и не idle", async () => {
      const cuts = Array.from({ length: stream.byteLength + 1 }, (_, i) => i);
      const a = await continuous(stream, cuts);
      let ready = 0;
      let refused = 0;
      for (const chunks of [[], [1], [3, 5]]) {
        for (const idle of [false, true]) {
          for (const s of snapshots(stream, { cuts: "all", chunks, idle, noVtGuards: true })) {
            const label = `C-08 cut=${s.cut} plan=[${chunks}] idle=${idle}`;
            if (s.cut >= panicAt && s.cut < risAt) {
              expect(s.ready, `${label}: кадр из недостоверного зеркала`).toBe(false);
              expect(s.untrusted, `${label}: отказ не помечен`).toBe(true);
              refused++;
              continue;
            }
            expect(s.untrusted ?? false, `${label}: пометка без паники или после RIS`).toBe(false);
            const ground = a.ground.get(s.cut)!;
            if (!ground) expect(s.ready, `${label}: кадр выдан посреди последовательности`).toBe(false);
            if (idle && ground) expect(s.ready, `${label}: снимок на границе withheld`).toBe(true);
            if (!s.ready) continue;
            ready++;
            const b = await restored(s, stream, s.cut);
            assertConforms(label,
              diffSnapshotAgainstMirror(a.states.get(s.cut)!, b.atCut, mirrorOf(s), allowed),
              diffTerminalState(a.end, b.end, allowed, "tail"));
          }
        }
      }
      expect(refused).toBeGreaterThan(0);
      expect(ready).toBeGreaterThan(0);
      summary.push(`C-08 паника vt (без предохранителей): ${stream.byteLength} Б, готово ${ready}, отказов до RIS ${refused}`);
    }, 120_000);
  });

  describe("Unicode: информационный wide-корпус, строгое правило C==A или C==B", () => {
    const WIDE: Fixture[] = [
      { name: "emoji-zwj-flags", prefix: "a\u{1F600}b \u{1F468}‍\u{1F469}‍\u{1F467} ☀️ \u{1F1F7}\u{1F1FA} \u{1F44D}\u{1F3FD}\r\n", tail: "c", gaps: ["unicode-width-v6-vs-grapheme"] },
      { name: "emoji-edge", prefix: `${"w".repeat(COLS - 1)}\u{1F600}x`, tail: "y", gaps: ["unicode-width-v6-vs-grapheme"] },
    ];
    for (const fx of WIDE) {
      it(fx.name, async () => {
        const prefix = enc(fx.prefix);
        const stream = concat(prefix, enc(fx.tail));
        const cut = prefix.byteLength;
        const [snap] = snapshots(stream, { cuts: [cut], idle: true });
        expect(snap.ready).toBe(true);
        const a = await continuous(stream, [cut]);
        const b = await restored(snap, stream, cut);
        const bad = cellRuleMismatches(activeGrid(a.states.get(cut)!), mirrorOf(snap).grid, activeGrid(b.atCut));
        expect(bad, `${fx.name}: кадр дал третье значение в клетке`).toEqual([]);
        // Безопасность разрезов для wide — строго; остальное — информационно.
        const all = snapshots(stream, { cuts: "all", chunks: [1] });
        const ga = await continuous(stream, all.map((s) => s.cut));
        for (const s of all) if (!ga.ground.get(s.cut)) expect(s.ready, `${fx.name} cut=${s.cut}`).toBe(false);
        const rSnap = diffTerminalState(a.states.get(cut)!, b.atCut, [], "snapshot");
        const rTail = diffTerminalState(a.end, b.end, [], "tail");
        summary.push(`wide ${fx.name} (информационно): различий в момент снимка ${rSnap.all.length}, после хвоста ${rTail.all.length}; третье значение: 0`);
      }, 60_000);
    }
  });
});
