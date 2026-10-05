import { describe, expect, it } from "vitest";
import { Terminal as HeadlessTerminal } from "@xterm/headless";
import { planSnapshotApply, SNAPSHOT_RIS, snapshotStepPayload } from "./snapshotApply";
import type { SnapshotApplyInput, SnapshotStep } from "./snapshotApply";

const dec = (b: Uint8Array) => new TextDecoder().decode(b);

// Зеркало отдаёт S строк scrollback + R видимых: здесь S=3, R=2.
const BASE: SnapshotApplyInput = {
  history: "h1\r\nh2\r\nh3\r\nv1\r\nv2",
  histLines: 5,
  screenRows: 2,
  snapCols: 20,
  termCols: 20,
  localScrollback: 0,
  historyState: "ready",
};

const kinds = (steps: SnapshotStep[]) => steps.map((s) => s.kind);

function historyText(steps: SnapshotStep[]): string | null {
  const h = steps.find((s) => s.kind === "history");
  return h && h.kind === "history" ? dec(h.bytes) : null;
}

describe("план применения снимка (C-06, ST-05)", () => {
  it("замена: RIS → scrollback-часть истории → досылка строк → кадр", () => {
    const p = planSnapshotApply(BASE);
    expect(p.replace).toBe(true);
    expect(p.nextHistoryState).toBe("done");
    expect(p.serverScrollback).toBe(3);
    expect(kinds(p.steps)).toEqual(["ris", "history", "filler", "frame"]);
    // Видимые строки снимка (v1, v2) не печатаются: их перерисует кадр.
    expect(historyText(p.steps)).toBe("h1\r\nh2\r\nh3\r\n");
    expect(p.steps[1]).toMatchObject({ kind: "history", lines: 3 });
    expect(p.steps[2]).toEqual({ kind: "filler", target: 3 });
  });

  it("своя прокрутка не хуже присланной — только кадр, но готовность соединения израсходована", () => {
    const p = planSnapshotApply({ ...BASE, localScrollback: 3 });
    expect(p.replace).toBe(false);
    expect(kinds(p.steps)).toEqual(["frame"]);
    // Как в компоненте: решение по истории снимается ДО сравнения прокруток.
    expect(p.nextHistoryState).toBe("done");
  });

  it("resumed-соединение (none) — только кадр, состояние не меняется", () => {
    const p = planSnapshotApply({ ...BASE, historyState: "none" });
    expect(p.replace).toBe(false);
    expect(kinds(p.steps)).toEqual(["frame"]);
    expect(p.nextHistoryState).toBe("none");
  });

  it("старый агент без hist_lines/screen_rows — только кадр, историю целиком не печатаем", () => {
    const p = planSnapshotApply({ ...BASE, histLines: undefined, screenRows: undefined });
    expect(p.serverScrollback).toBe(-1);
    expect(p.replace).toBe(false);
    expect(kinds(p.steps)).toEqual(["frame"]);
  });

  it("чужая ширина: заменяем, только если своей истории нет вовсе", () => {
    expect(planSnapshotApply({ ...BASE, snapCols: 30 }).replace).toBe(true);
    expect(planSnapshotApply({ ...BASE, snapCols: 30, localScrollback: 1 }).replace).toBe(false);
    // Ширина не числом (старый агент) — «не совпала».
    expect(planSnapshotApply({ ...BASE, snapCols: undefined, localScrollback: 1 }).replace).toBe(false);
  });

  it("кадр без истории не расходует готовность соединения", () => {
    const p = planSnapshotApply({ ...BASE, history: "" });
    expect(p.replace).toBe(false);
    expect(kinds(p.steps)).toEqual(["frame"]);
    expect(p.nextHistoryState).toBe("ready");
  });

  it("аномальный ESC[3J внутри истории вырезается, а не исполняется", () => {
    const p = planSnapshotApply({ ...BASE, history: "h1\x1b[3J\r\nh2\r\nv1", histLines: 3, screenRows: 1 });
    expect(historyText(p.steps)).toBe("h1\r\nh2\r\n");
  });

  it("байты шагов: RIS, история как есть, LF по замеру ПОСЛЕ истории, кадр", () => {
    const p = planSnapshotApply(BASE);
    const frame = "\x1b[H\x1b[2JFRAME";
    const noMeasure = () => { throw new Error("замер нужен только шагу filler"); };
    expect(snapshotStepPayload(p.steps[0], frame, noMeasure)).toBe(SNAPSHOT_RIS);
    expect(dec(snapshotStepPayload(p.steps[1], frame, noMeasure) as Uint8Array)).toBe("h1\r\nh2\r\nh3\r\n");
    // fillerRows(3, baseY=1, cursorY=2, rows=4) = (3−1) + (4−1−2) = 3.
    expect(snapshotStepPayload(p.steps[2], frame, () => ({ baseY: 1, cursorY: 2, rows: 4 }))).toBe("\n\n\n");
    expect(snapshotStepPayload(p.steps[2], frame, () => ({ baseY: 5, cursorY: 0, rows: 4 }))).toBe("");
    expect(snapshotStepPayload(p.steps[3], frame, noMeasure)).toBe(frame);
  });

  // Исполнение плана на настоящем xterm: ровно S строк scrollback при любой
  // высоте клиента (R′ < R, R′ = R, R′ > R) — шов historySeam, но теперь
  // через тот же план, что исполняет компонент.
  for (const rows of [1, 2, 5]) {
    it(`исполнение на headless: scrollback ровно S=3 при высоте ${rows}`, async () => {
      const term = new HeadlessTerminal({ cols: 20, rows, allowProposedApi: true });
      const write = (d: string | Uint8Array) => new Promise<void>((r) => term.write(d, r));
      const p = planSnapshotApply({ ...BASE, localScrollback: term.buffer.normal.baseY });
      const frame = "\x1b[m\x1b[H\x1b[2J\x1b[1;1Hv1\x1b[2;1Hv2";
      for (const step of p.steps) {
        const payload = snapshotStepPayload(step, frame, () => ({
          baseY: term.buffer.active.baseY,
          cursorY: term.buffer.active.cursorY,
          rows: term.rows,
        }));
        await write(payload);
      }
      const b = term.buffer.normal;
      expect(b.baseY).toBe(3);
      const hist = [0, 1, 2].map((y) => b.getLine(y)?.translateToString(true));
      expect(hist).toEqual(["h1", "h2", "h3"]);
      term.dispose();
    });
  }

  // Строка истории зеркала шире клиента (напечатана до сужения — зеркало её не
  // переносит): у клиента она занимает несколько рядов. Цель досылки «по
  // строкам» оставляла самые свежие ряды на экране, и ESC[2J кадра их стирал
  // (стенд §6, C-03R seed 20261006: W2 терялась). Вся история — в прокрутке.
  for (const rows of [1, 3, 6]) {
    it(`строки истории шире клиента: вся история в прокрутке при высоте ${rows}`, async () => {
      const w1 = "W1".padEnd(19, "=");
      const w2 = "W2".padEnd(19, "=");
      const term = new HeadlessTerminal({ cols: 16, rows, allowProposedApi: true });
      const write = (d: string | Uint8Array) => new Promise<void>((r) => term.write(d, r));
      const p = planSnapshotApply({
        ...BASE, history: `${w1}\r\n${w2}\r\nv1`, histLines: 3, screenRows: 1, snapCols: 20, termCols: 16,
        localScrollback: term.buffer.normal.baseY,
      });
      expect(kinds(p.steps)).toEqual(["ris", "history", "filler", "frame"]);
      for (const step of p.steps) {
        await write(snapshotStepPayload(step, "\x1b[m\x1b[H\x1b[2Jv1", () => ({
          baseY: term.buffer.active.baseY,
          cursorY: term.buffer.active.cursorY,
          rows: term.rows,
        })));
      }
      const b = term.buffer.normal;
      const logical: string[] = [];
      for (let y = 0; y < b.baseY; y++) {
        const line = b.getLine(y)!;
        const text = line.translateToString(true);
        if (line.isWrapped && logical.length > 0) logical[logical.length - 1] = logical[logical.length - 1].padEnd(16) + text;
        else logical.push(text);
      }
      expect(logical).toEqual([w1, w2]);
      expect(b.getLine(b.baseY)?.translateToString(true)).toBe("v1");
      term.dispose();
    });
  }
});
