/**
 * Правило «Назад» — проверяется в node, без React и DOM.
 *
 * Аудит ИА 02.09.2026: на четырёх экранах «←» уходила к жёсткому родителю,
 * а «Мои компьютеры» были корнем безусловно — системная «Назад» сворачивала
 * приложение при живой стрелке на главную. Тесты фиксируют новое правило:
 * корень зависит от состояния, а вторичные экраны шагают по истории.
 */
import { beforeEach, describe, expect, it, vi } from "vitest";

const cfg = vi.hoisted(() => ({
  mode: "cloud" as "cloud" | "self_hosted",
  device: "" as string,
  hasConfig: true,
}));

vi.mock("./config", () => ({
  getMode: () => cfg.mode,
  getSelectedDeviceId: () => cfg.device,
  hasServerConfig: () => cfg.hasConfig,
}));

import { backFallback, goBack, isRootPath, isInnerPath } from "./navBack";

const loc = (pathname: string, extra: { search?: string; state?: unknown; key?: string } = {}) => ({
  pathname,
  search: extra.search ?? "",
  state: extra.state ?? null,
  key: extra.key ?? "default",
});

describe("isRootPath — корень зависит от состояния", () => {
  beforeEach(() => { cfg.mode = "cloud"; cfg.device = "a7a22f3d10bf"; cfg.hasConfig = true; });

  it("главная — корень всегда", () => {
    expect(isRootPath("/")).toBe(true);
  });

  it("«Мои компьютеры» — корень только в облаке без выбранной машины", () => {
    cfg.device = "";
    expect(isRootPath("/infrastructure")).toBe(true);
    expect(isRootPath("/devices")).toBe(true);
    cfg.device = "a7a22f3d10bf";
    expect(isRootPath("/infrastructure")).toBe(false);
    cfg.mode = "self_hosted"; cfg.device = "";
    expect(isRootPath("/infrastructure")).toBe(false);
  });

  it("экран входа — корень только при пустой конфигурации", () => {
    cfg.hasConfig = false;
    expect(isRootPath("/cloud-login")).toBe(true);
    expect(isRootPath("/login")).toBe(true);
    cfg.hasConfig = true; // «Войти в аккаунт» из живого LAN-подключения
    expect(isRootPath("/cloud-login")).toBe(false);
  });

  it("обычные разделы корнем не бывают", () => {
    for (const p of ["/settings", "/pty", "/files", "/ssh", "/account", "/agents"]) {
      expect(isRootPath(p)).toBe(false);
    }
  });
});

describe("goBack — шаг по истории, без неё — к родителю", () => {
  beforeEach(() => { cfg.mode = "cloud"; cfg.device = "a7a22f3d10bf"; cfg.hasConfig = true; });

  const run = (location: ReturnType<typeof loc>) => {
    const calls: unknown[] = [];
    goBack(((to: unknown) => { calls.push(to); }) as never, location);
    return calls[0];
  };

  it("настройки, кабинет, SSH и «Мои компьютеры» шагают по истории, когда она есть", () => {
    for (const p of ["/settings", "/account", "/plan", "/ssh", "/ssh/host-1", "/infrastructure", "/cloud-login"]) {
      expect(run(loc(p, { key: "k2" }))).toBe(-1);
    }
  });

  it("без истории те же экраны идут к логическому родителю", () => {
    expect(run(loc("/settings"))).toBe("/");
    expect(run(loc("/account"))).toBe("/");
    expect(run(loc("/ssh"))).toBe("/");
    expect(run(loc("/ssh/host-1"))).toBe("/ssh");
    expect(run(loc("/ssh-files"))).toBe("/ssh");
    expect(run(loc("/agents/sessions"))).toBe("/agents");
  });

  it("из всех бесед возвращает на прошлый экран, включая справку", () => {
    expect(run(loc("/agents/sessions", { key: "opened-from-guide" }))).toBe(-1);
  });

  it("раздел настроек по прямой ссылке возвращает в оглавление, а обычный переход — по истории", () => {
    expect(run(loc("/settings", { search: "?section=notifications" }))).toBe("/settings");
    expect(run(loc("/settings", { key: "replacement", state: { settingsIndexRoot: true } }))).toBe("/");
    expect(run(loc("/settings", { search: "?section=account", key: "opened-from-account" }))).toBe(-1);
    expect(run(loc("/settings", { search: "?section=help", state: { from: "/plan" } }))).toBe("/plan");
  });

  it("явный родитель из state.from или ?from= сильнее истории у экранов вне списка", () => {
    expect(backFallback(loc("/pty/abc", { state: { from: "/files" } }))).toBe("/files");
    expect(backFallback(loc("/pty/abc", { search: "?from=%2Fssh%2Fh1" }))).toBe("/ssh/h1");
    expect(backFallback(loc("/pty/abc", { search: "?from=//evil" }))).toBe("/pty");
  });

  // Корень как дом. Плитка быстрого запуска с ГЛАВНОЙ открывает терминал через
  // `/pty?shell=…`, и до 05.09.2026 «Назад» приводила в список терминалов, где
  // человек не был: проверка пути требовала символ ПОСЛЕ слэша, а у «/» его нет.
  it("«/» — валидный дом для «Назад», а чужой адрес — нет", () => {
    expect(isInnerPath("/")).toBe(true);
    expect(isInnerPath("/files")).toBe(true);
    expect(isInnerPath("//evil.com")).toBe(false);
    expect(isInnerPath("/\\evil.com")).toBe(false);
    expect(isInnerPath("https://evil.com")).toBe(false);
    expect(isInnerPath("")).toBe(false);
    expect(isInnerPath(null)).toBe(false);
    expect(backFallback(loc("/pty/abc", { search: "?from=%2F" }))).toBe("/");
  });

  it("терминал по-прежнему возвращает в список (или в SSH-центр), а не шагает по истории", () => {
    expect(run(loc("/pty/abc", { key: "k9" }))).toBe("/pty");
    expect(run(loc("/pty/abc", { key: "k9", search: "?ssh=1" }))).toBe("/ssh");
  });
});
