import { describe, expect, it } from "vitest";
import { downloadGoalFor, metrikaAllowed } from "./metrika";

// 07.09.2026: перед рекламным трафиком (РСЯ, сети Директа) Метрика видела только
// лендинг; вехи приложения (регистрация, привязка, агент, оплата) — нет.
describe("Метрика в приложении: где ей можно быть", () => {
  const base = { hostname: "remotai.ru", nativeApp: false, telegramMiniApp: false, analyticsEnabled: true };

  it("веб на remotai.ru с включённой аналитикой — да", () => {
    expect(metrikaAllowed(base)).toBe(true);
    expect(metrikaAllowed({ ...base, hostname: "www.remotai.ru" })).toBe(true);
  });

  it("стенд и чужие домены — нет (стенд однажды накрутил визиты втрое)", () => {
    expect(metrikaAllowed({ ...base, hostname: "127.0.0.1" })).toBe(false);
    expect(metrikaAllowed({ ...base, hostname: "localhost" })).toBe(false);
    expect(metrikaAllowed({ ...base, hostname: "remotai.ru.evil.com" })).toBe(false);
    expect(metrikaAllowed({ ...base, hostname: "" })).toBe(false);
  });

  it("APK и мини-апп Telegram — нет, даже на remotai.ru", () => {
    expect(metrikaAllowed({ ...base, nativeApp: true })).toBe(false);
    expect(metrikaAllowed({ ...base, telegramMiniApp: true })).toBe(false);
  });

  it("человек выключил аналитику в настройках — нет", () => {
    expect(metrikaAllowed({ ...base, analyticsEnabled: false })).toBe(false);
  });
});

describe("цель скачивания по ссылке", () => {
  it("каждая ОС — своя цель; DMG/DEB/RPM больше не «Windows»", () => {
    expect(downloadGoalFor("/download/remotai.apk")).toBe("download_apk");
    expect(downloadGoalFor("/download/remotai-macos.dmg")).toBe("download_mac");
    expect(downloadGoalFor("/download/remotai-linux-amd64.deb")).toBe("download_linux");
    expect(downloadGoalFor("/download/remotai-linux-amd64.rpm")).toBe("download_linux");
    expect(downloadGoalFor("/download/remotai.exe")).toBe("download_win");
    expect(downloadGoalFor("/download/remotai-setup.exe")).toBe("download_win");
    expect(downloadGoalFor("")).toBe("download_win");
  });
});
