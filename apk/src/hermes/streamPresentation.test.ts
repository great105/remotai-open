import { afterEach, describe, expect, it, vi } from "vitest";
import { createStreamPresentation } from "./streamPresentation";

afterEach(() => vi.useRealTimers());

describe("Hermes stream presentation", () => {
  it("spreads an incoming poll chunk across bounded intermediate prefixes", () => {
    vi.useFakeTimers();
    const stream = createStreamPresentation("Начало ");
    const source = "Начало " + "новый текст ".repeat(20);
    const frames: string[] = [];
    stream.subscribe(() => frames.push(stream.getSnapshot()));
    stream.update(source, { streaming: true });
    expect(stream.getSnapshot()).toBe("Начало ");
    vi.advanceTimersByTime(40);
    expect(stream.getSnapshot().length).toBeGreaterThan("Начало ".length);
    expect(stream.getSnapshot().length).toBeLessThan(source.length);
    vi.advanceTimersByTime(120);
    expect(stream.getSnapshot()).toBe(source);
    expect(frames.length).toBeLessThanOrEqual(4);
    expect(frames.every(frame => source.startsWith(frame))).toBe(true);
    expect(vi.getTimerCount()).toBe(0);
  });

  it.each([
    { streaming: false },
    { streaming: true, reducedMotion: true },
  ])("flushes pending text immediately for %j", options => {
    vi.useFakeTimers();
    const stream = createStreamPresentation("Начало");
    stream.update("Начало продолжение", { streaming: true });
    stream.update("Начало продолжение готово", options);
    expect(stream.getSnapshot()).toBe("Начало продолжение готово");
    expect(vi.getTimerCount()).toBe(0);
  });

  it("shows a corrected or shortened source immediately instead of replaying stale text", () => {
    vi.useFakeTimers();
    const stream = createStreamPresentation("Старый ответ");
    stream.update("Старый ответ продолжение", { streaming: true });
    stream.update("Новый", { streaming: true });
    expect(stream.getSnapshot()).toBe("Новый");
    vi.advanceTimersByTime(200);
    expect(stream.getSnapshot()).toBe("Новый");
    expect(vi.getTimerCount()).toBe(0);
  });

  it("does not build an unbounded typewriter backlog on a large poll", () => {
    vi.useFakeTimers();
    const stream = createStreamPresentation("");
    const source = "x".repeat(8000);
    stream.update(source, { streaming: true });
    expect(source.length - stream.getSnapshot().length).toBeLessThanOrEqual(800);
    vi.advanceTimersByTime(160);
    expect(stream.getSnapshot()).toBe(source);
  });

  it("avoids repeatedly parsing very large answers while streaming", () => {
    vi.useFakeTimers();
    const stream = createStreamPresentation("x".repeat(24000));
    const source = "x".repeat(24100);
    stream.update(source, { streaming: true });
    expect(stream.getSnapshot()).toBe(source);
    expect(vi.getTimerCount()).toBe(0);
  });

  it("never emits a prefix that cuts a surrogate pair", () => {
    vi.useFakeTimers();
    const stream = createStreamPresentation("");
    const frames: string[] = [];
    stream.subscribe(() => frames.push(stream.getSnapshot()));
    stream.update("😀😀😀😀😀", { streaming: true });
    vi.advanceTimersByTime(160);
    expect(frames.length).toBeGreaterThan(1);
    expect(frames.every(frame => !/[\uD800-\uDBFF]$/.test(frame))).toBe(true);
    expect(stream.getSnapshot()).toBe("😀😀😀😀😀");
  });

  it("cancels scheduled publication when disposed", () => {
    vi.useFakeTimers();
    const stream = createStreamPresentation("old");
    stream.update("old next", { streaming: true });
    stream.dispose();
    expect(vi.getTimerCount()).toBe(0);
    vi.advanceTimersByTime(200);
    expect(stream.getSnapshot()).toBe("old");
  });

  it("flushes finished content during render, before an effect can run", () => {
    vi.useFakeTimers();
    const stream = createStreamPresentation("prefix");
    stream.update("prefix pending", { streaming: true });
    expect(stream.getDisplay("prefix pending final", { streaming: false })).toBe("prefix pending final");
  });

  it("never shows the old stream on an identity change even if the new text shares its prefix", () => {
    vi.useFakeTimers();
    const stream = createStreamPresentation("same", "chat-a");
    stream.update("same old", { streaming: true, streamKey: "chat-a" });
    expect(stream.getDisplay("same new", { streaming: true, streamKey: "chat-b" })).toBe("same new");
    stream.update("same new", { streaming: true, streamKey: "chat-b" });
    expect(stream.getSnapshot()).toBe("same new");
    expect(vi.getTimerCount()).toBe(0);
  });

  it("does not restart its deadline when chunks arrive faster than presentation ticks", () => {
    vi.useFakeTimers();
    const stream = createStreamPresentation("");
    for (let i = 1; i <= 8; i++) {
      stream.update("x".repeat(i * 20), { streaming: true });
      vi.advanceTimersByTime(20);
    }
    expect(stream.getSnapshot()).toBe("x".repeat(160));
    expect(vi.getTimerCount()).toBe(0);
  });
});
