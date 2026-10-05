/**
 * ПРЕЖНЕЕ ПРАВИЛО ВЫБОРА КАНАЛА (до 13.09.2026) — ТОЛЬКО ДЛЯ ОТКАТА И ТЕНИ.
 *
 * План стабилизации 13.09, раздел 9.1: у новой навигации отдельный
 * переключатель, а теневой режим вычисляет и сравнивает решение, ничего не
 * отправляя. Здесь — дословный перенос `shouldScrollViewport` + `pickChannel`
 * из altScroll.ts (версия edf251d), выраженный через те же входы, что у
 * navigationDecision.ts. Свидетельства проецируются на прежний трёхзначный
 * флаг pageAnswered/wheelAnswered (answeredView).
 *
 * ⚠ Что откат НЕ возвращает: двойное исполнение намерения. Прежний
 * dispatch повторял молчаливую PgUp своей историей тем же жестом (I-01); этот
 * дефект остаётся исправленным при любом положении переключателя — откатывается
 * только правило выбора исполнителя (ранняя локальная ветка по плотности и т. п.).
 *
 * Известные различия с новым правилом, ради которых оно и заменено:
 *   • ранняя локальная ветка обходит объявленный реестром канал (A02, T-05);
 *   • у Codex (канал «транскрипт») при owner=application жест не делает ничего;
 *   • закрепления источника чтения нет (T-09).
 */
import type { AltScrollChannel } from "./altScroll";
import type { NavigationDecision, NavigationInput } from "./navigationDecision";
import type { EvidenceDecision } from "./navigationEvidence";

const answered = (ev: EvidenceDecision): boolean | null =>
  ev === "use" || ev === "verify" ? true : ev === "local" ? false : null;

function legacyShouldScrollViewport(s: NavigationInput, owner: NavigationInput["owner"]): boolean {
  if (s.override === "terminal") return true;
  if (s.override === "agent" || !s.localCanScroll) return false;
  if (s.alt || !s.agent) return true;
  return answered(s.page) === false || owner !== "application";
}

function legacyPickChannel(s: NavigationInput, owner: NavigationInput["owner"]): AltScrollChannel {
  const pageAnswered = answered(s.page);
  const wheelAnswered = answered(s.wheel);
  const terminalOwnsHistory = owner === "terminal";
  const appOwnsHistory = owner === "application";
  if (s.agent && s.declaredChannel) {
    if (s.declaredChannel === "transcript") return "none";
    if (s.declaredChannel === "page" && pageAnswered !== false) return "page";
    if (s.declaredChannel === "wheel" && wheelAnswered !== false) return "wheel";
  }
  if (appOwnsHistory && s.agent && !s.alt && !terminalOwnsHistory && pageAnswered !== false) return "page";
  // Прежний вызов передавал сюда localCanScroll=false: локальный путь уже
  // отклонён shouldScrollViewport.
  if (terminalOwnsHistory) return "viewport";
  if (!s.alt) {
    if (!s.agent) return "viewport";
    return pageAnswered !== false ? "page" : "none";
  }
  const mouseOn = s.mouse !== "none" && s.mouse !== "";
  if (mouseOn && wheelAnswered !== false) return "wheel";
  if (s.agent) return pageAnswered !== false ? "page" : "none";
  if (!mouseOn) return "arrows";
  return "none";
}

/**
 * Прежнее решение в форме нового. `owner` здесь — владелец истории С УЧЁТОМ
 * ручного режима, как у прежнего historyOwner (effectiveHistoryOwner).
 */
export function legacyDecision(s: NavigationInput, effectiveOwner: NavigationInput["owner"]): NavigationDecision {
  if (legacyShouldScrollViewport(s, effectiveOwner)) {
    return { executor: "local", channel: "viewport", mode: "send", reason: "legacy" };
  }
  const channel = legacyPickChannel(s, effectiveOwner);
  if (channel === "viewport" || channel === "none") {
    // Прежний «viewport» с запретом своей прокрутки не делал ничего (тост).
    return { executor: "none", channel: "none", mode: "send", reason: "legacy" };
  }
  const ev = channel === "page" ? s.page : channel === "wheel" ? s.wheel : "use";
  const mode = s.override === "agent" || channel === "arrows" || ev === "use" || ev === "verify" ? "send" : "probe";
  return { executor: "remote", channel, mode, reason: "legacy" };
}

/** Различие для теневого журнала: пусто — решения совпали. */
export function navigationShadowDiff(v2: NavigationDecision, legacy: NavigationDecision): string {
  if (v2.executor === legacy.executor && v2.channel === legacy.channel) return "";
  return `${legacy.executor}/${legacy.channel}→${v2.executor}/${v2.channel}:${v2.reason}`;
}
