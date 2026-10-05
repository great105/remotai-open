/**
 * Временная шкала событий клиента терминала (ST-01).
 *
 * До неё диагностика была разовыми {t:"diag"} в тот же сокет: при закрытом WS
 * событие терялось, и собрать, что происходило ПЕРЕД обрывом, было нечем
 * (карта ST-01, gap high). Здесь — ограниченное кольцо событий в памяти
 * страницы с монотонным временем, сквозным seq и снимком состояния конвейера
 * (поколение, эпоха, offsets, очереди, ревизия геометрии) на момент решения.
 *
 * Правила, которые держит сам модуль, а не дисциплина мест вызова:
 * - только метаданные (I-15): sanitizeFields пропускает конечные числа,
 *   boolean и короткие ASCII-строки, ключи содержимого отвергаются;
 * - кольцо ограничено, вытесненное считается в dropped;
 * - у потоковых событий (rx, parse-done) СВОЙ бюджет кольца: поток агента
 *   вытесняет только поток, а жест, случившийся за минуты до «Зафиксировать
 *   проблему», остаётся (волна 4, находка ресурсов: раньше кольцо 4096 уходило
 *   на rx/parse-done за 3,5 мин при 10 кадрах в секунду);
 * - подряд идущие rx и их разбор сливаются в пару записей (rx + parse-done),
 *   пока между ними ничего не случилось: раньше parse-done каждого спуска
 *   вставал между кадрами и слияние rx не срабатывало вовсе;
 * - нажатия клавиш сливаются в серию: одно событие на серию, длина корзиной,
 *   время начала грубо (волна 4, приватность: по отдельным событиям с временем
 *   0,01 мс восстанавливались длина пароля и ритм набора);
 * - вывод, пришедший ВО ВРЕМЯ набора, огрублён так же (волна 6, приватность:
 *   эхо «*» от sudo с pwfeedback сливалось в rx с точными bytes/count, и длина
 *   пароля читалась мимо корзины ввода). От нажатия человека до первого кадра
 *   после паузы INPUT_SERIES_GAP_MS и спуска, который его разобрал, rx и
 *   parse-done — записи reason "during-input": объём корзиной, время с шагом
 *   1 с, без числа кадров и позиций потока; у всех событий этого отрезка нет
 *   позиций и объёмов потока ни в ctx, ни в полях. А чтобы длину набранного
 *   нельзя было вычесть из точных позиций ДО и ПОСЛЕ отрезка (при повторе
 *   неверного пароля ответ программы «Sorry, try again.» известен побайтно —
 *   скептик волны 6), позиции потока ПОСЛЕ отрезка перебазируются на его конец:
 *   абсолютные offsets через отрезок не проходят, объём, съеденный набором, в
 *   файл не попадает (spansRebased, отметка в notes; toEvent вычитает posBase);
 * - база перебазирования — своя у каждой эпохи потока (волна 8, скептик волны 7:
 *   база не знала эпохи, и первая позиция новой эпохи отдавала длину пароля).
 *   Маркер reset/resumed называет эпоху (noteSync). Новая эпоха (перезапуск
 *   агента или PTY, offsets заново) пишется сырыми позициями: с отрезком прежней
 *   она не связана. Досылка той же эпохи с места раньше конца отрезка
 *   (переподключение без resume) несёт эхо набранного — она огрублена, как сам
 *   отрезок. Позиция ниже базы не прижимается к 0 и не пишется отрицательной —
 *   её нет в файле (иначе по известной сырой позиции база вычислялась бы).
 *   Предел: если новая эпоха заново проигрывает с 0 то же кольцо (старый хост
 *   2.57.20 и раньше при обновлении агента), её сырые позиции совпадают с
 *   позициями прежней, и конец отрезка вычислим; клиент такую переигровку от
 *   свежего потока не отличит — это сказано в pty.traceMetaOnly;
 * - накопленные поля diag flow (максимумы спусков и очередей, сумма выброшенного)
 *   после отрезка набора пишутся, только если не выросли с последней точной
 *   записи до него (волна 8: вставленный секрет с эхом одним кадром мог стать
 *   самым большим спуском, и maxBatch отдал бы его длину);
 * - замер потока приложения в diag alt-scroll* (счётчики с маркера reset), пока
 *   окно замера может содержать отрезок набора, пишется ОГРУБЛЁННО: объём —
 *   корзиной (bytesRange) и округлённым «строк на КиБ» (lpk), которым решение о
 *   владельце истории и принимается, с признаком typed; точного bytes нет
 *   (волна 8, скептик: bytes − ctx.app + offset маркера отдавали конец отрезка).
 *   Счётчики СТРОК (lines, own) точные: длины набранного в строках нет, а own
 *   несёт ровно то же, что lines (волна 9, скептик: опущенный замер лишал файл
 *   тех самых чисел, по которым разбирают «не листается»);
 * - identical:false, если из файла что-то убрано или пересчитано: огрублённые
 *   записи, снятые позиции и объёмы, опущенные накопленные поля, перебазирование.
 *   Каждое такое преобразование названо в notes; вытеснение кольцом — не правка;
 * - вытеснение каждого кольца видно отдельно: streamDropped/streamFirstSeq и
 *   mainFirstSeq в снимке и файле, в файле ещё и notes словами (волна 6: по
 *   общим dropped и firstSeq файл без rx в начале читался как «агент ничего
 *   не присылал»);
 * - горячий путь rx не аллоцирует: слоты кольца переиспользуются, строк не
 *   строится.
 * Модуль чистый (без React и DOM): часы инжектируются, проверяется в node.
 * Трасса живёт в ref, НЕ в React state — самоперерисовки уже роняли
 * qa:terminal (памятка terminal-selfrender).
 */
import { encodeRecording, type EncodedRecording, type RecordingSnapshot } from "./traceRecording";

/** Закрытое перечисление событий: новое событие — новая строка здесь. */
export const TRACE_KINDS = [
  "rx", "sync", "enqueue-drop", "parse", "parse-done", "erase", "gap", "writer",
  "screen-req", "screen-rx", "snap-reject", "snap-apply", "snap-watchdog", "barrier-release",
  "geom", "resize-send", "kbd", "conn", "overlay", "gesture", "route", "probe", "mode",
  "renderer", "flow", "vis", "input", "read", "diag", "mark", "presented", "recovery",
] as const;
export type TraceKind = (typeof TRACE_KINDS)[number];
const KINDS: ReadonlySet<string> = new Set<string>(TRACE_KINDS);

/**
 * Состояние конвейера на момент события. sid — индекс сессии в таблице
 * sessions (session()), чтобы переиспользованный экран (useLayoutEffect по id)
 * не смешал сессии в одной трассе. gen — поколение соединения, wep — эпоха
 * писателя, acc/app — принятый и показанный offset, qEnd/qBytes/qLen — очередь
 * склейки, wPending — записи в writer, unacked — неразобранное xterm, geomRev —
 * ревизия геометрии, snapTok — токен барьера кадра, paused — flow-пауза.
 */
export const TRACE_CONTEXT_KEYS = [
  "sid", "gen", "wep", "acc", "app", "qEnd", "qBytes", "qLen", "wPending", "unacked", "geomRev", "snapTok", "paused",
] as const;
export type TraceContextKey = (typeof TRACE_CONTEXT_KEYS)[number];
export type TraceScalar = number | string | boolean;
export type TraceContext = Partial<Record<TraceContextKey, TraceScalar | null | undefined>>;
export type TraceFields = Record<string, TraceScalar>;

export interface TraceEvent {
  seq: number;
  /** Монотонное время, мс от начала трассы (startedAtMs). */
  t: number;
  kind: TraceKind;
  ctx?: Partial<Record<TraceContextKey, TraceScalar>>;
  reason?: string;
  f?: TraceFields;
  /** У rx: сколько кадров слито, их байты и границы принятого потока. У
   * parse-done: сколько спусков слито (есть, только если больше одного).
   * У записей отрезка набора (reason "during-input") этих полей нет вовсе:
   * объём — корзиной в f.bytes. */
  count?: number;
  bytes?: number;
  firstEnd?: number;
  lastEnd?: number;
  /** Только у rx: время последнего слитого кадра. */
  tEnd?: number;
}

export const TRACE_DEFAULT_CAPACITY = 4096;
/** Доля кольца под поток по умолчанию: 1024 из 4096. */
export const TRACE_STREAM_SHARE = 4;
const STREAM_KINDS: ReadonlySet<TraceKind> = new Set<TraceKind>(["rx", "parse-done"]);
/** parse-done этих спусков сливаются (noteApplied в PtyTermView). */
const MERGEABLE_PARSE: ReadonlySet<string> = new Set(["write", "chain", "empty"]);
/** Пауза, после которой нажатия начинают новую серию. */
export const INPUT_SERIES_GAP_MS = 1500;
/** Шаг времени начала серии ввода: интервалы внутри серии не восстановить. */
export const INPUT_TIME_GRAIN_MS = 1000;
/** Корзина длины ввода: по ней не восстановить число нажатий. */
export function inputSizeBucket(bytes: number): string {
  if (!(bytes > 0)) return "0";
  if (bytes < 16) return "1-15";
  if (bytes < 64) return "16-63";
  if (bytes < 256) return "64-255";
  if (bytes < 1024) return "256-1023";
  return "1024+";
}
/** reason огрублённых rx/parse-done отрезка набора (см. noteInput). */
export const TRACE_DURING_INPUT = "during-input";
/** reason огрублённых rx/parse-done досылки той же эпохи с места раньше конца
 * отрезка набора (см. noteSync): в ней снова идёт эхо набранного. */
export const TRACE_RESENT_SPAN = "resent-span";
/** Сколько эпох помнит трасса: их меняет только перезапуск агента или PTY. */
const EPOCH_MEMORY = 64;
/**
 * Поля diag flow, накопленные с открытия экрана (FlowController.flowDiagFields):
 * максимумы и сумма выброшенного. Запись ПОСЛЕ отрезка набора несла бы и то, что
 * было во время него, поэтому их рост через отрезок не пишется (flowFields).
 */
const FLOW_MAX_FIELDS: readonly string[] = ["maxQueued", "maxUnacked", "maxBatch"];
const FLOW_SUM_FIELDS: readonly string[] = ["dropBytes"];
/**
 * diag с замером потока приложения (PtyTermView: streamBytesRef, streamLinesRef).
 * Их bytes и lines копятся с маркера reset, смены эпохи или переднего процесса и
 * до 16 КиБ не затухают, то есть bytes — это позиция под видом объёма: после
 * отрезка набора bytes − ctx.app (перебазирован) + offset маркера отдавали конец
 * отрезка и длину пароля (скептик волны 8).
 *
 * ⚠ Волна 9 (скептик): опускать замер целиком НЕЛЬЗЯ. Это те самые числа,
 * которыми принимается решение «своя история или приложение»
 * (altScroll.historyOwnerFromStream — отношение строк к объёму против порога), и
 * без них жалобу «не листается» по файлу разобрать нечем; а окно замера
 * открывалось ЛЮБЫМ нажатием и не закрывалось до маркера. Поэтому, пока окно
 * может содержать отрезок набора (windowSpan), объём ОГРУБЛЯЕТСЯ: вместо
 * точного bytes — корзина (bytesRange), отношение с грубым квантом (lpk) и
 * признак typed. Счётчики СТРОК (lines) и глубина своей истории (own) остаются
 * точными: own несёт ровно то же, что lines (streamLinesRef растёт ровно на
 * прирост normalBaseY, PtyTermView), а длины НАБРАННОГО в строках нет — эхо
 * пароля строк не добавляет. В журнал агента замер уходит как есть (I-14).
 */
export const TRACE_WINDOW_DIAG = ["alt-scroll", "alt-scroll-page-dead"] as const;
const WINDOW_DIAG: ReadonlySet<string> = new Set<string>(TRACE_WINDOW_DIAG);
/** Поле замера, которое огрубляется, и поля, которыми оно заменяется. */
export const TRACE_WINDOW_FIELD = "bytes";
export const TRACE_WINDOW_COARSE_FIELDS = ["bytesRange", "lpk", "typed"] as const;
/**
 * Ниже этого объёма вердикт по замеру не выносится вовсе
 * (altScroll.STREAM_SAMPLE_MIN_BYTES; равенство держит юнит). Ниже него lpk не
 * пишется: обратный счёт по ТОЧНЫМ строкам дал бы объём точнее корзины, а
 * решение всё равно «unknown» — прятать дешевле, чем отдавать.
 */
export const TRACE_SAMPLE_MIN_BYTES = 4096;
/**
 * Корзина объёма замера: шаг вчетверо, границы — вокруг порога вердикта. По ней
 * разность двух замеров через отрезок набора не считается.
 */
export function streamSampleBucket(bytes: number): string {
  if (!(bytes > 0)) return "0";
  if (bytes < 1024) return "1-1023";
  if (bytes < TRACE_SAMPLE_MIN_BYTES) return "1024-4095";
  if (bytes < 16384) return "4096-16383";
  if (bytes < 65536) return "16384-65535";
  return "65536+";
}
/**
 * Строк scrollback на КиБ вывода — число, которым принимается решение
 * (historyOwnerFromStream: порог 2 строки/КиБ, полоса 0,5). Квант — четверть
 * двоичного порядка величины: обратный счёт по точным строкам даёт объём не
 * точнее ±12 %, то есть от порога 4096 Б — не точнее ±256 Б. Это заведомо
 * грубее корзины самого ввода (inputSizeBucket), так что длины набранного из
 * отношения не достать.
 */
export function coarseLinesPerKib(lines: number, bytes: number): number {
  if (!(bytes > 0)) return 0;
  const rate = (lines * 1024) / bytes;
  if (!(rate > 0)) return 0;
  const step = Math.pow(2, Math.floor(Math.log2(rate)) - 2);
  return Math.round((Math.round(rate / step) * step) * 1000) / 1000;
}
/** Что убрано из записи (Slot.cut). Любой бит — файл не identical, notes называет. */
const CUT_STREAM = 1; // позиции и объёмы потока сняты с ctx или полей (отрезок набора, досылка)
const CUT_FLOW = 2; // накопленные поля diag flow
const CUT_WINDOW = 4; // объём замера потока приложения в diag alt-scroll* огрублён
const CUT_SEAM = 8; // end спуска на шве маркера
/**
 * Поля с позицией или объёмом потока (parse-done, sync, writer, enqueue-drop,
 * screen-rx, snap-reject, route, diag flow/snapshot-stale): на отрезке набора
 * они не пишутся — иначе соседнее событие отдало бы точную позицию посреди
 * эха. Новое поле такого смысла — сюда же.
 */
export const TRACE_STREAM_FIELDS = ["bytes", "items", "end", "offset", "base", "accepted", "applied", "buffered",
  "maxQueued", "maxUnacked", "maxBatch", "dropBytes"] as const;
const STREAM_FIELDS: ReadonlySet<string> = new Set<string>(TRACE_STREAM_FIELDS);
/** Геометрия, а не поток: base у kbd — высота окна, её не трогаем. */
const GEOMETRY_KINDS: ReadonlySet<string> = new Set<string>(["kbd"]);
/** Ключи ctx с позицией или объёмом потока: на отрезке набора не пишутся. */
export const TRACE_STREAM_CONTEXT = ["acc", "app", "qEnd", "qBytes", "qLen", "wPending", "unacked"] as const;
const STREAM_CTX: readonly number[] = TRACE_STREAM_CONTEXT.map((key) => TRACE_CONTEXT_KEYS.indexOf(key));
/**
 * АБСОЛЮТНЫЕ позиции потока (в отличие от объёмов bytes/items/qBytes/unacked):
 * после отрезка набора из них вычитается posBase — конец отрезка (toEvent),
 * иначе разность позиций ДО и ПОСЛЕ отрезка отдала бы его объём (длину пароля
 * при известном ответе программы). base у kbd — высота окна, не позиция: kbd
 * из перебазирования исключён (по kind).
 */
export const TRACE_POSITION_FIELDS = ["end", "offset", "base", "accepted", "applied"] as const;
const POSITION_FIELDS: readonly string[] = TRACE_POSITION_FIELDS;
export const TRACE_POSITION_CONTEXT = ["acc", "app", "qEnd"] as const;
const POSITION_CTX: ReadonlySet<number> = new Set(TRACE_POSITION_CONTEXT.map((key) => TRACE_CONTEXT_KEYS.indexOf(key)));
const grain = (t: number) => Math.floor(t / INPUT_TIME_GRAIN_MS) * INPUT_TIME_GRAIN_MS;

function withoutStream(fields: TraceFields): TraceFields | undefined {
  let out: TraceFields | undefined;
  for (const key of Object.keys(fields)) if (!STREAM_FIELDS.has(key)) (out ??= {})[key] = fields[key];
  return out;
}

/** Копия полей без одного ключа (поля места вызова не меняем). */
function withoutKey(fields: Record<string, unknown>, drop: string): Record<string, unknown> {
  const out: Record<string, unknown> = {};
  for (const key of Object.keys(fields)) if (key !== drop) out[key] = fields[key];
  return out;
}
export const TRACE_STRING_MAX = 32;
const MAX_FIELDS = 24;
const KEY_RE = /^[A-Za-z][A-Za-z0-9_]{0,31}$/;
/** Печатный ASCII до 32 символов: имена режимов, причин, версии. Кириллица,
 * управляющие символы и длинные строки — почти наверняка содержимое. */
const SHORT_RE = /^[\x20-\x7e]{0,32}$/;

/** Ключи содержимого: отвергаются при любом типе значения (I-15, T-37). */
export const TRACE_FORBIDDEN_KEYS = ["data", "text", "screen", "history", "name", "clipboard", "path", "file"] as const;
const FORBIDDEN: ReadonlySet<string> = new Set<string>(TRACE_FORBIDDEN_KEYS);
/** Строковое значение под ключом с таким словом тоже отвергается: fileName,
 * screenText, inputValue. Числа под ними (fileBytes, pasteLen) — метаданные. */
const CONTENT_WORDS: readonly string[] = [...TRACE_FORBIDDEN_KEYS, "input", "content", "value", "paste",
  "title", "url", "prompt", "cmd"];

function cleanScalar(v: unknown): TraceScalar | undefined {
  if (typeof v === "number") return Number.isFinite(v) ? v : undefined;
  if (typeof v === "boolean") return v;
  if (typeof v === "string") return SHORT_RE.test(v) ? v : undefined;
  return undefined;
}

/**
 * Allowlist полей события (I-15). Возвращает новый объект только с допустимым
 * или undefined, если не осталось ничего. report.rejected считает отброшенное,
 * чтобы ошибка места вызова была видна в трассе, а не молча терялась.
 */
export function sanitizeFields(fields: unknown, report?: { rejected: number }): TraceFields | undefined {
  if (!fields || typeof fields !== "object" || Array.isArray(fields)) return undefined;
  let out: TraceFields | undefined;
  let n = 0;
  for (const key of Object.keys(fields)) {
    const v = (fields as Record<string, unknown>)[key];
    const lower = key.toLowerCase();
    let ok = n < MAX_FIELDS && KEY_RE.test(key) && !FORBIDDEN.has(lower);
    if (ok) {
      if (typeof v === "number") ok = Number.isFinite(v);
      else if (typeof v === "boolean") ok = true;
      else if (typeof v === "string") ok = SHORT_RE.test(v) && !CONTENT_WORDS.some(w => lower.includes(w));
      else ok = false;
    }
    if (!ok) { if (report) report.rejected++; continue; }
    (out ??= {})[key] = v as TraceScalar;
    n++;
  }
  return out;
}

interface Slot {
  seq: number;
  t: number;
  kind: TraceKind;
  ctx: (TraceScalar | undefined)[];
  reason: string | undefined;
  fields: TraceFields | undefined;
  count: number;
  bytes: number;
  firstEnd: number;
  lastEnd: number;
  tEnd: number;
  /** Запись отрезка набора: наружу — корзина объёма, без позиций (noteInput). */
  coarse: boolean;
  /** База перебазирования полей: её вычитают из абсолютных позиций в toEvent.
   * После закрытия отрезка набора равна его концу — позиции региона считаются
   * от него; у каждой эпохи потока база своя. */
  posBase: number;
  /** База ctx. Та же, что posBase, кроме события sync: его ctx — состояние ДО
   * маркера (прежняя эпоха), поля — offset маркера (новая). */
  ctxBase: number;
  /** Что из записи убрано (биты CUT_*): файл с такой записью не identical. */
  cut: number;
}

const SID = 0;
const GEN = 1;

export interface TraceOptions {
  /** Всё кольцо (основное + потоковое), по умолчанию 4096. */
  capacity?: number;
  /** Сколько из него — под поток (rx, parse-done). По умолчанию четверть;
   * 0 — общее кольцо без отдельного бюджета (прежнее поведение). */
  streamCapacity?: number;
  /** Монотонные часы, мс (performance.now). В тестах — инжект. */
  now?: () => number;
  /** Настенные часы для startedAtMs/exportedAtMs (Date.now). */
  wallNow?: () => number;
}

export interface TraceSnapshot {
  events: TraceEvent[];
  /** Вытеснено всего (оба кольца). */
  dropped: number;
  /** Вытеснено из отдельного бюджета потока (rx, parse-done); 0 — бюджета нет,
   * и поток вытесняется вместе со всем (он в dropped). */
  streamDropped: number;
  /** seq самой старой сохранённой записи потока (или nextSeq, если их нет): при
   * streamDropped > 0 rx и parse-done раньше него вытеснены. Без своего
   * бюджета — то же, что mainFirstSeq. */
  streamFirstSeq: number;
  /** seq самого старого сохранённого события основного кольца (решения). */
  mainFirstSeq: number;
  /** seq самого старого сохранённого события (или nextSeq, если пусто). */
  firstSeq: number;
  nextSeq: number;
  capacity: number;
  /** Ёмкость бюджета потока; 0 — общее кольцо. */
  streamCapacity: number;
  sessions: string[];
  rejectedFields: number;
  /** Сколько отрезков набора закрыто с перебазированием позиций (для notes). */
  spansRebased: number;
  /** Хоть одна позиция в events перебазирована (или опущена как устаревшая,
   * ниже базы): файл не identical. */
  rebased: boolean;
  /** Хоть одна запись в events огрублена или без части полей (снятые позиции и
   * объёмы потока, накопленные поля, end на шве маркера): файл не identical. */
  omitted: boolean;
  /** Сколько записей diag flow в events без накопленных полей, выросших через отрезок набора. */
  flowWithheld: number;
  /** Сколько diag alt-scroll* в events с ОГРУБЛЁННЫМ объёмом замера потока приложения. */
  windowCoarsened: number;
  /** Сколько спусков в events без end: дописаны на шве маркера в прежних координатах. */
  seamCut: number;
  /** Сколько событий в events без позиций и объёмов потока, снятых на отрезке
   * набора или досылке (сами огрублённые rx и parse-done не в счёт). */
  streamCut: number;
}

const defaultNow = () => (typeof performance !== "undefined" ? performance.now() : Date.now());

/** Кольцо слотов фиксированной ёмкости: вытесняется самый старый. */
class Ring {
  readonly slots: Slot[] = [];
  head = 0;
  size = 0;
  dropped = 0;
  constructor(readonly capacity: number) {}

  /** Слот под новое событие (переиспользуется вытесненный). */
  take(kind: TraceKind): Slot {
    let index: number;
    if (this.size < this.capacity) {
      index = (this.head + this.size) % this.capacity;
      this.size++;
    } else {
      index = this.head;
      this.head = (this.head + 1) % this.capacity;
      this.dropped++;
    }
    let slot = this.slots[index];
    if (!slot) {
      slot = { seq: 0, t: 0, kind, ctx: new Array(TRACE_CONTEXT_KEYS.length), reason: undefined, fields: undefined,
        count: 0, bytes: 0, firstEnd: 0, lastEnd: 0, tEnd: 0, coarse: false, posBase: 0, ctxBase: 0, cut: 0 };
      this.slots[index] = slot;
    }
    return slot;
  }

  at(i: number): Slot { return this.slots[(this.head + i) % this.capacity]; }
  clear(): void { this.head = 0; this.size = 0; this.dropped = 0; }
}

/** Открытая запись, в которую ещё можно слить: сам слот и его seq (слот
 * переиспользуется кольцом — после вытеснения seq уже чужой). */
interface Open { slot: Slot; seq: number }

export class TerminalTrace {
  readonly capacity: number;
  readonly streamCapacity: number;
  readonly startedAtMs: number;
  readonly wallNow: () => number;
  private readonly clock: () => number;
  private readonly origin: number;
  private readonly main: Ring;
  /** Отдельный бюджет потока; null — поток в общем кольце. */
  private readonly stream: Ring | null;
  private seqNext = 1;
  private lastT = 0;
  private readonly report = { rejected: 0 };
  private readonly sessionIds: string[] = [];
  private readonly sessionIndex = new Map<string, number>();
  /** Пара потока, в которую сливаются следующие кадр и спуск. */
  private openRx: Open | null = null;
  private openParse: Open | null = null;
  /** Открытая серия нажатий и время последнего нажатия в ней. */
  private openInput: Open | null = null;
  private inputLastT = 0;
  private inputBytes = 0;
  /**
   * Отрезок набора человека (I-15, волна 6): 0 — нет; 1 — нажатие было, а
   * кадра после паузы INPUT_SERIES_GAP_MS ещё не пришло; 2 — первый кадр после
   * паузы принят, ждём спуск, который его разберёт (end ≥ typingExitEnd того
   * же поколения). Всё это время поток пишется огрублённо.
   */
  private typing: 0 | 1 | 2 = 0;
  private humanT = 0;
  private typingExitEnd = 0;
  private typingExitGen: TraceScalar | undefined = undefined;
  /** База перебазирования позиций ТЕКУЩЕЙ эпохи: 0 — абсолютные; после
   * закрытия отрезка набора равна его концу, и позиции следующего региона
   * считаются от неё. */
  private posBase = 0;
  /** Сколько отрезков набора закрыто с перебазированием (для notes). */
  private spansRebased = 0;
  /** Эпоха потока из маркера reset/resumed — система координат offsets; null —
   * маркера ещё не было (первая эпоха наследует текущую базу). */
  private epoch: string | null = null;
  /** База каждой эпохи, где поток уже был (posBase — база текущей). */
  private readonly epochBases = new Map<string, number>();
  /** Досылка той же эпохи с места раньше её базы: огрублена, пока спуск того
   * же соединения не разберёт поток до базы (noteSync). */
  private cover: { end: number; gen: TraceScalar | undefined } | null = null;
  /** Шов маркера при неразобранных записях писателя: они дописываются в
   * прежних координатах. Пока он открыт, спуск не закрывает ни отрезок, ни
   * досылку, а при другой базе end спуска не пишется. token — seq события sync. */
  private seam: { token: number; dropEnd: boolean } | null = null;
  /** Конец принятого потока (последний noteRx). */
  private acceptedEnd = 0;
  /** diag flow: последнее записанное значение накопленного поля; значение, в
   * которое мог войти отрезок набора (не записано); был ли отрезок с прошлой
   * точной записи flow. */
  private readonly flowKnown: Record<string, number> = {};
  private readonly flowAmbiguous: Record<string, number> = {};
  private flowSpan = false;
  /** Окно замера потока приложения (TRACE_WINDOW_DIAG) может содержать отрезок
   * набора или досылку: с их начала и до того, как PtyTermView обнулит замер —
   * на маркере (reset, смена эпохи) или на смене переднего процесса, о которой
   * он говорит сам (noteStreamSampleReset, волна 9: раньше трасса её не видела,
   * и объём замера оставался огрублённым до маркера). windowToken — seq маркера
   * reset: замер обнуляется ещё раз после разбора прежних записей
   * (noteSyncDrained). */
  private windowSpan = false;
  private windowToken = -1;

  constructor(options: TraceOptions = {}) {
    const cap = options.capacity ?? TRACE_DEFAULT_CAPACITY;
    this.capacity = Number.isSafeInteger(cap) && cap > 0 ? cap : TRACE_DEFAULT_CAPACITY;
    const wanted = options.streamCapacity ?? Math.floor(this.capacity / TRACE_STREAM_SHARE);
    this.streamCapacity = Number.isSafeInteger(wanted) && wanted > 0 && wanted < this.capacity ? wanted : 0;
    this.main = new Ring(this.capacity - this.streamCapacity);
    this.stream = this.streamCapacity > 0 ? new Ring(this.streamCapacity) : null;
    this.clock = options.now ?? defaultNow;
    this.wallNow = options.wallNow ?? Date.now;
    this.origin = this.clock();
    this.startedAtMs = this.wallNow();
  }

  /** Запись ещё в кольце и не отдана под другое событие. */
  private live(open: Open | null): Slot | null {
    return open && open.slot.seq === open.seq ? open.slot : null;
  }

  /** После пары потока ничего не случилось: последнее событие — её rx или спуск. */
  private pairIsLast(): boolean {
    const last = this.seqNext - 1;
    return (!!this.live(this.openRx) && this.openRx!.seq === last) || (!!this.live(this.openParse) && this.openParse!.seq === last);
  }

  /** Монотонное время трассы: часы, пошедшие назад (или NaN), не ломают порядок. */
  time(): number {
    const raw = this.clock() - this.origin;
    if (raw > this.lastT) this.lastT = raw;
    return this.lastT;
  }

  /** Индекс сессии для ctx.sid; сам id хранится один раз в таблице sessions. */
  session(id: string): number {
    const key = String(id).slice(0, 64);
    let index = this.sessionIndex.get(key);
    if (index === undefined) {
      index = this.sessionIds.length;
      this.sessionIds.push(key);
      this.sessionIndex.set(key, index);
    }
    return index;
  }

  /** Событие решения. Возвращает seq или -1, если kind не из перечисления. */
  note(kind: TraceKind, ctx?: TraceContext | null, fields?: Record<string, unknown> | null, reason?: string): number {
    if (!KINDS.has(kind)) { this.report.rejected++; return -1; }
    let cut = 0;
    if (kind === "parse-done") {
      // Шов маркера при другой базе: спуск может разобрать запись прежних
      // координат — его end не пишется (с базой новой эпохи он отдал бы прежнюю
      // абсолютную позицию).
      if (this.seam?.dropEnd && fields && "end" in fields) { fields = withoutKey(fields, "end"); cut = CUT_SEAM; }
      if (this.isCoarse()) return this.noteTypingParse(ctx, fields);
    }
    if (kind === "parse-done" && reason !== undefined && MERGEABLE_PARSE.has(reason)) {
      const merged = this.mergeParse(ctx, fields, reason, cut);
      if (merged >= 0) return merged;
    }
    // Пара потока — только rx и СРАЗУ за ним его спуск: событие между ними
    // (жест, связь) закрывает пару, и следующий кадр начнёт новую запись.
    const pairs = kind === "parse-done" && reason !== undefined && MERGEABLE_PARSE.has(reason)
      && !!this.live(this.openRx) && this.openRx!.seq === this.seqNext - 1;
    const slot = this.push(kind, ctx);
    slot.cut |= cut;
    if (fields) slot.fields = sanitizeFields(fields, this.report);
    // Замер потока приложения огрубляется ДО общего снятия полей потока: иначе
    // cutStream унёс бы bytes целиком, и огрублять было бы нечего.
    if (slot.fields && kind === "diag" && this.windowSpan && reason !== undefined && WINDOW_DIAG.has(reason)) {
      slot.fields = coarseSample(slot, slot.fields);
    }
    if (slot.fields && this.isCoarse() && !GEOMETRY_KINDS.has(kind)) slot.fields = cutStream(slot, slot.fields);
    else if (slot.fields && kind === "diag" && reason === "flow") slot.fields = this.flowFields(slot, slot.fields);
    if (reason !== undefined) {
      const clean = cleanScalar(reason);
      if (typeof clean === "string") slot.reason = clean;
      else this.report.rejected++;
    }
    if (kind === "parse-done") {
      slot.count = 1;
      slot.tEnd = slot.t;
      if (pairs) this.openParse = { slot, seq: slot.seq };
      else { this.openRx = null; this.openParse = null; }
    }
    return slot.seq;
  }

  /**
   * Спуск кадров пары потока: сливается в открытый parse-done, если после пары
   * ничего не случилось и сессия с поколением те же. Байты и пункты
   * суммируются, конец — последнего спуска, время разбора — наибольшее.
   */
  private mergeParse(ctx: TraceContext | null | undefined, fields: Record<string, unknown> | null | undefined, reason: string,
    cut: number): number {
    const rx = this.live(this.openRx);
    const parse = this.live(this.openParse);
    if (!rx || !parse || rx.coarse || parse.coarse || !this.pairIsLast() || parse.reason !== reason
      || parse.ctx[SID] !== cleanScalar(ctx?.sid) || parse.ctx[GEN] !== cleanScalar(ctx?.gen)) return -1;
    parse.cut |= cut;
    const next = fields ? sanitizeFields(fields, this.report) : undefined;
    const prev = parse.fields ?? {};
    const num = (v: unknown) => (typeof v === "number" ? v : 0);
    const merged: TraceFields = { ...prev };
    if (next) {
      for (const key of Object.keys(next)) {
        const v = next[key];
        if (key === "bytes" || key === "items") merged[key] = num(prev[key]) + num(v);
        else if (key === "ms") merged[key] = Math.max(num(prev[key]), num(v));
        else merged[key] = v;
      }
    }
    parse.fields = merged;
    parse.count++;
    parse.tEnd = this.time();
    copyContext(parse, ctx);
    return parse.seq;
  }

  /**
   * Спуск на отрезке набора: одна огрублённая запись на пару (корзина объёма,
   * время с шагом 1 с, без числа спусков, конца и позиций); наибольшее ms
   * разбора остаётся. Спуск, разобравший первый кадр после паузы (end не
   * меньше его границы, то же поколение), закрывает отрезок.
   */
  private noteTypingParse(ctx: TraceContext | null | undefined, fields: Record<string, unknown> | null | undefined): number {
    const clean = fields ? sanitizeFields(fields, this.report) : undefined;
    const bytes = typeof clean?.bytes === "number" && clean.bytes > 0 ? clean.bytes : 0;
    const ms = typeof clean?.ms === "number" ? clean.ms : undefined;
    const rx = this.live(this.openRx);
    const parse = this.live(this.openParse);
    const reason = this.coarseReason();
    let seq: number;
    if (rx?.coarse && parse?.coarse && parse.reason === reason && this.pairIsLast()
      && parse.ctx[SID] === cleanScalar(ctx?.sid) && parse.ctx[GEN] === cleanScalar(ctx?.gen)) {
      parse.bytes += bytes;
      if (ms !== undefined) parse.fields = { ms: Math.max(ms, typeof parse.fields?.ms === "number" ? parse.fields.ms : 0) };
      this.fill(parse, ctx);
      seq = parse.seq;
    } else {
      const pairs = !!rx?.coarse && rx.reason === reason && this.openRx!.seq === this.seqNext - 1;
      const slot = this.push("parse-done", ctx);
      slot.coarse = true;
      slot.t = grain(slot.t);
      slot.reason = reason;
      slot.bytes = bytes;
      slot.fields = ms !== undefined ? { ms } : undefined;
      if (pairs) this.openParse = { slot, seq: slot.seq };
      else { this.openRx = null; this.openParse = null; }
      seq = slot.seq;
    }
    const end = typeof clean?.end === "number" ? clean.end : undefined;
    const gen = cleanScalar(ctx?.gen);
    // Пока шов маркера открыт, спуск может разбирать запись прежних координат:
    // ни отрезок, ни досылку он не закрывает.
    if (end !== undefined && this.seam === null) {
      if (this.typing === 2 && end >= this.typingExitEnd && gen === this.typingExitGen) {
        // Отрезок кончился: следующий кадр начнёт точную запись — но с НОВОЙ
        // базы. Позиции региона после отрезка считаются от его конца (posBase
        // этой эпохи), поэтому вычесть позицию ДО отрезка из позиции ПОСЛЕ
        // нельзя, и объём набора (длина пароля при известном ответе программы)
        // из файла не восстановим.
        this.typing = 0;
        this.openRx = null;
        this.openParse = null;
        if (end > this.posBase) this.setBase(end);
        this.spansRebased++;
      }
      if (this.cover && end >= this.cover.end && gen === this.cover.gen) {
        // Досылка прошла конец отрезка: дальше — точные записи от той же базы.
        this.cover = null;
        this.openRx = null;
        this.openParse = null;
      }
    }
    return seq;
  }

  /** Кадр на отрезке набора (см. noteInput): сливается в огрублённую запись. */
  private noteTypingRx(bytes: number, acceptedEnd: number, ctx: TraceContext | null | undefined): number {
    const gen = cleanScalar(ctx?.gen);
    // Граница выхода — НЕ НИЖЕ базы: досылка той же эпохи (cover) начинается
    // раньше конца отрезка, и её первый кусок разбирается задолго до него.
    // Иначе (волна 9, скептик) он закрывал отрезок как «кадр после паузы», и
    // дальше куски досылки писались точно — с эхом набранного.
    const exitEnd = Math.max(acceptedEnd, this.posBase);
    if (this.typing === 1 && this.time() - this.humanT >= INPUT_SERIES_GAP_MS) {
      // Первый кадр после паузы — обычно ответ программы: он входит в отрезок,
      // чтобы точная позиция после набора не отдала объём одного эха.
      this.typing = 2;
      this.typingExitEnd = exitEnd;
      this.typingExitGen = gen;
    } else if (this.typing === 2 && gen !== this.typingExitGen) {
      // Новое соединение: прежний кадр уже не разберут — ждём разбор этого.
      this.typingExitEnd = exitEnd;
      this.typingExitGen = gen;
    }
    const last = this.live(this.openRx);
    const reason = this.coarseReason();
    if (last?.coarse && last.reason === reason && this.pairIsLast() && last.ctx[SID] === cleanScalar(ctx?.sid)
      && last.ctx[GEN] === gen && acceptedEnd - bytes === last.lastEnd) {
      last.bytes += bytes;
      last.lastEnd = acceptedEnd;
      this.fill(last, ctx);
      return last.seq;
    }
    const slot = this.push("rx", ctx);
    slot.coarse = true;
    slot.t = grain(slot.t);
    slot.reason = reason;
    slot.count = 1;
    slot.bytes = bytes;
    slot.firstEnd = acceptedEnd;
    slot.lastEnd = acceptedEnd;
    this.openRx = { slot, seq: slot.seq };
    this.openParse = null;
    return slot.seq;
  }

  /**
   * Приём бинарного кадра — горячий путь. Кадры одной сессии и поколения с
   * непрерывными offsets сливаются в одну запись, пока после неё не случилось
   * ничего, кроме разбора этих же кадров (пара rx + parse-done); разрыв
   * offsets (acceptedEnd − bytes ≠ lastEnd) начинает новую, чтобы трасса не
   * прятала дыру. Возвращает seq записи — к нему привязывается ByteRecorder.noteRx.
   */
  noteRx(bytes: number, acceptedEnd: number, ctx?: TraceContext | null): number {
    this.acceptedEnd = acceptedEnd;
    if (this.isCoarse()) return this.noteTypingRx(bytes, acceptedEnd, ctx);
    const last = this.live(this.openRx);
    if (last && !last.coarse && this.pairIsLast() && last.ctx[SID] === cleanScalar(ctx?.sid) && last.ctx[GEN] === cleanScalar(ctx?.gen)
      && acceptedEnd - bytes === last.lastEnd) {
      last.count++;
      last.bytes += bytes;
      last.lastEnd = acceptedEnd;
      last.tEnd = this.time();
      copyContext(last, ctx);
      return last.seq;
    }
    const slot = this.push("rx", ctx);
    slot.count = 1;
    slot.bytes = bytes;
    slot.firstEnd = acceptedEnd;
    slot.lastEnd = acceptedEnd;
    slot.tEnd = slot.t;
    this.openRx = { slot, seq: slot.seq };
    this.openParse = null;
    return slot.seq;
  }

  /**
   * Ввод человека (I-15): нажатия сливаются в СЕРИЮ — одно событие, пока пауза
   * между нажатиями меньше INPUT_SERIES_GAP_MS и признак автоответа тот же.
   * В событии только корзина суммарной длины (bytes), общий исход (ok — все
   * отправки удались) и автоответ; время начала огрублено до
   * INPUT_TIME_GRAIN_MS, конца нет. По файлу не восстановить ни число нажатий,
   * ни интервалы между ними — длину пароля на приглашении sudo/ssh и ритм.
   * Вставка — отдельное событие, тоже с корзиной длины.
   *
   * Нажатие или вставка человека (не автоответ xterm) открывает отрезок
   * набора: открытая пара потока закрывается, и до первого кадра после паузы
   * INPUT_SERIES_GAP_MS (включая его разбор) поток идёт огрублёнными записями
   * reason "during-input", а у всех событий отрезка нет позиций и объёмов
   * потока (TRACE_STREAM_CONTEXT, TRACE_STREAM_FIELDS). Иначе эхо набранного
   * («*» у sudo с pwfeedback) отдавало длину точным rx.bytes, числом кадров
   * или позицией соседнего события. По границам отрезка вычислим только его
   * общий объём — эхо вместе с первым ответом программы.
   */
  noteInput(ctx: TraceContext | null | undefined, input: { len: number; ok: boolean; auto: boolean },
    reason: "key" | "encoded-paste"): number {
    const now = this.time();
    const len = Number.isFinite(input.len) && input.len > 0 ? input.len : 0;
    if (!input.auto) {
      if (this.typing === 0) { this.openRx = null; this.openParse = null; }
      this.typing = 1;
      this.humanT = now;
      this.flowSpan = true;
      this.windowSpan = true;
    }
    if (reason === "key") {
      const open = this.live(this.openInput);
      if (open && now - this.inputLastT < INPUT_SERIES_GAP_MS && open.fields?.auto === input.auto) {
        this.inputLastT = now;
        this.inputBytes += len;
        open.fields = { bytes: inputSizeBucket(this.inputBytes), ok: open.fields.ok === true && input.ok, auto: input.auto };
        return open.seq;
      }
    }
    const slot = this.push("input", ctx);
    slot.t = Math.floor(now / INPUT_TIME_GRAIN_MS) * INPUT_TIME_GRAIN_MS;
    slot.reason = reason;
    slot.fields = { bytes: inputSizeBucket(len), ok: input.ok, auto: input.auto };
    if (reason === "key") {
      this.openInput = { slot, seq: slot.seq };
      this.inputLastT = now;
      this.inputBytes = len;
    } else this.openInput = null;
    return slot.seq;
  }

  /**
   * Маркер потока reset/resumed (I-15, волна 8): событие sync и система
   * координат. Эпоха протокола и есть система координат offsets, поэтому база
   * перебазирования — своя у каждой эпохи:
   * - новая эпоха (перезапуск агента или PTY, offsets заново) считается от своей
   *   базы — у свежей эпохи 0, то есть позиции сырые: с отрезком набора прежней
   *   эпохи они не связаны и его объёма не отдают (раньше база прежней эпохи
   *   вычиталась и отсюда, и первая позиция больше неё отдавала саму базу).
   *   Отрезок, открытый в момент смены, продолжается и ждёт кадра новой эпохи;
   * - та же эпоха с offset ниже её базы (переподключение без resume,
   *   устаревший resume): поток досылается с места раньше конца отрезка, вместе
   *   с эхом набранного. Точные bytes такой досылки отдали бы длину, поэтому до
   *   спуска, разобравшего её до базы, записи огрублены (reason "resent-span");
   * - при неразобранных записях писателя (посреди соединения) открывается шов:
   *   они дописываются в прежних координатах, пока PtyTermView не скажет
   *   noteSyncDrained(seq) из барьера маркера.
   * ctx события — состояние ДО маркера (прежние координаты), поля — offset
   * маркера (новые): у них разные базы. Возвращает seq события sync.
   */
  noteSync(ctx: TraceContext | null | undefined, fields: Record<string, unknown> | null | undefined, reason: string,
    marker: { epoch?: string | null; offset?: number | null }): number {
    const coarseBefore = this.isCoarse();
    const oldBase = this.posBase;
    const epoch = typeof marker.epoch === "string" ? marker.epoch.slice(0, 64) : this.epoch;
    const changed = this.epoch !== null && epoch !== this.epoch;
    if (changed) {
      // Отрезок, не закрытый в уходящей эпохе, кончается не дальше принятого:
      // вернись поток в неё — позиции считались бы от этого места.
      this.rememberBase(this.epoch!, this.typing !== 0 ? Math.max(this.posBase, this.acceptedEnd) : this.posBase);
      this.posBase = this.epochBases.get(epoch!) ?? 0;
      // Кадр «после паузы» прежней эпохи — в чужих координатах: ждём кадр новой.
      if (this.typing !== 0) this.typing = 1;
    }
    if (epoch !== null && epoch !== this.epoch) {
      this.epoch = epoch;
      this.rememberBase(epoch, this.posBase);
    }
    const offset = typeof marker.offset === "number" && Number.isFinite(marker.offset) ? marker.offset : null;
    // Конец отрезка набора, ниже которого досылка несёт эхо набранного. При
    // ОТКРЫТОМ отрезке база ещё старая, а сам отрезок кончается не дальше
    // принятого: без этого (волна 9, скептик) досылка огрублялась только до
    // первого куска, а дальше писалась точно — и длина набранного считалась по
    // позициям её кусков.
    // ⚠ Только в ТОЙ ЖЕ эпохе: при смене эпохи acceptedEnd ещё в прежних
    // координатах, и брать его за конец отрезка — смешать две шкалы. Там базу
    // уходящей эпохи уже подняла ветка changed, а новая пишется сырой.
    const spanEnd = this.typing !== 0 && !changed ? Math.max(this.posBase, this.acceptedEnd) : this.posBase;
    if (offset !== null && offset < spanEnd) {
      // Позиции досылки считаются от конца отрезка, а не от прежней базы: иначе
      // разность позиций через отрезок отдала бы его объём.
      if (spanEnd > this.posBase) this.setBase(spanEnd);
      this.cover = { end: spanEnd, gen: cleanScalar(ctx?.gen) };
      this.flowSpan = true;
      this.openRx = null;
      this.openParse = null;
    } else if (offset !== null || changed) this.cover = null;
    const slot = this.push("sync", ctx);
    slot.ctxBase = oldBase;
    if (coarseBefore) {
      for (const i of STREAM_CTX) if (slot.ctx[i] !== undefined) { slot.ctx[i] = undefined; slot.cut |= CUT_STREAM; }
    }
    if (fields) slot.fields = sanitizeFields(fields, this.report);
    if (slot.fields && (coarseBefore || this.isCoarse())) slot.fields = cutStream(slot, slot.fields);
    const clean = cleanScalar(reason);
    if (typeof clean === "string") slot.reason = clean;
    else this.report.rejected++;
    // Замер потока приложения PtyTermView обнуляет на reset и смене эпохи (reset —
    // ещё раз, когда прежние записи разобраны: noteSyncDrained). Отрезок или
    // досылка, идущие через маркер, попадут и в новый замер.
    if (this.isCoarse()) this.windowSpan = true;
    else if (reason === "reset" || changed) this.windowSpan = false;
    this.windowToken = reason === "reset" ? slot.seq : -1;
    // Шов: записи, поставленные писателю до маркера, ещё разберутся. Нет их —
    // шва нет (и прежний, если был, закрыт); маркер без смены координат шов не
    // открывает, но и чужой не закрывает.
    const idle = ctx?.qLen === 0 && ctx?.wPending === 0 && ctx?.unacked === 0;
    if (idle) this.seam = null;
    else if (changed || this.cover !== null) {
      this.seam = { token: slot.seq, dropEnd: !!this.seam?.dropEnd || oldBase !== this.posBase };
    }
    return slot.seq;
  }

  /**
   * PtyTermView обнулил замер потока приложения НЕ на маркере, а по смене
   * переднего процесса (streamLinesRef/streamBytesRef = 0): прежнего отрезка
   * набора в новом окне замера нет, и объём снова пишется точно. Пока отрезок
   * или досылка открыты, новый замер соберёт то же эхо — окно остаётся.
   */
  noteStreamSampleReset(): void {
    if (this.isCoarse()) { this.windowSpan = true; return; }
    this.windowSpan = false;
    this.windowToken = -1;
  }

  /** Барьер маркера seq разобран: записи прежних координат кончились. */
  noteSyncDrained(token: number): void {
    if (this.seam && this.seam.token === token) this.seam = null;
    // Барьер reset обнуляет замер потока приложения окончательно.
    if (token === this.windowToken && !this.isCoarse()) this.windowSpan = false;
  }

  /** Поток пишется огрублённо: отрезок набора или досылка поверх него. */
  private isCoarse(): boolean {
    return this.typing !== 0 || this.cover !== null;
  }

  private coarseReason(): string {
    return this.typing !== 0 ? TRACE_DURING_INPUT : TRACE_RESENT_SPAN;
  }

  private setBase(end: number): void {
    this.posBase = end;
    if (this.epoch !== null) this.rememberBase(this.epoch, end);
  }

  private rememberBase(epoch: string, base: number): void {
    this.epochBases.delete(epoch);
    this.epochBases.set(epoch, base);
    if (this.epochBases.size > EPOCH_MEMORY) this.epochBases.delete(this.epochBases.keys().next().value as string);
  }

  /**
   * Накопленные поля diag flow (I-15, волна 8). Максимумы спусков и очередей и
   * сумма выброшенного копятся с открытия экрана, поэтому запись ПОСЛЕ отрезка
   * набора несёт и то, что было во время него: вставленный секрет с эхом одним
   * кадром мог стать самым большим спуском, и maxBatch отдал бы его длину.
   * После отрезка поле, выросшее с последней записанной величины, не пишется и
   * запоминается как неоднозначное. Дальше максимум, выросший ещё, достигнут уже
   * после отрезка и о нём не говорит — пишется; сумма, в которую вошёл отрезок,
   * растёт вместе с ним — не пишется, пока не станет меньше (новый экран,
   * счётчики с нуля).
   */
  private flowFields(slot: Slot, fields: TraceFields): TraceFields | undefined {
    let out: TraceFields | undefined;
    let withheld = false;
    for (const key of Object.keys(fields)) {
      const v = fields[key];
      const isMax = FLOW_MAX_FIELDS.includes(key);
      if ((isMax || FLOW_SUM_FIELDS.includes(key)) && typeof v === "number") {
        const ambiguous = this.flowAmbiguous[key];
        const hide = this.flowSpan
          ? !(v <= (this.flowKnown[key] ?? -1))
          : ambiguous !== undefined && (isMax ? v === ambiguous : v >= ambiguous);
        if (hide) {
          if (isMax || ambiguous === undefined) this.flowAmbiguous[key] = v;
          withheld = true;
          continue;
        }
        this.flowKnown[key] = v;
        delete this.flowAmbiguous[key];
      }
      (out ??= {})[key] = v;
    }
    this.flowSpan = false;
    if (withheld) slot.cut |= CUT_FLOW;
    return out;
  }

  /** Метка человека или пробы («Зафиксировать проблему», шаг сценария). */
  mark(label: string, ctx?: TraceContext | null): number {
    const clean = cleanScalar(label);
    return this.note("mark", ctx, null, typeof clean === "string" ? clean : "mark");
  }

  /** События обоих колец одной шкалой по seq. */
  snapshot(): TraceSnapshot {
    const events: TraceEvent[] = [];
    const main = this.main, stream = this.stream;
    let i = 0, j = 0;
    const streamSize = stream ? stream.size : 0;
    const shifted: ExportReport = { rebased: false, omitted: false, flow: 0, window: 0, seam: 0, stream: 0 };
    while (i < main.size || j < streamSize) {
      const a = i < main.size ? main.at(i) : null;
      const b = stream && j < streamSize ? stream.at(j) : null;
      if (b && (!a || b.seq < a.seq)) { events.push(toEvent(b, shifted)); j++; } else { events.push(toEvent(a!, shifted)); i++; }
    }
    const oldest = (ring: Ring) => (ring.size > 0 ? ring.at(0).seq : this.seqNext);
    const mainFirstSeq = oldest(main);
    return {
      events,
      dropped: main.dropped + (stream ? stream.dropped : 0),
      streamDropped: stream ? stream.dropped : 0,
      streamFirstSeq: stream ? oldest(stream) : mainFirstSeq,
      mainFirstSeq,
      firstSeq: events.length > 0 ? events[0].seq : this.seqNext,
      nextSeq: this.seqNext,
      capacity: this.capacity,
      streamCapacity: this.streamCapacity,
      sessions: [...this.sessionIds],
      rejectedFields: this.report.rejected,
      spansRebased: this.spansRebased,
      rebased: shifted.rebased,
      omitted: shifted.omitted,
      flowWithheld: shifted.flow,
      windowCoarsened: shifted.window,
      seamCut: shifted.seam,
      streamCut: shifted.stream,
    };
  }

  /** «Удалить»: события уходят, seq продолжается — номера не переиспользуются,
   * и метка trace-mark в логе агента не совпадёт с чужим событием.
   * Эпоха, базы, незакрытый отрезок набора, досылка и окна накопленных
   * счётчиков — состояние ПОТОКА, а не событий, и остаются (волна 8): иначе
   * позиции после «Удалить» снова стали бы абсолютными, а досылка уже
   * набранного и счётчики через набор — точными. */
  clear(): void {
    this.main.clear();
    this.stream?.clear();
    this.openRx = null;
    this.openParse = null;
    this.openInput = null;
    this.report.rejected = 0;
  }

  private push(kind: TraceKind, ctx: TraceContext | null | undefined): Slot {
    const ring = this.stream && STREAM_KINDS.has(kind) ? this.stream : this.main;
    const slot = ring.take(kind);
    slot.seq = this.seqNext++;
    slot.t = this.time();
    slot.kind = kind;
    slot.reason = undefined;
    slot.fields = undefined;
    slot.count = 0;
    slot.bytes = 0;
    slot.firstEnd = 0;
    slot.lastEnd = 0;
    slot.tEnd = 0;
    slot.coarse = false;
    slot.posBase = this.posBase;
    slot.ctxBase = this.posBase;
    slot.cut = 0;
    this.fill(slot, ctx);
    return slot;
  }

  /** ctx в слот; на отрезке набора и досылке поверх него — без позиций и
   * объёмов потока (снятое отмечено в cut). */
  private fill(slot: Slot, ctx: TraceContext | null | undefined): void {
    copyContext(slot, ctx);
    if (this.isCoarse()) {
      for (const i of STREAM_CTX) if (slot.ctx[i] !== undefined) { slot.ctx[i] = undefined; slot.cut |= CUT_STREAM; }
    }
  }
}

/** Поля без ключей потока (TRACE_STREAM_FIELDS); снятое отмечено в cut. */
function cutStream(slot: Slot, fields: TraceFields): TraceFields | undefined {
  const out = withoutStream(fields);
  if ((out ? Object.keys(out).length : 0) < Object.keys(fields).length) slot.cut |= CUT_STREAM;
  return out;
}

/**
 * Замер потока приложения огрублённо (см. TRACE_WINDOW_DIAG): точный объём
 * заменяется корзиной и отношением, которым принимается решение; счётчики строк
 * остаются точными. Поле bytes не просто снято — иначе файл перестал бы
 * отвечать на вопрос, из-за которого его и собирают.
 */
function coarseSample(slot: Slot, fields: TraceFields): TraceFields {
  const bytes = fields[TRACE_WINDOW_FIELD];
  if (typeof bytes !== "number") return fields;
  const lines = typeof fields.lines === "number" ? fields.lines : 0;
  const out: TraceFields = {};
  for (const key of Object.keys(fields)) if (key !== TRACE_WINDOW_FIELD) out[key] = fields[key];
  out.bytesRange = streamSampleBucket(bytes);
  if (bytes >= TRACE_SAMPLE_MIN_BYTES) out.lpk = coarseLinesPerKib(lines, bytes);
  out.typed = true;
  slot.cut |= CUT_WINDOW;
  return out;
}

function copyContext(slot: Slot, ctx: TraceContext | null | undefined): void {
  for (let i = 0; i < TRACE_CONTEXT_KEYS.length; i++) {
    slot.ctx[i] = ctx ? cleanScalar(ctx[TRACE_CONTEXT_KEYS[i]]) : undefined;
  }
}

const round2 = (n: number) => Math.round(n * 100) / 100;

/**
 * Абсолютная позиция от базы её эпохи (конца последнего отрезка набора в ней).
 * Сентинелы (≤0, «неизвестно») и позиции без базы (0) — как сняты. Позиция НИЖЕ
 * базы (устаревшая той же шкалы: база кадра экрана, снятого до конца отрезка) —
 * undefined, её в файле нет: ни 0 (противоречил бы соседним полям), ни
 * отрицательной (по известной сырой позиции база вычислялась бы). У rx её не
 * бывает: досылка ниже базы огрублена (noteSync).
 */
function shift(v: number, base: number, report: { rebased: boolean }): number | undefined {
  if (!(base > 0 && v > 0)) return v;
  report.rebased = true;
  return v >= base ? v - base : undefined;
}

/** Что при выгрузке оказалось пересчитано или убрано в events (для identical и notes). */
interface ExportReport { rebased: boolean; omitted: boolean; flow: number; window: number; seam: number; stream: number }

function toEvent(slot: Slot, report: ExportReport): TraceEvent {
  if (slot.coarse || slot.cut !== 0) report.omitted = true;
  if (slot.cut & CUT_FLOW) report.flow++;
  if (slot.cut & CUT_WINDOW) report.window++;
  if (slot.cut & CUT_SEAM) report.seam++;
  if (slot.cut & CUT_STREAM && !slot.coarse) report.stream++;
  const base = slot.posBase;
  const event: TraceEvent = { seq: slot.seq, t: round2(slot.t), kind: slot.kind };
  let ctx: TraceEvent["ctx"];
  for (let i = 0; i < TRACE_CONTEXT_KEYS.length; i++) {
    let v: TraceScalar | undefined = slot.ctx[i];
    if (v === undefined) continue;
    if (typeof v === "number" && POSITION_CTX.has(i)) v = shift(v, slot.ctxBase, report);
    if (v !== undefined) (ctx ??= {})[TRACE_CONTEXT_KEYS[i]] = v;
  }
  if (ctx) event.ctx = ctx;
  if (slot.reason !== undefined) event.reason = slot.reason;
  if (slot.fields) {
    const f: TraceFields = { ...slot.fields };
    // base у kbd — высота окна, а не позиция потока: kbd не перебазируем.
    if (base > 0 && slot.kind !== "kbd") {
      for (const key of POSITION_FIELDS) {
        const v = f[key];
        if (typeof v !== "number") continue;
        const r = shift(v, base, report);
        if (r === undefined) delete f[key];
        else f[key] = r;
      }
    }
    if (Object.keys(f).length > 0) event.f = f;
  }
  if (slot.coarse) {
    // Отрезок набора: объём корзиной, без числа кадров, конца и позиций.
    event.f = { ...event.f, bytes: inputSizeBucket(slot.bytes) };
  } else if (slot.kind === "rx") {
    event.count = slot.count;
    event.bytes = slot.bytes;
    const firstEnd = shift(slot.firstEnd, base, report);
    const lastEnd = shift(slot.lastEnd, base, report);
    if (firstEnd !== undefined) event.firstEnd = firstEnd;
    if (lastEnd !== undefined) event.lastEnd = lastEnd;
    event.tEnd = round2(slot.tEnd);
  } else if (slot.kind === "parse-done" && slot.count > 1) {
    event.count = slot.count;
    event.tEnd = round2(slot.tEnd);
  }
  return event;
}

/** Сведения о сборке и окружении (ST-00, T-40): client, commit, surface,
 * bundle, apk, agent, features. Только скаляры и один уровень вложенности. */
export type TraceIdentity = Record<string, TraceScalar | null | undefined | Record<string, TraceScalar | null | undefined>>;
export type CleanIdentity = Record<string, TraceScalar | Record<string, TraceScalar>>;

const IDENTITY_RE = /^[\x20-\x7e]{0,128}$/;

function cleanIdentityValue(v: unknown): TraceScalar | undefined {
  if (typeof v === "string") return IDENTITY_RE.test(v) ? v : undefined;
  if (typeof v === "number") return Number.isFinite(v) ? v : undefined;
  if (typeof v === "boolean") return v;
  return undefined;
}

function cleanIdentity(identity: TraceIdentity | null | undefined): CleanIdentity {
  const out: CleanIdentity = {};
  if (!identity || typeof identity !== "object") return out;
  for (const key of Object.keys(identity)) {
    if (!KEY_RE.test(key)) continue;
    const v = identity[key];
    if (v && typeof v === "object" && !Array.isArray(v)) {
      const nested: Record<string, TraceScalar> = {};
      for (const k of Object.keys(v)) {
        const clean = KEY_RE.test(k) ? cleanIdentityValue((v as Record<string, unknown>)[k]) : undefined;
        if (clean !== undefined) nested[k] = clean;
      }
      out[key] = nested;
    } else {
      const clean = cleanIdentityValue(v);
      if (clean !== undefined) out[key] = clean;
    }
  }
  return out;
}

export const TRACE_BUNDLE_FORMAT = "remotai-terminal-trace";

export interface TraceBundle {
  format: typeof TRACE_BUNDLE_FORMAT;
  v: 1;
  /** "bytes" — в bundle есть записанный вывод: предпросмотр и предупреждение обязательны. */
  content: "metadata" | "bytes";
  /** true — запись как снята. Любое преобразование или правка ставит false (ST-01). */
  identical: boolean;
  build: CleanIdentity;
  startedAtMs: number;
  exportedAtMs: number;
  capacity: number;
  /** Ёмкость бюджета потока; 0 — общее кольцо. */
  streamCapacity: number;
  firstSeq: number;
  nextSeq: number;
  sessions: string[];
  events: TraceEvent[];
  dropped: number;
  /** Вытеснено из бюджета потока: rx и parse-done раньше streamFirstSeq. */
  streamDropped: number;
  streamFirstSeq: number;
  /** Решения (всё, кроме rx и parse-done) раньше mainFirstSeq вытеснены, если
   * dropped − streamDropped > 0. */
  mainFirstSeq: number;
  /** Словами (ASCII) — что в файле неполно или огрублено; нет — всё на месте. */
  notes?: string[];
  rejectedFields: number;
  recording?: EncodedRecording;
}

/**
 * Отметки файла словами: вытесненное каждого кольца отдельно и отрезки набора.
 * Без них файл, где кольцо потока вытеснено, а решения нет, начинался с
 * событий без единого rx при firstSeq=1 — «агент ничего не присылал».
 */
export function traceNotes(snap: Pick<TraceSnapshot, "events" | "dropped" | "streamDropped" | "streamFirstSeq"
  | "mainFirstSeq" | "streamCapacity" | "spansRebased"> & Partial<Pick<TraceSnapshot, "rebased" | "flowWithheld"
  | "windowCoarsened" | "seamCut" | "streamCut">>): string[] {
  const notes: string[] = [];
  if (snap.streamDropped > 0) {
    notes.push(`stream frames (rx, parse-done) before seq ${snap.streamFirstSeq} were evicted: ${snap.streamDropped} records;`
      + " the first rx in this file is not the first frame received");
  }
  const mainDropped = snap.dropped - snap.streamDropped;
  if (mainDropped > 0) {
    notes.push(snap.streamCapacity > 0
      ? `decision events (all kinds except rx, parse-done) before seq ${snap.mainFirstSeq} were evicted: ${mainDropped}`
      : `events before seq ${snap.mainFirstSeq} were evicted: ${mainDropped}`);
  }
  const duringInput = snap.events.some((e) => e.reason === TRACE_DURING_INPUT && (e.kind === "rx" || e.kind === "parse-done"));
  const resent = snap.events.some((e) => e.reason === TRACE_RESENT_SPAN && (e.kind === "rx" || e.kind === "parse-done"));
  if (duringInput) {
    notes.push(`rx/parse-done "${TRACE_DURING_INPUT}": output received while a person typed, coarse on purpose:`
      + " size as a range, time to 1 s, no frame count and no stream positions (in ctx and fields of that span too)");
  }
  if (resent) {
    notes.push(`rx/parse-done "${TRACE_RESENT_SPAN}": the same stream epoch was re-sent from before the end of a typing`
      + " span (reconnect without resume), so it carries the typed echo again: coarse like the span itself until the"
      + " stream passes that point");
  }
  if ((snap.streamCut ?? 0) > 0 && !duringInput && !resent) {
    notes.push(`events recorded during a typing span or a re-sent span carry no stream positions or volumes in ctx`
      + ` and fields (x${snap.streamCut})`);
  }
  if (snap.rebased ?? (snap.spansRebased ?? 0) > 0) {
    notes.push(`stream positions after a typing span are rebased to the span's end within its stream epoch`
      + ` (x${snap.spansRebased}): absolute offsets do not carry across a typing span, so the byte volume it consumed`
      + " (which would reveal the typed length) cannot be recovered from the file; a new stream epoch (agent or PTY"
      + " restart) keeps its raw offsets; a stale position from before the span's end is left out");
  }
  if ((snap.flowWithheld ?? 0) > 0) {
    notes.push(`diag flow: cumulative fields (maxQueued, maxUnacked, maxBatch, dropBytes) that may include output from`
      + ` a typing span are left out (x${snap.flowWithheld})`);
  }
  if ((snap.windowCoarsened ?? 0) > 0) {
    notes.push(`diag ${TRACE_WINDOW_DIAG.join("/")}: the app stream sample, counted since the last reset marker, may`
      + " include a typing span, so its byte volume is coarse - a range (bytesRange) and rounded lines per KiB (lpk,"
      + ` the number the history-owner decision is made on) instead of the exact byte count (x${snap.windowCoarsened});`
      + " line counts (lines, own) are exact");
  }
  if ((snap.seamCut ?? 0) > 0) {
    notes.push(`parse-done at a stream marker: the end of writes still in the old coordinates is left out`
      + ` (x${snap.seamCut})`);
  }
  return notes;
}

/**
 * Файл «Зафиксировать проблему». По умолчанию только метаданные; байты вывода
 * попадают внутрь, лишь если человек явно включил запись и в ней что-то есть.
 */
export function traceBundle(identity: TraceIdentity, trace: TerminalTrace, recording?: RecordingSnapshot | null): TraceBundle {
  const snap = trace.snapshot();
  const withBytes = !!recording && recording.chunks.length > 0;
  const bundle: TraceBundle = {
    format: TRACE_BUNDLE_FORMAT,
    v: 1,
    content: withBytes ? "bytes" : "metadata",
    // Перебазирование, огрубление и опущенные поля — преобразование (ST-01).
    // Вытеснение кольцом — нет: оно названо отдельно (streamDropped, notes).
    identical: !snap.rebased && !snap.omitted,
    build: cleanIdentity(identity),
    startedAtMs: trace.startedAtMs,
    exportedAtMs: trace.wallNow(),
    capacity: snap.capacity,
    streamCapacity: snap.streamCapacity,
    firstSeq: snap.firstSeq,
    nextSeq: snap.nextSeq,
    sessions: snap.sessions,
    events: snap.events,
    dropped: snap.dropped,
    streamDropped: snap.streamDropped,
    streamFirstSeq: snap.streamFirstSeq,
    mainFirstSeq: snap.mainFirstSeq,
    rejectedFields: snap.rejectedFields,
  };
  const notes = traceNotes(snap);
  if (notes.length > 0) bundle.notes = notes;
  if (withBytes) bundle.recording = encodeRecording(recording!);
  return bundle;
}
