/**
 * Явно включаемая запись вывода терминала для воспроизведения (ST-01).
 *
 * Трасса (terminalTrace.ts) хранит только метаданные. Чтобы воспроизвести
 * дефект на изолированном стенде, нужны оригинальные байты с ТОЧНЫМИ границами
 * кадров сокета: дефекты склейки и разбора живут именно на границах. Но вывод
 * может содержать секреты, поэтому (I-15, T-37):
 * - запись ВЫКЛЮЧЕНА по умолчанию и включается только явно, с TTL (30 мин),
 *   чтобы не остаться включённой молча;
 * - хранение локальное, в памяти страницы, не больше 4 МиБ: вытесняются целые
 *   старые чанки, и это отмечается в truncatedBefore;
 * - ввод и clipboard не пишутся никогда: API для них нет, а служебные
 *   сообщения принимаются только серверные (reset/resumed/screen/exit);
 * - clear() действительно удаляет всё записанное.
 * Asciicast v3 — дополнительный экспорт для просмотра, а не замена записи:
 * преобразование помечается identical:false.
 */
import type { TraceBundle, TraceEvent } from "./terminalTrace";

export const RECORDING_CAP_BYTES = 4 * 1024 * 1024;
export const RECORDING_TTL_MS = 30 * 60 * 1000;
/** Служебные сообщения СЕРВЕРА, нужные replay. Клиентские (ввод, resize,
 * pause) сюда не входят по построению. */
export const RECORDED_CONTROL_TYPES = ["reset", "resumed", "screen", "exit"] as const;
const RECORDED_CONTROLS: ReadonlySet<string> = new Set<string>(RECORDED_CONTROL_TYPES);

export type RecordedChunk =
  | { k: "rx"; seq: number; t: number; end: number; bytes: Uint8Array }
  | { k: "ctl"; seq: number; t: number; json: string };

/** Что вытеснено: seq последнего вытесненного чанка и принятый offset конца
 * последнего вытесненного rx (null — вытеснялись только служебные). */
export interface TruncatedBefore { seq: number; end: number | null }

export interface DecodedRecording {
  until: number;
  capBytes: number;
  bytes: number;
  chunks: RecordedChunk[];
  truncatedBefore: TruncatedBefore | null;
  evictedChunks: number;
  evictedBytes: number;
  rejectedControls: number;
}

export interface RecordingSnapshot extends DecodedRecording {
  enabled: boolean;
}

export interface RecorderOptions {
  capBytes?: number;
  ttlMs?: number;
  /** Настенные часы для TTL (Date.now): until хранится в localStorage. */
  now?: () => number;
}

const chunkSize = (c: RecordedChunk) => (c.k === "rx" ? c.bytes.byteLength : c.json.length);

function isRecordedControl(json: string): boolean {
  try {
    const msg: unknown = JSON.parse(json);
    return !!msg && typeof msg === "object" && !Array.isArray(msg)
      && RECORDED_CONTROLS.has(String((msg as Record<string, unknown>).t));
  } catch { return false; }
}

export class ByteRecorder {
  readonly capBytes: number;
  readonly ttlMs: number;
  private readonly now: () => number;
  private untilMs = 0;
  private chunks: (RecordedChunk | undefined)[] = [];
  private head = 0;
  private total = 0;
  private truncated: TruncatedBefore | null = null;
  private evictedChunks = 0;
  private evictedBytes = 0;
  private rejectedControls = 0;

  constructor(options: RecorderOptions = {}) {
    const cap = options.capBytes ?? RECORDING_CAP_BYTES;
    const ttl = options.ttlMs ?? RECORDING_TTL_MS;
    this.capBytes = Number.isFinite(cap) && cap > 0 ? cap : RECORDING_CAP_BYTES;
    this.ttlMs = Number.isFinite(ttl) && ttl > 0 ? ttl : RECORDING_TTL_MS;
    this.now = options.now ?? Date.now;
  }

  get enabled(): boolean { return this.now() < this.untilMs; }
  get until(): number { return this.untilMs; }

  /**
   * Включить до untilMs (по умолчанию now + TTL). Срок из хранилища не может
   * быть дальше now + TTL: порченая или старая запись в localStorage не
   * оставит запись включённой молча. Возвращает действующий срок.
   */
  enable(untilMs?: number): number {
    const max = this.now() + this.ttlMs;
    this.untilMs = typeof untilMs === "number" && Number.isFinite(untilMs) ? Math.min(untilMs, max) : max;
    return this.untilMs;
  }

  /** Выключить; записанное остаётся до clear() или закрытия страницы. */
  disable(): void { this.untilMs = 0; }

  /** Копия бинарного кадра с его точной границей в принятом потоке. */
  noteRx(bytes: Uint8Array, acceptedEnd: number, t: number, seq: number): boolean {
    if (!this.enabled) return false;
    if (!this.makeRoom(bytes.byteLength, seq, acceptedEnd)) return false;
    this.chunks.push({ k: "rx", seq, t, end: acceptedEnd, bytes: bytes.slice() });
    this.total += bytes.byteLength;
    return true;
  }

  /** Служебное сообщение сервера как пришло (JSON-строка). Любой другой тип
   * отвергается и считается в rejectedControls. */
  noteControl(json: string, t: number, seq: number): boolean {
    if (!this.enabled) return false;
    if (typeof json !== "string" || !isRecordedControl(json)) { this.rejectedControls++; return false; }
    if (!this.makeRoom(json.length, seq, null)) return false;
    this.chunks.push({ k: "ctl", seq, t, json });
    this.total += json.length;
    return true;
  }

  snapshot(): RecordingSnapshot {
    return {
      enabled: this.enabled,
      until: this.untilMs,
      capBytes: this.capBytes,
      bytes: this.total,
      chunks: this.chunks.slice(this.head) as RecordedChunk[],
      truncatedBefore: this.truncated ? { ...this.truncated } : null,
      evictedChunks: this.evictedChunks,
      evictedBytes: this.evictedBytes,
      rejectedControls: this.rejectedControls,
    };
  }

  /** «Удалить запись»: всё записанное уходит из памяти. Включённость не
   * меняется — выключает отдельный disable(). */
  clear(): void {
    this.chunks = [];
    this.head = 0;
    this.total = 0;
    this.truncated = null;
    this.evictedChunks = 0;
    this.evictedBytes = 0;
    this.rejectedControls = 0;
  }

  private makeRoom(size: number, seq: number, end: number | null): boolean {
    if (size > this.capBytes) {
      // Кадр больше всего лимита: не влезет никогда. Всё до него и он сам
      // вытеснены — replay должен знать, что поток до этой точки неполон.
      while (this.head < this.chunks.length) this.evictOldest();
      this.evictedChunks++;
      this.evictedBytes += size;
      this.truncated = { seq, end: end ?? this.truncated?.end ?? null };
      this.compact();
      return false;
    }
    while (this.total + size > this.capBytes && this.head < this.chunks.length) this.evictOldest();
    this.compact();
    return true;
  }

  private evictOldest(): void {
    const c = this.chunks[this.head]!;
    this.chunks[this.head] = undefined;
    this.head++;
    const size = chunkSize(c);
    this.total -= size;
    this.evictedChunks++;
    this.evictedBytes += size;
    this.truncated = { seq: c.seq, end: c.k === "rx" ? c.end : this.truncated?.end ?? null };
  }

  private compact(): void {
    if (this.head === this.chunks.length) { this.chunks = []; this.head = 0; }
    else if (this.head > 1024 && this.head * 2 > this.chunks.length) {
      this.chunks = this.chunks.slice(this.head);
      this.head = 0;
    }
  }
}

// ── base64 по чанку: обратимо байт в байт, без btoa и без String.fromCharCode
// на мегабайтных массивах (переполнение стека аргументов) ──

const B64 = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/";
const B64_LOOKUP = new Int16Array(128).fill(-1);
for (let i = 0; i < B64.length; i++) B64_LOOKUP[B64.charCodeAt(i)] = i;

export function bytesToBase64(bytes: Uint8Array): string {
  let out = "";
  const n = bytes.length;
  let i = 0;
  for (; i + 2 < n; i += 3) {
    const v = (bytes[i] << 16) | (bytes[i + 1] << 8) | bytes[i + 2];
    out += B64[v >> 18] + B64[(v >> 12) & 63] + B64[(v >> 6) & 63] + B64[v & 63];
  }
  if (n - i === 1) {
    const v = bytes[i] << 16;
    out += B64[v >> 18] + B64[(v >> 12) & 63] + "==";
  } else if (n - i === 2) {
    const v = (bytes[i] << 16) | (bytes[i + 1] << 8);
    out += B64[v >> 18] + B64[(v >> 12) & 63] + B64[(v >> 6) & 63] + "=";
  }
  return out;
}

function b64Code(s: string, i: number): number {
  const c = s.charCodeAt(i);
  return c < 128 ? B64_LOOKUP[c] : -1;
}

/** null — строка не base64 (правленный или порченый файл). */
export function base64ToBytes(s: string): Uint8Array | null {
  if (typeof s !== "string" || s.length % 4 !== 0) return null;
  const pad = s.endsWith("==") ? 2 : s.endsWith("=") ? 1 : 0;
  const n = (s.length / 4) * 3 - pad;
  const out = new Uint8Array(n);
  let o = 0;
  for (let i = 0; i < s.length; i += 4) {
    const last = i + 4 === s.length;
    const c0 = b64Code(s, i);
    const c1 = b64Code(s, i + 1);
    const c2 = last && pad >= 2 ? 0 : b64Code(s, i + 2);
    const c3 = last && pad >= 1 ? 0 : b64Code(s, i + 3);
    if (c0 < 0 || c1 < 0 || c2 < 0 || c3 < 0) return null;
    const v = (c0 << 18) | (c1 << 12) | (c2 << 6) | c3;
    out[o++] = v >> 16;
    if (o < n) out[o++] = (v >> 8) & 255;
    if (o < n) out[o++] = v & 255;
  }
  return out;
}

export type EncodedChunk =
  | { k: "rx"; seq: number; t: number; end: number; b64: string }
  | { k: "ctl"; seq: number; t: number; json: string };

export interface EncodedRecording {
  v: 1;
  until: number;
  capBytes: number;
  bytes: number;
  truncatedBefore: TruncatedBefore | null;
  evictedChunks: number;
  evictedBytes: number;
  rejectedControls: number;
  chunks: EncodedChunk[];
}

export function encodeRecording(rec: DecodedRecording): EncodedRecording {
  return {
    v: 1,
    until: rec.until,
    capBytes: rec.capBytes,
    bytes: rec.bytes,
    truncatedBefore: rec.truncatedBefore ? { ...rec.truncatedBefore } : null,
    evictedChunks: rec.evictedChunks,
    evictedBytes: rec.evictedBytes,
    rejectedControls: rec.rejectedControls,
    chunks: rec.chunks.map((c): EncodedChunk => (c.k === "rx"
      ? { k: "rx", seq: c.seq, t: c.t, end: c.end, b64: bytesToBase64(c.bytes) }
      : { k: "ctl", seq: c.seq, t: c.t, json: c.json })),
  };
}

const num = (v: unknown): v is number => typeof v === "number" && Number.isFinite(v);

/** Обратное encodeRecording. null — формат не наш или повреждён. */
export function decodeRecording(value: unknown): DecodedRecording | null {
  if (!value || typeof value !== "object") return null;
  const r = value as Record<string, unknown>;
  if (r.v !== 1 || !Array.isArray(r.chunks)) return null;
  const chunks: RecordedChunk[] = [];
  for (const raw of r.chunks) {
    if (!raw || typeof raw !== "object") return null;
    const c = raw as Record<string, unknown>;
    if (!num(c.seq) || !num(c.t)) return null;
    if (c.k === "rx") {
      if (!num(c.end) || typeof c.b64 !== "string") return null;
      const bytes = base64ToBytes(c.b64);
      if (!bytes) return null;
      chunks.push({ k: "rx", seq: c.seq, t: c.t, end: c.end, bytes });
    } else if (c.k === "ctl") {
      if (typeof c.json !== "string") return null;
      chunks.push({ k: "ctl", seq: c.seq, t: c.t, json: c.json });
    } else return null;
  }
  let truncatedBefore: TruncatedBefore | null = null;
  if (r.truncatedBefore !== null && r.truncatedBefore !== undefined) {
    const tb = r.truncatedBefore as Record<string, unknown>;
    if (typeof tb !== "object" || !num(tb.seq) || !(tb.end === null || num(tb.end))) return null;
    truncatedBefore = { seq: tb.seq, end: tb.end as number | null };
  }
  return {
    until: num(r.until) ? r.until : 0,
    capBytes: num(r.capBytes) ? r.capBytes : RECORDING_CAP_BYTES,
    bytes: num(r.bytes) ? r.bytes : 0,
    chunks,
    truncatedBefore,
    evictedChunks: num(r.evictedChunks) ? r.evictedChunks : 0,
    evictedBytes: num(r.evictedBytes) ? r.evictedBytes : 0,
    rejectedControls: num(r.rejectedControls) ? r.rejectedControls : 0,
  };
}

// ── asciicast v3 (https://docs.asciinema.org/manual/asciicast/v3/) ──
// Заголовок {"version":3,"term":{"cols","rows"}}, дальше строки
// [интервал в секундах от ПРЕДЫДУЩЕГО события, код, данные]; коды "o" вывод,
// "r" "COLSxROWS", "m" метка, "x" код выхода. Неизвестные поля заголовка
// проигрыватели обязаны игнорировать — туда кладём пометку identical:false.

export interface AsciicastExport {
  cast: string;
  identical: false;
  outputEvents: number;
  resizes: number;
  markers: number;
  truncated: boolean;
}

type CastItem = { t: number; seq: number; order: number; chunk?: RecordedChunk; event?: TraceEvent };

/** ESC c — полный сброс терминала, как клиент делает перед новой эпохой потока. */
const RIS = String.fromCharCode(0x1b) + "c";

function controlOf(json: string): Record<string, unknown> | null {
  try {
    const msg: unknown = JSON.parse(json);
    return msg && typeof msg === "object" && !Array.isArray(msg) ? msg as Record<string, unknown> : null;
  } catch { return null; }
}

const gridSize = (n: unknown): number | null =>
  typeof n === "number" && Number.isSafeInteger(n) && n > 0 && n < 10_000 ? n : null;

/**
 * Преобразование bundle в asciicast v3 для просмотра проигрывателем.
 *
 * Не байтовая копия (identical:false): кадры экрана (screen) клиент применяет
 * своей логикой, их здесь заменяет метка "screen"; reset дополнен RIS, как
 * делает клиент перед новой эпохой. UTF-8 декодируется потоково
 * (TextDecoder stream:true), поэтому символ, разорванный между кадрами
 * сокета, не превращается в U+FFFD. На reset/resumed незавершённый символ
 * старого потока сбрасывается — он и правда оборван.
 */
export function toAsciicastV3(bundle: TraceBundle, term: { cols: number; rows: number }): AsciicastExport {
  const rec = bundle.recording ? decodeRecording(bundle.recording) : null;
  const items: CastItem[] = [];
  rec?.chunks.forEach((chunk, order) => items.push({ t: chunk.t, seq: chunk.seq, order, chunk }));
  bundle.events.forEach((event, order) => {
    if (event.kind === "geom" || event.kind === "mark") items.push({ t: event.t, seq: event.seq, order, event });
  });
  items.sort((a, b) => a.t - b.t || a.seq - b.seq || (a.chunk ? 0 : 1) - (b.chunk ? 0 : 1) || a.order - b.order);

  let cols = gridSize(term.cols) ?? 80;
  let rows = gridSize(term.rows) ?? 24;
  const truncated = !!rec?.truncatedBefore;
  const header: Record<string, unknown> = {
    version: 3,
    term: { cols, rows, type: "xterm-256color" },
    title: "Remotai terminal trace",
    x_remotai: { source: bundle.format, v: bundle.v, identical: false, truncated },
  };
  if (Number.isFinite(bundle.startedAtMs)) header.timestamp = Math.floor(bundle.startedAtMs / 1000);
  const lines: string[] = [JSON.stringify(header)];
  let prevMs = 0;
  let outputEvents = 0, resizes = 0, markers = 0;
  const emit = (tMs: number, code: "o" | "r" | "m" | "x", data: string) => {
    const ms = Math.max(prevMs, Math.round(tMs));
    lines.push(JSON.stringify([(ms - prevMs) / 1000, code, data]));
    prevMs = ms;
    if (code === "o") outputEvents++;
    else if (code === "r") resizes++;
    else if (code === "m") markers++;
  };
  let decoder = new TextDecoder("utf-8");
  const flushDecoder = (t: number) => {
    const rest = decoder.decode();
    decoder = new TextDecoder("utf-8");
    if (rest) emit(t, "o", rest);
  };

  for (const item of items) {
    const { chunk, event } = item;
    if (chunk?.k === "rx") {
      const text = decoder.decode(chunk.bytes, { stream: true });
      if (text) emit(item.t, "o", text);
    } else if (chunk?.k === "ctl") {
      const msg = controlOf(chunk.json);
      const type = msg ? String(msg.t) : "";
      if (type === "reset" || type === "resumed") {
        // Тёплый resumed продолжает поток с точного offset: символ, разрезанный
        // переподключением, xterm склеит — декодер тоже не сбрасываем. Оборван
        // поток только на reset и на resumed с разрывом.
        if (type === "reset" || msg?.gap) flushDecoder(item.t);
        emit(item.t, "m", type === "resumed" && msg?.gap ? "resumed gap" : type);
        if (type === "reset") emit(item.t, "o", RIS);
      } else if (type === "screen") {
        emit(item.t, "m", "screen");
      } else if (type === "exit") {
        flushDecoder(item.t);
        const code = msg?.code;
        if (typeof code === "number" && Number.isSafeInteger(code)) emit(item.t, "x", String(code));
        else emit(item.t, "m", "exit");
      }
    } else if (event?.kind === "geom") {
      const c = gridSize(event.f?.cols), r = gridSize(event.f?.rows);
      if (c !== null && r !== null && (c !== cols || r !== rows)) {
        cols = c; rows = r;
        emit(item.t, "r", `${c}x${r}`);
      }
    } else if (event?.kind === "mark") {
      emit(item.t, "m", event.reason ?? "");
    }
  }
  flushDecoder(prevMs);
  return { cast: lines.join("\n") + "\n", identical: false, outputEvents, resizes, markers, truncated };
}
