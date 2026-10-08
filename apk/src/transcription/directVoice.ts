import type { TranscriptionAPI, TranscriptionJob } from "@tgcontrol/shared";
import { startPhoneRecording, type PhoneRecording } from "./phoneAudio";
import { cancelled, MAX_RECORDING_MS, transcribeAudio } from "./pipeline";
import { withDeadline } from "./deadline";
import { runLocalAction } from "./management";

export type VoicePhase = "idle" | "permission" | "recording" | "checking" | "uploading" | "recognizing" | "error";
export interface VoiceState { phase: VoicePhase; since: number; percent?: number; stage?: string; error?: unknown; retry: boolean; }
type Preferences = { model: string; language: string };

/** Owns exactly one recording on one captured computer and terminal draft. */
export class DirectVoice {
  private state: VoiceState = { phase: "idle", since: 0, retry: false };
  private listeners = new Set<() => void>();
  private controller?: AbortController;
  private recording?: PhoneRecording;
  private audio?: File;
  private preferences?: Promise<Preferences>;
  private limit?: ReturnType<typeof setTimeout>;
  constructor(private api: TranscriptionAPI, private insert: (text: string) => void,
    private capture: typeof startPhoneRecording = startPhoneRecording, private setupMs = 45_000) {}
  snapshot = () => this.state;
  subscribe = (listener: () => void) => { this.listeners.add(listener); return () => { this.listeners.delete(listener); }; };
  private update(state: Partial<VoiceState>) { this.state = { ...this.state, ...state }; this.listeners.forEach(fn => fn()); }
  cancel = () => {
    clearTimeout(this.limit); this.controller?.abort(); this.recording?.cancel();
    this.controller = undefined; this.recording = undefined; this.audio = undefined; this.preferences = undefined;
    this.update({ phase: "idle", since: 0, error: undefined, retry: false });
  };
  private current(abort: AbortController) { return this.controller === abort && !abort.signal.aborted; }
  private prepare(signal: AbortSignal): Promise<Preferences> {
    return withDeadline(signal, this.setupMs, "connection_timeout", async request => {
      const status = await this.api.status(request);
      if (!status.available) throw new Error("voice_setup");
      const details = await runLocalAction(this.api, { operation: "inspect" }, request);
      const model = details.models.find(item => item.id === details.settings.model);
      if (details.management && (!model?.ready || !model.supported)) throw new Error("voice_model");
      return { model: details.settings.model, language: details.settings.language };
    });
  }
  private fail(error: unknown, abort: AbortController) {
    if (!this.current(abort)) return;
    clearTimeout(this.limit); this.recording?.cancel(); this.recording = undefined;
    this.update({ phase: "error", error, retry: !!this.audio });
    abort.abort();
  }
  start = async () => {
    if (!["idle", "error"].includes(this.state.phase)) return;
    this.cancel();
    const abort = new AbortController(); this.controller = abort;
    this.update({ phase: "permission", since: Date.now() });
    // Called inside the tap, before any network request or module inspection.
    const microphone = this.capture({ signal: abort.signal });
    this.preferences = this.prepare(abort.signal);
    void this.preferences.catch(error => this.fail(error, abort));
    try {
      const recording = await microphone;
      if (!this.current(abort) || this.state.phase === "error") { recording.cancel(); return; }
      this.recording = recording;
      this.update({ phase: "recording", since: Date.now() });
      this.limit = setTimeout(() => { void this.finish(); }, MAX_RECORDING_MS);
    } catch (error) { this.fail(error, abort); }
  };
  finish = async () => {
    const recording = this.recording, abort = this.controller;
    if (!recording || !abort || !this.current(abort)) return;
    clearTimeout(this.limit); this.recording = undefined;
    this.update({ phase: "checking", since: Date.now() });
    try {
      const audio = await withDeadline(abort.signal, 10_000, "recording_failed", () => recording.stop());
      if (!this.current(abort)) return;
      this.audio = audio;
      await this.recognize(abort);
    } catch (error) { this.fail(error, abort); }
    finally { if (abort.signal.aborted) recording.cancel(); }
  };
  retry = async () => {
    if (this.state.phase !== "error" || !this.audio) return;
    this.controller?.abort();
    const abort = new AbortController(); this.controller = abort;
    this.update({ phase: "checking", since: Date.now(), error: undefined, retry: false });
    this.preferences = this.prepare(abort.signal);
    try { await this.recognize(abort); } catch (error) { this.fail(error, abort); }
  };
  private async recognize(abort: AbortController) {
    const preferences = await this.preferences;
    if (!preferences || !this.audio || !this.current(abort)) throw cancelled();
    const result = await transcribeAudio(this.audio, this.api, { signal: abort.signal, ...preferences,
      phase: (phase, percent) => { if (this.current(abort)) this.update({ phase, percent, stage: undefined }); },
      onJob: (job: TranscriptionJob) => { if (this.current(abort)) this.update({ stage: job.progress?.stage }); },
    });
    if (!this.current(abort)) return;
    if (!result.text?.trim()) throw new Error("voice_empty");
    this.insert(result.text.trim());
    this.cancel();
  }
}
