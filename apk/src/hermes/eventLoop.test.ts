import { afterEach, describe, expect, it, vi } from "vitest";
import { startHermesEventLoop } from "./eventLoop";

afterEach(() => vi.useRealTimers());

describe("Hermes visible event delivery", () => {
  it("starts immediately and never overlaps an outstanding long poll", async () => {
    vi.useFakeTimers();
    let resolve!: () => void;
    const load = vi.fn(() => new Promise<void>(done => { resolve = done; }));
    const stop = startHermesEventLoop({ load, visible: () => true, subscribe: () => () => {} });
    expect(load).toHaveBeenCalledTimes(1);
    await vi.advanceTimersByTimeAsync(20000);
    expect(load).toHaveBeenCalledTimes(1);
    resolve();
    await vi.advanceTimersByTimeAsync(32);
    expect(load).toHaveBeenCalledTimes(2);
    stop();
  });

  it("aborts hidden requests, resumes immediately and cancels on unmount", async () => {
    vi.useFakeTimers();
    let visible = true;
    let changed = () => {};
    const signals: AbortSignal[] = [];
    const load = vi.fn((signal: AbortSignal) => {
      signals.push(signal);
      return new Promise<void>(resolve => signal.addEventListener("abort", () => resolve(), { once: true }));
    });
    const unsubscribe = vi.fn();
    const stop = startHermesEventLoop({ load, visible: () => visible, subscribe: handler => { changed = handler; return unsubscribe; } });
    visible = false; changed();
    expect(signals[0].aborted).toBe(true);
    await vi.advanceTimersByTimeAsync(2000);
    expect(load).toHaveBeenCalledTimes(1);
    visible = true; changed();
    expect(load).toHaveBeenCalledTimes(2);
    stop();
    expect(signals[1].aborted).toBe(true);
    await vi.advanceTimersByTimeAsync(2000);
    changed();
    expect(load).toHaveBeenCalledTimes(2);
    expect(unsubscribe).toHaveBeenCalledOnce();
  });

  it("backs off on failures instead of retrying in a tight loop", async () => {
    vi.useFakeTimers();
    const load = vi.fn().mockRejectedValue(new Error("offline"));
    const stop = startHermesEventLoop({ load, visible: () => true, subscribe: () => () => {} });
    await vi.advanceTimersByTimeAsync(999);
    expect(load).toHaveBeenCalledTimes(1);
    await vi.advanceTimersByTimeAsync(1);
    expect(load).toHaveBeenCalledTimes(2);
    stop();
  });

  it("does not request while initially hidden or after teardown", async () => {
    vi.useFakeTimers();
    const load = vi.fn().mockResolvedValue(undefined);
    const stop = startHermesEventLoop({ load, visible: () => false, subscribe: () => () => {} });
    await vi.advanceTimersByTimeAsync(5000);
    stop();
    expect(load).not.toHaveBeenCalled();
  });
});
