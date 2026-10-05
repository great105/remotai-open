/**
 * Что человек читает вместо кода ошибки.
 *
 * Здесь закреплены правила, из-за которых в проде уже случались неверные
 * советы: «Failed to fetch» на экране входа, «сессия истекла» вместо «роль
 * только просмотр», «Ошибка сервера — попробуйте позже» у выключенного
 * компьютера и совет отвязать рабочий ПК на любом 409.
 */
import { beforeEach, describe, expect, it } from "vitest";
import {
  ApiError,
  ERR_NETWORK,
  ERR_NETWORK_CLOUD,
  isNetworkFailure,
  isPcOffline,
  mapApiError,
  setNetworkContext,
  setPcLiveProbe,
} from "./api-core";

beforeEach(() => {
  // Маршрут — глобальное состояние модуля; каждый случай задаёт его явно,
  // иначе тексты «нет связи» зависели бы от порядка тестов.
  setNetworkContext({ route: "unknown" });
  // Проба живости ПК — тоже глобальная: снимаем, чтобы «502 = офлайн»
  // проверялось в исходных условиях независимо от порядка тестов.
  setPcLiveProbe(null);
});

describe("isNetworkFailure", () => {
  it("TypeError от fetch — сетевой сбой", () => {
    expect(isNetworkFailure(new TypeError("Failed to fetch"))).toBe(true);
    expect(isNetworkFailure({ message: "NetworkError when attempting to fetch resource" })).toBe(true);
    expect(isNetworkFailure({ message: "Load failed" })).toBe(true);
  });

  it("ответ сервера с кодом состояния — не сеть", () => {
    expect(isNetworkFailure(new ApiError("нет прав", 403))).toBe(false);
  });

  it("отменённый и просроченный запрос — не сеть", () => {
    expect(isNetworkFailure({ code: "aborted" })).toBe(false);
    expect(isNetworkFailure({ code: "timeout" })).toBe(false);
  });
});

describe("isPcOffline", () => {
  it("машинный код релея", () => {
    expect(isPcOffline({ code: "pc_offline" })).toBe(true);
    expect(isPcOffline({ code: "pc_timeout" })).toBe(true);
  });

  it("старый релей говорит только текстом", () => {
    expect(isPcOffline({ message: "agent offline" })).toBe(true);
    expect(isPcOffline({ message: "agent did not respond" })).toBe(true);
  });

  it("502/504 — агента за релеем нет", () => {
    expect(isPcOffline(new ApiError("bad gateway", 502))).toBe(true);
  });

  it("502 при ДОКАЗАННО живом ПК — это не «компьютер выключен»", () => {
    // Живая жалоба из аудита путей 29.08.2026: зелёный индикатор «на связи» и
    // одновременно красное «компьютер не в сети, включите его».
    setPcLiveProbe(() => true);
    expect(isPcOffline(new ApiError("bad gateway", 502))).toBe(false);
    expect(isPcOffline(new ApiError("gateway timeout", 504))).toBe(false);
  });

  it("проба сказала «не на связи» — 502 снова означает офлайн", () => {
    setPcLiveProbe(() => false);
    expect(isPcOffline(new ApiError("bad gateway", 502))).toBe(true);
  });

  it("проба упала — считаем, что подтверждения нет", () => {
    setPcLiveProbe(() => { throw new Error("канал недоступен"); });
    expect(isPcOffline(new ApiError("bad gateway", 502))).toBe(true);
  });

  it("явный код релея сильнее пробы: pc_offline остаётся офлайном", () => {
    setPcLiveProbe(() => true);
    expect(isPcOffline({ code: "pc_offline" })).toBe(true);
  });

  it("сетевой сбой на ПРЯМОМ пути — это выключенный компьютер", () => {
    expect(isPcOffline(new ApiError("Компьютер не отвечает", 0, ERR_NETWORK))).toBe(true);
  });

  it("тот же сбой в облаке — это не «ПК выключен», а «нет связи с сервисом»", () => {
    expect(isPcOffline(new ApiError("Нет связи с сервисом", 0, ERR_NETWORK_CLOUD))).toBe(false);
  });
});

describe("mapApiError", () => {
  it("отказ денежного гейта релея назван своими словами", () => {
    // Релей отвечает 402 + code subscription_required (cloud_access.go).
    const text = mapApiError(new ApiError("subscription required", 402, "subscription_required"));
    expect(text).toMatch(/пробн|Про/i);
    expect(text).not.toMatch(/лимит компьютеров/i);
  });

  it("503 называет причину и срок, а не «ошибка сервера»", () => {
    const text = mapApiError(new ApiError("service unavailable", 503));
    expect(text).toMatch(/недоступен/i);
    expect(text).toMatch(/перезапуск/i);
    expect(text).not.toMatch(/^Ошибка сервера/);
  });

  it("машинный код важнее статуса: 403 no_permission — это не протухшая сессия", () => {
    const text = mapApiError(new ApiError("forbidden", 403, "no_permission"));
    expect(text).toBe("Недостаточно прав для этой операции.");
  });

  it("403 viewer_read_only называет роль, а не «войдите заново»", () => {
    expect(mapApiError(new ApiError("forbidden", 403, "viewer_read_only")))
      .toContain("только просмотр");
  });

  it("409 про пейринг советует «Отключить облако» — и только он", () => {
    expect(mapApiError(new ApiError("conflict", 409, "device_taken"))).toContain("Отключить облако");
    expect(mapApiError(new ApiError("conflict", 409, "workspace_not_empty"))).not.toContain("Отключить облако");
    expect(mapApiError(new ApiError("conflict", 409))).not.toContain("Отключить облако");
  });

  it("выключенный компьютер объясняется до общей ветки 5xx", () => {
    expect(mapApiError(new ApiError("bad gateway", 502))).toBe(
      "Компьютер не в сети. Включите его — Remotai подключится сам.",
    );
  });

  it("сырое «Failed to fetch» до человека не доезжает", () => {
    const text = mapApiError(new TypeError("Failed to fetch"));
    expect(text).not.toMatch(/failed to fetch/i);
    expect(text).toBe("Нет связи. Проверьте интернет и что компьютер включён.");
  });

  it("в облаке сбой связи говорит про сервис и VPN", () => {
    setNetworkContext({ route: "cloud" });
    expect(mapApiError(new TypeError("Failed to fetch"))).toContain("VPN");
  });

  it("лимиты называют выход, а не только запрет", () => {
    expect(mapApiError(new ApiError("limit", 402))).toContain("Мои компьютеры");
    expect(mapApiError(new ApiError("limit", 429, "pty_limit"))).toContain("Закройте ненужный терминал");
  });

  it("никакой текст ошибки не пустой", () => {
    for (const e of [new ApiError("x", 404), new ApiError("x", 500), new ApiError("x", 0), {}, null]) {
      expect(mapApiError(e).trim().length).toBeGreaterThan(0);
    }
  });
});
