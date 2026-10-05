import { describe, expect, it } from "vitest";
import { channelPayload } from "./altScroll";
import { decideNavigation, edgePlan, shouldDropPin, type NavigationInput } from "./navigationDecision";

// По умолчанию — «Авто», своей прокрутки нет, свидетельств нет: именно здесь и
// решается, кто исполняет жест.
const base: NavigationInput = {
  override: "auto",
  pin: null,
  alt: true,
  mouse: "any",
  agent: true,
  declaredChannel: "",
  localCanScroll: false,
  owner: "application",
  page: "probe",
  wheel: "probe",
};
const decide = (over: Partial<NavigationInput>) => decideNavigation({ ...base, ...over });

describe("приоритет вычисляется до побочного эффекта (T-05)", () => {
  // Аудит 13.09, A02 + разбор C-серии: у Claude (реестр — PgUp) при плотности
  // 32 строки на 16 КиБ ранняя локальная ветка крутила обрывки перерисовок,
  // при 31 — PgUp. Объявленный канал теперь выше замера и выше своей истории.
  it("объявленный канал бьёт замер плотности и наличие своей истории", () => {
    const d = decide({ alt: false, declaredChannel: "page", owner: "terminal", localCanScroll: true });
    expect(d).toMatchObject({ executor: "remote", channel: "page", reason: "declared" });
  });

  it("объявленный канал, который сейчас не отвечает, отдаёт жест своей истории", () => {
    expect(decide({ alt: false, declaredChannel: "page", page: "local", localCanScroll: true }))
      .toMatchObject({ executor: "local", reason: "channel-silent" });
    expect(decide({ alt: false, declaredChannel: "page", page: "local", localCanScroll: false }))
      .toMatchObject({ executor: "none", reason: "channel-silent" });
  });

  it("объявленный канал используется сразу: доказанный — без наблюдения, иначе с наблюдением, без удержания жеста", () => {
    expect(decide({ declaredChannel: "page", page: "use" }).mode).toBe("send");
    expect(decide({ declaredChannel: "page", page: "verify" }).mode).toBe("verify");
    expect(decide({ declaredChannel: "page", page: "probe" }).mode).toBe("verify");
    // Без объявления неизвестный канал по-прежнему пробуется с удержанием.
    expect(decide({ alt: false, page: "probe" }).mode).toBe("probe");
  });

  // Codex: «press ctrl + t to view the full transcript» — это переход в другой
  // вид, и ни колесо, ни PgUp этого не открывают.
  it("Codex (транскрипт) листается только своей историей и никогда не получает клавишу", () => {
    expect(decide({ alt: false, declaredChannel: "transcript", localCanScroll: true }))
      .toMatchObject({ executor: "local", reason: "declared-transcript" });
    expect(decide({ declaredChannel: "transcript" })).toMatchObject({ executor: "none", channel: "none" });
    expect(decide({ declaredChannel: "transcript", override: "agent" })).toMatchObject({ executor: "none" });
  });

  it("незнакомый агент — прежняя лестница целиком", () => {
    expect(decide({ declaredChannel: "" }).channel).toBe("wheel");
    expect(decide({ agent: false, mouse: "none" }).channel).toBe("arrows");
  });
});

describe("явный выбор человека", () => {
  it("«Вывод» — всегда своя история, даже у края и с живым каналом приложения", () => {
    expect(decide({ override: "terminal", page: "use", declaredChannel: "page" }))
      .toMatchObject({ executor: "local", reason: "override-terminal" });
  });

  it("«Агент» — отправка без пробы; живой отказ «Авто» ручной выбор не отменяет", () => {
    expect(decide({ override: "agent", alt: false, page: "local" }))
      .toMatchObject({ executor: "remote", channel: "page", mode: "send" });
    expect(decide({ override: "agent", alt: true, mouse: "any", wheel: "local" }))
      .toMatchObject({ executor: "remote", channel: "wheel", mode: "send" });
  });

  it("«Агент» не изобретает клавишу: оболочке в обычном буфере слать нечего", () => {
    expect(decide({ override: "agent", alt: false, agent: false })).toMatchObject({ executor: "none" });
  });
});

// Регресс 2.66.6 («после ↑ по своей истории вниз выбиралось none»): человек,
// читающий свою историю выше низа, возвращается к новому ею же, а не PgDn
// приложению — даже при доказанном канале приложения.
describe("возврат к live своей историей", () => {
  it("вниз при своей истории выше низа — своя история, при любом канале приложения", () => {
    expect(decide({ alt: false, declaredChannel: "page", page: "use", localCanScroll: true, towardLive: true }))
      .toMatchObject({ executor: "local", reason: "toward-live" });
    expect(decide({ alt: false, page: "probe", owner: "application", localCanScroll: true, towardLive: true }))
      .toMatchObject({ executor: "local", reason: "toward-live" });
  });

  it("у низа своей истории жест вниз идёт приложению, вверх — прежним правилом", () => {
    expect(decide({ alt: false, declaredChannel: "page", page: "use", localCanScroll: false, towardLive: true }))
      .toMatchObject({ executor: "remote", channel: "page" });
    expect(decide({ alt: false, declaredChannel: "page", page: "use", localCanScroll: true, towardLive: false }))
      .toMatchObject({ executor: "remote", channel: "page" });
  });

  // Ревью 14.09: условие «удалённое закрепление побеждает возврат» проходило
  // все тесты. Невидимая сейчас история приложения не должна утащить вниз
  // человека, читающего свою.
  it("возврат к live своей историей выше удалённого закрепления", () => {
    const pin = { executor: "remote", channel: "page" } as const;
    expect(decide({ alt: false, pin, page: "use", localCanScroll: true, towardLive: true }))
      .toMatchObject({ executor: "local", reason: "toward-live" });
  });

  it("ручной «Агент» возврат своей историей не перехватывает", () => {
    expect(decide({ alt: false, override: "agent", localCanScroll: true, towardLive: true }).executor).toBe("remote");
  });
});

describe("закреплённый источник чтения (ST-03, T-09, T-10)", () => {
  it("читающему свою историю новый вывод и плотность источник не меняют", () => {
    const pin = { executor: "local", channel: "viewport" } as const;
    expect(decide({ alt: false, pin, owner: "application", page: "probe", localCanScroll: true }))
      .toMatchObject({ executor: "local", reason: "pinned" });
  });

  it("край закреплённой своей истории — край, а не команда приложению", () => {
    const pin = { executor: "local", channel: "viewport" } as const;
    expect(decide({ alt: false, pin, localCanScroll: false, page: "use" }))
      .toMatchObject({ executor: "local", reason: "pinned" });
  });

  it("закреплённый канал приложения держит, пока отвечает; замолчавший — решаем заново", () => {
    const pin = { executor: "remote", channel: "page" } as const;
    expect(decide({ alt: false, pin, owner: "terminal", localCanScroll: true, page: "use" }))
      .toMatchObject({ executor: "remote", channel: "page", reason: "pinned" });
    expect(decide({ alt: false, pin, owner: "terminal", localCanScroll: true, page: "local" }))
      .toMatchObject({ executor: "local" });
  });

  it("закрепление снимает только возврат к низу или замолчавший канал", () => {
    const localPin = { executor: "local", channel: "viewport" } as const;
    const pagePin = { executor: "remote", channel: "page" } as const;
    expect(shouldDropPin(localPin, { atLiveBottom: false, page: "local", wheel: "local" })).toBe(false);
    expect(shouldDropPin(localPin, { atLiveBottom: true, page: "use", wheel: "use" })).toBe(true);
    expect(shouldDropPin(pagePin, { atLiveBottom: true, page: "use", wheel: "probe" })).toBe(false);
    expect(shouldDropPin(pagePin, { atLiveBottom: false, page: "local", wheel: "probe" })).toBe(true);
  });

  it("ручной выбор сильнее закрепления", () => {
    const pin = { executor: "remote", channel: "page" } as const;
    expect(decide({ pin, override: "terminal" }).executor).toBe("local");
  });
});

// Перенесено из altScroll.test.ts (pickChannel/shouldScrollViewport) и
// scrollSota.test.ts: каждая регрессия сохранена в новом правиле.
describe("лестница каналов без объявления", () => {
  it("своя история может двигаться в alt-screen — крутим её", () => {
    expect(decide({ localCanScroll: true })).toMatchObject({ executor: "local" });
  });

  // ГЛАВНОЕ 13.08.2026: Claude Code рисует в обычном буфере с пустой своей
  // прокруткой; история внутри него и открывается PgUp.
  it("обычный буфер, листать нечего, впереди агент — страницы, а не пустой ход", () => {
    expect(decide({ alt: false })).toMatchObject({ executor: "remote", channel: "page", mode: "probe" });
  });

  it("обычный буфер без агента (оболочка у края истории) — своя история, ничего не шлём", () => {
    expect(decide({ alt: false, agent: false })).toMatchObject({ executor: "local", reason: "shell" });
  });

  it("у терминала своя история и мы у её края — приложению НЕ шлём ничего", () => {
    expect(decide({ alt: false, owner: "terminal", localCanScroll: false })).toMatchObject({ executor: "local" });
  });

  // ЖИВОЙ РАЗБОР 14.08.2026: у Claude наша прокрутка — обрывки перерисовок.
  it("история приложения важнее своей: свайп идёт сразу в агента", () => {
    expect(decide({ alt: false, owner: "application", localCanScroll: true }))
      .toMatchObject({ executor: "remote", channel: "page" });
  });

  it("у Codex/Kimi с настоящей историей свою прокрутку не отбираем", () => {
    expect(decide({ alt: false, owner: "terminal", localCanScroll: true })).toMatchObject({ executor: "local" });
  });

  it("alt-screen с мышью — сначала колесо: так листаются vim, htop и less", () => {
    expect(decide({})).toMatchObject({ channel: "wheel", mode: "probe" });
    expect(decide({ wheel: "use" })).toMatchObject({ channel: "wheel", mode: "send" });
  });

  // Жалоба 12.08.2026: 682 попытки за сутки, alt=true mouse=any, экран не двигается.
  it("колесо не отвечает, на переднем плане агент — PgUp/PgDn", () => {
    expect(decide({ wheel: "local" })).toMatchObject({ channel: "page" });
    expect(decide({ mouse: "none" })).toMatchObject({ channel: "page" });
  });

  it("агенту стрелки не отправляются НИКОГДА", () => {
    for (const mouse of ["none", "any", "vt200", "x10", ""]) {
      for (const wheel of ["use", "verify", "probe", "local"] as const) {
        for (const page of ["use", "verify", "probe", "local"] as const) {
          for (const override of ["auto", "agent"] as const) {
            const d = decide({ mouse, wheel, page, override });
            expect(d.channel).not.toBe("arrows");
            expect(channelPayload(d.channel, -5, 48, 30)).not.toContain("\x1b[A");
          }
        }
      }
    }
  });

  it("пейджер без мыши и без агента — стрелки", () => {
    expect(decide({ agent: false, mouse: "none" })).toMatchObject({ executor: "remote", channel: "arrows" });
  });

  it("не агент, мышь есть, колесо не отвечает — честно говорим человеку", () => {
    expect(decide({ agent: false, wheel: "local" })).toMatchObject({ executor: "none" });
  });

  it("страничный канал сейчас не отвечает — своя настоящая история или честный тупик", () => {
    expect(decide({ alt: false, page: "local", localCanScroll: true })).toMatchObject({ executor: "local" });
    expect(decide({ alt: false, page: "local", localCanScroll: false })).toMatchObject({ executor: "none" });
    expect(decide({ wheel: "local", page: "local" })).toMatchObject({ executor: "none" });
    expect(decide({ mouse: "none", page: "local" })).toMatchObject({ executor: "none" });
  });
});

describe("край — отдельная операция (T25712-01)", () => {
  it("своя история: зажимаемся её краем, приложению не шлём ничего", () => {
    expect(edgePlan(decide({ alt: false, owner: "terminal" }), true)).toEqual({ kind: "local", edge: "start" });
    expect(edgePlan(decide({ alt: false, owner: "terminal" }), false)).toEqual({ kind: "local", edge: "end" });
  });

  it("история агента: ровно Ctrl+Home и Ctrl+End", () => {
    expect(edgePlan(decide({ alt: false }), true)).toEqual({ kind: "send", data: "\x1b[1;5H" });
    expect(edgePlan(decide({ alt: false }), false)).toEqual({ kind: "send", data: "\x1b[1;5F" });
  });

  it("пейджеру и колесу краевой команды нет — обычная прокрутка", () => {
    expect(edgePlan(decide({ agent: false, mouse: "none" }), true)).toEqual({ kind: "lines" });
    expect(edgePlan(decide({}), true)).toEqual({ kind: "lines" });
  });
});
