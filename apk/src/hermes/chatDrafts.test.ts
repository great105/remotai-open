import { describe, expect, it } from "vitest";
import { readChatDraft, saveChatDraft } from "./chatDrafts";

function storage() {
  const values = new Map<string, string>();
  return { getItem: (key: string) => values.get(key) ?? null, setItem: (key: string, value: string) => { values.set(key, value); } };
}

describe("черновик отдельного чата Hermes", () => {
  it("сохраняет независимые задачи разных проектов и компьютеров", () => {
    const store = storage();
    for (const [device, sessionId, text, cwd] of [["pc", "site", "Проверь сайт", "/site"], ["pc", "bot", "Проверь бота", "/bot"], ["server", "site", "Собери сайт", "/srv/site"]]) {
      saveChatDraft(store, device, { sessionId, text, cwd });
    }
    expect(readChatDraft(store, "pc", "site")).toMatchObject({ text: "Проверь сайт", cwd: "/site" });
    expect(readChatDraft(store, "pc", "bot")).toMatchObject({ text: "Проверь бота", cwd: "/bot" });
    expect(readChatDraft(store, "server", "site")).toMatchObject({ text: "Собери сайт", cwd: "/srv/site" });
  });

  it("не смешивает новый чат с сохранённым и избегает коллизий идентификаторов", () => {
    const store = storage();
    saveChatDraft(store, "pc:a", { sessionId: "b", text: "Первый", cwd: "" });
    saveChatDraft(store, "pc", { sessionId: "a:b", text: "Второй", cwd: "" });
    saveChatDraft(store, "pc", { sessionId: "", text: "Новый", cwd: "" });
    expect(readChatDraft(store, "pc:a", "b")?.text).toBe("Первый");
    expect(readChatDraft(store, "pc", "a:b")?.text).toBe("Второй");
    expect(readChatDraft(store, "pc", "")?.text).toBe("Новый");
    expect(readChatDraft(store, "pc", "missing")).toBeNull();
  });

  it("не блокирует работу при закрытом хранилище или повреждённых данных", () => {
    expect(readChatDraft(null, "pc", "chat")).toBeNull();
    const restricted = { getItem: () => { throw new Error("blocked"); }, setItem: () => { throw new Error("blocked"); } };
    expect(() => saveChatDraft(restricted, "pc", { sessionId: "chat", text: "task", cwd: "" })).not.toThrow();
    expect(readChatDraft(restricted, "pc", "chat")).toBeNull();
    for (const raw of ["invalid", "null", "{}", '{"sessionId":"other","text":"task","cwd":""}']) {
      expect(readChatDraft({ getItem: () => raw, setItem: () => {} }, "pc", "chat")).toBeNull();
    }
  });
});
