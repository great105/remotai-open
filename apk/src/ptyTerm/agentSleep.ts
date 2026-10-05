// Усыпление агента (internal/pty/agent_sleep.go): человек сам снимает
// простаивающего Claude или Codex кнопкой «Усыпить», терминал остаётся, а ту же
// беседу поднимает команда продолжения по номеру. Правила — здесь, без React,
// чтобы их можно было проверить тестом.

import type { AgentInfo, PtySleep } from "../types";
import { composeLaunch, type LaunchContext } from "./agentLaunch";

// Номер беседы уходит в строку шелла — только безопасные символы (сервер
// проверяет то же самое, это вторая линия).
const SESSION_ID = /^[A-Za-z0-9][A-Za-z0-9._-]{7,127}$/;

/** Агента этого вида можно усыпить: реестр знает, как поднять беседу по номеру. */
export function canSleepAgent(agent: AgentInfo | null | undefined): boolean {
  return Boolean(agent?.resume_id_cli && agent.resume_id_cli.includes("{session_id}"));
}

/**
 * Выключить в ЭТОМ терминале режимы, в которых он сам шлёт байты программе:
 * мышь, фокус, bracketed paste. Снятый агент их не выключил, и шелл получал
 * `ESC[O` от смены фокуса телефона — в PowerShell перед командой пробуждения
 * оставалось `[O` (жалоба владельца 29.09, скриншот). Alt-экран не трогаем.
 */
export const INPUT_MODES_OFF =
  "\x1b[?1000l\x1b[?1002l\x1b[?1003l\x1b[?1004l\x1b[?1005l\x1b[?1006l\x1b[?1015l\x1b[?2004l";

/**
 * Чем очистить строку шелла перед командой пробуждения. Живой прогон 29.09:
 * в PowerShell терминала Remotai строку редактирует консоль Windows, и Ctrl+C
 * вставлялся литералом (`[O[I<^C>claude --resume …` → ошибка разбора), а Esc
 * стирает строку — после него та же беседа поднялась. На POSIX Esc — префикс
 * Meta и склеился бы с первой буквой команды, там Ctrl+U. Enter не годится:
 * выполнил бы недописанную человеком команду.
 */
export function wakeClearLine(posix: boolean): string {
  return posix ? "\x15" : "\x1b";
}

const SLEEP_ERROR_CODES = new Set(["busy", "waking", "not_sleeping", "no_session", "not_agent", "ssh", "dead", "kill_failed"]);

/** Отказ сервера усыпления/пробуждения — его собственными словами, а не «конфликт состояния». */
export function sleepErrorText(e: unknown): string {
  const err = e as { code?: unknown; message?: unknown } | null;
  if (err && typeof err.code === "string" && SLEEP_ERROR_CODES.has(err.code) && typeof err.message === "string" && err.message) {
    return err.message;
  }
  return "";
}

/** Статусы, при которых усыплять нельзя: сервер откажет с тем же смыслом. */
export function sleepBlockedByStatus(status: string | undefined): boolean {
  return status === "working" || status === "stalled" || status === "waiting";
}

/**
 * Команда пробуждения: продолжение ТОЙ беседы тем же агентом в том же режиме.
 * Флаги режима (--dangerously-skip-permissions) сервер снял с командной строки
 * спящего агента; аккаунт, прокси и хуки добавляет composeLaunch, как у любого
 * запуска. Пусто — будить нечем (агент не тот, номер кривой, аккаунт закрыт).
 */
export function wakeCommand(
  agent: AgentInfo | null | undefined,
  sleep: PtySleep | null | undefined,
  ctx: LaunchContext,
): string {
  if (!agent || !sleep || agent.id !== sleep.agent || !canSleepAgent(agent)) return "";
  if (!SESSION_ID.test(sleep.session_id || "")) return "";
  const base = agent.resume_id_cli!.replace("{session_id}", sleep.session_id);
  return composeLaunch(base, { flags: sleep.flags || [], extra: "" }, {
    ...ctx,
    specs: agent.launch_flags || [],
  });
}
