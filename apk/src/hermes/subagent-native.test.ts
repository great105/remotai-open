import { describe, expect, it } from "vitest";
import { normalizeNativeSubagentActivity } from "./subagent-progress";
const native = (entries: unknown[]) => ({supported:true,available:true,source:"native_live_log",source_time_zone:"unknown",captured_at_ms:4000,truncated:false,history_incomplete:true,entries});
describe("native safe bounded log snapshots", () => {
 it("projects exact time-of-day and real callback duration without pairing names or copying private fields", () => {
  const rows = normalizeNativeSubagentActivity(native([
   {id:"one",kind:"tool_start",tool_name:"terminal",source_time_text:"23:59:59",args:"PRIVATE"},
   {id:"two",kind:"tool_result",tool_name:"terminal",source_time_text:"00:00:01",status:"error",duration_seconds:1.2,result:"PRIVATE",reasoning:"PRIVATE"},
  ]));
  expect(rows).toEqual({state:"supported",partial:true,capturedAt:4000,entries:[
   {id:"one",kind:"tool_start",toolName:"terminal",sourceTimeText:"23:59:59"},
   {id:"two",kind:"tool_result",toolName:"terminal",sourceTimeText:"00:00:01",status:"error",durationSeconds:1.2},
  ]});
 });
 it("distinguishes unsupported/unavailable/malformed from an empty supported log", () => {
  expect(normalizeNativeSubagentActivity({supported:false}).state).toBe("unsupported");
  expect(normalizeNativeSubagentActivity({...native([]),available:false}).state).toBe("unavailable");
  expect(normalizeNativeSubagentActivity(native([]))).toMatchObject({state:"supported",entries:[]});
  expect(normalizeNativeSubagentActivity({text:"PRIVATE",entries:[]})).toMatchObject({state:"error",entries:[]});
 });
 it("discards invalid private names and limits the untrusted window to 200 records", () => {
  const rows = normalizeNativeSubagentActivity(native(Array.from({length:250},(_,i)=>({id:String(i),kind:"tool_start",tool_name:"read_file",source_time_text:"12:34:56"}))));
  expect(rows.entries).toHaveLength(200);
  expect(rows.partial).toBe(true);
  expect(normalizeNativeSubagentActivity(native([{id:"bad",kind:"tool_result",tool_name:"terminal(secret)",source_time_text:"99:99:99",status:"ok",duration_seconds:Infinity}])).entries).toEqual([]);
 });
});
