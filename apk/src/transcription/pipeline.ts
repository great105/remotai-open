import type { TranscriptionAPI, TranscriptionJob } from "@tgcontrol/shared";
import { withDeadline } from "./deadline";

export type TranscriptionPhase = "uploading" | "recognizing";
export const MAX_RECORDING_MS = 120_000;
export const MAX_AUDIO_BYTES = 25 * 1024 * 1024;

export const cancelled = () => new DOMException("Recording cancelled", "AbortError");
function check(signal: AbortSignal) { if (signal.aborted) throw cancelled(); }

function pause(ms: number, signal: AbortSignal): Promise<void> {
  return new Promise((resolve, reject) => {
    const abort = () => { clearTimeout(timer); signal.removeEventListener("abort", abort); reject(cancelled()); };
    const timer = setTimeout(() => { signal.removeEventListener("abort", abort); resolve(); }, ms);
    signal.addEventListener("abort", abort, { once: true });
    if (signal.aborted) abort();
  });
}

export function recordingID(): string {
  return Array.from(crypto.getRandomValues(new Uint8Array(16)), byte => byte.toString(16).padStart(2, "0")).join("");
}

/** Returns a transcript for the draft; never sends terminal input. */
export async function transcribeAudio(file: File, api: TranscriptionAPI, options: {
  signal: AbortSignal; model: string; language: string;
  phase: (state: TranscriptionPhase, progress?: number) => void;
  onJob?: (job: TranscriptionJob) => void;
  pollMs?: number; id?: string; requestMs?: number; timeoutMs?: number;
}): Promise<TranscriptionJob> {
  return withDeadline(options.signal, options.timeoutMs ?? 315_000, "recognition_timeout", signal => run(file, api, { ...options, signal }));
}

async function run(file: File, api: TranscriptionAPI, options: Parameters<typeof transcribeAudio>[2]): Promise<TranscriptionJob> {
  const { signal } = options;
  check(signal);
  if (!file.size || file.size > MAX_AUDIO_BYTES) throw new Error("audio_size");
  options.phase("uploading", 0);
  const uploaded = await withDeadline(signal, options.requestMs ?? 60_000, "connection_timeout", request => api.upload(file, progress => { if (!signal.aborted) options.phase("uploading", progress); }, request));
  check(signal);
  if (!uploaded.path) throw new Error("upload_incomplete");
  const id = options.id ?? recordingID();
  const abort = () => { void api.cancel(id).catch(() => {}); };
  signal.addEventListener("abort", abort, { once: true });
  let finished = false;
  try {
    check(signal);
    options.phase("recognizing");
    let job = await withDeadline(signal, options.requestMs ?? 30_000, "connection_timeout", request => api.start({ path: uploaded.path, model: options.model, language: options.language, id }, request));
    check(signal);
    if (job.id !== id) throw new Error("invalid_recognition_result");
    options.onJob?.(job);
    while (job.state === "running") {
      check(signal);
      await pause(options.pollMs ?? 800, signal);
      job = await withDeadline(signal, options.requestMs ?? 30_000, "connection_timeout", request => api.job(id, request));
      check(signal);
      if (job.id !== id) throw new Error("invalid_recognition_result");
      options.onJob?.(job);
    }
    check(signal);
    if (job.state === "failed") throw new Error(job.error || "recognition_failed");
    if (job.state === "cancelled") throw cancelled();
    if (job.state !== "done" || job.id !== id) throw new Error("invalid_recognition_result");
    finished = true;
    return job;
  } finally { signal.removeEventListener("abort", abort); if (!finished && !signal.aborted) abort(); }
}
