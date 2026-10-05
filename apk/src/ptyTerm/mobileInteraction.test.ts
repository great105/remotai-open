import { describe, it, expect, vi } from "vitest";
import headless from "@xterm/headless";
import { terminalCoordinates, visibleRows, cellAt } from "./geometry/TerminalCoordinates";
import { releaseVelocity, WheelAccumulator } from "./gestures/GestureController";
import { bufferText } from "./reading/bufferText";
import { registerReadAnchor } from "./reading/readAnchor";
import { createClipboardService } from "./input/ClipboardService";
import { pastePayload, needsMultilineReview, transmitInput } from "./input/InputController";
import { frameGeometryAction } from "./geometry";
import { keyboardPeek } from "./geometry/KeyboardAdapter";

const write = (term: InstanceType<typeof headless.Terminal>, text: string) => new Promise<void>(resolve => term.write(text, resolve));

describe("mobile terminal plan: coordinates and motion", () => {
  it("keeps the active cursor visible without always jumping to the bottom", () => {
    expect(keyboardPeek(480, 160, 30, 2)).toBe(0);
    expect(keyboardPeek(480, 160, 30, 29)).toBe(320);
    expect(keyboardPeek(480, 160, 30, 2, 320)).toBe(32);
    expect(keyboardPeek(480, 160, 30, -2, 320)).toBe(0);
    expect(keyboardPeek(480, 480, 30, 29, 320)).toBe(0);
  });
  it("48 physical pixels remain three rows after keyboard clipping and screen translation", () => {
    for (const top of [0, -320]) {
      const g = terminalCoordinates({ left: 8, top, width: 640, height: 480 },
        { left: 0, top: 0, width: 656, height: top === 0 ? 480 : 160 }, 80, 30)!;
      expect(48 / g.cellHeight).toBe(3);
      expect(cellAt(g, 12, 8).row).toBe(top === 0 ? 0 : 20);
      expect(visibleRows(g)).toEqual({ start: top === 0 ? 0 : 20, end: 29 });
    }
  });
  it("holding still cancels release momentum", () => {
    const samples = [{ dy: 0, t: 0 }, { dy: 20, t: 16 }, { dy: 20, t: 32 }];
    expect(releaseVelocity(samples, 32)).toBe(1.25);
    expect(releaseVelocity(samples, 1000)).toBe(0);
  });
  it("trackpad preserves sub-row pixels, direction and wheel units", () => {
    const wheel = new WheelAccumulator();
    const event = { deltaX: 0, deltaY: 1, deltaMode: 0, ctrlKey: false, metaKey: false };
    for (let i = 0; i < 15; i++) expect(wheel.lines(event, 16, 160, i)).toBe(0);
    expect(wheel.lines(event, 16, 160, 15)).toBe(1);
    expect(wheel.lines({ ...event, deltaY: 120 }, 16, 160, 20)).toBe(7);
    expect(wheel.lines({ ...event, deltaY: -2, deltaMode: 1 }, 16, 160, 21)).toBe(-2);
    expect(wheel.lines({ ...event, deltaMode: 2 }, 16, 160, 22)).toBe(10);
    expect(wheel.lines({ ...event, ctrlKey: true, deltaY: 120 }, 16, 160, 23)).toBe(0);
    expect(wheel.lines({ ...event, deltaX: 120 }, 16, 160, 24)).toBe(0);
  });
});

describe("real xterm reading and public markers", () => {
  it("joins soft wraps, preserves spaces and hard line breaks", async () => {
    const term = new headless.Terminal({ cols: 8, rows: 5, allowProposedApi: true });
    await write(term, "abcdefghijk\r\n  code\r\nПривет!");
    expect(bufferText(term.buffer.active, 0, 3)).toBe("abcdefghijk\n  code\nПривет!");
    expect(bufferText(term.buffer.active, 0, 1, true)).toBe("abcdefgh\nijk");
    term.dispose();
  });
  it("copies the viewed history, independently of the live cursor", async () => {
    const term = new headless.Terminal({ cols: 16, rows: 3, scrollback: 100, allowProposedApi: true });
    await write(term, Array.from({ length: 40 }, (_, i) => `line-${i}`).join("\r\n"));
    term.scrollToLine(10);
    expect(bufferText(term.buffer.active, term.buffer.active.viewportY, term.buffer.active.viewportY + 2))
      .toBe("line-10\nline-11\nline-12");
    term.dispose();
  });
  it("public marker follows scrollback trimming and is disposed when evicted", async () => {
    const term = new headless.Terminal({ cols: 16, rows: 3, scrollback: 10, allowProposedApi: true });
    await write(term, Array.from({ length: 13 }, (_, i) => `line-${i}`).join("\r\n"));
    term.scrollToLine(5);
    const marker = registerReadAnchor(term)!;
    expect(marker.line).toBe(5);
    await write(term, "\r\nnext\r\nnext");
    expect(marker.line).toBe(3);
    expect(term.buffer.active.getLine(marker.line)?.translateToString(true)).toBe("line-5");
    await write(term, "\r\nnext".repeat(6));
    expect(marker.isDisposed).toBe(true);
    await write(term, "\x1b[?1049h");
    expect(registerReadAnchor(term)).toBeUndefined();
    term.dispose();
  });
});

describe("clipboard and input outcomes", () => {
  it("rejects false/throwing legacy copy and accepts actual adapter completion", async () => {
    const denied = { write: async () => { throw new Error("denied"); }, read: async () => "" };
    expect(await createClipboardService(denied, () => false).write("retained")).toBe(false);
    expect(await createClipboardService(denied, () => { throw new Error("unavailable"); }).write("retained")).toBe(false);
    expect(await createClipboardService(denied, () => true).write("copied")).toBe(true);
    expect(await createClipboardService({ ...denied, write: async () => {} }).write("copied")).toBe(true);
  });
  it("all text sources share CR normalization and negotiated bracketing", () => {
    const text = "Привет 😀\r\nnext\n  code";
    for (const bracketed of [true, false]) {
      const send = vi.fn();
      for (const _source of ["clipboard", "composer", "editor"]) {
        expect(transmitInput({ readyState: 1, send }, { kind: "paste", text, bracketed })).toBe(true);
      }
      expect(new Set(send.mock.calls.map(([payload]) => payload)).size).toBe(1);
      expect(JSON.parse(send.mock.calls[0][0]).text).toBe(pastePayload(text, bracketed));
      expect(needsMultilineReview(text, bracketed)).toBe(!bracketed);
    }
  });
  it("submit follows paste in the same message; a throwing/closed socket is failure without retry", () => {
    const send = vi.fn();
    transmitInput({ readyState: 1, send }, { kind: "paste", text: "a\nb", bracketed: true, submit: true });
    expect(send).toHaveBeenCalledExactlyOnceWith(JSON.stringify({ t: "paste", text: "\x1b[200~a\rb\x1b[201~\r" }));
    expect(transmitInput({ readyState: 3, send }, { kind: "key", data: "\r" })).toBe(false);
    expect(transmitInput({ readyState: 1, send: () => { throw new Error("closed"); } }, { kind: "paste", text: "x", bracketed: false })).toBe(false);
    expect(send).toHaveBeenCalledTimes(1);
  });
});

it("same-width viewers converge on authoritative height and retain different-width adoption", () => {
  const frame = { snapCols: 80, snapRows: 24, authoritativeCols: 80, authoritativeRows: 24, keyboardOpen: false };
  expect(frameGeometryAction({ ...frame, termCols: 80, termRows: 40 })).toEqual({ kind: "adopt", cols: 80, rows: 24 });
  for (let i = 0; i < 100; i++) expect(frameGeometryAction({ ...frame, termCols: 80, termRows: 24 })).toEqual({ kind: "apply" });
  expect(frameGeometryAction({ ...frame, termCols: 120, termRows: 40 })).toEqual({ kind: "adopt", cols: 80, rows: 24 });
});
