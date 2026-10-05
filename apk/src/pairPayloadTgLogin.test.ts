import { describe, expect, it } from "vitest";

import { parseTelegramLoginLink } from "./pairPayload";

describe("parseTelegramLoginLink", () => {
  // Живой случай 23.08: на компьютере нажали «Показать QR», навели на него
  // сканер ВНУТРИ приложения — и он ответил «Это не код Remotai». Формально
  // верно, по делу бесполезно: код показал сам Remotai двумя экранами раньше.
  it("узнаёт QR входа, который показывает Remotai на большом экране", () => {
    const link = parseTelegramLoginLink("https://t.me/Autocode1_bot?start=login_ab12cd34ef");
    expect(link?.token).toBe("ab12cd34ef");
    expect(link?.url).toContain("t.me");
  });

  it("принимает tg:// и www, потому что камера читает что дали", () => {
    expect(parseTelegramLoginLink("tg://resolve?domain=Autocode1_bot&start=login_zz99")?.token).toBe("zz99");
    expect(parseTelegramLoginLink("https://www.t.me/Autocode1_bot?start=login_qq11")?.token).toBe("qq11");
  });

  it("не открывает чужие ссылки на Telegram", () => {
    // Наведение камеры не должно уводить человека по любой ссылке t.me —
    // только по нашей, с параметром входа.
    expect(parseTelegramLoginLink("https://t.me/somechannel")).toBeNull();
    expect(parseTelegramLoginLink("https://t.me/someone?start=hello")).toBeNull();
    expect(parseTelegramLoginLink("https://t.me/bot?start=login_")).toBeNull();
  });

  it("не принимает вообще посторонний QR", () => {
    expect(parseTelegramLoginLink("https://example.com/?start=login_abc")).toBeNull();
    expect(parseTelegramLoginLink("FX42-9KQ7")).toBeNull();
    expect(parseTelegramLoginLink("")).toBeNull();
  });
});

describe("matchScanPayload: три вида кода в одном видоискателе", () => {
  it("QR входа с большого экрана больше не считается чужим", async () => {
    const { matchScanPayload } = await import("./components/QrScanSheet");
    const m = matchScanPayload("https://t.me/Autocode1_bot?start=login_deadbeef");
    expect(m?.kind).toBe("tglogin");
    if (m?.kind === "tglogin") expect(m.token).toBe("deadbeef");
  });

  it("код привязки компьютера по-прежнему главный", async () => {
    const { matchScanPayload } = await import("./components/QrScanSheet");
    const m = matchScanPayload("remotai://pair?relay=https%3A%2F%2Fremotai.ru&code=FX4Z-9KQ7");
    expect(m?.kind).toBe("cloud");
  });

  it("посторонний QR остаётся посторонним", async () => {
    const { matchScanPayload } = await import("./components/QrScanSheet");
    expect(matchScanPayload("https://example.com/hello")).toBeNull();
  });
});
