import { describe, expect, it } from "vitest";
import { Terminal as HeadlessTerminal } from "@xterm/headless";
import {
  AGENT_RESYNC_MAX_BYTES, AGENT_SUBQ_MAX_BYTES, FLOW_DIAG_MIN_MS, FLOW_DIAG_PERIOD_MS, FLUSH_BATCH_MAX_BYTES,
  FLUSH_SAFETY_MS, FlowStatsAccumulator, emptyFlowStats, flowDiagDue, flowDiagFields, flowQueueCap, flowReasons,
  carryTailAcrossMarker, flowStatsKey, flowTransition, noteHanded, reconnectResume, selectFlushBatch, type FlushItem, type HandedMark,
} from "./FlowController";
import { TerminalWriter, type TerminalWriteData } from "../terminalWriter";

describe("flowQueueCap (ST-09, I-11)", () => {
  const K = 1024;
  const high = 256 * K;
  it("covers the agent's worst legal burst: a resync at the limit plus the subscriber reserve on top of a queue at the pause threshold", () => {
    // Сценарий ревьюера 14.09: досылка 2 090 000 Б без пропуска и 64 КиБ живого
    // вывода следом. Прежний потолок 2 МиБ выбрасывал голову (3 из 3).
    expect(2_090_000 + 64 * K).toBeGreaterThan(2 << 20);
    expect(flowQueueCap(high)).toBeGreaterThan(2_090_000 + 64 * K);
    // Худший законный случай по контракту агента.
    expect(flowQueueCap(high)).toBeGreaterThanOrEqual(high + AGENT_RESYNC_MAX_BYTES + AGENT_SUBQ_MAX_BYTES);
    expect(AGENT_RESYNC_MAX_BYTES).toBe(2 << 20);
    expect(AGENT_SUBQ_MAX_BYTES).toBe(4 << 20);
  });
  it("stays bounded and sane for broken thresholds", () => {
    expect(flowQueueCap(high)).toBeLessThan(8 << 20);
    for (const bad of [NaN, -1, 0, Infinity]) {
      const cap = flowQueueCap(bad);
      expect(Number.isFinite(cap)).toBe(true);
      expect(cap).toBeGreaterThanOrEqual(AGENT_RESYNC_MAX_BYTES + AGENT_SUBQ_MAX_BYTES);
    }
  });
});

it("keeps a hidden viewer paused after parser ACKs and resumes only when both reasons clear", () => {
  expect(flowTransition(false, 0, false, 100, 20)).toBe("pause");
  expect(flowTransition(true, 0, false, 100, 20)).toBeNull();
  expect(flowTransition(true, 50, true, 100, 20)).toBeNull();
  expect(flowTransition(true, 0, true, 100, 20)).toBe("resume");
  expect(flowTransition(false, 101, true, 100, 20)).toBe("pause");
});

describe("flow diag (ST-09, I-15)", () => {
  const base = { now: 100_000, lastSentAt: 90_000, changed: true, pending: false, visible: true };
  it("never repeats identical counters, even on a transition", () => {
    expect(flowDiagDue({ ...base, changed: false, pending: true, lastSentAt: 0 })).toBe(false);
    expect(flowDiagDue({ ...base, changed: false, lastSentAt: 0 })).toBe(false);
  });
  it("sends a transition at once, but not more often than FLOW_DIAG_MIN_MS", () => {
    expect(flowDiagDue({ ...base, pending: true, lastSentAt: 0 })).toBe(true);
    expect(flowDiagDue({ ...base, pending: true, lastSentAt: base.now - FLOW_DIAG_MIN_MS + 1 })).toBe(false);
    expect(flowDiagDue({ ...base, pending: true, lastSentAt: base.now - FLOW_DIAG_MIN_MS })).toBe(true);
    // Переход уходит и из скрытой страницы: уход в фон — тоже переход паузы.
    expect(flowDiagDue({ ...base, pending: true, visible: false, lastSentAt: 0 })).toBe(true);
  });
  it("sends changed counters periodically only on a visible page", () => {
    expect(flowDiagDue({ ...base, lastSentAt: base.now - FLOW_DIAG_PERIOD_MS + 1 })).toBe(false);
    expect(flowDiagDue({ ...base, lastSentAt: base.now - FLOW_DIAG_PERIOD_MS })).toBe(true);
    expect(flowDiagDue({ ...base, visible: false, lastSentAt: 0 })).toBe(false);
  });
  it("keys dedup by counters, pause time to the second", () => {
    const a = emptyFlowStats();
    expect(flowStatsKey({ ...a, pausedMs: 400 })).toBe(flowStatsKey({ ...a, pausedMs: 900 }));
    expect(flowStatsKey({ ...a, pausedMs: 400 })).not.toBe(flowStatsKey({ ...a, pausedMs: 1400 }));
    expect(flowStatsKey(a)).not.toBe(flowStatsKey({ ...a, drops: 1 }));
    expect(flowStatsKey(a)).not.toBe(flowStatsKey({ ...a, timerFlushes: 1 }));
  });
  it("carries only numbers and short labels; trace fields are flat", () => {
    const acc = new FlowStatsAccumulator();
    acc.noteQueues(300_000, 1000); acc.noteDrop(5); acc.noteTimerFlush();
    acc.noteReasons({ hidden: true, backlog: false, next: true }, 0);
    const { wire, trace } = flowDiagFields(acc.snapshot(1500), "pause", "hidden");
    expect(wire).toEqual({ state: "pause", reason: "hidden", maxQueued: 300_000, maxUnacked: 1000, maxBatch: 0,
      pausedMs: 1500, drops: 1, dropBytes: 5, maxFlushMs: 0, timerFlushes: 1, pauses: { hidden: 1, backlog: 0 } });
    for (const value of Object.values(trace)) expect(["number", "string"]).toContain(typeof value);
    expect(trace).toMatchObject({ pausesHidden: 1, pausesBacklog: 0 });
  });
  it("keeps the rAF safety timer well under the hidden-page flush period", () => {
    expect(FLUSH_SAFETY_MS).toBeGreaterThan(16);
    expect(FLUSH_SAFETY_MS).toBeLessThan(250);
  });
});

// Прежняя формула FlowController.ts до ST-09 — эталон совместимости обёртки.
function legacyTransition(paused: boolean, unacked: number, visible: boolean, high: number, low: number) {
  const nextPaused = !visible || (paused ? unacked >= low : unacked > high);
  return nextPaused === paused ? null : nextPaused ? "pause" : "resume";
}

describe("flowReasons (ST-09, T-35)", () => {
  const K = 1024;
  it("pauses on the client merge queue alone: 300 KiB queued with nothing unacked", () => {
    expect(flowReasons(false, { visible: true, queuedBytes: 300 * K, unackedBytes: 0 }, 256 * K, 64 * K))
      .toEqual({ hidden: false, backlog: true, next: true });
  });

  it("applies hysteresis to the SUM of both stages", () => {
    const s = (q: number, u: number) => ({ visible: true, queuedBytes: q, unackedBytes: u });
    // Ниже high по отдельности, выше в сумме — пауза.
    expect(flowReasons(false, s(200, 57), 256, 64).backlog).toBe(true);
    expect(flowReasons(false, s(200, 56), 256, 64).backlog).toBe(false);
    // На паузе держим, пока сумма не опустится ниже low.
    expect(flowReasons(true, s(40, 24), 256, 64).backlog).toBe(true);
    expect(flowReasons(true, s(40, 23), 256, 64).backlog).toBe(false);
  });

  it("keeps hidden independent from backlog: resume only when both reasons clear", () => {
    const hidden = flowReasons(true, { visible: false, queuedBytes: 0, unackedBytes: 0 }, 256, 64);
    expect(hidden).toEqual({ hidden: true, backlog: false, next: true });
    const back = flowReasons(true, { visible: true, queuedBytes: 100, unackedBytes: 0 }, 256, 64);
    expect(back).toEqual({ hidden: false, backlog: true, next: true });
    expect(flowReasons(true, { visible: true, queuedBytes: 0, unackedBytes: 0 }, 256, 64).next).toBe(false);
    expect(flowReasons(false, { visible: false, queuedBytes: 1e9, unackedBytes: 1e9 }, 256, 64))
      .toEqual({ hidden: true, backlog: true, next: true });
  });

  it("does not let a NaN stage hide a real backlog", () => {
    expect(flowReasons(false, { visible: true, queuedBytes: NaN, unackedBytes: 300 }, 256, 64).backlog).toBe(true);
  });

  it("flowTransition stays equal to the legacy formula on the whole grid", () => {
    for (const paused of [false, true]) for (const visible of [false, true])
      for (let unacked = 0; unacked <= 130; unacked++)
        expect(flowTransition(paused, unacked, visible, 100, 20)).toBe(legacyTransition(paused, unacked, visible, 100, 20));
  });
});

type Item = FlushItem & { id: number };
const g = (generation: number, epoch = "e") => ({ generation, epoch });
function items(sizes: number[], guards = sizes.map(() => g(1))): Item[] {
  let end = 1000;
  return sizes.map((n, id) => {
    const bytes = new Uint8Array(n).map((_, i) => (end + i) & 0xff);
    end += n;
    return { id, bytes, streamEnd: end, guard: guards[id] };
  });
}

describe("selectFlushBatch (ST-09)", () => {
  it("never merges different guards even when they fit", () => {
    const q = items([10, 10, 10], [g(1), g(1, "x"), g(1, "x")]);
    const a = selectFlushBatch(q, 1000);
    expect(a.take.map(i => i.id)).toEqual([0]);
    expect(a.rest.map(i => i.id)).toEqual([1, 2]);
    const b = selectFlushBatch(items([10, 10], [g(1), g(2)]), 1000);
    expect(b.take).toHaveLength(1);
  });

  it("keeps the sum under the cap without splitting later items", () => {
    const a = selectFlushBatch(items([40, 40, 40]), 100);
    expect(a.take.map(i => i.id)).toEqual([0, 1]);
    expect(a.takeBytes).toBe(80);
    expect(a.rest[0].bytes.byteLength).toBe(40);
  });

  it("splits an oversize first item with the exact stream position of the head", () => {
    const q = items([300, 5]);
    const a = selectFlushBatch(q, 128);
    expect(a.take).toHaveLength(1);
    expect(a.take[0].bytes.byteLength).toBe(128);
    expect(a.take[0].streamEnd).toBe(q[0].streamEnd - 172);
    expect(a.rest[0].bytes.byteLength).toBe(172);
    expect(a.rest[0].streamEnd).toBe(q[0].streamEnd);
    expect(a.rest[0].guard).toBe(q[0].guard);
    expect(a.rest[1]).toBe(q[1]);
  });

  it("without a cap takes the whole guard run like the legacy flush", () => {
    const q = items([FLUSH_BATCH_MAX_BYTES, FLUSH_BATCH_MAX_BYTES, 7], [g(1), g(1), g(2)]);
    for (const cap of [Infinity, 0, NaN, 0.5]) {
      const a = selectFlushBatch(q, cap);
      expect(a.take.map(i => i.id)).toEqual([0, 1]);
    }
  });

  // mulberry32: детерминированный генератор, чтобы падение воспроизводилось по seed.
  function mulberry32(seed: number) {
    return () => {
      seed |= 0; seed = (seed + 0x6D2B79F5) | 0;
      let t = Math.imul(seed ^ (seed >>> 15), 1 | seed);
      t = (t + Math.imul(t ^ (t >>> 7), 61 | t)) ^ t;
      return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
    };
  }

  for (const seed of [1, 7, 42, 2026, 91313]) {
    it(`reassembles the exact stream and keeps streamEnd monotonic (seed ${seed})`, () => {
      const rnd = mulberry32(seed);
      const cap = 1 + Math.floor(rnd() * 400);
      const sizes: number[] = [];
      const guards: { generation: number; epoch: string }[] = [];
      let gen = 1;
      for (let i = 0; i < 300; i++) {
        if (rnd() < 0.05) gen++;
        sizes.push(Math.floor(rnd() * 1000));
        guards.push(g(gen, `e${gen}`));
      }
      const input = items(sizes, guards);
      const expected = Array.from(input.flatMap(i => [...i.bytes]));
      const out: number[] = [];
      let queue: Item[] = input;
      let lastEnd = -Infinity;
      let rounds = 0;
      // Прогресс гарантирован: каждый раунд забирает целый элемент или ровно
      // cap байт разрезом, а каждый разрез порождает не больше одного хвоста.
      const maxRounds = input.length + 2 * Math.ceil(expected.length / cap) + 1;
      while (queue.length > 0) {
        const { take, rest, takeBytes } = selectFlushBatch(queue, cap);
        expect(take.length).toBeGreaterThan(0);
        expect(takeBytes).toBeLessThanOrEqual(cap);
        expect(takeBytes).toBe(take.reduce((n, i) => n + i.bytes.byteLength, 0));
        for (const item of take) {
          expect(item.guard.generation).toBe(take[0].guard.generation);
          expect(item.guard.epoch).toBe(take[0].guard.epoch);
          expect(item.streamEnd).toBeGreaterThanOrEqual(lastEnd);
          lastEnd = item.streamEnd;
          out.push(...item.bytes);
          // streamEnd — позиция последнего байта элемента в потоке.
          expect(item.streamEnd).toBe(1000 + out.length);
        }
        queue = rest;
        expect(++rounds).toBeLessThanOrEqual(maxRounds);
      }
      expect(out).toEqual(expected);
    });
  }
});

describe("selectFlushBatch on the real xterm 6 parser (L2)", () => {
  it("a cut at any byte (mid UTF-8, mid CSI/OSC) gives the same screen as one write", async () => {
    const ESC = String.fromCharCode(0x1b);
    const BEL = String.fromCharCode(7);
    const CRLF = String.fromCharCode(13, 10);
    const line = `${ESC}[1;31mкрасный${ESC}[0m 😀 中文 ${ESC}]0;t${BEL}ok${ESC}[2K${ESC}[5Gконец`;
    // 14 строк: при потолке 7 байт это ~150 записей, каждая ждёт свой callback xterm.
    const bytes = new TextEncoder().encode(Array.from({ length: 14 }, (_, i) => `${i} ${line}`).join(CRLF));
    const make = () => new HeadlessTerminal({ cols: 40, rows: 10, scrollback: 200, allowProposedApi: true });
    const write = (t: HeadlessTerminal, data: Uint8Array) => new Promise<void>(resolve => t.write(data, resolve));
    const dump = (t: HeadlessTerminal) => {
      const b = t.buffer.active;
      const lines = Array.from({ length: b.length }, (_, i) => b.getLine(i)!.translateToString(true));
      return { lines, x: b.cursorX, y: b.cursorY, base: b.baseY };
    };
    const whole = make();
    await write(whole, bytes);
    // Очередь из кадров случайной длины, потолок 7 байт: режутся и кадры, и символы.
    const rnd = (() => { let s = 5; return () => (s = (s * 16807) % 2147483647) / 2147483647; })();
    let queue: FlushItem[] = [];
    for (let off = 0; off < bytes.length;) {
      const n = Math.min(bytes.length - off, 1 + Math.floor(rnd() * 40));
      queue.push({ bytes: bytes.subarray(off, off + n), streamEnd: off + n, guard: g(1) });
      off += n;
    }
    const split = make();
    let writes = 0;
    while (queue.length > 0) {
      const { take, rest, takeBytes } = selectFlushBatch(queue, 7);
      const merged = new Uint8Array(takeBytes);
      let at = 0;
      for (const item of take) { merged.set(item.bytes, at); at += item.bytes.byteLength; }
      await write(split, merged);
      writes++;
      queue = rest;
    }
    expect(writes).toBeGreaterThan(bytes.length / 7 - 1);
    expect(dump(split)).toEqual(dump(whole));
    whole.dispose();
    split.dispose();
  }, 30_000);
});

describe("FlowStatsAccumulator (ST-09, §10)", () => {
  it("counts reasons by rising edge, paused time and maxima", () => {
    const s = new FlowStatsAccumulator();
    s.noteQueues(300, 10); s.noteQueues(100, 70); s.noteBatch(128); s.noteBatch(64);
    s.noteFlush(12); s.noteFlush(3); s.noteDrop(2048); s.noteDrop(NaN); s.noteTimerFlush();
    s.noteReasons({ hidden: false, backlog: true, next: true }, 1000);
    s.noteReasons({ hidden: true, backlog: true, next: true }, 1100);
    s.noteReasons({ hidden: true, backlog: false, next: true }, 1200);
    s.noteReasons({ hidden: false, backlog: false, next: false }, 1500);
    s.noteReasons({ hidden: false, backlog: true, next: true }, 2000);
    expect(s.snapshot(2250)).toEqual({ maxQueued: 300, maxUnacked: 70, maxBatch: 128,
      pauses: { hidden: 1, backlog: 2 }, pausedMs: 750, drops: 2, dropBytes: 2048, maxFlushMs: 12, timerFlushes: 1 });
    s.reset();
    expect(s.snapshot(9999).pausedMs).toBe(0);
  });
});

describe("held tail across a same-socket resumed marker (ST-05, skeptic ST-10)", () => {
  // Резинк отстающего зрителя: resumed без gap, та же эпоха, база = принятое.
  const resync = { flowBacklog: true, marker: "resumed", gap: false, epochChanged: false, markerOffset: 27, accepted: 27 };

  it("contiguous resumed carries the tail into the new writer epoch", () => {
    expect(carryTailAcrossMarker(resync)).toBe(true);
  });

  it("reset, gap, another epoch or another base drop it as before", () => {
    expect(carryTailAcrossMarker({ ...resync, marker: "reset" })).toBe(false);
    expect(carryTailAcrossMarker({ ...resync, gap: true })).toBe(false);
    expect(carryTailAcrossMarker({ ...resync, epochChanged: true })).toBe(false);
    // База отстаёт или обгоняет принятое — поток не непрерывен.
    expect(carryTailAcrossMarker({ ...resync, markerOffset: 26 })).toBe(false);
    expect(carryTailAcrossMarker({ ...resync, markerOffset: 28 })).toBe(false);
    // Без базы (старый агент) или с мусором — не угадываем.
    expect(carryTailAcrossMarker({ ...resync, markerOffset: undefined })).toBe(false);
    expect(carryTailAcrossMarker({ ...resync, markerOffset: "27" })).toBe(false);
    expect(carryTailAcrossMarker({ ...resync, markerOffset: NaN, accepted: NaN })).toBe(false);
  });

  it("flowBacklog=false keeps the old drop byte for byte (rollback)", () => {
    expect(carryTailAcrossMarker({ ...resync, flowBacklog: false })).toBe(false);
  });
});

describe("resume after a drop with a non-empty client queue (ST-09, reviewer 14.09)", () => {
  const g1 = { generation: 1, epoch: "flow#1" };
  const live = { flowBacklog: true, unapplied: true, hasEpoch: true, current: g1, handed: { ...g1, end: 531_072 }, applied: 400_000 };

  it("noteHanded only grows within one guard and restarts on a new one", () => {
    let m = noteHanded(null, g1, 100);
    expect(m).toEqual({ generation: 1, epoch: "flow#1", end: 100 });
    m = noteHanded(m, g1, 90);
    expect(m!.end).toBe(100);
    m = noteHanded(m, g1, 228);
    expect(m!.end).toBe(228);
    m = noteHanded(m, { generation: 1, epoch: "flow#2" }, 5);
    expect(m).toEqual({ generation: 1, epoch: "flow#2", end: 5 });
    expect(noteHanded(m, g1, NaN)).toBeNull();
    expect(noteHanded(m, g1, -1)).toBeNull();
  });

  it("keeps the old rule without unapplied work and on rollback (flowBacklog=false)", () => {
    expect(reconnectResume({ ...live, unapplied: false })).toEqual({ kind: "accepted" });
    expect(reconnectResume({ ...live, unapplied: false, flowBacklog: false })).toEqual({ kind: "accepted" });
    expect(reconnectResume({ ...live, flowBacklog: false })).toEqual({ kind: "reset", why: "rollback" });
  });

  it("ST-05 tail: a held ESC[3J prefix is re-requested, not lost on a warm resume", () => {
    const idle = { ...live, unapplied: false, accepted: 10_000 };
    // Удержан 1 байт ESC: resume на байт раньше, сервер дошлёт его сам.
    expect(reconnectResume({ ...idle, heldTail: 1 })).toEqual({ kind: "handed", offset: 9_999 });
    expect(reconnectResume({ ...idle, heldTail: 3 })).toEqual({ kind: "handed", offset: 9_997 });
    // Без хвоста — прежнее правило.
    expect(reconnectResume({ ...idle, heldTail: 0 })).toEqual({ kind: "accepted" });
    expect(reconnectResume({ ...idle })).toEqual({ kind: "accepted" });
    // Откат, нет эпохи, мусор — прежнее правило, позиция не выдумывается.
    expect(reconnectResume({ ...idle, heldTail: 2, flowBacklog: false })).toEqual({ kind: "accepted" });
    expect(reconnectResume({ ...idle, heldTail: 2, hasEpoch: false })).toEqual({ kind: "accepted" });
    expect(reconnectResume({ ...idle, heldTail: 2, accepted: 1 })).toEqual({ kind: "accepted" });
    expect(reconnectResume({ ...idle, heldTail: NaN })).toEqual({ kind: "accepted" });
    expect(reconnectResume({ ...idle, heldTail: 2, accepted: NaN })).toEqual({ kind: "accepted" });
  });

  it("resumes from what xterm was handed instead of dropping the position", () => {
    // Сценарий ревьюера: 2 МБ приняты, в xterm отдано 128 КиБ сверх 400 000.
    expect(reconnectResume(live)).toEqual({ kind: "handed", offset: 531_072 });
    // Отложенное стирание в конце цепочки разобрано без записи: applied впереди.
    expect(reconnectResume({ ...live, applied: 531_076 })).toEqual({ kind: "handed", offset: 531_076 });
  });

  it("falls back to reset only when the mark does not describe the screen", () => {
    expect(reconnectResume({ ...live, hasEpoch: false })).toEqual({ kind: "reset", why: "no-epoch" });
    expect(reconnectResume({ ...live, handed: null })).toEqual({ kind: "reset", why: "no-mark" });
    // Маркер сменил эпоху писателя, а его граница (RIS, режимы) ещё не отдана.
    expect(reconnectResume({ ...live, current: { generation: 1, epoch: "flow#2" } })).toEqual({ kind: "reset", why: "stale-mark" });
    expect(reconnectResume({ ...live, current: { generation: 2, epoch: "flow#1" } })).toEqual({ kind: "reset", why: "stale-mark" });
  });

  it("with the real TerminalWriter the mark equals exactly what reaches xterm after the generation change", () => {
    // xterm держит запись, пока её не «разберут»: так видно, что отдано, а что
    // только стоит в очереди writer.
    const held: Array<() => void> = [];
    const parsed: number[] = [];
    const size = (d: TerminalWriteData) => (typeof d === "string" ? d.length : d.byteLength);
    const writer = new TerminalWriter({ write: (d, cb) => { held.push(() => { parsed.push(size(d)); cb?.(); }); } }, g1);
    let mark: HandedMark | null = null;
    let applied = 0;
    const put = (bytes: number, end: number) => writer.lazy(() => {
      mark = noteHanded(mark, g1, end);
      return new Uint8Array(bytes);
    }, { guard: g1, after: () => { applied = end; } });
    put(128, 128); put(128, 256); put(128, 384);
    expect(writer.pending).toBe(3);
    expect(mark).toEqual({ ...g1, end: 128 });
    held.shift()!();
    expect(applied).toBe(128);
    expect(mark!.end).toBe(256);
    // connect(): решение ДО смены поколения.
    const plan = reconnectResume({ flowBacklog: true, unapplied: writer.pending > 0, hasEpoch: true, current: g1, handed: mark, applied });
    expect(plan).toEqual({ kind: "handed", offset: 256 });
    writer.setContext({ generation: 2, epoch: "flow#1" });
    // Отданную запись xterm дописывает, стоявшая в очереди выброшена.
    held.shift()!();
    expect(held).toHaveLength(0);
    expect(parsed.reduce((a, b) => a + b, 0)).toBe(256);
    // applied (128) позади экрана — resume с него повторил бы 128 байт;
    // принятое (384) впереди — resume с него потерял бы 128.
    expect(applied).toBe(128);
  });
});
