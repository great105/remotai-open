import { describe, expect, it } from "vitest";
import { scrollLocalBottom } from "./localBottom";

describe("local bottom after viewport reflow", () => {
  it("finishes when xterm's DOM position trails its buffer by three rows", () => {
    const active = { viewportY: 3, baseY: 700 };
    let domRow = 0, calls = 0;
    const term = {
      buffer: { active },
      scrollToBottom() {
        calls++;
        domRow = Math.min(active.baseY, domRow + active.baseY - active.viewportY);
        active.viewportY = domRow;
      },
    };
    scrollLocalBottom(term);
    expect(active.viewportY).toBe(700);
    expect(domRow).toBe(700);
    expect(calls).toBe(2);
  });

  it("makes one call when the viewport already agrees with the buffer", () => {
    const active = { viewportY: 20, baseY: 700 };
    let calls = 0;
    scrollLocalBottom({ buffer: { active }, scrollToBottom() { calls++; active.viewportY = active.baseY; } });
    expect(calls).toBe(1);
  });

  it("bounds completion even if the renderer cannot move", () => {
    let calls = 0;
    scrollLocalBottom({ buffer: { active: { viewportY: 3, baseY: 700 } }, scrollToBottom() { calls++; } });
    expect(calls).toBe(2);
  });
});
