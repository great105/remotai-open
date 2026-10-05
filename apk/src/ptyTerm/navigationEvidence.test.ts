import { describe, expect, it } from "vitest";
import {
  CONFIRMED_TTL_MS,
  DIR_DOWN,
  DIR_UP,
  EDGE_SILENCE_LIMIT,
  LATE_ANSWER_DELIVERY_MS,
  LATE_ANSWER_MS,
  LATE_ANSWER_STEP_MS,
  MIN_REPROBE_MS,
  REVERIFY_MS,
  NavigationEvidence,
  SILENT_COOLDOWN_BASE_MS,
  SILENT_COOLDOWN_MAX_MS,
  WEAK_TTL_MS,
  NAVIGATION_ADAPTER_VERSION,
  answeredView,
  evaluateObservation,
  evidenceScopeKey,
  nextLateAnswerCheck,
  observeOutcome,
  silentCooldownMs,
  type ProbeOutcome,
} from "./navigationEvidence";

describe("область свидетельства (ST-02: streamEpoch, приложение, версия адаптера; волна 4)", () => {
  const base = { process: "4242:codex:local", generation: 1757000000123, epoch: "e1", buffer: "normal", mouse: "none", channel: "" };
  it("тёплое переподключение (та же эпоха) — та же область; новая эпоха, поколение, режим или адаптер — другая", () => {
    const key = evidenceScopeKey(base);
    expect(key).toBe(evidenceScopeKey({ ...base })); // resumed в ту же эпоху
    expect(JSON.parse(key)).toEqual(["4242:codex:local@1757000000123", "e1", "normal", "none", "", NAVIGATION_ADAPTER_VERSION]);
    for (const changed of [{ epoch: "e2" }, { generation: 1757000000999 }, { buffer: "alternate" }, { mouse: "any" },
      { channel: "page" }, { process: "4243:codex:local" }]) {
      expect(evidenceScopeKey({ ...base, ...changed }), JSON.stringify(changed)).not.toBe(key);
    }
  });
  it("без процесса или до первого маркера (эпоха неизвестна) области нет", () => {
    expect(evidenceScopeKey({ ...base, process: "" })).toBe("");
    expect(evidenceScopeKey({ ...base, epoch: "" })).toBe("");
    expect(JSON.parse(evidenceScopeKey({ ...base, generation: 0 }))[0]).toBe("4242:codex:local");
  });
});

const screen = (n: number, tag = "row") => Array.from({ length: n }, (_, i) => `${tag} ${i}`);

/** Пять нижних строк — спиннер, как в probe-page-dead-fallback-live. */
const busy = (rows: string[], tick: number) =>
  rows.map((r, i) => (i >= rows.length - 5 ? `busy ${i} ${tick}` : r));

const silent = (before: string[], after: string[], direction = DIR_UP): ProbeOutcome =>
  ({ kind: "none", edge: false, direction, before, after });

describe("A01: отрицательная проба не управляет открытым экраном вечно", () => {
  it("значимый новый вывод того же процесса разрешает ограниченную повторную пробу", () => {
    const ev = new NavigationEvidence();
    ev.setScope("pc|term|claude:42|normal|none|page");
    const before = busy(screen(20), 1);
    const after = busy(screen(20), 2);
    ev.record("page", silent(before, after), 0);
    // Сразу после молчания — локальный путь, без нового ожидания.
    expect(ev.evaluate("page", { now: 300, direction: DIR_UP, rows: busy(screen(20), 3) })).toBe("local");
    // Процесс напечатал новую строку вне спиннера — канал снова пробуем.
    const recovered = busy(screen(20), 9);
    recovered[0] = "AUDIT-READY-AFTER-NEGATIVE";
    expect(ev.evaluate("page", { now: MIN_REPROBE_MS, direction: DIR_UP, rows: recovered })).toBe("probe");
  });

  it("одни тики спиннера повторной пробы не дают — двадцать жестов не ждут двадцать раз", () => {
    const ev = new NavigationEvidence();
    ev.setScope("s");
    ev.record("page", silent(busy(screen(20), 1), busy(screen(20), 2)), 0);
    let probes = 0;
    for (let g = 0; g < 20; g++) {
      const now = 100 + g * 90; // вся серия короче первой отсрочки
      if (ev.evaluate("page", { now, direction: DIR_UP, rows: busy(screen(20), 10 + g) }) === "probe") probes++;
    }
    expect(probes).toBe(0);
  });

  it("без значимого вывода проверка повторяется только после отсрочки, и отсрочка растёт", () => {
    expect(silentCooldownMs(1)).toBe(SILENT_COOLDOWN_BASE_MS);
    expect(silentCooldownMs(2)).toBe(SILENT_COOLDOWN_BASE_MS * 2);
    expect(silentCooldownMs(40)).toBe(SILENT_COOLDOWN_MAX_MS);
    const ev = new NavigationEvidence();
    ev.setScope("s");
    const rows = screen(20);
    ev.record("page", silent(rows, rows), 0);
    expect(ev.evaluate("page", { now: SILENT_COOLDOWN_BASE_MS - 1, direction: DIR_UP, rows })).toBe("local");
    expect(ev.evaluate("page", { now: SILENT_COOLDOWN_BASE_MS, direction: DIR_UP, rows })).toBe("probe");
    // Повторное молчание удваивает отсрочку.
    ev.record("page", silent(rows, rows), SILENT_COOLDOWN_BASE_MS);
    const second = ev.get("page")!;
    expect(second.silentProbes).toBe(2);
    expect(ev.evaluate("page", {
      now: SILENT_COOLDOWN_BASE_MS + SILENT_COOLDOWN_BASE_MS * 2 - 1, direction: DIR_UP, rows,
    })).toBe("local");
  });

  it("строки, дёргавшиеся на прошлых пробах, остаются шумом, а порог повторной пробы растёт", () => {
    const ev = new NavigationEvidence();
    ev.setScope("s");
    const base = screen(10);
    const clock = (t: string) => base.map((r, i) => (i === 0 ? `clock ${t}` : r));
    // Первая проба: часы не тикнули за окно, шумом их не признали.
    ev.record("page", silent(clock("a"), clock("a")), 0);
    expect(ev.evaluate("page", { now: MIN_REPROBE_MS, direction: DIR_UP, rows: clock("b") })).toBe("probe");
    // Повторная проба поймала тик часов — строка 0 стала шумом навсегда для этой серии.
    ev.record("page", silent(clock("b"), clock("c")), MIN_REPROBE_MS);
    expect(ev.get("page")!.volatileRows).toEqual([0]);
    expect(ev.evaluate("page", { now: MIN_REPROBE_MS * 10, direction: DIR_UP, rows: clock("z") })).toBe("local");
    // Настоящая новая строка всё ещё будит проверку, но не раньше удвоенного порога.
    const fresh = clock("z");
    fresh[5] = "new answer";
    expect(ev.evaluate("page", { now: MIN_REPROBE_MS + MIN_REPROBE_MS * 2 - 1, direction: DIR_UP, rows: fresh })).toBe("local");
    expect(ev.evaluate("page", { now: MIN_REPROBE_MS + MIN_REPROBE_MS * 2, direction: DIR_UP, rows: fresh })).toBe("probe");
  });

  it("молчание вверх не запрещает пробу вниз — у края приложения это разные намерения", () => {
    const ev = new NavigationEvidence();
    ev.setScope("s");
    const rows = screen(10);
    ev.record("page", silent(rows, rows, DIR_UP), 0);
    expect(ev.evaluate("page", { now: 10, direction: DIR_UP, rows })).toBe("local");
    expect(ev.evaluate("page", { now: 10, direction: DIR_DOWN, rows })).toBe("probe");
  });
});

describe("A03: большой независимый repaint не становится доказанной способностью", () => {
  const before = screen(24, "old");
  const repaint: ProbeOutcome = { kind: "repaint", edge: false, direction: DIR_UP, before, after: screen(24, "new") };

  it("один repaint — слабое наблюдение с коротким сроком: слать, но смотреть на итог", () => {
    const obs = observeOutcome(null, repaint, 0)!;
    expect(obs.state).toBe("weak");
    expect(evaluateObservation(obs, { now: WEAK_TTL_MS, direction: DIR_UP })).toBe("verify");
    expect(evaluateObservation(obs, { now: WEAK_TTL_MS + 1, direction: DIR_UP })).toBe("probe");
  });

  it("повторные repaint остаются слабыми: постоянно перерисовывающий TUI канал не доказывает (T-06)", () => {
    let obs = observeOutcome(null, repaint, 0)!;
    for (let i = 1; i <= 10; i++) obs = observeOutcome(obs, repaint, i * 500)!;
    expect(obs.state).toBe("weak");
    expect(evaluateObservation(obs, { now: 5000, direction: DIR_UP })).toBe("verify");
  });

  it("доказанный канал живёт свой срок; repaint его не понижает", () => {
    const shift: ProbeOutcome = { kind: "shift", edge: false, direction: DIR_UP, before, after: before };
    const confirmed = observeOutcome(observeOutcome(null, shift, 0), repaint, 500)!;
    expect(confirmed.state).toBe("confirmed");
    expect(evaluateObservation(confirmed, { now: 500 + REVERIFY_MS, direction: DIR_UP })).toBe("use");
    // Раз в REVERIFY_MS доказанный канал отправляет с наблюдением итога.
    expect(evaluateObservation(confirmed, { now: 501 + REVERIFY_MS, direction: DIR_UP })).toBe("verify");
    expect(evaluateObservation(confirmed, { now: 500 + CONFIRMED_TTL_MS, direction: DIR_UP })).toBe("verify");
    expect(evaluateObservation(confirmed, { now: 501 + CONFIRMED_TTL_MS, direction: DIR_UP })).toBe("probe");
  });

  it("сдвиг к НОВОМУ тексту (жест вниз) не доказывает: так же выглядит новый вывод", () => {
    const down: ProbeOutcome = { kind: "shift", edge: false, direction: DIR_DOWN, before, after: before };
    expect(observeOutcome(null, down, 0)!.state).toBe("weak");
  });

  // Волна 8: тот же класс, что у досмотра позднего ответа на ↓, но в самом
  // окне пробы — приложение печатает, пока PgDn-проба у низа ждёт ответа.
  // Сдвиг к новому на ↓ — слабое наблюдение ТОЛЬКО для ↓: про ↑ он ничего не
  // говорит, и молчание ↑ после него — не «край истории приложения».
  it("сдвиг к новому на ↓ не даёт ↑ режима verify, и молчание ↑ после него — не край истории", () => {
    const ev = new NavigationEvidence();
    ev.setScope("s");
    const printed = [...before.slice(4), ...screen(4, "printed")];
    ev.record("page", { kind: "shift", edge: false, direction: DIR_DOWN, before, after: printed }, 0);
    expect(ev.evaluate("page", { now: 10, direction: DIR_DOWN, rows: printed })).toBe("verify");
    expect(ev.evaluate("page", { now: 10, direction: DIR_UP, rows: printed })).toBe("probe");
    // ↑1 — проба, PgUp молчит: канал вверх сейчас молчит, а не «край».
    ev.record("page", silent(printed, printed, DIR_UP), 1300);
    expect(ev.get("page")!.state).toBe("unconfirmed");
    expect(ev.evaluate("page", { now: 1400, direction: DIR_UP, rows: printed })).toBe("local");
    expect(ev.evaluate("page", { now: 3100, direction: DIR_UP, rows: printed })).toBe("local");
  });

  it("настоящий слабый или доказанный ответ ↑ сдвигом на ↓ не теряется, и наоборот", () => {
    const printed = [...before.slice(4), ...screen(4, "printed")];
    const downShift: ProbeOutcome = { kind: "shift", edge: false, direction: DIR_DOWN, before, after: printed };
    // ↓-сдвиг, потом repaint на ↑ — обычное слабое для обоих направлений.
    const thenUp = observeOutcome(observeOutcome(null, downShift, 0), repaint, 100)!;
    expect(thenUp.state).toBe("weak");
    expect(evaluateObservation(thenUp, { now: 200, direction: DIR_UP })).toBe("verify");
    // repaint на ↑, потом ↓-сдвиг — слабое ↑ остаётся.
    const thenDown = observeOutcome(observeOutcome(null, repaint, 0), downShift, 100)!;
    expect(evaluateObservation(thenDown, { now: 200, direction: DIR_UP })).toBe("verify");
    // Молчание ↑ после настоящего ответа ↑ — по-прежнему край истории.
    const quietUp: ProbeOutcome = { ...repaint, kind: "none", after: before };
    expect(observeOutcome(thenDown, quietUp, 300)!.edgeSilences).toBe(1);
    // Доказанный канал сдвигом на ↓ не понижается.
    const shiftUp: ProbeOutcome = { kind: "shift", edge: false, direction: DIR_UP, before, after: before };
    const confirmed = observeOutcome(observeOutcome(null, shiftUp, 0), downShift, 100)!;
    expect(confirmed.state).toBe("confirmed");
    expect(evaluateObservation(confirmed, { now: 200, direction: DIR_UP })).toBe("use");
  });

  it("полная страница из тишины доказывает, тот же repaint на фоне вывода — нет", () => {
    expect(observeOutcome(null, { ...repaint, quietBefore: true }, 0)!.state).toBe("confirmed");
    expect(observeOutcome(null, { ...repaint, quietBefore: false }, 0)!.state).toBe("weak");
    expect(observeOutcome(null, { ...repaint, quietBefore: true, direction: DIR_DOWN }, 0)!.state).toBe("weak");
  });

  it("сдвиг строк — сильное доказательство сразу", () => {
    const shift: ProbeOutcome = { kind: "shift", edge: false, direction: DIR_UP, before, after: before };
    expect(observeOutcome(null, shift, 0)!.state).toBe("confirmed");
  });

  it("молчание после ответов — сначала край истории, после лимита — канал молчит", () => {
    let obs = observeOutcome(null, repaint, 0)!;
    const quiet: ProbeOutcome = { ...repaint, kind: "none", after: before };
    for (let i = 1; i <= EDGE_SILENCE_LIMIT; i++) {
      obs = observeOutcome(obs, quiet, i * 100)!;
      expect(obs.state).toBe("weak");
      expect(obs.edgeSilences).toBe(i);
    }
    obs = observeOutcome(obs, quiet, 1000)!;
    expect(obs.state).toBe("unconfirmed");
    expect(obs.silentProbes).toBe(1);
  });

  it("ответ после краевых молчаний сбрасывает счёт края", () => {
    const quiet: ProbeOutcome = { ...repaint, kind: "none", after: before };
    const edge = observeOutcome(observeOutcome(null, repaint, 0), quiet, 100)!;
    expect(edge.edgeSilences).toBe(1);
    expect(observeOutcome(edge, repaint, 200)!.edgeSilences).toBe(0);
  });
});

describe("шум между пробами и новый экран", () => {
  // Ревью 14.09: без накопления volatileRows все тесты оставались зелёными.
  // Часы тикнули во время ПЕРВОЙ молчаливой пробы и стояли во время второй —
  // строка часов обязана остаться шумом и не будить проверку.
  it("строка, дёрнувшаяся на первой пробе, остаётся шумом после второй", () => {
    const ev = new NavigationEvidence();
    ev.setScope("s");
    const base = screen(10);
    const clock = (t: string) => base.map((r, i) => (i === 0 ? `clock ${t}` : r));
    ev.record("page", silent(clock("a"), clock("b")), 0);
    ev.record("page", silent(clock("b"), clock("b")), MIN_REPROBE_MS);
    expect(ev.get("page")!.volatileRows).toEqual([0]);
    expect(ev.evaluate("page", { now: MIN_REPROBE_MS * 20, direction: DIR_UP, rows: clock("c") })).toBe("local");
  });

  it("новый экран (reset) оставляет только доказанное, без якоря", () => {
    const ev = new NavigationEvidence();
    ev.setScope("s");
    ev.record("page", { kind: "shift", edge: false, direction: DIR_UP, before: screen(5), after: screen(5) }, 0);
    ev.record("wheel", silent(screen(5), screen(5)), 0);
    ev.forgetScreen();
    expect(ev.get("page")!.state).toBe("confirmed");
    expect(ev.get("page")!.anchor).toBeUndefined();
    expect(ev.get("wheel")).toBeNull();
  });
});

describe("T-08: поздний ответ досматривается до края плана с запасом доставки (волна 7)", () => {
  const PROBE_WINDOW_MS = 1200;
  // Моменты проверок после молчаливой пробы, как их расставляет клиент:
  // отправка в 0, вердикт окна пробы в 1200, дальше по nextLateAnswerCheck.
  const checks = (legacy = false): number[] => {
    const out: number[] = [];
    let now = PROBE_WINDOW_MS;
    for (let wait = nextLateAnswerCheck(0, now, legacy); wait != null && out.length < 1000; wait = nextLateAnswerCheck(0, now, legacy)) {
      now += wait;
      out.push(now);
    }
    return out;
  };
  const firstSeen = (arrivedAt: number, legacy = false) => checks(legacy).find((t) => t >= arrivedAt);

  // probe-scroll-late-answer-live, 3000 мс: ответ приходил ПОСЛЕ единственной
  // проверки в 3,0 с — свидетельства не было, следующий ↑ снова пробовался.
  it("ответ на верхнем краю плана (3 с) плюс доставка становится свидетельством для следующего ↑", () => {
    for (const delivery of [0, 40, 250, 600, 900]) {
      const arrivedAt = LATE_ANSWER_MS + delivery;
      const seen = firstSeen(arrivedAt);
      expect(seen, `доставка ${delivery} мс`).toBeDefined();
      const ev = new NavigationEvidence();
      const before = screen(20, "live");
      ev.record("page", silent(before, before), PROBE_WINDOW_MS);
      expect(ev.evaluate("page", { now: PROBE_WINDOW_MS + 1, direction: DIR_UP, rows: before })).toBe("local");
      const older = [...screen(8, "older"), ...before.slice(0, 12)];
      ev.record("page", { kind: "shift", edge: false, direction: DIR_UP, before, after: older }, seen!);
      // Доказанный канал: следующий ↑ уходит без пробы — второго «страница не ответила» нет.
      expect(ev.evaluate("page", { now: seen! + 800, direction: DIR_UP, rows: older })).toBe("use");
    }
  });

  it("ответ раньше края засчитывается сразу, а не в конце досмотра", () => {
    for (const arrivedAt of [1500, 2000, 2600]) {
      expect(firstSeen(arrivedAt)! - arrivedAt, `${arrivedAt} мс`).toBeLessThanOrEqual(LATE_ANSWER_STEP_MS);
    }
  });

  it("досмотр конечен: последняя проверка — край плана с запасом, дальше ничего", () => {
    const all = checks();
    expect(all[all.length - 1]).toBe(LATE_ANSWER_MS + LATE_ANSWER_DELIVERY_MS);
    expect(all.length).toBeLessThanOrEqual(Math.ceil((LATE_ANSWER_MS + LATE_ANSWER_DELIVERY_MS - PROBE_WINDOW_MS) / LATE_ANSWER_STEP_MS));
    expect(nextLateAnswerCheck(0, LATE_ANSWER_MS + LATE_ANSWER_DELIVERY_MS)).toBeNull();
    expect(nextLateAnswerCheck(1000, 500)).toBeNull(); // часы назад
    expect(nextLateAnswerCheck(Number.NaN, 500)).toBeNull();
  });

  it("переключатель navigation ≠ v2 возвращает прежний досмотр: одна проверка через 3,0 с", () => {
    expect(checks(true)).toEqual([LATE_ANSWER_MS]);
    expect(firstSeen(LATE_ANSWER_MS + 40, true)).toBeUndefined();
  });

  // Волна 8 (скептик волны 7, verify:nav [1]): при ↓ (PgDn) сдвиг строк к
  // новому тексту — ровно то, как выглядит обычный новый вывод. Досмотр писал
  // его «поздним ответом» → weak → verify для ↑: три следующих ↑ уходили PgUp в
  // молчащее приложение, своя история не листалась. Окно волны 7 (1,35–4,0 с)
  // расширило класс, который был и при одной проверке в 3,0 с.
  it("↓: поздний сдвиг к новому неотличим от вывода — досмотра нет (v2); ↑ досматривается как прежде", () => {
    expect(nextLateAnswerCheck(0, PROBE_WINDOW_MS, false, DIR_DOWN)).toBeNull();
    expect(nextLateAnswerCheck(0, 2000, false, DIR_DOWN)).toBeNull();
    expect(nextLateAnswerCheck(0, PROBE_WINDOW_MS, false, DIR_UP)).toBe(LATE_ANSWER_STEP_MS);
    // Без направления — прежний контракт (намерение ↑).
    expect(nextLateAnswerCheck(0, PROBE_WINDOW_MS)).toBe(LATE_ANSWER_STEP_MS);
    // Откат navigation ≠ v2 — прежний досмотр побайтно: одна проверка в обе стороны.
    expect(nextLateAnswerCheck(0, PROBE_WINDOW_MS, true, DIR_DOWN)).toBe(LATE_ANSWER_MS - PROBE_WINDOW_MS);
  });

  it("мир скептика: PgDn молчит, вывод через 2,0 и 3,3 с, затем ↑ ×3 — со второго ↑ своя история", () => {
    for (const outputAt of [2000, 3300]) {
      const ev = new NavigationEvidence();
      ev.setScope("s");
      const before = screen(20, "live");
      // ↓ у низа: PgDn-проба, окно 1,2 с — молчание.
      ev.record("page", silent(before, before, DIR_DOWN), PROBE_WINDOW_MS);
      // Приложение печатает 4 строки — строки уезжают ВВЕРХ, как «ответ» на ↓.
      const printed = [...before.slice(4), ...screen(4, "printed")];
      // Досмотр так, как его расставляет клиент: проверка видит сдвиг, если он уже на экране.
      let now = PROBE_WINDOW_MS;
      for (let wait = nextLateAnswerCheck(0, now, false, DIR_DOWN); wait != null;
        wait = nextLateAnswerCheck(0, now, false, DIR_DOWN)) {
        now += wait;
        if (now >= outputAt) {
          ev.record("page", { kind: "shift", edge: false, direction: DIR_DOWN, before, after: printed }, now);
          break;
        }
      }
      const t0 = 4500;
      // ↑1 — законная проба (молчал только ↓), PgUp не отвечает.
      expect(ev.evaluate("page", { now: t0, direction: DIR_UP, rows: printed }), `${outputAt}: ↑1`).toBe("probe");
      ev.record("page", silent(printed, printed, DIR_UP), t0 + PROBE_WINDOW_MS);
      // ↑2, ↑3 — своя история, без PgUp.
      expect(ev.evaluate("page", { now: t0 + 1700, direction: DIR_UP, rows: printed }), `${outputAt}: ↑2`).toBe("local");
      expect(ev.evaluate("page", { now: t0 + 3400, direction: DIR_UP, rows: printed }), `${outputAt}: ↑3`).toBe("local");
    }
  });
});

describe("область действия, края, часы и проекция", () => {
  it("смена области действия стирает наблюдения, повтор той же — нет", () => {
    const ev = new NavigationEvidence();
    expect(ev.setScope("a")).toBe(true);
    ev.record("page", silent(screen(5), screen(5)), 0);
    expect(ev.setScope("a")).toBe(false);
    expect(ev.get("page")).not.toBeNull();
    expect(ev.setScope("b")).toBe(true);
    expect(ev.get("page")).toBeNull();
  });

  it("молчание на точном краевом действии ничего не записывает", () => {
    const outcome: ProbeOutcome = { kind: "none", edge: true, direction: DIR_UP, before: screen(5), after: screen(5) };
    expect(observeOutcome(null, outcome, 0)).toBeNull();
  });

  it("хранилище не перетирает живое наблюдение", () => {
    const ev = new NavigationEvidence();
    ev.setScope("s");
    ev.record("page", { kind: "shift", edge: false, direction: DIR_UP, before: [], after: [] }, 0);
    const stale = observeOutcome(null, silent(screen(5), screen(5)), 0)!;
    expect(ev.restore("page", stale)).toBe(false);
    expect(ev.get("page")!.state).toBe("confirmed");
    expect(ev.restore("wheel", stale)).toBe(true);
  });

  it("время, ушедшее назад, даёт одну пробу, а не падение и не вечный запрет", () => {
    const obs = observeOutcome(null, silent(screen(5), screen(5)), 10_000)!;
    expect(evaluateObservation(obs, { now: 5_000, direction: DIR_UP })).toBe("probe");
  });

  it("проекция для чистых правил канала: доказан / сейчас не отвечает / можно пробовать", () => {
    const rows = screen(5);
    const dead = observeOutcome(null, silent(rows, rows), 0)!;
    expect(answeredView(dead, { now: 1, direction: DIR_UP, rows })).toEqual({ answered: false, dead: DIR_UP });
    expect(answeredView(dead, { now: 1, direction: DIR_DOWN, rows })).toEqual({ answered: null, dead: 0 });
    expect(answeredView(null, { now: 1, direction: DIR_UP })).toEqual({ answered: null, dead: 0 });
    const alive = observeOutcome(null, { kind: "shift", edge: false, direction: DIR_UP, before: rows, after: rows }, 0);
    expect(answeredView(alive, { now: 1, direction: DIR_UP })).toEqual({ answered: true, dead: 0 });
  });
});
