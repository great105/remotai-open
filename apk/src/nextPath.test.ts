import { describe, it, expect } from "vitest";
import { safeNextPath, loginNextPath } from "./nextPath";

describe("safeNextPath", () => {
  it("свои экраны пропускает вместе с запросом", () => {
    expect(safeNextPath("/pty")).toBe("/pty");
    expect(safeNextPath("/pty/abc123")).toBe("/pty/abc123");
    expect(safeNextPath("/files?cwd=%2Fhome")).toBe("/files?cwd=%2Fhome");
    expect(safeNextPath("/ssh?host=srv&ssh=1")).toBe("/ssh?host=srv&ssh=1");
    expect(safeNextPath("/infrastructure?add=1&type=server")).toBe("/infrastructure?add=1&type=server");
  });

  it("чужое и подозрительное уводит на главную", () => {
    expect(safeNextPath("https://example.com")).toBe("/");
    expect(safeNextPath("//example.com")).toBe("/");
    expect(safeNextPath("/pty#top")).toBe("/");
    expect(safeNextPath("/nope")).toBe("/"); // раздела нет в списке
    expect(safeNextPath("/settings")).toBe("/settings"); // аккаунтные разделы — свои экраны
    expect(safeNextPath("/plan")).toBe("/plan"); // сюда возвращает ЮKassa после оплаты
    expect(safeNextPath("/agents")).toBe("/agents");
    expect(safeNextPath("/pty/a/b")).toBe("/"); // два сегмента — не наш адрес
  });

  it("пустое значение — это главная, а не пустая строка", () => {
    expect(safeNextPath("")).toBe("/");
    expect(safeNextPath(null)).toBe("/");
    expect(safeNextPath(undefined)).toBe("/");
  });
});

describe("loginNextPath", () => {
  it("сохраняет выбор сервера в адресе после перезагрузки входа", () => {
    expect(loginNextPath(null, "?next=%2Finfrastructure%3Fadd%3D1%26type%3Dserver")).toBe("/infrastructure?add=1&type=server");
  });
  it("сохраняет существующий вход в личный кабинет из настроек", () => {
    expect(loginNextPath({ next: "/account" }, "")).toBe("/account");
  });
  it("не направляет на чужой адрес или обратно на экран входа", () => {
    expect(loginNextPath(null, "?next=https%3A%2F%2Fevil.test")).toBe("/");
    expect(loginNextPath({ next: "//evil.test" }, "")).toBe("/");
    expect(loginNextPath(null, "?next=%2Fcloud-login")).toBe("/");
    expect(loginNextPath({ next: 12 }, "")).toBe("/");
  });
});
