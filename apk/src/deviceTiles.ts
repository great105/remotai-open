/**
 * Что написано на плитке машины и в каком порядке плитки стоят.
 *
 * Правило продукта: сначала то, что ЖДЁТ человека, потом то, что идёт само,
 * потом свободное, офлайн — в конец. Цвет и место в очереди — разные вещи:
 * работающая машина стоит второй, но тревожным цветом не красится (ряд из
 * жёлтых плиток читался как «везде что-то не так», и на его фоне терялась
 * единственная, которой действительно нужен ответ).
 *
 * Живёт отдельно от `DeviceSwitcher.tsx`, чтобы проверяться без React:
 * порядок плиток — это правило, а не разметка.
 */
import { agentDisplayName, formatAgoValue, isAgentKind, t } from "@tgcontrol/shared";
import type { CloudDevice } from "./cloud/api";
import type { DeviceRuntimeSummary } from "./deviceSummaries";

export type Tone = "attention" | "working" | "idle" | "offline";

/** Порядок плиток: сначала те, кому от вас что-то нужно, офлайн — в конец. */
export const TONE_RANK: Record<Tone, number> = { attention: 0, working: 1, idle: 2, offline: 3 };

/**
 * Цвет плитки — отдельно от её места в очереди.
 *
 * Работающая машина красилась предупреждающим жёлтым, хотя от человека ей
 * ничего не нужно: агент печатает сам. Тревожный цвет остаётся только у той,
 * что ЖДЁТ ответа (attention), работа — обычная строка под мятной точкой «в
 * сети». В сортировке working по-прежнему идёт вторым: цвет ушёл, приоритет
 * остался.
 */
export const TONE_CLASS: Record<Tone, string> = {
  attention: "attention",
  working: "",
  idle: "",
  offline: "offline",
};

/**
 * Что происходит на машине — одной строкой. Порядок ветвей = порядок, в котором
 * это важно человеку: сначала то, что ЖДЁТ его, потом то, что идёт само.
 */
export function tileState(
  device: CloudDevice,
  summary?: DeviceRuntimeSummary,
): { text: string; tone: Tone } {
  if (!device.online) {
    const since = device.last_seen_at ? Date.parse(device.last_seen_at) : NaN;
    return {
      tone: "offline",
      text: Number.isFinite(since)
        ? t("devices.tile.offlineAgo", { value: formatAgoValue(since) })
        : t("devices.tile.offline"),
    };
  }
  if (!summary) return { text: t("devices.tile.checking"), tone: "idle" };
  if (summary.waitingCount > 0) {
    return { text: t("devices.tile.waiting", { n: summary.waitingCount }), tone: "attention" };
  }
  if (summary.errorCount > 0) return { text: t("devices.tile.error"), tone: "attention" };
  if (summary.workingCount > 0) {
    // Имя агента говорит больше числа: «Claude Code работает» сразу отвечает,
    // стоит ли туда идти, а «работает 1 терминал» — нет.
    const busy = summary.sessions.find(
      (session) => session.alive
        && (session.status === "working" || session.status === "stalled")
        && isAgentKind(session.agent_kind),
    );
    if (busy) {
      // Строка собиралась как «Claude Code · 1д» — имя агента и число через
      // точку, без единого глагола: что именно случилось «1д» назад, из плитки
      // узнать было нельзя (запустили? ответил? упал?). Теперь это готовая
      // фраза из словаря — «Claude Code работает, ответ 1д назад».
      return {
        tone: "working",
        text: t("devices.tile.agentWorking", {
          agent: agentDisplayName(busy.agent_kind),
          value: formatAgoValue(busy.last_active || busy.created),
        }),
      };
    }
    return { text: t("devices.tile.working", { n: summary.workingCount }), tone: "working" };
  }
  if (summary.terminalCount > 0) {
    return { text: t("devices.tile.terminals", { n: summary.terminalCount }), tone: "idle" };
  }
  return { text: t("devices.tile.free"), tone: "idle" };
}

export type DeviceTile = { device: CloudDevice; text: string; tone: Tone };

/**
 * Плитки соседних машин в том порядке, в котором человек их читает.
 * При равном состоянии — по имени, чтобы ряд не перетасовывался на каждом
 * опросе (машины приходят с релея в произвольном порядке).
 */
export function orderTiles(
  devices: readonly CloudDevice[],
  summaries: Record<string, DeviceRuntimeSummary | undefined>,
  nameOf: (device: CloudDevice) => string,
): DeviceTile[] {
  return devices
    .map((device) => ({ device, ...tileState(device, summaries[device.id]) }))
    .sort((a, b) => TONE_RANK[a.tone] - TONE_RANK[b.tone]
      || nameOf(a.device).localeCompare(nameOf(b.device)));
}
