import { transport } from "./api-transport";

export interface TranscriptionStatus {
  available: boolean;
  executable?: string;
  platform: string;
  max_bytes: number;
  active?: TranscriptionJob;
  module?: { supported: boolean; version: string; download_bytes: number; required_bytes: number; managed: boolean };
}
export interface TranscriptionJob {
  id: string;
  state: "running" | "done" | "failed" | "cancelled";
  text?: string;
  error?: string;
  duration?: number;
  device?: string;
  kind?: string;
  result?: LocalDetails;
  progress?: { stage: "downloading" | "verifying" | "extracting" | "checking" | "connecting" | "preparing" | "loading" | "recognizing" | "finishing"; completed_bytes: number; total_bytes: number; elapsed_seconds?: number };
}
export interface LocalModel { id: string; label: string; description?: string; engine?: string; disk_gb: number; vram_gb: number; ram_gb: number; cached: boolean; ready: boolean; supported: boolean; russian_only: boolean; gpu_only: boolean }
export interface LocalPreferences { model: string; language: string; dictionary: Array<{ heard: string; written: string }> }
export interface LocalDetails { management: boolean; module_version: string; models: LocalModel[]; settings: LocalPreferences; hardware: { name: string; has_nvidia: boolean; memory_mb: number; recommended_model: string } }
export interface LocalAction { id: string; operation: "inspect" | "install" | "save" | "module.install"; model?: string; settings?: LocalPreferences }
export type TranscriptionRequest = <T>(path: string, init?: RequestInit) => Promise<T>;
export interface TranscriptionAPI {
  status(signal?: AbortSignal): Promise<TranscriptionStatus>;
  connect(executable: string, signal?: AbortSignal): Promise<TranscriptionStatus>;
  upload(file: File, onProgress: (pct: number) => void, signal?: AbortSignal): Promise<{ path: string }>;
  start(input: { path: string; model: string; language: string; id: string }, signal?: AbortSignal): Promise<TranscriptionJob>;
  job(id: string, signal?: AbortSignal): Promise<TranscriptionJob>;
  cancel(id: string): Promise<unknown>;
  action(input: LocalAction, signal?: AbortSignal): Promise<TranscriptionJob>;
  updateAgent(signal?: AbortSignal): Promise<unknown>;
}

/** The supplied request is bound to one computer by the platform adapter. */
export function createTranscriptionAPI(
  request: TranscriptionRequest = (path, init) => transport().request(path, init),
  upload: TranscriptionAPI["upload"] = (file, onProgress, signal) => {
    const form = new FormData(); form.append("file", file, file.name);
    return transport().uploadForm("/api/pty/upload", form, onProgress, { signal, resumable: true });
  },
): TranscriptionAPI {
  return {
    status: signal => request("/api/transcription/status", { signal }),
    connect: (executable, signal) => request("/api/transcription/connection", { method: "POST", body: JSON.stringify({ executable }), signal }),
    upload,
    start: (input, signal) => request("/api/transcription/jobs", { method: "POST", body: JSON.stringify(input), signal }),
    job: (id, signal) => request(`/api/transcription/jobs/${encodeURIComponent(id)}`, { signal }),
    cancel: id => request(`/api/transcription/jobs/${encodeURIComponent(id)}`, { method: "DELETE" }),
    action: (input, signal) => request("/api/transcription/actions", { method: "POST", body: JSON.stringify(input), signal }),
    updateAgent: signal => request("/api/system/update", { method: "POST", body: JSON.stringify({}), signal }),
  };
}
