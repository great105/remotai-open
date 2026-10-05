/**
 * ОДНО ПРАВИЛО «КТО ИСПОЛНЯЕТ ЖЕСТ ПРОКРУТКИ» (план стабилизации 13.09, ST-02/ST-03).
 *
 * До этого решение принималось в два приёма: сначала `shouldScrollViewport`
 * решал «крутить ли своё» (по замеру плотности потока), и только ПОТОМ
 * `pickChannel` смотрел на объявленный реестром канал. Ранняя локальная ветка
 * обходила объявленный канал: у Claude Code (реестр: PgUp) при плотности
 * 32 строки на 16 КиБ жест уходил в обрывки перерисовок, при 31 — в PgUp
 * (аудит 13.09, A02; разбор C-серии). Здесь приоритет вычисляется ДО любого
 * побочного эффекта и в одном месте:
 *
 *   1. явный выбор человека («Вывод»/«Агент»);
 *   2. закреплённый источник чтения (ST-03): человек уже читает старый текст —
 *      новый ответ и изменение плотности не переключают ему источник;
 *   3. проверенный адаптер (канал из реестра компьютера);
 *   4. подходящее актуальное наблюдение (navigationEvidence.ts);
 *   5. безопасное действие с доступным текстом: своя история, а в приложение —
 *      одна ограниченная проба, если замер говорит, что история у него.
 *
 * Правило чистое: ни xterm, ни DOM, ни сети — проверяется тестом в node.
 *
 * ⚠ ЧЕГО ЗДЕСЬ НЕТ И НЕ БУДЕТ:
 *   • стрелок агенту — у чат-агента ↑/↓ подменяют набранный запрос (altScroll.ts);
 *   • перехода «край своей истории → команда приложению» (T-10): край — это край;
 *   • изобретения клавиши под ручной «Агент»: ручной выбор выбирает исполнителя,
 *     а не разрешает неподдержанный канал (у Codex это переход в транскрипт).
 */
import type { AltScrollChannel, HistoryOwner } from "./altScroll";
import type { EvidenceDecision } from "./navigationEvidence";

export type NavOverride = "auto" | "terminal" | "agent";
export type DeclaredChannel = "page" | "wheel" | "transcript" | "";

/** Кто исполняет намерение: своя история терминала, приложение или никто. */
export type NavExecutor = "local" | "remote" | "none";

/**
 * Как отправлять удалённое действие:
 *   • send   — сразу, без наблюдения (канал доказан или выбран человеком);
 *   • verify — сразу, с наблюдением итога без задержки остальных действий;
 *   • probe  — одна проба: следующие действия ждут вердикта в ограниченной очереди.
 */
export type NavMode = "send" | "verify" | "probe";

export type NavReason =
  | "override-terminal"
  | "override-agent"
  | "toward-live"
  | "pinned"
  | "declared"
  | "declared-transcript"
  | "evidence"
  | "probe"
  | "channel-silent"
  | "safe-local"
  | "shell"
  | "pager"
  | "no-channel"
  /** Прежнее правило (legacyNavigation.ts) — только переключатель отката. */
  | "legacy";

/** Закреплённый источник чтения: кем листали, пока человек не вернулся к live. */
export interface ReadingPin {
  readonly executor: "local" | "remote";
  readonly channel: AltScrollChannel;
}

export interface NavigationInput {
  readonly override: NavOverride;
  readonly pin: ReadingPin | null;
  readonly alt: boolean;
  /** `mouseTrackingMode` из xterm: none | x10 | vt200 | drag | any. */
  readonly mouse: string;
  /** На переднем плане AI-агент. */
  readonly agent: boolean;
  /** Канал из реестра компьютера для этого агента (пусто — не знаем). */
  readonly declaredChannel: DeclaredChannel;
  /** Может ли своя история сдвинуться в направлении жеста прямо сейчас. */
  readonly localCanScroll: boolean;
  /** Жест к новому выводу (вниз). */
  readonly towardLive?: boolean;
  /**
   * Слабая эвристика «чья история» (замер плотности с гистерезисом + глубина
   * своей истории). Решает ТОЛЬКО там, где нет ни объявления, ни наблюдения.
   */
  readonly owner: HistoryOwner;
  /** Свидетельства каналов для направления жеста (navigationEvidence.ts). */
  readonly page: EvidenceDecision;
  readonly wheel: EvidenceDecision;
}

export interface NavigationDecision {
  readonly executor: NavExecutor;
  /** viewport для local, wheel/page/arrows для remote, none — сказать правду. */
  readonly channel: AltScrollChannel;
  readonly mode: NavMode;
  readonly reason: NavReason;
}

const local = (reason: NavReason): NavigationDecision =>
  ({ executor: "local", channel: "viewport", mode: "send", reason });
const none = (reason: NavReason): NavigationDecision =>
  ({ executor: "none", channel: "none", mode: "send", reason });
const modeOf = (ev: EvidenceDecision): NavMode =>
  ev === "use" ? "send" : ev === "verify" ? "verify" : "probe";
const remote = (channel: AltScrollChannel, ev: EvidenceDecision, reason: NavReason): NavigationDecision =>
  ({ executor: "remote", channel, mode: modeOf(ev), reason });

const mouseOn = (mouse: string) => mouse !== "none" && mouse !== "";

/** Канал, которым ручной «Агент» листает приложение, без проб и без догадок. */
function forcedChannel(s: NavigationInput): AltScrollChannel {
  if (s.agent && (s.declaredChannel === "page" || s.declaredChannel === "wheel")) return s.declaredChannel;
  if (s.alt && mouseOn(s.mouse)) return "wheel";
  if (s.agent) return "page";
  if (s.alt) return "arrows";
  return "none";
}

export function decideNavigation(s: NavigationInput): NavigationDecision {
  // ── 1. Явный выбор человека ──────────────────────────────────────────────
  if (s.override === "terminal") return local("override-terminal");
  if (s.override === "agent") {
    // Транскрипт Codex — переход в другой вид, а не прокрутка. Ручной «Агент»
    // не даёт права молча нажать за человека Ctrl+T.
    if (s.agent && s.declaredChannel === "transcript") return none("declared-transcript");
    const channel = forcedChannel(s);
    return channel === "none" ? none("no-channel")
      : { executor: "remote", channel, mode: "send", reason: "override-agent" };
  }

  // ── 1б. Возврат к новому своей историей ──────────────────────────────────
  // Человек читает свою историю выше низа и ведёт вниз: это возврат к live по
  // тому, что он видит, а не PgDn приложению (регресс 2.66.6: после ↑ по своей
  // истории вниз выбиралось «ничего»). Выше закрепления: удалённое закрепление
  // не должно утащить вниз невидимую сейчас историю приложения.
  if (s.towardLive && s.localCanScroll) return local("toward-live");

  // ── 2. Закреплённый источник чтения (только «Авто») ──────────────────────
  if (s.pin) {
    // Край своей истории остаётся краем: жест не превращается в команду агенту.
    if (s.pin.executor === "local") return local("pinned");
    if (s.pin.channel === "page" || s.pin.channel === "wheel") {
      const ev = s.pin.channel === "page" ? s.page : s.wheel;
      // Закреплённый канал замолчал — закрепление больше не держит: решаем
      // заново (вызывающий снимает закрепление, см. shouldDropPin).
      if (ev !== "local") return remote(s.pin.channel, ev, "pinned");
    } else if (s.pin.channel === "arrows") {
      return { executor: "remote", channel: "arrows", mode: "send", reason: "pinned" };
    }
  }

  // ── 3. Проверенный адаптер из реестра ────────────────────────────────────
  if (s.agent && s.declaredChannel) {
    if (s.declaredChannel === "transcript") {
      // Своя история — единственное, что здесь листается жестом.
      return s.localCanScroll ? local("declared-transcript") : none("declared-transcript");
    }
    const ev = s.declaredChannel === "page" ? s.page : s.wheel;
    // Проверенный адаптер используется сразу (ST-03: «без обязательного
    // пробного путешествия»): без свидетельства — отправка с наблюдением итога,
    // а не проба, придерживающая следующие фрагменты жеста.
    if (ev !== "local") return remote(s.declaredChannel, ev === "probe" ? "verify" : ev, "declared");
    return s.localCanScroll ? local("channel-silent") : none("channel-silent");
  }

  // ── 4–5. Наблюдения и безопасное действие ────────────────────────────────
  if (s.alt) {
    // У alt-screen своей истории нет; сюда попадаем только при странном буфере.
    if (s.localCanScroll) return local("safe-local");
    // Колесо — правильный канал для vim/htop/less, подменять его страницами нельзя.
    if (mouseOn(s.mouse) && s.wheel !== "local") {
      return remote("wheel", s.wheel, s.wheel === "probe" ? "probe" : "evidence");
    }
    if (s.agent) {
      return s.page !== "local"
        ? remote("page", s.page, s.page === "probe" ? "probe" : "evidence")
        : none("channel-silent");
    }
    // Пейджер без мыши (less, man): стрелки. Агенту — никогда (см. выше).
    if (!mouseOn(s.mouse)) return { executor: "remote", channel: "arrows", mode: "send", reason: "pager" };
    return none("channel-silent");
  }

  // Обычный буфер без агента: оболочка. Край её истории — край.
  if (!s.agent) return local("shell");

  // Обычный буфер, агент без объявления (Kimi, Gemini, новые CLI).
  if (s.page === "use" || s.page === "verify") return remote("page", s.page, "evidence");
  if (s.page === "local") return s.localCanScroll ? local("channel-silent") : none("channel-silent");
  // Ни объявления, ни наблюдения: история у приложения только по замеру потока.
  // ⚠ И тогда она важнее своей, даже если своя может двигаться: живой разбор
  // 14.08.2026 на телефоне владельца — у перерисовывающего агента наша история
  // состоит из обрывков его кадров, человек листал мусор и упирался в его верх.
  if (s.owner === "application") return remote("page", "probe", "probe");
  return local("safe-local");
}

/** Что делать по кнопке «в самое начало/конец» при уже принятом решении. */
export type EdgePlan =
  | { kind: "local"; edge: "start" | "end" }  // своя история: зажаться её краем
  | { kind: "send"; data: string }            // история приложения: Ctrl+Home/Ctrl+End
  | { kind: "lines" };                        // краевой команды нет — обычная прокрутка

/**
 * Край — ОТДЕЛЬНАЯ операция, а не «очень много строк».
 *
 * ⚠ В 2.57.12 кнопки считали `max(40, rows × 5)` строк и отдавали их обычному
 * маршруту. При экране 31 строка и истории 500 строк нажатие ⇈ уводило человека
 * на 155 строк вверх — он оставался на 345 строк НИЖЕ начала (T25712-01).
 */
export function edgePlan(d: NavigationDecision, up: boolean): EdgePlan {
  if (d.executor === "local") return { kind: "local", edge: up ? "start" : "end" };
  if (d.executor === "remote" && d.channel === "page") return { kind: "send", data: up ? "\x1b[1;5H" : "\x1b[1;5F" };
  // Пейджеру (less/man) и колесу краевой команды не шлём: у них Ctrl+Home ничего
  // не значит, зато обычная прокрутка работает.
  return { kind: "lines" };
}

/**
 * Закрепление снимается, если оно больше не описывает, где человек:
 *   • локальное — человек вернулся к низу своей истории (это и есть возврат к live);
 *   • удалённое — закреплённый канал сейчас не отвечает.
 */
export function shouldDropPin(pin: ReadingPin, s: { atLiveBottom: boolean; page: EvidenceDecision; wheel: EvidenceDecision }): boolean {
  if (pin.executor === "local") return s.atLiveBottom;
  if (pin.channel === "page") return s.page === "local";
  if (pin.channel === "wheel") return s.wheel === "local";
  return false;
}
