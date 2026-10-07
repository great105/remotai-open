import { useEffect, useId, useRef, useState } from "react";
import { useNavigate } from "react-router-dom";
import { FolderNavSheet, SheetShell, useEscape, type TranscriptionStatus } from "@tgcontrol/shared";
import { captureTranscriptionAPI } from "../api";
import { getTerminalContextKey } from "../config";
import { onSelectedDeviceChange } from "../devices";
import { IconClose, IconFolder } from "../components/icons";
import { t } from "../i18n";
import { startPhoneRecording, type PhoneRecording } from "./phoneAudio";
import { MAX_RECORDING_MS, MAX_AUDIO_BYTES, transcribeAudio } from "./pipeline";
import "./voice.css";
import { runLocalAction } from "./management";
import type { LocalDetails } from "@tgcontrol/shared";

type Phase = "checking" | "idle" | "permission" | "recording" | "uploading" | "recognizing" | "done";

export function VoiceInputSheet({ onClose, onInsert, acceptLabel }: { onClose: () => void; onInsert: (text: string) => void; acceptLabel?: string }) {
  const navigate = useNavigate();
  const heading = useId();
  const [api] = useState(captureTranscriptionAPI);
  const [scope] = useState(getTerminalContextKey);
  const [status, setStatus] = useState<TranscriptionStatus | null>(null);
  const [details, setDetails] = useState<LocalDetails | null>(null);
  const [phase, setPhase] = useState<Phase>("checking");
  const [error, setError] = useState("");
  const [path, setPath] = useState("");
  const [folderOpen, setFolderOpen] = useState(false);
  const [connecting, setConnecting] = useState(false);
  const [model, setModel] = useState("medium");
  const [language, setLanguage] = useState("ru");
  const [seconds, setSeconds] = useState(0);
  const [progress, setProgress] = useState(0);
  const [text, setText] = useState("");
  const [fileName, setFileName] = useState("");
  const input = useRef<HTMLInputElement>(null);
  const recording = useRef<PhoneRecording | null>(null);
  const controller = useRef<AbortController | null>(null);
  const revision = useRef(0);
  const mounted = useRef(true);
  const finishRef = useRef<() => void>(() => {});
  const busy = ["permission", "recording", "uploading", "recognizing"].includes(phase);

  const stop = () => { revision.current++; controller.current?.abort(); recording.current?.cancel(); recording.current = null; };
  const close = () => { stop(); onClose(); };
  async function loadState(signal: AbortSignal) {
    const value = await api.status(signal);
    if (!mounted.current || signal.aborted) return;
    setStatus(value); setPath(value.executable ?? "");
    if (value.available) {
      const info = await runLocalAction(api, { operation: "inspect" }, signal);
      if (!mounted.current || signal.aborted) return;
      setDetails(info); setModel(info.settings.model); setLanguage(info.settings.language);
    }
    setPhase("idle");
  }
  useEscape(!folderOpen, close);
  useEffect(() => {
    mounted.current = true;
    const abort = new AbortController(); controller.current = abort;
    void loadState(abort.signal).catch(err => { if (!abort.signal.aborted && mounted.current) { setError(message(err)); setPhase("idle"); } });
    const off = onSelectedDeviceChange(() => { if (getTerminalContextKey() !== scope) close(); });
    return () => { mounted.current = false; off(); stop(); };
  }, [api, scope]);

  useEffect(() => {
    if (phase !== "recording") return;
    const start = Date.now();
    const tick = setInterval(() => setSeconds(Math.floor((Date.now() - start) / 1000)), 1000);
    const limit = setTimeout(() => finishRef.current(), MAX_RECORDING_MS);
    return () => { clearInterval(tick); clearTimeout(limit); };
  }, [phase]);

  function message(err: unknown): string {
    const name = (err as Error)?.name;
    if (name === "NotAllowedError" || name === "SecurityError") return t("voice.permissionDenied");
    if (name === "NotFoundError") return t("voice.noMicrophone");
    const detail = err instanceof Error ? err.message : String(err);
    if (detail === "audio_size") return t("voice.fileSize");
    if (detail === "upload_incomplete") return t("voice.uploadIncomplete");
    if (detail === "recognition_timeout") return t("voice.timeout");
    if (!navigator.mediaDevices?.getUserMedia && /Микрофон/.test(detail)) return t("voice.microphoneUnavailable");
    return detail;
  }

  async function recognize(file: File, abort: AbortController, observed: number) {
    if (abort.signal.aborted || observed !== revision.current) return;
    setFileName(file.name);
    try {
      const result = await transcribeAudio(file, api, {
        signal: abort.signal, model, language,
        phase: (next, percent) => { if (mounted.current && observed === revision.current) { setPhase(next); setProgress(percent ?? 0); } },
      });
      if (!mounted.current || abort.signal.aborted || observed !== revision.current || scope !== getTerminalContextKey()) return;
      setText(result.text ?? ""); setPhase("done");
      if (!result.text?.trim()) setError(t("voice.noSpeech"));
    } catch (err) {
      if (mounted.current && !abort.signal.aborted && observed === revision.current) { setError(message(err)); setPhase("idle"); }
    }
  }

  async function start() {
    stop(); const observed = revision.current;
    const abort = new AbortController(); controller.current = abort;
    setError(""); setText(""); setSeconds(0); setPhase("permission");
    try {
      const active = await startPhoneRecording({ signal: abort.signal });
      if (!mounted.current || abort.signal.aborted || observed !== revision.current) { active.cancel(); return; }
      recording.current = active; setPhase("recording");
    } catch (err) {
      if (mounted.current && !abort.signal.aborted && observed === revision.current) { setError(message(err)); setPhase("idle"); }
    }
  }

  async function finish() {
    const active = recording.current, abort = controller.current, observed = revision.current;
    if (!active || !abort || abort.signal.aborted) return;
    recording.current = null; setPhase("uploading");
    try { await recognize(await active.stop(), abort, observed); }
    catch (err) { if (mounted.current && !abort.signal.aborted && observed === revision.current) { setError(message(err)); setPhase("idle"); } }
  }
  finishRef.current = () => { void finish(); };

  async function pick(file?: File) {
    if (!file) return;
    stop(); const observed = revision.current;
    const abort = new AbortController(); controller.current = abort;
    setError(""); setText("");
    if (!file.size || file.size > MAX_AUDIO_BYTES) { setError(t("voice.fileSize")); return; }
    await recognize(file, abort, observed);
  }

  async function connect() {
    const abort = new AbortController(); controller.current = abort;
    setError(""); setConnecting(true);
    try {
      await api.connect(path, abort.signal);
      if (!mounted.current || abort.signal.aborted) return;
      await loadState(abort.signal);
    } catch (err) { if (mounted.current && !abort.signal.aborted) setError(message(err)); }
    finally { if (mounted.current) setConnecting(false); }
  }

  return <SheetShell open onClose={close} labelledBy={heading} className="folder-sheet voice-sheet" overlayClassName="folder-sheet-overlay">
    <div className="folder-sheet-header"><strong id={heading}>{t("voice.title")}</strong><button type="button" className="folder-sheet-close" aria-label={t("voice.close")} onClick={close}><IconClose size={20} /></button></div>
    <div className="voice-body">
      <p className="voice-hint">{t("voice.intro")}</p>
      {phase === "checking" && <p role="status">{t("voice.checking")}</p>}
      {status && !status.available && <div className="voice-setup">
        <p>{status.platform === "windows" ? t("voice.setup") : t("voice.windowsOnly")}</p>
        {status.platform === "windows" && <><label htmlFor={`${heading}-path`}>{t("voice.modulePath")}</label>
          <input id={`${heading}-path`} value={path} onChange={event => setPath(event.target.value)} placeholder="C:\…\AgentDeskBridge.exe" autoComplete="off" spellCheck={false} />
          <div className="voice-actions"><button type="button" className="btn btn-secondary" onClick={() => setFolderOpen(true)}><IconFolder size={18} />{t("voice.chooseFolder")}</button><button type="button" className="btn btn-primary" disabled={!path.trim() || connecting} onClick={() => void connect()}>{connecting ? t("voice.connecting") : t("voice.connect")}</button></div></>}
      </div>}
      {status?.available && <>
        <div className="voice-settings"><label>{t("voice.model")}<select value={model} disabled={busy || !details} onChange={event => setModel(event.target.value)}>{details?.models.map(item => <option key={item.id} value={item.id} disabled={details.management && (!item.ready || !item.supported)}>{item.label}</option>)}</select></label><label>{t("voice.language")}<select value={language} disabled={busy} onChange={event => setLanguage(event.target.value)}><option value="ru">Русский</option><option value="en" disabled={details?.models.find(item => item.id === model)?.russian_only}>English</option><option value="auto">{t("voice.autoLanguage")}</option></select></label></div>
        <p className="voice-hint">{t("voice.modelHint")}</p>
        {!busy && phase !== "done" && <div className="voice-actions"><button type="button" className="btn btn-primary" disabled={!details || (details.management && !details.models.find(item => item.id === model)?.ready)} onClick={() => void start()}><VoiceIcon />{t("voice.record")}</button><button type="button" className="btn btn-secondary" disabled={!details || (details.management && !details.models.find(item => item.id === model)?.ready)} onClick={() => input.current?.click()}>{t("voice.chooseAudio")}</button></div>}
        {busy && <div className="voice-progress" role="status"><p>{phase === "permission" ? t("voice.permission") : phase === "recording" ? `${t("voice.recording")} ${Math.floor(seconds / 60)}:${String(seconds % 60).padStart(2, "0")}` : phase === "uploading" ? `${t("voice.uploading")} ${Math.round(progress)}%` : t("voice.recognizing")}</p><div className="voice-actions">{phase === "recording" && <button type="button" className="btn btn-primary" onClick={() => void finish()}>{t("voice.stop")}</button>}<button type="button" className="btn btn-secondary" onClick={() => { stop(); setPhase("idle"); }}>{t("voice.cancel")}</button></div></div>}
        {phase === "done" && <><label htmlFor={`${heading}-result`}>{t("voice.result")}</label><textarea id={`${heading}-result`} className="voice-result" value={text} onChange={event => setText(event.target.value)} rows={6} /><div className="voice-actions"><button type="button" className="btn btn-primary" disabled={!text.trim()} onClick={() => { if (scope === getTerminalContextKey()) { onInsert(text.trim()); close(); } }}>{acceptLabel ?? t("voice.insert")}</button><button type="button" className="btn btn-secondary" onClick={() => { setPhase("idle"); setError(""); }}>{t("voice.again")}</button></div></>}
        {fileName && <small className="voice-filename" title={fileName}>{fileName}</small>}
        <details className="voice-connection"><summary>{t("voice.connection")}</summary><p className="voice-path">{status.executable}</p><button type="button" className="btn btn-secondary" disabled={busy} onClick={() => setStatus({ ...status, available: false })}>{t("voice.changeModule")}</button></details>
      </>}
      {error && <p className="voice-error" role="alert">{error}</p>}
      {(!status || (status.available && !details)) && phase !== "checking" && <button type="button" className="btn btn-secondary" onClick={() => { const abort=new AbortController(); controller.current=abort; setPhase("checking"); void loadState(abort.signal).catch(err => { if (mounted.current && !abort.signal.aborted) { setError(message(err)); setPhase("idle"); } }); }}>{t("voice.retry")}</button>}
      {!acceptLabel && <button type="button" className="btn btn-secondary" disabled={busy} onClick={() => { close(); navigate("/settings?section=local"); }}>{t("local.manage")}</button>}
      <input ref={input} type="file" disabled={busy || !details || (details.management && !details.models.find(item => item.id === model)?.ready)} accept="audio/*,video/*,.ogg,.opus,.webm,.m4a,.mp3,.wav,.flac" hidden onChange={event => { const file = event.target.files?.[0]; event.target.value = ""; void pick(file); }} />
    </div>
    <FolderNavSheet open={folderOpen} onClose={() => setFolderOpen(false)} initialTab="browse" currentCwd={path.replace(/[\\/][^\\/]*$/, "")} title={t("voice.chooseFolder")} pickLabel={t("voice.chooseFolder")} onPick={folder => { setPath(`${folder.replace(/[\\/]+$/, "")}\\AgentDeskBridge.exe`); setFolderOpen(false); }} />
  </SheetShell>;
}

export function VoiceIcon() {
  return <svg width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.8" aria-hidden="true"><rect x="9" y="2" width="6" height="12" rx="3" /><path d="M5 10v2a7 7 0 0 0 14 0v-2M12 19v3M8 22h8" /></svg>;
}
