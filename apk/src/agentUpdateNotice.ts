/**
 * Плашка состояния обновления АГЕНТА вне экранов «Мои компьютеры» и «Настройки».
 *
 * Зачем отдельный модуль: одно и то же состояние («версия отстала» / «сильно
 * устарела» / «ждёт перезапуска») показывается теперь в двух местах — на
 * главной и поверх терминала, — а правило «когда об этом вообще говорить» уже
 * живёт в fleetRules.agentUpdateState. Здесь — тонкая обёртка над ним плюс две
 * вещи, которым нечего делать в компонентах: загрузка актуальной версии с
 * релея (с кэшем, чтобы два экрана не дёргали манифест по очереди) и память
 * «плашку скрыли» — per версия, иначе закрытый крестиком баннер воскресал бы
 * при каждом маунте.
 *
 * React здесь нет намеренно: правило проверяет agentUpdateNotice.test.ts.
 */
import { agentUpdateState } from "./fleetRules";
import { getMode, getRelayBase } from "./config";

/** Манифест релея перечитываем не чаще, чем раз в полчаса: версия релиза
 *  меняется раз в дни, а маунтов экранов за это время — десятки. */
const LATEST_TTL_MS = 30 * 60 * 1000;

let latestCache: { version: string; at: number } | null = null;
// Главная и терминал могут спросить одновременно — второй ждёт первого,
// а не шлёт свой запрос.
let latestInflight: Promise<string> | null = null;

/**
 * Актуальная версия агента по манифесту релея (`GET /v1/latest`).
 *
 * Есть только в облаке: в LAN-режиме релея в цепочке нет, а актуальную версию
 * там сообщает сам агент (GET /api/system/version). Любая ошибка — «не знаем»
 * (пустая строка), и плашка просто не рисуется: отсутствие новости лучше
 * выдуманной тревоги.
 */
export function fetchLatestVersion(): Promise<string> {
  if (getMode() !== "cloud") return Promise.resolve("");
  if (latestCache && Date.now() - latestCache.at < LATEST_TTL_MS) {
    return Promise.resolve(latestCache.version);
  }
  if (latestInflight) return latestInflight;
  latestInflight = (async () => {
    try {
      const res = await fetch(`${getRelayBase().replace(/\/+$/, "")}/v1/latest`);
      if (!res.ok) return "";
      const body: unknown = await res.json().catch(() => null);
      const version = typeof (body as { version?: unknown })?.version === "string"
        ? ((body as { version: string }).version).trim()
        : "";
      if (version) latestCache = { version, at: Date.now() };
      return version;
    } catch {
      return "";
    } finally {
      latestInflight = null;
    }
  })();
  return latestInflight;
}

export interface AgentUpdateNotice {
  state: "auto" | "stuck";
  latest: string;
}

/**
 * Стоит ли говорить человеку про версию агента — и как именно.
 *
 * Обёртка над fleetRules.agentUpdateState: «не знаем» и «актуально» — это
 * отсутствие новости (null), а не состояние плашки.
 *
 * `online` передаём как true: плашка рисуется про ВЫБРАННУЮ машину, с которой
 * клиент в этот момент работает, а её флаг online в конфиге не хранится.
 * Считать её офлайн было бы хуже: отставание на мажорную версию навсегда
 * осталось бы спокойным «обновится само» — а именно его самообновление уже не
 * догоняет.
 */
export function agentUpdateNotice(agentVersion: string, latest: string): AgentUpdateNotice | null {
  const state = agentUpdateState(
    { workspace_id: "", agent_version: agentVersion, online: true },
    latest,
  );
  if (state !== "auto" && state !== "stuck") return null;
  return { state, latest };
}

/** Ключ localStorage, которым запоминается «эту версию обсудили». */
const DISMISS_PREFIX = "remotai.agent-update-dismissed.";

/**
 * Скрыть плашку до следующей версии. Память per версия (тот же паттерн, что
 * `remotai.agent-update-seen.*` в настройках): иначе «скрыть навсегда» прятало
 * бы и НОВУЮ версию, о которой человек ещё не знает.
 */
export function dismissAgentUpdateNotice(version: string): void {
  try {
    localStorage.setItem(`${DISMISS_PREFIX}${version}`, "1");
  } catch { /* приватный режим: плашка просто появится ещё раз */ }
}

/** Читается СИНХРОННО при рендере: плашка не должна мигать на каждом маунте. */
export function agentUpdateNoticeDismissed(version: string): boolean {
  try {
    return !!localStorage.getItem(`${DISMISS_PREFIX}${version}`);
  } catch {
    return false;
  }
}
