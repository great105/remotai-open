import { describe, expect, it } from "vitest";
import { ScrollRouter, WHEEL_TAIL_MAX_MS } from "./ScrollRouter";

describe("scroll intent ownership", () => {
  it("keeps touch and inertia local at the edge and after a direction change", () => {
    const router = new ScrollRouter<string>();
    router.beginTouch("process-1");
    expect(router.route("touch", "process-1", 0, () => "local")?.destination).toBe("local");
    expect(router.route("touch", "process-1", 20, () => "application")?.destination).toBe("local");
    expect(router.route("inertia", "process-1", 200, () => "application")?.destination).toBe("local");
    router.beginTouch("process-1");
    expect(router.route("touch", "process-1", 300, () => "application")?.destination).toBe("application");
  });
  it("rejects delayed actions after cancel, replacement or a changed runtime identity", () => {
    const router = new ScrollRouter<string>();
    router.beginTouch("epoch-1");
    const first = router.route("touch", "epoch-1", 0, () => "page")!;
    expect(router.valid(first.ticket, "epoch-2")).toBe(false);
    expect(router.route("inertia", "epoch-2", 10, () => "page")).toBeNull();
    router.beginTouch("epoch-1");
    expect(router.valid(first.ticket, "epoch-1")).toBe(false);
    const button = router.action("epoch-1");
    router.cancel();
    expect(router.valid(button, "epoch-1")).toBe(false);
    expect(router.route("inertia", "epoch-1", 20, () => "page")).toBeNull();
  });
  // Волна 7, probe-reading-pin-live мир C: после серии свайпов ⇊ возвращал к
  // live, а докрутка последнего рывка шла прежним маршрутом «своя история,
  // закреплено» — поднимала вьюпорт обратно и снова закрепляла источник чтения.
  it("кнопка завершает жест пальцем: его инерция и неотправленные фрагменты больше не исполняются", () => {
    const router = new ScrollRouter<string>();
    router.beginTouch("same");
    const swipe = router.route("touch", "same", 0, () => "local-pinned")!;
    expect(router.route("inertia", "same", 16, () => "other")?.destination).toBe("local-pinned");
    // ↑/↓/«к команде» — новое намерение, решается само.
    expect(router.route("button", "same", 120, () => "toward-live")?.destination).toBe("toward-live");
    expect(router.route("inertia", "same", 136, () => "local-pinned")).toBeNull();
    expect(router.valid(swipe.ticket, "same")).toBe(false);
    // Новое касание после кнопки решается заново.
    router.beginTouch("same");
    expect(router.route("touch", "same", 200, () => "fresh")?.destination).toBe("fresh");
  });
  it("⇈/⇊ (explicit) завершают жест так же; сам край действителен, пока среда та же", () => {
    const router = new ScrollRouter<string>();
    router.beginTouch("same");
    const swipe = router.route("touch", "same", 0, () => "local-pinned")!;
    expect(router.touching("same")).toBe(true);
    const edge = router.explicit("same");
    expect(router.touching("same")).toBe(false);
    expect(router.route("inertia", "same", 32, () => "local-pinned")).toBeNull();
    expect(router.valid(swipe.ticket, "same")).toBe(false);
    expect(router.valid(edge, "same")).toBe(true);
    expect(router.valid(edge, "other")).toBe(false);
  });
  // Волна 8 (скептик волны 7, S7): explicit() завершал только касание. Серия
  // колеса жила до 200 мс после последнего события, и хвост инерции трекпада
  // после ⇊ шёл прежним маршрутом «своя история, закреплено»: поднимал вьюпорт
  // (435 → 426 из 435) и снова закреплял чтение; новый вывод к live не вёл.
  it("кнопка завершает серию колеса: хвост инерции трекпада после ⇊ не исполняется", () => {
    const router = new ScrollRouter<string>();
    const burst = router.route("wheel", "same", 0, () => "local-pinned")!;
    expect(router.route("wheel", "same", 16, () => "other")?.destination).toBe("local-pinned");
    const edge = router.explicit("same"); // ⇊ посреди инерции
    expect(router.valid(burst.ticket, "same")).toBe(false);
    // Хвост идёт без паузы — это всё ещё прежняя серия, ни одного маршрута.
    for (const at of [32, 48, 180, 370]) {
      expect(router.route("wheel", "same", at, () => "fresh"), `${at} мс`).toBeNull();
    }
    // Пауза дольше серии — новое намерение, решается заново.
    const next = router.route("wheel", "same", 600, () => "fresh")!;
    expect(next.destination).toBe("fresh");
    expect(router.valid(next.ticket, "same")).toBe(true);
    expect(router.valid(edge, "same")).toBe(true);
  });
  it("хвост колеса глотается не дольше WHEEL_TAIL_MAX_MS: непрерывное колесо не теряется навсегда", () => {
    const router = new ScrollRouter<string>();
    router.route("wheel", "same", 0, () => "old");
    router.explicit("same");
    let first: { at: number; destination: string } | null = null;
    for (let at = 16; at <= WHEEL_TAIL_MAX_MS + 200 && !first; at += 16) {
      const route = router.route("wheel", "same", at, () => "fresh");
      if (route) first = { at, destination: route.destination };
    }
    expect(first?.destination).toBe("fresh");
    expect(first!.at).toBeGreaterThan(WHEEL_TAIL_MAX_MS);
    expect(first!.at).toBeLessThanOrEqual(WHEEL_TAIL_MAX_MS + 16);
  });
  it("без живой серии колеса кнопка ничего не глотает", () => {
    const router = new ScrollRouter<string>();
    router.explicit("same");
    expect(router.route("wheel", "same", 0, () => "fresh")?.destination).toBe("fresh");
    router.explicit("same");
    // Колесо после кнопки, но позже паузы серии (мышь дошла до кнопки и назад).
    expect(router.route("wheel", "same", 250, () => "again")?.destination).toBe("again");
  });
  // Волна 8 (скептик волны 7, [4]): вердикт пробы, запущенной кнопкой ↑
  // (action-ticket), после ⇊ снова закреплял удалённый канал — explicit() не
  // делал недействительными action-ticket'ы, а judge звал pinRemote().
  it("явный возврат к live: итог прежнего действия больше не закрепляет чтение", () => {
    const router = new ScrollRouter<string>();
    const up = router.action("same"); // ↑ кнопкой: проба PgUp в полёте
    expect(router.mayPin(up)).toBe(true);
    router.explicit("same"); // ещё одно нажатие ↑ — не возврат к live
    expect(router.mayPin(up)).toBe(true);
    const back = router.returnToLive("same"); // ⇊
    // Отправленное действие по-прежнему действительно: его итог — свидетельство.
    expect(router.valid(up, "same")).toBe(true);
    expect(router.mayPin(up)).toBe(false);
    expect(router.mayPin(back)).toBe(true);
    // Новое намерение после возврата закреплять вправе.
    expect(router.mayPin(router.action("same"))).toBe(true);
    const wheel = router.route("wheel", "same", 0, () => "remote")!;
    expect(router.mayPin(wheel.ticket)).toBe(true);
  });
  it("возврат к live завершает и жест пальцем, и серию колеса", () => {
    const router = new ScrollRouter<string>();
    router.beginTouch("same");
    const swipe = router.route("touch", "same", 0, () => "local-pinned")!;
    router.route("wheel", "same", 10, () => "local-pinned");
    router.returnToLive("same");
    expect(router.valid(swipe.ticket, "same")).toBe(false);
    expect(router.route("inertia", "same", 26, () => "local-pinned")).toBeNull();
    expect(router.route("wheel", "same", 26, () => "local-pinned")).toBeNull();
  });
  // Волна 8 (скептик доработки, мир E probe-reading-pin-live): серия мерилась по
  // событиям, дошедшим до route(), а туда приходят лишь набравшие строку.
  // Затухающая инерция трекпада (−2…−1 px за кадр) набирает строку раз в ~270 мс:
  // хвост после ⇊ в 448, 720 и 992 мс каждый раз начинал новую серию
  // ("safe-local"), уходил в свою историю и снова закреплял чтение.
  it("хвост после ⇊ меряется по сырым событиям: строка раз в 272 мс при событиях каждые 16 мс не маршрутизируется", () => {
    const router = new ScrollRouter<string>();
    for (let at = 0; at <= 176; at += 16) {
      expect(router.wheelActivity(at)).toBe(false);
      expect(router.route("wheel", "same", at, () => "local-pinned")?.destination).toBe("local-pinned");
    }
    router.returnToLive("same"); // ⇊ посреди инерции
    const lineAt = new Set([448, 720, 992]);
    for (let at = 192; at <= 1008; at += 16) {
      expect(router.wheelActivity(at), `${at} мс`).toBe(true);
      if (lineAt.has(at)) expect(router.route("wheel", "same", at, () => "safe-local"), `${at} мс`).toBeNull();
    }
    // Пауза в сырых событиях дольше серии — новое намерение, решается заново.
    expect(router.wheelActivity(1300)).toBe(false);
    expect(router.route("wheel", "same", 1300, () => "fresh")?.destination).toBe("fresh");
  });
  it("медленная серия (строка реже паузы, сырые события без паузы) — одно намерение, маршрут не пересобирается", () => {
    const router = new ScrollRouter<string>();
    router.wheelActivity(0);
    const first = router.route("wheel", "same", 0, () => "local")!;
    for (let at = 16; at <= 1000; at += 16) {
      router.wheelActivity(at);
      if (at % 272 === 0) expect(router.route("wheel", "same", at, () => "page")?.destination, `${at} мс`).toBe("local");
    }
    expect(router.valid(first.ticket, "same")).toBe(true);
    // Пауза в сырых событиях — новая серия, прежний ticket недействителен.
    router.wheelActivity(1300);
    const next = router.route("wheel", "same", 1300, () => "page")!;
    expect(next.destination).toBe("page");
    expect(router.valid(first.ticket, "same")).toBe(false);
  });
  it("сырые события хвоста глотаются не дольше WHEEL_TAIL_MAX_MS, дальше колесо решается заново", () => {
    const router = new ScrollRouter<string>();
    router.wheelActivity(0);
    router.route("wheel", "same", 0, () => "old");
    router.explicit("same");
    let released = -1;
    for (let at = 16; at <= WHEEL_TAIL_MAX_MS + 200 && released < 0; at += 16) if (!router.wheelActivity(at)) released = at;
    expect(released).toBeGreaterThan(WHEEL_TAIL_MAX_MS);
    expect(released).toBeLessThanOrEqual(WHEEL_TAIL_MAX_MS + 16);
    expect(router.route("wheel", "same", released, () => "fresh")?.destination).toBe("fresh");
  });
  it("pins a wheel burst without combining independent button intents", () => {
    const router = new ScrollRouter<string>();
    const first = router.route("wheel", "same", 0, () => "local")!;
    expect(router.route("wheel", "same", 150, () => "page")?.destination).toBe("local");
    expect(router.route("button", "same", 160, () => "page")?.destination).toBe("page");
    expect(router.route("wheel", "same", 400, () => "page")?.destination).toBe("page");
    expect(router.valid(first.ticket, "same")).toBe(false);
  });
});
