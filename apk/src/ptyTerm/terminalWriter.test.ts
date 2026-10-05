import { describe, expect, it } from "vitest";
import { Terminal as HeadlessTerminal } from "@xterm/headless";
import { TerminalWriter } from "./terminalWriter";

class ManualTerminal {
  readonly writes: string[] = [];
  private callbacks: Array<() => void> = [];
  inFlight = 0;
  maxInFlight = 0;

  write(data: string | Uint8Array, callback?: () => void) {
    this.writes.push(typeof data === "string" ? data : new TextDecoder().decode(data));
    this.inFlight++;
    this.maxInFlight = Math.max(this.maxInFlight, this.inFlight);
    this.callbacks.push(() => {
      this.inFlight--;
      callback?.();
    });
  }

  completeOne() {
    const callback = this.callbacks.shift();
    if (!callback) throw new Error("no terminal write is pending");
    callback();
  }
}

describe("TerminalWriter", () => {
  it("serializes callback-enqueued B ahead of already waiting C (ABC, never ACB)", () => {
    const terminal = new ManualTerminal();
    const writer = new TerminalWriter(terminal, { generation: 1, epoch: "e1" });

    writer.write("A", { after: () => writer.write("B") });
    writer.write("C");

    expect(terminal.writes).toEqual(["A"]);
    terminal.completeOne();
    expect(terminal.writes).toEqual(["A", "B"]);
    terminal.completeOne();
    expect(terminal.writes).toEqual(["A", "B", "C"]);
    terminal.completeOne();
    expect(terminal.maxInFlight).toBe(1);
  });

  it("produces ABC with the real xterm parser for callback B plus concurrent C", async () => {
    const terminal = new HeadlessTerminal({ allowProposedApi: true, cols: 20, rows: 4 });
    const writer = new TerminalWriter(terminal, { generation: 1, epoch: "e1" });

    await new Promise<void>((resolve) => {
      writer.write("A", { after: () => writer.write("B") });
      writer.write("C", { after: resolve });
    });

    expect(terminal.buffer.active.getLine(0)?.translateToString(true)).toBe("ABC");
    terminal.dispose();
  });

  it("keeps multiple callback continuations together ahead of concurrent work", () => {
    const terminal = new ManualTerminal();
    const writer = new TerminalWriter(terminal, { generation: 1, epoch: "e1" });
    writer.write("A", { after: () => { writer.write("B1"); writer.write("B2"); } });
    writer.write("C");

    terminal.completeOne();
    terminal.completeOne();
    terminal.completeOne();
    terminal.completeOne();
    expect(terminal.writes).toEqual(["A", "B1", "B2", "C"]);
    expect(terminal.maxInFlight).toBe(1);
  });

  it("drops queued work from an old generation and epoch", () => {
    const terminal = new ManualTerminal();
    const writer = new TerminalWriter(terminal, { generation: 3, epoch: "old" });
    const completed: string[] = [];

    writer.write("A", { after: () => completed.push("old-after") });
    writer.write("OLD", { guard: { generation: 3, epoch: "old" } });
    writer.setContext({ generation: 4, epoch: "new" });
    writer.write("NEW", {
      guard: { generation: 4, epoch: "new" },
      after: () => completed.push("new-after"),
    });

    terminal.completeOne();
    expect(terminal.writes).toEqual(["A", "NEW"]);
    expect(completed).toEqual([]);
    terminal.completeOne();
    expect(completed).toEqual(["new-after"]);
  });

  it("keeps a barrier in the same FIFO as data", () => {
    const terminal = new ManualTerminal();
    const writer = new TerminalWriter(terminal, { generation: 1, epoch: "e" });
    const order: string[] = [];

    writer.write("A", { after: () => order.push("A") });
    writer.barrier(() => order.push("barrier"));
    writer.write("B", { after: () => order.push("B") });

    terminal.completeOne();
    terminal.completeOne();
    terminal.completeOne();
    expect(order).toEqual(["A", "barrier", "B"]);
    expect(terminal.maxInFlight).toBe(1);
  });

  it("puts an epoch boundary ahead of already queued future data", () => {
    const terminal = new ManualTerminal();
    const writer = new TerminalWriter(terminal, { generation: 7, epoch: "old" });
    const next = { generation: 7, epoch: "new" };

    writer.write("OLD");
    writer.write("NEW", { guard: next });
    writer.transition(next, () => {
      writer.write("RIS");
      writer.write("MODES");
    });

    terminal.completeOne(); // OLD
    terminal.completeOne(); // transition barrier
    terminal.completeOne(); // RIS
    terminal.completeOne(); // MODES
    terminal.completeOne(); // NEW
    expect(terminal.writes).toEqual(["OLD", "", "RIS", "MODES", "NEW"]);
    expect(terminal.maxInFlight).toBe(1);
  });

  it("serializes consecutive epoch transitions while the old write is in flight", () => {
    const terminal = new ManualTerminal();
    const writer = new TerminalWriter(terminal, { generation: 9, epoch: "A" });
    const b = { generation: 9, epoch: "B" };
    const c = { generation: 9, epoch: "C" };

    writer.write("OLD");
    writer.transition(b, () => writer.write("RIS-B"));
    writer.transition(c, () => writer.write("RIS-C"));
    writer.write("NEW-C", { guard: c });

    while (terminal.inFlight > 0) terminal.completeOne();
    expect(terminal.writes).toEqual(["OLD", "", "RIS-B", "", "RIS-C", "NEW-C"]);
    expect(writer.currentGuard()).toEqual(c);
    expect(terminal.maxInFlight).toBe(1);
  });

  it("keeps data of each pending epoch before the next transition boundary", () => {
    const terminal = new ManualTerminal();
    const writer = new TerminalWriter(terminal, { generation: 11, epoch: "A" });
    const b = { generation: 11, epoch: "B" };
    const c = { generation: 11, epoch: "C" };

    writer.write("OLD");
    writer.transition(b, () => writer.write("RIS-B"));
    writer.write("B-DATA", { guard: b });
    writer.transition(c, () => writer.write("RIS-C"));
    writer.write("C-DATA", { guard: c });

    while (terminal.inFlight > 0) terminal.completeOne();
    expect(terminal.writes).toEqual([
      "OLD", "", "RIS-B", "B-DATA", "", "RIS-C", "C-DATA",
    ]);
    expect(writer.currentGuard()).toEqual(c);
  });

  it("discards a late transition from a replaced socket generation", () => {
    const terminal = new ManualTerminal();
    const writer = new TerminalWriter(terminal, { generation: 3, epoch: "A" });
    const stale = { generation: 3, epoch: "B" };
    const fresh = { generation: 4, epoch: "X" };

    writer.write("OLD");
    writer.transition(stale, () => writer.write("STALE-RIS"));
    writer.setContext(fresh);
    writer.write("FRESH", { guard: fresh });

    while (terminal.inFlight > 0) terminal.completeOne();
    expect(terminal.writes).toEqual(["OLD", "FRESH"]);
    expect(writer.currentGuard()).toEqual(fresh);
  });

  it("rejects a transition requested only after its socket was replaced", () => {
    const terminal = new ManualTerminal();
    const writer = new TerminalWriter(terminal, { generation: 4, epoch: "X" });
    const stale = { generation: 3, epoch: "B" };

    writer.transition(stale, () => writer.write("STALE-RIS"));
    writer.write("FRESH");
    terminal.completeOne();

    expect(terminal.writes).toEqual(["FRESH"]);
    expect(writer.currentGuard()).toEqual({ generation: 4, epoch: "X" });
  });
});
