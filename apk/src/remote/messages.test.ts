/**
 * Что человек читает, когда экран компьютера не открылся, и когда пробовать
 * снова бессмысленно. Оба правила ломались в проде: 502 от релея превращался в
 * общее «Ошибка подключения», а выключенный компьютер получал минуту спиннера
 * вместо честного «Повторить».
 */
import { describe, expect, it } from "vitest";
import { clipboardErrorMessage, fatalRemoteError, formatTraffic, remoteErrorMessage } from "./messages";

describe("remoteErrorMessage", () => {
  it("выключенный компьютер объясняется раньше всех прочих причин", () => {
    expect(remoteErrorMessage({ code: "pc_offline", status: 502 })).toContain("не в сети");
  });

  it("машинные коды агента названы своими словами", () => {
    expect(remoteErrorMessage({ code: "no_display" })).not.toBe("");
    expect(remoteErrorMessage({ code: "service_mode" })).not.toBe(remoteErrorMessage({ code: "no_display" }));
    expect(remoteErrorMessage({ code: "rate_limited" })).not.toBe(remoteErrorMessage({ code: "no_display" }));
  });

  it("сырой английский текст ошибки до человека не доезжает", () => {
    const text = remoteErrorMessage({ status: 500, message: "no displays found" });
    expect(text).not.toMatch(/no displays found|Failed to fetch/i);
    expect(text.trim().length).toBeGreaterThan(0);
  });

  it("ошибка без кода и статуса — всё равно понятная строка", () => {
    expect(remoteErrorMessage(null).trim().length).toBeGreaterThan(0);
    expect(remoteErrorMessage(new Error("boom")).trim().length).toBeGreaterThan(0);
  });
});

describe("fatalRemoteError", () => {
  it("отказы, которые повтором не лечатся", () => {
    for (const code of ["no_display", "service_mode", "remote_limit", "rate_limited"]) {
      expect(fatalRemoteError({ code })).toBe(true);
    }
    for (const status of [401, 403, 409, 429, 503]) {
      expect(fatalRemoteError({ status })).toBe(true);
    }
  });

  it("выключенный компьютер — тоже приговор: десять попыток его не разбудят", () => {
    expect(fatalRemoteError({ code: "pc_offline" })).toBe(true);
  });

  it("временный сбой стоит пережить молча — иначе оборвём сеанс на ровном месте", () => {
    expect(fatalRemoteError({ status: 500 })).toBe(false);
    expect(fatalRemoteError({ code: "capture_failed" })).toBe(false);
    expect(fatalRemoteError(null)).toBe(false);
  });
});

describe("formatTraffic", () => {
  it("единицы растут вместе с числом", () => {
    expect(formatTraffic(512)).toBe("512 Б");
    expect(formatTraffic(2048)).toBe("2.0 КБ");
    expect(formatTraffic(150 * 1024 * 1024)).toBe("150.0 МБ");
  });
});

describe("clipboardErrorMessage", () => {
  it("у каждого отказа буфера своя причина, а не общий «не получилось»", () => {
    const codes = ["xclip_missing", "clipboard_empty", "clipboard_busy", "image_too_large", "invalid_image"];
    const texts = codes.map(clipboardErrorMessage);
    expect(new Set(texts).size).toBe(codes.length);
    for (const text of texts) expect(text.trim().length).toBeGreaterThan(0);
  });

  it("незнакомый код не оставляет человека без объяснения", () => {
    expect(clipboardErrorMessage("что-то новое").trim().length).toBeGreaterThan(0);
  });
});
