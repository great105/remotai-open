import { describe, expect, it } from "vitest";
import { lastViewedTerminal, rememberTerminal, terminalToContinue } from "./terminalHistory";
import type { PtySessionInfo } from "./types";

const session = (id: string, alive = true, last_active = 1) => ({ id, alive, last_active } as PtySessionInfo);

describe("return to the user's terminal", () => {
  it("keeps visits separate for every connected machine", () => {
    const values = new Map<string, string>();
    const storage = { getItem: (k: string) => values.get(k) ?? null, setItem: (k: string, v: string) => { values.set(k, v); } };
    rememberTerminal(storage, "cloud:relay:work", "work-session");
    rememberTerminal(storage, "lan:http://home:8080", "home-session");
    expect(lastViewedTerminal(storage, "cloud:relay:work")).toBe("work-session");
    expect(lastViewedTerminal(storage, "lan:http://home:8080")).toBe("home-session");
    expect(lastViewedTerminal(storage, "cloud:relay:another")).toBe("");
  });

  it("ignores output activity and preserves input ordering", () => {
    const list = [session("background", true, 9999), session("visited", true, 1)];
    expect(terminalToContinue(list, "visited")?.id).toBe("visited");
    expect(list.map(s => s.id)).toEqual(["background", "visited"]);
  });

  it("does not substitute another terminal when the visited one ended or disappeared", () => {
    const list = [session("ended", false), session("other")];
    expect(terminalToContinue(list, "ended")).toBeUndefined();
    expect(terminalToContinue(list, "missing")).toBeUndefined();
    expect(terminalToContinue(list, "")).toBeUndefined();
  });

  it("remains usable when storage is unavailable", () => {
    const storage = { getItem: () => { throw new Error("blocked"); }, setItem: () => { throw new Error("blocked"); } };
    expect(() => rememberTerminal(storage, "pc", "session")).not.toThrow();
    expect(lastViewedTerminal(storage, "pc")).toBe("");
  });
});
