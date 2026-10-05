// Прогресс освоения приложения для блока «Первые шаги» на главной.
//
// Правила: шаг отмечается по ФАКТУ (терминал существует, агент виден в
// foreground, пользователь дошёл до экрана ПК), но запоминается «липко» —
// закрытый терминал не должен снимать галочку и возвращать новичковый вид.
// Хранится локально на устройстве: это UI-прогресс конкретного телефона, а не
// свойство ПК, и синхронизировать его через агента нечего.

import { useEffect, useState } from "react";

export type HomeStep = "terminal" | "agent" | "remote" | "files";

const KEY = "tg.home.progress.v1";
const DISMISS_KEY = "tg.home.progress.dismissed.v1";
// След запуска AI-агента, который оставляет экран терминала: `pty.lastAgent.<id>`
// с непустым видом агента (PtyTermView пишет его, как только агент оказался в
// foreground открытого терминала). До этого шаг «Запустить AI-агента»
// отмечался ТОЛЬКО живым снимком в момент, когда открыта главная: человек,
// который закрывает терминал после работы, не закрывал чеклист новичка никогда
// (находка N100). След переживает и закрытие терминала, и перезапуск клиента.
const PTY_LAST_AGENT_PREFIX = "pty.lastAgent.";
// Последний известный ответ ПК на вопрос «телефон привязан?» (шаг чеклиста,
// который считается не локально, а запросом к ПК). Кэш нужен именно для
// недоступного ПК: без него запрос падал, шаг снимался, и чеклист новичка
// возвращался к «0 из 3» у человека, который всё уже настроил.
const PAIRED_KEY = "tg.home.paired.v1";

export type HomeProgress = Partial<Record<HomeStep, true>>;

type Progress = HomeProgress;

/**
 * Первый результат — запущенный агент на компьютере или сервере.
 * Старые ключи прогресса сохраняем для совместимости локального хранилища.
 */
export function homeStepIds(_hasDisplay?: boolean): HomeStep[] {
  return ["agent"];
}

/**
 * Ставить ли чеклист «Первые шаги» НАВЕРХ главной.
 *
 * Порядок блоков главной рассчитан на того, кто уже работает: «жив ли ПК →
 * что происходит → запустить → что ещё освоить». Новичку же чеклист — первый
 * вопрос, а внизу он не виден вовсе: замер 04.09.2026 на телефоне 390×844
 * показал его заголовок на 745 px, то есть под нижней панелью навигации.
 *
 * Правило: наверх, пока не запущен первый агент. Скрытый вручную
 * чеклист наверх не всплывает никогда: «Скрыть» — это решение человека.
 */
export function firstStepsGoesOnTop(done: HomeProgress, dismissed: boolean): boolean {
  if (dismissed) return false;
  return !done.agent;
}

/** Все шаги пройдены — главная переключается в «рабочий» вид. */
export function homeOnboardingDone(done: HomeProgress, hasDisplay: boolean): boolean {
  return homeStepIds(hasDisplay).every((s) => done[s]);
}

let cache: Progress | null = null;
const listeners = new Set<() => void>();

function read(): Progress {
  if (cache) return cache;
  try {
    const raw = localStorage.getItem(KEY);
    cache = raw ? (JSON.parse(raw) as Progress) : {};
  } catch {
    cache = {};
  }
  return cache!;
}

function emit() {
  for (const fn of listeners) fn();
}

/** Отметить шаг выполненным (идемпотентно, без лишних ре-рендеров). */
export function markHomeStep(step: HomeStep): void {
  const cur = read();
  if (cur[step]) return;
  cache = { ...cur, [step]: true };
  try {
    localStorage.setItem(KEY, JSON.stringify(cache));
  } catch {
    /* приватный режим / переполнение — прогресс просто не переживёт перезапуск */
  }
  emit();
}

export function getHomeProgress(): Progress {
  return read();
}

/** Хоть один терминал этого клиента когда-либо показывал агента в foreground. */
function hasAgentLaunchTrace(): boolean {
  try {
    for (let i = 0; i < localStorage.length; i++) {
      const key = localStorage.key(i);
      if (key?.startsWith(PTY_LAST_AGENT_PREFIX) && localStorage.getItem(key)) return true;
    }
  } catch {
    /* приватный режим / нет localStorage — судить о следе нечем */
  }
  return false;
}

/**
 * Отметить шаг «Запустить AI-агента» по факту запуска, а не по случайному
 * снимку живых сессий. Идемпотентно и дёшево: как только шаг отмечен, след
 * больше не сканируем. Зовём при загрузке модуля (агента запускали в прошлый
 * раз) и при показе чеклиста (запускали минуту назад, пока главная была
 * закрыта).
 */
export function syncAgentStep(): void {
  if (read().agent) return;
  if (hasAgentLaunchTrace()) markHomeStep("agent");
}

/**
 * Запомнить ответ ПК про привязку телефона. `undefined` = ПК ещё ни разу не
 * отвечал (совсем новый клиент) — тогда шаг честно показываем невыполненным.
 */
export function rememberPhonePaired(paired: boolean): void {
  try {
    localStorage.setItem(PAIRED_KEY, paired ? "1" : "0");
  } catch { /* приватный режим — переживём без кэша */ }
}

export function lastKnownPhonePaired(): boolean | undefined {
  try {
    const raw = localStorage.getItem(PAIRED_KEY);
    return raw === null ? undefined : raw === "1";
  } catch {
    return undefined;
  }
}

/** Пользователь скрыл чеклист вручную — больше не показываем. */
export function dismissFirstSteps(): void {
  try {
    localStorage.setItem(DISMISS_KEY, "1");
  } catch { /* ignore */ }
  emit();
}

/**
 * Вернуть чеклист на главную. До 28.08.2026 функции сброса не было ни одной:
 * `DISMISS_KEY` только писался и читался, поэтому «Скрыть» было необратимым —
 * промах по кнопке 48×20 стоил человеку всего обучения (UX-аудит 2026-08-23,
 * HOLISTIC-9, NIELSEN-19). `emit()` обязателен: на нём держится подписка
 * `useHomeProgress`, без него главная не перерисуется.
 */
export function restoreFirstSteps(): void {
  try {
    localStorage.removeItem(DISMISS_KEY);
  } catch { /* ignore */ }
  emit();
}

export function isFirstStepsDismissed(): boolean {
  try {
    return localStorage.getItem(DISMISS_KEY) === "1";
  } catch {
    return false;
  }
}

/** Подписка на прогресс: главная перестраивается сразу после отметки шага. */
export function useHomeProgress(): { done: Progress; dismissed: boolean } {
  const [, bump] = useState(0);
  useEffect(() => {
    const fn = () => bump((n) => n + 1);
    listeners.add(fn);
    // Агента могли запустить, пока главная была закрыта: перечитываем след
    // здесь, а не в read() — отметка шага дёргает подписчиков, а делать это
    // во время рендера нельзя.
    syncAgentStep();
    return () => { listeners.delete(fn); };
  }, []);
  return { done: read(), dismissed: isFirstStepsDismissed() };
}

// Прошлые запуски агента подхватываем ещё до первого рендера — иначе чеклист
// успевал мигнуть непройденным шагом.
syncAgentStep();
