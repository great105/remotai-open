import { describe, expect, it } from "vitest";
import {
  CAPTURE_KEY, LEGACY_CAPTURE_KEY, captureTerminals, clearAllCaptures, clockLabel, fileExtension, readCaptureUntil,
  traceFileName, tracePreview, uploadDiagFields, writeCaptureUntil,
} from "./traceExport";
import { TerminalTrace, traceBundle } from "./terminalTrace";
import { ByteRecorder, RECORDING_TTL_MS } from "./traceRecording";

function memoryStorage(initial: Record<string, string> = {}) {
  const data = new Map(Object.entries(initial));
  return {
    data,
    getItem: (k: string) => (data.has(k) ? data.get(k)! : null),
    setItem: (k: string, v: string) => { data.set(k, v); },
    removeItem: (k: string) => { data.delete(k); },
  };
}

describe("capture switch in storage (I-15: never silently on)", () => {
  const now = 1_800_000_000_000;
  const map = (entries: Record<string, unknown>) => JSON.stringify({ t: entries });
  it("is off without a key, with junk, with an expired or non-numeric until", () => {
    expect(readCaptureUntil(null, now, "A")).toBe(0);
    expect(readCaptureUntil(memoryStorage(), now, "A")).toBe(0);
    for (const raw of ["x", "null", "[]", "1", map({ A: "9e15" }), map({ A: now - 1 }), map({ A: Number.NaN }),
      JSON.stringify({}), JSON.stringify({ t: [now + 5] }), JSON.stringify({ until: now + 5000 })]) {
      expect(readCaptureUntil(memoryStorage({ [CAPTURE_KEY]: raw }), now, "A"), raw).toBe(0);
    }
  });
  it("clamps a far-future until to now + TTL", () => {
    const far = memoryStorage({ [CAPTURE_KEY]: map({ A: now + 10 * RECORDING_TTL_MS }) });
    expect(readCaptureUntil(far, now, "A")).toBe(now + RECORDING_TTL_MS);
    const near = memoryStorage({ [CAPTURE_KEY]: map({ A: now + 5000 }) });
    expect(readCaptureUntil(near, now, "A")).toBe(now + 5000);
  });
  it("consent belongs to ONE terminal: B opened after A's consent does not record (wave 4, I-15)", () => {
    const s = memoryStorage();
    writeCaptureUntil(s, now + 1000, "A");
    expect(readCaptureUntil(s, now, "A")).toBe(now + 1000);
    expect(readCaptureUntil(s, now, "B")).toBe(0);
    writeCaptureUntil(s, now + 2000, "B");
    expect(captureTerminals(s, now).sort()).toEqual(["A", "B"]);
    // «Выключить» в A не трогает согласие B — и наоборот.
    writeCaptureUntil(s, 0, "A");
    expect(readCaptureUntil(s, now, "A")).toBe(0);
    expect(readCaptureUntil(s, now, "B")).toBe(now + 2000);
    // Истёкшие согласия вычищаются при записи, последнее снятое снимает ключ.
    writeCaptureUntil(s, 0, "B");
    expect(s.data.has(CAPTURE_KEY)).toBe(false);
  });
  it("the page-wide v1 consent is never honoured and is removed on the first write", () => {
    const s = memoryStorage({ [LEGACY_CAPTURE_KEY]: JSON.stringify({ until: now + 5000 }) });
    expect(readCaptureUntil(s, now, "A")).toBe(0);
    writeCaptureUntil(s, now + 1000, "A");
    expect(s.data.has(LEGACY_CAPTURE_KEY)).toBe(false);
    clearAllCaptures(s);
    expect(s.data.has(CAPTURE_KEY)).toBe(false);
    expect(readCaptureUntil(s, now, "A")).toBe(0);
  });
  it("write then read round-trips; 0 removes the key; a throwing storage is survived", () => {
    const s = memoryStorage();
    writeCaptureUntil(s, now + 1000, "A");
    expect(readCaptureUntil(s, now, "A")).toBe(now + 1000);
    writeCaptureUntil(s, 0, "A");
    expect(s.data.has(CAPTURE_KEY)).toBe(false);
    const broken = { getItem: () => { throw new Error("denied"); }, setItem: () => { throw new Error("denied"); }, removeItem: () => { throw new Error("denied"); } };
    expect(() => writeCaptureUntil(broken, now + 1, "A")).not.toThrow();
    expect(() => clearAllCaptures(broken)).not.toThrow();
    expect(readCaptureUntil(broken, now, "A")).toBe(0);
    expect(captureTerminals(broken, now)).toEqual([]);
  });
});

describe("tracePreview", () => {
  it("counts events, span and dropped; metadata while nothing is recorded", () => {
    let clock = 0;
    const trace = new TerminalTrace({ capacity: 4, streamCapacity: 0, now: () => clock, wallNow: () => 0 });
    for (let i = 0; i < 6; i++) { clock = i * 1000; trace.note("geom", null, { cols: 80 }); }
    const rec = new ByteRecorder({ now: () => 0 });
    const p = tracePreview(trace.snapshot(), rec.snapshot());
    expect(p).toMatchObject({ events: 4, dropped: 2, seconds: 3, content: "metadata", recording: false, until: 0, recordedBytes: 0,
      streamDropped: 0 });
  });
  it("names the stream eviction apart: frames before seq N are not in the file (wave 6)", () => {
    let clock = 0;
    const trace = new TerminalTrace({ capacity: 8, streamCapacity: 4, now: () => clock, wallNow: () => 0 });
    trace.note("gesture", null, null, "touch");
    for (let i = 0; i < 10; i++) { clock += 10; trace.noteRx(5, 7 * (i + 1), { sid: 0, gen: 1 }); } // дыры: каждый кадр — своя запись
    const p = tracePreview(trace.snapshot(), new ByteRecorder({ now: () => 0 }).snapshot());
    expect(p).toMatchObject({ events: 5, dropped: 6, streamDropped: 6, streamFirstSeq: 8 });
    // Снимок без новых полей (старый вызов) — ноль, а не NaN в подписи.
    expect(tracePreview({ events: [], dropped: 0, rejectedFields: 0 }, new ByteRecorder().snapshot()))
      .toMatchObject({ streamDropped: 0, streamFirstSeq: 0 });
  });
  it("says bytes exactly when traceBundle would carry the recording", () => {
    let wall = 1000;
    const trace = new TerminalTrace({ now: () => 0, wallNow: () => wall });
    const rec = new ByteRecorder({ now: () => wall });
    rec.enable();
    const seq = trace.noteRx(3, 3, { sid: 0, gen: 1 });
    rec.noteRx(new Uint8Array([1, 2, 3]), 3, 0, seq);
    const p = tracePreview(trace.snapshot(), rec.snapshot());
    expect(p).toMatchObject({ content: "bytes", recording: true, recordedBytes: 3, recordedChunks: 1, truncated: false });
    expect(traceBundle({}, trace, rec.snapshot()).content).toBe(p.content);
    // Выключенная, но не удалённая запись всё ещё попадёт в файл — предпросмотр
    // обязан это сказать, а не показать «только метаданные».
    rec.disable();
    expect(tracePreview(trace.snapshot(), rec.snapshot())).toMatchObject({ content: "bytes", recording: false, until: 0 });
    rec.clear();
    expect(tracePreview(trace.snapshot(), rec.snapshot()).content).toBe("metadata");
    wall++;
  });
});

describe("file name and labels", () => {
  it("builds a stable ASCII file name", () => {
    const name = traceFileName(new Date(2026, 8, 14, 7, 5, 9).getTime());
    expect(name).toBe("remotai-trace-20260914-070509.json");
    expect(traceFileName(new Date(2026, 8, 14, 7, 5, 9).getTime(), "cast")).toBe("remotai-trace-20260914-070509.cast");
    expect(clockLabel(new Date(2026, 8, 14, 7, 5, 9).getTime())).toBe("07:05");
  });
  it("extracts only a plain extension", () => {
    expect(fileExtension("Отчёт за май.PDF")).toBe("pdf");
    expect(fileExtension("C:\\dir.v2\\archive")).toBe("");
    expect(fileExtension(".bashrc")).toBe("");
    expect(fileExtension("a.tar.gz")).toBe("gz");
    expect(fileExtension("x.ex e")).toBe("");
    expect(fileExtension("x.abcdefghijk")).toBe("");
  });
});

describe("upload diag without the user's file name (I-15)", () => {
  it("masks the name, keeps size/type/code/status and scrubs the name from the error text", () => {
    const fields = uploadDiagFields(
      { name: "Паспорт Иванова.jpg", size: 1234, type: "image/jpeg" },
      { code: "too_large", status: 413, message: "upload of Паспорт Иванова.jpg failed" },
    );
    expect(fields).toEqual({ name: "*.jpg", size: 1234, type: "image/jpeg", code: "too_large", status: 413, message: "upload of * failed" });
    expect(JSON.stringify(fields)).not.toContain("Иванова");
  });
  it("tolerates a bare error and a name without extension", () => {
    expect(uploadDiagFields({ name: "README", size: 1 }, new Error("boom"))).toMatchObject({ name: "*", message: "boom", code: "", status: "" });
    expect(uploadDiagFields({}, "network down")).toMatchObject({ name: "*", size: -1, message: "network down" });
  });
});
