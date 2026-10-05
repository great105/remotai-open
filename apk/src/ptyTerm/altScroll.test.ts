import { describe, expect, it } from "vitest";
import {
  MAX_PAGES_PER_GESTURE,
  OWN_HISTORY_LINES,
  arrowSeq,
  channelPayload,
  clickSeq,
  decayStreamSample,
  edgeSeq,
  historyOwnerFromStream,
  historyOwnerFromStreamStable,
  pageSeq,
  pagesFromAccum,
  refillPageBudget,
  refillPageBudgetAt,
  resolveHistoryOwner,
  wheelSeq,
} from "./altScroll";

// Выбор ступени лестницы и его регрессии живут в navigationDecision.test.ts.

describe("порог своей истории", () => {
  // ⚠ Порог должен быть АБСОЛЮТНЫМ. Первая версия сравнивала с высотой окна и
  // сломала прокрутку у владельца в тот же вечер: с поднятой клавиатурой экран
  // ужимается до 11 строк, и 66 строк снапшота уже считались «своей историей».
  // Боевые числа: Codex/Kimi 500–1097 строк, Claude 0–79.
  it("крохи снапшота своей историей НЕ считаются", () => {
    expect(OWN_HISTORY_LINES).toBeGreaterThan(79);
    expect(OWN_HISTORY_LINES).toBeLessThan(500);
  });
});

describe("сборка последовательностей", () => {
  it("колесо: одна строка на N шагов, кнопка 64 вверх и 65 вниз", () => {
    expect(wheelSeq(-2, 48, 30)).toBe("\x1b[<64;24;15M\x1b[<64;24;15M");
    expect(wheelSeq(1, 48, 30)).toBe("\x1b[<65;24;15M");
    expect(wheelSeq(0, 48, 30)).toBe("");
  });

  it("страницы: меньше одной не шлём — иначе жест не даёт ничего", () => {
    expect(pageSeq(-3, 30)).toBe("\x1b[5~");
    expect(pageSeq(-29, 30)).toBe("\x1b[5~");
    expect(pageSeq(-58, 30)).toBe("\x1b[5~\x1b[5~");
    expect(pageSeq(5, 30)).toBe("\x1b[6~");
    expect(pageSeq(0, 30)).toBe("");
  });

  it("кнопки ⇈/⇊ у агента — сразу край разговора, а не сотня страниц", () => {
    expect(edgeSeq(true)).toBe("\x1b[1;5H");
    expect(edgeSeq(false)).toBe("\x1b[1;5F");
    expect(channelPayload("page", -150, 48, 30, true)).toBe("\x1b[1;5H");
  });

  it("стрелки — по одной на строку", () => {
    expect(arrowSeq(-2)).toBe("\x1b[A\x1b[A");
    expect(arrowSeq(2)).toBe("\x1b[B\x1b[B");
  });

  // Раньше каждая строка уходила отдельным ws.send: одно нажатие ⇈ при rows=30
  // давало 150 сообщений. Здесь всё едет одним куском.
  it("вся прокрутка — один кусок, а не сообщение на строку", () => {
    const data = channelPayload("wheel", -150, 48, 30);
    expect(data.split("M").length - 1).toBe(150);
  });

  // Плашку «N new messages (ctrl+End)» и пункты меню за компьютером нажимают
  // мышью. С телефона было нечем: слали только колесо.
  it("тап превращается в клик: нажатие и отпускание в одной ячейке", () => {
    expect(clickSeq(12, 5, 48, 30)).toBe("\x1b[<0;12;5M\x1b[<0;12;5m");
  });

  it("координаты клика зажимаются экраном, а не уезжают за край", () => {
    expect(clickSeq(0, 0, 48, 30)).toBe("\x1b[<0;1;1M\x1b[<0;1;1m");
    expect(clickSeq(999, 999, 48, 30)).toBe("\x1b[<0;48;30M\x1b[<0;48;30m");
    expect(clickSeq(12.6, 5.4, 48, 30)).toBe("\x1b[<0;13;5M\x1b[<0;13;5m");
  });

  it("каналу viewport и тупику none слать нечего", () => {
    expect(channelPayload("viewport", -5, 48, 30)).toBe("");
    expect(channelPayload("none", -5, 48, 30)).toBe("");
  });
});

describe("накопитель страниц: один свайп — не десять экранов", () => {
  // Медленный drag на Android даёт по строке в кадре анимации, а накопитель
  // прокрутки обнуляется КАЖДЫЙ кадр: тридцать кадров превращались в тридцать
  // PgUp, и человек улетал на десяток экранов (внешний разбор 2.57.9, T259-03).
  it("тридцать кадров по одной строке дают не больше трёх страниц", () => {
    let accum = 0;
    let pages = 0;
    for (let i = 0; i < 30; i++) {
      accum += -1;
      const r = pagesFromAccum(accum, 31);
      accum = r.rest;
      pages += Math.abs(r.pages);
    }
    expect(pages).toBeLessThanOrEqual(3);
    expect(pages).toBeGreaterThan(0); // но и не ноль: жест обязан листать
  });

  it("остаток переносится, а не теряется", () => {
    const a = pagesFromAccum(-10, 31); // полэкрана = 15 строк, ещё рано
    expect(a.pages).toBe(0);
    expect(a.rest).toBe(-10);
    const b = pagesFromAccum(-16, 31);
    expect(b.pages).toBe(-1);
    expect(b.rest).toBe(-1);
  });

  it("быстрый flick листает бодрее, но не дальше трёх страниц", () => {
    expect(pagesFromAccum(-1000, 31).pages).toBe(-3);
    expect(pagesFromAccum(1000, 31).pages).toBe(3);
  });

  // ГЛАВНОЕ ЧИСЛО ПОВТОРНОГО АУДИТА 2.57.12: потолок «три страницы» считался на
  // ОДИН вызов, а остаток переносился между кадрами — тысяча накопленных строк
  // давала 66 страниц за 22 вызова при «cap=3». Бюджет теперь принадлежит
  // ЖЕСТУ и тратится отправками.
  it("потолок считается на весь жест, а не на каждый кадр", () => {
    let accum = -1000;
    let budget = MAX_PAGES_PER_GESTURE;
    let pages = 0;
    for (let i = 0; i < 30; i++) {
      const r = pagesFromAccum(accum, 31, budget);
      accum = r.rest;
      pages += Math.abs(r.pages);
      budget -= Math.abs(r.pages);
    }
    expect(pages).toBe(3);
  });

  // ⚠ Жёсткие «три страницы на жест» принесли свою жалобу: длинный свайп без
  // отрыва пальца вставал после третьей страницы («и дальше не скролит»,
  // 14.08.2026). Защита нужна от БЫСТРОГО броска, а не от долгого движения.
  it("запас страниц восстанавливается со временем", () => {
    expect(refillPageBudget(0, 250)).toBe(1);
    expect(refillPageBudget(0, 1000)).toBe(MAX_PAGES_PER_GESTURE); // не выше потолка
    expect(refillPageBudget(1, 100)).toBe(1);                      // ещё рано
    expect(refillPageBudget(3, 5000)).toBe(MAX_PAGES_PER_GESTURE);
  });

  it("долгий свайп листает непрерывно, а быстрый флик — не больше трёх", () => {
    let budget = MAX_PAGES_PER_GESTURE;
    let pages = 0;
    for (let i = 0; i < 10; i++) {
      budget = refillPageBudget(budget, 500);
      const r = pagesFromAccum(-1000, 31, budget);
      pages += Math.abs(r.pages);
      budget -= Math.abs(r.pages);
    }
    expect(pages).toBeGreaterThan(MAX_PAGES_PER_GESTURE * 3);
    let fast = MAX_PAGES_PER_GESTURE;
    let flick = 0;
    for (let i = 0; i < 10; i++) {
      fast = refillPageBudget(fast, 16);
      const r = pagesFromAccum(-1000, 31, fast);
      flick += Math.abs(r.pages);
      fast -= Math.abs(r.pages);
    }
    expect(flick).toBe(MAX_PAGES_PER_GESTURE);
  });

  it("исчерпанный бюджет не копит остаток на потом", () => {
    // Иначе накопленное выстрелило бы страницами уже после остановки пальца.
    expect(pagesFromAccum(-1000, 31, 0)).toEqual({ pages: 0, rest: 0 });
  });

  // ⚠ ВНЕШНИЙ АУДИТ 2.57.18, находка P0-01: настоящий жест зовёт пополнение
  // КАЖДЫЙ КАДР (16–50 мс). Метка, сдвигаемая на «сейчас» при каждом кадре,
  // выбрасывала недобранные до 250 мс — и после исходных трёх страниц долгий
  // свайп вставал навсегда.
  it("долгий свайп на 60 Гц: остаток времени не теряется между кадрами", () => {
    let budget = MAX_PAGES_PER_GESTURE;
    let base = 1_000_000;
    let pages = 0;
    for (let i = 1; i <= 187; i++) {
      const nowMs = 1_000_000 + i * 16;
      const refill = refillPageBudgetAt(budget, nowMs - base);
      budget = refill.budget;
      base += refill.consumedMs;
      if (budget > 0) {
        budget -= 1;
        pages += 1;
      }
    }
    expect(pages).toBeGreaterThanOrEqual(10);
    expect(pages).toBeLessThanOrEqual(14);
  });

  it("недобранный интервал не сгорает: 16 мс кадры складываются в страницу", () => {
    let budget = 0;
    let base = 0;
    for (let now = 16; now <= 256; now += 16) {
      const refill = refillPageBudgetAt(budget, now - base);
      budget = refill.budget;
      base += refill.consumedMs;
    }
    expect(budget).toBe(1);
  });
});

// ⚠ Оба промаха порога `baseY >= 200` из повторного аудита 2.57.12 (T25712-02).
describe("чья история: замер потока, а не глубина буфера", () => {
  it("боевые числа 13.08.2026 разделяются с запасом", () => {
    expect(historyOwnerFromStream(617, 32 * 1024)).toBe("terminal");
    expect(historyOwnerFromStream(176, 32 * 1024)).toBe("terminal");
    expect(historyOwnerFromStream(29, 32 * 1024)).toBe("application");
    expect(historyOwnerFromStream(3, 32 * 1024)).toBe("application");
  });

  it("на крохах вывода вердикта нет — это честнее выдумки", () => {
    expect(historyOwnerFromStream(0, 100)).toBe("unknown");
    expect(historyOwnerFromStream(50, 1024)).toBe("unknown");
  });

  it("свежий Codex со 120 строками истории её НЕ теряет", () => {
    expect(resolveHistoryOwner({
      stream: historyOwnerFromStream(120, 8 * 1024),
      ownScrollback: 120,
      agent: true,
      alt: false,
    })).toBe("terminal");
  });

  it("Claude поверх 300 строк старого шелла листает СВОЙ разговор", () => {
    expect(resolveHistoryOwner({
      stream: historyOwnerFromStream(9, 32 * 1024),
      ownScrollback: 300,
      agent: true,
      alt: false,
    })).toBe("application");
  });

  it("простаивающая сессия без замера решается прежним порогом", () => {
    const idle = { stream: "unknown" as const, agent: true, alt: false };
    expect(resolveHistoryOwner({ ...idle, ownScrollback: 500 })).toBe("terminal");
    expect(resolveHistoryOwner({ ...idle, ownScrollback: OWN_HISTORY_LINES - 1 })).toBe("application");
  });

  it("старая история оболочки затухает под живым выводом агента", () => {
    let s = { lines: 300, bytes: 20 * 1024 };
    expect(historyOwnerFromStream(s.lines, s.bytes)).toBe("terminal");
    for (let i = 0; i < 8; i++) {
      s = decayStreamSample(s.lines + 2, s.bytes + 8 * 1024);
    }
    expect(historyOwnerFromStream(s.lines, s.bytes)).toBe("application");
  });

  it("затухание не переворачивает вердикт у потокового агента", () => {
    let s = { lines: 0, bytes: 0 };
    for (let i = 0; i < 20; i++) {
      s = decayStreamSample(s.lines + 154, s.bytes + 8 * 1024);
    }
    expect(historyOwnerFromStream(s.lines, s.bytes)).toBe("terminal");
  });

  it("без агента впереди история всегда терминала — стучаться некуда", () => {
    expect(resolveHistoryOwner({ stream: "application", ownScrollback: 0, agent: false, alt: false })).toBe("terminal");
  });

  it("у alt-screen своей истории нет ни в одном эмуляторе", () => {
    expect(resolveHistoryOwner({ stream: "terminal", ownScrollback: 900, agent: false, alt: true })).toBe("application");
  });
});

// Аудит 13.09, A02 / план T-04: на одном размере 16 КиБ 31 строка давала
// application, 32 и 33 — terminal. Одна строка вывода меняла источник жеста.
describe("гистерезис замера у границы 31/32/33 строки на 16 КиБ (T-04)", () => {
  const KIB16 = 16 * 1024;
  it("без прежнего вердикта — прежний порог (так воспроизводился A02)", () => {
    expect(historyOwnerFromStream(31, KIB16)).toBe("application");
    expect(historyOwnerFromStream(32, KIB16)).toBe("terminal");
    expect(historyOwnerFromStreamStable("unknown", 31, KIB16)).toBe("application");
    expect(historyOwnerFromStreamStable("unknown", 32, KIB16)).toBe("terminal");
  });

  it("вынесенный вердикт не качается на 31/32/33", () => {
    for (const lines of [31, 32, 33]) {
      expect(historyOwnerFromStreamStable("application", lines, KIB16)).toBe("application");
      expect(historyOwnerFromStreamStable("terminal", lines, KIB16)).toBe("terminal");
    }
  });

  it("уверенный уход за полосу вердикт меняет", () => {
    expect(historyOwnerFromStreamStable("application", 40, KIB16)).toBe("terminal");   // 2,5 строки/КиБ
    expect(historyOwnerFromStreamStable("terminal", 23, KIB16)).toBe("application");     // 1,44
    expect(historyOwnerFromStreamStable("terminal", 10, 1024)).toBe("unknown");          // мало байт
  });
});
