import { describe, expect, it } from "vitest";
import {
  TRACE_STORE_LIMIT, TraceStore, createTraceSlot, releaseTraceSlot, terminalTraceStore, wipeAllRecordings,
} from "./traceStore";

describe("трасса переживает «Открыть заново» и уход с экрана (ST-01, ревью S1)", () => {
  it("тот же id — тот же слот: новый экземпляр экрана продолжает кольцо и его seq", () => {
    const store = new TraceStore(createTraceSlot, 4, releaseTraceSlot);
    const first = store.acquire("pty-1");
    const seq = first.trace.mark("before-reopen");
    // «Открыть заново»: App пересоздаёт экран с тем же id.
    const again = store.acquire("pty-1");
    expect(again).toBe(first);
    const events = again.trace.snapshot().events;
    expect(events.some((e) => e.seq === seq && e.reason === "before-reopen")).toBe(true);
    expect(again.trace.mark("after-reopen")).toBeGreaterThan(seq);
  });

  it("ограничено последними N терминалами; вытесненная запись вывода выключена и стёрта", () => {
    const evicted: string[] = [];
    const store = new TraceStore(createTraceSlot, 3, (slot, key) => { releaseTraceSlot(slot); evicted.push(key); });
    const a = store.acquire("a");
    a.rec.enable();
    a.rec.noteRx(new Uint8Array([65, 66]), 2, 0, 1);
    expect(a.rec.snapshot().bytes).toBe(2);
    store.acquire("b");
    store.acquire("c");
    // Открыли снова «a» — он свежий, вытесняться должен «b».
    store.acquire("a");
    store.acquire("d");
    expect(evicted).toEqual(["b"]);
    expect(store.keys()).toEqual(["c", "a", "d"]);
    store.acquire("e");
    store.acquire("f");
    expect(evicted).toEqual(["b", "c", "a"]);
    expect(a.rec.enabled).toBe(false);
    expect(a.rec.snapshot().bytes).toBe(0);
    expect(a.trace.snapshot().events).toEqual([]);
    expect(store.size).toBe(3);
  });

  it("свой же id при открытии не вытесняется, даже при пределе 1", () => {
    const store = new TraceStore(createTraceSlot, 1, releaseTraceSlot);
    const x = store.acquire("x");
    expect(store.acquire("x")).toBe(x);
    const y = store.acquire("y");
    expect(y).not.toBe(x);
    expect(store.keys()).toEqual(["y"]);
  });

  it("«Удалить запись» стирает вывод во всех терминалах и честно считает чужие (волна 4, T-37)", () => {
    const store = new TraceStore(createTraceSlot, 4, releaseTraceSlot);
    const a = store.acquire("A"), b = store.acquire("B"), c = store.acquire("C");
    a.rec.enable(); a.rec.noteRx(new Uint8Array([1]), 1, 0, 1);
    b.rec.enable(); b.rec.noteRx(new Uint8Array([2, 3]), 2, 0, 1);
    b.trace.mark("b-event");
    expect(wipeAllRecordings(store, "A")).toEqual({ others: 1 });
    for (const slot of [a, b, c]) {
      expect(slot.rec.enabled).toBe(false);
      expect(slot.rec.snapshot().chunks).toEqual([]);
    }
    // События чужого терминала — его диагностика, не вывод: остаются.
    expect(b.trace.snapshot().events.some((e) => e.reason === "b-event")).toBe(true);
    expect(store.keys()).toEqual(["A", "B", "C"]);
    expect(wipeAllRecordings(store, "A")).toEqual({ others: 0 });
  });

  it("модульное хранилище страницы: предел по умолчанию и общий объект для одного id", () => {
    expect(TRACE_STORE_LIMIT).toBe(4);
    const key = `probe-${Math.random()}`;
    expect(terminalTraceStore.acquire(key)).toBe(terminalTraceStore.acquire(key));
  });
});
