/**
 * Правила flow control клиента терминала (ST-09, T-33, T-35).
 *
 * Модуль чистый: ни React, ни DOM, ни часов — время передаёт вызывающий.
 * PtyTermView только связывает: когда просить агента о паузе, какими порциями
 * отдавать очередь склейки в xterm и какие числа копить для замера.
 */

/** Причины паузы. Они независимы: опустошение очереди не снимает паузу, пока
 * страница в фоне, и возврат страницы не снимает её, пока не разобран backlog. */
export interface FlowReasons {
  hidden: boolean;
  backlog: boolean;
  /** Итоговое состояние: пауза нужна, если действует хотя бы одна причина. */
  next: boolean;
}

export interface FlowSample {
  visible: boolean;
  /** Очередь склейки клиента: принято из сокета, ещё не отдано в writer. */
  queuedBytes: number;
  /** Отдано в writer/xterm, но ещё не разобрано. */
  unackedBytes: number;
}

const finiteOrZero = (n: number) => (Number.isFinite(n) ? n : 0);

/**
 * backlog считается по ВСЕЙ очереди клиента: склейка плюс неразобранное в
 * xterm. Раньше в решение шло только второе слагаемое, и склейка молча
 * дорастала до аварийных 2 МиБ: выброс головы, invalidateResume и полный reset
 * на следующем соединении, то есть потеря истории там, где сервер мог встать на
 * паузу и дослать из кольца без потерь (карта ST-09, gap high).
 * Гистерезис прежний: встать при > high, сняться только при < low.
 */
export function flowReasons(paused: boolean, s: FlowSample, high: number, low: number): FlowReasons {
  const total = finiteOrZero(s.queuedBytes) + finiteOrZero(s.unackedBytes);
  const hidden = !s.visible;
  const backlog = paused ? total >= low : total > high;
  return { hidden, backlog, next: hidden || backlog };
}

/** Visibility and parser backpressure share one owner of pause/resume.
 * Прежняя сигнатура (только unacked writer/xterm) — обёртка над flowReasons. */
export function flowTransition(paused: boolean, unacked: number, visible: boolean,
  high: number, low: number): "pause" | "resume" | null {
  const { next } = flowReasons(paused, { visible, queuedBytes: 0, unackedBytes: unacked }, high, low);
  return next === paused ? null : next ? "pause" : "resume";
}

/**
 * Потолок одной записи в xterm. Равен MAX_PARSEBUFFER_LENGTH xterm 6: запись
 * больше него xterm разбирает СИНХРОННО кусками в одной macrotask, а 12 мс
 * проверяет только МЕЖДУ записями (InputHandler.ts:471-482, WriteBuffer.ts:226).
 * Догон после фона (досылка до 2 МиБ одним кадром) без потолка превращался в
 * одну длинную задачу, и жесты с вводом стояли (ST-09). Значение — baseline для
 * замера, а не окончательная цифра.
 */
export const FLUSH_BATCH_MAX_BYTES = 128 * 1024;

/**
 * Сколько агент вправе прислать сверх того, что клиент уже держит, — числа
 * контракта агента (internal/pty/manager.go):
 *  - досылка после паузы или отставания — не больше liveResyncLimit (2 МиБ);
 *  - запас подписчика subQueueBytes (4 МиБ): писатель api_pty.go вычерпывает
 *    канал подписчика, не глядя на паузу (case data := <-ch), поэтому то, что
 *    легло в канал до того, как агент прочёл {t:"pause"}, уходит клиенту уже
 *    ПОСЛЕ паузы. После разпаузы канал снова открыт, пока агент не прочтёт
 *    следующую паузу.
 */
export const AGENT_RESYNC_MAX_BYTES = 2 << 20;
export const AGENT_SUBQ_MAX_BYTES = 4 << 20;

/**
 * Аварийный потолок очереди склейки при flow control по всей очереди (ST-09).
 *
 * Потолок — не рычаг паузы (им служит flowReasons), а защита от агента,
 * который паузы не слышит. Его выброс головы стирает позицию resume, поэтому
 * законный поток нового агента дотягиваться до него не должен. Худший законный
 * случай: очередь стоит у порога паузы (high) и ещё не разобрана, приходит
 * досылка на пределе, а за ней — всё, что агент успел положить в канал до
 * своей следующей паузы. Прежние 2 МиБ были равны одной досылке, и первая же
 * живая пачка после неё выбрасывала голову (возврат из фона при печатающем
 * агенте, замер ревьюера 14.09: 3 из 3). Спуск по FLUSH_BATCH_MAX_BYTES за
 * кадр медленнее прежнего «всё за кадр», так что очередь склейки держит эти
 * байты дольше — запас нужен именно ей. Потолок по-прежнему конечен (I-11).
 */
export function flowQueueCap(high: number): number {
  const h = Number.isFinite(high) && high > 0 ? high : 0;
  return AGENT_RESYNC_MAX_BYTES + AGENT_SUBQ_MAX_BYTES + h + FLUSH_BATCH_MAX_BYTES;
}

/**
 * Страховочный срок спуска очереди склейки рядом с кадром анимации (ST-09,
 * T-33 raf-stall). Корректность не должна зависеть от того, что rAF придёт:
 * WebView, у которого вид отцеплен или перекрыт, кадров не даёт, а
 * visibilitychange не присылает — очередь стояла бы до 2 МиБ и выбрасывала
 * голову. Спускает тот, кто сработал первым; rAF обычно успевает за ~16 мс.
 */
export const FLUSH_SAFETY_MS = 100;

/** Diag flow по переходу паузы или выбросу — не чаще раза в этот срок. */
export const FLOW_DIAG_MIN_MS = 5000;
/** Периодический diag flow на видимой странице — не чаще раза в этот срок. */
export const FLOW_DIAG_PERIOD_MS = 30000;

export interface FlowDiagClock {
  now: number;
  /** Когда diag flow уходил последний раз (0 — ни разу). */
  lastSentAt: number;
  /** Отличаются ли счётчики от последних отправленных (дедупликация). */
  changed: boolean;
  /** Есть неотправленный переход паузы или выброс. */
  pending: boolean;
  visible: boolean;
}

/**
 * Пора ли слать diag flow (ST-09, I-15: только числа). Одинаковые счётчики
 * не повторяются; переход или выброс уходит сразу, но не чаще FLOW_DIAG_MIN_MS
 * (под быстрым потоком пауза может качаться несколько раз в секунду — журнал
 * агента не для этого); без событий — раз в FLOW_DIAG_PERIOD_MS и только на
 * видимой странице.
 */
export function flowDiagDue(c: FlowDiagClock): boolean {
  if (!c.changed) return false;
  const since = c.lastSentAt > 0 ? c.now - c.lastSentAt : Infinity;
  if (c.pending) return since >= FLOW_DIAG_MIN_MS;
  return c.visible && since >= FLOW_DIAG_PERIOD_MS;
}

/** Ключ дедупликации: время паузы — с точностью до секунды. */
export function flowStatsKey(s: FlowStats): string {
  return [s.maxQueued, s.maxUnacked, s.maxBatch, s.pauses.hidden, s.pauses.backlog,
    Math.floor(s.pausedMs / 1000), s.drops, s.dropBytes, Math.round(s.maxFlushMs), s.timerFlushes].join(",");
}

/**
 * Поля diag flow: wire — в сокет (сервер разворачивает pauses в
 * pauses.hidden/pauses.backlog), trace — плоские скаляры для кольца трассы
 * (allowlist не принимает вложенных объектов). Только числа и короткие метки.
 */
export function flowDiagFields(s: FlowStats, state: string, reason: string):
  { wire: Record<string, unknown>; trace: Record<string, unknown> } {
  const common = {
    maxQueued: s.maxQueued, maxUnacked: s.maxUnacked, maxBatch: s.maxBatch,
    pausedMs: Math.round(s.pausedMs), drops: s.drops, dropBytes: s.dropBytes,
    maxFlushMs: Math.round(s.maxFlushMs * 10) / 10, timerFlushes: s.timerFlushes,
  };
  return {
    wire: { state, reason, ...common, pauses: { hidden: s.pauses.hidden, backlog: s.pauses.backlog } },
    trace: { state, reason, ...common, pausesHidden: s.pauses.hidden, pausesBacklog: s.pauses.backlog },
  };
}

export interface FlushGuard { generation: number; epoch: string }
export interface FlushItem { bytes: Uint8Array; streamEnd: number; guard: FlushGuard }

export interface FlushBatch<T> {
  take: T[];
  rest: T[];
  takeBytes: number;
}

const sameGuard = (a: FlushGuard, b: FlushGuard) =>
  a.generation === b.generation && a.epoch === b.epoch;

/**
 * Какую часть очереди склейки отдать в xterm одной записью.
 *
 * - Разные guard (поколение соединения или эпоха писателя) НИКОГДА не
 *   склеиваются: маркер reset может ждать в writer, пока из сокета уже идут
 *   байты следующей эпохи (как в прежнем flushTermWrites).
 * - Сумма взятого не больше maxBytes.
 * - Если первый элемент сам больше maxBytes, он режется: голова subarray(0, max)
 *   получает streamEnd = streamEnd − (len − max), то есть позицию своего
 *   последнего байта в потоке; хвост сохраняет исходный streamEnd. Так
 *   «показано» (appliedOffset) продвигается ровно до разобранного, а не дальше
 *   (I-04). Резать можно на любом байте: парсер xterm держит состояние
 *   UTF-8 и ESC-последовательностей между записями, перенос хвоста ESC[3J
 *   (parserCarry) тоже работает на произвольных границах.
 * - maxBytes < 1 или не число — потолка нет: прежнее поведение «вся серия
 *   одного guard» (режим сравнения для §6).
 */
export function selectFlushBatch<T extends FlushItem>(queue: readonly T[], maxBytes: number): FlushBatch<T> {
  if (queue.length === 0) return { take: [], rest: [], takeBytes: 0 };
  const limit = maxBytes >= 1 ? Math.floor(maxBytes) : Infinity;
  const first = queue[0];
  const firstLen = first.bytes.byteLength;
  if (firstLen > limit) {
    const head: T = { ...first, bytes: first.bytes.subarray(0, limit), streamEnd: first.streamEnd - (firstLen - limit) };
    const tail: T = { ...first, bytes: first.bytes.subarray(limit) };
    return { take: [head], rest: [tail, ...queue.slice(1)], takeBytes: limit };
  }
  let end = 1;
  let total = firstLen;
  while (end < queue.length) {
    const item = queue[end];
    if (!sameGuard(item.guard, first.guard)) break;
    const next = total + item.bytes.byteLength;
    if (next > limit) break;
    total = next;
    end++;
  }
  return { take: queue.slice(0, end), rest: queue.slice(end), takeBytes: total };
}

/**
 * Позиция потока, до которой вывод УЖЕ отдан в xterm (ST-09).
 *
 * TerminalWriter держит в xterm не больше одной записи. При смене поколения
 * соединения всё, что стоит за ней, выбрасывается, а отданную xterm разберёт в
 * любом случае. Значит, что окажется на экране после обрыва, описывает конец
 * последней ОТДАННОЙ записи вывода — не принятое (очередь склейки пропадёт) и
 * не разобранное (отданная запись ещё допишется на экран).
 */
export interface HandedMark { generation: number; epoch: string; end: number }

/** Запись вывода отдана в xterm. В одном guard позиция только растёт; новый
 * guard (поколение или эпоха писателя) начинает отсчёт заново. */
export function noteHanded(prev: HandedMark | null, guard: FlushGuard, end: number): HandedMark | null {
  if (!Number.isFinite(end) || end < 0) return null;
  if (prev && sameGuard(prev, guard)) return prev.end >= end ? prev : { ...prev, end };
  return { generation: guard.generation, epoch: guard.epoch, end };
}

/** Откуда новое соединение продолжает поток. */
export type ReconnectResume =
  /** Неразобранного нет: resume с принятого, как раньше. */
  | { kind: "accepted" }
  /** Неразобранное есть: resume с отданного xterm, сервер дошлёт остальное. */
  | { kind: "handed"; offset: number }
  /** Позиция не описывает экран: без resume, полный reset. */
  | { kind: "reset"; why: "rollback" | "no-epoch" | "no-mark" | "stale-mark" };

export interface ReconnectResumeInput {
  flowBacklog: boolean;
  /** В очереди склейки или во writer остались записи. */
  unapplied: boolean;
  /** Эпоха потока известна (позицию не сбросили выброс головы или откат). */
  hasEpoch: boolean;
  /** Логически текущие поколение соединения и эпоха писателя — до смены поколения. */
  current: FlushGuard;
  handed: HandedMark | null;
  /** Разобранное xterm (appliedOffset) в той же шкале. */
  applied: number;
  /** Принятое из сокета (offsetRef) в той же шкале — нужно правилу хвоста. */
  accepted?: number;
  /**
   * Сколько байт удержано под текущим guard как возможное начало ESC[3J
   * (parserCarry, 0–3). Они приняты, но xterm их не получил.
   */
  heldTail?: number;
}

/**
 * Позиция resume на новом соединении (ST-09).
 *
 * Прежнее правило: осталось неразобранное — позиция считается враньём, сокет
 * уходит без resume, сервер отвечает reset, RIS стирает прокрутку. С потолком
 * записи 128 КиБ очередь склейки держит принятое дольше (2 МиБ — 16 кадров, в
 * фоне — 128 КиБ за тик), и мгновенное переподключение (online при смене сети,
 * возврат вкладки) почти всегда заставало её непустой: resume=нет 3 из 3
 * (замер ревьюера 14.09). Но выброшенное connect() — это ровно то, что сервер
 * дошлёт из кольца после отданной позиции: дыры нет, если продолжить с неё.
 *
 * Отметка годится, только если она из того же поколения и той же эпохи
 * писателя: маркер, чья граница (RIS, режимы) ещё не отдана xterm, делает её
 * чужой — тогда прежний reset. applied может обогнать отметку (отложенное
 * стирание в конце цепочки разобрано без записи) — берётся большее.
 * flowBacklog=false — прежнее правило байт в байт.
 *
 * Хвост (ST-05, хвост отчёта «тёплый resume»). Неразобранного нет, но под
 * текущим guard удержано 1–3 байта возможного начала ESC[3J: они приняты
 * (входят в accepted), xterm их не видел, а connect() выбрасывает их вместе с
 * parserCarry. Resume с accepted терял их навсегда: разрезанная
 * последовательность (ESC | [31m…) печаталась текстом «[31m». Resume на хвост
 * раньше — сервер дошлёт эти байты из кольца сам, и они склеятся с
 * продолжением уже в новом соединении.
 */
export function reconnectResume(s: ReconnectResumeInput): ReconnectResume {
  if (!s.unapplied) {
    const tail = Number.isFinite(s.heldTail) && s.heldTail! > 0 ? Math.trunc(s.heldTail!) : 0;
    const accepted = Number.isFinite(s.accepted) ? s.accepted! : -1;
    if (s.flowBacklog && s.hasEpoch && tail > 0 && accepted >= tail) {
      return { kind: "handed", offset: accepted - tail };
    }
    return { kind: "accepted" };
  }
  if (!s.flowBacklog) return { kind: "reset", why: "rollback" };
  if (!s.hasEpoch) return { kind: "reset", why: "no-epoch" };
  if (!s.handed) return { kind: "reset", why: "no-mark" };
  if (!sameGuard(s.handed, s.current)) return { kind: "reset", why: "stale-mark" };
  const applied = Number.isFinite(s.applied) && s.applied > 0 ? s.applied : 0;
  return { kind: "handed", offset: Math.max(s.handed.end, applied) };
}

export interface MarkerTailInput {
  flowBacklog: boolean;
  /** Вид маркера синхронизации: "reset" | "resumed". */
  marker: string;
  /** resumed с пропуском (сервер не смог дослать середину). */
  gap: boolean;
  /** Маркер назвал другую эпоху потока, чем была до него. */
  epochChanged: boolean;
  /** База маркера (msg.offset) — как пришла, без проверки типа. */
  markerOffset: unknown;
  /** Принятое из сокета ДО маркера (offsetRef) в той же шкале. */
  accepted: number;
}

/**
 * Удержанный хвост при маркере на ТОМ ЖЕ сокете (ST-05, находка скептика ST-10).
 *
 * Сервер шлёт `resumed` без пропуска и посреди соединения отстающему зрителю
 * (resync, api_pty.go decideResyncMarker, T-35): полезная нагрузка продолжает
 * поток ровно с `sent`, то есть с принятого клиентом. Удержанные 1–3 байта
 * возможного начала ESC[3J (parserCarry) — начало этой же последовательности;
 * выброшенные, они превращали продолжение в текст («[31m») или съедали
 * маркер OSC 133 D (блок оставался открытым навсегда). Исправление connect()
 * (reconnectResume, heldTail) этот путь не покрывало.
 *
 * Переносить хвост в новую эпоху писателя можно, только если поток
 * непрерывен: resumed, без gap, та же эпоха сервера и база маркера равна
 * принятой позиции. reset, gap, другая эпоха или иная база — прежнее правило:
 * хвост принадлежит старой эпохе и выбрасывается. flowBacklog=false — прежнее
 * поведение байт в байт (откат).
 */
export function carryTailAcrossMarker(s: MarkerTailInput): boolean {
  if (!s.flowBacklog || s.marker !== "resumed" || s.gap || s.epochChanged) return false;
  if (typeof s.markerOffset !== "number" || !Number.isFinite(s.markerOffset) || !Number.isFinite(s.accepted)) return false;
  return s.markerOffset === s.accepted;
}

/** Счётчики стадий очередей (ST-09, §10). Только числа, без содержимого (I-15). */
export interface FlowStats {
  maxQueued: number;
  maxUnacked: number;
  maxBatch: number;
  /** Сколько раз включалась каждая причина паузы (передний фронт). */
  pauses: { hidden: number; backlog: number };
  /** Суммарное время в паузе, включая текущую незавершённую. */
  pausedMs: number;
  /** Аварийные выбросы головы очереди склейки. */
  drops: number;
  dropBytes: number;
  /** Самая долгая запись: от отдачи в xterm до callback разбора. */
  maxFlushMs: number;
  /** Спуски, сделанные страховочным таймером на ВИДИМОЙ странице: кадр
   * анимации не пришёл за FLUSH_SAFETY_MS (T-33 raf-stall). */
  timerFlushes: number;
}

export function emptyFlowStats(): FlowStats {
  return { maxQueued: 0, maxUnacked: 0, maxBatch: 0, pauses: { hidden: 0, backlog: 0 },
    pausedMs: 0, drops: 0, dropBytes: 0, maxFlushMs: 0, timerFlushes: 0 };
}

const positive = (n: number) => (Number.isFinite(n) && n > 0 ? n : 0);

/**
 * Чистый аккумулятор FlowStats: без часов и побочных эффектов, время приходит
 * аргументом. Замер нужен раньше смены порогов 256/64 КиБ: сначала числа
 * стадий, потом решения (ST-09 «сначала измерить»).
 */
export class FlowStatsAccumulator {
  private stats = emptyFlowStats();
  private pausedSince: number | null = null;
  private last = { hidden: false, backlog: false };

  noteQueues(queuedBytes: number, unackedBytes: number): void {
    this.stats.maxQueued = Math.max(this.stats.maxQueued, positive(queuedBytes));
    this.stats.maxUnacked = Math.max(this.stats.maxUnacked, positive(unackedBytes));
  }

  noteBatch(bytes: number): void {
    this.stats.maxBatch = Math.max(this.stats.maxBatch, positive(bytes));
  }

  noteFlush(ms: number): void {
    this.stats.maxFlushMs = Math.max(this.stats.maxFlushMs, positive(ms));
  }

  noteDrop(bytes: number): void {
    this.stats.drops++;
    this.stats.dropBytes += positive(bytes);
  }

  noteTimerFlush(): void {
    this.stats.timerFlushes++;
  }

  /** Решение flowReasons в момент now. Причины считаются по переднему фронту
   * независимо друг от друга; время паузы — пока действует хотя бы одна. */
  noteReasons(r: FlowReasons, now: number): void {
    if (r.hidden && !this.last.hidden) this.stats.pauses.hidden++;
    if (r.backlog && !this.last.backlog) this.stats.pauses.backlog++;
    this.last = { hidden: r.hidden, backlog: r.backlog };
    if (r.next && this.pausedSince === null) this.pausedSince = now;
    else if (!r.next && this.pausedSince !== null) {
      this.stats.pausedMs += positive(now - this.pausedSince);
      this.pausedSince = null;
    }
  }

  snapshot(now: number): FlowStats {
    const open = this.pausedSince === null ? 0 : positive(now - this.pausedSince);
    return { ...this.stats, pauses: { ...this.stats.pauses }, pausedMs: this.stats.pausedMs + open };
  }

  /** Новое соединение: пауза снята сервером вместе с сокетом. */
  reset(): void {
    this.stats = emptyFlowStats();
    this.pausedSince = null;
    this.last = { hidden: false, backlog: false };
  }
}
