/**
 * ЕДИНАЯ МОДЕЛЬ СВИДЕТЕЛЬСТВ НАВИГАЦИИ «Авто» (план стабилизации 13.09, ST-02).
 *
 * До этого у экрана было два разных алгоритма: живые `pageAnswered`/
 * `wheelAnswered` в памяти компонента и сохранённая запись `scrollProbeMemory`.
 * Сохранённая запись устаревала (срок, новый вывод), а живой `false` — никогда:
 * `restoreProbeVerdict` заполнял только пустое значение. Аудит 13.09, A01: после
 * отрицательной пробы тот же процесс снова отвечает на PgUp, ручной «Агент»
 * листает, а «Авто» до переоткрытия терминала шлёт жест в локальную историю.
 *
 * Здесь одно правило для обоих мест. Хранилище — только сериализация этого же
 * состояния (см. scrollProbeMemory.ts), а не второй алгоритм.
 *
 * Термины:
 *   • capability — объявленный адаптером способ навигации (реестр компьютера,
 *     `declaredScrollChannel`); здесь не хранится, приходит снаружи;
 *   • observation — последнее наблюдение по каналу в конкретной области действия:
 *       confirmed   — ответ доказан сдвигом строк в сторону намерения;
 *       weak        — был большой repaint (сколько угодно раз подряд): действие
 *                     считаем доставленным, но способность НЕ доказанной (A03,
 *                     T-06: независимая перерисовка во время пробы выглядит так
 *                     же, как ответ страницы, а у постоянно перерисовывающего
 *                     TUI совпадает в каждом окне);
 *       unconfirmed — экран не ответил в окне пробы. Это «не ответил сейчас»,
 *                     а не «не умеет»: timeout ≠ unsupported.
 *
 * Правила актуальности проверяются В МОМЕНТ РЕШЕНИЯ, а не только при чтении из
 * хранилища — поэтому открытый экран больше не держит устаревший `false`.
 */

export type NavChannel = "page" | "wheel";
export type ObservationState = "confirmed" | "weak" | "unconfirmed";

/** Бит 1 — вверх, бит 2 — вниз (как прежние *DeadDirections). */
export const DIR_UP = 1;
export const DIR_DOWN = 2;
export const directionOf = (lines: number): number => (lines < 0 ? DIR_UP : DIR_DOWN);

export interface Observation {
  readonly state: ObservationState;
  /** Для unconfirmed: в каких направлениях канал промолчал. */
  readonly deadDirections: number;
  /** Монотонное время записи наблюдения (performance.now в клиенте). */
  readonly at: number;
  /** Сколько проб подряд закончились молчанием — основание для отсрочки. */
  readonly silentProbes: number;
  /** Сколько ответов подряд было (для журнала; доказательство — только сдвиг или ответ из тишины). */
  readonly answers: number;
  /**
   * Сколько молчаний подряд после ответов канала: у края истории приложения
   * законная PgUp «вхолостую» — это край, а не смерть канала (ревью 14.09).
   */
  readonly edgeSilences?: number;
  /**
   * Слабое наблюдение, все ответы которого — сдвиг к НОВОМУ тексту на жест ↓.
   * Ровно так выглядит обычный новый вывод приложения: для ↓ канал считается
   * доставленным (verify), а про ↑ такое наблюдение не говорит ничего — ни
   * режима verify, ни «края истории» для молчания ↑ (волна 8: вывод во время
   * PgDn-пробы у низа съедал три следующих ↑). Только в памяти открытого
   * экрана: слабое наблюдение не сохраняется (scrollProbeMemory).
   */
  readonly outputLike?: boolean;
  /**
   * Строки экрана сразу после наблюдения и индексы строк, которые менялись во
   * время пробы БЕЗ ответа (спиннер, часы). Только в памяти открытого экрана:
   * текст терминала в хранилище и диагностику не попадает (I-15).
   */
  readonly anchor?: readonly string[];
  readonly volatileRows?: readonly number[];
}

/** Подтверждённый ответ помнится как факт; срок — страховка. */
export const CONFIRMED_TTL_MS = 6 * 60 * 60 * 1000;
/** Слабое наблюдение не переживает контекста чтения. */
export const WEAK_TTL_MS = 2 * 60 * 1000;
/**
 * Первая отсрочка повторной проверки после молчания; дальше — вдвое.
 *
 * ⚠ Отсрочка — страховка, а не главный путь. Главный путь — значимый новый
 * вывод (см. meaningfulChangeSince). Короткая отсрочка вернула бы боевую
 * жалобу 06.09.2026: терминал, открытый повторно через несколько секунд,
 * снова начинался бы с пробы в 1,2 с (probe-scroll-verdict-memory-live, мир B).
 * У восстановленного из памяти наблюдения нет якоря экрана (текст терминала не
 * хранится), и оно устаревает только отсрочкой или сроком.
 */
export const SILENT_COOLDOWN_BASE_MS = 30_000;
export const SILENT_COOLDOWN_MAX_MS = 10 * 60_000;
/**
 * Значимый новый вывод разрешает повторную проверку раньше отсрочки, но не
 * чаще этого интервала (растёт с каждым молчанием подряд): иначе серия жестов
 * при оживающем или шумящем агенте превращалась бы в серию одинаковых ожиданий.
 */
export const MIN_REPROBE_MS = 1000;
/**
 * Сколько молчаний подряд после ответов считаем краем истории приложения. Дальше
 * канал признаётся сейчас молчащим: вечно слать в пустоту тоже нельзя.
 */
export const EDGE_SILENCE_LIMIT = 2;
/**
 * Доказанный канал раз в этот срок отправляет с наблюдением итога: приложение
 * могло смениться внутри той же области (агент обновился), и доказательство без
 * перепроверки жило бы до конца срока (ревью 14.09).
 */
export const REVERIFY_MS = 60_000;

/**
 * Досмотр позднего ответа приложения (план 13.09, T-08; I-01).
 *
 * План задаёт задержку ответа 1,5–3 с — это задержка САМОГО приложения. До
 * экрана ответ идёт ещё через релей и разбор записи, поэтому досматриваем до
 * верхнего края плана ПЛЮС запас доставки, и не одной проверкой в конце, а
 * шагами: ответ, пришедший раньше края, засчитывается сразу.
 *
 * ⚠ До волны 7 проверка была одна — ровно через 3,0 с после отправки (окно
 * пробы 1,2 с + 1,8 с). Ответ приложения на 3,0 с плюс доставка приходил ПОСЛЕ
 * неё (probe-scroll-late-answer-live, 3000 мс, 3 из 3): поздний ответ не
 * становился свидетельством, и следующий ↑ снова пробовал живой канал — второе
 * «страница не ответила».
 *
 * Засчитывается по-прежнему только сдвиг строк в сторону намерения
 * (screenScrollResponse): большой repaint позже окна ничего не доказывает (A03).
 */
export const LATE_ANSWER_MS = 3000;
/** Запас на доставку ответа сверх задержки приложения: релей туда-обратно, разбор записи. */
export const LATE_ANSWER_DELIVERY_MS = 1000;
/** Шаг досмотра: проверка — сравнение строк экрана, дешёвая, но не на каждый кадр. */
export const LATE_ANSWER_STEP_MS = 150;

/**
 * Через сколько мс следующая проверка позднего ответа на действие, отправленное
 * в `sentAt` (монотонные часы); null — досмотр окончен. `legacy` — прежний
 * досмотр (переключатель navigation ≠ "v2"): одна проверка в `sentAt + 3 с`, в
 * обе стороны. `direction` — направление намерения (DIR_UP/DIR_DOWN).
 *
 * ⚠ Волна 8 (скептик волны 7, verify:nav [1]): штатно досматривается только ↑.
 * На ↓ (PgDn) сдвиг строк к новому тексту — ровно то, как выглядит обычный
 * новый вывод приложения; доказательством он не бывает (observeOutcome), а
 * слабое наблюдение давало ↑ режим verify: вывод через 2,0 или 3,3 с после
 * молчаливой PgDn — и три следующих ↑ уходили PgUp в молчащее приложение, своя
 * история не листалась, в журнале ложный alt-scroll-late-answer. Для ↑ новый
 * вывод — сдвиг ПРОТИВ намерения, и досмотр его не засчитывает.
 */
export function nextLateAnswerCheck(sentAt: number, now: number, legacy = false, direction = DIR_UP): number | null {
  // Часы назад или мусор на входе: досмотр кончается, а не крутится вечно.
  if (!Number.isFinite(sentAt) || !Number.isFinite(now) || now < sentAt) return null;
  if (!legacy && direction !== DIR_UP) return null;
  const deadline = sentAt + LATE_ANSWER_MS + (legacy ? 0 : LATE_ANSWER_DELIVERY_MS);
  if (now >= deadline) return null;
  return legacy ? deadline - now : Math.min(LATE_ANSWER_STEP_MS, deadline - now);
}

export function silentCooldownMs(silentProbes: number): number {
  const n = Math.max(1, Math.min(16, Math.trunc(silentProbes)));
  return Math.min(SILENT_COOLDOWN_MAX_MS, SILENT_COOLDOWN_BASE_MS * 2 ** (n - 1));
}

export function minReprobeMs(silentProbes: number): number {
  const n = Math.max(1, Math.min(16, Math.trunc(silentProbes)));
  return Math.min(silentCooldownMs(n), MIN_REPROBE_MS * 2 ** (n - 1));
}

/** Как понимать ответ экрана на отправленное действие. */
export type ResponseKind = "shift" | "repaint" | "none";

export interface ProbeOutcome {
  readonly kind: ResponseKind;
  /** Точное краевое действие (Ctrl+Home/End): молчание у края ничего не доказывает. */
  readonly edge: boolean;
  readonly direction: number;
  readonly before: readonly string[];
  readonly after: readonly string[];
  /**
   * Приложение молчало перед отправкой (≥ 1 с без вывода). Большой repaint в
   * ответ на действие из тишины — ответ страницы; тот же repaint у постоянно
   * перерисовывающего TUI — совпадение (T-06).
   */
  readonly quietBefore?: boolean;
}

/** Строки, различающиеся между двумя снимками одинаковой высоты. */
export function changedRowIndexes(a: readonly string[], b: readonly string[]): number[] {
  if (a.length !== b.length) return [];
  const out: number[] = [];
  for (let i = 0; i < a.length; i++) if (a[i] !== b[i]) out.push(i);
  return out;
}

/**
 * Новое наблюдение по итогу отправленного действия. Возвращает null, если итог
 * ничего не меняет (молчание на точном краевом действии).
 */
export function observeOutcome(prev: Observation | null, outcome: ProbeOutcome, now: number): Observation | null {
  const answeredBefore = prev?.state === "confirmed" || prev?.state === "weak";
  if (outcome.kind === "shift" || outcome.kind === "repaint") {
    const answers = answeredBefore ? prev!.answers + 1 : 1;
    // Доказывают канал только два вида ответа:
    //   • сдвиг строк к СТАРОМУ тексту (намерение «вверх»). Сдвиг к новому при
    //     жесте вниз выглядит ровно как новый вывод приложения — не доказательство;
    //   • большой repaint на действие «вверх», отправленное в тишине. Полная
    //     страница PgUp оставляет одну общую строку и сдвигом не распознаётся, а
    //     repaint у постоянно перерисовывающего TUI (стрим ответа, watch, htop)
    //     случается в каждом окне сам — поэтому без тишины он только слабый (T-06).
    const proves = outcome.direction === DIR_UP
      && (outcome.kind === "shift" || outcome.quietBefore === true);
    const state: ObservationState = prev?.state === "confirmed" || proves ? "confirmed" : "weak";
    // Сдвиг к новому на ↓ выглядит ровно как новый вывод: такое слабое
    // наблюдение — только для ↓ (outputLike). Любой другой ответ оговорку
    // снимает, а к уже настоящему ответу она не прибавляется (волна 8).
    const outputLike = state === "weak" && outcome.kind === "shift" && outcome.direction === DIR_DOWN
      && (!answeredBefore || prev!.outputLike === true);
    return { state, deadDirections: 0, at: now, silentProbes: 0, answers, edgeSilences: 0, ...(outputLike ? { outputLike } : {}) };
  }
  if (outcome.edge) return null;
  // Молчание сразу после ответов — скорее край истории приложения (PgUp у самого
  // начала разговора законно ничего не меняет), чем смерть канала. Несколько
  // таких подряд — канал признаётся сейчас молчащим. Ответы, похожие на вывод
  // (outputLike), про ↑ не говорят ничего: молчание ↑ после них — молчание
  // канала, а не край (иначе ещё два ↑ уходили бы PgUp в пустоту).
  const answeredThisWay = answeredBefore && !(prev!.outputLike === true && outcome.direction === DIR_UP);
  if (answeredThisWay && (prev!.edgeSilences ?? 0) < EDGE_SILENCE_LIMIT) {
    return { ...prev!, edgeSilences: (prev!.edgeSilences ?? 0) + 1 };
  }
  const again = prev?.state === "unconfirmed";
  const silentProbes = again ? prev!.silentProbes + 1 : 1;
  const dead = (again ? prev!.deadDirections : 0) | outcome.direction;
  // Строки, дёргавшиеся во время прошлых молчаливых проб, остаются шумом:
  // часы, тикающие реже окна пробы, не должны раз за разом выдаваться за новый
  // вывод и будить проверку.
  const volatile = new Set([...(again ? prev!.volatileRows ?? [] : []), ...changedRowIndexes(outcome.before, outcome.after)]);
  return {
    state: "unconfirmed",
    deadDirections: dead,
    at: now,
    silentProbes,
    answers: 0,
    anchor: outcome.after,
    volatileRows: [...volatile].sort((a, b) => a - b),
  };
}

/** Что делать с этим каналом для действия в заданном направлении. */
export type EvidenceDecision =
  /** Слать без ожидания и без наблюдения: канал доказан. */
  | "use"
  /**
   * Слать без ожидания, но посмотреть на итог: канал слабо подтверждён одним
   * repaint (A03). Молчание переведёт его в unconfirmed для СЛЕДУЮЩЕГО
   * намерения; уже отправленное локально не повторяется (I-01).
   */
  | "verify"
  /** Канал сейчас не отвечает: выбрать локальный путь ДО отправки. */
  | "local"
  /** Свидетельства нет или оно устарело: допустима одна ограниченная проба. */
  | "probe";

export interface EvaluateInput {
  readonly now: number;
  readonly direction: number;
  /** Текущие строки экрана — для обнаружения значимого нового вывода. */
  readonly rows?: readonly string[];
}

/**
 * Значимый вывод после наблюдения: изменилась строка, которая НЕ дёргалась во
 * время пробы. Пять строк спиннера меняются всегда — это не новое состояние
 * приложения; новая строка текста вне их — да.
 */
export function meaningfulChangeSince(obs: Observation, rows: readonly string[] | undefined): boolean {
  if (!obs.anchor || !rows || rows.length !== obs.anchor.length) return false;
  const volatile = new Set(obs.volatileRows ?? []);
  for (let i = 0; i < rows.length; i++) {
    if (!volatile.has(i) && rows[i] !== obs.anchor[i]) return true;
  }
  return false;
}

export function evaluateObservation(obs: Observation | null, input: EvaluateInput): EvidenceDecision {
  if (!obs) return "probe";
  // Часы шли назад (монотонность нарушена снаружи) — наблюдению не верим, но
  // и лавину проб не устраиваем: одна проба, как на пустом месте.
  if (input.now < obs.at) return "probe";
  const age = input.now - obs.at;
  if (obs.state === "confirmed") return age > CONFIRMED_TTL_MS ? "probe" : age > REVERIFY_MS ? "verify" : "use";
  if (obs.state === "weak") {
    if (age > WEAK_TTL_MS) return "probe";
    // Слабое, похожее на вывод (сдвиг к новому на ↓), про ↑ ничего не говорит:
    // для ↑ свидетельства нет — одна ограниченная проба, а не verify (волна 8).
    return obs.outputLike === true && input.direction === DIR_UP ? "probe" : "verify";
  }
  // unconfirmed: молчание в одном направлении ничего не говорит о другом.
  if ((obs.deadDirections & input.direction) === 0) return "probe";
  if (age >= silentCooldownMs(obs.silentProbes)) return "probe";
  if (age >= minReprobeMs(obs.silentProbes) && meaningfulChangeSince(obs, input.rows)) return "probe";
  return "local";
}

/**
 * Проекция наблюдения на прежний трёхзначный флаг (`true`/`false`/`null`),
 * который читают чистые правила выбора канала (altScroll.ts). `null` означает
 * «можно пробовать», `false` — «сейчас не отвечает», `true` — «доказан».
 */
export function answeredView(obs: Observation | null, input: EvaluateInput): { answered: boolean | null; dead: number } {
  const decision = evaluateObservation(obs, input);
  if (decision === "use" || decision === "verify") return { answered: true, dead: 0 };
  if (decision === "local") return { answered: false, dead: obs?.deadDirections ?? 0 };
  return { answered: null, dead: 0 };
}

/**
 * Живое хранилище наблюдений одного экрана. Область действия (компьютер,
 * терминал, поколение процесса, режим буфера и мыши, объявленный канал
 * адаптера) задаёт вызывающий: при её смене хранилище очищается.
 */
export class NavigationEvidence {
  private scope = "";
  private readonly byChannel = new Map<NavChannel, Observation>();

  /** Новая область действия стирает всё прежнее; та же — ничего не меняет. */
  setScope(scope: string): boolean {
    if (scope === this.scope) return false;
    this.scope = scope;
    this.byChannel.clear();
    return true;
  }
  currentScope(): string { return this.scope; }
  clear(): void { this.byChannel.clear(); }
  /**
   * Новый экран той же области (маркер reset): якорь и слабые/молчаливые
   * наблюдения принадлежали прежнему экрану, а доказанный канал — факт о
   * процессе и остаётся (иначе при отказавшем хранилище каждый reset заново
   * пробовал бы доказанный канал — T-03).
   */
  forgetScreen(): void {
    for (const [channel, obs] of [...this.byChannel]) {
      if (obs.state === "confirmed") {
        this.byChannel.set(channel, { ...obs, anchor: undefined, volatileRows: undefined, edgeSilences: 0 });
      } else {
        this.byChannel.delete(channel);
      }
    }
  }
  get(channel: NavChannel): Observation | null { return this.byChannel.get(channel) ?? null; }

  evaluate(channel: NavChannel, input: EvaluateInput): EvidenceDecision {
    return evaluateObservation(this.get(channel), input);
  }

  record(channel: NavChannel, outcome: ProbeOutcome, now: number): Observation | null {
    const next = observeOutcome(this.get(channel), outcome, now);
    if (next) this.byChannel.set(channel, next);
    return next;
  }

  /**
   * Наблюдение из хранилища. Живое наблюдение того же канала всегда новее и
   * дороже: отказ или старость хранилища не уничтожают хорошее живое состояние.
   */
  restore(channel: NavChannel, obs: Observation): boolean {
    if (this.byChannel.has(channel)) return false;
    this.byChannel.set(channel, obs);
    return true;
  }
}

/**
 * Версия адаптера навигации клиента (план ST-02: «версия адаптера» в области
 * свидетельства). Меняется, когда меняются правила, по которым наблюдение
 * записано и толкуется: наблюдение прежнего адаптера — чужое.
 */
export const NAVIGATION_ADAPTER_VERSION = "nav2";

/**
 * Область действия свидетельства (план ST-02): процесс и его поколение,
 * streamEpoch, режим буфера и мыши, объявленный реестром канал и версия
 * адаптера. Приложение — agent_kind в ключе процесса; ВЕРСИИ приложения агент
 * не сообщает, её роль играет поколение (fg_started): обновлённое приложение —
 * новый процесс. "" — области ещё нет (процесс не назван или эпоха потока не
 * известна до первого маркера): тогда свидетельства не сверяются и не
 * вспоминаются. Тёплое переподключение (resumed в ту же эпоху) даёт ТУ ЖЕ
 * область — свидетельства не сбрасываются; новая эпоха — новая область.
 */
export function evidenceScopeKey(s: {
  process: string; generation: number; epoch: string; buffer: string; mouse: string; channel: string;
}): string {
  if (!s.process || !s.epoch) return "";
  const generation = s.generation > 0 ? `@${s.generation}` : "";
  return JSON.stringify([`${s.process}${generation}`, s.epoch, s.buffer, s.mouse, s.channel, NAVIGATION_ADAPTER_VERSION]);
}
