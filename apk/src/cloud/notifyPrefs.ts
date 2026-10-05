/**
 * Сообщения бота в Telegram — настройка АККАУНТА (GET/PUT /v1/me/notify).
 *
 * Тумблеры в «Уведомлениях» до этого гасили только локальные Capacitor-пуши
 * Android (notifications.ts, localStorage): человек выключал «уведомления», а
 * ночные сообщения бота «агент ждёт ответа» продолжали приходить — их гасила
 * только команда /notify в чате самого бота, о которой в приложении не было ни
 * слова (находки N67/N169).
 *
 * Флага два и они независимы: погасив вопросы агента, нельзя терять ответ
 * поддержки на своё же обращение. `*_available` говорит, дойдёт ли сообщение
 * физически (нужен постоянный Telegram-аккаунт и включённый канал на релее) —
 * по этим полям экран рисует тумблер или строку-объяснение и не гадает по
 * режиму подключения.
 */
import { relayFetch } from "./support";

export interface CloudNotifyPrefs {
  /** Аккаунт связан с настоящим Telegram (не анонимный пейринг по QR). */
  telegram_linked: boolean;
  agent_waiting: boolean;
  agent_waiting_available: boolean;
  support_reply: boolean;
  support_reply_available: boolean;
}

/** Текущее состояние обоих флагов. */
export function getCloudNotifyPrefs(): Promise<CloudNotifyPrefs> {
  return relayFetch<CloudNotifyPrefs>("/v1/me/notify");
}

/**
 * Изменить один флаг. Тело частичное — релей не трогает не переданное, чтобы
 * два тумблера не затирали друг друга при гонке двух клиентов одного аккаунта.
 * В ответе приезжает ПОЛНОЕ состояние: его и рисуем, не додумывая локально.
 */
export function setCloudNotifyPrefs(
  patch: { agent_waiting?: boolean; support_reply?: boolean },
): Promise<CloudNotifyPrefs> {
  return relayFetch<CloudNotifyPrefs>("/v1/me/notify", {
    method: "PUT",
    body: JSON.stringify(patch),
  });
}
