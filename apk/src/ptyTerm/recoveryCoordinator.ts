/**
 * ST-05 (план 13.09.2026): ОДИН координатор восстановления экрана на соединение.
 *
 * Было (карта ST-05): каждый вызов requestScreenFrame ставил свой таймер на
 * 500 мс и слал свой {t:"screen"}. Семь мест вызова, три разных троттла и ни
 * состояния «запрос в пути», ни срока ответа, ни предела повторов. 20 причин за
 * полсекунды давали 20 запросов; кадр чужой высоты просил следующий раз в
 * секунду, пока геометрия не сойдётся; молчаливый отказ сервера (зеркало не
 * готово, транспорт без ACK) терял причину до следующего случайного события.
 *
 * Здесь чистый автомат без React, DOM и таймеров. Время и случайность приходят
 * аргументами, поэтому каждый сценарий T-24/T-25/T-28/T-36 воспроизводится в
 * node детерминированно. PtyTermView остаётся тонким адаптером: зовёт переход,
 * исполняет эффекты (send / fallback / diag) и держит ОДИН таймер на
 * nextWakeAt().
 *
 *   idle → scheduled → in-flight → validating → applying → idle
 *                          │                         │
 *                          └──── retry / degraded ←──┘
 *
 * Ради чего модуль существует:
 * - одинаковые причины объединяются, а debounce ограничен MAX_WAIT_MS:
 *   поток событий не откладывает отправку бесконечно (T-24);
 * - в пути не больше одного запроса, и для v1, и для legacy (T-36);
 * - поздний ответ прежнего соединения, эпохи, геометрии или вытесненного
 *   запроса отбрасывается БЕЗ изменения состояния (T-25, I-05, I-08);
 * - повтор без прогресса ограничен MAX_NO_PROGRESS с backoff Full Jitter;
 * - холодная страница без кадра откатывается на reset ровно один раз (T-28).
 *
 * Диагностика только из метаданных: номера, причины, счётчики (I-15).
 */
import { frameIsStale } from "./geometry";
import { screenGeometryRevisionMatches } from "./geometryRevision";

/** Тишина перед отправкой. Равна прежней SCREEN_REQUEST_DELAY_MS в PtyTermView. */
export const SETTLE_MS = 500;
/** Потолок debounce от первой причины: непрерывные события не морят запрос голодом. */
export const MAX_WAIT_MS = 1500;
/** Срок ответа: больше серверных hold 750 мс + min-gap 1 с + транзит релея. */
export const RESPONSE_TIMEOUT_MS = 3000;
/** Сколько попыток подряд без прогресса допускается до degraded. */
export const MAX_NO_PROGRESS = 3;
/** Full Jitter: огибающая 1000·2^(n-1), но не больше 8000 мс. */
export const BACKOFF_BASE_MS = 1000;
export const BACKOFF_CAP_MS = 8000;
/** Кадр чужой ширины просим не чаще этого. Перенесено из GEOM_RESYNC_GAP_MS. */
export const WIDTH_MIN_INTERVAL_MS = 5000;
/**
 * Окно degraded. Внутри него повтор уже провалившейся причины не будит
 * координатор: частый queue-gap иначе крутил бы цикл «запрос → таймаут».
 * После окна та же причина — снова новое требование и одна попытка: queue-gap
 * через час — это новая потеря данных, а не повтор старой (ST-05, «свежие
 * требования не голодают»; backoff контролируемый, а не бесконечный).
 * Не меньше BACKOFF_CAP_MS: degraded не должен будить чаще, чем сам backoff.
 */
export const DEGRADED_COOLDOWN_MS = 30_000;
/**
 * Потолок ожидания первого размера нового сокета (см. holdForResize). Адаптер
 * ждёт рендер не дольше RENDERER_WAIT_MS = 1500 и сам отпускает запрос
 * (resizeSent); потолок — страховка, чтобы потерянное отпускание не оставило
 * соединение без кадра. Больше 1500, чтобы штатно отпускал адаптер.
 */
export const RESIZE_HOLD_MAX_MS = 2000;

export type RecoveryCause =
  | "open"
  | "cold-restore"
  | "queue-gap"
  | "stream-gap"
  | "foreground-gap"
  | "frame-stale"
  | "geometry-stale"
  | "geometry-height"
  | "geometry-width"
  | "epoch-reset"
  /**
   * Кадр ради ГЛУБИНЫ ИСТОРИИ зеркала (hist_lines), а не ради экрана: тёплое
   * переподключение, у страницы сохранённое решение «холодное открытие —
   * реплей» (hist < RICH_HISTORY_LINES или неизвестно), а поток с прошлого
   * кадра ушёл в прокрутку. Без него hist в pty.resume.<id> замирал на числе
   * последнего кадра страницы, и следующее холодное открытие тянуло весь хвост
   * кольца вместо дельты (ревью 15.09, дефект 1; см. causesForMarker).
   */
  | "hist-probe";

export type RecoveryPhase = "idle" | "scheduled" | "in-flight" | "validating" | "applying" | "degraded";

/** unknown — способность ещё не объявлена; legacy — агент без req; v1 — screen-request-v1. */
export type RecoveryPeer = "unknown" | "legacy" | "v1";

/** Постоянный порядок: набор причин хранится массивом без дублей, одинаковым для одинаковых множеств. */
const CAUSE_ORDER: readonly RecoveryCause[] = [
  "open",
  "cold-restore",
  "epoch-reset",
  "stream-gap",
  "queue-gap",
  "foreground-gap",
  "frame-stale",
  "geometry-stale",
  "geometry-height",
  "geometry-width",
  // В конце: биты прежних причин в трассе (causeMask) не сдвигаются.
  "hist-probe",
];

/**
 * Причины, которые рождает сам полученный кадр: он не подошёл. Их повтор
 * означает «сервер снова дал то же», это НЕ прогресс. Именно так кадр чужой
 * высоты крутил запрос раз в секунду (PtyTermView, ветка resync по высоте).
 * geometry-stale сюда не входит: новая локальная сетка — это новый мир, а не
 * неудача сервера.
 */
const NO_PROGRESS_CAUSES: ReadonlySet<RecoveryCause> = new Set<RecoveryCause>([
  "frame-stale",
  "geometry-height",
  "geometry-width",
]);

export interface RecoveryRequest {
  readonly id: number;
  readonly conn: number;
  readonly epoch: string;
  readonly geomRev: number;
  readonly causes: readonly RecoveryCause[];
  readonly sentAt: number;
  readonly deadline: number;
}

/** Кадр принят к применению и ждёт writer (validating → applying). */
export interface RecoveryPending {
  readonly id: number;
  readonly causes: readonly RecoveryCause[];
  readonly deadline: number;
  /** Причины, поднятые во время применения ЭТОГО кадра: по ним видно, был ли прогресс. */
  readonly raised: readonly RecoveryCause[];
}

export interface RecoveryState {
  readonly phase: RecoveryPhase;
  /** Неудовлетворённые причины. Снимаются только успешным applied(). */
  readonly causes: readonly RecoveryCause[];
  readonly conn: number;
  /** writerEpoch текущего маркера; "" — маркера на этом соединении ещё не было. */
  readonly epoch: string;
  readonly geomRev: number;
  readonly firstDemandAt: number | null;
  readonly dueAt: number | null;
  /** Единственный запрос в пути. */
  readonly request: RecoveryRequest | null;
  /**
   * Запрос, чей срок истёк, но который ещё актуален по conn/epoch/geomRev.
   * Сервер повторяет сам (OfferIfEmpty) и может ответить позже срока. Такой
   * ответ полезен, пока новый запрос не ушёл (риск из карты ST-05).
   */
  readonly late: RecoveryRequest | null;
  readonly pending: RecoveryPending | null;
  /** Причины, поднятые после отправки последнего запроса: этот кадр их не закрывает. */
  readonly fresh: readonly RecoveryCause[];
  readonly freshSince: number | null;
  readonly noProgress: number;
  readonly backoffUntil: number;
  readonly lastWidthSentAt: number | null;
  readonly peer: RecoveryPeer;
  readonly nextId: number;
  /** Откат на reset уже потрачен. Переживает реконнект, но не смену страницы (новое состояние). */
  readonly fallbackUsed: boolean;
  /** Причины на момент ухода в degraded: их повтор не будит координатор до конца окна. */
  readonly degradedCauses: readonly RecoveryCause[];
  /** Когда автомат последний раз ушёл в degraded; окно DEGRADED_COOLDOWN_MS считается от него. */
  readonly degradedAt: number | null;
  /** screen-none unavailable: у транспорта кадров нет вовсе, до нового соединения не просим. */
  readonly unavailable: boolean;
  /**
   * Ревизия сетки, для которой уже попросили кадр из-за кадра без req в чужой
   * ревизии (см. frame()). Одна просьба на ревизию — прежнее правило клиента
   * (geometryRetryRevisionRef) для старых агентов, T-36.
   */
  readonly staleGeomAskedRev: number | null;
  /**
   * Сокета нет или он ещё не открыт (обрыв, backoff, рукопожатие). Отсчитывать
   * нечего: запрос некуда слать, а срок ответа, истёкший без соединения, —
   * это обрыв, а не молчание агента. Причины копятся, но не будят; online()
   * ставит их в отправку. Замер ревью 14.09: без этого срок запроса умершего
   * сокета истекал в backoff и откат холодной страницы открывал ЛИШНИЙ сокет
   * в обход reconnectTimer (3 сокета вместо 2, T-25).
   */
  readonly offline: boolean;
  /**
   * Страница открыта с сохранённым offset и ещё не восстановлена ни кадром, ни
   * reset с реплеем кольца. Переживает обрыв и новое соединение (как
   * fallbackUsed): после обрыва в xterm уже лежит хвост, и без этого флага
   * следующий resumed выглядел бы тёплым — кадр не попросил бы никто (T-28).
   */
  readonly coldPage: boolean;
  /**
   * Причины, которые страница так и не закрыла к обрыву: накопленные и в
   * запросе/кадре умершего сокета. Без соединения лежат без действия и
   * возвращаются в causes в online(). Ревью 14.09, проверено запуском: без них
   * resumed без пропуска на странице, которая кадра так и не получила, выглядел
   * тёплым — кадр не просил никто, хотя прежний путь просил его на каждом onopen.
   */
  readonly owed: readonly RecoveryCause[];
  /**
   * Первый размер ЭТОГО сокета ещё не ушёл (ST-08: ждёт рендер WebGL), и
   * запрос кадра держится до resizeSent(), но не позже этого момента. null —
   * держать нечего. Не переживает соединение: у нового сокета своё ожидание.
   */
  readonly resizeHoldUntil: number | null;
}

export type RecoveryDiagWhat =
  | "timeout"
  | "stale-base"
  | "no-progress"
  | "degraded"
  | "revived"
  | "invalidated"
  | "negative"
  | "fallback";

export interface RecoverySend {
  readonly id: number;
  readonly geomRev: number;
  readonly causes: readonly RecoveryCause[];
}

export type RecoveryEffect =
  | ({ readonly kind: "send" } & RecoverySend)
  /** Один раз на страницу: сбросить resume и переподключиться (reset с реплеем кольца). */
  | { readonly kind: "fallback"; readonly action: "reconnect-reset" }
  | {
    readonly kind: "diag";
    readonly what: RecoveryDiagWhat;
    readonly id: number | null;
    readonly noProgress: number;
    readonly causes: readonly RecoveryCause[];
    readonly reason?: string;
  };

export interface RecoveryStep {
  readonly state: RecoveryState;
  readonly effects: readonly RecoveryEffect[];
}

export type FrameVerdict =
  | "discard:conn"
  | "discard:epoch"
  | "discard:geometry"
  | "discard:old-request"
  | "retry:stale-base"
  | "apply"
  /**
   * Кадр без req, когда операции нет (idle/scheduled/degraded без late):
   * внутренний повтор старого агента (OfferIfEmpty последней просьбы) или
   * кадр, отданный сервером без нашей просьбы. Соединение, эпоха, сетка и база
   * у него проверены так же строго, поэтому писать его безопасно — как писал
   * прежний клиент. Операцией он НЕ считается: состояние координатора не
   * меняется, причины не снимаются, счётчики не трогаются (ticket нет).
   */
  | "apply:unsolicited";

export interface FrameStep extends RecoveryStep {
  readonly verdict: FrameVerdict;
  /** Для apply: номер, который адаптер вернёт в applying()/applied(). */
  readonly ticket?: number;
}

export interface RecoveryFrameMeta {
  /** Поколение соединения, на котором кадр получен. */
  readonly conn: number;
  /** writerEpoch в момент получения кадра. */
  readonly epoch: string;
  /** geom_rev из сообщения как есть (unknown: проверка та же, что у screenGeometryRevisionMatches). */
  readonly geomRev: unknown;
  /** Эхо req от агента v1; у legacy его нет. */
  readonly req?: unknown;
  /** base_offset кадра; у старого агента его нет. */
  readonly base?: unknown;
}

/** Причина отказа от агента v1 ({t:"screen-none"}). Незнакомая причина читается как not-ready. */
export type ScreenNoneReason = "not-ready" | "unavailable" | "resize-pending" | (string & {});

export function createRecoveryState(init: { conn?: number; epoch?: string; geomRev?: number } = {}): RecoveryState {
  return {
    phase: "idle",
    causes: [],
    conn: init.conn ?? 0,
    epoch: init.epoch ?? "",
    geomRev: init.geomRev ?? 0,
    firstDemandAt: null,
    dueAt: null,
    request: null,
    late: null,
    pending: null,
    fresh: [],
    freshSince: null,
    noProgress: 0,
    backoffUntil: 0,
    lastWidthSentAt: null,
    peer: "unknown",
    nextId: 1,
    fallbackUsed: false,
    degradedCauses: [],
    degradedAt: null,
    unavailable: false,
    staleGeomAskedRev: null,
    offline: false,
    coldPage: false,
    owed: [],
    resizeHoldUntil: null,
  };
}

/** Причина поднята после отправки текущего запроса: после его кадра нужен ещё один. */
export function hasNewerDemand(s: RecoveryState): boolean {
  return s.fresh.length > 0;
}

/**
 * Full Jitter (AWS): случайная задержка в [0, min(cap, base·2^(n-1))].
 * Случайность приходит аргументом в [0, 1], поэтому тест задаёт её сам.
 */
export function backoffDelay(attempt: number, random: number): number {
  const n = Math.max(1, Math.floor(attempt));
  const envelope = Math.min(BACKOFF_CAP_MS, BACKOFF_BASE_MS * 2 ** (n - 1));
  const r = Number.isFinite(random) ? Math.min(1, Math.max(0, random)) : 0.5;
  return Math.floor(r * envelope);
}

function withCause(list: readonly RecoveryCause[], cause: RecoveryCause): readonly RecoveryCause[] {
  if (list.includes(cause)) return list;
  return CAUSE_ORDER.filter((c) => c === cause || list.includes(c));
}

function step(state: RecoveryState, effects: readonly RecoveryEffect[] = []): RecoveryStep {
  return { state, effects };
}

function diag(s: RecoveryState, what: RecoveryDiagWhat, id: number | null, reason?: string): RecoveryEffect {
  return reason === undefined
    ? { kind: "diag", what, id, noProgress: s.noProgress, causes: s.causes }
    : { kind: "diag", what, id, noProgress: s.noProgress, causes: s.causes, reason };
}

function isBusy(phase: RecoveryPhase): boolean {
  return phase === "in-flight" || phase === "validating" || phase === "applying";
}

/** Debounce внутри scheduled: срок сдвигается тишиной, но не дальше firstDemandAt + MAX_WAIT_MS. */
function debounced(s: RecoveryState, now: number): { firstDemandAt: number; dueAt: number } {
  const firstDemandAt = s.firstDemandAt ?? now;
  const dueAt = Math.min(Math.max(s.dueAt ?? now, now + SETTLE_MS), firstDemandAt + MAX_WAIT_MS);
  return { firstDemandAt, dueAt };
}

function scheduleFresh(s: RecoveryState, now: number): RecoveryState {
  return { ...s, phase: "scheduled", firstDemandAt: now, dueAt: now + SETTLE_MS };
}

/** Повтор после неудачи: тишина SETTLE_MS и backoff по счётчику без прогресса. */
function retry(s: RecoveryState, now: number, random: number, effects: RecoveryEffect[], minWait = 0): RecoveryStep {
  const wait = Math.max(SETTLE_MS, backoffDelay(s.noProgress, random), minWait);
  const next: RecoveryState = {
    ...s,
    phase: "scheduled",
    request: null,
    pending: null,
    firstDemandAt: now,
    dueAt: now + SETTLE_MS,
    backoffUntil: now + wait,
  };
  return step(next, effects);
}

function degrade(s: RecoveryState, effects: RecoveryEffect[], now: number): RecoveryStep {
  // Холодная страница, так и не получившая кадр (трижды not-ready, устаревшая
  // база, не записан), остаётся с одним хвостом после offset — ровно то, что
  // T-28 запрещает считать восстановлением. Откат на reset тот же, что на
  // первом молчании (timeout) и на unavailable, и так же один раз на страницу.
  const fallback = s.causes.includes("cold-restore") && !s.fallbackUsed;
  const next: RecoveryState = {
    ...s,
    phase: "degraded",
    request: null,
    pending: null,
    firstDemandAt: null,
    dueAt: null,
    degradedCauses: s.causes,
    degradedAt: now,
    fallbackUsed: s.fallbackUsed || fallback,
  };
  const tail: RecoveryEffect[] = [diag(next, "degraded", null)];
  if (fallback) tail.push({ kind: "fallback", action: "reconnect-reset" }, diag(next, "fallback", null, "degraded"));
  return step(next, [...effects, ...tail]);
}

/**
 * Набор причин одним числом для трассы (бит = место в CAUSE_ORDER): строка из
 * имён причин не влезает в короткое поле трассы (I-15, 32 символа).
 */
export function causeMask(causes: readonly RecoveryCause[]): number {
  let mask = 0;
  for (const c of causes) {
    const bit = CAUSE_ORDER.indexOf(c);
    if (bit >= 0) mask |= 1 << bit;
  }
  return mask;
}

/** Обратное к causeMask: для чтения трассы и для тестов. */
export function causesFromMask(mask: number): RecoveryCause[] {
  return CAUSE_ORDER.filter((_, bit) => (mask & (1 << bit)) !== 0);
}

/** Момент, когда scheduled вправе отправить: debounce, max-wait, backoff и троттл ширины. */
function effectiveDueAt(s: RecoveryState): number | null {
  if (s.phase !== "scheduled" || s.dueAt === null || s.causes.length === 0) return null;
  let at = s.dueAt;
  if (s.firstDemandAt !== null) at = Math.min(at, s.firstDemandAt + MAX_WAIT_MS);
  at = Math.max(at, s.backoffUntil);
  // Первый размер сокета ещё не ушёл: кадр, снятый до него, агент снимет в
  // ПРЕЖНЕЙ сетке PTY (см. holdForResize).
  if (s.resizeHoldUntil !== null) at = Math.max(at, s.resizeHoldUntil);
  // Кадр чужой ширины может не сойтись никогда (рядом открыт более узкий
  // зритель, PTY идёт по минимуму), поэтому запрос с этой причиной уходит не
  // чаще WIDTH_MIN_INTERVAL_MS. Прежний троттл ветки ширины, теперь в одном месте.
  if (s.causes.includes("geometry-width") && s.lastWidthSentAt !== null) {
    at = Math.max(at, s.lastWidthSentAt + WIDTH_MIN_INTERVAL_MS);
  }
  return at;
}

function validReq(value: unknown): number | undefined {
  return Number.isSafeInteger(value) && (value as number) >= 1 ? (value as number) : undefined;
}

/**
 * Новая причина. Повтор той же причины идемпотентен для набора. В scheduled
 * он продлевает тишину, но не дальше max-wait. Причина во время запроса
 * становится «более новым требованием»: после применения уйдёт ровно один
 * следующий запрос.
 */
export function demand(prev: RecoveryState, cause: RecoveryCause, now: number): RecoveryStep {
  const s = cause === "cold-restore" && !prev.coldPage ? { ...prev, coldPage: true } : prev;
  // hist-probe нужен ради hist_lines, а их несёт ЛЮБОЙ кадр: операция, уже
  // поставленная или в пути, ответит и на него. Как причина она дала бы после
  // своего кадра ещё один запрос (fresh) — лишний (ревью 15.09, повторная).
  if (cause === "hist-probe" && !s.offline && (s.phase === "scheduled" || isBusy(s.phase))) return step(s);
  const causes = withCause(s.causes, cause);
  // Без соединения причина только копится: будить нечего и слать некуда.
  if (s.offline) return step({ ...s, causes });
  switch (s.phase) {
    case "idle":
      return step({ ...s, phase: "scheduled", causes, firstDemandAt: now, dueAt: now + SETTLE_MS });
    case "scheduled": {
      const { firstDemandAt, dueAt } = debounced(s, now);
      // fresh считается относительно запроса, чей ответ ещё может прийти (late).
      const fresh = s.late ? withCause(s.fresh, cause) : s.fresh;
      return step({ ...s, causes, firstDemandAt, dueAt, fresh, freshSince: s.late ? (s.freshSince ?? now) : s.freshSince });
    }
    case "in-flight":
      return step({ ...s, causes, fresh: withCause(s.fresh, cause), freshSince: s.freshSince ?? now });
    case "validating":
    case "applying": {
      const pending = s.pending ? { ...s.pending, raised: withCause(s.pending.raised, cause) } : s.pending;
      return step({ ...s, causes, pending, fresh: withCause(s.fresh, cause), freshSince: s.freshSince ?? now });
    }
    case "degraded": {
      const fresh = s.late ? withCause(s.fresh, cause) : s.fresh;
      // Повтор уже провалившейся причины внутри окна не будит: иначе частый
      // queue-gap превратил бы degraded в цикл «запрос → таймаут» раз в 3,5 с.
      // Новая причина или та же после окна (свежие требования не голодают)
      // даёт ровно одну попытку; её провал снова открывает окно.
      const repeat = s.degradedCauses.includes(cause)
        && s.degradedAt !== null
        && now < s.degradedAt + DEGRADED_COOLDOWN_MS;
      if (s.unavailable || repeat) return step({ ...s, causes, fresh });
      const next: RecoveryState = {
        ...scheduleFresh({ ...s, causes, fresh }, now),
        noProgress: Math.min(s.noProgress, MAX_NO_PROGRESS - 1),
        degradedCauses: withCause(s.degradedCauses, cause),
      };
      const reason = s.degradedCauses.includes(cause) ? `${cause}:cooldown` : cause;
      return step(next, [diag(next, "revived", null, reason)]);
    }
  }
}

/**
 * Локальная сетка xterm сменилась. Текущий запрос и кадр в применении
 * устарели: их поздний ответ отбросит проверка геометрии, а причины
 * возвращаются в набор. Непрерывная смена геометрии сдвигает отправку, но не
 * дальше max-wait. Счётчик без прогресса обнуляется: новая сетка — новый мир.
 */
export function geometry(s: RecoveryState, rev: number, now: number): RecoveryStep {
  if (rev === s.geomRev) return step(s);
  // Троттл ширины считался против ПРЕЖНЕЙ сетки: в новой просьба — новая
  // попытка, как и счётчик без прогресса (иначе давно сошедшаяся ширина
  // задерживала бы на 5 с чужие причины — замер 14.09, T-25).
  const base: RecoveryState = {
    ...s, geomRev: rev, late: null, noProgress: 0, backoffUntil: 0, degradedCauses: [], degradedAt: null,
    lastWidthSentAt: null,
  };
  if (isBusy(s.phase)) {
    const id = s.request?.id ?? s.pending?.id ?? null;
    const next = scheduleFresh({
      ...base,
      request: null,
      pending: null,
      fresh: [],
      freshSince: null,
      causes: withCause(s.causes, "geometry-stale"),
    }, now);
    return step(next, [diag(next, "invalidated", id, "geometry")]);
  }
  if (s.phase === "scheduled") return step({ ...base, ...debounced(s, now) });
  if (s.phase === "degraded" && !s.unavailable && s.causes.length > 0) return step(scheduleFresh(base, now));
  return step(base);
}

/**
 * Сетку xterm сменил САМ кадр операции `id`, пока его применяют: принял сетку
 * PTY (adopt) или вернул свою логическую высоту под клавиатурой (restore).
 * Кадр пишется ровно в эту сетку, поэтому он не устарел: только новая ревизия,
 * без invalidated и без geometry-stale. Иначе geometry() отменял операцию,
 * чей кадр сейчас пишется, и через SETTLE_MS уходил второй запрос на каждое
 * открытие (ревью 15.09, повторная проверка: 2 запроса вместо 1 и на сборке до
 * исправления). Чужой номер или операции нет — ничего: решает geometry().
 */
export function adopted(s: RecoveryState, id: number, rev: number): RecoveryStep {
  if (rev === s.geomRev || (s.phase !== "validating" && s.phase !== "applying") || s.pending?.id !== id) return step(s);
  return step({ ...s, geomRev: rev });
}

function mergeCauses(a: readonly RecoveryCause[], b: readonly RecoveryCause[]): readonly RecoveryCause[] {
  if (b.every((c) => a.includes(c))) return a;
  const out = [...a];
  for (const c of b) if (!out.includes(c)) out.push(c);
  return out;
}

/** Неснятые причины страницы: уже отложенные, накопленные, в запросе (и опоздавшем), в кадре у writer. */
function owedCauses(s: RecoveryState): readonly RecoveryCause[] {
  let out = mergeCauses(s.owed, s.causes);
  for (const c of [s.request?.causes, s.late?.causes, s.pending?.causes]) if (c) out = mergeCauses(out, c);
  return out;
}

/**
 * Новое соединение: полный сброс. Переживают только номера запросов,
 * геометрия, потраченный откат, незакрытая холодная страница и неснятые
 * причины (owed — до online()).
 */
export function connection(s: RecoveryState, conn: number): RecoveryStep {
  return step({
    ...createRecoveryState({ conn, geomRev: s.geomRev }),
    nextId: s.nextId,
    fallbackUsed: s.fallbackUsed,
    coldPage: s.coldPage,
    owed: owedCauses(s),
  });
}

/**
 * Соединения нет: сокет закрылся или ещё не открыт. Запрос в пути и кадр в
 * применении принадлежали умершему сокету — их сроки больше ничего не решают
 * (T-25): ни повтора, ни degraded, ни отката холодной страницы. Автомат стоит
 * в idle без будильника, пока online() не скажет, что сокет открыт. Переживает
 * то же, что и в connection(), плюс номер соединения: кадр прежнего сокета
 * отбросит проверка conn уже в новом.
 */
export function offline(s: RecoveryState): RecoveryStep {
  if (s.offline) return step(s);
  const id = s.request?.id ?? s.pending?.id ?? null;
  const next: RecoveryState = {
    ...createRecoveryState({ conn: s.conn, epoch: s.epoch, geomRev: s.geomRev }),
    nextId: s.nextId,
    fallbackUsed: s.fallbackUsed,
    coldPage: s.coldPage,
    owed: owedCauses(s),
    offline: true,
  };
  return step(next, id === null ? [] : [diag(next, "invalidated", id, "offline")]);
}

/**
 * Сокет открыт: причины, накопленные без соединения, и неснятые причины
 * прежнего сокета (owed) уходят обычной тишиной. Честно тёплой странице
 * просить нечего — лишней перерисовки нет (ST-09 A6).
 */
export function online(s: RecoveryState, now: number): RecoveryStep {
  if (!s.offline) return step(s);
  const base: RecoveryState = s.owed.length > 0
    ? { ...s, offline: false, causes: mergeCauses(s.causes, s.owed), owed: [] }
    : { ...s, offline: false };
  return step(base.causes.length > 0 ? scheduleFresh(base, now) : base);
}

/**
 * Первый размер нового сокета отложен до подключения рендера (ST-08, T-32):
 * иначе новый зритель давал два PTY resize на join. Запрос кадра, ушедший
 * РАНЬШЕ этого размера, агент снимает в прежней сетке PTY (23 строки при
 * наших 20), клиент его отвергает или пишет с resync — и просит снова.
 * Ревью 15.09 (дефект 2, живьём на 2.71.1): до четырёх запросов на открытие,
 * причина open уходила через SETTLE_MS = 500, а размер ждал рендер до 1500.
 * Причины копятся как обычно; отправка — после resizeSent() или по потолку
 * RESIZE_HOLD_MAX_MS. Зовётся на onopen, пока автомат ещё offline.
 */
export function holdForResize(s: RecoveryState, now: number): RecoveryStep {
  return step({ ...s, resizeHoldUntil: now + RESIZE_HOLD_MAX_MS });
}

/**
 * Первый размер ушёл (или ждать его больше нечего). Созревший запрос уходит
 * ближайшим будильником — уже после размера в том же сокете, поэтому агент
 * снимает кадр в нашей сетке, а запрос считается доставленным (delivered).
 */
export function resizeSent(s: RecoveryState): RecoveryStep {
  return step(s.resizeHoldUntil === null ? s : { ...s, resizeHoldUntil: null });
}

/**
 * Маркер reset: терминал стёрт RIS и получил реплей кольца — страница больше
 * не холодная, её базовую точку закрывает причина open/epoch-reset. Остальное
 * состояние не меняется.
 */
export function streamReset(s: RecoveryState): RecoveryStep {
  return step(s.coldPage ? { ...s, coldPage: false } : s);
}

/**
 * Новый маркер reset/resumed дал новую эпоху writer. Запрос и кадр прежней
 * эпохи устарели. Первый маркер соединения (эпоха была "") ничего не
 * отменяет: сервер всегда шлёт маркер раньше любого кадра, так что ответ на
 * запрос, ушедший до маркера, уже относится к новой эпохе.
 */
export function epoch(s: RecoveryState, e: string, now: number): RecoveryStep {
  if (e === s.epoch) return step(s);
  if (s.epoch === "") return step({ ...s, epoch: e });
  // Новая базовая точка потока: кадр новой эпохи не ждёт троттла ширины,
  // заработанного кадрами прежней.
  const base: RecoveryState = {
    ...s, epoch: e, late: null, noProgress: 0, backoffUntil: 0, degradedCauses: [], degradedAt: null,
    lastWidthSentAt: null,
  };
  if (isBusy(s.phase)) {
    const id = s.request?.id ?? s.pending?.id ?? null;
    const next = scheduleFresh({ ...base, request: null, pending: null, fresh: [], freshSince: null }, now);
    return step(next, [diag(next, "invalidated", id, "epoch")]);
  }
  if (s.phase === "degraded" && !s.unavailable && s.causes.length > 0) return step(scheduleFresh(base, now));
  return step(base);
}

/** Агент подтвердил screen-request-v1 (или адаптер решил, что подтверждения не будет). */
export function capability(s: RecoveryState, v1: boolean): RecoveryStep {
  const peer: RecoveryPeer = v1 ? "v1" : "legacy";
  return step(s.peer === peer ? s : { ...s, peer });
}

/** Причины «кадр не совпал с сеткой»: их снимает только новая сетка сервера. */
const GRID_CAUSES: readonly RecoveryCause[] = ["geometry-height", "geometry-width"];

/**
 * Сервер подтвердил новую сетку PTY (/state или terminal-controls). Это новое
 * обстоятельство, а не повтор причины. Клиентский debounce строк 2000 мс:
 * три попытки на кадр чужой высоты уходят раньше, чем сервер применит resize,
 * и без этого перехода кадр правильной высоты не запросился бы никогда.
 * - degraded по высоте/ширине: ровно одна попытка, даже внутри окна;
 * - запрос или кадр в пути: он ушёл до подтверждения, поэтому после его
 *   возможного провала гарантирована ещё одна попытка;
 * - scheduled: отправка и так будет после подтверждения, менять нечего.
 * Число вызовов ограничено числом настоящих смен сетки, цикла нет.
 */
export function serverGrid(s: RecoveryState, now: number): RecoveryStep {
  if (s.unavailable || !s.causes.some((c) => GRID_CAUSES.includes(c))) return step(s);
  if (s.phase === "degraded") {
    const next: RecoveryState = { ...scheduleFresh(s, now), noProgress: MAX_NO_PROGRESS - 1 };
    return step(next, [diag(next, "revived", null, "server-grid")]);
  }
  if (isBusy(s.phase)) {
    const noProgress = Math.min(s.noProgress, Math.max(0, MAX_NO_PROGRESS - 2));
    return step(noProgress === s.noProgress ? s : { ...s, noProgress });
  }
  return step(s);
}

/**
 * Пора ли отправлять. Срабатывает только из scheduled, поэтому в пути не
 * больше одного запроса, и для v1, и для legacy. req можно слать всегда:
 * старый parseScreenFrameRequest читает только geom_rev (I-14).
 */
export function due(s: RecoveryState, now: number): RecoveryStep & { send?: RecoverySend } {
  const at = effectiveDueAt(s);
  if (at === null || now < at) return step(s);
  const id = s.nextId;
  const request: RecoveryRequest = {
    id,
    conn: s.conn,
    epoch: s.epoch,
    geomRev: s.geomRev,
    causes: s.causes,
    sentAt: now,
    deadline: now + RESPONSE_TIMEOUT_MS,
  };
  const next: RecoveryState = {
    ...s,
    phase: "in-flight",
    request,
    late: null,
    fresh: [],
    freshSince: null,
    firstDemandAt: null,
    dueAt: null,
    nextId: id < Number.MAX_SAFE_INTEGER ? id + 1 : 1,
    lastWidthSentAt: s.causes.includes("geometry-width") ? now : s.lastWidthSentAt,
  };
  const send: RecoverySend = { id, geomRev: s.geomRev, causes: s.causes };
  return { state: next, effects: [{ kind: "send", ...send }], send };
}

/**
 * Пришёл кадр. Порядок проверок: чужое соединение, эпоха, геометрия, затем
 * соответствие запросу и только потом база. Любой discard возвращает ТО ЖЕ
 * состояние (T-25): поздний кадр ничего не меняет, даже счётчики.
 *
 * `applied` — позиция, которую человек уже видит (appliedOffset, не принятый
 * offset: см. комментарий у writeFrameOnce в PtyTermView).
 */
export function frame(
  s: RecoveryState,
  meta: RecoveryFrameMeta,
  applied: number,
  now: number,
  random = 0.5,
): FrameStep {
  const discard = (verdict: FrameVerdict): FrameStep => ({ state: s, effects: [], verdict });
  if (s.offline || meta.conn !== s.conn) return discard("discard:conn");
  if (meta.epoch !== s.epoch) return discard("discard:epoch");
  const req = validReq(meta.req);
  if (!screenGeometryRevisionMatches(meta.geomRev, s.geomRev)) {
    // Кадр без req в чужой ревизии, когда своей операции нет: сервер отдаёт
    // кадры (внутренний повтор последней просьбы старого агента), но снятые в
    // сетке, которой у нас уже нет. Прежний клиент в этом случае один раз на
    // ревизию просил свежий (geometryRetryRevisionRef) — старый агент иначе не
    // узнает новую ревизию никогда. Сам кадр отброшен, как и раньше; просьба —
    // новая операция в новой сетке, текущую она не трогает (её нет). Ответ
    // вытесненной просьбе (с req) и кадр во время операции ничего не просят:
    // новую сетку операции уже учёл geometry().
    if (req === undefined && !isBusy(s.phase) && !s.unavailable && s.staleGeomAskedRev !== s.geomRev) {
      const asked = demand({ ...s, staleGeomAskedRev: s.geomRev }, "geometry-stale", now);
      return { ...asked, verdict: "discard:geometry" };
    }
    return discard("discard:geometry");
  }

  let answered: RecoveryRequest | null = null;
  if (s.phase === "in-flight" && s.request) {
    // legacy не знает req: ответ сопоставляется по conn/epoch/geomRev, а в
    // пути всегда не больше одного запроса, так что путать не с чем.
    if (req === undefined || req === s.request.id) answered = s.request;
  } else if ((s.phase === "scheduled" || s.phase === "degraded") && s.late) {
    if (req === undefined || req === s.late.id) answered = s.late;
  }
  if (!answered) {
    // Без req и без операции в пути: это не ответ на вытесненный запрос (у v1
    // он нёс бы req), а кадр, который сервер отдал сам. Прежний клиент такой
    // писал; отбросить его — оставить экран без кадра, который никто больше
    // не пришлёт. Писать можно только свежий и только вне применения другого.
    if (req === undefined && !isBusy(s.phase)) {
      const base = typeof meta.base === "number" ? meta.base : undefined;
      if (!frameIsStale(base, applied)) return { state: s, effects: [], verdict: "apply:unsolicited" };
    }
    // Кадр в применении уже есть или req чужой: это не текущая операция.
    // Поток при этом непрерывен, так что лишний кадр ничего не добавил бы.
    return discard("discard:old-request");
  }

  // Эхо req доказывает v1. Ответ без req на наш запрос — старый агент.
  const peer: RecoveryPeer = req !== undefined ? "v1" : (s.peer === "unknown" ? "legacy" : s.peer);

  const base = typeof meta.base === "number" ? meta.base : undefined;
  if (frameIsStale(base, applied)) {
    const noProgress = s.noProgress + 1;
    const next: RecoveryState = {
      ...s,
      peer,
      request: null,
      late: null,
      fresh: [],
      freshSince: null,
      causes: withCause(s.causes, "frame-stale"),
      noProgress,
    };
    const effects = [diag(next, "stale-base", answered.id)];
    const res = noProgress >= MAX_NO_PROGRESS ? degrade(next, effects, now) : retry(next, now, random, effects);
    return { ...res, verdict: "retry:stale-base" };
  }

  const pending: RecoveryPending = {
    id: answered.id,
    causes: answered.causes,
    deadline: now + RESPONSE_TIMEOUT_MS,
    raised: [],
  };
  const next: RecoveryState = {
    ...s,
    peer,
    phase: "validating",
    request: null,
    late: null,
    pending,
    firstDemandAt: null,
    dueAt: null,
  };
  return { state: next, effects: [], verdict: "apply", ticket: answered.id };
}

/** Адаптер начал писать кадр в TerminalWriter. Только отметка фазы для trace. */
export function applying(s: RecoveryState, id: number): RecoveryStep {
  if (s.phase !== "validating" || s.pending?.id !== id) return step(s);
  return step({ ...s, phase: "applying" });
}

/**
 * Кадр разобран (ok) или не записан (ok=false: чужая ширина, барьер сорван,
 * терминал умер). Колбэк чужого номера — пустышка (I-08). Успех снимает
 * причины запроса, кроме поднятых после его отправки. Если кадр тут же поднял
 * причину «не подошёл» (чужая высота/ширина), прогресса не было.
 */
export function applied(s: RecoveryState, id: number, ok: boolean, now: number, random = 0.5): RecoveryStep {
  if ((s.phase !== "validating" && s.phase !== "applying") || !s.pending || s.pending.id !== id) return step(s);
  const p = s.pending;
  const causes = ok ? s.causes.filter((c) => !p.causes.includes(c) || s.fresh.includes(c)) : s.causes;
  const progress = ok && !p.raised.some((c) => NO_PROGRESS_CAUSES.has(c));
  // Записанный кадр, просивший cold-restore, закрывает холодную страницу, даже
  // если сразу поднял причину «не подошёл»: экран уже не один хвост.
  const coldPage = s.coldPage && !(ok && p.causes.includes("cold-restore"));
  const base: RecoveryState = { ...s, pending: null, causes, fresh: [], freshSince: null, coldPage };
  if (progress) {
    const next: RecoveryState = { ...base, noProgress: 0, backoffUntil: 0, degradedCauses: [], degradedAt: null };
    if (causes.length === 0) return step({ ...next, phase: "idle", firstDemandAt: null, dueAt: null });
    const firstDemandAt = s.freshSince ?? now;
    return step({ ...next, phase: "scheduled", firstDemandAt, dueAt: Math.max(now, Math.min(now + SETTLE_MS, firstDemandAt + MAX_WAIT_MS)) });
  }
  const noProgress = s.noProgress + 1;
  const failed: RecoveryState = { ...base, noProgress };
  const effects = [diag(failed, "no-progress", id, ok ? "raised" : "not-applied")];
  if (noProgress >= MAX_NO_PROGRESS) return degrade(failed, effects, now);
  return retry(failed, now, random, effects);
}

/**
 * Отказ агента v1 ({t:"screen-none", req, reason}). Чужой req игнорируется.
 * - unavailable: у транспорта кадров нет (SSH, прямой PTY, старый pty-host).
 *   Просить бессмысленно до нового соединения; холодной странице — откат.
 * - resize-pending: сервер ждёт resize. Ждём geometry() или срок ответа.
 * - not-ready и незнакомое: зеркало не в Ground, повтор с backoff.
 */
export function negative(
  s: RecoveryState,
  n: { req: unknown; reason: ScreenNoneReason },
  now: number,
  random = 0.5,
): RecoveryStep {
  if (s.phase !== "in-flight" || !s.request || validReq(n.req) !== s.request.id) return step(s);
  const id = s.request.id;
  const base: RecoveryState = { ...s, peer: "v1", request: null, late: null, fresh: [], freshSince: null };
  if (n.reason === "unavailable") {
    const fallback = base.causes.includes("cold-restore") && !base.fallbackUsed;
    const next: RecoveryState = { ...base, unavailable: true, fallbackUsed: base.fallbackUsed || fallback };
    const effects: RecoveryEffect[] = [diag(next, "negative", id, n.reason)];
    if (fallback) effects.push({ kind: "fallback", action: "reconnect-reset" }, diag(next, "fallback", id, n.reason));
    return degrade(next, effects, now);
  }
  const noProgress = s.noProgress + 1;
  // resize-pending: сервер сам повторит снимок после ACK, и кадр придёт с ТЕМ
  // ЖЕ req (B1, контракт screen-none). Такой ответ полезен, пока наш повтор не
  // ушёл, — ровно роль late. Без этого поздний кадр отбрасывался бы как
  // old-request, а свой повтор ждал бы полный срок ответа.
  const pendingRetry = n.reason === "resize-pending" ? s.request : null;
  const next: RecoveryState = { ...base, noProgress, late: pendingRetry };
  const effects = [diag(next, "negative", id, n.reason)];
  if (noProgress >= MAX_NO_PROGRESS) return degrade(next, effects, now);
  return retry(next, now, random, effects, n.reason === "resize-pending" ? RESPONSE_TIMEOUT_MS : 0);
}

/**
 * Срок истёк. Запрос без ответа — это попытка без прогресса. Холодная
 * страница (только хвост после offset, T-28) не ждёт трёх попыток с
 * backoff: при первом же молчании откат на reset, ровно один раз на страницу.
 * У кадра в применении свой срок: writer не ответил — считаем «не записан».
 */
export function timeout(s: RecoveryState, now: number, random = 0.5): RecoveryStep {
  if (s.phase === "in-flight" && s.request && now >= s.request.deadline) {
    const id = s.request.id;
    const noProgress = s.noProgress + 1;
    const base: RecoveryState = { ...s, request: null, late: s.request, noProgress };
    const effects: RecoveryEffect[] = [diag(base, "timeout", id)];
    if (base.causes.includes("cold-restore") && !base.fallbackUsed) {
      const next: RecoveryState = { ...base, fallbackUsed: true };
      effects.push({ kind: "fallback", action: "reconnect-reset" }, diag(next, "fallback", id, "timeout"));
      return degrade(next, effects, now);
    }
    if (noProgress >= MAX_NO_PROGRESS) return degrade(base, effects, now);
    return retry(base, now, random, effects);
  }
  if ((s.phase === "validating" || s.phase === "applying") && s.pending && now >= s.pending.deadline) {
    return applied(s, s.pending.id, false, now, random);
  }
  return step(s);
}

/** Единственная точка пробуждения: адаптер держит ОДИН таймер на соединение. */
export function nextWakeAt(s: RecoveryState): number | null {
  switch (s.phase) {
    case "scheduled":
      return effectiveDueAt(s);
    case "in-flight":
      return s.request ? s.request.deadline : null;
    case "validating":
    case "applying":
      return s.pending ? s.pending.deadline : null;
    default:
      return null;
  }
}

/** Срабатывание таймера: сначала сроки, потом отправка. Поздний таймер безвреден. */
export function wake(s: RecoveryState, now: number, random = 0.5): RecoveryStep & { send?: RecoverySend } {
  const t = timeout(s, now, random);
  const d = due(t.state, now);
  return { state: d.state, effects: [...t.effects, ...d.effects], send: d.send };
}

export type MarkerPath = "warm" | "hist-probe" | "cold-restore" | "stream-gap" | "epoch-reset" | "baseline";

/**
 * Какие причины рождает маркер reset/resumed. Прежде ветка маркеров кадр не
 * просила вовсе, в том числе на resumed+gap и на смене эпохи посреди
 * соединения (серверный resync): TUI, рисующий диффом, оставался собранным из
 * неполного потока (карта ST-05, P2).
 *
 * - baseline: первый reset соединения. Причина open уже заявлена на onopen.
 * - epoch-reset: reset посреди соединения, новая базовая точка.
 * - cold-restore: resumed в пустой xterm (страница пересоздана, есть только
 *   offset). Один offset не восстанавливает экран, режимы и историю.
 * - stream-gap: resumed с пропуском вне кольца.
 * - warm: resumed без пропуска на живой странице. Экрану ничего не нужно.
 * - hist-probe: тот же warm, но адаптер сказал histProbe — сохранённое решение
 *   следующего холодного открытия «реплей» (hist < RICH_HISTORY_LINES или
 *   неизвестно), а поток с прошлого кадра ушёл в прокрутку, то есть зеркало
 *   могло разбогатеть. Один кадр ради hist_lines, не чаще раза за соединение
 *   (решает адаптер). История к нему не применяется: resumed на живой странице
 *   запрещает её (snapshotHistory.noteSyncMarker). Богатое зеркало или поток
 *   без новых строк — прежний warm, 0 запросов (ST-09 A6).
 */
export function causesForMarker(m: {
  marker: "reset" | "resumed";
  gap: boolean;
  epochChanged: boolean;
  termVirgin: boolean;
  firstMarker: boolean;
  histProbe?: boolean;
}): { path: MarkerPath; causes: RecoveryCause[] } {
  if (m.marker === "reset") {
    return m.firstMarker ? { path: "baseline", causes: [] } : { path: "epoch-reset", causes: ["epoch-reset"] };
  }
  if (m.termVirgin) {
    return { path: "cold-restore", causes: m.gap ? ["cold-restore", "stream-gap"] : ["cold-restore"] };
  }
  if (m.epochChanged && !m.firstMarker) return { path: "epoch-reset", causes: ["epoch-reset"] };
  if (m.gap) return { path: "stream-gap", causes: ["stream-gap"] };
  if (m.histProbe) return { path: "hist-probe", causes: ["hist-probe"] };
  return { path: "warm", causes: [] };
}
