/**
 * Свои команды человека — ОДИН список, живущий на компьютере.
 *
 * Что было. Своих команд в продукте оказалось ДВА независимых списка, и оба в
 * localStorage ПУЛЬТА:
 *   • ряд под терминалом — `ptyQuickCmdsCustom`;
 *   • шторка ⚡ — `tg.snippets.custom.v1`.
 * Хранилище своё у каждого пульта (приложение на телефоне, окно exe на
 * 127.0.0.1, браузер, вебвью Telegram), поэтому команда, заведённая с телефона,
 * в окне на компьютере просто отсутствовала. И два списка не знали друг о
 * друге: замер — положил запись в один, в другом её нет, поиска между ними нет.
 * Плюс три разных названия на два раздела («Быстрые команды», «⚡ Команды»,
 * «Горячие команды») при действующем правиле «один раздел — одно имя».
 *
 * Что стало. Список один и лежит на компьютере (`/api/commands` →
 * `~/.tgcontrol-commands.json`) — ровно так, как давно работают пресеты. Пульт
 * при первом обращении молча переносит туда обе свои локальные пачки и дальше
 * читает с компьютера.
 *
 * ВАЖНО про `pinned`: он меняет не только МЕСТО кнопки, но и ПУТЬ ОТПРАВКИ.
 * Закреплённая стоит в ряду под терминалом и уходит по тапу сразу, без вопроса
 * «команда уйдёт агенту в чат» — ряд держит и текстовые заготовки для агента,
 * это решение владельца (2.46.12). Незакреплённая живёт в шторке и по тапу
 * ВСТАВЛЯЕТСЯ в поле ввода: отправляет её человек сам.
 */
import { getUserCommands, saveUserCommand, deleteUserCommand, importUserCommands } from "../api";
import type { UserCommand } from "../api";

/** Старые ключи пульта. Читаются один раз — ради переноса. */
const LEGACY_ROW_KEY = "ptyQuickCmdsCustom";
const LEGACY_SNIPPETS_KEY = "tg.snippets.custom.v1";
/** Отметка «этот пульт своё уже отдал»: перенос повторять незачем. */
const MIGRATED_KEY = "pty.commands.migrated.v1";

function readLegacyRow(): string[] {
  try {
    const raw = JSON.parse(localStorage.getItem(LEGACY_ROW_KEY) || "[]");
    return Array.isArray(raw) ? raw.filter((x): x is string => typeof x === "string" && !!x.trim()) : [];
  } catch { return []; }
}

function readLegacySnippets(): { label: string; cmd: string }[] {
  try {
    const raw = JSON.parse(localStorage.getItem(LEGACY_SNIPPETS_KEY) || "[]");
    if (!Array.isArray(raw)) return [];
    return raw
      .map((x) => (x && typeof x === "object"
        ? { label: String((x as any).label || (x as any).title || ""), cmd: String((x as any).cmd || (x as any).command || "") }
        : { label: "", cmd: typeof x === "string" ? x : "" }))
      .filter((x) => !!x.cmd.trim());
  } catch { return []; }
}

/** Что этот пульт хранил локально — в виде записей общего списка. */
export function legacyLocalCommands(): Partial<UserCommand>[] {
  const out: Partial<UserCommand>[] = [];
  const seen = new Set<string>();
  // Ряд — это закреплённые: человек их туда и ставил, чтобы были под рукой.
  for (const cmd of readLegacyRow()) {
    const c = cmd.trim();
    if (!c || seen.has(c)) continue;
    seen.add(c);
    out.push({ cmd: c, pinned: true });
  }
  for (const s of readLegacySnippets()) {
    const c = s.cmd.trim();
    if (!c || seen.has(c)) continue;
    seen.add(c);
    out.push({ cmd: c, label: s.label || undefined, pinned: false });
  }
  return out;
}

function markMigrated(): void {
  try { localStorage.setItem(MIGRATED_KEY, "1"); } catch { /* приватный режим */ }
}

function alreadyMigrated(): boolean {
  try { return localStorage.getItem(MIGRATED_KEY) === "1"; } catch { return false; }
}

/**
 * Загрузить список с компьютера, при первом обращении перенеся своё локальное.
 *
 * Перенос ОДИН РАЗ НА ПУЛЬТ и только когда есть что переносить: сервер сам
 * схлопывает дубли по тексту команды, поэтому повторный вызов безопасен, но
 * гонять его на каждое открытие незачем.
 *
 * Отказ сервера — не повод показать пустоту: возвращаем то, что лежит локально,
 * чтобы кнопки не исчезли у человека с оборванной связью.
 */
export async function loadCommands(): Promise<UserCommand[]> {
  try {
    if (!alreadyMigrated()) {
      const mine = legacyLocalCommands();
      if (mine.length > 0) {
        const res = await importUserCommands(mine);
        markMigrated();
        return res.commands || [];
      }
      markMigrated();
    }
    const res = await getUserCommands();
    return res.commands || [];
  } catch {
    return legacyLocalCommands().map((c, i) => ({ id: `local-${i}`, cmd: c.cmd || "", label: c.label, pinned: c.pinned }));
  }
}

/** Добавить или изменить. Возвращает весь список — источник правды один. */
export async function upsertCommand(cmd: Partial<UserCommand>): Promise<{ commands: UserCommand[]; existed: boolean }> {
  const res = await saveUserCommand(cmd);
  return { commands: res.commands || [], existed: Boolean(res.existed) };
}

export async function removeCommand(id: string): Promise<UserCommand[]> {
  const res = await deleteUserCommand(id);
  return res.commands || [];
}

/** Закреплённые — те, что стоят в ряду под терминалом. */
export function pinnedOf(list: UserCommand[]): UserCommand[] {
  return list.filter((c) => c.pinned && !!c.cmd);
}

/** Подпись кнопки: имя, если человек его дал, иначе сама команда. */
export function commandLabel(c: UserCommand): string {
  return (c.label || "").trim() || c.cmd;
}
