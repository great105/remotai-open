/**
 * Правила экрана терминала: что человек видит, пока связь идёт, и чем отвечает
 * агенту.
 *
 * Жили внутри `PtyTermView.tsx` (2547 строк) вперемешку с разметкой, хотя это
 * чистые правила от состояния сессии — и ошибались они заметно: экран
 * одновременно писал «Переподключение…» и «Процесс завершён» с кнопкой
 * перезапуска, и человек перезапускал живой терминал из-за мигнувшей сети.
 */
import type { PtyState } from "../api";

/** Поля, которых пока нет в общем типе PtyState (их правит другая волна). */
export type PtyStateExtra = PtyState & { hint_options?: string[]; viewers?: number };

/** Кнопка ответа агенту: цифра пункта и подпись с ПК (может быть пустой). */
export interface AnswerChoice { digit: string; label: string }

/**
 * Кнопки нумерованного меню агента.
 *
 * Подпись берём с компьютера («1 · Yes, and don't ask again»): голые цифры не
 * говорили, что выберет «2», а меню к моменту ответа обычно уже под
 * клавиатурой. Старый агент подписей не присылает — тогда остаются цифры, но
 * кнопки всё равно есть: ответить надо чем-то.
 */
export function answerChoicesOf(state: PtyStateExtra, answerKind: string): AnswerChoice[] {
  const labelled = (answerKind === "choice" ? (state.hint_options || []).slice(0, 5) : [])
    .map((label, i) => ({ digit: String(i + 1), label }));
  if (labelled.length > 0) return labelled;
  return ["1", "2", "3"].map((digit) => ({ digit, label: "" }));
}

/**
 * Что сейчас важнее сказать: «связь идёт» или «процесс завершён».
 *
 * Пока связь не установлена, состояние процесса вторично — данные о нём взяты
 * из прошлого ответа и могут быть протухшими. `gaveUp` — уже не «в процессе»:
 * ждать перестали, и если процесс мёртв, плашка с перезапуском как раз то, что
 * нужно.
 */
export function terminalLinkState(input: {
  showConnecting: boolean;
  reconnecting: boolean;
  gaveUp: boolean;
  alive: boolean;
}): { linkPending: boolean; processDead: boolean } {
  const linkPending = (input.showConnecting || input.reconnecting) && !input.gaveUp;
  return { linkPending, processDead: !input.alive && !linkPending };
}

/**
 * Заголовок уже назвал агента — бейдж рядом оставляем иконкой, чтобы «Claude»
 * не стояло в узкой шапке дважды.
 */
export function titleNamesAgent(state: PtyState): boolean {
  return !state.name && !!state.agent_kind
    && state.agent_kind !== "shell" && state.agent_kind !== "other";
}

/**
 * Имя терминала, показанное в прошлый заход.
 *
 * Ответ /state приходит не мгновенно (у выключенного ПК — только через
 * таймаут), и всё это время в шапке стояло безликое «Терминал»: вернувшись из
 * фона к четырём открытым терминалам, человек не мог понять, в каком он сейчас.
 * Терминал за это время своим именем не становится другим, поэтому прошлое имя
 * — честная догадка на первый кадр, а не выдумка.
 */
export function readTitleCache(sessionId: string | undefined): string {
  if (!sessionId) return "";
  try { return localStorage.getItem(`pty.title.${sessionId}`) || ""; } catch { return ""; }
}

export function writeTitleCache(sessionId: string, title: string): void {
  try { localStorage.setItem(`pty.title.${sessionId}`, title); } catch { /* приватный режим */ }
}

/** Терминала на компьютере больше нет — незачем помнить и его имя. */
export function forgetTitleCache(sessionId: string): void {
  try { localStorage.removeItem(`pty.title.${sessionId}`); } catch { /* приватный режим */ }
}

/**
 * Расшифровка исхода, который сам по себе ничего не объясняет.
 *
 * Строку в карточку пишет агент — ровно ту, что была в выводе. Обычно этого
 * хватает («npm ERR! code E404»), но смерть от нехватки памяти выглядит как
 * одинокое «Killed»: человек видит, что команда исчезла, и не знает почему.
 * Живой случай владельца — установка Claude Code на VPS с 1 ГБ без подкачки:
 * «нажал установку — и всё вылетело».
 *
 * Возвращает ключ подсказки или "" — фразы живут в i18n, здесь только правило.
 */
export function outcomeAdviceKey(hint: string | undefined): string {
  const text = (hint || "").toLowerCase();
  if (!text) return "";
  if (/^killed\b/.test(text)
    || /\bout of memory\b/.test(text)
    || /\bcannot allocate memory\b/.test(text)
    || /\bsignal sigkill\b/.test(text)) {
    return "pty.outcomeOom";
  }
  return "";
}
