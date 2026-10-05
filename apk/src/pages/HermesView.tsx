import { useCallback, useEffect, useRef, useState, type FormEvent } from "react";
import { FolderNavSheet, mapApiError, transport } from "@tgcontrol/shared";
import { useNavigate } from "react-router-dom";
import "../api";
import { BottomNav } from "../components/BottomNav";
import { DeviceChip } from "../components/DeviceChip";
import { IconArrow, IconChat, IconCheck, IconCopy, IconFolder, IconGear, IconPlus, IconRefresh, IconSearch, IconSend } from "../components/icons";
import { getSelectedDeviceName, getTerminalContextKey } from "../config";
import { useGoBack } from "../navBack";
import { usePolling } from "../hooks/usePolling";
import { openExternalLink } from "../openExternal";
import { terminalClipboard } from "../ptyTerm/input/ClipboardService";
import {
  createHermesClient, emptyRunState, providerConnected, readDraft, runtimeBusy, runtimeLabel,
  safeVerificationUrl, unseenEvents, writeDraft, acceptDraft, HermesUserError,
  type DeviceLogin, type HermesPrompt, type HermesRunState, type HermesStatus, type OAuthProvider,
} from "../hermes/client";
import { applyHermesEvent, durableSessionId, gatewayRestarted, modelSelection, stateFromSession, type SessionSnapshot } from "../hermes/state";
import "../hermes/hermes.css";

interface ModelProvider {
  slug: string; name: string; models: string[]; authenticated?: boolean;
  auth_type?: string; key_env?: string; warning?: string;
  pricing?: Record<string, { input: string; output: string; free: boolean }>;
}
interface ModelOptions { providers: ModelProvider[]; model: string; provider: string }
interface StoredSession { id: string; title: string; preview: string; message_count: number; started_at: number }
interface CommandCatalog {
  pairs: Array<[string, string]>;
  commands?: Record<string, { argument_mode?: string; desktop?: string }>;
  warning?: string;
}
interface RuntimeCheck { ok: boolean; error?: string; provider?: string; model?: string }
interface CommandResult { type?: string; output?: string; warning?: string; target?: string; message?: string; display?: string; notice?: string }

function storage() { try { return window.localStorage; } catch { return null; } }
function errorText(error: unknown): string {
  const known = error as { code?: string; message?: string } | null;
  if (error instanceof HermesUserError || (["hermes_rpc_failed", "hermes_backend_error"].includes(known?.code || "") && known?.message)) return known!.message!;
  return mapApiError(error) || "Не удалось связаться с Hermes. Проверьте подключение и попробуйте снова.";
}

/** One surface for desktop, web, Telegram and APK; all execution stays on the selected host. */
export function HermesView() {
  const navigate = useNavigate();
  const goBack = useGoBack();
  const [device] = useState(getTerminalContextKey);
  const [saved] = useState(() => readDraft(storage(), device));
  const [text, setText] = useState(saved.text);
  const [cwd, setCwd] = useState(saved.cwd);
  const [storedId, setStoredId] = useState(saved.sessionId);
  const [liveId, setLiveId] = useState("");
  const [status, setStatus] = useState<HermesStatus | null>(null);
  const [statusError, setStatusError] = useState("");
  const [hostNeedsUpdate, setHostNeedsUpdate] = useState(false);
  const [error, setError] = useState("");
  const [notice, setNotice] = useState("");
  const [pending, setPending] = useState("");
  const [run, setRun] = useState<HermesRunState>(emptyRunState);
  const [providers, setProviders] = useState<OAuthProvider[]>([]);
  const [models, setModels] = useState<ModelOptions | null>(null);
  const [provider, setProvider] = useState(saved.provider || "");
  const [model, setModel] = useState(saved.model || "");
  const [modelConfirm, setModelConfirm] = useState<{ provider: string; model: string; message: string } | null>(null);
  const [modelReady, setModelReady] = useState(false);
  const [modelError, setModelError] = useState("");
  const [login, setLogin] = useState<(DeviceLogin & { provider: string; expiresAt: number }) | null>(null);
  const [loginError, setLoginError] = useState("");
  const [sessions, setSessions] = useState<StoredSession[]>([]);
  const [historyError, setHistoryError] = useState("");
  const [catalog, setCatalog] = useState<CommandCatalog | null>(null);
  const [catalogError, setCatalogError] = useState("");
  const [commandQuery, setCommandQuery] = useState("");
  const [settingsOpen, setSettingsOpen] = useState(false);
  const [historyOpen, setHistoryOpen] = useState(false);
  const [commandsOpen, setCommandsOpen] = useState(false);
  const [folderOpen, setFolderOpen] = useState(false);
  const [connectionOpen, setConnectionOpen] = useState(false);
  const [apiKeyOpen, setApiKeyOpen] = useState(false);
  const [keyProvider, setKeyProvider] = useState("");
  const [apiKey, setApiKey] = useState("");
  const [streamError, setStreamError] = useState("");
  const [connectionReady, setConnectionReady] = useState(false);
  const [bootstrapAttempt, setBootstrapAttempt] = useState(0);
  const cursor = useRef(0);
  const busyAction = useRef(false);
  const alive = useRef(true);
  const bootstrapBusy = useRef(false);
  const sessionRef = useRef("");
  const initialized = useRef(false);
  const selection = useRef({ provider, model });
  const runtimeEpoch = useRef<number | undefined>(undefined);
  const runtimeReady = useRef(false);
  const preparedPrompt = useRef<{ display: string; message: string; source: string } | null>(null);
  const textarea = useRef<HTMLTextAreaElement>(null);
  const bottom = useRef<HTMLDivElement>(null);

  const [client] = useState(() => createHermesClient(transport, () => {
    if (getTerminalContextKey() !== device) throw new HermesUserError("Выбран другой компьютер. Откройте Hermes на нужном устройстве; черновик сохранен на прежнем.");
  }));
  useEffect(() => { alive.current = true; return () => { alive.current = false; }; }, []);
  useEffect(() => { sessionRef.current = liveId; }, [liveId]);
  useEffect(() => { selection.current = { provider, model }; }, [provider, model]);
  useEffect(() => { writeDraft(storage(), device, { text: preparedPrompt.current?.display === text ? preparedPrompt.current.source : text, cwd, sessionId: storedId, provider, model }); }, [device, text, cwd, storedId, provider, model]);
  useEffect(() => { bottom.current?.scrollIntoView({ block: "nearest" }); }, [run.messages.length, run.prompts.length]);

  // Action responses and polls must advance the same runtime identity before React
  // runs bootstrap; a later identical poll must not invalidate that bootstrap.
  const acceptRuntimeStatus = useCallback((next: HermesStatus) => {
    if (!alive.current) return;
    if (!next.ready || (runtimeEpoch.current !== undefined && next.backend_generation !== undefined && runtimeEpoch.current !== next.backend_generation)) {
      sessionRef.current = ""; setLiveId(""); initialized.current = false; setModelReady(false); setConnectionReady(false);
    }
    runtimeEpoch.current = next.backend_generation;
    runtimeReady.current = next.ready;
    setStatus(next); setStatusError(""); setHostNeedsUpdate(false);
  }, []);

  const refreshStatus = useCallback(async () => {
    try { acceptRuntimeStatus(await client.status()); }
    catch (e) {
      if (!alive.current) return;
      const unsupported = (e as { status?: unknown })?.status === 404;
      setHostNeedsUpdate(unsupported);
      setStatusError(unsupported
        ? "Этот компьютер пока не поддерживает Hermes в приложении. Обновите Remotai в разделе «Подключение и обновления», затем вернитесь сюда. Черновик сохранен."
        : errorText(e));
    }
  }, [client, acceptRuntimeStatus]);
  usePolling(refreshStatus, status && runtimeBusy(status) ? 1500 : 5000);

  const refreshInventory = useCallback(async (preferredProvider?: string) => {
    const results = await Promise.allSettled([
      client.providers(),
      client.rpc<ModelOptions>("model.options", { profile: "default", ...(sessionRef.current ? { session_id: sessionRef.current } : {}), include_unconfigured: true }),
      client.rpc<{ sessions: StoredSession[] }>("session.list", { profile: "default", limit: 100, include_hidden: false }),
      client.rpc<CommandCatalog>("commands.catalog", { profile: "default", ...(sessionRef.current ? { session_id: sessionRef.current } : {}) }),
    ]);
    if (!alive.current) return;
    const [auth, inventory, history, commands] = results;
    if (auth.status === "fulfilled") { setProviders(auth.value.providers || []); setLoginError(""); }
    else setLoginError(errorText(auth.reason));
    if (inventory.status === "fulfilled") {
      setModels(inventory.value);
      const selected = modelSelection(inventory.value, selection.current, !!sessionRef.current, preferredProvider);
      const activeProvider = selected.provider;
      selection.current = selected; setProvider(selected.provider); setModel(selected.model);
      try {
        const check = await client.rpc<RuntimeCheck>("setup.runtime_check", { profile: "default", ...(activeProvider ? { provider: activeProvider } : {}) });
        if (!alive.current) return;
        if (!selected.model && check.model) { setModel(check.model); selection.current.model = check.model; } setModelReady(check.ok === true && !!(selected.model || check.model)); setModelError(check.ok ? "" : check.error || "Подключите аккаунт или API-ключ, чтобы Hermes мог отвечать.");
      } catch (e) { setModelReady(false); setModelError(errorText(e)); }
    } else { setModelReady(false); setModelError(errorText(inventory.reason)); }
    if (history.status === "fulfilled") { setSessions(history.value.sessions || []); setHistoryError(""); }
    else setHistoryError(errorText(history.reason));
    if (commands.status === "fulfilled") { setCatalog(commands.value); setCatalogError(""); }
    else setCatalogError(errorText(commands.reason));
  }, [client]);

  const adoptSession = useCallback((snapshot: SessionSnapshot, requestedId = "") => {
    setLiveId(snapshot.session_id); sessionRef.current = snapshot.session_id;
    setStoredId(durableSessionId(snapshot, requestedId));
    setRun(stateFromSession(snapshot));
    if (snapshot.info?.cwd) setCwd(snapshot.info.cwd);
    if (snapshot.info?.model) setModel(snapshot.info.model);
    if (snapshot.info?.provider) setProvider(snapshot.info.provider);
    selection.current = { provider: snapshot.info?.provider || selection.current.provider, model: snapshot.info?.model || selection.current.model };
    const seq = (snapshot as SessionSnapshot & { _remotai_event_seq?: number })._remotai_event_seq;
    if (typeof seq === "number") cursor.current = seq;
  }, []);

  useEffect(() => {
    if (!status?.ready || initialized.current || bootstrapBusy.current) return;
    bootstrapBusy.current = true;
    const observedEpoch = runtimeEpoch.current;
    void (async () => {
      try {
        setError(""); setConnectionReady(false);
        await client.rpc("client.capabilities", { server_requests: true });
        const baseline = await client.events(0);
        if (!alive.current || runtimeEpoch.current !== observedEpoch || !runtimeReady.current) return;
        cursor.current = baseline.latest_seq;
        await refreshInventory();
        if (!alive.current || runtimeEpoch.current !== observedEpoch || !runtimeReady.current) return;
        if (storedId && !sessionRef.current) {
          try {
            const snapshot = await client.rpc<SessionSnapshot>("session.resume", { profile: "default", session_id: storedId, inline_images: false, source: "remotai" });
            if (alive.current && runtimeEpoch.current === observedEpoch && runtimeReady.current) adoptSession(snapshot, storedId);
          } catch (e) { if (alive.current) setError(`Не удалось открыть прежний чат. ${errorText(e)} Можно выбрать другой чат или начать новый.`); }
        }
        if (alive.current && runtimeEpoch.current === observedEpoch && runtimeReady.current) { initialized.current = true; setConnectionReady(true); }
      } catch (e) { if (alive.current) setError(errorText(e)); }
      finally {
        bootstrapBusy.current = false;
        if (alive.current && runtimeEpoch.current !== observedEpoch && runtimeReady.current) setBootstrapAttempt(value => value + 1);
      }
    })();
  }, [status?.ready, status?.backend_generation, bootstrapAttempt, client, storedId, refreshInventory, adoptSession]);

  const refreshEvents = useCallback(async () => {
    if (!initialized.current || busyAction.current) return;
    const observedSession = sessionRef.current;
    const observedEpoch = runtimeEpoch.current;
    try {
      const page = await client.events(cursor.current);
      const sameSession = observedSession === sessionRef.current && observedEpoch === runtimeEpoch.current;
      if (!alive.current || busyAction.current || !sameSession) return;
      const events = unseenEvents(page, cursor.current);
      if (gatewayRestarted(events, page.reset, observedSession) && storedId) {
        setConnectionReady(false);
        await client.rpc("client.capabilities", { server_requests: true });
        const snapshot = await client.rpc<SessionSnapshot>("session.resume", { profile: "default", session_id: storedId, inline_images: false, source: "remotai" });
        if (!alive.current) return;
        adoptSession(snapshot, storedId);
        if (!("_remotai_event_seq" in snapshot)) cursor.current = page.latest_seq;
        setStreamError("");
        await refreshInventory();
        if (alive.current) setConnectionReady(true);
        return;
      }
      if (sessionRef.current) setRun(previous => events.reduce((current, event) => applyHermesEvent(current, event, sessionRef.current), previous));
      for (const event of events) {
        const params = event.frame.params;
        if (params?.type === "session.info" && params.session_id === sessionRef.current) {
          const info = params.payload as { provider?: string; model?: string; cwd?: string } | undefined;
          if (info?.provider) { setProvider(info.provider); selection.current.provider = info.provider; }
          if (info?.model) { setModel(info.model); selection.current.model = info.model; }
          if (info?.cwd) setCwd(info.cwd);
        }
      }
      cursor.current = page.reset ? page.latest_seq : Math.max(cursor.current, page.latest_seq);
      setStreamError("");
      if (events.some(event => event.frame.params?.type === "sessions.changed" || event.frame.params?.type === "session.title")) {
        const history = await client.rpc<{ sessions: StoredSession[] }>("session.list", { profile: "default", limit: 100 });
        if (alive.current) setSessions(history.sessions || []);
      }
    } catch (e) { if (alive.current) setStreamError(errorText(e)); }
  }, [client, storedId, adoptSession, refreshInventory]);
  usePolling(refreshEvents, run.busy || run.prompts.length ? 650 : 2000, { enabled: !!status?.ready, immediate: false });

  usePolling(async () => {
    if (!login) return;
    if (Date.now() >= login.expiresAt) { setLogin(null); setLoginError("Код входа истек. Получите новый код и повторите вход."); return; }
    try {
      const result = await client.pollLogin(login.provider, login.session_id);
      if (!alive.current) return;
      if (result.status === "approved") { setLogin(null); setNotice("Вход подтвержден. Проверяем доступные модели…"); await refreshInventory(login.provider); }
      else if (result.status !== "pending") { setLogin(null); setLoginError(result.error_message || "Вход не завершен. Попробуйте получить новый код."); }
    } catch (e) { if (alive.current) setLoginError(errorText(e)); }
  }, Math.max(2000, (login?.poll_interval || 5) * 1000), { enabled: !!login, immediate: false });

  async function act(label: string, action: () => Promise<void>) {
    if (busyAction.current) return;
    busyAction.current = true; setPending(label); setError(""); setNotice("");
    try { await action(); } catch (e) { if (alive.current) setError(errorText(e)); }
    finally { busyAction.current = false; if (alive.current) setPending(""); }
  }

  async function prepare() {
    await act("prepare", async () => {
      const result = status?.installed && status.ownership !== "external" ? await client.start() : await client.install();
      if (alive.current) { acceptRuntimeStatus(result); setNotice("Задача сохранена. Когда Hermes будет готов, можно подключить модель и начать."); }
    });
  }

  async function connect(item: OAuthProvider) {
    await act("login", async () => {
      setLoginError("");
      const result = await client.login(item.id);
      if (result.flow !== "device_code" || !safeVerificationUrl(result.verification_url) || !result.session_id || !result.user_code) {
        throw new HermesUserError("Этот способ входа пока не поддерживает код устройства. Выберите другой аккаунт или API-ключ.");
      }
      if (alive.current) setLogin({ ...result, provider: item.id, expiresAt: Date.now() + result.expires_in * 1000 });
    });
  }

  async function saveKey(event: FormEvent) {
    event.preventDefault();
    if (!keyProvider || !apiKey.trim()) return;
    await act("key", async () => {
      await client.rpc("model.save_key", { profile: "default", slug: keyProvider, api_key: apiKey.trim() });
      setApiKey(""); setNotice("Ключ сохранен на компьютере. Проверяем модель…");
      await refreshInventory(keyProvider);
    });
  }

  async function ensureSession(): Promise<string> {
    if (sessionRef.current) return sessionRef.current;
    const snapshot = await client.rpc<SessionSnapshot>("session.create", {
      profile: "default", source: "remotai", ...(cwd ? { cwd, cwd_explicit: true } : {}),
      ...(model ? { model } : {}), ...(provider ? { provider } : {}),
    });
    if (!snapshot.session_id) throw new HermesUserError("Hermes не создал чат. Попробуйте запустить его снова; текст задачи сохранен.");
    const durable = durableSessionId(snapshot);
    if (durable) writeDraft(storage(), device, { ...readDraft(storage(), device), sessionId: durable });
    if (alive.current) adoptSession(snapshot);
    return snapshot.session_id;
  }

  async function send(event?: FormEvent) {
    event?.preventDefault();
    if (!text.trim() || pending || run.busy) return;
    if (!status?.ready) { await prepare(); return; }
    if (!connectionReady) return;
    if (!modelReady) { setConnectionOpen(true); return; }
    const submitted = text;
    const prepared = preparedPrompt.current?.display === submitted ? preparedPrompt.current : null;
    await act("send", async () => {
      const id = await ensureSession();
      if (submitted.trim().startsWith("/") && !prepared) {
        const result = await client.rpc<CommandResult>("slash.exec", { profile: "default", session_id: id, command: submitted.trim() });
        await handleCommand(result, submitted, id);
        return;
      }
      const promptText = prepared?.message || submitted;
      const result = await client.rpc<{ status?: string; voice_stopped?: boolean; user_row_id?: number }>("prompt.submit", { profile: "default", session_id: id, text: promptText, surface: "desktop" });
      if (!result.status && !result.voice_stopped) throw new HermesUserError("Hermes не подтвердил получение задачи. Текст сохранен; проверьте чат перед повторной отправкой.");
      acceptDraft(storage(), device, prepared?.source || submitted, readDraft(storage(), device).sessionId);
      if (!alive.current) return;
      preparedPrompt.current = null;
      setRun(previous => ({ ...previous, busy: !result.voice_stopped, error: "", messages: [...previous.messages, {
        id: `user-${result.user_row_id ?? Date.now()}`, role: "user", content: submitted,
      }] }));
      setText(current => current === submitted ? "" : current);
    });
  }

  async function handleCommand(result: CommandResult, display: string, id: string, depth = 0): Promise<void> {
    if (depth > 5) throw new HermesUserError("Команда ссылается на себя. Выберите другую команду Hermes.");
    if (result.type === "alias" && result.target) {
      const target = result.target.startsWith("/") ? result.target : `/${result.target}`;
      const next = await client.rpc<CommandResult>("slash.exec", { profile: "default", session_id: id, command: target });
      await handleCommand(next, display, id, depth + 1); return;
    }
    if (result.type === "prefill") {
      const safeDisplay = result.display || result.message || display;
      preparedPrompt.current = result.message ? { display: safeDisplay, message: result.message, source: display } : null;
      setText(safeDisplay); setNotice(result.notice || "Проверьте подготовленный запрос и отправьте его."); return;
    }
    if ((result.type === "send" || result.type === "skill") && result.message) {
      const accepted = await client.rpc<{ status?: string }>("prompt.submit", { profile: "default", session_id: id, text: result.message, surface: "desktop" });
      if (!accepted.status) throw new HermesUserError("Hermes не подтвердил запуск команды. Проверьте чат перед повторной отправкой.");
      setRun(previous => ({ ...previous, busy: true, messages: [...previous.messages, { id: `command-${Date.now()}`, role: "user", content: result.display || display }] }));
    } else if (result.output || result.notice || result.warning || result.type === "exec" || result.type === "plugin") {
      setRun(previous => ({ ...previous, messages: [...previous.messages, { id: `command-${Date.now()}`, role: "system", content: [result.output, result.notice, result.warning].filter(Boolean).join("\n") }] }));
    } else throw new HermesUserError("Hermes не вернул результат команды. Проверьте ее параметры и попробуйте снова.");
    setText(current => current === display ? "" : current);
    acceptDraft(storage(), device, display, readDraft(storage(), device).sessionId);
    preparedPrompt.current = null;
  }

  async function openSession(id: string) {
    await act("history", async () => {
      const snapshot = await client.rpc<SessionSnapshot>("session.resume", { profile: "default", session_id: id, inline_images: false, source: "remotai" });
      if (alive.current) { adoptSession(snapshot, id); setHistoryOpen(false); }
    });
  }

  async function changeModel(nextProvider: string, nextModel: string, confirm = false) {
    if (!nextModel) { setProvider(nextProvider); setModel(""); return; }
    await act("model", async () => {
      if (sessionRef.current) {
        const result = await client.rpc<{ confirm_required?: boolean; confirm_message?: string; deferred?: boolean }>("config.set", {
          profile: "default", key: "model", value: `${nextProvider}:${nextModel}`, session_id: sessionRef.current, scope: "session", confirm_expensive_model: confirm,
        });
        if (result.confirm_required) { setModelConfirm({ provider: nextProvider, model: nextModel, message: result.confirm_message || "Hermes просит подтвердить выбор этой модели." }); return; }
        if (result.deferred) { setNotice("Hermes применит выбранную модель после текущей работы."); return; }
      }
      setModelConfirm(null);
      setProvider(nextProvider); setModel(nextModel); selection.current = { provider: nextProvider, model: nextModel };
      const check = await client.rpc<RuntimeCheck>("setup.runtime_check", { profile: "default", provider: nextProvider });
      setModelReady(check.ok === true); setModelError(check.ok ? "" : check.error || "Подключите выбранный сервис.");
    });
  }

  async function respond(prompt: HermesPrompt, result: unknown) {
    await act("answer", async () => {
      await client.reply(prompt.id, result);
      if (alive.current) setRun(previous => ({ ...previous, prompts: previous.prompts.filter(row => String(row.id) !== String(prompt.id)) }));
    });
  }

  const selectedProvider = models?.providers.find(item => item.slug === provider);
  const connectedProviders = providers.filter(providerConnected);
  const keyProviders = (models?.providers || []).filter(item => item.auth_type === "api_key" || (!!item.key_env && !item.auth_type?.includes("oauth")));
  const commands = (catalog?.pairs || []).filter(([name, description]) => `${name} ${description}`.toLocaleLowerCase().includes(commandQuery.toLocaleLowerCase()));
  const unavailable = !!statusError || !status;
  const canSend = !!text.trim() && !pending && !run.busy && !runtimeBusy(status) && !unavailable && (!status?.ready || connectionReady);
  const showConnection = !!status?.ready && (!modelReady || connectionOpen);
  const deviceName = getSelectedDeviceName() || "выбранном компьютере";

  return <div className="page hermes-page">
    <header className="hermes-header">
      <button type="button" className="hermes-icon-button" onClick={goBack} aria-label="Назад к агентам"><IconArrow dir="left" /></button>
      <div className="hermes-heading"><h1>Hermes</h1><DeviceChip /></div>
      <div className="hermes-header-actions">
        <button type="button" className="hermes-icon-button" onClick={() => setHistoryOpen(value => !value)} aria-expanded={historyOpen} aria-label="История чатов"><IconChat /></button>
        <button type="button" className="hermes-icon-button" onClick={() => setSettingsOpen(value => !value)} aria-expanded={settingsOpen} aria-label="Настройки Hermes"><IconGear /></button>
      </div>
    </header>
    <main className="hermes-content">
      <div className="hermes-context">
        <span className={`hermes-runtime ${status?.ready ? "is-ready" : ""}`} role="status">{status?.ready && <IconCheck size={15} />}{runtimeLabel(status)}</span>
        <button type="button" className="hermes-context-button" onClick={() => setConnectionOpen(value => !value)} disabled={!status?.ready}>{modelReady ? model || "Модель подключена" : "Подключить модель"}</button>
      </div>
      {statusError && <div className="hermes-error" role="alert"><p>{statusError}</p>{hostNeedsUpdate
        ? <button type="button" className="btn btn-secondary" onClick={() => navigate("/settings?section=connection")}>Открыть обновления Remotai</button>
        : <button type="button" className="btn btn-secondary" onClick={() => void refreshStatus()}>Проверить снова</button>}</div>}
      {(error || run.error || streamError) && <div className="hermes-error" role="alert"><p>{error || run.error || streamError}</p><button type="button" className="btn btn-secondary" onClick={() => { setError(""); initialized.current = false; setBootstrapAttempt(value => value + 1); void refreshStatus(); }}>Проверить подключение</button></div>}
      {notice && <p className="hermes-notice" role="status">{notice}</p>}

      {historyOpen && <section className="hermes-panel" aria-label="История Hermes">
        <div className="hermes-panel-heading"><h2>Чаты на этом компьютере</h2><button type="button" className="btn btn-secondary" disabled={!!pending || run.busy} onClick={() => { setStoredId(""); setLiveId(""); sessionRef.current = ""; preparedPrompt.current = null; setRun(emptyRunState()); setHistoryOpen(false); textarea.current?.focus(); }}><IconPlus size={16} /> Новый чат</button></div>
        {historyError ? <p role="alert">{historyError}</p> : sessions.length ? <ul className="hermes-history">{sessions.map(item => <li key={item.id}><button type="button" disabled={!!pending || run.busy} className={storedId === item.id ? "is-current" : ""} onClick={() => void openSession(item.id)}><b>{item.title || item.preview || "Чат Hermes"}</b><small>{item.message_count} сообщений</small></button></li>)}</ul> : <p className="hermes-muted">Здесь появятся ваши чаты с Hermes.</p>}
      </section>}

      {settingsOpen && <section className="hermes-panel" aria-label="Настройки Hermes">
        <h2>Hermes на этом компьютере</h2>
        {status?.ownership === "managed" && <p className="hermes-muted">Канал Hermes: основной (main). Здесь изменения появляются раньше, чем в стабильных выпусках.</p>}
        <div className="hermes-setting-row"><div><b>Автоматические обновления</b><p>Обновление устанавливается, когда Hermes свободен.</p></div><button type="button" className="hermes-switch-target" role="switch" aria-label="Автоматически обновлять Hermes" aria-checked={status?.auto_update === true} disabled={!status || !!pending || status.ownership === "external"} onClick={() => void act("settings", async () => { const next = await client.settings(!status?.auto_update); acceptRuntimeStatus(next); })}><span className={`settings-toggle ${status?.auto_update ? "on" : ""}`} aria-hidden="true"><span className="settings-toggle-knob" /></span></button></div>
        {status?.ownership === "external" && <><p className="hermes-muted">Найдена ваша установка Hermes. Для запуска здесь и автоматических обновлений подготовьте отдельную копию под управлением Remotai.</p><button type="button" className="btn btn-secondary" disabled={!!pending || status.running || run.busy || runtimeBusy(status)} onClick={() => void act("install", async () => { const next = await client.install(); acceptRuntimeStatus(next); })}>Установить копию Remotai</button></>}
        <div className="hermes-setting-row"><div><b>{status?.version ? `Версия ${status.version}` : "Версия Hermes"}</b><p>{status?.update_pending ? "Обновление ожидает завершения текущей работы." : status?.update_available ? `Доступно обновление${status.latest_version ? ` ${status.latest_version}` : ""}` : status?.ownership === "external" ? "Обновления выполняются вашим установщиком Hermes." : "Проверка доступна по кнопке."}</p></div><button type="button" className="btn btn-secondary" disabled={!status?.installed || status.ownership === "external" || !!pending || runtimeBusy(status)} onClick={() => void act("update", async () => { const next = status?.update_available ? await client.update() : await client.checkUpdate(); acceptRuntimeStatus(next); })}><IconRefresh size={16} />{status?.update_available ? "Обновить" : "Проверить"}</button></div>
        <button type="button" className="btn btn-secondary" onClick={() => { setCommandsOpen(true); setSettingsOpen(false); }}>Команды Hermes</button>
      </section>}

      {!!status && !status.ready && !statusError && <section className="hermes-setup" aria-label="Подготовка Hermes">
        <h2>{runtimeBusy(status) ? "Готовим помощника" : "Начните с вашей задачи"}</h2>
        <p>{runtimeBusy(status) ? "Можно написать задачу ниже. Текст сохранится, пока идет подготовка." : "Hermes выполняет задачи на выбранном компьютере. Remotai подготовит его без команд в терминале."}</p>
        {status.operation && <p className="hermes-muted" role="status">{typeof status.operation === "string" ? status.operation_detail || runtimeLabel(status) : status.operation.message || status.operation_detail || runtimeLabel(status)}</p>}
        {status.ownership === "external" && !status.running && <p className="hermes-muted">Найдена другая установка. Remotai подготовит отдельную копию с автоматическими обновлениями.</p>}{status.last_error && <p className="hermes-error-text" role="alert">{status.last_error}</p>}
        {!runtimeBusy(status) && <button type="button" className="btn btn-primary" disabled={!!pending} onClick={() => void prepare()}>{pending === "prepare" ? "Подготавливаем…" : status.installed && status.ownership !== "external" ? "Запустить Hermes" : "Подготовить Hermes"}</button>}
        {status.installed && status.last_error && !runtimeBusy(status) && <button type="button" className="btn btn-secondary" disabled={!!pending} onClick={() => void act("repair", async () => { const next = await client.install(); acceptRuntimeStatus(next); })}>Восстановить установку</button>}
      </section>}

      {showConnection && <section className="hermes-panel hermes-connection" aria-label="Подключение модели">
        <div className="hermes-panel-heading"><h2>{modelReady ? "Подключение и модель" : "Подключите свой аккаунт"}</h2>{modelReady && <button type="button" className="hermes-context-button" onClick={() => setConnectionOpen(false)}>Готово</button>}</div>
        <p className="hermes-muted">Войдите в поддерживаемый аккаунт или добавьте API-ключ. Доступ и лимиты определяет выбранный сервис.</p>
        {loginError && <p className="hermes-error-text" role="alert">{loginError}</p>}
        {login ? <div className="hermes-login">
          <p>Откройте страницу входа и введите код:</p><div className="hermes-login-code"><code>{login.user_code}</code><button type="button" className="hermes-icon-button" aria-label="Скопировать код входа" onClick={() => void terminalClipboard.write(login.user_code).then(copied => copied ? setNotice("Код скопирован") : setLoginError("Не удалось скопировать код. Его можно выделить и скопировать вручную."))}><IconCopy size={18} /></button></div>
          <button type="button" className="btn btn-primary" onClick={() => { const url = safeVerificationUrl(login.verification_url); if (url) void openExternalLink(url); }}>Открыть страницу входа</button>
          <p className="hermes-muted" role="status">Ждем подтверждения. После входа вернитесь в Remotai.</p>
          <button type="button" className="hermes-context-button" onClick={() => { setLogin(null); setLoginError(""); }}>Выбрать другой способ</button>
        </div> : <div className="hermes-provider-list">{providers.map(item => <div className="hermes-provider-row" key={item.id}><div><b>{item.name}</b><small>{providerConnected(item) ? "Аккаунт подключен" : item.flow === "external" ? "Вход выполняется через приложение провайдера на компьютере" : item.flow !== "device_code" ? "Этот способ входа требует настройки через Hermes" : "Вход по коду, без API-ключа"}</small></div>{item.flow === "device_code" && <button type="button" className="btn btn-secondary" disabled={!!pending} onClick={() => void connect(item)}>{providerConnected(item) ? "Войти заново" : "Войти"}</button>}</div>)}</div>}
        {!providers.length && !loginError && <p className="hermes-muted" role="status">Загружаем способы входа…</p>}
        {connectedProviders.length > 0 && <p className="hermes-muted">Подключено: {connectedProviders.map(item => item.name).join(", ")}.</p>}
        {models && <div className="hermes-model-fields"><label>Сервис<select value={provider} disabled={!!pending || run.busy} onChange={event => { const next = models.providers.find(item => item.slug === event.target.value); if (next) void changeModel(next.slug, next.models[0] || ""); }}><option value="">Выберите сервис</option>{models.providers.map(item => <option key={item.slug} value={item.slug}>{item.name}{item.authenticated ? " — подключен" : ""}</option>)}</select></label><label>Модель<select value={model} disabled={!selectedProvider || !!pending || run.busy} onChange={event => void changeModel(provider, event.target.value)}><option value="">Выберите модель</option>{model && !selectedProvider?.models.includes(model) && <option value={model}>{model}</option>}{selectedProvider?.models.map(name => <option key={name} value={name}>{name}</option>)}</select></label></div>}
        {selectedProvider?.warning && <p className="hermes-muted">{selectedProvider.warning}</p>}
        {modelConfirm && <div className="hermes-model-confirm"><p>{modelConfirm.message}</p><div className="hermes-request-actions"><button type="button" className="btn btn-secondary" disabled={!!pending} onClick={() => setModelConfirm(null)}>Отменить</button><button type="button" className="btn btn-primary" disabled={!!pending} onClick={() => void changeModel(modelConfirm.provider, modelConfirm.model, true)}>Подтвердить выбор</button></div></div>}
        {modelError && !modelReady && <p className="hermes-muted">{modelError}</p>}
        <button type="button" className="hermes-context-button" aria-expanded={apiKeyOpen} onClick={() => setApiKeyOpen(value => !value)}>У меня есть API-ключ</button>
        {apiKeyOpen && <form className="hermes-key-form" onSubmit={event => void saveKey(event)}><label>Сервис API<select value={keyProvider} onChange={event => setKeyProvider(event.target.value)} required><option value="">Выберите сервис</option>{keyProviders.map(item => <option key={item.slug} value={item.slug}>{item.name}</option>)}</select></label><label>API-ключ<input type="password" autoComplete="off" value={apiKey} onChange={event => setApiKey(event.target.value)} required placeholder="Вставьте ключ" /></label><p className="hermes-muted">Ключ сохранится на выбранном компьютере в настройках Hermes.</p><button type="submit" className="btn btn-secondary" disabled={!keyProvider || !apiKey.trim() || !!pending}>{pending === "key" ? "Сохраняем…" : "Подключить ключ"}</button></form>}
      </section>}

      {commandsOpen && <section className="hermes-panel" aria-label="Команды Hermes"><div className="hermes-panel-heading"><h2>Команды Hermes</h2><button type="button" className="hermes-context-button" onClick={() => setCommandsOpen(false)}>Закрыть</button></div><p className="hermes-muted">Навыки, память, инструменты и другие команды приходят из установленного Hermes.</p><label className="hermes-search"><IconSearch size={18} /><input type="search" value={commandQuery} onChange={event => setCommandQuery(event.target.value)} placeholder="Поиск команды или возможности" aria-label="Поиск команд Hermes" /></label>{catalogError && <p role="alert">{catalogError}</p>}{catalog?.warning && <p>{catalog.warning}</p>}<ul className="hermes-command-list">{commands.map(([name, description]) => <li key={name}><button type="button" onClick={() => { setText(`/${name} `); setCommandsOpen(false); textarea.current?.focus(); }}><b>/{name}</b><small>{description}</small></button></li>)}</ul>{catalog && !commands.length && <p className="hermes-muted">Команды не найдены. Попробуйте другое слово.</p>}</section>}

      <section className="hermes-conversation" aria-label="Разговор с Hermes">
        {!run.messages.length && !showConnection && status?.ready && <div className="hermes-empty"><h2>Что хотите сделать?</h2><p>Опишите задачу своими словами. Если нужны файлы, выберите папку на компьютере.</p><div className="hermes-suggestions">{["Помоги разобраться в проекте", "Составь план моей задачи"].map(value => <button type="button" key={value} onClick={() => { setText(value); textarea.current?.focus(); }}>{value}</button>)}</div></div>}
        {run.messages.map(message => <article className={`hermes-message is-${message.role}`} key={message.id}><b>{message.role === "user" ? "Вы" : message.role === "system" ? "Hermes · сообщение" : "Hermes"}</b><div className="hermes-message-content">{message.content}</div></article>)}
        {run.activities.length > 0 && <details className="hermes-activity" open={run.busy}><summary>Действия Hermes · {run.activities.length}</summary><ol>{run.activities.map(item => <li key={item.id}><span>{item.complete && <IconCheck size={14} />}{item.text}</span>{item.details && <details><summary>Подробности</summary><pre>{item.details}</pre></details>}</li>)}</ol></details>}
        {run.prompts.map(prompt => <HermesRequest key={String(prompt.id)} prompt={prompt} disabled={!!pending} deviceName={deviceName} cwd={cwd} onReply={result => void respond(prompt, result)} />)}
        {run.busy && <div className="hermes-working" role="status">Hermes работает на {deviceName}<button type="button" className="btn btn-secondary" disabled={!!pending} onClick={() => void act("stop", async () => { const result = await client.rpc<{ status: string }>("session.interrupt", { profile: "default", session_id: liveId }); if (result.status === "not_interrupted") setNotice("Активное выполнение уже завершено."); })}>{pending === "stop" ? "Останавливаем…" : "Остановить"}</button></div>}
        <div ref={bottom} />
      </section>
      <form className="hermes-composer" onSubmit={event => void send(event)}>
        <div className="hermes-composer-context"><button type="button" className="hermes-context-button hermes-folder" onClick={() => setFolderOpen(true)} disabled={run.busy || !!pending}><IconFolder size={16} /><span>{cwd || "Выбрать папку на компьютере"}</span></button>{cwd && !liveId && <button type="button" className="hermes-context-button" onClick={() => setCwd("")}>Убрать</button>}<button type="button" className="hermes-context-button" onClick={() => setCommandsOpen(value => !value)} disabled={!status?.ready}>Команды</button></div>
        <label className="hermes-sr-only" htmlFor="hermes-task">Задача для Hermes</label>
        <textarea id="hermes-task" ref={textarea} value={text} onChange={event => { preparedPrompt.current = null; setText(event.target.value); }} placeholder={run.messages.length ? "Напишите Hermes…" : "Что хотите сделать?"} rows={3} onKeyDown={event => { if ((event.ctrlKey || event.metaKey) && event.key === "Enter") { event.preventDefault(); void send(); } }} />
        <div className="hermes-composer-actions"><small>{run.busy ? "Можно подготовить следующее сообщение." : "Черновик сохраняется для этого компьютера."}</small><button type="submit" className="btn btn-primary" disabled={!canSend}><IconSend size={17} />{pending === "send" ? "Отправляем…" : !status?.ready ? "Подготовить и продолжить" : !modelReady ? "Подключить и продолжить" : "Отправить"}</button></div>
      </form>
    </main>
    <FolderNavSheet open={folderOpen} onClose={() => setFolderOpen(false)} currentCwd={cwd} title="Папка для Hermes на компьютере" pickLabel="Выбрать" onPick={async path => { if (liveId) { try { await client.rpc("session.cwd.set", { profile: "default", session_id: liveId, cwd: path }); } catch (e) { setError(errorText(e)); return false; } } setCwd(path); setFolderOpen(false); }} />
    <BottomNav active="usage" />
  </div>;
}

function HermesRequest({ prompt, disabled, deviceName, cwd, onReply }: {
  prompt: HermesPrompt; disabled: boolean; deviceName: string; cwd: string; onReply: (result: unknown) => void;
}) {
  const questions = (Array.isArray(prompt.params.questions) ? prompt.params.questions : []).map((value, index) => {
    const row = value as { qid?: string; question?: string; choices?: string[]; multi_select?: boolean };
    return { ...row, qid: row.qid || String(index) };
  });
  const [answers, setAnswers] = useState<Record<string, string>>({});
  const [raw, setRaw] = useState("");
  const [rawError, setRawError] = useState("");
  const [secret, setSecret] = useState("");
  const [identifier, setIdentifier] = useState("");
  const secretRequest = ["sudo", "secret", "vault.unlock_prompt", "vault.save_login", "vault.code"].includes(prompt.method);
  const secretLabel = prompt.method === "sudo" ? "Пароль для команды" : prompt.method === "vault.unlock_prompt"
    ? `Пароль ${String(prompt.params.display_name || "хранилища")}` : prompt.method === "vault.code" ? "Код подтверждения"
      : prompt.method === "secret" ? String(prompt.params.prompt || prompt.params.env_var || "Секретный ключ") : "Пароль для сайта";
  return <section className="hermes-request" aria-label={prompt.title}><h3>{prompt.title}</h3><p className="hermes-muted">На {deviceName}{cwd ? ` · ${cwd}` : ""}</p>{prompt.description && <pre>{prompt.description}</pre>}
    {secretRequest ? <form onSubmit={event => { event.preventDefault(); const value = prompt.method === "vault.save_login" ? JSON.stringify({ identifier, password: secret }) : secret; onReply({ value }); setSecret(""); setIdentifier(""); }}>
      {prompt.method === "vault.save_login" && <><p>{String(prompt.params.site || prompt.params.origin || "")}</p><label>Логин<input value={identifier} autoComplete="off" onChange={event => setIdentifier(event.target.value)} /></label></>}
      <label>{secretLabel}<input type="password" value={secret} autoComplete="new-password" onChange={event => setSecret(event.target.value)} /></label><p className="hermes-muted">Значение передается Hermes на этом компьютере и не сохраняется в чате или черновике.</p><div className="hermes-request-actions"><button type="button" className="btn btn-secondary" disabled={disabled} onClick={() => { setSecret(""); setIdentifier(""); onReply({ value: "" }); }}>Пропустить</button><button type="submit" className="btn btn-primary" disabled={disabled || !secret}>Передать Hermes</button></div>
    </form> : prompt.kind === "approval" ? <><div className="hermes-request-actions"><button type="button" className="btn btn-secondary" disabled={disabled} onClick={() => onReply({ choice: "deny" })}>Отклонить</button><button type="button" className="btn btn-primary" disabled={disabled} onClick={() => onReply({ choice: "once" })}>Разрешить один раз</button>{prompt.choices.includes("session") && prompt.params.allow_session !== false && <button type="button" className="btn btn-secondary" disabled={disabled} onClick={() => onReply({ choice: "session" })}>Разрешить в этом чате</button>}</div>{prompt.choices.includes("always") && prompt.params.allow_permanent !== false && <details className="hermes-approval-options"><summary>Постоянное разрешение</summary><p>Hermes сохранит разрешение для следующих таких действий.</p><button type="button" className="btn btn-secondary" disabled={disabled} onClick={() => onReply({ choice: "always" })}>Разрешать всегда</button></details>}</>
      : prompt.kind === "question" ? <form onSubmit={event => { event.preventDefault(); onReply({ answers }); }}>{questions.map(question => <fieldset key={question.qid}><legend>{question.question}</legend>{question.choices?.map(choice => <label className="hermes-answer-choice" key={choice}><input type={question.multi_select ? "checkbox" : "radio"} name={question.qid} checked={question.multi_select ? (answers[question.qid] || "").split(", ").includes(choice) : answers[question.qid] === choice} onChange={event => setAnswers(previous => ({ ...previous, [question.qid]: question.multi_select ? (event.target.checked ? [...(previous[question.qid] || "").split(", ").filter(Boolean), choice] : (previous[question.qid] || "").split(", ").filter(value => value !== choice)).join(", ") : choice }))} />{choice}</label>)}<label className="hermes-answer-text">Свой ответ<input value={answers[question.qid] || ""} onChange={event => setAnswers(previous => ({ ...previous, [question.qid]: event.target.value }))} /></label></fieldset>)}<div className="hermes-request-actions"><button type="button" className="btn btn-secondary" disabled={disabled} onClick={() => onReply({})}>Отменить вопрос</button><button type="submit" className="btn btn-primary" disabled={disabled || !questions.every(question => !!answers[question.qid]?.trim())}>Ответить</button></div></form>
      : <form onSubmit={event => { event.preventDefault(); try { const value: unknown = JSON.parse(raw); setRawError(""); onReply(value); } catch { setRawError("Введите корректный JSON-ответ."); } }}><p>Hermes запросил действие «{prompt.method}». Подробности доступны ниже.</p><details><summary>Параметры запроса</summary><pre>{JSON.stringify(prompt.params, null, 2)}</pre></details><label>Ответ в формате JSON<textarea value={raw} onChange={event => setRaw(event.target.value)} placeholder="{}" rows={3} /></label>{rawError && <p role="alert">{rawError}</p>}<button type="submit" className="btn btn-secondary" disabled={disabled || !raw.trim()}>Отправить ответ</button></form>}
  </section>;
}
