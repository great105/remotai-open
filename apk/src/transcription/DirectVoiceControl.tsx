import { useEffect, useMemo, useRef, useState, useSyncExternalStore } from "react";
import { useNavigate } from "react-router-dom";
import { captureTranscriptionAPI } from "../api";
import { getTerminalContextKey } from "../config";
import { onSelectedDeviceChange } from "../devices";
import { t } from "../i18n";
import { DirectVoice } from "./directVoice";
import { VoiceIcon } from "./VoiceInputSheet";
import "./direct-voice.css";

export function voiceError(error: unknown): string {
  const name = (error as Error)?.name;
  if (name === "NotAllowedError" || name === "SecurityError") return t("voice.permissionDenied");
  if (name === "NotFoundError") return t("voice.noMicrophone");
  const detail = error instanceof Error ? error.message : String(error);
  const keys: Record<string, string> = { voice_setup: "voice.setupHint", voice_model: "voice.modelNotReady", voice_empty: "voice.noSpeech", recording_failed: "voice.recordingFailed", connection_timeout: "voice.connectionTimeout", recognition_timeout: "voice.timeout", audio_size: "voice.fileSize", upload_incomplete: "voice.uploadIncomplete" };
  if (keys[detail]) return t(keys[detail]);
  if (!navigator.mediaDevices?.getUserMedia && /Микрофон/.test(detail)) return t("voice.microphoneUnavailable");
  return detail;
}

export function useDirectVoice(owner: string, enabled: boolean, onInsert: (text: string) => void) {
  const navigate = useNavigate();
  const latest = useRef({ owner, onInsert }); latest.current = { owner, onInsert };
  const voice = useMemo(() => {
    const computer = getTerminalContextKey();
    return new DirectVoice(captureTranscriptionAPI(), text => {
      if (latest.current.owner === owner && getTerminalContextKey() === computer) latest.current.onInsert(text);
    });
  }, [owner]);
  const state = useSyncExternalStore(voice.subscribe, voice.snapshot);
  const [now, setNow] = useState(Date.now());
  useEffect(() => {
    const computer = getTerminalContextKey();
    const off = onSelectedDeviceChange(() => { if (getTerminalContextKey() !== computer) voice.cancel(); });
    return () => { off(); voice.cancel(); };
  }, [voice]);
  useEffect(() => { if (!enabled) voice.cancel(); }, [voice, enabled]);
  useEffect(() => {
    if (state.phase === "idle" || state.phase === "error") return;
    setNow(Date.now());
    const timer = setInterval(() => setNow(Date.now()), 1000);
    return () => clearInterval(timer);
  }, [state.phase]);
  const recording = state.phase === "recording";
  const busy = !["idle", "error", "recording"].includes(state.phase);
  const label = recording ? t("voice.stop") : t("voice.record");
  const seconds = Math.max(0, Math.floor((now - state.since) / 1000));
  const time = `${Math.floor(seconds / 60)}:${String(seconds % 60).padStart(2, "0")}`;
  const stages: Record<string, string> = { preparing: "voice.preparing", loading: "voice.loading", recognizing: "voice.recognizing", finishing: "voice.finishing" };
  const status = state.phase === "permission" ? t("voice.permission") : recording ? t("voice.recording") : state.phase === "checking" ? t("voice.checking") : state.phase === "uploading" ? `${t("voice.uploading")} ${Math.round(state.percent ?? 0)}%` : t(stages[state.stage ?? ""] ?? "voice.recognizing");
  const setup = ["voice_setup", "voice_model"].includes((state.error as Error)?.message);
  return {
    button: <button type="button" className={`pty-key-btn pty-voice-btn${recording ? " pty-voice-recording" : ""}`} title={label} aria-label={label} aria-pressed={recording} disabled={!enabled || busy} onClick={() => { void (recording ? voice.finish() : voice.start()); }}>
      {recording ? <span className="pty-voice-stop" aria-hidden="true" /> : <VoiceIcon />}
    </button>,
    status: state.phase !== "idle" && <div className={`pty-voice-status${state.phase === "error" ? " pty-voice-error" : ""}`}>
      <span role={state.phase === "error" ? "alert" : "status"}>{state.phase === "error" ? voiceError(state.error) : status}</span>
      {state.phase !== "error" && <time aria-label={t("voice.elapsed")}>{time}</time>}
      <div className="pty-voice-actions">
        {state.retry && <button type="button" onClick={() => { void voice.retry(); }}>{t("voice.retry")}</button>}
        {setup && <button type="button" onClick={() => { voice.cancel(); navigate("/settings?section=local"); }}>{t("voice.openSettings")}</button>}
        <button type="button" onClick={voice.cancel}>{state.phase === "error" ? t("voice.close") : t("voice.cancel")}</button>
      </div>
    </div>,
  };
}
