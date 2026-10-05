import { afterEach, describe, expect, it } from "vitest";
import { getLanguage, getLocale, initialLanguage, setLanguage, subscribeLanguage } from "./locale";
import { plural, t } from "./i18n";
import { ownedText } from "./ownedText";

afterEach(() => setLanguage("ru"));

describe("language preference", () => {
  it("uses an explicit language, then saved preference, then browser language", () => {
    expect(initialLanguage({ query: "en", saved: "ru", browser: "ru-RU" })).toBe("en");
    expect(initialLanguage({ saved: "ru", browser: "en-US" })).toBe("ru");
    expect(initialLanguage({ browser: "ru-KZ" })).toBe("ru");
    expect(initialLanguage({ browser: "de-DE" })).toBe("en");
    expect(initialLanguage({ query: "invalid", saved: "invalid" })).toBe("ru");
  });

  it("notifies only on an actual language change and supports unsubscribe", () => {
    setLanguage("ru");
    const values: string[] = [];
    const unsubscribe = subscribeLanguage(() => values.push(getLanguage()));
    setLanguage("en"); setLanguage("en"); setLanguage("ru");
    unsubscribe(); setLanguage("en");
    expect(values).toEqual(["en", "ru"]);
    expect(getLocale()).toBe("en-US");
  });
});

describe("translated messages", () => {
  it("changes the same key between complete RU and EN messages", () => {
    setLanguage("ru"); expect(t("generic.hide")).toBe("Скрыть");
    setLanguage("en"); expect(t("generic.hide")).toBe("Hide");
    expect(t("settings.languageValue")).toBe("English");
    expect(ownedText("Агент по умолчанию")).toMatch(/^Default [Aa]gent$/);
    expect(ownedText("My custom project {name}")).toBe("My custom project {name}");
    setLanguage("ru"); expect(t("settings.languageValue")).toBe("Русский");
  });

  it("preserves user text, interpolates values and chooses English plurals", () => {
    setLanguage("en");
    expect(t("files.removeFavorite", { name: "Мой проект" })).toContain("Мой проект");
    expect(t("files.removeFavorite", { name: "Мой {name} проект" })).toContain("Мой {name} проект");
    expect(plural(1, ["agent", "agents", "agents"])).toBe("agent");
    expect(plural(21, ["agent", "agents", "agents"])).toBe("agents");
    expect(t("skills.found", { n: 1 })).toBe("Found 1 skill");
    expect(t("skills.found", { n: 21 })).toBe("Found 21 skills");
    setLanguage("ru");
    expect(plural(21, ["агент", "агента", "агентов"])).toBe("агент");
  });
});
