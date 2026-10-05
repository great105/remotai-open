import { describe, it, expect, vi } from "vitest";
import { createHermesClient } from "./client";
import * as control from "./control";
import { promptFromFrame } from "./state";

describe("Hermes control transport", () => {
 it("read commands fail closed on missing old or malformed HTTP capability metadata",()=>{
  const available=(control as any).availableReadCommand;expect(typeof available).toBe("function");
  for(const metadata of [undefined,{}, {version:0,without_arguments:["status"]},{version:1}, {version:1,without_arguments:"status"}])expect(available(metadata,"status")).toBe(false);
  const marker={version:1,without_arguments:["status","model"]};expect(available(marker,"status")).toBe(true);expect(available(marker,"/model")).toBe(true);
  for(const command of ["context","skills","plugins","status args","/status args"])expect(available(marker,command)).toBe(false);
 });
 it("maps the recorded approval.request method to contextual native choices", () => {
  const prompt=promptFromFrame({id:"alias",method:"approval.request",params:{choices:["once","deny"],command:"fixture"}});
  expect(prompt).toMatchObject({kind:"approval",title:"Разрешить действие?",choices:["once","deny"]});
 });
 it("opens linked conversations only on the exact selected cloud owner device",()=>{
  const parse=(control as any).controlDeepLink;expect(typeof parse).toBe("function");
  expect(parse("#/hermes?device=pc-A&session=chat-A&attention=req-A","pc-A","cloud")).toEqual({device:"pc-A",session:"chat-A",attention:"req-A",matches:true});
  expect(parse("#/hermes?device=pc-B&session=chat-B","pc-A","cloud").matches).toBe(false);
  expect(parse("#/hermes?device=pc-B&session=chat-B","pc-B","self_hosted").matches).toBe(false);
  expect(parse("#/hermes?device=pc-A&session=../wrong","pc-A","cloud")).toBeNull();
 });
 it("retains request identity across lost responses and reopen without leaking secret replies", () => {
  const values=new Map<string,string>();const storage={getItem:(key:string)=>values.get(key)??null,setItem:(key:string,value:string)=>{values.set(key,value)}};
  const identity=(control as any).submissionIdentity;
  expect(typeof identity).toBe("function");
  const first=identity(storage,"fixture-device","stored-A","task",true,()=>"id-1");
  expect(identity(storage,"fixture-device","stored-A","task",true,()=>"id-2")).toBe(first);
  expect(identity(storage,"fixture-device","stored-B","task",true,()=>"id-3")).not.toBe(first);
  expect(identity(storage,"fixture-device","stored-A","changed",true,()=>"id-4")).not.toBe(first);
 });
 it("retains uncertain request identity when private storage is unavailable", () => {
  const storage={getItem:()=>{throw Error("blocked")},setItem:()=>{throw Error("blocked")}};
  const first=control.submissionIdentity(storage,"private-device","stored-A","task",true,()=>"00000000-0000-4000-8000-000000000001");
  expect(control.submissionIdentity(storage,"private-device","stored-A","task",true,()=>"00000000-0000-4000-8000-000000000002")).toBe(first);
  control.clearSubmissionIdentity(storage,"private-device","stored-A",first);
  expect(control.submissionIdentity(storage,"private-device","stored-A","task",true,()=>"00000000-0000-4000-8000-000000000003")).toBe("00000000-0000-4000-8000-000000000003");
 });
 it("retains every unresolved fingerprint across edits, queue changes and module reload", async () => {
  const values = new Map<string, string>();
  const storage = { getItem: (key: string) => values.get(key) ?? null, setItem: (key: string, value: string) => { values.set(key, value); } };
  const first = control.submissionIdentity(storage, "reload-device", "stored-A", "A", true, () => "00000000-0000-4000-8000-000000000011");
  const second = control.submissionIdentity(storage, "reload-device", "stored-A", "B", true, () => "00000000-0000-4000-8000-000000000012");
  expect(control.submissionIdentity(storage, "reload-device", "stored-A", "A", false, () => "00000000-0000-4000-8000-000000000013")).toBe("00000000-0000-4000-8000-000000000013");
  vi.resetModules(); const reloaded = await import("./control");
  expect(reloaded.submissionIdentity(storage, "reload-device", "stored-A", "A", true, () => "00000000-0000-4000-8000-000000000014")).toBe(first);
  reloaded.clearSubmissionIdentity(storage, "reload-device", "stored-A", second);
  expect(reloaded.submissionIdentity(storage, "reload-device", "stored-A", "A", true, () => "00000000-0000-4000-8000-000000000014")).toBe(first);
  expect(reloaded.submissionIdentity(storage, "reload-device", "stored-A", "B", true, () => "00000000-0000-4000-8000-000000000015")).toBe("00000000-0000-4000-8000-000000000015");
 });
 it("refuses new fingerprints at the bound without evicting unresolved originals", () => {
  const values = new Map<string, string>();
  const storage = { getItem: (key: string) => values.get(key) ?? null, setItem: (key: string, value: string) => { values.set(key, value); } };
  for (let i = 0; i < 128; i++) control.submissionIdentity(storage, "bounded-device", "stored-A", `text-${i}`, true, () => `00000000-0000-4000-8000-${String(i).padStart(12, "0")}` as const);
  expect(() => control.submissionIdentity(storage, "bounded-device", "stored-B", "overflow", true)).toThrow(/журнал/);
  expect(control.submissionIdentity(storage, "bounded-device", "stored-A", "text-0", true, () => "00000000-0000-4000-8000-000000000016")).toBe("00000000-0000-4000-8000-000000000000");
 });
 it("migrates an unresolved v1 ID without losing it on edit and reload", async () => {
  const original = "00000000-0000-4000-8000-000000000021";
  const values = new Map([["remotai.hermes.admission.v1:legacy-device:stored-A", JSON.stringify({ id: original, text: "A", queued: true })]]);
  const storage = { getItem: (key: string) => values.get(key) ?? null, setItem: (key: string, value: string) => { values.set(key, value); } };
  control.submissionIdentity(storage, "legacy-device", "stored-A", "B", true);
  vi.resetModules(); const reloaded = await import("./control");
  expect(reloaded.submissionIdentity(storage, "legacy-device", "stored-A", "A", true)).toBe(original);
  reloaded.clearSubmissionIdentity(storage, "legacy-device", "stored-A", original);
  expect(storage.getItem("remotai.hermes.admission.v1:legacy-device:stored-A")).toBe("null");
 });
 it("keeps distinct unknown texts in memory when reads and writes are denied", () => {
  const storage = { getItem: () => { throw Error("blocked"); }, setItem: () => { throw Error("blocked"); } };
  const first = control.submissionIdentity(storage, "private-edits", "stored-A", "A", true);
  const second = control.submissionIdentity(storage, "private-edits", "stored-A", "B", true);
  control.clearSubmissionIdentity(storage, "private-edits", "stored-A", first);
  expect(control.submissionIdentity(storage, "private-edits", "stored-A", "B", true)).toBe(second);
  expect(control.submissionIdentity(storage, "private-edits", "stored-A", "A", true)).not.toBe(first);
 });
 it("admits idempotent tasks and pinned replies through authenticated device", async () => {
  const calls: Array<{path:string;body:unknown}> = [];
  const client = createHermesClient(() => ({ request: async <T>(path:string, init?:RequestInit) => {calls.push({path,body:init?.body?JSON.parse(String(init.body)):undefined});return {status:"accepted",run_id:"fixture-run"} as T;} })) as any;
  expect(typeof client.submitTask).toBe("function");
  await client.submitTask({client_request_id:"fixture-1",session_id:"live-1",stored_session_id:"stored-1",text:"fixture",queued:true});
  await client.controlReply("epoch:request:1",{choice:"once"});
  await client.controlSnapshot();
  expect(calls.map(call=>call.path)).toEqual(["/api/hermes/control/submit","/api/hermes/control/reply","/api/hermes/control/snapshot"]);
  expect(calls[0].body).toEqual({client_request_id:"fixture-1",session_id:"live-1",stored_session_id:"stored-1",text:"fixture",queued:true});
  expect(calls[1].body).toEqual({id:"epoch:request:1",result:{choice:"once"}});
 });
});
