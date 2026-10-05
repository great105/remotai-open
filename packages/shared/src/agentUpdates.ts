// Обновление CLI-агентов тем же менеджером, которым он поставлен.
//
// Сервер (internal/agentupdate) знает версию, владельца установки, последнюю
// версию и команду. Здесь — типы, вызовы API и ПРАВИЛА показа: что написать в
// строке агента, о чём предупредить перед обновлением. Правила без React, чтобы
// их можно было проверить тестом в node (agentUpdates.test.ts).

import { transport } from "./api-transport";
import { t } from "./i18n";

/** Один агент в ответе GET /api/agents/updates. */
export interface CliAgentUpdate {
  id: string;
  name: string;
  path: string;
  /** Текущая версия ("" — агент не ответил на --version). */
  version: string;
  /** Последняя версия из реестра ("" — не знаем). */
  latest: string;
  /** Кто поставил: npm, pnpm, bun, volta, claude-native, brew, winget, scoop, unknown. */
  owner: string;
  owner_title: string;
  package?: string;
  /** Что напечатать в терминал; пусто, если обновить отсюда нельзя. */
  update_command: string;
  can_update: boolean;
  /** Почему нельзя — для человека. */
  reason?: string;
  /** false — последнюю версию проверяет сам установщик (нативный Claude). */
  latest_known: boolean;
  latest_error?: string;
  version_error?: string;
  update_available: boolean;
  /** Живые терминалы с этим агентом прямо сейчас. */
  running_sessions: number;
  checked_at: number;
}

/** Одна смена версии, замеченная компьютером. from "" — первый замер. */
export interface CliAgentVersionEvent {
  at: number;
  from: string;
  to: string;
  owner: string;
}

export function getCliAgentUpdates(refresh = false): Promise<{ agents: CliAgentUpdate[] }> {
  return transport().request(`/api/agents/updates${refresh ? "?refresh=1" : ""}`);
}

export function getCliAgentVersions(id: string): Promise<{ id: string; events: CliAgentVersionEvent[] }> {
  return transport().request(`/api/agents/${encodeURIComponent(id)}/versions`);
}

/**
 * Сравнение версий: -1, 0, 1. Правило то же, что у сервера
 * (agentupdate.CompareVersions): числа по частям, предрелиз меньше релиза,
 * пустая версия меньше любой.
 */
export function compareVersions(a: string, b: string): number {
  a = (a || "").trim().replace(/^v/, "");
  b = (b || "").trim().replace(/^v/, "");
  if (a === b) return 0;
  if (!a) return -1;
  if (!b) return 1;
  const [an, apre = ""] = splitPre(a);
  const [bn, bpre = ""] = splitPre(b);
  const ap = an.split("."), bp = bn.split(".");
  for (let i = 0; i < Math.max(ap.length, bp.length); i++) {
    const x = parseInt(ap[i] ?? "0", 10) || 0;
    const y = parseInt(bp[i] ?? "0", 10) || 0;
    if (x !== y) return x < y ? -1 : 1;
  }
  if (apre === bpre) return 0;
  if (!apre) return 1;
  if (!bpre) return -1;
  return apre < bpre ? -1 : 1;
}

function splitPre(v: string): [string, string] {
  const i = v.indexOf("-");
  return i < 0 ? [v, ""] : [v.slice(0, i), v.slice(i + 1)];
}

/** Как назвать владельца установки человеку. */
export function ownerLabel(owner: string, fallback?: string): string {
  const key = `agentUpdate.owner.${owner}`;
  const text = t(key);
  return text === key || text.startsWith("⟦") ? (fallback || t("agentUpdate.owner.unknown")) : text;
}

export type UpdateLineKind = "update" | "current" | "installer" | "cannot" | "unknown";

export interface UpdateLine {
  kind: UpdateLineKind;
  /** Главная строка: «Есть обновление 2.1.3 → 2.1.5 · npm». */
  text: string;
  /** Пояснение мельче (почему нельзя, что проверит установщик). */
  hint?: string;
  /** Показывать кнопку «Обновить». */
  canUpdate: boolean;
}

/**
 * Что сказать в строке агента. Порядок — от того, что человеку делать, к
 * справке: есть обновление → кнопка; нативный Claude → кнопка «Проверить и
 * обновить» (последнюю версию знает только установщик); обновлено → тихая
 * строка версии; обновить нельзя → честная причина.
 */
export function updateLine(u: CliAgentUpdate): UpdateLine {
  const owner = ownerLabel(u.owner, u.owner_title);
  const version = u.version || t("agentUpdate.versionUnknown");
  if (!u.can_update) {
    return {
      kind: "cannot",
      text: u.version ? t("agentUpdate.versionOwner", { version, owner }) : t("agentUpdate.versionUnknownLine", { owner }),
      hint: u.reason || t("agentUpdate.cannotHint"),
      canUpdate: false,
    };
  }
  const newer = !!u.latest && !!u.version && compareVersions(u.version, u.latest) < 0;
  if (newer) {
    return {
      kind: "update",
      text: t("agentUpdate.available", { from: u.version, to: u.latest, owner }),
      canUpdate: true,
    };
  }
  if (!u.latest_known) {
    return {
      kind: "installer",
      text: t("agentUpdate.versionOwner", { version, owner }),
      hint: t("agentUpdate.installerChecks"),
      canUpdate: true,
    };
  }
  if (u.latest && u.version) {
    return { kind: "current", text: t("agentUpdate.current", { version, owner }), canUpdate: false };
  }
  return {
    kind: "unknown",
    text: t("agentUpdate.versionOwner", { version, owner }),
    hint: u.latest_error ? t("agentUpdate.latestUnknown") : undefined,
    canUpdate: false,
  };
}

/**
 * Предупреждение перед обновлением, если агент сейчас запущен. Пусто — можно
 * без вопросов. Открытые сессии останутся на старой версии до перезапуска, а
 * на Windows работающий агент держит свои файлы, и npm может не заменить их.
 */
export function runningWarning(u: CliAgentUpdate): string {
  const n = u.running_sessions || 0;
  if (n <= 0) return "";
  return t("agentUpdate.runningWarn", { n, name: u.name });
}

/** Строка истории: «29.09, 14:02 · 2.1.3 → 2.1.5». */
export function versionEventText(e: CliAgentVersionEvent, now = new Date()): string {
  const d = new Date(e.at);
  const sameYear = d.getFullYear() === now.getFullYear();
  const pad = (x: number) => String(x).padStart(2, "0");
  const date = `${pad(d.getDate())}.${pad(d.getMonth() + 1)}${sameYear ? "" : "." + d.getFullYear()}, ${pad(d.getHours())}:${pad(d.getMinutes())}`;
  const what = e.from
    ? t("agentUpdate.historyChange", { from: e.from, to: e.to })
    : t("agentUpdate.historyFirst", { to: e.to });
  return `${date} · ${what}`;
}

/** Статус терминала, по которому считаем, что команда обновления закончилась. */
export function updateFinished(status: string | undefined, alive: boolean, sinceStartMs: number): boolean {
  if (!alive) return true;
  // Первые секунды терминал ещё поднимает шелл и может честно быть «idle» до
  // того, как npm начал работать.
  if (sinceStartMs < 8000) return false;
  return status === "idle" || status === "error" || status === "dead" || status === "ready";
}
