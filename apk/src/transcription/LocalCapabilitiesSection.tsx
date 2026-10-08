import { useEffect, useRef, useState } from "react";
import { FolderNavSheet, type LocalDetails, type LocalPreferences, type TranscriptionStatus } from "@tgcontrol/shared";
import { captureTranscriptionAPI } from "../api";
import { getTerminalContextKey } from "../config";
import { onSelectedDeviceChange } from "../devices";
import { t } from "../i18n";
import { VoiceInputSheet } from "./VoiceInputSheet";
import { dictionaryText, parseDictionary, runLocalAction, followLocalAction } from "./management";
import { recordingID } from "./pipeline";
import "./local-capabilities.css";

export function LocalCapabilitiesSection({ machineName }: { machineName: string }) {
  const [api, setAPI] = useState(captureTranscriptionAPI);
  const [status, setStatus] = useState<TranscriptionStatus | null>(null);
  const [details, setDetails] = useState<LocalDetails | null>(null);
  const [phase, setPhase] = useState("checking");
  const [error, setError] = useState("");
  const [path, setPath] = useState("");
  const [folderOpen, setFolderOpen] = useState(false);
  const [language, setLanguage] = useState("ru");
  const [dictionary, setDictionary] = useState("");
  const [testOpen, setTestOpen] = useState(false);
  const [testText, setTestText] = useState("");
  const request = useRef<AbortController | null>(null);
  const task = useRef("");
  const scope = useRef(getTerminalContextKey());
  const busy = phase !== "idle";

  function apply(value: LocalDetails) { setDetails(value); setLanguage(value.settings.language); setDictionary(dictionaryText(value.settings.dictionary ?? [])); }
  function failure(err: unknown) {
    const text = err instanceof Error ? err.message : String(err);
    setError(text === "dictionary_format" || text === "dictionary_limit" ? t(`local.${text}`) : text);
  }
  async function load() {
    request.current?.abort(); const abort = new AbortController(); request.current = abort;
    setError(""); setPhase("checking");
    try {
      const value = await api.status(abort.signal); if (abort.signal.aborted) return;
      setStatus(value); setPath(value.executable ?? "");
      if (value.available) {
        if (value.active?.kind) {
          task.current = value.active.id; setPhase(value.active.kind);
          apply(await followLocalAction(api, value.active, abort.signal));
        } else {
          task.current = recordingID();
          apply(await runLocalAction(api, { operation: "inspect" }, abort.signal, 800, { id: task.current, cancelOnAbort: false }));
        }
      }
    } catch (err) { if (!abort.signal.aborted) failure(err); }
    finally { if (!abort.signal.aborted) { task.current = ""; setPhase("idle"); } }
  }
  useEffect(() => {
    void load();
    const off = onSelectedDeviceChange(() => {
      if (scope.current === getTerminalContextKey()) return;
      request.current?.abort(); scope.current = getTerminalContextKey();
      setDetails(null); setStatus(null); setTestOpen(false); setTestText(""); setAPI(() => captureTranscriptionAPI());
    });
    return () => { off(); request.current?.abort(); };
  }, [api]);

  async function action(operation: "install" | "save", model?: string) {
    const abort = new AbortController(); request.current?.abort(); request.current = abort;
    setError(""); task.current = recordingID();
    try {
      let settings: LocalPreferences | undefined;
      if (operation === "save") {
        const selected = details?.models.find(item => item.id === (model ?? details.settings.model));
        settings = { model: model ?? details!.settings.model, language: selected?.russian_only && language === "en" ? "ru" : language, dictionary: parseDictionary(dictionary) };
      }
      setPhase(operation);
      apply(await runLocalAction(api, { operation, model, settings }, abort.signal, 800, { id: task.current, cancelOnAbort: false }));
    } catch (err) { if (!abort.signal.aborted) failure(err); }
    finally { if (!abort.signal.aborted) { task.current = ""; setPhase("idle"); } }
  }
  async function connect() {
    setError(""); setPhase("connecting");
    const abort = new AbortController(); request.current?.abort(); request.current = abort;
    try { await api.connect(path, abort.signal); if (!abort.signal.aborted) await load(); }
    catch (err) { if (!abort.signal.aborted) { failure(err); setPhase("idle"); } }
  }
  async function cancel() {
    const id = task.current, taskScope = scope.current; request.current?.abort();
    if (id) { try { await api.cancel(id); } catch (err) { if (scope.current === taskScope) failure(err); } }
    if (scope.current !== taskScope) return;
    task.current = ""; setPhase("idle");
  }
  const selected = details?.models.find(model => model.id === details.settings.model);
  return <div className="local-capabilities">
    <p className="local-machine">{machineName}</p>
    <p>{t("local.intro")}</p>
    {error && <p role="alert" className="local-error">{error}</p>}
    {busy && <div className="local-task" role="status"><span>{t(phase === "install" ? "local.installing" : phase === "save" ? "local.saving" : "local.checking")}</span>{task.current && <button type="button" className="btn btn-secondary" onClick={() => void cancel()}>{t("voice.cancel")}</button>}</div>}
    <section className="local-section" aria-labelledby="local-module-title">
      <h2 id="local-module-title">{t("local.module")}</h2>
      {status?.platform !== "windows" && status && <p className="local-note">{t("voice.windowsOnly")}</p>}
      <p>{t(status?.available ? "local.connected" : "local.notConnected")}{details?.module_version && ` · ${details.module_version}`}</p>
      {details?.hardware.name && <p className="local-secondary">{details.hardware.name}{details.hardware.memory_mb > 0 && ` · ${(details.hardware.memory_mb / 1024).toFixed(1)} GB`}</p>}
      {details && !details.management && <p className="local-note">{t("local.legacy")}</p>}
      <details className="local-connection" open={status?.available === false || undefined}>
        <summary>{t(status?.available ? "local.updateModule" : "voice.setup")}</summary>
        <p className="local-secondary">{t("local.connectHint")}</p>
        {status?.platform === "windows" && <a className="btn btn-secondary local-download" href="https://github.com/great105/remotai-open/releases/download/v2.75.17/remotai-local-windows-amd64.zip" target="_blank" rel="noopener noreferrer">{t("local.downloadModule")}</a>}
        <label>{t("voice.modulePath")}<input value={path} onChange={event => setPath(event.target.value)} disabled={busy} spellCheck={false} placeholder="C:\AgentDesk\AgentDeskBridge.exe" /></label>
        <div className="local-actions"><button type="button" className="btn btn-secondary" disabled={busy} onClick={() => setFolderOpen(true)}>{t("voice.chooseFolder")}</button><button type="button" className="btn btn-primary" disabled={busy || !path.trim()} onClick={() => void connect()}>{t("voice.connect")}</button></div>
      </details>
      <button type="button" className="btn btn-secondary" disabled={busy} onClick={() => void load()}>{t("local.refresh")}</button>
    </section>
    {details && <>
      <section className="local-section" aria-labelledby="local-models-title"><h2 id="local-models-title">{t("local.models")}</h2>
        <p className="local-secondary">{t("local.downloadHint")}</p>
        <ul className="local-model-list">{details.models.map(model => <li key={model.id} data-local-model={model.id}>
          <div className="local-model-copy"><h3>{model.label}{model.id === details.hardware.recommended_model && <span className="local-model-tag">{t("local.recommended")}</span>}</h3>
            {model.description && <p>{model.description}</p>}
            <small>{model.disk_gb > 0 && `${t("local.disk")} ${model.disk_gb} GB · `}{model.gpu_only ? t("local.gpuRequired") : t("local.cpuAvailable")}{model.russian_only && ` · ${t("local.russianOnly")}`}</small>
            {details.management && <small>{t(model.ready ? "local.ready" : model.cached ? "local.engineMissing" : "local.notDownloaded")}</small>}
          </div>
          {details.management && (model.id === details.settings.model && model.ready ? <span className="local-active-model">{t("local.active")}</span> : <button type="button" className={`btn ${model.ready ? "btn-secondary" : "btn-primary"}`} disabled={busy || !model.supported} onClick={() => void action(model.ready ? "save" : "install", model.id)}>{t(model.ready ? "local.use" : model.cached ? "local.installEngine" : "local.download")}</button>)}
        </li>)}</ul>
      </section>
      <section className="local-section" aria-labelledby="local-preferences-title"><h2 id="local-preferences-title">{t("local.preferences")}</h2>
        <p>{t("local.currentModel")}: {selected?.label ?? details.settings.model}</p>
        <label>{t("voice.language")}<select value={language} disabled={busy || !details.management} onChange={event => setLanguage(event.target.value)}><option value="ru">Русский</option><option value="en" disabled={selected?.russian_only}>English</option><option value="auto">{t("voice.autoLanguage")}</option></select></label>
        {details.management && <details className="local-dictionary"><summary>{t("local.dictionary")}</summary><p className="local-secondary">{t("local.dictionaryHint")}</p><textarea aria-label={t("local.dictionary")} value={dictionary} maxLength={20000} rows={5} disabled={busy} onChange={event => setDictionary(event.target.value)} spellCheck={false} /></details>}
        <div className="local-actions">{details.management && <button type="button" className="btn btn-primary" disabled={busy || !selected?.ready} onClick={() => void action("save")}>{t("local.save")}</button>}<button type="button" className="btn btn-secondary" disabled={busy || (details.management && !selected?.ready)} onClick={() => setTestOpen(true)}>{t("local.test")}</button></div>
        {testText && <div className="local-test-result" role="status"><h3>{t("local.testResult")}</h3><p>{testText}</p></div>}
      </section>
    </>}
    <FolderNavSheet open={folderOpen} onClose={() => setFolderOpen(false)} initialTab="browse" currentCwd={path.replace(/[\\/][^\\/]*$/, "")} title={t("voice.chooseFolder")} pickLabel={t("voice.chooseFolder")} onPick={folder => { setPath(`${folder.replace(/[\\/]+$/, "")}\\AgentDeskBridge.exe`); setFolderOpen(false); }} />
    {testOpen && <VoiceInputSheet onClose={() => setTestOpen(false)} onInsert={setTestText} acceptLabel={t("local.showResult")} />}
  </div>;
}
