import { describe, expect, it } from "vitest";
import type { PtySessionInfo } from "@tgcontrol/shared";
import type { CloudDevice } from "../cloud/api";
import { computerIds, computerListHome, computerTerminalKey, filterComputerTerminals, loadComputerTerminals, matchingComputerTerminals, orderComputerTerminals } from "./computerList";

const device = (id: string, online = true) => ({ id, name: id, hostname: id, workspace_name: "Team", online }) as CloudDevice;
const session = (id: string, fields: Partial<PtySessionInfo> = {}) => ({ id, name: id, created: 1, cwd: "/project", ...fields }) as PtySessionInfo;

describe("combined computer terminals", () => {
  it("accepts only unique computer IDs from stored preferences", () => {
    expect(computerIds(["work", null, "", " ", 4, "work", "linux"])).toEqual(["work", "linux"]);
    expect(computerIds({ work: true })).toEqual([]);
  });
  it("encodes the exact home computer for return navigation", () => {
    const home = new URL(computerListHome("pc/a&b"), "https://fixture.invalid");
    expect(home.pathname).toBe("/pty");
    expect(home.searchParams.get("computer")).toBe("pc/a&b");
  });
  it("applies status filters to remote terminals and includes stalled work", () => {
    const sessions = [session("stalled", { status: "stalled" }), session("working", { status: "working" }), session("waiting", { status: "waiting" })];
    expect(filterComputerTerminals(sessions, "working").map(s => s.id)).toEqual(["stalled", "working"]);
    expect(filterComputerTerminals(sessions, "waiting").map(s => s.id)).toEqual(["waiting"]);
  });
  it("keeps colliding local ids separate, including delimiter-containing ids", () => {
    expect(computerTerminalKey("a:b", "c")).not.toBe(computerTerminalKey("a", "b:c"));
    expect(computerTerminalKey("work", "same")).not.toBe(computerTerminalKey("home", "same"));
  });
  it("searches machine names, groups and paths without translating user data", () => {
    const sessions = [session("a", { group: "Дизайн" }), session("b", { cwd: "/code/Backend" })];
    expect(matchingComputerTerminals(device("Laptop"), sessions, "laptop")).toEqual(sessions);
    expect(matchingComputerTerminals(device("Laptop"), sessions, "дизайн")).toEqual([sessions[0]]);
    expect(matchingComputerTerminals(device("Laptop"), sessions, "BACKEND")).toEqual([sessions[1]]);
  });
  it("activity changes do not move terminals, and preserves manual order", () => {
    const sessions = [session("a", { created: 2 }), session("b", { created: 3 }), session("c", { sort: -5 })];
    expect(orderComputerTerminals(sessions).map(s => s.id)).toEqual(["c", "b", "a"]);
    expect(orderComputerTerminals(sessions.map(s => ({ ...s, status: "working", last_active: Date.now() }))).map(s => s.id)).toEqual(["c", "b", "a"]);
  });
  it("does not poll offline PCs, bounds concurrency and delivers healthy PCs before a slow one", async () => {
    const seen: string[] = [], delivered: string[] = []; let active = 0, peak = 0;
    let release!: () => void;
    const slow = new Promise<void>(resolve => { release = resolve; });
    const pending = loadComputerTerminals([device("slow"), device("offline", false), ...Array.from({ length: 7 }, (_, n) => device(String(n)))], async d => {
      seen.push(d.id); peak = Math.max(peak, ++active);
      if (d.id === "slow") await slow;
      else await Promise.resolve();
      active--; return d.id;
    }, (d, result) => { expect(result.status).toBe("fulfilled"); delivered.push(d.id); }, new AbortController().signal);
    await new Promise(resolve => setTimeout(resolve, 0));
    expect(delivered).toHaveLength(7); expect(delivered).not.toContain("slow"); expect(seen).not.toContain("offline"); expect(peak).toBeLessThanOrEqual(4);
    release(); await pending; expect(delivered).toHaveLength(8);
  });
  it("an individual failure preserves success on other machines", async () => {
    const results: PromiseSettledResult<string>[] = [];
    await loadComputerTerminals([device("bad"), device("good")], async d => { if (d.id === "bad") throw Error("offline"); return d.id; }, (_, result) => results.push(result), new AbortController().signal);
    expect(results.map(r => r.status).sort()).toEqual(["fulfilled", "rejected"]);
  });
  it("ignores late results and queued requests after leaving the aggregate view", async () => {
    const controller = new AbortController(); const seen: string[] = []; let delivered = 0;
    let release!: () => void; const wait = new Promise<void>(resolve => { release = resolve; });
    const pending = loadComputerTerminals(Array.from({ length: 8 }, (_, n) => device(String(n))), async d => { seen.push(d.id); await wait; return d.id; }, () => delivered++, controller.signal);
    controller.abort(); release(); await pending;
    expect(seen).toHaveLength(4); expect(delivered).toBe(0);
  });
});
