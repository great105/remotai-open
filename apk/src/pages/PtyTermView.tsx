import { useEffect, useLayoutEffect, useRef, useState, useCallback } from "react";
import { useParams, useNavigate, useSearchParams } from "react-router-dom";
import { Terminal } from "@xterm/xterm";
import { cellAt, measureTerminalCoordinates, visibleRows as clippedRows } from "../ptyTerm/geometry/TerminalCoordinates";
import { compatMouseAfterTap, decayVelocity, releaseVelocity, WheelAccumulator } from "../ptyTerm/gestures/GestureController";
import { bufferText } from "../ptyTerm/reading/bufferText";
import { registerReadAnchor } from "../ptyTerm/reading/readAnchor";
import { captureReadDocument, continuationValid, documentCell, ringCutAfterRis } from "../ptyTerm/reading/ReadDocument";
import { ReadSurface, type ReadSurfaceProps } from "../ptyTerm/reading/ReadSurface";
import { AgentHistoryClient } from "../ptyTerm/reading/AgentHistoryClient";
import { AgentHistorySurface } from "../ptyTerm/reading/AgentHistorySurface";
import {
  FLUSH_BATCH_MAX_BYTES, FLUSH_SAFETY_MS, FlowStatsAccumulator, flowDiagDue, flowDiagFields, flowQueueCap, flowReasons,
  carryTailAcrossMarker, flowStatsKey, flowTransition, noteHanded, reconnectResume, selectFlushBatch, type HandedMark,
} from "../ptyTerm/runtime/FlowController";
import { wordRange } from "../ptyTerm/selection/SelectionController";
import { attachCopyOnSelect } from "../ptyTerm/selection/copyOnSelect";
import { terminalClipboard } from "../ptyTerm/input/ClipboardService";
import { enterIntent, reviewedInput, sameInputTarget, transmitInput } from "../ptyTerm/input/InputController";
import type { InputTarget } from "../ptyTerm/input/InputController";
import { appendSessionDraft, draftOwner } from "../ptyTerm/input/SessionDraft";
import { useSessionDraft } from "../ptyTerm/input/useSessionDraft";
import { foregroundTask, silenceRecheckDelay, silenceVerdict, thawedAt } from "../ptyTerm/runtime/ForegroundTask";
import {
  keyboardPeek, lastInkRow, observeNativeKeyboard, OCCLUSION_BUSY_RECHECK_MS, OCCLUSION_IDLE, OCCLUSION_SYNC_RECHECK_MS,
  occlusionStep, type OcclusionRecheck, type OcclusionState,
} from "../ptyTerm/geometry/KeyboardAdapter";
import { SizeOwnerControls } from "../ptyTerm/geometry/SizeOwnerControls";
import { isShortLandscape } from "../ptyTerm/geometry/shortLandscape";
import { parseTerminalControls, terminalFeatures } from "../ptyTerm/runtime/TerminalControls";
import { androidWebglNeedsDom } from "../ptyTerm/runtime/androidWebgl";
import { CommandBlockModel, attachCommandBlocks, availableBlockActions, blockCopyOutcome, blockNotice, blockText } from "../ptyTerm/commandBlocks";
import { panAxis, panBy, panFollows, touchAxis, wheelPanDelta } from "../ptyTerm/geometry/viewportPan";
import type { TerminalControls } from "../ptyTerm/runtime/TerminalControls";
import { FitAddon } from "@xterm/addon-fit";
import "@xterm/xterm/css/xterm.css";
import "../ptyTerm/header.css";
import "../ptyTerm/layout.css";
import { haptic, hapticSuccess, hapticError, tgConfirm, getTelegram } from "../telegram";
import { isShareCancel, saveBlob } from "../saveFile";
import { savePtyLog, sendPtyLogToTelegram, ptyLogFileName, sendBlobToTelegram } from "../ptyLogExport";
import {
  uploadPtyFile, getPtyState, handoffPty, ptyWSUrl,
  downloadBlob, createPtySession, sendToTelegram, getAgents, getAgentAccounts, ptyInput, onWSEvent,
  createSshHost, onConnectionChange, setPtyAccount, renamePty, ptySleep, ptyWake,
  getBookmarks, addBookmark, removeBookmark, saveScreenshotToPC,
} from "../api";
import type { PtyState } from "../api";
import { getCloudJWT, getMode, getRelayBase, getSelectedDeviceAgentVersion, isNativeApp, getTerminalContextKey } from "../config";
import { rememberTerminal } from "../terminalHistory";
import { CLIENT_COMMIT, CLIENT_VERSION } from "../clientUpdate";
import { traceBundle } from "../ptyTerm/terminalTrace";
import type { TerminalTrace, TraceContext, TraceIdentity, TraceKind } from "../ptyTerm/terminalTrace";
import { toAsciicastV3, type ByteRecorder } from "../ptyTerm/traceRecording";
import { terminalTraceStore, wipeAllRecordings } from "../ptyTerm/traceStore";
import {
  captureStorage, clearAllCaptures, clockLabel, readCaptureUntil, traceFileName, tracePreview, uploadDiagFields,
  writeCaptureUntil,
} from "../ptyTerm/traceExport";
import type { TracePreview } from "../ptyTerm/traceExport";
import {
  agentUpdateNotice,
  agentUpdateNoticeDismissed,
  dismissAgentUpdateNotice,
  fetchLatestVersion,
} from "../agentUpdateNotice";
import type { AgentInfo } from "../types";
import {
  agentDisplayName, agentHistoryChannel, builtinRetention, declaredRetention, declaredScrollChannel, promptDialog, mapApiError, ptyDisplayTitle,
  formatDurationMs,
  CONNECTION_BANNER_DELAY_MS,
} from "@tgcontrol/shared";
import { t } from "../i18n";
import { useBotAvailable } from "../hooks/useBotAvailable";
import { ProcessBadge, PtySearchBar, FolderNavSheet, DownloadSheet, SnippetsSheet, isAgentKind, useEscape } from "@tgcontrol/shared";
import type { AgentKind } from "@tgcontrol/shared";
import { HelpSheet } from "../components/HelpSheet";
import { IconArrow, IconCheck, IconClose, IconCopy, IconEye, IconFolder, IconHelp, IconKeyboard, IconPencil, IconRefresh, IconStar } from "../components/icons";
import { TerminalModeMenu } from "../ptyTerm/TerminalModeMenu";
import { AgentLaunchSheet } from "../components/AgentLaunchSheet";
import { useAgentAccounts } from "../components/AgentAccounts";
import {
  composeLaunch, EMPTY_PREFS, launchAccountForAgent, recordedResumeAccount,
} from "../ptyTerm/agentLaunch";
import { shortCommandLabel } from "../ptyTerm/commandLabel";
import { isTerminalAutoReply } from "../ptyTerm/autoReply";
import { commitAccountScopedLaunch } from "../ptyTerm/agentLaunchTransaction";
import { AGENT_LAUNCH_WAIT_MS, agentEntryMode, type AgentLaunchAttempt } from "../ptyTerm/agentEntry";
import { trackFirstAgent } from "../cloud/support";
// Правила экрана (кнопки ответа агенту, «связь идёт» против «процесс завершён»,
// имя терминала из прошлого захода) — чистые функции от состояния сессии,
// проверяются без React. См. ptyTerm/rules.ts.
import {
  answerChoicesOf,
  forgetTitleCache,
  readTitleCache,
  terminalLinkState,
  writeTitleCache,
  type PtyStateExtra,
} from "../ptyTerm/rules";
// Код привязки сервера, замеченный в выводе (`remotai pair` на новом сервере).
import { pairCodeInOutput } from "../ptyTerm/pairOffer";
// Защита истории прокрутки от «ESC[3J» (агент стирает её при перерисовке).
import {
  EMPTY_BYTES, eraseActionFor, eraseChainEnds, eraseRoute, generationDropsPendingErase, keepPendingErase, keepPendingScrollbackErase,
  navigationChoiceDropsPendingErase, retentionFor, RetentionShadowLog, SCROLLBACK_ERASE,
  ScrollbackEraseGate, scrollbackEraseAction, splitScrollbackErase, stripScrollbackErase,
} from "../ptyTerm/keepHistory";
import type { RetentionPolicy, ScrollbackEraseAction } from "../ptyTerm/keepHistory";
import type { EraseChainItem } from "../ptyTerm/keepHistory";
// План применения кадра (RIS → история → досылка строк → кадр) — одно правило
// для компонента и стенда эквивалентности (ptyTerm/snapshotApply.ts, §6).
import { SNAPSHOT_RIS, planSnapshotApply, snapshotStepPayload } from "../ptyTerm/snapshotApply";
import type { SnapshotStep } from "../ptyTerm/snapshotApply";
import {
  TXN_CLOSED, abandonTxn, beginSnapshotTxn, finishSnapshotTxn, markerPrefixOpens, openTxn, planMarkerPrefix,
  resetTxn, snapshotStepParts, txnAfterParse, txnWatchdog,
} from "../ptyTerm/presentation";
import type { PresentationRenderer, PresentationTxn, SnapshotStepParts, SnapshotTxnHead } from "../ptyTerm/presentation";
// Параметры эмуляции, от которых зависит СОСТОЯНИЕ буфера: те же у стенда.
import { terminalEmulationOptions } from "../ptyTerm/terminalEmulation";
// ST-05: один координатор восстановления экрана на соединение (чистый автомат).
import * as recovery from "../ptyTerm/recoveryCoordinator";
import type { RecoveryCause, RecoveryState, RecoveryStep } from "../ptyTerm/recoveryCoordinator";
// SOTA-снапшот (replay полной заменой): когда к кадру экрана применять
// присланную сервером историю scrollback — чистый автомат, проверяется без
// React (см. ptyTerm/snapshotHistory.ts и его тест).
import {
  SNAPSHOT_HISTORY_INIT, noteSyncMarker,
} from "../ptyTerm/snapshotHistory";
import type { SnapshotHistory } from "../ptyTerm/snapshotHistory";
import { resumeParam } from "../ptyTerm/streamPath";
import { canSleepAgent, INPUT_MODES_OFF, sleepBlockedByStatus, sleepErrorText, wakeClearLine, wakeCommand } from "../ptyTerm/agentSleep";
// Позиция в потоке, пережившая закрытие приложения (иначе каждое открытие —
// полный хвост кольца: 0,5–0,9 МБ, замер на релее 11.08.2026).
import { clearResumePos, loadResumePos, saveResumePos, shouldProbeHistory, shouldUseColdResume } from "../ptyTerm/resumeStore";
// Сколько ждать тишины перед отправкой размера: качель панели браузера должна
// успеть вернуться, а настоящий поворот — уйти сразу.
import {
  RESIZE_QUIET_MS, capacityDelivered, capacityForServer, frameAnswersDeliveredRequest, frameAnswersRequest, resizeDelayMs, sentBaseline,
  shouldFlushResize, type ScreenRequestNote,
} from "../ptyTerm/resizePolicy";
import {
  MAX_PAGES_PER_GESTURE,
  channelPayload,
  clickSeq,
  decayStreamSample,
  historyOwnerFromStreamStable,
  pageSeq,
  pagesFromAccum,
  refillPageBudgetAt,
  resolveHistoryOwner,
} from "../ptyTerm/altScroll";
import { ScrollRouter } from "../ptyTerm/gestures/ScrollRouter";
import type { ScrollRoute, ScrollTicket } from "../ptyTerm/gestures/ScrollRouter";
import type { AltScrollChannel, HistoryOwner } from "../ptyTerm/altScroll";
// Одно правило «кто исполняет жест» и свидетельства каналов (план 13.09, ST-02/03).
import { decideNavigation, edgePlan, shouldDropPin } from "../ptyTerm/navigationDecision";
import { legacyDecision, navigationShadowDiff } from "../ptyTerm/legacyNavigation";
import type { NavigationDecision, NavMode, ReadingPin } from "../ptyTerm/navigationDecision";
import { NavigationEvidence, directionOf, evidenceScopeKey, nextLateAnswerCheck } from "../ptyTerm/navigationEvidence";
import { TerminalFeaturesPanel } from "../ptyTerm/TerminalFeaturesPanel";
import { diagReachesAgent, stateShowsNewAgent } from "../ptyTerm/diagCompat";
import type { EvidenceDecision, NavChannel, Observation } from "../ptyTerm/navigationEvidence";
import {
  OCCLUDER_SELECTORS, capacityWithOccluder, frameGeometryAction, frameIsStale, fullViewportHeight, inputGrowthPx, nativeKeyboardOpen,
  layoutChange, logicalRowsForKeyboard, reconcileAdoptedGrid, shouldExplainNarrowOutput,
} from "../ptyTerm/geometry";
import { TerminalWidthNotice } from "../ptyTerm/TerminalWidthNotice";
import {
  STORED_CHANNELS, forgetScrollProbeVerdict, readScrollProbeVerdict, restoredObservation,
  saveScrollProbeVerdict, storedEvidence,
} from "../ptyTerm/scrollProbeMemory";
import { noteTerminalGeometry, screenGeometryRevisionMatches } from "../ptyTerm/geometryRevision";
// Прокрутка ряда команд — не нажатие: команда отсюда уходит в терминал сразу.
import {
  emptyTapGuard, noteDown, noteMove, noteScroll, noteExpanded, tapVerdict,
} from "../ptyTerm/tapGuard";
import {
  loadCommands as loadUserCommands, upsertCommand as upsertUserCommand,
  removeCommand as removeUserCommand, pinnedOf, commandLabel,
} from "../ptyTerm/commands";
import { TerminalWriter } from "../ptyTerm/terminalWriter";
import type { TerminalWriteGuard, TerminalWriteOptions } from "../ptyTerm/terminalWriter";
import { GuardedParserCarry } from "../ptyTerm/parserCarry";
import { captureTerminalRows, screenScrollResponse } from "../ptyTerm/scrollResponse";
import {
  LatestOnlyGate, advanceAppliedOffset, afterSyncMarker, effectiveHistoryOwner,
  sameRuntimeGuard, scrollClassifierKey,
  shouldResetScrollClassifier, writerEpochKey,
} from "../ptyTerm/sessionRuntime";
import type { ScrollOverride } from "../ptyTerm/sessionRuntime";
import { readScrollPreference, saveScrollPreference } from "../ptyTerm/scrollPreference";
import type { UserCommand } from "../api";
import { runPair } from "../cloud/pair";
import { useCapabilities } from "../hooks/useCapabilities";
import { isInnerPath } from "../navBack";

/**
 * Имя чанка бандла, в котором собран этот экран (ST-00, T-40): версия одна на
 * выпуск, а сборок с ней несколько — хеш в имени чанка называет именно ту, что
 * написала трассу. В dev — путь исходника.
 */
const TRACE_BUNDLE_CHUNK = (() => {
  try { return new URL(import.meta.url).pathname.split("/").pop() || ""; } catch { return ""; }
})();

type ScrollFallback =
  | { kind: "lines"; lines: number }
  | { kind: "edge"; up: boolean };

/** Решение, закреплённое за намерением (жест, серия колеса, нажатие) до его конца. */
type ScrollDestination = { decision: NavigationDecision; owner: HistoryOwner };

type PendingScrollAction = {
  channel: AltScrollChannel;
  data: string;
  lines: number;
  owner: HistoryOwner;
  fallback: ScrollFallback;
  ticket: ScrollTicket;
  mode: NavMode;
};

/**
 * Глубина СВОЕЙ прокрутки — по НОРМАЛЬНОМУ буферу, а не по активному.
 *
 * ⚠ У alt-screen baseY ноль по определению: меряя активный буфер, мы бы решили,
 * что у полноэкранного TUI истории нет вовсе, — и отобрали бы её у терминала
 * (грабля 2.57.7, записана в зоне).
 */
function normalBaseY(term: Terminal): number {
  try {
    const b = term.buffer as unknown as { normal?: { baseY?: number } };
    return b.normal?.baseY ?? term.buffer.active.baseY;
  } catch {
    return 0;
  }
}

/**
 * Сколько строк ВИДНО прямо сейчас: логическая высота минус то, что уехало за
 * верхний край под клавиатуру. Нужно только диагностике и жестам — размером PTY
 * это не является и на компьютер не отправляется.
 */
function visibleRows(term: Terminal, peekPx: number): number {
  if (peekPx <= 0 || term.rows < 1) return term.rows;
  try {
    const screen = (term.element?.querySelector(".xterm-screen") as HTMLElement | null);
    const drawn = screen ? screen.getBoundingClientRect().height : 0;
    if (drawn <= 0) return term.rows;
    const rowH = drawn / term.rows;
    if (rowH <= 0) return term.rows;
    return Math.max(1, term.rows - Math.round(peekPx / rowH));
  } catch {
    return term.rows;
  }
}

/**
 * Может ли СВОЯ прокрутка терминала сдвинуться в нужную сторону прямо сейчас:
 * вверх — есть ли что выше вьюпорта, вниз — есть ли что ниже. Это и есть ответ
 * на «чья тут история», а не alt-screen: Claude Code рисует в обычном буфере, но
 * перерисовывает экран на месте (замер 13.08.2026: 3–29 переводов строки на
 * 32 КБ против 655–812 адресаций курсора).
 */
function localCanScroll(term: Terminal, lines: number): boolean {
  try {
    const b = term.buffer.active;
    return lines < 0 ? b.viewportY > 0 : b.viewportY < b.baseY;
  } catch {
    return false;
  }
}

type BufferedTermWrite = {
  bytes: Uint8Array;
  streamEnd: number;
  guard: TerminalWriteGuard;
};

/** Ряд действий блоков команд (ST-10, T-39): только флаги, без текста (I-15). */
interface BlockUi { copyCommand: boolean; copyOutput: boolean; jump: "none" | "local" | "app"; notice: "none" | "restored" }
const NO_BLOCK_UI: BlockUi = { copyCommand: false, copyOutput: false, jump: "none", notice: "none" };
const sameBlockUi = (a: BlockUi, b: BlockUi) => a.copyCommand === b.copyCommand && a.copyOutput === b.copyOutput
  && a.jump === b.jump && a.notice === b.notice;

export function PtyTermView({ onReopen }: { onReopen: () => void }) {
  const { id } = useParams<{ id: string }>();
  const terminalContext = getTerminalContextKey();
  const navigate = useNavigate();
  const [searchParams] = useSearchParams();
  // Есть ли у машины дисплей: на headless-сервере «Открыть на ПК» невозможно.
  const { hasDisplay, platform } = useCapabilities();
  // SSH-сессия (?ssh=1&host=…): показываем закреплённый баннер bootstrap'а —
  // предложение поставить Remotai на этот сервер. В обычных терминалах баннера нет.
  const sshHost = searchParams.get("ssh") === "1" ? searchParams.get("host") || "" : "";
  const showSSHInstall = searchParams.get("install") === "1";
  const [sshBannerOff, setSshBannerOff] = useState(false);
  // Плашка «агент на этом компьютере устарел» поверх терминала (не SSH):
  // состояние обновления агента раньше было видно только в «Моих компьютерах»
  // и настройках, а человек живёт именно здесь. Один запрос манифеста на маунт
  // страницы (внутри fetchLatestVersion — кэш на 30 минут); «✕» глушит плашку
  // ровно на эту версию.
  const [agentUpdate, setAgentUpdate] = useState<
    { state: "auto" | "stuck"; version: string; latest: string } | null
  >(null);
  // Плашка «агент закончил» поверх терминала: сервер шлёт pty_event/finished
  // с duration_ms (длительность завершившегося эпизода работы агента).
  // Самогаснущая (~9 с) + крестик. finished БЕЗ duration_ms — это обычная
  // команда в шелле, её покрывает системное уведомление, здесь молчим.
  const [agentDone, setAgentDone] = useState<{ who: string; ms: number } | null>(null);
  const agentDoneTimer = useRef<ReturnType<typeof setTimeout> | null>(null);
  useEffect(() => {
    // Только облачный терминал самого агента: в SSH-сессии версия агента —
    // про другую машину, а в LAN актуальную версию релей не знает.
    if (getMode() !== "cloud" || sshHost) return;
    const version = getSelectedDeviceAgentVersion().trim();
    if (!version) return;
    let cancelled = false;
    void fetchLatestVersion().then((latest) => {
      if (cancelled) return;
      const n = agentUpdateNotice(version, latest);
      if (n && !agentUpdateNoticeDismissed(n.latest)) {
        setAgentUpdate({ state: n.state, version, latest: n.latest });
      }
    });
    return () => { cancelled = true; };
  }, [sshHost]);
  const botAvailable = useBotAvailable();
  const termRef = useRef<HTMLDivElement>(null);
  const terminalRef = useRef<Terminal | null>(null);
  const fitRef = useRef<FitAddon | null>(null);
  const wsRef = useRef<WebSocket | null>(null);
  const reconnectTimer = useRef<ReturnType<typeof setTimeout> | null>(null);
  const stableTimer = useRef<ReturnType<typeof setTimeout> | null>(null);
  // Вотчдог первого байта: мобильный WebView может молча потерять соединение
  // (VPN/смена сети) БЕЗ onclose — сокет выглядит OPEN, но мёртв. Страница тогда
  // навсегда зависает на «Подключение к терминалу…»: реконнект ждёт onclose,
  // который не придёт (наблюдали: страница 75 минут поллила /state при давно
  // умершем стриме на релее). Если за 15с после создания сокета не пришло ни
  // байта — закрываем его сами, onclose запускает обычный backoff-реконнект.
  const firstByteTimer = useRef<ReturnType<typeof setTimeout> | null>(null);
  // Прежний путь (features.recoveryV1 = false): свой таймер на каждую просьбу.
  const screenRequestTimersRef = useRef<Set<number>>(new Set());
  // ST-05 (features.recoveryV1): ОДИН координатор восстановления экрана и ОДИН
  // его таймер на соединение. Состояние — чистый автомат ptyTerm/
  // recoveryCoordinator.ts; здесь только исполнение эффектов. Живёт от маунта:
  // потраченный откат на reset (fallbackUsed) переживает переподключение, но
  // не новое открытие терминала. Переключатель читается при открытии, поэтому
  // таймеры другого пути в этом экземпляре не заводятся вовсе (план 9.1).
  const recoveryRef = useRef<RecoveryState>(recovery.createRecoveryState());
  const recoveryTimerRef = useRef<number | null>(null);
  // Был ли уже маркер reset/resumed на ТЕКУЩЕМ соединении: первый reset —
  // базовая точка (причина open), повторный — смена эпохи посреди соединения.
  const markerSeenRef = useRef(false);
  // Входы адаптера координатора для мест, объявленных РАНЬШЕ него (горячий путь
  // очереди, геометрия, эффекты /state). Заполняется ниже, у requestScreenFrame.
  const recoveryApiRef = useRef<{
    demand: (cause: RecoveryCause) => void;
    syncGeometry: () => void;
    serverGrid: (why: string) => void;
    resizeSent: () => void;
  }>({ demand: () => {}, syncGeometry: () => {}, serverGrid: () => {}, resizeSent: () => {} });
  // Idle-вотчдог по heartbeat: сервер объявляет в маркере reset/resumed поле
  // hb (сек) и шлёт `{"t":"hb"}` с этим периодом. Тишина дольше 2.5×hb значит
  // «сокет полумёртв» (заморозка в фоне / смена сети без onclose) — закрываем
  // сами, onclose запускает реконнект, resume не перекачивает ничего лишнего.
  // Старый агент поля hb не шлёт → лимит 0 → вотчдог выключен (иначе убивали
  // бы живые молчащие терминалы, protocol-пинги из JS не видны).
  const lastMsgRef = useRef(0);
  const hbLimitRef = useRef(0); // 2.5×hb в миллисекундах; 0 = выключен
  const IDLE_TICK_MS = 5000;
  // Момент последнего возврата страницы из фона или разморозки (ST-09, T-33):
  // от него отсчитывается льгота сторожа тишины. 0 — страница не уходила.
  const visibleSinceRef = useRef(0);
  // Разовая перепроверка тишины по истечении льготы (ST-09, T-33): мёртвый
  // после фона сокет закрывается через ~3 с, а не следующим тиком сторожа.
  const silenceRecheckTimer = useRef<ReturnType<typeof setTimeout> | null>(null);
  const idleWatchTimer = useRef<ReturnType<typeof setInterval> | null>(null);
  const attemptRef = useRef(0);
  // Гард против «зомби-циклов» реконнекта: onclose у WebSocket срабатывает
  // АСИНХРОННО, уже после cleanup размонтирования, и без этого флага планировал
  // новый connect() на мёртвом компоненте. Живая сессия шлёт байты → markStable
  // сбрасывает счётчик попыток → цикл никогда не упирается в maxAttempts и
  // живёт до перезапуска приложения. Каждый заход на страницу терминала
  // оставлял по такому циклу; при возврате телефона из фона все они
  // реконнектились разом (шторм в connstat: десятки opens/мин, ~18 сокетов на
  // сессию), и реальный терминал пробивался к релею минуту-две.
  const disposedRef = useRef(false);
  const [connected, setConnected] = useState(false);
  const [features] = useState(() => terminalFeatures());
  const historyClientRef = useRef(new AgentHistoryClient());
  const [historyAvailable, setHistoryAvailable] = useState(false);
  const [historyOpen, setHistoryOpen] = useState(false);
  useEffect(() => () => historyClientRef.current.reset(), []);
  // ST-10: команды как блоки по OSC 133. Модель живёт с терминалом (эффект
  // терминала), ряд действий — флаги из неё; пересчёт — refreshBlockUi.
  const blockModelRef = useRef<CommandBlockModel | null>(null);
  const [blockUi, setBlockUi] = useState<BlockUi>(NO_BLOCK_UI);
  const refreshBlockUiRef = useRef<() => void>(() => {});
  const [sizeControls, setSizeControls] = useState<TerminalControls | null>(null);
  const sizeControlsRef = useRef(sizeControls);
  sizeControlsRef.current = sizeControls;
  const [sizeControlsOpen, setSizeControlsOpen] = useState(false);
  const [sizeControlPending, setSizeControlPending] = useState(false);
  const sizeControlTimerRef = useRef<ReturnType<typeof setTimeout> | null>(null);
  useEffect(() => {
    if (!connected || !features.sizeOwner) return;
    return foregroundTask(() => {
      const state = sizeControlsRef.current, socket = wsRef.current;
      if (state?.owner && state.owner === state.you && socket?.readyState === WebSocket.OPEN) {
        try { socket.send(JSON.stringify({ t: "size-control", op: "renew", revision: state.revision })); } catch { /* no replay */ }
      }
    }, 10000);
  }, [connected, features.sizeOwner]);
  useEffect(() => () => { if (sizeControlTimerRef.current) clearTimeout(sizeControlTimerRef.current); }, []);
  const [reconnecting, setReconnecting] = useState(false);
  const [gaveUp, setGaveUp] = useState(false);
  const [sessionMissing, setSessionMissing] = useState(false);
  const sessionMissingRef = useRef(false);
  const [showConnecting, setShowConnecting] = useState(true);
  // Релей ответил «компьютер на связи, но канал до него открыть не смог»
  // (код 1013). Отдельное состояние, потому что и лечится оно иначе, чем
  // отсутствие сети: см. ws.onclose.
  const [streamRefused, setStreamRefused] = useState(false);
  const gotOutputRef = useRef(false);
  // Resume по offset+epoch: позиция в потоке вывода сессии. На реконнекте шлём
  // серверу (epoch, offset), и он досылает только хвост вместо всего scrollback.
  // Прокрутка нужна обработчику колеса, который ставится при СОЗДАНИИ терминала,
  // то есть раньше, чем объявлен сам scrollByLines. Через ref, а не прямой
  // ссылкой — иначе та же TDZ-грабля, что однажды была с browserTouch.
  const scrollByLinesRef = useRef<(
    lines: number,
    opts?: { source?: "touch" | "inertia" | "button" | "wheel" },
  ) => void>(() => {});
  const scrollRouterRef = useRef(new ScrollRouter<ScrollDestination>());
  const pendingScrollRouteRef = useRef<ScrollRoute<ScrollDestination> | null>(null);
  const cancelGestureRef = useRef<() => void>(() => { scrollRouterRef.current.cancel(); });
  const scrollIdentityRef = useRef<() => string>(() => "");
  const dispatchScrollPayloadRef = useRef<(action: PendingScrollAction) => void>(() => {});
  // Начальное значение — из хранилища: ref живёт только пока жив компонент, а
  // человек уходит с экрана и закрывает приложение постоянно. Читаем ЛЕНИВО и
  // ровно один раз (как соседний pty.draft), а не на каждый рендер.
  // ⚠ ХОЛОДНОЕ ОТКРЫТИЕ: сохранённая позиция годится не всегда.
  //
  // Она хранит только место в БАЙТОВОМ потоке. Сам буфер xterm со всей
  // прокруткой умирает вместе со страницей, а сервер, получив resume, честно
  // досылает лишь дельту — у простаивающего агента она пустая. Значит вся
  // прокрутка новой страницы обязана прийти из истории зеркала, и когда зеркало
  // бедное, брать её неоткуда: человек видит один экран и «выше не листается».
  // Боевая диагностика 13.08.2026: `зеркало=4 своя=0` на сессии Claude.
  // Правило и живые числа — в ptyTerm/resumeStore.ts.
  const [savedResume] = useState(() => {
    const pos = loadResumePos(id || "");
    return shouldUseColdResume(pos) ? pos : null;
  });
  const epochRef = useRef<string>(savedResume?.epoch ?? "");
  // Эпоха, ПОДТВЕРЖДЁННАЯ маркером в этой странице (ST-02, волна 4): область
  // свидетельств навигации строится по ней, а не по сохранённой позиции resume.
  const confirmedEpochRef = useRef("");
  // Writer identity is separate from resume validity. Queue overflow invalidates
  // the next resume request, but the live parser generation must keep rendering.
  // It is also deliberately NOT just the server epoch: a full `reset` can use
  // the SAME server epoch when only the requested offset fell out of the ring.
  // Every sync marker therefore gets a unique writer epoch, so post-marker
  // binary can never overtake its RIS/modes boundary and then be erased by it.
  const writerEpochSeqRef = useRef(0);
  const writerEpochRef = useRef<string>(writerEpochKey(savedResume?.epoch ?? "", 0));
  // Connection generation + writer epoch guard every queued xterm mutation.
  // The writer itself is route-local; App additionally keys this component by
  // `/pty/:id`, so no parser queue can cross from one terminal to another.
  const connectionGenRef = useRef(0);
  const terminalWriterRef = useRef<TerminalWriter | null>(null);
  if (!terminalWriterRef.current) {
    terminalWriterRef.current = new TerminalWriter(null, {
      generation: connectionGenRef.current,
      epoch: writerEpochRef.current,
    });
  }
  const offsetRef = useRef<number>(savedResume?.offset ?? 0);
  // ПРИНЯТО и ПОКАЗАНО — разные позиции, и на диск идёт только вторая.
  //
  // offsetRef растёт в onmessage, по факту приёма кадра из сокета. Между
  // приёмом и появлением байт на экране стоят три очереди: своя склейка
  // (writeQueueRef, кадр анимации или 250 мс в фоне), внутренняя очередь xterm
  // (до 256 КБ, разбирается порциями ~12 мс) и отрисовка. Сервер же понимает
  // сохранённую позицию буквально — как «эти байты человек уже видел», и
  // досылает строго после неё. Значит уход в фон или закрытие экрана с
  // непустой очередью терял вывод НАВСЕГДА: позиция говорила «показано», а
  // очередь выбрасывалась (внешний аудит 13.08.2026, находка T-004; серверный
  // контракт — комментарий у Session.ResyncFrom).
  //
  // appliedOffsetRef двигается только из write-callback xterm — то есть тогда,
  // когда байты РАЗОБРАНЫ. Цена ошибки поменяла знак: в худшем случае несколько
  // килобайт придут повторно и перерисуются в свежесозданный терминал (человек
  // этого не увидит), вместо того чтобы пропасть без следа.
  const appliedOffsetRef = useRef<number>(savedResume?.offset ?? 0);
  // Позиция в потоке ПОСЛЕ последнего кадра, положенного в очередь склейки.
  // По ней спуск очереди знает, до какого места он двигает applied.
  const queueEndRef = useRef<number>(savedResume?.offset ?? 0);
  // ST-09: ОТДАНО xterm — третья позиция между принятым и показанным. Конец
  // последней записи вывода, которую TerminalWriter передал xterm (там не
  // больше одной записи). Отданное xterm разберёт при любом обрыве, всё, что
  // стоит за ним, connect() выбросит — поэтому при непустой очереди склейки
  // новое соединение продолжает поток отсюда, а не сбрасывает позицию
  // (FlowController.reconnectResume). null — позиция экран не описывает: в
  // xterm ушёл кадр экрана, позицию сбросили. Только при features.flowBacklog.
  const handedMarkRef = useRef<HandedMark | null>(null);
  // ── ТРАССА СОБЫТИЙ КЛИЕНТА (ST-01) ─────────────────────────────────────
  // Ограниченное кольцо метаданных в памяти страницы: что происходило ДО
  // жалобы, даже когда сокет давно закрыт. Раньше диагностика была разовыми
  // {t:"diag"} в тот же сокет и при обрыве терялась. Правила (allowlist полей
  // I-15, кольцо, слияние rx) — в ptyTerm/terminalTrace.ts; здесь только
  // снимок состояния конвейера на момент решения. Живёт в ref, НЕ в state:
  // самоперерисовки уже роняли qa:terminal. Переключатель features.trace
  // (план 9.1) читается при открытии терминала: выключенный — всё как раньше,
  // diag уходят в сокет, кольцо и запись вывода не ведутся.
  // Кольцо и запись вывода — в хранилище МОДУЛЯ по id терминала
  // (ptyTerm/traceStore.ts, последние 4 терминала): «Открыть заново» и уход с
  // экрана создают новый экземпляр, а трасса того, что было ДО переоткрытия,
  // нужна как раз тогда (ревью S1). seq продолжается, запись не обрывается.
  const [traceSlot] = useState(() => terminalTraceStore.acquire(id || ""));
  const traceRef = useRef<TerminalTrace | null>(null);
  if (!traceRef.current) traceRef.current = traceSlot.trace;
  // Запись вывода для воспроизведения — только явно и со сроком (I-15). Срок
  // лежит в хранилище, чтобы сценарий можно было снять с самого открытия
  // терминала (включил → перезапустил экран), и обрезается до TTL.
  // ⚠ Согласие — ЭТОГО терминала (волна 4): прежнее одно-на-страницу включало
  // запись в любом терминале, открытом за 30 минут после включения в другом.
  // Нет согласия у этого id — запись слота выключается явно: слот мог остаться
  // включённым с прошлого открытия, когда согласие уже сняли.
  const recRef = useRef<ByteRecorder | null>(null);
  const [initialCaptureUntil] = useState(() => (features.trace ? readCaptureUntil(captureStorage(), Date.now(), id || "") : 0));
  if (!recRef.current) {
    recRef.current = traceSlot.rec;
    if (initialCaptureUntil > 0) recRef.current.enable(initialCaptureUntil);
    else recRef.current.disable();
  }
  // Видимый признак записи вывода в шапке ЭТОГО терминала: срок, до которого
  // пишется; 0 — не пишется. React state меняется только по действию человека
  // и по истечении срока — не на каждый кадр.
  const [recUntil, setRecUntil] = useState(() => (recRef.current!.enabled ? recRef.current!.until : 0));
  useEffect(() => {
    if (recUntil <= 0) return;
    const left = recUntil - Date.now();
    if (left <= 0) { setRecUntil(0); return; }
    const timer = window.setTimeout(() => setRecUntil(0), Math.min(left + 50, 2 ** 31 - 1));
    return () => window.clearTimeout(timer);
  }, [recUntil]);
  // Один переиспользуемый объект контекста: rx зовёт трассу на каждом кадре
  // сокета, а note() копирует значения в слот кольца сразу.
  const traceCtxRef = useRef<TraceContext>({});
  const traceCtx = (): TraceContext => {
    const c = traceCtxRef.current;
    c.sid = traceRef.current!.session(id || "");
    c.gen = connectionGenRef.current;
    c.wep = writerEpochSeqRef.current;
    c.acc = offsetRef.current;
    c.app = appliedOffsetRef.current;
    c.qEnd = queueEndRef.current;
    c.qBytes = queueBytesRef.current;
    c.qLen = writeQueueRef.current.length;
    c.wPending = terminalWriterRef.current?.pending ?? 0;
    c.unacked = writeUnackedRef.current;
    c.geomRev = geometryRevisionRef.current;
    c.snapTok = snapshotTokenRef.current;
    c.paused = flowPausedRef.current;
    return c;
  };
  /** Событие решения в трассу; seq или −1 (трасса выключена). */
  const trace = (kind: TraceKind, fields?: Record<string, unknown> | null, reason?: string): number =>
    features.trace ? traceRef.current!.note(kind, traceCtx(), fields, reason) : -1;
  /**
   * Ввод человека в трассу (I-15, приватность). Второй путь помимо term.onData:
   * поле «Сообщение/команда», вставка из буфера, путь загруженного файла. Как и
   * onData, открывает отрезок набора и огрубляет его эхо — иначе на приглашении
   * sudo с pwfeedback эхо «*» вставленного пароля ложилось точным rx.bytes и
   * длина читалась мимо корзины ввода (скептик волны 6). Сами байты — НИКОГДА,
   * только грубая длина серии (terminalTrace.noteInput).
   */
  const noteHumanInput = (len: number, how: "key" | "encoded-paste"): void => {
    if (features.trace) traceRef.current!.noteInput(traceCtx(), { len, ok: true, auto: false }, how);
  };
  /**
   * ЕДИНСТВЕННЫЙ путь диагностики в журнал агента. Пишет событие в трассу и
   * отправляет, если сокет открыт: закрытый сокет больше не значит «событие
   * потеряно» — оно остаётся в кольце и попадёт в «Зафиксировать проблему».
   * Сообщение агенту то же, что раньше (I-14: {t:"diag", what, …поля}).
   * traceFields — если в трассу нужно записать не то же, что в журнал.
   */
  // Агент доказал, что он новый (screen-capability или поля /state, которых у
  // 2.71.1 нет): только ему уходят новые виды diag (ptyTerm/diagCompat.ts).
  const peerNewRef = useRef(false);
  const sendDiag = (
    what: string,
    fields: Record<string, unknown> = {},
    opts?: { sock?: WebSocket | null; traceFields?: Record<string, unknown> },
  ): void => {
    // null в журнале значит «нет значения»; в трассе такое поле просто не
    // пишется — иначе allowlist считал бы его ошибкой места вызова.
    if (features.trace) {
      trace("diag", opts?.traceFields
        ?? Object.fromEntries(Object.entries(fields).filter(([, v]) => v !== null && v !== undefined)), what);
    }
    // Старый агент печатает незнакомый вид строкой из <nil> (волна 4, I-14):
    // новому виду — только новому агенту; в трассе он уже есть.
    if (!diagReachesAgent(what, peerNewRef.current)) return;
    const socket = opts?.sock ?? wsRef.current;
    if (!socket || socket.readyState !== WebSocket.OPEN) return;
    try { socket.send(JSON.stringify({ t: "diag", what, ...fields })); } catch { /* диагностика не повод падать */ }
  };
  /** Служебное сообщение сервера (reset/resumed/screen/exit) в запись вывода,
   * привязанное к seq события трассы. Только при явно включённой записи. */
  const recordControl = (json: unknown, seq: number): void => {
    if (seq < 0 || typeof json !== "string") return;
    const rec = recRef.current!;
    if (rec.enabled) rec.noteControl(json, traceRef.current!.time(), seq);
  };
  // versionCode/versionName APK — только в нативном приложении и асинхронно.
  const apkInfoRef = useRef<{ version: string; build: string }>({ version: "", build: "" });
  useEffect(() => {
    if (!isNativeApp || !features.trace) return;
    let cancelled = false;
    void import("@capacitor/app")
      .then(({ App }) => App.getInfo())
      .then((info) => { if (!cancelled) apkInfoRef.current = { version: info.version || "", build: info.build || "" }; })
      .catch(() => { /* без versionCode трасса всё равно полезна */ });
    return () => { cancelled = true; };
  }, [features.trace]);
  /**
   * Кто написал трассу (ST-00, T-40): версия и commit клиента, чанк бандла,
   * поверхность, версия агента, APK и все переключатели терминала. Без этого
   * «проверили не ту сборку» не отличить от «сборка сломана».
   */
  const traceIdentity = (): TraceIdentity => {
    const tg = getTelegram();
    const term = terminalRef.current;
    return {
      client: CLIENT_VERSION,
      commit: CLIENT_COMMIT,
      bundle: TRACE_BUNDLE_CHUNK,
      surface: tg?.initData ? "telegram" : isNativeApp ? "apk" : "web",
      agent: getSelectedDeviceAgentVersion() || "",
      apkVersion: apkInfoRef.current.version,
      apkBuild: apkInfoRef.current.build,
      cols: term?.cols ?? 0,
      rows: term?.rows ?? 0,
      features: { ...features },
    };
  };
  // Глубина истории зеркала по последнему снапшоту. −1 = снапшота ещё не было.
  // Страница, открытая с сохранённым resume, знает её из хранилища (resume и
  // выбран потому, что она богатая): иначе запись позиции на маркере, раньше
  // первого кадра, стирала hist, и страница, закрытая до кадра, делала
  // следующее холодное открытие реплеем.
  const snapHistRef = useRef<number>(savedResume?.hist ?? -1);
  // Ревью 15.09 (дефект 1): строки, ушедшие в прокрутку от ЖИВОГО потока с
  // последнего кадра экрана, без затухания (в отличие от streamLinesRef). По
  // ним тёплое переподключение решает, могло ли зеркало разбогатеть
  // (shouldProbeHistory → причина hist-probe координатора).
  const linesSinceFrameRef = useRef(0);
  // Соединение (connectionGenRef), на котором уже просили кадр ради глубины
  // истории: такой кадр — не чаще одного за соединение.
  const histProbeConnRef = useRef(-1);
  // Тёплое соединение, чей маркер решил «кадр не нужен»: дельта и живой поток
  // идут ПОСЛЕ маркера (агент шлёт resumed раньше дельты, api_pty.go), и строки,
  // напечатанные, пока телефон был отключён, маркер ещё не видел. Первая такая
  // строка решает заново (noteStreamGrowth) — ревью 15.09, повторная проверка.
  const histProbeWatchRef = useRef(-1);
  // Номер операции координатора, чей кадр сейчас сам меняет сетку xterm
  // (adopt/restore): такой resize — не устаревание этого кадра (recovery.adopted).
  const adoptTicketRef = useRef<number | null>(null);
  // SOTA-снапшот: состояние «применять ли к кадру экрана присланную историю»
  // на текущее соединение. Сбрасывается в connect() (новое соединение — новое
  // решение), дальше ведут sync-маркеры и сам кадр (ptyTerm/snapshotHistory.ts).
  const snapshotHistoryRef = useRef<SnapshotHistory>(SNAPSHOT_HISTORY_INIT);
  // Печатал ли терминал ЭТОЙ СТРАНИЦЫ хоть байт вывода. НЕ сбрасывается в
  // connect(): признак живёт от маунта, и по нему noteSyncMarker отличает
  // свежую страницу с resume из localStorage (истории нет — снапшот применять)
  // от реконнекта живой страницы (история своя — чужая продублировала бы её).
  const termWroteRef = useRef(false);
  // Писать позицию на КАЖДОМ кадре нельзя: localStorage синхронный, а это
  // горячий путь вывода терминала. Поэтому по троттлу, но в моменты, когда
  // экран реально могут погасить (уход в фон, размонтирование, маркер
  // синхронизации), — немедленно, через force.
  const resumeSavedAt = useRef(0);
  const persistResume = useCallback((force = false) => {
    if (!id || !epochRef.current) return;
    const now = Date.now();
    if (!force && now - resumeSavedAt.current < 2000) return;
    resumeSavedAt.current = now;
    // Именно applied, а не offset: см. комментарий у appliedOffsetRef.
    // hist — глубина истории зеркала, замеренная последним снапшотом. По ней
    // СЛЕДУЮЩЕЕ холодное открытие решит, хватит ли снапшота вместо реплея
    // (shouldUseColdResume). Пока снапшота не было, поле не пишется вовсе:
    // «неизвестно» и «пусто» — разные вещи, и путать их нельзя.
    saveResumePos(id, {
      epoch: epochRef.current,
      offset: appliedOffsetRef.current,
      ...(snapHistRef.current >= 0 ? { hist: snapHistRef.current } : {}),
    });
  }, [id]);
  // Инвалидация позиции resume из горячего пути (дроп очереди в
  // enqueueTermWrite, у него пустые deps и он видел бы устаревший id — отсюда
  // ref по образцу scheduleFlushRef). Выброшенные из очереди байты уже
  // посчитаны в offsetRef (onmessage инкрементит его при ПРИЁМЕ кадра), поэтому
  // с момента дропа позиция — враньё: реконнект с ней пропустил бы выброшенный
  // кусок потока навсегда (дыра в выводе без маркера). Сбрасываем в «нет
  // позиции»: следующий коннект уйдёт в полный reset и получит новую эпоху.
  const invalidateResumeRef = useRef(() => {});
  invalidateResumeRef.current = () => {
    epochRef.current = "";
    offsetRef.current = 0;
    appliedOffsetRef.current = 0;
    queueEndRef.current = 0;
    handedMarkRef.current = null;
    // Глубина истории принадлежала ПРОШЛОЙ эпохе: перенести её на новую значит
    // решить следующее холодное открытие по чужим числам.
    snapHistRef.current = -1;
    if (id) clearResumePos(id);
  };
  // Номер попытки реконнекта живёт только в ref: на экран он больше не выходит
  // (счётчик «(3/10)» повторял верхнюю полосу связи и ничего не добавлял), а
  // решение «пора сдаваться» принимается по нему же.
  const [toast, setToast] = useState("");
  // Правка имени прямо в шапке: открыта ли она и что набрано.
  const [renaming, setRenaming] = useState(false);
  const [renameDraft, setRenameDraft] = useState("");
  const toastTimer = useRef<ReturnType<typeof setTimeout> | null>(null);
  const maxAttempts = 10;
  const [inputText, setInputText] = useSessionDraft(terminalContext, id ?? "");
  const [composing, setComposing] = useState(false);
  const [composerOpen, setComposerOpen] = useState(false);
  /**
   * Выбор под скрепкой: файл с телефона или снимок экрана компьютера.
   *
   * Замер 09.09.2026 (`build/qa/measure-input-bar.mjs`): на телефоне 320 px
   * кнопки строки ввода занимали 203 px из 320, полю ввода оставалось 71 px —
   * примерно восемь знаков. Отдельная 📷 стоила из них 44 px, то есть поле
   * росло с 71 до 121 px, как только она уходит. Обе двери ведут к одному:
   * положить путь к файлу туда, откуда агент умеет читать, — значит им место
   * под одной кнопкой.
   */
  const [attachMenuOpen, setAttachMenuOpen] = useState(false);
  const fileInputRef = useRef<HTMLInputElement>(null);
  const [uploading, setUploading] = useState(false);
  const [uploadProgress, setUploadProgress] = useState(0);
  const uploadAbortRef = useRef<AbortController | null>(null);
  // Пути файлов, догруженных при мёртвом сокете. Загрузка идёт по REST и живёт
  // независимо от WS: раньше обрыв моста отменял её (и abortUpload сносил
  // серверный частичный файл, обнуляя resume). Теперь байты доезжают, а путь
  // ждёт здесь и вставляется в терминал сразу после переподключения (ws.onopen).
  const pendingPathsRef = useRef<string[]>([]);
  const [restarting, setRestarting] = useState(false);
  // Идёт сохранение SSH-сервера из этого терминала (см. openSshFiles): второй
  // тап по скрепке не должен завести второй такой же хост.
  const [savingHost, setSavingHost] = useState(false);
  const [state, setState] = useState<PtyState>({ cwd: "", alive: true });
  const statePollGateRef = useRef<LatestOnlyGate | null>(null);
  if (!statePollGateRef.current) statePollGateRef.current = new LatestOnlyGate(id || "");
  const cwd = state.cwd;
  // Папка терминала в избранном? Раньше ответ на это был только внутри обзора
  // папок (долгий тап по строке или «📌 закрепить текущую»), и владелец не
  // нашёл, где добавлять: «работаю в терминале… не пойму где можно добавить»
  // (31.08.2026). Звезда живёт в шапке, рядом с папкой. null — ещё не знаем:
  // до ответа сервера кнопка не должна врать ни «в избранном», ни обратным.
  const [cwdPinned, setCwdPinned] = useState<boolean | null>(null);
  const [pinning, setPinning] = useState(false);
  // Активность агента: бейдж «Работает…/Готов» по паузе в выводе (см. эффект ниже).
  const [agentBusy, setAgentBusy] = useState(false);
  const lastOutputRef = useRef(0);
  // Агент не отвечает: ввод человека ушёл, вывода нет дольше AGENT_SILENCE_MS.
  const [agentStuck, setAgentStuck] = useState(false);
  const lastUserInputAtRef = useRef(0);
  const [folderOpen, setFolderOpen] = useState(false);
  const [downloadOpen, setDownloadOpen] = useState(false);
  const [snippetsOpen, setSnippetsOpen] = useState(false);
  const [snippetAgents, setSnippetAgents] = useState<AgentInfo[]>([]);
  // Ряд быстрых команд под терминалом: сворачивается в одну строку (высота
  // ряда — это минус ~40 px видимого лога на телефоне) и пополняется своими
  // командами. И то, и другое — настройки этого устройства, как размер шрифта.
  const [quickCmdsCollapsed, setQuickCmdsCollapsed] = useState<boolean>(() => {
    try { return localStorage.getItem("ptyQuickCmdsCollapsed") === "1"; } catch { return false; }
  });
  // Состояние жеста в ряду команд — в ref, а не в state: оно меняется на каждый
  // pointermove, и перерисовывать из-за этого ряд (а с ним и терминал) нельзя.
  const tapGuardRef = useRef(emptyTapGuard());
  // Свои команды живут на КОМПЬЮТЕРЕ одним списком, общим со шторкой «⚡
  // Команды» (см. ptyTerm/commands.ts). Раньше их было два, оба в localStorage
  // пульта, и заведённая с телефона в окне exe отсутствовала.
  const [userCmds, setUserCmds] = useState<UserCommand[]>([]);
  const customCmds = pinnedOf(userCmds).map((c) => c.cmd);
  useEffect(() => {
    let cancelled = false;
    loadUserCommands()
      .then((list) => { if (!cancelled) setUserCmds(list); })
      .catch(() => { /* связи нет — ряд останется со встроенными */ });
    return () => { cancelled = true; };
  }, []);
  // Убранные из ряда встроенные команды (dir, cls, ls…): список по команде,
  // а не по позиции — платформенный набор зависит от шелла (Windows/POSIX),
  // и замороженный слепок показывал бы dir в SSH-сессии на Linux.
  const [hiddenCmds, setHiddenCmds] = useState<string[]>(() => {
    try {
      const value = JSON.parse(localStorage.getItem("ptyQuickCmdsHidden") || "[]");
      return Array.isArray(value) ? value.filter((x): x is string => typeof x === "string") : [];
    } catch { return []; }
  });
  /**
   * Убрать системную клавиатуру iOS.
   *
   * `textInputRef.blur()` этого НЕ делал: у xterm есть своё скрытое textarea, и
   * фокус чаще всего сидит именно в нём (тап по терминалу, `cd`, вставка). Пока
   * оно в фокусе, iOS держит свою клавиатуру поднятой — владелец 08.09: «на
   * айфоне когда клавиатуру включаешь, он что-то свою выдвигает». Гасим ТО, что
   * в фокусе на самом деле, чем бы оно ни было.
   */
  const dismissSystemKeyboard = () => {
    const active = document.activeElement as HTMLElement | null;
    if (active && typeof active.blur === "function") active.blur();
  };
  const [cmdSheetOpen, setCmdSheetOpen] = useState(false);
  const [newCmd, setNewCmd] = useState("");
  // Правка своей команды — прямо в строке списка. Отдельного экрана не заводим:
  // до сих пор изменить команду было НЕЧЕМ, оставался путь «удалить и завести
  // заново», а он упирался в неработавший крестик (жалоба владельца 09.08).
  const [editCmd, setEditCmd] = useState<{ id: string; cmd: string; label: string } | null>(null);
  const [agentSheetOpen, setAgentSheetOpen] = useState(searchParams.get("agent") === "1");
  const [launchAttempt, setLaunchAttempt] = useState<AgentLaunchAttempt | null>(null);
  useEffect(() => {
    if (!launchAttempt) return;
    if (launchAttempt.sessionId !== id) { setLaunchAttempt(null); return; }
    if (isAgentKind(state.agent_kind)) {
      trackFirstAgent();
      setLaunchAttempt(null);
      return;
    }
    if (launchAttempt.expired) return;
    const timer = window.setTimeout(() => {
      setLaunchAttempt((current) => current === launchAttempt ? { ...current, expired: true } : current);
    }, Math.max(0, AGENT_LAUNCH_WAIT_MS - (Date.now() - launchAttempt.startedAt)));
    return () => window.clearTimeout(timer);
  }, [id, state.agent_kind, launchAttempt]);
  // Аккаунт, ради которого сюда пришли (`?account=` — аудит ИА 02.09.2026, D4):
  // «Войти» у аккаунта в разделе «Агенты» уводит в новый терминал, и шторка
  // должна запустить агента под ЭТИМ аккаунтом, а не под активным.
  const launchAccountId = searchParams.get("account") || "";
  const lastAgentKindRef = useRef<string>((() => {
    try { return id ? localStorage.getItem(`pty.lastAgent.${id}`) || "" : ""; } catch { return ""; }
  })());
  const lastAgentRouteRef = useRef(id || "");
  if (lastAgentRouteRef.current !== (id || "")) {
    lastAgentRouteRef.current = id || "";
    try {
      lastAgentKindRef.current = id ? localStorage.getItem(`pty.lastAgent.${id}`) || "" : "";
    } catch {
      lastAgentKindRef.current = "";
    }
  }
  const loadedResumeKeyRef = useRef("");
  const [loadedResumeAgent, setLoadedResumeAgent] = useState<{
    key: string;
    kind: string;
    agent: AgentInfo;
  } | null>(null);
  const resumeIsLocal = state.kind !== "ssh" && state.remote !== true;
  const currentResumeKind = isAgentKind(state.agent_kind)
    ? state.agent_kind!
    : (state.sleep?.agent || lastAgentKindRef.current);
  const currentResumeKey = resumeIsLocal && currentResumeKind
    ? `${id || ""}\n${currentResumeKind}`
    : "";
  // Старый GET /api/agents может завершиться после перехода на другой id.
  // Такой результат даже на один render не должен стать кнопкой Resume.
  const resumeAgent = loadedResumeAgent?.key === currentResumeKey ? loadedResumeAgent : null;
  const resumeAccountsState = useAgentAccounts(
    Boolean(resumeAgent?.agent.account_env) && resumeIsLocal,
    cwd,
  );
  const resumeRecordedAccount = resumeAgent?.agent.account_env && resumeAccountsState.accountsReady
    ? recordedResumeAccount(resumeAccountsState.accounts, resumeAgent.kind, state.account_id)
    : null;
  const resumeLaunchAccount = resumeAgent && resumeIsLocal
    ? launchAccountForAgent(resumeAgent.agent, resumeAccountsState, resumeRecordedAccount)
    : undefined;
  const resumePosix = platform
    ? platform !== "windows"
    : !(/powershell|cmd(?:\.exe)?$/i.test(state.shell || "") || /^[A-Za-z]:[\\/]/.test(cwd || ""));
  const resumeCommand = resumeAgent && resumeIsLocal
    ? composeLaunch(resumeAgent.agent.resume_cli || "", EMPTY_PREFS, {
      agentID: resumeAgent.agent.id,
      accountEnvName: resumeAgent.agent.account_env || "",
      proxyContractVersion: resumeAccountsState.proxyContractVersion,
      blockedReason: resumeLaunchAccount?.blockedReason,
      account: resumeLaunchAccount,
      posix: resumePosix,
      launchArgs: resumeAgent.agent.launch_args,
    })
    : "";
  const resumeBlockedReason = resumeLaunchAccount?.blockedReason || (
    resumeAgent && !resumeCommand ? t("ui.ptytermview.m08af1cd944") : ""
  );
  const [searchOpen, setSearchOpen] = useState(false);
  const [exportOpen, setExportOpen] = useState(false);
  const [exporting, setExporting] = useState(false);
  // «Зафиксировать проблему»: предпросмотр снимается по действию человека, а
  // не на каждое событие трассы — трасса в state не живёт.
  const [traceView, setTraceView] = useState<TracePreview | null>(null);
  const [traceBusy, setTraceBusy] = useState(false);
  const [traceTgConfirm, setTraceTgConfirm] = useState(false);
  // «Очистить историю на этом устройстве» (ST-04): второй шаг с честным текстом.
  const [clearConfirm, setClearConfirm] = useState(false);
  // «Функции терминала» (план 9.1, волна 4): откат функции с телефона.
  const [featuresOpen, setFeaturesOpen] = useState(false);
  useEffect(() => {
    if (!exportOpen) { setTraceView(null); setTraceTgConfirm(false); setClearConfirm(false); setFeaturesOpen(false); }
  }, [exportOpen]);
  const [selectMode, setSelectMode] = useState(false);
  const selectModeRef = useRef(selectMode);
  selectModeRef.current = selectMode;
  const suppressTerminalTapUntilRef = useRef(0);
  const [readView, setReadView] = useState<Omit<ReadSurfaceProps, "hasNewOutput" | "onClose"> | null>(null);
  const readWindowAnchorRef = useRef<ReturnType<typeof registerReadAnchor>>(undefined);
  const streamGapRef = useRef(false);
  const [readHasNewOutput, setReadHasNewOutput] = useState(false);
  const readRequestRef = useRef(0);
  useEffect(() => () => readWindowAnchorRef.current?.dispose(), []);
  useEffect(() => {
    // Navigating to another terminal is a different document/source. A
    // reconnect to this terminal can keep a frozen page, but navigation cannot.
    historyClientRef.current.reset(); setHistoryOpen(false); setHistoryAvailable(false);
    readWindowAnchorRef.current?.dispose(); readWindowAnchorRef.current = undefined;
    readRequestRef.current++; streamGapRef.current = false; setReadHasNewOutput(false);
    setReadView(null); setSelectMode(false); selectModeRef.current = false;
  }, [id]);
  const beginReadingRef = useRef<(point?: { x: number; y: number }, all?: boolean) => void>(() => {});
  const inputSendPendingRef = useRef(false);
  const inputTextRef = useRef(inputText);
  inputTextRef.current = inputText;
  const inputTargetRef = useRef<() => InputTarget & { draft: string }>(() => ({ transport: null, identity: "", bracketed: false, draft: "" }));
  inputTargetRef.current = () => ({
    transport: disposedRef.current ? null : wsRef.current,
    identity: JSON.stringify([getTerminalContextKey(), id, connectionGenRef.current, writerEpochRef.current, scrollClassifierKeyRef.current]),
    draft: draftOwner(getTerminalContextKey(), id ?? ""),
    bracketed: terminalRef.current?.modes.bracketedPasteMode === true,
  });
  const [helpOpen, setHelpOpen] = useState(false);
  // Agent entry has its own visible CTA. Auto-expanding all tools on first
  // entry would consume the entire output area on a narrow phone.
  const [toolsOpen, setToolsOpen] = useState(false);
  // Предупреждение агента о неизбежном перезапуске (автообновление): без него
  // человек видел молчаливый обрыв и «Переподключение… (1/10)».
  const [pcUpdating, setPcUpdating] = useState<string | null>(null);
  const pcUpdatingAtRef = useRef(0);
  // Верхняя полоса связи (ConnectionBanner в App.tsx) СЕЙЧАС говорит про обрыв.
  // Нужно не для реконнекта, а для молчания: об одном обрыве человек читал два
  // сообщения сразу — «Переподключение…» полосой сверху и «Переподключение…
  // (3/10)» посреди экрана. Пока говорит полоса, оверлей терминала молчит.
  const [linkSpeaks, setLinkSpeaks] = useState(false);
  // Имя терминала из прошлого захода — до первого ответа /state (см. readTitleCache).
  const [cachedTitle, setCachedTitle] = useState(() => readTitleCache(id));
  useEffect(() => {
    if (!snippetsOpen) return;
    // В SSH-терминале подменяем команду установки на POSIX-вариант: `install`
    // резолвится по ОС компьютера-бастиона, и с Windows-ПК в bash сервера уехал
    // бы `winget install …`. Раньше список для SSH не грузился вовсе — и
    // установить агента на сервер было нечем ни отсюда, ни из шторки «🤖».
    const forServer = state.kind === "ssh" || state.remote === true;
    getAgents()
      .then((d) => setSnippetAgents((d.agents || []).map((a) => (
        forServer ? { ...a, install: a.install_posix || a.install } : a
      ))))
      .catch(() => setSnippetAgents([]));
  }, [snippetsOpen, state.kind, state.remote]);
  useEffect(() => {
    if (state.kind === "ssh" || state.remote === true) {
      loadedResumeKeyRef.current = "";
      setLoadedResumeAgent(null);
      return;
    }
    // Спящий агент: в терминале сейчас шелл, а на этом устройстве подсказки
    // «какой агент тут был» может не быть вовсе — его называет запись сна.
    const kind = isAgentKind(state.agent_kind) ? state.agent_kind! : (state.sleep?.agent || lastAgentKindRef.current);
    if (!kind) {
      loadedResumeKeyRef.current = "";
      setLoadedResumeAgent(null);
      return;
    }
    lastAgentKindRef.current = kind;
    try { if (id) localStorage.setItem(`pty.lastAgent.${id}`, kind); } catch { /* ignore */ }
    const key = `${id || ""}\n${kind}`;
    if (loadedResumeKeyRef.current === key) return;
    loadedResumeKeyRef.current = key;
    setLoadedResumeAgent(null);
    let cancelled = false;
    getAgents().then((d) => {
      if (cancelled || loadedResumeKeyRef.current !== key) return;
      const agent = (d.agents || []).find((a) => a.id === kind && a.supports_resume && a.resume_cli);
      setLoadedResumeAgent(agent ? { key, kind, agent } : null);
    }).catch(() => {
      if (!cancelled && loadedResumeKeyRef.current === key) setLoadedResumeAgent(null);
    });
    return () => { cancelled = true; };
  }, [id, state.agent_kind, state.sleep?.agent, state.kind, state.remote]);
  const FONT_SIZES = [11, 12, 14, 16] as const;
  const [fontSize, setFontSize] = useState<number>(() => {
    try {
      const stored = Number(localStorage.getItem("pty.fontSize") || 14);
      return FONT_SIZES.includes(stored as typeof FONT_SIZES[number]) ? stored : 14;
    } catch {
      return 14;
    }
  });
  const [termSize, setTermSize] = useState({ cols: 80, rows: 24 });
  const [scrollButtonsVisible, setScrollButtonsVisible] = useState(true);
  const [localScrolledUp, setLocalScrolledUp] = useState(false);
  const scrollButtonsTimer = useRef<number | null>(null);
  // The visible mode belongs to the person and this terminal, not the current
  // foreground process. Auto keeps learning as programs start and stop.
  const [scrollOverride, setScrollOverride] = useState<ScrollOverride>(() =>
    readScrollPreference(terminalContext, id ?? ""));
  const scrollOverrideRef = useRef<ScrollOverride>(scrollOverride);
  scrollOverrideRef.current = scrollOverride;
  const scrollClassifierKeyRef = useRef("");
  // Панель спецклавиш (стрелки и кнопки инструментов) сворачивается, чтобы
  // терминал занимал весь экран. Выбор запоминаем между сессиями.
  const [keysCollapsed, setKeysCollapsed] = useState(() => {
    try { return localStorage.getItem("pty.keysCollapsed") === "1"; } catch { return false; }
  });
  const [shortLandscape, setShortLandscape] = useState(() =>
    isShortLandscape(window.screen.width, window.screen.height, window.innerHeight));
  const [landscapeKeysOpen, setLandscapeKeysOpen] = useState(() => {
    try { return sessionStorage.getItem("pty.landscapeKeysOpen") === "1"; } catch { return false; }
  });
  useEffect(() => {
    const measure = () => {
      const short = isShortLandscape(window.screen.width, window.screen.height, window.innerHeight);
      setShortLandscape(short);
    };
    window.addEventListener("resize", measure);
    window.screen.orientation?.addEventListener?.("change", measure);
    measure();
    return () => {
      window.removeEventListener("resize", measure);
      window.screen.orientation?.removeEventListener?.("change", measure);
    };
  }, []);
  const keysCollapsedForView = shortLandscape ? !landscapeKeysOpen : keysCollapsed;
  const toggleKeys = () => {
    haptic();
    if (shortLandscape) {
      const next = !landscapeKeysOpen;
      setLandscapeKeysOpen(next);
      try { sessionStorage.setItem("pty.landscapeKeysOpen", next ? "1" : "0"); } catch { /* private mode */ }
      if (next) dismissSystemKeyboard();
      return;
    }
    const next = !keysCollapsed;
    setKeysCollapsed(next);
    try { localStorage.setItem("pty.keysCollapsed", next ? "1" : "0"); } catch { /* private mode */ }
    // Открываем свою панель — системная клавиатура обязана уйти, иначе на
    // iPhone они стоят друг на друге и от терминала не остаётся ничего.
    if (!next) dismissSystemKeyboard();
  };
  // IME-безопасное поле ввода: после отправки фокус должен возвращаться СЮДА, а
  // не в xterm — иначе Gboard теряет автозамену и предиктивный ввод, и следующая
  // фраза летит в терминал посимвольно. На десктопе (окно exe, веб с мышью)
  // фокус в терминале осмыслен: дальше печатают с железной клавиатуры.
  const textInputRef = useRef<HTMLTextAreaElement | null>(null);
  const touchFirstUi = isNativeApp
    || !!getTelegram()?.initData
    || (typeof window !== "undefined" && !!window.matchMedia?.("(pointer: coarse)").matches);
  // Живой агент в foreground — читается из обработчиков, у которых не должно
  // меняться тождество (scrollByLines сидит в зависимостях жестов).
  const agentInFgRef = useRef(false);
  // Вид агента строкой: нужен тому же обработчику прокрутки, чтобы спросить у
  // реестра, чем у этого агента открывается его собственная история.
  const agentKindRef = useRef("");
  const altScrollWarnAtRef = useRef(0);
  // Когда терминал последний раз что-то прислал (см. enqueueTermWrite) и
  // запущенная проверка «ответил ли он на прокрутку» (см. scrollByLines).
  const lastOutputAtRef = useRef(0);
  const altScrollProbeRef = useRef<number | null>(null);
  const altScrollProbeTokenRef = useRef(0);
  // Actions arriving during the 1.2 s channel probe keep their exact payload.
  // A scalar line sum loses Ctrl+Home/Ctrl+End semantics: a rapid ⇈ then ⇊
  // used to replay as generic pages (or rows×5 fallback) instead of an edge.
  const altScrollQueuedActionsRef = useRef<PendingScrollAction[]>([]);
  // СВИДЕТЕЛЬСТВА КАНАЛОВ НАВИГАЦИИ (ptyTerm/navigationEvidence.ts, план 13.09
  // ST-02). До этого здесь жили четыре флага pageAnswered/wheelAnswered с
  // масками направлений, и живой `false` не устаревал никогда: аудит 13.09 A01 —
  // после отрицательной пробы тот же процесс снова отвечает на PgUp, ручной
  // «Агент» листает, а «Авто» до переоткрытия терминала шлёт жест в локальную
  // историю. Теперь актуальность (срок, отсрочка, значимый новый вывод, область
  // действия) проверяется В МОМЕНТ РЕШЕНИЯ, одним правилом для живого экрана и
  // для памяти (scrollProbeMemory.ts — только сериализация).
  const evidenceRef = useRef(new NavigationEvidence());
  // Закреплённый источник чтения (ST-03): человек уже читает старый текст, и
  // новый ответ агента или смена плотности потока не переключают ему источник
  // между свайпами. Снимается возвратом к live, сменой режима, области действия
  // (процесс, режим буфера и мыши) или замолчавшим закреплённым каналом.
  const readingPinRef = useRef<ReadingPin | null>(null);
  // Поколение переднего процесса (время старта с компьютера, 0 — неизвестно):
  // PID без поколения не ключ (ST-02), номер мог достаться новому процессу.
  const fgStartedRef = useRef(0);
  // Прежний вердикт замера плотности — для гистерезиса у порога (A02, T-04).
  const streamOwnerRef = useRef<HistoryOwner>("unknown");
  // Наблюдение итога без удержания очереди (канал подтверждён слабо, «verify»).
  const altScrollObserveRef = useRef<number | null>(null);
  // Досмотр позднего ответа после молчаливой пробы (T-08): ответ, пришедший
  // через 1,5–3 с, повышает свидетельство, но само действие НЕ повторяется.
  const altScrollLateRef = useRef<number | null>(null);
  // Память свидетельств на время жизни процесса (ptyTerm/scrollProbeMemory.ts).
  // Колбэки объявлены ниже — им нужны terminalContext/id и область действия, —
  // а зовутся из более ранних обработчиков (onopen, sync-маркер), поэтому через ref.
  const persistProbeVerdictRef = useRef<() => void>(() => {});
  const restoreProbeVerdictRef = useRef<() => void>(() => {});
  // Все отложенные наблюдения прокрутки одним вызовом: проба с очередью,
  // наблюдение итога и досмотр позднего ответа. Отправленное не отзывается.
  const cancelScrollObservations = () => {
    for (const timer of [altScrollProbeRef, altScrollObserveRef, altScrollLateRef]) {
      if (timer.current != null) { clearTimeout(timer.current); timer.current = null; }
    }
  };
  const scrollbackEraseGateRef = useRef<ScrollbackEraseGate>(null!);
  if (!scrollbackEraseGateRef.current) scrollbackEraseGateRef.current = new ScrollbackEraseGate();
  // Parser state is declared later, beside the write queue. The callback ref
  // lets a synchronous mode choice invalidate and discard deferred erase.
  const invalidatePendingScrollbackEraseRef = useRef<() => void>(() => {});
  // ── ХРАНЕНИЕ ИСТОРИИ (ST-04, план 13.09) ─────────────────────────────
  // Судьбу CSI 3 J решает политика ПОКОЛЕНИЯ переднего процесса
  // (keepHistory.retentionFor), а не владелец истории навигации: замер
  // плотности (31/32/33 строки на 16 КиБ) и ручной режим «Вывод»/«Агент» больше
  // не меняют состав истории (I-02, T-04). Переключатель features.retention
  // (план 9.1): legacy — прежнее правило; shadow — исполняет прежнее, а
  // расхождения уходят в журнал агента; policy — исполняет политика.
  // Всё новое поведение ST-04 исполняется ТОЛЬКО при === "policy": снятие
  // отложенного на границе поколения, листание reader по historySeq, текст
  // «ранее стёрто» (erasedBefore) и кнопка «Очистить историю на этом
  // устройстве». shadow в этих точках делает то же, что legacy, и лишь пишет
  // retention-shadow (точки chain/pending/strip/mode/generation/page).
  // Прежнее решение считается ВСЕГДА, в тех же точках и тем же вызовом: у
  // владельца истории есть побочный эффект — гистерезис замера плотности для
  // навигации, и хранение не должно обратным ходом менять навигацию.
  const retentionRef = useRef<RetentionPolicy>(retentionFor({ generation: "", agentInFg: false, declared: "" }));
  // Номер поколения для журнала: строковый ключ процесса бывает длиннее поля.
  const retentionGenRef = useRef(0);
  const retentionShadowRef = useRef(new RetentionShadowLog());
  // События, реально уносившие историю: исполненный CSI 3 J в обычном буфере и
  // RIS. Считает сам разборщик xterm (обработчики в эффекте терминала) — значит
  // и досылку отложенного, и цепочку у низа, и ручную очистку, и reset, и RIS
  // приложения. Листание reader сверяет его вместо эпохи writer (ST-04, T-14).
  const historySeqRef = useRef(0);
  // Начало буфера — след стирания (программой или вручную), а не начало вывода
  // (I-11). Снимает только наш RIS: буфер после него собирается заново.
  const erasedInGenerationRef = useRef(false);
  // Буфер собран нашим RIS из ХВОСТА потока эпохи (I-11, волна 4): маркер
  // reset с базой > 0 или кадр с историей зеркала на её пределе. Ставится там
  // же, где наш RIS снимает erasedInGeneration (правило — ReadDocument.ringCutAfterRis).
  const ringTruncatedRef = useRef(false);
  const noteRetentionShadow =(point: string, legacy: string, policy: string, reading: boolean) => {
    if (!retentionShadowRef.current.first(String(retentionGenRef.current), `${point}:${legacy}:${policy}:${reading}`)) return;
    sendDiag("retention-shadow", { legacy, policy, reading, gen: retentionGenRef.current, point });
  };
  /** Что сделать с CSI 3 J в точке решения — то, что исполняет переключатель. */
  const eraseDecision = (term: Terminal, reading: boolean, point: string): ScrollbackEraseAction => {
    const legacy = scrollbackEraseAction(historyOwnerRef.current(term), reading);
    const policy = eraseActionFor(retentionRef.current, reading);
    const route = eraseRoute(features.retention, legacy, policy);
    if (route.diverged) noteRetentionShadow(point, legacy, policy, reading);
    return route.use;
  };
  /** Держать ли уже отложенное стирание в точке решения. */
  const keepPendingDecision = (term: Terminal, point: string): boolean => {
    const legacy = keepPendingScrollbackErase(historyOwnerRef.current(term));
    const policy = keepPendingErase(retentionRef.current);
    const route = eraseRoute(features.retention, legacy, policy);
    if (route.diverged) noteRetentionShadow(point, legacy ? "keep" : "drop", policy ? "keep" : "drop", false);
    return route.use;
  };
  const chooseScrollOverride = useCallback((
    next: ScrollOverride,
  ) => {
    // Publish synchronously: a wheel/touch event can arrive before React's next
    // render. A verdict that belonged to auto mode must not leak into a human
    // override (or fire its delayed fallback after the choice changed).
    cancelGestureRef.current();
    scrollOverrideRef.current = next;
    // Auto can itself resolve to terminal. Publish the override first, then ask
    // the same live owner resolver used by wheel/touch and erase callbacks.
    const live = terminalRef.current;
    const nextOwner = live
      ? historyOwnerRef.current(live)
      : effectiveHistoryOwner("unknown", next);
    // Смена режима навигации — не граница хранения (ST-04, I-02): в политике
    // отложенное стирание переживает Агент → Вывод → Агент и снимается только
    // границей поколения или reset. Прежнее правило снимало его навсегда.
    if (navigationChoiceDropsPendingErase(features.retention, nextOwner)) {
      invalidatePendingScrollbackEraseRef.current();
      if (features.retention === "shadow") noteRetentionShadow("mode", "drop", "keep", false);
    }
    setScrollOverride(next);
    if (altScrollProbeRef.current != null) {
      clearTimeout(altScrollProbeRef.current);
      altScrollProbeRef.current = null;
    }
    // Смена режима отменяет неотправленное, но уже отправленные байты «назад»
    // не забирает (ST-03). Наблюдения автоматики принадлежали прежнему режиму.
    cancelScrollObservations();
    altScrollProbeTokenRef.current++;
    altScrollQueuedActionsRef.current = [];
    evidenceRef.current.clear();
    readingPinRef.current = null;
  }, []);
  // React can reuse this view for another route. Restore before paint/input so
  // the previous terminal's forced mode never handles the new terminal's data.
  useLayoutEffect(() => {
    chooseScrollOverride(readScrollPreference(terminalContext, id ?? ""));
  }, [terminalContext, id, chooseScrollOverride]);
  // Приговор пробы (PgUp/колесо ответили или нет) переживает закрытие экрана и
  // новое соединение, пока процесс на переднем плане тот же. Боевой лог
  // 06.09.2026: терминал Codex за минуту открыт дважды, и оба раза первый жест
  // уходил PgUp в Codex, который не отвечает, — свою историю человек получал
  // через 1,2 с пробы. Замер потока агентов больше не разделяет (Codex 1,9
  // строк/КБ при пороге 2), надёжен только факт ответа — его и помним.
  // Область действия свидетельства: процесс И его поколение, эпоха потока,
  // режим буфера и мыши, объявленный реестром канал, версия адаптера
  // (navigationEvidence.evidenceScopeKey). Сменилась — прежние наблюдения чужие.
  const evidenceScope = () => {
    const live = terminalRef.current;
    return evidenceScopeKey({
      process: scrollClassifierKeyRef.current,
      generation: fgStartedRef.current,
      epoch: confirmedEpochRef.current,
      buffer: live?.buffer.active.type ?? "normal",
      mouse: live?.modes.mouseTrackingMode ?? "none",
      channel: declaredScrollChannel(agentKindRef.current),
    });
  };
  const evidenceScopeRef = useRef(evidenceScope);
  evidenceScopeRef.current = evidenceScope;
  const observedProbeMemoryRef = useRef("");
  const persistProbeVerdict = useCallback(() => {
    const scope = evidenceRef.current.currentScope();
    if (!scope) return;
    observedProbeMemoryRef.current = JSON.stringify([terminalContext, id, scope]);
    saveScrollProbeVerdict(terminalContext, id ?? "", scope, {
      page: storedEvidence(evidenceRef.current.get("page")),
      wheel: storedEvidence(evidenceRef.current.get("wheel")),
    }, Date.now());
  }, [terminalContext, id]);
  persistProbeVerdictRef.current = persistProbeVerdict;
  /**
   * Сверить область действия и дополнить живые свидетельства памятью той же
   * области. Новая область стирает прежние наблюдения и закрепление чтения;
   * живое наблюдение канала всегда дороже памяти, а отказ хранилища его не
   * уничтожает (T-03).
   */
  const restoreProbeVerdict = useCallback(() => {
    const scope = evidenceScopeRef.current();
    if (!scope) return;
    const ev = evidenceRef.current;
    if (ev.setScope(scope)) {
      // Закрепление, наблюдения и неотправленная очередь принадлежали прежней
      // области (процесс, режим буфера и мыши, канал реестра).
      readingPinRef.current = null;
      cancelScrollObservations();
      altScrollQueuedActionsRef.current = [];
    }
    const memoryContext = JSON.stringify([terminalContext, id, scope]);
    // On the first matching restore, bytes received before the foreground
    // response include replay/bootstrap. Their arrival time cannot invalidate
    // a verdict from before reopening. Once observed here, later restores keep
    // the normal new-output expiry rule (ARCH-010).
    const observedHere = observedProbeMemoryRef.current === memoryContext;
    // Шестой аргумент — когда процесс печатал в последний раз: неподтверждённое
    // наблюдение без якоря экрана не должно переживать свою причину.
    const stored = readScrollProbeVerdict(
      terminalContext, id ?? "", scope, Date.now(),
      undefined, observedHere ? lastOutputAtRef.current : undefined,
    );
    if (!stored) return;
    observedProbeMemoryRef.current = memoryContext;
    let restored = false;
    for (const channel of STORED_CHANNELS) {
      const record = stored[channel];
      if (record && ev.restore(channel, restoredObservation(record))) restored = true;
    }
    if (!restored) return;
    sendDiag("alt-scroll-verdict-restored", {
      process: scrollClassifierKeyRef.current,
      page: ev.get("page")?.state ?? null, wheel: ev.get("wheel")?.state ?? null,
    });
  }, [terminalContext, id]);
  restoreProbeVerdictRef.current = restoreProbeVerdict;
  // Остаток строк страничного канала между кадрами анимации (pagesFromAccum).
  const pageAccumRef = useRef(0);
  // Бюджет страниц на ОДИН жест пальца. Обнуляется касанием, тратится
  // отправками и НЕ пополняется инерцией: потолок «три страницы» до 2.57.12
  // считался на каждый вызов, а вызовов за один flick десятки (замер повторного
  // аудита: v0=8 px/мс → 8 страниц, накопленная тысяча строк → 66 страниц).
  const pageBudgetRef = useRef(MAX_PAGES_PER_GESTURE);
  // Когда запас страниц пополнялся в последний раз (см. refillPageBudget).
  const pageBudgetAtRef = useRef(0);
  // Когда последний раз просили свежий кадр из-за несовпавшей геометрии.
  const geomResyncAtRef = useRef(0);
  const GEOM_RESYNC_GAP_MS = 5000;
  // Жест пришёл КНОПКОЙ, а не пальцем. Накопитель страниц — защита от медленного
  // свайпа, который иначе шлёт страницу на каждый кадр; к нажатию кнопки он не
  // относится: одно нажатие обязано пролистать сразу, иначе кнопка «мёртвая».
  const altScrollButtonRef = useRef(false);
  // Докрутка по инерции. Своему вьюпорту она полезна (жест продолжается
  // пикселями), страничному каналу — нет: шаг там дискретный, и «доехавшие»
  // после отпускания пальца страницы человек уже не связывает со своим жестом.
  const altScrollInertiaRef = useRef(false);
  // ЗАМЕР ПОТОКА текущего приложения — им и решается, чья это история
  // (ptyTerm/altScroll.ts, historyOwnerFromStream). Считаем строки, ушедшие в
  // scrollback от ЖИВОГО вывода, и байты этого вывода; снапшот и реплей истории
  // сюда не входят — они не говорят, что делает приложение СЕЙЧАС.
  const streamLinesRef = useRef(0);
  const streamBytesRef = useRef(0);
  // Diagnostics belong to the same foreground/stream ownership generation.
  const diagSentRef = useRef(false);
  const diagOwnerRef = useRef<HistoryOwner | "">("");
  // Досылка остатка страничного канала, когда жест кончился. Без неё короткий
  // свайп (меньше полэкрана) не делал НИЧЕГО — снаружи «скролл не всегда
  // работает». Жест считается законченным, если новых строк не пришло 200 мс.
  const pageIdleTimerRef = useRef<ReturnType<typeof setTimeout> | null>(null);
  // Сколько ждём ответа приложения на отправленное колесо. Принявшее его
  // приложение (vim, htop, less) перерисовывает экран сразу; секунда с запасом
  // покрывает и медленную мобильную связь через облако.
  const ALT_SCROLL_PROBE_MS = 1200;
  // Как часто повторяем объяснение. Жест из десятков шагов иначе утопил бы
  // экран в тостах.
  const ALT_SCROLL_WARN_GAP_MS = 10000;
  // Коалесцинг прокрутки alt-screen (см. scrollByLines/flushAltScroll): палец
  // и инерция зовут её на каждый touchmove и кадр инерции, и раньше каждый
  // вызов уходил отдельным ws.send через три плеча до компьютера — ответная
  // перерисовка TUI приходила с лагом, и скролл выглядел мёртвым. Копим
  // строки до ближайшего кадра анимации и шлём одним сообщением.
  const altScrollAccumRef = useRef(0);
  const altScrollRafRef = useRef<number | null>(null);
  // Уходящий канал уже забит недоставленным — накопленное пропускаем:
  // докручивать жест секундной давности хуже, чем потерять пару строк.
  const ALT_SCROLL_BUFFER_CAP = 64 * 1024;
  // Тост «нет связи» из прокрутки — не чаще раза в 5 с, иначе драг из
  // десятков шагов утопил бы экран в тостах (та же логика, что WARN_GAP выше).
  const scrollOfflineWarnAtRef = useRef(0);
  const SCROLL_OFFLINE_WARN_GAP_MS = 5000;
  /**
   * Сколько молчания после ввода человека считать «агент не отвечает».
   *
   * Восемь секунд намеренно много: живой агент отвечает эхом мгновенно, так
   * что это не про задержку сети, а про тишину. Порог меньше давал бы ложную
   * тревогу на медленном канале, больше — человек успел бы решить, что сломано
   * приложение (01.09.2026 он именно так и решил).
   */
  const AGENT_SILENCE_MS = 8000;
  // Сколько ждём перед просьбой о кадре экрана: за это время fit() домеряет
  // высоту и размер PTY успевает устояться (см. onopen).
  const SCREEN_REQUEST_DELAY_MS = 500;
  // Человек прокрутил вверх полноэкранное приложение — значит оно придержало
  // новый вывод и ждёт, когда его отпустят вниз (см. кнопку «вернуться к
  // новому» в разметке).
  const [altScrolledUp, setAltScrolledUp] = useState(false);

  useEffect(() => {
    sessionMissingRef.current = false;
    setSessionMissing(false);
    setGaveUp(false);
    // Открыли другой терминал — и заголовок «на первый кадр» тоже его.
    setCachedTitle(readTitleCache(id));
    setComposerOpen(false);
  }, [terminalContext, id]);

  const revealScrollButtons = useCallback(() => {
    setScrollButtonsVisible(true);
    if (scrollButtonsTimer.current) window.clearTimeout(scrollButtonsTimer.current);
    scrollButtonsTimer.current = window.setTimeout(() => setScrollButtonsVisible(false), 2000);
  }, []);

  useEffect(() => {
    revealScrollButtons();
    return () => {
      if (scrollButtonsTimer.current) window.clearTimeout(scrollButtonsTimer.current);
    };
  }, [revealScrollButtons]);

  // Reset the reconnect budget only once the link proves real (see onopen).
  const markStable = useCallback(() => {
    if (stableTimer.current) { clearTimeout(stableTimer.current); stableTimer.current = null; }
    attemptRef.current = 0;
  }, []);

  /**
   * Сообщить PTY размер окна. Размер у терминала ОБЩИЙ для всех, кто его открыл
   * (сервер держит его по самому узкому зрителю — Session.ResizeFor), поэтому
   * шлём его не при каждом шевелении вёрстки, а только когда есть что сказать:
   *
   *  • страница не видна (телефон в кармане, окно exe свёрнуто) — молчим: пока
   *    мы в фоне, наш размер ничего не значит, а верстку у того, кто СЕЙЧАС
   *    работает на другом экране, он зажимает;
   *  • cols/rows не изменились — молчим: ResizeObserver срабатывает и на
   *    прокрутку, и на смену темы, а каждый resize перерисовывает TUI агента;
   *  • вырожденные 1×1 (контейнер скрыт, fit() посчитал по нулевой высоте) не
   *    отправляем никогда — именно от них Claude Code/Codex складывались в кашу.
   *
   * force — новый сокет: сервер нашего размера ещё не знает, отправляем всегда.
   *
   * Не-force вызовы ещё и ДЕБАУНСЯТСЯ (RESIZE_QUIET_MS тишины перед отправкой):
   * анимация клавиатуры/поворота на телефоне даёт серию промежуточных высот, и
   * на КАЖДУЮ из них ConPTY просил TUI перерисовать кадр — Codex стробил и
   * терял вывод прямо во время работы (жалоба 2026-07-29). Интересует только
   * финальный устоявшийся размер.
   */
  /**
   * Экранная клавиатура НЕ меняет размер PTY.
   *
   * Открытие клавиатуры отнимает у вьюпорта 250–400 px, и раньше мы честно
   * пересчитывали rows и слали новый размер агенту. Для полноэкранного TUI это
   * полная перерисовка всей переписки — и дважды за круг «открыл, написал,
   * закрыл». В боевом логе такие смены дают 81% ВСЕХ resize (171 из 211), и
   * каждая стоит агенту сотен строк вывода заново (жалоба владельца 04.08:
   * «нельзя, чтобы клавиатура так мешала при открытии?»).
   *
   * Вместо этого терминал остаётся прежнего размера, а видимое окно показывает
   * его НИЗ — то место, где печатает агент и стоит курсор. Верх уходит за
   * верхний край коробки (`overflow: hidden`), как будто мы прокрутили страницу
   * к последней строке. Уменьшать rows тут нечем помочь: агенту всё равно
   * пришлось бы перерисовать кадр под новый размер, а человеку при наборе нужен
   * именно низ.
   *
   * Почему нельзя просто «не звать fit()»: xterm продолжит рисовать прежние
   * rows в уменьшившуюся коробку, и `overflow: hidden` срежет их СНИЗУ — ровно
   * там, где курсор (это была жалоба 2.49.12). Поэтому сдвигаем сами.
   */
  const KEYBOARD_MIN_PX = 150; // клавиатура — сотни px; панель браузера 50–60
  const keyboardOpenRef = useRef(false);
  const baseViewportRef = useRef(0);
  const baseViewportWidthRef = useRef(0);
  /** Сетка сейчас выше видимой коробки (ST-08): курсор нужно держать в окне и без клавиатуры. */
  const peekOverflowRef = useRef(false);
  /**
   * ДВУМЕРНЫЙ VIEWPORT ПО X (ST-10, T-17, T-32). Сетка PTY бывает ШИРЕ коробки:
   * общий размер держит более широкий владелец, или принята сетка кадра шире
   * нашего экрана (adopt при полу 20×10). Раньше правую часть просто срезал
   * `overflow: hidden`, и курсор в колонке 200 при экране в 48 колонок был
   * невидим. Теперь окно сдвигается по X (`--pty-pan-x`, transform на
   * `.xterm-screen`) — PTY при этом не меняет размер (I-09/I-10): сдвиг чисто
   * локальный. Правила — geometry/viewportPan.ts; здесь только замер и CSS.
   * Попадание касаний и мыши считается по прямоугольнику `.xterm-screen` после
   * трансформации (TerminalCoordinates.cellAt, мышь xterm) — клики точные.
   */
  const panXRef = useRef(0);
  const panManualAtRef = useRef(0);
  const panInputAtRef = useRef(0);
  const panOverflowXRef = useRef(false);
  const setPanX = (box: HTMLElement, x: number) => {
    panXRef.current = x;
    const px = Math.round(x);
    box.style.setProperty("--pty-pan-x", `${px}px`);
    box.classList.toggle("pty-panned", px > 0);
  };
  // Размеры для жеста и колеса: сетка и коробка меняются только при resize и
  // смене шрифта, а замер на каждый touchmove после смены transform — лишний
  // пересчёт стилей на слабом WebView. Сбрасывается на resize терминала.
  const panLimitsRef = useRef<{ at: number; drawnW: number; innerW: number } | null>(null);
  const panLimits = (): { drawnW: number; innerW: number } | null => {
    const box = termRef.current;
    const screen = box?.querySelector<HTMLElement>(".xterm-screen");
    if (!box || !screen) return null;
    const now = performance.now();
    const cached = panLimitsRef.current;
    if (cached && now - cached.at < 250) return cached;
    const cs = window.getComputedStyle(box);
    const next = { at: now, drawnW: screen.getBoundingClientRect().width,
      innerW: box.clientWidth - (parseFloat(cs.paddingLeft) || 0) - (parseFloat(cs.paddingRight) || 0) };
    panLimitsRef.current = next;
    return next;
  };
  /** Ручной сдвиг окна на dx px (палец, колесо). false — сдвигать нечего. */
  const panByPixelsRef = useRef<(dx: number) => boolean>(() => false);
  panByPixelsRef.current = (dx: number): boolean => {
    const box = termRef.current;
    const lim = features.viewportPan ? panLimits() : null;
    if (!box || !lim || !(lim.drawnW > lim.innerW + 1)) return false;
    const next = panBy({ x: panXRef.current, y: 0 }, dx, 0, { drawnW: lim.drawnW, drawnH: 0, visibleW: lim.innerW, visibleH: 0 });
    panManualAtRef.current = Date.now();
    setPanX(box, next.x);
    return true;
  };
  /** Есть ли куда сдвигать по X (сетка шире коробки). */
  const panRoomRef = useRef<() => boolean>(() => false);
  panRoomRef.current = () => {
    const lim = features.viewportPan ? panLimits() : null;
    return !!lim && lim.drawnW > lim.innerW + 1;
  };
  // ST-08 × ST-06: память правила перекрытия (гистерезис occlusionStep),
  // пересчёт, отложенный до конца ?2026 приложения, и таймеры повторного
  // пересчёта — у каждого своё назначение (OcclusionRecheck): "sync" —
  // страховка 2026, "hold" — конец выдержки. Таймер, чья причина ушла
  // (транзакция закрылась обычным путём, курсор вернулся), снимается: раньше
  // один общий таймер через ~1 с звал пересчёт «от вывода» без вывода и при
  // смене сдвига гасил жест (скептик 15.09, повторная проверка).
  const occlusionStateRef = useRef<OcclusionState>(OCCLUSION_IDLE);
  const occlusionDeferredRef = useRef(false);
  type RecheckTimer = { timer: ReturnType<typeof setTimeout>; at: number };
  const occlusionRecheckRef = useRef<Record<OcclusionRecheck, RecheckTimer | null>>({ sync: null, hold: null });
  /** Снять таймер пересчёта этого назначения (без аргумента — оба). */
  const clearOcclusionRecheck = (purpose?: OcclusionRecheck): void => {
    for (const p of purpose ? [purpose] : (["sync", "hold"] as const)) {
      const pending = occlusionRecheckRef.current[p];
      if (pending) clearTimeout(pending.timer);
      occlusionRecheckRef.current[p] = null;
    }
  };
  /** Пересчитать сдвиг от вывода не позже чем через ms (ранний срок побеждает). */
  const scheduleOcclusionRecheck = (purpose: OcclusionRecheck, ms: number): void => {
    const wait = Math.max(0, ms);
    const at = performance.now() + wait;
    const pending = occlusionRecheckRef.current[purpose];
    if (pending && pending.at <= at) return;
    if (pending) clearTimeout(pending.timer);
    occlusionRecheckRef.current[purpose] = {
      at,
      timer: setTimeout(() => {
        occlusionRecheckRef.current[purpose] = null;
        if (disposedRef.current) return;
        // Причина ушла — пересчитывать нечего.
        if (purpose === "sync" ? !occlusionDeferredRef.current : occlusionStateRef.current.holdSince === null) return;
        // Человек листает историю, выделяет или ведёт жест: смена сдвига
        // погасила бы жест (cancelGesture ниже). Спросим снова позже.
        const b = terminalRef.current?.buffer.active;
        if (gestureActiveRef.current || selectModeRef.current || (!!b && b.viewportY < b.baseY)) {
          scheduleOcclusionRecheck(purpose, OCCLUSION_BUSY_RECHECK_MS);
          return;
        }
        applyKeyboardPeekRef.current("output");
      }, wait + 1),
    };
  };
  /**
   * reason "output" — пересчёт от вывода программы (курсор сдвинулся, закрылась
   * транзакция 2026, истекла выдержка). Под перекрытием такой пересчёт не ходит
   * за промежуточными положениями курсора (occlusionStep). Остальные вызовы —
   * геометрия, клавиатура, жест — применяются сразу, как раньше.
   */
  const applyKeyboardPeek = useCallback((reason?: "output") => {
    const box = termRef.current;
    if (!box) return;
    const screen = box.querySelector<HTMLElement>(".xterm-screen");
    const screenRect = screen ? screen.getBoundingClientRect() : null;
    const drawn = screenRect ? Math.round(screenRect.height) : 0;
    const cs = window.getComputedStyle(box);
    const inner = box.clientHeight
      - (parseFloat(cs.paddingTop) || 0)
      - (parseFloat(cs.paddingBottom) || 0);
    const live = terminalRef.current;
    const buffer = live?.buffer.active;
    const cursor = buffer ? buffer.baseY + buffer.cursorY - buffer.viewportY : -1;
    const previous = parseFloat(box.style.getPropertyValue("--pty-peek")) || 0;
    // ST-08 (T-31, I-09): к курсору сдвигаем не только под клавиатурой, но и
    // когда коробку перекрывает плашка или выросло поле ввода — сетка PTY
    // прежняя, видимое окно меньше, и срезать низ (там курсор) нельзя. Допуск
    // в пиксель: округление вёрстки сдвига не заслуживает. Без переключателя —
    // только клавиатура, как было.
    peekOverflowRef.current = drawn > inner + 1;
    // Под перекрытием (клавиатура закрыта) держим видимыми и строки ПОД
    // курсором — подвал TUI (occlusionStep). Клавиатура — прежнее правило курсора.
    const occluded = !keyboardOpenRef.current && features.occlusion && peekOverflowRef.current;
    let peek = 0;
    if (live && keyboardOpenRef.current) {
      peek = keyboardPeek(drawn, inner, live.rows, cursor, previous);
    } else if (live && occluded && reason === "output" && live.modes.synchronizedOutputMode) {
      // ST-06: приложение внутри ?2026h…?2026l — кадр не показан, курсор стоит
      // там, где его оставила середина перерисовки. Сдвиг — CSS, синхронный
      // вывод его не держит (скептик: 98 смен на 60 перерисовок), поэтому окно
      // ждёт конца транзакции: onWriteParsed после ?2026l, страховка — таймер.
      occlusionDeferredRef.current = true;
      scheduleOcclusionRecheck("sync", OCCLUSION_SYNC_RECHECK_MS);
      peek = previous;
    } else if (live && occluded) {
      const step = occlusionStep(occlusionStateRef.current, {
        drawn, visible: inner, rows: live.rows, cursorRow: cursor,
        lastTextRow: buffer ? lastInkRow(buffer, live.rows) : -1,
        previous, now: performance.now(), fromOutput: reason === "output",
        // Эхо нажатия человека (onData без автоответов, ряд клавиш, поле
        // ввода — все пишут panInputAtRef) выдержку не ждёт.
        humanInputAgoMs: panInputAtRef.current > 0 ? Date.now() - panInputAtRef.current : undefined,
      });
      occlusionStateRef.current = step.state;
      peek = step.peek;
      if (step.recheckMs !== null) scheduleOcclusionRecheck("hold", step.recheckMs);
      else clearOcclusionRecheck("hold");
    }
    // Вне перекрытия (клавиатура, сетка помещается) память правила
    // сбрасывается: следующее перекрытие решает заново.
    if (!occluded) {
      occlusionStateRef.current = OCCLUSION_IDLE;
      occlusionDeferredRef.current = false;
      clearOcclusionRecheck();
    }
    if (peek !== previous) cancelGestureRef.current();
    box.style.setProperty("--pty-peek", `${peek}px`);
    box.classList.toggle("pty-peeking", peek > 0);
    // ST-10: по X — то же правило минимального сдвига (panAxis). За курсором
    // окно следует, пока человек не сдвинул его сам; его ввод возвращает окно
    // курсору (panFollows). Сетка помещается — сдвига нет.
    if (features.viewportPan) {
      const drawnW = screenRect ? screenRect.width : 0;
      const innerW = box.clientWidth - (parseFloat(cs.paddingLeft) || 0) - (parseFloat(cs.paddingRight) || 0);
      panOverflowXRef.current = drawnW > innerW + 1;
      const cellW = live && live.cols > 0 ? drawnW / live.cols : 0;
      setPanX(box, panAxis(drawnW, innerW, cellW, buffer ? buffer.cursorX : -1, panXRef.current,
        panFollows(panInputAtRef.current, panManualAtRef.current)));
    }
    return { drawn, inner, peek };
  }, []);
  const applyKeyboardPeekRef = useRef(applyKeyboardPeek);
  applyKeyboardPeekRef.current = applyKeyboardPeek;
  const nativeKeyboardRef = useRef<boolean | null>(null);
  useEffect(() => observeNativeKeyboard(open => {
    nativeKeyboardRef.current = open;
    cancelGestureRef.current();
    updateKeyboardModeRef.current();
    applyKeyboardPeekRef.current();
  }), []);

  /**
   * Клавиатура сейчас на экране? Решение принимается В ОДНОМ месте и зовётся
   * ОБОИМИ путями — наблюдателем контейнера и обработчиком вьюпорта. Первая
   * попытка развести их не удалась: ResizeObserver успевает сработать раньше, и
   * пока флаг ставил только обработчик вьюпорта, наблюдатель уже звал fit() и
   * отправлял размер (замер: rows 25 → 3, один resize на открытие клавиатуры).
   *
   * Признак клавиатуры — высота вьюпорта упала на сотни px при НЕИЗМЕННОЙ
   * ширине. Поворот меняет и ширину; панель браузера даёт 50–60 px и порога не
   * проходит. Возвращает true, пока клавиатура поднята.
   */
  const updateKeyboardMode = useCallback((): boolean => {
    if (typeof window === "undefined") return false;
    const h = Math.round(window.visualViewport?.height ?? window.innerHeight);
    const w = Math.round(window.visualViewport?.width ?? window.innerWidth);
    if (baseViewportRef.current === 0) {
      // Страница открыта с УЖЕ поднятой клавиатурой: первая измеренная высота —
      // клавиатурная, и «базой без клавиатуры» ей быть нельзя, иначе весь режим
      // мёртв до первого закрытия. Боевой лог 13.08.2026: терминал открыт с
      // клавиатурой → 48x31 → 48x12 → 48x10 → 48x31 за 22 секунды, три полных
      // перерисовки TUI. На телефоне (coarse pointer) WebView занимает весь
      // экран, полная высота ≈ screen.height (боевая диагностика: вьюпорт 933
      // при экране 934) — если видимая высота меньше её на клавиатурный порог,
      // клавиатура уже на экране и базой становится высота экрана. На десктопе
      // окно меньше монитора — это норма, там база по-прежнему первая высота.
      // Цена ошибки (split-screen на планшете, где вьюпорт честно меньше
      // экрана): refit по вьюпорту не зовётся, но начальный fit при создании
      // терминала уже отмерил коробку, а смена ширины сбрасывает режим.
      //
      // ⚠ Стороны экрана берём ПО ОРИЕНТАЦИИ ОКНА (fullViewportHeight): iPad в
      // ландшафте отдаёт `screen` портретным, и прежний прямой `screen.height`
      // делал базу 1180 при вьюпорте 820 — клавиатура «поднята» навсегда,
      // размер терминала не уходит, PTY остаётся телефонным (10.09.2026).
      const coarse = typeof window.matchMedia === "function"
        && window.matchMedia("(pointer: coarse)").matches;
      const full = fullViewportHeight(
        w, h,
        Math.round(window.screen?.width ?? 0), Math.round(window.screen?.height ?? 0),
        coarse);
      baseViewportRef.current = h < full - KEYBOARD_MIN_PX ? full : h;
      baseViewportWidthRef.current = w;
    }
    const was = keyboardOpenRef.current;
    let kbdSource = "viewport";
    if (nativeKeyboardRef.current !== null) {
      kbdSource = "native";
      keyboardOpenRef.current = nativeKeyboardOpen(
        nativeKeyboardRef.current, h, w, baseViewportRef.current, baseViewportWidthRef.current, KEYBOARD_MIN_PX);
      if (!keyboardOpenRef.current && h > baseViewportRef.current) baseViewportRef.current = h;
      baseViewportWidthRef.current = w;
    } else if (Math.abs(w - baseViewportWidthRef.current) >= 2) {
      // Ширина поехала — это поворот или смена окна, а не клавиатура.
      kbdSource = "width";
      keyboardOpenRef.current = false;
      baseViewportRef.current = h;
      baseViewportWidthRef.current = w;
    } else if (h < baseViewportRef.current - KEYBOARD_MIN_PX) {
      keyboardOpenRef.current = true;
    } else {
      keyboardOpenRef.current = false;
      if (h > baseViewportRef.current) baseViewportRef.current = h;
    }
    if (was !== keyboardOpenRef.current) {
      trace("kbd", { open: keyboardOpenRef.current, h, w, base: baseViewportRef.current }, kbdSource);
    }
    // Вышли из режима — сдвиг обязан сняться, иначе низ останется приподнятым.
    if (was && !keyboardOpenRef.current) applyKeyboardPeekRef.current();
    // Ряды под терминалом при клавиатуре уступают место выводу (см. styles.css
    // `.pty-page.pty-kbd`). Класс ставится ЗДЕСЬ, СИНХРОННО, а не состоянием
    // React: попытка 2.61.0 прятать ряды через `setKeyboardShown` уронила
    // четыре пробы геометрии — ререндер асинхронный, и при закрытии
    // клавиатуры наблюдатель коробки видел вьюпорт уже большим, а ряды ещё
    // скрытыми, считал «честный» размер и слал его агенту, следом ряды
    // возвращались и размер уезжал второй раз. Синхронный класс меняет
    // раскладку в той же вёрстке, которую наблюдатель тут же и меряет:
    // после клавиатуры коробка равна докладиатурной, resize не уходит.
    const page = termRef.current?.closest(".pty-page");
    if (page) page.classList.toggle("pty-kbd", keyboardOpenRef.current);
    return keyboardOpenRef.current;
  }, []);
  const updateKeyboardModeRef = useRef(updateKeyboardMode);
  updateKeyboardModeRef.current = updateKeyboardMode;

  /**
   * ЛОГИЧЕСКАЯ высота терминала — та, в которой рисует агент и живёт PTY.
   *
   * ⚠ Это НЕ то, что померил `fit()` при поднятой клавиатуре. Приходит с
   * компьютера: в состоянии сессии (`/state` отдаёт применённый к PTY размер) и
   * в каждом кадре экрана (`screen_rows`). Ноль — ещё не знаем; тогда остаёмся
   * на измеренном (см. ptyTerm/geometry.ts).
   */
  const logicalRowsRef = useRef(0);
  /**
   * ЛОГИЧЕСКАЯ ширина — авторитетная ширина PTY с компьютера (`state.cols`).
   *
   * ⚠ ЗАЧЕМ ОНА, ЕСЛИ ШИРИНУ МОЖНО ПОМЕРИТЬ. PTY общий для всех зрителей и
   * идёт по самому УЗКОМУ из них: телефон 48 колонок + окно на ПК 232 → PTY
   * 48. Если широкий зритель оставит свою измеренную ширину логической, он
   * никогда не примет кадр (кадры приходят в 48) — «отверг → попроси свежий»
   * крутился вечно, лишь троттлясь (внешний аудит 2.57.18, находка P0-05).
   * Поэтому известная авторитетная ширина зажимает нашу логическую сетку
   * сверху, а серверу мы по-прежнему сообщаем СВОЮ измеренную (reportSizeRef)
   * — иначе при уходе узкого зрителя PTY некуда было бы расти.
   *
   * Ноль — ещё не знаем; тогда логическая ширина равна измеренной.
   */
  const logicalColsRef = useRef(0);
  const authoritativeRowsRef = useRef(0);
  // ST-08 (ревью скептика): когда авторитетную сетку в последний раз принял
  // КАДР (null — не принимал или уже сверена с /state) и когда ушёл запрос
  // последнего принятого /state. Часы — performance.now(). Правило сверки —
  // geometry.reconcileAdoptedGrid, исполнение — эффект после state.cols.
  const gridAdoptedAtRef = useRef<number | null>(null);
  const stateAskedAtRef = useRef(0);
  // Token of the ACTUAL local xterm cell grid. A screen request captures this
  // value, and the agent echoes it. If fit()/resize changes the grid while the
  // frame is in flight, that old frame must not resize us back or overwrite the
  // new screen (same-viewer 80x24 -> 48x30 race, 14.08.2026).
  const geometryRevisionRef = useRef(0);
  // Reading-page coordinates depend on actual row layout, not a viewer-size
  // report which the shared-grid host can decline without changing xterm.
  const readGeometryRevisionRef = useRef(0);
  const geometrySizeRef = useRef({ cols: 0, rows: 0 });
  // At most one retry per unchanged local geometry. This still asks for a fresh
  // frame after a race, but an old agent which cannot echo geom_rev cannot make
  // the client request snapshots forever; its raw stream remains usable.
  const geometryRetryRevisionRef = useRef<number | null>(null);
  /**
   * Размер, который мы СООБЩАЕМ компьютеру: измеренный честно, без зажатия
   * под авторитетную ширину. Именно по сообщённым размерам зрителей сервер
   * считает общий минимум — и растит PTY, когда узкий зритель ушёл.
   */
  const reportSizeRef = useRef({ cols: 0, rows: 0 });
  const noteReportSize = (size: { cols: number; rows: number }) => {
    const previous = reportSizeRef.current;
    if (previous.cols > 0 && (previous.cols !== size.cols || previous.rows !== size.rows)) {
      // A frame requested before this viewer-size intent is obsolete even
      // while the parser waits for the host to confirm its new grid.
      geometryRevisionRef.current++;
      geometryRetryRevisionRef.current = null;
      cancelGestureRef.current();
      // Сообщённая вместимость, не сетка xterm: поэтому не cols/rows (их
      // asciicast читает как размер терминала).
      trace("geom", { reportCols: size.cols, reportRows: size.rows }, "report");
      // Запрос в пути снят в прежней ревизии: координатор узнаёт о новой сразу.
      recoveryApiRef.current.syncGeometry();
    }
    reportSizeRef.current = size;
  };
  /**
   * Строки отчёта измерены БЕЗ клавиатуры (ST-08, I-10). Только такие строки —
   * вместимость; под клавиатурой высоту мерить нечем, и выдавать за неё
   * логическую высоту кадра или /state нельзя (resizePolicy.capacityForServer).
   */
  const measuredWithoutKeyboardRef = useRef(false);
  /** Высота панели ввода при ОДНОЙ строке поля — база роста поля (H4). */
  const oneRowInputBarRef = useRef(0);
  /**
   * Сколько пикселей коробки сейчас занимают временные перекрытия (ST-08,
   * T-31): плашки из OCCLUDER_SELECTORS и рост поля ввода сверх одной строки.
   * Только метрики вёрстки, без текста (I-15).
   */
  const measureOccluderPx = (): number => {
    const page = termRef.current?.closest(".pty-page");
    if (!page) return 0;
    let px = 0;
    for (const selector of OCCLUDER_SELECTORS) {
      for (const el of page.querySelectorAll<HTMLElement>(selector)) px += el.getBoundingClientRect().height;
    }
    const bar = page.querySelector<HTMLElement>(".pty-input-bar");
    const field = bar?.querySelector<HTMLTextAreaElement>(".pty-text-input");
    if (bar && field) {
      const barPx = bar.getBoundingClientRect().height;
      if (field.rows <= 1 && barPx > 0) oneRowInputBarRef.current = barPx;
      px += inputGrowthPx(field.rows, barPx, oneRowInputBarRef.current);
    }
    return px;
  };
  const measureOccluderPxRef = useRef(measureOccluderPx);
  measureOccluderPxRef.current = measureOccluderPx;
  /**
   * Вместимость по высоте с учётом перекрытий: сколько строк насчитал бы fit,
   * не будь плашек. Та же арифметика, что у FitAddon (высота родителя минус
   * отступы .xterm, деление на высоту ячейки), только с пикселями плашек —
   * иначе появление и уход плашки давали бы разницу в строку и resize.
   */
  const occludedCapacityRows = (measuredRows: number): number => {
    const occluderPx = measureOccluderPx();
    if (occluderPx <= 0.5) return measuredRows;
    const term = terminalRef.current;
    const box = termRef.current;
    const screen = box?.querySelector<HTMLElement>(".xterm-screen");
    if (!term || !box || !screen || term.rows <= 0) return measuredRows;
    const cellPx = screen.getBoundingClientRect().height / term.rows;
    const own = term.element ? window.getComputedStyle(term.element) : null;
    const padPx = own ? (parseInt(own.paddingTop, 10) || 0) + (parseInt(own.paddingBottom, 10) || 0) : 0;
    const boxPx = (parseInt(window.getComputedStyle(box).height, 10) || 0) - padPx;
    return capacityWithOccluder(measuredRows, occluderPx, cellPx, boxPx);
  };
  /**
   * Привести локальную геометрию в порядок: ширину считаем всегда, а высоту при
   * поднятой клавиатуре НЕ ужимаем до видимой — оставляем логическую и
   * показываем низ сдвигом (applyKeyboardPeek).
   *
   * ⚠ До 2.57.14 здесь стоял простой `fit()`, и он ужимал терминал до
   * клавиатурных 9–11 строк. Отправку такого размера мы благоразумно
   * блокировали — но локальный xterm уже был ужат, PTY оставался
   * тридцатистрочным, и кадр в 30 строк складывался в последнюю видимую
   * (боевые снимки: `снапшот=48x31 клиент=48x11`).
   */
  const fitLocal = useCallback((): { cols: number; rows: number } | null => {
    const term = terminalRef.current;
    const fit = fitRef.current;
    if (!term || !fit) return null;
    const keyboard = updateKeyboardModeRef.current();
    if (!keyboard) {
      let measured: { cols: number; rows: number } | undefined;
      try { measured = fit.proposeDimensions(); } catch { /* ignore */ }
      if (!measured) return null;
      // ST-08 (T-31): вместимость — без временных перекрытий. fit мерит
      // коробку, уже сжатую плашкой; строки под ней возвращаем так, как
      // посчитал бы fit без неё. Выключено — прежний замер как есть.
      const capacity = features.occlusion
        ? { cols: measured.cols, rows: occludedCapacityRows(measured.rows) }
        : { ...measured };
      measuredWithoutKeyboardRef.current = true;
      noteReportSize(capacity);
      const auth = logicalColsRef.current;
      const authRows = authoritativeRowsRef.current;
      // Capacity is reported independently. Until the host applies it, the
      // parser must keep the authoritative grid even if controls clip it.
      const cols = auth >= 2 ? auth : capacity.cols;
      const rows = authRows >= 2 ? authRows : capacity.rows;
      if (cols !== term.cols || rows !== term.rows) try { term.resize(cols, rows); } catch { /* ignore */ }
      // Сетка могла стать выше коробки (плашка, авторитетная высота): курсор в окне.
      if (features.occlusion) applyKeyboardPeekRef.current();
      return { cols: term.cols, rows: term.rows };
    }
    // Клавиатура на экране: считаем предложенный размер, но высоту берём
    // логическую — одним resize, без промежуточного ужатия и лишней
    // перерисовки.
    let proposed: { cols?: number; rows?: number } | undefined;
    try { proposed = fit.proposeDimensions?.(); } catch { /* ignore */ }
    const measuredCols = proposed?.cols && proposed.cols >= 2 ? proposed.cols : term.cols;
    const measured = proposed?.rows && proposed.rows >= 2 ? proposed.rows : term.rows;
    const rows = logicalRowsForKeyboard(measured, logicalRowsRef.current);
    const auth = logicalColsRef.current;
    const cols = auth >= 2 ? auth : measuredCols;
    if (cols !== term.cols || rows !== term.rows) {
      try { term.resize(cols, rows); } catch { /* ignore */ }
    }
    if (features.capacity) {
      // ST-08, I-10 (дыра (а) карты): строки, измеренные без клавиатуры,
      // сохраняются; иначе вместимость по высоте НЕИЗВЕСТНА (0) — логическую
      // высоту кадра или /state за неё не выдаём: принятая сетка вернулась бы
      // серверу как наша вместимость, и общий PTY перестал бы расти.
      const keep = measuredWithoutKeyboardRef.current && reportSizeRef.current.rows >= 2;
      measuredWithoutKeyboardRef.current = keep;
      noteReportSize({ cols: measuredCols, rows: keep ? reportSizeRef.current.rows : 0 });
    } else {
      noteReportSize({ cols: measuredCols, rows: reportSizeRef.current.rows >= 2 ? reportSizeRef.current.rows : rows });
    }
    applyKeyboardPeekRef.current();
    return { cols: term.cols, rows: term.rows };
  }, []);
  const fitLocalRef = useRef(fitLocal);
  fitLocalRef.current = fitLocal;

  const sentSizeRef = useRef({ cols: 0, rows: 0 });
  const resizeQuietTimer = useRef<ReturnType<typeof setTimeout> | null>(null);
  // Размер ДЛЯ СЕРВЕРА — измеренный честно (reportSizeRef), а не зажатая под
  // авторитетную ширину логическая сетка: сервер считает общий PTY по
  // сообщённым размерам зрителей, и зажатая ширина лишила бы его куда расти
  // при уходе узкого зрителя (внешний аудит 2.57.18, P0-05). До первого
  // fitLocal измерения нет — тогда терминальный размер, как раньше.
  // ST-08: сокет, в который ушёл sentSizeRef. Новый сокет на сервере — новый
  // Viewer без размера, и прежнее «уже отправлено» ему ничего не говорит
  // (resizePolicy.sentBaseline).
  const sentOnSocketRef = useRef<WebSocket | null>(null);
  // ST-08 (T-32): рендер WebGL ещё загружается — ячейка, а с ней вместимость,
  // сменится при его подключении. Первый размер нового сокета ждёт этого
  // момента (не дольше RENDERER_WAIT_MS), иначе новый зритель давал ДВА PTY
  // resize на join: замер — 123x15 → 45x15 (DOM-рендер) → 47x15 (WebGL).
  const rendererPendingRef = useRef(false);
  const openResizeWaitRef = useRef<WebSocket | null>(null);
  const RENDERER_WAIT_MS = 1500;
  // ST-08 (T-32): последний запрос кадра — сокет, ревизия, номер и была ли к
  // моменту отправки наша вместимость доставлена этому сокету.
  const lastScreenRequestRef = useRef<ScreenRequestNote | null>(null);
  const sizeForServer = (): { cols: number; rows: number } | null => {
    const term = terminalRef.current;
    if (!term) return null;
    const rep = reportSizeRef.current;
    if (features.capacity) {
      // I-10: неизвестная вместимость — ничего, а не сетка терминала.
      return capacityForServer({
        report: rep, measuredWithoutKeyboard: measuredWithoutKeyboardRef.current,
        termCols: term.cols, termRows: term.rows,
      });
    }
    if (rep.cols >= 2 && rep.rows >= 2) return rep;
    return { cols: term.cols, rows: term.rows };
  };
  /** Запрос кадра ушёл: запомнить, доставлена ли к этому моменту вместимость (ST-08). */
  const noteScreenRequest = (socket: WebSocket, geomRev: number, req: number | null) => {
    if (!features.capacity) return;
    const sentOn = sentOnSocketRef.current;
    lastScreenRequestRef.current = {
      socket, geomRev, req,
      capacity: { ...reportSizeRef.current },
      delivered: capacityDelivered({
        sent: sentOn === socket ? sentSizeRef.current : null,
        report: reportSizeRef.current,
        measuredWithoutKeyboard: measuredWithoutKeyboardRef.current,
        resizePending: resizeQuietTimer.current !== null,
        sentOnSocket: sentOn,
        socket,
      }),
    };
  };
  const flushResize = useCallback(() => {
    resizeQuietTimer.current = null;
    // A resize queued while the keyboard was still closed may fire after it
    // opens. reportSizeRef then still contains the short pre-keyboard layout;
    // sending it would shrink the shared PTY even though the live xterm has
    // deliberately kept its logical rows. The force path already has this
    // guard; delayed delivery needs the same invariant.
    const size = sizeForServer();
    if (!size) return;
    const { cols, rows } = size;
    if (typeof document !== "undefined" && document.visibilityState === "hidden") {
      trace("resize-send", { cols, rows }, "skip-hidden");
      return;
    }
    // ST-08 (T-32, ревью): сокет ждёт рендер для ПЕРВОГО размера — отложенный
    // размер вёрстки молчит, иначе он уходит в ячейке DOM-рендера до WebGL.
    // Замер (dev-стенд, чанк WebGL задержан на 800 мс): сокет 946 мс, resize
    // 45x23 через отстой 1283 мс, WebGL 1735 мс, затем 47x23 — два PTY resize
    // на join. Первый размер гарантированно уйдёт: подключение рендера
    // (rendererSettled) или таймаут RENDERER_WAIT_MS шлют текущий силой.
    if (features.capacity && openResizeWaitRef.current && openResizeWaitRef.current === wsRef.current) {
      trace("resize-send", { cols, rows }, "skip-renderer");
      return;
    }
    const sent = features.capacity
      ? sentBaseline(sentSizeRef.current, sentOnSocketRef.current, wsRef.current)
      : sentSizeRef.current;
    if (!shouldFlushResize(sent, size, updateKeyboardModeRef.current())) {
      trace("resize-send", { cols, rows }, "skip-policy");
      return;
    }
    const ws = wsRef.current;
    if (ws?.readyState !== WebSocket.OPEN) {
      trace("resize-send", { cols, rows }, "skip-closed");
      return;
    }
    sentSizeRef.current = { cols, rows };
    sentOnSocketRef.current = ws;
    ws.send(JSON.stringify({ t: "resize", cols, rows }));
    trace("resize-send", { cols, rows }, "sent");
  }, []);
  const sendResize = useCallback((force = false) => {
    const term = terminalRef.current;
    if (!term) return;
    if (force) {
      // Новый сокет: размер нужен серверу сразу, дебаунс отменяем.
      // НО не с клавиатурной высотой: терминал, открытый с уже поднятой
      // клавиатурой, отсылал force'ом 48x12 — агент перерисовывал TUI при
      // открытии и ещё раз при закрытии клавиатуры, а история зеркала
      // зарастала копиями экрана. Без resize сервер оставит сессии прежний
      // размер (у агентских его и так держит targetSizeLocked); когда
      // клавиатура уйдёт, refit пришлёт честный размер обычным путём.
      if (features.capacity) {
        // ST-08 (шаг 2): молчим, только если вместимость НЕИЗВЕСТНА (не
        // измерена без клавиатуры). Измеренная без неё годится и под
        // клавиатурой: иначе новый сокет, открытый во время набора, оставался
        // на сервере зрителем без размера, а после закрытия клавиатуры
        // flushResize видел «уже отправлено» прежнему сокету и молчал.
        if (!sizeForServer()) {
          trace("resize-send", null, updateKeyboardModeRef.current() ? "force-skip-keyboard" : "force-skip-unknown");
          return;
        }
      } else if (updateKeyboardModeRef.current()) { trace("resize-send", null, "force-skip-keyboard"); return; }
      if (resizeQuietTimer.current) { clearTimeout(resizeQuietTimer.current); resizeQuietTimer.current = null; }
      const size = sizeForServer();
      if (!size) return;
      const { cols, rows } = size;
      if (cols < 2 || rows < 2) { trace("resize-send", { cols, rows }, "force-skip-tiny"); return; }
      const ws = wsRef.current;
      if (ws?.readyState !== WebSocket.OPEN) { trace("resize-send", { cols, rows }, "force-skip-closed"); return; }
      sentSizeRef.current = { cols, rows };
      sentOnSocketRef.current = ws;
      ws.send(JSON.stringify({ t: "resize", cols, rows }));
      trace("resize-send", { cols, rows }, "force-sent");
      return;
    }
    if (resizeQuietTimer.current) clearTimeout(resizeQuietTimer.current);
    // Мелкая качель по высоте (панель браузера ездит сама) отстаивается дольше:
    // за это время высота обычно возвращается к отправленной, и flushResize
    // молча ничего не шлёт — агент не перерисовывает экран впустую.
    const size = sizeForServer() ?? { cols: 0, rows: 0 };
    const { cols, rows } = size;
    const sentBefore = features.capacity
      ? sentBaseline(sentSizeRef.current, sentOnSocketRef.current, wsRef.current)
      : sentSizeRef.current;
    const wait = cols >= 2 && rows >= 2 ? resizeDelayMs(sentBefore, { cols, rows }) : RESIZE_QUIET_MS;
    resizeQuietTimer.current = setTimeout(flushResize, wait);
  }, [flushResize]);

  // Склейка вывода по кадру анимации. TUI-агенты (Kimi со спиннером, Codex на
  // переигровке буфера) шлют вывод частыми мелкими WS-кадрами, и каждый кадр
  // гонял свой проход парсера/рендера xterm — на WebView телефона это видно как
  // мерцание (жалоба 2026-07-29). Копим кадры до ближайшего rAF и отдаём в
  // терминал одним write: за кадр экрана — один проход отрисовки.
  // Порядок с управляющими сообщениями сохраняем: reset/exit сначала принудительно
  // спускают очередь (эти байты пришли РАНЬШЕ маркера), потом применяются сами.
  const writeQueueRef = useRef<BufferedTermWrite[]>([]);
  const writeRafRef = useRef<number | null>(null);
  // Хвост, который может оказаться началом «ESC[3J» (см. keepHistory.ts): она
  // приходит разорванной между кадрами, и без переноса стирание истории
  // проскочило бы в терминал.
  const parserCarryRef = useRef<GuardedParserCarry>(null!);
  if (!parserCarryRef.current) parserCarryRef.current = new GuardedParserCarry();
  invalidatePendingScrollbackEraseRef.current = () => {
    scrollbackEraseGateRef.current.invalidate();
    parserCarryRef.current.clearAllErases();
  };
  // Отложенное «стереть историю»: агент попросил, пока человек читал прошлый
  // вывод. Досылаем сами при возвращении к низу — тогда прокрутка снова
  // показывает ОДНУ переписку, а не десяток её копий.
  // Активный жест прокрутки: палец на экране или инерция после отпускания
  // (ведётся в touch-эффекте ниже). Пока жест идёт, стирание истории из потока
  // не исполняем, даже если вьюпорт у низа: человек уже трогает прокрутку, и
  // исполненный в этот момент ESC[3J бросит его наверх (жалоба 07.08).
  const gestureActiveRef = useRef(false);
  // Счётчик прокруток от человека: всё (кнопки ⇈↑↓⇊, палец, инерция) идёт через
  // scrollByLines, который его наращивает. Якорь чтения (см. flushTermWrites) по
  // нему отличает сдвиг вьюпорта от потока от сдвига пальцем.
  const userScrollSeqRef = useRef(0);
  // Планировщик спуска очереди. Кадр анимации — правильный выбор, когда экран
  // видно; в СКРЫТОЙ вкладке кадров не бывает вовсе, и очередь копилась бы до
  // возвращения человека. Тогда планируем таймером: терминал остаётся в
  // актуальном состоянии, а история оседает в scrollback самого xterm — то
  // есть вернувшись, человек может прокрутить назад, а не начинает с пустого.
  const writeTimerRef = useRef<ReturnType<typeof setTimeout> | null>(null);
  const BACKGROUND_FLUSH_MS = 250;
  // Потолок очереди склейки. Сокет принимает и в фоне, а отрисовка планируется
  // кадром анимации — час фонового агента (замер 2.46.4: ~20 КБ/с) это ~72 МБ в
  // массиве. На возврате они склеивались в одну строку, проходились побайтным
  // разбором в stripScrollbackErase и уходили в xterm, который свыше 50 МБ
  // запись просто ОТБРАСЫВАЕТ — кусок вывода исчезал молча, без пометки, а
  // экран замерзал на секунды. Держим последние 2 МБ (несколько экранов TUI с
  // запасом) и о выброшенной голове говорим той же строкой, что и серверный
  // gap: потерять начало давнего фонового вывода не страшно, потерять молча —
  // страшно.
  const QUEUE_CAP_BYTES = 2 << 20;
  const queueBytesRef = useRef(0);
  // Прерывание незакрытой escape-последовательности после выброшенных байт:
  // CAN (0x18) отменяет начатую CSI/DCS, ST (`ESC \`) закрывает строковую
  // OSC/DCS/APC. Ни экран, ни историю не трогает — этим и отличается от RIS.
  const PARSER_ABORT = "\x18\x1b\\";
  // ── Flow control, сторона отправителя (клиентская половина контракта;
  // серверную пишет другой агент). Считаем байты, отданные в xterm, но ещё не
  // разобранные его внутренней очередью (write-callback не сработал): xterm
  // разбирает запись порциями ~12 мс, и когда поток быстрее разбора (спиннер
  // Kimi, переигровка буфера Codex), очередь растёт, а main thread тонет в
  // разборе — снаружи это «терминал фризится». По мотивам xterm flow control
  // guide + ttyd: перевалило HIGH — просим сервер приостановить поток
  // ({t:"pause"}), спустилось ниже LOW — отпускаем ({t:"resume"}). Шлём
  // только на ПЕРЕХОДЕ порога (флаг flowPausedRef): иначе каждый кадр вывода
  // таскал бы за собой служебное сообщение. JSON text-кадром внутри уже
  // открытого стрима — тем же каналом, что {t:"screen"}: релея на этом уровне
  // нет, а старый агент неизвестное сообщение молча игнорирует.
  const FLOW_HIGH_BYTES = 256 * 1024;
  const FLOW_LOW_BYTES = 64 * 1024;
  // ST-09: при паузе по всей очереди аварийный потолок склейки — выше худшего
  // законного залпа агента (досылка + запас подписчика поверх очереди у порога
  // паузы, flowQueueCap). Прежние 2 МиБ равнялись одной досылке: живая пачка
  // следом выбрасывала голову и стирала позицию resume. Откат flowBacklog=false
  // — прежние QUEUE_CAP_BYTES.
  const FLOW_QUEUE_CAP_BYTES = flowQueueCap(FLOW_HIGH_BYTES);
  const flowPausedRef = useRef(false);
  const writeUnackedRef = useRef(0);
  // ST-09 (features.flowBacklog): счётчики стадий очередей и их diag flow —
  // только числа (I-15). Правила — ptyTerm/runtime/FlowController.ts.
  const flowStatsRef = useRef<FlowStatsAccumulator | null>(null);
  if (!flowStatsRef.current) flowStatsRef.current = new FlowStatsAccumulator();
  const flowDiagRef = useRef({ lastSentAt: 0, lastKey: "", pending: false, state: "", reason: "" });
  /**
   * diag flow в журнал агента: переход паузы или выброс уходит сразу, но не
   * чаще FLOW_DIAG_MIN_MS; без событий — раз в FLOW_DIAG_PERIOD_MS на видимой
   * странице (тик idle-сторожа); одинаковые счётчики не повторяются. Закрытый
   * сокет не теряет событие: pending дождётся следующего соединения.
   */
  const reportFlow = (state: "pause" | "resume" | "drop" | "periodic", reason = "") => {
    if (!features.flowBacklog) return;
    const d = flowDiagRef.current;
    if (state !== "periodic") { d.pending = true; d.state = state; d.reason = reason; }
    const ws = wsRef.current;
    if (ws?.readyState !== WebSocket.OPEN) return;
    const now = performance.now();
    const stats = flowStatsRef.current!.snapshot(now);
    const key = flowStatsKey(stats);
    if (!flowDiagDue({ now, lastSentAt: d.lastSentAt, changed: key !== d.lastKey, pending: d.pending, visible: !document.hidden })) return;
    const fields = flowDiagFields(stats, d.pending ? d.state : "periodic", d.pending ? d.reason : "");
    sendDiag("flow", fields.wire, { sock: ws, traceFields: fields.trace });
    d.lastSentAt = now; d.lastKey = key; d.pending = false;
  };
  const flowCtl = () => {
    const visible = !document.hidden;
    const unacked = writeUnackedRef.current;
    let what: "pause" | "resume" | null;
    let why = "";
    let queued = 0, hidden = false, backlog = false;
    if (features.flowBacklog) {
      // Пауза по ВСЕЙ очереди клиента: склейка (принято, ещё не отдано в
      // writer) плюс неразобранное в xterm. Склейка стоит, пока барьер
      // снапшота держит очередь или кадр анимации не приходит, — раньше она
      // молча дорастала до аварийных 2 МиБ: выброс головы, invalidateResume и
      // полный reset, хотя сервер мог встать на паузу и дослать из кольца.
      queued = queueBytesRef.current;
      const r = flowReasons(flowPausedRef.current, { visible, queuedBytes: queued, unackedBytes: unacked }, FLOW_HIGH_BYTES, FLOW_LOW_BYTES);
      const stats = flowStatsRef.current!;
      stats.noteQueues(queued, unacked);
      stats.noteReasons(r, performance.now());
      what = r.next === flowPausedRef.current ? null : r.next ? "pause" : "resume";
      hidden = r.hidden; backlog = r.backlog;
      why = hidden && backlog ? "hidden+backlog" : hidden ? "hidden" : backlog ? "backlog" : "clear";
    } else {
      what = flowTransition(flowPausedRef.current, unacked, visible, FLOW_HIGH_BYTES, FLOW_LOW_BYTES);
    }
    if (!what) return;
    // Флаг ставим ДО отправки: переход состоялся, даже если сокет уже мёртв и
    // кадр не ушёл (сервер на новом соединении стартует неприостановленным).
    flowPausedRef.current = what === "pause";
    trace("flow", features.flowBacklog ? { unacked, queued, visible, hidden, backlog } : { unacked, visible }, what);
    const ws = wsRef.current;
    if (ws?.readyState === WebSocket.OPEN) {
      try { ws.send(JSON.stringify({ t: what })); } catch { /* сокет умер на отправке */ }
    }
    reportFlow(what, why);
  };
  // ST-09 (T-33): приговор сторожа тишины — один для тика и для разовой
  // перепроверки по истечении льготы. Правило — silenceVerdict.
  const closeIfSilent = (ws: WebSocket, now: number, recheck: boolean): boolean => {
    const limit = hbLimitRef.current;
    if (!silenceVerdict(now, lastMsgRef.current, limit, visibleSinceRef.current)) return false;
    trace("conn", { limitMs: limit, recheck: recheck ? 1 : 0 }, "idle-timeout");
    try { ws.close(); } catch { /* ignore */ }
    return true;
  };
  /**
   * Разовая перепроверка тишины ровно по истечении льготы (ST-09, T-33). Льгота
   * после возврата или разморозки только откладывает приговор; без таймера
   * следующая проверка ждала тика сторожа (IDLE_TICK_MS), и мёртвый после фона
   * сокет закрывался через 6,6–6,9 с вместо ~3 с. Живой таймер один; снимается
   * закрытием сокета и размонтированием.
   */
  const armSilenceRecheck = () => {
    if (!features.flowBacklog) return;
    if (silenceRecheckTimer.current) { clearTimeout(silenceRecheckTimer.current); silenceRecheckTimer.current = null; }
    const ws = wsRef.current;
    if (!ws || ws.readyState !== WebSocket.OPEN) return;
    const delay = silenceRecheckDelay(Date.now(), visibleSinceRef.current, hbLimitRef.current);
    if (delay == null) return;
    silenceRecheckTimer.current = setTimeout(() => {
      silenceRecheckTimer.current = null;
      if (disposedRef.current || wsRef.current !== ws || ws.readyState !== WebSocket.OPEN) return;
      closeIfSilent(ws, Date.now(), true);
    }, delay);
  };
  // term.write с учётом неподтверждённых байт. Только массивные пути
  // (flushTermWrites, кадр экрана): мелкие служебные записи (RIS, modes,
  // gap-строки, SCROLLBACK_ERASE, EMPTY_BYTES-барьер) не трекаем — они порог
  // не двигают.
  /**
   * ST-09: запись с отметкой «отдано xterm» (handedMarkRef). handed: число —
   * после отдачи этой записи в xterm экран описывается этой позицией потока;
   * null — в xterm уходит кадр экрана, позиция потока экран больше не
   * описывает; undefined — служебная запись, отметку не трогает. Отметка
   * ставится, когда writer ОТДАЁТ запись xterm (lazy), а не при постановке в
   * очередь: поставленное, но не отданное connect() выбросит.
   * flowBacklog=false — прежний write без отметки.
   */
  const writeMarked = (data: Uint8Array | string, options: TerminalWriteOptions, handed: number | null | undefined) => {
    const writer = terminalWriterRef.current!;
    if (!features.flowBacklog || handed === undefined) { writer.write(data, options); return; }
    const guard = options.guard ?? writer.currentGuard();
    writer.lazy(() => {
      handedMarkRef.current = handed === null ? null : noteHanded(handedMarkRef.current, guard, handed);
      return data;
    }, options);
  };
  const writeTracked = (
    data: Uint8Array | string,
    cb: (() => void) | undefined,
    guard: TerminalWriteGuard,
    handed?: number | null,
  ) => {
    // У строки нет byteLength; length занижает кириллицу ~вдвое (UTF-16 против
    // UTF-8) — для порога безразлично, а TextEncoder здесь был бы лишней
    // аллокацией на горячем пути.
    const n = typeof data === "string" ? data.length : data.byteLength;
    writeUnackedRef.current += n;
    flowCtl();
    writeMarked(data, {
      guard,
      after: cb,
      settled: (result) => {
        writeUnackedRef.current = Math.max(0, writeUnackedRef.current - n);
        flowCtl();
        // Исход записи, кроме штатного: устаревшая эпоха/поколение или сбой.
        if (result !== "written") trace("writer", { bytes: n, tracked: true }, result);
      },
    }, handed);
  };
  const writeSerial = (
    data: Uint8Array | string,
    cb: (() => void) | undefined,
    guard: TerminalWriteGuard,
    handed?: number | null,
  ) => writeMarked(data, {
    guard,
    after: cb,
    settled: (result) => {
      if (result !== "written") trace("writer", { bytes: data.length, tracked: false }, result);
    },
  }, handed);
  const cancelFlushSchedule = () => {
    if (writeRafRef.current != null) { cancelAnimationFrame(writeRafRef.current); writeRafRef.current = null; }
    if (writeTimerRef.current != null) { clearTimeout(writeTimerRef.current); writeTimerRef.current = null; }
  };
  // Барьер применения снапшота (внешний аудит 13.08.2026, находка T-002).
  //
  // flushTermWrites НЕ был барьером, хотя все вызывающие считали его таковым.
  // term.write только КЛАДЁТ байты в очередь xterm, а цепочка writeEraseChain
  // ставит следующий сегмент лишь из колбэка предыдущего — то есть к моменту
  // возврата из flushTermWrites в очереди лежит РОВНО ОДИН сегмент, а остальные
  // (и стирания истории между ними) допишутся потом, в хвост. Обработчик кадра
  // тут же синхронно писал RIS → историю → кадр, и старый вывод исполнялся
  // ПОВЕРХ них: `ESC[3J` стирал только что восстановленный scrollback, хвост
  // старого потока дорисовывался поверх абсолютно адресованного кадра. Отсюда
  // «дубли после открытия» и «прокрутка пропала сразу после появления».
  //
  // Барьер закрывает и обратную сторону: пока снапшот применяется, свежие байты
  // ждут в очереди. Иначе они легли бы ПОД кадр и погибли под его же стиранием.
  const snapshotBarrierRef = useRef(false);
  const snapshotBarrierTimerRef = useRef<ReturnType<typeof setTimeout> | null>(null);
  // ПОКОЛЕНИЕ СОЕДИНЕНИЯ и НОМЕР ТРАНЗАКЦИИ СНАПШОТА.
  //
  // ⚠ Барьер, его сторож и все колбэки записи жили НА КОМПОНЕНТ и переживали
  // переподключение: колбэк снапшота от мёртвого сокета мог написать свой
  // (уже устаревший) кадр поверх свежего вывода, снять барьер НОВОГО соединения
  // и погасить его сторожевой таймер (повторный аудит 2.57.12, T259-08). Теперь
  // каждая асинхронная операция сверяет оба числа и на чужом становится
  // пустышкой.
  const snapshotTokenRef = useRef(0);
  const releaseSnapshotBarrier = useCallback((token?: number) => {
    if (token != null && token !== snapshotTokenRef.current) return;
    if (snapshotBarrierTimerRef.current != null) {
      clearTimeout(snapshotBarrierTimerRef.current);
      snapshotBarrierTimerRef.current = null;
    }
    if (!snapshotBarrierRef.current) return;
    snapshotBarrierRef.current = false;
    trace("barrier-release", { token: token ?? -1 });
    if (writeQueueRef.current.length > 0 && !disposedRef.current) scheduleFlushRef.current();
  }, []);
  // ── ПОКАЗ ЗАМЕНЫ ЦЕЛИКОМ (ST-06, features.presentation) ──────────────────
  // Замер probe-terminal-flicker (14.09, CPU×4, собранный клиент): маркер reset
  // писал RIS отдельной записью — пустой кадр (15–17 мс), затем ~0,5 с голых
  // «обрывков» реплея до кадра экрана; снимок с заменой писал RIS, историю,
  // досылку и кадр до четырёх записей — кадр «история без экрана» (на DOM ещё
  // и пустой). Правила — ptyTerm/presentation.ts; здесь только связывание:
  // тип рендера, ОДНА транзакция показа на терминал, её сторож и гашение
  // кликов по удерживаемой старой картинке (ST-06, ограничение плана).
  const rendererRef = useRef<PresentationRenderer>("dom");
  const presentTxnRef = useRef<PresentationTxn>(TXN_CLOSED);
  const presentTimerRef = useRef<ReturnType<typeof setTimeout> | null>(null);
  const presentTokenRef = useRef(0);
  /** Замена разобрана и ждёт первой отрисовки — событие presented (I-06). */
  const pendingPresentRef = useRef<{ what: "snapshot" | "marker"; token: number; parsedAt: number } | null>(null);
  const presentGuardNow = (): TerminalWriteGuard => ({ generation: connectionGenRef.current, epoch: writerEpochRef.current });
  const presentModeOn = (): boolean => terminalRef.current?.modes.synchronizedOutputMode ?? false;
  /** Удерживаем ли старую картинку: касание по ней кликом не уходит. */
  const presentHolding = (): boolean => presentTxnRef.current.kind === "open";
  const presentWatchdogRef = useRef<() => void>(() => {});
  /** Новое состояние транзакции; сторож — один таймер на терминал. */
  const presentSet = (next: PresentationTxn, why: string): void => {
    const prev = presentTxnRef.current;
    presentTxnRef.current = next;
    if (presentTimerRef.current != null) { clearTimeout(presentTimerRef.current); presentTimerRef.current = null; }
    const handover = prev.kind === "open" && (next.kind !== "open" || next.token !== prev.token);
    if (handover && prev.kind === "open") trace("sync", { owner: prev.owner, token: prev.token }, `txn-close:${why}`);
    if (next.kind === "open") {
      presentTimerRef.current = setTimeout(() => presentWatchdogRef.current(), Math.max(0, next.deadline - performance.now()) + 5);
      if (prev.kind !== "open" || prev.token !== next.token) trace("sync", { owner: next.owner, token: next.token }, `txn-open:${why}`);
    }
  };
  /** Отказ от своей транзакции token: END, только если режим ещё стоит. */
  const presentAbandon = (token: number, why: string, guard?: TerminalWriteGuard): void => {
    const r = abandonTxn(presentTxnRef.current, token, presentModeOn());
    if (r.bytes) writeSerial(r.bytes, undefined, guard ?? presentGuardNow());
    presentSet(r.state, why);
  };
  /** Снять любое удержание (смена рендера, локальный resize, resumed). */
  const presentDrop = (why: string, guard?: TerminalWriteGuard): void => {
    const s = presentTxnRef.current;
    if (s.kind === "open") presentAbandon(s.token, why, guard);
  };
  const presentDropRef = useRef(presentDrop);
  presentDropRef.current = presentDrop;
  /** Токен транзакции, чей BEGIN xterm уже разобрал (колбэк записи). */
  const presentParsedRef = useRef<number | null>(null);
  /**
   * Режим 2026 снят не нами: `?2026l` приложения в реплее за маркером (Kimi
   * рисует кадры в 2026), RIS/DECSTR в потоке, сторож xterm. Картинка уже
   * живая — удержание закрываем без END, касания снова уходят (ревью ST-06).
   */
  const presentModeOff = (): void => {
    const term = terminalRef.current;
    if (!term) return;
    const r = txnAfterParse(presentTxnRef.current, {
      parsedToken: presentParsedRef.current, modeOn: term.modes.synchronizedOutputMode,
    });
    if (r.ended) presentSet(r.state, "mode-off");
  };
  const presentModeOffRef = useRef(presentModeOff);
  presentModeOffRef.current = presentModeOff;
  presentWatchdogRef.current = () => {
    presentTimerRef.current = null;
    const s = presentTxnRef.current;
    // Отсутствующий конец не даёт бесконечной заморозки: свой сторож 1000 мс
    // (xterm снимает режим сам по своему — это наш флаг и гашение кликов).
    const r = txnWatchdog(s, performance.now(), presentModeOn());
    if (!r.expired) { if (s.kind === "open") presentSet(s, "rearm"); return; }
    if (r.bytes) writeSerial(r.bytes, undefined, presentGuardNow());
    presentSet(r.state, "watchdog");
  };
  const flushTermWrites = useCallback((doneArg?: unknown) => {
    // ⚠ Планировщик зовёт нас через requestAnimationFrame, а тот передаёт в
    // колбэк ЧИСЛО (метку времени). Барьером считаем только настоящую функцию,
    // иначе снапшот «применился бы» на первом же кадре анимации.
    const done = typeof doneArg === "function" ? (doneArg as () => void) : undefined;
    cancelFlushSchedule();
    const term = terminalRef.current;
    if (!term) {
      // Терминал ещё не создан — а история УЖЕ пришла: соединение открывается
      // быстрее, чем xterm успевает встать в разметку. Выбрасывать накопленное
      // здесь нельзя: ровно из-за этого человек видел пустой экран у живого
      // терминала, и вывод появлялся только когда приходил НОВЫЙ вывод (живая
      // жалоба 2026-07-30: «всё пусто, пока я не загрузил туда файл»).
      // Ждём следующего кадра — очередь остаётся на месте.
      if (writeQueueRef.current.length > 0 && !disposedRef.current) {
        scheduleFlushRef.current();
      }
      return;
    }
    // Снапшот применяется прямо сейчас — свежий вывод подождёт своей очереди.
    if (!done && snapshotBarrierRef.current) return;
    const pending = writeQueueRef.current;
    if (pending.length === 0) {
      if (done) {
        // External batching may be empty while the actor still contains a sync
        // transition/RIS/modes. A real xterm barrier in the logical writer epoch
        // makes snapshot history decisions wait until that boundary is parsed.
        terminalWriterRef.current!.barrier(done, {
          guard: {
            generation: connectionGenRef.current,
            epoch: writerEpochRef.current,
          },
        });
      }
      return;
    }
    // Never merge stream epochs. A reset marker may be waiting in the actor
    // while bytes of the next epoch are already arriving from the socket.
    //
    // ST-09: одна запись в xterm — не больше FLUSH_BATCH_MAX_BYTES: xterm 6
    // разбирает запись больше 128 КиБ синхронно, одной задачей, и на догоне
    // после фона (досылка до 2 МиБ одним кадром) жесты и ввод стояли. Разрез
    // по байту со streamEnd головы — selectFlushBatch; остаток перепланирует
    // finish(). Барьерный спуск (done: маркер, снапшот, чтение, exit) обязан
    // отдать ВСЮ серию до своего колбэка — там потолка нет, как и раньше; её
    // размер ограничен паузой по всей очереди (flowCtl).
    const { take: q, rest } = selectFlushBatch(pending, features.flowBacklog && !done ? FLUSH_BATCH_MAX_BYTES : Infinity);
    const batchGuard = q[0].guard;
    writeQueueRef.current = rest;
    queueBytesRef.current = rest.reduce((n, item) => n + item.bytes.byteLength, 0);
    // До какой позиции потока продвинется «показано», когда эта запись будет
    // РАЗОБРАНА xterm. Хвост (0–3 байта возможного начала ESC[3J) переезжает в
    // следующий спуск, поэтому вычитается ниже, когда станет известен.
    const queuedEnd = q[q.length - 1].streamEnd;
    const logicalGuard = () => ({
      generation: connectionGenRef.current,
      epoch: writerEpochRef.current,
    });
    const flushStartedAt = features.trace || features.flowBacklog ? performance.now() : 0;
    const noteApplied = (end: number, how: "write" | "chain" | "empty") => {
      if (features.flowBacklog) {
        flowStatsRef.current!.noteBatch(total);
        flowStatsRef.current!.noteFlush(performance.now() - flushStartedAt);
      }
      appliedOffsetRef.current = advanceAppliedOffset(
        appliedOffsetRef.current,
        end,
        batchGuard,
        logicalGuard(),
      );
      // «Разобрано» (I-06): байты спуска прошли парсер xterm. Одно событие на
      // спуск: время от спуска до разбора и объём — без отдельного parse, чтобы
      // поток агента не вытеснял из кольца всё остальное.
      trace("parse-done", {
        bytes: total, items: q.length, end,
        ms: Math.round((performance.now() - flushStartedAt) * 10) / 10,
      }, how);
    };
    const finish = () => {
      if (done) terminalWriterRef.current!.barrier(done, { guard: batchGuard });
      // Спуск разобран: и склейка, и неразобранное в xterm стали меньше —
      // пауза пересчитывается и там, где writeTracked не звался (пустой спуск,
      // цепочка из одних стираний).
      if (features.flowBacklog) flowCtl();
      if (writeQueueRef.current.length > 0 && !disposedRef.current) scheduleFlushRef.current();
    };
    const carriedTail = parserCarryRef.current.takeTail(batchGuard);
    let total = carriedTail.byteLength;
    for (const item of q) total += item.bytes.byteLength;
    const merged = new Uint8Array(total);
    merged.set(carriedTail, 0);
    let off = carriedTail.byteLength;
    for (const item of q) { merged.set(item.bytes, off); off += item.bytes.byteLength; }
    // Решение «исполнить, придержать или выбросить стирание истории» принимается
    // по её ЭФФЕКТИВНОМУ владельцу и по тому, что человек делает прямо сейчас
    // (см. keepHistory.ts). Режим «Вывод» обещает viewer-local history, поэтому
    // resize-repaint агента не вправе обнулить её даже у низа. Для режима
    // «Агент» прежнее правило сохраняется: исполнить у низа, отложить при чтении.
    // У сессии БЕЗ агента поведение прежнее: `clear` чистит экран, история
    // переживает — там нет источника, который перепечатывал бы её заново.
    //
    // ⚠ 07.08: «прямо сейчас» должно быть как можно ближе к ИСПОЛНЕНИЮ
    // стирания. term.write только кладёт байты в очередь xterm, а разбирает её
    // порциями ~12 мс (кадр Kimi 90 КБ — сотни мс): решение, принятое здесь, к
    // моменту исполнения ESC[3J могло устареть — человек начал прокрутку, и
    // стирание бросило его наверх. Поэтому ветка «у низа» пишет цепочкой и
    // перепроверяет позицию перед КАЖДЫМ стиранием (writeEraseChain), а
    // досылка отложенного перепроверяет её через барьер пустой записи.
    const isReadingNow = () => {
      const b = term.buffer.active;
      return selectModeRef.current || term.hasSelection() || gestureActiveRef.current || b.viewportY < b.baseY;
    };
    // Пишет байты цепочкой: между сегментами в потоке стояло «стереть историю»,
    // и решение о нём принимается в колбэке — когда очередь xterm дошла до
    // этого места, а не когда кадр прилетел по сети. Ушёл от низа — стирание
    // уходит в отложенные, как при обычном вырезании.
    // handedAt(i) — позиция потока после элемента i (ST-09, отметка «отдано
    // xterm»); без flowBacklog её нет.
    const writeEraseChain = (items: EraseChainItem[], chainDone?: () => void, handedAt?: (i: number) => number) => {
      let idx = 0;
      const step = () => {
        if (disposedRef.current) return;
        const live = terminalRef.current;
        if (!live) return;
        for (; idx < items.length; idx++) {
          const it = items[idx];
          if (it.kind === "erase") {
            // Policy/reading may change while previous xterm segments are
            // parsing. Sample here, immediately before the asynchronous erase
            // write (ST-04: политика поколения, не режим навигации).
            const action = eraseDecision(live, isReadingNow(), "chain");
            trace("erase", null, `chain-${action}`);
            if (action !== "write") {
              if (action === "defer") parserCarryRef.current.markErase(batchGuard);
              continue;
            }
            // ⚠ СТИРАНИЕ — ТОЖЕ ЗВЕНО ЦЕПИ, И ЕГО НАДО ДОЖДАТЬСЯ. Раньше здесь
            // стоял write без колбэка и `continue`: если стирание оказывалось
            // ПОСЛЕДНИМ элементом, chainDone объявлял «вывод разобран», когда
            // `ESC[3J` только лёг в очередь xterm. Снапшот тут же мерил глубину
            // истории (ещё старую, 100 строк), решал «своя не хуже» и оставлял
            // её — а через мгновение очередь доходила до стирания, и от истории
            // не оставалось ничего. Снаружи ровно «прокрутка появилась и
            // исчезла» (повторный аудит 2.57.12, T259-06).
            idx++;
            writeSerial(SCROLLBACK_ERASE, step, batchGuard, handedAt?.(idx - 1));
            return;
          }
          idx++;
          writeTracked(it.data, step, batchGuard, handedAt?.(idx - 1));
          return;
        }
        // Цепочка дошла до конца — только ЗДЕСЬ вывод действительно разобран.
        chainDone?.();
      };
      // Цепочка начинается со стирания: его решение тоже должно дождаться
      // своей очереди — впереди могут быть недоразобранные куски прошлых
      // спусков (на переигровке gap это секунды). Пустая запись — барьер:
      // колбэк сработает, когда очередь дойдёт до этого места.
      if (items.length > 0 && items[0].kind === "erase") writeSerial(EMPTY_BYTES, step, batchGuard);
      else step();
    };
    // Якорь позиции чтения на время асинхронного разбора записи (механизм
    // «улетел на самый верх», 07.08). Перед записью помечаем верхнюю видимую
    // строку маркером буфера: маркер переживает обрезку scrollback сверху и
    // всегда указывает на ту же строку текста (или умирает, если сама строка
    // вытеснена — тогда восстанавливать и нечего). После разбора возвращаем
    // вьюпорт к якорю, ЕСЛИ он ушёл ВВЕРХ больше чем на пару экранов и это не
    // прокрутка самого человека (жест или кнопки ⇈↑↓⇊ — всё идёт через
    // scrollByLines и наращивает userScrollSeqRef). В обычный ход якорь не
    // вмешивается: xterm сам держит текст читающего при обрезке, сдвига нет.
    const captureReadAnchor = (): (() => void) => {
      const b = term.buffer.active;
      if ((b as any).type === "alternate") return () => {}; // у alt-screen нет scrollback
      if (b.viewportY >= b.baseY) return () => {};           // у низа — якорь не нужен
      const marker = registerReadAnchor(term);
      if (!marker) return () => {};
      const scrollSeq = userScrollSeqRef.current;
      return () => {
        try {
          if (disposedRef.current) return;
          if (!sameRuntimeGuard(batchGuard, logicalGuard())) return;
          if (marker.isDisposed) return;    // строку съела обрезка scrollback
          if (gestureActiveRef.current) return;
          if (userScrollSeqRef.current !== scrollSeq) return; // человек крутил сам
          const live = terminalRef.current;
          if (!live) return;
          const now = live.buffer.active;
          if ((now as any).type === "alternate") return;
          const delta = marker.line - now.viewportY;
          // Только большой уход ВВЕРХ (стирание/обрезка бросают к самому верху);
          // мелкие сдвиги и движение к низу не трогаем.
          if (delta > Math.max(live.rows * 2, 40)) live.scrollLines(delta);
        } finally {
          marker.dispose();
        }
      };
    };
    // ЗАМЕР ПОТОКА: сколько строк ушло в scrollback, пока приложение печатало
    // эти байты. Прокручивающийся поток (Codex, Kimi, обычная оболочка) гонит
    // строки вверх, перерисовывающий экран на месте (Claude Code) — нет, и это
    // единственный способ узнать, чья история, не спрашивая имя агента
    // (ptyTerm/altScroll.ts, historyOwnerFromStream). Меряем ЗДЕСЬ, вокруг
    // настоящей записи: история снапшота идёт другим путём и в замер не входит.
    const streamBaseBefore = normalBaseY(term);
    const noteStreamGrowth = (bytes: number) => {
      const live = terminalRef.current;
      if (!live || disposedRef.current) return;
      if (!sameRuntimeGuard(batchGuard, logicalGuard())) return;
      const grew = normalBaseY(live) - streamBaseBefore;
      if (grew > 0) { streamLinesRef.current += grew; linesSinceFrameRef.current += grew; }
      // Ревью 15.09, повторная проверка: на тёплом соединении без кадра строки,
      // пришедшие ПОСЛЕ маркера (дельта обрыва, живой поток), решают заново —
      // один кадр ради hist_lines за соединение, как на маркере.
      if (grew > 0 && histProbeWatchRef.current === connectionGenRef.current
        && histProbeConnRef.current !== connectionGenRef.current
        && shouldProbeHistory(snapHistRef.current, linesSinceFrameRef.current)) {
        histProbeWatchRef.current = -1;
        histProbeConnRef.current = connectionGenRef.current;
        trace("recovery", { lines: linesSinceFrameRef.current, hist: snapHistRef.current }, "marker:hist-probe-late");
        recoveryApiRef.current.demand("hist-probe");
      }
      streamBytesRef.current += bytes;
      // Затухание: вердикт принадлежит тому, что приложение делает СЕЙЧАС, а не
      // тому, что оболочка напечатала до его запуска (см. decayStreamSample).
      const decayed = decayStreamSample(streamLinesRef.current, streamBytesRef.current);
      streamLinesRef.current = decayed.lines;
      streamBytesRef.current = decayed.bytes;
    };
    const atBottom = !isReadingNow();
    if (
      parserCarryRef.current.hasErase(batchGuard)
      && !keepPendingDecision(term, "pending")
    ) {
      parserCarryRef.current.clearErase(batchGuard);
    }
    if (atBottom && parserCarryRef.current.hasErase(batchGuard)) {
      parserCarryRef.current.clearErase(batchGuard);
      const eraseTicket = scrollbackEraseGateRef.current.capture();
      // Досылка отложенного стирания — через барьер: пока очередь дойдёт до
      // этих байт, человек мог снова уйти от низа. Перепроверяем в момент
      // исполнения; не сошлось — кладём обратно в отложенные. Ticket нужен,
      // потому что снятое pending уже не удалит clearAllErases: переход через
      // Output обязан навсегда отменить и такой in-flight intent.
      writeSerial(EMPTY_BYTES, () => {
        if (disposedRef.current) return;
        const live = terminalRef.current;
        if (!live) return;
        // Re-check owner and viewport at the actual execution boundary: the
        // human can switch Agent -> Output while this barrier is queued.
        if (scrollbackEraseGateRef.current.isCurrent(eraseTicket)) {
          const action = eraseDecision(live, isReadingNow(), "deferred");
          trace("erase", null, `deferred-${action}`);
          if (action === "defer") {
            parserCarryRef.current.markErase(batchGuard);
            return;
          }
          if (action === "write") writeSerial(SCROLLBACK_ERASE, undefined, batchGuard);
        }
        // При write метка должна идти ПОСЛЕ стирания, иначе оно её сотрёт. При
        // discard стирания нет, но тот же порядок оставляет gap на своём месте.
        if (parserCarryRef.current.takeGap(batchGuard)) {
          trace("gap", null, "queue-drop");
          writeSerial(`\r\n\x1b[2m${t("pty.historyGap")}\x1b[0m\r\n`, undefined, batchGuard);
        }
      }, batchGuard);
    } else if (parserCarryRef.current.takeGap(batchGuard)) {
      // Пометка о выброшенной голове очереди — ПЕРЕД самими байтами: иначе она
      // легла бы после уже отрисованного продолжения и указывала не туда.
      trace("gap", null, "queue-drop");
      writeSerial(`\r\n\x1b[2m${t("pty.historyGap")}\x1b[0m\r\n`, undefined, batchGuard);
    }
    // Цепочка с решением по каждому стиранию нужна тому, чья политика стирание
    // исполняет (honor). Прежнее условие — «агент на переднем плане»; у
    // оболочки без агента (preserve/shell) они совпадают.
    const splitAtBottom = eraseRoute(features.retention, agentInFgRef.current, retentionRef.current.erase === "honor").use;
    if (splitAtBottom && atBottom) {
      const { items, tail } = splitScrollbackErase(merged);
      parserCarryRef.current.storeTail(batchGuard, tail);
      const appliedEnd = queuedEnd - tail.byteLength;
      let chainBytes = 0;
      for (const it of items) if (it.kind === "data") chainBytes += it.data.byteLength;
      // ST-09: merged начинается там, где кончается поток до этой склейки
      // (удержанный хвост прошлой — перед q[0]); конец каждого элемента — от него.
      const mergedStart = queuedEnd - total;
      const chainEnds = features.flowBacklog ? eraseChainEnds(merged, items) : null;
      writeEraseChain(
        items,
        () => { noteStreamGrowth(chainBytes); noteApplied(appliedEnd, "chain"); finish(); },
        chainEnds ? (i) => mergedStart + chainEnds[i] : undefined,
      );
      return;
    }
    const { data, tail, erased } = stripScrollbackErase(merged);
    parserCarryRef.current.storeTail(batchGuard, tail);
    if (erased > 0) {
      const reading = isReadingNow();
      // Прежнее условие вычисляется так же, как раньше (с тем же коротким
      // замыканием по агенту), — ради побочного эффекта навигации.
      const legacyDefer = agentInFgRef.current
        && scrollbackEraseAction(historyOwnerRef.current(term), reading) === "defer";
      const policyDefer = eraseActionFor(retentionRef.current, reading) === "defer";
      const route = eraseRoute(features.retention, legacyDefer, policyDefer);
      if (route.diverged) noteRetentionShadow("strip", legacyDefer ? "defer" : "discard", policyDefer ? "defer" : "discard", reading);
      if (route.use) {
        trace("erase", { erased }, "strip-defer");
        parserCarryRef.current.markErase(batchGuard);
      }
    }
    const appliedEnd = queuedEnd - tail.byteLength;
    if (data.byteLength > 0) {
      const anchor = captureReadAnchor();
      writeTracked(data, () => {
        anchor();
        noteStreamGrowth(data.byteLength);
        noteApplied(appliedEnd, "write");
        finish();
      }, batchGuard, appliedEnd);
    } else {
      noteApplied(appliedEnd, "empty");
      finish();
    }
  }, []);
  // Планирование спуска: видимой странице — кадр анимации (один проход разбора
  // на кадр экрана, ради него склейка и делалась), скрытой — таймер, потому что
  // кадров там не бывает вовсе. Ссылка через ref: flushTermWrites объявлен выше
  // и должен уметь перепланировать себя, не попадая в круг зависимостей.
  const scheduleFlush = useCallback(() => {
    if (writeRafRef.current != null || writeTimerRef.current != null) return;
    if (typeof document !== "undefined" && document.visibilityState === "hidden") {
      writeTimerRef.current = setTimeout(() => {
        writeTimerRef.current = null;
        flushTermWrites();
      }, BACKGROUND_FLUSH_MS);
      return;
    }
    writeRafRef.current = requestAnimationFrame(flushTermWrites);
    // ST-09 (T-33 raf-stall): страховочный таймер рядом с кадром — корректность
    // не держится на том, что rAF придёт (WebView с отцепленным или перекрытым
    // видом кадров не даёт и visibilitychange не шлёт). Спускает тот, кто успел
    // первым: flushTermWrites снимает оба через cancelFlushSchedule.
    if (features.flowBacklog) {
      writeTimerRef.current = setTimeout(() => {
        writeTimerRef.current = null;
        if (writeQueueRef.current.length > 0 && !document.hidden) flowStatsRef.current!.noteTimerFlush();
        flushTermWrites();
      }, FLUSH_SAFETY_MS);
    }
  }, [flushTermWrites]);
  const scheduleFlushRef = useRef(scheduleFlush);
  scheduleFlushRef.current = scheduleFlush;
  const enqueueTermWrite = useCallback((bytes: Uint8Array, streamEnd: number) => {
    // Отметка «терминал что-то ответил». По ней прокрутка в alt-screen отличает
    // приложение, которое колесо ПРИНЯЛО (перерисовало экран), от того, которое
    // его игнорирует, — см. scrollByLines.
    lastOutputAtRef.current = Date.now();
    writeQueueRef.current.push({
      bytes,
      streamEnd,
      guard: {
        generation: connectionGenRef.current,
        epoch: writerEpochRef.current,
      },
    });
    queueBytesRef.current += bytes.byteLength;
    queueEndRef.current = streamEnd;
    let dropped = false;
    let droppedBytes = 0;
    const queueCap = features.flowBacklog ? FLOW_QUEUE_CAP_BYTES : QUEUE_CAP_BYTES;
    while (queueBytesRef.current > queueCap && writeQueueRef.current.length > 1) {
      const head = writeQueueRef.current.shift();
      queueBytesRef.current -= head ? head.bytes.byteLength : 0;
      droppedBytes += head ? head.bytes.byteLength : 0;
      if (head) parserCarryRef.current.clearTail(head.guard);
      dropped = true;
      // Позиция resume стала враньём (байты посчитаны в offset, но в терминал
      // не попадут никогда) — следующий коннект пусть идёт полным reset,
      // а не resume с дырой. См. invalidateResumeRef у persistResume.
      invalidateResumeRef.current();
    }
    if (dropped) {
      trace("enqueue-drop", { bytes: droppedBytes, cap: queueCap });
      flowStatsRef.current!.noteDrop(droppedBytes);
      if (features.flowBacklog) {
        // I-11: неполнота видна не только строкой в xterm, но и документу
        // чтения — выброшенные байты в него не попадут никогда (ST-09 A4).
        streamGapRef.current = true;
        reportFlow("drop", "queue-cap");
      }
      const survivorGuard = writeQueueRef.current[0]?.guard ?? {
        generation: connectionGenRef.current,
        epoch: writerEpochRef.current,
      };
      parserCarryRef.current.markGap(survivorGuard);
      // ⚠ Выброшена ПРОИЗВОЛЬНАЯ голова очереди, а не целые команды: WS режет
      // поток по границе своего кадра, и на месте разреза могло остаться
      // начало escape-последовательности — `ESC` без CSI, половина OSC,
      // недобранный UTF-8. Уцелевший хвост продолжать разбирать тем же
      // состоянием парсера нельзя: незакрытый OSC проглатывает весь
      // последующий вывод молча (внешний аудит 13.08.2026, находка T-005).
      //
      // CAN (0x18) прерывает начатую последовательность, ST закрывает
      // строковую (OSC/DCS/APC). Историю и экран это НЕ трогает — в отличие от
      // RIS, которым было бы соблазнительно «начать с чистого листа»: человек
      // потерял бы всю прокрутку из-за фонового переполнения.
      writeSerial(PARSER_ABORT, undefined, survivorGuard);
      // И просим свежий кадр: экран после дыры собран из неполного потока.
      if (features.recoveryV1) recoveryApiRef.current.demand("queue-gap");
      else requestScreenFrameRef.current(undefined, "queue-drop");
    }
    // Пауза раньше потерь: очередь склейки входит в решение о паузе.
    if (features.flowBacklog) flowCtl();
    scheduleFlushRef.current();
  }, []);

  // «Пришли готовый кадр экрана». Просим ВНУТРИ соединения, а не флагом в
  // адресе: путь до агента проверяет релей по закрытому белому списку и на
  // незнакомый параметр отвечает 400 — 12.08.2026 из-за этого телефон
  // перестал открывать терминалы вовсе (35 отказов подряд, до компьютера
  // соединение не доходило). Здесь релея нет вообще: старый агент
  // неизвестную просьбу молча игнорирует, и мы просто остаёмся на потоке.
  //
  // ⚠ НЕ СРАЗУ, а когда размер устоялся (SCREEN_REQUEST_DELAY_MS). Кадр
  // снимается в геометрии PTY, а она идёт от нашего же fit() — и при открытии
  // он успевает соврать: в боевом логе 12.08 в 20:57 кадр снялся как `48x10`,
  // потому что высота ещё не домерилась (клавиатура, анимация шторки). Экран
  // схлопнулся в десять строк, и снаружи это выглядело как «терминал завис».
  // Полсекунды тишины дешевле такого кадра.
  //
  // Сокет привязывается при постановке: таймер от уже закрытого соединения не
  // должен стрелять в новое (у того свой запрос в onopen). Без аргумента берётся
  // текущий живой сокет — так зовёт будильник возврата из фона.
  // why — причина просьбы для трассы (T-24: сколько причин и таймеров сошлось
  // в одну просьбу, видно по screen-req; сам запрос агенту прежний).
  const requestScreenFrame = useCallback((sock?: WebSocket | null, why = "other") => {
    const target = sock ?? wsRef.current;
    if (!target) { trace("screen-req", null, `skip-nosocket:${why}`); return; }
    const timer = window.setTimeout(() => {
      screenRequestTimersRef.current.delete(timer);
      if (disposedRef.current) return;
      if (wsRef.current !== target || target.readyState !== WebSocket.OPEN) {
        trace("screen-req", null, `drop-closed:${why}`);
        return;
      }
      try {
        const geomRev = geometryRevisionRef.current;
        target.send(JSON.stringify({
          t: "screen",
          geom_rev: geomRev,
        }));
        noteScreenRequest(target, geomRev, null);
        trace("screen-req", { pending: screenRequestTimersRef.current.size }, `sent:${why}`);
      } catch { /* ignore */ }
    }, SCREEN_REQUEST_DELAY_MS);
    screenRequestTimersRef.current.add(timer);
    trace("screen-req", { pending: screenRequestTimersRef.current.size }, `scheduled:${why}`);
  }, []);
  // Через ref — из горячего пути очереди (enqueueTermWrite объявлен выше и с
  // пустыми deps, как и scheduleFlushRef рядом).
  const requestScreenFrameRef = useRef(requestScreenFrame);
  requestScreenFrameRef.current = requestScreenFrame;

  // ── АДАПТЕР КООРДИНАТОРА ВОССТАНОВЛЕНИЯ (ST-05, features.recoveryV1) ──────
  // Правила — в ptyTerm/recoveryCoordinator.ts: объединение причин, max-wait,
  // один запрос в пути, срок ответа, backoff, degraded, откат холодной страницы.
  // Здесь только исполнение: вызвать переход, выполнить его эффекты (отправка,
  // откат, трасса) и держать ОДИН таймер на nextWakeAt(). Функции читают только
  // ref'ы, поэтому версия, пойманная обработчиками сокета при connect(), ведёт
  // себя так же, как свежая. Прежний путь (requestScreenFrame выше) в этом
  // экземпляре не работает вовсе, и наоборот (план 9.1).
  const connectRef = useRef<() => void>(() => {});
  const recoveryNow = () => performance.now();
  /** События координатора, которые идут и в журнал агента, а не только в трассу. */
  const RECOVERY_LOGGED: ReadonlySet<string> = new Set(["timeout", "negative", "degraded", "fallback", "revived"]);
  const armRecovery = () => {
    if (recoveryTimerRef.current != null) {
      window.clearTimeout(recoveryTimerRef.current);
      recoveryTimerRef.current = null;
    }
    if (disposedRef.current || !features.recoveryV1) return;
    const at = recovery.nextWakeAt(recoveryRef.current);
    if (at === null) return;
    recoveryTimerRef.current = window.setTimeout(() => {
      recoveryTimerRef.current = null;
      if (disposedRef.current) return;
      syncRecoveryGeometry();
      runRecovery(recovery.wake(recoveryRef.current, recoveryNow(), Math.random()), "wake");
    }, Math.max(0, Math.ceil(at - recoveryNow())));
  };
  const runRecovery = (result: RecoveryStep, why: string) => {
    const before = recoveryRef.current;
    recoveryRef.current = result.state;
    let dropped = false;
    for (const e of result.effects) {
      if (e.kind === "send") {
        const ws = wsRef.current;
        const fields = { req: e.id, geomRev: e.geomRev, causes: recovery.causeMask(e.causes) };
        if (!ws || ws.readyState !== WebSocket.OPEN) {
          // Сокет уже закрывается, а onclose ещё не пришёл. Запрос в пути НЕ
          // засчитываем: его срок истёк бы без соединения (замер ревью 14.09).
          // Ниже автомат уходит в offline, onclose это лишь подтвердит.
          trace("screen-req", fields, "drop-closed");
          dropped = true;
          continue;
        }
        try {
          // Тот же {t:"screen", geom_rev}, плюс req: старый агент читает только
          // geom_rev (I-14), новый возвращает req эхом в screen/screen-none.
          ws.send(JSON.stringify({ t: "screen", geom_rev: e.geomRev, req: e.id }));
          noteScreenRequest(ws, e.geomRev, e.id);
          trace("screen-req", fields, `sent:${e.causes[0] ?? "none"}`);
        } catch { trace("screen-req", fields, "send-failed"); }
      } else if (e.kind === "fallback") {
        // Холодная страница так и не получила кадр (T-28): один раз на
        // страницу — сбросить позицию и переподключиться полным reset с
        // реплеем кольца (прежний путь холодного старта). Не изнутри
        // обработчика, который сейчас исполняется, а следующим тиком.
        // Переподключается только ЖИВОЕ соединение, на котором откат выдан.
        // Если сокет тем временем закрылся, позицию стираем и всё: обычный
        // реконнект по backoff уйдёт без resume, то есть тем же reset, а
        // свой connect() открыл бы второй сокет в обход reconnectTimer.
        const gen = connectionGenRef.current;
        trace("recovery", { conn: result.state.conn }, "fallback:reconnect-reset");
        window.setTimeout(() => {
          if (disposedRef.current) return;
          invalidateResumeRef.current();
          const ws = wsRef.current;
          if (connectionGenRef.current !== gen || !ws || ws.readyState !== WebSocket.OPEN) {
            trace("recovery", { conn: gen }, "fallback:via-reconnect");
            return;
          }
          if (reconnectTimer.current) { clearTimeout(reconnectTimer.current); reconnectTimer.current = null; }
          connectRef.current();
        }, 0);
      } else {
        const fields = {
          req: e.id ?? -1, noProgress: e.noProgress, causes: recovery.causeMask(e.causes), phase: result.state.phase,
        };
        trace("recovery", e.reason ? { ...fields, why: e.reason } : fields, e.what);
        if (RECOVERY_LOGGED.has(e.what)) {
          sendDiag("recovery", {
            event: e.what, req: e.id, np: e.noProgress, causes: e.causes.join(","),
            reason: e.reason ?? "", phase: result.state.phase, peer: result.state.peer,
          });
        }
      }
    }
    if (dropped) recoveryRef.current = recovery.offline(recoveryRef.current).state;
    if (before.phase !== recoveryRef.current.phase) {
      trace("recovery", { prev: before.phase, phase: recoveryRef.current.phase, causes: recovery.causeMask(recoveryRef.current.causes) }, `phase:${why}`);
    }
    armRecovery();
  };
  /** Ревизия сетки xterm сменилась: запрос прежней ревизии устарел (T-25). */
  const syncRecoveryGeometry = () => {
    if (!features.recoveryV1 || disposedRef.current) return;
    const rev = geometryRevisionRef.current;
    if (recoveryRef.current.geomRev === rev) return;
    // Сетку сменил сам кадр операции (adopt/restore): он пишется в неё и не
    // устарел — без второго запроса (ревью 15.09, повторная проверка).
    const adopting = adoptTicketRef.current;
    if (adopting !== null) {
      const kept = recovery.adopted(recoveryRef.current, adopting, rev);
      if (kept.state.geomRev === rev) {
        trace("recovery", { req: adopting, rev }, "adopt-kept");
        runRecovery(kept, "adopt");
        return;
      }
    }
    runRecovery(recovery.geometry(recoveryRef.current, rev, recoveryNow()), "geometry");
  };
  /** Причина нового кадра. Повтор той же причины объединяется координатором (T-24). */
  const demandRecovery = (cause: RecoveryCause) => {
    if (!features.recoveryV1 || disposedRef.current) return;
    syncRecoveryGeometry();
    runRecovery(recovery.demand(recoveryRef.current, cause, recoveryNow()), cause);
  };
  recoveryApiRef.current = {
    demand: demandRecovery,
    syncGeometry: syncRecoveryGeometry,
    // Сервер подтвердил сетку PTY (/state или terminal-controls): причина
    // чужой высоты/ширины получает ещё одну попытку и не застревает в degraded.
    serverGrid: (why: string) => {
      if (!features.recoveryV1 || disposedRef.current) return;
      runRecovery(recovery.serverGrid(recoveryRef.current, recoveryNow()), `server-grid:${why}`);
    },
    // Первый размер сокета ушёл (rendererSettled или таймаут ожидания рендера):
    // запрос кадра, державшийся ради него (holdForResize), уходит ближайшим
    // будильником — после размера, в своей сетке (ревью 15.09, дефект 2).
    resizeSent: () => {
      if (!features.recoveryV1 || disposedRef.current) return;
      if (recoveryRef.current.resizeHoldUntil === null) return;
      runRecovery(recovery.resizeSent(recoveryRef.current), "resize-sent");
    },
  };

  const connect = useCallback(() => {
    if (!id || disposedRef.current) return;

    // Tear down any previous socket before opening a new one. A visibilitychange
    // reconnect racing the backoff timer can otherwise leave two live sockets to
    // the same PTY session — the relay bridges each as its own stream, so the
    // session shows duplicate opens (inflates opens_total, and wastes a bridge
    // until the relay's 30s ping reaps the stale one). Detach handlers FIRST so
    // the old socket's onclose can't schedule yet another reconnect.
    const prev = wsRef.current;
    if (prev) {
      wsRef.current = null;
      prev.onopen = prev.onmessage = prev.onclose = prev.onerror = null;
      try { prev.close(); } catch { /* ignore */ }
    }

    // Show the "connecting…" overlay until the first byte from the bridge: over
    // the cloud the reverse tunnel (phone→relay→PC dial-back) takes a beat, and
    // a black screen reads as "stuck". Cleared in onmessage.
    gotOutputRef.current = false;
    setShowConnecting(true);
    // Новое соединение — новое решение об истории снапшота: маркеры прошлого
    // соединения не вправе разрешать историю в этом (ptyTerm/snapshotHistory.ts).
    snapshotHistoryRef.current = SNAPSHOT_HISTORY_INIT;
    // НОВОЕ ПОКОЛЕНИЕ. Всё асинхронное, начатое прошлым сокетом (кадр снапшота,
    // его сторож, отложенный reset), с этого момента ничего не меняет: иначе
    // старый кадр ложится поверх нового вывода и снимает чужой барьер (T259-08).
    const writer = terminalWriterRef.current!;
    const hadUnappliedWrites = writeQueueRef.current.length > 0 || writer.pending > 0;
    // Откуда продолжить поток — решается ДО смены поколения: отметка «отдано
    // xterm» сверяется с поколением и эпохой писателя старого сокета.
    const acceptedBefore = offsetRef.current;
    const resumePlan = reconnectResume({
      flowBacklog: features.flowBacklog,
      unapplied: hadUnappliedWrites,
      hasEpoch: !!epochRef.current,
      current: { generation: connectionGenRef.current, epoch: writerEpochRef.current },
      handed: handedMarkRef.current,
      applied: appliedOffsetRef.current,
      // ST-05 (хвост): удержанное начало возможного ESC[3J — принято, но не
      // отдано xterm; parserCarry.clear() ниже его выбросит.
      accepted: offsetRef.current,
      heldTail: parserCarryRef.current.tailLength({ generation: connectionGenRef.current, epoch: writerEpochRef.current }),
    });
    cancelGestureRef.current();
    connectionGenRef.current++;
    // A new socket must not inherit parser work from the old stream: the queue
    // below is dropped, and the writer discards everything except the write
    // xterm already holds. If some accepted bytes were not applied, the ACCEPTED
    // offset is no longer truthful. ST-09 (flowBacklog): continue from what was
    // handed to xterm — the server re-sends the dropped bytes from its ring, so
    // there is no hole and no reset (RIS would wipe the scrollback). Rollback or
    // an unknown position: request a full reset instead of a silent hole.
    if (resumePlan.kind === "handed") {
      offsetRef.current = resumePlan.offset;
      queueEndRef.current = resumePlan.offset;
    } else if (resumePlan.kind === "reset") {
      invalidateResumeRef.current();
    }
    if (features.flowBacklog) {
      // Отметка переезжает в новое поколение: пока новый сокет не прислал
      // маркер, экран описывается той же позицией (обрыв до маркера и снова
      // connect при ещё занятом writer не должен сбрасывать её).
      handedMarkRef.current = epochRef.current
        ? { generation: connectionGenRef.current, epoch: writerEpochRef.current, end: offsetRef.current }
        : null;
    }
    cancelFlushSchedule();
    writeQueueRef.current = [];
    queueBytesRef.current = 0;
    scrollbackEraseGateRef.current.invalidate();
    parserCarryRef.current.clear();
    writer.setContext({ generation: connectionGenRef.current, epoch: writerEpochRef.current });
    snapshotTokenRef.current++;
    if (snapshotBarrierTimerRef.current != null) {
      clearTimeout(snapshotBarrierTimerRef.current);
      snapshotBarrierTimerRef.current = null;
    }
    snapshotBarrierRef.current = false;
    // Остаток страничного канала принадлежал прошлому соединению.
    pageAccumRef.current = 0;
    pageBudgetRef.current = MAX_PAGES_PER_GESTURE;
    if (pageIdleTimerRef.current != null) {
      clearTimeout(pageIdleTimerRef.current);
      pageIdleTimerRef.current = null;
    }
    // Новое соединение отменяет отложенные наблюдения прокрутки (их байты ушли
    // в прежний сокет), но НЕ стирает свидетельства: процесс за ним, скорее
    // всего, тот же, а смену процесса или режима поймает область действия
    // (restoreProbeVerdict). Иначе каждый обрыв при отказавшем хранилище
    // уничтожал бы хорошее живое состояние (T-03).
    cancelScrollObservations();
    altScrollProbeTokenRef.current++;
    altScrollQueuedActionsRef.current = [];
    geometryRetryRevisionRef.current = null;
    if (features.recoveryV1) {
      // Новое соединение — новая операция восстановления: запрос, кадр в
      // применении и таймер прежнего сокета больше ничего не решают (T-25).
      // Переживают номера запросов, сетка, потраченный откат на reset и
      // незакрытая холодная страница. До onopen автомат offline: запрос в
      // сокет, который ещё не открыт, ушёл бы в никуда и тикал бы сроком.
      if (recoveryTimerRef.current != null) {
        window.clearTimeout(recoveryTimerRef.current);
        recoveryTimerRef.current = null;
      }
      recoveryRef.current = recovery.offline(
        recovery.connection(recoveryRef.current, connectionGenRef.current).state,
      ).state;
      markerSeenRef.current = false;
    }
    trace("conn", {
      hadUnapplied: hadUnappliedWrites, resume: !!epochRef.current, attempt: attemptRef.current,
      ...(features.flowBacklog ? {
        plan: resumePlan.kind,
        ...(resumePlan.kind === "handed" ? { behind: acceptedBefore - resumePlan.offset } : {}),
        ...(resumePlan.kind === "reset" ? { why: resumePlan.why } : {}),
      } : {}),
    }, "connect");

    let url: string;
    try {
      // resume по offset+epoch: если уже знаем позицию в потоке — просим сервер
      // дослать только хвост, а не весь scrollback (меньше трафика, нет дублей).
      //
      // Параметр отдаём В ptyWSUrl, а не дописываем к готовому адресу: в
      // облачном режиме готовый адрес принадлежит РЕЛЕЮ, и дописанный resume
      // доставался ему, а не агенту. Замер 02.08.2026: 33 переподключения из
      // 33 приходили полным снимком 512 КиБ с маркером reset — то есть каждый
      // разрыв стирал человеку прокрученную историю и стоил лишнего трафика.
      //
      // ⚠ Здесь ПРИНЯТОЕ (offsetRef). Очередь склейки обрыв НЕ переживает:
      // выше connect() её выбрасывает. Если в ней или во writer оставались
      // неразобранные байты (hadUnappliedWrites): с flowBacklog offsetRef уже
      // переставлен на ОТДАННОЕ xterm (reconnectResume) — сервер дошлёт
      // выброшенное из кольца; без него (откат) или без отметки позиция
      // сброшена invalidateResume — соединение уйдёт без resume, полным reset
      // (прежний комментарий обещал обратное). Иначе всё принятое уже разобрано и
      // принятое равно показанному — кроме удержанного хвоста возможного
      // ESC[3J (до 3 байт, parserCarry.storeTail): его connect() тоже
      // выбрасывает, а сервер после принятой позиции его не пришлёт. С
      // flowBacklog позиция уже отодвинута на длину хвоста (reconnectResume,
      // «хвост» ST-05) — сервер дошлёт эти байты сам; в откате — прежнее
      // ограничение тёплого resume. На диск уходит applied: там
      // страница умирает вместе с очередью (см. appliedOffsetRef).
      url = ptyWSUrl(id, resumeParam(epochRef.current, offsetRef.current));
    } catch {
      return; // not configured (e.g. no device selected in cloud mode)
    }

    const ws = new WebSocket(url);
    ws.binaryType = "arraybuffer";
    wsRef.current = ws;

    // Вотчдог первого байта (см. объявление firstByteTimer): покрывает и
    // зависший хендшейк (onopen не пришёл), и «глухой» открытый сокет.
    if (firstByteTimer.current) clearTimeout(firstByteTimer.current);
    firstByteTimer.current = setTimeout(() => {
      firstByteTimer.current = null;
      if (wsRef.current === ws && !gotOutputRef.current) {
        trace("conn", null, "first-byte-timeout");
        try { ws.close(); } catch { /* ignore */ }
      }
    }, 15000);

    ws.onopen = () => {
      trace("conn", null, "open");
      // Способности — одним сообщением и ВСЕГДА с полем capabilities: без него
      // сервер читает клиента как size-owner-v1 первой версии
      // (terminalCapability, internal/web/pty_history.go). Без сообщения вовсе
      // размерами он не управляет — поэтому список без size-owner-v1 ничего не
      // меняет клиенту с выключенным sizeOwner. screen-request-v1 (ST-05, T-36)
      // объявляет только новый путь восстановления: откат recoveryV1=false
      // возвращает протокол байт в байт прежним.
      const capabilities = [
        ...(features.sizeOwner ? ["size-owner-v1"] : []),
        ...(features.agentHistory ? ["agent-history-v1"] : []),
        ...(features.recoveryV1 ? ["screen-request-v1"] : []),
      ];
      if (capabilities.length > 0) try {
        ws.send(JSON.stringify({ t: "terminal-capabilities", v: 1, capabilities }));
      } catch { /* legacy connection */ }
      flowPausedRef.current = false; // every new server subscription starts unpaused
      flowCtl();
      setConnected(true);
      setReconnecting(false);
      setGaveUp(false);
      setSessionMissing(false);
      sessionMissingRef.current = false;
      // Новое соединение: область действия сверяется заново (смена процесса
      // или режима сотрёт чужие наблюдения), память той же области дополняет.
      restoreProbeVerdictRef.current();
      // Плашку «Компьютер обновляется» снимает именно ВОССТАНОВЛЕННАЯ связь, а
      // не любой коннект: предупреждение приходит по живому сокету, и гасить его
      // мгновенно (до обрыва) нельзя — человек не успел бы прочитать.
      if (pcUpdatingAtRef.current && Date.now() - pcUpdatingAtRef.current > 5000) {
        pcUpdatingAtRef.current = 0;
        setPcUpdating(null);
      }
      // Idle-вотчдог этого соединения (лимит берётся из hbLimitRef, который
      // выставит маркер). lastMsg сбрасываем сейчас, чтобы не сработать на
      // паузе МЕЖДУ соединениями.
      lastMsgRef.current = Date.now();
      if (idleWatchTimer.current) clearInterval(idleWatchTimer.current);
      let idleTickAt = Date.now();
      idleWatchTimer.current = setInterval(() => {
        if (wsRef.current !== ws) return; // устаревший интервал; снимется в onclose/cleanup
        const limit = hbLimitRef.current;
        const now = Date.now();
        if (features.flowBacklog) {
          // ST-09 (T-33): после разморозки таймер и доставка накопившихся hb
          // идут в неизвестном порядке. Видимая страница, чей тик опоздал,
          // стояла целиком — тишина до этого момента ничего не доказывает.
          // Льгота SILENCE_THAW_GRACE_MS от возврата/разморозки (silenceVerdict).
          const thaw = thawedAt(idleTickAt, now, IDLE_TICK_MS, !document.hidden);
          if (thaw != null) {
            trace("vis", { lateMs: now - idleTickAt }, "thaw");
            visibleSinceRef.current = Math.max(visibleSinceRef.current, thaw);
          }
          idleTickAt = now;
          if (closeIfSilent(ws, now, false)) return;
          // Разморозка без visibilitychange: приговор отложен льготой —
          // перепроверить ровно по её истечении, не ждать следующего тика.
          if (thaw != null) armSilenceRecheck();
          reportFlow("periodic");
        } else if (limit > 0 && now - lastMsgRef.current > limit) {
          trace("conn", { limitMs: limit }, "idle-timeout");
          try { ws.close(); } catch { /* ignore */ }
        }
      }, IDLE_TICK_MS);
      // Do NOT reset the retry budget here. Over the relay a socket to a DEAD
      // pty session (e.g. PC rebooted — sessions are RAM-only) still "opens" and
      // is closed a moment later; resetting on open would loop reconnect forever
      // and never hit maxAttempts. Reset only once the link proves real: first
      // byte from the bridge (onmessage → markStable) or staying open a few sec.
      if (stableTimer.current) clearTimeout(stableTimer.current);
      stableTimer.current = setTimeout(markStable, 3000);

      // Send current terminal size. Re-fit first: on reconnect the layout may
      // have changed (keyboard, rotation) while the socket was dead, and a stale
      // cols makes Claude Code's full-width status line render past the right edge
      // (the "output gets cut off" symptom). fit() syncs cols/rows to the visible
      // area before we tell the PTY.
      const term = terminalRef.current;
      if (term) {
        // ⚠ ЧЕРЕЗ fitLocal, а не голым fit(). Переподключение случается и во
        // время набора текста: голый fit ужимал терминал до клавиатурных 9–11
        // строк, а sendResize(true) при клавиатуре молчит — терминал оставался
        // ужатым при тридцатистрочном PTY, и следующий кадр складывался в
        // последнюю строку (повторный аудит 2.57.13, разбор геометрии).
        fitLocalRef.current();
        setTermSize({ cols: term.cols, rows: term.rows });
        if (features.capacity && rendererPendingRef.current) {
          // ST-08 (T-32): рендер WebGL ещё подключается, и его ячейка поменяет
          // вместимость. Размер, посланный сейчас, через миг пришлось бы
          // исправлять вторым — два PTY resize на один join. Ждём рендер (его
          // подключение шлёт этот размер, rendererSettled), но не дольше
          // RENDERER_WAIT_MS: сервер держит прежний размер, пока нового нет.
          openResizeWaitRef.current = ws;
          trace("resize-send", null, "force-wait-renderer");
          // Ревью 15.09 (дефект 2): запрос кадра этого сокета ждёт этот размер
          // (holdForResize), иначе агент снимет кадр в прежней сетке PTY и
          // запросов станет до четырёх. Отпускают rendererSettled и таймаут.
          if (features.recoveryV1) runRecovery(recovery.holdForResize(recoveryRef.current, recoveryNow()), "resize-wait");
          window.setTimeout(() => {
            if (disposedRef.current || openResizeWaitRef.current !== ws || wsRef.current !== ws) return;
            openResizeWaitRef.current = null;
            trace("resize-send", null, "force-renderer-timeout");
            sendResize(true);
            recoveryApiRef.current.resizeSent();
          }, RENDERER_WAIT_MS);
        } else {
          sendResize(true); // новый сокет — сервер нашего размера ещё не знает
        }
      }

      // «Пришли готовый кадр экрана» — отложенно и внутри соединения; почему
      // именно так — в комментарии у requestScreenFrame.
      // ST-09 A6: у координатора кадр просит МАРКЕР, а не сам факт соединения —
      // первый reset (базовая точка), холодная страница, пропуск, смена эпохи.
      // Тёплое переподключение живой страницы (resumed без пропуска) досылает
      // хвост и лишней перерисовки экрана не получает.
      if (!features.recoveryV1) {
        requestScreenFrame(ws, "open");
      } else {
        // Сокет открыт: координатор снова вправе слать и отсчитывать сроки.
        // Причины, поднятые за время рукопожатия, уходят обычной тишиной.
        runRecovery(recovery.online(recoveryRef.current, recoveryNow()), "online");
      }

      // Геометрия окна Telegram — в лог агента. Заведено 04.08.2026: на iPad
      // владельца кнопка «✕ Закрыть» ложилась поверх нашей «←», а на стенде
      // воспроизводились только те значения, которые мы сами и придумали. Пусть
      // клиент назовёт свои: platform, экран, окно, что сообщил Telegram и что
      // в итоге получила шапка. Одна строка на открытие терминала, и только в
      // Telegram — в APK, вебе и окне exe плавающих кнопок клиента нет.
      // ⚠ Шлём ВСЕГДА, а не только из Telegram. Первая версия этой строки жила
      // под условием `tg?.initData`, и в логе не появилось ни одной записи —
      // потому что человек смотрел в окне на ПК и в вебе, где Telegram нет
      // вовсе. Диагностика, которая молчит именно тогда, когда её ждут,
      // бесполезна: пусть каждая поверхность называет себя сама.
      try {
        const tg = getTelegram();
        const backTop = Math.round(
          document.querySelector(".pty-header .back-btn")?.getBoundingClientRect().top ?? -1,
        );
        sendDiag("tg-chrome", {
          client: CLIENT_VERSION,
          surface: tg?.initData ? "telegram" : isNativeApp ? "apk" : "web",
          platform: tg?.platform || "",
          ver: tg?.version || "",
          fullscreen: !!tg?.isFullscreen,
          screenW: window.screen?.width ?? 0,
          screenH: window.screen?.height ?? 0,
          winW: window.innerWidth,
          winH: window.innerHeight,
          vpW: Math.round(window.visualViewport?.width ?? 0),
          vpH: Math.round(window.visualViewport?.height ?? 0),
          reported: tg?.contentSafeAreaInset?.top ?? -1,
          inset: getComputedStyle(document.documentElement)
            .getPropertyValue("--tg-content-safe-area-inset-top").trim() || "0px",
          backTop,
        }, { sock: ws });
      } catch { /* диагностика не должна мешать работе терминала */ }

      // Файлы, догрузившиеся при мёртвом сокете: байты уже на ПК, вставляем их
      // пути в терминал — иначе загрузка была бы напрасной.
      if (pendingPathsRef.current.length > 0) {
        const queued = pendingPathsRef.current.join(" ");
        try {
          ws.send(new TextEncoder().encode(queued));
          pendingPathsRef.current = []; // чистим ТОЛЬКО после удачной отправки
          showToast(t("pty.uploadPathInserted"));
        } catch { /* сокет умер в этот же момент — путь дождётся следующего onopen */ }
      }
    };

    ws.onmessage = (ev) => {
      markStable(); // any byte from the bridge means the session is real
      lastMsgRef.current = Date.now(); // питает idle-вотчдог (hb или любые данные)
      if (!gotOutputRef.current) {
        gotOutputRef.current = true;
        setShowConnecting(false);
        if (firstByteTimer.current) { clearTimeout(firstByteTimer.current); firstByteTimer.current = null; }
      }
      const term = terminalRef.current;
      if (!term) return;

      if (ev.data instanceof ArrayBuffer) {
        const bytes = new Uint8Array(ev.data);
        termWroteRef.current = true;
        offsetRef.current += bytes.byteLength; // ПРИНЯТО (не «показано», см. appliedOffsetRef)
        if (features.trace) {
          // Горячий путь: подряд идущие кадры сливаются в одну запись. Копия
          // байтов — только при явно включённой записи и с её точной границей.
          const tr = traceRef.current!;
          const rxSeq = tr.noteRx(bytes.byteLength, offsetRef.current, traceCtx());
          const rec = recRef.current!;
          if (rec.enabled) rec.noteRx(bytes, offsetRef.current, tr.time(), rxSeq);
        }
        enqueueTermWrite(bytes, offsetRef.current);
        persistResume(); // по троттлу: экран могут закрыть в любой момент
        lastOutputRef.current = Date.now();
      } else {
        try {
          const msg = JSON.parse(ev.data);
          if (msg.t === "agent-history-capability" && msg.v === 1 && features.agentHistory) {
            setHistoryAvailable(true); return;
          }
          if (msg.t === "agent-history") { historyClientRef.current.accept(msg); return; }
          if (msg.t === "screen-capability") {
            // Агент понимает screen-request-v1: отказы придут screen-none с
            // причиной, кадры — с эхом req (T-36). Без него путь legacy: срок ответа.
            if (msg.v === 1) peerNewRef.current = true;
            if (features.recoveryV1 && msg.v === 1) {
              trace("recovery", { v: 1 }, "capability");
              runRecovery(recovery.capability(recoveryRef.current, true), "capability");
            }
            return;
          }
          if (msg.t === "screen-none") {
            // Честный отказ вместо молчания: not-ready (повтор с backoff),
            // unavailable (кадров не будет до нового соединения), resize-pending
            // (сервер повторит сам с тем же req), invalid-request и незнакомое —
            // как not-ready. Чужой req координатор игнорирует.
            if (features.recoveryV1) {
              const reason = typeof msg.reason === "string" ? msg.reason : "not-ready";
              trace("screen-rx", {
                req: typeof msg.req === "number" ? msg.req : -1,
                frameRev: typeof msg.geom_rev === "number" ? msg.geom_rev : -1,
              }, `none:${reason.slice(0, 24)}`);
              runRecovery(recovery.negative(recoveryRef.current, { req: msg.req, reason }, recoveryNow(), Math.random()), "negative");
            }
            return;
          }
          if (msg.t === "terminal-controls" && features.sizeOwner) {
            const controls = parseTerminalControls(msg);
            if (controls) {
              const controlsRevisionChanged = sizeControlsRef.current?.revision !== controls.revision;
              if (controlsRevisionChanged) recoveryApiRef.current.serverGrid("controls");
              sizeControlsRef.current = controls;
              setSizeControls(controls);
              setSizeControlPending(false);
              if (sizeControlTimerRef.current) clearTimeout(sizeControlTimerRef.current);
              sizeControlTimerRef.current = null;
            }
            return;
          }
          if (msg.t === "reset" || msg.t === "resumed") {
            // Трасса (I-15, волна 8): маркер называет эпоху потока — систему
            // координат offsets. Перебазирование после набора действует только в
            // своей эпохе, досылка уже набранного огрубляется (noteSync); ctx —
            // состояние ДО маркера, как и у прежнего trace("sync").
            const syncSeq = features.trace ? traceRef.current!.noteSync(traceCtx(), {
              epochChanged: typeof msg.epoch === "string" && msg.epoch !== epochRef.current,
              offset: typeof msg.offset === "number" ? msg.offset : -1,
              gap: !!msg.gap, hb: typeof msg.hb === "number" ? msg.hb : 0,
              modes: typeof msg.modes === "string" ? msg.modes.length : 0,
            }, msg.t, {
              epoch: typeof msg.epoch === "string" ? msg.epoch : null,
              offset: typeof msg.offset === "number" ? msg.offset : null,
            }) : -1;
            recordControl(ev.data, syncSeq);
            // A new sync marker supersedes any older snapshot transaction in
            // this connection. Its writer guard will discard stale frame work;
            // release the batching barrier now so the new epoch cannot sit
            // frozen behind the old snapshot watchdog for three seconds.
            snapshotTokenRef.current++;
            if (snapshotBarrierTimerRef.current != null) {
              clearTimeout(snapshotBarrierTimerRef.current);
              snapshotBarrierTimerRef.current = null;
            }
            snapshotBarrierRef.current = false;
            // Заголовок синхронизации от сервера (offset+epoch resume):
            //   reset   → весь scrollback заново: чистим терминал, чтобы снимок
            //             ЗАМЕНИЛ экран, а не дописался снизу (иначе дубль вывода);
            //   resumed → сервер дошлёт только хвост — НЕ чистим, дописываем;
            //   resumed+gap → сервер не смог дослать середину потока (offset
            //             вытеснен из кольца за время обрыва). Раньше это был
            //             полный reset со стиранием экрана — и своя история
            //             пропадала («Codex: скроллится два экрана», 2026-07-29).
            //             Историю сохраняем, пропуск честно помечаем строкой.
            // В обоих случаях запоминаем epoch и базовый offset; дальше offset
            // ведём инкрементами по длине binary-кадров (см. ветку ArrayBuffer).
            // Байты в очереди склейки пришли РАНЬШЕ маркера — спускаем их до
            // reset, иначе стирание экрана обгонит ещё не отрисованный вывод.
            //
            // ⚠ ЖДЁМ ЗАВЕРШЕНИЯ, А НЕ ВЫЗОВА. `flushTermWrites()` без колбэка
            // барьером не является: он возвращает управление, поставив в
            // очередь xterm ОДИН сегмент, а остальные (и стирания истории между
            // ними) допишутся из write-колбэков — то есть уже ПОСЛЕ нашего RIS.
            // Требуемый порядок `OLD-A → ERASE → OLD-B → RIS → MODES` на деле
            // превращался в `OLD-A → RIS → MODES → ERASE → OLD-B`: хвост старой
            // эпохи исполнялся в новой, и это один из главных источников
            // «то продублировалось, то пропало» (повторный аудит, T259-05).
            //
            // Позиции потока (epoch/offset) при этом обновляются СИНХРОННО,
            // ниже: очередь уже снята этим вызовом, а бинарные кадры, пришедшие
            // следом, обязаны считаться от новой базы.
            const isReset = msg.t === "reset";
            // ST-06: удерживать есть что, только если экран уже что-то показал;
            // у девственного терминала держать нечего — лишь отложили бы показ.
            const markerHold = features.presentation && termWroteRef.current;
            // До обновления epochRef ниже: сменил ли маркер эпоху потока (ST-05).
            const markerEpochChanged = typeof msg.epoch === "string" && msg.epoch !== epochRef.current;
            if (isReset) scrollbackEraseGateRef.current.invalidate();
            const syncGeneration = connectionGenRef.current;
            const oldWriterGuard = {
              generation: syncGeneration,
              epoch: writerEpochRef.current,
            };
            // First clear prevents an already deferred old erase from running
            // while the old batch drains. The decisive clear is repeated in the
            // drain callback because preprocessing that batch may re-arm it.
            if (isReset) parserCarryRef.current.clearErase(oldWriterGuard);
            const syncRuntime = afterSyncMarker(msg.t, {
              pageAccum: pageAccumRef.current,
              queuedProbeLines: altScrollQueuedActionsRef.current.length,
            });
            if (syncRuntime.resetEvidence) {
              // Новый экран: якорь, слабые и молчаливые наблюдения и закрепление
              // чтения принадлежали прежнему; доказанный канал — факт о процессе
              // и остаётся даже при отказавшем хранилище (T-03).
              evidenceRef.current.forgetScreen();
              readingPinRef.current = null;
              restoreProbeVerdictRef.current();
            }
            pageAccumRef.current = syncRuntime.pageAccum;
            if (syncRuntime.queuedProbeLines === 0) altScrollQueuedActionsRef.current = [];
            // Every marker changes writerEpoch, so an in-flight probe can no
            // longer produce a valid verdict. Cancel it explicitly; otherwise
            // its early epoch-mismatch return leaves queued fragments behind.
            cancelScrollObservations();
            altScrollProbeTokenRef.current++;
            if (isReset) {
              streamLinesRef.current = 0;
              streamBytesRef.current = 0;
              streamOwnerRef.current = "unknown";
              diagSentRef.current = false;
              diagOwnerRef.current = "";
              pageBudgetRef.current = MAX_PAGES_PER_GESTURE;
              pageBudgetAtRef.current = Date.now();
              if (pageIdleTimerRef.current != null) {
                clearTimeout(pageIdleTimerRef.current);
                pageIdleTimerRef.current = null;
              }
              if (altScrollRafRef.current != null) {
                cancelAnimationFrame(altScrollRafRef.current);
                altScrollRafRef.current = null;
              }
              altScrollAccumRef.current = 0;
              altScrollButtonRef.current = false;
              altScrollInertiaRef.current = false;
            }
            const syncModes = typeof msg.modes === "string" ? msg.modes : "";
            const syncGap = msg.t === "resumed" && !!msg.gap;
            if (typeof msg.epoch === "string" && msg.epoch !== epochRef.current) streamGapRef.current = false;
            if (msg.gap) streamGapRef.current = true;
            const gapNotice = `\r\n\x1b[2m${t("pty.historyGap")}\x1b[0m\r\n`;
            const syncProtocolEpoch = typeof msg.epoch === "string" ? msg.epoch : epochRef.current;
            // This token changes on EVERY marker, even when the server epoch is
            // unchanged (e.g. offset-too-old full reset). It is published before
            // returning from this message handler, so all later binary receives
            // are tagged as future work while the actor still drains the old one.
            const syncWriterEpoch = writerEpochKey(syncProtocolEpoch, ++writerEpochSeqRef.current);
            const syncGuard = { generation: syncGeneration, epoch: syncWriterEpoch };
            // ST-05 (хвост на том же сокете): решение — ДО спуска. Барьер ниже
            // может сработать синхронно (очередь пуста, писатель свободен), и
            // тогда колбэк успел бы выбросить хвост раньше переноса.
            const carryHeldTail = carryTailAcrossMarker({
              flowBacklog: features.flowBacklog, marker: msg.t, gap: syncGap,
              epochChanged: markerEpochChanged, markerOffset: msg.offset, accepted: offsetRef.current,
            });
            flushTermWrites(() => {
              // Записи до маркера разобраны: у трассы кончился шов прежних координат.
              if (syncSeq >= 0) traceRef.current!.noteSyncDrained(syncSeq);
              const live = terminalRef.current;
              if (!live || disposedRef.current) return;
              // Соединение успело смениться, пока разбирался старый вывод —
              // сброс от прошлого сокета стёр бы экран уже новому (T259-08).
              if (connectionGenRef.current !== syncGeneration) return;
              // A partial ESC[3J prefix belongs to the old epoch. Never glue it
              // to the first bytes of the new stream — unless the marker
              // continues the same stream byte for byte (carryHeldTail): then
              // the tail is moved into the new guard right after this flush.
              if (!carryHeldTail) parserCarryRef.current.clearTail(oldWriterGuard);
              parserCarryRef.current.clearGap(oldWriterGuard);
              if (isReset) {
                parserCarryRef.current.clearErase(oldWriterGuard);
                // Old write callbacks may have sampled growth after the marker's
                // eager clear. This is the last point after old drain and before
                // any new guarded write can complete, so it is the authoritative
                // ownership reset boundary.
                streamLinesRef.current = 0;
                streamBytesRef.current = 0;
                streamOwnerRef.current = "unknown";
                diagSentRef.current = false;
                diagOwnerRef.current = "";
                // The marker synchronously changed the offset scale while old
                // callbacks were still outstanding. Reassert only the applied
                // baseline here; accepted offset/queueEnd may already include
                // future binary and must not be rewound.
                if (typeof msg.offset === "number") appliedOffsetRef.current = msg.offset;
              } else if (parserCarryRef.current.hasErase(oldWriterGuard)) {
                // `resumed` keeps the same screen: carry a legitimate deferred
                // erase into the new writer token, but never across full reset
                // or after the effective owner switched to terminal/Output.
                parserCarryRef.current.clearErase(oldWriterGuard);
                if (keepPendingDecision(live, "resumed")) {
                  parserCarryRef.current.markErase(syncGuard);
                }
              }
              terminalWriterRef.current!.transition(syncGuard, () => {
                if (isReset) {
                // Сброс отправляем В ПОТОК последовательностью RIS, а не зовём
                // term.reset(). Разница неочевидная и стоила артефактов: write()
                // только КЛАДЁТ байты в очередь xterm (он разбирает её порциями
                // по 12 мс), а reset() срабатывает немедленно и очереди не
                // касается — то есть только что спущенный вывод дорисовывался
                // ПОВЕРХ уже очищенного экрана. При двух обрывах подряд (лифт,
                // переход Wi-Fi↔LTE) это давало дубль на пол-экрана. RIS встаёт в
                // очередь на своё место, и порядок «допечатать → стереть →
                // показать снимок» соблюдается по-настоящему.
                  // Наш RIS собирает буфер заново: след прежних стираний
                  // (erasedBefore) к новому буферу не относится. historySeq
                  // поднимает сам разборщик (обработчики в эффекте терминала).
                  //
                  // ST-06: на WebGL и непустом экране RIS идёт вместе с BEGIN
                  // (DEC 2026) — холст держит прежнюю картинку, пока разбирается
                  // реплей, и до END хвоста кадра экрана (сторож 1000 мс). Замер
                  // до правки: пустой кадр и ~0,5 с «обрывков». На DOM RIS
                  // чистит строки синхронно — 2026 там только продлил бы пустоту.
                  let markerBytes = "\x1bc";
                  let markerOpened = false;
                  if (features.presentation) {
                    const renderer: PresentationRenderer = markerHold ? rendererRef.current : "dom";
                    markerBytes = planMarkerPrefix({ renderer, isReset: true });
                    // RIS сам закрывает любую открытую транзакцию.
                    presentSet(resetTxn(presentTxnRef.current), "marker-ris");
                    if (markerPrefixOpens({ renderer, isReset: true })) {
                      presentSet(openTxn(TXN_CLOSED, { owner: "marker", token: ++presentTokenRef.current, now: performance.now() }).state, "marker");
                      markerOpened = true;
                    }
                  }
                  const markerToken = presentTokenRef.current;
                  const markerRingCut = ringCutAfterRis({ kind: "marker", base: typeof msg.offset === "number" ? msg.offset : undefined });
                  writeSerial(markerBytes, () => {
                    erasedInGenerationRef.current = false;
                    ringTruncatedRef.current = markerRingCut;
                    // BEGIN маркера разобран: снятый дальше режим — уже не наш.
                    if (markerOpened) presentParsedRef.current = markerToken;
                    if (features.trace) pendingPresentRef.current = { what: "marker", token: markerToken, parsedAt: performance.now() };
                  }, syncGuard);
                } else if (features.presentation) {
                  // resumed: своего RIS нет — удержание прежней картинки снимаем.
                  presentDrop("marker-resumed", syncGuard);
                }
              // Синхронизация DEC-режимов (alt-screen/mouse/bracketed-paste):
              // на reset — reassert активных (их включающая последовательность
              // вытеснена из буфера, иначе проброс прокрутки в full-screen TUI
              // молча ломается), на gap — полный SET+RESET сервера (в пропущенной
              // середине режимы могли переключиться в любую сторону). Эти байты
              // НЕ влияют на offsetRef (он растёт только по binary-кадрам).
                if (syncModes) writeSerial(syncModes, undefined, syncGuard);
                if (syncGap) {
                // Пометка ПОСЛЕ режимов: если gap вывел нас из застрявшего
                // alt-screen, строка должна лечь в нормальный буфер, а не в
                // брошенный альтернативный.
                  writeSerial(gapNotice, undefined, syncGuard);
                }
                // ST-09: граница эпохи (RIS, режимы, пометка пропуска стоят
                // перед этой отметкой в той же очереди) отдана xterm — с этого
                // места экран описывается базой маркера, даже если вывод новой
                // эпохи ещё ни одной записью не ушёл в xterm. Пустая lazy-запись
                // в xterm ничего не пишет.
                if (features.flowBacklog && typeof msg.offset === "number") {
                  const markerBase = msg.offset;
                  terminalWriterRef.current!.lazy(() => {
                    handedMarkRef.current = noteHanded(handedMarkRef.current, syncGuard, markerBase);
                    return null;
                  }, { guard: syncGuard });
                }
              });
            });
            // ST-05 (хвост на том же сокете): resumed без пропуска посреди
            // соединения продолжает поток ровно с принятого (resync отстающего
            // зрителя, T-35) — удержанное начало ESC-последовательности
            // переезжает в новую эпоху писателя, а не выбрасывается (иначе
            // «[31m» текстом и потерянный D OSC 133). Синхронно: вызов выше
            // уже разобрал очередь старой эпохи и сложил её хвост (в очереди
            // на маркере записи только текущего guard), а спуск новой эпохи
            // ещё не начинался — он заберёт хвост первым же takeTail(syncGuard).
            if (carryHeldTail) {
              const held = parserCarryRef.current.takeTail(oldWriterGuard);
              if (held.byteLength > 0) parserCarryRef.current.storeTail(syncGuard, held);
            }
            cancelGestureRef.current();
            writerEpochRef.current = syncWriterEpoch;
            if (typeof msg.epoch === "string") {
              // Смена эпохи = другая сессия потока: измеренная глубина истории
              // к ней не относится (внешний разбор 2.57.9, T259-11.1). Замер
              // потока — тем более: там уже другое приложение.
              if (msg.epoch !== epochRef.current) {
                snapHistRef.current = -1;
                streamLinesRef.current = 0;
                streamBytesRef.current = 0;
                streamOwnerRef.current = "unknown";
              }
              epochRef.current = msg.epoch;
              // Эпоха входит в область свидетельств (ST-02, волна 4) — та, что
              // ПОДТВЕРЖДЕНА маркером этой страницы, а не сохранённая позиция
              // resume (сервер мог начать новую). Новая — прежние наблюдения
              // чужие; первая подтверждённая — только теперь можно вспомнить
              // приговор этой области. Тёплый resumed в ту же эпоху область не меняет.
              if (msg.epoch !== confirmedEpochRef.current) {
                confirmedEpochRef.current = msg.epoch;
                restoreProbeVerdictRef.current();
              }
            }
            if (typeof msg.offset === "number") {
              // Обе позиции разом: сервер назвал базу потока, и всё до неё уже
              // либо показано, либо больше не придёт.
              offsetRef.current = msg.offset;
              appliedOffsetRef.current = msg.offset;
              queueEndRef.current = msg.offset;
            }
            // SOTA-снапшот: запоминаем, какой это был маркер. reset разрешает
            // кадру применить присланную историю (терминал стёрт RIS'ом выше),
            // resumed запрещает (своя история валидна) — КРОМЕ свежей страницы,
            // которая пришла с resume из localStorage и ещё ничего не печатала:
            // у неё истории нет, и снапшот — единственный источник прокрутки
            // (боевой скриншот 13.08: «вот тут не скролит», scrollback пуст) —
            // ptyTerm/snapshotHistory.ts.
            snapshotHistoryRef.current = noteSyncMarker(msg.t, !termWroteRef.current);
            if (features.recoveryV1) {
              // ST-05: маркер называет путь восстановления (T-28). Новая эпоха
              // writer делает запрос и кадр прежней неактуальными (T-25), а
              // причины маркера уходят в набор координатора:
              //   первый reset соединения — базовая точка (open): терминал
              //     стёрт RIS, кадр и история восстанавливают экран;
              //   resumed в пустой xterm — cold-restore: один offset не
              //     восстанавливает ни экран, ни режимы, ни историю;
              //   resumed+gap — stream-gap; reset посреди соединения — epoch-reset;
              //   resumed без пропуска на живой странице — warm: ничего.
              const firstMarker = !markerSeenRef.current;
              markerSeenRef.current = true;
              runRecovery(recovery.epoch(recoveryRef.current, syncWriterEpoch, recoveryNow()), "marker");
              // Холодная страница, которую обрыв прервал до кадра, уже держит
              // хвост прежнего сокета, но восстановленной не считается (T-28):
              // coldPage координатора переживает соединение.
              // Ревью 15.09 (дефект 1): тёплому resumed — один кадр за соединение
              // ради hist_lines, если следующее холодное открытие по сохранённой
              // глубине будет реплеем, а поток с прошлого кадра ушёл в прокрутку.
              const histProbe = histProbeConnRef.current !== connectionGenRef.current
                && shouldProbeHistory(snapHistRef.current, linesSinceFrameRef.current);
              const route = recovery.causesForMarker({
                marker: msg.t, gap: syncGap, epochChanged: markerEpochChanged,
                termVirgin: !termWroteRef.current || recoveryRef.current.coldPage, firstMarker, histProbe,
              });
              if (route.path === "hist-probe") histProbeConnRef.current = connectionGenRef.current;
              // Тёплый без кадра: дельта обрыва ещё впереди — решит первая её строка.
              else if (route.path === "warm") histProbeWatchRef.current = connectionGenRef.current;
              if (msg.t === "reset") runRecovery(recovery.streamReset(recoveryRef.current), "stream-reset");
              trace("recovery", { first: firstMarker, gap: syncGap, epochChanged: markerEpochChanged }, `marker:${route.path}`);
              const causes: RecoveryCause[] = route.path === "baseline" ? ["open"] : route.causes;
              for (const cause of causes) demandRecovery(cause);
            }
            // Сразу на диск: это точка, где эпоха может смениться (сессию
            // пересоздали), и старая запись стала бы враньём.
            persistResume(true);
            // Сервер объявил период heartbeat → включаем idle-вотчдог (2.5×hb).
            // Нет поля (старый агент) → выключаем, чтобы не убивать живой idle.
            hbLimitRef.current = typeof msg.hb === "number" && msg.hb > 0 ? msg.hb * 2500 : 0;
          } else if (msg.t === "screen" && typeof msg.screen === "string" && msg.screen) {
            recordControl(ev.data, trace("screen-rx", {
              frameRev: typeof msg.geom_rev === "number" ? msg.geom_rev : -1,
              base: typeof msg.base_offset === "number" ? msg.base_offset : -1,
              snapCols: typeof msg.screen_cols === "number" ? msg.screen_cols : -1,
              snapRows: typeof msg.screen_rows === "number" ? msg.screen_rows : -1,
              histLines: typeof msg.hist_lines === "number" ? msg.hist_lines : -1,
              req: typeof msg.req === "number" ? msg.req : -1,
            }, "frame"));
            // The screen may have been captured before a local fit/rotation but
            // arrive after it. Reject it BEFORE taking the snapshot barrier or
            // touching logicalRows/logicalCols/history/xterm: otherwise stale
            // state.cols can classify it as authoritative and resize the same
            // viewer straight back to its old grid.
            const currentGeometryRevision = geometryRevisionRef.current;
            // ST-05: номер операции координатора, на которую отвечает кадр.
            // null — прежний путь или кадр, отданный сервером без нашей просьбы
            // (apply:unsolicited): такой пишется, но операцией не считается.
            let recoveryTicket: number | null = null;
            // ST-08: какому запросу координатор приписал кадр — номер (apply),
            // null (кадр без просьбы) или undefined (координатора нет, откат).
            let frameTicket: number | null | undefined;
            /** Исход операции: записан (ok) или нет. Один раз на кадр. */
            const settleRecovery = (ok: boolean) => {
              if (recoveryTicket === null) return;
              const ticket = recoveryTicket;
              recoveryTicket = null;
              runRecovery(recovery.applied(recoveryRef.current, ticket, ok, recoveryNow(), Math.random()),
                ok ? "applied" : "not-applied");
            };
            if (features.recoveryV1) {
              // Один вердикт ДО барьера и до любых правок сетки/истории:
              // чужое соединение, эпоха, сетка, чужой или вытесненный запрос,
              // устаревшая база (T-25). Отброшенный кадр не меняет ни экран, ни
              // offset, ни операцию в пути; повтор, если нужен, уже поставил
              // сам координатор.
              syncRecoveryGeometry();
              const verdict = recovery.frame(recoveryRef.current, {
                conn: connectionGenRef.current,
                epoch: writerEpochRef.current,
                geomRev: msg.geom_rev,
                req: msg.req,
                base: msg.base_offset,
              }, appliedOffsetRef.current, recoveryNow(), Math.random());
              runRecovery(verdict, "frame");
              frameTicket = verdict.verdict === "apply" ? (verdict.ticket ?? null) : null;
              if (verdict.verdict === "apply") {
                recoveryTicket = verdict.ticket ?? null;
              } else if (verdict.verdict !== "apply:unsolicited") {
                trace("snap-reject", {
                  frameRev: typeof msg.geom_rev === "number" ? msg.geom_rev : -1,
                  req: typeof msg.req === "number" ? msg.req : -1,
                }, verdict.verdict);
                if (verdict.verdict === "discard:geometry") {
                  sendDiag("snapshot-geometry-stale", {
                    frame_rev: msg.geom_rev,
                    client_rev: currentGeometryRevision,
                  }, { sock: ws });
                } else if (verdict.verdict === "retry:stale-base") {
                  sendDiag("snapshot-stale", {
                    base: typeof msg.base_offset === "number" ? msg.base_offset : undefined,
                    accepted: offsetRef.current,
                    applied: appliedOffsetRef.current,
                  }, { sock: ws });
                }
                return;
              }
            } else if (!screenGeometryRevisionMatches(msg.geom_rev, currentGeometryRevision)) {
              const retry = geometryRetryRevisionRef.current !== currentGeometryRevision;
              trace("snap-reject", { frameRev: typeof msg.geom_rev === "number" ? msg.geom_rev : -1, retry }, "geometry");
              sendDiag("snapshot-geometry-stale", {
                frame_rev: msg.geom_rev,
                client_rev: currentGeometryRevision,
              }, { sock: ws });
              if (retry) {
                geometryRetryRevisionRef.current = currentGeometryRevision;
                // requestScreenFrame itself waits for geometry/layout to settle
                // and reads the revision only when its timer actually sends.
                requestScreenFrameRef.current(ws, "geometry-stale");
              }
              return;
            }
            // ГОТОВЫЙ КАДР ЭКРАНА от компьютера. Без него экран агента
            // восстановить невозможно в принципе: Claude Code, Codex и Gemini
            // рисуют диффом ПО ЯЧЕЙКАМ, и полного кадра в потоке нет вовсе
            // (замер: из хвоста восстанавливалось 2,5 % ячеек — отсюда «пустой
            // экран с обрывками»).
            //
            // Кадр приходит ПОСЛЕ уже отрисованного хвоста и переписывает
            // картинку целиком — он самодостаточен (вход в alt-screen, очистка,
            // все строки, позиция курсора).
            //
            // ⚠ Спуск очереди — ЧЕРЕЗ БАРЬЕР, а не вызовом «до кучи».
            // flushTermWrites возвращает управление, поставив в очередь xterm
            // только первый сегмент: остальные (и стирания истории между ними)
            // дописываются из write-колбэков. Синхронное «RIS → история →
            // кадр» следом обгоняло их, и старый вывод исполнялся ПОВЕРХ
            // восстановленного — вплоть до ESC[3J, стиравшего свежий scrollback
            // (внешний аудит 13.08.2026, находка T-002). Теперь снапшот
            // применяется в колбэке, когда весь прежний вывод разобран, а
            // свежие байты на это время ждут (snapshotBarrierRef).
            snapshotBarrierRef.current = true;
            // Своя транзакция у каждого кадра: колбэки прошлой становятся
            // пустышками, даже если их запись доедет позже (T259-08).
            const snapToken = ++snapshotTokenRef.current;
            const snapGeneration = connectionGenRef.current;
            const snapWriterEpoch = writerEpochRef.current;
            const snapGuard = { generation: snapGeneration, epoch: snapWriterEpoch };
            const snapStale = () => snapToken !== snapshotTokenRef.current
              || snapGeneration !== connectionGenRef.current
              || snapWriterEpoch !== writerEpochRef.current;
            const histField = typeof msg.history === "string" ? msg.history : "";
            const screenField = msg.screen;
            const histN = msg.hist_lines;
            const snapCols = msg.screen_cols;
            const snapRows = msg.screen_rows;
            // Позиция потока, которую кадр уже содержит (агент с 2.57.16).
            const snapBase = typeof msg.base_offset === "number" ? msg.base_offset : undefined;
            // Наш RIS кадра с заменой оставит только историю зеркала (I-11).
            const snapRingCut = ringCutAfterRis({
              kind: "frame", histLines: typeof histN === "number" ? histN : undefined,
              screenRows: typeof snapRows === "number" ? snapRows : undefined, base: snapBase,
            });
            // ⚠ КАДР ОБЯЗАН БЫТЬ НАПИСАН ВСЕГДА. Он и есть картинка экрана: у
            // агентских CLI полного кадра в самом потоке нет вовсе (рисуют
            // диффом по ячейкам), и пропустить его — значит показать человеку
            // пустой или вчерашний экран. Поэтому ни одна ветка ниже не
            // выходит раньше writeFrame, а сторож не просто снимает барьер, а
            // ДОПИСЫВАЕТ кадр, если до него почему-то не дошли.
            let framed = false;
            // ST-06: голова снимка записана — хвост закрывает её транзакцию.
            let snapBegun: SnapshotTxnHead | null = null;
            let snapFiller = "";
            let snapTailRis = false;
            /** Кадр писать не будем: своё удержание (если есть) снимаем. */
            const abandonSnapPresent = (why: string) => {
              const token = snapBegun?.closeToken;
              snapBegun = null;
              if (token != null) presentAbandon(token, why);
            };
            const writeFrameOnce = () => {
              if (framed) return;
              framed = true;
              // ⚠ СТАРЫЙ КАДР НЕ ПИШЕМ. Он снят на позиции потока раньше той,
              // что человек уже видит: применить его — значит откатить картинку
              // назад, а откаченные байты второй раз не придут. Экран при этом
              // не пустой (у нас есть более свежий вывод), поэтому правило
              // «кадр обязан быть написан всегда» здесь не нарушается — оно про
              // случай, когда показывать больше нечего.
              //
              // ⚠ «ВИДИТ» — это appliedOffset, а не offset. До 2.61.16 сравнивали
              // с ПРИНЯТЫМ: байты, пришедшие после кадра, уже посчитаны, но
              // лежат за барьером и на экран не попали. У Claude Code в простое
              // ~10 сообщений в секунду, и пока очередь до кадра разбиралась,
              // ещё пара таких успевала прийти — кадр объявлялся устаревшим и
              // выбрасывался, хотя ничего новее него человек не видел. Боевой лог
              // 01.09.2026: 62 таких вердикта на ~300 открытий (каждое пятое),
              // а следом сервер ещё и глотал повторную просьбу. Итог — чёрный
              // экран со строкой спиннера до следующего открытия терминала.
              // Настоящая устарелость (кадр из «догона», T259-07) по applied
              // видна так же: те байты уже НАРИСОВАНЫ, applied ушёл за базу.
              if (frameIsStale(snapBase, appliedOffsetRef.current)) {
                trace("snap-reject", { base: snapBase ?? -1 }, "base-stale");
                sendDiag("snapshot-stale", {
                  base: snapBase, accepted: offsetRef.current,
                  applied: appliedOffsetRef.current,
                });
                releaseSnapshotBarrier(snapToken);
                abandonSnapPresent("frame-stale");
                if (!features.recoveryV1) {
                  requestScreenFrameRef.current(undefined, "base-stale");
                } else if (recoveryTicket !== null) {
                  // Устарел уже ПОСЛЕ разбора очереди (кадр из «догона»):
                  // причина frame-stale и «не записан» — повтор с backoff и
                  // пределом решает координатор, а не свой таймер.
                  demandRecovery("frame-stale");
                  settleRecovery(false);
                }
                return;
              }
              const term2 = terminalRef.current;
              // Кадр устаревшей транзакции писать НЕЛЬЗЯ: за время разбора
              // очереди соединение могло смениться, и он лёг бы поверх свежего
              // вывода уже другой сессии потока.
              if (!term2 || disposedRef.current || snapStale()) {
                trace("snap-reject", { token: snapToken }, "stale-token");
                releaseSnapshotBarrier(snapToken);
                settleRecovery(false);
                abandonSnapPresent("stale-token");
                return;
              }
              termWroteRef.current = true; // кадр/история — уже напечатанный вывод
              trace("snap-apply", { token: snapToken, frameChars: screenField.length }, "frame-write");
              if (recoveryTicket !== null) {
                runRecovery(recovery.applying(recoveryRef.current, recoveryTicket), "applying");
              }
              // ST-06: хвост — досылка + кадр + END той транзакции, что осталась
              // нашей, ОДНОЙ записью. Флаг выключен — кадр как был.
              let tail: string | Uint8Array = screenField;
              const tailRis = snapTailRis;
              if (snapBegun) {
                const done = finishSnapshotTxn(snapBegun, {
                  filler: snapFiller, frame: screenField, modeOn: term2.modes.synchronizedOutputMode,
                });
                snapBegun = null;
                tail = done.chunk;
                presentSet(done.txn, "snapshot-tail");
              } else if (features.presentation && presentHolding()) {
                // Кадр мимо плана (сторож барьера): удержание снимаем тем же write.
                const begun = beginSnapshotTxn({
                  renderer: rendererRef.current, modeOn: term2.modes.synchronizedOutputMode,
                  txn: presentTxnRef.current, token: ++presentTokenRef.current, now: performance.now(),
                });
                const done = finishSnapshotTxn(begun, { frame: screenField, modeOn: term2.modes.synchronizedOutputMode });
                tail = done.chunk;
                presentSet(done.txn, "frame");
              }
              writeTracked(tail, () => {
                // RIS, перенесённый в хвост (истории нет), собрал буфер заново.
                if (tailRis) {
                  erasedInGenerationRef.current = false;
                  ringTruncatedRef.current = snapRingCut;
                }
                releaseSnapshotBarrier(snapToken);
                // Разобран: причины запроса закрыты (кроме поднятых этим же
                // кадром — чужая высота/ширина прогрессом не считается).
                settleRecovery(true);
                if (features.trace) pendingPresentRef.current = { what: "snapshot", token: snapToken, parsedAt: performance.now() };
              }, snapGuard, null);
            };
            // Сторож: если колбэк не придёт (терминал умирает, запись выбросила
            // исключение), кадр всё равно уходит, а барьер падает — иначе вывод
            // замрёт молча и человек останется без экрана.
            if (snapshotBarrierTimerRef.current != null) clearTimeout(snapshotBarrierTimerRef.current);
            snapshotBarrierTimerRef.current = setTimeout(() => {
              snapshotBarrierTimerRef.current = null;
              trace("snap-watchdog", { token: snapToken, framed });
              writeFrameOnce();
            }, 3000);
            flushTermWrites(() => {
              const live = terminalRef.current;
              if (!live || disposedRef.current || snapStale()) {
                trace("snap-reject", { token: snapToken }, "stale-token");
                releaseSnapshotBarrier(snapToken);
                settleRecovery(false);
                return;
              }
              // SOTA-снапшот (replay полной заменой, модель VS Code): у кадра
              // может быть поле history — строки scrollback от теневого буфера
              // сервера, каждая со SGR-диффами, разделитель \r\n. Применяем
              // только на reset-пути и один раз за соединение (правило —
              // ptyTerm/snapshotHistory.ts): на resumed своя история валидна, и
              // чужая продублировала бы её; кадру по запросу из фона (маркера в
              // соединении не было) история не положена — терминал не пуст.
              // ── ГЕОМЕТРИЯ КАДРА ──────────────────────────────────────────
              // Кадр адресует строки АБСОЛЮТНО, поэтому «просто применить» его
              // можно только в своей же высоте. Если он выше нашего терминала,
              // строки за краем зажимаются последней и складываются в неё —
              // 24 снимка из 200 в боевом логе, из них 12 катастрофически
              // (кадр 30–31 строка в терминал 9–11). Виновата не сеть, а мы:
              // `fit()` при поднятой клавиатуре ужимал ЛОГИЧЕСКУЮ геометрию до
              // видимой. Правило — ptyTerm/geometry.ts.
              if (typeof snapRows === "number" && snapRows >= 2) {
                logicalRowsRef.current = snapRows;
              }
              // ST-08 (T-32): кадр — ОТВЕТ на наш запрос, ушедший ПОСЛЕ доставки
              // нашей вместимости в этот сокет: он снят в min(всех зрителей,
              // включая нас), это сетка PTY по обеим осям. Ответ — только кадр,
              // приписанный координатором запросу в пути, или (без координатора)
              // первый совпавший; кадр без просьбы ничего не доказывает
              // (resizePolicy.frameAnswersRequest). Без переключателя — false.
              // Правило целиком — resizePolicy.frameAnswersDeliveredRequest
              // (ответ на наш запрос + доставлено + не больше вместимости);
              // решение ДО пометки answered: у отката без координатора ответ
              // по первому совпавшему кадру иначе был бы уже израсходован.
              const requestNote = lastScreenRequestRef.current;
              const frameIdentity = { socket: ws, geomRev: msg.geom_rev, req: msg.req, ticket: frameTicket };
              const frameDelivered = features.capacity
                && frameAnswersDeliveredRequest(requestNote, frameIdentity, { cols: snapCols, rows: snapRows });
              if (features.capacity && requestNote && frameAnswersRequest(requestNote, frameIdentity)) requestNote.answered = true;
              const geo0 = frameGeometryAction({
                snapCols: typeof snapCols === "number" ? snapCols : undefined,
                snapRows: typeof snapRows === "number" ? snapRows : undefined,
                termCols: live.cols,
                termRows: live.rows,
                keyboardOpen: keyboardOpenRef.current,
                authoritativeCols: logicalColsRef.current,
                authoritativeRows: authoritativeRowsRef.current,
                capacityDelivered: frameDelivered,
              });
              let geo = geo0;
              if (geo0.kind === "adopt") {
                // ST-08: принятая сетка авторитетна и по высоте — иначе
                // следующий fitLocal вернул бы прежнюю (authoritativeRows
                // знал только /state). reportSizeRef НЕ трогаем (I-10).
                if (features.capacity) {
                  authoritativeRowsRef.current = geo0.rows;
                  logicalRowsRef.current = geo0.rows;
                  // Сетка из кадра живёт до первого /state, спрошенного после
                  // неё (reconcileAdoptedGrid): PTY общий и может смениться
                  // так, что значение /state не изменится вовсе.
                  gridAdoptedAtRef.current = performance.now();
                }
                // Кадр снят в АВТОРИТЕТНОЙ ширине PTY (state.cols), а наш экран
                // шире: рядом открыт более узкий зритель, и сервер держит общий
                // PTY по минимуму. Воевать за свою ширину бессмысленно — кадр
                // в 48 придёт снова. Принимаем авторитетную сетку ЛОКАЛЬНО, не
                // сообщая её компьютеру (ему по-прежнему уходит наша измеренная
                // — см. reportSizeRef), и пишем кадр как родной. Узкий зритель
                // уйдёт — следующий /state поднимет зажатие (эффект state.cols).
                logicalColsRef.current = geo0.cols;
                const wasSize = `${live.cols}x${live.rows}`;
                // onResize → syncGeometry синхронно, внутри resize: сетку меняет
                // этот кадр, операцию он не отменяет (recovery.adopted).
                adoptTicketRef.current = recoveryTicket;
                try { live.resize(geo0.cols, geo0.rows); } catch { /* ignore */ } finally { adoptTicketRef.current = null; }
                applyKeyboardPeekRef.current();
                setTermSize({ cols: live.cols, rows: live.rows });
                sendDiag("snapshot-adopt", {
                  snap: `${snapCols ?? "?"}x${snapRows ?? "?"}`,
                  was: wasSize, client: `${live.cols}x${live.rows}`,
                  geom_rev: typeof msg.geom_rev === "number" ? msg.geom_rev : null,
                  // Почему принята: доставленная вместимость или известная
                  // авторитетная сетка /state (метка, без содержимого, I-15).
                  ...(features.capacity ? { reason: frameDelivered && !keyboardOpenRef.current ? "delivered" : "authoritative" } : {}),
                });
                geo = { kind: "apply" };
              }
              if (geo.kind === "restore") {
                // Возвращаем СВОЮ логическую высоту — локально, не сообщая
                // компьютеру: его размер верен, ошибались мы. Видимое окно не
                // меняется, низ показывает сдвиг (applyKeyboardPeek).
                // Сетка — под этот же кадр: операцию не отменяет (как adopt выше).
                adoptTicketRef.current = recoveryTicket;
                try { live.resize(geo.cols, geo.rows); } catch { /* ignore */ } finally { adoptTicketRef.current = null; }
                applyKeyboardPeekRef.current();
                setTermSize({ cols: live.cols, rows: live.rows });
              } else if (geo.kind === "resync" && !geo.apply) {
                // Кадр снят в ЧУЖОЙ ШИРИНЕ: писать его — значит своими руками
                // сделать кашу (перенос строк и вся сетка посчитаны под другую
                // ширину). Экран не пустеет: живой поток на месте, а свежий
                // кадр придёт следом (боевое 14.08: `снапшот=48x30
                // логический=232x40`).
                framed = true; // ни одна ветка ниже кадр уже не напишет
                sendResize();
                // ⚠ ТРОТТЛ. Компьютер отдаёт кадр в геометрии PTY, а PTY идёт
                // по самому УЗКОМУ зрителю: если рядом открыт экран у́же нашего,
                // ширина не сойдётся никогда, и «отверг → попроси новый» стало
                // бы бесконечным кругом. Просим не чаще раза в пять секунд.
                if (features.recoveryV1) {
                  // Тот же троттл живёт в координаторе (WIDTH_MIN_INTERVAL_MS,
                  // затем degraded), а кадр не записан: операция не удалась.
                  demandRecovery("geometry-width");
                  settleRecovery(false);
                } else {
                  const at = Date.now();
                  if (at - geomResyncAtRef.current > GEOM_RESYNC_GAP_MS) {
                    geomResyncAtRef.current = at;
                    requestScreenFrameRef.current(undefined, "width");
                  }
                }
                // ⚠ НЕ «snapshot-stale»: расхождение ширины — это геометрия,
                // а не устаревшая позиция, и в разборе их путали (внешний
                // аудит 2.57.18, P1: разделять причины resync).
                trace("snap-reject", {
                  snapCols: typeof snapCols === "number" ? snapCols : -1, cols: live.cols, auth: logicalColsRef.current || 0,
                }, "width");
                sendDiag("snapshot-width", {
                  snap: `${snapCols ?? "?"}x${snapRows ?? "?"}`,
                  client: `${live.cols}x${live.rows}`,
                  auth: logicalColsRef.current || 0,
                });
                releaseSnapshotBarrier(snapToken);
                return;
              } else if (geo.kind === "resync") {
                // Расхождение не клавиатурное: наш размер честный, а кадр снят
                // в чужой геометрии. Кадр всё равно напишем (пустой экран хуже
                // неточного), но назовём свой размер и попросим свежий.
                //
                // ⚠ Размер называем ОДИН раз и даём дебаунсу дожить. Раньше
                // каждый кадр в чужой высоте (они шли ~раз в секунду, пока
                // геометрия не сойдётся) заново взводил двухсекундный таймер
                // resizePolicy, и resize не уходил НИКОГДА: живой замер
                // 03.09.2026 на ПК владельца — первый размер 154×29 ушёл в
                // первые 150 мс, до того как под терминалом появились ряды
                // кнопок, итоговая сетка 154×24, а компьютер так и держал 29
                // строк. Кадр в 29 строк складывался в 24: нижние пять строк
                // любого вывода «пропадали», а в прокрутке на шве истории и
                // экрана эти же пять строк шли дважды. На телефоне (18 строк)
                // терялись одиннадцать.
                if (!resizeQuietTimer.current) sendResize();
                // Запрос кадра здесь НЕ троттлим (в отличие от ветки ширины):
                // проба keyboard geometry ждёт кадр после поднятия клавиатуры,
                // и на стенде его источник — именно этот повтор. Пока размер
                // не сошёлся, кадр идёт ~раз в секунду; после resize геометрия
                // совпадает, и повтор прекращается сам.
                // Координатор (ST-05): причина geometry-height. Кадр, тут же её
                // поднявший, прогрессом не считается, поэтому повтор ограничен
                // (MAX_NO_PROGRESS, затем degraded), а подтверждённая сервером
                // сетка (/state, terminal-controls → serverGrid) даёт ещё попытку.
                if (features.recoveryV1) demandRecovery("geometry-height");
                else requestScreenFrameRef.current(undefined, "resync");
              }
              // ⚠ КАДР ПРИМЕНЯЕТСЯ КАК ЕСТЬ — БЕЗ ЕДИНОГО РЕСАЙЗА ПОД НЕГО.
              //
              // Первая версия делала «как VS Code»: ресайз в геометрию кадра,
              // запись, ресайз обратно (forceExactSize). У них это локальная
              // операция, а у НАС размер терминала на телефоне управляет
              // размером PTY на компьютере — и каждый такой ресайз заставлял
              // Claude Code перерисовать весь экран и СБРАСЫВАЛ прокрутку.
              // Живой лог 12.08 21:24: `48x30 → 48x28`, кадр, через три секунды
              // `48x28 → 48x30`. А в 20:57 кадр успел сняться до того, как
              // телефон домерил высоту: `48x30 → 48x10`, кадр в десять строк,
              // экран схлопнулся — снаружи «зависло и ничего не выводит». Приём
              // вернул качели размеров, которые проект лечил месяцами
              // (targetSizeLocked, resizePolicy.ts).
              //
              // Разница ВЫСОТЫ кадру безразлична — строки в нём адресованы
              // абсолютно. Для ИСТОРИИ это неверно, и она подгоняется
              // арифметикой ниже, тоже без ресайза.
              // Отвергнутый выше кадр чужой ширины не расходует возможность
              // восстановить историю. Иначе первый кадр во время домера
              // экрана оставлял свежую страницу без прокрутки навсегда.
              // ⚠ СВОЮ прокрутку мерим по НОРМАЛЬНОМУ буферу, а не по активному:
              // у alt-screen baseY ноль по определению, и полноэкранный TUI
              // выглядел бы терминалом без истории — то есть у него бы её и
              // отобрали. И мерим ЗДЕСЬ, после барьера: до него значение ещё
              // старое, потому что прежний вывод не разобран.
              const localScrollback = ((live.buffer as unknown as {
                normal?: { baseY?: number };
              }).normal?.baseY) ?? live.buffer.active.baseY;
              // ПЛАН ПРИМЕНЕНИЯ — одно правило с стендом эквивалентности
              // (ptyTerm/snapshotApply.ts, §6): takeSnapshotHistory,
              // scrollbackLineCount, shouldReplaceHistory, takeScrollbackLines,
              // stripScrollbackErase. Решение по истории снимается ВСЕГДА (даже
              // без замены — готовность соединения расходуется), байты и
              // порядок шагов прежние.
              const historyStateBefore = snapshotHistoryRef.current;
              const plan = planSnapshotApply({
                history: histField,
                histLines: histN,
                screenRows: snapRows,
                snapCols,
                termCols: live.cols,
                localScrollback,
                historyState: historyStateBefore,
              });
              snapshotHistoryRef.current = plan.nextHistoryState;
              // Историю разрешено применить ровно тогда, когда решение
              // израсходовало готовность (ready → done).
              const histApply = plan.nextHistoryState !== historyStateBefore;
              const sbLines = plan.serverScrollback;
              // Запоминаем глубину истории этой сессии: по ней СЛЕДУЮЩЕЕ
              // холодное открытие решит, хватит ли снапшота вместо реплея.
              // Пишем и когда снапшот отвергнут, и когда истории нет вовсе —
              // именно бедное зеркало и есть то, что надо запомнить.
              snapHistRef.current = Math.max(0, sbLines);
              linesSinceFrameRef.current = 0; // глубина свежая: рост считаем от этого кадра
              persistResume(true);
              const replace = plan.replace;
              // Диагностика решения — числами и без единого символа вывода.
              // Именно её не хватало, когда владелец спросил «почему в Kimi вижу
              // всё, а в Claude только часть»: ответ пришлось собирать из логов
              // агента и живых сессий вручную.
              // ⚠ ГЕОМЕТРИИ НАЗЫВАЮТСЯ РАЗДЕЛЬНО. Прежнее поле `client=48x11`
              // смешивало логический терминал с видимым окном и подталкивало к
              // неверному выводу — будто компьютер прислал кадр не того
              // размера. На деле терминал был тридцатистрочным, а ужимали себя
              // мы сами. Теперь в логе видно и то, и другое, и что решили.
              const peekPx = (() => {
                const box = termRef.current;
                const v = box?.style.getPropertyValue("--pty-peek") || "0px";
                return parseInt(v, 10) || 0;
              })();
              trace("snap-apply", {
                token: snapToken, replace, histApply, sbLines, local: localScrollback,
                cols: live.cols, rows: live.rows, keyboard: keyboardOpenRef.current,
              }, geo.kind);
              sendDiag("snapshot", {
                server: sbLines, local: localScrollback, replace,
                snap: `${snapCols ?? "?"}x${snapRows ?? "?"}`,
                client: `${live.cols}x${live.rows}`,
                logical: `${live.cols}x${live.rows}`,
                visible: `${live.cols}x${visibleRows(live, peekPx)}`,
                keyboard: keyboardOpenRef.current,
                action: geo.kind,
                // Ревизия запроса, на который ответил применённый кадр:
                // без неё сбой CI «4→7» (A05) разобрать было нечем.
                geom_rev: typeof msg.geom_rev === "number" ? msg.geom_rev : null,
                // ST-08: кадр ответил на запрос после доставленной вместимости.
                ...(features.capacity ? { delivered: frameDelivered } : {}),
              });
              // Шаги плана исполняются теми же writeSerial/writeTracked, под тем
              // же барьером и теми же snapGuard/snapStale:
              //   без замены — только кадр: своя прокрутка не хуже присланной
              //     (сброс стоил бы человеку всей истории Claude Code, её
              //     источник — реплей кольца, а не зеркало); кадр — ВСЕГДА;
              //   с заменой — RIS В ОЧЕРЕДЬ (term.reset() обгонял бы
              //     неразобранные байты), затем ТОЛЬКО scrollback-часть истории
              //     (первые hist_lines − screen_rows строк, historySeam.ts,
              //     T-001/T-019) мимо keepHistory-политики — готовые строки
              //     теневого буфера, аномальный ESC[3J в них вырезан, хвост
              //     начала последовательности НЕ доклеен; затем LF по ЗАМЕРУ
              //     живого буфера после её разбора (перенос длинных строк,
              //     ширина emoji Go/xterm); затем кадр.
              // history — текстовое сообщение, в offsetRef не входит (как modes
              // и сам кадр): инвариант offset сохранён.
              const runSnapshotSteps = (steps: readonly SnapshotStep[], from: number): void => {
                for (let i = from; i < steps.length; i++) {
                  const snapStep = steps[i];
                  if (snapStep.kind === "ris") {
                    // null: кадр экрана — позиция потока экран больше не описывает (ST-09).
                    writeSerial(SNAPSHOT_RIS, () => {
                      erasedInGenerationRef.current = false;
                      ringTruncatedRef.current = snapRingCut;
                    }, snapGuard, null);
                  } else if (snapStep.kind === "history") {
                    writeTracked(snapStep.bytes, () => {
                      if (!terminalRef.current || disposedRef.current || snapStale()) {
                        releaseSnapshotBarrier(snapToken);
                        settleRecovery(false);
                        return;
                      }
                      runSnapshotSteps(steps, i + 1);
                    }, snapGuard, null);
                    return;
                  } else if (snapStep.kind === "filler") {
                    const term2 = terminalRef.current;
                    if (!term2) { releaseSnapshotBarrier(snapToken); settleRecovery(false); return; }
                    // Активный буфер здесь и есть нормальный: выше исполнен RIS.
                    const payload = snapshotStepPayload(snapStep, screenField, () => ({
                      baseY: term2.buffer.active.baseY,
                      cursorY: term2.buffer.active.cursorY,
                      rows: term2.rows,
                    }));
                    if (payload.length > 0) {
                      writeSerial(payload, () => runSnapshotSteps(steps, i + 1), snapGuard, null);
                      return;
                    }
                  } else {
                    writeFrameOnce();
                    return;
                  }
                }
              };
              // ST-06: те же байты шагов, но двумя записями (доказательство —
              // snapshotPresentation.test.ts): голова — RIS + история (+ BEGIN
              // на WebGL), хвост — досылка по замеру ПОСЛЕ головы + кадр (+ END).
              const presentSnapshot = (parts: SnapshotStepParts): void => {
                const live2 = terminalRef.current;
                if (!live2) { releaseSnapshotBarrier(snapToken); settleRecovery(false); return; }
                const begun = beginSnapshotTxn({
                  renderer: rendererRef.current,
                  ris: parts.ris ? SNAPSHOT_RIS : undefined,
                  history: parts.history ?? undefined,
                  modeOn: live2.modes.synchronizedOutputMode,
                  txn: presentTxnRef.current,
                  token: ++presentTokenRef.current,
                  now: performance.now(),
                });
                snapBegun = begun;
                presentSet(begun.txn, "snapshot-head");
                trace("snap-apply", {
                  token: snapToken, ris: parts.ris, hasHist: parts.history !== null, sync: begun.head.opened,
                  renderer: rendererRef.current,
                }, "present-head");
                if (begun.head.chunk === null) {
                  // Истории нет: RIS (если есть) уходит в хвост, одним write с кадром.
                  snapTailRis = parts.ris;
                  writeFrameOnce();
                  return;
                }
                writeTracked(begun.head.chunk, () => {
                  erasedInGenerationRef.current = false; // RIS головы собрал буфер заново
                  if (parts.ris) ringTruncatedRef.current = snapRingCut;
                  if (begun.head.opened) presentParsedRef.current = begun.closeToken;
                  const term2 = terminalRef.current;
                  if (!term2 || disposedRef.current || snapStale()) {
                    releaseSnapshotBarrier(snapToken);
                    settleRecovery(false);
                    abandonSnapPresent("snapshot-stale");
                    return;
                  }
                  snapFiller = parts.filler
                    ? snapshotStepPayload(parts.filler, screenField, () => ({
                      baseY: term2.buffer.active.baseY, cursorY: term2.buffer.active.cursorY, rows: term2.rows,
                    })) as string
                    : "";
                  writeFrameOnce();
                }, snapGuard, null);
              };
              const parts = features.presentation ? snapshotStepParts(plan.steps) : null;
              if (parts) presentSnapshot(parts);
              else runSnapshotSteps(plan.steps, 0);
            });
          } else if (msg.t === "exit") {
            recordControl(ev.data, trace("conn", { code: typeof msg.code === "number" ? msg.code : -1 }, "exit"));
            // Баннер — в колбэке полного спуска (см. reset): иначе «[Process
            // exited]» вставал в очередь ПЕРЕД последними строками самого
            // процесса и человек читал прощание раньше вывода.
            const exitGeneration = connectionGenRef.current;
            const exitGuard = {
              generation: connectionGenRef.current,
              epoch: writerEpochRef.current,
            };
            flushTermWrites(() => {
              const live = terminalRef.current;
              if (!live || disposedRef.current) return;
              if (connectionGenRef.current !== exitGeneration) return;
              writeSerial("\r\n\x1b[33m[Process exited]\x1b[0m\r\n", undefined, exitGuard);
            });
            // Продолжать больше нечего: позицию забываем, чтобы она не пережила
            // саму сессию и не досталась чужой с тем же id.
            if (id) clearResumePos(id);
            // Показываем плашку «Процесс завершён» сразу, не дожидаясь
            // ближайшего тика полла getPtyState (до 2с живого инпута).
            setState((s) => ({ ...s, alive: false }));
          }
        } catch { /* ignore */ }
      }
    };

    ws.onclose = (ev) => {
      trace("conn", { code: typeof ev?.code === "number" ? ev.code : -1, gotOutput: gotOutputRef.current }, "close");
      historyClientRef.current.reset(); setHistoryAvailable(false);
      // Запрос и кадр умершего сокета больше ничего не решают: их срок истёк
      // бы в backoff, и откат холодной страницы открыл бы лишний сокет в
      // обход reconnectTimer (замер ревью 14.09, T-25). Таймер снимает сам
      // runRecovery → armRecovery: у offline будильника нет.
      if (features.recoveryV1) runRecovery(recovery.offline(recoveryRef.current), "close");
      // Код закрытия читаем — раньше обработчик объявлялся без параметра, и
      // любая причина выглядела одинаково: немой backoff под шапкой
      // «Подключение к терминалу…». 1013 «Try Again Later» релей шлёт, когда
      // компьютер на связи, но канал до него открыть не смог (агент не
      // дозвонился обратно за 15 с). Это НЕ «нет сети» и не «терминал умер»,
      // и человек должен это видеть, иначе диагноза нет ни у него, ни у нас.
      if (ev?.code === 1013) {
        setStreamRefused(true);
      } else if (gotOutputRef.current) {
        setStreamRefused(false);
      }
      if (firstByteTimer.current) { clearTimeout(firstByteTimer.current); firstByteTimer.current = null; }
      if (idleWatchTimer.current) { clearInterval(idleWatchTimer.current); idleWatchTimer.current = null; }
      if (silenceRecheckTimer.current) { clearTimeout(silenceRecheckTimer.current); silenceRecheckTimer.current = null; }
      // Загрузку файла НЕ отменяем: она идёт по своему REST-каналу и обрыв моста
      // ей не мешает, а abort вызывал abortUpload → сервер удалял частичный файл,
      // и resume начинал 20 МБ с нуля. Отмена осталась ручной (кнопка в полосе
      // прогресса) и на размонтировании страницы (cleanup эффекта).
      if (disposedRef.current) return; // размонтированы — не воскрешать цикл
      setConnected(false);
      sizeControlsRef.current = null;
      setSizeControls(null);
      setSizeControlsOpen(false);
      setSizeControlPending(false);
      wsRef.current = null;
      // Flow control живёт на соединение: сервер на новом стартует
      // неприостановленным, и наш флаг обязан это отражать. Неподтверждённые
      // байты (writeUnackedRef) НЕ сбрасываем: они про очередь самого xterm,
      // а она переживает реконнект.
      flowPausedRef.current = false;
      if (stableTimer.current) { clearTimeout(stableTimer.current); stableTimer.current = null; }

      // Auto-reconnect with capped exponential backoff. После десяти попыток
      // не сдаёмся: шапка меняется на «всё ещё пробуем», но цикл продолжает
      // ждать возвращения сети/релея.
      if (sessionMissingRef.current) {
        setReconnecting(false);
        setGaveUp(true);
        return;
      }
      attemptRef.current++;
      const a = attemptRef.current;
      setReconnecting(true);
      if (a > maxAttempts) setGaveUp(true);
      // Full Jitter (AWS «Exponential Backoff And Jitter»): delay = random(0,
      // min(cap, base·2^attempt)). Без джиттера все клиенты, отвалившиеся в один
      // момент (рестарт сервера), реконнектятся синхронно — thundering herd.
      const ceil = Math.min(15000, 1000 * Math.pow(2, Math.min(a, maxAttempts) - 1));
      const delay = Math.random() * ceil;
      trace("conn", { attempt: a, delayMs: Math.round(delay), gaveUp: a > maxAttempts }, "retry-scheduled");
      reconnectTimer.current = setTimeout(connect, delay);
    };

    ws.onerror = () => ws.close();
  }, [id, markStable, sendResize, enqueueTermWrite, flushTermWrites, requestScreenFrame]);
  // Откат координатора (холодная страница без кадра) переподключается сам.
  connectRef.current = connect;

  // Manual reconnect: reset the backoff counter and connect immediately.
  const retryNow = useCallback(() => {
    trace("conn", null, "retry-now");
    if (reconnectTimer.current) { clearTimeout(reconnectTimer.current); reconnectTimer.current = null; }
    attemptRef.current = 0;
    setGaveUp(false);
    setSessionMissing(false);
    sessionMissingRef.current = false;
    setReconnecting(true);
    connect();
  }, [connect]);

  const verifyLostSession = async () => {
    if (!id) return;
    const ticket = statePollGateRef.current!.begin();
    const askedAt = performance.now();
    try {
      const current = await getPtyState(id);
      if (!statePollGateRef.current!.accept(ticket)) return;
      stateAskedAtRef.current = askedAt;
      setState(current);
      trace("conn", { alive: !!current.alive }, "verify");
      if (current.alive) {
        showToast(t("pty.termAliveOnPc"));
        retryNow();
      } else {
        sessionMissingRef.current = true;
        setReconnecting(false);
        setGaveUp(false);
      }
    } catch (e: any) {
      if (!statePollGateRef.current!.accept(ticket)) return;
      if (e?.status === 404) {
        trace("conn", null, "verify-missing");
        sessionMissingRef.current = true;
        setSessionMissing(true);
        setReconnecting(false);
        setGaveUp(true);
        if (reconnectTimer.current) {
          clearTimeout(reconnectTimer.current);
          reconnectTimer.current = null;
        }
      } else {
        showToast(mapApiError(e));
      }
    }
  };

  // Два будильника реконнекта.
  //  1) visibilitychange: мобильные WebView молча замораживают фоновые сокеты
  //     (onclose не приходит) — вернулись на передний план с мёртвым сокетом и
  //     переподключаемся. Parity with the miniapp copy.
  //  2) online: сеть вернулась (Wi-Fi↔LTE, выход из туннеля/VPN, самолётный
  //     режим). Раньше после исчерпания maxAttempts экран латчился в gaveUp и
  //     ждал ручного тапа по «Переподключиться», даже когда связь уже была.
  useEffect(() => {
    // Будим только по-настоящему мёртвый сокет. OPEN не трогаем (Android шлёт
    // online на любую смену интерфейса — слепой retryNow сжигал бы мост на
    // релее и мигал оверлеем), CONNECTING тоже: незавершённый хендшейк уже
    // сторожит firstByteTimer, а перезапуск по каждому возврату в foreground
    // растягивал бы «Подключение к терминалу…» бесконечно.
    const wakeIfDead = () => {
      if (disposedRef.current) return; // размонтированы — не воскрешать цикл
      const ws = wsRef.current;
      if (ws && ws.readyState !== WebSocket.CLOSED && ws.readyState !== WebSocket.CLOSING) return;
      retryNow(); // внутри снимает латч gaveUp и обнуляет счётчик попыток
    };
    const onVis = () => {
      if (document.visibilityState !== "visible") {
        trace("vis", null, "hidden");
        // ST-09 (T-33): в скрытом документе кадров анимации нет, а взведённый
        // rAF не давал поставить фоновый таймер (scheduleFlush выходит сразу) —
        // очередь стояла до возврата. Висящий спуск переводим на таймер.
        if (features.flowBacklog && writeRafRef.current != null) {
          cancelFlushSchedule();
          scheduleFlushRef.current();
        }
        // Уход в фон — последний надёжный момент записать позицию: дальше
        // WebView замораживают, а приложение могут выгрузить без предупреждения.
        // Именно отсюда человек чаще всего и возвращается «открыть терминал».
        persistResume(true);
        flowCtl();
        return;
      }
      trace("vis", { ws: wsRef.current?.readyState ?? -1 }, "visible");
      if (features.flowBacklog) {
        // Льгота сторожа тишины: накопившиеся за фон hb ещё едут.
        visibleSinceRef.current = Date.now();
        // Фоновый таймер спуска (250 мс) — обратно на кадр анимации.
        if (writeRafRef.current == null && writeTimerRef.current != null) {
          cancelFlushSchedule();
          scheduleFlushRef.current();
        }
      }
      wakeIfDead();
      // Сокет OPEN, но мог тихо умереть в фоне: приговор тишины — по истечении
      // льготы, разовым таймером (ST-09, T-33).
      if (features.flowBacklog) armSilenceRecheck();
      flowCtl();
      // Сокет пережил фон (wakeIfDead ничего не переподключал), но за время
      // заморозки очередь вывода могла резаться по потолку (QUEUE_CAP_BYTES,
      // см. enqueueTermWrite): середина потока потеряна, и экран полноэкранного
      // TUI, рисующего диффами по ячейкам, разошёлся с приложением. Просим
      // свежий кадр тем же отложенным путём, что onopen. Без queueGap не
      // просим: кадр — это лишняя перерисовка, а сервер и так троттлит
      // (screenFrameMinGap).
      if (parserCarryRef.current.hasAnyGap && wsRef.current?.readyState === WebSocket.OPEN) {
        if (features.recoveryV1) recoveryApiRef.current.demand("foreground-gap");
        else requestScreenFrame(undefined, "gap");
      }
    };
    const onOnline = () => wakeIfDead();
    // Page Lifecycle: страницу разморозили (Chrome/WebView шлют resume, а
    // visibilitychange при заморозке видимой страницы может не прийти).
    const onResume = () => {
      if (!features.flowBacklog) return;
      trace("vis", null, "resume");
      visibleSinceRef.current = Date.now();
      armSilenceRecheck();
    };
    document.addEventListener("visibilitychange", onVis);
    document.addEventListener("resume", onResume);
    window.addEventListener("online", onOnline);
    return () => {
      document.removeEventListener("visibilitychange", onVis);
      document.removeEventListener("resume", onResume);
      window.removeEventListener("online", onOnline);
      persistResume(true); // уходим с экрана — фиксируем, ref сейчас умрёт
    };
  }, [retryNow, persistResume, requestScreenFrame]);

  // Initialize terminal + connect.
  useEffect(() => {
    if (!termRef.current) return;
    disposedRef.current = false; // эффект может пере-запуститься при смене id

    const term = new Terminal({
      cursorBlink: true,
      fontSize,
      lineHeight: 1.2,
      fontFamily: "'Cascadia Code', 'Fira Code', 'Consolas', monospace",
      // Семантика буфера (scrollback, неявная таблица Unicode '6') — из одной
      // константы с стендом эквивалентности (ptyTerm/terminalEmulation.ts).
      ...terminalEmulationOptions(),
      // 0 осознанно: ненулевое значение анимирует КАЖДЫЙ scrollLines, а наш
      // тач-слой зовёт его на каждый touchmove и кадр инерции — десятки
      // перенацеленных анимаций в секунду. На слабом WebView скролл становился
      // «резиновым» и отставал от пальца (разбор 12.08).
      smoothScrollDuration: 0,
      theme: {
        background: "#0d1117",
        foreground: "#c9d1d9",
        cursor: "#58a6ff",
        selectionBackground: "#264f78",
        black: "#0d1117",
        red: "#ff7b72",
        green: "#3fb950",
        yellow: "#d29922",
        blue: "#58a6ff",
        magenta: "#bc8cff",
        cyan: "#39c5cf",
        white: "#c9d1d9",
        brightBlack: "#484f58",
        brightRed: "#ffa198",
        brightGreen: "#56d364",
        brightYellow: "#e3b341",
        brightBlue: "#79c0ff",
        brightMagenta: "#d2a8ff",
        brightCyan: "#56d4dd",
        brightWhite: "#f0f6fc",
      },
    });

    const fit = new FitAddon();
    term.loadAddon(fit);
    term.open(termRef.current);
    // ST-10 (T-39): команды как блоки по OSC 133. Обработчик вешается на
    // парсер и зовётся внутри разбора байтов, пришедших через TerminalWriter
    // (I-03): маркер встаёт ровно в своём месте потока при любом разрезе
    // чанков. Модель ничего не отдаёт в маршрут навигации (страж T-39):
    // долгая команда без D не делает терминал «Агентом». Эпоха модели
    // постоянна — сбрасываем её сами в точке RIS (ниже), где буфер
    // действительно пересобирается (reset-маркер, кадр с историей, RIS
    // приложения); resumed без RIS буфер сохраняет, и блоки живут.
    const blockModel = new CommandBlockModel();
    blockModelRef.current = blockModel;
    const blockOsc = features.commandBlocks ? attachCommandBlocks(term, blockModel, () => 0) : null;
    // ST-04: ИСТОРИЯ РЕАЛЬНО УШЛА — считает сам разборщик, в момент исполнения.
    // CSI 3 J (и ?3J) в обычном буфере и RIS доходят сюда из любого пути:
    // досылка отложенного, цепочка у низа, ручная очистка, reset-маркер, снимок,
    // RIS самого приложения. false — «не обработано»: исполнит штатный
    // обработчик xterm, поведение терминала не меняется. В alt-буфере CSI 3 J
    // обычную историю не трогает — и не считается.
    const noteScrollbackErased = (params: (number | number[])[]) => {
      if (params[0] === 3 && term.buffer.active.type === "normal") {
        historySeqRef.current++;
        erasedInGenerationRef.current = true;
      }
      return false;
    };
    term.parser.registerCsiHandler({ final: "J" }, noteScrollbackErased);
    term.parser.registerCsiHandler({ prefix: "?", final: "J" }, noteScrollbackErased);
    term.parser.registerEscHandler({ final: "c" }, () => {
      historySeqRef.current++;
      erasedInGenerationRef.current = true;
      // ST-10: RIS пересобирает буфер — блоки до этой точки пропали (I-11),
      // ряд действий честно скажет «блоки до восстановления недоступны».
      if (blockOsc) blockModel.reset("epoch");
      return false;
    });

    // ⚠ Телефон: не давать xterm держать фокус.
    //
    // ⚠ ЗДЕСЬ СТОЯЛО ГАШЕНИЕ ФОКУСА XTERM, И ЕГО СНЯЛИ ОСОЗНАННО (10.09.2026).
    // Обработчик `focusin` на контейнере отбирал фокус у скрытого textarea
    // xterm на сенсорных поверхностях, чтобы тап по выводу не поднимал
    // системную клавиатуру (жалоба 08.09 с iPhone). Владелец 10.09 попросил
    // ровно обратного: «когда я туда нажимаю, тоже мог туда вводить именно
    // клавиатуру телефона» — то есть тап по выводу должен давать ввод прямо в
    // PTY, как в любом SSH-клиенте. С гашением это невозможно по построению:
    // xterm терял фокус в тот же момент, когда его получал.
    //
    // Набор с автозаменой никуда не делся — он живёт в нижнем поле
    // (`pty-text-input`), и после отправки фокус возвращается туда же.

    // WebGL-рендер поверх DOM. Жирные кадры TUI (Kimi ~90 КБ на перерисовку)
    // DOM-рендер разбирает и рисует на main thread сотни миллисекунд — на
    // WebView телефона это и есть «терминал фризится». Аддон грузим
    // ДИНАМИЧЕСКИ: создание Terminal и так живёт только в этом браузерном
    // эффекте, а в node-окружении тестов модуль не должен появиться вовсе;
    // заодно он уезжает в отдельный чанк бандла и подтягивается только на
    // экране терминала. WebView без WebGL2 или любое исключение = молча
    // остаёмся на DOM. onContextLoss → dispose(): откат на DOM-рендер при
    // потере контекста встроен в xterm 6.
    let webglStale = false;
    // Смена рендерера видна в трассе (T-34): с чего начали, поднялся ли WebGL,
    // терялся ли его контекст, куда откатились и вернулись ли.
    //
    // ST-06 (T-34, features.presentation). Замер до правки (скринкаст): после
    // потери контекста экран был ПУСТ 3004 мс — аддон ждёт восстановления 3 с на
    // мёртвом холсте и только потом отдаёт DOM. Теперь потеря сразу уводит на
    // DOM-рендер (он рисует тот же буфер — семантика терминала не меняется), а
    // WebGL пробуем ещё РОВНО один раз — при возврате страницы на передний план
    // (на телефоне контекст теряется именно в фоне).
    rendererRef.current = "dom";
    trace("renderer", null, "dom");
    let webglAddon: { dispose(): void } | null = null;
    let webglLost = false;
    let webglRetryLeft = features.presentation ? 1 : 0;
    const dropWebgl = (addon: { dispose(): void }, why: string) => {
      if (webglAddon !== addon) return;
      webglAddon = null;
      rendererRef.current = "dom";
      // Отложенный показ со сменой рендера показал бы пустые строки DOM.
      presentDropRef.current("renderer");
      try { addon.dispose(); } catch { /* уже прибран */ }
      trace("renderer", null, `fallback-dom:${why}`);
    };
    // ST-08: рендер определился (WebGL подключён или не подключится). Первый
    // размер нового сокета, отложенный до этого момента, уходит сейчас — уже в
    // ячейке окончательного рендера; иначе после смены рендера вместимость
    // просто пересчитывается (refit).
    const rendererSettled = (refit: boolean) => {
      // Эффект прибран (размонтирование, повторный проход StrictMode, смена
      // зависимостей): rendererPendingRef и openResizeWaitRef общие для
      // компонента и уже принадлежат НОВОМУ терминалу, а term здесь мёртвый.
      // Поздний import прежнего эффекта их не трогает — новый ведёт свой
      // loadWebgl (ревью ST-08).
      if (webglStale) return;
      rendererPendingRef.current = false;
      const waiting = openResizeWaitRef.current;
      openResizeWaitRef.current = null;
      const live = !!waiting && waiting === wsRef.current && waiting.readyState === WebSocket.OPEN;
      if (!refit && !live) return;
      fitLocalRef.current();
      setTermSize((prev) => (
        prev.cols === term.cols && prev.rows === term.rows ? prev : { cols: term.cols, rows: term.rows }
      ));
      if (live) {
        sendResize(true);
        // Запрос кадра этого сокета ждал именно этот размер (ревью 15.09).
        recoveryApiRef.current.resizeSent();
      } else sendResize();
    };
    const loadWebgl = (retry: boolean) => {
      if (features.capacity) rendererPendingRef.current = true;
      void import("@xterm/addon-webgl")
        .then(({ WebglAddon }) => {
          // эффект прибран (смена id/размонтирование) или WebGL уже есть
          if (webglStale || webglAddon) { rendererSettled(false); return; }
          try {
            const addon = new WebglAddon();
            addon.onContextLoss(() => dropWebgl(addon, "timeout"));
            if (retry) presentDropRef.current("renderer");
            term.loadAddon(addon);
            webglAddon = addon;
            rendererRef.current = "webgl";
            trace("renderer", null, retry ? "webgl-retry" : "webgl");
            // ST-08: рендер WebGL считает ячейку по-своему (до пикселя
            // устройства), а первый fit был на DOM-рендере — вместимость
            // поменялась. Без пересчёта поправка уходила агенту при СЛЕДУЮЩЕМ
            // событии вёрстки: замер T-31 — первое закрытие клавиатуры после
            // открытия давало PTY resize 45x23 → 47x23 при неизменной коробке.
            // Причина смены вместимости — рендер: пересчитываем сразу, один раз.
            if (features.capacity) requestAnimationFrame(() => {
              if (webglStale || webglAddon !== addon) { rendererSettled(false); return; }
              trace("geom", null, "renderer");
              rendererSettled(true);
            });
          } catch {
            trace("renderer", null, retry ? "webgl-retry-failed" : "webgl-failed");
            rendererSettled(false); /* нет WebGL2 или сбой активации — живём на DOM-рендере */
          }
        })
        .catch(() => {
          trace("renderer", null, "webgl-import-failed");
          rendererSettled(false); /* модуль не загрузился — живём на DOM-рендере */
        });
    };
    // Потеря приходит на холст аддона и не всплывает — ловим на фазе перехвата,
    // раньше слушателя аддона (он поставил бы трёхсекундное ожидание).
    const glHost = termRef.current;
    const onContextLost = () => {
      trace("renderer", null, "webgl-context-lost");
      webglLost = true;
      if (features.presentation && webglAddon) dropWebgl(webglAddon, "lost");
    };
    const onContextRestored = () => { trace("renderer", null, "webgl-context-restored"); };
    const onVisibleWebglRetry = () => {
      if (document.visibilityState !== "visible" || webglStale || webglAddon || !webglLost || webglRetryLeft <= 0) return;
      webglRetryLeft--;
      loadWebgl(true);
    };
    glHost.addEventListener("webglcontextlost", onContextLost, { capture: true });
    glHost.addEventListener("webglcontextrestored", onContextRestored, { capture: true });
    document.addEventListener("visibilitychange", onVisibleWebglRetry);
    // Android WebView's SwiftShader advertises WebGL2 but can render glyphs as
    // clipped fragments. Keep the same terminal buffer and use its DOM renderer.
    if (androidWebglNeedsDom(isNativeApp, navigator.userAgent)) {
      trace("renderer", null, "dom-android-swiftshader");
      rendererSettled(false);
    } else loadWebgl(false);

    // Колесо мыши в полноэкранном TUI без mouse-tracking xterm сам превращает в
    // ↑/↓ и отдаёт их приложению. Для АГЕНТА это не прокрутка, а перебор
    // истории запросов: человек крутит колесо, чтобы перечитать вывод, а вместо
    // этого его набранный промпт подменяется предыдущим сообщением — и вернуть
    // текст нечем. Тот же запрет у нас уже стоял, но только в scrollByLines,
    // куда попадают лишь жест пальцем и кнопки ⇈↑↓⇊.
    // Возврат false = «событие обработали, дальше не пускать».
    //
    // ⚠ И обратное: `if (!alt) return true` отдавало колесо родному xterm ВСЕГДА,
    // когда приложение в обычном буфере. У Claude Code там двигать нечего
    // (baseY = 0) — на телефоне свайп работал, а колесом и трекпадом в окне exe
    // и в вебе не листалось ничего (повторный аудит 2.57.12, T259-04). Решает
    // тот же вопрос, что и палец: может ли своя прокрутка сдвинуться.
    const wheel = new WheelAccumulator();
    let wheelGeometry: ReturnType<typeof measureTerminalCoordinates> = null;
    let wheelMeasuredAt = -Infinity;
    const onTerminalWheel = (ev: WheelEvent) => {
      // xterm's internal scrollable element receives bubble events before the
      // custom wheel callback. Own the event at the host's capture boundary,
      // otherwise fractional deltas can scroll twice (native + our router).
      ev.stopPropagation();
      // ST-10: горизонталь колеса и трекпада сдвигает окно по сетке шире
      // коробки. В приложение не уходит ничего; вертикаль — прежний маршрут.
      if (features.viewportPan) {
        const lim = panLimits();
        const dx = wheelPanDelta(ev, lim && term.cols > 0 ? lim.drawnW / term.cols : 0, lim?.innerW ?? 0);
        if (dx !== null) {
          if (panByPixelsRef.current(dx)) {
            ev.preventDefault();
            trace("gesture", { px: Math.round(dx) }, "wheel-pan");
          }
          return false;
        }
      }
      // Leave browser zoom alone; never convert horizontal travel into input.
      if (ev.ctrlKey || ev.metaKey || Math.abs(ev.deltaX) > Math.abs(ev.deltaY)) return false;
      ev.preventDefault();
      // Серия колеса меряется по КАЖДОМУ сырому событию, а не только по набравшим
      // строку (ScrollRouter.wheelActivity; часы те же, что у route — Date.now):
      // затухающая инерция трекпада после ⇊ набирает строку реже паузы серии, но
      // это всё тот же жест — не исполняется, остаток накопителя сбрасывается
      // (волна 8, probe-reading-pin-live мир E).
      if (scrollRouterRef.current.wheelActivity(Date.now())) { wheel.reset(); return false; }
      const now = performance.now();
      if (now - wheelMeasuredAt > 100) {
        wheelGeometry = measureTerminalCoordinates(termRef.current!, term.cols, term.rows);
        wheelMeasuredAt = now;
      }
      if (!wheelGeometry) return false;
      const lines = wheel.lines(ev, wheelGeometry.cellHeight, wheelGeometry.visible.height, now);
      if (lines === 0) return false;
      trace("gesture", { lines, deltaMode: ev.deltaMode }, "wheel");
      // Mouse/trackpad, finger and buttons all enter through scrollByLines. It
      // decides local-vs-application and owns the same response probe/fallback.
      scrollByLinesRef.current(lines, { source: "wheel" });
      return false;
    };
    const wheelHost = termRef.current;
    wheelHost.addEventListener("wheel", onTerminalWheel, { capture: true, passive: false });
    term.attachCustomWheelEventHandler(() => false);
    terminalRef.current = term;
    terminalWriterRef.current!.setTarget(term);
    fitRef.current = fit;
    // onResize is the single source of truth for actual xterm geometry. It
    // covers FitAddon, explicit restore/adopt resize and future resize sites;
    // DOM/visualViewport noise which leaves cols/rows unchanged is ignored.
    // I-06: «разобрано» ≠ «показано». Первая отрисовка после разбора замены
    // (кадр экрана, маркер reset) — событие presented. Под удержанием 2026
    // отрисовки нет до END или сторожа, поэтому ms — видимая задержка показа.
    const presentedDisposable = term.onRender(() => {
      const p = pendingPresentRef.current;
      if (!p) return;
      pendingPresentRef.current = null;
      trace("presented", { token: p.token, ms: Math.round(performance.now() - p.parsedAt), renderer: rendererRef.current }, p.what);
    });
    // ST-06: режим 2026 снят не нами — удержание закрываем (presentModeOff).
    const presentModeOffDisposable = term.onWriteParsed(() => presentModeOffRef.current());
    let lastScrollMode = `${term.buffer.active.type}:${term.modes.mouseTrackingMode}`;
    const scrollModeDisposable = term.onWriteParsed(() => {
      const mode = `${term.buffer.active.type}:${term.modes.mouseTrackingMode}`;
      if (mode === lastScrollMode) return;
      lastScrollMode = mode;
      trace("mode", { alt: term.buffer.active.type === "alternate", mouse: term.modes.mouseTrackingMode }, "buffer-mouse");
      cancelGestureRef.current();
      // Режим буфера и мыши входит в область действия свидетельств: сверка
      // сотрёт наблюдения прежнего режима и вернёт память нового.
      restoreProbeVerdictRef.current();
    });
    // ST-10: полноэкранная программа взяла экран — открытый блок НЕ
    // выбрасывается, а помечается (noteAltScreen): команда и напечатанное вне
    // программы лежат в нормальном буфере, D после выхода закроет блок. Ряд
    // действий пересчитывается после разбора записи и прокрутки не чаще раза
    // в 120 мс — только флаги, без текста.
    let blockUiTimer: ReturnType<typeof setTimeout> | null = null;
    const scheduleBlockUi = () => {
      if (blockUiTimer !== null) return;
      blockUiTimer = setTimeout(() => { blockUiTimer = null; if (!disposedRef.current) refreshBlockUiRef.current(); }, 120);
    };
    const blockAltDisposable = blockOsc ? term.buffer.onBufferChange((active) => {
      if (active.type === "alternate") blockModel.noteAltScreen();
      scheduleBlockUi();
    }) : null;
    const blockParsedDisposable = blockOsc ? term.onWriteParsed(scheduleBlockUi) : null;
    const blockScrollDisposable = blockOsc ? term.onScroll(scheduleBlockUi) : null;
    let cursorPeekRaf: number | null = null;
    const cursorPeekDisposable = term.onCursorMove(() => {
      // ST-10: за курсором по X — и когда сетка шире коробки (viewportPan).
      const follow = keyboardOpenRef.current || (features.occlusion && peekOverflowRef.current)
        || (features.viewportPan && panOverflowXRef.current);
      if (!follow || document.hidden || cursorPeekRaf !== null) return;
      cursorPeekRaf = requestAnimationFrame(() => { cursorPeekRaf = null; applyKeyboardPeekRef.current("output"); });
    });
    // ST-06: пересчёт перекрытия, отложенный на время ?2026h…?2026l приложения,
    // — после разбора записи, закрывшей транзакцию (кадр показан целиком).
    const occlusionSyncDisposable = term.onWriteParsed(() => {
      if (!occlusionDeferredRef.current || term.modes.synchronizedOutputMode) return;
      occlusionDeferredRef.current = false;
      // Транзакция закрылась обычным путём — страховочный таймер больше не нужен.
      clearOcclusionRecheck("sync");
      if (document.hidden || cursorPeekRaf !== null) return;
      cursorPeekRaf = requestAnimationFrame(() => { cursorPeekRaf = null; applyKeyboardPeekRef.current("output"); });
    });
    geometrySizeRef.current = { cols: term.cols, rows: term.rows };
    const geometryResizeDisposable = term.onResize(({ cols, rows }) => {
      const next = noteTerminalGeometry({
        revision: geometryRevisionRef.current,
        geometry: geometrySizeRef.current,
      }, { cols, rows });
      if (next.revision === geometryRevisionRef.current) return;
      // ST-06: resize очищает холст WebGL, а под удержанием 2026 отрисовка
      // отложена — удержание снимаем, иначе пусто до сторожа.
      if (presentHolding()) presentDropRef.current("resize");
      readGeometryRevisionRef.current++;
      cancelGestureRef.current();
      geometryRevisionRef.current = next.revision;
      geometrySizeRef.current = next.geometry;
      geometryRetryRevisionRef.current = null;
      // Настоящая сетка xterm: cols/rows отсюда asciicast берёт как resize.
      trace("geom", { cols, rows }, "xterm");
      recoveryApiRef.current.syncGeometry();
      // ST-10: сетка стала шире или уже коробки (принятая сетка кадра, общий
      // размер) — окно по X пересчитывается: запас сдвига и зажим по краям.
      if (features.viewportPan) {
        panLimitsRef.current = null;
        requestAnimationFrame(() => { if (!disposedRef.current) applyKeyboardPeekRef.current(); });
      }
    });
    // ⚠ ПЕРВЫЙ ЗАМЕР — тоже через fitLocal: терминал часто открывают, когда
    // клавиатура УЖЕ на экране (человек вернулся в приложение из чата), и голый
    // fit() создавал десятистрочный терминал при тридцатистрочном PTY.
    fitLocalRef.current();
    setTermSize({ cols: term.cols, rows: term.rows });
    // Вывод, пришедший ДО этого момента, ждёт в очереди — спускаем его сразу, а
    // не следующим кадром: это первое, что человек должен увидеть, открыв
    // терминал.
    flushTermWrites();

    // Returning to live must remain reachable while reading local history,
    // even after the idle toolbar timer expires. This is UI state only.
    const updateLocalReading = () => {
      const b = term.buffer.active;
      setLocalScrolledUp(b.viewportY < b.baseY);
    };
    updateLocalReading();
    const localReadingScroll = term.onScroll(updateLocalReading);
    const localReadingBuffer = term.buffer.onBufferChange(updateLocalReading);
    // Copy-on-select: выделил мышью → сразу в буфер обмена (привычное поведение
    // терминала на ПК; в терминале Ctrl+C = прерывание, а не копирование). На
    // сенсоре остаётся режим «Выделить» + кнопка «Копировать».
    const selHost = termRef.current;
    const stopCopyOnSelect = attachCopyOnSelect(selHost, () => term.getSelection(), sel => {
      void terminalClipboard.write(sel).then(ok => showToast(t(ok ? "pty.copied" : "pty.copyFailed")));
    }, () => !disposedRef.current && !presentHolding());
    // ST-06: пока удерживается старая картинка (≤ 1 с), клики мышью и
    // совместимые события касания до xterm не доходят: его отчёт о мыши ушёл
    // бы приложению по координатам нового, ещё не показанного буфера.
    // ⚠ Гасится ОТЧЁТ О МЫШИ, а не намерение человека. Касание по терминалу —
    // это ещё и фокус для клавиатуры, и ставит его САМ xterm на своём mousedown
    // (наш handleTap после маркера и так молчит: маркер отменяет жест, а отмена
    // взводит подавление тапа на 800 мс). Погасив mousedown, мы отняли у xterm и
    // фокус — поэтому ставим его здесь сами, внутри того же жеста, иначе
    // клавиатура телефона не поднимется. Скептик после ревью S4 (14.09):
    // удержание идёт на каждом маркере reset, то есть ровно при возврате из
    // фона, когда человек тапает, чтобы печатать, — и тап пропадал.
    const onHoldPointer = (ev: Event) => {
      if (!presentHolding()) return;
      ev.stopPropagation();
      ev.preventDefault();
      if (ev.type !== "mousedown") return;
      trace("input", { hold: true }, "click-suppressed");
      if (!selectModeRef.current) term.focus();
    };
    const holdEvents = ["mousedown", "mouseup", "click"] as const;
    for (const type of holdEvents) selHost.addEventListener(type, onHoldPointer, { capture: true });
    // ST-10 (tapClickOnce): одно касание — один клик приложению. Касание при
    // слежении за мышью уже ушло кликом в точную ячейку (sendTapAsClick, с
    // учётом сдвига окна); совместимые mousedown/mouseup того же касания xterm
    // отдавал ВТОРЫМ кликом — замер пробы viewport pan: «<0;211;8» и затем
    // «<0;212;8», то же на baseline. click не трогаем: он поднимает клавиатуру.
    const compatEvents = ["mousedown", "mouseup"] as const;
    const onCompatMouse = (ev: Event) => {
      if (!compatMouseAfterTap(performance.now(), tapClickSentAtRef.current)) return;
      ev.stopPropagation();
      if (ev.type === "mousedown") trace("input", null, "tap-compat-mouse-dropped");
    };
    if (features.tapClickOnce) for (const type of compatEvents) selHost.addEventListener(type, onCompatMouse, { capture: true });

    // Forward user input to WebSocket.
    term.onData((data) => {
      // Время ввода ЧЕЛОВЕКА — опора для признака «агент не отвечает».
      // ⚠ Не всякий onData — человек: xterm сам отвечает приложению на его
      // запросы (отчёт о фокусе ESC[I/ESC[O при ?1004h, который включает
      // Claude Code; позиция курсора; атрибуты терминала). Ответа на них
      // не бывает, и каждый переход в приложение через 8 с поднимал бейдж
      // «Агент не отвечает» на живом простаивающем агенте (скриншот
      // владельца 03.09.2026, 10:33). Правило — ptyTerm/autoReply.ts.
      const autoReply = isTerminalAutoReply(data);
      if (!autoReply) {
        lastUserInputAtRef.current = Date.now();
        // ST-10: ввод человека возвращает окно по X курсору (panFollows).
        panInputAtRef.current = lastUserInputAtRef.current;
      }
      // В трассу — НИКОГДА сами символы, и даже длина одиночных нажатий не
      // пишется (I-15, волна 4): нажатия сливаются в серию с корзиной длины и
      // грубым временем (terminalTrace.noteInput) — длину пароля и ритм по
      // файлу не восстановить.
      const noteInput = (ok: boolean, how: "key" | "encoded-paste") => {
        if (features.trace) traceRef.current!.noteInput(traceCtx(), { len: data.length, ok, auto: autoReply }, how);
      };
      if (data.length > 100) {
        // Large input (paste) — use paste protocol for chunked ConPTY delivery.
        // xterm already encoded this input; do not normalize or wrap it twice.
        const ok = transmitInput(wsRef.current, { kind: "encodedPaste", data });
        noteInput(ok, "encoded-paste");
        if (!ok) showToast(t("pty.noConnection"));
      } else {
        const ok = transmitInput(wsRef.current, { kind: "key", data });
        noteInput(ok, "key");
        if (!ok) {
          // Ввод в мёртвый сокет молча пропадал (UX ТОП-10 #2) — сигналим.
          showToast(t("pty.noConnection"));
        }
      }
    });

    // Handle resize.
    // Контейнер терминала изменился. Тот же порог, что у оконного refit: на
    // телефоне прокрутка прячет панель браузера, контейнер вырастает на 50-60 px
    // (2-4 строки), и раньше КАЖДЫЙ такой скачок уходил агенту как новый размер
    // — а TUI на resize перерисовывает окно целиком и стирает историю (замер:
    // +58 px → 90 КБ, внутри «стереть экран» и «стереть историю»). Человек при
    // этом улетал в начало вывода прямо во время чтения (жалоба 31.07).
    //
    // ВАЖНО, чем порог обязан управлять. Он ставился против ДРОЖАНИЯ ОКНА и
    // только против него. Но применялся ко всякому изменению коробки — а её
    // меняет и НАША СОБСТВЕННАЯ вёрстка, у которой окно стоит на месте:
    // приехал ответ /state и в шапке появилась строка «📍 папка» (+21 px),
    // человек набрал длинный промпт и поле выросло 40→122 px, свернулся ряд
    // клавиш (115 px), появилась полоса загрузки файла. Все эти изменения
    // меньше 120 px, поэтому fit() не звался, xterm продолжал рисовать прежнее
    // число строк в уменьшившуюся коробку, а `overflow: hidden` срезал их
    // СНИЗУ — то есть ровно там, где печатает агент и стоит курсор. Замер
    // 04.08.2026 (390×844): после пяти строк в поле ввода за кадром 100 px ≈
    // пять строк, при открытии терминала — последняя строка с приглашением.
    //
    // Поэтому решаем по ИСТОЧНИКУ изменения, а не по его величине:
    //   • окно не менялось → перестроилась наша вёрстка. Считаем размер честно
    //     (fit + resize): лишняя перерисовка TUI лучше невидимого вывода, и
    //     такие перестройки редки — это не серия кадров, как у панели браузера;
    //   • окно менялось → это панель браузера/клавиатура, работает прежний
    //     порог 120 px (жалоба 31.07 «улетает в самый верх»: +58 px давали
    //     90 КБ перерисовки со стиранием истории).
    //
    // Чинить «fit() всегда, порог только для sendResize» нельзя: локальные
    // rows разошлись бы с размером PTY, и агент рисовал бы больше строк, чем
    // помещается — те же потерянные строки, только под другим соусом.
    const OBSERVER_NOISE_PX = 120;
    const winHeight = () => Math.round(window.visualViewport?.height ?? window.innerHeight);
    let lastBoxHeight = termRef.current.clientHeight;
    let lastBoxWidth = termRef.current.clientWidth;
    let lastWinHeight = winHeight();
    // ST-08 (T-31): высота плашек в той же вёрстке, где коробку мерили в
    // последний раз (пара с lastBoxHeight). Изменение коробки, целиком
    // объяснённое изменением плашек, — перекрытие (geometry.ts layoutChange):
    // видимое окно сдвигается к курсору, а fit + resize не зовутся (I-09).
    let lastOccluderPx = features.occlusion ? measureOccluderPxRef.current() : 0;
    const resizeObserver = new ResizeObserver(() => {
      const el = termRef.current;
      if (!el) return;
      // Клавиатура открыта — коробка уменьшилась, но размер PTY мы не трогаем
      // (см. applyKeyboardPeek): только пересчитываем, насколько показать низ.
      // Без этой ветки именно наблюдатель контейнера и делал бы тот resize,
      // от которого мы уходим: скачок высоты от клавиатуры больше его порога.
      if (updateKeyboardModeRef.current()) { applyKeyboardPeekRef.current(); return; }
      const h = el.clientHeight;
      const w = el.clientWidth;
      const wh = winHeight();
      const widthChanged = Math.abs(w - lastBoxWidth) >= 2;
      const windowChanged = Math.abs(wh - lastWinHeight) >= 2;
      lastWinHeight = wh;
      // Ранние выходы не меняют вместимость, но видимое окно под перекрытием
      // могло поменяться (плашка ушла, пока была клавиатура): сдвиг заново.
      if (windowChanged && !widthChanged && Math.abs(h - lastBoxHeight) < OBSERVER_NOISE_PX) {
        if (features.occlusion) applyKeyboardPeekRef.current();
        return;
      }
      if (h === lastBoxHeight && !widthChanged) { // коробка не двигалась — считать нечего
        if (features.occlusion) applyKeyboardPeekRef.current();
        return;
      }
      if (features.occlusion) {
        const occluderPx = measureOccluderPxRef.current();
        const change = layoutChange({
          windowChanged, widthChanged, boxDeltaPx: h - lastBoxHeight, occluderDeltaPx: occluderPx - lastOccluderPx,
        });
        lastOccluderPx = occluderPx;
        if (change === "occlusion") {
          lastBoxHeight = h;
          trace("geom", { boxW: w, boxH: h, occluderPx: Math.round(occluderPx) }, "occlusion");
          applyKeyboardPeekRef.current();
          return;
        }
      }
      lastBoxHeight = h;
      lastBoxWidth = w;
      trace("geom", { boxW: w, boxH: h, windowChanged }, "box");
      fitLocalRef.current();
      setTermSize((prev) => (
        prev.cols === term.cols && prev.rows === term.rows ? prev : { cols: term.cols, rows: term.rows }
      ));
      sendResize();
    });
    resizeObserver.observe(termRef.current);

    // Connect WebSocket.
    connect();

    return () => {
      // Порядок важен: сначала флаг + отцепить обработчики сокета, ПОТОМ close().
      // onclose приходит асинхронно после cleanup — если оставить его висеть, он
      // запланирует connect() на размонтированном компоненте (см. disposedRef).
      disposedRef.current = true;
      webglStale = true; // динамический import WebGL-аддона не должен догрузиться в мёртвый терминал
      // Ожидание рендера принадлежало этому терминалу и его сокету: новый
      // эффект начинает с чистого листа и сам взведёт его своим loadWebgl.
      rendererPendingRef.current = false;
      openResizeWaitRef.current = null;
      resizeObserver.disconnect();
      geometryResizeDisposable.dispose();
      wheelHost.removeEventListener("wheel", onTerminalWheel, { capture: true });
      scrollModeDisposable.dispose();
      localReadingScroll.dispose();
      localReadingBuffer.dispose();
      cursorPeekDisposable.dispose();
      occlusionSyncDisposable.dispose();
      if (cursorPeekRaf !== null) cancelAnimationFrame(cursorPeekRaf);
      clearOcclusionRecheck();
      occlusionStateRef.current = OCCLUSION_IDLE;
      occlusionDeferredRef.current = false;
      // ST-10: обработчик OSC 133 и подписки блоков принадлежат этому терминалу.
      blockOsc?.dispose();
      blockAltDisposable?.dispose();
      blockParsedDisposable?.dispose();
      blockScrollDisposable?.dispose();
      if (blockUiTimer !== null) clearTimeout(blockUiTimer);
      if (blockModelRef.current === blockModel) blockModelRef.current = null;
      refreshBlockUiRef.current();
      stopCopyOnSelect();
      for (const type of holdEvents) selHost.removeEventListener(type, onHoldPointer, { capture: true });
      for (const type of compatEvents) selHost.removeEventListener(type, onCompatMouse, { capture: true });
      glHost.removeEventListener("webglcontextlost", onContextLost, { capture: true });
      glHost.removeEventListener("webglcontextrestored", onContextRestored, { capture: true });
      document.removeEventListener("visibilitychange", onVisibleWebglRetry);
      presentedDisposable.dispose();
      presentModeOffDisposable.dispose();
      if (presentTimerRef.current != null) { clearTimeout(presentTimerRef.current); presentTimerRef.current = null; }
      presentTxnRef.current = TXN_CLOSED;
      presentParsedRef.current = null;
      pendingPresentRef.current = null;
      // Уходим со страницы — вставлять путь уже некуда, поэтому загрузку рвём
      // здесь (а не в ws.onclose, где обрыв моста стоил серверного прогресса).
      uploadAbortRef.current?.abort();
      uploadAbortRef.current = null;
      pendingPathsRef.current = [];
      if (reconnectTimer.current) { clearTimeout(reconnectTimer.current); reconnectTimer.current = null; }
      if (stableTimer.current) { clearTimeout(stableTimer.current); stableTimer.current = null; }
      if (firstByteTimer.current) { clearTimeout(firstByteTimer.current); firstByteTimer.current = null; }
      if (idleWatchTimer.current) { clearInterval(idleWatchTimer.current); idleWatchTimer.current = null; }
      if (silenceRecheckTimer.current) { clearTimeout(silenceRecheckTimer.current); silenceRecheckTimer.current = null; }
      if (resizeQuietTimer.current) { clearTimeout(resizeQuietTimer.current); resizeQuietTimer.current = null; }
      cancelScrollObservations();
      altScrollProbeTokenRef.current++;
      altScrollQueuedActionsRef.current = [];
      if (pageIdleTimerRef.current != null) { clearTimeout(pageIdleTimerRef.current); pageIdleTimerRef.current = null; }
      pageAccumRef.current = 0;
      if (altScrollRafRef.current != null) { cancelAnimationFrame(altScrollRafRef.current); altScrollRafRef.current = null; }
      for (const timer of screenRequestTimersRef.current) window.clearTimeout(timer);
      screenRequestTimersRef.current.clear();
      // Единственный таймер координатора: после размонтирования не будит никого.
      if (recoveryTimerRef.current != null) { window.clearTimeout(recoveryTimerRef.current); recoveryTimerRef.current = null; }
      if (toastTimer.current) { clearTimeout(toastTimer.current); toastTimer.current = null; }
      // Очередь склейки вывода: неотрисованные кадры умирают вместе со страницей.
      // Позиция resume их не считает показанными (appliedOffsetRef двигается
      // только из write-колбэка xterm), поэтому потерянного вывода тут больше нет.
      cancelFlushSchedule();
      writeQueueRef.current = [];
      queueBytesRef.current = 0;
      scrollbackEraseGateRef.current.invalidate();
      parserCarryRef.current.clear();
      if (snapshotBarrierTimerRef.current != null) { clearTimeout(snapshotBarrierTimerRef.current); snapshotBarrierTimerRef.current = null; }
      snapshotBarrierRef.current = false;
      const ws = wsRef.current;
      if (ws) {
        wsRef.current = null;
        ws.onopen = ws.onmessage = ws.onclose = ws.onerror = null;
        try { ws.close(); } catch { /* ignore */ }
      }
      terminalRef.current = null;
      fitRef.current = null;
      terminalWriterRef.current!.detach();
      term.dispose();
    };
  }, [connect, sendResize]);

  useEffect(() => {
    try { localStorage.setItem("pty.fontSize", String(fontSize)); } catch { /* private mode */ }
    const term = terminalRef.current;
    if (!term) return;
    term.options.fontSize = fontSize;
    trace("geom", { fontSize }, "font");
    // Шрифт меняет и ширину, и высоту ячейки — считаем заново, но высоту при
    // клавиатуре по-прежнему держим логическую (fitLocal).
    fitLocalRef.current();
    setTermSize({ cols: term.cols, rows: term.rows });
    sendResize();
  }, [fontSize, sendResize]);

  // Refit terminal on visualViewport changes (keyboard open/close, rotation,
  // status-bar appearance). ResizeObserver alone misses some Android WebView
  // cases where layout viewport stays the same while visualViewport shrinks.
  useEffect(() => {
    let raf: number | null = null;
    // Высота, с которой мы в последний раз согласились менять размер PTY.
    // Прокрутка на телефоне прячет панель браузера/Telegram: высота прыгает на
    // 50–60 px, это 2–4 строки. Раньше каждый такой прыжок уходил агенту как
    // новый размер — а TUI на resize перерисовывает окно ЦЕЛИКОМ. Замер на
    // живой сессии Kimi: +58 px → 90 КБ перерисовки, внутри дважды «стереть
    // экран» и дважды «стереть историю» (жалоба 31.07 «улетает в самый верх»).
    // Поэтому мелкую разницу по высоте игнорируем: пара строк снизу — цена
    // куда меньшая, чем потерянный вывод. Клавиатура и поворот меняют высоту
    // сотнями пикселей и порог проходят.
    const HEIGHT_NOISE_PX = 120;
    let lastHeight = typeof window !== "undefined"
      ? Math.round(window.visualViewport?.height ?? window.innerHeight)
      : 0;
    let lastWidth = typeof window !== "undefined"
      ? Math.round(window.visualViewport?.width ?? window.innerWidth)
      : 0;
    const refit = (opts?: { force?: boolean }) => {
      if (raf != null) cancelAnimationFrame(raf);
      raf = requestAnimationFrame(() => {
        const term = terminalRef.current;
        const fit = fitRef.current;
        if (!term || !fit) return;
        const h = Math.round(window.visualViewport?.height ?? window.innerHeight);
        const w = Math.round(window.visualViewport?.width ?? window.innerWidth);
        const widthChanged = Math.abs(w - lastWidth) >= 2;
        const heightJump = Math.abs(h - lastHeight);
        const wasKeyboard = keyboardOpenRef.current;

        // ── Клавиатура ───────────────────────────────────────────────────────
        // Размер PTY при ней не трогаем вовсе: терминал остаётся прежним, а
        // видимое окно показывает его низ. Признак — общий с наблюдателем
        // контейнера (updateKeyboardMode), иначе они разъезжаются.
        const keyboard = opts?.force ? false : updateKeyboardModeRef.current();
        if (keyboard) {
          lastHeight = h;
          applyKeyboardPeekRef.current();
          return;
        }
        if (wasKeyboard && !opts?.force) {
          // Клавиатура только что ушла. Коробка вернулась к прежней, а xterm мы
          // не трогали — значит агенту слать нечего, и круг «открыл-написал-
          // закрыл» стоит НОЛЬ перерисовок. Но пока клавиатура была поднята,
          // вёрстка могла перестроиться (вырос ряд клавиш, приехала плашка),
          // поэтому расхождение больше одной строки всё же считаем честно.
          lastHeight = h;
          const m = applyKeyboardPeekRef.current();
          const rowH = term.rows > 0 && m ? m.drawn / term.rows : 0;
          // ST-08 (T-31): плашка, приехавшая под клавиатурой, — перекрытие:
          // коробка вместе с ней равна докладиатурной, вместимость прежняя.
          const occludedPx = features.occlusion ? measureOccluderPxRef.current() : 0;
          if (m && Math.abs(m.drawn - (m.inner + occludedPx)) <= Math.max(rowH, 1)) return;
        }

        if (!opts?.force && !widthChanged && heightJump < HEIGHT_NOISE_PX) {
          // Панель браузера уехала/вернулась — терминал не трогаем вовсе:
          // ни fit(), ни resize. Иначе агент перерисует экран и выбросит
          // человека из того места вывода, которое он читал.
          return;
        }
        lastHeight = h;
        lastWidth = w;
        trace("geom", { vpW: w, vpH: h, force: !!opts?.force }, "window");
        fitLocalRef.current();
        setTermSize((prev) => (
          prev.cols === term.cols && prev.rows === term.rows ? prev : { cols: term.cols, rows: term.rows }
        ));
        sendResize();
      });
    };
    const onWindowResize = () => refit();
    // Поворот и возврат из фона — настоящая смена геометрии: и базовую высоту
    // вьюпорта, и режим клавиатуры считаем заново, иначе после поворота с
    // поднятой клавиатурой остались бы старый сдвиг и чужая база.
    const onOrientation = () => {
      keyboardOpenRef.current = false;
      baseViewportRef.current = 0;
      applyKeyboardPeekRef.current();
      refit({ force: true });
    };
    window.addEventListener("resize", onWindowResize);
    window.addEventListener("orientationchange", onOrientation);
    // Вернулись на передний план — отдаём свой размер: пока страница была в
    // фоне, мы молчали (см. sendResize), и на общем PTY мог остаться размер
    // другого экрана.
    // Возврат из фона и поворот — настоящая смена геометрии: размер отдаём
    // принудительно, мимо порога (пока экран был в фоне, на общем PTY мог
    // остаться размер другого клиента).
    const onVisible = () => {
      if (document.visibilityState !== "visible") return;
      keyboardOpenRef.current = false;
      baseViewportRef.current = 0;
      applyKeyboardPeekRef.current();
      refit({ force: true });
    };
    document.addEventListener("visibilitychange", onVisible);
    const vv = window.visualViewport;
    const onVvResize = () => refit();
    vv?.addEventListener("resize", onVvResize);
    // vv "scroll" НЕ слушаем: это панорама вьюпорта (клавиатура/пинч), размер
    // терминала при ней не меняется, а событие идёт серией — каждый срабатывавший
    // здесь refit гонял лишний resize в общий PTY и перерисовывал TUI агента.
    return () => {
      if (raf != null) cancelAnimationFrame(raf);
      window.removeEventListener("resize", onWindowResize);
      window.removeEventListener("orientationchange", onOrientation);
      document.removeEventListener("visibilitychange", onVisible);
      vv?.removeEventListener("resize", onVvResize);
    };
  }, [sendResize]);

  // Первичное состояние — сразу, не дожидаясь WebSocket: REST доступен раньше,
  // и когда приходишь с главной по кнопке «Ответить», вопрос агента должен быть
  // виден уже на «Подключение к терминалу…», а не после установки сокета.
  useEffect(() => {
    if (!id) return;
    let cancelled = false;
    const ticket = statePollGateRef.current!.begin();
    const askedAt = performance.now();
    const terminalContext = getTerminalContextKey();
    getPtyState(id)
      .then((d) => {
        // A newer state poll may win the gate; the confirmed user visit still counts.
        if (!cancelled && d.alive && terminalContext === getTerminalContextKey()) {
          rememberTerminal(localStorage, terminalContext, id);
        }
        // Мимо фильтра свежести: опрос может обогнать этот ответ, и тогда
        // сон «при открытии» терялся (см. noteFirstState).
        if (!cancelled) noteFirstState(d);
        if (!cancelled && statePollGateRef.current!.accept(ticket)) {
          stateAskedAtRef.current = askedAt;
          setState(d);
        }
      })
      .catch((e: any) => {
        if (!cancelled && statePollGateRef.current!.accept(ticket) && e?.status === 404) {
          sessionMissingRef.current = true;
          setSessionMissing(true);
          setGaveUp(true);
          setShowConnecting(false);
        }
      });
    return () => { cancelled = true; };
  }, [id]);

  // Poll live state (cwd + foreground process) every 2s while connected.
  useEffect(() => {
    if (!id || !connected) return;
    let cancelled = false;
    const tick = async () => {
      const ticket = statePollGateRef.current!.begin();
      const askedAt = performance.now();
      try {
        const d = await getPtyState(id);
        if (!cancelled) noteFirstState(d);
        if (!cancelled && !document.hidden && statePollGateRef.current!.accept(ticket)) {
          stateAskedAtRef.current = askedAt;
          setState(d);
        }
      } catch (e: any) {
        if (!cancelled && statePollGateRef.current!.accept(ticket) && e?.status === 404) {
          sessionMissingRef.current = true;
          setSessionMissing(true);
          setGaveUp(true);
          setReconnecting(false);
        }
      }
    };
    const stop = foregroundTask(tick, 2000);
    return () => { cancelled = true; stop(); };
  }, [id, connected]);

  // Route-keyed runtime cleanup: rejects every promise callback that outlives
  // this exact component instance (including React StrictMode's first pass).
  useEffect(() => () => statePollGateRef.current?.invalidate(id || ""), [id]);

  // Предупреждение о перезапуске агента. Компьютер шлёт `agent_updating` всем
  // живым сеансам ПЕРЕД тем, как применить обновление (warnLiveSessions в
  // cmd/tgcontrol/autoupdate.go) — до этой правки событие не читал никто, и
  // единственным сигналом был обрыв терминала посреди работы.
  useEffect(() => onWSEvent((e) => {
    const raw = e as { type?: string; version?: unknown };
    if (raw.type !== "agent_updating") return;
    pcUpdatingAtRef.current = Date.now();
    setPcUpdating(typeof raw.version === "string" ? raw.version : "");
  }), []);

  // Плашка «агент закончил». finished от агентского PTY несёт duration_ms —
  // длительность завершившегося эпизода работы. Ложные срабатывания отсекает
  // сервер (устойчивая тишина после эпизода, не чаще одного finished на
  // эпизод); здесь только фильтруем чужие терминалы, finished без длительности
  // (обычная команда) и старый реплей после переподключения — плашка про
  // «только что», минутная свежесть как у системных уведомлений.
  useEffect(() => {
    const off = onWSEvent((e) => {
      const raw = e as {
        type?: string; event?: string; pty_id?: string;
        agent?: string; name?: string; ts?: number; duration_ms?: unknown;
      };
      if (raw.type !== "pty_event" || raw.event !== "finished") return;
      if (!id || raw.pty_id !== id) return;
      if (typeof raw.duration_ms !== "number" || raw.duration_ms <= 0) return;
      if (typeof raw.ts === "number" && Date.now() - raw.ts > 60_000) return;
      const who = raw.agent && isAgentKind(raw.agent)
        ? agentDisplayName(raw.agent)
        : raw.name?.trim() || t("nav.terminal");
      if (agentDoneTimer.current) clearTimeout(agentDoneTimer.current);
      setAgentDone({ who, ms: raw.duration_ms });
      agentDoneTimer.current = setTimeout(() => setAgentDone(null), 9000);
    });
    return () => {
      off();
      if (agentDoneTimer.current) clearTimeout(agentDoneTimer.current);
    };
  }, [id]);

  // Что со связью до самого ПК — из того же источника, что кормит верхнюю
  // полосу. Повторяем и её правило появления: причину она называет сразу, а
  // безымянный обрыв показывает только пережившим паузу
  // (CONNECTION_BANNER_DELAY_MS). Так экран терминала молчит ровно те секунды,
  // когда говорит полоса: на коротком блипе единственным сообщением остаётся
  // оверлей терминала, при настоящем обрыве — одна полоса сверху.
  useEffect(() => {
    let timer: ReturnType<typeof setTimeout> | null = null;
    const clear = () => { if (timer) { clearTimeout(timer); timer = null; } };
    const off = onConnectionChange((s) => {
      clear();
      if (s.connected) { setLinkSpeaks(false); return; }
      if (s.reason === "pc_offline") { setLinkSpeaks(true); return; }
      timer = setTimeout(() => { timer = null; setLinkSpeaks(true); }, CONNECTION_BANNER_DELAY_MS);
    });
    return () => { off(); clear(); };
  }, []);

  // Живой агент в foreground — для обработчиков со стабильным тождеством.
  useEffect(() => {
    agentInFgRef.current = isAgentKind(state.agent_kind);
    agentKindRef.current = state.agent_kind || "";
  }, [state.agent_kind]);

  // ЛОГИЧЕСКАЯ высота приходит с компьютера: `/state` отдаёт размер,
  // ПРИМЕНЁННЫЙ к PTY. Своей меркой её не восстановить — с поднятой
  // клавиатурой мы видим 9–11 строк из тридцати, и именно этим клиент себя и
  // ужимал. Узнав настоящую, сразу приводим себя в порядок.
  useEffect(() => {
    const rows = state.rows ?? 0;
    if (rows < 2) return;
    logicalRowsRef.current = rows;
    authoritativeRowsRef.current = rows;
    const size = fitLocalRef.current();
    if (size) setTermSize(size);
    // Сервер подтвердил высоту PTY: кадр чужой высоты стоит спросить ещё раз.
    recoveryApiRef.current.serverGrid("state-rows");
  }, [state.rows]);

  // ЛОГИЧЕСКАЯ ширина — тоже с компьютера (`state.cols`, размер, применённый к
  // PTY). PTY общий и идёт по самому узкому зрителю: без этого зажатия широкий
  // зритель (ПК рядом с телефоном) отвергал каждый кадр в 48 колонках и просил
  // свежий — бесконечно (внешний аудит 2.57.18, находка P0-05). Узкий зритель
  // ушёл — авторитетная ширина вырастет, зажатие снимется, и refit вернёт нам
  // честную ширину коробки.
  useEffect(() => {
    const cols = state.cols ?? 0;
    if (cols < 2) return;
    if (logicalColsRef.current === cols) return;
    logicalColsRef.current = cols;
    const size = fitLocalRef.current();
    if (size) setTermSize(size);
    recoveryApiRef.current.serverGrid("state-cols");
  }, [state.cols]);

  // ⚠ СЕТКА ИЗ КАДРА БЕЗ ЯКОРЯ /state (ST-08, ревью скептика). Кадр на запрос
  // после доставленной вместимости сам пишет авторитетную сетку (adopt в
  // обработчике кадра), а два эффекта выше срабатывают только на ИЗМЕНЕНИЕ
  // значения /state. PTY вернулся к прежнему размеру, пока вкладка была скрыта
  // (опроса /state там нет), — /state промежуточного не видел, и сетка залипала
  // в чужом размере: компьютер 47 колонок при PTY 123 после ухода телефона, на
  // проводе после показа вкладки пусто. Правило — geometry.reconcileAdoptedGrid:
  // первый /state, спрошенный ПОСЛЕ приёма, сверяет сетку; расходится —
  // просим ОДИН свежий кадр, а сетку по /state не трогаем: кадр на запрос после
  // доставленной вместимости сам принесёт сетку PTY (adopt), совпавший просто
  // запишется. ⚠ Первая редакция брала сетку из /state — на моке A01 (кадры 20
  // строк при /state 30) она качалась 20→30→20 раз в опрос, каждый resize
  // отменял жест, и навигационные пробы падали 2 из 2.
  // Эффект стоит ПОСЛЕ state.rows/state.cols и срабатывает на каждый принятый
  // ответ: смену значения они уже отработали, здесь остаётся только «значение
  // прежнее, а сетка чужая». Без capacity сетку из кадра не принимают вовсе.
  useEffect(() => {
    if (!features.capacity) return;
    const cols = state.cols ?? 0;
    const rows = state.rows ?? 0;
    const check = reconcileAdoptedGrid({
      adoptedAt: gridAdoptedAtRef.current, stateAskedAt: stateAskedAtRef.current,
      stateCols: cols, stateRows: rows,
      gridCols: logicalColsRef.current, gridRows: authoritativeRowsRef.current,
    });
    if (check.consumed) gridAdoptedAtRef.current = null;
    if (!check.rows && !check.cols) return;
    // Только размеры, без содержимого (I-15).
    sendDiag("snapshot-grid-state", {
      grid: `${logicalColsRef.current}x${authoritativeRowsRef.current}`, state: `${cols}x${rows}`,
    });
    // Кадр — через координатор (S3): причина по оси, его троттл ширины и
    // degraded ограничивают повтор; своих таймеров здесь нет.
    if (features.recoveryV1) recoveryApiRef.current.demand(check.cols ? "geometry-width" : "geometry-height");
    else requestScreenFrameRef.current(undefined, "state-grid");
  }, [state]);

  // Поля /state, которых у агента 2.71.1 нет, доказывают новый агент: ему уходят
  // и новые виды diag (волна 4, ptyTerm/diagCompat.ts).
  useEffect(() => {
    if (stateShowsNewAgent(state as unknown as Record<string, unknown>)) peerNewRef.current = true;
  }, [state.fg_started, state.history_retention]);

  // Сменился передний процесс — замер потока начинается заново: вердикт «чья
  // история» принадлежит ПРИЛОЖЕНИЮ, а не терминалу. Иначе оболочка, которая
  // напечатала полсотни строк перед запуском Claude, навсегда оставила бы
  // терминал «владельцем истории», и разговор Claude листать было бы нечем
  // (повторный аудит 2.57.12, T25712-02). Заодно обнуляем остаток страничного
  // накопителя: он принадлежал прошлому приложению.
  useEffect(() => {
    const next = scrollClassifierKey({
      fgPid: state.fg_pid,
      agentKind: state.agent_kind,
      remote: state.remote,
    });
    const reset = shouldResetScrollClassifier(scrollClassifierKeyRef.current, next);
    if (next) {
      scrollClassifierKeyRef.current = next;
      // Поколение процесса (время его старта) — часть области действия
      // свидетельств: PID без поколения не ключ (ST-02). Старый агент поля не
      // шлёт — тогда 0, и область держится одного PID, как раньше.
      fgStartedRef.current = Math.max(0, Math.trunc(Number(state.fg_started) || 0));
    }
    // Ключ процесса известен — сверить область действия и вернуть наблюдения,
    // запомненные для ЭТОГО процесса (scrollProbeMemory.ts).
    if (!reset) { restoreProbeVerdictRef.current(); return; }
    cancelGestureRef.current();
    streamLinesRef.current = 0;
    streamBytesRef.current = 0;
    // Замер начался заново БЕЗ маркера потока: трасса сама смены переднего
    // процесса не видит, и без этой строки объём замера в diag alt-scroll*
    // оставался бы огрублённым до ближайшего reset (ST-01, волна 9).
    if (features.trace) traceRef.current!.noteStreamSampleReset();
    streamOwnerRef.current = "unknown";
    pageAccumRef.current = 0;
    pageBudgetRef.current = MAX_PAGES_PER_GESTURE;
    // Наблюдения и закрепление чтения принадлежали прошлому приложению: новый
    // процесс на PgUp может отвечать иначе (Kimi молчит, Claude листает).
    // Сверка области ниже сотрёт их; отложенные наблюдения гасим здесь.
    cancelScrollObservations();
    altScrollProbeTokenRef.current++;
    altScrollQueuedActionsRef.current = [];
    readingPinRef.current = null;
    if (pageIdleTimerRef.current != null) {
      clearTimeout(pageIdleTimerRef.current);
      pageIdleTimerRef.current = null;
    }
    restoreProbeVerdictRef.current();
  }, [state.fg_pid, state.fg_started, state.agent_kind, state.remote]);

  // Политика хранения ПОКОЛЕНИЯ (ST-04). Ключ поколения — тот же, что у
  // области свидетельств навигации (процесс + время его старта), второго ключа
  // «чья история» не заводим. Объявление — из state этого терминала
  // (history_retention, основной путь), запасной путь — кэш реестра. Меняется
  // только вместе с процессом или объявлением, никогда — от режима навигации,
  // замера плотности или жеста.
  useEffect(() => {
    const raw = state.history_retention;
    // Третий, последний путь — встроенный ответ для известных агентов (волна 4):
    // агент 2.71.1 поля не шлёт нигде, и Codex терял прокрутку на перерисовке.
    const declared = raw === "honor" || raw === "preserve"
      ? raw : declaredRetention(state.agent_kind) || builtinRetention(state.agent_kind);
    const next = retentionFor({
      generation: `${scrollClassifierKeyRef.current}@${fgStartedRef.current}`,
      agentInFg: isAgentKind(state.agent_kind),
      declared,
    });
    const prev = retentionRef.current;
    if (prev.generation === next.generation && prev.erase === next.erase && prev.source === next.source) return;
    retentionRef.current = next;
    if (prev.generation !== next.generation) retentionGenRef.current++;
    trace("erase", { erase: next.erase, source: next.source, gen: retentionGenRef.current }, "retention");
    // Отложенное стирание прежнего процесса не переносится в поколение, чья
    // политика его не держит (preserve): это и есть граница поколения. shadow
    // ничего не снимает, а только сообщает — и лишь когда снимать было что.
    const boundary = generationDropsPendingErase(features.retention, next);
    if (boundary.use) invalidatePendingScrollbackEraseRef.current();
    else if (boundary.diverged && parserCarryRef.current.hasAnyErase) noteRetentionShadow("generation", "keep", "drop", false);
  }, [state.agent_kind, state.fg_pid, state.fg_started, state.history_retention, state.remote]);

  // Бейдж активности: агент «работает», пока шлёт вывод; «готов» после паузы.
  // Опрос таймером (не в onmessage), чтобы не плодить ререндеры на каждый байт.
  useEffect(() => {
    const stop = foregroundTask(() => {
      const busy = Date.now() - lastOutputRef.current < 1500;
      setAgentBusy((prev) => (prev === busy ? prev : busy));
      // Четвёртое состояние: ввод ушёл, а в ответ НИ ОДНОГО байта.
      //
      // 01.09.2026 у владельца зависли две сессии Claude Code — процесс жив,
      // экран пуст, клавиши игнорируются, — а бейдж писал «Готов». Разбор занял
      // час, и весь этот час экран уверял, что всё в порядке.
      //
      // Порог 8 секунд намеренно велик: живой агент отвечает эхом мгновенно
      // (рисует набранный символ), так что молчание такой длины после ввода —
      // это уже не задержка, а тишина. Считаем только ввод ЧЕЛОВЕКА: прокрутка
      // краевой командой у агента бывает законным no-op (см. edgeAction), и по
      // ней «не отвечает» показывалось бы на исправном терминале.
      const inputAt = lastUserInputAtRef.current;
      const stuck = inputAt > 0
        && lastOutputRef.current < inputAt
        && Date.now() - inputAt > AGENT_SILENCE_MS;
      setAgentStuck((prev) => (prev === stuck ? prev : stuck));
    }, 250);
    return stop;
  }, []);

  /**
   * Тап по выводу терминала = «хочу писать ПРЯМО СЮДА».
   *
   * Правило переписывалось трижды, каждый раз по живому слову владельца, и
   * держать историю тут важнее обычного — иначе следующий агент снова примет
   * прошлую жалобу за действующее правило.
   *
   * 08.09 (2.68.0) подъём клавиатуры по тапу убрали совсем: человек тапал,
   * чтобы читать, и получал системную клавиатуру поверх панели («когда
   * клавиатуру включаешь, он что-то свою выдвигает», iPhone).
   * 10.09 утром вернули подъём, но фокусом в наше нижнее поле: клавиатура
   * открывалась, а печатал человек в `pty-text-input`.
   * 10.09 вечером владелец уточнил, чего хотел на самом деле: «когда я туда
   * нажимаю, тоже мог туда вводить именно клавиатуру телефона». То есть тап по
   * выводу должен давать ввод В САМ ТЕРМИНАЛ, как в любом SSH-клиенте.
   *
   * Поэтому фокус уходит в xterm на всех поверхностях, и `dropXtermFocus`
   * снят. ⚠ Цена известна и принята: при прямом вводе в PTY Gboard теряет
   * автозамену и предиктивный ввод, фраза уходит агенту посимвольно. Кому
   * нужен нормальный набор — тапает по нижнему полю, оно никуда не делось и
   * после отправки фокус возвращается именно туда (handleInputSend).
   *
   * Прокрутке это не мешает: обработчик висит на `onClick`, а свайп пальцем
   * клика не даёт.
   */
  const handleTap = () => {
    if (selectMode || Date.now() < suppressTerminalTapUntilRef.current) {
      trace("input", { select: selectMode }, "tap-ignored");
      return;
    }
    terminalRef.current?.focus();
  };

  // Unified, alt-screen-aware scroll, shared by the finger-drag gesture AND the
  // ⇈↑↓⇊ buttons. On the normal screen we drive xterm's own scrollback. In an
  // alt-screen app (Claude Code, vim, htop) there is NO terminal scrollback, so
  // we forward the scroll to the app: mouse-aware TUIs scroll their own output
  // via SGR wheel events; plain pagers (less/man) take arrow keys. lines<0 = up
  // (older content). A one-shot diag frame reports the live mode to the backend
  // so we can confirm in remotai.log whether the app is mouse-tracked and
  // actually honours the wheel (otherwise scrolling a full-screen TUI is the
  // app's own responsibility, not something the client can force).
  // Тап пальцем → клик мыши тому приложению, которое сам за мышью и следит.
  // Ссылка через ref: обработчик жеста живёт в эффекте и не должен зависеть от
  // тождества колбэка.
  const sendTapAsClick = useCallback((ev: TouchEvent) => {
    const term = terminalRef.current;
    const ws = wsRef.current;
    if (!term || ws?.readyState !== WebSocket.OPEN) return;
    // ST-06: на экране удерживается СТАРАЯ картинка (транзакция показа), а
    // буфер и режимы мыши уже новые — клик ушёл бы по чужим координатам.
    if (presentHolding()) { trace("input", { hold: true }, "tap-suppressed"); return; }
    let mouse = "none";
    try { mouse = (term as any).modes?.mouseTrackingMode || "none"; } catch { /* ignore */ }
    // Обычной оболочке отчёт о клике не нужен: он превратится в мусор в строке
    // ввода. Достаточный и честный признак — приложение САМО включило слежение
    // за мышью.
    //
    // ⚠ Требования `alt === true` здесь БОЛЬШЕ НЕТ. Claude Code рисует в обычном
    // буфере, но мышь запрашивает — и его плашка «N new messages», пункты меню
    // разрешений и свёрнутый вывод пальцем не нажимались вовсе (повторный аудит
    // 2.57.12, T259-04). Alt-screen про историю, а не про мышь.
    if (mouse === "none") return;
    const t = ev.changedTouches?.[0];
    // Координаты считаем по САМОМУ ЭКРАНУ терминала, а не по контейнеру:
    // у контейнера есть отступы и наши накладки, и клик уезжал на строку-другую.
    const el = termRef.current;
    if (!t || !el) return;
    const geometry = measureTerminalCoordinates(el, term.cols, term.rows);
    if (!geometry) return;
    const cell = cellAt(geometry, t.clientX, t.clientY);
    const col = cell.col + 1, row = cell.row + 1;
    try {
      ws.send(new TextEncoder().encode(clickSeq(col, row, term.cols, term.rows)));
      // ST-10: клик ушёл — совместимые mousedown/mouseup, которые браузер
      // пришлёт следом за этим касанием, xterm не должен превратить во второй.
      tapClickSentAtRef.current = performance.now();
    } catch { /* ignore */ }
  }, []);
  const tapClickSentAtRef = useRef(-Infinity);
  const sendTapAsClickRef = useRef(sendTapAsClick);
  sendTapAsClickRef.current = sendTapAsClick;

  // Спуск накопленной прокрутки alt-screen — объявлен через ref по тому же
  // образцу, что scrollByLinesRef: scrollByLines зовёт его, будучи объявленным
  // раньше (та же TDZ-грабля, что с browserTouch).
  const flushAltScrollRef = useRef<() => void>(() => {});
  // Чья история прокрутки прямо сейчас. Единственная точка решения: её же
  // спрашивают палец, кнопки ⇈/⇊, «Вернуться к новому» и колесо на ПК. Правило —
  // в ptyTerm/altScroll.ts (проверяется тестом без React и без xterm).
  /**
   * Крутить ли СВОЮ прокрутку этим жестом.
   *
   * ⚠ Мало того, что ей есть куда двигаться, — надо ещё, чтобы она была
   * человеку нужна. У сессии с историей ВНУТРИ агента наша прокрутка состоит из
   * обрывков его перерисовок, и человек, свайпая, листал этот мусор, упирался в
   * его верх и до настоящей истории не доходил («и дальше не скролит»,
   * 14.08.2026, разобрано прямо на телефоне владельца). Правило — в
   * ptyTerm/altScroll.ts, здесь только применение к живому терминалу.
   */
  // Слабая эвристика «чья история» без поправки на ручной режим: замер
  // плотности с гистерезисом у порога (A02, T-04) и глубина своей истории.
  // Решает только там, где нет ни объявленного канала, ни наблюдения.
  // commit=false — запрос без намерения (ряд инструментов, ST-10): тот же ответ,
  // но гистерезис потока не сдвигается от одного лишь взгляда на кнопку.
  const autoHistoryOwner = useCallback((term: Terminal, commit = true): HistoryOwner => {
    let alt = false;
    try { alt = (term.buffer.active as any).type === "alternate"; } catch { /* ignore */ }
    const stream = historyOwnerFromStreamStable(streamOwnerRef.current, streamLinesRef.current, streamBytesRef.current);
    if (commit && stream !== "unknown") streamOwnerRef.current = stream;
    return resolveHistoryOwner({ stream, ownScrollback: normalBaseY(term), agent: agentInFgRef.current, alt });
  }, []);
  const historyOwner = useCallback((term: Terminal): HistoryOwner =>
    effectiveHistoryOwner(autoHistoryOwner(term), scrollOverrideRef.current), [autoHistoryOwner]);
  const historyOwnerRef = useRef(historyOwner);
  historyOwnerRef.current = historyOwner;

  /** Свидетельство канала для направления жеста — в момент решения (ST-02). */
  const evidenceFor = (channel: NavChannel, lines: number, rows: readonly string[]): EvidenceDecision =>
    evidenceRef.current.evaluate(channel, { now: Date.now(), direction: directionOf(lines), rows });
  const evidenceForRef = useRef(evidenceFor);
  evidenceForRef.current = evidenceFor;

  /**
   * Кто исполняет жест — одно правило до любого побочного эффекта
   * (ptyTerm/navigationDecision.ts). Здесь только сбор входов с живого
   * терминала: объявленный реестром канал, свидетельства, закрепление чтения.
   */
  const navShadowRef = useRef("");
  /**
   * peek=true — вопрос «что решило бы правило», без намерения человека (ряд
   * инструментов показывает доступность «↑ К команде», ST-10). Тот же вход и
   * то же правило, но состояние правила не двигается: ни сверки области, ни
   * снятия закрепления, ни гистерезиса потока, ни теневой записи. Решение с
   * побочными эффектами принимается только на жест или нажатие.
   */
  const decideFor = useCallback((term: Terminal, lines: number, peek = false): ScrollDestination => {
    // Сверка области действия: дешёвое сравнение строки, а при смене процесса
    // или режима — сброс чужих наблюдений до того, как они повлияют на жест.
    if (!peek) restoreProbeVerdictRef.current();
    let alt = false;
    try { alt = term.buffer.active.type === "alternate"; } catch { /* ignore */ }
    const rows = captureTerminalRows(term, "screen");
    // A restored refusal has no persisted screen text. Baseline it only now:
    // mode callbacks can restore mid-replay, and toolbar peeks are not intent.
    if (!peek) evidenceRef.current.anchorRestoredScreen(rows);
    const page = evidenceForRef.current("page", lines, rows);
    const wheel = evidenceForRef.current("wheel", lines, rows);
    const override = scrollOverrideRef.current;
    let pin = readingPinRef.current;
    if (pin) {
      let atLiveBottom = true;
      try { atLiveBottom = term.buffer.active.viewportY >= term.buffer.active.baseY; } catch { /* ignore */ }
      if (override !== "auto" || shouldDropPin(pin, { atLiveBottom, page, wheel })) {
        pin = null;
        if (!peek) readingPinRef.current = null;
      }
    }
    const auto = autoHistoryOwner(term, !peek);
    const input = {
      override,
      pin,
      alt,
      mouse: term.modes.mouseTrackingMode || "none",
      agent: agentInFgRef.current,
      declaredChannel: declaredScrollChannel(agentKindRef.current),
      localCanScroll: localCanScroll(term, lines),
      towardLive: lines > 0,
      owner: auto,
      page,
      wheel,
    };
    const decision = decideNavigation(input);
    const owner = effectiveHistoryOwner(auto, override);
    // Переключатель отката и тень (план 9.1): режим читается при открытии
    // терминала, так что переключение происходит на границе нового экрана.
    if (features.navigation !== "v2") {
      const legacy = legacyDecision(input, owner);
      if (!peek && features.navigation === "shadow") {
        const diff = navigationShadowDiff(decision, legacy);
        if (diff && diff !== navShadowRef.current) sendDiag("nav-shadow", { diff });
        navShadowRef.current = diff;
      }
      return { decision: legacy, owner };
    }
    return { decision, owner };
  }, [autoHistoryOwner, features.navigation]);
  const decideForRef = useRef(decideFor);
  decideForRef.current = decideFor;
  // «Крутить ли свою историю этим жестом» — тот же ответ единого правила
  // (для действий вне жеста, например перехода к началу команды, ST-10).
  const shouldDriveViewportRef = useRef((term: Terminal, lines: number, peek = false) =>
    decideForRef.current(term, lines, peek).decision.executor === "local");

  scrollIdentityRef.current = () => {
    const live = terminalRef.current;
    return JSON.stringify([connectionGenRef.current, writerEpochRef.current,
      scrollClassifierKeyRef.current, scrollOverrideRef.current,
      live?.buffer.active.type, live?.modes.mouseTrackingMode,
      geometryRevisionRef.current, live?.cols, live?.rows]);
  };

  // Сколько неотправленных действий ждут вердикта пробы (очередь ограничена,
  // ST-03). Докуда досматривать поздний ответ приложения (T-08: 1,5–3 с плюс
  // доставка) — navigationEvidence.ts, nextLateAnswerCheck.
  const ALT_SCROLL_QUEUE_MAX = 8;
  // Сколько приложение должно молчать перед отправкой, чтобы большой repaint в
  // ответ считался ответом страницы, а не совпадением с его же перерисовкой (T-06).
  const ALT_SCROLL_QUIET_MS = 1000;

  /**
   * Режим отправки удалённого действия, пересчитанный по свидетельству НА
   * МОМЕНТ исполнения: действие, ждавшее в очереди, могло устареть. null —
   * канал в этом направлении сейчас молчит, и действие не исполняется вовсе:
   * другим способом его выполнить нельзя (I-01), а новое намерение выберет
   * источник заново.
   */
  const modeForChannel = (channel: AltScrollChannel, lines: number): NavMode | null => {
    if (channel !== "page" && channel !== "wheel") return "send";
    if (scrollOverrideRef.current === "agent") return "send";
    const live = terminalRef.current;
    const ev = evidenceForRef.current(channel, lines, live ? captureTerminalRows(live, "screen") : []);
    return ev === "use" ? "send" : ev === "verify" ? "verify" : ev === "probe" ? "probe" : null;
  };

  /**
   * One send/probe path for touch, short-touch flush, buttons and wheel.
   *
   * ⚠ ОДНО НАМЕРЕНИЕ — ОДИН ИСПОЛНИТЕЛЬ (план 13.09, I-01, ST-03). Раньше, если
   * приложение не ответило за окно пробы, тот же жест повторялся своей
   * историей. При ответе, пришедшем через 1,5–3 с, одно намерение исполнялось
   * на двух поверхностях сразу. Теперь исполнитель выбирается ДО отправки
   * (navigationDecision.ts); молчание после отправки — только свидетельство
   * для СЛЕДУЮЩЕГО намерения и честный тост человеку.
   */
  const dispatchScrollPayload = useCallback((
    channel: AltScrollChannel,
    data: string,
    lines: number,
    owner: HistoryOwner,
    fallback: ScrollFallback = { kind: "lines", lines },
    ticket: ScrollTicket = scrollRouterRef.current.action(scrollIdentityRef.current()),
    mode: NavMode = "send",
  ) => {
    if (!data || lines === 0 || !scrollRouterRef.current.valid(ticket, scrollIdentityRef.current())) return;
    const term = terminalRef.current;
    const ws = wsRef.current;
    if (!term || ws?.readyState !== WebSocket.OPEN) return;
    const up = lines < 0;
    const edge = fallback.kind === "edge";
    const send = (): boolean => {
      try { ws.send(new TextEncoder().encode(data)); return true; } catch { return false; }
    };
    // Человек ушёл вверх по истории приложения: следующий свайп не должен
    // уехать в свою историю из-за нового вывода или плотности (ST-03).
    // Явный возврат к live (⇊) после выдачи ticket закрепление отменяет: итог
    // действия, начатого до возврата (вердикт пробы кнопки ↑, досылка очереди),
    // чтение не закрепляет (волна 8; ScrollRouter.returnToLive/mayPin).
    const pinRemote = () => {
      if (up && scrollOverrideRef.current === "auto" && (channel === "page" || channel === "wheel")
        && scrollRouterRef.current.mayPin(ticket)) {
        readingPinRef.current = { executor: "remote", channel };
      }
    };
    const navChannel: NavChannel | null = channel === "page" || channel === "wheel" ? channel : null;
    // Режим пересчитывается по свидетельству НА МОМЕНТ фрагмента, а не на начало
    // жеста: длинный drag после вердикта не должен ни держать очередь заново, ни
    // слать новые пробы в замолчавший канал. Замолчал — фрагмент роняется: тот же
    // жест другим способом не исполняется (I-01), новый выберет источник заново.
    if (navChannel && mode !== "send" && scrollOverrideRef.current !== "agent") {
      const current = modeForChannel(channel, lines);
      if (!current) return;
      mode = current;
    }
    // Доказанный канал, ручной «Агент» и пейджерные стрелки уходят без
    // наблюдения: ручной выбор авторитетен, а доказанному нечего доказывать.
    if (!navChannel || mode === "send" || scrollOverrideRef.current === "agent") {
      const ok = send();
      trace("probe", { lines, ok }, `send:${channel}`);
      if (ok) pinRemote();
      return;
    }
    // Одна проба в полёте; следующие фрагменты ждут вердикта неотправленными.
    // Exact edge payloads are preserved: reducing Ctrl+Home/Ctrl+End to a
    // scalar line sum made rapid ⇈/⇊ replay as PgUp/PgDn instead of edges.
    if (altScrollProbeRef.current != null) {
      const queued = altScrollQueuedActionsRef.current;
      const last = queued[queued.length - 1];
      if (
        last
        && last.fallback.kind === "lines"
        && fallback.kind === "lines"
        && last.ticket === ticket
        && last.channel === channel
        && last.owner === owner
        // Opposite directions are not algebraic deltas at an application
        // boundary: Up may be a no-op at the top while the following Down is
        // the user's only way back. Summing them to zero silently lost both.
        && Math.sign(last.lines) === Math.sign(lines)
      ) {
        last.data += data;
        last.lines += lines;
        last.fallback.lines += fallback.lines;
      } else if (queued.length < ALT_SCROLL_QUEUE_MAX) {
        queued.push({ channel, data, lines, owner, fallback, ticket, mode });
      }
      trace("probe", { lines, queued: queued.length }, `queued:${channel}`);
      return;
    }

    const before = captureTerminalRows(term, "screen");
    const quietBefore = Date.now() - lastOutputAtRef.current >= ALT_SCROLL_QUIET_MS;
    const generation = connectionGenRef.current;
    const epoch = writerEpochRef.current;
    const scope = evidenceRef.current.currentScope();
    const holdsQueue = mode === "probe";
    const token = holdsQueue ? ++altScrollProbeTokenRef.current : altScrollProbeTokenRef.current;
    // Слабо подтверждённый канал наблюдаем, только если никто не наблюдает
    // сейчас: два одновременных окна не отличили бы, чей это ответ.
    const observe = holdsQueue || altScrollObserveRef.current == null;
    if (!send()) { trace("probe", { lines, ok: false }, `${mode}:${channel}`); return; }
    // Монотонные часы: досмотр позднего ответа меряется от отправки.
    const sentAt = performance.now();
    trace("probe", { lines, ok: true, observe, quietBefore }, `${mode}:${channel}`);
    if (!holdsQueue) pinRemote();
    if (!observe) return;

    const stillOurs = () => !disposedRef.current
      && generation === connectionGenRef.current
      && epoch === writerEpochRef.current
      && scope === evidenceRef.current.currentScope();
    const redispatch = (action: PendingScrollAction) => {
      const next = modeForChannel(action.channel, action.lines);
      if (next) dispatchScrollPayloadRef.current({ ...action, mode: next });
    };
    const judge = () => {
      if (!stillOurs()) {
        // Соединение, эпоха или область уже другие: неотправленное принадлежало
        // прежним — не оставлять его ждать чужого вердикта (ревью 14.09, P3).
        if (holdsQueue) altScrollQueuedActionsRef.current = [];
        return;
      }
      if (holdsQueue && token !== altScrollProbeTokenRef.current) return;
      const live = terminalRef.current;
      const after = live ? captureTerminalRows(live, "screen") : [];
      const response = screenScrollResponse(before, after, up ? "up" : "down");
      // Свидетельство пишется, даже если жест уже закончился или начался новый:
      // PgUp ушла, и её итог — факт о канале. Иначе при свайпах чаще окна пробы
      // свидетельство не появлялось бы никогда (ревью 14.09, P1). От
      // действительности жеста зависят только его неотправленные фрагменты:
      // dispatch сам отбросит фрагменты недействительного жеста.
      const obs: Observation | null = evidenceRef.current.record(navChannel, {
        kind: response.kind, edge, direction: directionOf(lines), before, after, quietBefore,
      }, Date.now());
      trace("probe", {
        answered: response.answered, changed: response.changedRows, shifted: response.shiftedRows, edge,
      }, `verdict:${navChannel}:${response.kind}`);
      persistProbeVerdictRef.current();
      const ticketLive = scrollRouterRef.current.valid(ticket, scrollIdentityRef.current());
      const queued = holdsQueue ? altScrollQueuedActionsRef.current.splice(0) : [];
      if (response.answered) {
        if (holdsQueue && ticketLive) pinRemote();
        for (const action of queued) redispatch(action);
        return;
      }
      if (!edge) {
        if (channel === "page") {
          sendDiag("alt-scroll-page-dead", {
            owner, lines: streamLinesRef.current, bytes: streamBytesRef.current,
            own: live ? normalBaseY(live) : -1,
            changed: response.changedRows, shifted: response.shiftedRows,
          });
        }
        // Правда человеку: жест ушёл, программа промолчала. Тот же жест своей
        // историей НЕ повторяется; следующий пойдёт туда, где есть что листать.
        if (live && localCanScroll(live, lines)) showToast(t("pty.scrollAppSilent"));
        // Поздний ответ (1,5–3 с у приложения плюс доставка) повышает
        // свидетельство по сдвигу строк в сторону намерения; большой repaint
        // позже окна ничего не доказывает (A03). Досмотр — шагами до края плана
        // с запасом доставки (nextLateAnswerCheck, волна 7): прежняя одна
        // проверка через 3,0 с после отправки опережала ответ на 3,0 с.
        // Штатно досматривается только ↑ (волна 8): на ↓ поздний сдвиг к
        // новому неотличим от обычного вывода и делал молчащий канал «живым»
        // для трёх следующих ↑. Откат navigation ≠ v2 — прежняя одна проверка.
        if (altScrollLateRef.current != null) clearTimeout(altScrollLateRef.current);
        altScrollLateRef.current = null;
        const legacyLate = features.navigation !== "v2";
        const watchLate = () => {
          const wait = nextLateAnswerCheck(sentAt, performance.now(), legacyLate, directionOf(lines));
          if (wait == null) return;
          altScrollLateRef.current = window.setTimeout(() => {
            altScrollLateRef.current = null;
            if (!stillOurs() || evidenceRef.current.get(navChannel) !== obs) return;
            const liveLate = terminalRef.current;
            if (!liveLate) return;
            const late = captureTerminalRows(liveLate, "screen");
            if (screenScrollResponse(before, late, up ? "up" : "down").kind !== "shift") { watchLate(); return; }
            evidenceRef.current.record(navChannel, {
              kind: "shift", edge: false, direction: directionOf(lines), before, after: late,
            }, Date.now());
            persistProbeVerdictRef.current();
            sendDiag("alt-scroll-late-answer", { channel });
          }, wait);
        };
        watchLate();
      }
      // Действия, ждавшие вердикта: режим пересчитывается по новому
      // свидетельству, молчащий канал их роняет (не исполняет другим способом).
      for (const action of queued) redispatch(action);
    };
    if (holdsQueue) {
      altScrollProbeRef.current = window.setTimeout(() => {
        altScrollProbeRef.current = null;
        judge();
      }, ALT_SCROLL_PROBE_MS);
    } else {
      altScrollObserveRef.current = window.setTimeout(() => {
        altScrollObserveRef.current = null;
        judge();
      }, ALT_SCROLL_PROBE_MS);
    }
  }, []);
  dispatchScrollPayloadRef.current = (action) => dispatchScrollPayload(
    action.channel,
    action.data,
    action.lines,
    action.owner,
    action.fallback,
    action.ticket,
    action.mode,
  );

  // Каждая СМЕНА решения навигации — одна строка в журнал агента (без
  // содержимого, I-15): разбирать жалобу «Авто не туда листает» по одной
  // строке за сессию было нечем.
  const navDiagRef = useRef("");
  const noteNavDecision = (d: ScrollDestination) => {
    const { executor, channel, mode, reason } = d.decision;
    const key = `${executor}|${channel}|${mode}|${reason}|${d.owner}`;
    if (key === navDiagRef.current) return;
    navDiagRef.current = key;
    sendDiag("nav", { executor, channel, mode, reason, owner: d.owner });
  };
  const noteNavDecisionRef = useRef(noteNavDecision);
  noteNavDecisionRef.current = noteNavDecision;

  const scrollByLines = useCallback((
    lines: number,
    opts?: { source?: "touch" | "inertia" | "button" | "wheel" },
  ) => {
    const term = terminalRef.current;
    if (!term || lines === 0) return;
    const source = opts?.source ?? "touch";
    // Отметка «человек крутит сам» — якорь чтения (flushTermWrites) по ней не
    // тянет вьюпорт обратно, если сдвиг был от пальца/кнопок, а не от потока.
    userScrollSeqRef.current++;
    // Исполнитель выбирается один раз на намерение (жест, серию колеса,
    // нажатие) и ДО отправки: ScrollRouter закрепляет его за ticket.
    const route = scrollRouterRef.current.route(source, scrollIdentityRef.current(), Date.now(), () => {
      const destination = decideForRef.current(term, lines);
      noteNavDecisionRef.current(destination);
      // Одно событие на НАМЕРЕНИЕ: исполнитель закрепляется за ticket до конца.
      const d = destination.decision;
      trace("route", {
        executor: d.executor, channel: d.channel, mode: d.mode, owner: destination.owner, source, lines,
      }, d.reason);
      return destination;
    });
    if (!route) return;
    if (route.destination.decision.executor === "local") {
      term.scrollLines(lines);
      // Человек ушёл вверх по своей истории — источник чтения закрепляется за
      // ней: новый ответ агента и плотность потока его не переключат (ST-03).
      if (lines < 0 && scrollOverrideRef.current === "auto") {
        readingPinRef.current = { executor: "local", channel: "viewport" };
      }
      return;
    }
    // Never coalesce different intents into the same frame/page accumulator.
    if (pendingScrollRouteRef.current?.ticket !== route.ticket) {
      flushAltScrollRef.current();
      pageAccumRef.current = 0;
      if (pageIdleTimerRef.current != null) clearTimeout(pageIdleTimerRef.current);
      pageIdleTimerRef.current = null;
    }
    pendingScrollRouteRef.current = route;
    // Alt-screen: строки копятся до ближайшего кадра анимации и уходят одним
    // сообщением (см. flushAltScroll). Сам вызов дёшев — его дёргают
    // touchmove и инерция по много раз за кадр.
    altScrollAccumRef.current += lines;
    altScrollInertiaRef.current = source === "inertia";
    if (source === "button") altScrollButtonRef.current = true;
    if (altScrollRafRef.current == null) {
      altScrollRafRef.current = requestAnimationFrame(() => flushAltScrollRef.current());
    }
  }, []);
  scrollByLinesRef.current = scrollByLines;

  // Кадр прокрутки alt-screen. Выбор канала и полезная нагрузка — в
  // ptyTerm/altScroll.ts, чтобы проверяться тестом без React и без xterm
  // (лестницу каналов не меняем). Здесь остаётся только отправка и проба.
  const flushAltScroll = useCallback(() => {
    if (altScrollRafRef.current != null) cancelAnimationFrame(altScrollRafRef.current);
    altScrollRafRef.current = null;
    const lines = altScrollAccumRef.current;
    const fromButton = altScrollButtonRef.current;
    const fromInertia = altScrollInertiaRef.current;
    altScrollAccumRef.current = 0;
    altScrollButtonRef.current = false;
    altScrollInertiaRef.current = false;
    if (lines === 0) return;
    const term = terminalRef.current;
    if (!term) return;
    const route = pendingScrollRouteRef.current;
    if (!route || !scrollRouterRef.current.valid(route.ticket, scrollIdentityRef.current())) return;
    const { decision, owner } = route.destination;
    let alt = false;
    try { alt = term.buffer.active.type === "alternate"; } catch { /* ignore */ }
    const mouseMode = term.modes.mouseTrackingMode || "none";

    const ws = wsRef.current;
    if (ws?.readyState !== WebSocket.OPEN) {
      trace("route", { lines }, "skip-offline");
      // Жест ни во что не превратится, а молчание читалось как «терминал
      // залип». Говорим правду, но не чаще раза в 5 с.
      const offlineAt = Date.now();
      if (offlineAt - scrollOfflineWarnAtRef.current > SCROLL_OFFLINE_WARN_GAP_MS) {
        scrollOfflineWarnAtRef.current = offlineAt;
        showToast(t("pty.scrollOffline"));
      }
      return;
    }
    // Канал забит недоставленным — пропускаем накопленное (см. ALT_SCROLL_BUFFER_CAP).
    if (ws.bufferedAmount > ALT_SCROLL_BUFFER_CAP) {
      trace("route", { lines, buffered: ws.bufferedAmount }, "skip-buffered");
      return;
    }
    // Диагностика — при первом жесте И при КАЖДОЙ смене вердикта: разбирать
    // жалобу «не листается» по одной строке за сессию нечем, а именно так
    // разбирались все прошлые (см. зону 13.08.2026). Числа те же, по которым
    // решение и принято: строки/байты замера и глубина своей прокрутки.
    if (!diagSentRef.current || diagOwnerRef.current !== owner) {
      diagSentRef.current = true;
      diagOwnerRef.current = owner;
      // cols/rows здесь — сетка на момент жеста, не resize: в трассу без них.
      const streamFields = { owner, lines: streamLinesRef.current, bytes: streamBytesRef.current, own: normalBaseY(term) };
      sendDiag("alt-scroll", {
        alt, mouse: mouseMode, cols: term.cols, rows: term.rows, ...streamFields,
      }, { sock: ws, traceFields: { alt, mouse: mouseMode, ...streamFields } });
    }
    // Канал выбран единым правилом в начале намерения (decideFor), здесь только
    // собирается последовательность для него.
    const channel = decision.channel;
    let data = channelPayload(channel, lines, term.cols, term.rows);
    // Страничный канал копится МЕЖДУ кадрами: иначе медленный свайп (по строке
    // за кадр) отправлял бы страницу на каждый кадр — см. pagesFromAccum.
    if (channel === "page" && !fromButton) {
      // ⚠ ИНЕРЦИЯ СТРАНИЦАМИ НЕ ЛИСТАЕТ. Докрутка после отпускания пальца
      // осмысленна для своего вьюпорта — там шаг пиксельный и жест выглядит
      // продолжением. У страничного канала шаг дискретный: те же кадры инерции
      // давали 4–8 страниц сверх потолка «три» (повторный аудит, T25712-03), и
      // человек улетал туда, откуда не находил дороги назад.
      if (fromInertia) { trace("route", { lines }, "skip-inertia-page"); return; }
      // Запас страниц пополняется СО ВРЕМЕНЕМ: жёсткие «три на жест»
      // останавливали длинный свайп посреди движения (см. refillPageBudgetAt).
      // ⚠ Метку двигаем ТОЛЬКО на израсходованное пополнением время: кадры
      // жеста идут каждые 16–50 мс, а страница возвращается раз в 250 мс —
      // иначе остаток выбрасывался на каждом кадре и бюджет не пополнялся
      // никогда (внешний аудит 2.57.18, находка P0-01).
      const nowMs = Date.now();
      const refill = refillPageBudgetAt(pageBudgetRef.current, nowMs - pageBudgetAtRef.current);
      pageBudgetRef.current = refill.budget;
      pageBudgetAtRef.current += refill.consumedMs;
      pageAccumRef.current += lines;
      const { pages, rest } = pagesFromAccum(pageAccumRef.current, term.rows, pageBudgetRef.current);
      pageAccumRef.current = rest;
      if (pageIdleTimerRef.current != null) clearTimeout(pageIdleTimerRef.current);
      pageIdleTimerRef.current = null;
      if (pages === 0) {
        // На страницу ещё не набралось. Ждём продолжения жеста, а если его нет —
        // досылаем ОДНУ страницу: короткий свайп обязан листать, иначе человек
        // видит «не работает». Именно этого не хватило в 2.57.11.
        const dir = pageAccumRef.current;
        if (dir !== 0 && pageBudgetRef.current > 0) {
          pageIdleTimerRef.current = setTimeout(() => {
            pageIdleTimerRef.current = null;
            if (disposedRef.current || !scrollRouterRef.current.valid(route.ticket, scrollIdentityRef.current())) return;
            const live = terminalRef.current;
            if (!live || wsRef.current?.readyState !== WebSocket.OPEN) return;
            pageAccumRef.current = 0;
            if (pageBudgetRef.current <= 0) return;
            pageBudgetRef.current -= 1;
            dispatchScrollPayload(
              "page",
              pageSeq(dir, live.rows),
              dir,
              owner,
              { kind: "lines", lines: dir },
              route.ticket,
              decision.mode,
            );
            if (dir < 0) setAltScrolledUp(true);
          }, 200);
        }
        return;
      }
      pageBudgetRef.current = Math.max(0, pageBudgetRef.current - Math.abs(pages));
      data = pageSeq(pages * Math.max(1, term.rows - 1), term.rows);
    }

    // Ушли вверх — приложение придержит новый вывод, и человеку нужен выход
    // обратно.
    if (channel !== "viewport" && lines < 0) setAltScrolledUp(true);

    if (data) {
      // ОДНИМ сообщением. Раньше здесь стоял цикл `for (…) ws.send(…)`: одно
      // нажатие ⇈ при rows=30 уходило 150 отдельными сообщениями по ~12
      // полезных байт при ~36 байтах накладных — через три плеча до компьютера.
      dispatchScrollPayload(channel, data, lines, owner, { kind: "lines", lines }, route.ticket, decision.mode);
      return;
    }

    const now = Date.now();
    if (now - altScrollWarnAtRef.current <= ALT_SCROLL_WARN_GAP_MS) return;
    altScrollWarnAtRef.current = now;
    // Opening an application's transcript is an explicit toolbar action.
    // A swipe must never trigger a dialog or a hidden application command.
    showToast(t(agentHistoryChannel(agentKindRef.current)?.key ? "pty.historyOwnHint" : "pty.altScrollIgnored"));
  }, [dispatchScrollPayload]);
  flushAltScrollRef.current = flushAltScroll;

  // Native finger scroll over the terminal (replaces tapping ↑/↓ buttons).
  // Tracks one-finger vertical drag; uses term.scrollLines for normal screen,
  // sends wheel/↑↓ for alternate screen apps (Claude Code, vim, less, htop) via
  // scrollByLines. Adds inertia after release.
  useEffect(() => {
    if (selectMode) return; // select-mode owns touch in its own effect
    const el = termRef.current;
    const term = terminalRef.current;
    if (!el || !term) return;

    let startX = 0, startY = 0, lastY = 0, startT = 0, lastX = 0;
    let mode: "idle" | "braking" | "scroll" | "stopped" | "passthrough" | "pan" = "idle";
    let samples: { dy: number; t: number }[] = [];
    let inertiaRaf: number | null = null;
    // Residual sub-row pixels carried between touchmoves (see onMove): Android
    // fires touchmove every 1-5px, far less than one row, so rounding each move
    // on its own floored every step to 0 and a slow drag never scrolled.
    let scrollAccum = 0;
    let rowH = 0;
    let gestureIdentity = "";
    let longPress: ReturnType<typeof setTimeout> | null = null;
    const stopLongPress = () => { if (longPress != null) clearTimeout(longPress); longPress = null; };

    const stopInertia = () => {
      if (inertiaRaf != null) {
        cancelAnimationFrame(inertiaRaf);
        inertiaRaf = null;
      }
    };
    // Alt-screen-aware scroll lives in scrollByLines (shared with the buttons).
    const scrollBy = (lines: number) => scrollByLines(lines, { source: "touch" });

    const onStart = (e: TouchEvent) => {
      // Новое касание завершает прежний жест, но не отменяет наблюдение уже
      // отправленного действия (см. onCancel).
      if (e.touches.length !== 1) { onCancel({ keepObservations: true }); mode = "passthrough"; return; }
      const stopping = inertiaRaf != null;
      const previousTapSuppression = suppressTerminalTapUntilRef.current;
      onCancel({ keepObservations: true });
      suppressTerminalTapUntilRef.current = previousTapSuppression;
      gestureIdentity = scrollIdentityRef.current();
      scrollRouterRef.current.beginTouch(gestureIdentity);
      // НОВЫЙ ЖЕСТ — новый бюджет страниц и чистый накопитель. Без этого остаток
      // прошлого свайпа досылался страницей уже внутри следующего, а потолок
      // «три страницы» считался на кадр, а не на жест (повторный аудит
      // 2.57.12, T25712-03).
      pageBudgetRef.current = MAX_PAGES_PER_GESTURE;
      pageBudgetAtRef.current = Date.now();
      pageAccumRef.current = 0;
      if (pageIdleTimerRef.current != null) {
        clearTimeout(pageIdleTimerRef.current);
        pageIdleTimerRef.current = null;
      }
      // Жест начался (ещё не факт, что прокрутка — но палец уже на экране):
      // стирание истории из потока на это время не исполняем.
      gestureActiveRef.current = true;
      revealScrollButtons();
      stopInertia();
      const t0 = e.touches[0];
      startX = t0.clientX;
      startY = lastY = t0.clientY;
      startT = Date.now();
      // Braking consumes a tap, but the same contact may become a new drag.
      // stopped is reserved for a long press that already handed off to reading.
      mode = stopping ? "braking" : "idle";
      trace("gesture", { braking: stopping }, "touch-start");
      if (stopping) suppressTerminalTapUntilRef.current = Date.now() + 800;
      // One grid measurement per gesture; keyboard clipping never scales rows.
      rowH = measureTerminalCoordinates(el, term.cols, term.rows)?.cellHeight ?? 0;
      samples = [{ dy: 0, t: startT }];
      scrollAccum = 0;
      if (!stopping) longPress = setTimeout(() => {
        longPress = null;
        if (mode !== "idle" || gestureIdentity !== scrollIdentityRef.current()) return;
        mode = "stopped";
        trace("gesture", null, "long-press");
        suppressTerminalTapUntilRef.current = Date.now() + 1000;
        beginReadingRef.current({ x: startX, y: startY });
      }, 500);
    };

    const onMove = (e: TouchEvent) => {
      if (gestureIdentity !== scrollIdentityRef.current()) { onCancel(); return; }
      if (mode === "passthrough" || mode === "stopped") return;
      if (e.touches.length !== 1) return;
      const t1 = e.touches[0];
      const dx = t1.clientX - startX;
      const dy = t1.clientY - startY;

      if (mode === "idle" || mode === "braking") {
        // Блокировка оси (ST-10, viewportPan.touchAxis): первое уверенное
        // движение решает на весь жест. Вертикаль — прежняя прокрутка
        // навигации ST-02/03 без изменений; горизонталь сдвигает окно по сетке
        // шире коробки, а когда сдвигать нечего — прежний passthrough.
        const axis = touchAxis(dx, dy, features.viewportPan && panRoomRef.current());
        if (axis === null) return; // hesitation zone
        stopLongPress();
        if (axis === "passthrough") {
          // Горизонтальный сдвиг — не тап (T-11): если WebView всё же выпустит
          // click после короткого сдвига, клавиатура подниматься не должна.
          if (features.inputSafety) suppressTerminalTapUntilRef.current = Date.now() + 800;
          mode = "passthrough";
          return;
        }
        if (axis === "pan") {
          // Сдвиг окна — не тап и не прокрутка: в PTY ничего не уходит.
          // Пиксели зоны неуверенности тоже идут в сдвиг (от startX).
          suppressTerminalTapUntilRef.current = Date.now() + 800;
          mode = "pan";
          lastX = startX;
          trace("gesture", null, "pan-start");
        }
      }

      if (mode === "pan") {
        e.preventDefault();
        const stepX = t1.clientX - lastX;
        lastX = t1.clientX;
        // Палец тянет содержимое: окно едет в обратную сторону.
        if (stepX !== 0) panByPixelsRef.current(-stepX);
        return;
      }

      if (mode === "idle" || mode === "braking") {
        // Any vertical drag scrolls the buffer via scrollBy → term.scrollLines.
        // This xterm uses the DOM renderer: its viewport is NOT natively touch-
        // scrollable, so we must drive scrolling ourselves. It's the same call
        // the ⇈/↑ buttons use and reaches the whole session top-to-bottom. The
        // old "fast flick up = open folders" gesture is gone — it hijacked the
        // scroll-up flick.
        mode = "scroll";
        suppressTerminalTapUntilRef.current = Date.now() + 800;
      }

      if (mode === "scroll") {
        e.preventDefault();
        const stepY = t1.clientY - lastY;
        lastY = t1.clientY;
        const now = Date.now();
        samples.push({ dy: stepY, t: now });
        if (samples.length > 6) samples.shift();
        if (rowH <= 0) return;
        // Drag down → see older content (scroll up by N lines). Accumulate the
        // sub-row remainder so a slow drag (a few px per move) still scrolls
        // once the pixels add up to a whole line.
        scrollAccum += stepY;
        const lines = Math.trunc(scrollAccum / rowH);
        if (lines !== 0) {
          scrollAccum -= lines * rowH;
          scrollBy(-lines);
        }
      }
    };

    const onEnd = (e: TouchEvent) => {
      if (gestureIdentity !== scrollIdentityRef.current()) { onCancel(); return; }
      stopLongPress();
      if (mode !== "scroll") {
        if (mode === "stopped" || mode === "braking" || mode === "pan") suppressTerminalTapUntilRef.current = Date.now() + 800;
        // ТАП — это КЛИК для полноэкранного приложения, которое следит за
        // мышью. Claude Code рисует кликабельные вещи: плашку «1 new message
        // (ctrl+End)», пункты меню разрешений, свёрнутый вывод команды. За
        // компьютером они нажимаются мышью, а с телефона было нечем: мы слали
        // только колесо. Человек видел кнопку и не мог её нажать — жалоба
        // 12.08 «окно как зависло, я не могу ни проскроллить, ни посмотреть»
        // была ровно про это (там agent придержал вывод и ждал нажатия).
        //
        // Шлём только когда приложение САМО запросило мышь: у обычной оболочки
        // отчёт о клике превратился бы в мусор в командной строке.
        // Тап = палец так и не вышел из зоны неуверенности (mode остался
        // "idle"; мультитач и горизонталь уходят в "passthrough" ещё в onStart/
        // onMove). Раньше здесь стояла проверка `!gestureActiveRef.current`, но
        // onStart ставит этот флаг в true при ЛЮБОМ касании — условие было
        // ложным всегда, и тап-как-клик не уходил НИКОГДА. Плашка «N new
        // messages (ctrl+End)» у Claude Code пальцем не нажималась: снаружи
        // это и выглядело как «не скроллится и ничего не нажимается» (12.08).
        const wasTap = mode === "idle";
        trace("gesture", null, wasTap ? "tap" : `end-${mode}`);
        mode = "idle";
        gestureActiveRef.current = false;
        if (wasTap) sendTapAsClickRef.current(e);
        return;
      }
      mode = "idle";
      suppressTerminalTapUntilRef.current = Date.now() + 800;
      // ⚠ КОРОТКИЙ, НО ОСОЗНАННЫЙ DRAG ОБЯЗАН ЛИСТАТЬ. Палец вышел из зоны
      // неуверенности (8 px), но не набрал даже одной строки (rowH ≈ 16 px) —
      // и до сюда не доходило НИ ОДНОГО вызова прокрутки: ни строки во
      // вьюпорте, ни строки в страничный накопитель, ни, стало быть, досылки
      // через 200 мс. Снаружи это ровно «скролл работает не всегда» (повторный
      // аудит 2.57.12, T25712-03). Доводим остаток до одной строки.
      if (Math.abs(scrollAccum) >= 4 && Date.now() - (samples[samples.length - 1]?.t ?? 0) <= 100) {
        const dir = scrollAccum > 0 ? 1 : -1;
        scrollAccum = 0;
        scrollBy(-dir);
      }
      let velocity = releaseVelocity(samples, Date.now());
      const flick = Math.abs(velocity) >= 0.4 && rowH > 0;
      trace("gesture", { v: Math.round(velocity * 100) / 100, rowH: Math.round(rowH * 10) / 10 }, flick ? "scroll-fling" : "scroll-end");
      if (Math.abs(velocity) < 0.4) { gestureActiveRef.current = false; return; }

      if (rowH <= 0) { gestureActiveRef.current = false; return; }

      // Инерция — продолжение жеста: флаг держим до её остановки, иначе окно
      // между отпусканием пальца и концом докрутки пропускало бы стирание.
      let prevTs = performance.now();
      scrollAccum = 0;
      const tick = (now: number) => {
        if (gestureIdentity !== scrollIdentityRef.current()) { onCancel(); return; }
        // Явное нажатие (⇊, кнопки) завершило этот жест (ScrollRouter.explicit):
        // докрутка останавливается, а не листает поверх нового намерения.
        if (!scrollRouterRef.current.touching(gestureIdentity)) {
          inertiaRaf = null;
          gestureActiveRef.current = false;
          return;
        }
        const elapsed = Math.min(50, now - prevTs);
        prevTs = now;
        scrollAccum += velocity * elapsed;
        const lines = Math.trunc(scrollAccum / rowH);
        if (lines !== 0) {
          scrollAccum -= lines * rowH;
          // Источник «inertia»: своей прокрутке докрутка полезна, страничному
          // каналу — нет (там она и давала лишние страницы, см. flushAltScroll).
          scrollByLines(-lines, { source: "inertia" });
        }
        velocity = decayVelocity(velocity, elapsed);
        if (Math.abs(velocity) > 0.05) {
          inertiaRaf = requestAnimationFrame(tick);
        } else {
          inertiaRaf = null;
          gestureActiveRef.current = false;
        }
      };
      inertiaRaf = requestAnimationFrame(tick);
    };

    // Browser/OS cancelled ownership (notification shade, app background,
    // second finger): this is not a release gesture and must never launch
    // inertia or the delayed one-page short-swipe action.
    // opts приходит только из onStart; как обработчик событий (touchcancel,
    // blur, visibilitychange) получает Event — и тогда это полная отмена.
    const onCancel = (opts?: unknown) => {
      // Новое касание (keepObservations) завершает прежний жест, но НЕ отменяет
      // наблюдение уже отправленного действия: при свайпах чаще окна пробы
      // свидетельство иначе не появлялось бы никогда (ревью 14.09, P1).
      // Системная отмена, уход в фон и смена режима — отменяют: экран в это
      // время не обязан отражать ответ приложения.
      const keepObservations = (opts as { keepObservations?: boolean } | undefined)?.keepObservations === true;
      // Отмена системой, уходом в фон или сменой режима — в трассу; новое
      // касание (keepObservations) штатно завершает прежний жест и шума не даёт.
      if (!keepObservations && (mode === "scroll" || mode === "braking" || inertiaRaf != null)) {
        trace("gesture", { mode, inertia: inertiaRaf != null }, "cancel");
      }
      // Касание было НАШИМ: палец на экране, инерция или долгое нажатие. Только
      // тогда следующий click — хвост отменённого жеста и гасится. Отмена без
      // жеста (маркер, сдвиг окна при смене плашки, смена режима) тап человека
      // больше не съедает: волна 4, проба мигания «обрыв во время удержания» —
      // тап через ~0,5 с после переподключения не поднимал клавиатуру
      // (трасса: gesture:tap → input:tap-ignored). Откат — inputSafety=false.
      const owned = gestureActiveRef.current || inertiaRaf != null || longPress != null;
      scrollRouterRef.current.cancel();
      pendingScrollRouteRef.current = null;
      stopLongPress();
      mode = "passthrough";
      if (owned || !features.inputSafety) suppressTerminalTapUntilRef.current = Date.now() + 800;
      stopInertia();
      samples = [];
      scrollAccum = 0;
      gestureActiveRef.current = false;
      // touchmove may already have queued an application/page scroll for the
      // next animation frame. A system cancel transfers gesture ownership to
      // the browser/OS; letting that rAF run afterwards creates a "ghost"
      // scroll (and may arm the delayed one-page fallback) after the finger is
      // no longer ours.
      if (altScrollRafRef.current != null) {
        cancelAnimationFrame(altScrollRafRef.current);
        altScrollRafRef.current = null;
      }
      altScrollAccumRef.current = 0;
      altScrollButtonRef.current = false;
      altScrollInertiaRef.current = false;
      if (!keepObservations) {
        // The probe may already have sent its one tentative PgUp/wheel action.
        // It cannot be unsent, but a system-cancelled gesture must not turn into
        // a delayed verdict after the OS/browser has taken touch ownership away.
        cancelScrollObservations();
        altScrollProbeTokenRef.current++;
      }
      altScrollQueuedActionsRef.current = [];
      pageAccumRef.current = 0;
      if (pageIdleTimerRef.current != null) {
        clearTimeout(pageIdleTimerRef.current);
        pageIdleTimerRef.current = null;
      }
    };

    cancelGestureRef.current = onCancel;
    el.addEventListener("touchstart", onStart, { passive: true, capture: true });
    el.addEventListener("touchmove", onMove, { passive: false, capture: true });
    el.addEventListener("touchend", onEnd, { passive: true, capture: true });
    el.addEventListener("touchcancel", onCancel, { passive: true, capture: true });
    // T-12 (волна 4): браузер или система забрали касание — это и pointercancel,
    // а не только touchcancel. Отменённый жест не запускает инерцию и отложенную
    // прокрутку. touch-action: none стоит и на САМОЙ коробке (.pty-terminal), а
    // не только на дереве xterm: поле коробки (4 px + вырез экрана) было auto,
    // и касание, которое браузер отдал бы коробке, он вправе забрать под
    // панораму (горизонтальный passthrough) — тогда pointercancel снял бы
    // наблюдение уже отправленного действия. Это защита, а не доказанный
    // случай: в Chromium касание из поля в 4 px подгонка касания отдаёт узлу
    // xterm (там none) — pointerup, не pointercancel, на обеих сборках (скептик
    // 15.09). Касается широкого поля (вырез экрана, альбомная ориентация);
    // проба input-gestures-live мерит его на расширенном поле, на телефоне
    // (L4) не проверено.
    const onPointerCancel = (e: PointerEvent) => { if (e.pointerType === "touch") onCancel(); };
    if (features.inputSafety) el.addEventListener("pointercancel", onPointerCancel, { capture: true });
    const onContextMenu = (e: Event) => {
      if (longPress != null || selectModeRef.current) { e.preventDefault(); e.stopPropagation(); }
    };
    el.addEventListener("contextmenu", onContextMenu, { capture: true });
    const onVisibility = () => { if (document.hidden) onCancel(); };
    window.addEventListener("blur", onCancel);
    window.addEventListener("orientationchange", onCancel);
    document.addEventListener("visibilitychange", onVisibility);
    return () => {
      onCancel();
      if (cancelGestureRef.current === onCancel) cancelGestureRef.current = () => scrollRouterRef.current.cancel();
      gestureActiveRef.current = false;
      el.removeEventListener("touchstart", onStart, { capture: true } as any);
      el.removeEventListener("touchmove", onMove, { capture: true } as any);
      el.removeEventListener("touchend", onEnd, { capture: true } as any);
      el.removeEventListener("touchcancel", onCancel, { capture: true } as any);
      el.removeEventListener("pointercancel", onPointerCancel, { capture: true });
      el.removeEventListener("contextmenu", onContextMenu, { capture: true });
      window.removeEventListener("blur", onCancel);
      window.removeEventListener("orientationchange", onCancel);
      document.removeEventListener("visibilitychange", onVisibility);
    };
  }, [selectMode, scrollByLines, revealScrollButtons]);

  const closeReading = useCallback(() => {
    if (selectModeRef.current) trace("read", null, "close");
    readWindowAnchorRef.current?.dispose(); readWindowAnchorRef.current = undefined;
    readRequestRef.current++;
    selectModeRef.current = false;
    setSelectMode(false);
    setReadView(null);
    setReadHasNewOutput(false);
    terminalRef.current?.clearSelection();
    suppressTerminalTapUntilRef.current = Date.now() + 400;
  }, []);

  const beginReading = useCallback((point?: { x: number; y: number }, all = false,
    window?: { anchor: NonNullable<ReturnType<typeof registerReadAnchor>>; epoch: string; historySeq: number;
      geometryRevision: number; rows: number; direction: -1 | 1 }) => {
    const term = terminalRef.current, host = termRef.current;
    if (!term || !host) return;
    const request = ++readRequestRef.current;
    trace("read", { all, geomRev: readGeometryRevisionRef.current }, window ? (window.direction < 0 ? "page-older" : "page-newer") : "open");
    selectModeRef.current = true;
    setSelectMode(true);
    term.blur();
    flushTermWrites(() => {
      if (request !== readRequestRef.current || disposedRef.current || term !== terminalRef.current) return;
      const geometry = measureTerminalCoordinates(host, term.cols, term.rows);
      if (!geometry) { closeReading(); return; }
      // Продолжать от прежней страницы можно, пока история та же: в политике —
      // ни одного исполненного стирания/RIS после снятия и та же геометрия;
      // resumed без пропуска больше не блокирует Older/Newer (ST-04, T-14).
      // legacy и shadow — прежняя сверка эпохи writer (меняется на каждом
      // маркере); shadow сообщает, где политика решила бы иначе.
      let sameSource = true;
      if (window) {
        const legacySame = window.epoch === writerEpochRef.current && window.geometryRevision === readGeometryRevisionRef.current;
        const policySame = continuationValid(window, { historySeq: historySeqRef.current, geometryRevision: readGeometryRevisionRef.current });
        const route = eraseRoute(features.retention, legacySame, policySame);
        if (route.diverged) noteRetentionShadow("page", legacySame ? "same" : "changed", policySame ? "same" : "changed", true);
        sameSource = route.use;
      }
      if (window && (window.anchor.isDisposed || !sameSource || term.buffer.active.type !== "normal"
        || window.direction < 0 && window.anchor.line <= 0
        || window.direction > 0 && window.anchor.line + window.rows >= term.buffer.active.length)) {
        setReadView(view => view ? { ...view, notice: t("pty.readSourceChanged") } : view); return;
      }
      const cell = point ? cellAt(geometry, point.x, point.y) : cellAt(geometry, geometry.visible.left, geometry.visible.top);
      const anchorRow = term.buffer.active.viewportY + cell.row;
      const firstRow = window?.direction === 1 ? window.anchor.line + window.rows : undefined;
      const endRow = window?.direction === -1 ? window.anchor.line : undefined;
      const document = captureReadDocument(term.buffer.active, term.cols, {
        session: id || "", epoch: writerEpochRef.current, offset: appliedOffsetRef.current,
        geometryRevision: readGeometryRevisionRef.current, streamGap: streamGapRef.current,
        // ST-04: версия истории и полнота (I-07, I-11): стиралась ли она,
        // упёрся ли буфер в предел scrollback, какая политика у поколения.
        // Текст «ранее стёрто» — поведение политики: в legacy и shadow
        // документ несёт прежнюю полноту (erasedBefore=false, без retention).
        // historySeq экрана не меняет — только сверку соседней страницы.
        historySeq: historySeqRef.current,
        erasedBefore: features.retention === "policy" && erasedInGenerationRef.current,
        // I-11 (волна 4): буфер начат с хвоста эпохи — честная отметка неполноты.
        ringTruncatedBefore: features.retention === "policy" && ringTruncatedRef.current,
        retention: features.retention === "policy" ? retentionRef.current.erase : undefined,
        capacity: { scrollback: term.options.scrollback ?? 10000, rows: term.rows },
      }, { rows: 5000, chars: 1024 * 1024, firstRow, endRow, anchorRow });
      readWindowAnchorRef.current?.dispose();
      const anchor = registerReadAnchor(term, document.firstBufferRow);
      readWindowAnchorRef.current = anchor;
      const anotherPage = (direction: -1 | 1) => anchor && beginReading(undefined, false,
        { anchor, epoch: document.epoch, historySeq: document.historySeq, geometryRevision: document.geometryRevision,
          rows: document.lines.length, direction });
      const row = window ? 0 : term.buffer.active.viewportY - document.firstBufferRow;
      const offset = documentCell(document, row + cell.row, cell.col);
      setReadView({ document, initialRange: all ? { start: 0, end: document.text.length } : window ? { start: 0, end: 0 } : wordRange(document, offset),
        older: document.truncatedBefore && anchor ? () => anotherPage(-1) : undefined,
        newer: document.truncatedAfter && anchor ? () => anotherPage(1) : undefined,
        initialRow: row + (clippedRows(geometry)?.start ?? 0), cellWidth: geometry.cellWidth, cellHeight: geometry.cellHeight,
        fontFamily: String(term.options.fontFamily || "monospace"), fontSize: term.options.fontSize || 14 });
      setReadHasNewOutput(false);
    });
  }, [id, closeReading]);
  beginReadingRef.current = beginReading;

  useEffect(() => {
    if (!readView) return;
    const term = terminalRef.current;
    let changed = false;
    const disposable = term?.onWriteParsed(() => {
      if (!changed && (writerEpochRef.current !== readView.document.epoch || appliedOffsetRef.current > readView.document.offset)) {
        changed = true; setReadHasNewOutput(true);
      }
    });
    return () => disposable?.dispose();
  }, [readView]);
  useEffect(() => { closeReading(); }, [id, closeReading]);

  const toggleSelectMode = () => {
    haptic();
    if (selectModeRef.current) closeReading(); else beginReading();
  };

  const handleSelectAll = () => { haptic(); beginReading(undefined, true); };

  const quotePath = (p: string) => {
    const escaped = p.replace(/"/g, '\\"');
    return `"${escaped}"`;
  };

  /** «12м» / «3ч» — сколько агент ждёт ответа (status_at = начало ожидания). */
  const sinceValue = (ms: number): string => {
    const sec = Math.max(0, ((Date.now() - ms) / 1000) | 0);
    if (sec < 60) return t("ui.ptytermview.mee89ed3363", { p0: (sec) });
    if (sec < 3600) return t("ui.ptytermview.m2cc2493765", { p0: ((sec / 60) | 0) });
    if (sec < 86400) return t("ui.ptytermview.mb190c47c3b", { p0: ((sec / 3600) | 0) });
    return t("ui.ptytermview.m991812ce32", { p0: ((sec / 86400) | 0) });
  };

  /**
   * Отправить ШЕЛЛ-команду в терминал. Возвращает false, если пользователь
   * отказался.
   *
   * Пока в терминале работает AI-агент, ввод идёт не в шелл, а В ЧАТ агента:
   * тап по 📁 отправлял Claude Code сообщение `cd "C:\proj"` — потраченные
   * токены и сбитая задача, а шапка при этом показывала папку, в которой шелл
   * не находится. Esc здесь не предлагаем: у агентов он прерывает текущую
   * работу, то есть «безопасный» путь оказался бы разрушительным.
   */
  const sendShellCommand = async (cmd: string): Promise<boolean> => {
    if (!isAgentKind(state.agent_kind)) {
      sendRaw(cmd + "\r");
      return true;
    }
    const agent = state.fg_process || t("pty.agentBusy");
    // В вопрос идёт СУТЬ команды, а не её служебная обёртка: запуск агента под
    // аккаунтом на Windows — это 2579 символов PowerShell, среди которых сам
    // вопрос терялся (аудит онбординга 30.08.2026, commandLabel.ts).
    const ok = await tgConfirm(t("pty.agentBusyConfirm", { agent, cmd: shortCommandLabel(cmd) }), {
      confirmText: t("pty.agentBusySend"),
      cancelText: t("modal.cancel"),
    });
    if (!ok) return false;
    return sendRaw(cmd + "\r");
  };

  /**
   * Запуск CLI из шторки — отдельная транзакция от обычной shell-команды.
   * Подтверждение живого агента идёт ПЕРВЫМ и в этом случае команда остаётся
   * сообщением текущему агенту: metadata не меняем, потому что новый процесс
   * не стартовал. В idle-shell сначала надёжно записываем выбранный аккаунт,
   * затем отправляем CLI; если сокет уже закрылся, возвращаем прежнюю отметку.
   */
  const sendAgentLaunchCommand = async (
    cmd: string,
    account: { id: string; label: string; isDefault: boolean } | null,
  ): Promise<boolean> => {
    if (!id || state.kind === "ssh" || state.remote === true) return sendShellCommand(cmd);

    const agentAlreadyRunning = isAgentKind(state.agent_kind);
    const previous = { id: state.account_id || "", label: state.account_label || "" };
    const next = {
      id: account?.isDefault ? "" : (account?.id || ""),
      label: account?.isDefault ? "" : (account?.label || ""),
    };
    const sent = await commitAccountScopedLaunch({
      agentAlreadyRunning,
      sendToExistingAgent: () => sendShellCommand(cmd),
      previous,
      next,
      persist: async (marker) => (await setPtyAccount(id, marker.id, marker.label)).ok,
      sendToShell: () => sendRaw(cmd + "\r"),
    });
    if (sent && !agentAlreadyRunning) {
      setState((current) => ({
        ...current,
        account_id: next.id,
        account_label: next.label,
      }));
    }
    return sent;
  };

  // Ряд быстрых команд внизу — это и шелл-команды, и просто текстовые
  // заготовки для агента (решение владельца 2026-07-30, 2.46.12): тап шлёт
  // сразу, без вопроса «уйдёт в чат агента» — для заготовок вопрос был
  // лишним шагом на каждый тап. Подтверждение остаётся только у 📁 cd:
  // там цена ошибки выше — шапка соврёт про папку, в которой шелл не был.
  const sendQuickCommand = (cmd: string) => {
    sendRaw(cmd + "\r");
  };

  const handleFolderPick = async (path: string) => {
    if (!path) return;
    // cwd НЕ меняем оптимистично: команда могла не уйти в шелл вообще, и шапка
    // показывала бы папку, в которой шелл не находится.
    if (!(await sendShellCommand(`cd ${quotePath(path)}`))) return;
    hapticSuccess();
    showToast(t("pty.cdDone", { path }));
    terminalRef.current?.focus();
  };

  const handleCopyCwd = async () => {
    if (!cwd) return;
    if (features.inputSafety) {
      // Через ClipboardService (T-18): на телефоне — адаптер Capacitor, а отказ
      // называется человеку, а не глотается молча.
      if (await terminalClipboard.write(cwd)) { hapticSuccess(); showToast(t("folder.pathCopied")); }
      else showToast(t("pty.cwdCopyFailed"));
      return;
    }
    try {
      await navigator.clipboard.writeText(cwd);
      hapticSuccess();
      showToast(t("folder.pathCopied"));
    } catch { /* ignore */ }
  };

  // Лежит ли папка ЭТОГО терминала в избранном. Спрашиваем на каждую смену
  // папки: терминал живёт долгими днями, и `cd` внутри него меняет ответ.
  // У SSH-сессии закладки чужие — они ходят по файловой системе ПК-бастиона,
  // а человек смотрит на удалённый сервер (то же правило, что у кнопки «📁»).
  useEffect(() => {
    const ssh = state.kind === "ssh" || state.shell === "ssh";
    if (!cwd || ssh) { setCwdPinned(null); return; }
    let cancelled = false;
    getBookmarks()
      .then((d) => {
        if (!cancelled) setCwdPinned((d.bookmarks || []).some((b) => b.path === cwd));
      })
      .catch(() => { if (!cancelled) setCwdPinned(null); });
    return () => { cancelled = true; };
  }, [cwd, state.kind, state.shell]);

  /**
   * Звезда в шапке: папка терминала в избранное и обратно.
   *
   * До этого добавить папку можно было только внутри обзора папок — долгим
   * тапом по строке или кнопкой «📌 закрепить текущую» на вкладке избранного.
   * Владелец, работая в терминале, эту дорогу не нашёл: «не пойму где можно
   * добавить» (31.08.2026). Действие переехало туда, где человек стоит.
   */
  const toggleFavorite = async () => {
    if (!cwd || pinning || cwdPinned === null) return;
    haptic();
    setPinning(true);
    // Звезда загорается сразу, но прежнее состояние помним: у телефона сеть
    // рвётся, а «звезда горит, папки в избранном нет» — худший из исходов.
    const was = cwdPinned;
    setCwdPinned(!was);
    try {
      if (was) {
        await removeBookmark(cwd);
        showToast(t("pty.unpinnedFolder"));
      } else {
        const name = cwd.split(/[/\\]/).filter(Boolean).pop() || cwd;
        await addBookmark(name, cwd);
        hapticSuccess();
        showToast(t("pty.pinnedFolder"));
      }
    } catch (e: any) {
      setCwdPinned(was);
      showToast(mapApiError(e));
    } finally {
      setPinning(false);
    }
  };

  /**
   * Переименование ОТСЮДА, а не только из списка.
   *
   * Живая жалоба владельца 31.08 «не могу переименовать Терминал»: механика в
   * списке исправна (проверено запуском, `build/qa/probe-pty-rename.mjs`), но
   * человек стоит в открытом терминале, видит его имя в шапке — и переименовать
   * его отсюда было нечем. Имя правится там, где оно написано.
   *
   * Диалог, а не поле в шапке: места в ней нет (кнопка «Открыть на ПК» убрана
   * отсюда именно за это), а свой диалог — единственный ввод, работающий во
   * всех четырёх интерфейсах, включая окно exe: WebView2 подавляет нативный
   * prompt.
   */
  const startRename = () => {
    if (!id) return;
    haptic();
    setRenameDraft(state.name || "");
    setRenaming(true);
  };

  /**
   * Сохранение имени — только по «✓» или Enter.
   *
   * ⚠ НЕ вешать на `onBlur`: в списке терминалов это стоило трёх обращений
   * владельца за день — потеря фокуса (клавиатура, перерисовка, промах пальцем)
   * записывала СТАРОЕ имя и закрывала ввод («нажимаю переименовать — сразу
   * пишет "имя сохранено"»). Здесь ровно то же поле и ровно та же ловушка.
   */
  const handleRenameSave = async () => {
    if (!id) return;
    const next = renameDraft.trim();
    try {
      const saved = await renamePty(id, next);
      // Имя показываем то, которое ПРИНЯЛ компьютер: он режет управляющие
      // символы и длину, и шапка не должна обещать больше, чем сохранено.
      setState((s) => ({ ...s, name: saved?.name ?? next }));
      hapticSuccess();
      showToast(t("pty.renameDone"));
      setRenaming(false);
    } catch (e: any) {
      // Ввод оставляем открытым: имя не сохранено, и текст терять нельзя.
      showToast(mapApiError(e));
    }
  };

  const handleRenameCancel = () => {
    setRenaming(false);
    setRenameDraft("");
  };

  const handleHandoff = async () => {
    if (!id) return;
    haptic();
    try {
      await handoffPty(id);
      hapticSuccess();
      showToast(t("pty.handoffDone"));
    } catch (e: any) {
      showToast(mapApiError(e));
    }
  };

  // Перезапуск в той же папке: новая PTY-сессия с тем же cwd (как handleCreate
  // в PtyListView) и переход на неё — старый процесс уже завершён / потерян.
  // Компонент при смене id НЕ размонтируется, поэтому сбрасываем alive заранее,
  // иначе плашка «Процесс завершён» мигнёт на новой сессии до первого полла.
  const handleRestartHere = async () => {
    if (restarting) return;
    haptic();
    setRestarting(true);
    try {
      const res = await createPtySession(cwd || ".", "", 80, 24);
      hapticSuccess();
      setState((s) => ({ ...s, alive: true }));
      setRestarting(false);
      navigate(`/pty/${res.id}`);
    } catch (e: any) {
      showToast(mapApiError(e));
      setRestarting(false);
    }
  };

  const handleResumeAgent = async () => {
    if (restarting || !resumeAgent || !resumeCommand) {
      if (resumeBlockedReason) showToast(resumeBlockedReason);
      return;
    }
    haptic();
    setRestarting(true);
    try {
      // UI уже проверил snapshot, но перед необратимым созданием новой PTY
      // перечитываем аккаунт: другая шторка могла сменить proxy/profile после
      // него. Ошибка сети здесь означает стоп, а не direct fallback.
      let freshAccount = resumeRecordedAccount;
      let freshCommand = resumeCommand;
      if (resumeAgent.agent.account_env) {
        const payload = await getAgentAccounts(undefined, cwd || ".");
        const version = Number.isSafeInteger(payload.proxy_contract_version)
          ? Number(payload.proxy_contract_version)
          : 0;
        const accounts = payload.accounts || [];
        freshAccount = recordedResumeAccount(accounts, resumeAgent.kind, state.account_id);
        const launchAccount = launchAccountForAgent(resumeAgent.agent, {
          accounts,
          proxyContractVersion: version,
          accountsReady: true,
        }, freshAccount);
        freshCommand = composeLaunch(resumeAgent.agent.resume_cli || "", EMPTY_PREFS, {
          agentID: resumeAgent.agent.id,
          accountEnvName: resumeAgent.agent.account_env || "",
          proxyContractVersion: version,
          blockedReason: launchAccount?.blockedReason,
          account: launchAccount,
          posix: resumePosix,
          launchArgs: resumeAgent.agent.launch_args,
        });
        if (!freshCommand) {
          throw new Error(launchAccount?.blockedReason || t("ui.ptylistview.mc16c569984"));
        }
      }
      const res = await createPtySession(cwd || ".", "", 80, 24);
      await setPtyAccount(
        res.id,
        freshAccount?.is_default ? "" : (freshAccount?.id || ""),
        freshAccount && !freshAccount.is_default ? freshAccount.label : "",
      );
      await ptyInput(res.id, { data: freshCommand + "\r" });
      try { localStorage.setItem(`pty.lastAgent.${res.id}`, resumeAgent.kind); } catch { /* ignore */ }
      hapticSuccess();
      setState((s) => ({ ...s, alive: true }));
      navigate(`/pty/${res.id}`);
    } catch (e: any) {
      showToast(mapApiError(e));
    } finally {
      setRestarting(false);
    }
  };

  // Усыпление агента (ptyTerm/agentSleep.ts, internal/pty/agent_sleep.go).
  // Просьба владельца 29.09.2026: десять открытых Claude держали 7,5 ГБ, почти
  // все просто ждали его. Решает человек — кнопкой, без таймера. Терминал
  // остаётся, агент снимается вместе с MCP, беседа поднимается по номеру.
  const [sleepBusy, setSleepBusy] = useState(false);
  // Этот экран сам усыпил агента — будить его тут же, на том же экране, нельзя:
  // автопробуждение только при ОТКРЫТИИ спящего терминала.
  const sleptHereRef = useRef(false);
  const autoWakeTriedRef = useRef("");
  // Автопробуждение — только если терминал спал уже в момент открытия: сон,
  // пришедший позже опросом, — это другой экран только что усыпил агента, а
  // не приглашение будить (скептик 29.09). «Момент открытия» — ПЕРВОЕ
  // состояние этого экрана, от первичного запроса или от опроса, что раньше.
  // Состояние, а не ref: эффект пробуждения обязан перезапуститься, если
  // признак появился после того, как он уже отработал.
  const firstStateRef = useRef("");
  const [openedAsleepId, setOpenedAsleepId] = useState("");
  const noteFirstState = (d: PtyState) => {
    if (!id || firstStateRef.current === id) return;
    firstStateRef.current = id;
    if (d.sleep) setOpenedAsleepId(id);
  };
  const sleepAgent = resumeAgent && canSleepAgent(resumeAgent.agent) ? resumeAgent : null;
  const sleepName = (kind?: string) => (
    sleepAgent?.agent.name || agentDisplayName(kind || sleepAgent?.kind || "")
  );

  const handleSleepAgent = async () => {
    if (!id || sleepBusy) return;
    if (sleepBlockedByStatus(state.status)) {
      showToast(t("pty.sleepBusy"));
      return;
    }
    haptic();
    setSleepBusy(true);
    try {
      const res = await ptySleep(id);
      sleptHereRef.current = true;
      hapticSuccess();
      setToolsOpen(false);
      setState((s) => ({ ...s, sleep: res.sleep }));
      terminalRef.current?.write(INPUT_MODES_OFF);
      showToast(t("pty.sleepDone", { name: sleepName(res.sleep.agent) }));
    } catch (e: any) {
      hapticError();
      showToast(sleepErrorText(e) || mapApiError(e));
    } finally {
      setSleepBusy(false);
    }
  };

  const handleWakeAgent = async () => {
    if (!id || sleepBusy || !state.sleep) return;
    const agent = sleepAgent?.agent;
    if (!agent || sleepAgent?.kind !== state.sleep.agent) {
      showToast(t("pty.wakeUnavailable"));
      return;
    }
    haptic();
    setSleepBusy(true);
    try {
      // Аккаунт перечитываем перед запуском по той же причине, что и в
      // handleResumeAgent: другая шторка могла сменить прокси или профиль.
      let launchAccount = resumeLaunchAccount;
      let proxyContractVersion = resumeAccountsState.proxyContractVersion;
      if (agent.account_env) {
        const payload = await getAgentAccounts(undefined, cwd || ".");
        proxyContractVersion = Number.isSafeInteger(payload.proxy_contract_version)
          ? Number(payload.proxy_contract_version)
          : 0;
        const accounts = payload.accounts || [];
        launchAccount = launchAccountForAgent(agent, {
          accounts,
          proxyContractVersion,
          accountsReady: true,
        }, recordedResumeAccount(accounts, state.sleep.agent, state.account_id));
      }
      if (launchAccount?.blockedReason) throw new Error(launchAccount.blockedReason);
      // Забираем запись ПОСЛЕ проверок: забранная запись 20 секунд недоступна
      // другим экранам, и провал на аккаунте запер бы пробуждение зря.
      const res = await ptyWake(id);
      const command = wakeCommand(agent, res.sleep, {
        agentID: agent.id,
        accountEnvName: agent.account_env || "",
        proxyContractVersion,
        blockedReason: launchAccount?.blockedReason,
        account: launchAccount,
        posix: resumePosix,
        launchArgs: agent.launch_args,
      });
      if (!command) throw new Error(t("pty.wakeUnavailable"));
      // Сначала гасим режимы ввода, оставленные снятым агентом, и чистим
      // строку шелла: в ней уже могли лежать `[O`/`[I` от смены фокуса
      // (жалоба 29.09). Esc на Windows / Ctrl+U на POSIX (см. wakeClearLine);
      // сервер только что подтвердил, что на переднем плане шелл.
      terminalRef.current?.write(INPUT_MODES_OFF);
      await ptyInput(id, { data: wakeClearLine(resumePosix) });
      await new Promise((resolve) => setTimeout(resolve, 300));
      await ptyInput(id, { data: command + "\r" });
      hapticSuccess();
      setState((s) => ({ ...s, sleep: undefined }));
    } catch (e: any) {
      hapticError();
      showToast(sleepErrorText(e) || mapApiError(e));
    } finally {
      setSleepBusy(false);
    }
  };

  // Пока агент спит, этот экран не должен слать шеллу события мыши и фокуса:
  // сервер их режимы уже снял, но переподключение могло переиграть старый
  // кадр с ними (реассерт pty-host). Повторяем на каждое (пере)подключение.
  useEffect(() => {
    if (!state.alive || !state.sleep || !connected) return;
    terminalRef.current?.write(INPUT_MODES_OFF);
  }, [state.alive, state.sleep?.at, connected]);

  // Открыли спящий терминал — будим сами, как только известен агент: человек
  // пришёл работать с этой беседой. Один раз на открытие и не на том экране,
  // где агента только что усыпили.
  useEffect(() => {
    if (!id || !state.alive || !state.sleep || state.sleep.waking_at) return;
    if (sleptHereRef.current || autoWakeTriedRef.current === id || openedAsleepId !== id) return;
    if (!sleepAgent || sleepAgent.kind !== state.sleep.agent) return;
    if (sleepAgent.agent.account_env && !resumeAccountsState.accountsReady) return;
    autoWakeTriedRef.current = id;
    void handleWakeAgent();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [id, openedAsleepId, state.alive, state.sleep?.at, state.sleep?.waking_at, sleepAgent?.kind, resumeAccountsState.accountsReady]);

  // Экспорт scrollback. Тост показываем по ФАКТУ доставки: «shared» = блоб ушёл
  // в системный лист (нативный APK), «downloaded» = браузерная загрузка, про
  // которую честно сказано «проверьте загрузки» — в WebView Telegram она может
  // не сработать вовсе, поэтому там первым предлагается путь через бота.
  const exportName = (format: "txt" | "md") => ptyLogFileName(
    state.name || headerTitle || `terminal-${(id || "").slice(0, 8)}`,
    format,
  );

  const exportTerm = async (format: "txt" | "md") => {
    if (!id || exporting) return;
    haptic();
    setExporting(true);
    try {
      const how = await savePtyLog(id, format, exportName(format), botAvailable);
      if (how === "failed") {
        showToast(t("pty.exportUnavailable"));
        return;
      }
      hapticSuccess();
      showToast(
        how === "telegram" ? t("pty.exportSentTg")
          : how === "shared" ? t("pty.exportShared")
            : t("pty.exportSaved"),
      );
      setExportOpen(false);
    } catch (e: any) {
      // Раньше здесь стоял чужой ключ pty.uploadFailed («Ошибка загрузки») —
      // человек читал про загрузку файла, хотя сохранял лог.
      if (!isShareCancel(e)) showToast(`${t("pty.exportFailed")}: ${mapApiError(e)}`);
    } finally {
      setExporting(false);
    }
  };

  // «Прислать лог файлом в чат»: единственный путь, который в Telegram ТОЧНО
  // доносит файл (нативного скачивания там нет). Экспорт кладётся файлом на ПК,
  // и уже его отправляет бот — см. apk/src/ptyLogExport.ts.
  const exportToTelegram = async (format: "txt" | "md") => {
    if (!id || exporting) return;
    haptic();
    setExporting(true);
    showToast(t("pty.exportSending"));
    try {
      await sendPtyLogToTelegram(id, format, exportName(format));
      hapticSuccess();
      showToast(t("pty.exportSentTg"));
      setExportOpen(false);
    } catch (e: any) {
      showToast(`${t("pty.exportFailed")}: ${mapApiError(e)}`);
    } finally {
      setExporting(false);
    }
  };

  // ── «Зафиксировать проблему» (ST-01) ──────────────────────────────────
  // Трасса локальная, поэтому всё это работает и на мёртвой сессии: ряд
  // инструментов и лист экспорта там видны, сокет не нужен.
  const refreshTracePreview = () => {
    setTraceView(tracePreview(traceRef.current!.snapshot(), recRef.current!.snapshot()));
  };
  const openTrace = () => {
    haptic();
    setTraceTgConfirm(false);
    refreshTracePreview();
  };
  /** Явное включение записи вывода ЭТОГО терминала на 30 минут (I-15) и её выключение. */
  const setTraceCapture = (on: boolean) => {
    haptic();
    const rec = recRef.current!;
    if (on) {
      const until = rec.enable();
      writeCaptureUntil(captureStorage(), until, id || "");
      setRecUntil(until);
      traceRef.current!.mark("capture-on", traceCtx());
    } else {
      rec.disable();
      writeCaptureUntil(captureStorage(), 0, id || "");
      setRecUntil(0);
      traceRef.current!.mark("capture-off", traceCtx());
    }
    refreshTracePreview();
  };
  /**
   * «Удалить запись» (T-37): запись ВЫВОДА выключается и стирается во всех
   * терминалах страницы вместе с их согласиями (оставить чужой вывод в памяти —
   * не удаление), события трассы — у этого терминала. Уведомление называет,
   * сколько других терминалов задело.
   */
  const deleteTraceRecording = () => {
    haptic();
    const { others } = wipeAllRecordings(terminalTraceStore, id || "");
    clearAllCaptures(captureStorage());
    setRecUntil(0);
    traceRef.current!.clear();
    setTraceTgConfirm(false);
    refreshTracePreview();
    showToast(others > 0 ? t("pty.traceDeletedOthers", { n: others }) : t("pty.traceDeleted"));
  };
  /**
   * asciicast v3 для проигрывателя (ST-01). Не байтовая копия (identical:false,
   * кадры экрана — метками), и появляется только при записанном выводе: в нём
   * тот же вывод, о котором предупреждает лист (I-15).
   */
  const saveCast = async () => {
    if (traceBusy) return;
    haptic();
    setTraceBusy(true);
    try {
      const tr = traceRef.current!;
      const seq = tr.mark("cast", traceCtx());
      const bundle = traceBundle(traceIdentity(), tr, recRef.current!.snapshot());
      sendDiag("trace-mark", { seq, events: bundle.events.length, dropped: bundle.dropped, bytes: bundle.recording?.bytes ?? 0 });
      const term = terminalRef.current;
      const cast = toAsciicastV3(bundle, { cols: term?.cols ?? 80, rows: term?.rows ?? 24 });
      const how = await saveBlob(new Blob([cast.cast], { type: "application/x-asciicast" }), traceFileName(Date.now(), "cast"));
      if (how === "failed") { showToast(t("pty.traceUnavailable")); return; }
      hapticSuccess();
      showToast(how === "shared" ? t("pty.traceShared") : t("pty.traceSaved"));
    } catch (e: any) {
      if (!isShareCancel(e)) showToast(`${t("pty.traceFailed")}: ${mapApiError(e)}`);
    } finally {
      setTraceBusy(false);
      refreshTracePreview();
    }
  };
  /**
   * «Очистить историю на этом устройстве» (ST-04, T-37). Уходит всё, что этот
   * экран хранит из вывода: открытое чтение и история агента закрываются,
   * отложенное стирание снимается, прокрутка стирается CSI 3 J через тот же
   * TerminalWriter (I-03), запись вывода «Зафиксировать проблему» стирается.
   * historySeq поднимает разборщик: прежние страницы чтения больше не
   * продолжаются. Кольцо на компьютере и другие зрители — не трогаются, и лист
   * говорит это прямо (I-11).
   */
  const clearDeviceHistory = () => {
    haptic();
    setClearConfirm(false);
    setExportOpen(false);
    const term = terminalRef.current;
    if (!term) return;
    cancelGestureRef.current();
    closeReading();
    historyClientRef.current.reset();
    setHistoryOpen(false);
    invalidatePendingScrollbackEraseRef.current();
    recRef.current!.clear();
    trace("erase", { alt: term.buffer.active.type === "alternate" }, "user-clear");
    writeSerial(SCROLLBACK_ERASE, () => {
      const live = terminalRef.current;
      if (!live || disposedRef.current) return;
      // Проверка по буферу, а не по намерению: в полноэкранном режиме CSI 3 J
      // обычную прокрутку не трогает — так и говорим.
      showToast(t(live.buffer.normal.baseY === 0 ? "pty.clearHistoryDone" : "pty.clearHistoryAlt"));
    }, { generation: connectionGenRef.current, epoch: writerEpochRef.current });
  };
  /** Файл трассы. Метка user и trace-mark в журнал агента связывают его со
   * строками журнала по seq. */
  const buildTraceFile = () => {
    const tr = traceRef.current!;
    const seq = tr.mark("user", traceCtx());
    const bundle = traceBundle(traceIdentity(), tr, recRef.current!.snapshot());
    sendDiag("trace-mark", {
      seq, events: bundle.events.length, dropped: bundle.dropped, bytes: bundle.recording?.bytes ?? 0,
    });
    return {
      name: traceFileName(Date.now()),
      blob: new Blob([JSON.stringify(bundle)], { type: "application/json" }),
    };
  };
  const saveTrace = async () => {
    if (traceBusy) return;
    haptic();
    setTraceBusy(true);
    try {
      const file = buildTraceFile();
      const how = await saveBlob(file.blob, file.name);
      if (how === "failed") { showToast(t("pty.traceUnavailable")); return; }
      hapticSuccess();
      showToast(how === "shared" ? t("pty.traceShared") : t("pty.traceSaved"));
    } catch (e: any) {
      if (!isShareCancel(e)) showToast(`${t("pty.traceFailed")}: ${mapApiError(e)}`);
    } finally {
      setTraceBusy(false);
      refreshTracePreview();
    }
  };
  /**
   * Доставка через Telegram кладёт файл на ПК и отдаёт боту. Файл с
   * записанным выводом — только после ОТДЕЛЬНОГО согласия (I-15): первый
   * тап показывает предупреждение, второй отправляет.
   */
  const sendTraceToTelegram = async (confirmed = false) => {
    if (traceBusy) return;
    haptic();
    if (!confirmed && recRef.current!.snapshot().chunks.length > 0) { setTraceTgConfirm(true); return; }
    setTraceTgConfirm(false);
    setTraceBusy(true);
    showToast(t("pty.traceSending"));
    try {
      const file = buildTraceFile();
      await sendBlobToTelegram(file.blob, file.name);
      hapticSuccess();
      showToast(t("pty.traceSentTg"));
    } catch (e: any) {
      showToast(`${t("pty.traceFailed")}: ${mapApiError(e)}`);
    } finally {
      setTraceBusy(false);
      refreshTracePreview();
    }
  };

  // Кнопки ⇈↑↓⇊ идут ТЕМ ЖЕ маршрутом, что и палец: ↑/↓ — обычная прокрутка,
  // ⇈/⇊ — край (scrollToEdge). Отдельного «а не alt ли сейчас» им не нужно —
  // на этот вопрос давно отвечает владелец истории, а не режим экрана.
  const scrollUp = () => scrollByLines(-10, { source: "button" });
  const scrollDown = () => scrollByLines(10, { source: "button" });
  const edgeLines = () => Math.max(40, (terminalRef.current?.rows ?? 24) * 5);
  const scrollTop = () => { scrollToEdge(true); };
  const scrollBottom = () => { scrollToEdge(false); };

  // Send raw bytes to PTY. Ввод в мёртвый сокет раньше молча ПРОПАДАЛ
  // (UX-аудит ТОП-10 #2) — теперь юзер видит тост «нет соединения».
  // Возвращает true, только если байты действительно ушли: вызывающий обязан
  // решить, можно ли считать ввод отправленным (например, чистить ли поле).
  // quiet — вызывающий покажет свой, более точный тост.
  /**
   * Попросить агента перерисовать экран.
   *
   * Проверено на ЗАВИСШИХ сессиях владельца 01.09.2026: такой агент не слышит
   * ни Ctrl+End, ни букв, ни backspace — ноль байт в ответ, — но на смену
   * размера окна отвечает немедленно и рисует экран заново. Значит рычаг,
   * которым его можно растормошить, ровно один, и это он.
   *
   * Меняем высоту на строку и тут же возвращаем: сервер получает честный
   * размер обратно, а агент — повод перерисоваться.
   */
  const redrawAgentScreen = () => {
    const ws = wsRef.current;
    const term = terminalRef.current;
    if (ws?.readyState !== WebSocket.OPEN || !term) {
      showToast(t("pty.noConnection"));
      return;
    }
    haptic();
    const { cols, rows } = term;
    if (cols < 2 || rows < 3) return;
    ws.send(JSON.stringify({ t: "resize", cols, rows: rows - 1 }));
    window.setTimeout(() => {
      const live = wsRef.current;
      if (live?.readyState === WebSocket.OPEN) {
        live.send(JSON.stringify({ t: "resize", cols, rows }));
      }
    }, 150);
    showToast(t("pty.agentRedrawSent"));
  };

  const sendRaw = (data: string, opts?: { quiet?: boolean }): boolean => {
    // ST-10: клавиша из ряда — ввод человека, окно по X снова за курсором.
    panInputAtRef.current = Date.now();
    if (transmitInput(wsRef.current, { kind: "key", data })) return true;
    if (!opts?.quiet) showToast(t("pty.noConnection"));
    return false;
  };

  /**
   * ⇈/⇊ и «Вернуться к новому» — это КРАЙ, а не «очень много строк».
   *
   * ⚠ До 2.57.13 кнопки считали `max(40, rows × 5)` строк и отдавали их общему
   * маршруту, а тот на живой своей истории уходил в `term.scrollLines()` и
   * зажимался... ничем: при экране 31 строка и истории 500 строк нажатие «в
   * самое начало» поднимало на 155 строк — человек оставался на 345 строк НИЖЕ
   * начала и жал кнопку снова и снова (повторный аудит 2.57.12, T25712-01).
   *
   * Возвращает true, только если переход ДЕЙСТВИТЕЛЬНО ушёл: по этому ответу
   * прячется кнопка «Вернуться к новому». Раньше она пряталась всегда — в том
   * числе при мёртвом сокете, то есть ровно тогда, когда была нужнее всего.
   */
  const scrollToEdge = (up: boolean): boolean => {
    const term = terminalRef.current;
    if (!term) return false;
    // Тот же учёт «человек крутит сам», что и в scrollByLines: якорь чтения не
    // должен тянуть вьюпорт обратно после нашего же перехода.
    userScrollSeqRef.current++;
    // Явное нажатие — новое намерение: жест пальцем, чья инерция ещё крутит,
    // здесь кончается (волна 7, probe-reading-pin-live мир C: докрутка
    // последнего свайпа после ⇊ поднимала вьюпорт и снова закрепляла чтение),
    // как и серия колеса с хвостом инерции трекпада (волна 8, мир D). ⇊ — ещё
    // и явный возврат к live: итог действий, начатых до него (вердикт пробы
    // кнопки ↑), чтение за удалённым каналом больше не закрепляет (волна 8,
    // ScrollRouter.returnToLive/mayPin).
    const identity = scrollIdentityRef.current();
    const ticket = up ? scrollRouterRef.current.explicit(identity) : scrollRouterRef.current.returnToLive(identity);
    // ⇊ — явный возврат к live (ST-03): закрепление чтения снимается. Решение —
    // ДО любого действия (I-01, правило 1б toward-live): своя история выше
    // низа — возврат только ею, без Ctrl+End приложению. Раньше здесь сначала
    // безусловно звался term.scrollToBottom(), и правило видело уже «своей
    // истории некуда вниз» — одно нажатие исполняли двое (волна 4, ревью I-01).
    if (!up) readingPinRef.current = null;
    const destination = decideForRef.current(term, up ? -1 : 1);
    noteNavDecisionRef.current(destination);
    if (!up && destination.decision.executor === "none") {
      // Вниз уже некуда и слать некому: мы у live — это не повод для тоста.
      setAltScrolledUp(false);
      return true;
    }
    const action = edgePlan(destination.decision, up);
    if (action.kind === "local") {
      if (up) term.scrollToTop(); else term.scrollToBottom();
      if (!up) setAltScrolledUp(false);
      else if (scrollOverrideRef.current === "auto") readingPinRef.current = { executor: "local", channel: "viewport" };
      return true;
    }
    if (action.kind === "send") {
      const ws = wsRef.current;
      if (ws?.readyState !== WebSocket.OPEN) {
        const at = Date.now();
        if (at - scrollOfflineWarnAtRef.current > SCROLL_OFFLINE_WARN_GAP_MS) {
          scrollOfflineWarnAtRef.current = at;
          showToast(t("pty.scrollOffline"));
        }
        return false;
      }
      // Edge buttons use the very same send + row-delta probe as a short touch
      // gesture and the ordinary scroll buttons. This catches an ignored
      // Ctrl+Home/Ctrl+End without sending a second competing action.
      dispatchScrollPayload(
        "page",
        action.data,
        up ? -edgeLines() : edgeLines(),
        destination.owner,
        { kind: "edge", up },
        ticket,
        destination.decision.mode,
      );
      if (up) setAltScrolledUp(true); else setAltScrolledUp(false);
      return true;
    }
    // Краевой команды у этого приложения нет (пейджер без агента) — прежнее
    // поведение: обычная прокрутка на несколько экранов.
    scrollByLines(up ? -edgeLines() : edgeLines(), { source: "button" });
    return true;
  };

  // Send text as a paste command — backend handles chunked writing to ConPTY
  // with delays to prevent input buffer overflow.
  const sendPaste = useCallback((text: string, opts?: { quiet?: boolean; submit?: boolean }): boolean => {
    if (transmitInput(wsRef.current, { kind: "paste", text,
      bracketed: terminalRef.current?.modes.bracketedPasteMode === true, submit: opts?.submit })) return true;
    if (!opts?.quiet) showToast(t("pty.noConnection"));
    return false;
  }, []);

  const showToast = (msg: string) => {
    if (toastTimer.current) clearTimeout(toastTimer.current);
    setToast(msg);
    // 3 секунды: за 1.5с длинные подсказки (пути, ошибки загрузки) не дочитывались.
    toastTimer.current = setTimeout(() => setToast(""), 3000);
  };

  const handleCopy = async (scope: "selection" | "screen" = "selection") => {
    haptic();
    const term = terminalRef.current;
    if (!term) return;

    let text = scope === "selection" ? term.getSelection() : "";
    if (scope === "screen" && termRef.current) {
      const geometry = measureTerminalCoordinates(termRef.current, term.cols, term.rows);
      const rows = geometry && clippedRows(geometry);
      if (rows) text = bufferText(term.buffer.active,
        term.buffer.active.viewportY + rows.start, term.buffer.active.viewportY + rows.end);
    }

    await deliverCopy(text);
  };

  /** Отдать текст в буфер обмена; отказ — ручное окно (T-18). Текст — никуда больше (I-15). */
  async function deliverCopy(text: string, copiedKey = "pty.copied") {
    if (!text) {
      showToast(t("pty.nothingToCopy"));
      return;
    }

    if (await terminalClipboard.write(text)) {
      hapticSuccess();
      showToast(t(copiedKey));
    } else {
      showToast(t("pty.copyFailed"));
      await promptDialog(t("pty.manualCopy"), { defaultValue: text, multiline: true });
    }
  }

  /**
   * «Копировать команду» / «Копировать вывод» (ST-10, T-39). Текст снимается
   * на барьере писателя: всё принятое до нажатия разобрано, и конец блока (D)
   * на месте. Блоки живут в НОРМАЛЬНОМ буфере — читаем его, даже если сейчас
   * экран занят полноэкранной программой. В трассу — только вид действия (I-15).
   */
  const copyBlock = (kind: "command" | "output") => {
    haptic();
    const term = terminalRef.current, model = blockModelRef.current;
    if (!term || !model) return;
    // Вид трассы — прежний «input» (контракт S1 не расширяем): это действие
    // человека; метка без текста (I-15).
    trace("input", null, `block-copy-${kind}`);
    flushTermWrites(() => {
      if (disposedRef.current || term !== terminalRef.current || model !== blockModelRef.current) return;
      const last = model.lastComplete();
      const range = last ? model.ranges(last)[kind] : null;
      const text = range ? blockText(term.buffer.normal, range, term.cols) : "";
      // Полноэкранная программа (less, vim): её экран в вывод не входит, и
      // человек видит это в подписи, а не догадывается по пустому буферу.
      const outcome = blockCopyOutcome(last, kind, range, text);
      if (outcome === "fullscreen-empty") { showToast(t("pty.blockFullscreenEmpty")); return; }
      void deliverCopy(text, outcome === "truncated" ? "pty.blockTruncated"
        : outcome === "fullscreen" ? "pty.blockFullscreen" : "pty.copied");
    });
  };

  /**
   * «↑ К команде» (ST-10): начало ближайшей команды выше окна. Прокручивает
   * ТОЛЬКО единственный исполнитель (scrollByLines → ScrollRouter, ST-03), и
   * только если единое правило навигации даёт этому намерению локальный путь.
   * Иначе ничего не отправляется: сотни строк PgUp приложению — это не переход
   * к команде.
   */
  const jumpToCommand = () => {
    haptic();
    const term = terminalRef.current, model = blockModelRef.current;
    if (!term || !model || term.buffer.active.type !== "normal") return;
    cancelGestureRef.current();
    const top = term.buffer.active.viewportY;
    const target = model.previousStart(top);
    if (target === null) { showToast(t("pty.blockNoPrevious")); refreshBlockUiRef.current(); return; }
    const lines = target - top;
    if (!shouldDriveViewportRef.current(term, lines)) {
      trace("input", { lines }, "block-jump-app");
      showToast(t("pty.blockJumpAppHint"));
      refreshBlockUiRef.current();
      return;
    }
    trace("input", { lines }, "block-jump");
    scrollByLinesRef.current(lines, { source: "button" });
  };

  // Ряд действий блоков: флаги из модели; «↑ К команде» спрашивает единое
  // правило навигации, только пока ряд инструментов на экране, и спрашивает
  // без побочных эффектов (peek): обновление ряда идёт на каждый разобранный
  // вывод и не должно снимать закрепление чтения или двигать гистерезис.
  const toolsVisibleRef = useRef(false);
  toolsVisibleRef.current = !state.alive || toolsOpen;
  refreshBlockUiRef.current = () => {
    const term = terminalRef.current, model = blockModelRef.current;
    let next = NO_BLOCK_UI;
    if (features.commandBlocks && term && model) {
      const actions = availableBlockActions(model, true);
      let jump: BlockUi["jump"] = "none";
      if (actions.jumpPrevious && toolsVisibleRef.current && term.buffer.active.type === "normal") {
        const top = term.buffer.active.viewportY;
        const target = model.previousStart(top);
        if (target !== null) jump = shouldDriveViewportRef.current(term, target - top, true) ? "local" : "app";
      }
      next = { copyCommand: actions.copyCommand, copyOutput: actions.copyOutput, jump, notice: blockNotice(model, true) };
    }
    setBlockUi(prev => sameBlockUi(prev, next) ? prev : next);
  };
  useEffect(() => { refreshBlockUiRef.current(); }, [toolsOpen, state.alive]);

  const changeSizeControl = (op: "claim" | "release" | "transfer", target?: string) => {
    const controls = sizeControlsRef.current, socket = wsRef.current;
    if (!controls || socket?.readyState !== WebSocket.OPEN || sizeControlPending) return;
    cancelGestureRef.current();
    try {
      socket.send(JSON.stringify({ t: "size-control", op, target, revision: controls.revision }));
      setSizeControlPending(true);
      sizeControlTimerRef.current = setTimeout(() => {
        sizeControlTimerRef.current = null;
        setSizeControlPending(false);
        showToast(t("pty.sizeNoReply"));
      }, 4000);
    } catch { showToast(t("pty.noConnection")); }
  };

  // ── IME-safe text input ──────────────────────────────────────
  // `force` приходит от нажатия Enter, которое САМО сказало, что композиции
  // сейчас нет (см. handleInputKeyDown). Наш React-флаг к этому моменту может
  // ещё стоять: на iOS `compositionend` приходит ПОСЛЕ `keydown`.
  const handleInputSend = async (opts?: { force?: boolean }) => {
    if (inputSendPendingRef.current) return;
    if (composing && !opts?.force) return;
    // Второй путь ввода человека (первый — term.onData): отсюда уходит то, что
    // набрано в поле «Сообщение/команда».
    lastUserInputAtRef.current = Date.now();
    panInputAtRef.current = lastUserInputAtRef.current;
    // Пустое поле = голый Enter (подтвердить выбранный пункт меню агента).
    // "\r" = Enter: без него команда только печаталась в терминал, но не
    // выполнялась (в спецклавишах Enter нет, добраться до него было нельзя).
    // Пока байты не ушли, поле и черновик НЕ чистим: раньше отправка при
    // отвалившейся связи стирала промпт на шесть строк вместе с localStorage —
    // набирать заново одной рукой. Сначала пробуем отправить, и только по
    // подтверждённой отправке закрываем композер и очищаем поле.
    const text = inputText;
    const target = inputTargetRef.current();
    inputSendPendingRef.current = true;
    try {
    const result = await reviewedInput(target, inputTargetRef.current, text, true,
      () => tgConfirm(t("pty.multilineReview")));
    if (disposedRef.current || target.draft !== inputTargetRef.current().draft) return;
    if (result === "cancelled") return;
    if (result !== "sent") {
      // Пустое поле = голый Enter: сохранять нечего, тогда обычное «нет связи».
      showToast(text ? t("pty.notSentKept") : t("pty.noConnection"));
      return;
    }
    // I-15: поле «Сообщение/команда» — тоже ввод человека. Открываем отрезок
    // набора, чтобы эхо отправленного (пароль в приглашение sudo) огрублялось,
    // как при наборе в терминал.
    noteHumanInput(text.length, text ? "encoded-paste" : "key");
    const editedWhileWaiting = inputTextRef.current !== text;
    setInputText(current => current === text ? "" : current);
    if (!editedWhileWaiting) setComposerOpen(false);
    haptic();
    // На сенсорных поверхностях фокус ВОЗВРАЩАЕТСЯ в поле: справка обещает
    // «пишите в нижнее поле», а прыжок в xterm ломал автозамену и предиктивный
    // ввод Gboard — каждую следующую фразу приходилось начинать тапом по полю.
    // В окне exe и на веб-десктопе фокус в терминале осмыслен (дальше печатают
    // с железной клавиатуры прямо в PTY), поэтому поведение платформенное.
    if (editedWhileWaiting || touchFirstUi) textInputRef.current?.focus();
    else terminalRef.current?.focus();
    } finally { inputSendPendingRef.current = false; }
  };

  const handleInputKeyDown = (e: React.KeyboardEvent<HTMLTextAreaElement>) => {
    // ⚠ Про композицию спрашиваем СОБЫТИЕ, а не наш React-флаг. На iPad и
    // iPhone Safari держит композицию открытой всё время автозамены и
    // предиктивного ввода, а `compositionend` присылает ПОСЛЕ `keydown`: флаг
    // в момент нажатия ещё true, отправка молча пропускалась, и системный
    // Enter просто переносил строку (живая жалоба владельца 10.09.2026 с
    // iPad). `isComposing` и код 229 — состояние самого нажатия, поэтому
    // настоящая композиция (подсказка Gboard на Android) по-прежнему Enter не
    // отдаёт: там флаг события честно поднят.
    // Правило одно для поля и композера (InputController.enterIntent, T-21):
    // признак композиции — у самого события (isComposing или код 229).
    if (enterIntent({ key: e.key, shiftKey: e.shiftKey, ctrlKey: e.ctrlKey, metaKey: e.metaKey,
      isComposing: e.nativeEvent.isComposing, keyCode: e.keyCode }, "field") === "submit") {
      e.preventDefault();
      handleInputSend({ force: true });
    }
  };

  // Клавиатура БОЛЬШОГО композера («Длинный запрос»). Его открывают ровно ради
  // многострочного промпта, а Shift+Enter на экранной клавиатуре Android/iOS
  // нажать нечем — с общим обработчиком первая же строка «сделай X:» + ↵
  // отправляла агенту половину запроса. Здесь Enter — перевод строки, отправка
  // остаётся за кнопкой «Отправить» (и за Ctrl/⌘+Enter с железной клавиатуры).
  const handleComposerKeyDown = (e: React.KeyboardEvent<HTMLTextAreaElement>) => {
    if (enterIntent({ key: e.key, shiftKey: e.shiftKey, ctrlKey: e.ctrlKey, metaKey: e.metaKey,
      isComposing: e.nativeEvent.isComposing, keyCode: e.keyCode }, "composer") === "submit") {
      e.preventDefault();
      handleInputSend({ force: true });
    }
  };

  const inputRows = Math.max(
    1,
    Math.min(5, inputText.split("\n").reduce((rows, line) => rows + Math.max(1, Math.ceil(line.length / 38)), 0)),
  );

  // ── Image upload ───────────────────────────────────────────
  // Любой файл (не только фото): заливаем на ПК во временную папку и печатаем
  // его путь в терминал. При нескольких файлах пути разделяем пробелом, чтобы
  // годились как отдельные аргументы команды/агента.
  /**
   * Путь загруженного файла — ТОЙ цели, что была при нажатии (I-08, T-19).
   * Загрузка идёт по REST и длится сколько угодно; за это время соединение
   * могло смениться, а в той же сессии — процесс. Цель та же — путь печатается
   * в терминал; сменилась или сокет мёртв — путь ложится в черновик ИСХОДНОЙ
   * сессии, и отправит его человек сам. Молча печатать в новое соединение
   * (прежняя очередь до onopen) нельзя: там уже может быть другой процесс.
   */
  const deliverUploadedPath = (line: string, target: InputTarget & { draft: string }): "sent" | "draft" => {
    if (sameInputTarget(target, inputTargetRef.current())
      && transmitInput(target.transport, { kind: "key", data: line })) {
      noteHumanInput(line.length, "key"); // I-15: путь файла — тоже ввод человека
      return "sent";
    }
    appendSessionDraft(target.draft, line);
    if (!disposedRef.current && target.draft === inputTargetRef.current().draft) setComposerOpen(true);
    return "draft";
  };

  const handleFileUpload = async (files: FileList | null) => {
    if (!files || files.length === 0 || uploading) return;
    // Цель ввода — до первого ожидания (I-08).
    const target = inputTargetRef.current();
    const controller = new AbortController();
    uploadAbortRef.current = controller;
    setUploading(true);
    setUploadProgress(0);
    try {
      const list = Array.from(files);
      // Continue-on-error: раньше первая же ошибка молча обрывала остальные
      // файлы. Теперь грузим все, пути успешных печатаем разом (пробел между
      // ними — отдельные аргументы команды/агента), а про упавшие — один тост.
      const paths: string[] = [];
      const failed: string[] = [];
      // Причины падений — рядом с именами: тост обязан НАЗЫВАТЬ виновника, а не
      // сообщать «не загружено» и оставлять человека гадать.
      const failReasons: string[] = [];
      // true, если путь не удалось напечатать сразу (сокет мёртв) и он лёг в
      // очередь до переподключения — тогда тост говорит именно об этом.
      let pathQueued = false;
      // true — путь не напечатан, а положен в черновик исходной сессии (I-08).
      let pathKept = false;
      for (let index = 0; index < list.length; index++) {
        const file = list[index];
        try {
          const result = await uploadPtyFile(
            file,
            (pct) => setUploadProgress(Math.round(((index + pct / 100) / list.length) * 100)),
            { signal: controller.signal, resumable: true },
          );
          paths.push(result.path);
        } catch (e: any) {
          if (controller.signal.aborted || e?.code === "aborted") break;
          failed.push(file.name);
          // ⚠ ПРИЧИНУ НЕ ГЛОТАТЬ. 13.08.2026 владелец не смог отправить файл, и
          // разбирать было НЕЧЕГО: тост называл имя файла, до компьютера запрос
          // не доходил вовсе, а ошибка оставалась здесь и умирала. Теперь она
          // едет в лог агента тем же diag-каналом, что и остальная телеметрия
          // терминала: без содержимого файла И без его имени (I-15) — маска
          // «*.ext», размер, тип, код и текст ошибки (имя из текста вычищено).
          // Тост человеку имя по-прежнему называет: он у него на экране.
          failReasons.push(`${file.name}: ${e?.code || ""} ${e?.message || e}`.trim());
          const uploadFields = uploadDiagFields(file, e);
          sendDiag("upload", uploadFields, {
            traceFields: { size: uploadFields.size, code: uploadFields.code, status: uploadFields.status },
          });
        }
      }
      if (paths.length > 0) {
        // Пути в кавычках: без quotePath файл «C:\Мои файлы b.png»
        // приходил агенту/шеллу склеенным и битым.
        const line = paths.map(quotePath).join(" ");
        if (features.inputSafety) {
          if (deliverUploadedPath(line, target) === "sent") hapticSuccess();
          else pathKept = true;
        } else if (sendRaw(line, { quiet: true })) {
          hapticSuccess();
        } else {
          // Сокет умер, пока шла загрузка: байты уже на ПК, поэтому путь не
          // выбрасываем — вставим его сразу после переподключения.
          pendingPathsRef.current.push(line);
          pathQueued = true;
        }
      }
      if (controller.signal.aborted) {
        showToast(t("pty.uploadCancelled"));
      } else if (pathKept) {
        showToast(t("pty.uploadPathDraft"));
      } else if (pathQueued) {
        showToast(t("pty.uploadPathQueued"));
      } else if (failed.length === 0) {
        if (paths.length === 1) showToast(t("pty.uploaded", { name: list[0].name }));
        else if (paths.length > 1) showToast(t("pty.uploadedMany", { n: paths.length }));
      } else {
        showToast(t("pty.uploadedPartial", {
          failed: failed.length,
          total: list.length,
          // Имя И причина: «REMOTAI_....md: 413 файл больше лимита» полезнее,
          // чем одно имя. Длинные сообщения ужимаем — тост не отчёт.
          names: failReasons.length ? failReasons.join("; ").slice(0, 160) : failed.join(", "),
        }));
      }
    } finally {
      uploadAbortRef.current = null;
      setUploading(false);
      setUploadProgress(0);
      if (fileInputRef.current) fileInputRef.current.value = "";
    }
  };

  // Handle paste events — intercept BEFORE xterm.js (capture phase).
  // On Android WebView, xterm.js's hidden textarea paste is unreliable
  // for large text — we bypass it and send directly via WebSocket.
  useEffect(() => {
    const el = termRef.current;
    if (!el) return;
    const onPaste = async (e: ClipboardEvent) => {
      const target = inputTargetRef.current();
      // One owner, synchronously, including empty clipboardData fallbacks.
      e.preventDefault();
      e.stopPropagation();
      const items = e.clipboardData?.items;

      // Images take priority.
      for (const item of items ?? []) {
        if (item.type.startsWith("image/")) {
          e.preventDefault();
          e.stopPropagation();
          const file = item.getAsFile();
          if (file) {
            const controller = new AbortController();
            uploadAbortRef.current = controller;
            setUploading(true);
            setUploadProgress(0);
            try {
              const result = await uploadPtyFile(
                file,
                setUploadProgress,
                { signal: controller.signal, resumable: true },
              );
              // Тот же приём, что при загрузке файлов скрепкой: обрыв моста не
              // отменяет REST-загрузку. Путь — цели, снятой до ожидания (I-08).
              if (features.inputSafety) {
                if (deliverUploadedPath(quotePath(result.path), target) === "sent") {
                  hapticSuccess();
                  showToast(t("pty.imageUploaded"));
                } else showToast(t("pty.uploadPathDraft"));
              } else if (sendRaw(quotePath(result.path), { quiet: true })) {
                hapticSuccess();
                showToast(t("pty.imageUploaded"));
              } else {
                pendingPathsRef.current.push(quotePath(result.path));
                showToast(t("pty.uploadPathQueued"));
              }
            } catch (err: any) {
              showToast(controller.signal.aborted || err?.code === "aborted"
                ? t("pty.uploadCancelled")
                : t("pty.uploadFailed"));
            } finally {
              uploadAbortRef.current = null;
              setUploading(false);
              setUploadProgress(0);
            }
          }
          return;
        }
      }

      // Text paste — bypass xterm.js textarea, send directly.
      let text = e.clipboardData?.getData("text/plain");
      // Fallback: Clipboard API (some Android WebViews have empty clipboardData).
      if (!text) {
        try { text = await terminalClipboard.read(); } catch {}
      }
      if (text && text.length > 0) {
        e.preventDefault();
        e.stopPropagation();
        await pasteTextRef.current(text, target);
      }
    };
    // capture: true — fire BEFORE xterm.js's own paste handler.
    el.addEventListener("paste", onPaste, true);
    return () => el.removeEventListener("paste", onPaste, true);
  }, [sendPaste]);

  const pasteText = async (text: string, target = inputTargetRef.current()) => {
      const result = await reviewedInput(target, inputTargetRef.current, text, false,
        () => tgConfirm(t("pty.multilineReview")));
      if (result === "cancelled") return;
      if (result !== "sent") {
        appendSessionDraft(target.draft, text);
        if (disposedRef.current || target.draft !== inputTargetRef.current().draft) return;
        setComposerOpen(true);
        showToast(t("pty.notSentKept"));
        return;
      }
      // I-15: вставка (менеджер паролей на телефоне) — тоже ввод человека.
      // Открываем отрезок набора, чтобы эхо «*» пароля не легло точным rx.bytes.
      noteHumanInput(text.length, "encoded-paste");
      if (disposedRef.current || target.draft !== inputTargetRef.current().draft) return;
      hapticSuccess();
      showToast(t("pty.pastedChars", { n: text.length }));
      terminalRef.current?.focus();
  };
  const pasteTextRef = useRef(pasteText);
  pasteTextRef.current = pasteText;

  const handlePaste = async () => {
    const target = inputTargetRef.current();
    haptic();
    try {
      const text = await terminalClipboard.read();
      if (text) await pasteText(text, target);
    } catch {
      // Clipboard API denied — fall back to an in-app prompt.
      if (disposedRef.current || target.draft !== inputTargetRef.current().draft) return;
      const text = await promptDialog(t("pty.pasteText"), { multiline: true });
      if (text) await pasteText(text, target);
    }
  };

  /**
   * «Снимок» — картинка экрана ПК уезжает АГЕНТУ, а не в галерею телефона.
   *
   * Просьба владельца 08.09: «удобное приложение, которое делает скрины, чтобы
   * сразу закидывать в агента было легко». До этого он держал для снимков
   * отдельную программу, сохранял PNG в папку и набирал путь руками.
   *
   * Путь ВСТАВЛЯЕТСЯ В ПОЛЕ, а не отправляется сразу: одна картинка без слов
   * агенту почти ничего не говорит — к ней нужен вопрос («почему тут пусто?»).
   * Отправить голый путь человек всегда успеет одним Enter.
   */
  const handleScreenshotToAgent = async () => {
    haptic();
    try {
      const shot = await saveScreenshotToPC();
      setInputText((prev) => (prev ? `${prev.replace(/\s+$/, "")} ${shot.path} ` : `${shot.path} `));
      // Фокус в поле, а не в терминал: следующий шаг — дописать вопрос.
      textInputRef.current?.focus();
      hapticSuccess();
      showToast(t("pty.screenshotToAgent", { name: shot.name }));
    } catch (e) {
      hapticError();
      showToast(mapApiError(e));
    }
  };

  // ── Состояние агента: вопрос, тип ответа, приглушение шелл-кнопок ──
  // status/hint/hint_kind приходят из /api/pty/{id}/state (раньше экран их не
  // получал вовсе). У старого агента полей нет — ведём себя как прежде:
  // полный набор клавиш и без полосы вопроса.
  // SSH-сессия: часть действий экрана относится к ПК-бастиону, а не к серверу,
  // на который вы смотрите (папка ПК, «Открыть на ПК», загрузка файла в TEMP
  // компьютера) — такие кнопки прячем.
  const isSsh = state.kind === "ssh" || state.shell === "ssh";
  // Telegram Mini App: сохранение файла из WebView не работает, поэтому
  // экспортная шторка предлагает доставку ботом первой (см. ниже).
  const inTelegram = !!getTelegram()?.initData;
  const sshCenterPath = state.ssh_host_id
    ? `/ssh/${encodeURIComponent(state.ssh_host_id)}`
    : "/ssh";
  // Куда ведёт «Назад». Дом у SSH-сессии двойной: карточка сервера и список
  // терминалов, — и раньше экран всегда выбирал первый. Открыв сервер из
  // «Терминалов», человек уходил назад в другой раздел. Теперь источник
  // называет тот, кто открыл (?from=), а без него остаётся прежнее правило.
  // Берём только внутренний путь: «//host» браузер считает чужим адресом.
  const fromParam = searchParams.get("from") || "";
  const backPath = isInnerPath(fromParam)
    ? fromParam
    : (isSsh ? sshCenterPath : "/pty");
  const agentInFg = isAgentKind(state.agent_kind);
  const entryMode = agentEntryMode({ sessionId: id || "", alive: !!state.alive, connected,
    agentKind: state.agent_kind, agentRunning: agentInFg, attempt: launchAttempt });
  const inputTargetLabel = entryMode === "agent"
    ? t("pty.inputAgent", { name: agentDisplayName(state.agent_kind) })
    : state.agent_kind === "shell" && connected
      ? t(/powershell|pwsh/i.test(state.shell || "") ? "pty.inputPowerShell" : "pty.inputShell")
      : t("pty.inputUnknown");
  const agentAsking = state.status === "waiting" && !!state.hint;
  /**
   * ⚠ БЕЙДЖ «✓ Готов» НЕ ГАСИМ ПО ТАЙМЕРУ, хотя он и закрывает часть строки
   * состояния агента в правом нижнем углу (аудит путей 29.08.2026).
   *
   * Попытка (2.61.0) прятать его через 4 секунды роняла `probe-scroll-edges`:
   * таймер вызывает перерисовку экрана терминала, и «⇊ в самый низ» переставало
   * доводить до края. Проверено обратным ходом — с выключенным гашением проба
   * снова зелёная. Любая правка этого экрана, которая ререндерит его САМА, без
   * действия человека и без события от ПК, обязана проверяться `qa:terminal`
   * целиком: терминал — самая настроенная часть продукта.
   */
  const answerKind = agentAsking ? state.hint_kind || "" : "";
  // Поля, которых пока нет в общем типе PtyState (packages/shared/src/types.ts
  // правит другая волна): читаем узким кастом, чтобы не ждать её.
  //   hint_options — подписи пунктов меню агента в порядке цифр;
  //   viewers      — сколько экранов открыто на этом же терминале.
  const stateExtra = state as PtyStateExtra;
  const viewers = stateExtra.viewers ?? 0;
  // Кнопки ответа для нумерованного меню — правило в ptyTerm/rules.ts.
  const choiceDigits = answerChoicesOf(stateExtra, agentAsking ? answerKind : "");
  // Кнопки в плашке вопроса — только те, которых нет прямо под ней: пункты меню
  // с подписями, «Да/Нет», Enter. Поле ввода и Esc стоят ниже; Esc повторяется
  // в плашке, лишь когда панель клавиш свёрнута. Владелец 13.09 со скриншотом:
  // «А зачем тут написать ответ и esc, это же в клаве есть».
  const askActions = answerKind === "choice" || answerKind === "yes_no" || answerKind === "enter" || keysCollapsedForView;
  // Ответ агенту. Цифра пункта уходит БЕЗ Enter (меню Claude/Codex срабатывает
  // сразу), «Да/Нет» — с Enter, и по экрану различить это нельзя: полноэкранный
  // агент перерисовывается целиком. Поэтому подтверждаем отправку тостом — иначе
  // человек жмёт кнопку второй раз и отвечает уже на следующий вопрос.
  const sendAnswer = (data: string, label: string) => {
    haptic();
    if (sendRaw(data)) showToast(t("pty.answerSent", { answer: label }));
  };
  // Заголовок — тот же канон, что в списке терминалов и на главной («Claude ·
  // myproj», «SSH · root@srv»): безликое «Терминал» не давало отличить открытый
  // терминал от четырёх остальных при возврате из фона. shell приходит в
  // /state всегда, кроме самого первого рендера (state ещё не загружен) —
  // тогда голова заголовка это «Терминал», а не английское "terminal" изнутри
  // ptyDisplayTitle.
  //
  // Ответ /state — это как минимум один сетевой запрос, а при выключенном ПК он
  // упирается в таймаут, и всё это время шапка молчала словом «Терминал».
  // Поэтому до первого ответа показываем имя прошлого захода, а как только
  // ответ пришёл — сохраняем настоящее имя на следующий раз.
  const stateLoaded = !!(state.name || state.shell || state.kind || state.cwd);
  const headerTitle = stateLoaded
    ? ptyDisplayTitle({ ...state, shell: state.shell || t("pty.terminal") }, { preferAgent: true })
    : (cachedTitle || t("pty.terminal"));
  useEffect(() => {
    if (!id) return;
    if (sessionMissing) forgetTitleCache(id);
    else if (stateLoaded && headerTitle) writeTitleCache(id, headerTitle);
  }, [id, sessionMissing, stateLoaded, headerTitle]);

  /**
   * Код привязки сервера, замеченный в выводе (см. ptyTerm/pairOffer.ts).
   *
   * `remotai pair` на новом сервере предлагал взять телефон и сканировать QR —
   * хотя запускают эту команду обычно ИЗ терминала Remotai на компьютере, где
   * человек уже вошёл в аккаунт («а я захожу с компьютера)»). Терминал сам
   * видит код и предлагает подтвердить одним нажатием.
   *
   * Проверяем на тике вывода, а не на каждый байт: чтение буфера xterm стоит
   * заметно дороже, чем сравнение строки.
   */
  const [pairCode, setPairCode] = useState("");
  const [pairing, setPairing] = useState(false);
  useEffect(() => {
    // Условие — НАЛИЧИЕ АККАУНТА, а не режим подключения.
    //
    // Первая версия проверяла `getMode() === "cloud"` и промахнулась мимо
    // главного случая: окно Remotai на самом компьютере работает в режиме
    // self_hosted (local-token мост), и кнопка не появилась бы именно там, где
    // человек её и ждёт («а я захожу с компьютера)»). Привязать сервер можно,
    // если есть облачный аккаунт — им и подтверждаем; в Telegram у клиента
    // Bearer-JWT нет (там tma), и путь остаётся прежний: бот и QR.
    if (!getCloudJWT() || !!getTelegram()?.initData) return;
    const scan = () => {
      const term = terminalRef.current;
      if (!term) return;
      const buf = term.buffer.active;
      const lines: string[] = [];
      const from = Math.max(0, buf.baseY + buf.cursorY - 60);
      for (let i = from; i <= buf.baseY + buf.cursorY; i++) {
        const line = buf.getLine(i);
        if (line) lines.push(line.translateToString(true));
      }
      setPairCode(pairCodeInOutput(lines.join("\n")));
    };
    return foregroundTask(scan, 1500);
  }, []);

  const confirmPair = async () => {
    if (!pairCode || pairing) return;
    haptic();
    setPairing(true);
    try {
      await runPair(getRelayBase(), pairCode, { deviceType: "server" });
      hapticSuccess();
      showToast(t("pty.pairDone"));
      setPairCode("");
    } catch (e) {
      showToast(mapApiError(e));
    } finally {
      setPairing(false);
    }
  };
  // «Связь идёт» или «процесс завершён» — правило в ptyTerm/rules.ts: экран не
  // должен одновременно говорить «Переподключение…» и «Процесс завершён».
  const { linkPending, processDead } = terminalLinkState({
    showConnecting, reconnecting, gaveUp, alive: state.alive,
  });
  // Что экран терминала говорит поверх вывода — в трассу при каждой смене
  // (ST-01: «появление overlay»). Эффект, а не запись в местах set*: причин у
  // оверлея шесть, и пропустить одну легко.
  useEffect(() => {
    trace("overlay", {
      connecting: showConnecting, reconnecting, gaveUp, link: linkSpeaks,
      updating: pcUpdating !== null, dead: processDead, missing: sessionMissing, refused: streamRefused,
    }, "state");
  }, [showConnecting, reconnecting, gaveUp, linkSpeaks, pcUpdating, processDead, sessionMissing, streamRefused]);
  const changeFontSize = (direction: -1 | 1) => {
    const index = FONT_SIZES.indexOf(fontSize as typeof FONT_SIZES[number]);
    const next = FONT_SIZES[Math.max(0, Math.min(FONT_SIZES.length - 1, index + direction))];
    if (next !== fontSize) {
      haptic();
      setFontSize(next);
    }
  };
  const terminalIsWindows = platform === "windows"
    || (!platform && (/powershell|cmd(?:\.exe)?$/i.test(state.shell || "") || /^[A-Za-z]:[\\/]/.test(cwd || "")));
  // Платформа ТОГО ШЕЛЛА, куда реально уходят байты. В SSH-сессии это удалённый
  // сервер, а не ПК-бастион: с Windows-ПК на Linux-сервер ряд быстрых команд
  // предлагал dir/cls/tasklist, и ни одна кнопка не работала («command not
  // found»). Платформу самого сервера агент не сообщает (state.kind="ssh",
  // shell="ssh", cwd — ярлык «ssh:user@host»), поэтому для SSH берём POSIX:
  // подавляющее большинство SSH-серверов именно такие. То же правило уже
  // применено к сниппетам и шторке запуска агента (см. ниже).
  const shellIsWindows = !isSsh && terminalIsWindows;
  const defaultQuickCmds = shellIsWindows
    ? ["dir", "cd ..", "cls", "tasklist", "Get-Location"]
    : ["ls", "pwd", "cd ..", "clear", "ps aux", "df -h", "journalctl -n 50 --no-pager"];
  // Оба платформенных набора: встроенная команда узнаётся по имени в любом из
  // них, иначе «dir» после переезда из Windows-терминала в SSH стал бы
  // неудаляемой «своей» командой.
  const allBuiltinCmds = [
    "dir", "cd ..", "cls", "tasklist", "Get-Location",
    "ls", "pwd", "clear", "ps aux", "df -h", "journalctl -n 50 --no-pager",
  ];
  // Кнопка ряда: ЧТО отправить (`cmd`) и ЧТО написать на ней (`label`) — разные
  // вещи. Пока ряд был массивом строк, подпись показать было негде, и заданная
  // в шторке «⚡ Команды» она нигде не появлялась. Для длинной заготовки агенту
  // («Проверь обращения пользователей…») подпись — единственный способ прочитать
  // кнопку, а не гадать по обрезанному тексту.
  const quickCommands: { cmd: string; label: string; own?: UserCommand }[] = [
    ...defaultQuickCmds.filter((c) => !hiddenCmds.includes(c)).map((c) => ({ cmd: c, label: c })),
    // Свои команды — после платформенных: человек добавляет то, чем пользуется
    // чаще `ls`, и видит их в конце ряда, а не ищет среди чужих. Совпадающие
    // со встроенными не дублируем: встроенная возвращается снятием из скрытых.
    ...pinnedOf(userCmds)
      .filter((c) => !allBuiltinCmds.includes(c.cmd))
      .map((c) => ({ cmd: c.cmd, label: commandLabel(c), own: c })),
  ];
  const toggleQuickCmds = () => {
    haptic();
    setQuickCmdsCollapsed((prev) => {
      const next = !prev;
      try { localStorage.setItem("ptyQuickCmdsCollapsed", next ? "1" : "0"); } catch { /* ignore */ }
      // Разворот — не просто смена состояния: команды появляются РОВНО ТАМ, где
      // человек только что нажимал полосу, и следующее касание рискует уйти в
      // команду, которой в тот момент ещё не было. Ряд эти миллисекунды нажатий
      // не принимает (см. ptyTerm/tapGuard.ts).
      if (!next) tapGuardRef.current = noteExpanded(tapGuardRef.current, Date.now());
      return next;
    });
  };

  /**
   * Нажатие в ряду быстрых команд — только если это действительно нажатие.
   *
   * Команда отсюда уходит в терминал СРАЗУ, без подтверждения (решение 2.46.12),
   * поэтому цена случайного срабатывания — выполненная команда, а не опечатка.
   * Правила живут отдельным модулем и проверяются тестами без DOM.
   */
  const quickTap = (action: () => void) => () => {
    const verdict = tapVerdict(tapGuardRef.current, Date.now());
    if (!verdict.ok) return;
    action();
  };
  const persistHiddenCmds = (next: string[]) => {
    setHiddenCmds(next);
    try { localStorage.setItem("ptyQuickCmdsHidden", JSON.stringify(next)); } catch { /* ignore */ }
  };
  const addCustomCmd = () => {
    const cmd = newCmd.trim();
    if (!cmd) return;
    haptic();
    // Исход считаем ЯВНО и говорим о нём разными словами.
    //
    // Раньше тост «Команда сохранена» показывался безусловно — в том числе
    // когда не сохранялось РОВНО НИЧЕГО. Ветка «встроенная» узнавала команду по
    // ОБОИМ платформенным наборам, поэтому `ls`, вписанная в терминале Windows,
    // попадала в неё, в скрытых её не было, и функция молча выходила, показав
    // зелёный тост. Замер (390 px, сессия PowerShell): вписал `ls` → тост
    // «Команда сохранена» → `ptyQuickCmdsCustom` = null, ряд байт в байт
    // прежний. Это ровно та жалоба, которую чинили в 2.48.12 («пробовал
    // добавить, но что-то не сохранилось»), только теперь с прямой неправдой
    // вместо молчания.
    //
    // ПРАВИЛО: тост об успехе печатает только тот, кто успех совершил.
    let outcome: "added" | "returned" | "already";
    if (defaultQuickCmds.includes(cmd)) {
      // Встроенная ЭТОГО терминала: вписать её — значит вернуть в ряд, а не
      // завести вторую такую же кнопку (промах мимо нужной, а не «две команды»).
      if (hiddenCmds.includes(cmd)) {
        persistHiddenCmds(hiddenCmds.filter((c) => c !== cmd));
        outcome = "returned";
      } else {
        outcome = "already";
      }
    } else if (customCmds.includes(cmd)) {
      outcome = "already";
    } else {
      // Сюда попадает и встроенная ЧУЖОЙ платформы — и это правильно: `ls` в
      // PowerShell работает (псевдоним Get-ChildItem), а в ряду этого терминала
      // её нет. Человек попросил кнопку — он её получает.
      void upsertUserCommand({ cmd, pinned: true })
        .then(({ commands }) => setUserCmds(commands))
        .catch(() => showToast(t("pty.quickCmdSaveFailed")));
      outcome = "added";
    }
    setNewCmd("");
    // Результат стоит в ряду, а ряд мог быть свёрнут — тогда подтверждения не
    // видно вовсе (та же жалоба 30.07). Разворачиваем, но только когда есть что
    // показать.
    if (quickCmdsCollapsed && outcome !== "already") {
      setQuickCmdsCollapsed(false);
      try { localStorage.setItem("ptyQuickCmdsCollapsed", "0"); } catch { /* ignore */ }
    }
    showToast(t(
      outcome === "added" ? "pty.quickCmdSaved"
        : outcome === "returned" ? "pty.quickCmdReturned"
          : "pty.quickCmdAlready",
    ));
  };
  /**
   * Закрытие шторки своих команд. Набранный, но не добавленный текст — это
   * намерение сохранить: человек печатает команду и закрывает окно, считая
   * дело сделанным. Молча терять его нельзя (та же жалоба 30.07).
   */
  const closeCmdSheet = () => {
    if (newCmd.trim()) addCustomCmd();
    // Незакрытая правка — тоже намерение сохранить, по тому же правилу, что и
    // набранный, но не добавленный текст выше.
    if (editCmd && editCmd.cmd.trim()) saveEditCmd();
    setEditCmd(null);
    setCmdSheetOpen(false);
  };

  /**
   * Системная «Назад» (Android, свайп, кнопка Telegram) закрывает состояния
   * самого терминала, а не выкидывает в список терминалов: длинный запрос,
   * окно своих команд, экспорт, переименование, режим выделения. До этого в
   * файле не было ни одного useEscape — «Назад» при открытой шторке уводила на
   * `/pty`, шторка оставалась висеть, а набранное в окне «+» терялось
   * (аудит ИА 02.09.2026, P1-8/9/14). Регистрация LIFO: закрывается то, что
   * открыто последним. На перерисовку экрана хуки не влияют (qa:terminal).
   */
  useEscape(composerOpen, () => setComposerOpen(false));
  useEscape(attachMenuOpen, () => setAttachMenuOpen(false));
  useEscape(cmdSheetOpen, closeCmdSheet);
  useEscape(exportOpen && !exporting, () => setExportOpen(false));
  useEscape(renaming, handleRenameCancel);
  useEscape(selectMode, toggleSelectMode);
  /**
   * Сохранить правку своей команды.
   *
   * Пустой текст — это не «стереть команду», а промах: удаление живёт на
   * крестике рядом, и молча превращать одно в другое нельзя. Закрепление не
   * трогаем: человек правил текст, а не убирал кнопку из ряда.
   */
  const saveEditCmd = () => {
    if (!editCmd) return;
    const cmd = editCmd.cmd.trim();
    if (!cmd) return;
    haptic();
    const label = editCmd.label.trim();
    void upsertUserCommand({ id: editCmd.id, cmd, label, pinned: true })
      .then(({ commands }) => {
        setUserCmds(commands);
        showToast(t("pty.quickCmdUpdated"));
      })
      .catch(() => showToast(t("pty.quickCmdSaveFailed")));
    setEditCmd(null);
  };
  const removeQuickCmd = (cmd: string) => {
    haptic();
    const own = userCmds.find((c) => c.cmd === cmd && c.pinned);
    if (own && !allBuiltinCmds.includes(cmd)) {
      // Из РЯДА убираем откреплением, а не удалением: команда остаётся в
      // шторке «⚡ Команды», откуда её можно вернуть или удалить насовсем.
      void upsertUserCommand({ id: own.id, cmd: own.cmd, label: own.label, pinned: false })
        .then(({ commands }) => setUserCmds(commands))
        .catch(() => showToast(t("pty.quickCmdSaveFailed")));
    } else if (!hiddenCmds.includes(cmd)) {
      persistHiddenCmds([...hiddenCmds, cmd]);
    }
  };
  // Файлы SSH-сессии лежат на СЕРВЕРЕ, а не на ПК: и скрепка, и «Скачать»
  // ведут в SFTP-обзор (/ssh-files?host=…), иначе обзор открывался по пути
  // «ssh:user@host» и сразу падал ошибкой листинга.
  // Откуда пришли: «Назад» из файлов сервера должно возвращать в ЭТОТ терминал,
  // а не в список серверов (хвост находки #9 файлового аудита). Разбор from —
  // на стороне /ssh-files; лишний параметр старой сборке не мешает.
  const sshFilesFrom = id ? `/pty/${id}` : "";
  const openSshFiles = async () => {
    if (state.ssh_host_id) {
      const q = new URLSearchParams({ host: state.ssh_host_id, path: "", from: sshFilesFrom });
      navigate(`/ssh-files?${q.toString()}`);
      return;
    }
    // Хост не сохранён в SSH-центре (разовое подключение или «Недавние»).
    // Раньше здесь был тупик: тост «Сохраните сервер в SSH-центре» и выброс в
    // список — забрать лог с сервера, к которому уже подключён, было нельзя, а
    // адрес/логин/порт приходилось вбивать заново. Теперь сохраняем прямо
    // отсюда: все поля уже известны из самой сессии. Пароль здесь не трогаем —
    // сохранять его человек решает сам галочкой «Запомнить пароль».
    const host = state.ssh_host || "";
    if (!host || savingHost) {
      showToast(t("pty.sshFilesUnsaved"));
      navigate("/ssh");
      return;
    }
    const ok = await tgConfirm(t("pty.sshSaveHostAsk", { host }), {
      confirmText: t("pty.sshSaveHost"),
      cancelText: t("modal.cancel"),
    });
    if (!ok) return;
    setSavingHost(true);
    try {
      const res = await createSshHost({
        name: host,
        host,
        user: state.ssh_user || "",
        port: state.ssh_port || 22,
        proxy_jump: state.ssh_proxy_jump || undefined,
      });
      hapticSuccess();
      showToast(t("pty.sshHostSaved"));
      const q = new URLSearchParams({ host: res.host.id, path: "", from: sshFilesFrom });
      navigate(`/ssh-files?${q.toString()}`);
    } catch (e: any) {
      showToast(mapApiError(e));
    } finally {
      setSavingHost(false);
    }
  };
  /**
   * Скрепка. Если снимать нечего (нет экрана у компьютера) или мы смотрим на
   * SSH-сервер, где нашего пути нет, — выбирать не из чего, и лишний тап был бы
   * платой ни за что: ведём себя как раньше, сразу.
   */
  const canScreenshot = hasDisplay && !isSsh;
  const handleAttachClick = () => {
    haptic();
    if (isSsh) {
      void openSshFiles();
      return;
    }
    if (canScreenshot) {
      setAttachMenuOpen(true);
      return;
    }
    fileInputRef.current?.click();
  };

  // Navigation owns existing header space, not output or keypad pixels.
  const scrollNavigation = (
    <div
      className={`pty-scroll-buttons${scrollButtonsVisible || altScrolledUp || localScrolledUp ? " visible" : ""}`}
      onPointerDown={revealScrollButtons}
    >
      <div className="pty-scroll-navigation">
        <button className="pty-scroll-btn" onClick={scrollTop} title={t("pty.a11y.scrollTop")} aria-label={t("pty.a11y.scrollTop")}><span aria-hidden>{"\u21C8"}</span></button>
        <button className="pty-scroll-btn" onClick={scrollUp} title={t("pty.a11y.scrollUp")} aria-label={t("pty.a11y.scrollUp")}><span aria-hidden>{"\u2191"}</span></button>
        <button className="pty-scroll-btn" onClick={scrollDown} title={t("pty.a11y.scrollDown")} aria-label={t("pty.a11y.scrollDown")}><span aria-hidden>{"\u2193"}</span></button>
      </div>
      <button className="pty-scroll-btn" onClick={scrollBottom} title={t("pty.catchUp")} aria-label={t("pty.catchUp").replace(/^↓\s*/, "")}><span aria-hidden>{"\u21CA"}</span></button>
    </div>
  );

  return (
    <div className="pty-page" data-tools-open={!state.alive || (!keysCollapsedForView && toolsOpen) ? "" : undefined}>
      <div className="pty-header pty-header--clear">
        <div className="pty-header-top">
          <button
            className="back-btn pty-back-btn"
            onClick={() => { haptic(); navigate(backPath); }}
            aria-label={t("pty.a11y.back")}
          >
            <IconArrow dir="left" size={22} />
          </button>
          {/* Имя правится на месте, путь копируется отдельной строкой.
              Свободное место вокруг пути тоже остаётся целью копирования. */}
          <div className="pty-header-center" onClick={handleCopyCwd}>
            <div className="pty-title-row">
              {renaming ? (
                /* Имя правится ТУТ ЖЕ, в шапке — не отдельным окном: «раньше я
                   название менял прямо там, где название терминала» (владелец,
                   31.08.2026). Высота поля равна высоте строки заголовка: шапка
                   не должна расти, иначе правка имени пересчитает геометрию PTY.
                   Сохранение только по «✓»/Enter — про onBlur см. handleRenameSave. */
                <span className="pty-title-edit" onClick={(e) => e.stopPropagation()}>
                  <input
                    className="pty-title-input"
                    autoFocus
                    value={renameDraft}
                    placeholder={t("pty.renamePlaceholder")}
                    aria-label={t("pty.renameAction")}
                    onChange={(e) => setRenameDraft(e.target.value)}
                    onKeyDown={(e) => {
                      e.stopPropagation();
                      if (e.key === "Enter") { e.preventDefault(); void handleRenameSave(); }
                      else if (e.key === "Escape") { e.preventDefault(); handleRenameCancel(); }
                    }}
                  />
                  <button
                    className="pty-title-ok"
                    aria-label={t("dialog.confirm")}
                    title={t("dialog.confirm")}
                    onClick={(e) => { e.stopPropagation(); void handleRenameSave(); }}
                  >
                    <IconCheck size={16} />
                  </button>
                  <button
                    className="pty-title-cancel"
                    aria-label={t("modal.cancel")}
                    title={t("modal.cancel")}
                    onClick={(e) => { e.stopPropagation(); handleRenameCancel(); }}
                  >
                    <IconClose size={14} />
                  </button>
                </span>
              ) : (
                <button
                  className="pty-title-main"
                  aria-label={`${t("pty.renameAction")}: ${headerTitle}`}
                  title={t("pty.renameAction")}
                  onClick={(e) => { e.stopPropagation(); startRename(); }}
                >
                  {/* Обрезается ГОЛОВА, а не хвост.
                      Заголовок собирается как «агент · папка» (ptyDisplayTitle), и
                      на телефоне в него не влезает ни один из двух терминалов
                      владельца: «Claude Code · TGControl-ALL» просит 211 px при
                      доступных 147. Обычный ellipsis резал хвост — то есть ровно
                      имя папки, единственное, чем один терминал отличается от
                      другого; агент при этом и так назван бейджем рядом. Теперь
                      сжимается голова: «Claude C… · TGControl-ALL». */}
                  <span className="pty-title" title={headerTitle}>
                    {(() => {
                      const sep = " · ";
                      const cut = headerTitle.lastIndexOf(sep);
                      if (cut < 0) return <span className="pty-title-tail">{headerTitle}</span>;
                      return (
                        <>
                          <span className="pty-title-head">{headerTitle.slice(0, cut + sep.length)}</span>
                          <span className="pty-title-tail">{headerTitle.slice(cut + sep.length)}</span>
                        </>
                      );
                    })()}
                  </span>
                  <span className="pty-title-rename" aria-hidden="true">
                    <IconPencil size={14} />
                  </span>
                </button>
              )}
            </div>
            <div className="pty-header-meta">
              {cwd && (
                <button className="pty-cwd-row" title={cwd} aria-label={t("pty.headerCopyPath")}>
                  <span className="pty-cwd" title={cwd}>{cwd}</span>
                  <IconCopy size={12} />
                </button>
              )}
              {isSsh && <span className="pty-ssh-badge">SSH</span>}
              {viewers > 1 && (
                <span className="pty-viewers" title={t("pty.viewersHint", { n: viewers })}>
                  <IconEye size={13} /> {viewers}
                  <span className="sr-only">{t("pty.viewersHint", { n: viewers })}</span>
                </span>
              )}
              {state.account_label && (
                <span className="pty-account" title={t("pty.accountHint", { name: state.account_label })}>
                  {state.account_label}
                </span>
              )}
              {state.agent_kind && state.agent_kind !== "shell" && !agentInFg && (
                <ProcessBadge kind={state.agent_kind as AgentKind} name={state.fg_process} />
              )}
            </div>
          </div>
          {scrollNavigation}
          <div className="pty-header-state">
            <span
              className={`pty-conn-status ${connected && state.alive ? "online" : "offline"}`}
              role="status"
              aria-live="polite"
              aria-label={connected && state.alive ? t("pty.connOnline") : t("pty.connOffline")}
            >
              <span className="pty-conn-dot" aria-hidden="true" />
              <span>{connected && state.alive ? t("pty.headerOnline") : t("pty.headerOffline")}</span>
            </span>
            {/* Признак записи вывода (I-15, волна 4): виден в КАЖДОМ терминале,
                который пишет, и ведёт к листу, где запись выключают и удаляют. */}
            {recUntil > 0 && (
              <button
                type="button"
                className="pty-rec-badge"
                onClick={() => { haptic(); setExportOpen(true); openTrace(); }}
                title={t("pty.traceRecHint", { time: clockLabel(recUntil) })}
                aria-label={t("pty.traceRecHint", { time: clockLabel(recUntil) })}
              >
                <span className="pty-rec-dot" aria-hidden="true" />
                {t("pty.traceRecBadge")}
              </button>
            )}
            {agentInFg && connected && (
              /* ⚠ «Не отвечает» перебивает и «работает», и «готов».
                 01.09.2026 у владельца зависли две сессии Claude Code: процесс жив,
                 экран пуст, клавиш не слышит — а бейдж писал «Готов». Час ушёл на
                 разбор, и весь этот час экран уверял, что всё хорошо. Молчание
                 после ввода — единственное состояние, о котором человек обязан
                 узнать сразу, поэтому оно и первое в цепочке. */
              <div
                className={`pty-agent-status ${
                  agentStuck ? "stuck" : agentAsking ? "asking" : agentBusy ? "busy" : "idle"
                }`}
                aria-live="polite"
              >
                {agentStuck
                  ? <span className="pty-agent-status-ask" aria-hidden>{"⚠"}</span>
                  : agentAsking
                    ? <span className="pty-agent-status-ask" aria-hidden>{"⏳"}</span>
                    : agentBusy
                      ? <span className="pty-agent-status-dot" />
                      : <span className="pty-agent-status-check">{"✓"}</span>}
                <span title={agentStuck ? t("pty.agentStuckHint") : undefined}>
                  {agentStuck
                    ? t("pty.agentStuck")
                    : agentAsking ? t("pty.agentAsks") : agentBusy ? t("pty.agentBusy") : t("pty.agentIdle")}
                </span>
                {/* Ход прямо в бейдже: сказать «не отвечает» и не дать выхода —
                    то же молчание, только словами. Кнопка делает единственное, что
                    зависший агент слышит, — просит перерисовать экран. */}
                {agentStuck && (
                  <button
                    className="pty-agent-status-fix"
                    onClick={(e) => { e.stopPropagation(); redrawAgentScreen(); }}
                    title={t("pty.agentRedraw")}
                    aria-label={t("pty.agentRedraw")}
                  >
                    <IconRefresh size={15} />
                  </button>
                )}
              </div>
            )}
          </div>
        </div>
        <div className="pty-header-actions" role="group" aria-label={t("pty.headerActions")}>
          <TerminalModeMenu
            value={scrollOverride}
            onChange={next => {
              haptic();
              saveScrollPreference(terminalContext, id ?? "", next);
              // Явный выбор человека — сигнал, что автоматика ошиблась:
              // её запомненный приговор стираем (scrollProbeMemory.ts).
              forgetScrollProbeVerdict(terminalContext, id ?? "");
              chooseScrollOverride(next);
            }}
          />
          {/* Эти действия относятся к папке локального компьютера.
              В SSH они по-прежнему скрыты: чужой путь нельзя применить к нему. */}
          {!isSsh && (
            <>
              <button
                className="pty-header-action pty-folder-btn"
                onClick={() => { haptic(); setFolderOpen(true); }}
                title={t("folder.title")}
                aria-label={t("folder.title")}
              >
                <IconFolder size={18} />
                <span>{t("pty.headerFolder")}</span>
              </button>
              <button
                className={`pty-header-action pty-cwd-fav${cwdPinned ? " is-on" : ""}`}
                onClick={() => { void toggleFavorite(); }}
                disabled={pinning || !cwd || cwdPinned === null}
                title={cwdPinned ? t("pty.unpinFolder") : t("pty.pinFolder")}
                aria-label={cwdPinned ? t("pty.unpinFolder") : t("pty.pinFolder")}
                aria-pressed={cwdPinned === true}
              >
                <IconStar size={18} filled={cwdPinned === true} />
                <span>{cwdPinned ? t("pty.headerFavoriteOn") : t("pty.pinFolder")}</span>
              </button>
            </>
          )}
          <button
            className="pty-header-action pty-help-btn"
            onClick={() => { haptic(); setHelpOpen(true); }}
            title={t("help.title")}
            aria-label={t("help.title")}
          >
            <IconHelp size={18} />
            <span>{t("pty.headerHelp")}</span>
          </button>
        </div>
        <TerminalWidthNotice
          narrow={connected && shouldExplainNarrowOutput(termSize.cols, reportSizeRef.current.cols)}
          onReopen={onReopen}
        />
      </div>

      {/* Bootstrap-баннер для SSH-сессии (?ssh=1&host=…): install.sh сам
          ставит бинарь, автозапуск и печатает QR привязки — всё видно прямо
          в терминале. В обычных (не-ssh) терминалах баннер не показывается. */}
      {sshHost && showSSHInstall && !sshBannerOff && (
        <div className="pty-ssh-banner">
          <span className="pty-ssh-banner-text">
            {t("ui.ptytermview.mc6a69aa617")}{sshHost}{t("ui.ptytermview.md722764619")}</span>
          <button
            className="pty-key-btn pty-key-accent"
            onClick={() => { haptic(); sendRaw("curl -fsSL https://remotai.ru/install.sh | sh\r"); }}
          >
            {t("ui.ptytermview.m8fae251a47")}</button>
          <button
            className="pty-ssh-banner-close"
            aria-label={t("pty.a11y.hideBanner")}
            onClick={() => { haptic(); setSshBannerOff(true); }}
          >
            {"✕"}
          </button>
        </div>
      )}

      {/* Компьютер применяет обновление и вот-вот перезапустится. Событие
          приходит ДО обрыва, поэтому плашка успевает объяснить, что сейчас
          произойдёт: раньше человек видел только «Переподключение… (1/10)»
          посреди работы агента. */}
      {pcUpdating !== null && (
        <div className="pty-update-bar" role="status" aria-live="polite">
          <span className="pty-update-icon" aria-hidden>{"↻"}</span>
          <span className="pty-update-text">
            {pcUpdating ? t("pty.pcUpdating", { version: pcUpdating }) : t("pty.pcUpdatingNoVersion")}
          </span>
          <span className="pty-update-hint">
            {isSsh ? t("pty.sshEphemeral") : t("pty.pcUpdatingHint")}
          </span>
        </div>
      )}

      {/* First-connect overlay — the reverse tunnel takes a moment over cloud.
          Если связи с ПК нет вовсе, про это уже говорит верхняя полоса: ждать
          «Подключение к терминалу…» тут нечего и обещать нечего. */}
      {showConnecting && !reconnecting && !gaveUp && !linkSpeaks && (
        <div className="pty-overlay">
          {streamRefused ? t("pty.streamRefused") : t("pty.connecting")}
        </div>
      )}

      {/* Отвалился поток самого терминала при живой связи с ПК — беда ДРУГАЯ,
          чем «нет связи», и названа своими словами. Когда отвалилась связь
          целиком, сообщение одно и оно наверху: раньше человек читал про один
          обрыв сразу два разных текста. Исключение — обновление компьютера: там
          оверлей объясняет ПРИЧИНУ обрыва, и она ценнее общего «нет связи». */}
      {reconnecting && !gaveUp && (pcUpdating !== null || !linkSpeaks) && (
        <div className="pty-overlay" style={{ flexDirection: "column", gap: 6, textAlign: "center", padding: "0 24px" }}>
          <div>{pcUpdating !== null ? t("pty.pcUpdatingWait") : t("pty.streamLost")}</div>
          {/* Главный страх при обрыве — «моя работа пропала». Она не пропала:
              процесс живёт на компьютере, обрывается только связь с ним. Об
              этом молчали ровно там, где это важнее всего (аудит путей
              29.08.2026); говорим, только когда компьютер сам подтвердил, что
              терминал жив. */}
          {pcUpdating === null && state.alive && (
            <div style={{ fontSize: 13, opacity: 0.7 }}>{t("pty.streamLostAlive")}</div>
          )}
        </div>
      )}

      {/* После десяти попыток продолжаем capped retry в фоне. Новый терминал
          предлагаем только после REST-проверки и честного 404. */}
      {gaveUp && state.alive && (
        <div className="pty-overlay" style={{ flexDirection: "column", gap: 12, textAlign: "center", padding: "0 24px" }}>
          {/* Канон раздела — «терминал»: человек закрывает терминалы, а не
              сессии, а «сессиями» в настройках зовутся входы в аккаунт. */}
          <div style={{ fontWeight: 600 }}>
            {sessionMissing ? t("pty.termGone") : t("pty.termRetrying")}
          </div>
          <div style={{ fontSize: 13, opacity: 0.7 }}>
            {sessionMissing ? t("pty.termGoneHint") : t("pty.termRetryingHint")}
          </div>
          <div style={{ display: "flex", gap: 8 }}>
            {!sessionMissing && (
              <>
                <button className="pty-key-btn pty-key-accent" onClick={() => void verifyLostSession()}>
                  {t("pty.checkTerm")}
                </button>
                <button className="pty-key-btn" onClick={retryNow}>{t("pty.reconnect")}</button>
              </>
            )}
            {sessionMissing && (isSsh ? (
              // Та же развилка, что и в .pty-dead-bar ниже: SSH отсюда не поднять
              // (нужны пароль и подтверждение ключа — они в списке серверов), а
              // «перезапуск здесь» создал бы обычный терминал ПК с cwd-ярлыком
              // «ssh:user@host».
              <button className="pty-key-btn" onClick={() => { haptic(); navigate(sshCenterPath); }}>
                {t("pty.sshReconnect")}
              </button>
            ) : (
              <button className="pty-key-btn" onClick={handleRestartHere} disabled={restarting}>
                {restarting ? t("pty.restarting") : t("pty.restartHere")}
              </button>
            ))}
          </div>
        </div>
      )}

      {/* Terminal container */}
      <div
        className="pty-terminal-wrap"
        // Столбик ⇈↑↓⇊ будим и мышью. Раньше единственным живым вызовом был
        // touchstart, а собственный onPointerDown кнопок недостижим: скрытый
        // ряд стоит с `pointer-events: none`. На мышиных поверхностях (окно
        // exe, веб, Telegram Desktop) столбик показывался две секунды при
        // открытии терминала и не возвращался больше никогда. Слушаем ОБЁРТКУ,
        // а не сам ряд, и только мышь — у касания свой путь, и дублировать его
        // нельзя: жест прокрутки сыпал бы событиями.
        onMouseMove={revealScrollButtons}
        onWheel={revealScrollButtons}
      >
        {/* Панель поиска живёт ВНУТРИ обёртки вывода (у неё position: relative):
            как прямой потомок .pty-page она позиционировалась от вьюпорта и
            ложилась на шапку — на телефоне с вырезом закрывала «←», а при
            баннере связи ещё и съезжала относительно сдвинутой страницы. */}
        {searchOpen && (
          <PtySearchBar
            terminal={terminalRef.current}
            onClose={() => setSearchOpen(false)}
          />
        )}
        <div
          ref={termRef}
          className={`pty-terminal${selectMode ? " select-mode" : ""}`}
          onClick={handleTap}
        />
        {readView && <ReadSurface key={readView.document.id} {...readView} hasNewOutput={readHasNewOutput} onClose={closeReading} />}
        {/* Плашка про устаревший агент — поверх терминала сверху по центру,
            буфер не трогает (absolute, как бейдж активности ниже). Жесты
            терминала она не ловит: pointer-events только на крестике. */}
        {agentUpdate && (
          <div className={`pty-agent-update ${agentUpdate.state}`} role="status">
            <span className="pty-agent-update-text">
              {agentUpdate.state === "stuck"
                ? t("pty.agentUpdateStuck", { version: `v${agentUpdate.version}` })
                : t("pty.agentUpdateOutdated", {
                    version: `v${agentUpdate.version}`,
                    latest: `v${agentUpdate.latest}`,
                  })}
            </span>
            <button
              type="button"
              className="pty-agent-update-close"
              aria-label={t("common.dismiss")}
              onClick={() => {
                haptic();
                dismissAgentUpdateNotice(agentUpdate.latest);
                setAgentUpdate(null);
              }}
            >
              {"✕"}
            </button>
          </div>
        )}
        {/* Плашка «агент закончил» — тот же слой, что плашка обновления выше:
            absolute поверх терминала, буфер не трогает, жесты не ловит
            (pointer-events только на крестике). Зелёная — это хорошая новость;
            при одновременной плашке обновления сидит ниже неё (top в CSS). */}
        {agentDone && (
          <div className="pty-agent-update pty-agent-finished" role="status">
            <span className="pty-agent-update-text">
              {"✅ "}
              {t("pty.agentFinished", {
                who: agentDone.who,
                duration: formatDurationMs(agentDone.ms),
              })}
            </span>
            <button
              type="button"
              className="pty-agent-update-close"
              aria-label={t("common.dismiss")}
              onClick={() => {
                haptic();
                if (agentDoneTimer.current) clearTimeout(agentDoneTimer.current);
                setAgentDone(null);
              }}
            >
              {"✕"}
            </button>
          </div>
        )}
      </div>

      {/* Toast notification */}
      {toast && <div className="pty-toast">{toast}</div>}

      {/* Процесс завершён: ввод и спецклавиши бессмысленны, но вывод как раз и
          нужен (упал агент — нужен трейс). Прячем только строку ввода, первый
          ряд клавиш и быстрые команды; ряд инструментов (выделить/копировать/
          поиск/⤓экспорт/скачать) остаётся. В плашку добавлена «Сохранить лог»:
          раньше её единственная кнопка уводила на НОВЫЙ id, и вывод терялся.
          Пока идёт (пере)подключение, плашку не показываем: экран не должен
          сразу и «Переподключение…», и «Процесс завершён» — см. processDead. */}
      {/* Агент спит: терминал жив, в нём шелл, беседа ждёт продолжения. Та же
          плашка, что у завершённого процесса, — одна кнопка «Разбудить». */}
      {state.alive && !processDead && state.sleep && (
        <div className="pty-dead-bar pty-sleep-bar" role="status">
          <span className="pty-dead-text">
            {"💤 "}{t("pty.sleepingBar", { name: sleepName(state.sleep.agent) })}
          </span>
          <div className="pty-dead-actions">
            <button
              className="pty-key-btn pty-key-accent"
              onClick={() => void handleWakeAgent()}
              disabled={sleepBusy}
            >
              {sleepBusy ? t("pty.waking") : t("pty.wakeAgent")}
            </button>
          </div>
        </div>
      )}

      {processDead && (
        <div className="pty-dead-bar">
          <span className="pty-dead-text">{isSsh ? t("pty.sshExited") : t("pty.processExited")}</span>
          <div className="pty-dead-actions">
            <button
              className="pty-key-btn"
              onClick={() => { haptic(); setExportOpen(true); }}
              title={t("pty.saveLog")}
            >
              <span aria-hidden>{"⤓"}</span> {t("pty.saveLog")}
            </button>
            {isSsh ? (
              // SSH отсюда не перезапустить: нужны пароль и подтверждение ключа,
              // они живут в списке серверов. «Перезапустить в этой папке» на SSH
              // создавал бы обычный терминал ПК с cwd-ярлыком «ssh:user@host».
              <button className="pty-key-btn pty-key-accent" onClick={() => { haptic(); navigate(sshCenterPath); }}>
                {t("pty.sshReconnect")}
              </button>
            ) : (
              <>
                {resumeAgent && (
                  <button
                    className="pty-key-btn pty-key-accent"
                    onClick={handleResumeAgent}
                    disabled={restarting || !resumeCommand}
                    title={resumeBlockedReason || undefined}
                  >
                    {restarting ? t("pty.restarting") : t("pty.resumeAgent", {
                      name: resumeAgent.agent.name || agentDisplayName(resumeAgent.kind),
                    })}
                  </button>
                )}
                <button className="pty-key-btn" onClick={handleRestartHere} disabled={restarting}>
                  {restarting ? t("pty.restarting") : t("pty.restartHere")}
                </button>
              </>
            )}
          </div>
        </div>
      )}

      {/* Вопрос агента — над строкой ввода, чтобы было видно, ЧТО он спросил:
          раньше hint жил только в списке терминалов, и с главной по кнопке
          «Ответить» приходили на экран без вопроса. */}
      {state.alive && agentAsking && (
        <div className="pty-agent-ask">
          <div className="pty-agent-ask-head">
            <span className="pty-agent-ask-icon" aria-hidden>{"⏳"}</span>
            <span className="pty-agent-ask-text">{state.hint}</span>
            {state.status_at ? (
              <span className="pty-agent-ask-since">
                {t("pty.waitingSince", { value: sinceValue(state.status_at) })}
              </span>
            ) : null}
          </div>
          {/* Кнопки ответа живут ЗДЕСЬ, а не только в первом ряду клавиш: ряд
              сворачивается кнопкой ⌨ один раз и глобально для всех терминалов,
              и человек, пришедший по уведомлению «агент ждёт ответа», видел
              вопрос вообще без действий. Байты те же, что у ряда клавиш. */}
          {askActions && (
          <div className="pty-agent-ask-actions" role="group" aria-label={t("pty.answerActions")}>
            {answerKind === "choice" ? (
              // Подпись пункта — на кнопке: «2» само по себе не говорит, что это
              // «да, и больше не спрашивать», а меню на телефоне обычно уже
              // уехало под клавиатуру.
              <>
                {choiceDigits.map(({ digit, label }) => (
                  <button
                    key={digit}
                    className="pty-key-btn pty-key-accent pty-answer-option"
                    onClick={() => sendAnswer(digit, label ? `${digit} · ${label}` : digit)}
                    title={label || digit}
                  >
                    <span className="pty-answer-digit">{digit}</span>
                    {label && <span className="pty-answer-label">{label}</span>}
                  </button>
                ))}
              </>
            ) : answerKind === "yes_no" ? (
              <>
                <button className="pty-key-btn pty-key-accent" onClick={() => sendAnswer("y\r", t("pty.answerYes"))}>
                  {t("pty.answerYes")}
                </button>
                <button className="pty-key-btn" onClick={() => sendAnswer("n\r", t("pty.answerNo"))}>
                  {t("pty.answerNo")}
                </button>
              </>
            ) : answerKind === "enter" ? (
              <button className="pty-key-btn pty-key-accent" onClick={() => sendAnswer("\r", t("pty.enterKey"))}>
                {"↵ "}{t("pty.enterKey")}
              </button>
            ) : null}
            {/* Esc у агента прерывает работу — отдельно, в конце и красным (тот
                же приём, что у Ctrl+C в ряду клавиш). Только при свёрнутой
                панели клавиш: иначе такой же Esc стоит прямо под плашкой. */}
            {keysCollapsedForView && (
              <button
                className="pty-key-btn pty-key-danger pty-key-gap-left"
                onClick={() => { haptic(); sendRaw("\x1b"); }}
                title={t("pty.answerCancel")}
                aria-label={t("pty.answerCancel")}
              >
                Esc
              </button>
            )}
          </div>
          )}
        </div>
      )}

      {/* Полоса прогресса загрузки — вне зависимости от жизни процесса и связи:
          загрузка идёт по REST, переживает обрыв моста (см. ws.onclose), и
          единственная кнопка «Отменить» не должна пропадать вместе с сокетом. */}
      {uploading && (
        <div className="pty-upload-strip" role="status" aria-live="polite">
          <span>{t("pty.uploadProgress", { n: uploadProgress })}</span>
          <div className="pty-upload-meter" aria-hidden>
            <span style={{ width: `${uploadProgress}%` }} />
          </div>
          <button onClick={() => uploadAbortRef.current?.abort()}>{t("modal.cancel")}</button>
        </div>
      )}
      {/* IME-safe text input (Gboard friendly) */}
      {["shell", "starting", "unconfirmed"].includes(entryMode) && (
        <div className="pty-agent-entry" role="status" aria-live="polite">
          <div className="pty-agent-entry-copy">
            <span>{entryMode === "shell" ? t("pty.agentEntryShell")
              : t(entryMode === "starting" ? "pty.agentEntryStarting" : "pty.agentEntryUnconfirmed", { name: launchAttempt?.name || "" })}</span>
            {entryMode !== "shell" && <small>{t(entryMode === "starting" ? "pty.agentEntryWait" : "pty.agentEntryCheck")}</small>}
          </div>
          {entryMode !== "starting" && state.agent_kind === "shell" && (
            <button className="btn btn-secondary btn-sm" onClick={() => { haptic(); setAgentSheetOpen(true); }}>
              {t(entryMode === "unconfirmed" ? "pty.agentEntryRetry" : "pty.agentEntryStart")}
            </button>
          )}
        </div>
      )}
      {state.alive && (
      <div className="pty-input-bar">
        <button
          className="pty-key-btn pty-key-accent pty-attach-btn"
          onClick={handleAttachClick}
          disabled={uploading}
          title={isSsh ? t("pty.sshFiles") : t("pty.a11y.attach")}
          aria-label={isSsh ? t("pty.sshFiles") : t("pty.a11y.attach")}
        >
          {uploading ? "..." : <span aria-hidden>{"\uD83D\uDCCE"}</span>}
        </button>
        <textarea
          ref={textInputRef}
          className="pty-text-input"
          value={inputText}
          rows={inputRows}
          onChange={(e) => setInputText(e.target.value)}
          onKeyDown={handleInputKeyDown}
          onCompositionStart={() => setComposing(true)}
          onCompositionEnd={() => setComposing(false)}
          placeholder={inputTargetLabel}
          /* Подсказка внутри поля — не имя поля: она исчезает с первым знаком,
             а на узком телефоне ещё и обрезается. Скринридер до этой подписи
             читал «текстовое поле» (аудит путей 29.08.2026). */
          aria-label={inputTargetLabel}
          autoComplete="off"
          autoCorrect="on"
        />
        <button
          className="pty-key-btn pty-compose-expand"
          onClick={() => { haptic(); setComposerOpen(true); }}
          title={t("pty.expandComposer")}
          aria-label={t("pty.expandComposer")}
        >
          <span aria-hidden>{"⛶"}</span>
        </button>
        {/* \u041F\u0443\u0441\u0442\u043E\u0435 \u043F\u043E\u043B\u0435 + \u21B5 = \u0433\u043E\u043B\u044B\u0439 Enter: \u0438\u043C\u0435\u043D\u043D\u043E \u0442\u0430\u043A \u043F\u043E\u0434\u0442\u0432\u0435\u0440\u0436\u0434\u0430\u0435\u0442\u0441\u044F \u0432\u044B\u0431\u0440\u0430\u043D\u043D\u044B\u0439
            \u043F\u0443\u043D\u043A\u0442 \u043C\u0435\u043D\u044E \u0443 Claude Code. \u0420\u0430\u043D\u044C\u0448\u0435 \u043A\u043D\u043E\u043F\u043A\u0430 \u0433\u0430\u0441\u043B\u0430, \u0438 Enter \u043C\u043E\u0436\u043D\u043E \u0431\u044B\u043B\u043E
            \u043F\u043E\u0441\u043B\u0430\u0442\u044C \u0442\u043E\u043B\u044C\u043A\u043E \u0441 \u0441\u0438\u0441\u0442\u0435\u043C\u043D\u043E\u0439 \u043A\u043B\u0430\u0432\u0438\u0430\u0442\u0443\u0440\u044B. */}
        <button
          className="pty-key-btn pty-key-accent pty-send-btn"
          onClick={() => handleInputSend()}
          disabled={composing}
          title={t("pty.a11y.sendEnter")}
          aria-label={t("pty.a11y.sendEnter")}
        >
          <span aria-hidden>{"\u21B5"}</span>
        </button>
        <button
          className={`pty-key-btn pty-keys-toggle${keysCollapsedForView ? "" : " pty-key-active"}`}
          onClick={toggleKeys}
          title={t("pty.keypadToggle")}
          aria-label={t("pty.keypadToggle")}
          aria-pressed={!keysCollapsedForView}
          aria-expanded={!keysCollapsedForView}
        >
          <IconKeyboard size={22} />
        </button>
        <input
          ref={fileInputRef}
          type="file"
          multiple
          style={{ display: "none" }}
          onChange={(e) => handleFileUpload(e.target.files)}
        />
      </div>
      )}

      {/* Привязка сервера — прямо здесь.
          `remotai pair` печатает код и предлагает взять телефон, хотя команду
          запускают из этого же терминала, где человек уже вошёл в аккаунт.
          Кнопка подтверждает привязку тем же аккаунтом, без телефона и QR. */}
      {pairCode && (
        <div className="pty-pair-offer" role="status">
          <span className="pty-pair-offer-text">
            {t("pty.pairOffer", { code: pairCode })}
          </span>
          <button
            className="btn btn-primary btn-sm"
            disabled={pairing}
            onClick={() => void confirmPair()}
          >
            {pairing ? t("pty.pairing") : t("pty.pairConfirm")}
          </button>
        </div>
      )}

      {/* Special keys bar (mobile) — два фиксированных ряда без гориз. скролла.
          Ряд 1: клавиши терминала (стрелки, Ctrl+C, клавиши режима).
          Ряд 2: инструменты (выделение, буфер, файлы, поиск, сниппеты).
          Сворачивается кнопкой ⌨ в строке ввода — терминал на весь экран.
          На мёртвой сессии панель рендерим принудительно, игнорируя
          keysCollapsed: кнопка ⌨ живёт в строке ввода, которой уже нет, и
          свёрнутый ряд инструментов стал бы недостижим. */}
      {(!state.alive || !keysCollapsedForView) && (
      <>
      <div className="pty-keys-bar">
        {/* Цифры пунктов меню — ПЕРВОЙ строкой панели, всегда на виду.
            Раньше здесь стояла кнопка «123», открывавшая отдельную клавиатуру
            4×5 (0–9, Tab, Пробел, Backspace) ВМЕСТО этой панели: чтобы выбрать
            пункт меню Claude Code, приходилось переключить панель, нажать цифру
            и переключиться назад — а стрелки и Esc в этот момент исчезали.
            Просьба владельца 08.09 дословно: «убрать отдельную клавиатуру,
            оставить эту панель, но сделать её прямо клавиатурой вайб-кодера».
            Шесть цифр, а не десять: меню агентов длиннее шести пунктов не
            бывает, а 7–9 и 0 съедали ширину, из-за которой ряд уезжал в третью
            строку. Цифра уходит БЕЗ Enter — меню Claude/Codex срабатывает
            сразу.

            ⚠ Скрываем ряд, когда включена настройка «Кнопки ответа на вопросы
            агента» (`detect_agent_questions`) И она распознала нумерованное
            меню: там же, в ряду клавиш, встают цифры этого меню С ПОДПИСЯМИ
            пунктов. Две группы цифр рядом — это выбор без разницы: подписанная
            строго лучше, «2» само по себе не говорит, что это «да, и больше не
            спрашивать». */}
        {state.alive && answerKind !== "choice" && (
          <div className="pty-keys-row pty-digits-row" role="group" aria-label={t("pty.digitsRow")}>
            {["1", "2", "3", "4", "5", "6"].map(digit => (
              <button
                key={digit}
                className="pty-key-btn pty-key-accent pty-digit-key"
                data-key={digit}
                disabled={!connected}
                onPointerDown={event => event.preventDefault()}
                onClick={() => { haptic(); sendRaw(digit); }}
                title={digit}
              >{digit}</button>
            ))}
          </div>
        )}
        {state.alive && (
        <div className="pty-keys-row">
          {/* \u0426\u0438\u0444\u0440\u044b \u0442\u0435\u043f\u0435\u0440\u044c \u0441\u0442\u0440\u043e\u043a\u043e\u0439 \u0432\u044b\u0448\u0435 \u0438 \u0432\u0441\u0435\u0433\u0434\u0430 \u043d\u0430 \u0432\u0438\u0434\u0443, \u043f\u043e\u044d\u0442\u043e\u043c\u0443 \u00ab\u043c\u0435\u0441\u0442\u0430 \u0434\u043b\u044f
              1/2/3 \u043d\u0435 \u0445\u0432\u0430\u0442\u0430\u0435\u0442\u00bb \u0431\u043e\u043b\u044c\u0448\u0435 \u043d\u0435 \u043f\u0440\u043e \u044d\u0442\u043e\u0442 \u0440\u044f\u0434. \u0412\u043b\u0435\u0432\u043e/\u0432\u043f\u0440\u0430\u0432\u043e \u0432 \u043c\u0435\u043d\u044e
              \u0430\u0433\u0435\u043d\u0442\u0430 \u043d\u0435 \u043d\u0443\u0436\u043d\u044b \u2014 \u0443\u0441\u043b\u043e\u0432\u0438\u0435 \u043e\u0441\u0442\u0430\u0432\u043b\u0435\u043d\u043e \u043a\u0430\u043a \u0431\u044b\u043b\u043e. */}
          {answerKind !== "choice" && (
            <button className="pty-key-btn pty-key-accent" onClick={() => { haptic(); sendRaw("\x1b[D"); }} title={t("pty.a11y.cursorLeft")} aria-label={t("pty.a11y.cursorLeft")}><span aria-hidden>{"\u2190"}</span></button>
          )}
          <button className="pty-key-btn pty-key-accent" onClick={() => { haptic(); sendRaw("\x1b[A"); }} title={t("pty.a11y.cursorUp")} aria-label={t("pty.a11y.cursorUp")}><span aria-hidden>{"\u2191"}</span></button>
          <button className="pty-key-btn pty-key-accent" onClick={() => { haptic(); sendRaw("\x1b[B"); }} title={t("pty.a11y.cursorDown")} aria-label={t("pty.a11y.cursorDown")}><span aria-hidden>{"\u2193"}</span></button>
          {answerKind !== "choice" && (
            <button className="pty-key-btn pty-key-accent" onClick={() => { haptic(); sendRaw("\x1b[C"); }} title={t("pty.a11y.cursorRight")} aria-label={t("pty.a11y.cursorRight")}><span aria-hidden>{"\u2192"}</span></button>
          )}
          {/* \u0426\u0438\u0444\u0440\u044b \u043e\u0442\u0432\u0435\u0442\u0430 \u2014 \u043f\u043e \u043a\u043d\u043e\u043f\u043a\u0435, \u0430 \u043d\u0435 \u043f\u043e \u0434\u043e\u0433\u0430\u0434\u043a\u0435. \u0420\u0430\u043d\u044c\u0448\u0435 \u00ab1 2 3\u00bb \u0432\u0441\u0442\u0430\u0432\u0430\u043b\u0438
              \u0432 \u0440\u044f\u0434 \u0441\u0430\u043c\u0438, \u043a\u043e\u0433\u0434\u0430 \u043a\u043e\u043c\u043f\u044c\u044e\u0442\u0435\u0440 \u0420\u0415\u0428\u0410\u041b, \u0447\u0442\u043e \u0430\u0433\u0435\u043d\u0442 \u0437\u0430\u0434\u0430\u043b \u0432\u043e\u043f\u0440\u043e\u0441; \u0440\u0435\u0448\u0435\u043d\u0438\u0435
              \u044d\u0442\u043e \u0431\u044b\u0432\u0430\u043b\u043e \u043e\u0448\u0438\u0431\u043e\u0447\u043d\u044b\u043c, \u0438 \u0440\u0430\u0441\u043f\u043e\u0437\u043d\u0430\u0432\u0430\u043d\u0438\u0435 \u0432\u044b\u043a\u043b\u044e\u0447\u0435\u043d\u043e (2.49.4). \u0422\u0435\u043f\u0435\u0440\u044c
              \u043f\u0430\u043d\u0435\u043b\u044c \u043e\u0442\u043a\u0440\u044b\u0432\u0430\u0435\u0442 \u0447\u0435\u043b\u043e\u0432\u0435\u043a: \u043e\u043d \u0438 \u0442\u0430\u043a \u0432\u0438\u0434\u0438\u0442 \u043d\u0430 \u044d\u043a\u0440\u0430\u043d\u0435, \u0435\u0441\u0442\u044c \u043c\u0435\u043d\u044e \u0438\u043b\u0438
              \u043d\u0435\u0442. \u041a\u043d\u043e\u043f\u043a\u0430 \u0441\u0442\u043e\u0438\u0442 \u043c\u0435\u0436\u0434\u0443 \u0441\u0442\u0440\u0435\u043b\u043a\u0430\u043c\u0438 \u0438 Ctrl+C \u2014 \u043f\u043e \u043f\u0440\u043e\u0441\u044c\u0431\u0435 \u0432\u043b\u0430\u0434\u0435\u043b\u044c\u0446\u0430
              \u0438 \u043f\u043e\u0442\u043e\u043c\u0443, \u0447\u0442\u043e \u044d\u0442\u043e \u0435\u0434\u0438\u043d\u0441\u0442\u0432\u0435\u043d\u043d\u043e\u0435 \u043c\u0435\u0441\u0442\u043e, \u0433\u0434\u0435 \u043e\u043d\u0430 \u043d\u0435 \u0442\u043e\u043b\u043a\u0430\u0435\u0442
              \u0434\u0435\u0441\u0442\u0440\u0443\u043a\u0442\u0438\u0432\u043d\u0443\u044e \u043a\u043d\u043e\u043f\u043a\u0443 \u043f\u043e\u0434 \u043f\u0430\u043b\u0435\u0446. */}
          {/* Ctrl+C \u2014 \u0434\u0435\u0441\u0442\u0440\u0443\u043a\u0442\u0438\u0432\u043d\u0430\u044f \u043a\u043d\u043e\u043f\u043a\u0430 \u0432\u043f\u043b\u043e\u0442\u043d\u0443\u044e \u043a \u0441\u0442\u0440\u0435\u043b\u043a\u0430\u043c: \u043c\u0438\u0441-\u0442\u0430\u043f \u0443\u0431\u0438\u0432\u0430\u043b
              \u0440\u0430\u0431\u043e\u0442\u0443 \u0430\u0433\u0435\u043d\u0442\u0430. \u041a\u043b\u0430\u0441\u0441 \u0434\u0430\u0451\u0442 \u0435\u0439 \u043e\u0442\u0441\u0442\u0443\u043f \u0441\u043b\u0435\u0432\u0430 (\u0441\u043c. styles.css). */}
          <button
            className="pty-key-btn pty-key-danger pty-key-gap-left"
            onClick={() => { haptic(); sendRaw("\x03"); }}
            title={t("pty.a11y.interrupt")}
            aria-label={t("pty.a11y.interrupt")}
          >Ctrl+C</button>

          {/* Голый Enter нужен всегда: разрешения Claude Code — это выбор
              пункта меню, а не «y», и до сих пор Enter можно было отправить
              только подняв системную клавиатуру (кнопка ↵ гасла на пустом
              поле). */}
          <button
            className="pty-key-btn pty-key-accent"
            onClick={() => { haptic(); sendRaw("\r"); }}
            title={t("pty.enterKey")}
            aria-label={t("pty.enterKey")}
          >
            <span aria-hidden>{"↵"}</span>
          </button>

          {/* У агента Esc ПРЕРЫВАЕТ работу — как Ctrl+C, только без красной
              подписи: мис-тап с соседней «/» или Tab стоил четырёх минут правок.
              Поэтому в агентских ветках он последний в ряду, красный и с
              отступом (класс pty-key-gap-left, см. styles.css). Не-агентные
              ветки ниже не трогаем: там Esc безобиден. */}
          {isAgentKind(state.agent_kind) ? (
            answerKind === "choice" ? (
              // Нумерованное меню: отвечаем цифрой, y/n тут не работает.
              <>
                {choiceDigits.map(({ digit, label }) => (
                  <button
                    key={digit}
                    className="pty-key-btn pty-key-accent"
                    onClick={() => sendAnswer(digit, label ? `${digit} · ${label}` : digit)}
                    title={label || digit}
                  >{digit}</button>
                ))}
                <button
                  className="pty-key-btn pty-key-danger pty-key-gap-left"
                  onClick={() => { haptic(); sendRaw("\x1b"); }}
                  title={t("pty.answerCancel")}
                >Esc</button>
              </>
            ) : answerKind === "yes_no" ? (
              <>
                <button className="pty-key-btn pty-key-accent" onClick={() => sendAnswer("y\r", t("pty.answerYes"))}>Y</button>
                <button className="pty-key-btn pty-key-accent" onClick={() => sendAnswer("n\r", t("pty.answerNo"))}>N</button>
                <button className="pty-key-btn" onClick={() => { haptic(); sendRaw("/"); }}>/</button>
                <button className="pty-key-btn" onClick={() => { haptic(); sendRaw("\t"); }}>Tab</button>
                <button
                  className="pty-key-btn pty-key-danger pty-key-gap-left"
                  onClick={() => { haptic(); sendRaw("\x1b"); }}
                  title={t("pty.answerCancel")}
                >Esc</button>
              </>
            ) : (
              // Агент в терминале ЕСТЬ, но ни о чём не спрашивает: Y и N здесь —
              // ловушка. Тап по ним отправляет в чат агента литерал «y» и Enter,
              // то есть сбивает работающую задачу и жжёт токены (ровно тот риск,
              // ради которого шелл-команды спрашивают подтверждение). Пока
              // вопроса нет, держим на их месте безобидные клавиши самого агента:
              // «/» открывает его меню команд, Tab дополняет путь.
              <>
                <button className="pty-key-btn" onClick={() => { haptic(); sendRaw("/"); }}>/</button>
                <button className="pty-key-btn" onClick={() => { haptic(); sendRaw("\t"); }}>Tab</button>
                <button
                  className="pty-key-btn pty-key-danger pty-key-gap-left"
                  onClick={() => { haptic(); sendRaw("\x1b"); }}
                  title={t("pty.answerCancel")}
                >Esc</button>
              </>
            )
          ) : state.agent_kind === "git" ? (
            <>
              <button className="pty-key-btn" onClick={() => { haptic(); sendRaw("q"); }}>q</button>
              <button className="pty-key-btn" onClick={() => { haptic(); sendRaw(":q\r"); }}>:q</button>
              <button className="pty-key-btn" onClick={() => { haptic(); sendRaw("\x04"); }}>Ctrl+D</button>
              <button className="pty-key-btn" onClick={() => { haptic(); sendRaw("\x1b"); }}>Esc</button>
            </>
          ) : (
            <>
              <button className="pty-key-btn" onClick={() => { haptic(); sendRaw("\x04"); }}>Ctrl+D</button>
              <button className="pty-key-btn" onClick={() => { haptic(); sendRaw("\x1b"); }}>Esc</button>
              <button className="pty-key-btn" onClick={() => { haptic(); sendRaw("\t"); }}>Tab</button>
              <button className="pty-key-btn" onClick={() => { haptic(); sendRaw("\x1a"); }}>Ctrl+Z</button>
            </>
          )}
          {/* Дверь ко второму ряду прибита к правому краю (pty-key-more): за ней
              живут выделение, копирование, ⚡ Команды и 🤖 Агент — половина
              работы с терминалом. Ряд клавиш при работающем агенте длиннее
              экрана (замер: 509px против 344 на телефоне 360px), и кнопка
              уезжала за край вместе с Tab и Esc. Человек листать не догадывался
              и считал, что кнопки в приложении больше нет. */}
          <button
            className={`pty-key-btn pty-key-more${toolsOpen ? " pty-key-active" : ""}`}
            onClick={() => { haptic(); setToolsOpen((open) => !open); }}
            aria-expanded={toolsOpen}
            aria-label={t("pty.moreTools")}
          >
            {"⋮ "}{t("pty.moreTools")}
          </button>
        </div>
        )}

        {/* Ряд инструментов живёт и на мёртвой сессии: выделение, копирование,
            поиск и экспорт работают с уже полученным выводом, а «Скачать» и ⤓
            ходят по REST к ПК. Прячем только то, что пишет в мёртвый сокет
            (Вставить, ⚡, 🤖) — иначе кнопка давала бы лишь тост «нет связи». */}
        {(!state.alive || toolsOpen) && (
        <div className="pty-keys-row pty-tools-row">
          {/* Вторая дверь наружу. Экранная «←» живёт в шапке, в самом левом
              верхнем углу, — и ровно туда Telegram на планшете кладёт свою
              плавающую «✕ Закрыть» (живая жалоба 04.08: «в мини-аппе на iPad всё
              равно не могу нажать назад»). Геометрию мы чиним отдельно, но
              выход из терминала не должен зависеть от того, угадали мы чужую
              раскладку или нет: здесь он в ряду инструментов, куда никакие
              кнопки клиента не дотягиваются. */}
          <button
            className="pty-key-btn pty-key-accent"
            onClick={() => { haptic(); navigate(backPath); }}
            title={t("pty.a11y.back")}
          >
            <span aria-hidden>{"←"}</span> {t("pty.backToList")}
          </button>
          <button
            className="pty-key-btn"
            onClick={() => changeFontSize(-1)}
            disabled={fontSize === FONT_SIZES[0]}
            aria-label={t("pty.fontSmaller")}
          >
            A−
          </button>
          {/* Ряд инструментов — самое дефицитное место на телефоне: на 320px он
              прячет за краем больше 400px кнопок. «45×38» (колонки×строки) —
              внутренняя величина, нужная при разборе полётов, а не в работе,
              поэтому на узком экране её нет вовсе (класс скрыт медиазапросом),
              а размер шрифта рядом с A−/A+ остаётся: это обратная связь кнопок. */}
          <span className="pty-term-size" role="status" title={`${termSize.cols}×${termSize.rows}`}>
            {fontSize}px
            <span className="pty-term-dims"> · {termSize.cols}×{termSize.rows}</span>
          </span>
          <button
            className="pty-key-btn"
            onClick={() => changeFontSize(1)}
            disabled={fontSize === FONT_SIZES[FONT_SIZES.length - 1]}
            aria-label={t("pty.fontLarger")}
          >
            A+
          </button>
          <button
            className={`pty-key-btn pty-key-accent${selectMode ? " pty-key-active" : ""}`}
            onClick={toggleSelectMode}
          >
            {selectMode ? t("pty.selectDone") : t("pty.select")}
          </button>
          {!readView && <button className="pty-key-btn" onClick={handleSelectAll}>{t("pty.selectAll")}</button>}
          {!readView && <button className="pty-key-btn pty-key-accent" disabled={selectMode} onClick={() => void handleCopy()}>{t("pty.copy")}</button>}
          <button className="pty-key-btn" onClick={() => void handleCopy("screen")}>{t("pty.copyScreen")}</button>
          {/* ST-10 (T-39): команды как блоки — только при разметке оболочки
              (OSC 133) и флаге commandBlocks. Без интеграции ряда нет. */}
          {features.commandBlocks && blockUi.copyCommand && (
            <button className="pty-key-btn" onClick={() => copyBlock("command")}>{t("pty.blockCopyCommand")}</button>
          )}
          {features.commandBlocks && blockUi.copyOutput && (
            <button className="pty-key-btn" onClick={() => copyBlock("output")}>{t("pty.blockCopyOutput")}</button>
          )}
          {/* «Листает программа» — действие недоступно (aria-disabled), но
              нажимается: на телефоне title не виден, и касание показывает
              объяснение, а jumpToCommand решает заново и не шлёт ни байта. */}
          {features.commandBlocks && blockUi.jump !== "none" && (
            <button className={`pty-key-btn${blockUi.jump === "app" ? " pty-key-unavailable" : ""}`}
              aria-disabled={blockUi.jump === "app" ? true : undefined} onClick={jumpToCommand}
              title={blockUi.jump === "app" ? t("pty.blockJumpAppHint") : undefined}>
              {blockUi.jump === "app" ? t("pty.blockJumpApp") : t("pty.blockJump")}
            </button>
          )}
          {features.commandBlocks && blockUi.notice === "restored" && (
            <span className="pty-block-note" role="status" title={t("pty.blocksLostHint")}>{t("pty.blocksLost")}</span>
          )}
          {features.agentHistory && historyAvailable && <button className="pty-key-btn" onClick={() => {
            cancelGestureRef.current(); closeReading(); terminalRef.current?.blur(); setHistoryOpen(true);
          }}>{t("pty.readAgent")}</button>}
          {sizeControls && (sizeControls.viewers.length > 1 || sizeControls.owner) &&
            <button className="pty-key-btn" onClick={() => setSizeControlsOpen(true)}>{t("pty.sizeControl")}</button>}
          {state.alive && agentHistoryChannel(state.agent_kind)?.key && (
            <button className="pty-key-btn" onClick={() => {
              const own = agentHistoryChannel(agentKindRef.current);
              if (own?.key) { cancelGestureRef.current(); sendRaw(own.key); }
            }}>{t("pty.historyOwnOpen")}</button>
          )}
          {state.alive && (
            <button className="pty-key-btn pty-key-accent" onClick={handlePaste}>{t("pty.paste")}</button>
          )}
          {/* Усыпить агента (просьба владельца 29.09): процесс снимается вместе
              с MCP, терминал и беседа остаются. Кнопка видна только у агента,
              которого реестр умеет поднять по номеру беседы (Claude, Codex). */}
          {state.alive && !isSsh && !state.sleep && sleepAgent && sleepAgent.kind === state.agent_kind && (
            <button
              className={`pty-key-btn${sleepBlockedByStatus(state.status) ? " pty-key-unavailable" : ""}`}
              aria-disabled={sleepBlockedByStatus(state.status) ? true : undefined}
              disabled={sleepBusy}
              onClick={() => void handleSleepAgent()}
              title={t("pty.sleepHint")}
            >
              {sleepBusy ? t("pty.sleeping") : t("pty.sleepAgent")}
            </button>
          )}
          {/* В SSH-сессии «Скачать» открывало обзор файлов ПК-бастиона по пути
              «ssh:user@host» (cwd SSH-сессии — это ярлык соединения) и сразу
              падало ошибкой листинга. Теперь та же развилка, что у скрепки:
              файлы сервера — в SFTP-обзоре /ssh-files. */}
          {/* Аудит ИА 02.09.2026, P1-26: в SSH кнопка вела в файлы сервера, а
              называлась «Скачать» — имя обещало одно, дверь открывала другое.
              Подпись теперь называет то, что за ней: «Файлы сервера». */}
          <button
            className="pty-key-btn pty-key-accent"
            onClick={() => { haptic(); if (isSsh) void openSshFiles(); else setDownloadOpen(true); }}
            title={isSsh ? t("pty.sshFiles") : t("download.title")}
          >
            <span aria-hidden>{isSsh ? "📁" : "⬇️"}</span> {isSsh ? t("pty.sshFilesShort") : t("pty.download")}
          </button>
          {/* Аудит ИА 02.09.2026, P1-30: из терминала в «Файлы» той папки, где
              работает агент, пути не было — только через нижнее меню и ручной
              поиск папки. Параметр `cwd` — тот же, что у диплинка списка
              терминалов; его чтение на стороне /files. В SSH не показываем:
              файлы сервера — в /ssh-files (кнопка выше). */}
          {!isSsh && !!cwd && (
            <button
              className="pty-key-btn"
              onClick={() => { haptic(); navigate(`/files?cwd=${encodeURIComponent(cwd)}`); }}
              title={t("pty.openFilesHere")}
            >
              <span aria-hidden>{"📁"}</span> {t("pty.openFilesHere")}
            </button>
          )}
          <button className="pty-key-btn" onClick={() => { haptic(); setSearchOpen((v) => !v); }} title={t("pty.search")} aria-label={t("pty.search")}>
            <span aria-hidden>{"🔍"}</span>
          </button>
          <button className="pty-key-btn" onClick={() => { haptic(); setExportOpen(true); }} title={t("pty.export")} aria-label={t("pty.export")}>
            <span aria-hidden>{"⤓"}</span>
          </button>
          {state.alive && (
            <button
              className="pty-key-btn pty-key-accent"
              onClick={() => { haptic(); setSnippetsOpen(true); }}
              title={t("pty.snippets")}
              aria-label={t("pty.snippets")}
            >
              <span aria-hidden>{"⚡"}</span> {t("ui.ptytermview.mcdf80e6c15")}</button>
          )}
          {/* Отдельно от сниппетов: выбрать AI-агента и поднять его здесь же. */}
          {state.alive && (
            <button
              className="pty-key-btn pty-key-accent"
              onClick={() => { haptic(); setAgentSheetOpen(true); }}
              // Аудит ИА 02.09.2026, P1-17: одно имя у всех дверей к запуску агента.
              title={t("pty.agentButtonTitle")}
              aria-label={t("pty.agentButtonTitle")}
            >
              <span aria-hidden>{"🤖"}</span> {t("pty.agentBtn")}
            </button>
          )}
          {/* Handoff сюда из шапки (2026-07-30, просьба владельца): функция
              редкая, а место в шапке дорогое. На сервере без дисплея открыть
              окно терминала невозможно: handoff отвечает «no supported
              terminal emulator found», поэтому hasDisplay. В SSH не показываем
              (та же развилка, что была в шапке). */}
          {state.alive && hasDisplay && !isSsh && (
            <button
              className="pty-key-btn"
              onClick={() => { void handleHandoff(); }}
              title={t("pty.openOnPC")}
              aria-label={t("pty.openOnPC")}
            >
              <span aria-hidden>{"🖥️"}</span> {t("pty.openOnPC")}
            </button>
          )}
        </div>
        )}
      </div>

      {/* Quick commands row — через sendQuickCommand: это и команды шелла, и
          текстовые заготовки для агента, поэтому без вопроса «уйдёт в чат»
          (см. sendQuickCommand). Приглушаем при работающем агенте, чтобы было
          видно: сейчас ввод идёт не в шелл. Свёрнутый ряд — одна
          строка-кнопка: высота ряда это минус ~40 px видимого лога, а нужен
          он не в каждую секунду. */}
      {/* При поднятой клавиатуре ряд быстрых команд скрыт — но НЕ отсюда и не
          состоянием React: класс `pty-kbd` на странице ставит updateKeyboardMode
          синхронно, правило в styles.css. Ряд клавиш и открытая человеком
          лента инструментов остаются.
          Попытка 2.61.0 прятать их через `setKeyboardShown` роняла четыре
          пробы геометрии (см. комментарий у updateKeyboardMode). Замер —
          probe-keyboard-rows-live в qa:terminal. */}
      {state.alive && (
      quickCmdsCollapsed ? (
        <button className="pty-quick-cmds-bar" onClick={toggleQuickCmds} aria-expanded={false}>
          <span className="pty-folder-chev" aria-hidden>{"▸"}</span>
          <span>{t("pty.quickCmdsBar")}</span>
          <span className="pty-tools-bar-count">{quickCommands.length}</span>
        </button>
      ) : (
      <div
        className={`pty-quick-cmds${agentInFg ? " pty-shell-muted" : ""}`}
        // Прокрутка ряда и движение пальца по нему — не нажатие. Слушаем на
        // КОНТЕЙНЕРЕ, а не на каждой кнопке: палец опускается на одну, а уезжает
        // над соседней, и события до неё уже не доходят.
        onPointerDown={(e) => {
          // Мышью промахнуться прокруткой нечем — правила движения только для пальца.
          if (e.pointerType === "mouse") return;
          tapGuardRef.current = noteDown(tapGuardRef.current, e.clientX, e.clientY);
        }}
        onPointerMove={(e) => {
          if (e.pointerType === "mouse") return;
          tapGuardRef.current = noteMove(tapGuardRef.current, e.clientX, e.clientY);
        }}
        onScroll={() => { tapGuardRef.current = noteScroll(tapGuardRef.current, Date.now()); }}
      >
        {/* «Свернуть» липкая у ЛЕВОГО края — как «+» у правого.
            Раньше она просто стояла первой в ряду и уезжала за край при
            прокрутке: чтобы свернуть ряд, надо было сперва пролистать его в
            начало. Живая жалоба владельца «неудобно сворачивать» — про это.
            Правило то же, что у «⋮ Ещё» в ряду клавиш: дверь всегда видна. */}
        <button
          className="pty-quick-cmd pty-quick-cmd-ctl pty-quick-cmd-collapse"
          onClick={quickTap(toggleQuickCmds)}
          aria-expanded={true}
          title={t("pty.quickCmdsCollapse")}
          aria-label={t("pty.quickCmdsCollapse")}
        >
          {/* Со СЛОВОМ, а не одним значком: на неё пожаловались дважды —
              «неудобно сворачивать» и «плохо видно». Голая стрелка среди
              команд не читается как действие, сколько её ни подсвечивай. */}
          <span aria-hidden>{"▾"}</span>
          <span className="pty-quick-cmd-ctl-text">{t("pty.quickCmdsHide")}</span>
        </button>
        {/* Отправляем `cmd`, показываем `label` — путать нельзя: на кнопке
            может стоять «Обращения», а уйти обязана вся заготовка целиком. */}
        {quickCommands.map((q) => (
          <button key={q.cmd} className="pty-quick-cmd" onClick={quickTap(() => { haptic(); sendQuickCommand(q.cmd); })}>
            {q.label}
          </button>
        ))}
        {/* Своя команда — через «+»: вписать можно любую, хранится на этом
            устройстве. Кнопка в конце ряда, а не в меню: иначе про неё не
            узнает никто. */}
        <button
          className="pty-quick-cmd pty-quick-cmd-ctl pty-quick-cmd-add"
          onClick={quickTap(() => { haptic(); setCmdSheetOpen(true); })}
          title={t("pty.quickCmdsAdd")}
          aria-label={t("pty.quickCmdsAdd")}
        >
          {"+"}
        </button>
      </div>
      )
      )}
      </>
      )}

      {/* Выбор под скрепкой. Причина, почему он выбор, а не две кнопки в
          строке ввода, — в комментарии у attachMenuOpen (замер места). */}
      {attachMenuOpen && (
        <div className="remote-sheet-backdrop" onClick={() => setAttachMenuOpen(false)}>
          <div
            className="remote-menu-sheet"
            role="dialog"
            aria-modal="true"
            aria-label={t("pty.attachMenuTitle")}
            onClick={(e) => e.stopPropagation()}
          >
            <div className="remote-quick-section">{t("pty.attachMenuTitle")}</div>
            <button
              className="remote-quick-row"
              onClick={() => { setAttachMenuOpen(false); fileInputRef.current?.click(); }}
            >
              <span className="remote-quick-row-icon" aria-hidden>{"📎"}</span>
              <span className="remote-quick-row-label">
                {t("pty.attachFile")}
                <small className="pty-attach-row-hint">{t("pty.attachFileHint")}</small>
              </span>
            </button>
            <button
              className="remote-quick-row"
              onClick={() => { setAttachMenuOpen(false); void handleScreenshotToAgent(); }}
            >
              <span className="remote-quick-row-icon" aria-hidden>{"📷"}</span>
              <span className="remote-quick-row-label">
                {t("pty.attachScreenshot")}
                <small className="pty-attach-row-hint">{t("pty.screenshotHint")}</small>
              </span>
            </button>
          </div>
        </div>
      )}

      {composerOpen && (
        <div className="modal-overlay pty-composer-overlay" onClick={() => setComposerOpen(false)}>
          <div className="modal-sheet pty-composer-sheet" onClick={(e) => e.stopPropagation()}>
            <div className="help-sheet-header">
              <strong>{t("pty.longPrompt")}</strong>
              <button className="folder-sheet-close" onClick={() => setComposerOpen(false)}>{"✕"}</button>
            </div>
            <textarea
              autoFocus
              className="pty-composer-textarea"
              value={inputText}
              onChange={(e) => setInputText(e.target.value)}
              onKeyDown={handleComposerKeyDown}
              onCompositionStart={() => setComposing(true)}
              onCompositionEnd={() => setComposing(false)}
              placeholder={inputTargetLabel}
              aria-label={inputTargetLabel}
            />
            <div className="pty-composer-hint">{t("pty.composerHintMulti")}</div>
            <div className="modal-actions">
              <button className="btn btn-secondary" onClick={() => setComposerOpen(false)}>{t("modal.cancel")}</button>
              <button className="btn btn-primary" disabled={composing} onClick={() => handleInputSend()}>
                {t("pty.sendPrompt")}
              </button>
            </div>
          </div>
        </div>
      )}

      <FolderNavSheet
        open={folderOpen}
        onClose={() => setFolderOpen(false)}
        onPick={handleFolderPick}
        currentCwd={cwd}
        title={t("folder.cdTitle")}
        pickLabel={t("folder.cdHere")}
        initialTab="browse"
      />

      {sizeControlsOpen && sizeControls && <SizeOwnerControls state={sizeControls} pending={sizeControlPending}
        onAction={changeSizeControl} onClose={() => setSizeControlsOpen(false)} />}
      {historyOpen && <AgentHistorySurface session={id || ""} read={async cursor => {
        const target = inputTargetRef.current();
        const page = await historyClientRef.current.read(wsRef.current, cursor);
        if (target.identity !== inputTargetRef.current().identity) throw new Error("history_source_changed");
        return page;
      }} onClose={() => { historyClientRef.current.reset(); setHistoryOpen(false); suppressTerminalTapUntilRef.current = Date.now() + 400; }} />}

      {/* Только для терминалов ПК: listFiles/downloadBlob ходят по файловой
          системе компьютера, а cwd SSH-сессии — ярлык «ssh:user@host». */}
      <DownloadSheet
        open={downloadOpen && !isSsh}
        onClose={() => setDownloadOpen(false)}
        initialPath={cwd || "."}
        fetchBlob={downloadBlob}
        botAvailable={botAvailable}
        sendTelegram={sendToTelegram}
      />

      {/* Шторка своих команд: добавить новую и почистить список. Удаление
          только здесь, не долгим тапом по кнопке ряда: рядом с «отправить в
          терминал» жест-ловушка — это промахи и случайные стирания. */}
      {cmdSheetOpen && (
        <div className="modal-overlay" onClick={closeCmdSheet}>
          <div className="modal-sheet" onClick={(e) => e.stopPropagation()}>
            <div className="help-sheet-header">
              <strong>{t("pty.quickCmdsAddTitle")}</strong>
              <button className="folder-sheet-close" onClick={closeCmdSheet} aria-label={t("modal.close")}>{"✕"}</button>
            </div>
            <div className="pty-cmd-add-row">
              <input
                autoFocus
                value={newCmd}
                onChange={(e) => setNewCmd(e.target.value)}
                onKeyDown={(e) => {
                  if (e.key !== "Enter") return;
                  e.preventDefault();
                  addCustomCmd();
                }}
                placeholder={t("pty.quickCmdsPlaceholder")}
                aria-label={t("pty.quickCmdsPlaceholder")}
              />
              <button className="btn btn-primary" disabled={!newCmd.trim()} onClick={addCustomCmd}>
                {t("generic.add")}
              </button>
            </div>
            {quickCommands.length > 0 && (
              <div className="pty-cmd-custom-list">
                {/* Весь ряд: и встроенные, и свои. Встроенная «удаляется» в
                    скрытые (вернуть — вписать заново), своя стирается насовсем. */}
                {quickCommands.map((q) => (
                  editCmd && q.own && editCmd.id === q.own.id ? (
                    <div key={q.cmd} className="pty-cmd-edit-item">
                      <input
                        autoFocus
                        value={editCmd.cmd}
                        onChange={(e) => setEditCmd({ ...editCmd, cmd: e.target.value })}
                        onKeyDown={(e) => { if (e.key === "Enter") { e.preventDefault(); saveEditCmd(); } }}
                        placeholder={t("pty.quickCmdsPlaceholder")}
                        aria-label={t("pty.quickCmdsPlaceholder")}
                      />
                      <input
                        value={editCmd.label}
                        onChange={(e) => setEditCmd({ ...editCmd, label: e.target.value })}
                        onKeyDown={(e) => { if (e.key === "Enter") { e.preventDefault(); saveEditCmd(); } }}
                        placeholder={t("pty.quickCmdLabelPlaceholder")}
                        aria-label={t("pty.quickCmdLabelPlaceholder")}
                      />
                      <div className="pty-cmd-edit-actions">
                        <button className="btn btn-secondary" onClick={() => setEditCmd(null)}>
                          {t("generic.cancel")}
                        </button>
                        <button className="btn btn-primary" disabled={!editCmd.cmd.trim()} onClick={saveEditCmd}>
                          {t("generic.save")}
                        </button>
                      </div>
                    </div>
                  ) : (
                    <div key={q.cmd} className="pty-cmd-custom-item">
                      {/* С подписью показываем ОБЕ строки: иначе в списке стоит
                          «Обращения», и крестиком легко снести не ту команду —
                          какая под названием, по одному названию не видно. */}
                      <span className="pty-cmd-custom-text">
                        {q.label}
                        {q.label !== q.cmd && <em className="pty-cmd-custom-sub">{q.cmd}</em>}
                      </span>
                      {/* Карандаш только у СВОИХ: встроенная `ls` платформенная,
                          править в ней нечего — «своя ls» заводится добавлением. */}
                      {q.own && (
                        <button
                          className="pty-cmd-custom-edit"
                          onClick={() => { haptic(); setEditCmd({ id: q.own!.id, cmd: q.own!.cmd, label: q.own!.label || "" }); }}
                          title={t("generic.edit")}
                          aria-label={`${t("generic.edit")}: ${q.label}`}
                        >
                          {"✎"}
                        </button>
                      )}
                      <button
                        className="pty-cmd-custom-del"
                        onClick={() => removeQuickCmd(q.cmd)}
                        title={t("generic.remove")}
                        aria-label={`${t("generic.remove")}: ${q.label}`}
                      >
                        {"✕"}
                      </button>
                    </div>
                  )
                ))}
              </div>
            )}
            <div className="pty-composer-hint">{t("pty.quickCmdsHint")}</div>
          </div>
        </div>
      )}

      <SnippetsSheet
        userCommands={userCmds}
        onAddCommand={(label, cmd) => {
          void upsertUserCommand({ cmd, label, pinned: false })
            .then(({ commands }) => setUserCmds(commands))
            .catch(() => showToast(t("pty.quickCmdSaveFailed")));
        }}
        onRemoveCommand={(cmdId) => {
          void removeUserCommand(cmdId)
            .then((commands) => setUserCmds(commands))
            .catch(() => showToast(t("pty.quickCmdSaveFailed")));
        }}
        onTogglePin={(cmdId, pinned) => {
          const own = userCmds.find((c) => c.id === cmdId);
          if (!own) return;
          void upsertUserCommand({ id: own.id, cmd: own.cmd, label: own.label, pinned })
            .then(({ commands }) => setUserCmds(commands))
            .catch(() => showToast(t("pty.quickCmdSaveFailed")));
        }}
        open={snippetsOpen}
        onClose={() => setSnippetsOpen(false)}
        onInsert={(cmd) => { setInputText((v) => v ? v + " " + cmd : cmd); }}
        onRun={(cmd) => { haptic(); void sendShellCommand(cmd); }}
        // SSH-сессия исполняет команды на удалённом POSIX-сервере. Платформа
        // управляемого ПК здесь лишь бастион: если он Windows, фильтровать по
        // нему нельзя — иначе пропадают Remotai/Xvfb и все Linux-сниппеты
        // (см. shellIsWindows — тем же правилом живёт ряд быстрых команд).
        platform={shellIsWindows ? "windows" : "linux"}
        agentInstalls={snippetAgents}
      />

      <AgentLaunchSheet
        open={agentSheetOpen}
        onClose={() => setAgentSheetOpen(false)}
        onLaunchRequested={(agent) => {
          if (id && !isAgentKind(state.agent_kind)) {
            setLaunchAttempt({ sessionId: id, name: agent.name, startedAt: Date.now() });
          }
        }}
        onRun={(cmd) => { void sendShellCommand(cmd); }}
        // Metadata меняется только после подтверждения и только если команда
        // действительно уходит в idle shell. Cancel/сообщение живому агенту
        // не могут переписать аккаунт последующего Resume.
        onRunAgent={sendAgentLaunchCommand}
        // Папка терминала: с ней шторка показывает аккаунт, закреплённый за
        // проектом, и умеет закреплять новый. Идентификатор сессии — чтобы
        // запомнить, каким аккаунтом запущен агент ИМЕННО ЗДЕСЬ.
        cwd={cwd}
        // Аккаунт из `?account=` (D4): «Войти» в разделе «Агенты» привёл сюда
        // ради конкретного аккаунта — «Запустить» уйдёт под ним.
        accountId={launchAccountId}
        remote={state.kind === "ssh" || state.remote === true}
        // Платформа терминала выбирает команду установки Node.js (winget против
        // apt): раньше шторка про ОС не знала вовсе и на Windows вести человека
        // было некуда — см. SnippetsSheet рядом, ему platform уже передают.
        platform={shellIsWindows ? "windows" : "linux"}
      />

      {exportOpen && id && (
        <div className="pty-export-modal" onClick={() => setExportOpen(false)}>
          <div className="pty-export-sheet" onClick={(e) => e.stopPropagation()}>
            {/* Крестик: единственным выходом был тап по фону, который на
                телефоне не читается как «закрыть» (аудит ИА 02.09.2026). */}
            <div style={{ display: "flex", alignItems: "center", justifyContent: "space-between", gap: 8 }}>
              <h3 style={{ margin: 0 }}>{featuresOpen ? t("pty.featuresTitle") : traceView ? t("pty.traceTitle") : t("pty.export")}</h3>
              <button className="folder-sheet-close" onClick={() => setExportOpen(false)} aria-label={t("modal.close")}>{"✕"}</button>
            </div>
            {featuresOpen ? (
              <TerminalFeaturesPanel active={features} onBack={() => setFeaturesOpen(false)} onToast={showToast} />
            ) : traceView ? (
              // Предпросмотр трассы: что именно уйдёт в файл, прежде чем он
              // появится (I-15). Вывод терминала — только если запись включали.
              <>
                <p className="pty-trace-summary">
                  {t("pty.traceSummary", { events: traceView.events, seconds: traceView.seconds, dropped: traceView.dropped })}{traceView.streamDropped > 0 ? ` ${t("pty.traceStreamEvicted", { seq: traceView.streamFirstSeq, n: traceView.streamDropped })}` : ""}
                </p>
                {traceView.content === "bytes" ? (
                  <p className="pty-trace-warn" role="alert">
                    {t("pty.traceBytesWarn", {
                      kb: (traceView.recordedBytes / 1024).toFixed(1), chunks: traceView.recordedChunks,
                    })}
                    {traceView.truncated ? ` ${t("pty.traceTruncated")}` : ""}
                  </p>
                ) : (
                  <p className="pty-trace-note">{t("pty.traceMetaOnly")}</p>
                )}
                <label className="pty-trace-capture">
                  <input
                    type="checkbox"
                    checked={traceView.recording}
                    disabled={traceBusy}
                    onChange={(e) => setTraceCapture(e.target.checked)}
                  />
                  <span>
                    {t("pty.traceCapture")}
                    {traceView.recording ? ` ${t("pty.traceCaptureUntil", { time: clockLabel(traceView.until) })}` : ""}
                  </span>
                </label>
                {traceTgConfirm ? (
                  <>
                    <p className="pty-trace-warn" role="alert">{t("pty.traceTgConfirm")}</p>
                    <button className="pty-export-option" disabled={traceBusy} onClick={() => void sendTraceToTelegram(true)}>
                      {"✈️ "}{t("pty.traceTgConfirmYes")}
                    </button>
                    <button className="pty-export-option" onClick={() => setTraceTgConfirm(false)}>
                      {t("modal.cancel")}
                    </button>
                  </>
                ) : (
                  <>
                    <button className="pty-export-option" disabled={traceBusy} onClick={() => void saveTrace()}>
                      {"💾 "}{t("pty.traceSave")}
                    </button>
                    {traceView.content === "bytes" && (
                      <button className="pty-export-option" disabled={traceBusy} onClick={() => void saveCast()}>
                        {"🎞 "}{t("pty.traceCast")}
                      </button>
                    )}
                    {botAvailable && (
                      <button className="pty-export-option" disabled={traceBusy} onClick={() => void sendTraceToTelegram()}>
                        {"✈️ "}{t("pty.traceSendTg")}
                      </button>
                    )}
                    <button className="pty-export-option" disabled={traceBusy} onClick={deleteTraceRecording}>
                      {"🗑 "}{t("pty.traceDelete")}
                    </button>
                  </>
                )}
                <button className="pty-export-option" onClick={() => { setTraceView(null); setTraceTgConfirm(false); }}>
                  {"← "}{t("pty.traceBack")}
                </button>
              </>
            ) : (
              <>
                {/* В Telegram нативного скачивания нет: blob-ссылка молча ничего не
                    сохраняет. Поэтому там доставка ботом стоит ПЕРВОЙ — это
                    единственный путь, который точно донесёт лог упавшего агента. */}
                {botAvailable && inTelegram && (
                  <button className="pty-export-option" disabled={exporting} onClick={() => void exportToTelegram("txt")}>
                    {"✈️ "}{t("pty.exportSendTg")}
                  </button>
                )}
                <button className="pty-export-option" disabled={exporting} onClick={() => void exportTerm("txt")}>
                  {t("pty.exportTxt")}
                </button>
                <button className="pty-export-option" disabled={exporting} onClick={() => void exportTerm("md")}>
                  {t("pty.exportMd")}
                </button>
                {botAvailable && !inTelegram && (
                  <button className="pty-export-option" disabled={exporting} onClick={() => void exportToTelegram("txt")}>
                    {"✈️ "}{t("pty.exportSendTg")}
                  </button>
                )}
                {/* ST-01: трасса клиента, а не содержимое терминала. Выключенный
                    features.trace убирает пункт вместе с самой трассой. */}
                {features.trace && (
                  <button className="pty-export-option" disabled={exporting} onClick={openTrace}>
                    {"🩺 "}{t("pty.traceOpen")}
                  </button>
                )}
                {/* План 9.1: откат функции без выпуска — и в APK, и в Telegram, где
                    DevTools нет. Пункт есть всегда, в том числе с выключенной трассой. */}
                <button className="pty-export-option" onClick={() => { haptic(); setFeaturesOpen(true); }}>
                  {"🧪 "}{t("pty.featuresOpen")}
                </button>
                {/* ST-04, T-37: удаление истории на ЭТОМ устройстве — вторым
                    шагом, с честным «кольцо компьютера и другие зрители не
                    очищаются». Новое действие — только в policy: в legacy
                    (откат ST-04) и в shadow (наблюдение) пункта нет. */}
                {features.retention === "policy" && (clearConfirm ? (
                  <>
                    <p className="pty-trace-warn" role="alert">{t("pty.clearHistoryWarn")}</p>
                    <button className="pty-export-option" onClick={clearDeviceHistory}>
                      {"🧹 "}{t("pty.clearHistoryYes")}
                    </button>
                    <button className="pty-export-option" onClick={() => setClearConfirm(false)}>
                      {t("modal.cancel")}
                    </button>
                  </>
                ) : (
                  <button className="pty-export-option" disabled={exporting} onClick={() => { haptic(); setClearConfirm(true); }}>
                    {"🧹 "}{t("pty.clearHistory")}
                  </button>
                ))}
              </>
            )}
          </div>
        </div>
      )}

      <HelpSheet
        open={helpOpen}
        onClose={() => setHelpOpen(false)}
        // Аудит ИА 02.09.2026, P1-5: из подсказок экрана — в тему гида.
        guide="terminal"
        title={t("pty.termHelpTitle")}
        items={[
          { icon: "\uD83E\uDD16", title: t("pty.termHelpAgentTitle"), text: t("pty.termHelpAgentText") },
          { icon: "\uD83D\uDC46", title: t("pty.termHelpInputTitle"), text: t("pty.termHelpInputText") },
          { icon: "\u2195", title: t("pty.termHelpScrollTitle"), text: t("pty.termHelpScrollText") },
          { icon: "\u2702", title: t("pty.termHelpSelectTitle"), text: t("pty.termHelpSelectText") },
          { icon: "\uD83D\uDCC1", title: t("pty.termHelpFolderTitle"), text: t("pty.termHelpFolderText") },
          { icon: "\uD83D\uDCCE", title: t("pty.termHelpFilesTitle"), text: t("pty.termHelpFilesText") },
          { icon: "\uD83D\uDCAC", title: t("pty.termHelpSendTitle"), text: t("pty.termHelpSendText") },
        ]}
      />
    </div>
  );
}
