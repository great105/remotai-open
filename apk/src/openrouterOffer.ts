/**
 * Когда предлагать OpenRouter на ГЛАВНОМ пути — в шторке запуска агента.
 *
 * Аудит онбординга 30.08.2026: слова «OpenRouter» в шторке запуска не было
 * вовсе. Человек без подписки на Claude/ChatGPT — а это и есть заявленный
 * плацдарм продукта — упирался в «войдите в аккаунт» и выходил из приложения;
 * единственное предложение ключа жило в свёрнутой секции другого экрана.
 *
 * Правило вынесено из компонента намеренно: его можно позвать из теста без
 * React и DOM (`AGENTS.md`, «правила клиента выносим из экранов в модули»).
 */
import type { AIUsageSnapshot } from "./aiUsage";

export interface OpenRouterOfferInput {
  /** Ключ уже подключён на этом компьютере. */
  configured: boolean;
  /** Сколько CLI-агентов на компьютере найдено (0 = ставить ещё нечего). */
  installedAgents: number;
  /** Снимок лимитов подписок; `null` — ещё не приехал. */
  usage: AIUsageSnapshot | null;
}

/**
 * Предлагать, когда человеку НЕЧЕМ думать: ключа нет и при этом либо агентов
 * на машине нет вовсе, либо ни одна подписка не готова к работе.
 *
 * Молчание при неприехавших лимитах — то же правило, что у `accountSignedOut`:
 * пока мы не знаем, ничего не утверждаем, иначе предложение мигало бы на
 * каждой загрузке у человека с оплаченной подпиской.
 */
export function shouldOfferOpenRouter({ configured, installedAgents, usage }: OpenRouterOfferInput): boolean {
  if (configured) return false;
  if (installedAgents === 0) return true;
  const providers = usage?.providers || [];
  if (providers.length === 0) return false;
  return providers.every((p) => p.status !== "available");
}
