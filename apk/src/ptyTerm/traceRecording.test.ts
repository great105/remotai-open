import { describe, expect, it } from "vitest";
import {
  ByteRecorder, RECORDING_TTL_MS, base64ToBytes, bytesToBase64, decodeRecording, encodeRecording, toAsciicastV3,
} from "./traceRecording";
import { TerminalTrace, traceBundle } from "./terminalTrace";

// Управляющие символы собираются из кодов, а не пишутся escape-строками в
// исходнике (памятка edit-not-generated-strings).
const ESC = String.fromCharCode(0x1b);
const CRLF = String.fromCharCode(13, 10);
const REPLACEMENT = String.fromCharCode(0xfffd);
const enc = (s: string) => new TextEncoder().encode(s);

function mulberry32(seed: number) {
  return () => {
    seed |= 0; seed = (seed + 0x6D2B79F5) | 0;
    let t = Math.imul(seed ^ (seed >>> 15), 1 | seed);
    t = (t + Math.imul(t ^ (t >>> 7), 61 | t)) ^ t;
    return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
  };
}

describe("ByteRecorder (ST-01, I-15, ST01-U3)", () => {
  it("is off by default and switches itself off after the TTL", () => {
    const clock = { now: 1_000_000 };
    const rec = new ByteRecorder({ now: () => clock.now });
    expect(rec.enabled).toBe(false);
    expect(rec.noteRx(enc("a"), 1, 0, 1)).toBe(false);
    expect(rec.noteControl('{"t":"reset"}', 0, 2)).toBe(false);
    expect(rec.snapshot().chunks).toEqual([]);
    expect(rec.enable()).toBe(clock.now + RECORDING_TTL_MS);
    // Срок из хранилища не дальше TTL и не NaN: запись не останется включённой молча.
    expect(rec.enable(clock.now + 10 * RECORDING_TTL_MS)).toBe(clock.now + RECORDING_TTL_MS);
    expect(rec.enable(NaN)).toBe(clock.now + RECORDING_TTL_MS);
    rec.enable(clock.now + 1000);
    expect(rec.noteRx(enc("ab"), 2, 0, 1)).toBe(true);
    clock.now += 1000;
    expect(rec.enabled).toBe(false);
    expect(rec.noteRx(enc("c"), 3, 0, 2)).toBe(false);
    expect(rec.snapshot().chunks).toHaveLength(1);
    rec.enable();
    rec.disable();
    expect(rec.noteRx(enc("d"), 4, 0, 3)).toBe(false);
  });

  it("evicts whole old chunks under the byte cap and marks truncatedBefore", () => {
    const rec = new ByteRecorder({ capBytes: 64 });
    rec.enable();
    for (let i = 1; i <= 3; i++) expect(rec.noteRx(new Uint8Array(20).fill(i), 20 * i, i, i)).toBe(true);
    expect(rec.snapshot().truncatedBefore).toBeNull();
    expect(rec.noteControl('{"t":"exit"}', 4, 4)).toBe(true);
    let s = rec.snapshot();
    expect(s.chunks.map(c => c.seq)).toEqual([2, 3, 4]);
    expect(s.bytes).toBe(52);
    expect(s.truncatedBefore).toEqual({ seq: 1, end: 20 });
    // Кадр больше всего лимита не влезет никогда: всё до него неполно.
    expect(rec.noteRx(new Uint8Array(65), 125, 5, 5)).toBe(false);
    s = rec.snapshot();
    expect(s.chunks).toEqual([]);
    expect(s).toMatchObject({ bytes: 0, truncatedBefore: { seq: 5, end: 125 }, evictedChunks: 5, evictedBytes: 137 });
    expect(rec.noteRx(new Uint8Array(10), 135, 6, 6)).toBe(true);
    expect(rec.snapshot().chunks.map(c => c.seq)).toEqual([6]);
  });

  it("stores a private copy with the exact frame boundary", () => {
    const rec = new ByteRecorder();
    rec.enable();
    const frame = new Uint8Array([1, 2, 3, 4]);
    rec.noteRx(frame.subarray(1, 3), 7, 1.5, 9);
    frame.fill(0);
    expect(rec.snapshot().chunks).toEqual([{ k: "rx", seq: 9, t: 1.5, end: 7, bytes: new Uint8Array([2, 3]) }]);
  });

  it("accepts only server control messages and has no input or clipboard API", () => {
    const rec = new ByteRecorder();
    rec.enable();
    for (const json of ['{"t":"reset","epoch":"e","offset":0}', '{"t":"resumed","gap":true}',
      '{"t":"screen","screen":"x"}', '{"t":"exit"}']) expect(rec.noteControl(json, 0, 1)).toBe(true);
    for (const json of ['{"t":"input","data":"ls"}', '{"t":"clipboard","text":"pw"}', '{"t":"resize","cols":80}',
      '{"t":"pause"}', "[1]", "not json", ""]) expect(rec.noteControl(json, 0, 1)).toBe(false);
    expect(rec.snapshot().rejectedControls).toBe(7);
    expect(Object.getOwnPropertyNames(ByteRecorder.prototype).filter(n => /input|clip|paste|key/i.test(n))).toEqual([]);
  });

  it("clear() really deletes everything recorded (T-37)", () => {
    const rec = new ByteRecorder({ capBytes: 8 });
    rec.enable();
    rec.noteRx(enc("secret-1"), 8, 0, 1);
    rec.noteRx(enc("secret-2"), 16, 0, 2);
    rec.noteControl('{"t":"input"}', 0, 3);
    rec.clear();
    expect(rec.snapshot()).toMatchObject({ enabled: true, bytes: 0, chunks: [], truncatedBefore: null,
      evictedChunks: 0, evictedBytes: 0, rejectedControls: 0 });
    expect(JSON.stringify(encodeRecording(rec.snapshot()))).not.toContain(bytesToBase64(enc("secret-2")));
  });
});

describe("encodeRecording / decodeRecording", () => {
  it("round-trips bytes and chunk boundaries 1:1 through JSON (seeded)", () => {
    const rnd = mulberry32(2026);
    const rec = new ByteRecorder({ capBytes: 1 << 20 });
    rec.enable();
    let end = 0;
    for (let i = 0; i < 200; i++) {
      const n = Math.floor(rnd() * 300);
      const bytes = new Uint8Array(n);
      for (let j = 0; j < n; j++) bytes[j] = Math.floor(rnd() * 256);
      end += n;
      rec.noteRx(bytes, end, i * 1.5, i + 1);
      if (i % 50 === 0) rec.noteControl(`{"t":"resumed","offset":${end}}`, i * 1.5, i + 1);
    }
    const snap = rec.snapshot();
    const decoded = decodeRecording(JSON.parse(JSON.stringify(encodeRecording(snap))))!;
    expect(decoded.chunks).toEqual(snap.chunks);
    expect(decoded.bytes).toBe(snap.bytes);
    for (const c of snap.chunks) {
      if (c.k === "rx") expect(bytesToBase64(c.bytes)).toBe(btoa(String.fromCharCode(...c.bytes)));
    }
  });

  it("rejects edited or damaged data instead of guessing", () => {
    for (let n = 0; n <= 5; n++) {
      const bytes = new Uint8Array(n).map((_, i) => 250 + i);
      expect(base64ToBytes(bytesToBase64(bytes))).toEqual(bytes);
    }
    for (const bad of ["abc", "ab=c", "a$==", "=AAA", "AA=A"]) expect(base64ToBytes(bad)).toBeNull();
    expect(decodeRecording({ v: 2, chunks: [] })).toBeNull();
    expect(decodeRecording({ v: 1, chunks: [{ k: "in", seq: 1, t: 0 }] })).toBeNull();
    expect(decodeRecording({ v: 1, chunks: [{ k: "rx", seq: 1, t: 0, end: 1, b64: "!!!!" }] })).toBeNull();
    expect(decodeRecording(null)).toBeNull();
  });
});

describe("toAsciicastV3 (ST-01, ST01-U4)", () => {
  function recorded() {
    const clock = { now: 0 };
    const trace = new TerminalTrace({ now: () => clock.now, wallNow: () => 1_757_000_000_123 });
    const rec = new ByteRecorder();
    rec.enable();
    const rx = (bytes: Uint8Array, end: number) => {
      const seq = trace.noteRx(bytes.byteLength, end, { gen: 1 });
      rec.noteRx(bytes, end, trace.time(), seq);
    };
    const ctl = (json: string) => rec.noteControl(json, trace.time(), trace.note("sync", { gen: 1 }));
    return { clock, trace, rec, rx, ctl };
  }

  it("decodes UTF-8 split between frames without U+FFFD, emits r/m and marks identical:false", () => {
    const { clock, trace, rec, rx, ctl } = recorded();
    clock.now = 100; ctl('{"t":"reset","epoch":"e","offset":0}');
    const text = enc("hi 😀 ж" + CRLF);
    // Разрезы посреди эмодзи (F0 9F | 98 80) и посреди «ж» (D0 | B6).
    clock.now = 250; rx(text.subarray(0, 5), 5);
    clock.now = 400; rx(text.subarray(5, 9), 9);
    clock.now = 1000; rx(text.subarray(9), text.length);
    clock.now = 1200; trace.note("geom", { gen: 1 }, { cols: 100, rows: 30 }, "fit");
    clock.now = 1300; trace.note("geom", { gen: 1 }, { cols: 100, rows: 30 }, "same");
    clock.now = 1500; trace.mark("user");
    const out = toAsciicastV3(traceBundle({ client: "t" }, trace, rec.snapshot()), { cols: 80, rows: 24 });
    const [headerLine, ...lines] = out.cast.trimEnd().split("\n");
    expect(JSON.parse(headerLine)).toMatchObject({ version: 3, term: { cols: 80, rows: 24 }, timestamp: 1_757_000_000,
      x_remotai: { source: "remotai-terminal-trace", identical: false, truncated: false } });
    const events = lines.map(line => JSON.parse(line) as [number, string, string]);
    const output = events.filter(e => e[1] === "o").map(e => e[2]).join("");
    expect(output).toBe(ESC + "c" + "hi 😀 ж" + CRLF);
    expect(output).not.toContain(REPLACEMENT);
    expect(events.filter(e => e[1] === "r").map(e => e[2])).toEqual(["100x30"]);
    expect(events.filter(e => e[1] === "m").map(e => e[2])).toEqual(["reset", "user"]);
    expect(events.map(e => e[1])).not.toContain("i");
    // Интервалы относительные (от предыдущего события), в секундах.
    expect(events.every(e => e[0] >= 0)).toBe(true);
    expect(events.reduce((sum, e) => sum + e[0], 0)).toBeCloseTo(1.5, 9);
    expect(events[0]).toEqual([0.1, "m", "reset"]);
    expect(out).toMatchObject({ identical: false, outputEvents: 4, resizes: 1, markers: 2, truncated: false });
  });

  it("flushes a character cut by a new stream epoch honestly and works without bytes", () => {
    const { clock, trace, rec, rx, ctl } = recorded();
    clock.now = 10; rx(new Uint8Array([0x61, 0xf0, 0x9f]), 3);
    clock.now = 20; ctl('{"t":"resumed","gap":true}');
    clock.now = 30; rx(enc("b"), 4);
    const out = toAsciicastV3(traceBundle({}, trace, rec.snapshot()), { cols: 0, rows: -1 });
    const events = out.cast.trimEnd().split("\n").slice(1).map(line => JSON.parse(line));
    expect(events.filter(e => e[1] === "o").map(e => e[2]).join("")).toBe("a" + REPLACEMENT + "b");
    expect(events.filter(e => e[1] === "m").map(e => e[2])).toEqual(["resumed gap"]);
    expect(JSON.parse(out.cast.split("\n")[0]).term).toMatchObject({ cols: 80, rows: 24 });

    const meta = toAsciicastV3(traceBundle({}, trace, null), { cols: 80, rows: 24 });
    expect(meta.outputEvents).toBe(0);
    expect(meta.cast.trimEnd().split("\n")).toHaveLength(1);
  });

  it("keeps a character cut by a warm resume whole: the stream continues from the exact offset", () => {
    const { clock, trace, rec, rx, ctl } = recorded();
    const text = enc("ж😀");
    clock.now = 10; rx(text.subarray(0, 3), 3); // «ж» целиком + первый байт эмодзи
    clock.now = 20; ctl('{"t":"resumed"}');
    clock.now = 30; rx(text.subarray(3), text.length);
    const out = toAsciicastV3(traceBundle({}, trace, rec.snapshot()), { cols: 80, rows: 24 });
    const events = out.cast.trimEnd().split("\n").slice(1).map(line => JSON.parse(line));
    expect(events.filter(e => e[1] === "o").map(e => e[2]).join("")).toBe("ж😀");
    expect(events.filter(e => e[1] === "m").map(e => e[2])).toEqual(["resumed"]);
  });
});
