/** Чат поддержки + аналитика источников (события) — прямые запросы на релей.
 *  Авторизация как в cloud/api.ts: `tma initData` в Telegram, иначе Bearer JWT.
 *  События (postEvent) — fire-and-forget: ошибки глотаем, UX не блокируем. */
import { getRelayBase, getCloudJWT, getMode, getSelectedDeviceId, isNativeApp } from "../config";
import { getTelegram } from "../telegram";
import { CloudError } from "./api";
import { initMetrikaVisit, reachMetrikaGoal, type MetrikaGoal } from "./metrika";

/** Одно сообщение чата поддержки. */
export interface SupportMessage {
  id: number;
  sender: "user" | "admin";
  text: string;
  created_at: string; // RFC3339
}

/** Авторизация account-эндпоинтов релея (дублирует логику api.ts cloudAuthHeader,
 *  который оттуда не экспортируется). */
function cloudAuthHeader(): string {
  const tg = getTelegram();
  if (tg?.initData) return `tma ${tg.initData}`;
  return `Bearer ${getCloudJWT()}`;
}

/** Запрос на релей с облачной авторизацией; ошибки — CloudError, как в cloud/api.ts.
 *  Экспортируется для соседних account-модулей (cloud/notifyPrefs.ts), чтобы не
 *  заводить третью копию обёртки: машинный код ответа сохраняем — его читает
 *  mapApiError. */
export async function relayFetch<T>(path: string, init: RequestInit = {}): Promise<T> {
  const base = getRelayBase().replace(/\/+$/, "");
  const headers = new Headers(init.headers ?? {});
  headers.set("Authorization", cloudAuthHeader());
  if (!headers.has("Content-Type") && init.body) headers.set("Content-Type", "application/json");
  const res = await fetch(base + path, { ...init, headers });
  const text = await res.text();
  let body: unknown = null;
  try { body = text ? JSON.parse(text) : null; } catch { body = { raw: text }; }
  if (!res.ok) {
    const err = body as { error?: string; code?: string } | null;
    throw new CloudError(res.status, err?.error ?? res.statusText, err?.code);
  }
  return body as T;
}

/** История переписки с поддержкой. */
export function getSupportMessages(): Promise<{ messages: SupportMessage[] }> {
  return relayFetch<{ messages: SupportMessage[] }>("/v1/support/messages");
}

/** Отправить сообщение в поддержку. */
export interface SupportContext {
  agent_version?: string;
  platform?: string;
  hostname?: string;
}

export function sendSupportMessage(text: string, context: SupportContext = {}): Promise<{ ok: true; id: number }> {
  const tg = getTelegram();
  return relayFetch<{ ok: true; id: number }>("/v1/support/messages", {
    method: "POST",
    body: JSON.stringify({
      text,
      meta: {
        client_version: import.meta.env.VITE_APP_VERSION || "unknown",
        client_platform: tg?.initData ? "telegram" : isNativeApp ? "android" : "web",
        mode: getMode(),
        device_id: getSelectedDeviceId() || undefined,
        ...context,
      },
    }),
  });
}

/** Счётчик непрочитанных ответов поддержки (сырой запрос).
 *  В UI напрямую НЕ звать: экраны берут число из стора `supportUnread.ts` —
 *  он один ходит в сеть (троттлинг + дедуп), питает бейдж настроек, точку в
 *  нижней навигации и локальное уведомление APK. */
export function getSupportUnread(): Promise<{ count: number }> {
  return relayFetch<{ count: number }>("/v1/support/unread");
}

// ── Аналитика ──────────────────────────────────────────────────

export type EventKind = "app_open" | "pair_success" | "first_terminal" | "first_agent" | "register_source";

/** Ключ согласия на аналитику. Отсутствие значения = включено (opt-out, а не
 *  opt-in): вехи воронки не содержат контента терминалов, файлов и экрана.
 *  АНОНИМНЫМИ они при этом не являются: sendEvent идёт через relayFetch, а тот
 *  ВСЕГДА ставит Authorization (Bearer JWT или tma initData), и релей на этом
 *  основании пишет events.user_id — веха привязана к аккаунту. Тексты в
 *  настройках (settings.help.*) обязаны говорить именно это.
 *  "0" = пользователь выключил тумблер в настройках. */
const ANALYTICS_KEY = "tg.analytics.v1";

/** Разрешена ли отправка вех воронки. При недоступном localStorage (приватный
 *  режим) считаем, что разрешена — иначе выключим статистику всем молча. */
export function isAnalyticsEnabled(): boolean {
  try { return localStorage.getItem(ANALYTICS_KEY) !== "0"; } catch { return true; }
}

/** Сохранить выбор пользователя (Настройки → «Помощь»). */
export function setAnalyticsEnabled(on: boolean): void {
  try { localStorage.setItem(ANALYTICS_KEY, on ? "1" : "0"); } catch { /* приватный режим / quota */ }
}

/** События, которые уже летят на релей в этой вкладке/сессии. */
const pendingOnce = new Set<string>();

/** Отправка события на релей. Возвращает успех, но никогда не бросает:
 * статистика не должна ломать UX. */
async function sendEvent(
  kind: EventKind,
  extra?: { source?: string; utm?: unknown; meta?: unknown },
): Promise<boolean> {
  // Единственная точка гейта: гасит postEvent / postEventOnce /
  // postSessionEventOnce и все track*(), включая UTM из localStorage. Возвращаем
  // false — тогда once-флаги не проставляются и после повторного включения вехи
  // ещё могут уйти.
  if (!isAnalyticsEnabled()) return false;
  // Та же веха — целью в Метрику, если это веб на remotai.ru (metrika.ts):
  // рекламные кампании оптимизируются по целям счётчика, а не по нашей БД.
  const metrikaDeps = {
    nativeApp: isNativeApp,
    telegramMiniApp: Boolean(getTelegram()?.initData),
    analyticsEnabled: true,
  };
  const goal = metrikaGoalFor(kind);
  if (goal) reachMetrikaGoal(goal, metrikaDeps);
  else if (kind === "app_open") initMetrikaVisit(metrikaDeps);
  try {
    await relayFetch("/v1/events", {
      method: "POST",
      body: JSON.stringify({ kind, ...extra }),
    });
    return true;
  } catch {
    return false;
  }
}

/** Fire-and-forget событие. */
/** Веха воронки → цель Метрики. `app_open` целью не является. */
export function metrikaGoalFor(kind: EventKind): MetrikaGoal | null {
  switch (kind) {
    case "register_source": return "register";
    case "pair_success": return "pair_success";
    case "first_terminal": return "first_terminal";
    case "first_agent": return "first_agent";
    default: return null;
  }
}

export function postEvent(
  kind: EventKind,
  extra?: { source?: string; utm?: unknown; meta?: unknown },
): void {
  void sendEvent(kind, extra);
}

/** Отправить событие один раз за всё время. Флаг ставится только после
 * успешного ответа релея: временный 404/offline не должен терять событие
 * навсегда. */
export function postEventOnce(flag: string, kind: EventKind, extra?: { source?: string; utm?: unknown; meta?: unknown }): void {
  try {
    if (localStorage.getItem(flag) || pendingOnce.has("local:" + flag)) return;
  } catch { /* приватный режим — используем только in-memory гард */ }
  const key = "local:" + flag;
  if (pendingOnce.has(key)) return;
  pendingOnce.add(key);
  void sendEvent(kind, extra).then((ok) => {
    if (ok) {
      try { localStorage.setItem(flag, "1"); } catch { /* ignore */ }
    }
  }).finally(() => pendingOnce.delete(key));
}

/** Один раз за текущий запуск/вкладку. */
function postSessionEventOnce(flag: string, kind: EventKind): void {
  try {
    if (sessionStorage.getItem(flag) || pendingOnce.has("session:" + flag)) return;
  } catch { /* use in-memory guard */ }
  const key = "session:" + flag;
  if (pendingOnce.has(key)) return;
  pendingOnce.add(key);
  void sendEvent(kind).then((ok) => {
    if (ok) {
      try { sessionStorage.setItem(flag, "1"); } catch { /* ignore */ }
    }
  }).finally(() => pendingOnce.delete(key));
}

/** Откуда пришёл пользователь: Telegram Mini App / нативный APK / веб. */
function detectSource(): "tg" | "apk" | "web" {
  if (getTelegram()?.initData) return "tg";
  if (isNativeApp) return "apk";
  return "web";
}

/** Разово после успешного логина/пейринга: запоминаем источник установки.
 *  UTM лендинг кладёт в localStorage "utm" (JSON) на том же origin remotai.ru;
 *  если его нет — шлём только source. */
export function trackRegisterSource(): void {
  const extra: { source: string; utm?: unknown } = { source: detectSource() };
  try {
    const raw = localStorage.getItem("utm");
    if (raw) extra.utm = JSON.parse(raw);
  } catch { /* битый JSON — игнорируем, шлём без utm */ }
  postEventOnce("evt_register_source", "register_source", extra);
}

/** Разово за запуск приложения. */
export function trackAppOpen(): void {
  postSessionEventOnce("evt_app_open", "app_open");
}

/** Разово: первый ПК появился после пейринга. */
export function trackPairSuccess(): void {
  postEventOnce("evt_pair_success", "pair_success");
}

/** Разово: создан первый терминал. */
export function trackFirstTerminal(): void {
  postEventOnce("evt_first_terminal", "first_terminal");
}

/**
 * Разово: человек ЗАПУСТИЛ AI-агента.
 *
 * Главная веха продукта и его обещание («агент работает, пока тебя нет»), а в
 * воронке её не было вовсе: последним шагом числился «первый терминал», после
 * которого начинается всё интересное (аудит онбординга 30.08.2026).
 */
export function trackFirstAgent(): void {
  postEventOnce("evt_first_agent", "first_agent");
}
