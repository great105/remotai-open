/**
 * ST-05: L1-проверки координатора восстановления (T-24, T-25, T-28, T-36).
 *
 * Время и случайность детерминированы. Симулятор ниже будит автомат ТОЛЬКО в
 * nextWakeAt(): если сценарий проходит так, значит адаптеру хватает одного
 * таймера на соединение. Он же считает «отправку при запросе в пути», то есть
 * нарушение правила «не больше одного запроса».
 */
import { describe, expect, it } from "vitest";
import {
  BACKOFF_CAP_MS,
  DEGRADED_COOLDOWN_MS,
  MAX_NO_PROGRESS,
  MAX_WAIT_MS,
  RESIZE_HOLD_MAX_MS,
  RESPONSE_TIMEOUT_MS,
  SETTLE_MS,
  WIDTH_MIN_INTERVAL_MS,
  adopted,
  applied,
  applying,
  backoffDelay,
  capability,
  causeMask,
  causesForMarker,
  causesFromMask,
  connection,
  createRecoveryState,
  demand,
  due,
  epoch,
  frame,
  geometry,
  hasNewerDemand,
  holdForResize,
  negative,
  nextWakeAt,
  offline,
  online,
  resizeSent,
  serverGrid,
  streamReset,
  wake,
} from "./recoveryCoordinator";
import type {
  FrameVerdict,
  RecoveryCause,
  RecoveryFrameMeta,
  RecoverySend,
  RecoveryState,
  RecoveryStep,
} from "./recoveryCoordinator";

/** Почти максимум: огибающая Full Jitter видна как есть, backoff растёт предсказуемо. */
const R = 0.999;
const E1 = "stream:e1:1";
/** Что человек уже видит: кадр с base ≥ этого свежий, с меньшей базой устарел. */
const APPLIED = 500;

/** Живое соединение: conn=1, первый маркер уже был, сетка ревизии 7. */
function live(): RecoveryState {
  const s = connection(createRecoveryState({ geomRev: 7 }), 1).state;
  return epoch(s, E1, 0).state;
}

type Sent = RecoverySend & { at: number };
type Handler = (s: RecoveryState, now: number) => RecoveryStep;

class Sim {
  s: RecoveryState;
  now = 0;
  readonly sends: Sent[] = [];
  readonly verdicts: FrameVerdict[] = [];
  fallbacks = 0;
  /** Отправка, когда запрос уже в пути или кадр в применении. Должно быть 0. */
  overlaps = 0;
  onSend?: (send: Sent) => void;
  onFallback?: () => void;
  private readonly events: Array<{ at: number; seq: number; fn: Handler }> = [];
  private seq = 0;

  constructor(s: RecoveryState, readonly random = R) {
    this.s = s;
  }

  at(at: number, fn: Handler): void {
    this.events.push({ at, seq: this.seq++, fn });
  }

  private apply(result: RecoveryStep): void {
    const wasBusy = this.s.request !== null || this.s.pending !== null;
    this.s = result.state;
    for (const e of result.effects) {
      if (e.kind === "send") {
        if (wasBusy) this.overlaps++;
        const sent: Sent = { id: e.id, geomRev: e.geomRev, causes: e.causes, at: this.now };
        this.sends.push(sent);
        this.onSend?.(sent);
      } else if (e.kind === "fallback") {
        this.fallbacks++;
        this.onFallback?.();
      }
    }
  }

  run(until: number): void {
    for (let guard = 0; guard < 100_000; guard++) {
      this.events.sort((a, b) => a.at - b.at || a.seq - b.seq);
      const ev = this.events[0];
      const wakeAt = nextWakeAt(this.s);
      const t = Math.min(ev ? ev.at : Infinity, wakeAt ?? Infinity);
      if (t > until) {
        this.now = until;
        return;
      }
      this.now = Math.max(this.now, t);
      if (ev && ev.at <= (wakeAt ?? Infinity)) {
        this.events.shift();
        this.apply(ev.fn(this.s, this.now));
      } else {
        this.apply(wake(this.s, this.now, this.random));
      }
    }
    throw new Error("симуляция не сошлась: автомат будит себя без продвижения");
  }
}

interface ServeOpts {
  delay?: number;
  echoReq?: boolean;
  stale?: boolean;
  ok?: boolean;
  /** Какую причину поднимает адаптер, применяя кадр номер n (чужая высота/ширина). */
  raise?: (frameNo: number) => RecoveryCause | undefined;
  /** Отвечает ли агент на запрос номер n. */
  answer?: (sendNo: number) => boolean;
}

/** Адаптер как в PtyTermView: frame → applying → (решение геометрии) → applied. */
function adapterFrame(sim: Sim, s: RecoveryState, now: number, meta: RecoveryFrameMeta, opts: ServeOpts, frameNo: number): RecoveryStep {
  const f = frame(s, meta, APPLIED, now, sim.random);
  sim.verdicts.push(f.verdict);
  if (f.verdict !== "apply" || f.ticket === undefined) return f;
  let st = applying(f.state, f.ticket).state;
  const raised = opts.raise?.(frameNo);
  if (raised) st = demand(st, raised, now).state;
  return applied(st, f.ticket, opts.ok ?? true, now, sim.random);
}

function serve(sim: Sim, opts: ServeOpts = {}): void {
  let sendNo = 0;
  let frameNo = 0;
  sim.onSend = (send) => {
    sendNo++;
    if (opts.answer && !opts.answer(sendNo)) return;
    sim.at(send.at + (opts.delay ?? 50), (s, now) => {
      frameNo++;
      return adapterFrame(sim, s, now, {
        conn: s.conn,
        epoch: s.epoch,
        geomRev: send.geomRev,
        req: opts.echoReq ? send.id : undefined,
        base: opts.stale ? 10 : 1_000,
      }, opts, frameNo);
    });
  };
}

const gaps = (xs: readonly { at: number }[]) => xs.slice(1).map((x, i) => x.at - xs[i].at);

describe("T-24: одна актуальная операция восстановления", () => {
  it("20 одинаковых причин в пределах SETTLE дают ровно один запрос", () => {
    const sim = new Sim(live());
    serve(sim);
    for (let i = 0; i < 20; i++) sim.at(i * 20, (s, now) => demand(s, "queue-gap", now));
    sim.run(20_000);
    expect(sim.sends).toHaveLength(1);
    expect(sim.sends[0].causes).toEqual(["queue-gap"]);
    // debounce: последняя причина в 380 мс + тишина
    expect(sim.sends[0].at).toBe(380 + SETTLE_MS);
    expect(sim.overlaps).toBe(0);
    expect(sim.s.phase).toBe("idle");
    expect(sim.s.causes).toEqual([]);
  });

  it("непрерывные причины каждые 100 мс: отправка не позже firstDemandAt + MAX_WAIT_MS", () => {
    const sim = new Sim(live());
    serve(sim);
    for (let t = 0; t <= 6_000; t += 100) sim.at(t, (s, now) => demand(s, "foreground-gap", now));
    sim.run(6_000);
    // первая причина в 0, следующая пачка открывается в 1600 и в 3200
    expect(sim.sends.map((x) => x.at)).toEqual([MAX_WAIT_MS, 1_600 + MAX_WAIT_MS, 3_200 + MAX_WAIT_MS]);
    expect(sim.overlaps).toBe(0);
  });

  it("непрерывная смена геометрии: старый запрос устаревает, новый уходит не позже max-wait в новой сетке", () => {
    const sim = new Sim(live());
    serve(sim, { delay: 400 });
    sim.at(0, (s, now) => demand(s, "open", now));
    let rev = 7;
    for (let t = 0; t <= 3_000; t += 100) sim.at(t, (s, now) => geometry(s, ++rev, now));
    sim.run(20_000);
    expect(sim.sends.map((x) => x.at)).toEqual([1_500, 3_100]);
    // ответ на первый запрос пришёл в прежней сетке и отброшен, причины вернулись
    expect(sim.verdicts).toEqual(["discard:geometry", "apply"]);
    expect(sim.sends[1].geomRev).toBe(rev);
    expect(sim.sends[1].causes).toEqual(["open", "geometry-stale"]);
    expect(sim.s.phase).toBe("idle");
  });

  it("причина во время запроса: newer и ровно один следующий запрос после применения", () => {
    let s = demand(live(), "open", 0).state;
    const first = wake(s, SETTLE_MS);
    s = first.state;
    expect(first.send?.causes).toEqual(["open"]);
    for (let i = 0; i < 5; i++) s = demand(s, "queue-gap", 600 + i * 10).state;
    // та же причина open после отправки: этот кадр её не закрывает
    s = demand(s, "open", 650).state;
    expect(hasNewerDemand(s)).toBe(true);
    // второй запрос в пути невозможен, сколько бы ни прошло времени
    expect(due(s, 2_000).send).toBeUndefined();

    const f = frame(s, { conn: 1, epoch: E1, geomRev: 7, req: first.send!.id, base: 900 }, APPLIED, 700);
    expect(f.verdict).toBe("apply");
    s = applied(applying(f.state, f.ticket!).state, f.ticket!, true, 710).state;
    expect(s.phase).toBe("scheduled");
    expect(s.causes).toEqual(["open", "queue-gap"]);

    const sim = new Sim(s);
    sim.now = 710;
    serve(sim);
    sim.run(30_000);
    expect(sim.sends).toHaveLength(1);
    expect(sim.sends[0].causes).toEqual(["open", "queue-gap"]);
    // более новое требование ждало с 600: max-wait от него, а не от применения
    expect(sim.sends[0].at).toBe(1_210);
    expect(sim.s.phase).toBe("idle");
  });

  it("frame-stale: не больше MAX_NO_PROGRESS попыток с растущим backoff, затем degraded", () => {
    const sim = new Sim(live());
    serve(sim, { stale: true });
    sim.at(0, (s, now) => demand(s, "open", now));
    sim.run(120_000);
    expect(sim.sends).toHaveLength(MAX_NO_PROGRESS);
    expect(sim.verdicts).toEqual(Array(MAX_NO_PROGRESS).fill("retry:stale-base"));
    const g = gaps(sim.sends);
    expect(g).toEqual([50 + backoffDelay(1, R), 50 + backoffDelay(2, R)]);
    expect(g[1]).toBeGreaterThan(g[0]);
    expect(sim.s.phase).toBe("degraded");
    expect(sim.s.causes).toEqual(["open", "frame-stale"]);
    expect(nextWakeAt(sim.s)).toBeNull();
  });

  it("backoff Full Jitter: огибающая 1000·2^(n-1) до 8000, нулевая случайность не опускает повтор ниже SETTLE", () => {
    expect([1, 2, 3, 4, 5, 6].map((n) => backoffDelay(n, 1))).toEqual([1_000, 2_000, 4_000, 8_000, 8_000, 8_000]);
    expect(backoffDelay(3, 0)).toBe(0);
    expect(backoffDelay(2, 0.5)).toBe(1_000);
    let s = wake(demand(live(), "open", 0).state, SETTLE_MS).state;
    const f = frame(s, { conn: 1, epoch: E1, geomRev: 7, base: 10 }, APPLIED, 600, 0);
    s = f.state;
    expect(f.verdict).toBe("retry:stale-base");
    expect(nextWakeAt(s)).toBe(600 + SETTLE_MS);
  });

  it("кадр чужой высоты больше не зацикливает запрос раз в секунду", () => {
    const sim = new Sim(live());
    serve(sim, { raise: () => "geometry-height" });
    sim.at(0, (s, now) => demand(s, "open", now));
    sim.run(60_000);
    // прежний код: повтор примерно раз в секунду, ~60 запросов за минуту
    expect(sim.sends.length).toBeLessThanOrEqual(MAX_NO_PROGRESS);
    expect(sim.verdicts.every((v) => v === "apply")).toBe(true);
    expect(sim.s.phase).toBe("degraded");
    expect(sim.s.causes).toEqual(["geometry-height"]);
  });

  it("высота сошлась: прогресс обнуляет счётчик, автомат возвращается в idle", () => {
    const sim = new Sim(live());
    serve(sim, { raise: (n) => (n <= 2 ? "geometry-height" : undefined) });
    sim.at(0, (s, now) => demand(s, "open", now));
    sim.run(60_000);
    expect(sim.sends).toHaveLength(3);
    expect(sim.s.phase).toBe("idle");
    expect(sim.s.noProgress).toBe(0);
  });

  it("кадр чужой ширины: запросы с geometry-width не чаще WIDTH_MIN_INTERVAL_MS и не бесконечно", () => {
    const sim = new Sim(live());
    serve(sim, { raise: () => "geometry-width", ok: false });
    sim.at(0, (s, now) => demand(s, "open", now));
    sim.run(120_000);
    const widthSends = sim.sends.filter((x) => x.causes.includes("geometry-width"));
    expect(widthSends.length).toBeGreaterThanOrEqual(2);
    for (const g of gaps(widthSends)) expect(g).toBeGreaterThanOrEqual(WIDTH_MIN_INTERVAL_MS);
    expect(sim.sends).toHaveLength(MAX_NO_PROGRESS);
    // ok=false: кадр не записан, причина open не закрыта
    expect(sim.s.causes).toEqual(["open", "geometry-width"]);
    expect(sim.s.phase).toBe("degraded");
  });

  it("троттл ширины не переживает новую сетку и новую эпоху: epoch-reset уходит без 5-секундного ожидания", () => {
    // Кадр чужой ширины: запрос с geometry-width ушёл, троттл взведён.
    let s = demand(live(), "geometry-width", 0).state;
    const first = wake(s, SETTLE_MS);
    expect(first.send?.causes).toEqual(["geometry-width"]);
    s = first.state;
    // ответа нет: срок, повтор с причиной geometry-width ждёт троттл
    s = wake(s, SETTLE_MS + RESPONSE_TIMEOUT_MS, 0).state;
    expect(nextWakeAt(s)).toBe(SETTLE_MS + WIDTH_MIN_INTERVAL_MS);
    // смена эпохи посреди соединения (серверный resync): свой кадр сразу после тишины
    const at = SETTLE_MS + RESPONSE_TIMEOUT_MS + 10;
    let e = epoch(s, "stream:e1:2", at).state;
    e = demand(e, "epoch-reset", at).state;
    expect(nextWakeAt(e)).toBe(at + SETTLE_MS);
    // так же после новой локальной сетки
    let g = geometry(s, 8, at).state;
    expect(nextWakeAt(g)).toBe(at + SETTLE_MS);
    g = wake(g, at + SETTLE_MS).state;
    expect(g.phase).toBe("in-flight");
  });

  it("degraded: повтор уже провалившейся причины внутри окна не будит, новая причина даёт одну попытку", () => {
    const sim = new Sim(live());
    serve(sim, { stale: true });
    sim.at(0, (s, now) => demand(s, "open", now));
    sim.run(30_000);
    expect(sim.s.phase).toBe("degraded");
    // окно ещё открыто: degraded наступил в ~3,6 с
    expect(sim.s.degradedAt! + DEGRADED_COOLDOWN_MS).toBeGreaterThan(30_010);
    const before = sim.sends.length;
    sim.at(30_000, (s, now) => demand(s, "frame-stale", now));
    sim.at(30_010, (s, now) => demand(s, "open", now));
    sim.run(60_000);
    expect(sim.sends.length).toBe(before);
    sim.at(60_000, (s, now) => demand(s, "foreground-gap", now));
    sim.run(120_000);
    expect(sim.sends.length).toBe(before + 1);
    expect(sim.s.phase).toBe("degraded");
    expect(sim.overlaps).toBe(0);
  });

  it("degraded по queue-gap: частые queue-gap в окне молчат, после окна ровно одна попытка, её провал снова открывает окно", () => {
    // Сценарий ревью S1: агент молчит на queue-gap (транспорт без ACK).
    expect(DEGRADED_COOLDOWN_MS).toBeGreaterThanOrEqual(BACKOFF_CAP_MS);
    const sim = new Sim(live());
    sim.at(0, (s, now) => demand(s, "queue-gap", now));
    sim.run(20_000);
    expect(sim.sends).toHaveLength(MAX_NO_PROGRESS);
    expect(sim.s.phase).toBe("degraded");
    expect(sim.s.degradedCauses).toEqual(["queue-gap"]);
    const windowEnd = sim.s.degradedAt! + DEGRADED_COOLDOWN_MS;

    for (let t = sim.now; t < windowEnd; t += 250) sim.at(t, (s, now) => demand(s, "queue-gap", now));
    sim.run(windowEnd - 1);
    expect(sim.sends).toHaveLength(MAX_NO_PROGRESS);
    expect(nextWakeAt(sim.s)).toBeNull();

    // та же причина после окна — новая потеря данных: одна попытка
    sim.at(windowEnd, (s, now) => demand(s, "queue-gap", now));
    sim.run(windowEnd + 10_000);
    expect(sim.sends).toHaveLength(MAX_NO_PROGRESS + 1);
    expect(sim.sends[sim.sends.length - 1].at).toBe(windowEnd + SETTLE_MS);
    expect(sim.s.phase).toBe("degraded");
    // провал этой попытки открыл новое окно: queue-gap через секунду молчит
    expect(sim.s.degradedAt).toBe(windowEnd + SETTLE_MS + RESPONSE_TIMEOUT_MS);
    sim.at(sim.now + 1_000, (s, now) => demand(s, "queue-gap", now));
    sim.run(sim.now + 20_000);
    expect(sim.sends).toHaveLength(MAX_NO_PROGRESS + 1);

    // и через час снова одна попытка, а не вечное молчание
    const hour = 3_600_000;
    sim.at(hour, (s, now) => demand(s, "queue-gap", now));
    sim.run(hour + 10_000);
    expect(sim.sends).toHaveLength(MAX_NO_PROGRESS + 2);
    expect(sim.overlaps).toBe(0);
  });

  it("degraded: непрерывные причины при молчащем агенте — не чаще одного запроса за окно, но и не ноль", () => {
    const sim = new Sim(live());
    const until = 600_000;
    for (let t = 0; t <= until; t += 100) sim.at(t, (s, now) => demand(s, "queue-gap", now));
    sim.run(until);
    const after = sim.sends.slice(MAX_NO_PROGRESS - 1);
    expect(after.length).toBeGreaterThan(1);
    for (const g of gaps(after)) expect(g).toBeGreaterThanOrEqual(DEGRADED_COOLDOWN_MS + RESPONSE_TIMEOUT_MS);
    expect(sim.sends.length).toBeLessThanOrEqual(MAX_NO_PROGRESS + Math.ceil(until / (DEGRADED_COOLDOWN_MS + RESPONSE_TIMEOUT_MS)));
    expect(sim.overlaps).toBe(0);
  });

  it("v1 трижды not-ready: queue-gap через час снова даёт одну попытку", () => {
    // Сценарий ревью S2.
    const sim = new Sim(capability(live(), true).state);
    sim.onSend = (send) => sim.at(send.at + 30, (s, now) => negative(s, { req: send.id, reason: "not-ready" }, now, R));
    sim.at(0, (s, now) => demand(s, "queue-gap", now));
    sim.run(60_000);
    expect(sim.sends).toHaveLength(MAX_NO_PROGRESS);
    expect(sim.s.phase).toBe("degraded");
    sim.at(3_600_000, (s, now) => demand(s, "queue-gap", now));
    sim.run(3_700_000);
    expect(sim.sends).toHaveLength(MAX_NO_PROGRESS + 1);
    expect(sim.s.phase).toBe("degraded");
  });
});

describe("serverGrid: сервер подтвердил новую сетку PTY", () => {
  /** Сервер применяет resize строк через клиентский debounce 2000 мс: до этого кадры чужой высоты. */
  const SERVER_APPLIED_AT = 2_500;

  function heightSim(): Sim {
    // random 0,5: попытки в 500, 1050 и 2100 мс, все раньше, чем сервер применит строки (ревью S3)
    const sim = new Sim(live(), 0.5);
    serve(sim, { raise: () => (sim.now < SERVER_APPLIED_AT ? "geometry-height" : undefined) });
    sim.at(0, (s, now) => demand(s, "open", now));
    return sim;
  }

  it("degraded по высоте: без подтверждения окно молчит, подтверждение даёт одну попытку и idle", () => {
    const control = heightSim();
    control.run(4_000);
    expect(control.sends.map((x) => x.at)).toEqual([500, 1_050, 2_100]);
    expect(control.s.phase).toBe("degraded");
    expect(control.s.causes).toEqual(["geometry-height"]);
    // повтор той же причины внутри окна кадр правильной высоты не просит
    control.at(10_000, (s, now) => demand(s, "geometry-height", now));
    control.run(20_000);
    expect(control.sends).toHaveLength(MAX_NO_PROGRESS);

    const sim = heightSim();
    // /state опрашивается раз в 2 с: подтверждение приходит после коммита сервера
    sim.at(4_000, (s, now) => serverGrid(s, now));
    sim.run(20_000);
    expect(sim.sends.map((x) => x.at)).toEqual([500, 1_050, 2_100, 4_000 + SETTLE_MS]);
    expect(sim.s.phase).toBe("idle");
    expect(sim.s.causes).toEqual([]);
    expect(sim.overlaps).toBe(0);
  });

  it("подтверждение, пока запрос в пути: после его провала гарантирована ещё одна попытка", () => {
    const sim = heightSim();
    // третий запрос ушёл в 2100 до подтверждения, его кадр придёт в 2150 чужой высоты
    sim.at(2_120, (s, now) => {
      expect(s.phase).toBe("in-flight");
      expect(s.noProgress).toBe(MAX_NO_PROGRESS - 1);
      return serverGrid(s, now);
    });
    sim.run(20_000);
    expect(sim.sends).toHaveLength(MAX_NO_PROGRESS + 1);
    expect(sim.s.phase).toBe("idle");
    expect(sim.overlaps).toBe(0);
  });

  it("без причин высоты/ширины и при unavailable подтверждение ничего не меняет", () => {
    const idle = live();
    expect(serverGrid(idle, 0).state).toBe(idle);

    const sim = new Sim(live());
    sim.at(0, (s, now) => demand(s, "queue-gap", now));
    sim.run(20_000);
    expect(sim.s.phase).toBe("degraded");
    expect(serverGrid(sim.s, 20_000).state).toBe(sim.s);

    let s = capability(live(), true).state;
    s = demand(demand(s, "open", 0).state, "geometry-height", 0).state;
    const sent = wake(s, SETTLE_MS);
    s = negative(sent.state, { req: sent.send!.id, reason: "unavailable" }, 600).state;
    expect(s.unavailable).toBe(true);
    const r = serverGrid(s, 700);
    expect(r.state).toBe(s);
    expect(r.effects).toEqual([]);
  });
});

describe("T-25: запоздалые кадры и таймеры не меняют новое состояние", () => {
  /** v1: запрос 1 истёк, в пути запрос 2. */
  function withSupersededRequest(): RecoveryState {
    let s = capability(live(), true).state;
    s = demand(s, "open", 0).state;
    s = wake(s, SETTLE_MS).state;
    s = wake(s, SETTLE_MS + RESPONSE_TIMEOUT_MS, R).state;
    expect(s.late?.id).toBe(1);
    s = wake(s, nextWakeAt(s)!, R).state;
    expect(s.request?.id).toBe(2);
    return s;
  }

  it("кадр прежнего conn, epoch, geomRev или вытесненного req: discard, состояние то же самое", () => {
    const s = withSupersededRequest();
    const cases: Array<[RecoveryFrameMeta, FrameVerdict]> = [
      [{ conn: 0, epoch: E1, geomRev: 7, req: 2, base: 900 }, "discard:conn"],
      [{ conn: 1, epoch: "stream:e0:9", geomRev: 7, req: 2, base: 900 }, "discard:epoch"],
      [{ conn: 1, epoch: E1, geomRev: 6, req: 2, base: 900 }, "discard:geometry"],
      [{ conn: 1, epoch: E1, geomRev: "7", req: 2, base: 900 }, "discard:geometry"],
      [{ conn: 1, epoch: E1, geomRev: 7, req: 1, base: 900 }, "discard:old-request"],
    ];
    for (const [meta, verdict] of cases) {
      const f = frame(s, meta, APPLIED, 9_000, R);
      expect(f.verdict).toBe(verdict);
      expect(f.state).toBe(s);
      expect(f.effects).toEqual([]);
    }
    // и даже устаревшая база у чужого кадра не трогает счётчики
    expect(frame(s, { conn: 1, epoch: E1, geomRev: 7, req: 1, base: 10 }, APPLIED, 9_000).state).toBe(s);
    // текущий запрос по-прежнему ждёт свой кадр
    expect(frame(s, { conn: 1, epoch: E1, geomRev: 7, req: 2, base: 900 }, APPLIED, 9_000).verdict).toBe("apply");
  });

  it("смена сетки и эпохи во время запроса: причины возвращаются, поздний ответ отброшен", () => {
    let s = wake(demand(live(), "open", 0).state, SETTLE_MS).state;
    s = geometry(s, 8, 600).state;
    expect(s.phase).toBe("scheduled");
    expect(s.causes).toEqual(["open", "geometry-stale"]);
    expect(frame(s, { conn: 1, epoch: E1, geomRev: 7, base: 900 }, APPLIED, 700).verdict).toBe("discard:geometry");
    const next = wake(s, nextWakeAt(s)!);
    expect(next.send?.geomRev).toBe(8);

    let t = next.state;
    t = epoch(t, "stream:e1:2", 1_200).state;
    expect(t.request).toBeNull();
    expect(t.causes).toEqual(["open", "geometry-stale"]);
    expect(frame(t, { conn: 1, epoch: E1, geomRev: 8, base: 900 }, APPLIED, 1_300).verdict).toBe("discard:epoch");
  });

  it("колбэк applied от кадра прежней эпохи — пустышка (I-08)", () => {
    let s = wake(demand(live(), "open", 0).state, SETTLE_MS).state;
    const f = frame(s, { conn: 1, epoch: E1, geomRev: 7, base: 900 }, APPLIED, 600);
    s = applying(f.state, f.ticket!).state;
    s = epoch(s, "stream:e1:2", 650).state;
    expect(s.pending).toBeNull();
    expect(applied(s, f.ticket!, true, 700).state).toBe(s);
  });

  it("поздний таймер после connection() ничего не шлёт", () => {
    let s = demand(live(), "open", 0).state;
    const oldWake = nextWakeAt(s)!;
    s = connection(s, 2).state;
    expect(nextWakeAt(s)).toBeNull();
    const w = wake(s, oldWake, R);
    expect(w.effects).toEqual([]);
    expect(w.state).toBe(s);

    // запрос был в пути: срок старого соединения ничего не делает
    let t = wake(demand(live(), "open", 0).state, SETTLE_MS).state;
    t = connection(t, 2).state;
    expect(wake(t, SETTLE_MS + RESPONSE_TIMEOUT_MS, R).effects).toEqual([]);
    // новое соединение: старый таймер раньше нового срока не отправит
    t = epoch(t, "stream:e2:1", 1_000).state;
    t = demand(t, "open", 1_000).state;
    expect(wake(t, 1_200).send).toBeUndefined();
    const sent = wake(t, 1_000 + SETTLE_MS).send;
    expect(sent).toBeDefined();
    // номера запросов не повторяются между соединениями
    expect(sent!.id).toBe(2);
  });
});

describe("T-28: маркеры и холодный старт", () => {
  it("causesForMarker различает warm, cold-restore, stream-gap, epoch-reset и baseline", () => {
    const base = { gap: false, epochChanged: false, termVirgin: false, firstMarker: false };
    expect(causesForMarker({ ...base, marker: "reset", firstMarker: true, termVirgin: true, epochChanged: true }))
      .toEqual({ path: "baseline", causes: [] });
    expect(causesForMarker({ ...base, marker: "reset", epochChanged: true })).toEqual({ path: "epoch-reset", causes: ["epoch-reset"] });
    expect(causesForMarker({ ...base, marker: "resumed", firstMarker: true })).toEqual({ path: "warm", causes: [] });
    expect(causesForMarker({ ...base, marker: "resumed" })).toEqual({ path: "warm", causes: [] });
    expect(causesForMarker({ ...base, marker: "resumed", firstMarker: true, termVirgin: true }))
      .toEqual({ path: "cold-restore", causes: ["cold-restore"] });
    expect(causesForMarker({ ...base, marker: "resumed", firstMarker: true, termVirgin: true, gap: true }))
      .toEqual({ path: "cold-restore", causes: ["cold-restore", "stream-gap"] });
    expect(causesForMarker({ ...base, marker: "resumed", gap: true })).toEqual({ path: "stream-gap", causes: ["stream-gap"] });
    expect(causesForMarker({ ...base, marker: "resumed", epochChanged: true })).toEqual({ path: "epoch-reset", causes: ["epoch-reset"] });
  });

  /** Пустая страница с сохранённым offset: resumed + хвост, кадра нет. */
  function coldPage(sim: Sim, at: number): void {
    sim.at(at, (s, now) => {
      let st = demand(s, "open", now).state;
      const m = causesForMarker({ marker: "resumed", gap: false, epochChanged: false, termVirgin: true, firstMarker: true });
      for (const c of m.causes) st = demand(st, c, now).state;
      return { state: st, effects: [] };
    });
  }

  it("cold-restore без кадра: ровно один fallback reconnect-reset, без цикла", () => {
    const sim = new Sim(live());
    // агент молчит всегда (транспорт без ACK, старый агент)
    sim.onFallback = () => {
      sim.at(sim.now + 100, (s, now) => {
        let st = connection(s, s.conn + 1).state;
        st = epoch(st, `stream:e1:${st.conn}`, now).state;
        return { state: st, effects: [] };
      });
      // худший случай: страница снова пустая и снова resumed
      coldPage(sim, sim.now + 100);
    };
    coldPage(sim, 0);
    sim.run(120_000);
    expect(sim.sends[0].causes).toEqual(["open", "cold-restore"]);
    expect(sim.fallbacks).toBe(1);
    // первое соединение: один запрос и откат; второе: MAX_NO_PROGRESS попыток
    expect(sim.sends).toHaveLength(1 + MAX_NO_PROGRESS);
    expect(sim.s.conn).toBe(2);
    expect(sim.s.fallbackUsed).toBe(true);
    expect(sim.s.phase).toBe("degraded");
  });

  it("кадр пришёл в срок: cold-restore закрыт, отката нет", () => {
    const sim = new Sim(live());
    serve(sim);
    coldPage(sim, 0);
    sim.run(60_000);
    expect(sim.sends).toHaveLength(1);
    expect(sim.fallbacks).toBe(0);
    expect(sim.s.phase).toBe("idle");
    expect(sim.s.causes).toEqual([]);
  });

  it("cold-restore и screen-none unavailable: один fallback, до нового соединения больше не просим", () => {
    let s = capability(live(), true).state;
    s = demand(demand(s, "open", 0).state, "cold-restore", 0).state;
    const sent = wake(s, SETTLE_MS);
    s = sent.state;
    const n = negative(s, { req: sent.send!.id, reason: "unavailable" }, 700);
    expect(n.effects.filter((e) => e.kind === "fallback")).toHaveLength(1);
    s = n.state;
    expect(s.phase).toBe("degraded");
    expect(s.unavailable).toBe(true);
    s = demand(s, "queue-gap", 800).state;
    expect(nextWakeAt(s)).toBeNull();

    // новое соединение на том же транспорте: откат уже потрачен
    s = epoch(connection(s, 2).state, "stream:e1:2", 1_000).state;
    s = capability(s, true).state;
    s = demand(demand(s, "open", 1_000).state, "cold-restore", 1_000).state;
    const again = wake(s, 1_000 + SETTLE_MS);
    const n2 = negative(again.state, { req: again.send!.id, reason: "unavailable" }, 1_600);
    expect(n2.effects.filter((e) => e.kind === "fallback")).toHaveLength(0);
    expect(n2.state.phase).toBe("degraded");
  });

  it("cold-restore и трижды not-ready (или устаревшая база): degraded с одним fallback, не пустая страница навсегда", () => {
    // Холодная страница, v1 отвечает not-ready, пока зеркало не в Ground.
    const sim = new Sim(capability(live(), true).state);
    sim.onSend = (send) => sim.at(send.at + 30, (s, now) => negative(s, { req: send.id, reason: "not-ready" }, now, R));
    coldPage(sim, 0);
    sim.run(60_000);
    expect(sim.sends).toHaveLength(MAX_NO_PROGRESS);
    expect(sim.fallbacks).toBe(1);
    expect(sim.s.phase).toBe("degraded");
    expect(sim.s.fallbackUsed).toBe(true);

    // та же картина с устаревшей базой кадра
    const stale = new Sim(live());
    serve(stale, { stale: true });
    coldPage(stale, 0);
    stale.run(60_000);
    expect(stale.fallbacks).toBe(1);

    // тёплая страница, degraded по той же причине: отката нет
    const warm = new Sim(capability(live(), true).state);
    warm.onSend = (send) => warm.at(send.at + 30, (s, now) => negative(s, { req: send.id, reason: "not-ready" }, now, R));
    warm.at(0, (s, now) => demand(s, "queue-gap", now));
    warm.run(60_000);
    expect(warm.s.phase).toBe("degraded");
    expect(warm.fallbacks).toBe(0);
  });

  it("causeMask: набор причин одним числом и обратно, порядок не важен", () => {
    const all: RecoveryCause[] = ["open", "cold-restore", "epoch-reset", "stream-gap", "queue-gap",
      "foreground-gap", "frame-stale", "geometry-stale", "geometry-height", "geometry-width"];
    expect(causeMask([])).toBe(0);
    expect(causesFromMask(causeMask(all))).toEqual(all);
    expect(causeMask(["queue-gap", "open"])).toBe(causeMask(["open", "queue-gap"]));
    expect(causesFromMask(causeMask(["geometry-height", "open"]))).toEqual(["open", "geometry-height"]);
  });

  it("resumed с пропуском посреди соединения порождает stream-gap и один запрос", () => {
    const sim = new Sim(live());
    serve(sim);
    sim.at(5_000, (s, now) => {
      let st = epoch(s, "stream:e1:2", now).state;
      for (const c of causesForMarker({ marker: "resumed", gap: true, epochChanged: false, termVirgin: false, firstMarker: false }).causes) {
        st = demand(st, c, now).state;
      }
      return { state: st, effects: [] };
    });
    sim.run(20_000);
    expect(sim.sends).toHaveLength(1);
    expect(sim.sends[0].causes).toEqual(["stream-gap"]);
    expect(sim.s.phase).toBe("idle");
  });
});

describe("T-36: совместимость с агентом без screen-request-v1", () => {
  it("legacy без подтверждения: последовательный путь с таймаутом, в пути не больше одного запроса", () => {
    const sim = new Sim(live());
    serve(sim, { answer: (n) => n >= 3 });
    sim.at(0, (s, now) => demand(s, "open", now));
    sim.run(60_000);
    expect(sim.sends.map((x) => x.at)).toEqual([
      SETTLE_MS,
      SETTLE_MS + RESPONSE_TIMEOUT_MS + backoffDelay(1, R),
      SETTLE_MS + 2 * RESPONSE_TIMEOUT_MS + backoffDelay(1, R) + backoffDelay(2, R),
    ]);
    for (const g of gaps(sim.sends)) expect(g).toBeGreaterThanOrEqual(RESPONSE_TIMEOUT_MS);
    expect(sim.overlaps).toBe(0);
    // ответ без эха req на наш запрос выдаёт старого агента
    expect(sim.s.peer).toBe("legacy");
    expect(sim.verdicts).toEqual(["apply"]);
    expect(sim.s.phase).toBe("idle");
    expect(sim.s.noProgress).toBe(0);
  });

  it("причины сыплются, пока агент молчит: в пути всегда один запрос, отправки ограничены сроком ответа", () => {
    const sim = new Sim(live());
    // агент молчит: ни кадра, ни отказа (транспорт без ACK, старый агент)
    for (let t = 0; t <= 20_000; t += 100) {
      const cause: RecoveryCause = t % 200 === 0 ? "queue-gap" : "foreground-gap";
      sim.at(t, (s, now) => demand(s, cause, now));
    }
    sim.run(20_000);
    expect(sim.overlaps).toBe(0);
    // max-wait → срок → backoff 1 → срок → backoff 2 → срок → degraded
    expect(sim.sends.map((x) => x.at)).toEqual([1_500, 6_000, 10_998]);
    for (const g of gaps(sim.sends)) expect(g).toBeGreaterThanOrEqual(RESPONSE_TIMEOUT_MS);
    expect(sim.s.phase).toBe("degraded");
  });

  it("legacy: ответ после срока принимается, пока новый запрос не ушёл", () => {
    let s = wake(demand(live(), "open", 0).state, SETTLE_MS).state;
    s = wake(s, SETTLE_MS + RESPONSE_TIMEOUT_MS, R).state;
    expect(s.phase).toBe("scheduled");
    const f = frame(s, { conn: 1, epoch: E1, geomRev: 7, base: 900 }, APPLIED, 3_600);
    expect(f.verdict).toBe("apply");
    s = applied(f.state, f.ticket!, true, 3_610).state;
    expect(s.phase).toBe("idle");
    expect(nextWakeAt(s)).toBeNull();
  });

  it("v1: эхо req сопоставляет ответ, ответ без req тоже принимается, отказ чужого req игнорируется", () => {
    let s = capability(live(), true).state;
    s = wake(demand(s, "open", 0).state, SETTLE_MS).state;
    expect(negative(s, { req: 99, reason: "not-ready" }, 600).state).toBe(s);
    expect(frame(s, { conn: 1, epoch: E1, geomRev: 7, req: 99, base: 900 }, APPLIED, 600).verdict).toBe("discard:old-request");
    const noEcho = frame(s, { conn: 1, epoch: E1, geomRev: 7, base: 900 }, APPLIED, 600);
    expect(noEcho.verdict).toBe("apply");
    expect(noEcho.state.peer).toBe("v1");
  });

  it("v1 not-ready: повтор с backoff, не больше MAX_NO_PROGRESS попыток", () => {
    const sim = new Sim(capability(live(), true).state);
    sim.onSend = (send) => sim.at(send.at + 30, (s, now) => negative(s, { req: send.id, reason: "not-ready" }, now, R));
    sim.at(0, (s, now) => demand(s, "queue-gap", now));
    sim.run(60_000);
    expect(sim.sends).toHaveLength(MAX_NO_PROGRESS);
    expect(sim.s.phase).toBe("degraded");
    expect(sim.overlaps).toBe(0);
  });

  it("v1 resize-pending: ждём новую сетку до срока ответа, geometry() снимает ожидание", () => {
    let s = capability(live(), true).state;
    const sent = wake(demand(s, "open", 0).state, SETTLE_MS);
    s = negative(sent.state, { req: sent.send!.id, reason: "resize-pending" }, 600, R).state;
    expect(nextWakeAt(s)).toBe(600 + RESPONSE_TIMEOUT_MS);
    s = geometry(s, 8, 700).state;
    expect(nextWakeAt(s)).toBe(700 + SETTLE_MS);
    expect(s.noProgress).toBe(0);
  });

  it("v1 resize-pending: сервер повторяет сам с тем же req — его кадр принимается до нашего повтора", () => {
    let s = capability(live(), true).state;
    const sent = wake(demand(s, "open", 0).state, SETTLE_MS);
    s = negative(sent.state, { req: sent.send!.id, reason: "resize-pending" }, 600, R).state;
    expect(s.phase).toBe("scheduled");
    expect(s.late?.id).toBe(sent.send!.id);
    // после ACK resize сервер снимает кадр по той же просьбе
    const f = frame(s, { conn: 1, epoch: E1, geomRev: 7, req: sent.send!.id, base: 900 }, APPLIED, 1_200);
    expect(f.verdict).toBe("apply");
    s = applied(applying(f.state, f.ticket!).state, f.ticket!, true, 1_210).state;
    expect(s.phase).toBe("idle");
    expect(nextWakeAt(s)).toBeNull();
    // чужой req по-прежнему не принимается
    const other = negative(wake(demand(capability(live(), true).state, "open", 0).state, SETTLE_MS).state,
      { req: 1, reason: "resize-pending" }, 600, R).state;
    expect(frame(other, { conn: 1, epoch: E1, geomRev: 7, req: 5, base: 900 }, APPLIED, 700).verdict).toBe("discard:old-request");
  });

  it("кадр без req вне операции (внутренний повтор старого агента): apply:unsolicited, координатор не меняется", () => {
    const meta = { conn: 1, epoch: E1, geomRev: 7, base: 900 };
    // idle: запроса нет вовсе
    const idle = live();
    const a = frame(idle, meta, APPLIED, 100);
    expect(a.verdict).toBe("apply:unsolicited");
    expect(a.state).toBe(idle);
    expect(a.ticket).toBeUndefined();
    expect(a.effects).toEqual([]);
    // degraded после трёх устаревших кадров: сервер прислал свежий сам
    const sim = new Sim(live());
    serve(sim, { stale: true });
    sim.at(0, (s, now) => demand(s, "open", now));
    sim.run(30_000);
    expect(sim.s.phase).toBe("degraded");
    const d = frame(sim.s, meta, APPLIED, 30_000);
    expect(d.verdict).toBe("apply:unsolicited");
    expect(d.state).toBe(sim.s);
    // устаревший такой кадр не пишется (I-05), с req — это ответ вытесненной просьбе
    expect(frame(idle, { ...meta, base: 10 }, APPLIED, 100).verdict).toBe("discard:old-request");
    expect(frame(idle, { ...meta, req: 3 }, APPLIED, 100).verdict).toBe("discard:old-request");
    // чужие соединение, эпоха и сетка проверяются раньше
    expect(frame(idle, { ...meta, geomRev: 6 }, APPLIED, 100).verdict).toBe("discard:geometry");
    expect(frame(idle, { ...meta, epoch: "stream:e0:9" }, APPLIED, 100).verdict).toBe("discard:epoch");
    // другой кадр в применении: второй не пишется поверх
    const busy = frame(wake(demand(live(), "open", 0).state, SETTLE_MS).state, meta, APPLIED, 600).state;
    expect(busy.phase).toBe("validating");
    expect(frame(busy, meta, APPLIED, 650).verdict).toBe("discard:old-request");
  });

  it("кадр без req в чужой ревизии вне операции: одна просьба на ревизию (прежнее правило старого агента)", () => {
    const idle = geometry(live(), 8, 0).state; // сетка сменилась, операции нет
    expect(idle.phase).toBe("idle");
    const stale = { conn: 1, epoch: E1, geomRev: 7, base: 900 };
    const first = frame(idle, stale, APPLIED, 100);
    expect(first.verdict).toBe("discard:geometry");
    expect(first.state.phase).toBe("scheduled");
    expect(first.state.causes).toEqual(["geometry-stale"]);
    // тот же кадр ещё раз — ревизия уже спрошена: состояние то же
    const second = frame(first.state, stale, APPLIED, 150);
    expect(second.verdict).toBe("discard:geometry");
    expect(second.state).toBe(first.state);
    const sent = wake(first.state, 100 + SETTLE_MS);
    expect(sent.send?.geomRev).toBe(8);
    expect(sent.send?.causes).toEqual(["geometry-stale"]);
    // в пути запрос: чужой кадр ничего не просит (T-25)
    expect(frame(sent.state, stale, APPLIED, 700).state).toBe(sent.state);
    // ответ вытесненной просьбе (с req) — тоже ничего
    expect(frame(idle, { ...stale, req: 3 }, APPLIED, 100).state).toBe(idle);
    // новая ревизия — снова одна просьба
    const next = geometry(idle, 9, 200).state;
    expect(frame(next, stale, APPLIED, 300).state.causes).toEqual(["geometry-stale"]);
  });

  it("один таймер: nextWakeAt — срок отправки, срок ответа или срок применения", () => {
    let s = live();
    expect(nextWakeAt(s)).toBeNull();
    s = demand(s, "open", 0).state;
    expect(nextWakeAt(s)).toBe(SETTLE_MS);
    s = wake(s, SETTLE_MS).state;
    expect(nextWakeAt(s)).toBe(SETTLE_MS + RESPONSE_TIMEOUT_MS);
    const f = frame(s, { conn: 1, epoch: E1, geomRev: 7, base: 900 }, APPLIED, 600);
    s = f.state;
    expect(s.phase).toBe("validating");
    expect(nextWakeAt(s)).toBe(600 + RESPONSE_TIMEOUT_MS);
    // writer так и не ответил: кадр считается не записанным, причина open жива
    s = wake(s, 600 + RESPONSE_TIMEOUT_MS, R).state;
    expect(s.phase).toBe("scheduled");
    expect(s.causes).toEqual(["open"]);
    expect(s.noProgress).toBe(1);
  });
});

describe("обрыв сокета: сроки умершего соединения ничего не решают (ревью 14.09, T-25, T-28)", () => {
  /** Холодная страница на живом соединении: open + cold-restore, запрос в пути. */
  function coldInFlight(): RecoveryState {
    let s = demand(demand(live(), "open", 0).state, "cold-restore", 0).state;
    s = wake(s, SETTLE_MS).state;
    expect(s.phase).toBe("in-flight");
    return s;
  }

  it("обрыв во время запроса cold-restore: будильника нет, старый срок не даёт ни повтора, ни отката", () => {
    const s0 = coldInFlight();
    const deadline = nextWakeAt(s0)!;
    const off = offline(s0);
    expect(off.effects).toEqual([expect.objectContaining({ kind: "diag", what: "invalidated", id: 1, reason: "offline" })]);
    const s = off.state;
    expect(s.offline).toBe(true);
    expect(s.phase).toBe("idle");
    expect(s.request).toBeNull();
    expect(nextWakeAt(s)).toBeNull();
    // срок умершего запроса истёк в backoff: ни эффектов, ни нового состояния
    const w = wake(s, deadline + 1, R);
    expect(w.effects).toEqual([]);
    expect(w.state).toBe(s);
    expect(s.fallbackUsed).toBe(false);
    expect(s.coldPage).toBe(true);
    // onclose после drop-closed — тот же offline
    expect(offline(s).state).toBe(s);
    // кадр, пришедший без соединения, не операция
    expect(frame(s, { conn: s.conn, epoch: s.epoch, geomRev: 7, base: 900 }, APPLIED, deadline).verdict).toBe("discard:conn");
  });

  it("без соединения причины копятся без отправки; online() уводит их обычной тишиной", () => {
    let s = offline(connection(live(), 2).state).state;
    s = demand(s, "foreground-gap", 100).state;
    s = demand(s, "queue-gap", 200).state;
    expect(s.phase).toBe("idle");
    expect(nextWakeAt(s)).toBeNull();
    expect(due(s, 10_000).effects).toEqual([]);
    s = online(s, 1_000).state;
    expect(s.offline).toBe(false);
    expect(nextWakeAt(s)).toBe(1_000 + SETTLE_MS);
    expect(wake(s, 1_000 + SETTLE_MS).send?.causes).toEqual(["queue-gap", "foreground-gap"]);
    // online без причин — просто idle без будильника
    const quiet = online(offline(live()).state, 0).state;
    expect(quiet.phase).toBe("idle");
    expect(nextWakeAt(quiet)).toBeNull();
    // online на живом соединении ничего не меняет
    const alive = live();
    expect(online(alive, 0).state).toBe(alive);
  });

  it("холодная страница переживает обрыв: следующий resumed снова cold-restore, откат ровно один и на живом соединении", () => {
    const sim = new Sim(live());
    const fallbackAt: number[] = [];
    /** Маркер нового соединения: xterm уже держит хвост, пустоту выдаёт только coldPage. */
    const reconnect = (marker: "reset" | "resumed") => (s: RecoveryState, now: number): RecoveryStep => {
      let st = offline(connection(s, s.conn + 1).state).state;
      st = online(st, now).state;
      st = epoch(st, `stream:e1:${st.conn}`, now).state;
      const route = causesForMarker({ marker, gap: false, epochChanged: false, termVirgin: st.coldPage, firstMarker: true });
      if (marker === "reset") st = streamReset(st).state;
      for (const c of route.path === "baseline" ? ["open" as const] : route.causes) st = demand(st, c, now).state;
      return { state: st, effects: [] };
    };
    sim.onFallback = () => {
      fallbackAt.push(sim.now);
      sim.at(sim.now + 100, reconnect("reset"));
    };
    // холодная страница: open + cold-restore, агент молчит всегда
    sim.at(0, (s, now) => demand(demand(s, "open", now).state, "cold-restore", now));
    // обрыв через 2300 мс после запроса, backoff реконнекта ~1 с — дольше срока ответа
    sim.at(SETTLE_MS + 2_300, (s) => offline(s));
    sim.at(SETTLE_MS + 3_300, reconnect("resumed"));
    sim.run(60_000);
    // срок умершего запроса (500 + 3000) прошёл без отката
    expect(fallbackAt).toHaveLength(1);
    expect(fallbackAt[0]).toBeGreaterThanOrEqual(SETTLE_MS + 3_300 + SETTLE_MS + RESPONSE_TIMEOUT_MS);
    // второе соединение снова просило cold-restore
    expect(sim.sends[1].causes).toContain("cold-restore");
    expect(sim.overlaps).toBe(0);
    // откат дал reset: страница больше не холодная, откат потрачен
    expect(sim.s.conn).toBe(3);
    expect(sim.s.coldPage).toBe(false);
    expect(sim.s.fallbackUsed).toBe(true);
  });

  it("coldPage закрывают записанный кадр cold-restore и маркер reset, но не незаписанный кадр", () => {
    let s = coldInFlight();
    const f = frame(s, { conn: 1, epoch: E1, geomRev: 7, base: 900 }, APPLIED, 600);
    s = applying(f.state, f.ticket!).state;
    expect(applied(s, f.ticket!, false, 700).state.coldPage).toBe(true);
    expect(applied(s, f.ticket!, true, 700).state.coldPage).toBe(false);
    expect(streamReset(coldInFlight()).state.coldPage).toBe(false);
    const warm = live();
    expect(streamReset(warm).state).toBe(warm);
    // новое соединение холодную страницу не забывает
    expect(connection(coldInFlight(), 2).state.coldPage).toBe(true);
  });

  it("неснятые причины переживают обрыв: тёплый resume на странице без кадра всё равно просит кадр (ревью 14.09)", () => {
    // Первое открытие: open в пути, ответ не пришёл, обрыв.
    let s = demand(live(), "open", 0).state;
    s = wake(s, SETTLE_MS).state;
    expect(s.request?.causes).toEqual(["open"]);
    s = offline(s).state;
    expect(s.owed).toEqual(["open"]);
    expect(nextWakeAt(s)).toBeNull();
    // Реконнект по backoff, рукопожатие, onopen.
    s = offline(connection(s, s.conn + 1).state).state;
    expect(nextWakeAt(s)).toBeNull();
    s = online(s, 5_000).state;
    // Маркер resumed без пропуска на странице с хвостом — путь warm, своих причин нет.
    s = epoch(s, `stream:e1:${s.conn}`, 5_000).state;
    const route = causesForMarker({ marker: "resumed", gap: false, epochChanged: false, termVirgin: false, firstMarker: true });
    expect(route).toEqual({ path: "warm", causes: [] });
    expect(wake(s, 5_000 + SETTLE_MS).send?.causes).toEqual(["open"]);
    // Честно тёплая страница (кадр был применён) — просить нечего (ST-09 A6).
    const quiet = online(offline(connection(offline(live()).state, 3).state).state, 0).state;
    expect(quiet.causes).toEqual([]);
    expect(nextWakeAt(quiet)).toBeNull();
  });
});

describe("ревью 15.09, дефект 1: глубина истории на тёплом переподключении (hist-probe)", () => {
  /** Обрыв и тёплый resume живой страницы, уже получившей кадр: onopen и маркер, как в адаптере. */
  const warmReconnect = (histProbe: boolean) => (s: RecoveryState, now: number): RecoveryStep => {
    let st = offline(connection(s, s.conn + 1).state).state;
    st = online(st, now).state;
    st = epoch(st, `stream:e1:${st.conn}`, now).state;
    const route = causesForMarker({ marker: "resumed", gap: false, epochChanged: false, termVirgin: false, firstMarker: true, histProbe });
    for (const c of route.causes) st = demand(st, c, now).state;
    return { state: st, effects: [] };
  };

  it("causesForMarker: histProbe превращает только warm в hist-probe, остальные пути прежние", () => {
    const base = { gap: false, epochChanged: false, termVirgin: false, firstMarker: true, histProbe: true };
    expect(causesForMarker({ ...base, marker: "resumed" })).toEqual({ path: "hist-probe", causes: ["hist-probe"] });
    expect(causesForMarker({ ...base, marker: "resumed", histProbe: false })).toEqual({ path: "warm", causes: [] });
    expect(causesForMarker({ ...base, marker: "resumed", histProbe: undefined })).toEqual({ path: "warm", causes: [] });
    expect(causesForMarker({ ...base, marker: "resumed", gap: true })).toEqual({ path: "stream-gap", causes: ["stream-gap"] });
    expect(causesForMarker({ ...base, marker: "resumed", termVirgin: true })).toEqual({ path: "cold-restore", causes: ["cold-restore"] });
    expect(causesForMarker({ ...base, marker: "resumed", firstMarker: false, epochChanged: true }))
      .toEqual({ path: "epoch-reset", causes: ["epoch-reset"] });
    expect(causesForMarker({ ...base, marker: "reset" })).toEqual({ path: "baseline", causes: [] });
  });

  it("бедное зеркало и новые строки: тёплое переподключение даёт ровно один запрос hist-probe, кадр снимает причину", () => {
    const sim = new Sim(live());
    serve(sim);
    sim.at(5_000, (s) => offline(s));
    sim.at(6_000, warmReconnect(true));
    sim.run(30_000);
    expect(sim.sends).toHaveLength(1);
    expect(sim.sends[0].causes).toEqual(["hist-probe"]);
    expect(sim.sends[0].at).toBe(6_000 + SETTLE_MS);
    expect(sim.s.phase).toBe("idle");
    expect(sim.s.causes).toEqual([]);
    expect(sim.overlaps).toBe(0);
  });

  it("богатое зеркало или поток без новых строк: тёплое переподключение кадр не просит (ST-09 A6)", () => {
    const sim = new Sim(live());
    serve(sim);
    sim.at(5_000, (s) => offline(s));
    sim.at(6_000, warmReconnect(false));
    sim.run(30_000);
    expect(sim.sends).toHaveLength(0);
    expect(nextWakeAt(sim.s)).toBeNull();
  });

  it("hist-probe у молчащего агента ограничен общим пределом: MAX_NO_PROGRESS попыток, отката нет", () => {
    const sim = new Sim(live());
    sim.at(0, warmReconnect(true));
    sim.run(120_000);
    expect(sim.sends).toHaveLength(MAX_NO_PROGRESS);
    expect(sim.s.phase).toBe("degraded");
    expect(sim.fallbacks).toBe(0);
  });

  it("causeMask: hist-probe в конце порядка — биты прежних причин в трассе не сдвинулись", () => {
    expect(causeMask(["open"])).toBe(1);
    expect(causeMask(["geometry-width"])).toBe(1 << 9);
    expect(causeMask(["hist-probe"])).toBe(1 << 10);
    expect(causesFromMask(causeMask(["hist-probe", "open"]))).toEqual(["open", "hist-probe"]);
  });
});

describe("ревью 15.09, дефект 2: запрос кадра ждёт первый размер сокета (ST-05 × ST-08)", () => {
  /** Задержанный WebGL подключился: первый размер сокета ушёл (rendererSettled → sendResize(true)). */
  const RENDERER_AT = 1_000;
  /**
   * Новый сокет, как в адаптере: connect() → offline(connection()); onopen →
   * (размер отложен до рендера → holdForResize) → online(); маркер reset → open.
   * Агент снимает кадр в сетке PTY на момент запроса: до нашего размера —
   * прежняя сетка, адаптер отвергает кадр чужой ширины (geometry-width).
   */
  function openSocket(hold: boolean): Sim {
    let s = offline(connection(createRecoveryState({ geomRev: 7 }), 1).state).state;
    if (hold) s = holdForResize(s, 0).state;
    s = online(s, 0).state;
    const sim = new Sim(s);
    serve(sim, { raise: (n) => (sim.sends[n - 1].at < RENDERER_AT ? "geometry-width" : undefined) });
    sim.at(20, (st, now) => demand(epoch(st, E1, now).state, "open", now));
    sim.at(RENDERER_AT, (st) => resizeSent(st));
    sim.run(30_000);
    return sim;
  }

  it("без удержания (до исправления): запрос раньше размера, кадр в прежней сетке, повтор — больше одного запроса", () => {
    const sim = openSocket(false);
    expect(sim.sends[0].at).toBe(20 + SETTLE_MS);
    expect(sim.sends[0].at).toBeLessThan(RENDERER_AT);
    expect(sim.sends.length).toBeGreaterThanOrEqual(2);
  });

  it("с удержанием: ровно один запрос, сразу после размера, кадр в своей сетке", () => {
    const sim = openSocket(true);
    expect(sim.sends).toHaveLength(1);
    expect(sim.sends[0].at).toBe(RENDERER_AT);
    expect(sim.sends[0].causes).toEqual(["open"]);
    expect(sim.s.phase).toBe("idle");
    expect(sim.s.resizeHoldUntil).toBeNull();
    expect(sim.overlaps).toBe(0);
  });

  it("будильник не раньше потолка; без resizeSent запрос уходит по RESIZE_HOLD_MAX_MS; отпущенный — сразу", () => {
    let s = holdForResize(offline(connection(createRecoveryState({ geomRev: 7 }), 1).state).state, 0).state;
    s = online(s, 0).state;
    s = demand(epoch(s, E1, 20).state, "open", 20).state;
    expect(nextWakeAt(s)).toBe(RESIZE_HOLD_MAX_MS);
    expect(wake(s, 20 + SETTLE_MS).send).toBeUndefined();
    expect(wake(s, RESIZE_HOLD_MAX_MS).send?.causes).toEqual(["open"]);
    const released = resizeSent(s).state;
    expect(nextWakeAt(released)).toBe(20 + SETTLE_MS);
    expect(resizeSent(released).state).toBe(released);
  });

  it("удержание принадлежит сокету: новое соединение и обрыв его снимают, причины при нём копятся", () => {
    const held = holdForResize(live(), 0).state;
    expect(held.resizeHoldUntil).toBe(RESIZE_HOLD_MAX_MS);
    expect(connection(held, 2).state.resizeHoldUntil).toBeNull();
    expect(offline(held).state.resizeHoldUntil).toBeNull();
    const waiting = demand(held, "queue-gap", 10).state;
    expect(waiting.causes).toEqual(["queue-gap"]);
    expect(nextWakeAt(waiting)).toBe(RESIZE_HOLD_MAX_MS);
  });
});

describe("ревью 15.09, повторная проверка: кадр, сам сменивший сетку, не отменяет свою операцию (adopt/restore)", () => {
  /**
   * Открытие, как на проводе у скептика: запрос open → кадр 47x23 при сетке
   * xterm 80x24 → адаптер принимает сетку PTY (snapshot-adopt, live.resize) →
   * onResize синхронно зовёт syncGeometry, пока операция в validating → запись
   * кадра (applying) → applied. keep — адаптер помечает resize как «сетку сменил
   * этот кадр» (adopted перед geometry), как после исправления.
   */
  function openWithAdopt(keep: boolean): Sim {
    const s = online(offline(connection(createRecoveryState({ geomRev: 4 }), 1).state).state, 0).state;
    const sim = new Sim(s);
    let frames = 0;
    sim.onSend = (send) => {
      sim.at(send.at + 1, (st, now) => {
        const f = frame(st, { conn: st.conn, epoch: st.epoch, geomRev: send.geomRev, req: send.id, base: 1_000 },
          APPLIED, now, sim.random);
        sim.verdicts.push(f.verdict);
        if (f.verdict !== "apply" || f.ticket === undefined) return f;
        let cur = f.state;
        if (++frames === 1) {
          const rev = cur.geomRev + 1; // noteTerminalGeometry: 80x24 → 47x23
          if (keep) cur = adopted(cur, f.ticket, rev).state;
          cur = geometry(cur, rev, now).state;
        }
        cur = applying(cur, f.ticket).state;
        return applied(cur, f.ticket, true, now, sim.random);
      });
    };
    sim.at(20, (st, now) => demand(epoch(st, E1, now).state, "open", now));
    sim.run(30_000);
    return sim;
  }

  it("до исправления: приём сетки своим кадром отменяет его операцию — второй запрос geometry-stale через SETTLE_MS", () => {
    const sim = openWithAdopt(false);
    expect(sim.sends).toHaveLength(2);
    expect(sim.sends[1].causes).toContain("geometry-stale");
    expect(sim.sends[1].at).toBe(sim.sends[0].at + 1 + SETTLE_MS);
    expect(sim.sends[1].geomRev).toBe(5);
  });

  it("с adopted: ровно один запрос, кадр дописан, автомат idle в новой ревизии", () => {
    const sim = openWithAdopt(true);
    expect(sim.sends).toHaveLength(1);
    expect(sim.sends[0].causes).toEqual(["open"]);
    expect(sim.verdicts).toEqual(["apply"]);
    expect(sim.s.phase).toBe("idle");
    expect(sim.s.causes).toEqual([]);
    expect(sim.s.geomRev).toBe(5);
    expect(sim.overlaps).toBe(0);
  });

  it("adopted: только кадр своей операции в validating/applying; настоящая смена сетки после него по-прежнему отменяет", () => {
    let s = due(demand(live(), "open", 0).state, SETTLE_MS).state;
    expect(s.phase).toBe("in-flight");
    // В пути (кадра ещё нет), idle, чужой номер, та же ревизия — ничего.
    expect(adopted(s, 1, 8).state).toBe(s);
    expect(adopted(live(), 1, 8).state.geomRev).toBe(7);
    const f = frame(s, { conn: 1, epoch: E1, geomRev: 7, req: 1, base: 1_000 }, APPLIED, SETTLE_MS + 1);
    expect(f.verdict).toBe("apply");
    s = f.state;
    expect(adopted(s, 2, 8).state).toBe(s);
    expect(adopted(s, 1, 7).state).toBe(s);
    const kept = adopted(s, 1, 8).state;
    expect(kept.phase).toBe("validating");
    expect(kept.pending?.id).toBe(1);
    expect(kept.geomRev).toBe(8);
    // syncGeometry после resize: ревизия уже учтена — не invalidated.
    expect(geometry(kept, 8, SETTLE_MS + 2).effects).toEqual([]);
    // restore/adopt в applying — то же.
    expect(adopted(applying(s, 1).state, 1, 8).state.phase).toBe("applying");
    // Следующая, настоящая смена сетки (поворот, клавиатура) отменяет как раньше.
    const moved = geometry(kept, 9, SETTLE_MS + 3);
    expect(moved.state.phase).toBe("scheduled");
    expect(moved.state.causes).toContain("geometry-stale");
    expect(moved.effects.some((e) => e.kind === "diag" && e.what === "invalidated")).toBe(true);
  });
});

describe("ревью 15.09, повторная проверка: hist-probe после маркера (дельта обрыва и живой поток)", () => {
  it("операция уже поставлена или в пути: hist-probe её не дублирует — любой кадр несёт hist_lines", () => {
    const scheduled = demand(live(), "queue-gap", 0).state;
    expect(demand(scheduled, "hist-probe", 100).state).toBe(scheduled);
    const inFlight = due(scheduled, SETTLE_MS).state;
    expect(inFlight.phase).toBe("in-flight");
    const after = demand(inFlight, "hist-probe", SETTLE_MS + 10).state;
    expect(after).toBe(inFlight);
    expect(hasNewerDemand(after)).toBe(false);
    // Без соединения копится, как любая причина (online поставит её в отправку).
    const off = offline(live()).state;
    expect(demand(off, "hist-probe", 0).state.causes).toEqual(["hist-probe"]);
  });

  it("маркер тёплый без кадра, первая строка дельты просит hist-probe: ровно один запрос", () => {
    const sim = new Sim(live());
    serve(sim);
    sim.at(5_000, (s) => offline(s));
    sim.at(6_000, (s, now) => {
      let st = online(offline(connection(s, s.conn + 1).state).state, now).state;
      st = epoch(st, `stream:e1:${st.conn}`, now).state;
      const route = causesForMarker({ marker: "resumed", gap: false, epochChanged: false, termVirgin: false, firstMarker: true, histProbe: false });
      expect(route.path).toBe("warm");
      return { state: st, effects: [] };
    });
    // Адаптер: noteStreamGrowth на первой строке дельты, один раз за соединение.
    sim.at(6_040, (s, now) => demand(s, "hist-probe", now));
    sim.run(30_000);
    expect(sim.sends).toHaveLength(1);
    expect(sim.sends[0].causes).toEqual(["hist-probe"]);
    expect(sim.sends[0].at).toBe(6_040 + SETTLE_MS);
    expect(sim.s.phase).toBe("idle");
    expect(sim.overlaps).toBe(0);
  });

  it("тёплое переподключение с долгом (owed): дельта не добавляет второго запроса", () => {
    const sim = new Sim(live());
    serve(sim, { delay: 300 });
    sim.at(0, (s, now) => demand(s, "queue-gap", now));
    sim.at(SETTLE_MS + 100, (s) => offline(s)); // обрыв, пока запрос в пути: queue-gap в owed
    sim.at(2_000, (s, now) => {
      let st = online(offline(connection(s, s.conn + 1).state).state, now).state;
      st = epoch(st, `stream:e1:${st.conn}`, now).state;
      return { state: st, effects: [] };
    });
    sim.at(2_000 + SETTLE_MS + 50, (s, now) => demand(s, "hist-probe", now)); // строка дельты, запрос долга в пути
    sim.run(30_000);
    const second = sim.sends.filter((x) => x.at >= 2_000);
    expect(second).toHaveLength(1);
    expect(second[0].causes).toEqual(["queue-gap"]);
    expect(sim.s.phase).toBe("idle");
  });
});
