import { useCallback, useEffect, useId, useRef, useState, type FormEvent, type ReactNode } from "react";
import { FolderNavSheet, mapApiError, platform, SheetShell, transport, useEscape } from "@tgcontrol/shared";
import { useNavigate } from "react-router-dom";
import { uploadPtyFile } from "../api";
import { BottomNav } from "../components/BottomNav";
import { DeviceChip } from "../components/DeviceChip";
import { IconChat, IconCheck, IconClose, IconCopy, IconFolder, IconList, IconRefresh, IconSearch, IconSend } from "../components/icons";
import { getMode, getSelectedDeviceId, getSelectedDeviceName, getTerminalContextKey } from "../config";
import { humanDeviceName, selectDevice } from "../devices";
import { listDevicesCached } from "../deviceSummaries";
import { usePolling } from "../hooks/usePolling";
import { openExternalLink } from "../openExternal";
import { terminalClipboard } from "../ptyTerm/input/ClipboardService";
import {
  createHermesClient, emptyRunState, providerConnected, readDraft, runtimeBusy, runtimeLabel,
  safeVerificationUrl, unseenEvents, writeDraft, acceptDraft, HermesUserError,
  type DeviceLogin, type HermesPrompt, type HermesRunState, type HermesStatus, type OAuthProvider, type StoredSession,
} from "../hermes/client";
import { applyHermesEvent, durableSessionId, gatewayRestarted, modelSelection, promptFromFrame, stateFromSession, type SessionSnapshot } from "../hermes/state";
import { availableReadCommand, clearSubmissionIdentity, controlDeepLink, submissionIdentity, type ControlSnapshot, type ControlCapabilities, type ControlAttention } from "../hermes/control";
import { readChatDraft, saveChatDraft } from "../hermes/chatDrafts";
import { useChatScroll } from "../hermes/useChatScroll";
import { HermesChatSidebar } from "../hermes/HermesChatSidebar";
import { HermesMarkdown } from "../hermes/HermesMarkdown";
import { useHermesEvents } from "../hermes/useHermesEvents";
import { HermesExecutionStatus } from "../hermes/HermesExecutionStatus";
import { normalizeHermesSubagents, type HermesSubagent } from "../hermes/subagents";
import { HermesSubagentProgress, subagentToolLabel, subagentStatusLabel } from "../hermes/HermesSubagentProgress";
import { compactSubagentObservation, emptySubagentProgress, observeSubagentEvent, observeSubagentRoster, normalizeNativeSubagentActivity } from "../hermes/subagent-progress";
import { startSubagentActivityPolling } from "../hermes/subagent-polling";
import { HermesAutomation, type HermesStyleDraft } from "../hermes/HermesAutomation";
import { stageAttachmentRefs, type ChatAttachment } from "../hermes/attachments";
import { useHermesViewport } from "../hermes/useHermesViewport";
import "../hermes/hermes.css";

interface ModelProvider {
  slug: string; name: string; models: string[]; authenticated?: boolean;
  auth_type?: string; key_env?: string; warning?: string;
  pricing?: Record<string, { input: string; output: string; free: boolean }>;
}
interface ModelOptions { providers: ModelProvider[]; model: string; provider: string }
interface CommandCatalog {
  pairs: Array<[string, string]>;
  commands?: Record<string, { argument_mode?: string; desktop?: string }>;
  warning?: string;
  remotai_commands?: {version:number;without_arguments:string[]};
}
interface RuntimeCheck { ok: boolean; error?: string; provider?: string; model?: string }
interface CommandResult { type?: string; output?: string; warning?: string; target?: string; message?: string; display?: string; notice?: string }
interface ChatIdentity { revision: number; storedId: string; liveId: string; epoch: number | undefined }

function storage() { try { return window.localStorage; } catch { return null; } }
function errorText(error: unknown): string {
  const known = error as { code?: string; message?: string } | null;
  if (error instanceof HermesUserError || (["hermes_rpc_failed", "hermes_backend_error"].includes(known?.code || "") && known?.message)) return known!.message!;
  return mapApiError(error) || "Не удалось связаться с Hermes. Проверьте подключение и попробуйте снова.";
}

/** One surface for desktop, web, Telegram and APK; all execution stays on the selected host. */
export function HermesView() {
  const navigate = useNavigate();
  const viewport = useHermesViewport();
  const [device] = useState(getTerminalContextKey);
  const [linked, setLinked] = useState(() => controlDeepLink(window.location?.hash || "", getSelectedDeviceId?.() || "", getMode?.() || ""));
  const [saved] = useState(() => readDraft(storage(), device));
  const [text, setText] = useState(saved.text);
  const [cwd, setCwd] = useState(saved.cwd);
  const [storedId, setStoredId] = useState(linked?.matches ? linked.session : saved.sessionId);
  const [liveId, setLiveId] = useState("");
  const [status, setStatus] = useState<HermesStatus | null>(null);
  const [statusError, setStatusError] = useState("");
  const [hostNeedsUpdate, setHostNeedsUpdate] = useState(false);
  const [error, setError] = useState("");
  const [notice, setNotice] = useState("");
  const [pending, setPending] = useState("");
  const [run, setRun] = useState<HermesRunState>(emptyRunState);
  const [subagents, setSubagents] = useState<HermesSubagent[] | null>(null);
  const [childProgress, setChildProgress] = useState(() => emptySubagentProgress("", ""));
  const [selectedChild, setSelectedChild] = useState("");
  const focusAssistants = useRef(false);
  const [rosterTurnBusy, setRosterTurnBusy] = useState<boolean | null>(null);
  const [subagentError, setSubagentError] = useState("");
  const [providers, setProviders] = useState<OAuthProvider[]>([]);
  const [models, setModels] = useState<ModelOptions | null>(null);
  const [provider, setProvider] = useState(saved.provider || "");
  const [model, setModel] = useState(saved.model || "");
  const [modelConfirm, setModelConfirm] = useState<{ provider: string; model: string; message: string } | null>(null);
  const [modelReady, setModelReady] = useState(false);
  const [modelCheck, setModelCheck] = useState<"checking" | "ready" | "unconfigured" | "error">("checking");
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
  const [attentionOpen, setAttentionOpen] = useState(linked?.matches === true && !!linked.attention);
  const [controlState, setControlState] = useState<ControlSnapshot | null>(null);
  const [controlCaps, setControlCaps] = useState<ControlCapabilities | null>(null);
  const [controlError, setControlError] = useState("");
  const [controlReadiness, setControlReadiness] = useState<Awaited<ReturnType<ReturnType<typeof createHermesClient>["controlReadiness"]>> | null>(null);
  const controlRevision = useRef(0);
  const receiptRevision = useRef(0);
  const [commandsOpen, setCommandsOpen] = useState(false);
  const [folderOpen, setFolderOpen] = useState(false);
  const [taskFolderPicker, setTaskFolderPicker] = useState<{ cwd: string; onPick: (path: string) => void } | null>(null);
  const [styleDraft, setStyleDraft] = useState<HermesStyleDraft | null>(null);
  const [connectionOpen, setConnectionOpen] = useState(false);
  const [desktopPanel, setDesktopPanel] = useState(() => window.matchMedia("(min-width: 1100px)").matches);
  const [view, setView] = useState<"chat" | "work">("chat");
  const [composerMenuOpen, setComposerMenuOpen] = useState(false);
  const [decision, setDecision] = useState<{prompt:HermesPrompt; chat:ChatIdentity} | null>(null);
  const currentPrompts = useRef(run.prompts);
  currentPrompts.current = run.prompts;
  const decisionPrompt = decision && run.prompts.find(prompt => prompt === decision.prompt);
  function openDecision(prompt:HermesPrompt) {
    closePanels();
    if (prompt.kind === "approval" && typeof document !== "undefined" && /^(INPUT|TEXTAREA)$/.test(document.activeElement?.tagName || "")) (document.activeElement as HTMLElement | null)?.blur?.();
    setDecision({prompt,chat:observeChat()});
  }
  useEffect(() => { if (decision && (!decisionPrompt || !isCurrentChat(decision.chat))) setDecision(null); }, [decision, decisionPrompt, storedId, liveId, status?.backend_generation]);
  const [responseView, setResponseView] = useState<"text" | "details">("text");
  const [apiKeyOpen, setApiKeyOpen] = useState(false);
  const [keyProvider, setKeyProvider] = useState("");
  const [apiKey, setApiKey] = useState("");
  const [streamError, setStreamError] = useState("");
  const [connectionReady, setConnectionReady] = useState(false);
  const [bootstrapAttempt, setBootstrapAttempt] = useState(0);
  const cursor = useRef(0);
  const recoveredReset = useRef<{ chat: ChatIdentity; seq: number } | null>(null);
  const busyAction = useRef(false);
  const alive = useRef(true);
  const bootstrapBusy = useRef(false);
  const subagentsBusy = useRef(false);
  const rosterRevision = useRef(0);
  const historyRevision = useRef(0);
  const turnBusy = useRef(run.busy);
  const sessionRef = useRef("");
  const chatSelection = useRef({ revision: 0, storedId });
  const initialized = useRef(false);
  const selection = useRef({ provider, model });
  const runtimeEpoch = useRef<number | undefined>(undefined);
  const runtimeReady = useRef(false);
  const preparedPrompt = useRef<{ display: string; message: string; source: string } | null>(null);
  const [attachments, setAttachments] = useState<ChatAttachment[]>([]);
  const attachmentRefs = useRef(new Map<string, string>());
  const uploads = useRef(new Map<string, AbortController>());
  const filePicker = useRef<HTMLInputElement>(null);
  // The unsaved draft uses the same stable empty session key as chatDrafts.
  const attachmentOwner = storedId;
  const chatAttachments = attachments.filter(item => item.owner === attachmentOwner);
  const attachmentsReady = chatAttachments.every(item => !!item.path && !item.error);
  useEffect(() => () => { uploads.current.forEach(controller => controller.abort()); }, []);
  const textarea = useRef<HTMLTextAreaElement>(null);
  const focusNewChat = useRef(false);
  const scrollArea = useRef<HTMLDivElement>(null);
  const { detached, jumpToLatest } = useChatScroll(scrollArea, `${device}:${storedId}`, view === "chat");

  const [client] = useState(() => createHermesClient(transport, () => {
    if (getTerminalContextKey() !== device) throw new HermesUserError("Выбран другой компьютер. Откройте Hermes на нужном устройстве; черновик сохранен на прежнем.");
  }));
  useEffect(() => { alive.current = true; return () => { alive.current = false; }; }, []);
  useEffect(() => {
    const changed = () => setLinked(controlDeepLink(window.location?.hash || "", getSelectedDeviceId?.() || "", getMode?.() || ""));
    window.addEventListener?.("hashchange", changed);
    return () => window.removeEventListener?.("hashchange", changed);
  }, []);
  useEffect(() => { sessionRef.current = liveId; }, [liveId]);
  useEffect(() => { turnBusy.current = run.busy; }, [run.busy]);
  useEffect(() => {
    if (!focusNewChat.current || historyOpen || view !== "chat") return;
    focusNewChat.current = false;
    textarea.current?.focus();
  }, [historyOpen, view, storedId]);
  useEffect(() => {
    const field = textarea.current;
    if (!field) return;
    const scrollTop = field.scrollTop;
    field.style.height = "auto";
    field.style.height = `${Math.min(field.scrollHeight, viewport.keyboard ? viewport.height < 360 ? 44 : 80 : 160)}px`;
    // Autosizing resets the internal scroll. Keep the active end caret visible
    // when IME/text scaling caps the draft, without moving a reader's selection.
    field.scrollTop = typeof document !== "undefined" && document.activeElement === field && field.selectionEnd === field.value.length
      ? field.scrollHeight : scrollTop;
  }, [text, viewport.height, viewport.keyboard, view]);
  useEffect(() => { selection.current = { provider, model }; }, [provider, model]);
  useEffect(() => {
    const draft = { text: preparedPrompt.current?.display === text ? preparedPrompt.current.source : text, cwd, sessionId: storedId, provider, model };
    writeDraft(storage(), device, draft); saveChatDraft(storage(), device, draft);
  }, [device, text, cwd, storedId, provider, model]);
  useEffect(() => {
    if (view === "work") {
      if (focusAssistants.current) { focusAssistants.current = false; if (typeof document !== "undefined") document.getElementById("hermes-subagent-progress")?.scrollIntoView?.({block:"start"}); }
      else scrollArea.current?.scrollTo({ top: 0 });
    }
  }, [view]);
  useEffect(() => {
    const media = window.matchMedia("(min-width: 1100px)");
    const changed = () => setDesktopPanel(media.matches);
    media.addEventListener("change", changed);
    return () => media.removeEventListener("change", changed);
  }, []);
  useEffect(() => { if (modelReady) setConnectionOpen(false); }, [modelReady]);

  const observeChat = useCallback((): ChatIdentity => ({
    ...chatSelection.current, liveId: sessionRef.current, epoch: runtimeEpoch.current,
  }), []);
  const isCurrentChat = useCallback((observed: ChatIdentity) => alive.current && getTerminalContextKey() === device
    && chatSelection.current.revision === observed.revision
    && chatSelection.current.storedId === observed.storedId
    && sessionRef.current === observed.liveId && runtimeEpoch.current === observed.epoch, []);

  const childNamespace = useCallback((chat: ChatIdentity) => JSON.stringify([device,"default",chat.storedId,chat.revision,chat.epoch ?? null,chat.liveId]), []);
  const currentChildNamespace = childNamespace({revision:chatSelection.current.revision,storedId,liveId,epoch:status?.backend_generation});
  useEffect(() => {
    setChildProgress(previous => previous.namespace === currentChildNamespace ? previous : emptySubagentProgress(currentChildNamespace,liveId));
    setSelectedChild("");
  }, [currentChildNamespace]);

  // Action responses and polls must advance the same runtime identity before React
  // runs bootstrap; a later identical poll must not invalidate that bootstrap.
  const acceptRuntimeStatus = useCallback((next: HermesStatus) => {
    if (!alive.current) return;
    if (!next.ready || (runtimeEpoch.current !== undefined && next.backend_generation !== undefined && runtimeEpoch.current !== next.backend_generation)) {
      rosterRevision.current += 1;
      sessionRef.current = ""; setLiveId(""); initialized.current = false; setModelReady(false); setModelCheck("checking"); setConnectionReady(false); setSubagents(null); setSubagentError("");
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

  const refreshHistory = useCallback(async () => {
    const observedChat = observeChat();
    // Inventory and event refreshes share request order, not completion order.
    const revision = ++historyRevision.current;
    try {
      const history = await client.rpc<{ sessions: StoredSession[] }>("session.list", { profile: "default", limit: 100, include_hidden: false });
      if (!isCurrentChat(observedChat) || revision !== historyRevision.current) return;
      setSessions(history.sessions || []); setHistoryError("");
    } catch (e) {
      if (isCurrentChat(observedChat) && revision === historyRevision.current) setHistoryError(errorText(e));
    }
  }, [client, observeChat, isCurrentChat]);

  const refreshInventory = useCallback(async (preferredProvider?: string) => {
    const observedChat = observeChat();
    const sessionParams = observedChat.liveId ? { session_id: observedChat.liveId } : {};
    // Publish each lane as it arrives. A slow catalog/provider or runtime check
    // must not hold the chat list (or each other) behind an allSettled barrier.
    await Promise.allSettled([
      (async () => {
        try {
          const auth = await client.providers();
          if (!isCurrentChat(observedChat)) return;
          setProviders(auth.providers || []); setLoginError("");
        } catch (e) { if (isCurrentChat(observedChat)) setLoginError(errorText(e)); }
      })(),
      (async () => {
        try {
          const inventory = await client.rpc<ModelOptions>("model.options", { profile: "default", ...sessionParams, include_unconfigured: true });
          if (!isCurrentChat(observedChat)) return;
          setModels(inventory);
          const selected = modelSelection(inventory, selection.current, !!observedChat.liveId, preferredProvider);
          selection.current = selected; setProvider(selected.provider); setModel(selected.model);
          const check = await client.rpc<RuntimeCheck>("setup.runtime_check", { profile: "default", ...(selected.provider ? { provider: selected.provider } : {}) });
          if (!isCurrentChat(observedChat)) return;
          if (!selected.model && check.model) { setModel(check.model); selection.current.model = check.model; }
          const ready = check.ok === true && !!(selected.model || check.model);
          setModelReady(ready); setModelCheck(ready ? "ready" : check.ok === false ? "unconfigured" : "error");
          setModelError(check.ok ? "" : check.error || "Подключите аккаунт или API-ключ, чтобы Hermes мог отвечать.");
        } catch (e) { if (!isCurrentChat(observedChat)) return; setModelReady(false); setModelCheck("error"); setModelError(errorText(e)); }
        if (isCurrentChat(observedChat)) setNotice(current => ["Вход подтвержден. Проверяем доступные модели…", "Ключ сохранен на компьютере. Проверяем модель…"].includes(current) ? "" : current);
      })(),
      refreshHistory(),
      (async () => {
        try {
          const commands = await client.rpc<CommandCatalog>("commands.catalog", { profile: "default", ...sessionParams });
          if (!isCurrentChat(observedChat)) return;
          setCatalog(commands); setCatalogError("");
        } catch (e) { if (isCurrentChat(observedChat)) setCatalogError(errorText(e)); }
      })(),
    ]);
  }, [client, observeChat, isCurrentChat, refreshHistory]);

  const adoptSession = useCallback((snapshot: SessionSnapshot, requestedId = "") => {
    rosterRevision.current += 1;
    setSubagents(null); setSubagentError("");
    setLiveId(snapshot.session_id); sessionRef.current = snapshot.session_id;
    const durableId = durableSessionId(snapshot, requestedId);
    chatSelection.current.storedId = durableId; setStoredId(durableId);
    setRun(stateFromSession(snapshot));
    if (snapshot.info && "cwd" in snapshot.info) setCwd(snapshot.info.cwd || "");
    if (snapshot.info?.model) setModel(snapshot.info.model);
    if (snapshot.info?.provider) setProvider(snapshot.info.provider);
    selection.current = { provider: snapshot.info?.provider || selection.current.provider, model: snapshot.info?.model || selection.current.model };
    const seq = (snapshot as SessionSnapshot & { _remotai_event_seq?: number })._remotai_event_seq;
    if (typeof seq === "number") cursor.current = seq;
  }, []);

  useEffect(() => {
    if (!status?.ready || initialized.current || bootstrapBusy.current) return;
    bootstrapBusy.current = true;
    let observedChat = observeChat();
    void (async () => {
      try {
        setError(""); setConnectionReady(false);
        const [, baseline] = await Promise.all([
          client.rpc("client.capabilities", { server_requests: true }),
          client.eventCursor().catch(e => {
            // Compatibility with hosts predating the lightweight cursor route.
            if (![404, 405].includes((e as { status?: number })?.status || 0)) throw e;
            return client.events(0);
          }),
        ]);
        if (!isCurrentChat(observedChat) || !runtimeReady.current) return;
        cursor.current = baseline.latest_seq;
        if (observedChat.storedId && !sessionRef.current) {
          try {
            const snapshot = await client.rpc<SessionSnapshot>("session.resume", { profile: "default", session_id: observedChat.storedId, inline_images: false, source: "remotai" });
            if (!isCurrentChat(observedChat) || !runtimeReady.current) return;
            adoptSession(snapshot, observedChat.storedId); observedChat = observeChat();
          } catch (e) { if (isCurrentChat(observedChat)) setError(`Не удалось открыть прежний чат. ${errorText(e)} Можно выбрать другой чат или начать новый.`); }
        }
        if (isCurrentChat(observedChat) && runtimeReady.current) {
          initialized.current = true; setConnectionReady(true);
          // Inventory is not a prerequisite for restoring the transcript or events.
          void refreshInventory();
        }
      } catch (e) { if (isCurrentChat(observedChat)) setError(errorText(e)); }
      finally {
        bootstrapBusy.current = false;
        if (alive.current && !isCurrentChat(observedChat) && runtimeReady.current && !initialized.current) setBootstrapAttempt(value => value + 1);
      }
    })();
  }, [status?.ready, status?.backend_generation, bootstrapAttempt, client, storedId, refreshInventory, adoptSession, observeChat, isCurrentChat]);

  const refreshEvents = useCallback(async (signal: AbortSignal) => {
    if (!initialized.current || busyAction.current) return;
    let observedChat = observeChat();
    try {
      const began = performance.now();
      const page = await client.waitEvents(cursor.current, signal);
      if (signal.aborted || !isCurrentChat(observedChat) || busyAction.current) return;
      // Old hosts may ignore wait_ms. Avoid a hot request loop on empty replies.
      const nextDelay = !page.events.length && performance.now() - began < 150 ? (turnBusy.current ? 650 : 2000) : 32;
      const events = unseenEvents(page, cursor.current);
      if (events.some(event => event.frame.params?.session_id === observedChat.liveId && String(event.frame.params.type || "").startsWith("subagent."))) {
        rosterRevision.current += 1; setSubagents(null); setSubagentError("");
      }
      const acknowledgedEmptyReset = !!page.reset && !events.length
        && recoveredReset.current?.seq === page.latest_seq && isCurrentChat(recoveredReset.current.chat);
      if (gatewayRestarted(events, page.reset && !acknowledgedEmptyReset, observedChat.liveId) && observedChat.storedId) {
        initialized.current = false;
        setConnectionReady(false);
        try {
          await client.rpc("client.capabilities", { server_requests: true });
          if (!isCurrentChat(observedChat) || !runtimeReady.current) return;
          const snapshot = await client.rpc<SessionSnapshot>("session.resume", { profile: "default", session_id: observedChat.storedId, inline_images: false, source: "remotai" });
          if (!isCurrentChat(observedChat) || !runtimeReady.current) return;
          adoptSession(snapshot, observedChat.storedId); observedChat = observeChat();
          if (!("_remotai_event_seq" in snapshot)) cursor.current = page.latest_seq;
          recoveredReset.current = { chat: observedChat, seq: cursor.current };
          setStreamError("");
          if (isCurrentChat(observedChat) && runtimeReady.current) { initialized.current = true; setConnectionReady(true); void refreshInventory(); }
        } finally {
          if (alive.current && runtimeReady.current && !initialized.current) setBootstrapAttempt(value => value + 1);
        }
        return nextDelay;
      }
      if (sessionRef.current) {
        const receivedAt = Date.now(), namespace = childNamespace(observedChat);
        setChildProgress(previous => events.reduce((current,event)=>observeSubagentEvent(current,event,receivedAt),
          previous.namespace === namespace ? previous : emptySubagentProgress(namespace,observedChat.liveId)));
        setRun(previous => events.reduce((current, event) => applyHermesEvent(current, event, sessionRef.current), previous));
      }
      for (const event of events) {
        const params = event.frame.params;
        if (params?.type === "session.info" && params.session_id === sessionRef.current) {
          const info = params.payload as { provider?: string; model?: string; cwd?: string } | undefined;
          if (info?.provider) { setProvider(info.provider); selection.current.provider = info.provider; }
          if (info?.model) { setModel(info.model); selection.current.model = info.model; }
          if (info && "cwd" in info) setCwd(info.cwd || "");
        }
      }
      cursor.current = page.reset ? page.latest_seq : Math.max(cursor.current, page.latest_seq);
      setStreamError("");
      if (events.some(event => event.frame.params?.type === "sessions.changed" || event.frame.params?.type === "session.title")) {
        void refreshHistory();
      }
      return nextDelay;
    } catch (e) {
      if (signal.aborted) return;
      if (isCurrentChat(observedChat)) setStreamError(errorText(e));
      throw e;
    }
  }, [client, adoptSession, refreshInventory, refreshHistory, observeChat, isCurrentChat]);
  useHermesEvents(refreshEvents, !!status?.ready && connectionReady);

  const refreshSubagents = useCallback(async (): Promise<void> => {
    const observedChat = observeChat();
    if (!observedChat.liveId || !initialized.current || !runtimeReady.current || subagentsBusy.current) return;
    const observedRoster = rosterRevision.current;
    const observedTurnBusy = turnBusy.current;
    subagentsBusy.current = true;
    try {
      const result = await client.rpc("subagent.list", { profile: "default", session_id: observedChat.liveId });
      if (!isCurrentChat(observedChat) || observedRoster !== rosterRevision.current) return;
      const rows = normalizeHermesSubagents(result), namespace = childNamespace(observedChat), checkedAt = Date.now();
      setChildProgress(previous => observeSubagentRoster(previous.namespace === namespace ? previous : emptySubagentProgress(namespace,observedChat.liveId),rows,checkedAt));
      setSubagents(rows); setRosterTurnBusy(observedTurnBusy); setSubagentError("");
    } catch {
      if (!isCurrentChat(observedChat) || observedRoster !== rosterRevision.current) return;
      setSubagents(null); setSubagentError("Не удалось проверить фоновых помощников. Повторите проверку.");
    } finally {
      subagentsBusy.current = false;
      if (alive.current && observedRoster !== rosterRevision.current) void refreshSubagents();
    }
  }, [client, observeChat, isCurrentChat]);
  useEffect(() => {
    rosterRevision.current += 1;
    setControlReadiness(null);
    setSubagents(null); setSubagentError("");
    if (connectionReady && liveId) void refreshSubagents();
  }, [liveId, connectionReady, run.busy, refreshSubagents]);
  usePolling(refreshSubagents, 2500, { enabled: !!status?.ready && connectionReady && !!liveId });

  // This ancillary lane never gates history restore or the event reader.
  const refreshControl = useCallback(async () => {
    if (typeof client.controlSnapshot !== "function") return;
    const epoch = runtimeEpoch.current;
    const observedChat = observeChat();
    const observedCursor = cursor.current;
    const revision = ++controlRevision.current;
    try {
      const snapshot = await client.controlSnapshot();
      if (!alive.current || getTerminalContextKey() !== device || epoch !== runtimeEpoch.current || revision !== controlRevision.current) return;
      setControlState(snapshot); setControlError("");
      const pendingIds = new Set((snapshot.attention || []).filter(item => item.state === "pending" && item.generation === epoch).map(item => item.request_id));
      // A snapshot requested before a native event must not retract that event's
      // question. The next settled snapshot can reconcile the same chat/cursor.
      if (isCurrentChat(observedChat) && observedCursor === cursor.current) setRun(previous => ({ ...previous, prompts: previous.prompts.filter(prompt => pendingIds.has(String(prompt.id))) }));
    } catch (e) { if (alive.current && epoch === runtimeEpoch.current && revision === controlRevision.current) setControlError((e as {status?:number})?.status === 404 || (e as {status?:number})?.status === 501 ? "Обновите Remotai на компьютере для журнала задач и входящих. Черновик сохранён." : errorText(e)); }
  }, [client]);
  usePolling(refreshControl, 1500, { enabled: !!status });
  useEffect(() => {
    if (!status || typeof client.controlCapabilities !== "function") return;
    const epoch = status.backend_generation;
    let current = true;
    setControlCaps(null);
    void refreshControl();
    void client.controlCapabilities().then(caps => { if (current && alive.current && epoch === runtimeEpoch.current) setControlCaps(caps); }).catch(() => { /* fallback is explicit in controls; never fail chat */ });
    return () => { current = false; };
  }, [client, status?.backend_generation, refreshControl]);

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

  async function act(label: string, action: () => Promise<void>, observed?: ChatIdentity) {
    if (busyAction.current) return;
    busyAction.current = true; setPending(label); setError(""); setNotice("");
    try { await action(); } catch (e) { if (alive.current && (!observed || isCurrentChat(observed))) setError(errorText(e)); }
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

  async function ensureSession(observedChat: ChatIdentity): Promise<string> {
    if (!isCurrentChat(observedChat)) throw new HermesUserError("Чат изменился. Черновик сохранён.");
    if (sessionRef.current) return sessionRef.current;
    const snapshot = await client.rpc<SessionSnapshot>("session.create", {
      profile: "default", source: "remotai", ...(cwd ? { cwd, cwd_explicit: true } : {}),
      ...(model ? { model } : {}), ...(provider ? { provider } : {}),
    });
    if (!isCurrentChat(observedChat) || !runtimeReady.current) throw new HermesUserError("Выбран другой чат или Hermes перезапущен. Текст задачи сохранен; отправьте его в текущем чате.");
    if (!snapshot.session_id) throw new HermesUserError("Hermes не создал чат. Попробуйте запустить его снова; текст задачи сохранен.");
    const durable = durableSessionId(snapshot);
    if (durable) writeDraft(storage(), device, { ...readDraft(storage(), device), sessionId: durable });
    if (alive.current) {
      const oldOwner = chatSelection.current.storedId;
      if (durable) setAttachments(previous => previous.map(item => item.owner === oldOwner ? {...item, owner: durable} : item));
      adoptSession(snapshot);
    }
    // Only the synchronous session creation may advance this operation identity.
    observedChat.storedId = chatSelection.current.storedId;
    observedChat.liveId = sessionRef.current;
    return snapshot.session_id;
  }

  async function uploadAttachment(item: ChatAttachment) {
    if (getTerminalContextKey() !== device || !alive.current) return;
    uploads.current.set(item.id, item.controller);
    setAttachments(previous => previous.map(row => row.id === item.id ? {...row, progress: 0, error: undefined} : row));
    try {
      const result = await uploadPtyFile(item.file, progress => {
        if (alive.current && getTerminalContextKey() === device) setAttachments(previous => previous.map(row => row.id === item.id ? {...row, progress} : row));
      }, {signal: item.controller.signal});
      if (!result.path) throw new HermesUserError("Компьютер не подтвердил загрузку файла.");
      if (alive.current && getTerminalContextKey() === device && !item.controller.signal.aborted) setAttachments(previous => previous.map(row => row.id === item.id ? {...row, progress: 100, path: result.path} : row));
    } catch (cause) {
      if (alive.current && getTerminalContextKey() === device && !item.controller.signal.aborted) setAttachments(previous => previous.map(row => row.id === item.id ? {...row, error: errorText(cause)} : row));
    } finally { uploads.current.delete(item.id); }
  }
  function addAttachments(files: File[]) {
    if (pending || busyAction.current || getTerminalContextKey() !== device) return;
    const owner = chatSelection.current.storedId;
    const added = files.map(file => ({id: crypto.randomUUID(), owner, file, progress: 0, controller: new AbortController()}));
    setAttachments(previous => [...previous, ...added]);
    added.forEach(item => void uploadAttachment(item));
  }
  async function send(event?: FormEvent) {
    event?.preventDefault();
    if ((!text.trim() && !chatAttachments.length) || !attachmentsReady || pending || run.busy) return;
    if (!status?.ready) { await prepare(); return; }
    if (!connectionReady) return;
    if (!modelReady) { setConnectionOpen(true); return; }
    const submitted = text;
    const submittedAttachments = [...chatAttachments];
    const prepared = preparedPrompt.current?.display === submitted ? preparedPrompt.current : null;
    const observed = observeChat();
    await act("send", async () => {
      const id = await ensureSession(observed);
      if (!isCurrentChat(observed)) return;
      if (submitted.trim().startsWith("/") && !prepared && !submittedAttachments.length) {
        if (!availableReadCommand(catalog?.remotai_commands, submitted.trim())) throw new HermesUserError("Эта команда недоступна через защищённый HTTP-интерфейс Remotai или её доступность ещё не подтверждена. Доступные команды выполняются без аргументов; черновик сохранён.");
        const result = await client.rpc<CommandResult>("slash.exec", { profile: "default", session_id: id, command: submitted.trim() });
        if (!isCurrentChat(observed)) return;
        await handleCommand(result, submitted, id, observed);
        return;
      }
      const refs = await stageAttachmentRefs(submittedAttachments, client, id, () => isCurrentChat(observed), attachmentRefs.current);
      if (!refs) return;
      const promptText = [prepared?.message || submitted, ...refs].filter(Boolean).join("\n");
      const result = await admitPrompt(id, promptText);
      if (!isCurrentChat(observed)) return;
      if (!result.status && !result.voice_stopped) throw new HermesUserError("Hermes не подтвердил получение задачи. Текст сохранен; проверьте чат перед повторной отправкой.");
      if (result.status === "completed") {
        void restoreCompletedReceipt(id, observed);
      }
      acceptDraft(storage(), device, prepared?.source || submitted, readDraft(storage(), device).sessionId);
      clearAcceptedChatDraft(prepared?.source || submitted);
      if (!alive.current) return;
      preparedPrompt.current = null;
      if (result.status !== "completed") setRun(previous => ({ ...previous, busy: !result.voice_stopped, error: "", messages: [...previous.messages, {
        id: `user-${result.user_row_id ?? Date.now()}`, role: "user", content: [submitted, ...submittedAttachments.map(item => `📎 ${item.file.name}`)].filter(Boolean).join("\n"),
      }] }));
      setText(current => current === submitted ? "" : current);
      const acceptedIds = new Set(submittedAttachments.map(item => item.id));
      acceptedIds.forEach(id => attachmentRefs.current.delete(id));
      setAttachments(previous => previous.filter(item => !acceptedIds.has(item.id)));
    }, observed);
  }

  async function admitPrompt(id: string, promptText: string, queued = false): Promise<{status?:string;voice_stopped?:boolean;user_row_id?:number}> {
    receiptRevision.current += 1;
    const durable = chatSelection.current.storedId;
    const observed = observeChat();
    const requestId = submissionIdentity(storage(), device, durable || id, promptText, queued);
    let result;
    try { result = await client.submitTask({client_request_id:requestId,session_id:id,stored_session_id:durable,text:promptText,queued,generation:runtimeEpoch.current}); }
    catch (error) {
      // Only the server's typed, pre-journal/pre-wire refusal releases this ID.
      if ((error as {code?:string})?.code === "hermes_admission_rejected") clearSubmissionIdentity(storage(),device,durable || id,requestId);
      throw error;
    }
    if (["interrupted","failed"].includes(result.status)) throw new HermesUserError("Задача уже была принята, но её исход неизвестен или завершился ошибкой. Проверьте журнал; автоматического повтора не будет.");
    if (isCurrentChat(observed) && result.status) clearSubmissionIdentity(storage(),device,durable || id,requestId);
    return result;
  }
  async function restoreCompletedReceipt(id: string, observed: ChatIdentity) {
    const eventCursor = cursor.current;
    const revision = receiptRevision.current;
    const controlRequest = ++controlRevision.current;
    // Completed receipts reconcile off the admission lock; events and newer
    // submissions retain ownership of the transcript and active-turn state.
    const [history, snapshot] = await Promise.all([
      client.rpc<SessionSnapshot>("session.history", {profile:"default",session_id:id}).catch(() => null),
      typeof client.controlSnapshot === "function" ? client.controlSnapshot().catch(() => null) : Promise.resolve(null),
    ]);
    if (!isCurrentChat(observed) || eventCursor !== cursor.current || revision !== receiptRevision.current) return;
    if (snapshot && controlRequest === controlRevision.current) { setControlState(snapshot); setControlError(""); }
    const active = snapshot?.tasks?.some(task => task.session_id === id && task.generation === observed.epoch && ["accepted","running","waiting_user"].includes(task.state));
    setRun(previous => ({...previous, ...(history && Array.isArray(history.messages) ? {messages:stateFromSession(history).messages} : {}), busy: snapshot && controlRequest === controlRevision.current ? !!active : previous.busy}));
    setNotice(history ? "Эта задача уже завершена. История обновлена; повторного запуска не было." : "Эта задача уже завершена; повторного запуска не было. История сейчас недоступна — откройте беседу позже.");
  }
  async function steerNow() {
    if (!text.trim() || chatAttachments.length || !liveId || !controlCaps?.steer) return;
    const submitted=text;
    await act("steer",async () => {
      const observed=observeChat();
      const result=await client.controlIntent("steer",observed.liveId,submitted);
      if (!isCurrentChat(observed)) return;
      if (result.status!=="queued" && result.status!=="delivered") throw new HermesUserError("Hermes не принял уточнение. Черновик сохранён; можно отправить следующей задачей.");
      setNotice(result.status==="delivered"?"Уточнение доставлено Hermes.":"Уточнение принято Hermes в очередь к ближайшей границе инструмента. Доставка ещё не подтверждена.");
      setText(current=>current===submitted?"":current);
    });
  }
  async function queueNext() {
    if (!text.trim() || chatAttachments.length || !liveId || !controlCaps?.queue) return;
    const submitted=text;
    const observed=observeChat();
    await act("queue",async()=>{
      const result=await admitPrompt(observed.liveId,submitted,true);
      if(!isCurrentChat(observed))return;
      if (result.status === "completed") void restoreCompletedReceipt(observed.liveId, observed);
      acceptDraft(storage(),device,submitted,observed.storedId);
      clearAcceptedChatDraft(submitted);
      if (result.status !== "completed") setNotice(result.status==="accepted"?"Следующая задача принята в журнал. Очередь исполнения подтверждает Hermes; смотрите статус во входящих.":`Hermes: ${result.status}`);
      setText(current=>current===submitted?"":current);
      if (result.status !== "completed") void refreshControl();
    }, observed);
  }
  async function answerAttention(item: ControlAttention, result: unknown) {
    await act("answer",async()=>{
      if (item.generation !== runtimeEpoch.current || item.state !== "pending" || getTerminalContextKey() !== device) {
        void refreshControl(); throw new HermesUserError("Запрос больше не действует или выбран другой компьютер. Обновите входящие.");
      }
      const observed = observeChat();
      try { await client.controlReply(item.id,result); }
      finally { if (getTerminalContextKey() === device) void refreshControl(); }
      if(isCurrentChat(observed))setRun(previous=>({...previous,prompts:previous.prompts.filter(prompt=>String(prompt.id)!==item.request_id)}));
    });
  }
  async function handleCommand(result: CommandResult, display: string, id: string, observed: ChatIdentity, depth = 0): Promise<void> {
    if (!isCurrentChat(observed)) return;
    if (depth > 5) throw new HermesUserError("Команда ссылается на себя. Выберите другую команду Hermes.");
    if (result.type === "alias" && result.target) {
      const target = result.target.startsWith("/") ? result.target : `/${result.target}`;
      const next = await client.rpc<CommandResult>("slash.exec", { profile: "default", session_id: id, command: target });
      if (!isCurrentChat(observed)) return;
      await handleCommand(next, display, id, observed, depth + 1); return;
    }
    if (result.type === "prefill") {
      const safeDisplay = result.display || result.message || display;
      preparedPrompt.current = result.message ? { display: safeDisplay, message: result.message, source: display } : null;
      setText(safeDisplay); setNotice(result.notice || "Проверьте подготовленный запрос и отправьте его."); return;
    }
    if ((result.type === "send" || result.type === "skill") && result.message) {
      const accepted = await admitPrompt(id, result.message);
      if (!isCurrentChat(observed)) return;
      if (!accepted.status) throw new HermesUserError("Hermes не подтвердил запуск команды. Проверьте чат перед повторной отправкой.");
      if (accepted.status === "completed") {
        void restoreCompletedReceipt(id, observed);
      } else setRun(previous => ({ ...previous, busy: true, messages: [...previous.messages, { id: `command-${Date.now()}`, role: "user", content: result.display || display }] }));
    } else if (result.output || result.notice || result.warning || result.type === "exec" || result.type === "plugin") {
      setRun(previous => ({ ...previous, messages: [...previous.messages, { id: `command-${Date.now()}`, role: "system", content: [result.output, result.notice, result.warning].filter(Boolean).join("\n") }] }));
    } else throw new HermesUserError("Hermes не вернул результат команды. Проверьте ее параметры и попробуйте снова.");
    setText(current => current === display ? "" : current);
    acceptDraft(storage(), device, display, readDraft(storage(), device).sessionId);
    clearAcceptedChatDraft(display);
    preparedPrompt.current = null;
  }

  async function openSession(id: string) {
    if (busyAction.current) return;
    chatSelection.current.revision += 1;
    const observedChat = observeChat();
    saveChatDraft(storage(), device, { text: preparedPrompt.current?.display === text ? preparedPrompt.current.source : text, cwd, sessionId: storedId, provider, model });
    await act("history", async () => {
      const snapshot = await client.rpc<SessionSnapshot>("session.resume", { profile: "default", session_id: id, inline_images: false, source: "remotai" });
      if (isCurrentChat(observedChat) && runtimeReady.current) {
        preparedPrompt.current = null; adoptSession(snapshot, id); setText(readChatDraft(storage(), device, id)?.text || ""); setHistoryOpen(false);
        setModelReady(false); setModelCheck("checking"); void refreshInventory();
      }
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
      setModelReady(check.ok === true); setModelCheck(check.ok === true ? "ready" : check.ok === false ? "unconfigured" : "error"); setModelError(check.ok ? "" : check.error || "Подключите выбранный сервис.");
    });
  }

  async function respond(prompt: HermesPrompt, result: unknown) {
    await act("answer", async () => {
      if (getTerminalContextKey() !== device || status?.backend_generation !== runtimeEpoch.current
        || (prompt.params.session_id && prompt.params.session_id !== sessionRef.current)) {
        throw new HermesUserError("Выбран другой чат или Hermes перезапущен. Старый запрос недействителен.");
      }
      const observed = observeChat();
      await client.reply(prompt.id, result);
      if (isCurrentChat(observed)) setRun(previous => ({ ...previous, prompts: previous.prompts.filter(row => String(row.id) !== String(prompt.id)) }));
    });
  }

  const selectedProvider = models?.providers.find(item => item.slug === provider);
  const connectedProviders = providers.filter(providerConnected);
  const deviceProviders = providers.filter(item => item.flow === "device_code").sort((a, b) => Number(b.id === "openai-codex") - Number(a.id === "openai-codex"));
  const keyProviders = (models?.providers || []).filter(item => item.auth_type === "api_key" || (!!item.key_env && !item.auth_type?.includes("oauth")));
  const commandAvailable = (name: string) => availableReadCommand(catalog?.remotai_commands, name);
  const contextAvailable = commandAvailable("context");
  const commandUnavailableReason = "Недоступно через защищённый HTTP-интерфейс Remotai; используйте нативный Hermes. Доступные здесь команды выполняются только без аргументов.";
  const commands = (catalog?.pairs || []).filter(([name, description]) => `${name} ${description}`.toLocaleLowerCase().includes(commandQuery.toLocaleLowerCase()));
  const unavailable = !!statusError || !status;
  const canSend = (!!text.trim() || chatAttachments.length > 0) && attachmentsReady && !pending && !run.busy && !runtimeBusy(status) && !unavailable && (!status?.ready || connectionReady);
  const needsAccount = modelCheck === "unconfigured";
  const restoringConversation = !hostNeedsUpdate && !statusError && (!status || (status.ready && !connectionReady) || (!status.ready && (!!storedId || run.busy)));
  const showConnection = !!status?.ready && ((needsAccount && connectionReady) || connectionOpen);
  const deviceName = humanDeviceName(getSelectedDeviceName(), "выбранном компьютере");
  const deviceLabel = humanDeviceName(getSelectedDeviceName(), "Подключённый компьютер");
  const activePanel = attentionOpen ? "attention" : historyOpen ? "history" : settingsOpen ? "settings" : commandsOpen ? "commands" : connectionOpen && !needsAccount ? "connection" : "";
  function retryConnection() {
    if (bootstrapBusy.current) return;
    initialized.current = false; setConnectionReady(false); setModelCheck("checking"); setError(""); setBootstrapAttempt(value => value + 1);
    void refreshStatus();
  }
  function closePanels() { setDecision(null); setAttentionOpen(false); setHistoryOpen(false); setSettingsOpen(false); setCommandsOpen(false); setConnectionOpen(false); setComposerMenuOpen(false); }
  function togglePanel(panel: string) {
    setAttentionOpen(panel === "attention" && activePanel !== panel);
    setComposerMenuOpen(false);
    setHistoryOpen(panel === "history" && activePanel !== panel);
    setSettingsOpen(panel === "settings" && activePanel !== panel);
    setCommandsOpen(panel === "commands" && activePanel !== panel);
    setConnectionOpen(panel === "connection" && activePanel !== panel);
  }
  function newChat() {
    rosterRevision.current += 1;
    focusNewChat.current = true;
    chatSelection.current.revision += 1;
    saveChatDraft(storage(), device, { text: preparedPrompt.current?.display === text ? preparedPrompt.current.source : text, cwd, sessionId: storedId, provider, model });
    const draft = readChatDraft(storage(), device, "");
    setText(draft?.cwd === cwd ? draft.text : "");
    chatSelection.current.storedId = "";
    setStoredId(""); setLiveId(""); sessionRef.current = ""; preparedPrompt.current = null;
    // Detached inventory for the previous identity cannot publish here. Restart
    // those lanes, not the already negotiated cursor/capabilities bootstrap.
    if (initialized.current && runtimeReady.current) {
      setModelReady(false); setModelCheck("checking");
      void refreshInventory();
    }
    setRun(emptyRunState()); setSubagents(null); setSubagentError(""); setView("chat"); closePanels(); textarea.current?.focus();
  }
  function clearAcceptedChatDraft(submitted: string) {
    const draft = readChatDraft(storage(), device, storedId);
    if (draft?.text === submitted) saveChatDraft(storage(), device, { ...draft, text: "" });
  }
  useEscape((!!activePanel || composerMenuOpen) && !folderOpen && !decision, closePanels);
  useEscape(!!decision && !!decisionPrompt && !folderOpen, () => setDecision(null));
  const activeAnswer = run.messages.find(message => message.id === run.streamId);
  const workingLabel = run.prompts.length ? "Hermes ждет вашего ответа" : run.progressText || (activeAnswer?.content ? "Hermes отвечает…" : "Hermes думает…");
  const chatTitle = sessions.find(item => item.id === storedId)?.title || (storedId ? "Чат Hermes" : "Новый чат");
  const conversationResults = (controlState?.results || []).filter(item => storedId ? item.stored_session_id === storedId : item.session_id === liveId && !!liveId);
  const verifiedFiles = conversationResults.filter(item => item.kind === "file" && item.verified);
  const toolResults = conversationResults.filter(item => item.kind !== "file" || !item.verified);
  const attentionCount = controlState?.attention.filter(item => ["pending", "unread", "uncertain"].includes(item.state)).length || 0;
  const folderName = cwd.replace(/[\\/]+$/, "").split(/[\\/]/).pop() || cwd;
  const hasAnswer = run.messages.some(message => ["assistant", "agent"].includes(message.role) && !!message.content);
  const confirmedSubagents = rosterTurnBusy === run.busy ? subagents : null;
  const childRows = childProgress.namespace === currentChildNamespace ? childProgress.children : [];
  const selectedObservation = childRows.find(child=>child.id === selectedChild);
  const pollChild = view === "work" && connectionReady && selectedObservation?.present && !selectedObservation.terminalConfirmed
    && selectedObservation.status === "running" && !["unsupported","unavailable"].includes(selectedObservation.nativeState) ? selectedChild : "";
  useEffect(() => {
    if (!pollChild || typeof client.subagentActivity !== "function") return;
    const observed = observeChat(), namespace = childNamespace(observed);
    const publish = (nativeState: ReturnType<typeof normalizeNativeSubagentActivity>["state"], native?: ReturnType<typeof normalizeNativeSubagentActivity>) => {
      if (!isCurrentChat(observed)) return;
      setChildProgress(previous => previous.namespace !== namespace ? previous : { ...previous, children: previous.children.map(child => child.id !== pollChild ? child : {
        ...child, nativeState, ...(native ? {nativeJournal:native.entries, journal:native.entries.length < 200 ? child.journal.slice(-(200-native.entries.length)) : [], capturedAt:native.capturedAt, partial:child.partial || native.partial || child.journal.length+native.entries.length>200} : {}),
      }) });
    };
    return startSubagentActivityPolling({
      fetch:signal=>client.subagentActivity(observed.liveId,pollChild,signal),
      accept:value=>{
        if (!isCurrentChat(observed)) return false;
        const native = normalizeNativeSubagentActivity(value); publish(native.state,native);
        return native.state !== "unsupported" && native.state !== "unavailable";
      },
      error:reason=>{
        const e = reason as {status?:number;code?:string | number};
        publish(e?.status === 404 || e?.code === -32601 || e?.code === "-32601" ? "unsupported" : "error");
      },
      visible:()=>isCurrentChat(observed) && (typeof document === "undefined" || !document.hidden),
      subscribe:change=>{ if (typeof document === "undefined") return ()=>{};document.addEventListener("visibilitychange",change);return()=>document.removeEventListener("visibilitychange",change); },
    });
  }, [pollChild,currentChildNamespace,client,observeChat,isCurrentChat]);
  const recentChild = compactSubagentObservation(childRows, subagentError ? null : confirmedSubagents);
  const latestNative = recentChild?.nativeState === "supported" ? recentChild.nativeJournal[recentChild.nativeJournal.length - 1] : undefined;
  const childActivity = recentChild ? {
    unconfirmed: !recentChild.present,
    title: !recentChild.present ? "Завершение помощника не подтверждено" : ["queued","pending"].includes(recentChild.rawStatus || "") ? "Помощник в очереди" : recentChild.status === "running" ? "Помощник работает" : `Помощник: ${subagentStatusLabel(recentChild)}`,
    detail: latestNative ? `Последнее в журнале: ${latestNative.kind === "tool_result" ? "результат" : "запуск"} · ${subagentToolLabel(latestNative.toolName)} · ${latestNative.sourceTimeText}`
      : `${recentChild.lastTool ? `Последний запуск: ${subagentToolLabel(recentChild.lastTool)}` : "Запуск инструмента не наблюдался"}${recentChild.goal ? ` · ${recentChild.goal}` : ""}`,
  } : undefined;
  function openAssistantDetails() {
    closePanels(); setSelectedChild(recentChild?.id || childRows[0]?.id || "");
    if (view === "work") { if (typeof document !== "undefined") document.getElementById("hermes-subagent-progress")?.scrollIntoView?.({block:"start"}); }
    else { focusAssistants.current = true;setView("work"); }
  }
  const visibleActivities = run.activities.filter(item=>!item.kind.startsWith("subagent."));
  const modelLabel = modelReady ? model || "Модель подключена" : modelCheck === "checking" ? "Проверяем подключение…" : needsAccount ? "Сначала подключите аккаунт" : "Не удалось проверить подключение";

  const handledLink = useRef<typeof linked>(null);
  const attemptedLink = useRef<typeof linked>(null);
  useEffect(() => {
    if (!linked?.matches || handledLink.current === linked || !connectionReady || pending || run.busy
      || getTerminalContextKey() !== device || getSelectedDeviceId() !== linked.device) return;
    if (linked.session !== storedId) {
      if (attemptedLink.current === linked) return;
      attemptedLink.current = linked;
      void openSession(linked.session);
      return;
    }
    // Selection alone is not acknowledgement: a failed/pending resume has no live chat.
    if (!sessionRef.current || chatSelection.current.storedId !== linked.session) return;
    handledLink.current = linked;
    if (linked.attention) setAttentionOpen(true);
  }, [linked, connectionReady, pending, run.busy, storedId, liveId, device]);

  // Pending decisions own the fixed footer; optional task intents stay in the scrollable transcript.
  const intentActions = run.busy && <div className="hermes-intent-actions"><button type="button" className="btn btn-secondary" disabled={!!pending || !!chatAttachments.length || !text.trim() || !controlCaps?.steer || !connectionReady} onClick={steerNow}>Уточнить сейчас</button><button type="button" className="btn btn-secondary" disabled={!!pending || !!chatAttachments.length || !text.trim() || !controlCaps?.queue || !connectionReady} onClick={queueNext}>Следующая задача</button>{(!controlCaps?.steer||!controlCaps?.queue)&&<small>Возможность не объявлена установленным Hermes. Обновите его; черновик сохранён.</small>}</div>;

  return <div data-keyboard={viewport.keyboard} data-view={view} style={{"--hermes-viewport-height":`${viewport.height}px`, "--hermes-viewport-top":`${viewport.top}px`} as import("react").CSSProperties} className={`page hermes-page${desktopPanel && activePanel && activePanel !== "history" ? " has-side-panel" : ""}`}>
    <header className="hermes-header">
      {view === "work" ? <button type="button" className="hermes-icon-button hermes-menu-button" aria-label="Вернуться в чат" title="Вернуться в чат" onClick={() => setView("chat")}><span aria-hidden="true">←</span></button> : <button type="button" className="hermes-icon-button hermes-menu-button" onClick={() => desktopPanel ? document.getElementById("hermes-chat-search")?.focus() : togglePanel("history")} aria-expanded={historyOpen || desktopPanel} aria-label="История чатов"><IconList /></button>}
      <div className="hermes-identity"><h1 className="hermes-header-title" title={view === "work" ? "Ход работы" : chatTitle}>{view === "work" ? "Ход работы" : chatTitle}</h1>
        <button type="button" className="hermes-identity-context" aria-label="Контекст беседы и навигация" aria-expanded={composerMenuOpen} onClick={() => setComposerMenuOpen(true)} title={`${deviceLabel} · ${cwd || "Папка не выбрана"}`}>{viewport.keyboard ? <span className="hermes-identity-host">Разделы</span> : <><span className="hermes-identity-host">{deviceLabel}</span><span aria-hidden="true">·</span><span className="hermes-identity-folder">{folderName || "Без папки"}</span></>}<span aria-hidden="true">⌄</span></button></div>
      <button type="button" className="hermes-chat-menu-button" aria-label="Ход работы" onClick={() => { closePanels(); setView(view === "work" ? "chat" : "work"); }}>Работа{verifiedFiles.length > 0 ? ` · ${verifiedFiles.length}` : ""}</button>
    </header>
    <div className="hermes-workspace"><HermesChatSidebar desktop={desktopPanel} open={historyOpen} sessions={sessions} selectedId={storedId} disabled={!!pending || run.busy} ready={!!status?.ready} deviceName={deviceLabel} historyError={historyError} onClose={() => setHistoryOpen(false)} onNewChat={newChat} onSelect={id => { setView("chat"); void openSession(id); }} onSettings={() => togglePanel("settings")} onConnection={() => togglePanel("connection")} onCommands={() => togglePanel("commands")} onFolder={() => { closePanels(); setFolderOpen(true); }} /><main className="hermes-content">

      <div className="hermes-scroll" ref={scrollArea}><div className="hermes-scroll-body">
      {statusError && <div className="hermes-error" role="alert"><p>{statusError}</p>{hostNeedsUpdate
        ? <button type="button" className="btn btn-secondary" onClick={() => navigate("/settings?section=connection")}>Открыть обновления Remotai</button>
        : <button type="button" className="btn btn-secondary" onClick={() => void refreshStatus()}>Проверить снова</button>}</div>}
      {(error || run.error || streamError) && <div className="hermes-error" role="alert"><p>{error || run.error || streamError}</p>{!run.error && <button type="button" className="btn btn-secondary" onClick={retryConnection}>Проверить подключение</button>}</div>}
      {modelCheck === "error" && status?.ready && !showConnection && <div className="hermes-error" role="alert"><p>Не удалось проверить подключение модели. Беседа и черновик сохранены.</p><button type="button" className="btn btn-secondary" onClick={retryConnection}>Повторить проверку</button></div>}
      {notice && <p className="hermes-notice" role="status">{notice}</p>}
      {linked && !linked.matches && <div className="hermes-notice" role="status"><p>Уведомление относится к компьютеру {linked.device}, а не к текущей машине. Ответы и задачи не перенаправляются автоматически.</p><button type="button" className="btn btn-secondary" disabled={!!pending || getMode()!=="cloud"} onClick={()=>void act("linked-device",async()=>{const devices=await listDevicesCached();const target=devices.find(item=>item.id===linked.device);if(!target)throw new HermesUserError("Этот компьютер недоступен вашему аккаунту. Проверьте привязку.");selectDevice(target.id,target);})}>Открыть компьютер из уведомления</button></div>}

      {attentionOpen && <HermesPanel desktop={desktopPanel} title="Требует внимания" onClose={closePanels}><section className="hermes-panel" aria-label="Входящие Hermes">
        <p className="hermes-muted">Сохранено на {deviceLabel}. Закрытие страницы не останавливает работу. Неизвестный исход не означает успех.</p>
        {controlError && <p role="alert">{controlError}</p>}
        <button type="button" className="btn btn-secondary" onClick={()=>void refreshControl()}>Обновить входящие</button>
        {(controlState?.attention || []).slice().reverse().map(item => {
          const prompt=item.state==="pending" && item.generation===status?.backend_generation ? promptFromFrame({id:item.request_id,method:item.method,params:item.params || {}}) : null;
          return <article key={item.id} className="hermes-inbox-item" data-attention-id={item.id}>
            <small>{item.kind} · {item.state}{item.delivery ? ` · доставка: ${item.delivery}` : ""}</small>
            {prompt && ["approval","question"].includes(prompt.kind) ? <HermesRequest prompt={prompt} disabled={!!pending} deviceName={deviceLabel} cwd="" onReply={result=>void answerAttention(item,result)} /> : <p>{item.state==="expired"?"Запрос больше не действует":item.state==="uncertain"?"Исход ответа неизвестен. Не повторяем автоматически.":item.kind==="completed"?"Задача завершена Hermes":item.kind==="failed"?"Hermes сообщил об ошибке":item.kind==="interrupted"?"Задача прервана; проверьте историю":item.state==="pending"?"Откройте беседу для защищённого запроса":"Запрос закрыт"}</p>}
            {item.state==="unread" && <button type="button" className="hermes-context-button" disabled={!!pending} onClick={()=>void act("inbox-read",async()=>{await client.readAttention(item.id);await refreshControl();})}>Отметить прочитанным</button>}
            {item.stored_session_id && <button type="button" className="hermes-context-button" disabled={!!pending} onClick={()=>{closePanels();void openSession(item.stored_session_id);}}>Открыть беседу</button>}
            <details><summary>Источник</summary><small>run {item.run_id || "не предоставлен"} · epoch {item.generation} · request {item.request_id || item.id}</small></details>
          </article>;
        })}
        {controlState && !controlState.attention.length && <p>Нет запросов, требующих внимания.</p>}
        <h3>Журнал задач</h3>{(controlState?.tasks || []).slice().reverse().map(task=><article className="hermes-inbox-item" key={task.run_id}><b>{task.state}{task.native_status?` · Hermes: ${task.native_status}`:" · нативное подтверждение ожидается"}</b><p>run {task.run_id}</p>{task.state==="interrupted"&&<p>Исход может быть неоднозначным. Автоматического повтора нет; сначала проверьте историю и выполненные действия.</p>}{task.stored_session_id&&<button type="button" className="hermes-context-button" disabled={!!pending} onClick={()=>{closePanels();void openSession(task.stored_session_id);}}>Проверить беседу</button>}</article>)}
      </section></HermesPanel>}

      {settingsOpen && <HermesPanel desktop={desktopPanel} title="Настройки Hermes" onClose={closePanels}><section className="hermes-panel" aria-label="Настройки Hermes">
        <h2>Hermes на этом компьютере</h2>
        <HermesAutomation client={client} ready={!!status?.ready} busy={run.busy || !!pending || runtimeBusy(status)} cwd={cwd} deviceName={deviceLabel} sessionId={storedId}
          styleDraft={styleDraft} onStyleDraft={setStyleDraft} onChooseTaskFolder={(cwd, onPick) => { setTaskFolderPicker({ cwd, onPick }); setFolderOpen(true); }}
          onChooseFolder={() => setFolderOpen(true)} onOpenResult={id => { setView("chat"); void openSession(id); }}
          onPreparePrompt={value => { setText(current => current.trim() ? `${current}\n\n${value}` : value); setView("chat"); closePanels(); requestAnimationFrame(() => textarea.current?.focus()); }}
          contextAvailable={contextAvailable} contextUnavailableReason={commandUnavailableReason}
          onInspectContext={async () => { if (!contextAvailable) throw new HermesUserError(commandUnavailableReason); if (!sessionRef.current) throw new HermesUserError("Сначала откройте чат с Hermes."); const result = await client.rpc<CommandResult>("slash.exec", { profile: "default", session_id: sessionRef.current, command: "/context" }); return result.output || result.message || result.notice || result.warning || "Hermes не вернул сведения о контексте."; }} />
        {status?.ownership === "managed" && <p className="hermes-muted">Канал Hermes: основной (main). Здесь изменения появляются раньше, чем в стабильных выпусках.</p>}
        <div className="hermes-setting-row"><div><b>Запускать вместе с Remotai</b><p>Только эту управляемую копию. После сбоя задачи с неизвестным исходом не отправляются заново.</p></div><button type="button" role="switch" aria-label="Запускать Hermes вместе с Remotai" aria-checked={status?.auto_start===true} disabled={!status || status.ownership!=="managed" || !!pending} className="btn btn-secondary" onClick={()=>void act("autostart",async()=>acceptRuntimeStatus(await client.controlSettings({auto_start:!status?.auto_start})))}>{status?.auto_start?"Вкл":"Выкл"}</button></div>
        <div className="hermes-setting-row"><div><b>Приватные уведомления в Telegram</b><p>{status?.delivery_ready?"Уведомление без текста задачи, команды и файлов; ссылка на компьютер и беседу.":"Нужна привязка Telegram владельца компьютера и включённые уведомления Remotai. Подключение не доказывает доставку."}</p></div><button type="button" role="switch" aria-label="Доставлять уведомления Hermes в Telegram" aria-checked={status?.delivery_enabled===true} disabled={!status || !!pending || (!status.delivery_ready && !status.delivery_enabled)} className="btn btn-secondary" onClick={()=>void act("delivery",async()=>acceptRuntimeStatus(await client.controlSettings({delivery_enabled:!status?.delivery_enabled})))}>{status?.delivery_enabled?"Вкл":"Выкл"}</button></div>
        <div className="hermes-setting-row"><div><b>Автоматические обновления</b><p>Обновление устанавливается, когда Hermes свободен.</p></div><button type="button" className="hermes-switch-target" role="switch" aria-label="Автоматически обновлять Hermes" aria-checked={status?.auto_update === true} disabled={!status || !!pending || status.ownership === "external"} onClick={() => void act("settings", async () => { const next = await client.settings(!status?.auto_update); acceptRuntimeStatus(next); })}><span className={`settings-toggle ${status?.auto_update ? "on" : ""}`} aria-hidden="true"><span className="settings-toggle-knob" /></span></button></div>
        {status?.ownership === "external" && <><p className="hermes-muted">Найдена ваша установка Hermes. Для запуска здесь и автоматических обновлений подготовьте отдельную копию под управлением Remotai.</p><button type="button" className="btn btn-secondary" disabled={!!pending || status.running || run.busy || runtimeBusy(status)} onClick={() => void act("install", async () => { const next = await client.install(); acceptRuntimeStatus(next); })}>Установить копию Remotai</button></>}
        <div className="hermes-setting-row"><div><b>{status?.version ? `Версия ${status.version}` : "Версия Hermes"}</b><p>{status?.update_pending ? "Обновление ожидает завершения текущей работы." : status?.update_available ? `Доступно обновление${status.latest_version ? ` ${status.latest_version}` : ""}` : status?.ownership === "external" ? "Обновления выполняются вашим установщиком Hermes." : "Проверка доступна по кнопке."}</p></div><button type="button" className="btn btn-secondary" disabled={!status?.installed || status.ownership === "external" || !!pending || runtimeBusy(status)} onClick={() => void act("update", async () => { const next = status?.update_available ? await client.update() : await client.checkUpdate(); acceptRuntimeStatus(next); })}><IconRefresh size={16} />{status?.update_available ? "Обновить" : "Проверить"}</button></div>
        <button type="button" className="btn btn-secondary" onClick={() => togglePanel("commands")}>Команды Hermes</button>
      </section></HermesPanel>}

      {!!status && !status.ready && !statusError && <section className="hermes-setup" aria-label="Подготовка Hermes">
        <ol className="hermes-steps" aria-label="Первый запуск"><li aria-current="step">Подготовка</li><li>Вход в аккаунт</li><li>Ваш чат</li></ol>
        <h2>{runtimeBusy(status) ? "Готовим помощника" : "Помощник на вашем компьютере"}</h2>
        <p>{runtimeBusy(status) ? "Можно написать задачу ниже. Текст сохранится, пока идет подготовка." : "Hermes выполняет задачи на выбранном компьютере. Remotai подготовит его без команд в терминале."}</p>
        {!cwd && <button type="button" className="hermes-context-button" disabled={!!pending} onClick={() => setFolderOpen(true)}><IconFolder size={18} />Выбрать папку проекта</button>}
        <button type="button" className="btn btn-primary" onClick={() => void prepare()} disabled={!!pending || runtimeBusy(status)}>{pending === "prepare" || runtimeBusy(status) ? "Подготавливаем…" : status.installed && status.ownership !== "external" ? "Запустить Hermes" : "Подготовить Hermes"}</button>
        {status.operation && <p className="hermes-muted" role="status">{typeof status.operation === "string" ? status.operation_detail || runtimeLabel(status) : status.operation.message || status.operation_detail || runtimeLabel(status)}</p>}
        {status.ownership === "external" && !status.running && <p className="hermes-muted">Найдена другая установка. Remotai подготовит отдельную копию с автоматическими обновлениями.</p>}{status.last_error && <p className="hermes-error-text" role="alert">{status.last_error}</p>}
        {status.installed && status.last_error && !runtimeBusy(status) && <button type="button" className="btn btn-secondary" disabled={!!pending} onClick={() => void act("repair", async () => { const next = await client.install(); acceptRuntimeStatus(next); })}>Восстановить установку</button>}
      </section>}

      {showConnection && <HermesPanel desktop={desktopPanel} inline={needsAccount} title="Подключение" onClose={closePanels}><section className="hermes-panel hermes-connection" aria-label="Подключение модели">
        {needsAccount && <ol className="hermes-steps" aria-label="Первый запуск"><li className="is-complete">Подготовка</li><li aria-current="step">Вход в аккаунт</li><li>Ваш чат</li></ol>}
        <div className="hermes-panel-heading"><h2>{needsAccount ? "Подключите свой аккаунт" : "Подключение и модель"}</h2>{!needsAccount && <button type="button" className="hermes-context-button" onClick={() => setConnectionOpen(false)}>Готово</button>}</div>
        <p className="hermes-muted">Войдите в поддерживаемый аккаунт или добавьте API-ключ. Доступ и лимиты определяет выбранный сервис.</p>
        {loginError && <p className="hermes-error-text" role="alert">{loginError}</p>}
        {login ? <div className="hermes-login">
          <p>Откройте страницу входа и введите код:</p><div className="hermes-login-code"><code>{login.user_code}</code><button type="button" className="hermes-icon-button" aria-label="Скопировать код входа" onClick={() => void terminalClipboard.write(login.user_code).then(copied => copied ? setNotice("Код скопирован") : setLoginError("Не удалось скопировать код. Его можно выделить и скопировать вручную."))}><IconCopy size={18} /></button></div>
          <button type="button" className="btn btn-primary" onClick={() => { const url = safeVerificationUrl(login.verification_url); if (url) void openExternalLink(url); }}>Открыть страницу входа</button>
          <p className="hermes-muted" role="status">Ждем подтверждения. После входа вернитесь в Remotai.</p>
          <button type="button" className="hermes-context-button" onClick={() => { setLogin(null); setLoginError(""); }}>Выбрать другой способ</button>
        </div> : <div className="hermes-provider-list">{deviceProviders.map(item => <div className="hermes-provider-row" key={item.id}><div><b>{item.name}</b><small>{providerConnected(item) ? "Аккаунт подключен" : "Вход по коду, без API-ключа"}</small></div><button type="button" className="btn btn-secondary" disabled={!!pending} onClick={() => void connect(item)}>{providerConnected(item) ? "Войти заново" : "Войти"}</button></div>)}{providers.some(item => item.flow !== "device_code") && <details className="hermes-advanced"><summary>Другие способы входа</summary>{providers.filter(item => item.flow !== "device_code").map(item => <div className="hermes-provider-row" key={item.id}><div><b>{item.name}</b><small>{providerConnected(item) ? "Аккаунт подключен" : item.flow === "external" ? "Вход через приложение провайдера на компьютере" : "Настройка через Hermes"}</small></div></div>)}</details>}</div>}
        {!providers.length && !loginError && <p className="hermes-muted" role="status">Загружаем способы входа…</p>}
        {connectedProviders.length > 0 && <p className="hermes-muted">Подключено: {connectedProviders.map(item => item.name).join(", ")}.</p>}
        {models && (modelReady || connectedProviders.length > 0 || selectedProvider?.authenticated) && <details className="hermes-advanced" open={!!modelConfirm}><summary>Выбрать сервис и модель</summary><div className="hermes-model-fields"><label>Сервис<select value={provider} disabled={!!pending || run.busy} onChange={event => { const next = models.providers.find(item => item.slug === event.target.value); if (next) void changeModel(next.slug, next.models[0] || ""); }}><option value="">Выберите сервис</option>{models.providers.map(item => <option key={item.slug} value={item.slug}>{item.name}{item.authenticated ? " — подключен" : ""}</option>)}</select></label><label>Модель<select value={model} disabled={!selectedProvider || !!pending || run.busy} onChange={event => void changeModel(provider, event.target.value)}><option value="">Выберите модель</option>{model && !selectedProvider?.models.includes(model) && <option value={model}>{model}</option>}{selectedProvider?.models.map(name => <option key={name} value={name}>{name}</option>)}</select></label></div></details>}
        {selectedProvider?.warning && <p className="hermes-muted">{selectedProvider.warning}</p>}
        {modelConfirm && <div className="hermes-model-confirm"><p>{modelConfirm.message}</p><div className="hermes-request-actions"><button type="button" className="btn btn-secondary" disabled={!!pending} onClick={() => setModelConfirm(null)}>Отменить</button><button type="button" className="btn btn-primary" disabled={!!pending} onClick={() => void changeModel(modelConfirm.provider, modelConfirm.model, true)}>Подтвердить выбор</button></div></div>}
        {modelError && !modelReady && <p className="hermes-muted">{modelError}</p>}
        <button type="button" className="hermes-context-button" aria-expanded={apiKeyOpen} onClick={() => setApiKeyOpen(value => !value)}>У меня есть API-ключ</button>
        {apiKeyOpen && <form className="hermes-key-form" onSubmit={event => void saveKey(event)}><label>Сервис API<select value={keyProvider} onChange={event => setKeyProvider(event.target.value)} required><option value="">Выберите сервис</option>{keyProviders.map(item => <option key={item.slug} value={item.slug}>{item.name}</option>)}</select></label><label>API-ключ<input type="password" autoComplete="off" value={apiKey} onChange={event => setApiKey(event.target.value)} required placeholder="Вставьте ключ" /></label><p className="hermes-muted">Ключ сохранится на выбранном компьютере в настройках Hermes.</p><button type="submit" className="btn btn-secondary" disabled={!keyProvider || !apiKey.trim() || !!pending}>{pending === "key" ? "Сохраняем…" : "Подключить ключ"}</button></form>}
      </section></HermesPanel>}

      {commandsOpen && <HermesPanel desktop={desktopPanel} title="Команды Hermes" onClose={closePanels}><section className="hermes-panel" aria-label="Команды Hermes"><p className="hermes-muted">Навыки, память, инструменты и другие команды приходят из установленного Hermes.</p><label className="hermes-search"><IconSearch size={18} /><input type="search" value={commandQuery} onChange={event => setCommandQuery(event.target.value)} placeholder="Поиск команды или возможности" aria-label="Поиск команд Hermes" /></label>{catalogError && <p role="alert">{catalogError}</p>}{catalog?.warning && <p>{catalog.warning}</p>}<ul className="hermes-command-list">{commands.map(([name, description]) => <li key={name}><button type="button" disabled={!commandAvailable(name)} title={!commandAvailable(name) ? commandUnavailableReason : "Доступно без аргументов"} onClick={() => { setText(`/${name}`); closePanels(); textarea.current?.focus(); }}><b>/{name}</b><small>{description}{!commandAvailable(name) && " · Недоступно в Remotai"}</small></button></li>)}</ul>{catalog && !commands.length && <p className="hermes-muted">Команды не найдены. Попробуйте другое слово.</p>}</section></HermesPanel>}

      <section className="hermes-conversation" aria-label="Разговор с Hermes" hidden={view !== "chat"}>
        {!run.messages.length && !showConnection && status?.ready && connectionReady && <div className="hermes-empty"><div className="hermes-empty-heading"><h2>{cwd ? "Что сделаем в проекте?" : "С чего начнём?"}</h2><p>{cwd ? "Папка выбрана. Напишите, что нужно сделать." : "Выберите папку с проектом или начните разговор."}</p>{!cwd && <button type="button" className="hermes-project-start" disabled={!!pending || run.busy} onClick={() => setFolderOpen(true)}><IconFolder size={20} /><span>Выбрать папку проекта</span></button>}<small className="hermes-project-host">Файлы на компьютере: {deviceLabel}</small></div><div className="hermes-suggestions">{["Помоги разобраться в проекте", "Составь план моей задачи"].map(value => <button type="button" key={value} onClick={() => { setText(value); textarea.current?.focus(); }}><IconChat size={19} /><span>{value}</span></button>)}</div></div>}
        {run.messages.map(message => <HermesMessageView key={`${storedId}:${chatSelection.current.revision}:${message.id}`} message={message} streaming={run.streamId === message.id} streamKey={`${device}:${chatSelection.current.revision}:${message.id}`} showDetails={responseView === "details"} />)}
        {visibleActivities.length > 0 && <details className="hermes-activity" open={responseView === "details"}><summary>Действия беседы · {visibleActivities.length}</summary><p className="hermes-muted">Привязка исторических действий к отдельной задаче не предоставлена Hermes.</p><ol>{visibleActivities.map(item => <li key={item.id}><span>{item.complete && <IconCheck size={14} />}{item.text}</span>{item.details && <details><summary>Подробности</summary><pre>{item.details}</pre></details>}</li>)}</ol></details>}
        <HermesExecutionStatus completionOnly busy={run.busy} waiting={run.prompts.length > 0} progress="" answerStarted={false} hasAnswer={hasAnswer} restoring={restoringConversation || !!statusError || !!streamError || pending === "history"} subagents={confirmedSubagents} subagentError={subagentError} error={run.error} />
      </section>
      <section className="hermes-work-view" aria-label="Ход работы" hidden={view !== "work"}>
        <div className="hermes-work-header"><h2>Ход работы</h2><p>Результаты и выполнение в этой беседе.</p></div>
        <section className="hermes-work-section" aria-label="Проверенные результаты"><h3>Файлы и результаты</h3>{verifiedFiles.map(item=><article key={item.id} data-result-id={item.id} className="hermes-inbox-item"><b>{(item.path || item.tool || "Файл").split(/[\\/]/).pop()}</b><p>Файл проверен на компьютере · {item.outcome}</p><details><summary>Путь на компьютере</summary><p>{item.path || "Не предоставлен"}</p></details>{item.preview&&<details><summary>Предпросмотр</summary><pre>{item.preview}</pre></details>}{item.diff&&<details><summary>Diff, предоставленный Hermes (не проверка Git)</summary><pre>{item.diff}</pre></details>}{item.kind==="file"&&item.verified&&<button type="button" className="btn btn-secondary" disabled={!!pending} onClick={()=>void act("download",async()=>{const observed=observeChat();const file=await client.artifact(item.id);if(!isCurrentChat(observed))return;const bytes=Uint8Array.from(atob(file.base64),char=>char.charCodeAt(0));const result=await platform().saveBlob(new Blob([bytes]),file.name);if(!isCurrentChat(observed))return;if(result==="failed")throw new HermesUserError("Не удалось сохранить файл на этом устройстве.");setNotice("Проверенный файл передан устройству.");})}>Скачать проверенный файл</button>}<details><summary>Технические сведения</summary><small>run {item.run_id} · epoch {item.generation} · event {item.event_seq} · tool {item.tool_id}</small></details></article>)}</section>
        <section className="hermes-work-section" aria-label="Контекст работы"><h3>Где работает Hermes</h3><dl><div><dt>Компьютер</dt><dd>{deviceLabel}</dd></div><div><dt>Папка</dt><dd>{cwd || "Папка не выбрана · можно начать без нее"}</dd></div><div><dt>Модель</dt><dd>{modelLabel}</dd></div></dl><button type="button" className="hermes-context-button" disabled={run.busy || !!pending} onClick={() => setFolderOpen(true)}><IconFolder size={17} />{cwd ? "Изменить папку" : "Выбрать папку"}</button></section>
        <details className="hermes-work-section hermes-tool-journal" aria-label="Журнал инструментов"><summary>Журнал инструментов</summary>{toolResults.map(item => <article key={item.id} data-result-id={item.id} className="hermes-inbox-item"><b>{item.tool || "Инструмент Hermes"}</b><p>{item.outcome}{item.exit_code !== undefined ? ` · код ${item.exit_code}` : ""}</p>{item.path && <p>Указанный путь (файл не проверен): {item.path}</p>}{item.preview && <details><summary>Вывод инструмента</summary><pre>{item.preview}</pre></details>}{item.diff && <details><summary>Diff инструмента (не проверка Git)</summary><pre>{item.diff}</pre></details>}<details><summary>Технические сведения</summary><small>run {item.run_id} · epoch {item.generation} · event {item.event_seq} · tool {item.tool_id}</small></details></article>)}</details>

        {liveId && <HermesSubagentProgress children={childRows} selectedId={selectedChild} onSelect={setSelectedChild} error={subagentError} onRetry={()=>void refreshSubagents()} />}
        <section className="hermes-work-section" aria-label="Выполнение задачи"><h3>{run.busy ? "Задача выполняется" : "Действия Hermes"}</h3><p className={`hermes-runtime ${status?.ready ? "is-ready" : ""}`} role="status">{run.busy ? workingLabel : runtimeLabel(status)}</p>{visibleActivities.length ? <ol className="hermes-work-activities">{visibleActivities.map(item => <li key={item.id}><div>{item.complete && <IconCheck size={16} />}<span>{item.text}</span></div>{item.details && <details><summary>Подробности действия</summary><pre>{item.details}</pre></details>}</li>)}</ol> : <p className="hermes-muted">Здесь появятся действия, когда Hermes начнет выполнять вашу задачу.</p>}</section>
        <section className="hermes-work-section"><h3>Управление Hermes</h3><div className="hermes-work-actions"><button type="button" className="hermes-context-button" onClick={() => togglePanel("settings")}>Открыть настройки</button><button type="button" className="hermes-context-button" disabled={!status?.ready} onClick={() => togglePanel("commands")}>Выбрать команду</button></div></section>
        <details className="hermes-work-section hermes-diagnostics" aria-label="Готовность пульта"><summary>Готовность и диагностика</summary><p>Соединение: {status?.ready?"подключено":"не подключено"}. Аккаунт: {modelReady?"настроен":"требует настройки"}. Квота выбранной модели: неизвестна до реального запроса.</p><p>Уточнение: {controlCaps?.steer?"объявлено установленным Hermes":"не объявлено; обновите Hermes"}. Следующая задача: {controlCaps?.queue?"нативная очередь":"недоступна"}.</p><p>Каталог команд: {catalog ? "загружен" : "откройте команды и повторите проверку"}. Расписание: проверяйте heartbeat в настройках задач. Папка беседы: {cwd || "не предоставлена"}.</p><button type="button" className="btn btn-secondary" disabled={!liveId || !connectionReady || !!pending} onClick={()=>void act("readiness",async()=>{const observed=observeChat();const result=await client.controlReadiness(observed.liveId);if(isCurrentChat(observed))setControlReadiness(result);})}>Проверить инструменты без запроса модели</button>{controlReadiness&&<div><p>Инструменты: {controlReadiness.tools.status}{controlReadiness.tools.total!==undefined?` · ${controlReadiness.tools.total}`:""}. {controlReadiness.tools.remediation}</p>{controlReadiness.tools.sections?.map(section=><details key={section.name}><summary>{section.name}</summary>{section.tools.map(tool=><p key={tool.name}>{tool.name} · {tool.description}</p>)}</details>)}</div>}{controlError&&<p role="alert">{controlError}</p>}</details>
      </section>
      {intentActions}
      </div></div>
      {detached && <button type="button" className="hermes-jump-latest" onClick={jumpToLatest}>К последнему сообщению</button>}
      {run.prompts.length > 0 && <button type="button" className="hermes-decision-opener" aria-label={`Проверить запросы Hermes: ${run.prompts.length}`} onClick={() => openDecision(run.prompts[0])}>{run.prompts.some(prompt=>prompt.kind === "approval") ? "Нужно разрешение" : "Нужен ваш ответ"} · {run.prompts.length}<span>Проверить →</span></button>}
      <HermesExecutionStatus quietCompleted observableActivity={childActivity} onDetails={childRows.length ? openAssistantDetails : undefined} busy={run.busy} waiting={run.prompts.length > 0} progress={run.progressText} answerStarted={!!activeAnswer?.content} hasAnswer={hasAnswer} restoring={restoringConversation || !!statusError || !!streamError || pending === "history"} subagents={confirmedSubagents} subagentError={subagentError} error={run.error} onRetry={() => void refreshSubagents()}
        stopAction={<>{run.error && <span className="hermes-error hermes-recovery"><button type="button" aria-label="Проверить подключение" title="Проверить подключение" onClick={retryConnection}>Проверить подключение</button></span>}{run.busy && status?.ready && liveId && connectionReady && !statusError && !streamError && <button type="button" disabled={!!pending} onClick={() => void act("stop", async () => { const result = controlCaps?.interrupt ? await client.controlIntent("interrupt",liveId) : await client.rpc<{ status: string }>("session.interrupt", { profile: "default", session_id: liveId }); if (result.status === "not_interrupted") setNotice("Активное выполнение уже завершено."); })}>{pending === "stop" ? "Останавливаем…" : "Остановить"}</button>}</>} />
      {view === "chat" && <form className="hermes-composer" onSubmit={event => void send(event)} onDragOver={event => { if (event.dataTransfer.types.includes("Files")) { event.preventDefault(); event.dataTransfer.dropEffect = pending ? "none" : "copy"; } }} onDrop={event => { if (event.dataTransfer.files.length) { event.preventDefault(); addAttachments(Array.from(event.dataTransfer.files)); } }}>
        <input ref={filePicker} type="file" multiple hidden aria-label="Файлы для Hermes" onChange={event => { addAttachments(Array.from(event.target.files || [])); event.target.value = ""; }} />
        {chatAttachments.length > 0 && <div className="hermes-attachments" aria-label="Вложения">{chatAttachments.map(item => <div className="hermes-attachment" key={item.id}><span title={item.file.name}>{item.file.name}</span><small role="status">{item.error || (item.path ? "Готово" : `Загрузка ${Math.round(item.progress)}%`)}</small>{item.error && <button type="button" disabled={!!pending} onClick={() => void uploadAttachment({...item, controller: new AbortController()})}>Повторить</button>}<button type="button" aria-label={`Удалить ${item.file.name}`} disabled={!!pending} onClick={() => { uploads.current.get(item.id)?.abort(); attachmentRefs.current.delete(item.id); setAttachments(previous => previous.filter(row => row.id !== item.id)); }}><IconClose size={14} /></button></div>)}</div>}
        <div className="hermes-composer-row"><button type="button" className="hermes-icon-button hermes-composer-attach" aria-label="Прикрепить файлы" title="Прикрепить файлы" disabled={!!pending} onClick={() => filePicker.current?.click()}><svg width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.8" aria-hidden="true"><path d="m21 11-9 9a6 6 0 0 1-8-8L14 2a4 4 0 0 1 6 6L10 18a2 2 0 0 1-3-3l9-9" /></svg></button><label className="hermes-sr-only" htmlFor="hermes-task">Задача для Hermes</label>
        <textarea id="hermes-task" ref={textarea} onPaste={event => { const files = Array.from(event.clipboardData.files); if (files.length) { event.preventDefault(); addAttachments(files); } }} value={text} onChange={event => { preparedPrompt.current = null; setText(event.target.value); }} placeholder="Спросите Hermes" rows={1} onKeyDown={event => { if (!event.nativeEvent.isComposing && (event.ctrlKey || event.metaKey) && event.key === "Enter") { event.preventDefault(); void send(); } }} />
        <button type="submit" className="hermes-composer-send" aria-label={pending === "send" ? "Отправляем…" : "Отправить"} disabled={!status?.ready || !canSend || !modelReady}><IconSend size={19} /></button></div>
      </form>}
      {view === "chat" && (!status?.ready || !modelReady || restoringConversation) && <p className="hermes-composer-hint" role="status">{run.busy ? "Можно написать следующее сообщение." : restoringConversation ? "Возвращаемся к беседе. Черновик сохранен." : !status?.ready ? "Подготовка не отправит ваш черновик." : needsAccount ? "Черновик сохранен. Сначала войдите в аккаунт." : "Черновик сохранен. Проверяем подключение."}</p>}
    </main></div>
    {decision && decisionPrompt && isCurrentChat(decision.chat) && <SheetShell open onClose={() => setDecision(null)} className="hermes-sheet hermes-decision-sheet" overlayClassName="hermes-sheet-overlay" labelledBy="hermes-decision-title">
      <div className="hermes-panel-top"><h2 id="hermes-decision-title">{decisionPrompt.kind === "approval" ? "Разрешение" : "Ваш ответ"}</h2><button type="button" className="hermes-icon-button" aria-label="Ответить позже" onClick={() => setDecision(null)}><IconClose /></button></div>
      <HermesRequest key={`${device}:${decision.chat.revision}:${decision.chat.epoch}:${String(decisionPrompt.id)}`} prompt={decisionPrompt} disabled={!!pending} deviceName={deviceName} cwd={cwd} navigation={run.prompts.length > 1 && <nav className="hermes-decision-pager" aria-label="Ожидающие запросы">{run.prompts.map((prompt,index)=><button type="button" disabled={!!pending} key={String(prompt.id)} aria-current={prompt === decisionPrompt ? "true" : undefined} onClick={()=>openDecision(prompt)}>Запрос {index+1}</button>)}</nav>} onReply={result => {
        if (!isCurrentChat(decision.chat) || !currentPrompts.current.includes(decision.prompt)) { setDecision(null); setError("Запрос больше не действует. Ответ не отправлен."); return; }
        void respond(decision.prompt, result);
      }} />
    </SheetShell>}
    {composerMenuOpen && <SheetShell open onClose={() => setComposerMenuOpen(false)} className="hermes-sheet hermes-menu-sheet" overlayClassName="hermes-sheet-overlay" labelledBy="hermes-actions-title">
      <div className="hermes-panel-top"><h2 id="hermes-actions-title">Контекст и навигация</h2><button type="button" className="hermes-icon-button" aria-label="Закрыть меню чата" onClick={() => setComposerMenuOpen(false)}><IconClose /></button></div>
      {viewport.keyboard && <div className="hermes-context-navigation" aria-label="Навигация" onClick={event => { if ((event.target as HTMLElement).closest("button[data-nav-id]:not([data-nav-id='more'])")) setComposerMenuOpen(false); }}><h3>Разделы</h3><BottomNav active="hermes" /></div>}
      <div className="hermes-context-identity"><DeviceChip /><p>{cwd || "Папка не выбрана"}</p><small>{connectionReady ? "На связи" : "Соединение восстанавливается"}</small></div>
      <div className="hermes-composer-menu">
        <button type="button" aria-label="Ход работы" onClick={() => { setComposerMenuOpen(false); setView("work"); }}><span>Ход работы<small>Результаты, помощники и журнал инструментов</small></span></button>
        <button type="button" aria-label={`Требует внимания: ${attentionCount}`} onClick={() => togglePanel("attention")}><span>Требует внимания · {attentionCount}<small>Запросы разрешений и непрочитанные результаты</small></span></button>
        <button type="button" disabled={run.busy || !!pending} onClick={() => { setComposerMenuOpen(false); setFolderOpen(true); }}><IconFolder size={20} /><span>Рабочая папка<small>{cwd || "Работать без папки или выбрать проект"}</small></span></button>
        {!!cwd && !liveId && <button type="button" disabled={run.busy || !!pending} onClick={() => { setCwd(""); setComposerMenuOpen(false); }}>Работать без папки</button>}
        {run.messages.length > 0 && <button type="button" aria-pressed={responseView === "details"} onClick={() => { setResponseView(responseView === "text" ? "details" : "text"); setComposerMenuOpen(false); }}><span>Подробности ответа<small>{responseView === "details" ? "Включены · нажмите, чтобы свернуть" : "Скрыты · нажмите, чтобы показать"}</small></span></button>}
        <button type="button" disabled={!status?.ready} onClick={() => togglePanel("commands")}><span>Команды Hermes<small>Навыки, память и инструменты</small></span></button>
        <button type="button" onClick={() => togglePanel("connection")}><span>Модель и подключение<small>{modelLabel}</small></span></button>
        <button type="button" onClick={() => togglePanel("settings")}>Настройки Hermes</button>
      </div>
    </SheetShell>}
    <FolderNavSheet open={folderOpen} onClose={() => { setFolderOpen(false); setTaskFolderPicker(null); }} currentCwd={taskFolderPicker?.cwd ?? cwd} title={taskFolderPicker ? "Папка задачи на компьютере" : "Папка для Hermes на компьютере"} pickLabel="Выбрать" onPick={async path => { if (taskFolderPicker) { if (getTerminalContextKey() !== device) return false; taskFolderPicker.onPick(path); setTaskFolderPicker(null); setFolderOpen(false); return; } const observedChat = observeChat(); if (observedChat.liveId) { try { await client.rpc("session.cwd.set", { profile: "default", session_id: observedChat.liveId, cwd: path }); } catch (e) { if (isCurrentChat(observedChat)) setError(errorText(e)); return false; } } if (!isCurrentChat(observedChat)) return false; setCwd(path); setFolderOpen(false); }} />
    {!viewport.keyboard && <BottomNav active="hermes" />}
  </div>;
}

function HermesPanel({ desktop, inline = false, title, onClose, children }: { desktop: boolean; inline?: boolean; title: string; onClose: () => void; children: ReactNode }) {
  const heading = useId();
  if (inline) return <>{children}</>;
  const body = <><div className="hermes-panel-top"><h2 id={heading}>{title}</h2><button type="button" className="hermes-icon-button" aria-label="Закрыть панель" onClick={onClose}><IconClose size={20} /></button></div>{children}</>;
  return desktop ? <aside className="hermes-side-panel" aria-labelledby={heading}>{body}</aside> : <SheetShell open onClose={onClose} className="hermes-sheet" overlayClassName="hermes-sheet-overlay" labelledBy={heading}>{body}</SheetShell>;
}

function HermesMessageView({ message, streaming, streamKey, showDetails }: { streamKey: string; message: { id: string; role: string; content: string; reasoning?: string }; streaming: boolean; showDetails: boolean }) {
  const contentId = useId();
  const [expanded, setExpanded] = useState(streaming);
  useEffect(() => { if (streaming) setExpanded(true); }, [streaming]);
  const [copyStatus, setCopyStatus] = useState("");
  const assistant = message.role === "assistant" || message.role === "agent";
  const long = assistant && !streaming && (message.content.length > 1600 || message.content.split("\n").length > 18);
  return <article className={`hermes-message is-${message.role}`}><b>{message.role === "user" ? "Вы" : message.role === "system" ? "Hermes · сообщение" : "Hermes"}</b>{message.reasoning && <details className="hermes-reasoning" open={showDetails}><summary>Ход ответа</summary><div><HermesMarkdown>{message.reasoning}</HermesMarkdown></div></details>}<div id={contentId} className={`hermes-message-content${long && !expanded && !showDetails ? " is-folded" : ""}`}>{assistant ? <HermesMarkdown streaming={streaming} streamKey={streamKey}>{message.content}</HermesMarkdown> : message.content}</div>{assistant && message.content && <div className="hermes-message-actions">{long && !showDetails && <button type="button" className="hermes-context-button" aria-controls={contentId} aria-expanded={expanded} onClick={() => setExpanded(value => !value)}>{expanded ? "Свернуть" : "Показать полностью"}</button>}<button type="button" className="hermes-context-button" aria-label="Скопировать ответ Hermes" onClick={() => void terminalClipboard.write(message.content).then(copied => setCopyStatus(copied ? "Ответ скопирован" : "Не удалось скопировать. Выделите текст вручную."))}><IconCopy size={14} />Копировать</button>{copyStatus && <span role="status">{copyStatus}</span>}</div>}</article>;
}

function HermesRequest({ prompt, disabled, deviceName, cwd, navigation, onReply }: {
  prompt: HermesPrompt; disabled: boolean; deviceName: string; cwd: string; navigation?: ReactNode; onReply: (result: unknown) => void;
}) {
  const questions = (Array.isArray(prompt.params.questions) ? prompt.params.questions : []).map((value, index) => {
    const row = value as { qid?: string; question?: string; choices?: string[]; multi_select?: boolean };
    return { ...row, qid: row.qid || String(index) };
  });
  const [scope, setScope] = useState<"session" | "always" | null>(null);
  const [options, setOptions] = useState(false);
  const [acknowledged, setAcknowledged] = useState(false);
  const allowedSession = prompt.choices.includes("session") && prompt.params.allow_session !== false && prompt.params.smart_denied !== true;
  const allowedAlways = prompt.choices.includes("always") && prompt.params.allow_permanent !== false && prompt.params.allow_session !== false && prompt.params.smart_denied !== true;
  const [answers, setAnswers] = useState<Record<string, string>>({});
  const [raw, setRaw] = useState("");
  const [rawError, setRawError] = useState("");
  const [secret, setSecret] = useState("");
  const [identifier, setIdentifier] = useState("");
  const secretRequest = ["sudo", "secret", "vault.unlock_prompt", "vault.save_login", "vault.code"].includes(prompt.method);
  const secretLabel = prompt.method === "sudo" ? "Пароль для команды" : prompt.method === "vault.unlock_prompt"
    ? `Пароль ${String(prompt.params.display_name || "хранилища")}` : prompt.method === "vault.code" ? "Код подтверждения"
      : prompt.method === "secret" ? String(prompt.params.prompt || prompt.params.env_var || "Секретный ключ") : "Пароль для сайта";
  return <section className={`hermes-request${prompt.kind === "approval" ? " hermes-request-approval" : prompt.kind === "question" ? " hermes-request-question" : ""}`} aria-label={prompt.title}>{prompt.kind !== "question" && <div className="hermes-request-body">{navigation}<h3>{prompt.title}</h3><p className="hermes-muted">На {deviceName}{cwd ? ` · ${cwd}` : ""}</p>{prompt.description && <pre>{prompt.description}</pre>}
    {prompt.kind === "approval" && <dl className="hermes-approval-context">{[["Команда", prompt.params.command], ["Инструмент", prompt.params.tool || prompt.params.tool_name], ["Причина", prompt.params.reason], ["Папка запроса", prompt.params.cwd || cwd], ["Беседа", prompt.params.session_id]].map(([label, value]) => <div key={String(label)}><dt>{String(label)}</dt><dd>{typeof value === "string" && value ? value : "Не предоставлено Hermes"}</dd></div>)}</dl>}
      {(allowedSession || allowedAlways) && <><button type="button" className="hermes-context-button" aria-expanded={options} onClick={()=>setOptions(value=>!value)}>Другие варианты</button>{options && <div className="hermes-scope-options">{allowedSession && <button type="button" aria-pressed={scope === "session"} onClick={()=>{setScope("session");setAcknowledged(false);}}>На эту беседу</button>}{allowedAlways && <button type="button" aria-pressed={scope === "always"} onClick={()=>{setScope("always");setAcknowledged(false);}}>Всегда</button>}</div>}{scope && <div className="hermes-scope-confirm"><p>{scope === "session" ? "Разрешение действует для этой беседы." : "Hermes сохранит разрешение для следующих таких действий."}</p>{scope === "always" && <label className="hermes-answer-choice"><input type="checkbox" checked={acknowledged} onChange={event=>setAcknowledged(event.target.checked)} />Понимаю постоянный доступ</label>}<button type="button" className="btn btn-secondary" disabled={disabled || (scope === "always" && !acknowledged)} onClick={()=>onReply({choice:scope})}>{scope === "session" ? "Подтвердить: на эту беседу" : "Подтвердить: всегда"}</button></div>}</>}
    </div>}
    {secretRequest ? <form onSubmit={event => { event.preventDefault(); const value = prompt.method === "vault.save_login" ? JSON.stringify({ identifier, password: secret }) : secret; onReply({ value }); setSecret(""); setIdentifier(""); }}>
      {prompt.method === "vault.save_login" && <><p>{String(prompt.params.site || prompt.params.origin || "")}</p><label>Логин<input value={identifier} autoComplete="off" onChange={event => setIdentifier(event.target.value)} /></label></>}
      <label>{secretLabel}<input type="password" value={secret} autoComplete="new-password" onChange={event => setSecret(event.target.value)} /></label><p className="hermes-muted">Значение передается Hermes на этом компьютере и не сохраняется в чате или черновике.</p><div className="hermes-request-actions"><button type="button" className="btn btn-secondary" disabled={disabled} onClick={() => { setSecret(""); setIdentifier(""); onReply({ value: "" }); }}>Пропустить</button><button type="submit" className="btn btn-primary" disabled={disabled || !secret}>Передать Hermes</button></div>
    </form> : prompt.kind === "approval" ? <footer className="hermes-decision-footer"><div className="hermes-request-actions">{prompt.choices.includes("deny") && <button type="button" className="btn btn-secondary" disabled={disabled} aria-label="Отклонить" onClick={() => onReply({choice:"deny"})}><span className="hermes-deny-caption">Отклонить</span><span className="hermes-deny-compact" aria-hidden="true">Нет</span></button>}{prompt.choices.includes("once") && <button type="button" className="btn btn-primary" disabled={disabled} aria-label="Разрешить один раз" onClick={() => onReply({choice:"once"})}>Один раз</button>}</div>

    </footer>
      : prompt.kind === "question" ? <form onSubmit={event => { event.preventDefault(); onReply({ answers }); }}><div className="hermes-question-fields">{navigation}<h3>{prompt.title}</h3><p className="hermes-muted">На {deviceName}{cwd ? ` · ${cwd}` : ""}</p>{questions.map(question => <fieldset key={question.qid}><legend>{question.question}</legend>{question.choices?.map(choice => <label className="hermes-answer-choice" key={choice}><input type={question.multi_select ? "checkbox" : "radio"} name={question.qid} checked={question.multi_select ? (answers[question.qid] || "").split(", ").includes(choice) : answers[question.qid] === choice} onChange={event => setAnswers(previous => ({ ...previous, [question.qid]: question.multi_select ? (event.target.checked ? [...(previous[question.qid] || "").split(", ").filter(Boolean), choice] : (previous[question.qid] || "").split(", ").filter(value => value !== choice)).join(", ") : choice }))} />{choice}</label>)}<label className="hermes-answer-text">Свой ответ<input value={answers[question.qid] || ""} onChange={event => setAnswers(previous => ({ ...previous, [question.qid]: event.target.value }))} /></label></fieldset>)}</div><div className="hermes-request-actions"><button type="button" className="btn btn-secondary" disabled={disabled} aria-label="Отменить вопрос" onClick={() => onReply({})}>Отменить</button><button type="submit" className="btn btn-primary" disabled={disabled || !questions.every(question => !!answers[question.qid]?.trim())}>Ответить</button></div></form>
      : <form onSubmit={event => { event.preventDefault(); try { const value: unknown = JSON.parse(raw); setRawError(""); onReply(value); } catch { setRawError("Введите корректный JSON-ответ."); } }}><p>Hermes запросил действие «{prompt.method}». Подробности доступны ниже.</p><details><summary>Параметры запроса</summary><pre>{JSON.stringify(prompt.params, null, 2)}</pre></details><label>Ответ в формате JSON<textarea value={raw} onChange={event => setRaw(event.target.value)} placeholder="{}" rows={3} /></label>{rawError && <p role="alert">{rawError}</p>}<button type="submit" className="btn btn-secondary" disabled={disabled || !raw.trim()}>Отправить ответ</button></form>}
  </section>;
}
