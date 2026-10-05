import { describe, it, expect } from "vitest";
import { planState, type PlanMe, trialBannerOnTop, trialLine, trialNotice } from "./plan";

const base: PlanMe = { tier: "free", devices_count: 1, max_devices: 5 };

describe("planState", () => {
  it("founder → вечный Про, без апгрейда, даже поверх беты", () => {
    const s = planState({ ...base, founder: true, beta: true, billing_enabled: true });
    expect(s.kind).toBe("founder");
    expect(s.planName).toBe("Про");
    expect(s.showUpgrade).toBe(false);
  });

  it("бета → Pro для всех, кнопки оплаты нет (касса выключена в бете)", () => {
    const s = planState({ ...base, beta: true, effective_tier: "pro", billing_enabled: false });
    expect(s.kind).toBe("beta");
    expect(s.showUpgrade).toBe(false);
  });

  it("активный триал после беты → Pro с отсчётом и предложением оформить", () => {
    const s = planState({ ...base, beta: false, effective_tier: "pro", trial_days_left: 5, billing_enabled: true });
    expect(s.kind).toBe("trial");
    expect(s.trialDaysLeft).toBe(5);
    expect(s.planName).toBe("Про");
    expect(s.showUpgrade).toBe(true);
  });

  it("free после беты и с кассой → Локально и апгрейд", () => {
    const s = planState({ ...base, beta: false, effective_tier: "free", billing_enabled: true });
    expect(s.kind).toBe("free");
    expect(s.planName).toBe("Локально");
    expect(s.showUpgrade).toBe(true);
  });

  it("оплаченный Pro → Pro без апгрейда", () => {
    const s = planState({ ...base, tier: "pro", effective_tier: "pro", beta: false, billing_enabled: true });
    expect(s.kind).toBe("pro");
    expect(s.showUpgrade).toBe(false);
  });

  it("квота устройств считается в процентах", () => {
    const s = planState({ ...base, devices_count: 2, max_devices: 5 });
    expect(s.devices).toEqual({ used: 2, max: 5, percent: 40 });
  });

  it("касса выключена → апгрейд не предлагаем никому", () => {
    const s = planState({ ...base, effective_tier: "free", billing_enabled: false });
    expect(s.showUpgrade).toBe(false);
  });

  it("касса выключена → вместо оплаты предлагаем заявку", () => {
    const trial = planState({ ...base, effective_tier: "pro", trial_days_left: 3, billing_enabled: false, beta: false });
    expect(trial.showUpgrade).toBe(false);
    expect(trial.showRequest).toBe(true);

    const free = planState({ ...base, effective_tier: "free", billing_enabled: false, beta: false });
    expect(free.showRequest).toBe(true);
  });

  it("заявку не предлагаем тем, кому платить незачем", () => {
    // Касса включена — есть обычная оплата; founder платит всегда ноль;
    // в бете Про открыт всем; платящий уже заплатил.
    expect(planState({ ...base, billing_enabled: true }).showRequest).toBe(false);
    expect(planState({ ...base, founder: true, billing_enabled: false }).showRequest).toBe(false);
    expect(planState({ ...base, beta: true, billing_enabled: false }).showRequest).toBe(false);
    expect(planState({ ...base, tier: "pro", billing_enabled: false }).showRequest).toBe(false);
  });

  it("облако: сервер сказал — верим ему, а не своим догадкам", () => {
    // Релей — единственный судья (PROD-008). Если он сказал «нельзя», клиент
    // не должен рисовать доступ только потому, что видит триал.
    const s = planState({ ...base, trial_days_left: 5, cloud_allowed: false });
    expect(s.cloudAllowed).toBe(false);
  });

  it("облако: без ответа сервера выводим сами — тем же правилом", () => {
    expect(planState({ ...base, effective_tier: "free", beta: false }).cloudAllowed).toBe(false);
    expect(planState({ ...base, effective_tier: "pro", beta: false }).cloudAllowed).toBe(true);
  });

  it("Флит показываем только тому, кто реально на нём", () => {
    const fleet = planState({ ...base, tier: "team", effective_tier: "team", beta: false });
    expect(fleet.planName).toBe("Флит");
    const pro = planState({ ...base, tier: "pro", effective_tier: "pro", beta: false });
    expect(pro.planName).toBe("Про");
  });

  // Релей зовёт старшую полку `fleet` (pricing.go), лицензия агента — `team`.
  // Клиент ждал только `team`, и оплативший Флит человек получил бы «Локально»
  // без удалённого доступа (аудит ИА 02.09.2026, P0-8).
  it("id полки fleet от релея — та же оплаченная полка «Флит», что и team", () => {
    const fleet = planState({ ...base, tier: "fleet", effective_tier: "fleet", beta: false, billing_enabled: true });
    expect(fleet.kind).toBe("pro");
    expect(fleet.planName).toBe("Флит");
    expect(fleet.showUpgrade).toBe(false);
    expect(fleet.cloudAllowed).toBe(true);
  });
});

// ⚠ Разрыв, найденный ревизией системы оплаты 01.09.2026: баннер жил только
// последние 7 дней пробы и исчезал ровно тогда, когда доступ пропадал. То есть
// в момент, когда человеку ВПЕРВЫЕ понадобилось заплатить, ход к оплате с
// главной исчезал, а отказ гейта говорил «оформите Про», не говоря где.
describe("главная в состоянии «проба кончилась»", () => {
  const ended = { tier: "free", effective_tier: "free", trial_days_left: 0, billing_enabled: true };

  it("говорит, что удалённый доступ выключен, и ведёт к оплате", () => {
    const n = trialNotice(planState(ended as any));
    expect(n.show).toBe(true);
    expect(n.ended).toBe(true);
  });

  it("а во время активной пробы это обычное напоминание, не отказ", () => {
    const n = trialNotice(planState({ ...ended, trial_days_left: 3 } as any));
    expect(n.show).toBe(true);
    expect(n.ended).toBe(false);
    expect(n.days).toBe(3);
  });

  it("платящему и основателю баннер не показывается вовсе", () => {
    expect(trialNotice(planState({ tier: "pro", effective_tier: "pro" } as any)).show).toBe(false);
    expect(trialNotice(planState({ founder: true, tier: "free", effective_tier: "pro" } as any)).show).toBe(false);
  });
});

// Замер probe-trial-over-cta 06.09.2026: в день 31 кнопка плашки стояла на
// 750–794 px телефона 390×844 при панели на 768 — нажатие попадало в навигацию.
describe("место плашки про пробу на главной", () => {
  const notice = (over: Partial<ReturnType<typeof trialNotice>>) =>
    ({ show: true, days: 5, urgent: false, ended: false, ...over });

  it("проба кончилась — наверх: ход к оплате обязан быть под пальцем", () => {
    expect(trialBannerOnTop(notice({ days: 0, urgent: true, ended: true }))).toBe(true);
  });

  it("последний день — тоже наверх: это уже срок, а не напоминание", () => {
    expect(trialBannerOnTop(notice({ days: 1, urgent: true }))).toBe(true);
  });

  it("осталось несколько дней — внизу, чтобы не занимать первый экран", () => {
    expect(trialBannerOnTop(notice({ days: 5 }))).toBe(false);
    expect(trialBannerOnTop(notice({ days: 7 }))).toBe(false);
  });

  it("плашки нет вовсе — наверх не поднимаем", () => {
    expect(trialBannerOnTop(null)).toBe(false);
    expect(trialBannerOnTop(notice({ show: false, urgent: true }))).toBe(false);
  });

  it("состояния из настоящего planState: день 31 наверх, день 25 внизу", () => {
    const over = trialNotice(planState({ ...base, effective_tier: "free", trial_days_left: 0, billing_enabled: true }));
    const early = trialNotice(planState({ ...base, effective_tier: "pro", trial_days_left: 5, billing_enabled: true }));
    expect(trialBannerOnTop(over)).toBe(true);
    expect(trialBannerOnTop(early)).toBe(false);
  });
});

// Живой прогон нового аккаунта 06.09.2026: релей отдавал trial_days_left=29 и
// trial_end, а кабинет писал только «Про» — ни слова, что это проба и когда
// она кончится. Строка считается вне экрана, чтобы её можно было проверить.
describe("строка пробы в личном кабинете", () => {
  const trial = { ...base, effective_tier: "pro", trial_days_left: 29, trial_end: "2026-10-06T05:28:00Z", billing_enabled: true };

  it("пробнику называет остаток дней и дату конца", () => {
    const line = trialLine(trial, "");
    expect(line).toEqual({ days: 29, date: new Date("2026-10-06T05:28:00Z").toLocaleDateString("ru-RU") });
  });

  it("без даты от релея остаётся только счётчик дней", () => {
    expect(trialLine({ ...trial, trial_end: undefined }, "")).toEqual({ days: 29, date: "" });
    expect(trialLine({ ...trial, trial_end: "мусор" }, "")).toEqual({ days: 29, date: "" });
  });

  it("оплаченный срок важнее пробы, основатель и платящий строки не видят", () => {
    expect(trialLine(trial, "06.10.2026")).toBeNull();
    expect(trialLine({ ...trial, founder: true }, "")).toBeNull();
    expect(trialLine({ ...base, tier: "pro", effective_tier: "pro" }, "")).toBeNull();
    expect(trialLine({ ...trial, trial_days_left: 0 }, "")).toBeNull();
  });
});
