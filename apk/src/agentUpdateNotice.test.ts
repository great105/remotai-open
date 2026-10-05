/**
 * Правило плашки обновления агента и её память «скрыто на эту версию».
 * fetchLatestVersion здесь не дёргаем: она ходит на релей, тесту сеть не нужна.
 */
import { beforeEach, describe, expect, it } from "vitest";
import {
  agentUpdateNotice,
  agentUpdateNoticeDismissed,
  dismissAgentUpdateNotice,
} from "./agentUpdateNotice";

describe("agentUpdateNotice", () => {
  it("сравнивать не с чем — плашки нет (не выдумываем тревогу)", () => {
    expect(agentUpdateNotice("", "2.41.2")).toBeNull();
    expect(agentUpdateNotice("2.41.2", "")).toBeNull();
  });

  it("версия не отстала — новости нет", () => {
    expect(agentUpdateNotice("2.41.2", "2.41.2")).toBeNull();
    expect(agentUpdateNotice("2.42.0", "2.41.2")).toBeNull();
  });

  it("отстала в пределах мажора — спокойное «обновится само»", () => {
    expect(agentUpdateNotice("2.39.0", "2.41.2")).toEqual({ state: "auto", latest: "2.41.2" });
  });

  it("отстала на мажор — это «сильно устарел», и только оно требует действия", () => {
    expect(agentUpdateNotice("1.9.0", "2.41.2")).toEqual({ state: "stuck", latest: "2.41.2" });
  });
});

describe("dismiss per версия", () => {
  // Окружение тестов — node, localStorage в нём нет: подменяем минимальным
  // стабом (сам модуль и без него работает — все обращения в try/catch).
  const store = new Map<string, string>();
  beforeEach(() => {
    store.clear();
    (globalThis as { localStorage?: unknown }).localStorage = {
      getItem: (k: string) => store.get(k) ?? null,
      setItem: (k: string, v: string) => { store.set(k, v); },
      removeItem: (k: string) => { store.delete(k); },
    };
  });

  it("не скрытая версия не считается скрытой", () => {
    expect(agentUpdateNoticeDismissed("2.41.2")).toBe(false);
  });

  it("скрытие запоминается ровно на ту версию", () => {
    dismissAgentUpdateNotice("2.41.2");
    expect(agentUpdateNoticeDismissed("2.41.2")).toBe(true);
    // Следующая версия — новая новость, её «скрыть» заранее нельзя.
    expect(agentUpdateNoticeDismissed("2.42.0")).toBe(false);
  });
});
