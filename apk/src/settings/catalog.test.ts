import { describe, expect, it } from "vitest";
import {
  SETTING_HOMES,
  placeCatalog,
  placeSetting,
  settingValueText,
  settingWireValue,
  type SettingSpec,
} from "./catalog";

const spec = (over: Partial<SettingSpec>): SettingSpec => ({
  key: "k", title: "T", hint: "H", type: "bool", risk: "safe", ...over,
});

describe("раскладка каталога настроек", () => {
  it("безопасная настройка без своего места управляется прямо здесь", () => {
    // Сторож зависшего VPN — ровно этот случай: он есть на компьютере с 2.54.0,
    // агент про него знает, а в приложении его не было НИ В КАКОМ виде.
    const got = placeSetting(spec({ key: "vpn_watchdog", risk: "safe" }));
    expect(got.kind).toBe("editable");
  });

  it("опасная без своего места — только показ: apply её не меняет", () => {
    // Граница власти (решение владельца): POST /api/settings/apply отвечает
    // отказом на dangerous. Нарисовать тумблер значит пообещать несбыточное.
    const got = placeSetting(spec({ key: "peer_access", type: "string", risk: "dangerous" }));
    expect(got.kind).toBe("readonly");
  });

  it("автозапуск переключается ЗДЕСЬ, хотя он опасный", () => {
    // Граница «опасное — только владелец» стоит на apply и написана для
    // AI-АГЕНТА. У человека автозапуск всегда переключался своей ручкой, но из
    // настроек туда вела дверь в «Панель ПК» — владелец на это и пожаловался:
    // «нажимаешь автозапуск, переходит на панель ПК».
    const got = placeSetting(spec({ key: "autostart", risk: "dangerous" }));
    expect(got.kind).toBe("editable");
  });

  it("то, что из приложения не поменять, остаётся только показом", () => {
    // Белый список короткий намеренно: у порта, доступа с других устройств и
    // облачной привязки человеческого пути из приложения нет, и рисовать им
    // переключатель значило бы обещать несбыточное.
    for (const key of ["web_port", "peer_access", "cloud"]) {
      expect(placeSetting(spec({ key, risk: "dangerous" })).kind, key).toBe("readonly");
    }
  });

  it("дверей на другие экраны не осталось вовсе", () => {
    // «Если уж у нас настройки — чтобы всё можно было в них настраивать, а не
    // переходить по экранам» (владелец, 09.08.2026).
    expect(Object.keys(SETTING_HOMES)).toHaveLength(0);
  });

  it("незнакомая настройка не теряется — она появляется сама", () => {
    // Ради этого раскладка и живёт в данных: новая настройка на сервере обязана
    // показаться человеку без правки фронта (то же правило, что у реестра
    // агентов). Молча пропасть она не должна.
    const got = placeCatalog([spec({ key: "совершенно_новая", risk: "safe" })]);
    expect(got).toHaveLength(1);
    expect(got[0].kind).toBe("editable");
  });

  it("порядок сервера сохраняется, мусор отбрасывается", () => {
    const got = placeCatalog([
      spec({ key: "a" }),
      null as unknown as SettingSpec,
      spec({ key: "" }),
      spec({ key: "b" }),
    ]);
    expect(got.map((p) => p.spec.key)).toEqual(["a", "b"]);
  });

  it("карта не ведёт на САМ экран настроек", () => {
    // Дверь «настроить в Настройках», нажатая в Настройках, ведёт в никуда.
    // Ровно это и было 09.08.2026 у рабочей папки, папки входящих и
    // уведомлений — плюс те же настройки вторым экземпляром в «Расширенных».
    for (const [key, home] of Object.entries(SETTING_HOMES)) {
      expect(home.route, `${key} ведёт на сам экран настроек`).not.toBe("/settings");
    }
  });

  it("каждое место карты — существующий маршрут приложения", () => {
    // Опечатка в route увела бы человека на «*» → главную, и настройка стала бы
    // недостижимой при видимой кнопке.
    const routes = new Set(["/agents", "/settings", "/panel", "/pty", "/system"]);
    for (const [key, home] of Object.entries(SETTING_HOMES)) {
      expect(routes.has(home.route), `${key} ведёт в неизвестный ${home.route}`).toBe(true);
    }
  });
});

describe("значения настроек", () => {
  const words = { on: "включён", off: "выключен", unset: "—" };

  it("тумблер читается словами, а не true/false", () => {
    expect(settingValueText(spec({ value: true }), words)).toBe("включён");
    expect(settingValueText(spec({ value: false }), words)).toBe("выключен");
  });

  it("пустая строка и отсутствие значения — прочерк, а не «undefined»", () => {
    expect(settingValueText(spec({ type: "path", value: "" }), words)).toBe("—");
    expect(settingValueText(spec({ type: "path" }), words)).toBe("—");
  });

  it("на сервер уходит строка — её же присылает агент", () => {
    expect(settingWireValue(spec({ type: "bool" }), true)).toBe("true");
    expect(settingWireValue(spec({ type: "bool" }), false)).toBe("false");
    expect(settingWireValue(spec({ type: "int" }), 4)).toBe("4");
    expect(settingWireValue(spec({ type: "path" }), undefined)).toBe("");
  });
});
