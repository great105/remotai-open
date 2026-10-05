import { describe, expect, it } from "vitest";
import { GuardedParserCarry } from "./parserCarry";

describe("GuardedParserCarry", () => {
  const a = { generation: 1, epoch: "A" };
  const b = { generation: 1, epoch: "B" };

  it("never glues a partial escape prefix from the old epoch to future bytes", () => {
    const carry = new GuardedParserCarry();
    carry.storeTail(a, new TextEncoder().encode("\x1b[3"));

    expect(carry.takeTail(b)).toHaveLength(0);
    expect(new TextDecoder().decode(carry.takeTail(a))).toBe("\x1b[3");
  });

  it("clearing the old boundary cannot erase a future epoch tail", () => {
    const carry = new GuardedParserCarry();
    carry.storeTail(a, new Uint8Array([0x1b]));
    carry.storeTail(b, new Uint8Array([0x1b, 0x5b]));

    carry.clearTail(a);
    expect([...carry.takeTail(b)]).toEqual([0x1b, 0x5b]);
  });

  it("keeps queue-gap notices isolated by generation and epoch", () => {
    const carry = new GuardedParserCarry();
    carry.markGap(a);
    carry.markGap(b);

    expect(carry.takeGap(a)).toBe(true);
    expect(carry.hasGap(a)).toBe(false);
    expect(carry.hasGap(b)).toBe(true);
    carry.clearGap(a); // an old-boundary cleanup is scoped and idempotent
    expect(carry.takeGap(b)).toBe(true);
    expect(carry.hasAnyGap).toBe(false);
  });

  it("a reset boundary clears an old re-armed erase without touching future work", () => {
    const carry = new GuardedParserCarry();
    carry.clearErase(a);       // marker pre-clear
    carry.markErase(a);       // old batch preprocessing re-arms ESC[3J
    carry.markErase(b);       // future preprocessing may already have happened

    carry.clearErase(a);      // decisive boundary clear after old drain
    expect(carry.hasErase(a)).toBe(false);
    expect(carry.hasErase(b)).toBe(true);
  });

  it("an Output transition removes deferred erases from every writer guard", () => {
    const carry = new GuardedParserCarry();
    carry.markErase(a);
    carry.markErase(b);
    carry.markGap(a);
    carry.storeTail(a, new Uint8Array([0x1b, 0x5b]));

    carry.clearAllErases();

    expect(carry.hasErase(a)).toBe(false);
    expect(carry.hasErase(b)).toBe(false);
    expect(carry.hasGap(a)).toBe(true);
    expect([...carry.takeTail(a)]).toEqual([0x1b, 0x5b]);
  });

  it("tailLength reads the held tail of one guard without consuming it (ST-05 warm resume)", () => {
    const carry = new GuardedParserCarry();
    expect(carry.tailLength(a)).toBe(0);
    carry.storeTail(a, new Uint8Array([0x1b, 0x5b, 0x33]));
    carry.storeTail(b, new Uint8Array([0x1b]));
    expect(carry.tailLength(a)).toBe(3);
    expect(carry.tailLength(b)).toBe(1);
    expect([...carry.takeTail(a)]).toEqual([0x1b, 0x5b, 0x33]);
    expect(carry.tailLength(a)).toBe(0);
  });

  it("hasAnyErase sees a deferred erase under any guard and only erases, not gaps or tails", () => {
    const carry = new GuardedParserCarry();
    carry.markGap(a);
    carry.storeTail(a, new Uint8Array([0x1b]));
    expect(carry.hasAnyErase).toBe(false);

    carry.markErase(b);
    expect(carry.hasAnyErase).toBe(true);
    carry.clearErase(a); // other guard: still pending under b
    expect(carry.hasAnyErase).toBe(true);
    carry.clearAllErases();
    expect(carry.hasAnyErase).toBe(false);
  });
});
