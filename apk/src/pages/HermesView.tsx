import { t } from "@tgcontrol/shared";
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
  return mapApiError(error) || t("ui.hermesview.m577b862eab");
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
    if (getTerminalContextKey() !== device) throw new HermesUserError(t("ui.hermesview.mf5d172e52b"));
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
        ? t("ui.hermesview.m3236971045")
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
          setModelError(check.ok ? "" : check.error || t("ui.hermesview.m4958ab963e"));
        } catch (e) { if (!isCurrentChat(observedChat)) return; setModelReady(false); setModelCheck("error"); setModelError(errorText(e)); }
        if (isCurrentChat(observedChat)) setNotice(current => [t("ui.hermesview.m8008bb0f36"), t("ui.hermesview.mdb5c05cb2b")].includes(current) ? "" : current);
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
          } catch (e) { if (isCurrentChat(observedChat)) setError(t("ui.hermesview.m780120c821", { p0: (errorText(e)) })); }
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
      setSubagents(null); setSubagentError(t("ui.hermesview.me70636cfcb"));
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
    } catch (e) { if (alive.current && epoch === runtimeEpoch.current && revision === controlRevision.current) setControlError((e as {status?:number})?.status === 404 || (e as {status?:number})?.status === 501 ? t("ui.hermesview.ma45abddb50") : errorText(e)); }
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
    if (Date.now() >= login.expiresAt) { setLogin(null); setLoginError(t("ui.hermesview.mb51304deeb")); return; }
    try {
      const result = await client.pollLogin(login.provider, login.session_id);
      if (!alive.current) return;
      if (result.status === "approved") { setLogin(null); setNotice(t("ui.hermesview.m8008bb0f36")); await refreshInventory(login.provider); }
      else if (result.status !== "pending") { setLogin(null); setLoginError(result.error_message || t("ui.hermesview.m02707f2f89")); }
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
      if (alive.current) { acceptRuntimeStatus(result); setNotice(t("ui.hermesview.m2c849acfb3")); }
    });
  }

  async function connect(item: OAuthProvider) {
    await act("login", async () => {
      setLoginError("");
      const result = await client.login(item.id);
      if (result.flow !== "device_code" || !safeVerificationUrl(result.verification_url) || !result.session_id || !result.user_code) {
        throw new HermesUserError(t("ui.hermesview.m969a3daa9e"));
      }
      if (alive.current) setLogin({ ...result, provider: item.id, expiresAt: Date.now() + result.expires_in * 1000 });
    });
  }

  async function saveKey(event: FormEvent) {
    event.preventDefault();
    if (!keyProvider || !apiKey.trim()) return;
    await act("key", async () => {
      await client.rpc("model.save_key", { profile: "default", slug: keyProvider, api_key: apiKey.trim() });
      setApiKey(""); setNotice(t("ui.hermesview.mdb5c05cb2b"));
      await refreshInventory(keyProvider);
    });
  }

  async function ensureSession(observedChat: ChatIdentity): Promise<string> {
    if (!isCurrentChat(observedChat)) throw new HermesUserError(t("ui.hermesview.m5432ec36bd"));
    if (sessionRef.current) return sessionRef.current;
    const snapshot = await client.rpc<SessionSnapshot>("session.create", {
      profile: "default", source: "remotai", ...(cwd ? { cwd, cwd_explicit: true } : {}),
      ...(model ? { model } : {}), ...(provider ? { provider } : {}),
    });
    if (!isCurrentChat(observedChat) || !runtimeReady.current) throw new HermesUserError(t("ui.hermesview.mad8c6a8a3e"));
    if (!snapshot.session_id) throw new HermesUserError(t("ui.hermesview.mb36b89e54f"));
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
      if (!result.path) throw new HermesUserError(t("ui.hermesview.m2ab79d742f"));
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
        if (!availableReadCommand(catalog?.remotai_commands, submitted.trim())) throw new HermesUserError(t("ui.hermesview.me27b03ea8d"));
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
      if (!result.status && !result.voice_stopped) throw new HermesUserError(t("ui.hermesview.md9446cbea5"));
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
    if (["interrupted","failed"].includes(result.status)) throw new HermesUserError(t("ui.hermesview.mf30765005f"));
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
    setNotice(history ? t("ui.hermesview.mae371eb9ec") : t("ui.hermesview.me459caecf8"));
  }
  async function steerNow() {
    if (!text.trim() || chatAttachments.length || !liveId || !controlCaps?.steer) return;
    const submitted=text;
    await act("steer",async () => {
      const observed=observeChat();
      const result=await client.controlIntent("steer",observed.liveId,submitted);
      if (!isCurrentChat(observed)) return;
      if (result.status!=="queued" && result.status!=="delivered") throw new HermesUserError(t("ui.hermesview.m822a25bc15"));
      setNotice(result.status==="delivered"?t("ui.hermesview.md1518f525a"):t("ui.hermesview.mc57bec7417"));
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
      if (result.status !== "completed") setNotice(result.status==="accepted"?t("ui.hermesview.m98e1e2b568"):`Hermes: ${result.status}`);
      setText(current=>current===submitted?"":current);
      if (result.status !== "completed") void refreshControl();
    }, observed);
  }
  async function answerAttention(item: ControlAttention, result: unknown) {
    await act("answer",async()=>{
      if (item.generation !== runtimeEpoch.current || item.state !== "pending" || getTerminalContextKey() !== device) {
        void refreshControl(); throw new HermesUserError(t("ui.hermesview.mbdf8126f94"));
      }
      const observed = observeChat();
      try { await client.controlReply(item.id,result); }
      finally { if (getTerminalContextKey() === device) void refreshControl(); }
      if(isCurrentChat(observed))setRun(previous=>({...previous,prompts:previous.prompts.filter(prompt=>String(prompt.id)!==item.request_id)}));
    });
  }
  async function handleCommand(result: CommandResult, display: string, id: string, observed: ChatIdentity, depth = 0): Promise<void> {
    if (!isCurrentChat(observed)) return;
    if (depth > 5) throw new HermesUserError(t("ui.hermesview.m3ab89b5c00"));
    if (result.type === "alias" && result.target) {
      const target = result.target.startsWith("/") ? result.target : `/${result.target}`;
      const next = await client.rpc<CommandResult>("slash.exec", { profile: "default", session_id: id, command: target });
      if (!isCurrentChat(observed)) return;
      await handleCommand(next, display, id, observed, depth + 1); return;
    }
    if (result.type === "prefill") {
      const safeDisplay = result.display || result.message || display;
      preparedPrompt.current = result.message ? { display: safeDisplay, message: result.message, source: display } : null;
      setText(safeDisplay); setNotice(result.notice || t("ui.hermesview.m01a7dbaf07")); return;
    }
    if ((result.type === "send" || result.type === "skill") && result.message) {
      const accepted = await admitPrompt(id, result.message);
      if (!isCurrentChat(observed)) return;
      if (!accepted.status) throw new HermesUserError(t("ui.hermesview.m645ef8e424"));
      if (accepted.status === "completed") {
        void restoreCompletedReceipt(id, observed);
      } else setRun(previous => ({ ...previous, busy: true, messages: [...previous.messages, { id: `command-${Date.now()}`, role: "user", content: result.display || display }] }));
    } else if (result.output || result.notice || result.warning || result.type === "exec" || result.type === "plugin") {
      setRun(previous => ({ ...previous, messages: [...previous.messages, { id: `command-${Date.now()}`, role: "system", content: [result.output, result.notice, result.warning].filter(Boolean).join("\n") }] }));
    } else throw new HermesUserError(t("ui.hermesview.m04fecdb0b0"));
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
        if (result.confirm_required) { setModelConfirm({ provider: nextProvider, model: nextModel, message: result.confirm_message || t("ui.hermesview.mb2c6da6772") }); return; }
        if (result.deferred) { setNotice(t("ui.hermesview.mfe46e933bd")); return; }
      }
      setModelConfirm(null);
      setProvider(nextProvider); setModel(nextModel); selection.current = { provider: nextProvider, model: nextModel };
      const check = await client.rpc<RuntimeCheck>("setup.runtime_check", { profile: "default", provider: nextProvider });
      setModelReady(check.ok === true); setModelCheck(check.ok === true ? "ready" : check.ok === false ? "unconfigured" : "error"); setModelError(check.ok ? "" : check.error || t("ui.hermesview.mdee75552e8"));
    });
  }

  async function respond(prompt: HermesPrompt, result: unknown) {
    await act("answer", async () => {
      if (getTerminalContextKey() !== device || status?.backend_generation !== runtimeEpoch.current
        || (prompt.params.session_id && prompt.params.session_id !== sessionRef.current)) {
        throw new HermesUserError(t("ui.hermesview.mfe4757f77a"));
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
  const commandUnavailableReason = t("ui.hermesview.m963dc8b013");
  const commands = (catalog?.pairs || []).filter(([name, description]) => `${name} ${description}`.toLocaleLowerCase().includes(commandQuery.toLocaleLowerCase()));
  const unavailable = !!statusError || !status;
  const canSend = (!!text.trim() || chatAttachments.length > 0) && attachmentsReady && !pending && !run.busy && !runtimeBusy(status) && !unavailable && (!status?.ready || connectionReady);
  const needsAccount = modelCheck === "unconfigured";
  const restoringConversation = !hostNeedsUpdate && !statusError && (!status || (status.ready && !connectionReady) || (!status.ready && (!!storedId || run.busy)));
  const showConnection = !!status?.ready && ((needsAccount && connectionReady) || connectionOpen);
  const deviceName = humanDeviceName(getSelectedDeviceName(), t("ui.hermesview.mcf57f0b6dc"));
  const deviceLabel = humanDeviceName(getSelectedDeviceName(), t("devices.connectedComputer"));
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
  const workingLabel = run.prompts.length ? t("ui.hermesview.m048e9b9a00") : run.progressText || (activeAnswer?.content ? t("ui.hermesview.ma1627b3309") : t("ui.hermesview.m11297528a1"));
  const chatTitle = sessions.find(item => item.id === storedId)?.title || (storedId ? t("ui.hermeschatsidebar.m376b62b78f") : t("ui.hermeschatsidebar.m88c78c9ee6"));
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
    title: !recentChild.present ? t("ui.hermesview.m33b05ccfa6") : ["queued","pending"].includes(recentChild.rawStatus || "") ? t("ui.hermesview.m3edaf3ea86") : recentChild.status === "running" ? t("ui.hermesview.m0fbd7afbd8") : t("ui.hermesview.m068b46482b", { p0: (subagentStatusLabel(recentChild)) }),
    detail: latestNative ? t("ui.hermesview.mafab691574", { p0: (latestNative.kind === "tool_result" ? t("ui.hermesview.m25b7e56181") : t("ui.hermesview.m598639f607")), p1: (subagentToolLabel(latestNative.toolName)), p2: (latestNative.sourceTimeText ?? "") })
      : `${recentChild.lastTool ? t("ui.hermessubagentprogress.m5bf85ab4db", { p0: (subagentToolLabel(recentChild.lastTool)) }) : t("ui.hermesview.m10bbfbba5a")}${recentChild.goal ? ` · ${recentChild.goal}` : ""}`,
  } : undefined;
  function openAssistantDetails() {
    closePanels(); setSelectedChild(recentChild?.id || childRows[0]?.id || "");
    if (view === "work") { if (typeof document !== "undefined") document.getElementById("hermes-subagent-progress")?.scrollIntoView?.({block:"start"}); }
    else { focusAssistants.current = true;setView("work"); }
  }
  const visibleActivities = run.activities.filter(item=>!item.kind.startsWith("subagent."));
  const modelLabel = modelReady ? model || t("ui.hermesview.m1ad44fa159") : modelCheck === "checking" ? t("ui.hermesview.m7999c2d73f") : needsAccount ? t("ui.hermesview.m018620180a") : t("ui.hermesview.m1a74e3cd47");

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
  const intentActions = run.busy && <div className="hermes-intent-actions"><button type="button" className="btn btn-secondary" disabled={!!pending || !!chatAttachments.length || !text.trim() || !controlCaps?.steer || !connectionReady} onClick={steerNow}>{t("ui.hermesview.m98cc088a95")}</button><button type="button" className="btn btn-secondary" disabled={!!pending || !!chatAttachments.length || !text.trim() || !controlCaps?.queue || !connectionReady} onClick={queueNext}>{t("ui.hermesview.mf8afe0dc34")}</button>{(!controlCaps?.steer||!controlCaps?.queue)&&<small>{t("ui.hermesview.m2a32b97828")}</small>}</div>;

  return <div data-keyboard={viewport.keyboard} data-view={view} style={{"--hermes-viewport-height":`${viewport.height}px`, "--hermes-viewport-top":`${viewport.top}px`} as import("react").CSSProperties} className={`page hermes-page${desktopPanel && activePanel && activePanel !== "history" ? " has-side-panel" : ""}`}>
    <header className="hermes-header">
      {view === "work" ? <button type="button" className="hermes-icon-button hermes-menu-button" aria-label={t("ui.hermesview.md16035d8f1")} title={t("ui.hermesview.md16035d8f1")} onClick={() => setView("chat")}><span aria-hidden="true">←</span></button> : <button type="button" className="hermes-icon-button hermes-menu-button" onClick={() => desktopPanel ? document.getElementById("hermes-chat-search")?.focus() : togglePanel("history")} aria-expanded={historyOpen || desktopPanel} aria-label={t("ui.hermesview.m55e9b4af46")}><IconList /></button>}
      <div className="hermes-identity"><h1 className="hermes-header-title" title={view === "work" ? t("ui.hermesview.mc72d4830ee") : chatTitle}>{view === "work" ? t("ui.hermesview.mc72d4830ee") : chatTitle}</h1>
        <button type="button" className="hermes-identity-context" aria-label={t("ui.hermesview.m221a5024fb")} aria-expanded={composerMenuOpen} onClick={() => setComposerMenuOpen(true)} title={`${deviceLabel} · ${cwd || t("ui.hermesview.m9cd5ff536b")}`}>{viewport.keyboard ? <span className="hermes-identity-host">{t("more.sections")}</span> : <><span className="hermes-identity-host">{deviceLabel}</span><span aria-hidden="true">·</span><span className="hermes-identity-folder">{folderName || t("ui.hermesview.m60bf7f486b")}</span></>}<span aria-hidden="true">⌄</span></button></div>
      <button type="button" className="hermes-chat-menu-button" aria-label={t("ui.hermesview.mc72d4830ee")} onClick={() => { closePanels(); setView(view === "work" ? "chat" : "work"); }}>{t("ui.hermesview.m75436e3162")}{verifiedFiles.length > 0 ? ` · ${verifiedFiles.length}` : ""}</button>
    </header>
    <div className="hermes-workspace"><HermesChatSidebar desktop={desktopPanel} open={historyOpen} sessions={sessions} selectedId={storedId} disabled={!!pending || run.busy} ready={!!status?.ready} deviceName={deviceLabel} historyError={historyError} onClose={() => setHistoryOpen(false)} onNewChat={newChat} onSelect={id => { setView("chat"); void openSession(id); }} onSettings={() => togglePanel("settings")} onConnection={() => togglePanel("connection")} onCommands={() => togglePanel("commands")} onFolder={() => { closePanels(); setFolderOpen(true); }} /><main className="hermes-content">

      <div className="hermes-scroll" ref={scrollArea}><div className="hermes-scroll-body">
      {statusError && <div className="hermes-error" role="alert"><p>{statusError}</p>{hostNeedsUpdate
        ? <button type="button" className="btn btn-secondary" onClick={() => navigate("/settings?section=connection")}>{t("ui.hermesview.m57db025e28")}</button>
        : <button type="button" className="btn btn-secondary" onClick={() => void refreshStatus()}>{t("agentLaunch.recheck")}</button>}</div>}
      {(error || run.error || streamError) && <div className="hermes-error" role="alert"><p>{error || run.error || streamError}</p>{!run.error && <button type="button" className="btn btn-secondary" onClick={retryConnection}>{t("agentCheck.button")}</button>}</div>}
      {modelCheck === "error" && status?.ready && !showConnection && <div className="hermes-error" role="alert"><p>{t("ui.hermesview.m4250fd87b1")}</p><button type="button" className="btn btn-secondary" onClick={retryConnection}>{t("ui.hermesview.mf6346b37fe")}</button></div>}
      {notice && <p className="hermes-notice" role="status">{notice}</p>}
      {linked && !linked.matches && <div className="hermes-notice" role="status"><p>{t("ui.hermesview.mc972fdfd9a")}{linked.device}{t("ui.hermesview.m9c7e6c611e")}</p><button type="button" className="btn btn-secondary" disabled={!!pending || getMode()!=="cloud"} onClick={()=>void act("linked-device",async()=>{const devices=await listDevicesCached();const target=devices.find(item=>item.id===linked.device);if(!target)throw new HermesUserError(t("ui.hermesview.m7227a8d492"));selectDevice(target.id,target);})}>{t("ui.hermesview.m6ad14baeb6")}</button></div>}

      {attentionOpen && <HermesPanel desktop={desktopPanel} title={t("home.act.attention")} onClose={closePanels}><section className="hermes-panel" aria-label={t("ui.hermesview.mcddcfde451")}>
        <p className="hermes-muted">{t("ui.hermesview.m602420f01b")}{deviceLabel}{t("ui.hermesview.m99dca310f5")}</p>
        {controlError && <p role="alert">{controlError}</p>}
        <button type="button" className="btn btn-secondary" onClick={()=>void refreshControl()}>{t("ui.hermesview.mb5a10e1c99")}</button>
        {(controlState?.attention || []).slice().reverse().map(item => {
          const prompt=item.state==="pending" && item.generation===status?.backend_generation ? promptFromFrame({id:item.request_id,method:item.method,params:item.params || {}}) : null;
          return <article key={item.id} className="hermes-inbox-item" data-attention-id={item.id}>
            <small>{item.kind} · {item.state}{item.delivery ? t("ui.hermesview.mc9fa9e8e22", { p0: (item.delivery) }) : ""}</small>
            {prompt && ["approval","question"].includes(prompt.kind) ? <HermesRequest prompt={prompt} disabled={!!pending} deviceName={deviceLabel} cwd="" onReply={result=>void answerAttention(item,result)} /> : <p>{item.state==="expired"?t("ui.hermesview.m79d09c6540"):item.state==="uncertain"?t("ui.hermesview.m54b0a71a8f"):item.kind==="completed"?t("ui.hermesview.mdc5cfddb1f"):item.kind==="failed"?t("ui.state.m31ec01afb2"):item.kind==="interrupted"?t("ui.hermesview.mbb728467d2"):item.state==="pending"?t("ui.hermesview.m9c902aed84"):t("ui.hermesview.ma440b8e890")}</p>}
            {item.state==="unread" && <button type="button" className="hermes-context-button" disabled={!!pending} onClick={()=>void act("inbox-read",async()=>{await client.readAttention(item.id);await refreshControl();})}>{t("ui.hermesview.m1da96abc6a")}</button>}
            {item.stored_session_id && <button type="button" className="hermes-context-button" disabled={!!pending} onClick={()=>{closePanels();void openSession(item.stored_session_id);}}>{t("ui.hermesview.m4cddc0225c")}</button>}
            <details><summary>{t("ui.hermesview.m8290a3dbc0")}</summary><small>run {item.run_id || t("ui.hermesview.m4c0b57e443")} · epoch {item.generation} · request {item.request_id || item.id}</small></details>
          </article>;
        })}
        {controlState && !controlState.attention.length && <p>{t("ui.hermesview.mea49d9b9f9")}</p>}
        <h3>{t("ui.hermesview.m5b885fd261")}</h3>{(controlState?.tasks || []).slice().reverse().map(task=><article className="hermes-inbox-item" key={task.run_id}><b>{task.state}{task.native_status?` · Hermes: ${task.native_status}`:t("ui.hermesview.m7dc5b25aed")}</b><p>run {task.run_id}</p>{task.state==="interrupted"&&<p>{t("ui.hermesview.mc0bf672142")}</p>}{task.stored_session_id&&<button type="button" className="hermes-context-button" disabled={!!pending} onClick={()=>{closePanels();void openSession(task.stored_session_id);}}>{t("ui.hermesview.mb01f58ff8b")}</button>}</article>)}
      </section></HermesPanel>}

      {settingsOpen && <HermesPanel desktop={desktopPanel} title={t("ui.hermesview.md884937ec9")} onClose={closePanels}><section className="hermes-panel" aria-label={t("ui.hermesview.md884937ec9")}>
        <h2>{t("ui.hermesview.m1345848f6d")}</h2>
        <HermesAutomation client={client} ready={!!status?.ready} busy={run.busy || !!pending || runtimeBusy(status)} cwd={cwd} deviceName={deviceLabel} sessionId={storedId}
          styleDraft={styleDraft} onStyleDraft={setStyleDraft} onChooseTaskFolder={(cwd, onPick) => { setTaskFolderPicker({ cwd, onPick }); setFolderOpen(true); }}
          onChooseFolder={() => setFolderOpen(true)} onOpenResult={id => { setView("chat"); void openSession(id); }}
          onPreparePrompt={value => { setText(current => current.trim() ? `${current}\n\n${value}` : value); setView("chat"); closePanels(); requestAnimationFrame(() => textarea.current?.focus()); }}
          contextAvailable={contextAvailable} contextUnavailableReason={commandUnavailableReason}
          onInspectContext={async () => { if (!contextAvailable) throw new HermesUserError(commandUnavailableReason); if (!sessionRef.current) throw new HermesUserError(t("ui.hermesview.m25d8c6d0cc")); const result = await client.rpc<CommandResult>("slash.exec", { profile: "default", session_id: sessionRef.current, command: "/context" }); return result.output || result.message || result.notice || result.warning || t("ui.hermesview.mc579e8cfd5"); }} />
        {status?.ownership === "managed" && <p className="hermes-muted">{t("ui.hermesview.m2b5295d07c")}</p>}
        <div className="hermes-setting-row"><div><b>{t("ui.hermesview.m87286664bc")}</b><p>{t("ui.hermesview.mec23ebf2a2")}</p></div><button type="button" role="switch" aria-label={t("ui.hermesview.me03b50d96b")} aria-checked={status?.auto_start===true} disabled={!status || status.ownership!=="managed" || !!pending} className="btn btn-secondary" onClick={()=>void act("autostart",async()=>acceptRuntimeStatus(await client.controlSettings({auto_start:!status?.auto_start})))}>{status?.auto_start?t("ui.hermesview.ma60b4b83b2"):t("ui.hermesview.m622574d516")}</button></div>
        <div className="hermes-setting-row"><div><b>{t("ui.hermesview.m88a72af3ac")}</b><p>{status?.delivery_ready?t("ui.hermesview.m6b32ae13c2"):t("ui.hermesview.m9ae50b9623")}</p></div><button type="button" role="switch" aria-label={t("ui.hermesview.m2f80c2ea77")} aria-checked={status?.delivery_enabled===true} disabled={!status || !!pending || (!status.delivery_ready && !status.delivery_enabled)} className="btn btn-secondary" onClick={()=>void act("delivery",async()=>acceptRuntimeStatus(await client.controlSettings({delivery_enabled:!status?.delivery_enabled})))}>{status?.delivery_enabled?t("ui.hermesview.ma60b4b83b2"):t("ui.hermesview.m622574d516")}</button></div>
        <div className="hermes-setting-row"><div><b>{t("ui.hermesview.ma57faacf23")}</b><p>{t("ui.hermesview.m9cdfd15e96")}</p></div><button type="button" className="hermes-switch-target" role="switch" aria-label={t("ui.hermesview.mefee0ed2ad")} aria-checked={status?.auto_update === true} disabled={!status || !!pending || status.ownership === "external"} onClick={() => void act("settings", async () => { const next = await client.settings(!status?.auto_update); acceptRuntimeStatus(next); })}><span className={`settings-toggle ${status?.auto_update ? "on" : ""}`} aria-hidden="true"><span className="settings-toggle-knob" /></span></button></div>
        {status?.ownership === "external" && <><p className="hermes-muted">{t("ui.hermesview.mc9dc70ded1")}</p><button type="button" className="btn btn-secondary" disabled={!!pending || status.running || run.busy || runtimeBusy(status)} onClick={() => void act("install", async () => { const next = await client.install(); acceptRuntimeStatus(next); })}>{t("ui.hermesview.m8f61317753")}</button></>}
        <div className="hermes-setting-row"><div><b>{status?.version ? t("ui.hermesview.m0471a4c5b1", { p0: (status.version) }) : t("ui.hermesview.mbeac9e966e")}</b><p>{status?.update_pending ? t("ui.hermesview.mca14775e7e") : status?.update_available ? t("ui.hermesview.m0b728251a7", { p0: (status.latest_version ? ` ${status.latest_version}` : "") }) : status?.ownership === "external" ? t("ui.hermesview.mef8223b28c") : t("ui.hermesview.m20eebd482a")}</p></div><button type="button" className="btn btn-secondary" disabled={!status?.installed || status.ownership === "external" || !!pending || runtimeBusy(status)} onClick={() => void act("update", async () => { const next = status?.update_available ? await client.update() : await client.checkUpdate(); acceptRuntimeStatus(next); })}><IconRefresh size={16} />{status?.update_available ? t("agentUpdate.update") : t("agentCheck.check")}</button></div>
        <button type="button" className="btn btn-secondary" onClick={() => togglePanel("commands")}>{t("ui.hermesview.m869768f24e")}</button>
      </section></HermesPanel>}

      {!!status && !status.ready && !statusError && <section className="hermes-setup" aria-label={t("ui.hermesview.m82e9bc5249")}>
        <ol className="hermes-steps" aria-label={t("ui.hermesview.m7a0e97a32b")}><li aria-current="step">{t("ui.hermesview.m947023ee61")}</li><li>{t("ui.hermesview.mc1e9a4ea18")}</li><li>{t("ui.hermesview.mf6884101d2")}</li></ol>
        <h2>{runtimeBusy(status) ? t("ui.hermesview.m340f2ff91d") : t("ui.hermesview.mb17a91b542")}</h2>
        <p>{runtimeBusy(status) ? t("ui.hermesview.m1e8332247b") : t("ui.hermesview.mb839c5143e")}</p>
        {!cwd && <button type="button" className="hermes-context-button" disabled={!!pending} onClick={() => setFolderOpen(true)}><IconFolder size={18} />{t("ui.hermesautomation.m7bd26add53")}</button>}
        <button type="button" className="btn btn-primary" onClick={() => void prepare()} disabled={!!pending || runtimeBusy(status)}>{pending === "prepare" || runtimeBusy(status) ? t("ui.hermesview.mb18347b82d") : status.installed && status.ownership !== "external" ? t("ui.hermesview.mcec07b21d7") : t("ui.hermesview.mc1b55b71c1")}</button>
        {status.operation && <p className="hermes-muted" role="status">{typeof status.operation === "string" ? status.operation_detail || runtimeLabel(status) : status.operation.message || status.operation_detail || runtimeLabel(status)}</p>}
        {status.ownership === "external" && !status.running && <p className="hermes-muted">{t("ui.hermesview.mf49a0c62aa")}</p>}{status.last_error && <p className="hermes-error-text" role="alert">{status.last_error}</p>}
        {status.installed && status.last_error && !runtimeBusy(status) && <button type="button" className="btn btn-secondary" disabled={!!pending} onClick={() => void act("repair", async () => { const next = await client.install(); acceptRuntimeStatus(next); })}>{t("ui.hermesview.m69d069953f")}</button>}
      </section>}

      {showConnection && <HermesPanel desktop={desktopPanel} inline={needsAccount} title={t("ssh.help.connectTitle")} onClose={closePanels}><section className="hermes-panel hermes-connection" aria-label={t("ui.hermesview.m42673ebf0f")}>
        {needsAccount && <ol className="hermes-steps" aria-label={t("ui.hermesview.m7a0e97a32b")}><li className="is-complete">{t("ui.hermesview.m947023ee61")}</li><li aria-current="step">{t("ui.hermesview.mc1e9a4ea18")}</li><li>{t("ui.hermesview.mf6884101d2")}</li></ol>}
        <div className="hermes-panel-heading"><h2>{needsAccount ? t("ui.hermesview.m3a210a4ddb") : t("ui.hermesview.m42394801d5")}</h2>{!needsAccount && <button type="button" className="hermes-context-button" onClick={() => setConnectionOpen(false)}>{t("skills.res.head")}</button>}</div>
        <p className="hermes-muted">{t("ui.hermesview.m6a71e75498")}</p>
        {loginError && <p className="hermes-error-text" role="alert">{loginError}</p>}
        {login ? <div className="hermes-login">
          <p>{t("ui.hermesview.m8e8e07e066")}</p><div className="hermes-login-code"><code>{login.user_code}</code><button type="button" className="hermes-icon-button" aria-label={t("ui.hermesview.m9d3b4929b1")} onClick={() => void terminalClipboard.write(login.user_code).then(copied => copied ? setNotice(t("infra.invite.copied")) : setLoginError(t("ui.hermesview.mbdf31e170b")))}><IconCopy size={18} /></button></div>
          <button type="button" className="btn btn-primary" onClick={() => { const url = safeVerificationUrl(login.verification_url); if (url) void openExternalLink(url); }}>{t("ui.hermesview.m64de4e1171")}</button>
          <p className="hermes-muted" role="status">{t("ui.hermesview.m0962b29bd6")}</p>
          <button type="button" className="hermes-context-button" onClick={() => { setLogin(null); setLoginError(""); }}>{t("ui.hermesview.m14fcb8bf11")}</button>
        </div> : <div className="hermes-provider-list">{deviceProviders.map(item => <div className="hermes-provider-row" key={item.id}><div><b>{item.name}</b><small>{providerConnected(item) ? t("ui.hermesview.ma7c7cd28ae") : t("ui.hermesview.mf119a3ccbf")}</small></div><button type="button" className="btn btn-secondary" disabled={!!pending} onClick={() => void connect(item)}>{providerConnected(item) ? t("ui.hermesview.m6f6ae06722") : t("agentLaunch.accountSignIn")}</button></div>)}{providers.some(item => item.flow !== "device_code") && <details className="hermes-advanced"><summary>{t("ui.hermesview.m710e142854")}</summary>{providers.filter(item => item.flow !== "device_code").map(item => <div className="hermes-provider-row" key={item.id}><div><b>{item.name}</b><small>{providerConnected(item) ? t("ui.hermesview.ma7c7cd28ae") : item.flow === "external" ? t("ui.hermesview.mf656c3e4ff") : t("ui.hermesview.mbbfe51e820")}</small></div></div>)}</details>}</div>}
        {!providers.length && !loginError && <p className="hermes-muted" role="status">{t("ui.hermesview.m0abc863a94")}</p>}
        {connectedProviders.length > 0 && <p className="hermes-muted">{t("ui.hermesview.m0aaeb8eee1")}{connectedProviders.map(item => item.name).join(", ")}.</p>}
        {models && (modelReady || connectedProviders.length > 0 || selectedProvider?.authenticated) && <details className="hermes-advanced" open={!!modelConfirm}><summary>{t("ui.hermesview.m3e548cf4db")}</summary><div className="hermes-model-fields"><label>{t("ui.hermesview.m8149459e8a")}<select value={provider} disabled={!!pending || run.busy} onChange={event => { const next = models.providers.find(item => item.slug === event.target.value); if (next) void changeModel(next.slug, next.models[0] || ""); }}><option value="">{t("ui.hermesview.mf89437a2f4")}</option>{models.providers.map(item => <option key={item.slug} value={item.slug}>{item.name}{item.authenticated ? t("ui.hermesview.mfd527d6668") : ""}</option>)}</select></label><label>{t("agentCheck.field.model")}<select value={model} disabled={!selectedProvider || !!pending || run.busy} onChange={event => void changeModel(provider, event.target.value)}><option value="">{t("ui.hermesview.m78923c3f0a")}</option>{model && !selectedProvider?.models.includes(model) && <option value={model}>{model}</option>}{selectedProvider?.models.map(name => <option key={name} value={name}>{name}</option>)}</select></label></div></details>}
        {selectedProvider?.warning && <p className="hermes-muted">{selectedProvider.warning}</p>}
        {modelConfirm && <div className="hermes-model-confirm"><p>{modelConfirm.message}</p><div className="hermes-request-actions"><button type="button" className="btn btn-secondary" disabled={!!pending} onClick={() => setModelConfirm(null)}>{t("devices.switcher.undo")}</button><button type="button" className="btn btn-primary" disabled={!!pending} onClick={() => void changeModel(modelConfirm.provider, modelConfirm.model, true)}>{t("ui.hermesview.mfd26f9fd11")}</button></div></div>}
        {modelError && !modelReady && <p className="hermes-muted">{modelError}</p>}
        <button type="button" className="hermes-context-button" aria-expanded={apiKeyOpen} onClick={() => setApiKeyOpen(value => !value)}>{t("ui.hermesview.m738ec76d35")}</button>
        {apiKeyOpen && <form className="hermes-key-form" onSubmit={event => void saveKey(event)}><label>{t("ui.hermesview.m8b08155064")}<select value={keyProvider} onChange={event => setKeyProvider(event.target.value)} required><option value="">{t("ui.hermesview.mf89437a2f4")}</option>{keyProviders.map(item => <option key={item.slug} value={item.slug}>{item.name}</option>)}</select></label><label>{t("ui.hermesview.m0956cb8353")}<input type="password" autoComplete="off" value={apiKey} onChange={event => setApiKey(event.target.value)} required placeholder={t("ui.hermesview.mdbf8bbe621")} /></label><p className="hermes-muted">{t("ui.hermesview.me160d3f07f")}</p><button type="submit" className="btn btn-secondary" disabled={!keyProvider || !apiKey.trim() || !!pending}>{pending === "key" ? t("ui.hermesautomation.m73bfb5e424") : t("ui.hermesview.m21e222da26")}</button></form>}
      </section></HermesPanel>}

      {commandsOpen && <HermesPanel desktop={desktopPanel} title={t("ui.hermesview.m869768f24e")} onClose={closePanels}><section className="hermes-panel" aria-label={t("ui.hermesview.m869768f24e")}><p className="hermes-muted">{t("ui.hermesview.md1c2c61031")}</p><label className="hermes-search"><IconSearch size={18} /><input type="search" value={commandQuery} onChange={event => setCommandQuery(event.target.value)} placeholder={t("ui.hermesview.m9a79cb8f34")} aria-label={t("ui.hermesview.m49fcf6ae19")} /></label>{catalogError && <p role="alert">{catalogError}</p>}{catalog?.warning && <p>{catalog.warning}</p>}<ul className="hermes-command-list">{commands.map(([name, description]) => <li key={name}><button type="button" disabled={!commandAvailable(name)} title={!commandAvailable(name) ? commandUnavailableReason : t("ui.hermesview.m5b7ac4472b")} onClick={() => { setText(`/${name}`); closePanels(); textarea.current?.focus(); }}><b>/{name}</b><small>{description}{!commandAvailable(name) && t("ui.hermesview.m11f6cbcd0a")}</small></button></li>)}</ul>{catalog && !commands.length && <p className="hermes-muted">{t("ui.hermesview.m197fd991fc")}</p>}</section></HermesPanel>}

      <section className="hermes-conversation" aria-label={t("ui.hermesview.mf43b4afd9d")} hidden={view !== "chat"}>
        {!run.messages.length && !showConnection && status?.ready && connectionReady && <div className="hermes-empty"><div className="hermes-empty-heading"><h2>{cwd ? t("ui.hermesview.mc79ba23b71") : t("ui.hermesview.m8bcdeae318")}</h2><p>{cwd ? t("ui.hermesview.mea144f03c7") : t("ui.hermesview.maf2bcfbd22")}</p>{!cwd && <button type="button" className="hermes-project-start" disabled={!!pending || run.busy} onClick={() => setFolderOpen(true)}><IconFolder size={20} /><span>{t("ui.hermesautomation.m7bd26add53")}</span></button>}<small className="hermes-project-host">{t("ui.hermesview.m65328ca8ea")}{deviceLabel}</small></div><div className="hermes-suggestions">{[t("ui.hermesview.maaa43e0731"), t("ui.hermesview.m60fc3871f4")].map(value => <button type="button" key={value} onClick={() => { setText(value); textarea.current?.focus(); }}><IconChat size={19} /><span>{value}</span></button>)}</div></div>}
        {run.messages.map(message => <HermesMessageView key={`${storedId}:${chatSelection.current.revision}:${message.id}`} message={message} streaming={run.streamId === message.id} streamKey={`${device}:${chatSelection.current.revision}:${message.id}`} showDetails={responseView === "details"} />)}
        {visibleActivities.length > 0 && <details className="hermes-activity" open={responseView === "details"}><summary>{t("ui.hermesview.m3eddb993b0")}{visibleActivities.length}</summary><p className="hermes-muted">{t("ui.hermesview.m739fd6724f")}</p><ol>{visibleActivities.map(item => <li key={item.id}><span>{item.complete && <IconCheck size={14} />}{item.text}</span>{item.details && <details><summary>{t("ui.hermesview.med2f45185b")}</summary><pre>{item.details}</pre></details>}</li>)}</ol></details>}
        <HermesExecutionStatus completionOnly busy={run.busy} waiting={run.prompts.length > 0} progress="" answerStarted={false} hasAnswer={hasAnswer} restoring={restoringConversation || !!statusError || !!streamError || pending === "history"} subagents={confirmedSubagents} subagentError={subagentError} error={run.error} />
      </section>
      <section className="hermes-work-view" aria-label={t("ui.hermesview.mc72d4830ee")} hidden={view !== "work"}>
        <div className="hermes-work-header"><h2>{t("ui.hermesview.mc72d4830ee")}</h2><p>{t("ui.hermesview.m26e7c300ca")}</p></div>
        <section className="hermes-work-section" aria-label={t("ui.hermesview.mc90a85675b")}><h3>{t("ui.hermesview.m84022ceb9d")}</h3>{verifiedFiles.map(item=><article key={item.id} data-result-id={item.id} className="hermes-inbox-item"><b>{(item.path || item.tool || t("ui.hermesview.m94b8df93b6")).split(/[\\/]/).pop()}</b><p>{t("ui.hermesview.mfafd7af667")}{item.outcome}</p><details><summary>{t("ui.hermesview.m474f7b793d")}</summary><p>{item.path || t("ui.hermesview.md09731bf6e")}</p></details>{item.preview&&<details><summary>{t("ui.hermesview.m5d092ca855")}</summary><pre>{item.preview}</pre></details>}{item.diff&&<details><summary>{t("ui.hermesview.m506ee9286c")}</summary><pre>{item.diff}</pre></details>}{item.kind==="file"&&item.verified&&<button type="button" className="btn btn-secondary" disabled={!!pending} onClick={()=>void act("download",async()=>{const observed=observeChat();const file=await client.artifact(item.id);if(!isCurrentChat(observed))return;const bytes=Uint8Array.from(atob(file.base64),char=>char.charCodeAt(0));const result=await platform().saveBlob(new Blob([bytes]),file.name);if(!isCurrentChat(observed))return;if(result==="failed")throw new HermesUserError(t("ui.hermesview.m78646a0f4f"));setNotice(t("ui.hermesview.m5103c0b2b3"));})}>{t("ui.hermesview.mbcd77a3fd9")}</button>}<details><summary>{t("ui.hermesview.m406e037f90")}</summary><small>run {item.run_id} · epoch {item.generation} · event {item.event_seq} · tool {item.tool_id}</small></details></article>)}</section>
        <section className="hermes-work-section" aria-label={t("ui.hermesview.md70a3424b1")}><h3>{t("ui.hermesview.m90f005f22a")}</h3><dl><div><dt>{t("infra.local.thisPcName")}</dt><dd>{deviceLabel}</dd></div><div><dt>{t("agentSessions.confirmFolder")}</dt><dd>{cwd || t("ui.hermesview.m81f54abac7")}</dd></div><div><dt>{t("agentCheck.field.model")}</dt><dd>{modelLabel}</dd></div></dl><button type="button" className="hermes-context-button" disabled={run.busy || !!pending} onClick={() => setFolderOpen(true)}><IconFolder size={17} />{cwd ? t("ui.hermesview.m4c21650c0d") : t("settings.pickFolder")}</button></section>
        <details className="hermes-work-section hermes-tool-journal" aria-label={t("ui.hermesview.meac40e62a1")}><summary>{t("ui.hermesview.meac40e62a1")}</summary>{toolResults.map(item => <article key={item.id} data-result-id={item.id} className="hermes-inbox-item"><b>{item.tool || t("ui.hermessubagentprogress.m69371bfec9")}</b><p>{item.outcome}{item.exit_code !== undefined ? t("ui.hermesview.m0961873563", { p0: (item.exit_code) }) : ""}</p>{item.path && <p>{t("ui.hermesview.m80cf10dc0a")}{item.path}</p>}{item.preview && <details><summary>{t("ui.hermesview.m879621fbc9")}</summary><pre>{item.preview}</pre></details>}{item.diff && <details><summary>{t("ui.hermesview.m70842d3524")}</summary><pre>{item.diff}</pre></details>}<details><summary>{t("ui.hermesview.m406e037f90")}</summary><small>run {item.run_id} · epoch {item.generation} · event {item.event_seq} · tool {item.tool_id}</small></details></article>)}</details>

        {liveId && <HermesSubagentProgress children={childRows} selectedId={selectedChild} onSelect={setSelectedChild} error={subagentError} onRetry={()=>void refreshSubagents()} />}
        <section className="hermes-work-section" aria-label={t("ui.hermesview.m253b427b9d")}><h3>{run.busy ? t("ui.hermesautomation.m37acc42d76") : t("ui.hermesview.m00dd8d970b")}</h3><p className={`hermes-runtime ${status?.ready ? "is-ready" : ""}`} role="status">{run.busy ? workingLabel : runtimeLabel(status)}</p>{visibleActivities.length ? <ol className="hermes-work-activities">{visibleActivities.map(item => <li key={item.id}><div>{item.complete && <IconCheck size={16} />}<span>{item.text}</span></div>{item.details && <details><summary>{t("ui.hermesview.m42fd0f588f")}</summary><pre>{item.details}</pre></details>}</li>)}</ol> : <p className="hermes-muted">{t("ui.hermesview.mc541bbb8af")}</p>}</section>
        <section className="hermes-work-section"><h3>{t("ui.hermesview.mdf797b1e57")}</h3><div className="hermes-work-actions"><button type="button" className="hermes-context-button" onClick={() => togglePanel("settings")}>{t("ui.hermesview.m090fad991e")}</button><button type="button" className="hermes-context-button" disabled={!status?.ready} onClick={() => togglePanel("commands")}>{t("ui.hermesview.m73d448921e")}</button></div></section>
        <details className="hermes-work-section hermes-diagnostics" aria-label={t("ui.hermesview.mda5bb858d8")}><summary>{t("ui.hermesview.m378670da2a")}</summary><p>{t("ui.hermesview.m5562ba06b5")}{status?.ready?t("ui.hermesview.maa37b88e04"):t("ui.hermesview.me297493361")}{t("ui.hermesview.m9b0c32dfed")}{modelReady?t("ui.hermesview.ma6fc2ded35"):t("ui.hermesview.me9dbdd9419")}{t("ui.hermesview.mfcd24b5cd4")}</p><p>{t("ui.hermesview.mb5dc6a59f1")}{controlCaps?.steer?t("ui.hermesview.m0950163112"):t("ui.hermesview.m18495cc896")}{t("ui.hermesview.m18dca41850")}{controlCaps?.queue?t("ui.hermesview.m084f386e3c"):t("ui.hermesview.m1f2db02e6a")}.</p><p>{t("ui.hermesview.m7531c865d8")}{catalog ? t("ui.hermesview.mbad83d793b") : t("ui.hermesview.m0f7d45bc53")}{t("ui.hermesview.m6324ed82e1")}{cwd || t("ui.hermesview.me82c451229")}.</p><button type="button" className="btn btn-secondary" disabled={!liveId || !connectionReady || !!pending} onClick={()=>void act("readiness",async()=>{const observed=observeChat();const result=await client.controlReadiness(observed.liveId);if(isCurrentChat(observed))setControlReadiness(result);})}>{t("ui.hermesview.mab4e682adb")}</button>{controlReadiness&&<div><p>{t("ui.hermesview.m494757674d")}{controlReadiness.tools.status}{controlReadiness.tools.total!==undefined?` · ${controlReadiness.tools.total}`:""}. {controlReadiness.tools.remediation}</p>{controlReadiness.tools.sections?.map(section=><details key={section.name}><summary>{section.name}</summary>{section.tools.map(tool=><p key={tool.name}>{tool.name} · {tool.description}</p>)}</details>)}</div>}{controlError&&<p role="alert">{controlError}</p>}</details>
      </section>
      {intentActions}
      </div></div>
      {detached && <button type="button" className="hermes-jump-latest" onClick={jumpToLatest}>{t("ui.hermesview.m81bc72ebf0")}</button>}
      {run.prompts.length > 0 && <button type="button" className="hermes-decision-opener" aria-label={t("ui.hermesview.m98d375e373", { p0: (run.prompts.length) })} onClick={() => openDecision(run.prompts[0])}>{run.prompts.some(prompt=>prompt.kind === "approval") ? t("ui.hermesview.m3f85e36a6f") : t("ui.hermesview.m5cae89baa8")} · {run.prompts.length}<span>{t("ui.hermesview.m673d40782d")}</span></button>}
      <HermesExecutionStatus quietCompleted observableActivity={childActivity} onDetails={childRows.length ? openAssistantDetails : undefined} busy={run.busy} waiting={run.prompts.length > 0} progress={run.progressText} answerStarted={!!activeAnswer?.content} hasAnswer={hasAnswer} restoring={restoringConversation || !!statusError || !!streamError || pending === "history"} subagents={confirmedSubagents} subagentError={subagentError} error={run.error} onRetry={() => void refreshSubagents()}
        stopAction={<>{run.error && <span className="hermes-error hermes-recovery"><button type="button" aria-label={t("agentCheck.button")} title={t("agentCheck.button")} onClick={retryConnection}>{t("agentCheck.button")}</button></span>}{run.busy && status?.ready && liveId && connectionReady && !statusError && !streamError && <button type="button" disabled={!!pending} onClick={() => void act("stop", async () => { const result = controlCaps?.interrupt ? await client.controlIntent("interrupt",liveId) : await client.rpc<{ status: string }>("session.interrupt", { profile: "default", session_id: liveId }); if (result.status === "not_interrupted") setNotice(t("ui.hermesview.mc90f7c0114")); })}>{pending === "stop" ? t("ui.hermesview.m6e4737e38f") : t("confirm.btn.stop")}</button>}</>} />
      {view === "chat" && <form className="hermes-composer" onSubmit={event => void send(event)} onDragOver={event => { if (event.dataTransfer.types.includes("Files")) { event.preventDefault(); event.dataTransfer.dropEffect = pending ? "none" : "copy"; } }} onDrop={event => { if (event.dataTransfer.files.length) { event.preventDefault(); addAttachments(Array.from(event.dataTransfer.files)); } }}>
        <input ref={filePicker} type="file" multiple hidden aria-label={t("ui.hermesview.mc03c18eba3")} onChange={event => { addAttachments(Array.from(event.target.files || [])); event.target.value = ""; }} />
        {chatAttachments.length > 0 && <div className="hermes-attachments" aria-label={t("ui.hermesview.mbac10f46d8")}>{chatAttachments.map(item => <div className="hermes-attachment" key={item.id}><span title={item.file.name}>{item.file.name}</span><small role="status">{item.error || (item.path ? t("skills.res.head") : t("ui.hermesview.med166aaa43", { p0: (Math.round(item.progress)) }))}</small>{item.error && <button type="button" disabled={!!pending} onClick={() => void uploadAttachment({...item, controller: new AbortController()})}>{t("agentCheck.retry")}</button>}<button type="button" aria-label={t("ui.hermesview.m4128c5d0a7", { p0: (item.file.name) })} disabled={!!pending} onClick={() => { uploads.current.get(item.id)?.abort(); attachmentRefs.current.delete(item.id); setAttachments(previous => previous.filter(row => row.id !== item.id)); }}><IconClose size={14} /></button></div>)}</div>}
        <div className="hermes-composer-row"><button type="button" className="hermes-icon-button hermes-composer-attach" aria-label={t("ui.hermesview.mece32301e9")} title={t("ui.hermesview.mece32301e9")} disabled={!!pending} onClick={() => filePicker.current?.click()}><svg width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.8" aria-hidden="true"><path d="m21 11-9 9a6 6 0 0 1-8-8L14 2a4 4 0 0 1 6 6L10 18a2 2 0 0 1-3-3l9-9" /></svg></button><label className="hermes-sr-only" htmlFor="hermes-task">{t("ui.hermesview.m2180caca6d")}</label>
        <textarea id="hermes-task" ref={textarea} onPaste={event => { const files = Array.from(event.clipboardData.files); if (files.length) { event.preventDefault(); addAttachments(files); } }} value={text} onChange={event => { preparedPrompt.current = null; setText(event.target.value); }} placeholder={t("ui.hermesview.m3eb03821ce")} rows={1} onKeyDown={event => { if (!event.nativeEvent.isComposing && (event.ctrlKey || event.metaKey) && event.key === "Enter") { event.preventDefault(); void send(); } }} />
        <button type="submit" className="hermes-composer-send" aria-label={pending === "send" ? t("ui.cloudloginview.m4cbdc4d6d2") : t("support.send")} disabled={!status?.ready || !canSend || !modelReady}><IconSend size={19} /></button></div>
      </form>}
      {view === "chat" && (!status?.ready || !modelReady || restoringConversation) && <p className="hermes-composer-hint" role="status">{run.busy ? t("ui.hermesview.mb255fd3466") : restoringConversation ? t("ui.hermesview.meab01ba719") : !status?.ready ? t("ui.hermesview.m73af0e3a42") : needsAccount ? t("ui.hermesview.m5579aff321") : t("ui.hermesview.m990c2a7bf3")}</p>}
    </main></div>
    {decision && decisionPrompt && isCurrentChat(decision.chat) && <SheetShell open onClose={() => setDecision(null)} className="hermes-sheet hermes-decision-sheet" overlayClassName="hermes-sheet-overlay" labelledBy="hermes-decision-title">
      <div className="hermes-panel-top"><h2 id="hermes-decision-title">{decisionPrompt.kind === "approval" ? t("ui.hermesview.mb70e318770") : t("ui.hermesview.m83523c6cf2")}</h2><button type="button" className="hermes-icon-button" aria-label={t("ui.hermesview.m811b6032f1")} onClick={() => setDecision(null)}><IconClose /></button></div>
      <HermesRequest key={`${device}:${decision.chat.revision}:${decision.chat.epoch}:${String(decisionPrompt.id)}`} prompt={decisionPrompt} disabled={!!pending} deviceName={deviceName} cwd={cwd} navigation={run.prompts.length > 1 && <nav className="hermes-decision-pager" aria-label={t("ui.hermesview.m8f6358bd3d")}>{run.prompts.map((prompt,index)=><button type="button" disabled={!!pending} key={String(prompt.id)} aria-current={prompt === decisionPrompt ? "true" : undefined} onClick={()=>openDecision(prompt)}>{t("ui.hermesview.m777cdc62e1")}{index+1}</button>)}</nav>} onReply={result => {
        if (!isCurrentChat(decision.chat) || !currentPrompts.current.includes(decision.prompt)) { setDecision(null); setError(t("ui.hermesview.m673c644318")); return; }
        void respond(decision.prompt, result);
      }} />
    </SheetShell>}
    {composerMenuOpen && <SheetShell open onClose={() => setComposerMenuOpen(false)} className="hermes-sheet hermes-menu-sheet" overlayClassName="hermes-sheet-overlay" labelledBy="hermes-actions-title">
      <div className="hermes-panel-top"><h2 id="hermes-actions-title">{t("ui.hermesview.m6011ec1617")}</h2><button type="button" className="hermes-icon-button" aria-label={t("ui.hermesview.mea06bae3d1")} onClick={() => setComposerMenuOpen(false)}><IconClose /></button></div>
      {viewport.keyboard && <div className="hermes-context-navigation" aria-label={t("ui.hermesview.me37824987d")} onClick={event => { if ((event.target as HTMLElement).closest("button[data-nav-id]:not([data-nav-id='more'])")) setComposerMenuOpen(false); }}><h3>{t("more.sections")}</h3><BottomNav active="hermes" /></div>}
      <div className="hermes-context-identity"><DeviceChip /><p>{cwd || t("ui.hermesview.m9cd5ff536b")}</p><small>{connectionReady ? t("home.status.onlineShort") : t("ui.hermesview.m41f2f62aff")}</small></div>
      <div className="hermes-composer-menu">
        <button type="button" aria-label={t("ui.hermesview.mc72d4830ee")} onClick={() => { setComposerMenuOpen(false); setView("work"); }}><span>{t("ui.hermesview.mc72d4830ee")}<small>{t("ui.hermesview.m8fff49f4b4")}</small></span></button>
        <button type="button" aria-label={t("ui.hermesview.mb998ef3f23", { p0: (attentionCount) })} onClick={() => togglePanel("attention")}><span>{t("ui.hermesview.m260f60cb64")}{attentionCount}<small>{t("ui.hermesview.m519e857a61")}</small></span></button>
        <button type="button" disabled={run.busy || !!pending} onClick={() => { setComposerMenuOpen(false); setFolderOpen(true); }}><IconFolder size={20} /><span>{t("ui.hermesview.m72e5f45b5f")}<small>{cwd || t("ui.hermesview.m0d39d331e3")}</small></span></button>
        {!!cwd && !liveId && <button type="button" disabled={run.busy || !!pending} onClick={() => { setCwd(""); setComposerMenuOpen(false); }}>{t("ui.hermesview.m0cba60df60")}</button>}
        {run.messages.length > 0 && <button type="button" aria-pressed={responseView === "details"} onClick={() => { setResponseView(responseView === "text" ? "details" : "text"); setComposerMenuOpen(false); }}><span>{t("ui.hermesview.m18144770e4")}<small>{responseView === "details" ? t("ui.hermesview.m79e4245e2b") : t("ui.hermesview.m20d7fc433e")}</small></span></button>}
        <button type="button" disabled={!status?.ready} onClick={() => togglePanel("commands")}><span>{t("ui.hermesview.m869768f24e")}<small>{t("ui.hermesview.mdafe71ec34")}</small></span></button>
        <button type="button" onClick={() => togglePanel("connection")}><span>{t("ui.hermesview.mb167fb14ad")}<small>{modelLabel}</small></span></button>
        <button type="button" onClick={() => togglePanel("settings")}>{t("ui.hermesview.md884937ec9")}</button>
      </div>
    </SheetShell>}
    <FolderNavSheet open={folderOpen} onClose={() => { setFolderOpen(false); setTaskFolderPicker(null); }} currentCwd={taskFolderPicker?.cwd ?? cwd} title={taskFolderPicker ? t("ui.hermesview.m588c211704") : t("ui.hermesview.ma6db96ef6a")} pickLabel={t("openrouter.change")} onPick={async path => { if (taskFolderPicker) { if (getTerminalContextKey() !== device) return false; taskFolderPicker.onPick(path); setTaskFolderPicker(null); setFolderOpen(false); return; } const observedChat = observeChat(); if (observedChat.liveId) { try { await client.rpc("session.cwd.set", { profile: "default", session_id: observedChat.liveId, cwd: path }); } catch (e) { if (isCurrentChat(observedChat)) setError(errorText(e)); return false; } } if (!isCurrentChat(observedChat)) return false; setCwd(path); setFolderOpen(false); }} />
    {!viewport.keyboard && <BottomNav active="hermes" />}
  </div>;
}

function HermesPanel({ desktop, inline = false, title, onClose, children }: { desktop: boolean; inline?: boolean; title: string; onClose: () => void; children: ReactNode }) {
  const heading = useId();
  if (inline) return <>{children}</>;
  const body = <><div className="hermes-panel-top"><h2 id={heading}>{title}</h2><button type="button" className="hermes-icon-button" aria-label={t("ui.hermesview.m2c9858e506")} onClick={onClose}><IconClose size={20} /></button></div>{children}</>;
  return desktop ? <aside className="hermes-side-panel" aria-labelledby={heading}>{body}</aside> : <SheetShell open onClose={onClose} className="hermes-sheet" overlayClassName="hermes-sheet-overlay" labelledBy={heading}>{body}</SheetShell>;
}

function HermesMessageView({ message, streaming, streamKey, showDetails }: { streamKey: string; message: { id: string; role: string; content: string; reasoning?: string }; streaming: boolean; showDetails: boolean }) {
  const contentId = useId();
  const [expanded, setExpanded] = useState(streaming);
  useEffect(() => { if (streaming) setExpanded(true); }, [streaming]);
  const [copyStatus, setCopyStatus] = useState("");
  const assistant = message.role === "assistant" || message.role === "agent";
  const long = assistant && !streaming && (message.content.length > 1600 || message.content.split("\n").length > 18);
  return <article className={`hermes-message is-${message.role}`}><b>{message.role === "user" ? t("ui.hermesview.madb4032a9d") : message.role === "system" ? t("ui.hermesview.m3bb1f5de1f") : "Hermes"}</b>{message.reasoning && <details className="hermes-reasoning" open={showDetails}><summary>{t("ui.hermesview.mb06af61af3")}</summary><div><HermesMarkdown>{message.reasoning}</HermesMarkdown></div></details>}<div id={contentId} className={`hermes-message-content${long && !expanded && !showDetails ? " is-folded" : ""}`}>{assistant ? <HermesMarkdown streaming={streaming} streamKey={streamKey}>{message.content}</HermesMarkdown> : message.content}</div>{assistant && message.content && <div className="hermes-message-actions">{long && !showDetails && <button type="button" className="hermes-context-button" aria-controls={contentId} aria-expanded={expanded} onClick={() => setExpanded(value => !value)}>{expanded ? t("common.hide") : t("ui.hermesview.m1610fed984")}</button>}<button type="button" className="hermes-context-button" aria-label={t("ui.hermesview.maacaab351f")} onClick={() => void terminalClipboard.write(message.content).then(copied => setCopyStatus(copied ? t("ui.hermesview.m934edee4ef") : t("ui.hermesview.m8d085deff0")))}><IconCopy size={14} />{t("pty.copy")}</button>{copyStatus && <span role="status">{copyStatus}</span>}</div>}</article>;
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
  const secretLabel = prompt.method === "sudo" ? t("ui.hermesview.m1e6d7edef8") : prompt.method === "vault.unlock_prompt"
    ? t("ui.hermesview.m94a8946108", { p0: (String(prompt.params.display_name || t("ui.hermesview.mb39cf9cc33"))) }) : prompt.method === "vault.code" ? t("ui.hermesview.mbfee5d1b49")
      : prompt.method === "secret" ? String(prompt.params.prompt || prompt.params.env_var || t("ui.hermesview.m8e702542ec")) : t("ui.hermesview.mcf5ec2d84c");
  return <section className={`hermes-request${prompt.kind === "approval" ? " hermes-request-approval" : prompt.kind === "question" ? " hermes-request-question" : ""}`} aria-label={prompt.title}>{prompt.kind !== "question" && <div className="hermes-request-body">{navigation}<h3>{prompt.title}</h3><p className="hermes-muted">{t("ui.hermesview.m8fbed71b1d")}{deviceName}{cwd ? ` · ${cwd}` : ""}</p>{prompt.description && <pre>{prompt.description}</pre>}
    {prompt.kind === "approval" && <dl className="hermes-approval-context">{[[t("ui.hermessubagentprogress.m0fe2342c2e"), prompt.params.command], [t("ui.hermesview.m1fb9f5ce90"), prompt.params.tool || prompt.params.tool_name], [t("ui.hermesview.m4a9ea52cda"), prompt.params.reason], [t("ui.hermesview.mf6c76e7a29"), prompt.params.cwd || cwd], [t("ui.hermesview.mbe256905c1"), prompt.params.session_id]].map(([label, value]) => <div key={String(label)}><dt>{String(label)}</dt><dd>{typeof value === "string" && value ? value : t("ui.hermesview.mc97ac7d94c")}</dd></div>)}</dl>}
      {(allowedSession || allowedAlways) && <><button type="button" className="hermes-context-button" aria-expanded={options} onClick={()=>setOptions(value=>!value)}>{t("ui.hermesview.mf7cc76bfed")}</button>{options && <div className="hermes-scope-options">{allowedSession && <button type="button" aria-pressed={scope === "session"} onClick={()=>{setScope("session");setAcknowledged(false);}}>{t("ui.hermesview.m959ea95758")}</button>}{allowedAlways && <button type="button" aria-pressed={scope === "always"} onClick={()=>{setScope("always");setAcknowledged(false);}}>{t("ui.hermesview.mdeb9e5d902")}</button>}</div>}{scope && <div className="hermes-scope-confirm"><p>{scope === "session" ? t("ui.hermesview.m134f5d1963") : t("ui.hermesview.mc86d1d4efe")}</p>{scope === "always" && <label className="hermes-answer-choice"><input type="checkbox" checked={acknowledged} onChange={event=>setAcknowledged(event.target.checked)} />{t("ui.hermesview.me032035f16")}</label>}<button type="button" className="btn btn-secondary" disabled={disabled || (scope === "always" && !acknowledged)} onClick={()=>onReply({choice:scope})}>{scope === "session" ? t("ui.hermesview.m98275400b0") : t("ui.hermesview.m15315caf69")}</button></div>}</>}
    </div>}
    {secretRequest ? <form onSubmit={event => { event.preventDefault(); const value = prompt.method === "vault.save_login" ? JSON.stringify({ identifier, password: secret }) : secret; onReply({ value }); setSecret(""); setIdentifier(""); }}>
      {prompt.method === "vault.save_login" && <><p>{String(prompt.params.site || prompt.params.origin || "")}</p><label>{t("ui.hermesview.me2d97c93ec")}<input value={identifier} autoComplete="off" onChange={event => setIdentifier(event.target.value)} /></label></>}
      <label>{secretLabel}<input type="password" value={secret} autoComplete="new-password" onChange={event => setSecret(event.target.value)} /></label><p className="hermes-muted">{t("ui.hermesview.mfcc781d00a")}</p><div className="hermes-request-actions"><button type="button" className="btn btn-secondary" disabled={disabled} onClick={() => { setSecret(""); setIdentifier(""); onReply({ value: "" }); }}>{t("ui.hermesview.mfe10080140")}</button><button type="submit" className="btn btn-primary" disabled={disabled || !secret}>{t("ui.hermesview.m486002d08b")}</button></div>
    </form> : prompt.kind === "approval" ? <footer className="hermes-decision-footer"><div className="hermes-request-actions">{prompt.choices.includes("deny") && <button type="button" className="btn btn-secondary" disabled={disabled} aria-label={t("agents.requestReject")} onClick={() => onReply({choice:"deny"})}><span className="hermes-deny-caption">{t("agents.requestReject")}</span><span className="hermes-deny-compact" aria-hidden="true">{t("home.act.no")}</span></button>}{prompt.choices.includes("once") && <button type="button" className="btn btn-primary" disabled={disabled} aria-label={t("ui.hermesview.m4332148754")} onClick={() => onReply({choice:"once"})}>{t("ui.hermesautomation.mbfe9562ad7")}</button>}</div>

    </footer>
      : prompt.kind === "question" ? <form onSubmit={event => { event.preventDefault(); onReply({ answers }); }}><div className="hermes-question-fields">{navigation}<h3>{prompt.title}</h3><p className="hermes-muted">{t("ui.hermesview.m8fbed71b1d")}{deviceName}{cwd ? ` · ${cwd}` : ""}</p>{questions.map(question => <fieldset key={question.qid}><legend>{question.question}</legend>{question.choices?.map(choice => <label className="hermes-answer-choice" key={choice}><input type={question.multi_select ? "checkbox" : "radio"} name={question.qid} checked={question.multi_select ? (answers[question.qid] || "").split(", ").includes(choice) : answers[question.qid] === choice} onChange={event => setAnswers(previous => ({ ...previous, [question.qid]: question.multi_select ? (event.target.checked ? [...(previous[question.qid] || "").split(", ").filter(Boolean), choice] : (previous[question.qid] || "").split(", ").filter(value => value !== choice)).join(", ") : choice }))} />{choice}</label>)}<label className="hermes-answer-text">{t("ui.hermesview.m260ce651a6")}<input value={answers[question.qid] || ""} onChange={event => setAnswers(previous => ({ ...previous, [question.qid]: event.target.value }))} /></label></fieldset>)}</div><div className="hermes-request-actions"><button type="button" className="btn btn-secondary" disabled={disabled} aria-label={t("ui.hermesview.ma95a50eb0c")} onClick={() => onReply({})}>{t("devices.switcher.undo")}</button><button type="submit" className="btn btn-primary" disabled={disabled || !questions.every(question => !!answers[question.qid]?.trim())}>{t("home.act.answer")}</button></div></form>
      : <form onSubmit={event => { event.preventDefault(); try { const value: unknown = JSON.parse(raw); setRawError(""); onReply(value); } catch { setRawError(t("ui.hermesview.me3015595e4")); } }}><p>{t("ui.hermesview.me65d2524f0")}{prompt.method}{t("ui.hermesview.mf7c4095472")}</p><details><summary>{t("ui.hermesview.m88964dca07")}</summary><pre>{JSON.stringify(prompt.params, null, 2)}</pre></details><label>{t("ui.hermesview.m278c461230")}<textarea value={raw} onChange={event => setRaw(event.target.value)} placeholder="{}" rows={3} /></label>{rawError && <p role="alert">{rawError}</p>}<button type="submit" className="btn btn-secondary" disabled={disabled || !raw.trim()}>{t("ui.hermesview.m18523c1df9")}</button></form>}
  </section>;
}
