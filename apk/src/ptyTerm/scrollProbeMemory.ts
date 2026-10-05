/**
 * ПАМЯТЬ СВИДЕТЕЛЬСТВ НАВИГАЦИИ — СЕРИАЛИЗАЦИЯ, А НЕ ВТОРОЙ АЛГОРИТМ.
 *
 * С 13.09.2026 (план стабилизации, ST-02) правило актуальности живёт в
 * navigationEvidence.ts и применяется в момент решения. Здесь только перенос
 * наблюдения через закрытие экрана и новое соединение, пока область та же.
 *
 * Почему память вообще нужна — БОЕВОЙ ЛОГ 06.09.2026: в терминалах Codex каждое
 * открытие начиналось с пробы PgUp (1,2 с ожидания), потому что приговор жил
 * только в памяти экрана. Признак «строк на килобайт» агентов больше не
 * разделяет (Codex 1,9 при пороге 2, Claude 0,4–3,1) — надёжен только факт ответа.
 *
 * Правила сериализации:
 *   • ключ хранилища — компьютер + терминал; внутри до SCROLL_PROBE_SCOPES
 *     областей (процесс и его поколение, режим буфера и мыши, объявленный канал).
 *     Claude, переходящий между обычным и альтернативным экраном, не должен
 *     стирать наблюдение одного режима записью другого;
 *   • хранятся только confirmed и unconfirmed. Слабое наблюдение (один repaint)
 *     способность не доказывает и через переоткрытие не переносится;
 *   • у каждого канала своё время: срок и «вывод после молчания» считаются
 *     по нему, а не по последней записи соседнего канала;
 *   • текст экрана (якорь для «значимого вывода») НЕ хранится: он может
 *     содержать секреты, а хранилище синхронизируется браузером (I-15);
 *   • формат v2 не совместим с v1: старая запись не читается, а при записи
 *     стирается — отказ от кэша вместо угадывания прежнего смысла.
 */
import type { NavChannel, Observation } from "./navigationEvidence";

export type StoredEvidenceState = "confirmed" | "unconfirmed";

export interface StoredChannelEvidence {
  readonly state: StoredEvidenceState;
  /** Для unconfirmed — направления, в которых канал промолчал (1 вверх, 2 вниз). */
  readonly dead: number;
  /** Сколько проб подряд молчали — чтобы отсрочка не начиналась с нуля. */
  readonly silentProbes: number;
  /** Когда наблюдение сделано (Date.now()). */
  readonly at: number;
  /** Only row indexes, never screen text; keep known spinner rows quiet after reopen. */
  readonly volatileRows?: readonly number[];
}

export interface ScrollProbeVerdict {
  readonly page: StoredChannelEvidence | null;
  readonly wheel: StoredChannelEvidence | null;
}

/** Через сколько наблюдение не восстанавливается вовсе. */
export const SCROLL_PROBE_TTL_MS = 6 * 60 * 60 * 1000;
/** Сколько областей одного терминала помним одновременно. */
export const SCROLL_PROBE_SCOPES = 4;

// JSON ключа не даёт разделителям в идентификаторах склеить два терминала.
const keyV1 = (context: string, id: string) => `pty.scrollProbe.v1:${JSON.stringify([context, id])}`;
const keyV2 = (context: string, id: string) => `pty.scrollProbe.v2:${JSON.stringify([context, id])}`;

interface StoredEntry {
  scope: string;
  page: StoredChannelEvidence | null;
  wheel: StoredChannelEvidence | null;
}

interface StoredRecordV2 {
  v: 2;
  entries: StoredEntry[];
}

const isMask = (v: unknown): v is number => typeof v === "number" && Number.isInteger(v) && v >= 0 && v <= 3;
const isCount = (v: unknown): v is number => typeof v === "number" && Number.isInteger(v) && v >= 0 && v <= 1000;
const isTime = (v: unknown): v is number => typeof v === "number" && Number.isFinite(v) && v > 0;

function parseChannel(raw: unknown): StoredChannelEvidence | null | undefined {
  if (raw === null || raw === undefined) return null;
  if (typeof raw !== "object") return undefined;
  const r = raw as Record<string, unknown>;
  if (r.state !== "confirmed" && r.state !== "unconfirmed") return undefined;
  if (!isMask(r.dead) || !isCount(r.silentProbes) || !isTime(r.at)) return undefined;
  const indexes = r.volatileRows;
  if (indexes !== undefined && (!Array.isArray(indexes) || indexes.length > 1000
    || !indexes.every(v => typeof v === "number" && Number.isInteger(v) && v >= 0 && v < 1000))) return undefined;
  return { state: r.state, dead: r.state === "confirmed" ? 0 : r.dead, silentProbes: r.silentProbes, at: r.at,
    ...(r.state === "unconfirmed" && indexes?.length ? { volatileRows: [...new Set(indexes)] } : {}) };
}

function parseRecord(raw: string | null): StoredEntry[] | null {
  if (!raw) return null;
  let rec: Partial<StoredRecordV2> | null = null;
  try { rec = JSON.parse(raw) as Partial<StoredRecordV2>; } catch { return null; }
  if (!rec || typeof rec !== "object" || rec.v !== 2 || !Array.isArray(rec.entries)) return null;
  const out: StoredEntry[] = [];
  for (const e of rec.entries.slice(0, SCROLL_PROBE_SCOPES)) {
    if (!e || typeof e !== "object" || typeof e.scope !== "string" || !e.scope) return null;
    const page = parseChannel(e.page);
    const wheel = parseChannel(e.wheel);
    // Порченый канал портит всю запись: доверять половине чужого формата нельзя.
    if (page === undefined || wheel === undefined) return null;
    out.push({ scope: e.scope, page, wheel });
  }
  return out;
}

const fresh = (c: StoredChannelEvidence | null, now: number): StoredChannelEvidence | null =>
  c && now >= c.at && now - c.at <= SCROLL_PROBE_TTL_MS ? c : null;

export function readScrollProbeVerdict(
  context: string,
  id: string,
  scope: string,
  now: number,
  storage?: Pick<Storage, "getItem">,
  /**
   * Когда процесс последний раз печатал (не задано — не знаем). Неподтверждённое
   * наблюдение, после которого был вывод, — просроченное предположение: оно не
   * восстанавливается (у восстановленного нет якоря экрана, и отличить значимый
   * вывод от шума уже нечем). Подтверждённое — факт, выводом не портится.
   */
  lastOutputAt?: number,
): ScrollProbeVerdict | null {
  if (!context || !id || !scope) return null;
  let raw: string | null = null;
  try { raw = (storage ?? globalThis.localStorage).getItem(keyV2(context, id)); } catch { return null; }
  const entry = parseRecord(raw)?.find((e) => e.scope === scope);
  if (!entry) return null;
  const keep = (c: StoredChannelEvidence | null) => {
    const live = fresh(c, now);
    if (live?.state === "unconfirmed" && typeof lastOutputAt === "number" && lastOutputAt > live.at) return null;
    return live;
  };
  const result = { page: keep(entry.page), wheel: keep(entry.wheel) };
  if (!result.page && !result.wheel) return null;
  return result;
}

export function saveScrollProbeVerdict(
  context: string,
  id: string,
  scope: string,
  verdict: ScrollProbeVerdict,
  now: number,
  storage?: Pick<Storage, "getItem" | "setItem" | "removeItem">,
): void {
  if (!context || !id || !scope) return;
  try {
    const store = storage ?? globalThis.localStorage;
    store.removeItem(keyV1(context, id));
    // Чужие области остаются, пока свежи; порченая запись заменяется целиком.
    const others = (parseRecord(store.getItem(keyV2(context, id))) ?? [])
      .filter((e) => e.scope !== scope)
      .map((e) => ({ ...e, page: fresh(e.page, now), wheel: fresh(e.wheel, now) }))
      .filter((e) => e.page || e.wheel);
    const entries: StoredEntry[] = [];
    if (verdict.page || verdict.wheel) entries.push({ scope, page: verdict.page, wheel: verdict.wheel });
    entries.push(...others);
    if (entries.length === 0) {
      store.removeItem(keyV2(context, id));
      return;
    }
    const rec: StoredRecordV2 = { v: 2, entries: entries.slice(0, SCROLL_PROBE_SCOPES) };
    store.setItem(keyV2(context, id), JSON.stringify(rec));
  } catch { /* хранилище недоступно — наблюдение проживёт в памяти экрана (T-03) */ }
}

/** Явный выбор человека в меню режима: автоматика ошиблась, её память — долой. */
export function forgetScrollProbeVerdict(
  context: string,
  id: string,
  storage?: Pick<Storage, "removeItem">,
): void {
  if (!context || !id) return;
  try {
    const store = storage ?? globalThis.localStorage;
    store.removeItem(keyV1(context, id));
    store.removeItem(keyV2(context, id));
  } catch { /* ignore */ }
}

/** Живое наблюдение → запись хранилища. Слабое не переносится. */
export function storedEvidence(obs: Observation | null): StoredChannelEvidence | null {
  if (!obs || obs.state === "weak") return null;
  return obs.state === "confirmed"
    ? { state: "confirmed", dead: 0, silentProbes: 0, at: obs.at }
    : { state: "unconfirmed", dead: obs.deadDirections, silentProbes: obs.silentProbes, at: obs.at,
      ...(obs.volatileRows?.length ? { volatileRows: [...obs.volatileRows] } : {}) };
}

/** Restore metadata only; the first real navigation intent supplies the live anchor. */
export function restoredObservation(stored: StoredChannelEvidence): Observation {
  return {
    state: stored.state,
    deadDirections: stored.state === "confirmed" ? 0 : stored.dead,
    at: stored.at,
    silentProbes: stored.silentProbes,
    answers: stored.state === "confirmed" ? 1 : 0,
    ...(stored.state === "unconfirmed" && stored.volatileRows?.length ? { volatileRows: [...stored.volatileRows] } : {}),
  };
}

export const STORED_CHANNELS: readonly NavChannel[] = ["page", "wheel"];
