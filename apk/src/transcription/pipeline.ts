import type { TranscriptionAPI, TranscriptionJob } from "@tgcontrol/shared";

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

/** No terminal input is sent here: the caller explicitly accepts a transcript. */
export async function transcribeAudio(file: File, api: TranscriptionAPI, options: {
  signal: AbortSignal; model: string; language: string;
  phase: (state: TranscriptionPhase, progress?: number) => void;
  pollMs?: number; id?: string;
}): Promise<TranscriptionJob> {
  const { signal } = options;
  check(signal);
  if (!file.size || file.size > MAX_AUDIO_BYTES) throw new Error("audio_size");
  options.phase("uploading", 0);
  const uploaded = await api.upload(file, progress => options.phase("uploading", progress), signal);
  check(signal);
  if (!uploaded.path) throw new Error("upload_incomplete");
  const id = options.id ?? recordingID();
  const abort = () => { void api.cancel(id).catch(() => {}); };
  signal.addEventListener("abort", abort, { once: true });
  let finished = false;
  try {
    check(signal);
    options.phase("recognizing");
    let job = await api.start({ path: uploaded.path, model: options.model, language: options.language, id }, signal);
    const deadline = Date.now() + 5 * 60_000 + 15_000;
    while (job.state === "running") {
      check(signal);
      if (Date.now() >= deadline) { abort(); throw new Error("recognition_timeout"); }
      await pause(options.pollMs ?? 800, signal);
      job = await api.job(id, signal);
    }
    check(signal);
    if (job.state === "failed") throw new Error(job.error || "recognition_failed");
    if (job.state === "cancelled") throw cancelled();
    if (job.state !== "done" || job.id !== id) throw new Error("invalid_recognition_result");
    finished = true;
    return job;
  } finally { signal.removeEventListener("abort", abort); if (!finished && !signal.aborted) abort(); }
}
