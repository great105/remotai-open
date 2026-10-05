import { describe, expect, it, vi } from "vitest";
import { attachCopyOnSelect } from "./copyOnSelect";

function fixture() {
  const doc = new EventTarget();
  const window = new EventTarget();
  Object.assign(doc, { defaultView: window });
  const host = new EventTarget();
  Object.assign(host, { ownerDocument: doc });
  let text = "";
  let enabled = true;
  const copy = vi.fn();
  const dispose = attachCopyOnSelect(host as HTMLElement, () => text, copy, () => enabled);
  const mouse = (target: EventTarget, type: string, button = 0) => {
    const event = new Event(type);
    Object.assign(event, { button });
    target.dispatchEvent(event);
    // Model the document -> window bubble boundary, not native callback timing.
    // Native ordering is covered separately with Playwright mouse input.
    if (target === doc && type === "mouseup") window.dispatchEvent(event);
  };
  return { host, doc, window, copy, dispose, mouse,
    select: (value: string) => { text = value; }, enable: (value: boolean) => { enabled = value; } };
}

describe("completed terminal selection copy", () => {
  it("copies once when mouseup is outside the terminal host", async () => {
    const f = fixture(); f.mouse(f.host, "mousedown"); f.select("exact\nselection");
    f.mouse(f.doc, "mouseup"); await Promise.resolve();
    expect(f.copy).toHaveBeenCalledExactlyOnceWith("exact\nselection"); f.dispose();
  });
  it("reads the completed xterm selection after mouseup listeners", async () => {
    const f = fixture(); f.mouse(f.host, "mousedown"); f.select("partial");
    f.doc.addEventListener("mouseup", () => f.select("completed"));
    f.mouse(f.doc, "mouseup"); expect(f.copy).not.toHaveBeenCalled(); await Promise.resolve();
    expect(f.copy).toHaveBeenCalledExactlyOnceWith("completed"); f.dispose();
  });
  it("unrelated releases cannot recopy a stale selection", async () => {
    const f = fixture(); f.select("stale"); f.mouse(f.doc, "mouseup"); await Promise.resolve();
    expect(f.copy).not.toHaveBeenCalled(); f.dispose();
  });
  it("right button and empty selection do not write clipboard", async () => {
    const f = fixture(); f.select("stale"); f.mouse(f.host, "mousedown", 2); f.mouse(f.doc, "mouseup", 2);
    await Promise.resolve(); f.mouse(f.host, "mousedown"); f.select("  \n"); f.mouse(f.doc, "mouseup");
    await Promise.resolve(); expect(f.copy).not.toHaveBeenCalled(); f.dispose();
  });
  it("presentation hold and teardown cancel pending copy", async () => {
    const f = fixture(); f.select("hidden"); f.enable(false); f.mouse(f.host, "mousedown");
    f.enable(true); f.mouse(f.doc, "mouseup"); await Promise.resolve(); expect(f.copy).not.toHaveBeenCalled();
    f.mouse(f.host, "mousedown"); f.mouse(f.doc, "mouseup"); f.dispose(); await Promise.resolve();
    expect(f.copy).not.toHaveBeenCalled();
  });
  it("window blur cancels a drag that left the browser", async () => {
    const f = fixture(); f.select("stale"); f.mouse(f.host, "mousedown"); f.window.dispatchEvent(new Event("blur"));
    f.mouse(f.doc, "mouseup"); await Promise.resolve(); expect(f.copy).not.toHaveBeenCalled(); f.dispose();
  });
});
