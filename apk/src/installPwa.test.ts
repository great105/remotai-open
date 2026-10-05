import { beforeEach, describe, expect, it, vi } from "vitest";

const mocks = vi.hoisted(() => ({ native: false, telegram: vi.fn() }));
vi.mock("./config", () => ({ get isNativeApp() { return mocks.native; } }));
vi.mock("./telegram", () => ({ getTelegram: mocks.telegram }));
import { iosInstallHint, isInstallHintSnoozed, snoozeInstallHint, type InstallEnv } from "./installPwa";

// Живые строки, а не выдуманные: подсказка ошибается ровно на них.
const UA = {
  safari: "Mozilla/5.0 (iPhone; CPU iPhone OS 17_5 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.5 Mobile/15E148 Safari/604.1",
  chrome: "Mozilla/5.0 (iPhone; CPU iPhone OS 17_5 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) CriOS/125.0.6422.80 Mobile/15E148 Safari/604.1",
  // Ссылка, открытая ИЗ чата Telegram: встроенный WKWebView без Version/.
  tgWebview: "Mozilla/5.0 (iPhone; CPU iPhone OS 17_5 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Mobile/15E148",
  ipadDesktop: "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.5 Safari/605.1.15",
  android: "Mozilla/5.0 (Linux; Android 14; Pixel 7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/125.0.0.0 Mobile Safari/537.36",
  windows: "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/152.0.0.0 Safari/537.36",
};

const env = (patch: Partial<InstallEnv>): InstallEnv => ({
  ua: UA.safari, standalone: false, maxTouchPoints: 5, native: false, telegram: false, ...patch,
});

describe("iosInstallHint", () => {
  it("в Safari на iPhone показывает шаги установки", () => {
    expect(iosInstallHint(env({}))).toBe("steps");
  });

  it("в других браузерах iOS отправляет в Safari", () => {
    // «На экран Домой» там либо нет, либо она в другом меню — не гадаем.
    expect(iosInstallHint(env({ ua: UA.chrome }))).toBe("open-in-safari");
    expect(iosInstallHint(env({ ua: UA.tgWebview }))).toBe("open-in-safari");
  });

  it("молчит там, где ставить нечего", () => {
    expect(iosInstallHint(env({ ua: UA.android }))).toBeNull();
    expect(iosInstallHint(env({ ua: UA.windows, maxTouchPoints: 0 }))).toBeNull();
    // Настоящий Mac и iPad в «десктопном» режиме шлют ОДИН И ТОТ ЖЕ UA.
    // Разделяет их только тач: на Mac подсказка про «Поделиться» — бессмыслица.
    expect(iosInstallHint(env({ ua: UA.ipadDesktop, maxTouchPoints: 0 }))).toBeNull();
    expect(iosInstallHint(env({ ua: UA.ipadDesktop, maxTouchPoints: 5 }))).toBe("steps");
  });

  it("не предлагает установку там, где приложение уже открыто как приложение", () => {
    expect(iosInstallHint(env({ standalone: true }))).toBeNull();
    expect(iosInstallHint(env({ native: true }))).toBeNull();
    // Мини-апп живёт внутри Telegram: на главный экран его не поставить.
    expect(iosInstallHint(env({ telegram: true }))).toBeNull();
  });
});

describe("«Позже»", () => {
  beforeEach(() => {
    const store = new Map<string, string>();
    vi.stubGlobal("localStorage", {
      getItem: (k: string) => store.get(k) ?? null,
      setItem: (k: string, v: string) => void store.set(k, v),
    });
  });

  it("прячет подсказку на месяц и возвращает потом", () => {
    const now = Date.UTC(2026, 8, 8);
    expect(isInstallHintSnoozed(now)).toBe(false);
    snoozeInstallHint(now);
    expect(isInstallHintSnoozed(now + 29 * 86400_000)).toBe(true);
    expect(isInstallHintSnoozed(now + 31 * 86400_000)).toBe(false);
  });
});
