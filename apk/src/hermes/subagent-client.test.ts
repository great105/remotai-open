import { describe, expect, it } from "vitest";
import { createHermesClient } from "./client";
describe("safe scoped subagent activity HTTP client", () => {
 it("uses only the dedicated GET route with literal IDs and an abort signal", async () => {
  const calls: Array<{path:string;init?:RequestInit}> = [];
  const client = createHermesClient(() => ({ request: async <T>(path:string,init?:RequestInit) => {calls.push({path,init});return {supported:true} as T;} }));
  const abort = new AbortController();
  expect(typeof client.subagentActivity).toBe("function");
  await client.subagentActivity("live &1", "child/2", abort.signal);
  expect(calls).toEqual([{path:"/api/hermes/subagents/activity?session_id=live%20%261&subagent_id=child%2F2",init:{signal:abort.signal}}]);
 });
});
