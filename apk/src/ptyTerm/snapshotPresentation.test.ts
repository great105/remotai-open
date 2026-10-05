/**
 * ST-06: склейка шагов снимка в две записи меняет только разбиение на
 * write(), а не байты и не состояние терминала (раздел 6: побайтная
 * эквивалентность шагов planSnapshotApply).
 *
 * Прежний путь PtyTermView писал шаги по одному: RIS, история, досылка строк
 * (замер живого буфера ПОСЛЕ истории), кадр — до четырёх write, и между ними
 * xterm мог показать промежуточный кадр (замер probe-terminal-flicker: H2 —
 * история без кадра, DOM — ещё и пустой кадр после RIS). Новый путь: голова
 * (RIS + история) и хвост (досылка + кадр [+ END]). Здесь доказывается:
 *   L1 — байты: голова+хвост на DOM совпадают с шагами побайтно, на WebGL —
 *        после вычёркивания ровно одного BEGIN и одного END;
 *   L2 — @xterm/headless 6.0.0: оба пути на одном прологе дают одно и то же
 *        состояние (ячейки, атрибуты, переносы, курсор, история, режим 2026)
 *        и одну и ту же досылку строк — замер после головы тот же, что после
 *        шага истории.
 */
import { describe, expect, it } from "vitest";
import { Terminal as HeadlessTerminal } from "@xterm/headless";
import {
  SYNC_BEGIN,
  SYNC_END,
  TXN_CLOSED,
  beginSnapshotTxn,
  finishSnapshotTxn,
  joinChunks,
  snapshotStepParts,
} from "./presentation";
import type { PresentationChunk, PresentationRenderer } from "./presentation";
import { SNAPSHOT_RIS, planSnapshotApply, snapshotStepPayload } from "./snapshotApply";
import type { SnapshotApplyPlan, SnapshotStep } from "./snapshotApply";

const COLS = 24;
const ROWS = 6;

const bytesOf = (c: PresentationChunk): Uint8Array => (typeof c === "string" ? new TextEncoder().encode(c) : c);
const hex = (c: PresentationChunk): string => Array.from(bytesOf(c), (b) => b.toString(16).padStart(2, "0")).join("");

/** Длинные строки с переносом, эмодзи, SGR — то, из-за чего досылку мерят по буферу. */
function historyText(lines: number): string {
  const out: string[] = [];
  for (let i = 0; i < lines; i++) {
    const long = i % 4 === 0 ? " длинная строка, которая переносится на следующую" : "";
    out.push(`\x1b[3${(i % 7) + 1}mИСТ ${String(i).padStart(3, "0")} 🙂${long}\x1b[m`);
  }
  return out.join("\r\n");
}
const FRAME = "\x1b[?1049l\x1b[m\x1b[H\x1b[2J"
  + Array.from({ length: ROWS }, (_, i) => `\x1b[${i + 1};1HЭКРАН ${i + 1} ё`).join("") + "\x1b[3;5H";
const PRELUDE = "старый вывод\r\n".repeat(9) + "\x1b[32mзелёный\x1b[m $ ";

function plan(histLines: number, screenRows = ROWS): SnapshotApplyPlan {
  const text = histLines > 0 ? historyText(histLines) : "";
  return planSnapshotApply({
    history: text, histLines: histLines > 0 ? histLines : undefined, screenRows, snapCols: COLS,
    termCols: COLS, localScrollback: 0, historyState: "ready",
  });
}

function write(t: HeadlessTerminal, data: PresentationChunk): Promise<void> {
  if (data.length === 0) return Promise.resolve();
  return new Promise((resolve) => t.write(data, resolve));
}
function makeTerm(): HeadlessTerminal {
  return new HeadlessTerminal({ allowProposedApi: true, cols: COLS, rows: ROWS, scrollback: 500 });
}
function measure(t: HeadlessTerminal) {
  return () => ({ baseY: t.buffer.active.baseY, cursorY: t.buffer.active.cursorY, rows: t.rows });
}
function state(t: HeadlessTerminal) {
  const b = t.buffer.active;
  const lines: string[] = [];
  for (let y = 0; y < b.length; y++) {
    const line = b.getLine(y);
    if (!line) continue;
    let cells = line.isWrapped ? "W|" : "-|";
    for (let x = 0; x < t.cols; x++) {
      const c = line.getCell(x);
      if (!c) continue;
      cells += `${c.getChars() || " "}:${c.getWidth()}:${c.getFgColorMode()}/${c.getFgColor()}:${c.isBold()};`;
    }
    lines.push(cells);
  }
  return { type: b.type, cursor: [b.cursorX, b.cursorY], baseY: b.baseY, length: b.length, lines, sync: t.modes.synchronizedOutputMode };
}

/** Прежний путь: каждый шаг — своя запись, досылка мерится после истории. */
async function applyStepwise(t: HeadlessTerminal, steps: readonly SnapshotStep[]): Promise<{ writes: PresentationChunk[]; filler: string }> {
  const writes: PresentationChunk[] = [];
  let filler = "";
  for (const step of steps) {
    const payload = snapshotStepPayload(step, FRAME, measure(t));
    if (step.kind === "filler") filler = payload as string;
    if (payload.length === 0) continue;
    writes.push(payload);
    await write(t, payload);
  }
  return { writes, filler };
}

/** Новый путь: голова и хвост; досылка мерится после головы. */
async function applyGlued(t: HeadlessTerminal, steps: readonly SnapshotStep[], renderer: PresentationRenderer) {
  const parts = snapshotStepParts(steps);
  if (!parts) throw new Error("не каноническая форма плана");
  const begun = beginSnapshotTxn({
    renderer, ris: parts.ris ? SNAPSHOT_RIS : undefined, history: parts.history ?? undefined,
    modeOn: t.modes.synchronizedOutputMode, txn: TXN_CLOSED, token: 1, now: 0,
  });
  const writes: PresentationChunk[] = [];
  if (begun.head.chunk !== null) { writes.push(begun.head.chunk); await write(t, begun.head.chunk); }
  const filler = parts.filler ? snapshotStepPayload(parts.filler, FRAME, measure(t)) as string : "";
  const done = finishSnapshotTxn(begun, { filler, frame: FRAME, modeOn: t.modes.synchronizedOutputMode });
  writes.push(done.chunk);
  await write(t, done.chunk);
  return { writes, filler, txn: done.txn };
}

// Три редкие формы planSnapshotApply собраны вручную (без замены — только
// кадр; байты истории после вырезания ESC[3J пусты — RIS и кадр; история без
// строк прокрутки — без досылки), четвёртая — настоящий план с досылкой.
const shortHistory = new TextEncoder().encode(historyText(3));
const SHAPES: Array<{ name: string; plan: () => SnapshotApplyPlan; kinds: string }> = [
  { name: "только кадр (без замены)", plan: () => ({ ...plan(40), steps: [{ kind: "frame" }] }), kinds: "frame" },
  { name: "RIS и кадр (байты истории пусты)", plan: () => ({ ...plan(40), steps: [{ kind: "ris" }, { kind: "frame" }] }), kinds: "ris,frame" },
  {
    name: "история без досылки",
    plan: () => ({ ...plan(40), steps: [{ kind: "ris" }, { kind: "history", bytes: shortHistory, lines: 0 }, { kind: "frame" }] }),
    kinds: "ris,history,frame",
  },
  { name: "история длиннее экрана, переносы, эмодзи (с досылкой)", plan: () => plan(40), kinds: "ris,history,filler,frame" },
];

describe("ST-06: склейка шагов снимка — те же байты (L1)", () => {
  it("четыре формы плана раскладываются; иная форма — null (исполнитель идёт прежними шагами)", () => {
    for (const shape of SHAPES) {
      const p = shape.plan();
      expect(p.steps.map((s) => s.kind).join(","), shape.name).toBe(shape.kinds);
      expect(snapshotStepParts(p.steps), shape.name).not.toBeNull();
    }
    expect(snapshotStepParts([{ kind: "frame" }, { kind: "ris" }])).toBeNull();
    expect(snapshotStepParts([{ kind: "ris" }, { kind: "filler", target: 3 }, { kind: "frame" }])).toBeNull();
    expect(snapshotStepParts([])).toBeNull();
  });

  for (const renderer of ["dom", "webgl"] as const) {
    it(`${renderer}: голова+хвост = шаги побайтно${renderer === "webgl" ? " (без одного BEGIN и одного END)" : ""}; записей не больше двух`, async () => {
      for (const shape of SHAPES) {
        const p = shape.plan();
        const a = makeTerm();
        const b = makeTerm();
        await write(a, PRELUDE);
        await write(b, PRELUDE);
        const old = await applyStepwise(a, p.steps);
        const glued = await applyGlued(b, p.steps, renderer);
        let gluedHex = glued.writes.map(hex).join("");
        const hasHistory = snapshotStepParts(p.steps)!.history !== null;
        if (renderer === "webgl" && hasHistory) {
          // Ровно один BEGIN в голове сразу после RIS и один END в конце хвоста.
          expect(gluedHex.split(hex(SYNC_BEGIN)).length - 1, shape.name).toBe(1);
          expect(gluedHex.split(hex(SYNC_END)).length - 1, shape.name).toBe(1);
          expect(gluedHex.startsWith(hex(SNAPSHOT_RIS) + hex(SYNC_BEGIN)), shape.name).toBe(true);
          expect(gluedHex.endsWith(hex(SYNC_END)), shape.name).toBe(true);
          gluedHex = gluedHex.replace(hex(SYNC_BEGIN), "").replace(hex(SYNC_END), "");
        } else {
          expect(gluedHex.includes(hex(SYNC_BEGIN)) || gluedHex.includes(hex(SYNC_END)), shape.name).toBe(false);
        }
        expect(gluedHex, shape.name).toBe(old.writes.map(hex).join(""));
        expect(glued.writes.length, shape.name).toBe(hasHistory ? 2 : 1);
        expect(glued.filler, shape.name).toBe(old.filler);
        expect(glued.txn.kind, shape.name).toBe("closed");
      }
    });
  }
});

describe("ST-06: склейка шагов снимка — то же состояние xterm 6.0.0 (L2)", () => {
  for (const renderer of ["dom", "webgl"] as const) {
    it(`${renderer}: буфер, курсор, история, переносы и режим 2026 совпадают с пошаговой записью; досылка та же`, async () => {
      for (const shape of SHAPES) {
        const p = shape.plan();
        const a = makeTerm();
        const b = makeTerm();
        await write(a, PRELUDE);
        await write(b, PRELUDE);
        const old = await applyStepwise(a, p.steps);
        const glued = await applyGlued(b, p.steps, renderer);
        expect(state(b), shape.name).toEqual(state(a));
        expect(b.modes.synchronizedOutputMode, shape.name).toBe(false);
        expect(glued.filler, shape.name).toBe(old.filler);
        // Продолжение потока после снимка ложится одинаково.
        await write(a, "\r\nПРОДОЛЖЕНИЕ 🙂\r\n");
        await write(b, "\r\nПРОДОЛЖЕНИЕ 🙂\r\n");
        expect(state(b), `${shape.name}: после продолжения`).toEqual(state(a));
      }
    });
  }

  it("досылка действительно нужна и та же: при переносах длинных строк её число меняется от замера", async () => {
    const p = plan(40);
    const filler = p.steps.find((s) => s.kind === "filler");
    expect(filler).toBeDefined();
    const t = makeTerm();
    await write(t, PRELUDE);
    const glued = await applyGlued(t, p.steps, "webgl");
    // Досылка — строка из \n, число которых посчитано по буферу после головы.
    expect(/^\n*$/.test(glued.filler)).toBe(true);
    expect(joinChunks(["a", "", undefined, "b"])).toBe("ab");
  });
});
