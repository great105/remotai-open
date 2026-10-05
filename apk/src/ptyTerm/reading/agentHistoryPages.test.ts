import { describe, expect, it } from "vitest";
import { AgentHistoryError, type AgentHistoryPage } from "./AgentHistoryClient";
import {
  currentHistoryPage, HISTORY_STACK_MAX_CHARS, HISTORY_STACK_MAX_PAGES, historyErrorNotice, historyNeighbourEvicted,
  historyTarget, moveHistory, openHistoryStack, placeHistoryPage, type AgentHistoryStack, type HistoryStackLimits,
} from "./agentHistoryPages";

/**
 * T-23 (L1): огромный старый лог + короткий последний ответ агента. Последний
 * ответ доступен всегда; все заявленные части можно дочитать «старше» до
 * конца и вернуться «новее» к последнему ответу; память ограничена.
 */
const PAGES = 20;
function fixture(source = "session-a"): (cursor: string) => AgentHistoryPage {
  const pages = Array.from({ length: PAGES }, (_, i): AgentHistoryPage => ({
    source, agent: "codex", version: "0.154.0", schema: "codex-app-server-turns-v2",
    // Страница 0 — короткий последний ответ, дальше огромный старый вывод.
    text: i === 0 ? "последний ответ: готово" : `page-${i}\n` + "x".repeat(250_000),
    next: i + 1 < PAGES ? `c${i + 1}` : "", partial: false,
  }));
  return cursor => pages[cursor === "" ? 0 : Number(cursor.slice(1))];
}

function check(stack: AgentHistoryStack, limits: HistoryStackLimits) {
  const loaded = stack.entries.filter(entry => entry.page);
  expect(loaded.length).toBeLessThanOrEqual(limits.pages);
  expect(loaded.reduce((sum, entry) => sum + entry.page!.text.length, 0)).toBeLessThanOrEqual(limits.chars);
  expect(stack.entries[0].page?.text).toBe("последний ответ: готово"); // последний ответ не вытесняется
  expect(currentHistoryPage(stack)).not.toBeNull(); // текущая страница в памяти
}

function walk(stack: AgentHistoryStack, direction: "older" | "newer", read: (cursor: string) => AgentHistoryPage,
  limits: HistoryStackLimits, seen: string[], counters: { fetched: number; markedEvicted: number }) {
  for (;;) {
    const target = historyTarget(stack, direction);
    if (!target) return stack;
    if (historyNeighbourEvicted(stack)) counters.markedEvicted++;
    if (target.cached) stack = moveHistory(stack, target.index);
    else {
      counters.fetched++;
      const next = placeHistoryPage(stack, target.index, target.cursor, read(target.cursor), limits);
      expect(next).not.toBeNull();
      stack = next!;
    }
    check(stack, limits);
    seen.push(currentHistoryPage(stack)!.text.split("\n")[0]);
  }
}

describe("стек страниц истории агента (T-23)", () => {
  it.each([
    ["лимит страниц", { pages: HISTORY_STACK_MAX_PAGES, chars: HISTORY_STACK_MAX_CHARS }],
    ["лимит объёма текста", { pages: HISTORY_STACK_MAX_PAGES, chars: 600_000 }],
  ] as const)("%s: дочитывается до самой старой части и возвращается к последнему ответу", (_name, limits) => {
    const read = fixture();
    let stack = openHistoryStack(read(""));
    check(stack, limits);
    const older: string[] = [], counters = { fetched: 0, markedEvicted: 0 };
    stack = walk(stack, "older", read, limits, older, counters);
    // Все заявленные части прочитаны по порядку, ни одна не пропущена.
    expect(older).toEqual(Array.from({ length: PAGES - 1 }, (_, i) => `page-${i + 1}`));
    expect(stack.evicted).toBeGreaterThan(0);
    const newer: string[] = [];
    stack = walk(stack, "newer", read, limits, newer, counters);
    expect(newer).toEqual([...Array.from({ length: PAGES - 2 }, (_, i) => `page-${PAGES - 2 - i}`), "последний ответ: готово"]);
    expect(stack.position).toBe(0);
    // Выгруженные страницы запрашивались заново тем же курсором и были помечены явно.
    expect(counters.fetched).toBeGreaterThan(PAGES - 1);
    expect(counters.markedEvicted).toBeGreaterThan(0);
  });

  it("вытесняется самое дальнее от места чтения; ближние к читателю страницы остаются в памяти", () => {
    const read = fixture(), limits = { pages: 4, chars: HISTORY_STACK_MAX_CHARS };
    let stack = openHistoryStack(read(""));
    for (let i = 1; i <= 5; i++) stack = placeHistoryPage(stack, i, `c${i}`, read(`c${i}`), limits)!;
    // 0 закреплён, 5 текущая, рядом с читателем 4 и 3; самые дальние 1 и 2 выгружены.
    expect(stack.entries.map(entry => !!entry.page)).toEqual([true, false, false, true, true, true]);
    expect(historyTarget(stack, "newer")).toEqual({ index: 4, cursor: "c4", cached: true });
  });

  it("страница другого источника сбрасывает стек, а не склеивается с ним", () => {
    const read = fixture("session-a"), other = fixture("session-b");
    const stack = openHistoryStack(read(""));
    expect(placeHistoryPage(stack, 1, "c1", other("c1"))).toBeNull();
  });

  it("изменившаяся цепочка при повторном запросе и чужой курсор делают стек недействительным", () => {
    const read = fixture(), limits = { pages: 3, chars: HISTORY_STACK_MAX_CHARS };
    let stack = openHistoryStack(read(""));
    for (let i = 1; i <= 3; i++) stack = placeHistoryPage(stack, i, `c${i}`, read(`c${i}`), limits)!;
    expect(stack.entries[1].page).toBeNull();
    stack = moveHistory(stack, 2);
    expect(placeHistoryPage(stack, 1, "c1", { ...read("c1"), next: "changed" }, limits)).toBeNull();
    expect(placeHistoryPage(stack, 1, "c9", read("c1"), limits)).toBeNull();
    expect(placeHistoryPage(stack, 1, "c1", read("c1"), limits)).not.toBeNull();
  });

  it("у последнего ответа нет «новее», у самой старой части нет «старше»", () => {
    const read = fixture();
    const stack = openHistoryStack(read(""));
    expect(historyTarget(stack, "newer")).toBeNull();
    expect(historyTarget(stack, "older")).toEqual({ index: 1, cursor: "c1", cached: false });
    const lone = openHistoryStack({ ...read(""), next: "" });
    expect(historyTarget(lone, "older")).toBeNull();
  });
});

describe("различение отказов истории агента", () => {
  it("источник сменился, версия не поддержана (с версией), недоступно", () => {
    expect(historyErrorNotice(new AgentHistoryError("history_source_changed"))).toEqual({ key: "pty.readSourceChanged" });
    // PtyTermView бросает обычный Error с кодом, когда сменилась цель ввода.
    expect(historyErrorNotice(new Error("history_source_changed"))).toEqual({ key: "pty.readSourceChanged" });
    expect(historyErrorNotice(new AgentHistoryError("history_version_unsupported", "0.200.0")))
      .toEqual({ key: "pty.readHistoryVersionUnsupported", params: { version: "0.200.0" } });
    expect(historyErrorNotice(new AgentHistoryError("history_version_unsupported")))
      .toEqual({ key: "pty.readHistoryVersionUnsupported", params: { version: "?" } });
    expect(historyErrorNotice(new AgentHistoryError("history_unavailable"))).toEqual({ key: "pty.readUnavailable" });
    expect(historyErrorNotice("boom")).toEqual({ key: "pty.readUnavailable" });
  });
});
