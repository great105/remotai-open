import { t } from "@tgcontrol/shared";
export interface CommandAvailability { version:number; without_arguments:string[] }
/** HTTP policy is authoritative; upstream desktop command hints are not permission. */
export function availableReadCommand(metadata: CommandAvailability | null | undefined, command: string): boolean {
 return metadata?.version === 1 && Array.isArray(metadata.without_arguments) && metadata.without_arguments.includes(command.replace(/^\//, ""));
}
export interface HermesControlLink {device:string;session:string;attention:string;matches:boolean}
/** A link is routing input, never authorization; foreign devices require authorized inventory. */
export function controlDeepLink(hash:string,selected:string,mode:string):HermesControlLink|null {
 const query=hash.includes("?")?hash.slice(hash.indexOf("?")+1):"";const p=new URLSearchParams(query);
 const device=p.get("device")||"",session=p.get("session")||"",attention=p.get("attention")||"";
 if(!device || !session || !/^[A-Za-z0-9_-]{1,256}$/.test(device)||! /^[A-Za-z0-9_.:-]{1,256}$/.test(session)||session.includes("..")||attention.length>512)return null;
 return {device,session,attention,matches:mode==="cloud"&&device===selected};
}
export interface ControlTask {
 client_request_id:string; run_id:string; session_id:string; stored_session_id:string; generation:number;
 state:"accepted"|"running"|"waiting_user"|"completed"|"failed"|"interrupted"; native_status?:string; created_at:string;
}
export interface ControlAttention {
 id:string; request_id:string; session_id:string; stored_session_id:string; run_id:string; generation:number;
 kind:string; method:string; state:"pending"|"consuming"|"answered"|"expired"|"uncertain"|"unread"|"read";
 params?:Record<string,unknown>; delivery?:string;
}
export interface ControlResult {
 id:string; run_id:string; session_id:string; stored_session_id:string; generation:number; event_seq:number;
 tool_id:string; tool:string;kind:string;verified:boolean;outcome:string;path?:string;sha256?:string;preview?:string;diff?:string;exit_code?:number;
}
export interface ControlSnapshot {tasks:ControlTask[];attention:ControlAttention[];results:ControlResult[];epoch:number}
export interface ControlCapabilities {
 connected:boolean;generation:number;source:string;steer:boolean;interrupt:boolean;queue:boolean;methods:Record<string,boolean>;
 model_quota:"unknown";tools:string;scheduler:string;workdir:string;
}
export interface SubmitTask {client_request_id:string;session_id:string;stored_session_id:string;text:string;queued?:boolean;generation?:number}
export interface TaskReceipt {status:string;native_status?:string;run_id:string;client_request_id:string}

type AdmissionStorage = { getItem(key: string): string | null; setItem(key: string, value: string): void };
interface AdmissionIdentity { id: string; session: string; text: string; queued: boolean }
const volatileAdmissions = new Map<string, AdmissionIdentity[]>();
const confirmedAdmissions = new Map<string, Set<string>>();
const admissionKey = (device: string) => `remotai.hermes.admission.v2:${device}`;
const admissionLimit = 128;
const admissionBytesLimit = 1024 * 1024;

function readAdmissions(storage: AdmissionStorage | null, device: string, session: string): AdmissionIdentity[] {
 const records = [...(volatileAdmissions.get(device) || [])];
 const confirmed = confirmedAdmissions.get(device);
 const append = (record: AdmissionIdentity) => {
  if (record && typeof record.id === "string" && typeof record.text === "string" && typeof record.session === "string"
   && typeof record.queued === "boolean" && !confirmed?.has(record.id) && !records.some(item => item.id === record.id)) records.push(record);
 };
 try {
  const saved = JSON.parse(storage?.getItem(admissionKey(device)) || "null");
  if (Array.isArray(saved)) saved.forEach(append);
  // Upgrade the previous single-slot format without changing its wire ID.
  const legacy = JSON.parse(storage?.getItem(`remotai.hermes.admission.v1:${device}:${session}`) || "null");
  if (legacy) append({ ...legacy, session });
 } catch { /* Storage denied: keep every unresolved identity in this tab. */ }
 volatileAdmissions.set(device, records);
 return records;
}
function persistAdmissions(storage: AdmissionStorage | null, device: string, records: AdmissionIdentity[]) {
 volatileAdmissions.set(device, records);
 try { storage?.setItem(admissionKey(device), JSON.stringify(records)); } catch { /* Same-tab retry remains safe; reload durability is unavailable. */ }
}
/** Normal task drafts only: secret replies never enter this lane. Unknown outcomes
 * are keyed by machine/chat/text/queue intent, NOT the currently edited draft.
 * Never evict an unresolved ID to satisfy the bound: refuse a new fingerprint. */
export function submissionIdentity(storage: AdmissionStorage | null, device: string, session: string, text: string, queued: boolean, create = () => crypto.randomUUID()): string {
 const records = readAdmissions(storage, device, session);
 const previous = records.find(item => item.session === session && item.text === text && item.queued === queued);
 if (previous) return previous.id;
 const record = { id: create(), session, text, queued };
 if (records.length >= admissionLimit || JSON.stringify([...records, record]).length > admissionBytesLimit) {
  throw Object.assign(new Error(t("ui.control.m2951aa7e4e")), { code: "hermes_backend_error" });
 }
 persistAdmissions(storage, device, [...records, record]);
 return record.id;
}
/** A receipt may release only its exact fingerprint, never the whole chat slot. */
export function clearSubmissionIdentity(storage: AdmissionStorage | null, device: string, session: string, requestId?: string): void {
 if (!requestId) return;
 const records = readAdmissions(storage, device, session);
 if (!records.some(item => item.session === session && item.id === requestId)) return;
 const confirmed = confirmedAdmissions.get(device) || new Set<string>();
 confirmed.add(requestId);
 // Confirmed tombstones may be bounded; unresolved records may not.
 if (confirmed.size > admissionLimit) confirmed.delete(confirmed.values().next().value!);
 confirmedAdmissions.set(device, confirmed);
 persistAdmissions(storage, device, records.filter(item => item.session !== session || item.id !== requestId));
 try {
  const key = `remotai.hermes.admission.v1:${device}:${session}`;
  const legacy = JSON.parse(storage?.getItem(key) || "null");
  if (legacy?.id === requestId) storage?.setItem(key, "null");
 } catch { /* Confirmed tombstone prevents stale storage resurrecting this tab's ID. */ }
}
