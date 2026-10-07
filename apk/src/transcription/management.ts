import type { LocalAction, LocalDetails, LocalPreferences, TranscriptionAPI, TranscriptionJob } from "@tgcontrol/shared";
import { recordingID, cancelled } from "./pipeline";

export async function runLocalAction(api: TranscriptionAPI, input: Omit<LocalAction, "id">, signal: AbortSignal, pollMs = 800, options: { id?: string; cancelOnAbort?: boolean } = {}): Promise<LocalDetails> {
  if (signal.aborted) throw cancelled();
  const id = options.id ?? recordingID();
  const abort = () => { if (options.cancelOnAbort !== false) void api.cancel(id).catch(() => {}); };
  signal.addEventListener("abort", abort, { once: true });
  let complete = false;
  try {
    const job = await api.action({ ...input, id }, signal);
    if (job.id !== id) throw new Error("invalid_recognition_result");
    const result = await followLocalAction(api, job, signal, pollMs);
    complete = true; return result;
  } finally { signal.removeEventListener("abort", abort); if (!complete && !signal.aborted) abort(); }
}

export async function followLocalAction(api: TranscriptionAPI, initial: TranscriptionJob, signal: AbortSignal, pollMs = 800): Promise<LocalDetails> {
    let job = initial; const id = job.id;
    const deadline = Date.now() + 30 * 60_000 + 15_000;
    while (job.state === "running") {
      if (signal.aborted) throw cancelled();
      if (Date.now() > deadline) throw new Error("recognition_timeout");
      await new Promise<void>((resolve, reject) => {
        const onAbort = () => { clearTimeout(timer); signal.removeEventListener("abort", onAbort); reject(cancelled()); };
        const timer = setTimeout(() => { signal.removeEventListener("abort", onAbort); resolve(); }, pollMs);
        signal.addEventListener("abort", onAbort, { once: true }); if (signal.aborted) onAbort();
      });
      job = await api.job(id, signal);
      if (job.id !== id) throw new Error("invalid_recognition_result");
    }
    if (signal.aborted || job.state === "cancelled") throw cancelled();
    if (job.id !== id || job.state !== "done" || !job.result?.models?.length) throw new Error(job.error || "invalid_recognition_result");
    return job.result;
}

export function dictionaryText(entries: LocalPreferences["dictionary"]): string { return entries.map(entry => `${entry.heard} => ${entry.written}`).join("\n"); }
export function parseDictionary(text: string): LocalPreferences["dictionary"] {
  const lines = text.split(/\r?\n/).map(line => line.trim()).filter(Boolean);
  if (lines.length > 100) throw new Error("dictionary_limit");
  return lines.map(line => {
    const at = line.indexOf("=>"), heard = line.slice(0, at).trim(), written = line.slice(at + 2).trim();
    if (at < 1 || !written || heard.length > 200 || written.length > 200) throw new Error("dictionary_format");
    return { heard, written };
  });
}
