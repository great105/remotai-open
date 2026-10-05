import { AgentHistoryError, type AgentHistoryPage } from "./AgentHistoryClient";

/**
 * Ограниченный стек прочитанных страниц истории агента (T-23, ST-10).
 *
 * Раньше «Предыдущая страница» ЗАМЕНЯЛА страницу, а «Следующей» не было:
 * вернуться к последнему ответу можно было только закрыв чтение. Теперь
 * страницы лежат стопкой: 0 — последний ответ (курсор ""), дальше старше.
 * Курсор каждой страницы известен из `next` предыдущей, поэтому страницу,
 * выгруженную ради памяти, можно запросить заново тем же курсором.
 *
 * Правила:
 *   - стек привязан к page.source: страница другого источника его сбрасывает
 *     (другой разговор нельзя склеивать с этим, I-11);
 *   - в памяти не больше 8 страниц и 2 Ми символов текста (I-11);
 *   - последний ответ и текущая страница не вытесняются никогда; остальные
 *     вытесняются начиная с самой дальней от места чтения, с явной пометкой
 *     (page = null, счётчик evicted) — уведомление говорит об этом прямо;
 *   - повторно запрошенная страница обязана совпасть с цепочкой (тот же
 *     source и тот же next): страницы сервера заморожены, расхождение значит,
 *     что история поменялась, и стек сбрасывается, а не молча подменяется.
 */
export const HISTORY_STACK_MAX_PAGES = 8;
export const HISTORY_STACK_MAX_CHARS = 2 * 1024 * 1024;

export interface HistoryStackLimits { pages: number; chars: number }
const DEFAULT_LIMITS: HistoryStackLimits = { pages: HISTORY_STACK_MAX_PAGES, chars: HISTORY_STACK_MAX_CHARS };

export interface HistoryStackEntry {
  /** Курсор, которым страница запрашивается ("" — последний ответ). */
  readonly cursor: string;
  /** null — страница вытеснена из памяти и будет запрошена заново. */
  readonly page: AgentHistoryPage | null;
  /** Курсор следующей (более старой) страницы; "" — старше нет. */
  readonly next: string;
}

export interface AgentHistoryStack {
  readonly source: string;
  /** 0 — последний ответ, дальше старше. */
  readonly entries: readonly HistoryStackEntry[];
  readonly position: number;
  /** Сколько раз страницы вытеснялись ради лимита — для честного уведомления. */
  readonly evicted: number;
}

export function openHistoryStack(latest: AgentHistoryPage): AgentHistoryStack {
  return Object.freeze({ source: latest.source, position: 0, evicted: 0,
    entries: Object.freeze([Object.freeze({ cursor: "", page: latest, next: latest.next })]) });
}

export function currentHistoryPage(stack: AgentHistoryStack): AgentHistoryPage | null {
  return stack.entries[stack.position]?.page ?? null;
}

/** Куда ведёт «старше»/«новее»: индекс, курсор и есть ли страница в памяти; null — идти некуда. */
export function historyTarget(stack: AgentHistoryStack, direction: "older" | "newer"):
  { index: number; cursor: string; cached: boolean } | null {
  const current = stack.entries[stack.position];
  if (!current) return null;
  if (direction === "newer") {
    const index = stack.position - 1;
    const entry = stack.entries[index];
    return entry ? { index, cursor: entry.cursor, cached: !!entry.page } : null;
  }
  const index = stack.position + 1;
  const entry = stack.entries[index];
  if (entry) return { index, cursor: entry.cursor, cached: !!entry.page };
  return current.next ? { index, cursor: current.next, cached: false } : null;
}

/** Переход к странице, которая уже в памяти. */
export function moveHistory(stack: AgentHistoryStack, index: number): AgentHistoryStack {
  if (!stack.entries[index]?.page) return stack;
  return Object.freeze({ ...stack, position: index });
}

/** Есть ли рядом с местом чтения выгруженная страница — повод для явного уведомления. */
export function historyNeighbourEvicted(stack: AgentHistoryStack): boolean {
  const before = stack.entries[stack.position - 1], after = stack.entries[stack.position + 1];
  return !!(before && !before.page) || !!(after && !after.page);
}

/**
 * Положить полученную страницу на её место и сделать текущей. null — стек
 * недействителен (другой источник, чужой курсор или изменившаяся цепочка):
 * вызывающий сбрасывает чтение и говорит, что источник изменился.
 */
export function placeHistoryPage(stack: AgentHistoryStack, index: number, cursor: string, page: AgentHistoryPage,
  limits: HistoryStackLimits = DEFAULT_LIMITS): AgentHistoryStack | null {
  if (page.source !== stack.source || index <= 0 || index > stack.entries.length) return null;
  const entries = stack.entries.slice();
  const known = entries[index];
  if (known) {
    // Повторный запрос выгруженной страницы: курсор и продолжение те же.
    if (known.cursor !== cursor || known.next !== page.next) return null;
  } else if (entries[index - 1].next !== cursor) return null;
  entries[index] = Object.freeze({ cursor, page, next: page.next });
  let evicted = stack.evicted;
  const loaded = () => entries.reduce((sum, entry) => sum + (entry.page ? 1 : 0), 0);
  const chars = () => entries.reduce((sum, entry) => sum + (entry.page?.text.length ?? 0), 0);
  while (loaded() > limits.pages || chars() > limits.chars) {
    let victim = -1;
    for (let i = 1; i < entries.length; i++) {
      if (i === index || !entries[i].page) continue;
      // Самая дальняя от места чтения; при равенстве — более старая.
      if (victim < 0 || Math.abs(i - index) >= Math.abs(victim - index)) victim = i;
    }
    if (victim < 0) break; // остались только последний ответ и текущая — меньше нельзя
    entries[victim] = Object.freeze({ ...entries[victim], page: null });
    evicted++;
  }
  return Object.freeze({ source: stack.source, entries: Object.freeze(entries), position: index, evicted });
}

/**
 * Какое сообщение показать при отказе (T-23): «источник сменился»,
 * «версия агента не поддержана» (с версией, код сервера
 * history_version_unsupported) или общее «недоступно». Возвращает ключ i18n,
 * чтобы правило проверялось в node без интерфейса.
 */
export function historyErrorNotice(error: unknown): { key: string; params?: Record<string, string> } {
  const code = error instanceof AgentHistoryError ? error.code : error instanceof Error ? error.message : "";
  if (code === "history_source_changed") return { key: "pty.readSourceChanged" };
  if (code === "history_version_unsupported") {
    const version = error instanceof AgentHistoryError && error.version ? error.version : "?";
    return { key: "pty.readHistoryVersionUnsupported", params: { version } };
  }
  return { key: "pty.readUnavailable" };
}
