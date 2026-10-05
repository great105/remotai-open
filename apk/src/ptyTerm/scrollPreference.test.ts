import { describe, expect, it } from "vitest";
import { readScrollPreference, saveScrollPreference } from "./scrollPreference";

function memoryStorage() {
  const entries = new Map<string, string>();
  return {
    entries,
    getItem: (key: string) => entries.get(key) ?? null,
    setItem: (key: string, value: string) => { entries.set(key, value); },
  };
}

describe("terminal output preference", () => {
  it("starts on Auto and keeps all three explicit choices on reopening", () => {
    const storage = memoryStorage();
    expect(readScrollPreference("pc-a", "term-a", storage)).toBe("auto");
    for (const mode of ["terminal", "agent", "auto"] as const) {
      saveScrollPreference("pc-a", "term-a", mode, storage);
      expect(readScrollPreference("pc-a", "term-a", storage)).toBe(mode);
    }
  });

  it("does not leak a choice into other terminals or computers", () => {
    const storage = memoryStorage();
    saveScrollPreference("pc-a", "term-a", "agent", storage);
    saveScrollPreference("pc-a", "term-b", "terminal", storage);
    expect(readScrollPreference("pc-a", "term-a", storage)).toBe("agent");
    expect(readScrollPreference("pc-a", "term-b", storage)).toBe("terminal");
    expect(readScrollPreference("pc-b", "term-a", storage)).toBe("auto");
    expect(readScrollPreference("pc-a", "term-c", storage)).toBe("auto");
    saveScrollPreference("pc:a", "b", "agent", storage);
    expect(readScrollPreference("pc", "a:b", storage)).toBe("auto");
  });

  it("ignores invalid saved values and an incomplete terminal identity", () => {
    const storage = memoryStorage();
    saveScrollPreference("", "term-a", "agent", storage);
    saveScrollPreference("pc-a", "", "agent", storage);
    expect(storage.entries.size).toBe(0);
    saveScrollPreference("pc-a", "term-a", "agent", storage);
    for (const key of storage.entries.keys()) storage.setItem(key, "unknown");
    expect(readScrollPreference("pc-a", "term-a", storage)).toBe("auto");
  });

  it("keeps terminals usable when browser storage is unavailable", () => {
    const storage = {
      getItem: () => { throw new Error("denied"); },
      setItem: () => { throw new Error("quota"); },
    };
    expect(readScrollPreference("pc", "term", storage)).toBe("auto");
    expect(() => saveScrollPreference("pc", "term", "terminal", storage)).not.toThrow();
  });
});
