import type { TerminalWriteGuard } from "./terminalWriter";

const EMPTY = new Uint8Array(0);

function keyOf(guard: TerminalWriteGuard): string {
  return `${guard.generation}\u0000${guard.epoch}`;
}

/**
 * Parser carry state is scoped to a writer guard, never to the React component.
 * A future epoch may be preprocessed while the old xterm barrier is still in
 * flight; keyed tails/gaps prevent that future work from consuming old state.
 */
export class GuardedParserCarry {
  private tails = new Map<string, Uint8Array>();
  private gaps = new Set<string>();
  private erases = new Set<string>();

  takeTail(guard: TerminalWriteGuard): Uint8Array {
    const key = keyOf(guard);
    const tail = this.tails.get(key);
    this.tails.delete(key);
    return tail ?? EMPTY;
  }

  storeTail(guard: TerminalWriteGuard, tail: Uint8Array): void {
    const key = keyOf(guard);
    if (tail.byteLength === 0) this.tails.delete(key);
    else this.tails.set(key, tail);
  }

  /** Сколько байт удержано под guard (0–3 байта возможного начала ESC[3J). */
  tailLength(guard: TerminalWriteGuard): number {
    return this.tails.get(keyOf(guard))?.byteLength ?? 0;
  }

  clearTail(guard?: TerminalWriteGuard): void {
    if (guard) this.tails.delete(keyOf(guard));
    else this.tails.clear();
  }

  markGap(guard: TerminalWriteGuard): void {
    this.gaps.add(keyOf(guard));
  }

  hasGap(guard: TerminalWriteGuard): boolean {
    return this.gaps.has(keyOf(guard));
  }

  takeGap(guard: TerminalWriteGuard): boolean {
    const key = keyOf(guard);
    const present = this.gaps.has(key);
    this.gaps.delete(key);
    return present;
  }

  clearGap(guard: TerminalWriteGuard): void {
    this.gaps.delete(keyOf(guard));
  }

  markErase(guard: TerminalWriteGuard): void {
    this.erases.add(keyOf(guard));
  }

  hasErase(guard: TerminalWriteGuard): boolean {
    return this.erases.has(keyOf(guard));
  }

  clearErase(guard: TerminalWriteGuard): void {
    this.erases.delete(keyOf(guard));
  }

  clearAllErases(): void {
    // A mode change invalidates erase intent across writer epochs, but partial
    // parser tails and real queue-gap notices still belong to their guards.
    this.erases.clear();
  }

  get hasAnyGap(): boolean {
    return this.gaps.size > 0;
  }

  /** Есть ли отложенное стирание хоть под одним guard — тень хранения (ST-04)
   * сообщает о границе поколения, только когда снимать действительно есть что. */
  get hasAnyErase(): boolean {
    return this.erases.size > 0;
  }

  clear(): void {
    this.tails.clear();
    this.gaps.clear();
    this.erases.clear();
  }
}
