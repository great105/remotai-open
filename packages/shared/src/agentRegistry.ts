import { t } from "./i18n";
import { ownedText } from "./ownedText";
import type { AgentInfo } from "./types";

const names = new Map<string, string>();
/**
 * Чем у агента открывается ЕГО СОБСТВЕННАЯ история — из реестра компьютера, а
 * не из знаний клиента. То же правило, что у имён и флагов запуска: новый агент
 * появляется в интерфейсе без правок фронта.
 */
const history = new Map<string, { key: string; label: string; channel: string }>();
/**
 * Как профиль агента обращается с CSI 3 J (ST-04): свойство ХРАНЕНИЯ, не
 * навигации. Основной путь доставки — поле state терминала; этот кэш —
 * запасной, когда терминал открыт после экрана агентов.
 */
const retention = new Map<string, "honor" | "preserve">();

/** Prime the client-side display cache from the backend's canonical registry. */
export function setAgentRegistry(
  agents: Pick<AgentInfo, "id" | "name" | "history_key" | "history_label" | "history_channel" | "history_retention">[],
): void {
  for (const agent of agents) {
    if (agent.id && agent.name) names.set(agent.id, agent.name);
    if (agent.id) {
      // Реестр приходит целиком: убранное значение снимаем, а не держим
      // устаревшим — неверная политика хранения хуже отсутствующей.
      const declared = agent.history_retention;
      if (declared === "honor" || declared === "preserve") retention.set(agent.id, declared);
      else retention.delete(agent.id);
    }
    if (agent.id && (agent.history_key || agent.history_channel)) {
      history.set(agent.id, {
        key: agent.history_key || "",
        label: agent.history_label || "",
        channel: agent.history_channel || "",
      });
    }
  }
}

/**
 * Объявленный компьютером канал истории агента: "page" | "wheel" |
 * "transcript" | "" (не знаем).
 *
 * Первый слой решения режима «Авто». Пусто — работает прежний путь: замер
 * потока и проба фактом. Знание живёт в реестре компьютера, поэтому новый агент
 * получает правильную прокрутку без правок клиента.
 */
export function declaredScrollChannel(id: string | null | undefined): "page" | "wheel" | "transcript" | "" {
  if (!id) return "";
  const c = history.get(id)?.channel;
  return c === "page" || c === "wheel" || c === "transcript" ? c : "";
}

/**
 * Объявленная реестром политика стирания истории: "honor" | "preserve" | ""
 * (не объявлено — клиент берёт значение по умолчанию, keepHistory.retentionFor).
 * НЕ выводится из канала прокрутки или режима по умолчанию: это навигация (I-02).
 */
export function declaredRetention(id: string | null | undefined): "honor" | "preserve" | "" {
  if (!id) return "";
  return retention.get(id) ?? "";
}

/**
 * Встроенный запасной ответ о хранении истории для ИЗВЕСТНЫХ агентов (волна 4,
 * совместимость): агент 2.71.1 и старше не шлёт history_retention ни в /state,
 * ни в /api/agents, и Codex получал политику по умолчанию honor — его полная
 * перерисовка (CSI 2J 3J H: поворот, resize второго зрителя) стирала прокрутку.
 * Значения — копия internal/agents/registry.go (HistoryRetention; Codex —
 * инцидент 2.57.23). Берётся, только когда ни state, ни кэш реестра поля не
 * несут: объявление агента всегда главнее.
 */
const BUILTIN_RETENTION: Readonly<Record<string, "honor" | "preserve">> = { codex: "preserve" };

export function builtinRetention(id: string | null | undefined): "honor" | "preserve" | "" {
  if (!id) return "";
  return Object.prototype.hasOwnProperty.call(BUILTIN_RETENTION, id) ? BUILTIN_RETENTION[id] : "";
}

/** Канал собственной истории агента; null — компьютер про него не сказал. */
export function agentHistoryChannel(id: string | null | undefined): { key: string; label: string } | null {
  if (!id) return null;
  const rec = history.get(id);
  // Без клавиши предлагать нечего: канал "page" листается жестом, а не кнопкой.
  return rec?.key ? { key: rec.key, label: ownedText(rec.label) } : null;
}

/** Canonical display name, with a readable fallback for a newer backend id. */
export function agentDisplayName(id: string | null | undefined): string {
  if (!id) return t("sys.busyTerminal");
  const known = names.get(id);
  if (known) return known;
  return id
    .split(/[-_]+/)
    .filter(Boolean)
    .map((part) => part.charAt(0).toUpperCase() + part.slice(1))
    .join(" ") || t("sys.busyTerminal");
}
