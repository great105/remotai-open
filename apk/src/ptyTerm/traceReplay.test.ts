/**
 * Трасса клиента → проигрывание на стенде (ST-01). Файл пишет ЭТОТ клиент
 * (terminalTrace + traceRecording), разбирает стенд (build/qa/
 * terminal-replay-fixture.mjs). Проверяем, что между ними не теряется ни
 * байт, ни граница кадра сокета, ни принадлежность соединению, а правка файла
 * не проходит молча.
 */
import { describe, expect, it } from "vitest";
import { TerminalTrace, traceBundle } from "./terminalTrace";
import { ByteRecorder } from "./traceRecording";

type Replay = {
  parseReplayBundle: (input: unknown) => any;
  parseReplaySession: (id: string) => { run: number } | null;
  restampScreen: (json: string, request: { geom_rev?: number; req?: number } | null) => { json: string; changed: boolean };
};
const loadReplay = async (): Promise<Replay> =>
  await import(/* @vite-ignore */ new URL("../../../build/qa/terminal-replay-fixture.mjs", import.meta.url).href) as Replay;

const enc = (s: string) => new TextEncoder().encode(s);
const bytesOf = (b: Uint8Array) => Array.from(b);

/** Две сессии потока: разрыв и новое поколение, UTF-8 разорван между кадрами. */
function recordTwoConnections() {
  let clock = 0;
  const trace = new TerminalTrace({ now: () => clock, wallNow: () => 1_800_000_000_000 });
  const rec = new ByteRecorder({ now: () => 1_800_000_000_000 });
  rec.enable();
  const sid = trace.session("qa");
  const frames: Uint8Array[] = [];
  const control = (gen: number, msg: object, kind: "sync" | "screen-rx") => {
    clock += 5;
    const seq = trace.note(kind, { sid, gen }, null, (msg as { t: string }).t);
    rec.noteControl(JSON.stringify(msg), trace.time(), seq);
  };
  let offset = 0;
  const rx = (gen: number, bytes: Uint8Array) => {
    clock += 3;
    offset += bytes.byteLength;
    const seq = trace.noteRx(bytes.byteLength, offset, { sid, gen });
    rec.noteRx(bytes, offset, trace.time(), seq);
    frames.push(bytes);
  };
  control(1, { t: "reset", epoch: "e1", offset: 0, hb: 5 }, "sync");
  const smile = enc("A😀B"); // 😀 = 4 байта: режем посередине
  rx(1, smile.subarray(0, 3));
  rx(1, smile.subarray(3));
  rx(1, enc("\x1b[2J"));
  control(1, { t: "screen", geom_rev: 7, screen: "S", history: "", hist_lines: 1, screen_cols: 48, screen_rows: 1 }, "screen-rx");
  rx(1, enc("tail"));
  offset = 100;
  control(2, { t: "resumed", epoch: "e1", offset: 100, hb: 5 }, "sync");
  rx(2, enc("after"));
  return { trace, rec, frames };
}

describe("trace recording → replay plan", () => {
  it("keeps every byte, every socket-frame boundary and the connection split", async () => {
    const { parseReplayBundle } = await loadReplay();
    const { trace, rec, frames } = recordTwoConnections();
    const bundle = JSON.parse(JSON.stringify(traceBundle({ client: "t" }, trace, rec.snapshot())));
    const plan = parseReplayBundle(bundle);
    expect(plan.identical).toBe(true);
    expect(plan.connections.map((c: any) => c.gen)).toEqual([1, 2]);
    const rx = plan.connections.flatMap((c: any) => c.items.filter((it: any) => it.kind === "rx"));
    expect(rx.map((it: any) => bytesOf(it.bytes))).toEqual(frames.map(bytesOf));
    expect(rx.map((it: any) => it.end)).toEqual([3, 6, 10, 14, 105]);
    expect(plan.connections[0].marker).toMatchObject({ t: "reset", epoch: "e1", offset: 0 });
    expect(plan.connections[1].marker).toMatchObject({ t: "resumed", offset: 100 });
    expect(plan.connections[0].items.map((it: any) => it.kind === "ctl" ? it.type : "rx"))
      .toEqual(["reset", "rx", "rx", "rx", "screen", "rx"]);
    expect(plan.connections.map((c: any) => c.gaps)).toEqual([0, 0]);
    expect(plan.rxBytes).toBe(frames.reduce((n, f) => n + f.byteLength, 0));
  });

  it("splits by sync markers when the ring no longer has the events", async () => {
    const { parseReplayBundle } = await loadReplay();
    const { trace, rec } = recordTwoConnections();
    const bundle = JSON.parse(JSON.stringify(traceBundle({}, trace, rec.snapshot())));
    bundle.events = [];
    expect(parseReplayBundle(bundle).connections.length).toBe(2);
  });

  it("counts a hole in the accepted stream instead of hiding it", async () => {
    const { parseReplayBundle } = await loadReplay();
    const { trace, rec } = recordTwoConnections();
    const bundle = JSON.parse(JSON.stringify(traceBundle({}, trace, rec.snapshot())));
    const firstRx = bundle.recording.chunks.find((c: any) => c.k === "rx");
    firstRx.end += 1;
    expect(parseReplayBundle(bundle).connections[0].gaps).toBeGreaterThan(0);
  });

  it("refuses metadata-only traces, edited bytes and client-side controls", async () => {
    const { parseReplayBundle } = await loadReplay();
    const { trace, rec } = recordTwoConnections();
    const good = () => JSON.parse(JSON.stringify(traceBundle({}, trace, rec.snapshot())));
    expect(() => parseReplayBundle(traceBundle({}, trace, null))).toThrow(/нет записи вывода/);
    const noisy = good();
    noisy.recording.chunks.find((c: any) => c.k === "rx").b64 = "@@@@";
    expect(() => parseReplayBundle(noisy)).toThrow(/base64/);
    const nonCanonical = good();
    // «QQ==» и «QR==» декодируются в один байт — правка, которую нельзя пропустить.
    const one = nonCanonical.recording.chunks.find((c: any) => c.k === "rx");
    one.b64 = "QR==";
    expect(() => parseReplayBundle(nonCanonical)).toThrow(/обратим/);
    const input = good();
    input.recording.chunks.push({ k: "ctl", seq: 1e9, t: 1e9, json: JSON.stringify({ t: "input", data: "rm -rf" }) });
    expect(() => parseReplayBundle(input)).toThrow(/чужое служебное/);
    const other = good();
    other.format = "asciicast";
    expect(() => parseReplayBundle(other)).toThrow(/v1/);
  });

  it("marks a truncated recording as not identical", async () => {
    const { parseReplayBundle } = await loadReplay();
    const { trace, rec } = recordTwoConnections();
    const bundle = JSON.parse(JSON.stringify(traceBundle({}, trace, rec.snapshot())));
    bundle.recording.truncatedBefore = { seq: 1, end: 0 };
    expect(parseReplayBundle(bundle).identical).toBe(false);
  });
});

describe("replay helpers", () => {
  it("restamps a recorded frame with the live request revision and req", async () => {
    const { restampScreen } = await loadReplay();
    const json = JSON.stringify({ t: "screen", geom_rev: 7, screen: "S" });
    const same = restampScreen(json, { geom_rev: 7 });
    expect(JSON.parse(same.json)).toEqual({ t: "screen", geom_rev: 7, screen: "S" });
    expect(same.changed).toBe(false);
    const moved = restampScreen(json, { geom_rev: 9, req: 3 });
    expect(JSON.parse(moved.json)).toMatchObject({ geom_rev: 9, req: 3 });
    expect(moved.changed).toBe(true);
    expect(JSON.parse(restampScreen(JSON.stringify({ t: "screen", geom_rev: 1, req: 2 }), {}).json)).toEqual({ t: "screen" });
  });
  it("owns only qa-replay-<n> sessions", async () => {
    const { parseReplaySession } = await loadReplay();
    expect(parseReplaySession("qa-replay-3")).toEqual({ run: 3 });
    expect(parseReplaySession("qa-page-dead")).toBeNull();
    expect(parseReplaySession("qa-replay-x")).toBeNull();
  });
});
