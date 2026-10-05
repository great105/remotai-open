// Правила экрана «Беседы» (pages/AgentSessionsView.tsx) — без React, чтобы
// проверяться тестом в node: куда ведёт нажатие, какой командой продолжать
// беседу, как делить список по дням и что писать в строке.

import type { AgentSessionItem } from "@tgcontrol/shared";
import type { AgentInfo } from "../types";
import { composeLaunch, type LaunchContext } from "../ptyTerm/agentLaunch";
import { canSleepAgent } from "../ptyTerm/agentSleep";

// Номер беседы уходит в строку шелла — те же правила, что у сервера и у
// усыпления (agentSleep.ts). Вторая линия: сервер такие номера не отдаёт.
const SESSION_ID = /^[A-Za-z0-9][A-Za-z0-9._-]{7,127}$/;

/**
 * Что делать по нажатию. Беседа уже идёт или спит в терминале Remotai —
 * открыть ЕГО: второй `claude --resume` той же беседы — это две копии агента,
 * пишущие в один файл, и ответы, перемешанные между двумя окнами.
 */
export type SessionAction =
  | { kind: "open"; ptyId: string }
  | { kind: "resume" }
  | { kind: "unavailable" };

export function sessionAction(item: AgentSessionItem, agent: AgentInfo | null | undefined): SessionAction {
  if (item.open_pty_id) return { kind: "open", ptyId: item.open_pty_id };
  if (!agent || !canSleepAgent(agent) || !SESSION_ID.test(item.session_id || "")) return { kind: "unavailable" };
  return { kind: "resume" };
}

/**
 * Флаг «Без подтверждений» этого агента — первый опасный из `launch_flags`
 * реестра (Claude: --dangerously-skip-permissions, Codex:
 * --dangerously-bypass-approvals-and-sandbox). Клиент имён флагов не знает.
 */
export function skipPermissionsFlag(agent: AgentInfo | null | undefined): string {
  return agent?.launch_flags?.find((f) => f.danger)?.flag || "";
}

/**
 * Команда продолжения беседы по номеру. Аккаунт, прокси, хуки и окружение
 * флага добавляет composeLaunch — как у любого запуска и у пробуждения.
 * Пусто — продолжить нечем (агент не тот, номер кривой, аккаунт заблокирован).
 */
export function resumeSessionCommand(
  agent: AgentInfo | null | undefined,
  item: Pick<AgentSessionItem, "agent" | "session_id">,
  skipPermissions: boolean,
  ctx: LaunchContext,
): string {
  if (!agent || agent.id !== item.agent || !canSleepAgent(agent)) return "";
  if (!SESSION_ID.test(item.session_id || "")) return "";
  const base = agent.resume_id_cli!.replace("{session_id}", item.session_id);
  const flag = skipPermissions ? skipPermissionsFlag(agent) : "";
  return composeLaunch(base, { flags: flag ? [flag] : [], extra: "" }, {
    ...ctx,
    specs: agent.launch_flags || [],
  });
}

/** Шелл компьютера POSIX: ОС назвал агент, иначе судим по виду папки. */
export function isPosixShell(platform: string | undefined, cwd: string): boolean {
  if (platform) return platform !== "windows";
  return !/^[A-Za-z]:[\\/]/.test(cwd || "");
}

/** Последняя папка пути — её человек узнаёт быстрее полного пути. */
export function folderName(cwd: string): string {
  const clean = (cwd || "").replace(/[\\/]+$/, "");
  const parts = clean.split(/[\\/]/).filter(Boolean);
  return parts[parts.length - 1] || clean;
}

const MONTHS = [
  "января", "февраля", "марта", "апреля", "мая", "июня",
  "июля", "августа", "сентября", "октября", "ноября", "декабря",
];

function dayStart(ms: number): number {
  const d = new Date(ms);
  d.setHours(0, 0, 0, 0);
  return d.getTime();
}

/** Подпись дня: «Сегодня», «Вчера», «25 сентября», «3 марта 2025». */
export function dayLabel(ms: number, now: number, today: string, yesterday: string): string {
  const start = dayStart(ms);
  const todayStart = dayStart(now);
  if (start === todayStart) return today;
  if (start === dayStart(todayStart - 12 * 3600_000)) return yesterday;
  const d = new Date(ms);
  const base = `${d.getDate()} ${MONTHS[d.getMonth()]}`;
  return d.getFullYear() === new Date(now).getFullYear() ? base : `${base} ${d.getFullYear()}`;
}

export interface DayGroup {
  key: string;
  label: string;
  items: AgentSessionItem[];
}

/** Делит список (уже свежие сверху) на дни, сохраняя порядок. */
export function groupByDay(items: AgentSessionItem[], now: number, today: string, yesterday: string): DayGroup[] {
  const out: DayGroup[] = [];
  for (const item of items) {
    const key = String(dayStart(item.updated_at));
    let group = out[out.length - 1];
    if (!group || group.key !== key) {
      group = { key, label: dayLabel(item.updated_at, now, today, yesterday), items: [] };
      out.push(group);
    }
    group.items.push(item);
  }
  return out;
}

/** Время последней записи «14:05». */
export function timeLabel(ms: number): string {
  const d = new Date(ms);
  return `${String(d.getHours()).padStart(2, "0")}:${String(d.getMinutes()).padStart(2, "0")}`;
}

/** Ключ беседы в списке: номер уникален только внутри агента. */
export function sessionKey(item: Pick<AgentSessionItem, "agent" | "session_id">): string {
  return `${item.agent}/${item.session_id}`;
}

/** Склеить страницы без повторов (список мог сдвинуться между запросами). */
export function mergePages(prev: AgentSessionItem[], next: AgentSessionItem[]): AgentSessionItem[] {
  const seen = new Set(prev.map(sessionKey));
  return [...prev, ...next.filter((it) => !seen.has(sessionKey(it)))];
}
