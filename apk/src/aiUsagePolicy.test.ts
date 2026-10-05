import { describe, it, expect, vi } from "vitest";
import { mainUsageWindows, preferUsage, usageAgeLabel, usageIsStale, quotaFromUsage } from "./aiUsagePolicy";
import { createUsageResource } from "./aiUsageResource";
import type { AIProviderUsage } from "./aiUsage";

const now = Date.parse("2026-09-06T12:00:00Z");
const provider = (over: Partial<AIProviderUsage> = {}): AIProviderUsage => ({
  id:"claude",name:"Claude",installed:true,status:"available",captured_at:new Date(now-20_000).toISOString(),
  windows:[{id:"five_hour",label:"5h",used_percent:63}],...over,
});

describe("usage identity and age", () => {
 it("keeps Spark separate and respects an explicit weekly primary", () => {
  const p = provider({id:"codex", windows:[
   {id:"codex:primary",label:"main",duration_minutes:10080,used_percent:24},
   {id:"codex_bengalfox:primary",label:"Spark",duration_minutes:300,used_percent:0},
  ]});
  expect(mainUsageWindows(p).five).toBeUndefined();
  expect(mainUsageWindows(p).seven?.used_percent).toBe(24);
  expect(quotaFromUsage({captured_at:p.captured_at!, providers:[p]}, "codex")?.five_hour_used).toBeUndefined();
 });
 it("does not guess a window duration", () => {
  expect(mainUsageWindows(provider({id:"codex",windows:[{id:"codex:primary",label:"main",used_percent:42}]}))).toEqual({});
 });
 it("chooses newer data even with fewer windows", () => {
  const older = provider({captured_at:new Date(now-60_000).toISOString()});
  const newer = provider({windows:[{id:"five_hour",label:"5h",used_percent:90}]});
  expect(preferUsage(older,newer)).toBe(newer);
  expect(preferUsage(newer,older)).toBe(newer);
 });
 it("dates the observation and marks expired resets without inventing 100 percent", () => {
  const p = provider();
  expect(usageAgeLabel(p,now)).toContain("20 с назад");
  expect(usageIsStale(p,now+120_000)).toBe(true);
  p.windows[0].resets_at=(now-1_000)/1000;
  expect(usageIsStale(p,now)).toBe(true);
  expect(p.windows[0].used_percent).toBe(63);
 });
});

describe("shared usage resource", () => {
 it("publishes one result to all surfaces and force bypasses the client TTL", async () => {
  const fetcher = vi.fn(async () => 22);
  const resource = createUsageResource(34,fetcher);
  const values:number[]=[]; resource.subscribe(()=>values.push(resource.get()));
  await Promise.all([resource.load(),resource.load(),resource.load()]);
  expect(fetcher).toHaveBeenCalledTimes(1);
  expect(values).toEqual([22]);
  await resource.load(true);
  expect(fetcher).toHaveBeenCalledTimes(2);
 });
 it("rejects an old account response after invalidation", async () => {
  let release!:(n:number)=>void;
  const first=new Promise<number>(resolve=>{release=resolve;});
  const fetcher=vi.fn().mockReturnValueOnce(first).mockResolvedValue(9);
  const resource=createUsageResource(0,fetcher);
  const values:number[]=[];resource.subscribe(()=>values.push(resource.get()));
  const request=resource.load();
  await Promise.resolve();
  resource.invalidate(); release(88);
  expect(await request).toBe(9);
  expect(values).toEqual([0,9]);
 });
});
