import { describe, expect, it } from "vitest";
import {
  INPUT_SERIES_GAP_MS, TRACE_DURING_INPUT, TRACE_KINDS, TRACE_RESENT_SPAN, TRACE_SAMPLE_MIN_BYTES, TRACE_STREAM_CONTEXT,
  TRACE_STREAM_FIELDS, TerminalTrace, coarseLinesPerKib, inputSizeBucket, sanitizeFields, streamSampleBucket, traceBundle,
  traceNotes, type TraceEvent, type TraceKind,
} from "./terminalTrace";
import { ByteRecorder, decodeRecording } from "./traceRecording";
import { STREAM_SAMPLE_MIN_BYTES, STREAM_SCROLL_LINES_PER_KB } from "./altScroll";
import { t } from "@tgcontrol/shared";

const WALL = 1_757_000_000_123;
// Управляющий символ собирается из кода, а не пишется escape-строкой в исходнике.
const ESC = String.fromCharCode(0x1b);
/** Последний элемент (lib проекта без Array.prototype.at). */
function lastOf<T>(items: readonly T[]): T { return items[items.length - 1]; }
function makeTrace(capacity?: number, streamCapacity?: number) {
  const clock = { now: 1000 };
  const trace = new TerminalTrace({ capacity, streamCapacity, now: () => clock.now, wallNow: () => WALL });
  return { trace, clock };
}

/** Один кадр живого потока, как в PtyTermView: noteRx на приём, parse-done на спуск. */
function streamFrame(trace: TerminalTrace, clock: { now: number }, state: { end: number }, bytes = 120, ms = 100) {
  clock.now += ms;
  state.end += bytes;
  trace.noteRx(bytes, state.end, { sid: 0, gen: 1 });
  trace.note("parse-done", { sid: 0, gen: 1 }, { bytes, items: 1, end: state.end, ms: 1.5 }, "write");
}

describe("stream budget and rx/parse-done pairing (wave 4, ресурсы)", () => {
  it("a steady stream keeps two slots, and a gesture from before 10 minutes of it survives", () => {
    const { trace, clock } = makeTrace();
    trace.note("gesture", { sid: 0, gen: 1 }, { lines: -3 }, "touch");
    const state = { end: 0 };
    for (let i = 0; i < 10 * 60 * 10; i++) streamFrame(trace, clock, state); // 10 мин × 10 кадров/с
    const s = trace.snapshot();
    expect(s.events.map((e) => e.kind)).toEqual(["gesture", "rx", "parse-done"]);
    expect(s.events[1]).toMatchObject({ count: 6000, bytes: 720000, firstEnd: 120, lastEnd: 720000 });
    expect(s.events[2]).toMatchObject({ count: 6000, f: { bytes: 720000, items: 6000, end: 720000, ms: 1.5 } });
    expect(s.dropped).toBe(0);
  });

  it("an event between frames closes the pair; a hole in offsets is never merged", () => {
    const { trace, clock } = makeTrace();
    const state = { end: 0 };
    streamFrame(trace, clock, state);
    streamFrame(trace, clock, state);
    trace.note("gesture", { sid: 0, gen: 1 }, null, "touch");
    streamFrame(trace, clock, state);
    state.end += 5000; // дыра
    streamFrame(trace, clock, state);
    const kinds = trace.snapshot().events.map((e) => `${e.kind}${e.count ? `x${e.count}` : ""}`);
    expect(kinds).toEqual(["rxx2", "parse-donex2", "gesture", "rxx1", "parse-done", "rxx1", "parse-done"]);
  });

  it("stream evicts only stream: 1024 non-mergeable frames later every decision event is still there", () => {
    const { trace, clock } = makeTrace();
    const marks = [trace.mark("before"), trace.note("gesture", null, null, "touch"), trace.note("route", null, null, "local")];
    let end = 0;
    for (let i = 0; i < 5000; i++) { clock.now += 20; end += 7; trace.noteRx(5, end, { sid: 0, gen: 1 }); } // каждый кадр с дырой
    const s = trace.snapshot();
    for (const seq of marks) expect(s.events.some((e) => e.seq === seq)).toBe(true);
    expect(s.events.filter((e) => e.kind === "rx")).toHaveLength(1024);
    expect(s.dropped).toBe(5000 - 1024);
    expect(s.events.map((e) => e.seq)).toEqual([...s.events.map((e) => e.seq)].sort((a, b) => a - b));
  });
});

describe("input series (wave 4, приватность I-15)", () => {
  it("a typed password is one event: no key count, no exact length, no intervals", () => {
    const { trace, clock } = makeTrace();
    trace.noteRx(27, 27, { sid: 0, gen: 1 }); // приглашение sudo
    const gaps = [143.217, 88.931, 201.004, 97.56, 130.12, 76.79, 180.46, 110, 95.5];
    for (const gap of gaps) {
      clock.now += gap;
      trace.noteInput({ sid: 0 }, { len: 1, ok: true, auto: false }, "key");
    }
    const inputs = trace.snapshot().events.filter((e) => e.kind === "input");
    expect(inputs).toHaveLength(1);
    expect(inputs[0].f).toEqual({ bytes: "1-15", ok: true, auto: false });
    expect(inputs[0].t % 1000).toBe(0);
    expect(inputs[0]).not.toHaveProperty("count");
    expect(inputs[0]).not.toHaveProperty("tEnd");
    expect(JSON.stringify(inputs)).not.toMatch(/"len"/);
  });

  it("a pause starts a new series, a failed send and a paste stay visible, auto replies are their own series", () => {
    const { trace, clock } = makeTrace();
    trace.noteInput(null, { len: 1, ok: true, auto: false }, "key");
    clock.now += 50;
    trace.noteInput(null, { len: 1, ok: false, auto: false }, "key");
    clock.now += 50;
    trace.noteInput(null, { len: 3, ok: true, auto: true }, "key"); // ответ xterm приложению
    clock.now += INPUT_SERIES_GAP_MS + 1;
    trace.noteInput(null, { len: 40, ok: true, auto: false }, "key");
    trace.noteInput(null, { len: 700, ok: true, auto: false }, "encoded-paste");
    const f = trace.snapshot().events.map((e) => [e.reason, e.f]);
    expect(f).toEqual([
      ["key", { bytes: "1-15", ok: false, auto: false }],
      ["key", { bytes: "1-15", ok: true, auto: true }],
      ["key", { bytes: "16-63", ok: true, auto: false }],
      ["encoded-paste", { bytes: "256-1023", ok: true, auto: false }],
    ]);
    expect([0, 1, 15, 16, 63, 64, 255, 256, 1023, 1024].map(inputSizeBucket))
      .toEqual(["0", "1-15", "1-15", "16-63", "16-63", "64-255", "64-255", "256-1023", "256-1023", "1024+"]);
  });
});

describe("each ring's eviction is named in the snapshot and the file (wave 6)", () => {
  it("stream evicted, decisions kept: the file says frames before seq N are gone", () => {
    const { trace, clock } = makeTrace();
    const first = trace.mark("start");
    let end = 0;
    for (let i = 0; i < 2000; i++) { clock.now += 20; end += 7; trace.noteRx(5, end, { sid: 0, gen: 1 }); } // каждый кадр с дырой
    for (let i = 0; i < 991; i++) trace.note("gesture", null, null, "touch");
    const s = trace.snapshot();
    // Раньше: dropped=976 и firstSeq=1 — и ни слова о том, что вытеснен ПОТОК.
    expect(s).toMatchObject({ dropped: 976, streamDropped: 976, firstSeq: first, mainFirstSeq: first, streamCapacity: 1024 });
    expect(s.streamFirstSeq).toBe(first + 1 + 976);
    expect(s.events.filter((e) => e.kind === "rx")[0].seq).toBe(s.streamFirstSeq);
    const bundle = traceBundle({}, trace);
    expect(bundle).toMatchObject({ streamDropped: 976, streamFirstSeq: s.streamFirstSeq, mainFirstSeq: first, streamCapacity: 1024 });
    expect(bundle.notes).toEqual([`stream frames (rx, parse-done) before seq ${s.streamFirstSeq} were evicted: 976 records;`
      + " the first rx in this file is not the first frame received"]);
  });

  it("decisions evicted are named apart from the stream; a shared ring names all events; nothing lost — no notes", () => {
    const { trace } = makeTrace(8, 4); // основное 4, поток 4
    for (let i = 0; i < 6; i++) trace.note("gesture", null, null, "touch");
    trace.noteRx(3, 3, { sid: 0, gen: 1 });
    const s = trace.snapshot();
    expect(s).toMatchObject({ dropped: 2, streamDropped: 0, mainFirstSeq: 3, streamFirstSeq: 7, streamCapacity: 4 });
    expect(traceNotes(s)).toEqual(["decision events (all kinds except rx, parse-done) before seq 3 were evicted: 2"]);
    const shared = makeTrace(4, 0).trace;
    for (let i = 0; i < 6; i++) shared.note("geom");
    const ss = shared.snapshot();
    expect(ss).toMatchObject({ dropped: 2, streamDropped: 0, streamFirstSeq: 3, mainFirstSeq: 3, streamCapacity: 0 });
    expect(traceNotes(ss)).toEqual(["events before seq 3 were evicted: 2"]);
    const empty = makeTrace().trace;
    expect(empty.snapshot()).toMatchObject({ streamFirstSeq: 1, mainFirstSeq: 1, streamDropped: 0 });
    expect(traceBundle({}, empty).notes).toBeUndefined();
  });
});

/**
 * sudo с pwfeedback на живом клиенте: ctx с позициями, как traceCtx() в
 * PtyTermView; эхо «*» — кадр и его спуск; соседние события посреди набора.
 * Enter → "\r\n", команда думает дольше паузы серии, потом приглашение
 * (13 байт), дальше обычный вывод.
 */
function sudoSession(password: number, gaps: number[]) {
  const { trace, clock } = makeTrace();
  const st = { acc: 0, app: 0 };
  const ctx = () => ({ sid: 0, gen: 1, wep: 3, acc: st.acc, app: st.app, qEnd: st.acc, qBytes: st.acc - st.app,
    qLen: 0, wPending: 0, unacked: 0, geomRev: 2 });
  const frame = (bytes: number) => {
    st.acc += bytes;
    trace.noteRx(bytes, st.acc, ctx());
    clock.now += 2;
    st.app = st.acc;
    trace.note("parse-done", ctx(), { bytes, items: 1, end: st.acc, ms: 0.7 }, "write");
  };
  frame(26); // "[sudo] password for user: "
  clock.now += 900;
  for (let i = 0; i < password; i++) {
    clock.now += gaps[i];
    trace.noteInput(ctx(), { len: 1, ok: true, auto: false }, "key");
    clock.now += 3;
    frame(1); // эхо «*»
    if (i === 5) trace.note("diag", ctx(), { state: "periodic", maxQueued: 1, maxUnacked: 1, maxBatch: 1, pausedMs: 0 }, "flow");
  }
  clock.now += 120;
  trace.noteInput(ctx(), { len: 1, ok: true, auto: false }, "key"); // Enter
  clock.now += 3;
  frame(2); // "\r\n"
  clock.now += 2500; // команда думает: ответа нет дольше паузы серии
  trace.note("vis", ctx(), { ws: 1 }, "visible");
  trace.note("route", ctx(), { lines: 3, buffered: password }, "skip-buffered");
  trace.note("kbd", ctx(), { open: true, h: 400, w: 390, base: 844 }, "resize");
  frame(13); // "user@host:~$ "
  clock.now += 400;
  for (let i = 0; i < 5; i++) { clock.now += 20; frame(100); } // дальше — снова точно
  return trace.snapshot();
}
const GAPS_17 = [143.217, 88.931, 201.004, 97.56, 130.12, 76.79, 180.46, 110, 95.5, 160.3, 101.1, 122, 87.7, 140.4,
  99.9, 133.3, 111.1];
// Тот же набор без одного нажатия за то же время: две последние паузы слиты
// (+5 мс — эхо и спуск выпавшего нажатия в часах sudoSession).
const GAPS_16 = [...GAPS_17.slice(0, 15), GAPS_17[15] + GAPS_17[16] + 5];

describe("output during typing is coarse like the typing itself (wave 6, I-15)", () => {
  const exactKeys = ["count", "bytes", "firstEnd", "lastEnd", "tEnd"] as const;
  /** Всё, что отдало бы позицию или объём потока внутри отрезка набора. */
  const streamLeaks = (e: TraceEvent) => [
    ...TRACE_STREAM_CONTEXT.filter((k) => e.ctx && k in e.ctx).map((k) => `ctx.${k}`),
    ...(e.kind === "kbd" ? [] : TRACE_STREAM_FIELDS.filter((k) => typeof e.f?.[k] === "number").map((k) => `f.${k}`)),
    ...exactKeys.filter((k) => k in e),
  ];

  it("a 17-char password with '*' echo: the file does not give its length", () => {
    const s = sudoSession(17, GAPS_17);
    const firstKey = s.events.find((e) => e.kind === "input")!;
    const coarse = s.events.filter((e) => e.reason === TRACE_DURING_INPUT);
    const spanEnd = lastOf(coarse).seq;
    const span = s.events.filter((e) => e.seq >= firstKey.seq && e.seq <= spanEnd);
    // Отрезок: ввод, эхо до flow-diag, flow-diag, эхо после него с Enter и
    // "\r\n", vis/route/kbd, приглашение после паузы — каждое без позиций.
    expect(span.map((e) => `${e.kind}:${e.reason}`)).toEqual(["input:key", "rx:during-input", "parse-done:during-input",
      "diag:flow", "rx:during-input", "parse-done:during-input", "vis:visible", "route:skip-buffered", "kbd:resize",
      "rx:during-input", "parse-done:during-input"]);
    for (const e of span) expect(streamLeaks(e), `${e.kind} ${e.seq}`).toEqual([]);
    expect(coarse.map((e) => e.f?.bytes)).toEqual(["1-15", "1-15", "1-15", "1-15", "1-15", "1-15"]);
    expect(coarse.every((e) => e.t % 1000 === 0)).toBe(true);
    expect(span.find((e) => e.kind === "kbd")?.f).toEqual({ open: true, h: 400, w: 390, base: 844 }); // геометрия на месте
    expect(span.find((e) => e.kind === "diag")?.f).toEqual({ state: "periodic", pausedMs: 0 });
    // До и после отрезка — точно, как раньше: диагностика потока не потеряна.
    const before = s.events.filter((e) => e.seq < firstKey.seq);
    const after = s.events.filter((e) => e.seq > spanEnd);
    expect(before[0]).toMatchObject({ kind: "rx", bytes: 26, lastEnd: 26 });
    // Позиции ПОСЛЕ отрезка перебазированы на его конец (posBase): первый кадр
    // начинается со своего объёма (firstEnd 100 = bytes/count), а не с
    // абсолютной позиции 158; end спуска — 500, а не 558.
    expect(after[0]).toMatchObject({ kind: "rx", count: 5, bytes: 500, firstEnd: 100, lastEnd: 500 });
    expect(after[1]).toMatchObject({ kind: "parse-done", count: 5, f: { bytes: 500, items: 5, end: 500 } });
    // Вычесть позицию ДО отрезка (lastEnd 26) из позиции ПОСЛЕ теперь
    // бессмысленно: они в разных системах отсчёта. Объём отрезка (эхо + "\r\n" +
    // приглашение) и с ним длину пароля из файла не восстановить.
    expect(after[0].firstEnd).toBe(after[0].bytes! / after[0].count!);
    expect(traceNotes(s)).toEqual([
      expect.stringContaining(`"${TRACE_DURING_INPUT}": output received while a person typed`),
      expect.stringContaining("stream positions after a typing span are rebased"),
    ]);
  });

  it("16 and 17 chars: the whole file is the same, byte for byte — the length is not recoverable", () => {
    const s17 = sudoSession(17, GAPS_17), s16 = sudoSession(16, GAPS_16);
    // Раньше отличались точные позиции ПОСЛЕ отрезка на его объём (leak скептика
    // волны 6). Перебазирование убрало и это: события файла совпадают целиком.
    expect(JSON.stringify(s16.events)).toBe(JSON.stringify(s17.events));
    expect([s16.spansRebased, s17.spansRebased]).toEqual([1, 1]);
  });

  it("wrong-password retry: the second attempt length is not recoverable from the file", () => {
    // sudo с неверным паролем: после каждой попытки pam_faildelay ~2 с,
    // «Sorry, try again.\r\n» (19 б, закрывает отрезок), снова приглашение (26 б).
    // Скептик волны 6 вычислял длину 2-й попытки точно по границам вокруг неё.
    const g = (n: number) => Array.from({ length: n }, (_, i) => 90 + (i % 7) * 13);
    const sudoRetry = (attempts: number[]) => {
      const { trace, clock } = makeTrace();
      const st = { acc: 0, app: 0 };
      const ctx = () => ({ sid: 0, gen: 1, acc: st.acc, app: st.app, qEnd: st.acc, qBytes: st.acc - st.app });
      const frame = (bytes: number) => {
        st.acc += bytes; trace.noteRx(bytes, st.acc, ctx());
        clock.now += 2; st.app = st.acc;
        trace.note("parse-done", ctx(), { bytes, items: 1, end: st.acc, ms: 0.5 }, "write");
      };
      frame(26); // первое приглашение
      attempts.forEach((n) => {
        const gaps = g(n);
        clock.now += 700;
        for (let i = 0; i < n; i++) { clock.now += gaps[i]; trace.noteInput(ctx(), { len: 1, ok: true, auto: false }, "key"); clock.now += 3; frame(1); }
        clock.now += 100; trace.noteInput(ctx(), { len: 1, ok: true, auto: false }, "key"); clock.now += 3; frame(2); // Enter → "\r\n"
        clock.now += 2100; // pam_faildelay: ответа нет дольше паузы серии
        frame(19); // "Sorry, try again.\r\n" — закрывает отрезок этой попытки
        clock.now += 5; frame(26); // снова приглашение — точной записью, но с НОВОЙ базы
      });
      return trace.snapshot();
    };
    // Точные позиции приглашений (rx bytes 26): начало каждого — то, по чему
    // скептик вычислял длину предыдущей попытки. Обе попытки одной корзины.
    const promptEnds = (s: ReturnType<typeof sudoRetry>) =>
      s.events.filter((e) => e.kind === "rx" && e.reason === undefined && e.bytes === 26).map((e) => e.firstEnd);
    const a = sudoRetry([16, 16]);
    const b = sudoRetry([16, 17]);
    expect([a.spansRebased, b.spansRebased]).toEqual([2, 2]);
    // Раньше: firstEnd приглашения после 2-й попытки отличался на длину пароля
    // (leak). Перебазирование считает его от конца отрезка — 26 у обоих, и с
    // приглашением после 1-й попытки список одинаков независимо от N2.
    expect(promptEnds(a)).toEqual([26, 26, 26]);
    expect(promptEnds(b)).toEqual(promptEnds(a));
  });

  it("a pause inside the password leaves no exact position between its halves", () => {
    const { trace, clock } = makeTrace();
    let acc = 26;
    const ctx = () => ({ sid: 0, gen: 1, acc, app: acc, qEnd: acc });
    trace.noteRx(26, 26, ctx());
    const echo = () => {
      acc += 1;
      trace.noteRx(1, acc, ctx());
      trace.note("parse-done", ctx(), { bytes: 1, items: 1, end: acc, ms: 1 }, "write");
    };
    for (let i = 0; i < 10; i++) { clock.now += 100; trace.noteInput(ctx(), { len: 1, ok: true, auto: false }, "key"); echo(); }
    clock.now += 2000; // думает над второй половиной — кадров нет
    trace.note("geom", ctx(), { cols: 80, rows: 24 }, "fit");
    for (let i = 0; i < 7; i++) { clock.now += 100; trace.noteInput(ctx(), { len: 1, ok: true, auto: false }, "key"); echo(); }
    const s = trace.snapshot();
    const inputs = s.events.filter((e) => e.kind === "input");
    expect(inputs).toHaveLength(2); // две серии — пауза видна, их длины нет
    for (const e of s.events.filter((x) => x.seq >= inputs[0].seq)) expect(streamLeaks(e), `${e.kind} ${e.seq}`).toEqual([]);
    expect(s.events.filter((e) => e.kind === "rx" && e.reason === TRACE_DURING_INPUT).map((e) => e.f)).toEqual([{ bytes: "1-15" }, { bytes: "1-15" }]);
  });

  it("xterm's own replies open no span; the span ends only when its first answer after the pause is parsed", () => {
    const { trace, clock } = makeTrace();
    trace.noteInput({ sid: 0, gen: 1 }, { len: 6, ok: true, auto: true }, "key"); // ответ xterm приложению
    trace.noteRx(10, 10, { sid: 0, gen: 1, acc: 10 });
    expect(lastOf(trace.snapshot().events)).toMatchObject({ kind: "rx", bytes: 10, firstEnd: 10, ctx: { acc: 10 } });
    trace.noteInput(null, { len: 1, ok: true, auto: false }, "key");
    clock.now += INPUT_SERIES_GAP_MS;
    trace.noteRx(5, 15, { sid: 0, gen: 1 }); // первый кадр после паузы — ещё в отрезке
    trace.note("parse-done", { sid: 0, gen: 1 }, { bytes: 10, items: 1, end: 10 }, "write"); // старый спуск: не конец
    trace.noteRx(5, 20, { sid: 0, gen: 1 });
    trace.note("parse-done", { sid: 0, gen: 1 }, { bytes: 10, items: 2, end: 20 }, "write"); // разобрал ответ — конец
    trace.noteRx(7, 27, { sid: 0, gen: 1, acc: 27 });
    const events = trace.snapshot().events;
    expect(events.filter((e) => e.reason === TRACE_DURING_INPUT).map((e) => [e.kind, e.f])).toEqual([
      ["rx", { bytes: "1-15" }], ["parse-done", { bytes: "16-63" }]]);
    // После закрытия отрезка (posBase = 20) позиции точного rx считаются от него:
    // firstEnd/lastEnd/ctx.acc = 27 − 20 = 7, объём (bytes) остаётся 7.
    expect(lastOf(events)).toMatchObject({ kind: "rx", count: 1, bytes: 7, firstEnd: 7, lastEnd: 7, ctx: { acc: 7 } });
  });
});

describe("TerminalTrace ring (ST-01, ST01-U1)", () => {
  it("keeps the closed kind list of the plan", () => {
    expect([...TRACE_KINDS]).toEqual(["rx", "sync", "enqueue-drop", "parse", "parse-done", "erase", "gap", "writer",
      "screen-req", "screen-rx", "snap-reject", "snap-apply", "snap-watchdog", "barrier-release", "geom", "resize-send",
      "kbd", "conn", "overlay", "gesture", "route", "probe", "mode", "renderer", "flow", "vis", "input", "read", "diag",
      "mark", "presented", "recovery"]);
  });

  it("is bounded: evicts the oldest, counts dropped and reports firstSeq", () => {
    const { trace, clock } = makeTrace(4, 0); // одно общее кольцо
    expect(new TerminalTrace().streamCapacity).toBe(1024);
    for (const kind of ["conn", "sync", "parse", "parse-done", "geom", "flow"] as TraceKind[]) {
      clock.now += 10;
      trace.note(kind);
    }
    const s = trace.snapshot();
    expect(s.events.map(e => e.kind)).toEqual(["parse", "parse-done", "geom", "flow"]);
    expect(s.events.map(e => e.seq)).toEqual([3, 4, 5, 6]);
    expect(s.events.map(e => e.t)).toEqual([30, 40, 50, 60]);
    expect(s).toMatchObject({ dropped: 2, firstSeq: 3, nextSeq: 7, capacity: 4 });
    expect(new TerminalTrace().capacity).toBe(4096);
  });

  it("keeps time monotonic even when the injected clock goes back or breaks", () => {
    const { trace, clock } = makeTrace();
    clock.now = 1005; trace.note("conn");
    clock.now = 1003; trace.note("sync");
    clock.now = NaN; trace.note("parse");
    clock.now = 1010; trace.note("parse-done");
    expect(trace.snapshot().events.map(e => e.t)).toEqual([5, 5, 5, 10]);
  });

  it("coalesces consecutive contiguous rx of one session and generation only", () => {
    const { trace, clock } = makeTrace();
    const ctx = { sid: 0, gen: 1, qBytes: 10 };
    const first = trace.noteRx(10, 110, ctx);
    clock.now += 1;
    expect(trace.noteRx(5, 115, { ...ctx, qBytes: 15 })).toBe(first);
    clock.now += 1;
    expect(trace.noteRx(5, 120, { ...ctx, qBytes: 20 })).toBe(first);
    trace.note("parse", ctx);
    trace.noteRx(5, 125, ctx);
    trace.noteRx(5, 200, ctx); // разрыв offsets не прячется в слитой записи
    trace.noteRx(5, 205, { sid: 0, gen: 2 });
    trace.noteRx(5, 210, { sid: 1, gen: 2 });
    const events = trace.snapshot().events;
    expect(events.map(e => e.kind)).toEqual(["rx", "parse", "rx", "rx", "rx", "rx"]);
    expect(events[0]).toEqual({ seq: first, t: 0, kind: "rx", ctx: { sid: 0, gen: 1, qBytes: 20 },
      count: 3, bytes: 20, firstEnd: 110, lastEnd: 120, tEnd: 2 });
    expect(events[2]).toMatchObject({ count: 1, bytes: 5, firstEnd: 125, lastEnd: 125 });
    expect(events[3]).toMatchObject({ count: 1, firstEnd: 200 });
  });

  it("rejects unknown kinds and overlong reasons, labels marks, keeps seq across clear", () => {
    const { trace } = makeTrace();
    expect(trace.note("bogus" as TraceKind)).toBe(-1);
    trace.note("conn", null, null, "x".repeat(33));
    trace.note("conn", null, null, "закрыт");
    trace.note("conn", null, null, "close-1006");
    trace.mark("user");
    trace.mark("метка человека");
    let s = trace.snapshot();
    expect(s.events.map(e => e.reason)).toEqual([undefined, undefined, "close-1006", "user", "mark"]);
    expect(s.rejectedFields).toBe(3);
    trace.clear();
    s = trace.snapshot();
    expect(s.events).toEqual([]);
    expect(s.firstSeq).toBe(s.nextSeq);
    expect(trace.note("vis")).toBe(s.nextSeq);
  });

  it("maps session ids to a stable sid table", () => {
    const { trace } = makeTrace();
    expect([trace.session("abc"), trace.session("def"), trace.session("abc")]).toEqual([0, 1, 0]);
    expect(trace.snapshot().sessions).toEqual(["abc", "def"]);
  });
});

describe("sanitizeFields and traceBundle (I-15, T-37, ST01-U2)", () => {
  it("passes only finite numbers, booleans and short ASCII strings under non-content keys", () => {
    const report = { rejected: 0 };
    const clean = sanitizeFields({
      data: 1, text: "a", screen: true, history: 3, name: "x", clipboard: 1, path: "/", file: "f", Data: 2,
      fileName: "secret.txt", inputValue: "ls", fileBytes: 1234, screenRows: 40, pasteLen: 9,
      long: "x".repeat(33), ok32: "y".repeat(32), cyr: "привет", ctl: "a" + ESC + "[2J", nan: NaN, inf: Infinity,
      obj: { a: 1 }, arr: [1], nul: null, und: undefined, mode: "alt", flag: false, n: -3.5,
    }, report);
    expect(clean).toEqual({ fileBytes: 1234, screenRows: 40, pasteLen: 9, ok32: "y".repeat(32), mode: "alt", flag: false, n: -3.5 });
    expect(report.rejected).toBe(20);
    expect(Object.keys(sanitizeFields(JSON.parse('{"__proto__":"x","9lives":1,"has space":1,"ok":1}'))!)).toEqual(["ok"]);
    const many = Object.fromEntries(Array.from({ length: 30 }, (_, i) => [`k${i}`, i]));
    expect(Object.keys(sanitizeFields(many)!)).toHaveLength(24);
    expect(sanitizeFields({ data: "x" })).toBeUndefined();
    expect(sanitizeFields(null)).toBeUndefined();
    expect(sanitizeFields([1, 2])).toBeUndefined();
  });

  it("exports metadata only by default: no output bytes even if a caller passed content", () => {
    const { trace } = makeTrace();
    const sid = trace.session("s-1");
    trace.noteRx(6, 6, { sid, gen: 1 });
    trace.note("parse", { sid, gen: 1 }, { data: "SECRET-OUTPUT", text: "SECRET", batch: 6 }, "flush");
    trace.note("input", { sid }, { kind: "key", len: 1 });
    const rec = new ByteRecorder(); // выключен
    rec.noteRx(new TextEncoder().encode("SECRET"), 6, 0, 1);
    const bundle = traceBundle({ client: "2.71.1", commit: "a".repeat(40), surface: "apk",
      features: { trace: true, retention: "policy" }, junk: "x".repeat(200), bad: undefined }, trace, rec.snapshot());
    expect(bundle).toMatchObject({ format: "remotai-terminal-trace", v: 1, content: "metadata", identical: true,
      startedAtMs: WALL, exportedAtMs: WALL, sessions: ["s-1"], dropped: 0, firstSeq: 1, nextSeq: 4 });
    expect(bundle.build).toEqual({ client: "2.71.1", commit: "a".repeat(40), surface: "apk",
      features: { trace: true, retention: "policy" } });
    expect(bundle.recording).toBeUndefined();
    expect(bundle.events[1].f).toEqual({ batch: 6 });
    expect(JSON.stringify(bundle)).not.toContain("SECRET");
  });

  it("carries exact recorded bytes only after an explicit enable", () => {
    const { trace } = makeTrace();
    const rec = new ByteRecorder();
    rec.enable();
    const bytes = new Uint8Array([0x1b, 0x5b, 0x33, 0x4a, 0xf0, 0x9f]);
    const seq = trace.noteRx(bytes.byteLength, 6, { gen: 1 });
    rec.noteRx(bytes, 6, trace.time(), seq);
    const bundle = JSON.parse(JSON.stringify(traceBundle({ client: "dev" }, trace, rec.snapshot())));
    expect(bundle.content).toBe("bytes");
    const decoded = decodeRecording(bundle.recording)!;
    expect(decoded.chunks).toEqual([{ k: "rx", seq, t: 0, end: 6, bytes }]);
  });
});

describe("typing-span rebasing knows the stream epoch (wave 8, I-15, verify:cross wave 7)", () => {
  /** Маркер reset/resumed, как обработчик в PtyTermView (noteSync). */
  function sync(trace: TerminalTrace, ctx: Record<string, number>, fields: Record<string, unknown>, reason: string,
    marker: { epoch: string; offset: number }): number {
    return trace.noteSync(ctx, fields, reason, marker);
  }
  /**
   * Живой клиент: ctx как traceCtx() в PtyTermView (у маркера — состояние ДО
   * него). Сценарий скептика волны 7 до смены эпохи: приглашение sudo 26 Б,
   * N нажатий с эхом «*», Enter → "\r\n", пауза дольше серии, приглашение 13 Б
   * (закрывает отрезок), 300 Б вывода. База эпохи e1 = 26 + N + 2 + 13.
   */
  function typedSession(password: number) {
    const { trace, clock } = makeTrace();
    const st = { gen: 1, acc: 0, app: 0, qLen: 0 };
    const ctx = () => ({ sid: 0, gen: st.gen, wep: 1, acc: st.acc, app: st.app, qEnd: st.acc, qBytes: st.acc - st.app,
      qLen: st.qLen, wPending: 0, unacked: 0 });
    const frame = (bytes: number) => {
      st.acc += bytes;
      trace.noteRx(bytes, st.acc, ctx());
      clock.now += 2;
      st.app = st.acc;
      trace.note("parse-done", ctx(), { bytes, items: 1, end: st.acc, ms: 0.5 }, "write");
    };
    const marker = (t: "reset" | "resumed", epoch: string, offset: number) => {
      const seq = sync(trace, ctx(), { epochChanged: epoch !== "e1", offset }, t, { epoch, offset });
      st.acc = offset;
      st.app = offset;
      return seq;
    };
    const key = () => { trace.noteInput(ctx(), { len: 1, ok: true, auto: false }, "key"); };
    marker("reset", "e1", 0);
    frame(26);
    clock.now += 700;
    for (let i = 0; i < password; i++) { clock.now += 110; key(); clock.now += 3; frame(1); }
    // Тот же набор за то же время (как GAPS_16): время после отрезка точное, и
    // сравниваются позиции, а не часы.
    clock.now += (17 - password) * 115;
    clock.now += 120; key(); clock.now += 3; frame(2);
    clock.now += INPUT_SERIES_GAP_MS + 500;
    frame(13);
    for (let i = 0; i < 3; i++) { clock.now += 20; frame(100); }
    return { trace, clock, st, ctx, frame, marker, key };
  }
  /** Затем — перезапуск агента: новое соединение gen 2, новая эпоха с offset 0, 30 кадров по 20 Б. */
  function newEpochAfterTyping(password: number) {
    const s = typedSession(password);
    s.clock.now += 1000;
    s.st.gen = 2;
    s.trace.note("conn", { sid: 0, gen: 2 }, null, "open");
    s.marker("reset", "e2", 0);
    for (let i = 0; i < 30; i++) { s.clock.now += 10; s.frame(20); }
    return s.trace;
  }

  it("a new epoch after the span is exported raw (firstEnd 20, lastEnd 600), and 16 vs 17 chars give the same file", () => {
    const t16 = newEpochAfterTyping(16), t17 = newEpochAfterTyping(17);
    const s16 = t16.snapshot(), s17 = t17.snapshot();
    const syncE2 = s17.events.filter((e) => e.kind === "sync")[1];
    const after = s17.events.filter((e) => e.seq > syncE2.seq);
    const rx = after.find((e) => e.kind === "rx")!;
    // Было (скептик): {count 30, bytes 600, firstEnd 0, lastEnd 543}, и 600 − 543 = 57
    // = база e1 = 26 + 16 + 2 + 13 — длина пароля 16 (при 17 — 58).
    expect(rx).toMatchObject({ count: 30, bytes: 600, firstEnd: 20, lastEnd: 600, ctx: { acc: 600, qEnd: 600 } });
    expect(rx.bytes! - rx.lastEnd!).toBe(0);
    expect(after.find((e) => e.kind === "parse-done")).toMatchObject({ count: 30, f: { bytes: 600, items: 30, end: 600 } });
    // Маркер: ctx — прежняя эпоха от её базы (300 Б после отрезка), offset — новая эпоха.
    expect(syncE2).toMatchObject({ reason: "reset", f: { epochChanged: true, offset: 0 }, ctx: { gen: 2, acc: 300, app: 300, qEnd: 300 } });
    expect(JSON.stringify(s16.events)).toBe(JSON.stringify(s17.events));
    const bundle = traceBundle({}, t17);
    expect(bundle.identical).toBe(false);
    expect(bundle.notes).toContainEqual(expect.stringContaining("rebased to the span's end within its stream epoch"));
  });

  it("the same epoch re-sent from before the span's end (reconnect without resume) is coarse up to it", () => {
    const resend = (password: number) => {
      const s = typedSession(password);
      s.clock.now += 1000;
      s.st.gen = 2;
      s.trace.note("conn", { sid: 0, gen: 2 }, null, "open");
      s.marker("reset", "e1", 0); // вся эпоха заново: приглашение, эхо, ответ, вывод
      s.frame(26 + password + 2 + 13 + 300);
      for (let i = 0; i < 5; i++) { s.clock.now += 10; s.frame(10); }
      return s.trace.snapshot();
    };
    const a = resend(16), b = resend(17);
    // Было: досылка — точная запись rx.bytes 357 против 358, длина пароля из неё.
    expect(JSON.stringify(a.events)).toBe(JSON.stringify(b.events));
    const syncAgain = a.events.filter((e) => e.kind === "sync")[1];
    expect(syncAgain.f).toEqual({ epochChanged: false });
    expect(syncAgain.ctx).toEqual({ sid: 0, gen: 2, wep: 1 });
    const resent = a.events.filter((e) => e.reason === TRACE_RESENT_SPAN);
    expect(resent.map((e) => [e.kind, e.f?.bytes])).toEqual([["rx", "256-1023"], ["parse-done", "256-1023"]]);
    for (const e of resent) expect(["count", "bytes", "firstEnd", "lastEnd", "tEnd"].filter((k) => k in e)).toEqual([]);
    // Дальше — точно и от той же базы: до обрыва последний lastEnd был 300.
    const exact = a.events.filter((e) => e.seq > lastOf(resent).seq && e.kind === "rx");
    expect(exact).toEqual([expect.objectContaining({ count: 5, bytes: 50, firstEnd: 310, lastEnd: 350 })]);
    expect(traceNotes(a)).toContainEqual(expect.stringContaining(`"${TRACE_RESENT_SPAN}": the same stream epoch was re-sent`));
  });

  it("a warm resumed in the same epoch keeps exact positions from the same base", () => {
    const s = typedSession(16);
    s.st.gen = 2;
    s.marker("resumed", "e1", s.st.acc);
    s.frame(10);
    const events = s.trace.snapshot().events;
    expect(lastOf(events.filter((e) => e.kind === "sync")).f).toMatchObject({ offset: 300 });
    expect(lastOf(events.filter((e) => e.kind === "rx"))).toMatchObject({ count: 1, bytes: 10, firstEnd: 310, lastEnd: 310 });
    expect(events.some((e) => e.reason === TRACE_RESENT_SPAN)).toBe(false);
  });

  it("a mid-connection epoch change: old writes parsed after the marker lose end; the new epoch is raw", () => {
    const s = typedSession(16);
    s.st.acc += 40;
    s.trace.noteRx(40, s.st.acc, s.ctx()); // кадр прежней эпохи принят, запись ещё в очереди
    s.st.qLen = 1;
    const oldEnd = s.st.acc;
    const seq = s.marker("reset", "e2", 0); // тем же сокетом (gen 1)
    s.st.qLen = 0;
    s.trace.note("parse-done", s.ctx(), { bytes: 40, items: 1, end: oldEnd, ms: 1 }, "write"); // дописана старая
    s.trace.noteSyncDrained(seq);
    s.frame(20);
    const events = s.trace.snapshot().events;
    const marker = lastOf(events.filter((e) => e.kind === "sync"));
    const after = events.filter((e) => e.seq > marker.seq);
    // ctx маркера — прежняя эпоха от её базы (300 + 40), offset — новая.
    expect(marker).toMatchObject({ f: { offset: 0 }, ctx: { acc: 340, qLen: 1 } });
    expect(after[0]).toMatchObject({ kind: "parse-done", f: { bytes: 40, items: 1, ms: 1 } });
    expect(after[0].f).not.toHaveProperty("end");
    expect(after.find((e) => e.kind === "rx")).toMatchObject({ bytes: 20, firstEnd: 20, lastEnd: 20 });
    expect(lastOf(after)).toMatchObject({ kind: "parse-done", f: { end: 20 } });
    // Опущенный end — тоже правка: файл не identical, notes называет (скептик волны 8).
    const snap = s.trace.snapshot();
    expect(snap.seamCut).toBe(1);
    expect(traceNotes(snap)).toContainEqual(expect.stringContaining("parse-done at a stream marker"));
  });

  it("a span open at the epoch change closes on the new epoch's own frame, not on an old write", () => {
    const s = typedSession(16);
    s.clock.now += 3000;
    for (let i = 0; i < 5; i++) { s.clock.now += 100; s.key(); s.clock.now += 3; s.frame(1); }
    s.clock.now += INPUT_SERIES_GAP_MS + 100;
    s.st.acc += 7;
    s.trace.noteRx(7, s.st.acc, s.ctx()); // первый кадр после паузы прежней эпохи: принят, не разобран
    const oldEnd = s.st.acc;
    s.st.qLen = 1;
    const seq = s.marker("reset", "e2", 0);
    s.st.qLen = 0;
    s.st.acc += 20;
    s.trace.noteRx(20, s.st.acc, s.ctx()); // первый кадр новой эпохи — ещё в отрезке
    s.trace.note("parse-done", s.ctx(), { bytes: 7, items: 1, end: oldEnd, ms: 1 }, "write"); // старая запись
    s.trace.noteSyncDrained(seq);
    s.clock.now += 2;
    s.st.app = s.st.acc;
    s.trace.note("parse-done", s.ctx(), { bytes: 20, items: 1, end: 20, ms: 1 }, "write"); // разобран кадр e2 — конец
    s.frame(30);
    const snap = s.trace.snapshot();
    const marker = lastOf(snap.events.filter((e) => e.kind === "sync"));
    const after = snap.events.filter((e) => e.seq > marker.seq);
    expect(after.map((e) => `${e.kind}:${e.reason}`)).toEqual(["rx:during-input", "parse-done:during-input",
      "rx:undefined", "parse-done:write"]);
    // База e2 — конец отрезка в e2 (20): кадр 30 Б после него — 30.
    expect(after[2]).toMatchObject({ count: 1, bytes: 30, firstEnd: 30, lastEnd: 30 });
    expect(snap.spansRebased).toBe(2);
    // Маркер пришёл посреди отрезка: ни позиций, ни offset.
    expect(marker.f).toEqual({ epochChanged: true });
  });

  it("a stale position from before the span's end is left out — not clamped to 0, not negative", () => {
    const s = typedSession(16); // база e1 = 57, принято 357
    s.trace.note("screen-rx", s.ctx(), { frameRev: 3, base: 26, req: 1 }, "frame");
    s.trace.note("screen-rx", s.ctx(), { frameRev: 4, base: 357, req: 2 }, "frame");
    const frames = s.trace.snapshot().events.filter((e) => e.kind === "screen-rx");
    expect(frames.map((e) => e.f)).toEqual([{ frameRev: 3, req: 1 }, { frameRev: 4, base: 300, req: 2 }]);
  });

  it("«Удалить» clears the events, not the coordinates: positions after it stay rebased", () => {
    const s = typedSession(16);
    s.trace.clear();
    s.frame(10);
    const snap = s.trace.snapshot();
    expect(snap.events.map((e) => e.kind)).toEqual(["rx", "parse-done"]);
    expect(snap.events[0]).toMatchObject({ firstEnd: 310, lastEnd: 310, ctx: { acc: 310 } });
    expect(traceBundle({}, s.trace).identical).toBe(false);
    const plain = makeTrace().trace;
    plain.noteRx(10, 10, { sid: 0, gen: 1, acc: 10 });
    expect(traceBundle({}, plain).identical).toBe(true); // без набора — как снято
  });

  it("diag flow after a span: cumulative fields that grew through it are left out, later growth is written", () => {
    const { trace, clock } = makeTrace();
    const flow = (f: Record<string, number>) =>
      trace.note("diag", { sid: 0, gen: 1 }, { state: "periodic", ...f, pausedMs: 0 }, "flow");
    flow({ maxBatch: 900, maxQueued: 900, dropBytes: 0 });
    trace.noteInput({ sid: 0, gen: 1 }, { len: 1, ok: true, auto: false }, "encoded-paste"); // вставка секрета
    trace.noteRx(1200, 2100, { sid: 0, gen: 1 }); // эхо одним кадром — самый большой спуск
    clock.now += INPUT_SERIES_GAP_MS + 10;
    trace.noteRx(13, 2113, { sid: 0, gen: 1 });
    trace.note("parse-done", { sid: 0, gen: 1 }, { bytes: 1213, items: 2, end: 2113 }, "write");
    flow({ maxBatch: 1213, maxQueued: 900, dropBytes: 40 }); // рост через отрезок — не пишется
    flow({ maxBatch: 1213, maxQueued: 900, dropBytes: 40 }); // то же значение — тоже
    flow({ maxBatch: 5000, maxQueued: 900, dropBytes: 60 }); // максимум после отрезка — пишется; сумма — нет
    flow({ maxBatch: 300, maxQueued: 100, dropBytes: 0 }); // новый экран: счётчики с нуля
    const s = trace.snapshot();
    expect(s.events.filter((e) => e.kind === "diag").map((e) => e.f)).toEqual([
      { state: "periodic", maxBatch: 900, maxQueued: 900, dropBytes: 0, pausedMs: 0 },
      { state: "periodic", maxQueued: 900, pausedMs: 0 },
      { state: "periodic", maxQueued: 900, pausedMs: 0 },
      { state: "periodic", maxBatch: 5000, maxQueued: 900, pausedMs: 0 },
      { state: "periodic", maxBatch: 300, maxQueued: 100, dropBytes: 0, pausedMs: 0 },
    ]);
    expect(s.flowWithheld).toBe(3);
    expect(traceNotes(s)).toContainEqual(expect.stringContaining("diag flow: cumulative fields"));
  });
});

describe("app stream sample, identical and the legacy replay (wave 8, skeptic of wave 8)", () => {
  /**
   * Как PtyTermView: маркер — noteSync и noteSyncDrained из его барьера; замер
   * потока приложения (streamBytesRef, streamLinesRef) растёт на каждом спуске и
   * обнуляется на reset; жест в приложение — diag alt-scroll с этим замером
   * (sendDiag). Сценарий скептика: страница открыта внутри идущего ssh (reset e1,
   * offset 4096), кадр 200 Б, приглашение sudo 26 Б, N нажатий с эхом «*», Enter
   * (2 Б), пауза, ответ 13 Б закрывает отрезок, 3 кадра less по 100 Б, жест.
   */
  function sshSudoPager(password: number, typed = true) {
    const { trace, clock } = makeTrace();
    const st = { gen: 1, acc: 0, app: 0, stream: 0, lines: 0 };
    const ctx = () => ({ sid: 0, gen: st.gen, wep: 1, acc: st.acc, app: st.app, qEnd: st.acc, qBytes: st.acc - st.app,
      qLen: 0, wPending: 0, unacked: 0 });
    const frame = (bytes: number, lines = 0) => {
      st.acc += bytes;
      trace.noteRx(bytes, st.acc, ctx());
      clock.now += 2;
      st.app = st.acc;
      st.stream += bytes;
      st.lines += lines;
      trace.note("parse-done", ctx(), { bytes, items: 1, end: st.acc, ms: 0.5 }, "write");
    };
    const marker = (kind: "reset" | "resumed", epoch: string, offset: number) => {
      const seq = trace.noteSync(ctx(), { offset }, kind, { epoch, offset });
      st.acc = offset;
      st.app = offset;
      if (kind === "reset") { st.stream = 0; st.lines = 0; }
      trace.noteSyncDrained(seq);
    };
    const diag = (what: string) => trace.note("diag", ctx(),
      { alt: true, mouse: "none", owner: "application", lines: st.lines, bytes: st.stream, own: 40 }, what);
    const key = () => { trace.noteInput(ctx(), { len: 1, ok: true, auto: false }, "key"); };
    marker("reset", "e1", 4096);
    frame(200, 3);
    frame(26);
    clock.now += 700;
    if (typed) {
      for (let i = 0; i < password; i++) { clock.now += 110; key(); clock.now += 3; frame(1); }
      clock.now += (17 - password) * 115; // тот же набор за то же время: сравниваются поля, а не часы
      clock.now += 120; key(); clock.now += 3; frame(2, 1);
      clock.now += INPUT_SERIES_GAP_MS + 500;
      frame(13);
    }
    for (let i = 0; i < 3; i++) { clock.now += 20; frame(100); }
    diag("alt-scroll");
    return { trace, clock, st, frame, marker, diag };
  }
  const SAMPLE_OUT = { alt: true, mouse: "none", owner: "application", own: 40 };

  it("diag alt-scroll after a span keeps lines and a coarse volume, and 16 vs 17 chars give the same file (finding 1)", () => {
    const a = sshSudoPager(16), b = sshSudoPager(17);
    const sa = a.trace.snapshot(), sb = b.trace.snapshot();
    const alt = lastOf(sb.events);
    // Было (скептик): f.bytes 557 против 558 при ctx.app 300 у обоих, и
    // (4096 + bytes − app) − (4096 + 200 + 26) − 2 − 13 давало длину пароля.
    expect(alt).toMatchObject({ kind: "diag", reason: "alt-scroll", ctx: { app: 300 } });
    // Объём — корзиной (557 и 558 в одной), строки и глубина истории точные:
    // по ним и разбирают «не листается» (волна 9).
    expect(alt.f).toEqual({ ...SAMPLE_OUT, lines: 4, bytesRange: "1-1023", typed: true });
    expect(JSON.stringify(sa.events)).toBe(JSON.stringify(sb.events));
    expect(sb.windowCoarsened).toBe(1);
    const bundle = traceBundle({}, b.trace);
    expect(bundle.identical).toBe(false);
    expect(bundle.notes).toContainEqual(expect.stringContaining("the app stream sample"));
    // Без набора замер на месте: по нему разбирают «не листается».
    const plain = sshSudoPager(16, false);
    expect(lastOf(plain.trace.snapshot().events).f).toEqual({ ...SAMPLE_OUT, lines: 3, bytes: 526 });
    expect(traceBundle({}, plain.trace)).toMatchObject({ identical: true });
    expect(traceBundle({}, plain.trace).notes).toBeUndefined();
  });

  it("the volume stays coarse until a marker restarts the sample with no span in it", () => {
    const s = sshSudoPager(16);
    s.diag("alt-scroll-page-dead"); // тот же замер
    s.marker("resumed", "e1", s.st.acc); // тёплое продолжение: PtyTermView замер не обнуляет
    s.frame(50);
    s.diag("alt-scroll");
    s.st.gen = 2;
    s.marker("reset", "e2", 0); // новая эпоха: замер с нуля, набора в нём нет
    s.frame(80, 2);
    s.diag("alt-scroll");
    const snap = s.trace.snapshot();
    const coarse = { ...SAMPLE_OUT, lines: 4, bytesRange: "1-1023", typed: true };
    expect(snap.events.filter((e) => e.kind === "diag").map((e) => [e.reason, e.f])).toEqual([
      ["alt-scroll", coarse], ["alt-scroll-page-dead", coarse], ["alt-scroll", coarse],
      ["alt-scroll", { ...SAMPLE_OUT, lines: 2, bytes: 80 }],
    ]);
    expect(snap.windowCoarsened).toBe(3);
    // Переподключение без resume в той же эпохе досылает набранное: в новом
    // замере снова эхо, поля опущены.
    const r = sshSudoPager(16);
    r.st.gen = 2;
    r.marker("reset", "e1", 0);
    r.frame(4096 + 200 + 26 + 16 + 2 + 13 + 300);
    r.diag("alt-scroll");
    expect(lastOf(r.trace.snapshot().events).f).toEqual({ ...SAMPLE_OUT, lines: 0,
      bytesRange: "4096-16383", lpk: 0, typed: true });
    // На самом отрезке объём огрубляется тем же правилом, строки остаются.
    const { trace } = makeTrace();
    trace.noteInput({ sid: 0 }, { len: 1, ok: true, auto: false }, "key");
    trace.note("diag", { sid: 0 }, { owner: "application", lines: 7, bytes: 900, own: 40 }, "alt-scroll");
    expect(lastOf(trace.snapshot().events).f).toEqual({ owner: "application", own: 40, lines: 7,
      bytesRange: "1-1023", typed: true });
  });

  it("identical is false whenever a record is coarse or cut, and notes then name it (finding 3)", () => {
    // Скептик: reset e1, кадр 26 Б, одно нажатие человека, кадр 1 Б — было identical: true.
    const open = makeTrace().trace;
    open.noteSync({ sid: 0, gen: 1, qLen: 0, wPending: 0, unacked: 0 }, { offset: 0 }, "reset", { epoch: "e1", offset: 0 });
    open.noteRx(26, 26, { sid: 0, gen: 1, acc: 26 });
    open.noteInput({ sid: 0, gen: 1 }, { len: 1, ok: true, auto: false }, "key");
    open.noteRx(1, 27, { sid: 0, gen: 1, acc: 27 });
    const openBundle = traceBundle({}, open);
    expect(openBundle.events.map((e) => e.reason)).toContain(TRACE_DURING_INPUT);
    expect(openBundle.identical).toBe(false);
    // Огрублённые записи вытеснены кольцом потока, а событие посреди отрезка
    // осталось — без позиций: это тоже правка, и notes её называет.
    const small = makeTrace(64, 2);
    const c = { sid: 0, gen: 1, acc: 26, app: 26 };
    small.trace.noteRx(26, 26, c);
    small.trace.noteInput(c, { len: 1, ok: true, auto: false }, "key");
    small.trace.note("vis", { ...c, acc: 27 }, { ws: 1 }, "visible");
    small.trace.noteRx(1, 27, c);
    small.clock.now += INPUT_SERIES_GAP_MS + 10;
    small.trace.noteRx(13, 40, c);
    small.trace.note("parse-done", c, { bytes: 14, items: 2, end: 40 }, "write");
    small.trace.noteRx(10, 50, c);
    small.trace.note("parse-done", c, { bytes: 10, items: 1, end: 50 }, "write");
    const smallSnap = small.trace.snapshot();
    expect(smallSnap.events.some((e) => e.reason === TRACE_DURING_INPUT)).toBe(false);
    expect(smallSnap.events.find((e) => e.kind === "vis")?.ctx).toEqual({ sid: 0, gen: 1 });
    expect(smallSnap.events.find((e) => e.kind === "input")?.ctx).toEqual({ sid: 0, gen: 1 }); // нажатие открыло отрезок
    expect(smallSnap.streamCut).toBe(2);
    // identical ⇔ в notes нет ничего, кроме вытеснения (оно не правка).
    const plain = makeTrace().trace;
    plain.noteRx(10, 10, { sid: 0, gen: 1, acc: 10 });
    const evicted = makeTrace(8, 4).trace;
    for (let i = 0; i < 6; i++) evicted.note("gesture", null, null, "touch");
    const bundles = [openBundle, traceBundle({}, small.trace), traceBundle({}, plain), traceBundle({}, evicted),
      traceBundle({}, sshSudoPager(16).trace), traceBundle({}, sshSudoPager(16, false).trace)];
    const changed = (b: ReturnType<typeof traceBundle>) => (b.notes ?? []).filter((n) => !/were evicted/.test(n));
    expect(bundles.map((b) => b.identical)).toEqual([false, false, true, true, false, true]);
    for (const b of bundles) expect(b.identical, JSON.stringify(b.notes)).toBe(changed(b).length === 0);
    expect(changed(bundles[1])).toContainEqual(expect.stringContaining("carry no stream positions or volumes"));
  });

  it("after a span the sample is coarse but still decides: 16 and 17 chars give the same file (ST-01, skeptic of wave 9)", () => {
    // Скептик волны 9: bytes и lines просто не писались после ЛЮБОГО нажатия и
    // не возвращались до маркера — а именно ими принимается решение «своя
    // история или приложение» (historyOwnerFromStream: строк на КиБ против
    // порога), и без них жалобу «не листается» по файлу разобрать нечем.
    const run = (password: number) => {
      const s = sshSudoPager(password);
      for (let i = 0; i < 19; i++) s.frame(400, 1); // 7600 Б за 19 строк: замер выше порога вердикта
      s.diag("alt-scroll");
      return s.trace.snapshot();
    };
    const sa = run(16), sb = run(17);
    const alt = lastOf(sb.events);
    expect(alt).toMatchObject({ kind: "diag", reason: "alt-scroll" });
    // Решение читается: объём выше порога вердикта и строк на КиБ выше порога.
    expect(alt.f).toMatchObject({ bytesRange: "4096-16383", lines: 23, own: 40, typed: true });
    expect(typeof alt.f?.lpk).toBe("number");
    expect(alt.f?.lpk as number).toBeGreaterThanOrEqual(STREAM_SCROLL_LINES_PER_KB);
    // Точного объёма (8157 и 8158) в файле нет, и 16 против 17 неразличимы.
    expect(Object.values(alt.f ?? {})).not.toContain(541 + 17 + 7600);
    expect(JSON.stringify(sa.events)).toBe(JSON.stringify(sb.events));
  });

  it("a re-send of the same epoch while a typing span is still OPEN stays coarse to its end (finding 3)", () => {
    // Скептик волны 9: cover заводился только при offset < posBase, а на
    // ОТКРЫТОМ отрезке posBase ещё старая. Первый кусок досылки закрывал
    // отрезок как «кадр после паузы», дальше куски писались точно — и по их
    // позициям длина набранного восстанавливалась.
    const run = (password: number) => {
      const { trace, clock } = makeTrace();
      const st = { gen: 1, acc: 0 };
      const ctx = () => ({ sid: 0, gen: st.gen, acc: st.acc, app: st.acc, qEnd: st.acc, qBytes: 0,
        qLen: 0, wPending: 0, unacked: 0 });
      const frame = (bytes: number) => {
        st.acc += bytes;
        trace.noteRx(bytes, st.acc, ctx());
        clock.now += 2;
        trace.note("parse-done", ctx(), { bytes, items: 1, end: st.acc, ms: 0.5 }, "write");
      };
      const marker = (epoch: string, offset: number) => {
        const seq = trace.noteSync(ctx(), { offset }, "reset", { epoch, offset });
        st.acc = offset;
        trace.noteSyncDrained(seq);
      };
      const key = () => trace.noteInput(ctx(), { len: 1, ok: true, auto: false }, "key");
      marker("e1", 4096);
      frame(200); // вывод программы
      frame(26); // приглашение sudo
      clock.now += 700;
      for (let i = 0; i < password; i++) { clock.now += 110; key(); clock.now += 3; frame(1); }
      clock.now += (17 - password) * 115; // тот же набор за то же время: сравниваются поля, а не часы
      clock.now += 120; key(); clock.now += 3; frame(2); // Enter
      const spanEnd = st.acc; // отрезок НЕ закрыт: кадра после паузы не было
      // Переподключение БЕЗ resume в ту же эпоху: кольцо досылается с начала и
      // снова несёт эхо набранного.
      st.gen = 2;
      clock.now += INPUT_SERIES_GAP_MS + 500;
      marker("e1", 0);
      frame(2000); // первый кусок досылки
      frame(spanEnd - 2000); // досылка дошла до конца набранного
      frame(100);
      frame(100); // дальше живой поток — уже точные записи от конца отрезка
      return trace.snapshot();
    };
    const a = run(16), b = run(17);
    expect(JSON.stringify(a.events)).toBe(JSON.stringify(b.events));
    const rx = b.events.filter((e) => e.kind === "rx");
    expect(rx.some((e) => e.reason === TRACE_RESENT_SPAN || e.reason === TRACE_DURING_INPUT)).toBe(true);
    expect(lastOf(rx).lastEnd).toBe(200); // два кадра живого потока от конца отрезка
  });

  it("the window closes without a marker when the foreground process resets the sample (ST-01)", () => {
    const s = sshSudoPager(16);
    // Смена переднего процесса: PtyTermView обнуляет замер БЕЗ маркера (эффект
    // по state.fg_pid) и говорит об этом трассе — иначе объём оставался бы
    // огрублённым до самого маркера (скептик волны 9).
    s.st.stream = 0;
    s.st.lines = 0;
    s.trace.noteStreamSampleReset();
    s.frame(5000, 12);
    s.diag("alt-scroll");
    const snap = s.trace.snapshot();
    expect(lastOf(snap.events).f).toEqual({ ...SAMPLE_OUT, lines: 12, bytes: 5000 });
    expect(snap.windowCoarsened).toBe(1); // огрублён только замер ДО смены процесса
  });

  it("an open typing span keeps the window even when the sample is reset", () => {
    const { trace } = makeTrace();
    trace.noteInput({ sid: 0 }, { len: 1, ok: true, auto: false }, "key");
    trace.noteStreamSampleReset(); // новый замер соберёт то же эхо
    trace.note("diag", { sid: 0 }, { owner: "application", lines: 2, bytes: 8192, own: 40 }, "alt-scroll");
    expect(lastOf(trace.snapshot().events).f).toEqual({ owner: "application", own: 40, lines: 2,
      bytesRange: "4096-16383", lpk: 0.25, typed: true });
  });

  it("the sample floor and the rounded ratio match the decision in altScroll", () => {
    expect(TRACE_SAMPLE_MIN_BYTES).toBe(STREAM_SAMPLE_MIN_BYTES);
    expect(streamSampleBucket(STREAM_SAMPLE_MIN_BYTES - 1)).toBe("1024-4095");
    expect(streamSampleBucket(STREAM_SAMPLE_MIN_BYTES)).toBe("4096-16383");
    // Сам порог вердикта (2 строки/КиБ) округление не двигает.
    expect(coarseLinesPerKib(32, 16384)).toBe(STREAM_SCROLL_LINES_PER_KB);
    // Соседние объёмы неразличимы, вчетверо больший — уже другой.
    expect(coarseLinesPerKib(23, 8157)).toBe(coarseLinesPerKib(23, 8158));
    expect(coarseLinesPerKib(23, 8157)).not.toBe(coarseLinesPerKib(23, 8157 * 4));
  });

  it("a second attempt after a wrong password is not recoverable either (16 vs 17, repeat)", () => {
    const run = (password: number) => {
      const s = sshSudoPager(password); // первая попытка и ответ программы
      const key = () => s.trace.noteInput({ sid: 0, gen: 1 }, { len: 1, ok: true, auto: false }, "key");
      for (let i = 0; i < password; i++) { s.clock.now += 110; key(); s.clock.now += 3; s.frame(1); }
      s.clock.now += (17 - password) * 115;
      s.clock.now += 120; key(); s.clock.now += 3; s.frame(2, 1); // Enter
      s.clock.now += INPUT_SERIES_GAP_MS + 500;
      s.frame(13); // «Sorry, try again.» — известен побайтно, отрезок закрыт
      for (let i = 0; i < 19; i++) s.frame(400, 1); // замер выше порога вердикта
      s.diag("alt-scroll");
      return s.trace.snapshot();
    };
    expect(JSON.stringify(run(16).events)).toBe(JSON.stringify(run(17).events));
  });

  it("known limit: a legacy host replaying the same ring into a new epoch gives the span end, and the text says so (finding 2)", () => {
    // reattach_live.go: старый хост (2.57.20 и раньше) — полная переигровка того
    // же кольца в чистую эпоху с offset 0. Клиент её от свежего потока новой
    // эпохи не отличит: у обоих offset 0, а куски переигровки идут с известных
    // мест (wsPayloadChunk). Огрубление до прежней базы выдало бы её на свежем
    // потоке местом, где кончилось. Поэтому новая эпоха сырая, а предел назван словами.
    const replay = (password: number) => {
      const s = sshSudoPager(password);
      const q = lastOf(s.trace.snapshot().events.filter((e) => e.kind === "rx")).lastEnd!; // 300, от конца отрезка
      s.st.gen = 2;
      s.marker("reset", "e2", 0);
      s.frame(4096 + 200 + 26 + password + 2 + 13 + 300); // то же кольцо заново
      const x = lastOf(s.trace.snapshot().events.filter((e) => e.kind === "rx")).lastEnd!;
      return x - q - (4096 + 200 + 26) - 2 - 13;
    };
    expect([replay(16), replay(17)]).toEqual([16, 17]);
    const text = t("pty.traceMetaOnly");
    expect(text).toContain("проиграл тот же вывод с самого начала");
    expect(text).toContain("2.57.20");
    expect(text).toContain("пишутся только грубо");
    expect(text).toContain("счётчики строк и глубина истории — как есть");
    expect(text).toContain("вместе с ним длину набранного вычислить можно");
  });
});
