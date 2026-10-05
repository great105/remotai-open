/**
 * Случайное нажатие в прокручиваемом ряду — не нажатие.
 *
 * Живая жалоба владельца (09.08.2026): «когда скроллю быстрые команды, иногда
 * попадаю по ним и они отправляются; и когда разворачиваю ряд обратно — тоже».
 *
 * Цена ошибки здесь выше обычной опечатки: команда из этого ряда уходит в
 * терминал СРАЗУ, без подтверждения (сознательное решение 2.46.12 — там живут и
 * команды шелла, и текстовые заготовки для агента, и спрашивать про каждую
 * значило бы сделать ряд бесполезным). Значит защищать нужно не вопросом
 * «вы уверены?», а тем, чтобы вообще не считать нажатием то, что нажатием не
 * было.
 *
 * Три случая, и все три — не нажатие:
 *
 *  1. ПАЛЕЦ ЕХАЛ. Человек листал ряд вбок, а палец в этот момент стоял на чипе.
 *     Браузер честно шлёт click, потому что палец опустился и поднялся на одном
 *     элементе — сдвиг в 40 px его не смущает.
 *  2. РЯД ЕЩЁ ЕДЕТ. Инерционная прокрутка: палец уже поднят, лента скользит, и
 *     следующее касание человек делает, чтобы её ОСТАНОВИТЬ, а не выбрать
 *     команду. Ровно так же ведут себя нативные списки.
 *  3. РЯД ТОЛЬКО ЧТО ПОЯВИЛСЯ. Свёрнутый ряд — это полоса на том же месте, где
 *     через мгновение окажутся сами команды: развернув его, человек рискует
 *     попасть по команде, которой в момент нажатия ещё не было.
 *
 * Правила вынесены сюда, а не оставлены в компоненте, чтобы проверяться тестом
 * без React и DOM (общий приём проекта).
 */

/** Сдвиг пальца, после которого касание считается прокруткой, а не нажатием. */
export const MOVE_TOLERANCE_PX = 10;

/** Сколько ряд «остывает» после прокрутки. */
export const SCROLL_COOLDOWN_MS = 300;

/** Сколько ряд не принимает нажатий после разворачивания. */
export const EXPAND_COOLDOWN_MS = 400;

export interface TapGuardState {
  /** Где палец опустился; null — касания не было (мышь, клавиатура). */
  start: { x: number; y: number } | null;
  /** Уехал ли палец за порог с момента касания. */
  moved: boolean;
  /** Когда ряд последний раз прокручивался. */
  scrolledAt: number;
  /** Когда ряд последний раз разворачивали. */
  expandedAt: number;
}

export function emptyTapGuard(): TapGuardState {
  return { start: null, moved: false, scrolledAt: 0, expandedAt: 0 };
}

export function noteDown(s: TapGuardState, x: number, y: number): TapGuardState {
  return { ...s, start: { x, y }, moved: false };
}

export function noteMove(s: TapGuardState, x: number, y: number): TapGuardState {
  if (!s.start || s.moved) return s;
  const dx = Math.abs(x - s.start.x);
  const dy = Math.abs(y - s.start.y);
  // Порог по ОБЕИМ осям: ряд листается вбок, но палец, уехавший вниз (человек
  // начал прокручивать страницу), — тоже не нажатие.
  return dx > MOVE_TOLERANCE_PX || dy > MOVE_TOLERANCE_PX ? { ...s, moved: true } : s;
}

export function noteScroll(s: TapGuardState, now: number): TapGuardState {
  return { ...s, scrolledAt: now };
}

export function noteExpanded(s: TapGuardState, now: number): TapGuardState {
  return { ...s, expandedAt: now };
}

/** Почему нажатие не считается нажатием (для лога и тестов). */
export type TapReject = "moved" | "scrolling" | "just-expanded";

/**
 * Считать ли это нажатием.
 *
 * Мышью (`start === null`, потому что pointerdown с мышью мы не пишем) правила
 * движения не применяются: там промахнуться прокруткой невозможно, а вот
 * колесо мыши над рядом остаётся прокруткой и в остывание попадает честно.
 */
export function tapVerdict(s: TapGuardState, now: number): { ok: true } | { ok: false; why: TapReject } {
  if (s.moved) return { ok: false, why: "moved" };
  if (now - s.scrolledAt < SCROLL_COOLDOWN_MS) return { ok: false, why: "scrolling" };
  if (now - s.expandedAt < EXPAND_COOLDOWN_MS) return { ok: false, why: "just-expanded" };
  return { ok: true };
}
