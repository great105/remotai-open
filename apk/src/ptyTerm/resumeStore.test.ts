import { beforeEach, describe, expect, it, vi } from "vitest";
import {
  RICH_HISTORY_LINES, clearResumePos, loadResumePos, saveResumePos, shouldProbeHistory, shouldUseColdResume,
} from "./resumeStore";

describe("resume при ХОЛОДНОМ открытии новой страницы", () => {
  // Боевая диагностика 13.08.2026 (лог агента владельца, what=snapshot):
  // сессия 7285b79b — зеркало=4, своя=0: страница попросила дельту, сервер
  // прислал пустую, и прокрутке взяться было неоткуда. Сессия 10b8b057 —
  // зеркало=500 при том же агенте: там снапшот восстанавливает всё сам.
  it("бедное зеркало — начинаем полным реплеем, дельты не просим", () => {
    expect(shouldUseColdResume({ epoch: "e", offset: 100, hist: 4 })).toBe(false);
    expect(shouldUseColdResume({ epoch: "e", offset: 100, hist: 0 })).toBe(false);
  });

  it("богатое зеркало — сохранённая позиция остаётся, реплей не нужен", () => {
    expect(shouldUseColdResume({ epoch: "e", offset: 100, hist: 500 })).toBe(true);
    expect(shouldUseColdResume({ epoch: "e", offset: 100, hist: RICH_HISTORY_LINES })).toBe(true);
  });

  // Решать по ВИДУ АГЕНТА нельзя: у Claude бывает и пустое зеркало, и полное.
  // Признак — глубина, и она у нас измерена прошлым снапшотом.
  it("глубина неизвестна (первое открытие, запись прошлой версии) — реплей", () => {
    expect(shouldUseColdResume({ epoch: "e", offset: 100 })).toBe(false);
  });

  it("позиции нет вовсе — просить нечего", () => {
    expect(shouldUseColdResume(null)).toBe(false);
  });
});

// Тесты этого проекта идут без DOM (см. commands.test.ts — «без сети и без
// DOM»), поэтому хранилище подставляем сами. Заодно это ровно та поверхность,
// которой модуль пользуется: четыре метода и ничего больше.
class MemoryStorage {
  private map = new Map<string, string>();
  getItem(k: string): string | null {
    return this.map.has(k) ? this.map.get(k)! : null;
  }
  setItem(k: string, v: string): void {
    this.map.set(k, String(v));
  }
  removeItem(k: string): void {
    this.map.delete(k);
  }
  clear(): void {
    this.map.clear();
  }
}

describe("resumeStore", () => {
  beforeEach(() => {
    vi.restoreAllMocks();
    (globalThis as any).localStorage = new MemoryStorage();
  });

  it("позиция переживает перезапуск экрана", () => {
    saveResumePos("a1", { epoch: "6f1a", offset: 918273 });
    expect(loadResumePos("a1")).toEqual({ epoch: "6f1a", offset: 918273 });
  });

  // Глубина истории зеркала едет вместе с позицией: по ней следующее ХОЛОДНОЕ
  // открытие решает, хватит ли снапшота вместо полного реплея.
  it("глубина истории переживает запись и чтение", () => {
    saveResumePos("h1", { epoch: "e", offset: 42, hist: 500 });
    expect(loadResumePos("h1")).toEqual({ epoch: "e", offset: 42, hist: 500 });
    expect(shouldUseColdResume(loadResumePos("h1"))).toBe(true);
  });

  it("запись БЕЗ глубины валидна, но холодное открытие ей не доверяет", () => {
    saveResumePos("h2", { epoch: "e", offset: 42 });
    expect(loadResumePos("h2")).toEqual({ epoch: "e", offset: 42 });
    expect(shouldUseColdResume(loadResumePos("h2"))).toBe(false);
  });

  it("бедное зеркало запоминается нулём, а не отсутствием поля", () => {
    saveResumePos("h3", { epoch: "e", offset: 42, hist: 0 });
    expect(loadResumePos("h3")).toEqual({ epoch: "e", offset: 42, hist: 0 });
    expect(shouldUseColdResume(loadResumePos("h3"))).toBe(false);
  });

  it("у каждой сессии своя позиция", () => {
    saveResumePos("a1", { epoch: "6f1a", offset: 10 });
    saveResumePos("b2", { epoch: "77bc", offset: 20 });
    expect(loadResumePos("a1")).toEqual({ epoch: "6f1a", offset: 10 });
    expect(loadResumePos("b2")).toEqual({ epoch: "77bc", offset: 20 });
  });

  it("незнакомая сессия — начинаем с нуля", () => {
    expect(loadResumePos("нет-такой")).toBeNull();
  });

  it("процесс завершился — позицию забываем", () => {
    saveResumePos("a1", { epoch: "6f1a", offset: 5 });
    clearResumePos("a1");
    expect(loadResumePos("a1")).toBeNull();
  });

  // Дальше — про то, ради чего вся валидация: испорченная запись обязана
  // означать «начинаем с нуля» (прежнее поведение, полный снапшот), а не
  // сломанный экран терминала и не параметр вида `ep:NaN`, который агенту
  // разбирать нечем.
  it("мусор в хранилище не ломает открытие", () => {
    const broken = [
      "не json",
      "{}",
      '{"epoch":"","offset":10}',
      '{"epoch":"6f1a"}',
      '{"epoch":6,"offset":10}',
      '{"epoch":"6f1a","offset":"10"}',
      '{"epoch":"6f1a","offset":-1}',
      '{"epoch":"6f1a","offset":1.5}',
      '{"epoch":"6f1a","offset":null}',
    ];
    for (const raw of broken) {
      localStorage.setItem("pty.resume.a1", raw);
      expect(loadResumePos("a1"), `не должно приниматься: ${raw}`).toBeNull();
    }
  });

  it("пустая эпоха стирает прежнюю запись, а не оставляет старую", () => {
    saveResumePos("a1", { epoch: "6f1a", offset: 100 });
    saveResumePos("a1", { epoch: "", offset: 200 });
    expect(loadResumePos("a1")).toBeNull();
  });

  it("отказ хранилища не выбрасывает наружу", () => {
    vi.spyOn(localStorage, "setItem").mockImplementation(() => {
      throw new Error("QuotaExceededError");
    });
    expect(() => saveResumePos("a1", { epoch: "6f1a", offset: 1 })).not.toThrow();

    vi.spyOn(localStorage, "getItem").mockImplementation(() => {
      throw new Error("SecurityError");
    });
    expect(loadResumePos("a1")).toBeNull();

    vi.spyOn(localStorage, "removeItem").mockImplementation(() => {
      throw new Error("SecurityError");
    });
    expect(() => clearResumePos("a1")).not.toThrow();
  });

  it("пустой id игнорируется молча", () => {
    expect(loadResumePos("")).toBeNull();
    expect(() => saveResumePos("", { epoch: "6f1a", offset: 1 })).not.toThrow();
    expect(() => clearResumePos("")).not.toThrow();
  });
});

// Ревью 15.09, дефект 1: тёплое переподключение кадр не просит (ST-09 A6), а
// глубину истории обновляет только кадр. Страница, открытая при бедном
// зеркале, оставляла hist < 100 навсегда, и холодное открытие тянуло весь
// хвост кольца (живьём на 2.71.1: 15 КБ реплея против 58 Б resumed).
describe("кадр ради глубины истории на тёплом переподключении", () => {
  beforeEach(() => {
    (globalThis as any).localStorage = new MemoryStorage();
  });

  it("бедное или неизвестное зеркало и новые строки в прокрутке — один кадр", () => {
    expect(shouldProbeHistory(40, 30)).toBe(true);
    expect(shouldProbeHistory(0, 1)).toBe(true);
    expect(shouldProbeHistory(RICH_HISTORY_LINES - 1, 1)).toBe(true);
    // снапшота на странице ещё не было (snapHistRef = −1) или число мусорное
    expect(shouldProbeHistory(-1, 5)).toBe(true);
    expect(shouldProbeHistory(Number.NaN, 5)).toBe(true);
  });

  it("богатое зеркало — не просим: сохранённое решение и так «resume»", () => {
    expect(shouldProbeHistory(RICH_HISTORY_LINES, 30)).toBe(false);
    expect(shouldProbeHistory(500, 30)).toBe(false);
  });

  it("поток без новых строк — не просим: зеркалу копить было нечего", () => {
    expect(shouldProbeHistory(40, 0)).toBe(false);
    expect(shouldProbeHistory(-1, 0)).toBe(false);
    expect(shouldProbeHistory(40, Number.NaN)).toBe(false);
  });

  it("то же правило, что у холодного открытия: просим ровно тогда, когда решение — реплей", () => {
    for (const hist of [0, 4, 40, 99, 100, 101, 500]) {
      expect(shouldProbeHistory(hist, 1), `hist=${hist}`).toBe(!shouldUseColdResume({ epoch: "e", offset: 1, hist }));
    }
    expect(shouldProbeHistory(-1, 1)).toBe(!shouldUseColdResume({ epoch: "e", offset: 1 }));
  });

  it("сценарий: запомнено бедное зеркало, кадр hist-probe принёс 150 — следующее холодное открытие с resume", () => {
    saveResumePos("s1", { epoch: "e", offset: 900, hist: 40 });
    expect(shouldUseColdResume(loadResumePos("s1"))).toBe(false);
    expect(shouldProbeHistory(loadResumePos("s1")!.hist!, 30)).toBe(true);
    saveResumePos("s1", { epoch: "e", offset: 950, hist: 150 });
    expect(shouldUseColdResume(loadResumePos("s1"))).toBe(true);
    expect(shouldProbeHistory(150, 30)).toBe(false);
  });
});
