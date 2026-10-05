import { describe, expect, it } from "vitest";
import { searchOrUrl, tabAvatarColor, tabLetter } from "./browserNav";

describe("searchOrUrl", () => {
  it("адрес без схемы получает https://", () => {
    expect(searchOrUrl("example.com")).toBe("https://example.com");
    expect(searchOrUrl("example.com/path?q=1")).toBe("https://example.com/path?q=1");
  });

  it("фраза с пробелами — это поисковый запрос", () => {
    expect(searchOrUrl("купить слона оптом")).toBe(
      `https://www.google.com/search?q=${encodeURIComponent("купить слона оптом")}`,
    );
  });

  it("слово без точки — тоже запрос, а не адрес", () => {
    expect(searchOrUrl("слон")).toBe(
      `https://www.google.com/search?q=${encodeURIComponent("слон")}`,
    );
  });

  it("готовую схему не трогаем", () => {
    expect(searchOrUrl("http://example.com")).toBe("http://example.com");
    expect(searchOrUrl("https://example.com")).toBe("https://example.com");
    expect(searchOrUrl("about:blank")).toBe("about:blank");
    expect(searchOrUrl("localhost:8080")).toBe("localhost:8080");
  });

  it("пробелы по краям срезаем, пустой ввод никуда не ведёт", () => {
    expect(searchOrUrl("  example.com  ")).toBe("https://example.com");
    expect(searchOrUrl("   ")).toBe("");
  });
});

describe("tabLetter", () => {
  it("берёт первую букву хоста, а без хоста — заголовка", () => {
    expect(tabLetter("example.com", "")).toBe("E");
    expect(tabLetter("", "Слоны")).toBe("С");
    expect(tabLetter("", "")).toBe("•");
  });
});

describe("tabAvatarColor", () => {
  it("одинаковый хост — одинаковый цвет, разные — почти наверняка разные", () => {
    expect(tabAvatarColor("example.com")).toBe(tabAvatarColor("example.com"));
    expect(tabAvatarColor("example.com")).not.toBe(tabAvatarColor("example.org"));
  });

  it("пустой хост не падает", () => {
    expect(tabAvatarColor("")).toMatch(/^hsl\(/);
  });
});
