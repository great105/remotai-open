import { describe, expect, it } from "vitest";
import {
  SCROLL_PROBE_SCOPES,
  SCROLL_PROBE_TTL_MS,
  forgetScrollProbeVerdict,
  readScrollProbeVerdict,
  restoredObservation,
  saveScrollProbeVerdict,
  storedEvidence,
  type ScrollProbeVerdict,
} from "./scrollProbeMemory";
import { DIR_UP, evaluateObservation, observeOutcome } from "./navigationEvidence";

function memoryStorage() {
  const entries = new Map<string, string>();
  return {
    entries,
    getItem: (key: string) => entries.get(key) ?? null,
    setItem: (key: string, value: string) => { entries.set(key, value); },
    removeItem: (key: string) => { entries.delete(key); },
  };
}

const T0 = 1_700_000_000_000;
const dead: ScrollProbeVerdict = { page: { state: "unconfirmed", dead: 1, silentProbes: 1, at: T0 }, wheel: null };
const alive: ScrollProbeVerdict = {
  page: { state: "confirmed", dead: 0, silentProbes: 0, at: T0 },
  wheel: { state: "unconfirmed", dead: 3, silentProbes: 2, at: T0 },
};
const V1 = 'pty.scrollProbe.v1:["pc-a","term-a"]';
const V2 = 'pty.scrollProbe.v2:["pc-a","term-a"]';
const SCOPE = '["term-a","codex:4242@1700","normal","none","transcript"]';

// Боевой лог 06.09.2026: терминал Codex открыт дважды за минуту, и оба раза
// первый жест ушёл PgUp в Codex. Наблюдение обязано переживать закрытие
// экрана, пока область действия (процесс, режимы, адаптер) та же.
describe("сериализация свидетельств навигации", () => {
  it("наблюдение возвращается той же области того же терминала", () => {
    const storage = memoryStorage();
    saveScrollProbeVerdict("pc-a", "term-a", SCOPE, dead, T0, storage);
    expect(readScrollProbeVerdict("pc-a", "term-a", SCOPE, T0 + 60_000, storage)).toEqual(dead);
  });

  it("другая область — процесс, его поколение, режим буфера или адаптер — читается как «не знаем»", () => {
    const storage = memoryStorage();
    saveScrollProbeVerdict("pc-a", "term-a", SCOPE, dead, T0, storage);
    expect(readScrollProbeVerdict("pc-a", "term-a", SCOPE.replace("4242", "4243"), T0, storage)).toBeNull();
    expect(readScrollProbeVerdict("pc-a", "term-a", SCOPE.replace("@1700", "@1800"), T0, storage)).toBeNull();
    expect(readScrollProbeVerdict("pc-a", "term-a", SCOPE.replace("normal", "alternate"), T0, storage)).toBeNull();
    expect(readScrollProbeVerdict("pc-a", "term-a", SCOPE.replace("transcript", "page"), T0, storage)).toBeNull();
  });

  it("несколько областей одного терминала живут рядом: смена режима буфера не стирает соседний", () => {
    const storage = memoryStorage();
    const alt = SCOPE.replace("normal", "alternate");
    saveScrollProbeVerdict("pc-a", "term-a", SCOPE, dead, T0, storage);
    saveScrollProbeVerdict("pc-a", "term-a", alt, alive, T0 + 1, storage);
    expect(readScrollProbeVerdict("pc-a", "term-a", SCOPE, T0 + 2, storage)).toEqual(dead);
    expect(readScrollProbeVerdict("pc-a", "term-a", alt, T0 + 2, storage)).toEqual(alive);
  });

  it("областей не больше лимита: самая старая уходит первой", () => {
    const storage = memoryStorage();
    for (let i = 0; i <= SCROLL_PROBE_SCOPES; i++) {
      saveScrollProbeVerdict("pc-a", "term-a", `s${i}`, dead, T0 + i, storage);
    }
    expect(readScrollProbeVerdict("pc-a", "term-a", "s0", T0 + 10, storage)).toBeNull();
    expect(readScrollProbeVerdict("pc-a", "term-a", `s${SCROLL_PROBE_SCOPES}`, T0 + 10, storage)).toEqual(dead);
  });

  it("не течёт в другой терминал или компьютер", () => {
    const storage = memoryStorage();
    saveScrollProbeVerdict("pc-a", "term-a", SCOPE, dead, T0, storage);
    expect(readScrollProbeVerdict("pc-a", "term-b", SCOPE, T0, storage)).toBeNull();
    expect(readScrollProbeVerdict("pc-b", "term-a", SCOPE, T0, storage)).toBeNull();
  });

  it("после срока и при часах, ушедших назад, канал не восстанавливается (T-02, T-03)", () => {
    const storage = memoryStorage();
    saveScrollProbeVerdict("pc-a", "term-a", SCOPE, dead, T0, storage);
    expect(readScrollProbeVerdict("pc-a", "term-a", SCOPE, T0 + SCROLL_PROBE_TTL_MS, storage)).toEqual(dead);
    expect(readScrollProbeVerdict("pc-a", "term-a", SCOPE, T0 + SCROLL_PROBE_TTL_MS + 1, storage)).toBeNull();
    expect(readScrollProbeVerdict("pc-a", "term-a", SCOPE, T0 - 1, storage)).toBeNull();
  });

  it("срок у каждого канала свой: свежая запись колеса не продлевает старую страницу", () => {
    const storage = memoryStorage();
    const mixed: ScrollProbeVerdict = {
      page: { state: "confirmed", dead: 0, silentProbes: 0, at: T0 },
      wheel: { state: "confirmed", dead: 0, silentProbes: 0, at: T0 + SCROLL_PROBE_TTL_MS },
    };
    saveScrollProbeVerdict("pc-a", "term-a", SCOPE, mixed, T0 + SCROLL_PROBE_TTL_MS, storage);
    expect(readScrollProbeVerdict("pc-a", "term-a", SCOPE, T0 + SCROLL_PROBE_TTL_MS + 10, storage)).toEqual({
      page: null, wheel: mixed.wheel,
    });
  });

  it("вывод после молчания делает неподтверждённое устаревшим, подтверждённое — нет", () => {
    const storage = memoryStorage();
    saveScrollProbeVerdict("pc-a", "term-a", SCOPE, alive, T0, storage);
    expect(readScrollProbeVerdict("pc-a", "term-a", SCOPE, T0 + 10, storage, T0 + 5)).toEqual({
      page: alive.page, wheel: null,
    });
    saveScrollProbeVerdict("pc-a", "term-a", SCOPE, dead, T0, storage);
    expect(readScrollProbeVerdict("pc-a", "term-a", SCOPE, T0 + 10, storage, T0 + 5)).toBeNull();
  });

  it("пустое наблюдение стирает область; старый v1 не читается и стирается при записи", () => {
    const storage = memoryStorage();
    storage.setItem(V1, JSON.stringify({ process: "codex:4242", at: T0, page: false, pageDead: 1, wheel: null, wheelDead: 0 }));
    expect(readScrollProbeVerdict("pc-a", "term-a", SCOPE, T0, storage)).toBeNull();
    saveScrollProbeVerdict("pc-a", "term-a", SCOPE, dead, T0, storage);
    expect(storage.entries.has(V1)).toBe(false);
    saveScrollProbeVerdict("pc-a", "term-a", SCOPE, { page: null, wheel: null }, T0, storage);
    expect(storage.entries.size).toBe(0);
  });

  it("явный выбор человека стирает память автоматики обоих форматов", () => {
    const storage = memoryStorage();
    storage.setItem(V1, "{}");
    saveScrollProbeVerdict("pc-a", "term-a", SCOPE, dead, T0, storage);
    forgetScrollProbeVerdict("pc-a", "term-a", storage);
    expect(storage.entries.size).toBe(0);
  });

  it("порченая запись, чужая схема и пустые ключи — «не знаем», без исключений (T-03)", () => {
    const storage = memoryStorage();
    storage.setItem(V2, "{oops");
    expect(readScrollProbeVerdict("pc-a", "term-a", SCOPE, T0, storage)).toBeNull();
    storage.setItem(V2, JSON.stringify({ v: 2, entries: [{ scope: SCOPE, page: { state: "yes", dead: 0, silentProbes: 0, at: T0 }, wheel: null }] }));
    expect(readScrollProbeVerdict("pc-a", "term-a", SCOPE, T0, storage)).toBeNull();
    storage.setItem(V2, JSON.stringify({ v: 3, entries: [{ scope: SCOPE, page: dead.page, wheel: null }] }));
    expect(readScrollProbeVerdict("pc-a", "term-a", SCOPE, T0, storage)).toBeNull();
    expect(readScrollProbeVerdict("", "term-a", SCOPE, T0, storage)).toBeNull();
    expect(readScrollProbeVerdict("pc-a", "term-a", "", T0, storage)).toBeNull();
    // Порченая запись заменяется новой, а не роняет запись.
    saveScrollProbeVerdict("pc-a", "term-a", SCOPE, dead, T0, storage);
    expect(readScrollProbeVerdict("pc-a", "term-a", SCOPE, T0, storage)).toEqual(dead);
  });

  it("недоступное хранилище не роняет ни чтение, ни запись (T-03)", () => {
    const broken = {
      getItem: () => { throw new Error("denied"); },
      setItem: () => { throw new Error("quota"); },
      removeItem: () => { throw new Error("denied"); },
    };
    expect(readScrollProbeVerdict("pc-a", "term-a", SCOPE, T0, broken)).toBeNull();
    expect(() => saveScrollProbeVerdict("pc-a", "term-a", SCOPE, dead, T0, broken)).not.toThrow();
    expect(() => forgetScrollProbeVerdict("pc-a", "term-a", broken)).not.toThrow();
  });
});

describe("перевод живого наблюдения в запись и обратно", () => {
  const rows = ["SECRET-ROW-1", "SECRET-ROW-2", "SECRET-ROW-3"];
  it("слабое наблюдение не переносится, подтверждённое и молчание — переносятся без текста экрана", () => {
    const weak = observeOutcome(null, { kind: "repaint", edge: false, direction: DIR_UP, before: rows, after: ["x", "y", "z"] }, T0)!;
    expect(storedEvidence(weak)).toBeNull();
    const silent = observeOutcome(null, { kind: "none", edge: false, direction: DIR_UP, before: rows, after: rows }, T0)!;
    const stored = storedEvidence(silent)!;
    expect(stored).toEqual({ state: "unconfirmed", dead: DIR_UP, silentProbes: 1, at: T0 });
    expect(JSON.stringify(stored)).not.toContain("SECRET");
    const back = restoredObservation(stored);
    expect(back.anchor).toBeUndefined();
    // Восстановленное молчание без якоря держит локальный путь до отсрочки.
    expect(evaluateObservation(back, { now: T0 + 100, direction: DIR_UP, rows })).toBe("local");
  });
});
