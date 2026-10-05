export type TerminalWriteData = string | Uint8Array;

export interface TerminalWriteTarget {
  write(data: TerminalWriteData, callback?: () => void): void;
}

export interface TerminalWriteContext {
  generation: number;
  epoch: string;
}

export type TerminalWriteGuard = Readonly<TerminalWriteContext>;
export type TerminalWriteResult = "written" | "discarded" | "error";

export interface TerminalWriteOptions {
  /** Defaults to the writer context at enqueue time. */
  guard?: TerminalWriteGuard;
  /** Runs only after xterm parsed this write and the guard is still current. */
  after?: () => void;
  /** Runs exactly once, including stale/disposed/error paths. */
  settled?: (result: TerminalWriteResult) => void;
}

type Entry = {
  resolve: () => TerminalWriteData | null;
  guard: TerminalWriteGuard;
  /** Ordered sync markers may follow each other before the first barrier runs. */
  allowEpochDrift: boolean;
  /** Present only for the actor's ordered context-boundary entry. */
  transitionTo?: TerminalWriteContext;
  after?: () => void;
  settled?: (result: TerminalWriteResult) => void;
  settledOnce: boolean;
};

const EMPTY_WRITE = new Uint8Array(0);

/**
 * The only door into `Terminal.write`.
 *
 * xterm serializes bytes internally, but a callback that calls `term.write(B)`
 * does not get priority over an already queued `term.write(C)`: the real order
 * is A,C,B. Protocol callbacks in PtyTermView need A,B,C (old output, reset,
 * modes, then concurrently received new output), so this actor owns a queue and
 * allows exactly one xterm write in flight.
 */
export class TerminalWriter {
  private target: TerminalWriteTarget | null;
  private context: TerminalWriteContext;
  private queue: Entry[] = [];
  private active: Entry | null = null;
  private inAfter = false;
  private afterInsertAt = 0;
  private disposed = false;

  constructor(
    target: TerminalWriteTarget | null = null,
    context: TerminalWriteContext = { generation: 0, epoch: "" },
  ) {
    this.target = target;
    this.context = { ...context };
  }

  get pending(): number {
    return this.queue.length + (this.active ? 1 : 0);
  }

  currentGuard(): TerminalWriteGuard {
    return { ...this.context };
  }

  isCurrent(guard: TerminalWriteGuard): boolean {
    return guard.generation === this.context.generation && guard.epoch === this.context.epoch;
  }

  setTarget(target: TerminalWriteTarget | null): void {
    if (this.disposed) return;
    this.target = target;
    if (!this.inAfter) this.drain();
  }

  setContext(context: TerminalWriteContext): void {
    if (this.disposed) return;
    this.context = { ...context };
    if (!this.inAfter) this.drain();
  }

  write(data: TerminalWriteData, options: TerminalWriteOptions = {}): void {
    this.enqueue(() => data, options);
  }

  /** Resolve data only when its turn arrives; useful for conditional erase/gap. */
  lazy(resolve: () => TerminalWriteData | null, options: TerminalWriteOptions = {}): void {
    this.enqueue(resolve, options);
  }

  /** A real xterm callback barrier in the same FIFO as every data write. */
  barrier(after: () => void, options: Omit<TerminalWriteOptions, "after"> = {}): void {
    this.enqueue(() => EMPTY_WRITE, { ...options, after });
  }

  /**
   * Ordered epoch transition. Future-epoch data may already be waiting while
   * the old write is in flight; the boundary must overtake that data, switch
   * context atomically, enqueue reset/modes, and only then let future data run.
   */
  transition(context: TerminalWriteContext, boundary: () => void): void {
    const old = this.currentGuard();
    // Epochs may advance within one socket, generations may not. A delayed
    // callback from a replaced socket must never move the actor backwards.
    if (context.generation !== old.generation) return;
    const entry = this.makeEntry(() => EMPTY_WRITE, {
      guard: old,
      after: () => {
        this.setContext(context);
        boundary();
      },
    });
    // A second marker can be queued while the first marker still waits behind
    // an old xterm write. The first transition legitimately changes the epoch;
    // that must not make the second transition stale. Connection generation is
    // still strict, so a marker from a replaced socket remains discardable.
    entry.allowEpochDrift = true;
    entry.transitionTo = { ...context };
    if (this.disposed) {
      this.settle(entry, "discarded");
      return;
    }
    // Follow already pending transitions while finding the cut. Example:
    //   current A, transition→B, B-DATA, transition→C
    // C belongs after B-DATA, whereas the first transition must still overtake
    // B-DATA if that data raced in before transition→B was enqueued.
    let logical = this.active?.transitionTo
      && this.active.guard.generation === old.generation
      ? this.active.transitionTo
      : old;
    let insertAt = 0;
    for (; insertAt < this.queue.length; insertAt++) {
      const queued = this.queue[insertAt];
      if (queued.transitionTo && queued.guard.generation === old.generation) {
        logical = queued.transitionTo;
        continue;
      }
      if (!this.sameGuard(queued.guard, logical)) break;
    }
    this.queue.splice(insertAt, 0, entry);
    if (!this.inAfter) this.drain();
  }

  dispose(): void {
    if (this.disposed) return;
    this.disposed = true;
    this.target = null;
    const entries = this.active ? [this.active, ...this.queue] : this.queue.slice();
    this.active = null;
    this.queue = [];
    for (const entry of entries) this.settle(entry, "discarded");
  }

  /** Drop parser work and target while keeping the actor reusable (React StrictMode). */
  detach(): void {
    if (this.disposed) return;
    this.target = null;
    const entries = this.active ? [this.active, ...this.queue] : this.queue.slice();
    this.active = null;
    this.queue = [];
    for (const entry of entries) this.settle(entry, "discarded");
  }

  private enqueue(resolve: Entry["resolve"], options: TerminalWriteOptions): void {
    const entry = this.makeEntry(resolve, options);
    if (this.disposed) {
      this.settle(entry, "discarded");
      return;
    }
    // Writes created by A's completion are protocol continuations of A. Put
    // them ahead of C that arrived concurrently, while preserving B1,B2 order.
    if (this.inAfter) {
      this.queue.splice(this.afterInsertAt, 0, entry);
      this.afterInsertAt++;
    } else {
      this.queue.push(entry);
    }
    if (!this.inAfter) this.drain();
  }

  private makeEntry(resolve: Entry["resolve"], options: TerminalWriteOptions): Entry {
    return {
      resolve,
      guard: options.guard ? { ...options.guard } : this.currentGuard(),
      allowEpochDrift: false,
      transitionTo: undefined,
      after: options.after,
      settled: options.settled,
      settledOnce: false,
    };
  }

  private sameGuard(a: TerminalWriteGuard, b: TerminalWriteGuard): boolean {
    return a.generation === b.generation && a.epoch === b.epoch;
  }

  private isEntryCurrent(entry: Entry): boolean {
    return entry.guard.generation === this.context.generation
      && (entry.allowEpochDrift || entry.guard.epoch === this.context.epoch);
  }

  private drain(): void {
    if (this.disposed || this.active || !this.target) return;
    for (;;) {
      const entry = this.queue.shift();
      if (!entry) return;
      if (!this.isEntryCurrent(entry)) {
        this.settle(entry, "discarded");
        continue;
      }
      let data: TerminalWriteData | null;
      try {
        data = entry.resolve();
      } catch {
        this.settle(entry, "error");
        continue;
      }
      if (data == null) {
        this.complete(entry, "written");
        if (this.active) return;
        continue;
      }
      this.active = entry;
      try {
        this.target.write(data, () => {
          if (this.active !== entry) return; // disposed/replaced; already settled
          this.active = null;
          this.complete(entry, this.isEntryCurrent(entry) ? "written" : "discarded");
          this.drain();
        });
      } catch {
        this.active = null;
        this.complete(entry, "error");
        continue;
      }
      return;
    }
  }

  private complete(entry: Entry, result: TerminalWriteResult): void {
    this.settle(entry, result);
    if (result !== "written" || !entry.after || !this.isEntryCurrent(entry) || this.disposed) return;
    this.inAfter = true;
    this.afterInsertAt = 0;
    try {
      entry.after();
    } finally {
      this.inAfter = false;
      this.afterInsertAt = 0;
    }
  }

  private settle(entry: Entry, result: TerminalWriteResult): void {
    if (entry.settledOnce) return;
    entry.settledOnce = true;
    entry.settled?.(result);
  }
}
