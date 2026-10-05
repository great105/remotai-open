/**
 * «Зафиксировать проблему» (ST-01): чистые правила листа экспорта трассы.
 *
 * Трасса (terminalTrace.ts) и запись вывода (traceRecording.ts) живут в памяти
 * страницы. Здесь то, что между ними и человеком:
 * - срок явно включённой записи вывода в хранилище (remotai.terminal.capture.v2,
 *   по id терминала), чтобы сценарий можно было снять с самого открытия
 *   терминала — и чтобы запись не осталась включённой молча: срок не дальше
 *   now + TTL, и согласие одного терминала не включает запись другого (I-15);
 * - предпросмотр перед сохранением: сколько событий и секунд, сколько
 *   вытеснено (и отдельно — до какого seq вытеснены кадры потока, волна 6),
 *   есть ли в файле вывод и сколько его;
 * - имя файла и поля диагностики загрузки без имени файла пользователя.
 * Модуль без React и DOM: проверяется в node.
 */
import type { TraceSnapshot } from "./terminalTrace";
import { RECORDING_TTL_MS, type RecordingSnapshot } from "./traceRecording";

/**
 * Согласие на запись вывода — ПО ТЕРМИНАЛУ (волна 4, приватность I-15/T-37):
 * {"t":{"<id терминала>": until}}. Прежний ключ v1 хранил одно согласие на
 * страницу, и любой терминал, открытый в течение 30 минут после включения
 * записи в другом, писал свой вывод без действия человека и без признака.
 */
export const CAPTURE_KEY = "remotai.terminal.capture.v2";
/** Согласие v1 (одно на страницу) не принимается никогда и снимается при записи. */
export const LEGACY_CAPTURE_KEY = "remotai.terminal.capture.v1";

/** localStorage, если он доступен; в песочнице/приватном режиме доступ бросает. */
export function captureStorage(): Storage | null {
  try { return typeof localStorage === "undefined" ? null : localStorage; } catch { return null; }
}

const captureId = (id: string) => String(id ?? "").slice(0, 128);

/** Сроки по терминалам; порченое — пустая карта. */
function readCaptureMap(storage: Pick<Storage, "getItem"> | null | undefined): Record<string, number> {
  try {
    const raw = storage?.getItem(CAPTURE_KEY);
    if (!raw) return {};
    const parsed: unknown = JSON.parse(raw);
    const t = parsed && typeof parsed === "object" && !Array.isArray(parsed) ? (parsed as Record<string, unknown>).t : null;
    if (!t || typeof t !== "object" || Array.isArray(t)) return {};
    const out: Record<string, number> = {};
    for (const [id, until] of Object.entries(t as Record<string, unknown>)) {
      if (typeof until === "number" && Number.isFinite(until)) out[id] = until;
    }
    return out;
  } catch { return {}; }
}

/**
 * Действующий срок записи вывода ЭТОГО терминала, мс. 0 — выключено, истекло,
 * порчено, согласие дано другому терминалу или хранилища нет. Срок дальше
 * now + TTL обрезается: порченая или подложенная запись не включит запись
 * вывода на часы.
 */
export function readCaptureUntil(
  storage: Pick<Storage, "getItem"> | null | undefined,
  now: number,
  terminalId: string,
  ttlMs = RECORDING_TTL_MS,
): number {
  const until = readCaptureMap(storage)[captureId(terminalId)];
  if (typeof until !== "number" || until <= now) return 0;
  return Math.min(until, now + ttlMs);
}

/** Терминалы с действующим согласием (для честного текста «Удалить запись»). */
export function captureTerminals(storage: Pick<Storage, "getItem"> | null | undefined, now: number): string[] {
  return Object.entries(readCaptureMap(storage)).filter(([, until]) => until > now).map(([id]) => id);
}

/**
 * Записать срок терминала (until > 0) или снять его согласие (0). Истёкшие
 * сроки вычищаются, пустая карта снимает ключ, согласие v1 снимается всегда.
 * Ошибки хранилища глотаются.
 */
export function writeCaptureUntil(
  storage: Pick<Storage, "getItem" | "setItem" | "removeItem"> | null | undefined,
  until: number,
  terminalId: string,
  now = Date.now(),
): void {
  try {
    storage?.removeItem(LEGACY_CAPTURE_KEY);
    const map = readCaptureMap(storage);
    const id = captureId(terminalId);
    if (Number.isFinite(until) && until > 0) map[id] = until;
    else delete map[id];
    for (const key of Object.keys(map)) if (map[key] <= now) delete map[key];
    if (Object.keys(map).length > 0) storage?.setItem(CAPTURE_KEY, JSON.stringify({ t: map }));
    else storage?.removeItem(CAPTURE_KEY);
  } catch { /* приватный режим: запись просто не переживёт перезагрузку */ }
}

/** «Удалить запись»: снять согласия всех терминалов страницы. */
export function clearAllCaptures(storage: Pick<Storage, "removeItem"> | null | undefined): void {
  try {
    storage?.removeItem(CAPTURE_KEY);
    storage?.removeItem(LEGACY_CAPTURE_KEY);
  } catch { /* приватный режим */ }
}

export interface TracePreview {
  /** Событий в кольце сейчас. */
  events: number;
  /** Сколько секунд они покрывают (от первого до последнего), с точностью 0,1 с. */
  seconds: number;
  /** Вытеснено из кольца с начала трассы. */
  dropped: number;
  /** Из них — записей потока (rx, parse-done) из его отдельного бюджета. */
  streamDropped: number;
  /** Кадры потока раньше этого seq вытеснены (значимо при streamDropped > 0). */
  streamFirstSeq: number;
  /** Полей, отброшенных allowlist: ошибка места вызова, а не содержимое в файле. */
  rejectedFields: number;
  /** "bytes" — в файл попадёт записанный вывод терминала. */
  content: "metadata" | "bytes";
  /** Запись вывода включена прямо сейчас. */
  recording: boolean;
  /** До какого времени (мс, настенные часы) включена; 0 — выключена. */
  until: number;
  recordedBytes: number;
  recordedChunks: number;
  /** Начало записи вытеснено лимитом: воспроизведение начнётся не с начала. */
  truncated: boolean;
}

/** Предпросмотр листа «Зафиксировать проблему». Тот же критерий «bytes», что у traceBundle. */
export function tracePreview(trace: Pick<TraceSnapshot, "events" | "dropped" | "rejectedFields">
  & Partial<Pick<TraceSnapshot, "streamDropped" | "streamFirstSeq">>, rec: RecordingSnapshot): TracePreview {
  const events = trace.events;
  let seconds = 0;
  if (events.length > 1) {
    const first = events[0].t;
    const last = events[events.length - 1];
    seconds = Math.max(0, Math.round(((last.tEnd ?? last.t) - first) / 100) / 10);
  }
  return {
    events: events.length,
    seconds,
    dropped: trace.dropped,
    streamDropped: trace.streamDropped ?? 0,
    streamFirstSeq: trace.streamFirstSeq ?? 0,
    rejectedFields: trace.rejectedFields,
    content: rec.chunks.length > 0 ? "bytes" : "metadata",
    recording: rec.enabled,
    until: rec.enabled ? rec.until : 0,
    recordedBytes: rec.bytes,
    recordedChunks: rec.chunks.length,
    truncated: !!rec.truncatedBefore,
  };
}

const pad2 = (n: number) => String(n).padStart(2, "0");

/** remotai-trace-ГГГГММДД-ЧЧММСС.json (или .cast — asciicast v3) по местному времени. */
export function traceFileName(wallMs: number, ext: "json" | "cast" = "json"): string {
  const d = new Date(Number.isFinite(wallMs) ? wallMs : 0);
  return `remotai-trace-${d.getFullYear()}${pad2(d.getMonth() + 1)}${pad2(d.getDate())}`
    + `-${pad2(d.getHours())}${pad2(d.getMinutes())}${pad2(d.getSeconds())}.${ext === "cast" ? "cast" : "json"}`;
}

/** «ЧЧ:ММ» для подписи «запись до …». */
export function clockLabel(wallMs: number): string {
  const d = new Date(wallMs);
  return `${pad2(d.getHours())}:${pad2(d.getMinutes())}`;
}

/** Расширение имени файла: только [a-z0-9]{1,10}, иначе "". */
export function fileExtension(name: string): string {
  const base = String(name || "").split(/[\\/]/).pop() || "";
  const dot = base.lastIndexOf(".");
  if (dot <= 0 || dot === base.length - 1) return "";
  const ext = base.slice(dot + 1).toLowerCase();
  return /^[a-z0-9]{1,10}$/.test(ext) ? ext : "";
}

/**
 * Поля diag «upload» без имени файла (I-15). Раньше имя уходило в журнал агента
 * как есть. Ключ name сохранён ради совместимости журнала: сервер (и старый, и
 * новый) берёт из него только расширение, поэтому вместо имени — маска «*.ext».
 * Имя вычищается и из текста ошибки, если сервер или браузер его туда вставил.
 */
export function uploadDiagFields(
  file: { name?: string; size?: number; type?: string },
  error: { code?: unknown; status?: unknown; message?: unknown } | unknown,
): Record<string, string | number> {
  const e = (error && typeof error === "object" ? error : {}) as { code?: unknown; status?: unknown; message?: unknown };
  const ext = fileExtension(file.name || "");
  let message = String(e.message ?? error ?? "");
  const name = String(file.name || "");
  if (name) message = message.split(name).join("*");
  return {
    name: ext ? `*.${ext}` : "*",
    size: typeof file.size === "number" && Number.isFinite(file.size) ? file.size : -1,
    type: typeof file.type === "string" ? file.type.slice(0, 64) : "",
    code: typeof e.code === "string" || typeof e.code === "number" ? e.code : "",
    status: typeof e.status === "number" || typeof e.status === "string" ? e.status : "",
    message: message.slice(0, 200),
  };
}
