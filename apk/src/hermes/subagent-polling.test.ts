import { afterEach, describe, expect, it, vi } from "vitest";
import { startSubagentActivityPolling } from "./subagent-polling";
afterEach(()=>vi.useRealTimers());
describe("selected open child activity poll lifecycle",()=>{
 it("is non-overlapping, stops on unsupported, aborts on disposal and fences late publication",async()=>{
  vi.useFakeTimers();let resolve!:(v:unknown)=>void;let signal!:AbortSignal;const accept=vi.fn(()=>false);
  const fetch=vi.fn((s:AbortSignal)=>{signal=s;return new Promise(r=>{resolve=r;});});
  const stop=startSubagentActivityPolling({fetch,accept,error:vi.fn(),visible:()=>true,subscribe:()=>()=>{}});
  expect(fetch).toHaveBeenCalledTimes(1);
  await vi.advanceTimersByTimeAsync(10000);expect(fetch).toHaveBeenCalledTimes(1);
  resolve({supported:false});await vi.advanceTimersByTimeAsync(10000);
  expect(accept).toHaveBeenCalledTimes(1);expect(fetch).toHaveBeenCalledTimes(1);stop();expect(signal.aborted).toBe(true);
  const acceptLate=vi.fn(()=>true);const stopLate=startSubagentActivityPolling({fetch,accept:acceptLate,error:vi.fn(),visible:()=>true,subscribe:()=>()=>{}});
  stopLate();resolve({supported:true});await vi.advanceTimersByTimeAsync(3000);expect(acceptLate).not.toHaveBeenCalled();
 });
 it("polls at most every 2500ms and pauses/restarts on document visibility",async()=>{
  vi.useFakeTimers();let visible=true;let change!:()=>void;const fetch=vi.fn(async()=>({supported:true}));
  const stop=startSubagentActivityPolling({fetch,accept:()=>true,error:vi.fn(),visible:()=>visible,subscribe:f=>{change=f;return()=>{};}});
  await vi.advanceTimersByTimeAsync(2499);expect(fetch).toHaveBeenCalledTimes(1);
  await vi.advanceTimersByTimeAsync(1);expect(fetch).toHaveBeenCalledTimes(2);
  visible=false;change();await vi.advanceTimersByTimeAsync(10000);expect(fetch).toHaveBeenCalledTimes(2);
  visible=true;change();expect(fetch).toHaveBeenCalledTimes(3);stop();
 });
});
