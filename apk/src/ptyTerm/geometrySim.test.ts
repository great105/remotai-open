/**
 * SIM-TWO-VIEWERS (ST-08, T-32, I-10) — L1-модель двух зрителей одного PTY.
 *
 * Зачем модель, а не проба в браузере. Дефект «вечный цикл кадр → resync →
 * запрос» (журнал 2026-09-03, п. 4; карта ST-08) живёт на стыке трёх вещей:
 * серверного минимума по зрителям с отложенным ростом, порядка сообщений в
 * сокете и REST-опроса /state, которого нет в скрытой вкладке. Живьём его
 * воспроизвести трудно, а доказать «сходится всегда» пробой нельзя вовсе.
 * Здесь всё это собрано в детерминированную дискретно-событийную модель с
 * seed, и по ней видно: на СТАРОМ правиле (capacityDelivered = false) круг в
 * скрытой вкладке не сходится, на новом — сходится за один запрос.
 *
 * Что взято из кода как есть (правила клиента — настоящие функции):
 *   • frameGeometryAction, logicalRowsForKeyboard — geometry.ts;
 *   • resizeDelayMs, shouldFlushResize, capacityDelivered, capacityForServer —
 *     resizePolicy.ts.
 * Что смоделировано по коду (номера строк — на 13.09.2026):
 *   сервер (manager.go):
 *     • PTY = min по зрителям с размером, пол 20×10 (targetSizeLocked 2553-2621);
 *     • живой зритель — сразу в обе стороны (ResizeFor 2471-2496);
 *     • уход зрителя — рост через 4 с, отмена, если размер уже верный
 *       (RemoveViewer 2443-2458, resizeToViewers 2659-2697, applyGrow 2632-2650);
 *   соединение (api_pty.go):
 *     • resize и screen одного сокета обрабатываются по порядку (1377-1548);
 *     • кадр снимается в текущей сетке PTY, не чаще раза в секунду, частая
 *       просьба досчитывается, а не выбрасывается (1929-1942);
 *     • кадр несёт geom_rev своего запроса;
 *   клиент (PtyTermView.tsx) — как связывает правила сейчас и как свяжет
 *   оркестратор по карте ST-08 (шаги 1-2):
 *     • fitLocal 1222-1257, sizeForServer 1268-1274, flushResize 1275-1291,
 *       sendResize с force 1292-1323;
 *     • requestScreenFrame: таймер 500 мс, geom_rev на момент отправки
 *       (1846-1861); новое — запомнить { geomRev, delivered } запроса;
 *     • кадр чужой ревизии — один повтор на ревизию (2323-2340);
 *     • ветки adopt/restore/resync (2437-2546), троттл ширины 5 с;
 *       новое — при adopt выставить и авторитетную высоту;
 *     • эффекты /state (3410-3417 и соседний по cols): только при ИЗМЕНЕНИИ
 *       значения, fitLocal; опрос раз в 2 с и только в видимой вкладке;
 *     • новое по шагу 2: вместимость — только измеренная без клавиатуры
 *       (capacityForServer), отправка привязана к сокету, force-путь на новом
 *       сокете под клавиатурой шлёт её, если она измерена без клавиатуры.
 * Чего нет в модели: байтов потока (TUI после SIGWINCH), клавиатурного peek,
 * lease владельца размера, ACK/NACK (кадр ждёт геометрию — в модели resize
 * применяется мгновенно, что эквивалентно для порядка кадров).
 *
 * Проверки (правило 8.2 плана — числами):
 *   • ≤ 2 запроса кадра на событие у каждого зрителя;
 *   • 0 запросов, 0 resize и 0 смен PTY в покое;
 *   • ≤ 1 смены PTY на событие, 0 resize на клавиатурных событиях (T-31);
 *   • сетка клиента = PTY после события — у видимых и у тех скрытых, кто
 *     получил кадр после последней смены PTY;
 *   • всё, что клиент выдаёт серверу за вместимость, равно его вместимости по
 *     ОБЕИМ осям (I-10), и ни один отправленный resize не был чем-то другим.
 */
import { describe, expect, it } from "vitest";
import { frameGeometryAction, logicalRowsForKeyboard } from "./geometry";
import {
  RESIZE_QUIET_MS, capacityDelivered, capacityForServer, resizeDelayMs, shouldFlushResize, type TermSize,
} from "./resizePolicy";

type Rule = "old" | "new";
const same = (a: TermSize | null | undefined, b: TermSize | null | undefined) =>
  !!a && !!b && a.cols === b.cols && a.rows === b.rows;
const fmt = (s: TermSize | null | undefined) => (s ? `${s.cols}x${s.rows}` : "—");

// Константы из кода (см. шапку).
const SCREEN_REQUEST_DELAY_MS = 500;
const SCREEN_FRAME_MIN_GAP_MS = 1000;
const GEOM_RESYNC_GAP_MS = 5000;
const STATE_POLL_MS = 2000;
const VIEWER_GROW_QUIET_MS = 4000;
const MIN_PTY = { cols: 20, rows: 10 };

function mulberry32(seed: number) {
  let a = seed >>> 0;
  return () => {
    a = (a + 0x6d2b79f5) >>> 0;
    let t = a;
    t = Math.imul(t ^ (t >>> 15), t | 1);
    t ^= t + Math.imul(t ^ (t >>> 7), t | 61);
    return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
  };
}

interface Handle { cancelled: boolean }

/** Дискретные события по времени; при равном времени — порядок постановки. */
class Clock {
  now = 0;
  private queue: { at: number; seq: number; fn: () => void; h: Handle }[] = [];
  private seq = 0;
  constructor(readonly random: () => number) {}
  atTime(at: number, fn: () => void): Handle {
    const h = { cancelled: false };
    this.queue.push({ at: Math.max(at, this.now), seq: this.seq++, fn, h });
    return h;
  }
  after(ms: number, fn: () => void): Handle { return this.atTime(this.now + Math.max(0, ms), fn); }
  /** Сетевая задержка в одну сторону: 15–200 мс. */
  latency(): number { return 15 + this.random() * 185; }
  run(until: number): void {
    for (;;) {
      let best = -1;
      for (let i = 0; i < this.queue.length; i++) {
        const e = this.queue[i];
        if (e.at > until) continue;
        if (best < 0 || e.at < this.queue[best].at || (e.at === this.queue[best].at && e.seq < this.queue[best].seq)) best = i;
      }
      if (best < 0) break;
      const [e] = this.queue.splice(best, 1);
      this.now = e.at;
      if (!e.h.cancelled) e.fn();
    }
    this.now = until;
  }
}

/** Одно направление сокета: FIFO, как TCP. */
class Pipe {
  private last = 0;
  constructor(private clock: Clock) {}
  send(fn: () => void): void {
    const at = Math.max(this.last, this.clock.now + this.clock.latency());
    this.last = at;
    this.clock.atTime(at, fn);
  }
}

interface Frame { cols: number; rows: number; geomRev: number }

class Socket {
  open = true;
  readonly up: Pipe;
  readonly down: Pipe;
  constructor(clock: Clock, readonly client: Client) {
    this.up = new Pipe(clock);
    this.down = new Pipe(clock);
  }
  // Серверная сторона соединения.
  lastFrameAt = -Infinity;
  pendingRev: number | null = null;
  frameTimer: Handle | null = null;
}

class Server {
  pty: TermSize;
  viewers = new Map<Socket, TermSize>();
  private grow: Handle | null = null;
  ptyResizes = 0;
  changedAt = 0;
  constructor(private clock: Clock, born: TermSize) { this.pty = { ...born }; }

  private target(): TermSize | null {
    let cols = 0, rows = 0;
    for (const v of this.viewers.values()) {
      if (v.cols <= 0 || v.rows <= 0) continue; // зритель ещё не сообщил размер
      cols = cols === 0 ? v.cols : Math.min(cols, v.cols);
      rows = rows === 0 ? v.rows : Math.min(rows, v.rows);
    }
    // Никто не смотрит — агентская сессия держит прежний размер.
    if (cols <= 0 || rows <= 0) return null;
    return { cols: Math.max(MIN_PTY.cols, cols), rows: Math.max(MIN_PTY.rows, rows) };
  }
  private cancelGrow(): void { if (this.grow) { this.grow.cancelled = true; this.grow = null; } }
  private apply(t: TermSize): void { this.pty = { ...t }; this.ptyResizes++; this.changedAt = this.clock.now; }
  private resizeToViewers(deferGrow: boolean): void {
    const t = this.target();
    if (!t || same(t, this.pty)) { this.cancelGrow(); return; }
    if (deferGrow) {
      if (!this.grow) this.grow = this.clock.after(VIEWER_GROW_QUIET_MS, () => {
        this.grow = null;
        const now = this.target();
        if (now && !same(now, this.pty)) this.apply(now);
      });
      return;
    }
    this.cancelGrow();
    this.apply(t);
  }
  addViewer(s: Socket): void { this.viewers.set(s, { cols: 0, rows: 0 }); }
  removeViewer(s: Socket): void {
    if (!this.viewers.delete(s)) return;
    if (s.frameTimer) { s.frameTimer.cancelled = true; s.frameTimer = null; }
    this.resizeToViewers(true);
  }
  resizeFor(s: Socket, size: TermSize): void {
    if (!this.viewers.has(s)) return;
    this.viewers.set(s, { ...size });
    this.resizeToViewers(false);
  }
  screen(s: Socket, geomRev: number): void {
    if (!this.viewers.has(s)) return;
    const wait = s.lastFrameAt + SCREEN_FRAME_MIN_GAP_MS - this.clock.now;
    if (wait > 0) {
      s.pendingRev = geomRev;
      if (!s.frameTimer) s.frameTimer = this.clock.after(wait, () => {
        s.frameTimer = null;
        const rev = s.pendingRev;
        s.pendingRev = null;
        if (rev !== null && this.viewers.has(s)) this.emit(s, rev);
      });
      return;
    }
    this.emit(s, geomRev);
  }
  /** Кадр снимается СЕЙЧАС — после всего, что этот сокет прислал раньше. */
  private emit(s: Socket, geomRev: number): void {
    s.lastFrameAt = this.clock.now;
    const frame: Frame = { cols: this.pty.cols, rows: this.pty.rows, geomRev };
    s.down.send(() => s.client.onFrame(s, frame));
  }
  state(): TermSize { return { ...this.pty }; }
}

/** Строк видно под клавиатурой (fit в сжатой коробке). */
const KEYBOARD_ROWS = 8;

class Client {
  keyboard = false;
  hidden = false;
  connected = false;
  socket: Socket | null = null;
  term: TermSize;
  report: TermSize = { cols: 0, rows: 0 };
  measuredWithoutKeyboard = false;
  sent: TermSize | null = null;
  sentOn: Socket | null = null;
  resizeTimer: Handle | null = null;
  authCols = 0;
  authRows = 0;
  logicalRows = 0;
  stateCols = 0;
  stateRows = 0;
  geomRev = 0;
  retryRev: number | null = null;
  lastRequest: { geomRev: number; delivered: boolean } | null = null;
  lastResyncAt = -Infinity;
  lastFrameAt = -Infinity;
  // Счётчики.
  frameRequests = 0;
  resizesSent = 0;
  violations: string[] = [];

  constructor(private w: World, readonly name: string, public cap: TermSize) {
    this.term = { ...cap };
  }

  private get rule(): Rule { return this.w.rule; }

  // ── fitLocal ────────────────────────────────────────────────────────────
  private noteReport(size: TermSize, withoutKeyboard: boolean): void {
    const prev = this.report;
    if (prev.cols > 0 && (prev.cols !== size.cols || prev.rows !== size.rows)) {
      this.geomRev++;
      this.retryRev = null;
    }
    this.report = { ...size };
    this.measuredWithoutKeyboard = withoutKeyboard;
  }
  fitLocal(): void {
    if (!this.keyboard) {
      this.noteReport(this.cap, true);
      this.term = {
        cols: this.authCols >= 2 ? this.authCols : this.cap.cols,
        rows: this.authRows >= 2 ? this.authRows : this.cap.rows,
      };
      return;
    }
    const measuredCols = this.cap.cols;
    const rows = logicalRowsForKeyboard(KEYBOARD_ROWS, this.logicalRows);
    this.term = { cols: this.authCols >= 2 ? this.authCols : measuredCols, rows };
    if (this.rule === "old") {
      // Как сейчас (PtyTermView.tsx:1254): при пустом отчёте в него пишется
      // логическая/клавиатурная высота — дыра (а) карты ST-08.
      this.noteReport({ cols: measuredCols, rows: this.report.rows >= 2 ? this.report.rows : rows }, this.measuredWithoutKeyboard);
    } else {
      // Шаг 2: строки, измеренные без клавиатуры, сохраняются; иначе неизвестны.
      const keep = this.measuredWithoutKeyboard && this.report.rows >= 2;
      this.noteReport({ cols: measuredCols, rows: keep ? this.report.rows : 0 }, keep);
    }
  }

  // ── размер для сервера ──────────────────────────────────────────────────
  /** То, что клиент выдаст серверу как вместимость (I-10 проверяется по нему). */
  claimedCapacity(): TermSize | null {
    if (this.rule === "old") {
      // sizeForServer (1268-1274): при пустом отчёте — сетка терминала.
      return this.report.cols >= 2 && this.report.rows >= 2 ? this.report : { ...this.term };
    }
    return capacityForServer({
      report: this.report, measuredWithoutKeyboard: this.measuredWithoutKeyboard,
      termCols: this.term.cols, termRows: this.term.rows,
    });
  }
  private sentBaseline(): TermSize {
    if (this.rule === "new" && this.sentOn !== this.socket) return { cols: 0, rows: 0 };
    return this.sent ?? { cols: 0, rows: 0 };
  }
  private transmit(size: TermSize): void {
    const s = this.socket;
    if (!s || !s.open) return;
    if (!same(size, this.cap)) {
      this.violations.push(`${this.name}: t=${Math.round(this.w.clock.now)} отправлен resize ${fmt(size)} при вместимости ${fmt(this.cap)}`);
    }
    this.sent = { ...size };
    this.sentOn = s;
    this.resizesSent++;
    s.up.send(() => this.w.server.resizeFor(s, size));
  }
  private flushResize(): void {
    this.resizeTimer = null;
    const size = this.claimedCapacity();
    if (!size) return;
    if (this.hidden) return; // flushResize: document.visibilityState === "hidden"
    if (!shouldFlushResize(this.sentBaseline(), size, this.keyboard)) return;
    this.transmit(size);
  }
  sendResize(force = false): void {
    if (force) {
      // Старое: под клавиатурой force молчит всегда. Новое (шаг 2): молчит,
      // только если вместимость не измерена без клавиатуры.
      if (this.rule === "old" && this.keyboard) return;
      if (this.resizeTimer) { this.resizeTimer.cancelled = true; this.resizeTimer = null; }
      const size = this.claimedCapacity();
      if (!size || size.cols < 2 || size.rows < 2) return;
      this.transmit(size);
      return;
    }
    if (this.resizeTimer) this.resizeTimer.cancelled = true;
    const size = this.claimedCapacity() ?? { cols: 0, rows: 0 };
    const wait = size.cols >= 2 && size.rows >= 2 ? resizeDelayMs(this.sentBaseline(), size) : RESIZE_QUIET_MS;
    this.resizeTimer = this.w.clock.after(wait, () => this.flushResize());
  }

  // ── кадры ───────────────────────────────────────────────────────────────
  requestScreenFrame(sock: Socket | null = this.socket): void {
    const target = sock;
    if (!target) return;
    this.w.clock.after(SCREEN_REQUEST_DELAY_MS, () => {
      if (this.socket !== target || !target.open) return;
      const delivered = this.rule === "new" && capacityDelivered({
        sent: this.sent, report: this.report, measuredWithoutKeyboard: this.measuredWithoutKeyboard,
        resizePending: this.resizeTimer !== null, sentOnSocket: this.sentOn, socket: target,
      });
      const geomRev = this.geomRev;
      this.lastRequest = { geomRev, delivered };
      this.frameRequests++;
      target.up.send(() => this.w.server.screen(target, geomRev));
    });
  }
  onFrame(sock: Socket, frame: Frame): void {
    if (sock !== this.socket || !sock.open) return;
    this.lastFrameAt = this.w.clock.now;
    if (frame.geomRev !== this.geomRev) {
      if (this.retryRev !== this.geomRev) {
        this.retryRev = this.geomRev;
        this.requestScreenFrame(sock);
      }
      return;
    }
    this.logicalRows = frame.rows; // PtyTermView.tsx:2434-2436
    const delivered = this.rule === "new" && this.lastRequest?.geomRev === frame.geomRev && this.lastRequest.delivered;
    const geo = frameGeometryAction({
      snapCols: frame.cols, snapRows: frame.rows, termCols: this.term.cols, termRows: this.term.rows,
      keyboardOpen: this.keyboard, authoritativeCols: this.authCols, authoritativeRows: this.authRows,
      capacityDelivered: delivered,
    });
    switch (geo.kind) {
      case "adopt":
        this.authCols = geo.cols;
        if (this.rule === "new") this.authRows = geo.rows; // шаг 1: иначе fitLocal вернёт старую высоту
        this.term = { cols: geo.cols, rows: geo.rows };
        return;
      case "restore":
        this.term = { cols: geo.cols, rows: geo.rows };
        return;
      case "resync":
        if (!geo.apply) {
          this.sendResize();
          if (this.w.clock.now - this.lastResyncAt > GEOM_RESYNC_GAP_MS) {
            this.lastResyncAt = this.w.clock.now;
            this.requestScreenFrame();
          }
        } else {
          if (!this.resizeTimer) this.sendResize();
          this.requestScreenFrame();
        }
        return;
      case "apply":
        return;
    }
  }

  // ── /state ──────────────────────────────────────────────────────────────
  private schedulePoll(delay: number): void {
    this.w.clock.after(delay, () => {
      if (!this.connected) return;
      if (!this.hidden) {
        this.w.clock.after(this.w.clock.latency(), () => {
          const st = this.w.server.state();
          this.w.clock.after(this.w.clock.latency(), () => { if (this.connected) this.onState(st); });
        });
      }
      this.schedulePoll(STATE_POLL_MS);
    });
  }
  private onState(st: TermSize): void {
    if (st.rows >= 2 && st.rows !== this.stateRows) {
      this.stateRows = st.rows;
      this.logicalRows = st.rows;
      this.authRows = st.rows;
      this.fitLocal();
    }
    if (st.cols >= 2 && st.cols !== this.stateCols) {
      this.stateCols = st.cols;
      this.authCols = st.cols;
      this.fitLocal();
    }
  }

  // ── события сценария ────────────────────────────────────────────────────
  connect(): void {
    if (this.socket) this.socket.open = false;
    const s = new Socket(this.w.clock, this);
    this.socket = s;
    s.up.send(() => this.w.server.addViewer(s));
    if (!this.connected) {
      this.connected = true;
      this.schedulePoll(this.w.clock.random() * STATE_POLL_MS);
    }
    this.fitLocal();
    this.sendResize(true);
    this.requestScreenFrame(s);
  }
  reconnect(): void {
    const old = this.socket;
    if (old) {
      old.open = false;
      old.up.send(() => this.w.server.removeViewer(old));
    }
    this.connect();
  }
  leave(): void {
    const s = this.socket;
    this.connected = false;
    if (!s) return;
    s.open = false;
    s.up.send(() => this.w.server.removeViewer(s));
  }
  rotate(cap: TermSize): void { this.cap = { ...cap }; this.fitLocal(); this.sendResize(); }
  keyboardOpen(): void { this.keyboard = true; this.fitLocal(); }
  keyboardClose(): void { this.keyboard = false; this.fitLocal(); this.sendResize(); }
  hide(): void { this.hidden = true; }
  /** Возврат из фона: будильник просит кадр, опрос /state снова идёт. */
  show(): void { this.hidden = false; this.requestScreenFrame(); }
}

class World {
  readonly clock: Clock;
  readonly server: Server;
  readonly clients: Client[] = [];
  constructor(seed: number, readonly rule: Rule, born: TermSize = { cols: 80, rows: 24 }) {
    this.clock = new Clock(mulberry32(seed));
    this.server = new Server(this.clock, born);
  }
  /** Каждый join — новая страница: состояние клиента с нуля. */
  join(name: string, cap: TermSize, opts: { keyboard?: boolean; hidden?: boolean } = {}): Client {
    const c = new Client(this, name, cap);
    c.keyboard = !!opts.keyboard;
    c.hidden = !!opts.hidden;
    this.clients.push(c);
    c.connect();
    return c;
  }
  live(name: string): Client {
    const c = [...this.clients].reverse().find(x => x.name === name && x.connected);
    if (!c) throw new Error(`нет живого зрителя ${name}`);
    return c;
  }
}

interface WindowReport {
  label: string;
  requests: Record<string, number>;
  resizes: Record<string, number>;
  ptyResizes: number;
  gridMismatch: string[];
  i10: string[];
}

type Step = { at: number; label: string; act: (w: World) => void };

function counters(w: World) {
  const req = new Map<Client, number>(), res = new Map<Client, number>(), vio = new Map<Client, number>();
  for (const c of w.clients) { req.set(c, c.frameRequests); res.set(c, c.resizesSent); vio.set(c, c.violations.length); }
  return { req, res, vio, pty: w.server.ptyResizes };
}

function windowReport(w: World, label: string, before: ReturnType<typeof counters>): WindowReport {
  const requests: Record<string, number> = {}, resizes: Record<string, number> = {};
  const gridMismatch: string[] = [], i10: string[] = [];
  for (const c of w.clients) {
    const key = c.name;
    requests[key] = (requests[key] ?? 0) + c.frameRequests - (before.req.get(c) ?? 0);
    resizes[key] = (resizes[key] ?? 0) + c.resizesSent - (before.res.get(c) ?? 0);
    i10.push(...c.violations.slice(before.vio.get(c) ?? 0));
    if (!c.connected) continue;
    const claimed = c.claimedCapacity();
    if (claimed && !same(claimed, c.cap)) i10.push(`${c.name}: за вместимость выдаётся ${fmt(claimed)} при ${fmt(c.cap)}`);
    const sawGrid = !c.hidden || c.lastFrameAt > w.server.changedAt;
    if (sawGrid && !same(c.term, w.server.pty)) gridMismatch.push(`${c.name}: сетка ${fmt(c.term)} при PTY ${fmt(w.server.pty)}`);
  }
  return { label, requests, resizes, ptyResizes: w.server.ptyResizes - before.pty, gridMismatch, i10 };
}

function runScript(seed: number, rule: Rule, steps: Step[], windowMs = 10_000, restMs = 30_000) {
  const w = new World(seed, rule);
  const windows: WindowReport[] = [];
  for (let i = 0; i < steps.length; i++) {
    w.clock.run(steps[i].at);
    const before = counters(w);
    steps[i].act(w);
    w.clock.run(i + 1 < steps.length ? steps[i + 1].at : steps[i].at + windowMs);
    windows.push(windowReport(w, steps[i].label, before));
  }
  const before = counters(w);
  w.clock.run(w.clock.now + restMs);
  return { w, windows, rest: windowReport(w, "покой", before) };
}

// Телефон 48×20 (в ландшафте 90×10), компьютер 120×40: разные ширина И высота.
const PHONE = { cols: 48, rows: 20 };
const PHONE_LANDSCAPE = { cols: 90, rows: 10 };
const DESKTOP = { cols: 120, rows: 40 };

const JOURNEY: Step[] = [
  { at: 0, label: "компьютер открыл терминал", act: w => { w.join("desk", DESKTOP); } },
  { at: 10_000, label: "телефон присоединился", act: w => { w.join("phone", PHONE); } },
  { at: 20_000, label: "клавиатура на телефоне поднята", act: w => w.live("phone").keyboardOpen() },
  { at: 30_000, label: "клавиатура на телефоне убрана", act: w => w.live("phone").keyboardClose() },
  { at: 40_000, label: "телефон повернули в ландшафт", act: w => w.live("phone").rotate(PHONE_LANDSCAPE) },
  { at: 50_000, label: "телефон вернули в портрет", act: w => w.live("phone").rotate(PHONE) },
  { at: 60_000, label: "телефон ушёл (рост через 4 с)", act: w => w.live("phone").leave() },
  { at: 70_000, label: "вкладка компьютера скрыта", act: w => w.live("desk").hide() },
  { at: 80_000, label: "телефон вернулся, компьютер скрыт", act: w => { w.join("phone", PHONE); } },
  { at: 90_000, label: "скрытый компьютер переподключился", act: w => w.live("desk").reconnect() },
  { at: 100_000, label: "вкладка компьютера снова видна", act: w => w.live("desk").show() },
  { at: 110_000, label: "телефон ушёл снова", act: w => w.live("phone").leave() },
];

function expectConverges(seed: number, windows: WindowReport[], rest: WindowReport) {
  for (const win of windows) {
    const where = `seed ${seed}, «${win.label}»`;
    for (const [name, n] of Object.entries(win.requests)) expect(n, `${where}: запросов кадра у ${name}`).toBeLessThanOrEqual(2);
    expect(win.ptyResizes, `${where}: смен PTY`).toBeLessThanOrEqual(1);
    expect(win.gridMismatch, where).toEqual([]);
    expect(win.i10, where).toEqual([]);
    if (win.label.includes("клавиатура")) {
      for (const [name, n] of Object.entries(win.resizes)) expect(n, `${where}: resize у ${name}`).toBe(0);
      expect(win.ptyResizes, `${where}: смен PTY`).toBe(0);
    }
  }
  for (const [name, n] of Object.entries(rest.requests)) expect(n, `seed ${seed}, покой: запросов у ${name}`).toBe(0);
  for (const [name, n] of Object.entries(rest.resizes)) expect(n, `seed ${seed}, покой: resize у ${name}`).toBe(0);
  expect(rest.ptyResizes, `seed ${seed}, покой: смен PTY`).toBe(0);
  expect(rest.gridMismatch, `seed ${seed}, покой`).toEqual([]);
  expect(rest.i10, `seed ${seed}, покой`).toEqual([]);
}

const SEEDS = Array.from({ length: 25 }, (_, i) => 1000 + i * 7919);

describe("SIM-TWO-VIEWERS: телефон и компьютер, разные ширина и высота", () => {
  it("новое правило: join/leave/поворот/клавиатура/скрытая вкладка сходятся на всех seed", () => {
    for (const seed of SEEDS) {
      const { windows, rest } = runScript(seed, "new", JOURNEY);
      expectConverges(seed, windows, rest);
    }
  });

  it("числа сценария для отчёта: запросы кадра и смены PTY по событиям (seed 1000)", () => {
    const { windows } = runScript(1000, "new", JOURNEY);
    const totalRequests = windows.reduce((sum, w) => sum + Object.values(w.requests).reduce((a, b) => a + b, 0), 0);
    const totalPty = windows.reduce((sum, w) => sum + w.ptyResizes, 0);
    // join ×3, поворот ×2, уход ×2 = 7 смен PTY; клавиатура — ни одной.
    expect(totalPty).toBe(7);
    // По одному запросу на подключение (3), переподключение (1) и возврат из фона (1).
    expect(totalRequests).toBeLessThanOrEqual(5 + 2);
  });

  it("старое правило на том же сценарии: скрытый компьютер после переподключения застревает в чужой сетке", () => {
    // Замер модели (seed 1000): по ШИРИНЕ старое правило не крутится, а
    // застревает. Первый кадр 48×20 → resync без записи и запрос; второй кадр
    // → resync, но запрос срезан троттлом 5 с, а без запроса кадров больше нет.
    // Итог окна: 2 запроса, сетка 120×40 при PTY 48×20 — кадр не пишется, пока
    // вкладка не станет видимой и опрос /state не принесёт авторитетную ширину.
    // Круг «раз в секунду» — это вариант ВЫСОТЫ (следующий describe).
    let stuckSeeds = 0;
    for (const seed of SEEDS) {
      const { windows } = runScript(seed, "old", JOURNEY);
      const hidden = windows.find(w => w.label === "скрытый компьютер переподключился")!;
      const shown = windows.find(w => w.label === "вкладка компьютера снова видна")!;
      if (hidden.gridMismatch.some(m => m.startsWith("desk:")) && shown.gridMismatch.length === 0) stuckSeeds++;
    }
    expect(stuckSeeds).toBe(SEEDS.length);
  });
});

describe("SIM-TWO-VIEWERS: вечный цикл при двух высотах в скрытой вкладке (журнал 2026-09-03, п. 4)", () => {
  // Одна ширина, разная высота: высокий компьютер скрыт и переподключается.
  // Кадр приходит в общей сетке 48×20, клиент держит 48×40 и считает кадр
  // «возможно чужим»: resync → запрос → тот же кадр → … раз в секунду. Сойтись
  // мог только опрос /state, а в скрытой вкладке его нет.
  const LOOP: Step[] = [
    { at: 0, label: "высокий компьютер", act: w => { w.join("desk", { cols: 48, rows: 40 }); } },
    { at: 10_000, label: "вкладка скрыта", act: w => w.live("desk").hide() },
    { at: 20_000, label: "низкий телефон", act: w => { w.join("phone", { cols: 48, rows: 20 }); } },
    { at: 30_000, label: "скрытый компьютер переподключился", act: w => w.live("desk").reconnect() },
  ];

  function loopRun(seed: number, rule: Rule) {
    const { w, windows } = runScript(seed, rule, LOOP, 60_000, 0);
    const reconnect = windows[windows.length - 1];
    return { w, requests: reconnect.requests.desk, desk: w.live("desk"), i10: windows.flatMap(x => x.i10) };
  }

  it("СТАРОЕ правило (capacityDelivered=false): за минуту ≥ 30 запросов, сетка так и не сошлась", () => {
    for (const seed of SEEDS) {
      const { w, requests, desk, i10 } = loopRun(seed, "old");
      expect(requests, `seed ${seed}`).toBeGreaterThanOrEqual(30);
      expect(same(desk.term, w.server.pty), `seed ${seed}: ${fmt(desk.term)} против ${fmt(w.server.pty)}`).toBe(false);
      expect(i10, `seed ${seed}`).toEqual([]); // цикл — не из-за лжи о вместимости
    }
  });

  it("НОВОЕ правило: один запрос, сетка = PTY, вместимость серверу — своя (48×40)", () => {
    for (const seed of SEEDS) {
      const { w, requests, desk, i10 } = loopRun(seed, "new");
      expect(requests, `seed ${seed}`).toBeLessThanOrEqual(2);
      expect(desk.term, `seed ${seed}`).toEqual(w.server.pty);
      expect(w.server.pty).toEqual({ cols: 48, rows: 20 });
      expect(desk.claimedCapacity()).toEqual({ cols: 48, rows: 40 });
      expect(i10, `seed ${seed}`).toEqual([]);
    }
  });
});

describe("SIM-TWO-VIEWERS: телефон открыт уже с поднятой клавиатурой (I-10, дыра (а))", () => {
  const KB: Step[] = [
    { at: 0, label: "компьютер открыл терминал", act: w => { w.join("desk", DESKTOP); } },
    { at: 10_000, label: "телефон открыт с клавиатурой", act: w => { w.join("phone", PHONE, { keyboard: true }); } },
    { at: 20_000, label: "клавиатура убрана", act: w => w.live("phone").keyboardClose() },
  ];

  it("новое правило: под клавиатурой вместимость неизвестна и не выдумывается; после — ровно одна отправка", () => {
    for (const seed of SEEDS) {
      const { w, windows, rest } = runScript(seed, "new", KB);
      expectConverges(seed, windows.filter(x => x.label !== "клавиатура убрана"), rest);
      const opened = windows[1], closed = windows[2];
      expect(opened.resizes.phone, `seed ${seed}`).toBe(0);
      expect(closed.resizes.phone, `seed ${seed}`).toBe(1);
      expect(closed.gridMismatch, `seed ${seed}`).toEqual([]);
      expect(closed.i10, `seed ${seed}`).toEqual([]);
      expect(w.server.pty).toEqual(PHONE);
    }
  });

  it("старое правило: клиент выдаёт за вместимость клавиатурную высоту (I-10 нарушен)", () => {
    const { windows } = runScript(1000, "old", KB);
    const opened = windows[1];
    expect(opened.i10.some(v => v.includes("48x8"))).toBe(true);
  });
});
