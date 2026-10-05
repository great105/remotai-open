import { useEffect, useRef, useState, useCallback } from "react";
import { flushSync } from "react-dom";
import { useLocation, useNavigate } from "react-router-dom";
import { getTelegram, haptic, hapticSuccess, tgConfirm } from "../telegram";
import {
  connectRemote,
  isRemoteOffline,
  type RemoteTransport,
  type RemoteConnection,
  type TransportKind,
  type RtcClientStats,
  type RemoteConnectionStage,
} from "../remote/transport";
// Правила экрана компьютера живут рядом с транспортом, а не в разметке:
// память об этой машине (prefs), тексты отказов (messages) и клавиатура
// (keyboard) проверяются тестами без React.
import {
  clearJpegFallbackMark,
  constrainedNetwork,
  getStoredControlMode,
  getStoredRemoteProfile,
  getStoredSensitivity,
  hasPhysicalKeyboard,
  hasStoredRemoteProfile,
  readJpegFallbackMark,
  readKnownDisplays,
  remoteImageClipboardMax,
  remoteProfiles,
  remoteSensitivityLevels,
  remoteTrafficHintBytes,
  unknownNetworkMobile,
  writeJpegFallbackMark,
  writeKnownDisplays,
  type RemoteProfile,
} from "../remote/prefs";
import { paintRegions, parseScreenFrame, type ParsedFrame } from "../remote/frameFormat";
import { TrackpadGesture, type PadAction, type PadPoint, type PadSurface } from "../remote/trackpad";
import { WsVideoDecoder, canDecodeWsH264 } from "../remote/videoDecode";
import { RemoteAudioPlayer } from "../remote/audioPlayer";
import {
  clipboardErrorMessage,
  fatalRemoteError,
  formatTraffic,
  remoteErrorMessage,
} from "../remote/messages";
import {
  base64ToBlob,
  isNonAsciiChar,
  layoutToLatin,
  localEditCodes,
  localEditKeys,
} from "../remote/keyboard";
import {
  activateBrowserTab, addBrowserBookmark, answerVBrowserDialog, cancelVBrowserFileChooser,
  chooseVBrowserFiles, closeBrowserTab, deleteFile, emulateBrowserDevice, getBrowserDevices,
  getBrowserPage, getBrowserTabs, hitBrowserPoint,
  getServiceStatus, getVBrowserStatus, installVBrowserInput, keepaliveVBrowser, navigateVBrowser,
  newBrowserTab, getVBrowserDownloads, downloadUrl, setBrowserViewport,
  onConnectionChange, remotePreviewBlob, setBrowserScale, startVBrowser, stopVBrowser,
  takeScreenshot as takeScreenshotApi, uploadPtyFile, wakeWS,
} from "../api";
import { resetCapabilities } from "../capabilities";
import { openExternalLink } from "../openExternal";
import {
  chromeOnDrag, doubleTapScale, edgeSwipe, flingSteps, flingVelocity, isTap,
  pinchDistance, pinchScale, pullRefresh,
} from "../remote/browserGestures";
import { searchOrUrl, tabAvatarColor, tabLetter } from "../remote/browserNav";
import { isShareCancel, saveBlob } from "../saveFile";
import { isPcOffline, mapApiError, OfflineState, useEscape, useToast, humanSize } from "@tgcontrol/shared";
import type { BrowserDevice, BrowserHit, BrowserPage, BrowserTab, VBrowserDownload } from "@tgcontrol/shared";
import { t } from "../i18n";
import { HelpSheet } from "../components/HelpSheet";
// Значок двери в шапке — из общего линейного набора, а не новый эмодзи:
// шапка стрима и так пестрит знаками, а набор заведён ровно против разнобоя.
import { IconSliders } from "../components/icons";
import { BottomNav } from "../components/BottomNav";
import { RemoteLaunchScreen } from "../components/RemoteLaunchScreen";
// Оболочка браузера, которой у «удалёнки» не было: стартовый экран пустой
// вкладки, меню долгого нажатия, поиск по странице, пароли и автозаполнение.
import BrowserStartPage from "../components/BrowserStartPage";
import BrowserContextSheet from "../components/BrowserContextSheet";
import BrowserFindBar from "../components/BrowserFindBar";
import BrowserFillSheet from "../components/BrowserFillSheet";
import BrowserHistorySheet from "../components/BrowserHistorySheet";
import { markHomeStep } from "../homeProgress";
import { getMode, getSelectedDeviceId, getSelectedDeviceName, getServerUrl } from "../config";
import { humanDeviceName } from "../devices";
import { useCapabilities } from "../hooks/useCapabilities";
import { useGoBack } from "../navBack";

interface ScreenInfo { sw: number; sh: number }
interface Display { id: number; w: number; h: number }
interface BrowserFileChooser { id: number; multiple: boolean }
interface BrowserDialog {
  id: number;
  type: "alert" | "confirm" | "prompt" | "beforeunload" | string;
  message: string;
  defaultPrompt: string;
  url: string;
}

/** Типы сообщений-ввода: в режиме «только просмотр» их send() роняет. */
const INPUT_MESSAGE_TYPES = new Set(["m", "tp", "s", "k", "txt", "combo", "paste", "key"]);
interface Stats {
  rtt: number;
  fps: number;
  sentFps: number;
  quality: number;
  width: number;
  kbps: number;
  skipped: number;
  captureMs: number;
  encodeMs: number;
  profile: RemoteProfile;
}
type QueuedFrame = ParsedFrame;

export function RemoteView() {
  const navigate = useNavigate();
  const location = useLocation();
  const { platform } = useCapabilities();
  const isWindows = platform === "windows";
  const { toast, toastSuccess, toastError } = useToast();
  const requestedBack = (location.state as { from?: string } | null)?.from;
  const backTarget = requestedBack?.startsWith("/") ? requestedBack : "/";
  // «←» в шапке и системная «Назад» (Android, свайп от края, кнопка Telegram)
  // должны уводить в одно место. Стрелка шла в backTarget, а системная кнопка —
  // всегда на главную: человек, зашедший на «Экран ПК» из «Системы», возвращался
  // то туда, то сюда. Общее правило живёт в navBack.ts, backTarget остался его
  // запасным вариантом (он и приезжает в state.from).
  const goBack = useGoBack();
  const canvasRef = useRef<HTMLCanvasElement>(null);
  const wrapRef = useRef<HTMLDivElement>(null);
  // stage = the zoom/pan transform container holding <video>/<canvas> + the
  // local DOM cursor. Transform lives HERE (not on the canvas) so the cursor
  // and tap-ripple inherit zoom/pan for free.
  const stageRef = useRef<HTMLDivElement>(null);
  const cursorRef = useRef<HTMLDivElement>(null);
  const rippleRef = useRef<HTMLDivElement>(null);
  // The typing-bar text field. We focus it synchronously from the keyboard
  // button's tap handler (see toggleKeyboard) — mobile OSes only raise the
  // on-screen keyboard for a focus that happens inside a user gesture.
  const typingInputRef = useRef<HTMLInputElement>(null);
  const browserFileInputRef = useRef<HTMLInputElement>(null);
  const transportRef = useRef<RemoteTransport | null>(null);
  const connRef = useRef<RemoteConnection | null>(null);
  const screenRef = useRef<ScreenInfo | null>(null);

  const scaleRef = useRef(1);
  const offsetRef = useRef({ x: 0, y: 0 });
  const cursorPos = useRef({ x: 0.5, y: 0.5 });
  const lastFrame = useRef<ImageBitmap | null>(null);
  // Декодер H.264 по веб-сокету: живёт, только если браузер честно сказал, что
  // умеет (см. videoDecode.ts). Там, где WebRTC не поднялся, это единственный
  // способ получить видеокодек вместо картинок.
  const wsVideoRef = useRef<WsVideoDecoder | null>(null);
  // Живой WebRTC-видеотрек: пока он есть, второй видеопуть не нужен.
  const h264ActiveRef = useRef(false);
  // Звук компьютера: заводится только по нажатию человека (браузер иначе не
  // даст звучать) и только пока он включён.
  const audioPlayerRef = useRef<RemoteAudioPlayer | null>(null);
  // Включение ws-видео объявлено ниже (нужны холст и отрисовка), а зовётся
  // выше — из рукопожатия с компьютером.
  const enableWsVideoRef = useRef<(() => void) | null>(null);
  const wsVideoStartRef = useRef(0);

  const decodingFrame = useRef(false);
  const pendingFrame = useRef<QueuedFrame | null>(null);
  const lastRenderedSeq = useRef(0);
  const droppedFrames = useRef(0);

  // Move coalescing + unreliable input lane state. Moves are sent at most once
  // per rAF (latest wins) with a monotonically increasing seq — the agent drops
  // reordered ones. Wheel deltas accumulate fractionally so slow touch scrolls
  // don't die in integer truncation.
  const moveSeqRef = useRef(0);
  const movePendingRef = useRef<{ x: number; y: number } | null>(null);
  const moveRafRef = useRef<number | null>(null);
  const wheelAccRef = useRef(0);
  // Capabilities advertised by the agent in {t:"info"} — gate new protocol use
  // so a new client stays compatible with a pre-2.17 agent.
  const agentCapsRef = useRef({ moveCh: false, cursorPos: false, vack: false });
  const presentedRef = useRef(0); // frames presented (rvfc) → {t:"vack"}
  const firstFrameRef = useRef(false);
  const connectionStartedRef = useRef(Date.now());
  const activeDisplayRef = useRef(0);
  const preferH264Ref = useRef(true);
  // ICE/DTLS сошлись: только с этого момента честно судить, есть ли кадры.
  const rtcConnectedRef = useRef(false);
  const wakeTapRef = useRef(false);
  const backgroundedRef = useRef(document.visibilityState === "hidden");
  const releasedInBg = useRef(false);
  const bgReleaseTimer = useRef<number | null>(null);

  // Отметку «этому ПК H.264 не даётся» читаем РОВНО ОДИН раз за монтирование:
  // дальше режим меняют только вотчдог и кнопка «Попробовать H.264 снова».
  const jpegMarkReadRef = useRef(false);
  if (!jpegMarkReadRef.current) {
    jpegMarkReadRef.current = true;
    preferH264Ref.current = readJpegFallbackMark() == null;
  }

  // Canvas touch state
  const ct = useRef({
    startX: 0, startY: 0, startTime: 0,
    isPan: false, isPinch: false,
    panStartOX: 0, panStartOY: 0,
    pinchStartDist: 0, pinchStartScale: 1,
    moved: false, lastTapTime: 0,
    // Координаты ПЕРВОГО тапа пары — двойной клик шлём именно по ним:
    // между тапами палец всегда чуть смещается.
    lastTapX: 0, lastTapY: 0,
    // Тап двумя пальцами (= правый клик) против щипка: pinchMoved взводится,
    // как только пальцы разъехались.
    twoTapTime: 0, twoTapX: 0, twoTapY: 0, pinchMoved: false,
    // Прокрутка двумя пальцами по «Экрану»: twoScrollY — прошлое положение
    // центра между пальцами, isZooming — жест уже опознан как щипок и обратно
    // в прокрутку не превращается.
    twoScrollY: 0, isZooming: false,
  });

  // Тачпад: одна машина жестов на полосу и на картинку (remote/trackpad.ts).
  const padEngineRef = useRef(new TrackpadGesture());
  const padZoomStartRef = useRef(1);
  const padLongPressTimer = useRef<ReturnType<typeof setTimeout> | null>(null);
  // Обработчики тачпада объявлены ниже абсолютных (им нужны те же помощники);
  // абсолютные зовут их через ref, иначе const ещё не инициализирован.
  type PadHandler = (e: React.TouchEvent, source: "stage" | "strip") => void;
  const padStartRef = useRef<PadHandler>(() => {});
  const padMoveRef = useRef<PadHandler>(() => {});
  const padEndRef = useRef<PadHandler>(() => {});

  // Drag state
  const dragRef = useRef({
    state: "idle" as "idle" | "dragging",
    timer: null as ReturnType<typeof setTimeout> | null,
    source: "" as "" | "canvas" | "pad",
  });
  const mouseRef = useRef({
    down: false,
    button: "l" as "l" | "r" | "m",
  });
  // Chromium после touch иногда синтезирует совместимые mouse-события. Для
  // страницы браузера это превратило бы один палец в два нажатия: сначала tp,
  // затем m. Настоящую мышь не блокируем — только короткое окно после касания.
  const compatMouseUntilRef = useRef(0);

  // Inertia state
  const inertiaRef = useRef({ vx: 0, vy: 0, lastTime: 0 });
  const inertiaRAF = useRef<number | null>(null);

  // UI state
  // Стрим включается ТОЛЬКО кнопкой на экране запуска. Раньше переход на
  // вкладку «Экран ПК» сам открывал сокет и начинал стримить рабочий стол:
  // человек ничего не подтверждал, а компьютер уже слушался его пальца.
  const [streaming, setStreaming] = useState(false);
  // Зеркало для обработчиков, живущих вне рендера (поток событий, уход в фон).
  const streamingRef = useRef(false);
  // Почему стрим погас: причину показываем на экране запуска рядом с кнопкой
  // «Включить снова» — вместо мёртвого «Соединение закрыто» на чёрном поле.
  const [launchNotice, setLaunchNotice] = useState("");
  // Связь глазами экрана запуска — ТРИ состояния, а не «да/нет».
  //
  // Раньше здесь стоял флаг pcOnline, и в него клали один только факт обрыва:
  // любая потеря связи — телефон уехал в метро, Wi-Fi моргнул, релей
  // перезапустился — печаталась как «Компьютер не в сети». Человек читал
  // обвинение своей машине, пока она спокойно работала. Причина обрыва приходит
  // тем же событием (`state.reason`), поэтому «ПК выключен» (offline) и «мы
  // сами потеряли сеть» (checking) теперь разные строки, и красное остаётся
  // только за первым.
  //
  // Стартуем с "checking": до первого ответа мы про машину не знаем НИЧЕГО, а
  // «Компьютер на связи» авансом — то же враньё, только в другую сторону.
  // Ждать нечего: подписка на поток событий отдаёт текущее состояние сразу, а
  // /api/system/service отвечает первым же запросом экрана.
  const [linkState, setLinkState] = useState<"online" | "checking" | "offline">("checking");
  // Бамп «спросить машину заново» — им пользуется кнопка «Проверить снова» и
  // разбор безымянного обрыва (см. подписку на поток событий ниже).
  const [statusKey, setStatusKey] = useState(0);
  // Подписка на поток событий отдаёт текущее состояние ПЕРВЫМ же вызовом — это
  // не событие, а снимок. Отличаем его, чтобы не запускать по нему вторую
  // проверку статуса поверх той, что и так идёт при монтировании экрана.
  const linkSeenRef = useRef(false);
  const [knownDisplays, setKnownDisplays] = useState(readKnownDisplays);
  // Служба Windows не видит рабочий стол / дисплея нет вовсе: обе беды видны
  // из /api/system/service ещё до соединения, поэтому предупреждаем на экране
  // запуска, а не плашкой поверх уже открытой картинки.
  const [serviceMode, setServiceMode] = useState(false);
  const [noDisplay, setNoDisplay] = useState(false);
  const [connected, setConnected] = useState(false);
  const [reconnecting, setReconnecting] = useState(false);
  const [reconnectNum, setReconnectNum] = useState(0);
  const [retryKey, setRetryKey] = useState(0);
  const [connectionError, setConnectionError] = useState("");
  // Компьютер выключен/спит: отдельное честное состояние, а не «Ошибка
  // подключения». Ref — чтобы обработчики транспорта видели свежее значение.
  const [pcOffline, setPcOffline] = useState(false);
  const pcOfflineRef = useRef(false);
  // Взводится, когда правда о выключенном ПК пришла ИЗВНЕ (поток событий) уже
  // после старта connect(): onClose читает его и не запускает лестницу.
  const haltReconnectRef = useRef(false);
  const [connectionStage, setConnectionStage] = useState<RemoteConnectionStage>("route");
  const [connectSeconds, setConnectSeconds] = useState(0);
  const [firstFrame, setFirstFrame] = useState(false);
  const [previewReady, setPreviewReady] = useState(false);
  const [screenInfo, setScreenInfo] = useState<ScreenInfo | null>(null);
  const [showKb, setShowKb] = useState(false);
  const [showUI, setShowUI] = useState(true);
  const [controlMode, setControlMode] = useState<"screen" | "trackpad">(getStoredControlMode);
  // Чувствительность тачпада запоминается так же, как режим и качество (N141).
  const [sensitivity, setSensitivity] = useState(getStoredSensitivity);
  const [modifiers, setModifiers] = useState({ ctrl: false, alt: false, shift: false });
  // Активные Ctrl/Alt/Shift нужны и мышиным/тач-обработчикам — они живут вне
  // рендера, поэтому держим зеркало в ref (N138).
  const modifiersRef = useRef(modifiers);
  // Ноутбук/десктоп: клавиатура работает без режима ⌨ (N148).
  const [physicalKeyboard] = useState(hasPhysicalKeyboard);
  // Широкий экран — компьютер или планшет: там у браузера строка вкладок, как в
  // десктопном браузере (в шит их прятать незачем — мышь рядом). Следим за
  // размером: окно на компьютере меняют, и панель обязана появляться и уходить
  // вместе с ним.
  const [wideScreen, setWideScreen] = useState(() => window.innerWidth >= 768);
  useEffect(() => {
    const onResize = () => setWideScreen(window.innerWidth >= 768);
    window.addEventListener("resize", onResize);
    return () => window.removeEventListener("resize", onResize);
  }, []);
  // Набранное в строке ввода живёт в state: закрытие строки его больше не
  // съедает, а Enter больше не приклеен к отправке текста (N137).
  const [typingText, setTypingText] = useState("");
  const [fps, setFps] = useState(0);
  const [zoomLevel, setZoomLevel] = useState(1);
  const [displays, setDisplays] = useState<Display[]>([]);
  const [activeDisplay, setActiveDisplay] = useState(0);
  const [showQuickActions, setShowQuickActions] = useState(false);
  const [showClipboard, setShowClipboard] = useState(false);
  const [showQuality, setShowQuality] = useState(false);
  const [showHudDetails, setShowHudDetails] = useState(() => {
    try { return localStorage.getItem("tgcontrol.remote.hudDetails") === "1"; } catch { return false; }
  });
  const [helpOpen, setHelpOpen] = useState(false);
  const [clipboardText, setClipboardText] = useState("");
  // Чтение буфера ПК — лишь ОДНА из функций панели обмена: «Вставить из буфера
  // телефона», «Отправить введённый текст» и «Изображение → ПК» с ним не
  // связаны вовсе. Поэтому панель открывается сразу, а состояние чтения живёт
  // внутри неё: отказ ПК больше не запирает дверь на телефон → ПК.
  const [clipboardLoading, setClipboardLoading] = useState(false);
  const [clipboardError, setClipboardError] = useState("");
  const [manualClipboardText, setManualClipboardText] = useState("");
  const [streamProfile, setStreamProfile] = useState<RemoteProfile>(getStoredRemoteProfile);
  // "auto" — «Эконом» включился сам (Network Information API сказал «мобильная
  // сеть»), "offer" — API нет вовсе (iOS/Telegram-iOS), поэтому предлагаем
  // включить его нажатием (N140).
  const [networkSaverHint, setNetworkSaverHint] = useState<"" | "auto" | "offer">(() => {
    if (hasStoredRemoteProfile()) return "";
    if (constrainedNetwork()) return "auto";
    return unknownNetworkMobile() ? "offer" : "";
  });
  const [rotateHint, setRotateHint] = useState(false);
  // Чип расхода трафика: показываем один раз за сеанс после порога, дальше он
  // гаснет сам — трафик перестал быть виден только внутри шита.
  const [trafficHint, setTrafficHint] = useState(false);
  const trafficHintShownRef = useRef(false);
  const [trafficBytes, setTrafficBytes] = useState(0);
  const [decodeDrops, setDecodeDrops] = useState(0);
  const [transportKind, setTransportKind] = useState<TransportKind | null>(null);
  const [h264Active, setH264Active] = useState(false);
  // Видео «идёт», но кадров нет: декодер не собрался. См. вотчдог ниже.
  const [videoStuck, setVideoStuck] = useState(false);
  const h264WatchdogRef = useRef<number | null>(null);
  // Совместимый JPEG-режим запомнен для этого ПК: показываем это в шите
  // «Качество соединения» и даём кнопку возврата к H.264 (N136).
  const [jpegForced, setJpegForced] = useState(() => readJpegFallbackMark() != null);
  // Ввод не доходит до ПК (заблокированный рабочий стол / UAC) либо вводить
  // нечем: агент сообщает это кадром {t:"warn"} по control-каналу (N1).
  const [inputWarn, setInputWarn] = useState<"" | "input_blocked" | "input_unavailable">("");
  // Захват экрана падает на агенте: {t:"error",code:"capture_failed"}. Соединение
  // при этом живое и агент повторяет попытки, поэтому не рвём его — показываем
  // причину вместо бесконечного «Жду первый кадр».
  const [captureFailed, setCaptureFailed] = useState(false);
  const captureFailedRef = useRef(false);
  const [screenshotBusy, setScreenshotBusy] = useState(false);
  // Звук компьютера: выключен, ждём ответа компьютера, включён или машина его
  // не отдаёт. «pending» появился потому, что между нажатием и первым звуком
  // лежит дорога до ПК и обратно, а кнопка всё это время молчала.
  const [audioState, setAudioState] = useState<"off" | "pending" | "on" | "unavailable">("off");
  // Сколько ждём ответа {t:"audio"}, прежде чем сказать «компьютер не ответил».
  const audioWaitRef = useRef<number | null>(null);
  const [cstats, setCstats] = useState<RtcClientStats | null>(null);
  const [stats, setStats] = useState<Stats>({
    rtt: 0, fps: 0, sentFps: 0, quality: 0, width: 0, kbps: 0,
    skipped: 0, captureMs: 0, encodeMs: 0, profile: "auto",
  });
  const [dragging, setDragging] = useState(false);

  // ── Virtual browser gate (headless Linux agent) ─────────────────
  // "unknown" → probing; "needed" → no display, offer Xvfb+browser start;
  // "clear" → display exists (real or virtual), connect normally.
  const [vbGate, setVbGate] = useState<"unknown" | "needed" | "clear">("unknown");
  // Авто-старт vbrowser при входе на экран headless-сервера — один раз,
  // чтобы ошибка запуска не раскрутила цикл повторных попыток.
  const vbAutoStartRef = useRef(false);
  const [vbRunning, setVbRunning] = useState(false);
  const [vbStarting, setVbStarting] = useState(false);
  const [vbError, setVbError] = useState("");
  const [vbHint, setVbHint] = useState("");
  // Сколько секунд осталось до автоостановки браузера по простою (0 — баннера
  // нет). Агент предупреждает заранее, и у человека есть кнопка «Оставить
  // включённым» — без неё браузер гас бы посреди чтения длинной страницы.
  const [vbIdleWarn, setVbIdleWarn] = useState(0);
  // Чем машина принимает нажатия. На Linux это отдельная программа (xdotool), и
  // без неё экран показывается, а нажатия деваться некуда — живой случай: на
  // сервере были Xvfb и Chrome, картинка шла, а каждый тап упирался в совет
  // «установите его на компьютере», то есть в поход в SSH с телефона.
  // Старый агент поля не присылает — тогда молчим, как раньше.
  // Кадры и ввод идут через сам браузер (порт отладки на машине) — тогда в
  // кадре ЛИШЬ страница, без адресной строки браузера, и адрес нужно показать
  // своей строкой: иначе открыть новый сайт можно только вслепую.
  const [vbDirect, setVbDirect] = useState(false);
  // Палец работает как палец: страница получает настоящие касания, а не мышь.
  // Только в режиме «Экран»: «Тачпад» человек выбирает осознанно, чтобы целить
  // курсором, и подменять ему управление нельзя. Объявлено здесь, а не ближе к
  // вызовам: обработчики жестов выше по файлу берут его в зависимости
  // useCallback — иначе ловим TDZ на рендере.
  const browserTouch = vbDirect && controlMode === "screen";
  // Оболочка браузера: что за страница открыта, какие есть вкладки, какой
  // масштаб. В кадре этого нет — там только сама страница, — поэтому всё, что в
  // обычном браузере нарисовано вокруг неё, живёт здесь.
  const [browserPage, setBrowserPage] = useState<BrowserPage | null>(null);
  const [browserTabs, setBrowserTabs] = useState<BrowserTab[]>([]);
  const [tabsOpen, setTabsOpen] = useState(false);
  const pageScaleRef = useRef(1);
  // Жест пальца по странице: замеры для инерции, стартовая точка (тап или
  // протяжка), состояние щипка и остаток прокрутки. Живёт в ref — на каждое
  // движение пальца перерисовывать экран нельзя.
  const bt = useRef({
    active: false, startX: 0, startY: 0, startAt: 0,
    samples: [] as { x: number; y: number; t: number }[],
    pinching: false, pinchStart: 0, pinchBase: 1, lastScaleAt: 0,
    scrollAcc: 0,
    // Прятание хрома при скролле: последняя клиентская Y пальца и накопленный
    // сдвиг с прошлого решения (правила — в browserGestures.chromeOnDrag).
    chromeY: 0, chromeAcc: 0,
    // Жесты телефонного браузера: точка касания в координатах области экрана
    // (для свайпа от края), выбранное направление, натяжение сверху и память о
    // прошлом тапе — по ней узнаётся двойной.
    clientX0: 0, clientY0: 0, width: 0,
    edge: null as "back" | "forward" | null,
    pulling: false, doubleTap: false,
    lastTapAt: 0, lastTapX: 0, lastTapY: 0,
  });
  const [addressOpen, setAddressOpen] = useState(false);
  const [addressText, setAddressText] = useState("");
  // Где стоит страница (это присылает наблюдатель на машине) и состояние
  // жестов, которых у удалёнки не было вовсе: «потянуть вниз, чтобы обновить»,
  // свайп от края «назад/вперёд», меню долгого нажатия, поиск по странице,
  // заполнение форм.
  const pageScrollRef = useRef(0);
  const [pullProgress, setPullProgress] = useState(0);
  const [refreshing, setRefreshing] = useState(false);
  const [ctxHit, setCtxHit] = useState<BrowserHit | null>(null);
  const [findOpen, setFindOpen] = useState(false);
  const [fillOpen, setFillOpen] = useState(false);
  const [historyOpen, setHistoryOpen] = useState(false);
  // Горячие клавиши браузера (Ctrl+T/W/L/F/R) — через ref: обработчик клавиш
  // живёт выше объявления этих действий, и прямая ссылка на них падала бы на TDZ.
  const browserHotkeysRef = useRef<Record<string, () => void>>({});
  const [deviceList, setDeviceList] = useState<BrowserDevice[]>([]);
  const longPressRef = useRef<ReturnType<typeof setTimeout> | null>(null);
  // Меню ⋮ нижней панели браузера и упрятанный скроллом хром (как у настоящего
  // мобильного браузера: листаешь дальше — панели уезжают с глаз).
  const [browserMenu, setBrowserMenu] = useState(false);
  // «Только просмотр»: стрим идёт, а ввод в браузер не уходит (показать
  // коллеге/агенту, не дав тыкать). ref — чтобы send() не плодил замыканий.
  const [readOnly, setReadOnly] = useState(false);
  const readOnlyRef = useRef(false);
  // Загрузки виртуального браузера: шит со списком файлов с сервера.
  const [downloadsOpen, setDownloadsOpen] = useState(false);
  const [downloads, setDownloads] = useState<VBrowserDownload[]>([]);
  const [downloadsLoading, setDownloadsLoading] = useState(false);
  // Нативные окна Chrome не попадают в видеокадр. Когда сайт просит файл,
  // CDP присылает событие, а этот шит даёт выбрать файл на телефоне, загрузить
  // его на агент и назначить настоящему <input type=file>.
  const [browserFileChooser, setBrowserFileChooser] = useState<BrowserFileChooser | null>(null);
  const [browserFileUploading, setBrowserFileUploading] = useState(false);
  const [browserFileProgress, setBrowserFileProgress] = useState(0);
  const [browserFileError, setBrowserFileError] = useState("");
  const browserFileAbortRef = useRef<AbortController | null>(null);
  // alert/confirm/prompt тоже нативны и без этого состояния блокировали
  // страницу невидимым окном.
  const [browserDialog, setBrowserDialog] = useState<BrowserDialog | null>(null);
  const [browserDialogPrompt, setBrowserDialogPrompt] = useState("");
  const [browserDialogBusy, setBrowserDialogBusy] = useState(false);
  const [chromeHidden, setChromeHidden] = useState(false);
  const [vbInput, setVbInput] = useState({
    missing: false, canInstall: false, installing: false, hint: "", error: "",
  });

  useEffect(() => {
    let alive = true;
    getServiceStatus()
      .then((st) => {
        if (!alive) return;
        // Машина ответила — значит она на связи. Для LAN и окна exe это
        // ЕДИНСТВЕННЫЙ источник правды: там канал событий идёт прямо к ПК и при
        // его выключении просто молчит, не называя причину «pc_offline», —
        // ровно поэтому главная берёт офлайн из двух источников сразу
        // (Dashboard: кадр agent_status ИЛИ провал health).
        setLinkState("online");
        const vb = st?.vbrowser;
        setVbRunning(!!vb?.running);
        setVbDirect(!!vb?.running && !!vb?.debug_port);
        setVbInput({
          missing: vb?.input_ready === false,
          canInstall: !!vb?.can_install_input,
          installing: !!vb?.input_installing,
          hint: vb?.input_hint || "",
          error: vb?.input_error || "",
        });
        // Виртуальный дисплей снимает обе беды разом — при живом vbrowser
        // ни «служба Windows», ни «нет экрана» уже не мешают.
        setServiceMode(st?.running_as_service === true && !vb?.running);
        setNoDisplay(st?.has_display === false && !vb?.running);
        if (st?.has_display === false && vb?.available && !vb?.running) {
          setVbHint(vb.hint || "");
          setVbGate("needed");
          // Авто-старт (prewarm): человек открыл экран headless-сервера —
          // браузер ему нужен прямо сейчас. Пока шёл бы тап по кнопке, Chrome
          // уже поднимается: кадр появляется на секунды раньше. Один раз за
          // вход на экран; при ошибке остаётся обычная кнопка.
          if (!vbAutoStartRef.current) {
            vbAutoStartRef.current = true;
            handleVbStart();
          }
        } else {
          setVbGate("clear");
        }
      })
      .catch((e) => {
        if (!alive) return;
        setVbGate("clear");
        // Отказ отказу рознь: «агент не отвечает» — это выключенный компьютер и
        // право сказать об этом красным, а любая другая ошибка означает лишь
        // «не выяснили» — тогда остаёмся в «Проверяем связь…» и кнопку запуска
        // не гасим, иначе неизвестная ошибка запирала бы экран.
        setLinkState(isPcOffline(e) ? "offline" : "checking");
      });
    return () => { alive = false; };
  }, [statusKey]);

  // «Проверить снова» — тот же выход, что у офлайн-карточки главной и остальных
  // экранов (OfflineState → onRetry). До него из офлайна на этом экране выхода
  // не было вовсе: кнопка «Включить экран» горела ярко и вела в тупик.
  const recheckLink = useCallback(() => {
    haptic("light");
    setLinkState("checking");
    // Канал событий и REST-проба — те же два источника, из которых состояние и
    // складывается: сокет мог замёрзнуть в фоне, а мог быть жив и просто ждать
    // кадра agent_status от вернувшейся машины.
    wakeWS();
    setStatusKey((value) => value + 1);
  }, []);

/**
 * Размер виртуального экрана под этого зрителя.
 *
 * Берём БОЛЬШУЮ сторону как высоту: телефон почти всегда держат вертикально, а
 * альбомный экран сервера превращался у него в полоску. Ограничиваем сверху —
 * каждый лишний пиксель на сервере стоит копирования кадра, а ядро там одно.
 * Планшет/десктоп (широкий экран) оставляем как есть.
 */
/**
 * Пустая вкладка. Внутренние страницы браузера («новая вкладка», пустая
 * страница) — не сайты: показывать поверх них свой стартовый экран правильнее,
 * чем чужую десктопную страницу Chrome, которую человек всё равно не может ни
 * настроить, ни нормально нажать пальцем.
 */
function isBlankPage(url?: string): boolean {
  if (!url) return true;
  const value = url.trim().toLowerCase();
  return value === "" || value === "about:blank"
    || value.startsWith("chrome://new-tab-page") || value.startsWith("chrome://newtab")
    || value === "chrome://blank";
}

/**
 * Виды сайта на случай, когда агент старый и каталога не отдаёт. Значения — те
 * же имена, что понимает эмуляция; их и раньше слал клиент.
 */
const FALLBACK_DEVICES: BrowserDevice[] = [
  { id: "android", title: t("remote.profilePhone"), mobile: true, width: 412, height: 915 },
  { id: "ios", title: "iPhone", mobile: true, width: 393, height: 852 },
  { id: "desktop", title: t("infra.local.thisPcName"), mobile: false, width: 1280, height: 800 },
];

/** Имя сайта для адресной строки: полный URL в неё не влезает, а человеку
 *  важно видеть, где он находится, — как в мобильных браузерах. */
function hostOf(url: string): string {
  if (!url || url === "about:blank") return "";
  try {
    return new URL(url).host.replace(/^www\./, "");
  } catch {
    return url.slice(0, 40);
  }
}

function virtualScreenSize(): { width: number; height: number } {
  const w = Math.round(window.screen?.width || window.innerWidth || 1280);
  const h = Math.round(window.screen?.height || window.innerHeight || 800);
  const short = Math.min(w, h);
  const long = Math.max(w, h);
  const portrait = long / Math.max(1, short) > 1.3;
  if (!portrait) return { width: Math.min(1600, long), height: Math.min(1000, short) };
  // Портрет: не даём стороне уехать за 1280 — 1080×2400 это 10 МБ на кадр.
  const width = Math.min(1080, Math.max(600, short));
  const height = Math.min(1600, Math.max(800, Math.round(width * (long / short))));
  return { width, height };
}

  const handleVbStart = useCallback(async () => {
    if (vbStarting) return;
    setVbStarting(true);
    setVbError("");
    try {
      // Язык браузера НЕ берём из приложения: он живёт на сервере в чужой
      // стране, и русский интерфейс рядом с зарубежным адресом выглядит для
      // сайтов страннее нейтрального английского. Агент ставит en-US.
      //
      // А вот РАЗМЕР берём свой: экран под пропорции зрителя. На телефоне
      // альбомные 1280×800 показывались узкой полоской, и приходилось
      // поворачивать телефон, чтобы прочитать текст. Отдаём логические
      // пиксели (не физические): 1080×2400 превратились бы в неподъёмный для
      // одноядерного сервера кадр, а страница всё равно верстается по CSS-px.
      const size = virtualScreenSize();
      // Ответ на запуск уже содержит всё, что нужно экрану, — берём его, а не
      // ждём следующего опроса состояния. Иначе адресная строка и вкладки не
      // появлялись до перезахода на экран: признак «браузер отвечает напрямую»
      // обновлялся только при загрузке страницы (живая жалоба: приложение
      // свежее, а шапки браузера нет).
      const started = await startVBrowser(size);
      setVbDirect(!!started?.debug_port);
      // Сразу говорим странице, кто на неё смотрит: телефону нужна мобильная
      // вёрстка (без неё сайт присылает кнопки под мышь, и человек получает не
      // браузер, а картинку чужого компьютера), а на компьютере с мышью —
      // обычная десктопная. Профиль применяется к каждой новой вкладке (агент
      // помнит его между переподключениями), переключить вид можно в меню ⋮.
      try {
        const touchUI = controlMode === "screen";
        await emulateBrowserDevice({
          device: touchUI ? "android" : "desktop",
          width: touchUI ? size.width : 1280,
          height: touchUI ? size.height : 800,
          scale: touchUI ? (window.devicePixelRatio || 2) : 1,
        });
      } catch { /* старый агент профиля не знает — экран всё равно работает */ }
      haptic("light");
      resetCapabilities(); // has_display flipped → refetch for other screens
      setVbRunning(true);
      setVbGate("clear");
      setServiceMode(false);
      setNoDisplay(false);
      setRetryKey((value) => value + 1);
      // «Запустить браузер» — уже явное нажатие «покажи мне этот экран»:
      // просить второе подтверждение сразу после первого было бы издевательством.
      setStreaming(true);
    } catch (e: any) {
      setVbError(e?.message || t("remote.connectionError"));
    } finally {
      setVbStarting(false);
    }
  }, [vbStarting, controlMode]);

  // «Включить нажатия» — агент ставит xdotool сам. Запрос возвращается сразу
  // (apt на машине идёт минуты, облачный запрос живёт 30 секунд), поэтому
  // результат вычитываем поллингом статуса. Оставаться на экране необязательно:
  // установка на машине не прервётся, а вернувшись, человек увидит готовый ввод.
  const handleInstallInput = useCallback(async () => {
    if (vbInput.installing) return;
    haptic("light");
    setVbInput((prev) => ({ ...prev, installing: true, error: "" }));
    try {
      await installVBrowserInput();
    } catch (e: any) {
      setVbInput((prev) => ({ ...prev, installing: false, error: e?.message || t("remote.connectionError") }));
      return;
    }
    const deadline = Date.now() + 6 * 60_000;
    while (Date.now() < deadline) {
      await new Promise((resolve) => setTimeout(resolve, 3000));
      let vb: Awaited<ReturnType<typeof getVBrowserStatus>> | null = null;
      try { vb = await getVBrowserStatus(); } catch { continue; } // связь моргнула — ждём дальше
      if (vb.input_ready) {
        hapticSuccess();
        setVbInput({ missing: false, canInstall: false, installing: false, hint: "", error: "" });
        setInputWarn("");
        resetCapabilities();
        setRetryKey((value) => value + 1);
        return;
      }
      if (!vb.input_installing) {
        setVbInput({
          missing: true, canInstall: !!vb.can_install_input, installing: false,
          hint: vb.input_hint || "", error: vb.input_error || t("remote.inputInstallFailed"),
        });
        return;
      }
    }
    setVbInput((prev) => ({ ...prev, installing: false, error: t("remote.inputInstallFailed") }));
  }, [vbInput.installing]);

  const handleVbStop = useCallback(async () => {
    if (!(await tgConfirm(t("remote.vbStopConfirm"), { danger: true, confirmText: t("confirm.btn.stop") }))) return;
    try { await stopVBrowser(); } catch { /* already down */ }
    resetCapabilities();
    navigate(backTarget);
  }, [backTarget, navigate]);

  // «Оставить включённым» из баннера предупреждения о простое: один дешёвый
  // запрос сдвигает дедлайн автоостановки, баннер прячем сразу.
  const handleVbKeepalive = useCallback(async () => {
    haptic("light");
    setVbIdleWarn(0);
    try { await keepaliveVBrowser(); } catch { /* связь моргнула — браузер остановится, экран вернётся в панель запуска */ }
  }, []);

  // Шит загрузок виртуального браузера: список с сервера, тап — скачать на
  // это устройство обычным files/download (умеет поток и облако).
  const openDownloads = useCallback(async () => {
    setBrowserMenu(false);
    setDownloadsOpen(true);
    setDownloadsLoading(true);
    try {
      const res = await getVBrowserDownloads();
      setDownloads(res.downloads || []);
    } catch {
      setDownloads([]);
    } finally {
      setDownloadsLoading(false);
    }
  }, []);

  const toggleReadOnly = useCallback(() => {
    setReadOnly((v) => {
      readOnlyRef.current = !v;
      return !v;
    });
    setBrowserMenu(false);
  }, []);

  const dismissBrowserFileChooser = useCallback(async () => {
    const chooser = browserFileChooser;
    if (!chooser) return;
    browserFileAbortRef.current?.abort();
    setBrowserFileChooser(null);
    setBrowserFileUploading(false);
    setBrowserFileProgress(0);
    setBrowserFileError("");
    try {
      await cancelVBrowserFileChooser(chooser.id);
    } catch {
      // Вкладка могла закрыть picker сама; локально он уже закрыт.
    }
  }, [browserFileChooser]);

  const pickBrowserFiles = useCallback(async (event: React.ChangeEvent<HTMLInputElement>) => {
    const chooser = browserFileChooser;
    const picked = Array.from(event.currentTarget.files || []);
    event.currentTarget.value = "";
    if (!chooser || picked.length === 0 || browserFileUploading) return;
    const files = chooser.multiple ? picked : picked.slice(0, 1);
    const controller = new AbortController();
    browserFileAbortRef.current = controller;
    setBrowserFileUploading(true);
    setBrowserFileProgress(0);
    setBrowserFileError("");
    const uploaded: Array<{ path: string; name: string }> = [];
    try {
      for (let index = 0; index < files.length; index++) {
        const file = files[index];
        const result = await uploadPtyFile(file, (pct) => {
          const total = Math.round(((index + pct / 100) / files.length) * 100);
          setBrowserFileProgress(Math.max(0, Math.min(100, total)));
        }, { signal: controller.signal, resumable: true });
        uploaded.push({ path: result.path, name: file.name });
      }
      await chooseVBrowserFiles(chooser.id, uploaded);
      setBrowserFileProgress(100);
      setBrowserFileChooser(null);
      hapticSuccess();
      toastSuccess(t("remote.browserFilesChosen", { n: files.length }));
    } catch (e: any) {
      // Если до назначения input дошла лишь часть файлов, их транспортные
      // копии в ~/Remotai/files больше никому не нужны.
      await Promise.all(uploaded.map((item) => deleteFile(item.path).catch(() => undefined)));
      if (!controller.signal.aborted) {
        setBrowserFileError(mapApiError(e));
        toastError(mapApiError(e));
      }
    } finally {
      if (browserFileAbortRef.current === controller) browserFileAbortRef.current = null;
      setBrowserFileUploading(false);
    }
  }, [browserFileChooser, browserFileUploading, toastError, toastSuccess]);

  const respondBrowserDialog = useCallback(async (accept: boolean) => {
    const dialog = browserDialog;
    if (!dialog || browserDialogBusy) return;
    setBrowserDialogBusy(true);
    try {
      await answerVBrowserDialog(dialog.id, accept, browserDialogPrompt);
      setBrowserDialog(null);
      setBrowserDialogPrompt("");
    } catch (e: any) {
      // Диалог мог закрыться вместе со страницей; отдельное событие уберёт
      // шит. Если же он жив, показываем понятную причину и оставляем действия.
      toastError(mapApiError(e));
    } finally {
      setBrowserDialogBusy(false);
    }
  }, [browserDialog, browserDialogBusy, browserDialogPrompt, toastError]);

  const fpsCounter = useRef({ frames: 0, lastTime: Date.now() });
  const hideTimer = useRef<ReturnType<typeof setTimeout> | null>(null);
  const maxReconnect = 10;
  const streamProfileRef = useRef(streamProfile);

  // System Back closes overlays first (quality → quick actions → clipboard → keyboard);
  // with nothing open it leaves the screen via the global route fallback.
  useEscape(!!browserDialog && !browserDialogBusy, () => { void respondBrowserDialog(false); });
  useEscape(!browserDialog && !!browserFileChooser && !browserFileUploading, () => {
    void dismissBrowserFileChooser();
  });
  useEscape(!browserDialog && !browserFileChooser && downloadsOpen, () => setDownloadsOpen(false));
  useEscape(!browserDialog && !browserFileChooser && !downloadsOpen && showQuality, () => setShowQuality(false));
  useEscape(!browserDialog && !browserFileChooser && !downloadsOpen && showQuickActions, () => setShowQuickActions(false));
  useEscape(!browserDialog && !browserFileChooser && !downloadsOpen && !showQuality && !showQuickActions && showClipboard, () => setShowClipboard(false));
  useEscape(!browserDialog && !browserFileChooser && !downloadsOpen && !showQuality && !showQuickActions && !showClipboard && showKb, () => setShowKb(false));
  // Оверлеи браузера (адрес, вкладки, меню ⋮) закрываются системной «Назад»
  // первыми: они лежат поверх всего остального.
  useEscape(!browserDialog && !browserFileChooser && !downloadsOpen && (addressOpen || tabsOpen || browserMenu), () => {
    setAddressOpen(false);
    setTabsOpen(false);
    setBrowserMenu(false);
  });

  // ── Utility callbacks ──────────────────────────────────────────

  const resetHideTimer = useCallback(() => {
    if (hideTimer.current) clearTimeout(hideTimer.current);
    setShowUI(true);
    // Пока не пришёл первый кадр, прятать нечего: таймер заводился на открытии
    // канала, и на медленном облаке (>5 с до картинки) рабочий стол появлялся
    // уже БЕЗ единой кнопки. Отсчёт заводит эффект по firstFrame.
    if (!firstFrameRef.current) return;
    hideTimer.current = setTimeout(() => setShowUI(false), 5000);
  }, []);

  // Честное «Компьютер не в сети»: спиннер и лестница реконнектов гаснут, в
  // оверлее — то же состояние, что на /files, /pty и /system (OfflineState).
  const markPcOffline = useCallback(() => {
    pcOfflineRef.current = true;
    haltReconnectRef.current = true;
    setPcOffline(true);
    setConnectionError(t("conn.pcOffline"));
    setReconnecting(false);
    setReconnectNum(0);
  }, []);

  useEffect(() => {
    streamingRef.current = streaming;
  }, [streaming]);

  /**
   * Погасить стрим и вернуться на экран запуска.
   *
   * `reason` пуст, когда человек остановил экран сам («Остановить экран» в ⋮),
   * и заполнен, когда связь оборвалась и все попытки переподключения
   * исчерпаны: тогда причина стоит рядом с кнопкой «Включить снова».
   */
  const stopStream = useCallback((reason: string) => {
    setLaunchNotice(reason);
    setStreaming(false);
    setConnected(false);
    setFirstFrame(false);
    firstFrameRef.current = false;
    setReconnecting(false);
    setReconnectNum(0);
    setConnectionError("");
    setPcOffline(false);
    pcOfflineRef.current = false;
    haltReconnectRef.current = false;
    setPreviewReady(false);
    setVideoStuck(false);
    setCaptureFailed(false);
    captureFailedRef.current = false;
    setInputWarn("");
    // Всё, что открыто поверх стрима, гасим вместе с ним: иначе шит «Качество
    // соединения» повиснет над экраном запуска и будет показывать цифры
    // умершего соединения.
    setShowQuickActions(false);
    setShowClipboard(false);
    setShowQuality(false);
    setShowKb(false);
    browserFileAbortRef.current?.abort();
    browserFileAbortRef.current = null;
    setBrowserFileChooser(null);
    setBrowserFileUploading(false);
    setBrowserFileProgress(0);
    setBrowserFileError("");
    setBrowserDialog(null);
    setBrowserDialogPrompt("");
    setBrowserDialogBusy(false);
    setDownloadsOpen(false);
  }, []);

  const startStream = useCallback(() => {
    haptic("medium");
    setLaunchNotice("");
    setConnectionError("");
    setPcOffline(false);
    pcOfflineRef.current = false;
    haltReconnectRef.current = false;
    connectionStartedRef.current = Date.now();
    setConnectSeconds(0);
    setStreaming(true);
  }, []);

  useEffect(() => {
    streamProfileRef.current = streamProfile;
  }, [streamProfile]);

  useEffect(() => {
    modifiersRef.current = modifiers;
  }, [modifiers]);

  useEffect(() => {
    activeDisplayRef.current = activeDisplay;
  }, [activeDisplay]);

  useEffect(() => {
    // На экране запуска считать нечего: секунды подключения идут с нажатия
    // кнопки, а не с открытия вкладки.
    if (!streaming || firstFrame || connectionError) return;
    const update = () => setConnectSeconds(
      Math.max(0, Math.floor((Date.now() - connectionStartedRef.current) / 1000)),
    );
    update();
    const timer = window.setInterval(update, 1000);
    return () => window.clearInterval(timer);
  }, [connectionError, firstFrame, retryKey, streaming]);

  // Кадр пришёл — вот теперь честно заводить отсчёт автоскрытия контролов.
  useEffect(() => {
    if (firstFrame) resetHideTimer();
  }, [firstFrame, resetHideTimer]);

  useEffect(() => {
    if (!firstFrame) return;
    try {
      if (localStorage.getItem("tgcontrol.remote.helpSeen") === "1") return;
      localStorage.setItem("tgcontrol.remote.helpSeen", "1");
    } catch { /* storage is optional */ }
    setHelpOpen(true);
  }, [firstFrame]);

  // Подсказка «поверните телефон» имеет смысл только когда рабочий стол уже
  // виден: раньше она взводилась при монтировании и висела поверх «Ошибка
  // подключения», обещая крупный рабочий стол вместо чёрного экрана.
  useEffect(() => {
    if (!firstFrame) return;
    if (!window.matchMedia?.("(orientation: portrait)").matches) return;
    try {
      if (localStorage.getItem("tgcontrol.remote.rotateHintSeen") === "1") return;
    } catch { /* storage is optional */ }
    setRotateHint(true);
  }, [firstFrame]);

  // «Показана» отмечаем не при взводе, а когда подсказку действительно увидели:
  // закрыли крестиком, повернули телефон или она провисела 12 секунд на экране.
  // Прежний код писал флаг сразу и сжигал подсказку навсегда, даже если она
  // пряталась под оверлеем подключения и человек её ни разу не прочитал.
  const dismissRotateHint = useCallback(() => {
    setRotateHint(false);
    try { localStorage.setItem("tgcontrol.remote.rotateHintSeen", "1"); } catch { /* optional */ }
  }, []);

  // Подсказка видна только при живой картинке (оверлей подключения её прячет) —
  // отсчёт «прочитано» идёт лишь в это время. Модальная справка первого запуска
  // взводится тем же первым кадром и накрывает подсказку целиком: без !helpOpen
  // 12-секундный таймер сгорал под ней, и подсказку не видели НИ РАЗУ.
  const rotateHintVisible = rotateHint && connected && firstFrame && !helpOpen;

  useEffect(() => {
    if (!rotateHintVisible) return;
    const timer = window.setTimeout(dismissRotateHint, 12000);
    const mq = window.matchMedia?.("(orientation: portrait)");
    const onOrientation = () => { if (!mq?.matches) dismissRotateHint(); };
    // addEventListener у MediaQueryList есть не во всех старых WebView.
    mq?.addEventListener?.("change", onOrientation);
    return () => {
      window.clearTimeout(timer);
      mq?.removeEventListener?.("change", onOrientation);
    };
  }, [dismissRotateHint, rotateHintVisible]);

  const markFirstFrame = useCallback(() => {
    // Кадр пришёл — значит захват ожил: плашку «экран не снимается» снимаем без
    // таймеров. Проверка по ref, чтобы не дёргать состояние на каждом кадре.
    if (captureFailedRef.current) {
      captureFailedRef.current = false;
      setCaptureFailed(false);
    }
    if (firstFrameRef.current) return;
    firstFrameRef.current = true;
    setFirstFrame(true);
    setConnectionStage("frame");
    markHomeStep("remote");
  }, []);

  const send = useCallback((data: any) => {
    // Режим «только просмотр»: ввод в браузер не уходит вовсе — ни случайный
    // тап, ни клавиатура. Служебные сообщения (ack, vack, cstats, keepalive)
    // пропускаем, иначе стрим встанет.
    if (readOnlyRef.current && INPUT_MESSAGE_TYPES.has(data?.t)) return;
    transportRef.current?.send(data);
  }, []);

  // Светящийся Ctrl/Alt/Shift должен действовать на КЛИК и гаснуть после
  // применения — ровно как в sendKey. До этого подсветка врала: горела, но
  // Ctrl+клик и Shift+клик уходили обычными кликами и не гасли никогда (N138).
  // На агенте модификаторов в событии клика нет, поэтому дожимаем их клавишами.
  const withModifiers = useCallback((run: () => void) => {
    const m = modifiersRef.current;
    const active = m.ctrl || m.alt || m.shift;
    if (!active) { run(); return; }
    if (m.ctrl) send({ t: "k", k: "Control", d: true });
    if (m.alt) send({ t: "k", k: "Alt", d: true });
    if (m.shift) send({ t: "k", k: "Shift", d: true });
    run();
    if (m.shift) send({ t: "k", k: "Shift", d: false });
    if (m.alt) send({ t: "k", k: "Alt", d: false });
    if (m.ctrl) send({ t: "k", k: "Control", d: false });
    modifiersRef.current = { ctrl: false, alt: false, shift: false };
    setModifiers({ ctrl: false, alt: false, shift: false });
  }, [send]);

  const sendClientHints = useCallback(() => {
    const transport = transportRef.current;
    if (!transport) return;
    const wrap = wrapRef.current;
    transport.send({
      t: "client",
      vw: Math.round(wrap?.clientWidth || window.innerWidth),
      vh: Math.round(wrap?.clientHeight || window.innerHeight),
      dpr: Math.min(window.devicePixelRatio || 1, 3),
      // Умеем накладывать куски кадра поверх холста: компьютер пришлёт только
      // изменившиеся области вместо всего экрана.
      tiles: true,
      // H.264 запрашиваем ТОЛЬКО подтверждённый браузером: соврать здесь —
      // значит получить поток, который нечем декодировать, то есть чёрный
      // экран вместо картинки. Ответ приходит асинхронно, поэтому просьба
      // уходит вторым сообщением, как только он готов.
    });
    // H.264-путь имеет смысл лишь там, где не поднялся WebRTC: при живом
    // видеотреке кадры и так идут кодеком. Сборка декодера живёт ниже — ей
    // нужны отрисовка и холст, объявленные позже.
    enableWsVideoRef.current?.();
  }, []);

  const sendStreamProfile = useCallback((profile: RemoteProfile) => {
    transportRef.current?.send({ t: "profile", profile });
  }, []);

  // Режим потока выбирают ЯВНО из четырёх подписанных кнопок. Прежняя
  // циклическая пилюля меняла «Авто → Плавно → Чётко → Эконом» по тапу и ни
  // словом не объясняла, чем они отличаются, — а именно этим переключателем
  // лечится «Плохо» в шапке.
  const selectStreamProfile = useCallback((next: RemoteProfile) => {
    haptic("light");
    setNetworkSaverHint("");
    setTrafficHint(false);
    setStreamProfile(next);
    try { localStorage.setItem("tgcontrol.remote.profile", next); } catch {}
    sendStreamProfile(next);
    // Выбор режима — законченное действие: шит закрывается и уступает место
    // кадру, ради которого его и открывали. Раньше он оставался висеть, и
    // разница в картинке была не видна из-под него; соседний «Эконом» из того
    // же шита при этом закрывался — одно и то же нажатие вело себя по-разному.
    setShowQuickActions(false);
    setShowQuality(false);
    toastSuccess(t("remote.profileSet", { name: t(`remote.profile.${next}`) }));
  }, [sendStreamProfile, toastSuccess]);

  const clampOffset = useCallback(() => {
    const w = wrapRef.current, st = stageRef.current;
    if (!w || !st) return;
    const s = scaleRef.current;
    const ww = w.clientWidth, wh = w.clientHeight;
    const cw = st.clientWidth * s, ch = st.clientHeight * s;
    const o = offsetRef.current;
    if (cw <= ww) o.x = 0; else { const m = (cw - ww) / 2; o.x = Math.max(-m, Math.min(m, o.x)); }
    if (ch <= wh) o.y = 0; else { const m = (ch - wh) / 2; o.y = Math.max(-m, Math.min(m, o.y)); }
  }, []);

  // stageContent computes the letterboxed CONTENT box of the remote screen
  // inside the stage, in stage-layout units (pre-transform). Normalised 0..1
  // coordinates refer to the remote screen, not the stage box — when their
  // aspect ratios differ (desktop window, rotation) the letterbox margins
  // would otherwise skew every click.
  const stageContent = useCallback((): { x: number; y: number; w: number; h: number } | null => {
    const st = stageRef.current;
    if (!st) return null;
    const W = st.clientWidth, H = st.clientHeight;
    if (W <= 0 || H <= 0) return null;
    const si = screenRef.current;
    const ratio = si && si.sh > 0 ? si.sw / si.sh : W / H;
    let w = W, h = W / ratio;
    if (h > H) { h = H; w = h * ratio; }
    return { x: (W - w) / 2, y: (H - h) / 2, w, h };
  }, []);

  // Размер кадра — единственный источник правды для геометрии ввода. info от
  // агента может не дойти (ctrl-канал ещё закрыт) или описывать физический
  // дисплей, а не кадр виртуального браузера (его Xvfb под пропорции первого
  // зрителя): тогда screenRef пуст/чужой, фолбэк на пропорции бокса выше
  // зануляет letterbox и каждый тап промахивается на высоту чёрных полей.
  // Кадр, который мы реально рисуем, знает свой размер точно — сверяемся с
  // ним. Обновляем только при смене ПРОПОРЦИЙ: адаптив по сети меняет
  // разрешение кадра, не трогая пропорции, и дёргать состояние не надо.
  const syncScreenFromFrame = useCallback((w: number, h: number) => {
    if (w <= 0 || h <= 0) return;
    const cur = screenRef.current;
    if (cur && cur.sh > 0 && Math.abs(cur.sw / cur.sh - w / h) < 0.005) return;
    const info = { sw: w, sh: h };
    screenRef.current = info;
    setScreenInfo(info);
  }, []);

  const toRelativePoint = useCallback((clientX: number, clientY: number): [number, number] => {
    const st = stageRef.current;
    const box = stageContent();
    if (!st || !box) return [0, 0];
    const r = st.getBoundingClientRect();
    // getBoundingClientRect is post-transform; content box scales uniformly.
    const k = r.width / Math.max(1, st.clientWidth);
    return [
      Math.max(0, Math.min(1, (clientX - r.left - box.x * k) / (box.w * k))),
      Math.max(0, Math.min(1, (clientY - r.top - box.y * k) / (box.h * k))),
    ];
  }, [stageContent]);

  const toRelative = useCallback((touch: React.Touch | Touch): [number, number] =>
    toRelativePoint(touch.clientX, touch.clientY), [toRelativePoint]);

  // updateCursorEl moves the LOCAL DOM cursor. This is the perceived-latency
  // core: the arrow tracks the finger instantly (style.transform, no React
  // re-render, no server round-trip) while the video catches up behind it.
  // Inverse scale keeps the arrow a constant on-screen size under pinch-zoom.
  const updateCursorEl = useCallback(() => {
    const cur = cursorRef.current;
    const box = stageContent();
    if (!cur || !box) return;
    const x = box.x + cursorPos.current.x * box.w;
    const y = box.y + cursorPos.current.y * box.h;
    cur.style.transform = `translate3d(${x}px, ${y}px, 0) scale(${1 / scaleRef.current})`;
  }, [stageContent]);

  // Tap feedback: a ripple at the touch point, instant, purely local.
  const showRipple = useCallback((rx: number, ry: number) => {
    const el = rippleRef.current;
    const box = stageContent();
    if (!el || !box) return;
    el.style.left = `${box.x + rx * box.w}px`;
    el.style.top = `${box.y + ry * box.h}px`;
    const k = 1 / scaleRef.current;
    el.animate(
      [
        { opacity: 0.65, transform: `translate(-50%,-50%) scale(${0.35 * k})` },
        { opacity: 0, transform: `translate(-50%,-50%) scale(${1.3 * k})` },
      ],
      { duration: 320, easing: "ease-out" },
    );
  }, [stageContent]);

  // drawCursor (legacy name, many call sites): now just syncs the DOM cursor.
  const drawCursor = updateCursorEl;

  const applyTransform = useCallback(() => {
    const st = stageRef.current;
    if (!st) return;
    const s = scaleRef.current;
    const { x, y } = offsetRef.current;
    st.style.transform = `translate(${x}px, ${y}px) scale(${s})`;
    // The cursor lives inside the scaled stage — re-apply its inverse scale.
    updateCursorEl();
  }, [updateCursorEl]);

  // keepCursorInView — под увеличением картинка едет за курсором сама: в режиме
  // «Тачпад» палец занят курсором, и панорамировать ему нечем. Сцена
  // масштабируется от центра (transform-origin по умолчанию), offset — сдвиг
  // после масштаба, как в clampOffset.
  const keepCursorInView = useCallback(() => {
    const w = wrapRef.current, st = stageRef.current;
    const box = stageContent();
    const s = scaleRef.current;
    if (!w || !st || !box || s <= 1.02) return;
    const W = st.clientWidth, H = st.clientHeight;
    const cx = box.x + cursorPos.current.x * box.w;
    const cy = box.y + cursorPos.current.y * box.h;
    const o = offsetRef.current;
    const sx = (cx - W / 2) * s + o.x + W / 2;
    const sy = (cy - H / 2) * s + o.y + H / 2;
    const margin = 48;
    const ww = w.clientWidth, wh = w.clientHeight;
    let moved = false;
    if (sx < margin) { o.x += margin - sx; moved = true; } else if (sx > ww - margin) { o.x -= sx - (ww - margin); moved = true; }
    if (sy < margin) { o.y += margin - sy; moved = true; } else if (sy > wh - margin) { o.y -= sy - (wh - margin); moved = true; }
    if (!moved) return;
    clampOffset(); applyTransform();
  }, [applyTransform, clampOffset, stageContent]);

  // get2d — single place for canvas contexts so every call passes the same
  // attributes: only the FIRST getContext applies them. desynchronized lets
  // Chromium skip the compositor queue where supported (best-effort).
  const get2d = useCallback((c: HTMLCanvasElement) =>
    c.getContext("2d", { desynchronized: true } as CanvasRenderingContext2DSettings), []);

  // queueMove coalesces pointer moves to one per animation frame (latest wins)
  // and prefers the unreliable "input" lane when the agent supports it. A
  // 120Hz touchscreen otherwise floods the reliable ctrl channel, and every
  // queued packet is latency added to the drag.
  const queueMove = useCallback((x: number, y: number) => {
    movePendingRef.current = { x, y };
    if (moveRafRef.current != null) return;
    moveRafRef.current = requestAnimationFrame(() => {
      moveRafRef.current = null;
      const p = movePendingRef.current;
      movePendingRef.current = null;
      const tr = transportRef.current;
      if (!p || !tr) return;
      const msg = { t: "m", a: "move", x: p.x, y: p.y, seq: ++moveSeqRef.current };
      if (!(agentCapsRef.current.moveCh && tr.sendMove(msg))) tr.send(msg);
    });
  }, []);

  // queueScroll accumulates fractional wheel notches client-side; the agent
  // truncates to int, so sub-notch deltas (slow two-finger scroll) must add up
  // HERE or they vanish and the scroll moves in dead-zone steps.
  const wheelAccXRef = useRef(0);
  const queueScroll = useCallback((delta: number, deltaX = 0) => {
    if (!Number.isFinite(delta) || !Number.isFinite(deltaX)) return;
    wheelAccRef.current += delta;
    wheelAccXRef.current += deltaX;
    const whole = Math.trunc(wheelAccRef.current);
    const wholeX = Math.trunc(wheelAccXRef.current);
    if (whole === 0 && wholeX === 0) return;
    wheelAccRef.current -= whole;
    wheelAccXRef.current -= wholeX;
    // Ctrl+колесо (масштаб в браузере и редакторе на ПК) — тоже модификатор на
    // жест, а не только на спецклавишу. dx — горизонтальное колесо (с 2.61.18);
    // старый агент поле не знает и игнорирует.
    const msg: Record<string, unknown> = { t: "s", dy: whole };
    if (wholeX !== 0) msg.dx = wholeX;
    withModifiers(() => transportRef.current?.send(msg));
  }, [withModifiers]);

  // ── H.264 video-track rendering ────────────────────────────────
  // The WebRTC track plays DIRECTLY in a visible <video> inside the stage —
  // no per-frame canvas blit (that cost 15–35ms and tied smoothness to React
  // main-thread jank). The canvas remains only for the JPEG fallback and as a
  // scratch surface for screenshots. requestVideoFrameCallback is kept purely
  // for telemetry: decoded-fps for the HUD and {t:"vack"} frame-presented acks
  // that drive the agent's backpressure.
  const videoElRef = useRef<HTMLVideoElement | null>(null);
  const videoActive = useRef(false);

  const stopVideoDraw = useCallback(() => {
    videoActive.current = false;
    setH264Active(false);
    h264ActiveRef.current = false;
    if (h264WatchdogRef.current) {
      window.clearTimeout(h264WatchdogRef.current);
      h264WatchdogRef.current = null;
    }
    const v = videoElRef.current;
    if (v) { try { v.pause(); } catch { /* ignore */ } v.srcObject = null; }
  }, []);

  // ── Звук виртуального браузера (Opus-трек) ─────────────────────
  // Приходит отдельным MediaStream'ом (свой msid у трека) и играется скрытым
  // <audio>: <video> занят видеотреком и имеет muted ради автоплея, а
  // объединять стримы поздно — ontrack уже раздал их по отдельности.
  const audioElRef = useRef<HTMLAudioElement | null>(null);
  const [hasAudio, setHasAudio] = useState(false);
  const [soundMuted, setSoundMuted] = useState(false);

  const stopAudio = useCallback(() => {
    setHasAudio(false);
    setSoundMuted(false);
    const a = audioElRef.current;
    if (a) { try { a.pause(); } catch { /* ignore */ } a.srcObject = null; }
  }, []);

  const startAudio = useCallback((stream: MediaStream) => {
    const a = audioElRef.current;
    if (!a) return;
    a.srcObject = stream;
    setHasAudio(true);
    // Автоплей СО звуком WebView вправе придержать до жеста: тогда первый же
    // тап по экрану докликивает play() (одноразовый слушатель на отказ).
    a.play().catch(() => {
      document.addEventListener("pointerdown", () => {
        a.play().catch(() => { /* останется кнопка-динамик */ });
      }, { once: true });
    });
  }, []);

  // Динамик: один переключатель и для шапки (десктоп), и для меню ⋮ нижней
  // панели браузера — логика одна, мест кнопке два.
  const toggleSound = useCallback(() => {
    haptic("light");
    const next = !soundMuted;
    setSoundMuted(next);
    const a = audioElRef.current;
    if (a) { a.muted = next; if (!next) a.play().catch(() => { /* ignore */ }); }
  }, [soundMuted]);

  // Вотчдог чёрного экрана (#53, уточнён в N136). Если декодер H.264 не собрался
  // (нет аппаратной поддержки профиля, кривой WebView), поток «идёт», HUD пишет
  // H264, курсор рисуется — а картинки нет и выхода из этого состояния тоже.
  //
  // Считаем ТОЛЬКО от готового соединения (pc.connectionState === "connected") и
  // даём 8 секунд — столько же, сколько сам транспорт отводит на открытие
  // ctrl-канала. Прежний отсчёт стартовал в момент ontrack, то есть ДО
  // завершения ICE/DTLS, и на медленном LTE через TURN срабатывал ложно: ПК
  // навсегда получал ярлык «только JPEG».
  const armH264Watchdog = useCallback(() => {
    if (!videoActive.current || !rtcConnectedRef.current) return;
    const presentedAtStart = presentedRef.current;
    if (h264WatchdogRef.current) window.clearTimeout(h264WatchdogRef.current);
    h264WatchdogRef.current = window.setTimeout(() => {
      h264WatchdogRef.current = null;
      if (!videoActive.current) return;
      const el = videoElRef.current;
      const noFrames = presentedRef.current === presentedAtStart;
      const noSize = !el || el.videoWidth === 0;
      if (noFrames && noSize) {
        setVideoStuck(true);
        preferH264Ref.current = false;
        writeJpegFallbackMark();
        setJpegForced(true);
        transportRef.current?.send({ t: "novideo" });
      }
    }, 8000);
  }, []);

  const startVideoDraw = useCallback((stream: MediaStream) => {
    const v = videoElRef.current;
    if (!v) return;
    videoActive.current = true;
    setH264Active(true);
    h264ActiveRef.current = true;
    // Заработал WebRTC-трек — второй видеопуть закрываем. Мало закрыть декодер
    // у себя: компьютер продолжил бы кодировать и слать кадры в сокет, то есть
    // одна и та же картинка ехала бы дважды и за двойной трафик.
    if (wsVideoRef.current) {
      wsVideoRef.current.close();
      wsVideoRef.current = null;
      transportRef.current?.send({ t: "client", h264: false });
    }
    v.srcObject = stream;
    v.play().catch(() => { /* autoplay may defer; muted+playsinline retries */ });

    const rvfc = (v as any).requestVideoFrameCallback?.bind(v);
    if (rvfc) {
      const cb = () => {
        if (!videoActive.current) return;
        markFirstFrame();
        presentedRef.current++;
        syncScreenFromFrame(v.videoWidth, v.videoHeight);
        // H.264 на этом устройстве всё-таки работает — снимаем отметку
        // «только JPEG», чтобы прошлое неудачное рукопожатие не тянулось сутки.
        if (presentedRef.current === 1) {
          if (h264WatchdogRef.current) {
            window.clearTimeout(h264WatchdogRef.current);
            h264WatchdogRef.current = null;
          }
          clearJpegFallbackMark();
          setJpegForced(false);
        }
        if (agentCapsRef.current.vack) {
          transportRef.current?.send({ t: "vack", n: presentedRef.current });
        }
        fpsCounter.current.frames++;
        const now = Date.now();
        if (now - fpsCounter.current.lastTime >= 1000) {
          setFps(fpsCounter.current.frames);
          fpsCounter.current.frames = 0;
          fpsCounter.current.lastTime = now;
        }
        rvfc(cb);
      };
      rvfc(cb);
    }
    // No rvfc (older WebView): no vack (agent then runs без backpressure) and
    // the HUD fps comes from the getStats collector instead.

    // ontrack приходит раньше готового ICE — вотчдог взведётся сам, когда
    // соединение станет "connected" (или прямо сейчас, если уже стало).
    armH264Watchdog();
  }, [armH264Watchdog, markFirstFrame, syncScreenFromFrame]);

  // ── Inertia ────────────────────────────────────────────────────

  const stopInertia = useCallback(() => {
    if (inertiaRAF.current) {
      cancelAnimationFrame(inertiaRAF.current);
      inertiaRAF.current = null;
    }
  }, []);

  const startInertia = useCallback(() => {
    stopInertia();
    const friction = 0.92;
    const minSpeed = 0.0003;
    const animate = () => {
      const { vx, vy } = inertiaRef.current;
      if (Math.abs(vx) < minSpeed && Math.abs(vy) < minSpeed) {
        inertiaRAF.current = null;
        return;
      }
      const cp = cursorPos.current;
      cp.x = Math.max(0, Math.min(1, cp.x + vx));
      cp.y = Math.max(0, Math.min(1, cp.y + vy));
      queueMove(cp.x, cp.y);
      drawCursor();
      inertiaRef.current.vx *= friction;
      inertiaRef.current.vy *= friction;
      inertiaRAF.current = requestAnimationFrame(animate);
    };
    inertiaRAF.current = requestAnimationFrame(animate);
  }, [stopInertia, queueMove, drawCursor]);

  const ackFrame = useCallback((seq: number) => {
    const transport = transportRef.current;
    if (!transport) return;
    const dropped = droppedFrames.current;
    droppedFrames.current = 0;
    if (dropped > 0) setDecodeDrops((value) => value + dropped);
    transport.send({ t: "ack", seq, dropped });
  }, []);

  /**
   * Частичный кадр: компьютер прислал только изменившиеся области, кладём их
   * поверх того, что уже нарисовано. Холст при этом не трогаем — его размер
   * задаёт полный кадр, а куски приходят в его координатах.
   */
  /**
   * Видеокадр H.264 из веб-сокета. Декодер отдаёт VideoFrame, его рисуем на
   * тот же холст и НЕМЕДЛЕННО закрываем: кадр держит буфер декодера, и пара
   * незакрытых кадров вешает декодирование целиком.
   */
  const drawVideoFrame = useCallback((frame: Extract<ParsedFrame, { kind: "h264" }>) => {
    const dec = wsVideoRef.current;
    if (!dec) return;
    if (!dec.ready) return;
    if (dec.needsKeyframe && !frame.keyframe) {
      // Опоры нет — просим компьютер прислать ключевой кадр и ждём его.
      transportRef.current?.send({ t: "idr" });
      return;
    }
    if (!wsVideoStartRef.current) wsVideoStartRef.current = performance.now();
    const ts = Math.round((performance.now() - wsVideoStartRef.current) * 1000);
    if (dec.decode(frame.data, frame.keyframe, ts)) {
      lastRenderedSeq.current = frame.seq;
      ackFrame(frame.seq);
    }
  }, [ackFrame]);

  /**
   * Спросить браузер, умеет ли он декодировать наш H.264, и если да — сказать
   * об этом компьютеру и собрать декодер. Врать здесь нельзя: поток, который
   * нечем декодировать, — это чёрный экран вместо картинки.
   */
  const enableWsVideo = useCallback(() => {
    if (h264ActiveRef.current || wsVideoRef.current) return;
    void canDecodeWsH264().then((can) => {
      if (!can || h264ActiveRef.current || wsVideoRef.current) return;
      wsVideoRef.current = new WsVideoDecoder({
        onFrame: (frame) => {
          const c = canvasRef.current;
          const ctx = c ? get2d(c) : null;
          if (!c || !ctx) { frame.close(); return; }
          if (c.width !== frame.displayWidth || c.height !== frame.displayHeight) {
            c.width = frame.displayWidth;
            c.height = frame.displayHeight;
            syncScreenFromFrame(frame.displayWidth, frame.displayHeight);
          }
          ctx.drawImage(frame, 0, 0);
          frame.close(); // держит буфер декодера — закрывать сразу
          markFirstFrame();
          drawCursor();
          fpsCounter.current.frames++;
          const now = Date.now();
          if (now - fpsCounter.current.lastTime >= 1000) {
            setFps(fpsCounter.current.frames);
            fpsCounter.current.frames = 0;
            fpsCounter.current.lastTime = now;
          }
        },
        onError: () => {
          wsVideoRef.current?.configure();
          transportRef.current?.send({ t: "idr" });
        },
      });
      wsVideoRef.current.configure();
      transportRef.current?.send({ t: "client", h264: true });
    });
  }, [drawCursor, get2d, markFirstFrame, syncScreenFromFrame]);
  enableWsVideoRef.current = enableWsVideo;

  const drawRegionFrame = useCallback((frame: Extract<ParsedFrame, { kind: "regions" }>) => {
    const c = canvasRef.current;
    const ctx = c ? get2d(c) : null;
    // Ни одного полного кадра ещё не было — класть куски не на что. Компьютер
    // пришлёт целый кадр по keep-alive, тогда и начнём.
    if (!c || !ctx || !c.width || !firstFrameRef.current) return;

    decodingFrame.current = true;
    Promise.all(frame.regions.map((r) =>
      createImageBitmap(new Blob([r.data], { type: "image/jpeg" })).then((bmp) => ({ r, bmp })),
    ))
      .then((parts) => {
        if (frame.seq <= lastRenderedSeq.current) {
          parts.forEach((p) => p.bmp.close());
          return;
        }
        paintRegions(ctx, parts.map(({ r, bmp }) => ({ bitmap: bmp, x: r.x, y: r.y })));
        lastRenderedSeq.current = frame.seq;
        drawCursor();
        ackFrame(frame.seq);

        fpsCounter.current.frames++;
        const now = Date.now();
        if (now - fpsCounter.current.lastTime >= 1000) {
          setFps(fpsCounter.current.frames);
          fpsCounter.current.frames = 0;
          fpsCounter.current.lastTime = now;
        }
      })
      .catch(() => {
        droppedFrames.current++;
      })
      .finally(() => {
        decodingFrame.current = false;
        const next = pendingFrame.current;
        pendingFrame.current = null;
        if (next) decodeQueuedFrameRef.current?.(next);
      });
  }, [ackFrame, drawCursor, get2d]);

  const decodeQueuedFrame = useCallback((frame: QueuedFrame) => {
    if (frame.seq <= lastRenderedSeq.current) return;
    if (frame.kind === "audio") {
      audioPlayerRef.current?.push(frame.data);
      return;
    }
    if (frame.kind === "h264") {
      drawVideoFrame(frame);
      return;
    }
    if (frame.kind === "regions") {
      drawRegionFrame(frame);
      return;
    }
    decodingFrame.current = true;
    const blob = new Blob([frame.data], { type: "image/jpeg" });
    createImageBitmap(blob)
      .then((bmp) => {
        if (frame.seq <= lastRenderedSeq.current) {
          bmp.close();
          return;
        }
        const c = canvasRef.current;
        const ctx = c ? get2d(c) : null;
        if (!c || !ctx) {
          bmp.close();
          return;
        }
        if (c.width !== bmp.width || c.height !== bmp.height) {
          c.width = bmp.width;
          c.height = bmp.height;
        }
        syncScreenFromFrame(bmp.width, bmp.height);
        if (lastFrame.current) lastFrame.current.close();
        lastFrame.current = bmp;
        lastRenderedSeq.current = frame.seq;
        ctx.drawImage(bmp, 0, 0);
        markFirstFrame();
        drawCursor();
        ackFrame(frame.seq);

        fpsCounter.current.frames++;
        const now = Date.now();
        if (now - fpsCounter.current.lastTime >= 1000) {
          setFps(fpsCounter.current.frames);
          fpsCounter.current.frames = 0;
          fpsCounter.current.lastTime = now;
        }
      })
      .catch(() => {
        droppedFrames.current++;
      })
      .finally(() => {
        decodingFrame.current = false;
        const next = pendingFrame.current;
        pendingFrame.current = null;
        if (next) decodeQueuedFrame(next);
      });
  }, [ackFrame, drawCursor, drawRegionFrame, drawVideoFrame, get2d, markFirstFrame, syncScreenFromFrame]);

  // Ссылка на самый свежий декодер: очередь кадров внутри drawRegionFrame
  // вызывает его же, а замыкание на useCallback тут родилось бы циклом.
  const decodeQueuedFrameRef = useRef<((frame: QueuedFrame) => void) | null>(null);
  decodeQueuedFrameRef.current = decodeQueuedFrame;

  const queueFrame = useCallback((buffer: ArrayBuffer) => {
    const frame = parseScreenFrame(buffer);
    if (!frame) return;
    if (frame.seq <= lastRenderedSeq.current) return;
    if (decodingFrame.current) {
      if (pendingFrame.current) droppedFrames.current++;
      pendingFrame.current = frame;
      return;
    }
    decodeQueuedFrame(frame);
  }, [decodeQueuedFrame]);

  // ── WebSocket connection + auto-reconnect ──────────────────────

  useEffect(() => {
    // ГЛАВНОЕ ПРАВИЛО ЭКРАНА: пока человек не нажал «Включить экран», отсюда
    // не уходит ни одного соединения — ни WebRTC, ни ws, ни предпросмотра.
    if (!streaming) return;
    let attempt = 0;
    let reconnTimer: ReturnType<typeof setTimeout> | null = null;
    let previewAbort: AbortController | null = null;
    let disposed = false;
    let fatalError = false;
    // Последняя внятная причина отказа: её показываем на экране запуска, когда
    // лестница переподключений кончилась.
    let lastError = "";

    const loadPreview = () => {
      previewAbort?.abort();
      const controller = new AbortController();
      previewAbort = controller;
      remotePreviewBlob(640, controller.signal)
        .then((blob) => createImageBitmap(blob))
        .then((bmp) => {
          if (disposed || firstFrameRef.current) { bmp.close(); return; }
          const canvas = canvasRef.current;
          const ctx = canvas ? get2d(canvas) : null;
          if (!canvas || !ctx) { bmp.close(); return; }
          canvas.width = bmp.width;
          canvas.height = bmp.height;
          if (lastFrame.current) lastFrame.current.close();
          lastFrame.current = bmp;
          ctx.drawImage(bmp, 0, 0);
          const info = { sw: bmp.width, sh: bmp.height };
          setScreenInfo(info);
          screenRef.current = info;
          setPreviewReady(true);
        })
        .catch(() => { /* preview is best-effort; transport carries the real error */ });
    };

    const handleJSON = (msg: any) => {
      switch (msg.t) {
        case "info": {
          const i = { sw: msg.sw, sh: msg.sh };
          setScreenInfo(i);
          screenRef.current = i;
          if (Array.isArray(msg.displays)) {
            const nextDisplays = msg.displays as Display[];
            setDisplays(nextDisplays);
            // Запоминаем на будущее: экран запуска назовёт число мониторов ещё
            // до подключения, не спрашивая ПК лишний раз.
            if (nextDisplays.length > 0) {
              setKnownDisplays(nextDisplays.length);
              writeKnownDisplays(nextDisplays.length);
            }
            if (!nextDisplays.some((display) => display.id === activeDisplayRef.current)) {
              activeDisplayRef.current = 0;
              setActiveDisplay(0);
              transportRef.current?.send({ t: "display", id: 0 });
            }
          }
          // Agent capability flags (2.17+); absent on older agents → all off.
          agentCapsRef.current = {
            moveCh: !!msg.moveCh, cursorPos: !!msg.cursorPos, vack: !!msg.vack,
          };
          break;
        }
        case "cursor":
          // Host cursor echo — the PC's pointer moved without our input
          // (physical mouse, an app warping it, a second viewer). While we
          // drag locally our own cursor is the truth, so skip then.
          if (dragRef.current.state !== "dragging" &&
              typeof msg.x === "number" && typeof msg.y === "number") {
            cursorPos.current = { x: msg.x, y: msg.y };
            updateCursorEl();
          }
          break;
        case "audio": {
          // Ответ компьютера на просьбу о звуке: он либо начал слать куски,
          // либо честно сказал, что не умеет (Linux/мак пока не умеют).
          // Срок ожидания снимаем БЕЗУСЛОВНО и первой строкой: оставленный в
          // успешной ветке таймер через семь секунд выключил бы уже играющий
          // звук — регресс хуже молчащей кнопки.
          if (audioWaitRef.current) {
            window.clearTimeout(audioWaitRef.current);
            audioWaitRef.current = null;
          }
          if (msg.available === false) {
            setAudioState("unavailable");
            void audioPlayerRef.current?.stop();
            audioPlayerRef.current = null;
            break;
          }
          const rate = typeof msg.rate === "number" ? msg.rate : 24000;
          void audioPlayerRef.current?.start(rate).then(() => setAudioState("on"));
          break;
        }
        case "stats":
          setStats({
            rtt: msg.rtt || 0,
            fps: msg.fps || 0,
            sentFps: msg.sentFps || 0,
            quality: msg.quality || 0,
            width: msg.width || 0,
            kbps: msg.kbps || 0,
            skipped: msg.skipped || 0,
            captureMs: msg.captureMs || 0,
            encodeMs: msg.encodeMs || 0,
            profile: remoteProfiles.includes(msg.profile) ? msg.profile : streamProfileRef.current,
          });
          break;
        case "clip":
          setClipboardText(msg.text || "");
          setClipboardLoading(false);
          setClipboardError("");
          setShowClipboard(true);
          if (msg.empty) toast(t("remote.emptyClipboard"));
          else toastSuccess(t("remote.clipReadDone"));
          break;
        case "clip_result":
          toastSuccess(msg.op === "set_image"
            ? t("remote.clipImageSent")
            : t("remote.clipTextSent"));
          break;
        case "clip_error":
          // Причину пишем ВНУТРЬ панели: закрывать из-за неё дверь «телефон →
          // ПК» нельзя — отправка текста и картинки к чтению буфера ПК
          // отношения не имеет.
          setClipboardLoading(false);
          setClipboardError(clipboardErrorMessage(msg.code || ""));
          setShowClipboard(true);
          toastError(clipboardErrorMessage(msg.code || ""));
          break;
        case "video_mode":
          if (msg.mode === "jpeg") {
            stopVideoDraw();
            setVideoStuck(false);
            setConnectionStage("waiting");
            toast(t("remote.compatModeOn"));
          }
          break;
        case "pong":
          break;
        // События самой страницы. Главное из них — фокус: в настоящем браузере
        // клавиатура выезжает по тапу в поле, а не по отдельной кнопке.
        case "browser":
          if (msg.kind === "focus") {
            if (msg.editable) setShowKb(true);
          } else if (msg.kind === "scroll") {
            // Где стоит страница. Нужно жестам: «потянуть вниз, чтобы
            // обновить» работает только у самого верха, а спрашивать об этом в
            // момент жеста — лишние сотни миллисекунд через облако.
            pageScrollRef.current = Number(msg.y) || 0;
          } else if (msg.kind === "download" && msg.file) {
            toast(t("remote.browserDownloaded", { file: String(msg.file) }));
            void refreshBrowserPage();
          } else if (msg.kind === "file_chooser" && Number(msg.id) > 0) {
            // Chrome подавил своё системное окно: показываем видимый picker
            // Remotai. Клавиатура и другие шиты не должны лежать поверх него.
            setShowKb(false);
            setAddressOpen(false);
            setTabsOpen(false);
            setBrowserMenu(false);
            setDownloadsOpen(false);
            setBrowserFileError("");
            setBrowserFileProgress(0);
            setBrowserFileChooser({ id: Number(msg.id), multiple: !!msg.multiple });
          } else if (msg.kind === "file_chooser_closed") {
            setBrowserFileChooser((current) => (
              !current || !msg.id || current.id === Number(msg.id) ? null : current
            ));
            setBrowserFileUploading(false);
            setBrowserFileProgress(0);
            setBrowserFileError("");
          } else if (msg.kind === "dialog" && Number(msg.id) > 0) {
            setShowKb(false);
            setAddressOpen(false);
            setTabsOpen(false);
            setBrowserMenu(false);
            setDownloadsOpen(false);
            const next: BrowserDialog = {
              id: Number(msg.id),
              type: String(msg.dialog_type || "alert"),
              message: String(msg.message || ""),
              defaultPrompt: String(msg.default_prompt || ""),
              url: String(msg.url || ""),
            };
            setBrowserDialog(next);
            setBrowserDialogPrompt(next.defaultPrompt);
            setBrowserDialogBusy(false);
          } else if (msg.kind === "dialog_closed") {
            setBrowserDialog((current) => (
              !current || !msg.id || current.id === Number(msg.id) ? null : current
            ));
            setBrowserDialogBusy(false);
          } else if (msg.kind === "popup") {
            // target=_blank/window.open создали реальную вкладку браузера.
            // Page.windowOpen приходит чуть РАНЬШЕ появления нового target:
            // обновляем сразу и ещё раз после короткой задержки, иначе вкладка
            // существует, но счётчик иногда остаётся прежним.
            void refreshBrowserTabs();
            window.setTimeout(() => { void refreshBrowserTabs(); }, 350);
          } else if (msg.kind === "idle_warning") {
            // Браузер скоро погасят по простою: баннер с кнопкой «Оставить
            // включённым» плюс тост — баннер может быть перекрыт другой
            // плашкой, а предупреждение человек не должен пропустить.
            const left = typeof msg.seconds_left === "number" ? msg.seconds_left : 300;
            setVbIdleWarn(left);
            toast(t("remote.vbIdleWarn", { min: Math.max(1, Math.ceil(left / 60)) }));
          } else if (msg.kind === "idle_stopped") {
            // Агент сам погасил браузер: кадру больше нечего показывать.
            // Возвращаем панель запуска с причиной — как после «Остановить
            // браузер», только без ухода с экрана.
            setVbIdleWarn(0);
            setVbRunning(false);
            setVbDirect(false);
            setVbHint("");
            setVbError(t("remote.vbIdleStopped"));
            setVbGate("needed");
            setStreaming(false);
          } else {
            void refreshBrowserPage();
          }
          break;
        // Ввод не доходит до компьютера или вводить нечем: агент шлёт это по
        // control-каналу и сам же снимает предупреждение кодом input_ok (N1).
        case "warn":
          if (msg.code === "input_ok") {
            setInputWarn("");
          } else if (msg.code === "input_blocked" || msg.code === "input_unavailable") {
            setInputWarn(msg.code);
          }
          break;
        case "error":
          // Захват экрана падает: соединение живое, агент повторяет попытки —
          // рвать его нельзя, иначе человек получит лестницу реконнектов вместо
          // причины. Плашка снимется сама, как только придёт кадр.
          if (msg.code === "capture_failed") {
            captureFailedRef.current = true;
            setCaptureFailed(true);
            break;
          }
          fatalError = true;
          lastError = remoteErrorMessage({ code: msg.code, message: msg.msg });
          setConnectionError(lastError);
          transportRef.current?.close();
          break;
      }
    };

    function connect() {
      if (disposed) return;
      fatalError = false;
      haltReconnectRef.current = false;
      setPcOffline(false);
      firstFrameRef.current = false;
      connectionStartedRef.current = Date.now();
      setConnectSeconds(0);
      setFirstFrame(false);
      setPreviewReady(false);
      setConnectionStage("route");
      setVideoStuck(false);
      setTrafficBytes(0);
      presentedRef.current = 0; // vack counter is per-connection on the agent
      rtcConnectedRef.current = false;
      captureFailedRef.current = false;
      setCaptureFailed(false);
      setInputWarn("");
      setVbIdleWarn(0);
      setConnectionError("");
      stopAudio(); // прошлый трек умер вместе с PeerConnection
      loadPreview();
      // Tries WebRTC (UDP/DataChannels) first, falls back to the WS tunnel.
      connRef.current = connectRemote({
        onFrame: (buf) => {
          setTrafficBytes((value) => value + buf.byteLength);
          queueFrame(buf);
        },
        onJSON: handleJSON,
        onVideoTrack: (stream) => { if (!disposed) startVideoDraw(stream); },
        onAudioTrack: (stream) => { if (!disposed) startAudio(stream); },
        onStage: (stage) => { if (!disposed) setConnectionStage(stage); },
        onRtcState: (state) => {
          if (disposed) return;
          const wasConnected = rtcConnectedRef.current;
          rtcConnectedRef.current = state === "connected";
          // Соединение состоялось — вот теперь честно считать секунды без кадров.
          if (!wasConnected && rtcConnectedRef.current) armH264Watchdog();
        },
        onClientStats: (s) => {
          if (disposed) return;
          setCstats(s);
          if (typeof s.receivedBytes === "number") setTrafficBytes(s.receivedBytes);
          // Mirror to the agent: lands in GET /api/diag/connections so latency
          // decomposes from the PC side without touching the phone.
          transportRef.current?.send({
            t: "cstats", jb: s.jbMs, dec: s.decMs, loss: s.lossPct,
            rtt: s.rtt, route: s.route, jbt: s.jbTarget, rfps: s.recvFps,
          });
        },
        onOpen: (transport) => {
          if (disposed) { transport.close(); return; }
          transportRef.current = transport;
          setTransportKind(transport.kind);
          if (transport.kind === "ws") stopVideoDraw(); // WS path has no video track
          setConnected(true);
          setConnectionStage("waiting");
          setReconnecting(false);
          setReconnectNum(0);
          setConnectionError("");
          // Канал открылся — значит ПК доступен, даже если поток событий ещё
          // держал старое agent_status=offline.
          pcOfflineRef.current = false;
          haltReconnectRef.current = false;
          setPcOffline(false);
          attempt = 0;
          decodingFrame.current = false;
          pendingFrame.current = null;
          droppedFrames.current = 0;
          lastRenderedSeq.current = 0;
          wheelAccRef.current = 0;
          movePendingRef.current = null;
          setDecodeDrops(0);
          setCstats(null);
          resetHideTimer();
          sendStreamProfile(streamProfileRef.current);
          sendClientHints();
          // Restore cursor position after (re)connect (reliable lane, seq'd).
          const cp = cursorPos.current;
          transport.send({ t: "m", x: cp.x, y: cp.y, a: "move", seq: ++moveSeqRef.current });
          // Reconnect must restore the monitor whose button remains highlighted.
          transport.send({ t: "display", id: activeDisplayRef.current });
          if (backgroundedRef.current) transport.send({ t: "pause" });
        },
        onClose: () => {
          transportRef.current = null;
          stopVideoDraw();
          stopAudio();
          setConnected(false);
          if (fatalError || haltReconnectRef.current) { setReconnecting(false); return; }
          if (backgroundedRef.current) {
            releasedInBg.current = true;
            setReconnecting(false);
            return;
          }
          // Поток событий уже знает, что ПК не в сети (кадр agent_status) —
          // лестница из 10 попыток его не разбудит.
          if (!disposed && pcOfflineRef.current) { markPcOffline(); return; }
          if (!disposed && attempt < maxReconnect) {
            setReconnecting(true);
            setReconnectNum(attempt + 1);
            // Full Jitter (AWS): random(0, min(cap, base·2^attempt)) — гасит
            // thundering herd, когда все клиенты отваливаются разом при рестарте
            // сервера и иначе реконнектятся синхронно.
            const ceil = Math.min(1000 * Math.pow(2, attempt), 15000);
            const delay = Math.random() * ceil;
            reconnTimer = setTimeout(() => { attempt++; connect(); }, delay);
          } else if (!disposed) {
            // Попытки кончились. Раньше здесь оставался чёрный экран с
            // «Соединение закрыто» и одинокой кнопкой «Повторить» — теперь
            // возвращаемся на экран запуска: там и причина, и «Включить снова».
            stopStream(lastError || t("remote.disconnected"));
          }
        },
        onError: (e) => {
          if (disposed) return;
          // Голый Event от WebSocket не несёт ни статуса, ни кода — но если поток
          // событий уже сказал «ПК не в сети», молчать об этом нельзя.
          if (isRemoteOffline(e) || pcOfflineRef.current) {
            fatalError = true;
            markPcOffline();
            return;
          }
          if (fatalRemoteError(e)) fatalError = true;
          lastError = remoteErrorMessage(e);
          setConnectionError(lastError);
        },
      }, { preferH264: preferH264Ref.current });
    }

    connect();

    return () => {
      disposed = true;
      if (reconnTimer) clearTimeout(reconnTimer);
      previewAbort?.abort();
      if (hideTimer.current) clearTimeout(hideTimer.current);
      if (moveRafRef.current != null) { cancelAnimationFrame(moveRafRef.current); moveRafRef.current = null; }
      connRef.current?.close();
      transportRef.current = null;
      stopVideoDraw(); // the <video> itself is JSX-owned, only detach the stream
      stopAudio();     // то же со скрытым <audio>
      if (lastFrame.current) { lastFrame.current.close(); lastFrame.current = null; }
      // Звук держит AudioContext — закрываем вместе с потоком.
      void audioPlayerRef.current?.stop();
      audioPlayerRef.current = null;
      // Вместе с потоком снимаем и срок ожидания ответа про звук: иначе он
      // выстрелит уже на другом экране и соврёт «компьютер не ответил».
      if (audioWaitRef.current) { window.clearTimeout(audioWaitRef.current); audioWaitRef.current = null; }
      // Декодер держит буферы браузера — отпускаем вместе с потоком.
      wsVideoRef.current?.close();
      wsVideoRef.current = null;
      wsVideoStartRef.current = 0;
    };
  }, [
    armH264Watchdog, get2d, markPcOffline, queueFrame, resetHideTimer, retryKey,
    sendClientHints, sendStreamProfile, startAudio, startVideoDraw, stopAudio,
    stopVideoDraw, stopStream,
    streaming, toast, toastError, toastSuccess, updateCursorEl,
  ]);

  // restartRemote — переподключение без тактильного отклика: им пользуется и
  // автоматика (ПК вернулся в сеть, возврат из фона), где вибрация ни при чём.
  const restartRemote = useCallback(() => {
    setConnectionError("");
    setPcOffline(false);
    haltReconnectRef.current = false;
    setReconnecting(false);
    setReconnectNum(0);
    setRetryKey((v) => v + 1);
  }, []);

  const retryRemote = useCallback(() => {
    haptic("light");
    restartRemote();
  }, [restartRemote]);

  // Правда о компьютере приходит мгновенно кадром agent_status в поток событий:
  // глобальный баннер уже пишет «Компьютер не в сети», пока удалёнка крутила бы
  // лестницу «Переподключение… (7/10)». Подхватываем её здесь — и когда ПК
  // возвращается, сами поднимаем экран (баннер это и обещает: «включите его —
  // Remotai подключится сам»).
  useEffect(() => onConnectionChange((state) => {
    // Экран запуска пишет состояние связи этой же правдой — но обвинять машину
    // имеет право ТОЛЬКО при reason === "pc_offline". Всё прочее («net»: сеть
    // телефона, релей, замёрзший в фоне сокет) — это «проверяем», а не «ваш
    // компьютер умер»: в метро человек читал именно вторую версию.
    setLinkState(
      state.connected ? "online" : state.reason === "pc_offline" ? "offline" : "checking",
    );
    const firstCall = !linkSeenRef.current;
    linkSeenRef.current = true;
    // Обрыв без названной причины — не всегда «мы виноваты». В LAN и в окне exe
    // канал событий идёт ПРЯМО к машине и при её выключении просто умирает:
    // «pc_offline» там не приходит никогда, и без разбора экран навсегда завис
    // бы на «Проверяем связь…» у честно выключенного компьютера. Разбираем
    // REST-ом: прямой сетевой отказ при живом интернете телефона — это и есть
    // выключенный ПК (isPcOffline), всё прочее остаётся «проверяем».
    if (!firstCall && !state.connected && state.reason !== "pc_offline" && !streamingRef.current) {
      setStatusKey((value) => value + 1);
    }
    // Стрим выключен — поднимать его сами не имеем права: он включается рукой.
    if (!streamingRef.current) return;
    if (state.reason === "pc_offline") {
      pcOfflineRef.current = true;
      // Пока кадры идут, экран живой — не рушим его из-за одного кадра статуса;
      // если поток и правда умер, честное состояние поставит onClose.
      if (!firstFrameRef.current) markPcOffline();
      return;
    }
    if (state.connected) {
      const wasOffline = pcOfflineRef.current;
      pcOfflineRef.current = false;
      if (wasOffline && !firstFrameRef.current) restartRemote();
    }
  }), [markPcOffline, restartRemote]);

  // Hidden means immediate capture pause on the agent. After a full minute the
  // transport is released as well; returning resumes the live session or opens
  // a fresh one without letting background reconnect timers burn traffic.
  useEffect(() => {
    const onVis = () => {
      // Стрима нет — ни ставить на паузу, ни поднимать нечего.
      if (!streamingRef.current) return;
      if (document.visibilityState === "visible") {
        backgroundedRef.current = false;
        if (bgReleaseTimer.current) {
          window.clearTimeout(bgReleaseTimer.current);
          bgReleaseTimer.current = null;
        }
        // Вернулись: либо мы сами отпустили стрим, либо мобильный WebView
        // заморозил сокет и onclose не пришёл — в обоих случаях поднимаем.
        if (releasedInBg.current || !connected) {
          releasedInBg.current = false;
          restartRemote();
        } else {
          transportRef.current?.send({ t: "resume" });
        }
        return;
      }
      backgroundedRef.current = true;
      transportRef.current?.send({ t: "pause" });
      if (bgReleaseTimer.current) window.clearTimeout(bgReleaseTimer.current);
      bgReleaseTimer.current = window.setTimeout(() => {
        bgReleaseTimer.current = null;
        if (document.visibilityState === "visible") return;
        releasedInBg.current = true;
        transportRef.current?.close();
      }, 60000);
    };
    document.addEventListener("visibilitychange", onVis);
    return () => {
      document.removeEventListener("visibilitychange", onVis);
      if (bgReleaseTimer.current) window.clearTimeout(bgReleaseTimer.current);
    };
  }, [connected, restartRemote]);

  useEffect(() => {
    const wrap = wrapRef.current;
    if (!wrap) return;
    const resize = () => sendClientHints();
    const observer = new ResizeObserver(resize);
    observer.observe(wrap);
    window.addEventListener("orientationchange", resize);
    window.addEventListener("resize", resize);
    resize();
    return () => {
      observer.disconnect();
      window.removeEventListener("orientationchange", resize);
      window.removeEventListener("resize", resize);
    };
  }, [sendClientHints]);

  // ── Keyboard ───────────────────────────────────────────────────

  // Клавиши, по которым МЫ отправили keydown: keyup обязан уйти по любой из
  // них, иначе модификатор или буква залипают на ПК навсегда.
  const fwdKeysRef = useRef<Set<string>>(new Set());

  // На ноутбуке и в окне exe физическая клавиатура работает БЕЗ режима ⌨:
  // нажатия уходят на ПК, пока фокус на экране (или ни на чём) и не открыт ни
  // один шит. Мобильная строка ввода остаётся для телефонов — там физической
  // клавиатуры нет, а экранную иначе не поднять (N148).
  const overlayOpen = showQuickActions || showClipboard || showQuality || helpOpen;
  // На экране запуска клавиши принадлежат самому интерфейсу: перехватывать их
  // (и глушить preventDefault'ом) там нечем и незачем — стрима ещё нет.
  const directKeyboard = physicalKeyboard && streaming && !overlayOpen && vbGate !== "needed";

  useEffect(() => {
    const h = (e: KeyboardEvent) => {
      const target = e.target as HTMLElement | null;
      const inTyping = !!target?.classList?.contains("remote-typing-input");
      // Печатают в поле самого интерфейса (буфер ПК, переименование, поиск) —
      // это не ввод для удалённого компьютера.
      if (!inTyping && target && (
        target.tagName === "INPUT" || target.tagName === "TEXTAREA" || target.isContentEditable
      )) return;
      // Прямой форвард — только когда фокус на экране либо ни на чём: иначе
      // Tab и стрелки нужны самому интерфейсу.
      const focused = document.activeElement;
      const onStage = !focused || focused === document.body || focused === stageRef.current;
      if (!showKb && !(directKeyboard && onStage)) return;
      const down = e.type === "keydown";
      const combo = e.ctrlKey || e.altKey || e.metaKey;
      // Горячие клавиши БРАУЗЕРА обслуживаем сами: Ctrl+T/W/L/F/R в странице
      // не работают вовсе (вкладки и адресная строка живут не в ней, а в нашей
      // оболочке), и без перехвата человек за компьютером нажимал их в пустоту.
      // Ctrl+R страница поняла бы сама, но перезагрузку правильнее делать
      // вызовом: так она работает и с нашим индикатором загрузки.
      //
      // Действия берём из ref: сами функции (открыть вкладку, адрес, поиск)
      // объявлены НИЖЕ по файлу, и ссылка на них в зависимостях этого эффекта
      // упала бы на TDZ при первом рендере — та же грабля, что уже стоила
      // релиза на browserTouch.
      if (vbDirect && down && (e.ctrlKey || e.metaKey) && !e.altKey) {
        const key = (e.key || "").toLowerCase();
        const hotkey = isNonAsciiChar(key) ? (layoutToLatin[key] || key) : key;
        const run = browserHotkeysRef.current[hotkey];
        if (run) {
          e.preventDefault();
          run();
          return;
        }
      }
      // Печатный символ вне ASCII (кириллица, умляуты): у агента для него нет ни
      // виртуального кода, ни X keysym — {t:"k"} уходил в пустоту, а нажатие мы
      // к тому моменту уже погасили, поэтому буква пропадала и на ПК, и
      // локально. Справка обещает «печатайте как есть» — значит, такой символ
      // надо ПЕЧАТАТЬ ({t:"txt"}), а не «нажимать».
      if (isNonAsciiChar(e.key) && !combo) {
        if (inTyping) return; // в строке ввода символ печатает само поле
        e.preventDefault();
        if (down) send({ t: "txt", text: e.key });
        return;
      }
      // Сочетание набрано в национальной раскладке (Ctrl+«с»): на ПК уходит
      // латинская буква ТОЙ ЖЕ физической клавиши — ровно так это понимает и
      // сам Windows. Всё остальное едет как есть.
      const outKey = isNonAsciiChar(e.key)
        ? (layoutToLatin[e.key.toLowerCase()] || e.key)
        : e.key;
      if (inTyping) {
        // AltGr — не модификатор, а способ ввести @ € # ~ на европейских
        // раскладках. Windows выставляет при нём ctrlKey+altKey разом, поэтому
        // без этой проверки такие символы молча улетали бы на ПК комбинацией
        // и в поле не появлялись. (getModifierState — путь Firefox/Safari.)
        const altGraph = (e.ctrlKey && e.altKey) || e.getModifierState("AltGraph");
        // Голые Ctrl+A/C/V/X/Z — редактирование САМОГО поля: это обычный
        // текстовый инпут, выделить/скопировать/вставить в нём должно
        // работать локально. На ПК те же сочетания есть кнопками в «Действиях».
        const localEdit = e.ctrlKey && !e.altKey && !e.metaKey && !e.shiftKey
          && (e.code ? localEditCodes.has(e.code) : localEditKeys.has(e.key.toLowerCase()));
        // Обычный текст обрабатывает само поле, а остальные комбинации с
        // Ctrl/Alt/Meta (Ctrl+S, Ctrl+F, Win+…) должны уходить на ПК — иначе
        // с физической клавиатуры их отправить нечем.
        // Сам модификатор (его keydown приходит раньше основной клавиши) на ПК
        // всё же уходит — он нужен для Ctrl+S; парный keyup снимет его, так что
        // залипания нет, а зажатый Ctrl без сочетания Windows игнорирует.
        if (down && (altGraph || localEdit || !combo)) return;
        if (!down && !fwdKeysRef.current.has(outKey)) return;
      }
      e.preventDefault();
      if (down) fwdKeysRef.current.add(outKey); else fwdKeysRef.current.delete(outKey);
      send({ t: "k", k: outKey, d: down });
    };
    window.addEventListener("keydown", h);
    window.addEventListener("keyup", h);
    return () => {
      window.removeEventListener("keydown", h);
      window.removeEventListener("keyup", h);
      // Страховка от залипших клавиш при закрытии клавиатуры или уходе со страницы.
      for (const k of fwdKeysRef.current) send({ t: "k", k, d: false });
      fwdKeysRef.current.clear();
    };
  }, [directKeyboard, showKb, send, vbDirect]);

  // Фокус на экране — условие прямого форварда клавиш. Ставим его сами, как
  // только появился кадр: человек с ноутбука открывает /remote и сразу печатает,
  // а не ищет, куда нажать. Клик по экрану фокус возвращает (onCanvasMouseDown).
  useEffect(() => {
    if (!physicalKeyboard || !firstFrame || showKb) return;
    stageRef.current?.focus?.();
  }, [firstFrame, physicalKeyboard, showKb]);

  // Cleanup inertia on unmount
  useEffect(() => () => stopInertia(), [stopInertia]);

  const pinchDist = (a: React.Touch, b: React.Touch) =>
    Math.hypot(a.clientX - b.clientX, a.clientY - b.clientY);

  // ═════════════════════════════════════════════════════════════════
  // CANVAS touch: tap=click, double-tap=2×click (двойной клик), 2-finger tap=right,
  //               long-press=drag, drag=pan(zoomed), pinch=zoom
  // ═════════════════════════════════════════════════════════════════

  // ═════════════════════════════════════════════════════════════════
  // Касания СТРАНИЦЫ (виртуальный браузер): тап нажимает, движение тащит,
  // отпускание оставляет инерцию, два пальца меняют масштаб страницы.
  // Правила жеста живут отдельно и покрыты тестами (remote/browserGestures).
  // ═════════════════════════════════════════════════════════════════

  const sendTouch = useCallback((action: "start" | "move" | "end", touches: React.TouchList) => {
    const points: { x: number; y: number }[] = [];
    for (let i = 0; i < touches.length && i < 2; i++) {
      const [x, y] = toRelativePoint(touches[i].clientX, touches[i].clientY);
      points.push({ x, y });
    }
    send({ t: "tp", a: action, p: points });
  }, [send, toRelativePoint]);

  // Инерция: палец уже оторван, касаниями продолжать нельзя — досылаем
  // прокруткой. Дробную часть копим, иначе мелкие шаги пропадали бы при
  // округлении до «щелчка колеса».
  const flingScroll = useCallback((velocity: number) => {
    const steps = flingSteps(velocity, screenRef.current?.sh || 1200);
    if (!steps.length) return;
    let elapsed = 0;
    for (const step of steps) {
      elapsed += step.delay;
      window.setTimeout(() => {
        bt.current.scrollAcc += step.dy / 100; // щелчок колеса ≈ 100 px страницы
        const whole = Math.trunc(bt.current.scrollAcc);
        if (whole !== 0) {
          bt.current.scrollAcc -= whole;
          send({ t: "s", dy: whole });
        }
      }, elapsed);
    }
  }, [send]);

  // Меню долгого нажатия: спрашиваем у страницы, что лежит под пальцем, и
  // показываем то же, что телефонный браузер, — «открыть в новой вкладке»,
  // «скопировать ссылку», «скопировать текст».
  const openContextMenu = useCallback(async (rx: number, ry: number) => {
    try {
      const hit = await hitBrowserPoint(rx, ry, true);
      if (!hit || (!hit.link && !hit.image && !hit.text && !hit.selection)) return;
      haptic("medium");
      setCtxHit(hit);
    } catch { /* страница могла уйти — меню просто не появится */ }
  }, []);

  const cancelLongPress = useCallback(() => {
    if (longPressRef.current) {
      clearTimeout(longPressRef.current);
      longPressRef.current = null;
    }
  }, []);

  // «Потянуть вниз, чтобы обновить»: страница перезагружается, натяжение
  // снимается вместе с ответом.
  const runPullRefresh = useCallback(async () => {
    setPullProgress(0);
    setRefreshing(true);
    haptic("medium");
    try {
      await navigateVBrowser({ action: "reload" });
    } catch (e: any) {
      toastError(mapApiError(e));
    } finally {
      window.setTimeout(() => setRefreshing(false), 600);
    }
  }, [toastError]);

  const onPageTouchStart = useCallback((e: React.TouchEvent) => {
    // React делегирует touch-события пассивным слушателем в Chromium. Сам
    // экран уже имеет touch-action:none, поэтому браузерный жест подавлен CSS,
    // а preventDefault здесь лишь засорял консоль предупреждением.
    compatMouseUntilRef.current = Date.now() + 1000;
    const st = bt.current;
    const now = Date.now();
    if (e.touches.length >= 2) {
      // Второй палец — это щипок: касание страницы завершаем, чтобы она не
      // считала, что её всё ещё тащат.
      if (st.active) send({ t: "tp", a: "end", p: [] });
      st.active = false;
      st.pinching = true;
      st.pinchStart = pinchDistance(
        ...(([0, 1].map((i) => {
          const [x, y] = toRelativePoint(e.touches[i].clientX, e.touches[i].clientY);
          return { x, y };
        }) as [{ x: number; y: number }, { x: number; y: number }])),
      );
      st.pinchBase = pageScaleRef.current;
      return;
    }
    const [x, y] = toRelativePoint(e.touches[0].clientX, e.touches[0].clientY);
    st.active = true;
    st.pinching = false;
    st.startX = x; st.startY = y; st.startAt = now;
    st.samples = [{ x, y, t: now }];
    st.scrollAcc = 0;
    st.chromeY = e.touches[0].clientY;
    st.chromeAcc = 0;
    // Жесты, которых у «удалёнки» не было, а у телефонного браузера есть:
    // свайп от края назад/вперёд, натяжение сверху и меню долгого нажатия.
    const rect = wrapRef.current?.getBoundingClientRect();
    st.clientX0 = e.touches[0].clientX - (rect?.left || 0);
    st.clientY0 = e.touches[0].clientY - (rect?.top || 0);
    st.width = rect?.width || 0;
    st.edge = null;
    st.pulling = false;
    st.doubleTap = now - st.lastTapAt < 300
      && Math.hypot(x - st.lastTapX, y - st.lastTapY) < 0.05;
    cancelLongPress();
    longPressRef.current = setTimeout(() => {
      longPressRef.current = null;
      // Палец простоял на месте полсекунды — это вызов меню, а не листание.
      const last = st.samples[st.samples.length - 1];
      const moved = last ? Math.hypot(last.x - x, last.y - y) : 0;
      if (!st.active || moved > 0.02) return;
      send({ t: "tp", a: "end", p: [] });
      st.active = false;
      void openContextMenu(x, y);
    }, 520);
    sendTouch("start", e.touches);
  }, [cancelLongPress, openContextMenu, send, sendTouch, toRelativePoint]);

  const onPageTouchMove = useCallback((e: React.TouchEvent) => {
    const st = bt.current;
    if (st.pinching && e.touches.length >= 2) {
      const dist = pinchDistance(
        ...(([0, 1].map((i) => {
          const [x, y] = toRelativePoint(e.touches[i].clientX, e.touches[i].clientY);
          return { x, y };
        }) as [{ x: number; y: number }, { x: number; y: number }])),
      );
      const next = pinchScale(st.pinchBase, st.pinchStart, dist);
      pageScaleRef.current = next;
      // Масштаб уходит на машину не чаще десяти раз в секунду: чаще — это
      // поток запросов ради изменений, которых глаз не различит.
      const now = Date.now();
      if (now - st.lastScaleAt > 100) {
        st.lastScaleAt = now;
        void setBrowserScale(next).catch(() => { /* страница могла закрыться */ });
      }
      return;
    }
    if (!st.active || e.touches.length === 0) return;
    const [x, y] = toRelativePoint(e.touches[0].clientX, e.touches[0].clientY);
    st.samples.push({ x, y, t: Date.now() });
    if (st.samples.length > 12) st.samples.shift();
    const rect = wrapRef.current?.getBoundingClientRect();
    const dxClient = e.touches[0].clientX - (rect?.left || 0) - st.clientX0;
    const dyClient = e.touches[0].clientY - (rect?.top || 0) - st.clientY0;
    if (Math.hypot(dxClient, dyClient) > 12) cancelLongPress();
    // Свайп от края — «назад»/«вперёд». Решение принимается один раз за жест:
    // дальше палец уже не листает страницу под собой.
    if (!st.edge && !st.pulling) {
      const dir = edgeSwipe(st.clientX0, dxClient, dyClient, st.width);
      if (dir) {
        st.edge = dir;
        send({ t: "tp", a: "end", p: [] });
        haptic("light");
      }
    }
    if (st.edge) return;
    // Натяжение сверху: страница стоит у самого верха, палец идёт вниз.
    const pull = pullRefresh(pageScrollRef.current, dyClient);
    if (pull.progress > 0 && (st.pulling || Math.abs(dyClient) > Math.abs(dxClient))) {
      st.pulling = true;
      setPullProgress(pull.progress);
      if (pull.fire) {
        st.pulling = false;
        st.active = false;
        send({ t: "tp", a: "end", p: [] });
        void runPullRefresh();
        return;
      }
    } else if (st.pulling) {
      st.pulling = false;
      setPullProgress(0);
    }
    // Хром уезжает и возвращается по направлению драга — как у настоящего
    // мобильного браузера. Щипок сюда не доходит (выше return), инерция идёт
    // уже без пальца, так что панели не дёргаются ни там, ни там.
    const cy = e.touches[0].clientY;
    const drag = chromeOnDrag(st.chromeAcc, cy - st.chromeY);
    st.chromeY = cy;
    st.chromeAcc = drag.acc;
    if (drag.hidden !== null) setChromeHidden(drag.hidden);
    sendTouch("move", e.touches);
  }, [cancelLongPress, runPullRefresh, send, sendTouch, toRelativePoint]);

  const onPageTouchEnd = useCallback((e: React.TouchEvent) => {
    const st = bt.current;
    cancelLongPress();
    if (st.pulling) {
      // Натянули, но не дотянули — резинка возвращается на место.
      st.pulling = false;
      setPullProgress(0);
    }
    if (st.edge) {
      // Свайп от края довёлся до конца: навигация идёт прямым вызовом, а не
      // через browserGo — тот объявлен ниже по файлу, и ссылка на него здесь
      // упала бы на TDZ при первом рендере (эта грабля уже стоила релиза).
      const dir = st.edge;
      st.edge = null;
      st.active = false;
      st.samples = [];
      haptic("light");
      void navigateVBrowser({ action: dir }).catch((e: any) => toastError(mapApiError(e)));
      return;
    }
    if (st.pinching) {
      if (e.touches.length === 0) st.pinching = false;
      return;
    }
    if (!st.active) return;
    st.active = false;
    send({ t: "tp", a: "end", p: [] });
    const last = st.samples[st.samples.length - 1];
    const tap = !last || isTap({ x: st.startX, y: st.startY }, last, Date.now() - st.startAt);
    if (!tap) {
      flingScroll(flingVelocity(st.samples));
    } else if (st.doubleTap) {
      // Двойной тап приближает к содержимому и возвращает обратно — ровно как
      // в мобильном браузере, где щипок для этого не нужен.
      const next = doubleTapScale(pageScaleRef.current);
      pageScaleRef.current = next;
      st.lastTapAt = 0;
      haptic("light");
      void setBrowserScale(next).catch(() => { /* страница могла закрыться */ });
    } else {
      st.lastTapAt = Date.now();
      st.lastTapX = st.startX;
      st.lastTapY = st.startY;
    }
    st.samples = [];
  }, [cancelLongPress, flingScroll, send, toastError]);

  const onCanvasTouchStart = useCallback((e: React.TouchEvent) => {
    if (browserTouch) { onPageTouchStart(e); return; }
    // Режим «Тачпад»: вся картинка — тачпад (remote/trackpad.ts), а не только
    // полоса справа. Абсолютные жесты ниже — для режима «Экран».
    if (controlMode === "trackpad") { padStartRef.current(e, "stage"); return; }
    e.preventDefault();
    const s = ct.current;
    // Первое касание при скрытых контролах возвращает панель и НЕ кликает по
    // рабочему столу: возврат панели меняет высоту .remote-stage, и точка под
    // пальцем уезжает. Но само ДВИЖЕНИЕ теряться не должно — панорама, щипок и
    // прокрутка двумя пальцами обрабатываются как обычно (N142). Долгое нажатие
    // (перетаскивание) в этом жесте тоже не начинаем: оно шлёт mouse-down по той
    // же уехавшей точке.
    // Признак ставим на ВЕСЬ жест: второй палец приходит уже при видимой
    // панели, и без этого тап двумя пальцами всё равно слал бы ПКМ.
    if (e.touches.length === e.changedTouches.length) wakeTapRef.current = !showUI;
    const wake = wakeTapRef.current;
    // Режим «Тачпад» сюда не доходит (см. делегирование выше): там вся
    // картинка — тачпад, и клик идёт под курсором, а не в точку касания.
    resetHideTimer();
    stopInertia();

    if (e.touches.length === 1) {
      const t = e.touches[0];
      s.startX = t.clientX; s.startY = t.clientY;
      s.startTime = Date.now();
      s.isPan = false; s.isPinch = false; s.moved = false;
      s.panStartOX = offsetRef.current.x;
      s.panStartOY = offsetRef.current.y;

      // Long-press → drag
      if (dragRef.current.timer) clearTimeout(dragRef.current.timer);
      const touchCopy = { clientX: t.clientX, clientY: t.clientY } as Touch;
      dragRef.current.timer = wake ? null : setTimeout(() => {
        const [rx, ry] = toRelative(touchCopy);
        dragRef.current.state = "dragging";
        dragRef.current.source = "canvas";
        setDragging(true);
        cursorPos.current = { x: rx, y: ry };
        withModifiers(() => send({ t: "m", x: rx, y: ry, a: "down", b: "left" }));
        haptic("heavy");
        drawCursor();
      }, 400);

    } else if (e.touches.length === 2) {
      if (dragRef.current.timer) { clearTimeout(dragRef.current.timer); dragRef.current.timer = null; }
      s.isPinch = true;
      s.pinchStartDist = pinchDist(e.touches[0], e.touches[1]);
      s.pinchStartScale = scaleRef.current;
      // Заготовка под «тап двумя пальцами = ПКМ»: точка — середина между
      // пальцами, решение примем на touchend (если не разъехались).
      s.twoTapTime = Date.now();
      s.pinchMoved = false;
      s.twoTapX = (e.touches[0].clientX + e.touches[1].clientX) / 2;
      s.twoTapY = (e.touches[0].clientY + e.touches[1].clientY) / 2;
      // Точка отсчёта прокрутки двумя пальцами; какой это жест — прокрутка или
      // щипок — решаем по первому заметному движению.
      s.twoScrollY = s.twoTapY;
      s.isZooming = false;
    }
  }, [browserTouch, controlMode, drawCursor, onPageTouchStart, resetHideTimer, send, showUI, stopInertia, toRelative, withModifiers]);

  const onCanvasTouchMove = useCallback((e: React.TouchEvent) => {
    if (browserTouch) { onPageTouchMove(e); return; }
    if (controlMode === "trackpad") { padMoveRef.current(e, "stage"); return; }
    e.preventDefault();
    const s = ct.current;

    // Canvas drag mode
    if (dragRef.current.state === "dragging" && dragRef.current.source === "canvas") {
      if (e.touches.length === 1) {
        const [rx, ry] = toRelative(e.touches[0]);
        cursorPos.current = { x: rx, y: ry };
        queueMove(rx, ry);
        drawCursor();
      }
      return;
    }

    if (e.touches.length === 1 && !s.isPinch) {
      const t = e.touches[0];
      const dx = t.clientX - s.startX, dy = t.clientY - s.startY;
      if (Math.hypot(dx, dy) > 6) {
        s.moved = true;
        if (dragRef.current.timer) { clearTimeout(dragRef.current.timer); dragRef.current.timer = null; }
      }

      // Pan when zoomed
      if (scaleRef.current > 1.05 && s.moved) {
        s.isPan = true;
        offsetRef.current.x = s.panStartOX + dx;
        offsetRef.current.y = s.panStartOY + dy;
        clampOffset(); applyTransform();
      }

    } else if (e.touches.length === 2 && s.isPinch) {
      const dist = pinchDist(e.touches[0], e.touches[1]);
      // Разъехались или уехали в сторону — это щипок/свайп, а не тап двумя
      // пальцами: ПКМ на touchend не шлём.
      const midX = (e.touches[0].clientX + e.touches[1].clientX) / 2;
      const midY = (e.touches[0].clientY + e.touches[1].clientY) / 2;
      if (Math.abs(dist - s.pinchStartDist) > 12
        || Math.hypot(midX - s.twoTapX, midY - s.twoTapY) > 12) s.pinchMoved = true;

      // Два пальца, но дистанция почти не менялась → это ПРОКРУТКА, а не зум.
      // В режиме «Экран» прокрутки не было ни одним жестом: человек пытался
      // листать лог и получал зум (#48). Порог по относительному изменению
      // дистанции: пальцы всегда чуть «дышат», поэтому сравниваем не с нулём.
      const distRatio = s.pinchStartDist > 0 ? Math.abs(dist - s.pinchStartDist) / s.pinchStartDist : 1;
      if (!s.isZooming && distRatio < 0.15) {
        const dy = midY - s.twoScrollY;
        if (Math.abs(dy) >= 6) {
          s.twoScrollY = midY;
          // Курсор ОС прокручивает то окно, над которым стоит: сначала ведём
          // его под пальцы, иначе колесо уедет в предыдущее окно.
          const [rx, ry] = toRelativePoint(midX, midY);
          send({ t: "m", x: rx, y: ry, a: "move" });
          send({ t: "s", dy });
        }
        return;
      }
      // Дистанция поехала заметно — это уже щипок, и обратно в прокрутку
      // жест не превращается: иначе экран дёргается между зумом и колесом.
      s.isZooming = true;

      let ns = s.pinchStartScale * (dist / s.pinchStartDist);
      ns = Math.max(1, Math.min(5, ns));
      scaleRef.current = ns;
      setZoomLevel(ns);
      if (ns <= 1.02) { offsetRef.current.x = 0; offsetRef.current.y = 0; }
      clampOffset(); applyTransform();
    }
  }, [browserTouch, controlMode, onPageTouchMove, toRelative, toRelativePoint, send, queueMove, drawCursor, applyTransform, clampOffset]);

  const onCanvasTouchEnd = useCallback((e: React.TouchEvent) => {
    if (browserTouch) { onPageTouchEnd(e); return; }
    if (controlMode === "trackpad") { padEndRef.current(e, "stage"); return; }
    e.preventDefault();
    const s = ct.current;
    // Жест, которым вернули панель: клика по рабочему столу в нём нет, но всё
    // остальное (панорама, щипок, прокрутка) уже отработало в *TouchMove.
    // Флаг снимаем, только когда с экрана ушёл последний палец, иначе второй
    // отрыв в щипке проскочил бы как обычный тап.
    const wake = wakeTapRef.current;
    if (e.touches.length === 0) wakeTapRef.current = false;
    // Режим «Тачпад» сюда не доходит (делегирование выше): кликает он под
    // курсором сам. Здесь остаётся только правило «жест, вернувший панель, не
    // кликает».
    const noClick = wake;

    // End canvas drag
    if (dragRef.current.state === "dragging" && dragRef.current.source === "canvas") {
      if (e.changedTouches.length > 0) {
        const [rx, ry] = toRelative(e.changedTouches[0]);
        send({ t: "m", x: rx, y: ry, a: "up", b: "left" });
      } else {
        const cp = cursorPos.current;
        send({ t: "m", x: cp.x, y: cp.y, a: "up", b: "left" });
      }
      dragRef.current.state = "idle";
      dragRef.current.source = "";
      setDragging(false);
      haptic("light");
      return;
    }

    if (dragRef.current.timer) { clearTimeout(dragRef.current.timer); dragRef.current.timer = null; }
    // Тап двумя пальцами = правый клик (двойной тап теперь занят двойным кликом).
    // Заодно гасим ложный ЛЕВЫЙ клик: раньше отрыв второго пальца проваливался
    // в ветку одиночного тапа и слал click по случайной точке.
    if (s.isPinch) {
      s.isPinch = false;
      if (!noClick && !s.pinchMoved && s.twoTapTime && Date.now() - s.twoTapTime < 300) {
        const [rx, ry] = toRelativePoint(s.twoTapX, s.twoTapY);
        cursorPos.current = { x: rx, y: ry };
        withModifiers(() => send({ t: "m", x: rx, y: ry, a: "click", b: "r" }));
        haptic("medium");
        showRipple(rx, ry);
        drawCursor();
      }
      s.twoTapTime = 0;
      s.moved = true;      // подавляет ложный клик на втором touchend
      s.lastTapTime = 0;   // и не даёт засчитать это половиной двойного тапа
      return;
    }
    if (s.isPan) { s.isPan = false; return; }

    // Короткий тап → левый клик; двойной тап → ДВОЙНОЙ клик (открыть файл или
    // папку). Правый клик: тап двумя пальцами или кнопка «ПКМ» внизу.
    if (!noClick && !s.moved && Date.now() - s.startTime < 300 && e.changedTouches.length > 0) {
      const [rx, ry] = toRelative(e.changedTouches[0]);
      const now = Date.now();
      const near = Math.hypot(rx - s.lastTapX, ry - s.lastTapY) < 0.05;
      if (now - s.lastTapTime < 300 && near) {
        // ВНИМАНИЕ: здесь намеренно ВТОРОЙ ОБЫЧНЫЙ клик, а не a:"dblclick".
        // Одиночный click за первый тап уже ушёл на ПК ~200 мс назад, а
        // "dblclick" на агенте — это ЕЩЁ ДВА клика (MouseDoubleClick /
        // xdotool click --repeat 2), итого ТРИ клика за <400 мс: третий
        // улетает в только что открывшееся окно, а в тексте даёт выделение
        // абзаца. Два обычных клика в одну точку — ровно то, что делает
        // физическая мышь: ОС склеит их в двойной сама (системный порог
        // ≈500 мс, наше окно распознавания 300 мс заведомо внутри).
        // Не «чинить» обратно на dblclick.
        // Координаты — ПЕРВОГО тапа: между тапами палец всегда чуть смещается.
        withModifiers(() => send({ t: "m", x: s.lastTapX, y: s.lastTapY, a: "click", b: "l" }));
        haptic("medium");
        s.lastTapTime = 0;
      } else {
        cursorPos.current.x = rx;
        cursorPos.current.y = ry;
        // Ctrl+тап и Shift+тап: выделить несколько файлов или диапазон.
        withModifiers(() => send({ t: "m", x: rx, y: ry, a: "click", b: "l" }));
        haptic("light");
        s.lastTapTime = now;
        s.lastTapX = rx; s.lastTapY = ry;
      }
      showRipple(rx, ry); // instant local feedback — the frame echo comes later
      drawCursor();
    }
    s.moved = false;
  }, [browserTouch, controlMode, onPageTouchEnd, toRelative, toRelativePoint, send, drawCursor, showRipple, withModifiers]);

  // ═════════════════════════════════════════════════════════════════
  // TRACKPAD: одна машина жестов (remote/trackpad.ts) на полосу справа и — в
  // режиме «Тачпад» — на ВСЮ картинку. 1 палец = курсор относительно,
  // тап = клик под курсором, двойной = второй клик, 2 пальца = колесо (и по
  // горизонтали), тап двумя = правый клик, щипок = масштаб (только на
  // картинке), долгое нажатие = перетаскивание, 3 пальца = Alt+Tab.
  // До 02.09.2026 картинка в режиме «Тачпад» на касания не отвечала вовсе, а
  // относительная наводка жила на полосе 70–80 px — главный разрыв с AnyDesk
  // по словам владельца. Правила считаются в модуле и проверены тестами.
  // ═════════════════════════════════════════════════════════════════

  const padSurface = useCallback((allowZoom: boolean): PadSurface => {
    const st = stageRef.current;
    const w = st ? st.getBoundingClientRect().width : 1;
    const h = st ? st.getBoundingClientRect().height : 1;
    return { width: Math.max(1, w), height: Math.max(1, h), sensitivity, allowZoom };
  }, [sensitivity]);

  const padPoints = (list: React.TouchList): PadPoint[] => {
    const out: PadPoint[] = [];
    for (let i = 0; i < list.length; i++) out.push({ x: list[i].clientX, y: list[i].clientY });
    return out;
  };

  const applyPadActions = useCallback((actions: PadAction[], source: "stage" | "strip") => {
    for (const a of actions) {
      switch (a.kind) {
        case "armLongPress":
          if (padLongPressTimer.current) clearTimeout(padLongPressTimer.current);
          padLongPressTimer.current = setTimeout(() => {
            padLongPressTimer.current = null;
            applyPadActions(padEngineRef.current.longPress(), source);
          }, a.ms);
          break;
        case "disarmLongPress":
          if (padLongPressTimer.current) { clearTimeout(padLongPressTimer.current); padLongPressTimer.current = null; }
          break;
        case "move": {
          const cp = cursorPos.current;
          cp.x = Math.max(0, Math.min(1, cp.x + a.dx));
          cp.y = Math.max(0, Math.min(1, cp.y + a.dy));
          queueMove(cp.x, cp.y);
          drawCursor();
          // Под увеличением картинка едет за курсором сама: в режиме тачпада
          // палец занят курсором, панорамировать ему нечем.
          if (source === "stage") keepCursorInView();
          break;
        }
        case "click": {
          const cp = cursorPos.current;
          withModifiers(() => send({ t: "m", x: cp.x, y: cp.y, a: "click", b: a.button }));
          haptic(a.second || a.button === "r" ? "medium" : "light");
          showRipple(cp.x, cp.y);
          drawCursor();
          break;
        }
        case "dragStart": {
          dragRef.current.state = "dragging";
          dragRef.current.source = "pad";
          setDragging(true);
          const cp = cursorPos.current;
          withModifiers(() => send({ t: "m", x: cp.x, y: cp.y, a: "down", b: "left" }));
          haptic("heavy");
          break;
        }
        case "dragEnd": {
          const cp = cursorPos.current;
          send({ t: "m", x: cp.x, y: cp.y, a: "up", b: "left" });
          dragRef.current.state = "idle";
          dragRef.current.source = "";
          setDragging(false);
          haptic("light");
          break;
        }
        case "scroll":
          queueScroll(a.dy, a.dx);
          break;
        case "zoom": {
          let ns = padZoomStartRef.current * a.ratio;
          ns = Math.max(1, Math.min(5, ns));
          scaleRef.current = ns;
          setZoomLevel(ns);
          if (ns <= 1.02) { offsetRef.current.x = 0; offsetRef.current.y = 0; }
          clampOffset(); applyTransform();
          break;
        }
        case "altTab":
          haptic("heavy");
          send({ t: "k", k: "Alt", d: true });
          if (a.dir < 0) send({ t: "k", k: "Shift", d: true });
          send({ t: "k", k: "Tab", d: true });
          send({ t: "k", k: "Tab", d: false });
          if (a.dir < 0) send({ t: "k", k: "Shift", d: false });
          send({ t: "k", k: "Alt", d: false });
          break;
        case "fling":
          inertiaRef.current = { vx: a.vx, vy: a.vy, lastTime: performance.now() };
          startInertia();
          break;
      }
    }
  }, [applyTransform, clampOffset, drawCursor, keepCursorInView, queueMove, queueScroll, send, showRipple, startInertia, withModifiers]);

  const padStart = useCallback((e: React.TouchEvent, source: "stage" | "strip") => {
    e.preventDefault();
    if (source === "strip") e.stopPropagation();
    resetHideTimer();
    stopInertia();
    if (e.touches.length === 2) padZoomStartRef.current = scaleRef.current;
    applyPadActions(padEngineRef.current.start(padPoints(e.touches), Date.now(), padSurface(source === "stage")), source);
  }, [applyPadActions, padSurface, resetHideTimer, stopInertia]);

  const padMove = useCallback((e: React.TouchEvent, source: "stage" | "strip") => {
    e.preventDefault();
    if (source === "strip") e.stopPropagation();
    applyPadActions(padEngineRef.current.move(padPoints(e.touches), Date.now()), source);
  }, [applyPadActions]);

  const padEnd = useCallback((e: React.TouchEvent, source: "stage" | "strip") => {
    e.preventDefault();
    if (source === "strip") e.stopPropagation();
    applyPadActions(padEngineRef.current.end(e.touches.length, Date.now()), source);
  }, [applyPadActions]);

  padStartRef.current = padStart;
  padMoveRef.current = padMove;
  padEndRef.current = padEnd;

  const onPadTouchStart = useCallback((e: React.TouchEvent) => padStart(e, "strip"), [padStart]);

  const onPadTouchMove = useCallback((e: React.TouchEvent) => padMove(e, "strip"), [padMove]);

  const onPadTouchEnd = useCallback((e: React.TouchEvent) => padEnd(e, "strip"), [padEnd]);

  // ── Key helpers ────────────────────────────────────────────────

  const sendKey = useCallback((key: string) => {
    haptic("light");
    withModifiers(() => {
      send({ t: "k", k: key, d: true });
      send({ t: "k", k: key, d: false });
    });
  }, [send, withModifiers]);

  const sendCombo = useCallback((keys: string[]) => {
    haptic("medium");
    for (const k of keys) send({ t: "k", k, d: true });
    for (const k of [...keys].reverse()) send({ t: "k", k, d: false });
    setShowQuickActions(false);
  }, [send]);

  // Навигация вкладкой напрямую. Когда браузер на связи по своему протоколу,
  // это вызов; когда нет — прежний путь горячими клавишами (они уходят в окно
  // браузера через ОС).
  const browserGo = useCallback(async (action: "back" | "forward" | "reload") => {
    haptic("light");
    if (!vbDirect) {
      if (action === "reload") sendKey("F5");
      else sendCombo(["Alt", action === "back" ? "ArrowLeft" : "ArrowRight"]);
      return;
    }
    try {
      await navigateVBrowser({ action });
    } catch (e: any) {
      toastError(mapApiError(e));
    }
  }, [vbDirect, sendKey, sendCombo, toastError]);

  // Что открыто в браузере: адрес, заголовок, идёт ли загрузка. Спрашиваем по
  // событию от страницы, а не по таймеру: лишний опрос на слабой машине дороже
  // самой информации.
  const refreshBrowserPage = useCallback(async () => {
    if (!vbDirect) return;
    try {
      setBrowserPage(await getBrowserPage());
    } catch { /* браузер мог закрыться — молчим, экран скажет сам */ }
  }, [vbDirect]);

  // Список вкладок — фоновая синхронизация (стартует вместе с каналом к
  // браузеру и после каждой операции с вкладками): молчим, если не вышло, —
  // счётчик просто останется прежним, а не накроет экран тостом.
  const refreshBrowserTabs = useCallback(async () => {
    try {
      const res = await getBrowserTabs();
      setBrowserTabs(res.tabs || []);
    } catch { /* браузер мог закрыться — молчим, экран скажет сам */ }
  }, []);

  // Кнопка вкладок со счётчиком: открывает шит со списком и сразу подтягивает
  // свежий (со счётчика же человек судит, не залипло ли).
  const openTabsSheet = useCallback(() => {
    haptic();
    setChromeHidden(false);
    setTabsOpen(true);
    void refreshBrowserTabs();
  }, [refreshBrowserTabs]);

  const openBrowserMenu = useCallback(() => {
    haptic("light");
    setChromeHidden(false);
    setBrowserMenu(true);
  }, []);

  const openTab = useCallback(async (id: string) => {
    haptic("light");
    setTabsOpen(false);
    try {
      await activateBrowserTab(id);
      setRetryKey((v) => v + 1); // кадры пойдут из другой вкладки
      await refreshBrowserPage();
    } catch (e: any) {
      toastError(mapApiError(e));
    }
  }, [refreshBrowserPage, toastError]);

  const addTab = useCallback(async () => {
    haptic("light");
    setTabsOpen(false);
    try {
      await newBrowserTab("");
      void refreshBrowserTabs(); // счётчик вкладок на панели: 1 → 2
      setRetryKey((v) => v + 1);
      setAddressText("");
      setChromeHidden(false);
      setAddressOpen(true);
      await refreshBrowserPage();
    } catch (e: any) {
      toastError(mapApiError(e));
    }
  }, [refreshBrowserPage, refreshBrowserTabs, toastError]);

  const dropTab = useCallback(async (id: string) => {
    haptic("light");
    try {
      await closeBrowserTab(id);
      await refreshBrowserTabs();
      setRetryKey((v) => v + 1);
      await refreshBrowserPage();
    } catch (e: any) {
      toastError(mapApiError(e));
    }
  }, [refreshBrowserPage, refreshBrowserTabs, toastError]);

  // Вид сайта: телефон, планшет или компьютер. Ровно та же кнопка есть в
  // каждом телефонном браузере, и ждут её там же — в меню страницы. Профили
  // приходят каталогом с агента, поэтому имя — строка, а не три варианта.
  const switchDevice = useCallback(async (device: string) => {
    haptic("light");
    setShowQuickActions(false);
    setBrowserMenu(false);
    try {
      const size = virtualScreenSize();
      await emulateBrowserDevice({
        device,
        width: device === "desktop" ? 1280 : size.width,
        height: device === "desktop" ? 800 : size.height,
        scale: device === "desktop" ? 1 : (window.devicePixelRatio || 2),
      });
      setRetryKey((v) => v + 1);
      await refreshBrowserPage();
    } catch (e: any) {
      toastError(mapApiError(e));
    }
  }, [refreshBrowserPage, toastError]);

  // Появился прямой канал к браузеру — сразу узнаём, что за страница открыта
  // и сколько вкладок: адресная строка и счётчик не должны ждать первого
  // события.
  useEffect(() => {
    if (!vbDirect) {
      setBrowserPage(null);
      setBrowserTabs([]);
      return;
    }
    void refreshBrowserPage();
    void refreshBrowserTabs();
  }, [vbDirect, refreshBrowserPage, refreshBrowserTabs]);

  // Вышли из браузерного режима (тачпад, стоп) — хром всегда на месте.
  useEffect(() => {
    if (!browserTouch) setChromeHidden(false);
  }, [browserTouch]);

  // Клавиатура телефона закрывает половину экрана, а страница на сервере об
  // этом не знает и продолжает считать окно целым: поле ввода остаётся ПОД
  // клавиатурой, и человек печатает вслепую. Настоящий телефон уменьшает
  // видимую область — сообщаем машине ровно это, и страница сама поднимает
  // поле. Долю берём у визуального окна: на телефоне оно и есть то, что
  // осталось видно.
  useEffect(() => {
    if (!vbDirect) return;
    const report = () => {
      const vv = window.visualViewport;
      const full = window.innerHeight || 1;
      const visible = showKb && vv ? Math.min(1, Math.max(0.25, vv.height / full)) : 1;
      void setBrowserViewport(visible).catch(() => { /* браузер мог закрыться */ });
    };
    report();
    if (!showKb) return;
    // Клавиатура выезжает не мгновенно и меняет высоту несколько раз (ряд
    // подсказок, смена языка) — слушаем визуальное окно, пока она открыта.
    const vv = window.visualViewport;
    if (!vv) return;
    let timer = 0;
    const onResize = () => {
      window.clearTimeout(timer);
      timer = window.setTimeout(report, 180);
    };
    vv.addEventListener("resize", onResize);
    return () => {
      window.clearTimeout(timer);
      vv.removeEventListener("resize", onResize);
    };
  }, [showKb, vbDirect]);

  // Каталог видов сайта живёт на агенте: приложение обязано показывать ровно
  // те профили, которые умеет эта версия браузера.
  useEffect(() => {
    if (!vbDirect) { setDeviceList([]); return; }
    let alive = true;
    getBrowserDevices()
      .then((res) => { if (alive) setDeviceList(res.devices || []); })
      .catch(() => { /* старый агент каталога не знает — меню останется прежним */ });
    return () => { alive = false; };
  }, [vbDirect]);

  // «В закладки» — звёздочка телефонного браузера: сохраняет ТЕКУЩУЮ страницу,
  // адрес для этого вводить не нужно.
  const addBookmarkHere = useCallback(async () => {
    setBrowserMenu(false);
    try {
      await addBrowserBookmark();
      hapticSuccess();
      toastSuccess(t("remote.bookmarkAdded"));
    } catch (e: any) {
      toastError(mapApiError(e));
    }
  }, [toastError, toastSuccess]);

  // Скопировать на устройство человека: этим же путём работает «Копировать» в
  // меню долгого нажатия и показ придуманного пароля.
  const copyToDevice = useCallback(async (text: string) => {
    if (!text) return;
    try {
      await navigator.clipboard.writeText(text);
      hapticSuccess();
      toastSuccess(t("remote.ctxCopied"));
    } catch {
      toastError(t("remote.clipboardError"));
    }
  }, [toastError, toastSuccess]);

  const openAddress = useCallback(() => {
    haptic("light");
    if (!vbDirect) {
      sendCombo(["Control", "l"]); // адресная строка самого браузера
      return;
    }
    setChromeHidden(false);
    // Текущий адрес подставляем целиком — на фокусе он выделится, и набранное
    // поверх заменит его одним движением, как в настоящем браузере.
    const current = browserPage?.url || "";
    setAddressText(current && current !== "about:blank" ? current : "");
    setAddressOpen((open) => !open);
  }, [vbDirect, sendCombo, browserPage]);

  const submitAddress = useCallback(async () => {
    // «Поиск или адрес» — то же правило, что у строки настоящего браузера.
    const url = searchOrUrl(addressText);
    if (!url) return;
    setAddressOpen(false);
    try {
      await navigateVBrowser({ url });
      hapticSuccess();
    } catch (e: any) {
      toastError(mapApiError(e));
    }
  }, [addressText, toastError]);

  // Горячие клавиши браузера для человека за компьютером. В самой странице они
  // не работают вовсе: вкладки, адресная строка и поиск живут в нашей оболочке,
  // а не в ней, — до этого Ctrl+T с ноутбука уходил в пустоту.
  useEffect(() => {
    browserHotkeysRef.current = {
      t: () => { void addTab(); },
      w: () => {
        const id = browserTabs.find((tab) => tab.active)?.id || browserPage?.target_id;
        if (id) void dropTab(id);
      },
      l: () => openAddress(),
      f: () => setFindOpen(true),
      r: () => { void browserGo("reload"); },
    };
  }, [addTab, browserGo, browserPage, browserTabs, dropTab, openAddress]);

  // Win+L — «дверь в один конец»: после блокировки ПК отдаёт только экран
  // входа, поэтому спрашиваем подтверждение.
  const lockPc = useCallback(async () => {
    setShowQuickActions(false);
    if (!(await tgConfirm(t("remote.lockConfirm"), { confirmText: t("confirm.btn.lock") }))) return;
    sendCombo(["Meta", "l"]);
  }, [sendCombo]);

  // Alt+F4 закрывает активное окно вместе с несохранённым документом, отмены
  // нет. Плитка стоит в сетке 3-в-ряд вплотную к «Скриншоту» — промах пальцем
  // стоит слишком дорого, чтобы спрашивать подтверждение у заведомо более
  // безобидной блокировки экрана и не спрашивать здесь.
  const closeActiveWindow = useCallback(async () => {
    setShowQuickActions(false);
    if (!(await tgConfirm(t("remote.closeWindowConfirm"), {
      danger: true, confirmText: t("remote.closeWindow"),
    }))) return;
    sendCombo(["Alt", "F4"]);
  }, [sendCombo]);

  // Отправка набранного: с Enter и без него. Без Enter это единственный способ
  // ввести логин или путь и не отправить форму раньше времени (N137).
  const sendTypedText = useCallback((withEnter: boolean) => {
    const text = typingText;
    if (text && browserTouch) {
      // В браузере текст уже уехал в поле по мере набора (handleTypingChange):
      // отправить его второй раз значит удвоить строку.
      setTypingText("");
    } else if (text) {
      send({ t: "txt", text });
      setTypingText("");
    }
    if (withEnter) {
      // Модификаторы применяем к Enter (Ctrl+Enter — «отправить» в чатах и
      // редакторах) и тут же гасим: к самому тексту они неприменимы.
      withModifiers(() => {
        send({ t: "k", k: "Enter", d: true });
        send({ t: "k", k: "Enter", d: false });
      });
    } else {
      const m = modifiersRef.current;
      // Текст ушёл без клавиши — подсветку снимаем. Иначе горящий Ctrl
      // срабатывал позже и неожиданно: Ctrl+клик по экрану.
      if (m.ctrl || m.alt || m.shift) {
        modifiersRef.current = { ctrl: false, alt: false, shift: false };
        setModifiers({ ctrl: false, alt: false, shift: false });
      }
    }
    haptic("light");
  }, [browserTouch, send, typingText, withModifiers]);

  // Ctrl/Alt рядом с полем ввода читаются однозначно: «нажми Ctrl, потом
  // клавишу». Так это теперь и работает — пока модификатор горит, следующая
  // набранная буква уходит на ПК СОЧЕТАНИЕМ и в поле не попадает. Раньше она
  // молча падала в строку, на ПК не уходило ничего, а по «↵» в консоль улетал
  // мусор вида «c».
  const handleTypingChange = useCallback((next: string) => {
    const m = modifiersRef.current;
    // В браузере печать идёт В ПОЛЕ СТРАНИЦЫ ПО МЕРЕ НАБОРА, а не по кнопке
    // «→»: на телефоне текст появляется в поле сразу, и от этого зависит всё
    // остальное — подсказки адреса, счётчик символов, проверка пароля,
    // активная кнопка «Далее». Отправляем только РАЗНИЦУ, иначе строка
    // перепечатывалась бы целиком на каждую букву.
    if (browserTouch && !m.ctrl && !m.alt) {
      const prev = typingText;
      const back = (times: number) => {
        for (let i = 0; i < times; i++) {
          send({ t: "k", k: "Backspace", d: true });
          send({ t: "k", k: "Backspace", d: false });
        }
      };
      if (next.startsWith(prev)) {
        const added = next.slice(prev.length);
        if (added) send({ t: "txt", text: added });
      } else if (prev.startsWith(next)) {
        back(prev.length - next.length);
      } else {
        // Автозамена телефона переписала слово целиком — стираем прежнее и
        // печатаем новое, иначе в поле останется смесь двух вариантов.
        back(prev.length);
        if (next) send({ t: "txt", text: next });
      }
      setTypingText(next);
      return;
    }
    if ((m.ctrl || m.alt) && next.length > typingText.length) {
      // Позиция вставки: сравниваем с прежним значением, а не режем хвост, —
      // курсор мог стоять и в середине строки.
      let i = 0;
      while (i < typingText.length && typingText[i] === next[i]) i++;
      const ch = next[i];
      if (ch) {
        // В национальной раскладке буква сочетания приходит кириллицей: у неё
        // нет виртуального кода, поэтому шлём латиницу той же клавиши.
        const key = isNonAsciiChar(ch) ? (layoutToLatin[ch.toLowerCase()] || ch) : ch.toLowerCase();
        withModifiers(() => {
          send({ t: "k", k: key, d: true });
          send({ t: "k", k: key, d: false });
        });
        haptic("medium");
        return; // поле не трогаем: буква ушла сочетанием, а не текстом
      }
    }
    setTypingText(next);
  }, [browserTouch, send, typingText, withModifiers]);

  // Open/close the on-screen keyboard. Opening is the tricky half: the mobile
  // OS raises the soft keyboard ONLY for a .focus() that runs inside the user
  // gesture. autoFocus fires after React's async re-render — too late, no
  // gesture → no keyboard. flushSync mounts the typing bar synchronously so the
  // <input> exists NOW, then we focus it while still inside the tap handler.
  const toggleKeyboard = useCallback(() => {
    haptic("light");
    if (showKb) {
      typingInputRef.current?.blur();
      setShowKb(false);
    } else {
      flushSync(() => setShowKb(true));
      typingInputRef.current?.focus();
    }
  }, [showKb]);

  const resetZoom = useCallback(() => {
    scaleRef.current = 1;
    offsetRef.current = { x: 0, y: 0 };
    applyTransform();
    setZoomLevel(1);
    haptic("light");
  }, [applyTransform]);

  const clickMouse = useCallback((button: "l" | "r") => {
    const cp = cursorPos.current;
    withModifiers(() => send({ t: "m", x: cp.x, y: cp.y, a: "click", b: button }));
    haptic(button === "r" ? "medium" : "light");
  }, [send, withModifiers]);

  const onCanvasMouseDown = useCallback((e: React.MouseEvent<HTMLElement>) => {
    if (browserTouch && Date.now() < compatMouseUntilRef.current) {
      mouseRef.current.down = false;
      return;
    }
    if (e.button !== 0 && e.button !== 1 && e.button !== 2) return;
    // preventDefault оставляем (он гасит выделение текста и перетаскивание
    // картинки), но фокус переносим руками: без него нажатия с физической
    // клавиатуры уходили бы в интерфейс, а не на ПК (N148).
    e.preventDefault();
    if (physicalKeyboard) stageRef.current?.focus?.();
    resetHideTimer();
    const button = e.button === 2 ? "r" : e.button === 1 ? "m" : "l";
    const [rx, ry] = toRelativePoint(e.clientX, e.clientY);
    mouseRef.current = { down: true, button };
    cursorPos.current = { x: rx, y: ry };
    withModifiers(() => send({ t: "m", x: rx, y: ry, a: "down", b: button }));
    drawCursor();
  }, [browserTouch, drawCursor, physicalKeyboard, resetHideTimer, send, toRelativePoint, withModifiers]);

  const onCanvasMouseMove = useCallback((e: React.MouseEvent<HTMLElement>) => {
    if (browserTouch && Date.now() < compatMouseUntilRef.current) return;
    const [rx, ry] = toRelativePoint(e.clientX, e.clientY);
    cursorPos.current = { x: rx, y: ry };
    queueMove(rx, ry);
    drawCursor();
  }, [browserTouch, drawCursor, queueMove, toRelativePoint]);

  const onCanvasMouseUp = useCallback((e: React.MouseEvent<HTMLElement>) => {
    if (browserTouch && Date.now() < compatMouseUntilRef.current) return;
    if (!mouseRef.current.down) return;
    e.preventDefault();
    const [rx, ry] = toRelativePoint(e.clientX, e.clientY);
    const button = mouseRef.current.button;
    mouseRef.current.down = false;
    cursorPos.current = { x: rx, y: ry };
    send({ t: "m", x: rx, y: ry, a: "up", b: button });
    drawCursor();
  }, [browserTouch, drawCursor, send, toRelativePoint]);

  const onCanvasMouseLeave = useCallback(() => {
    if (browserTouch && Date.now() < compatMouseUntilRef.current) return;
    if (!mouseRef.current.down) return;
    const cp = cursorPos.current;
    const button = mouseRef.current.button;
    mouseRef.current.down = false;
    send({ t: "m", x: cp.x, y: cp.y, a: "up", b: button });
  }, [browserTouch, send]);

  const onCanvasWheel = useCallback((e: React.WheelEvent<HTMLElement>) => {
    e.preventDefault();
    // Горизонтальное колесо/трекпад с компьютера — тоже на ПК (с 2.61.18).
    queueScroll(
      Math.max(-6, Math.min(6, -e.deltaY / 80)),
      Math.max(-6, Math.min(6, e.deltaX / 80)),
    );
  }, [queueScroll]);

  const toggleMod = (m: "ctrl" | "alt" | "shift") => {
    haptic("light");
    setModifiers((p) => {
      const next = { ...p, [m]: !p[m] };
      modifiersRef.current = next; // обработчики жестов читают ref, не state
      return next;
    });
  };

  // Чувствительность запоминаем, как режим и качество: возвращать своё значение
  // четырьмя нажатиями при каждом входе было унизительно (N141).
  const cycleSens = () => {
    haptic("light");
    const levels = remoteSensitivityLevels;
    const next = levels[(levels.indexOf(sensitivity) + 1) % levels.length];
    setSensitivity(next);
    try { localStorage.setItem("tgcontrol.remote.sensitivity", String(next)); } catch {}
  };

  // Выбор режима «Экран/Тачпад» запоминаем — иначе каждый вход на /remote
  // сбрасывал его обратно на «Экран».
  const changeControlMode = (mode: "screen" | "trackpad") => {
    haptic("light");
    setControlMode(mode);
    try { localStorage.setItem("tgcontrol.remote.controlMode", mode); } catch {}
  };

  // ── Actions ────────────────────────────────────────────────────

  // Кадр потока как фолбэк: сжат до ~960 px, мелкий текст в нём не читается,
  // поэтому это резерв на случай, когда агент не отдал полноразмерный снимок.
  const streamFrameBlob = useCallback((): Promise<Blob | null> => new Promise((resolve) => {
    const c = canvasRef.current;
    if (!c) { resolve(null); return; }
    // H.264 mode renders in <video>; blit the current frame to the (otherwise
    // idle) canvas once, only for the screenshot.
    const v = videoElRef.current;
    if (videoActive.current && v && v.videoWidth > 0) {
      c.width = v.videoWidth;
      c.height = v.videoHeight;
      get2d(c)?.drawImage(v, 0, 0, c.width, c.height);
    }
    try {
      c.toBlob((blob) => resolve(blob), "image/png");
    } catch {
      resolve(null);
    }
  }), [get2d]);

  const saveScreenshotBlob = useCallback(async (blob: Blob) => {
    const name = `screenshot-${Date.now()}.png`;
    const nav = navigator as Navigator & {
      canShare?: (data: { files?: File[] }) => boolean;
      share?: (data: { files?: File[]; title?: string }) => Promise<void>;
    };
    // В Telegram (особенно на iOS) скачивание через `<a download>` с blob-URL
    // молча не срабатывает: ни файла, ни ошибки. Системное «Поделиться» там
    // работает — снимок уходит в «Фото» или в любой чат. Пробуем его только в
    // Telegram, чтобы не менять привычный путь в APK и браузере.
    if (getTelegram() && typeof nav.share === "function") {
      const file = new File([blob], name, { type: "image/png" });
      if (!nav.canShare || nav.canShare({ files: [file] })) {
        try {
          await nav.share({ files: [file], title: name });
          return;
        } catch (e) {
          // Отмену пробрасываем (её отфильтрует вызывающий), а отказ самого API
          // («нужен жест пользователя») не должен съесть снимок — падаем на
          // обычное скачивание.
          if (isShareCancel(e)) throw e;
        }
      }
    }
    await saveBlob(blob, name);
  }, []);

  // «Скриншот» отдаёт полноразмерный PNG (та же ручка, что на экране «Система»)
  // и всегда говорит, чем закончилось: раньше панель просто закрывалась, а в
  // файл уходил мыльный кадр потока (N139/N55).
  const takeScreenshot = useCallback(async () => {
    if (screenshotBusy) return;
    setScreenshotBusy(true);
    setShowQuickActions(false);
    haptic("medium");
    toast(t("remote.screenshotPreparing"));
    let fullSize = true;
    let blob: Blob | null = null;
    let failure: unknown = null;
    try {
      const shot = await takeScreenshotApi({ display: activeDisplayRef.current, original: true });
      blob = base64ToBlob(shot.data, shot.mime || "image/png");
    } catch (e) {
      failure = e;
      fullSize = false;
      blob = await streamFrameBlob();
    }
    try {
      if (!blob) {
        toastError(failure ? mapApiError(failure) : t("remote.screenshotFailed"));
        return;
      }
      await saveScreenshotBlob(blob);
      if (fullSize) toastSuccess(t("remote.screenshotSaved"));
      else toastSuccess(t("remote.screenshotSavedFrame"));
    } catch (e) {
      if (!isShareCancel(e)) toastError(mapApiError(e));
    } finally {
      setScreenshotBusy(false);
    }
  }, [saveScreenshotBlob, screenshotBusy, streamFrameBlob, toast, toastError, toastSuccess]);

  // Панель открывается СРАЗУ по нажатию, содержимое ПК подгружается в неё
  // асинхронно. Раньше она была побочным эффектом удачного чтения буфера, и
  // отказ ПК (нет xclip, буфер занят) отрезал единственный путь отправить на
  // компьютер текст или картинку с телефона.
  const openClipboardPanel = useCallback(() => {
    haptic("light");
    setShowQuickActions(false);
    setClipboardError("");
    setClipboardLoading(true);
    setShowClipboard(true);
    send({ t: "clip_get" });
  }, [send]);

  const sendPhoneClipboard = useCallback(async () => {
    try {
      const text = await navigator.clipboard.readText();
      if (!text) {
        toastError(t("remote.clipPhoneEmpty"));
        setShowClipboard(true);
        return;
      }
      send({ t: "clip_set", text });
      haptic("medium");
    } catch {
      // Подлежащее в отказе — не «WebView» (человек не знает такого слова и не
      // может его починить), а сам факт: буфер не прочитался, вот поле рядом.
      setShowClipboard(true);
      toastError(t("remote.clipReadFailed"));
    }
  }, [send, toastError]);

  const sendManualClipboard = useCallback(() => {
    const text = manualClipboardText.trim();
    if (!text) {
      toastError(t("remote.clipManualEmpty"));
      return;
    }
    send({ t: "clip_set", text });
    haptic("medium");
  }, [manualClipboardText, send, toastError]);

  const sendPhoneImage = useCallback(async () => {
    try {
      const items = await navigator.clipboard.read();
      for (const item of items) {
        const imgType = item.types.find(t => t.startsWith("image/"));
        if (imgType) {
          const blob = await item.getType(imgType);
          if (blob.size > remoteImageClipboardMax) {
            toastError(t("remote.clipImageTooLarge"));
            return;
          }
          const buf = await blob.arrayBuffer();
          const bytes = new Uint8Array(buf);
          let b64 = "";
          for (let i = 0; i < bytes.length; i += 8192) {
            b64 += String.fromCharCode(...bytes.subarray(i, i + 8192));
          }
          send({ t: "clip_set_image", data: btoa(b64), mime: imgType });
          haptic("medium");
          return;
        }
      }
      toastError(t("remote.clipNoImage"));
    } catch {
      toastError(t("remote.clipImageFailed"));
    }
  }, [send, toastError]);

  const copyToPhone = useCallback(async () => {
    if (!clipboardText) {
      toastError(t("remote.emptyClipboard"));
      return;
    }
    try {
      await navigator.clipboard.writeText(clipboardText);
      haptic("medium");
      toastSuccess(t("remote.clipCopied"));
    } catch {
      toastError(t("remote.clipWriteFailed"));
    }
  }, [clipboardText, toastError, toastSuccess]);

  const switchDisplay = useCallback((id: number) => {
    send({ t: "display", id });
    activeDisplayRef.current = id;
    setActiveDisplay(id);
    haptic("light");
  }, [send]);

  // «Эконом» по нажатию: единственный путь на iOS/Telegram-iOS, где признака
  // мобильной сети у браузера нет (N140), и он же — единственное действие,
  // которое реально помогает при «Терпимо/Плохо» в шите качества.
  const enableSaverProfile = useCallback(() => {
    setShowQuality(false);
    selectStreamProfile("saver");
  }, [selectStreamProfile]);

  /**
   * Звук компьютера включается только по нажатию: браузер не даст завести
   * AudioContext без жеста человека, да и внезапный звук из вкладки — это
   * последнее, чего ждёшь от удалённого экрана.
   */
  const toggleAudio = useCallback(() => {
    haptic();
    if (audioState === "on") {
      transportRef.current?.send({ t: "audio", on: false });
      void audioPlayerRef.current?.stop();
      audioPlayerRef.current = null;
      if (audioWaitRef.current) {
        window.clearTimeout(audioWaitRef.current);
        audioWaitRef.current = null;
      }
      setAudioState("off");
      return;
    }
    if (!audioPlayerRef.current) audioPlayerRef.current = new RemoteAudioPlayer();
    // «Включаю звук…» ставим ДО запуска контекста: между нажатием и первым
    // куском звука лежит дорога до компьютера и обратно, и всё это время
    // кнопка выглядела ненажатой — человек жал её второй раз и выключал то,
    // что только что включил.
    setAudioState("pending");
    // Заводим контекст ПРЯМО в обработчике нажатия — позже жест уже «остынет»
    // и Safari откажет.
    void audioPlayerRef.current.start().then(() => {
      transportRef.current?.send({ t: "audio", on: true });
      // Компьютер может не ответить вовсе (старый агент, Linux, потерянный
      // кадр). Без срока ожидание «Включаю звук…» осталось бы навсегда.
      if (audioWaitRef.current) window.clearTimeout(audioWaitRef.current);
      audioWaitRef.current = window.setTimeout(() => {
        audioWaitRef.current = null;
        setAudioState("off");
        toastError(t("remote.audioNoAnswer"));
      }, 7000);
    }).catch(() => {
      // Отказ AudioContext (Safari без «настоящего» жеста) — тоже конец
      // ожидания: иначе кнопка залипает в «Включаю звук…».
      setAudioState("off");
      toastError(t("remote.audioStartFailed"));
    });
  }, [audioState, toastError]);

  // Явный возврат к H.264: отметку «этому ПК H.264 не даётся» снимаем и
  // переподключаемся. Раньше выхода из совместимого режима не было вовсе (N136).
  const retryH264 = useCallback(() => {
    clearJpegFallbackMark();
    setJpegForced(false);
    preferH264Ref.current = true;
    setShowQuality(false);
    retryRemote();
  }, [retryRemote]);

  // Чип расхода трафика: один раз за сеанс после порога, дальше гаснет сам —
  // висящая вечно плашка поверх рабочего стола была бы хуже молчания.
  useEffect(() => {
    if (trafficHintShownRef.current) return;
    if (streamProfile === "saver") return;
    if (trafficBytes < remoteTrafficHintBytes) return;
    trafficHintShownRef.current = true;
    setTrafficHint(true);
  }, [streamProfile, trafficBytes]);

  useEffect(() => {
    if (!trafficHint) return;
    const timer = window.setTimeout(() => setTrafficHint(false), 15000);
    return () => window.clearTimeout(timer);
  }, [trafficHint]);

  useEffect(() => {
    if (networkSaverHint !== "offer") return;
    const timer = window.setTimeout(() => setNetworkSaverHint(""), 15000);
    return () => window.clearTimeout(timer);
  }, [networkSaverHint]);

  // ── Computed ───────────────────────────────────────────────────

  const aspect = screenInfo ? screenInfo.sw / screenInfo.sh : 16 / 9;
  // In H.264 mode the server's ack-RTT is idle — the ICE RTT from getStats is
  // the live number. Prefer it when present.
  const hudRtt = cstats?.rtt ?? stats.rtt;
  const qualityLevel = hudRtt === 0
    ? "checking"
    : (hudRtt >= 180 || (cstats?.lossPct ?? 0) > 5 || decodeDrops > 8)
      ? "bad"
      : (hudRtt >= 70 || (cstats?.lossPct ?? 0) > 1)
        ? "fair"
        : "fast";
  const qualityLabel = t(`remote.quality.${qualityLevel}`);
  const rttColor = qualityLevel === "fast"
    ? "#4caf50"
    : qualityLevel === "fair"
      ? "#ff9800"
      : qualityLevel === "bad" ? "#f44336" : "#8b949e";
  const profileLabel = t(`remote.profile.${streamProfile}`);
  // Имя машины: у человека их несколько (главная страница как раз про
  // переключение), а «Экран ПК» не отвечает на вопрос «чей это экран». То же имя
  // уходит в офлайн-состояние — иначе оно ругается на безымянный компьютер.
  //
  // Через humanDeviceName, а не напрямую: при подключении по локальной сети в
  // конфиге лежит адрес, и заголовок этого экрана крупно звал компьютер
  // «127.0.0.1». Фильтр общий (devices.ts) — он же у карточки готовности и чипа
  // в шапке. Запасное имя просим ПУСТОЕ: у трёх мест ниже оно разное — на
  // карточке запуска «Этот компьютер», в шапке стрима название раздела, а
  // офлайн-состоянию безымянную машину лучше не называть вовсе, иначе оно
  // ругается в кавычках на «Этот компьютер».
  const deviceName = humanDeviceName(getSelectedDeviceName(), "");
  // Связь просела: единственное действие, которое реально помогает, — «Эконом».
  // Раньше шит качества показывал восемь строк цифр и «Скопировать диагностику».
  const offerSaver = (qualityLevel === "bad" || qualityLevel === "fair") && streamProfile !== "saver";
  const bitrate = stats.kbps >= 1000
    ? `${(stats.kbps / 1000).toFixed(1)}Mbps`
    : `${Math.round(stats.kbps)}kbps`;
  const stageLabel = t(`remote.stage.${connectionStage}`);
  const diagnostics = [
    `quality=${qualityLevel}`,
    `rtt=${hudRtt}ms`,
    `fps=${fps}`,
    `sent_fps=${stats.sentFps}`,
    `width=${stats.width}`,
    `bitrate=${bitrate}`,
    `jitter_buffer=${cstats?.jbMs ?? "-"}ms`,
    `decode=${cstats?.decMs ?? "-"}ms`,
    `loss=${cstats?.lossPct ?? "-"}%`,
    `route=${cstats?.route || "-"}`,
    `transport=${transportKind || "-"}${h264Active ? "-h264" : ""}`,
    `drops=${decodeDrops}`,
    `traffic=${formatTraffic(trafficBytes)}`,
  ].join("\n");

  const copyDiagnostics = async () => {
    try {
      await navigator.clipboard.writeText(diagnostics);
      toastSuccess(t("remote.diagCopied"));
    } catch {
      toastError(t("remote.clipWriteFailed"));
    }
  };

  const toggleHudDetails = () => {
    setShowHudDetails((value) => {
      const next = !value;
      try { localStorage.setItem("tgcontrol.remote.hudDetails", next ? "1" : "0"); } catch {}
      return next;
    });
  };

  // ── Экран запуска ──────────────────────────────────────────────
  //
  // До нажатия «Включить экран» человек видит отдельный экран (компонент
  // RemoteLaunchScreen): раньше вкладка сразу отдавала управление компьютером —
  // сокет открывался при монтировании, и палец по картинке уже кликал по чужим
  // окнам. Разметка вынесена целиком: она не касается ни одного из состояний
  // живого стрима, а читается и правится чаще прочего.
  if (!streaming) {
    return (
      <RemoteLaunchScreen
        deviceName={deviceName}
        linkState={linkState}
        knownDisplays={knownDisplays}
        launchNotice={launchNotice}
        serviceMode={serviceMode}
        noDisplay={noDisplay}
        platform={platform}
        vbGate={vbGate}
        vbRunning={vbRunning}
        vbStarting={vbStarting}
        vbHint={vbHint}
        vbError={vbError}
        onBack={goBack}
        onStart={startStream}
        onRecheck={recheckLink}
        onVbStart={handleVbStart}
        onVbStop={handleVbStop}
      />
    );
  }

  // ── Render ─────────────────────────────────────────────────────

  // Адресная пилюля нижней панели: только хост (полный URL не влезает), а
  // замочек у https — тот же знак «соединение настоящее», что в любом браузере.
  // Пустая вкладка — это не сайт: в пилюле должно быть приглашение «адрес или
  // запрос», а не «new-tab-page» (внутреннее имя страницы Chrome).
  const pillHost = isBlankPage(browserPage?.url) ? "" : hostOf(browserPage?.url || "");
  const pillSecure = (browserPage?.url || "").startsWith("https:");

  return (
    <div className={`remote-page ${showUI && !showKb ? "nav-visible" : ""}`}>
      {/* Header — всегда видим: авто-скрытие уводило его за верх экрана
          (translateY -100%), тап по месту кнопки проваливался в canvas (ложный
          клик по ПК + просто возврат панели), а сама кнопка не нажималась.
          Скрываем по таймауту только крупные нижние контролы. */}
      <div className="remote-header">
        <button className="back-btn" aria-label={t("generic.back")} onClick={goBack}>{"\u2190"}</button>
        <span className="remote-title" title={deviceName || t("remote.title")}>
          {deviceName || t("remote.title")}
        </span>

        {/* На телефоне (≤430 px) сегмент остаётся, но только значками: раньше
            он прятался целиком, и человек, не открывший «⋮», не узнавал, что
            режим тачпада вообще существует (разбор 02.09.2026). */}
        <div className="remote-mode-toggle remote-header-modes" aria-label={t("remote.sectionControl")}>
          <button className={controlMode === "screen" ? "active" : ""}
            aria-label={t("remote.modeScreen")} aria-pressed={controlMode === "screen"}
            onClick={() => changeControlMode("screen")}>
            <span className="remote-mode-icon" aria-hidden>{"🖵"}</span>
            <span className="remote-mode-label">{t("remote.modeScreen")}</span>
          </button>
          <button className={controlMode === "trackpad" ? "active" : ""}
            aria-label={t("remote.modeTrackpad")} aria-pressed={controlMode === "trackpad"}
            onClick={() => changeControlMode("trackpad")}>
            <span className="remote-mode-icon" aria-hidden>{"☝"}</span>
            <span className="remote-mode-label">{t("remote.modeTrackpad")}</span>
          </button>
        </div>

        {/* Чип связи — это ДВЕРЬ в «Картинку и звук», а не показание прибора.
            Раньше он выглядел строкой телеметрии («Быстро · 12ms»), и человек не
            догадывался, что за ним лежат режимы картинки и звук компьютера.
            Значок ползунков + подпись словами говорят, куда ведёт нажатие;
            цветная точка и задержка остаются — по ним и смотрят на связь.

            ЗАМЕР (390px, стенд с QA_VBROWSER_UI=1): до правки чип обрезался на
            54 px из 101 — «12ms» на телефоне не было видно вовсе, и обрезка
            ничем себя не выдавала. После: чип 142 px без обрезки, scrollWidth
            шапки равен clientWidth на 360/390/430/1280. Слово «Быстро» и
            технические детали уходят в .remote-hud-extra (прячутся ≤430px). */}
        {connected && (
          <button className="remote-hud" onClick={() => setShowQuality(true)}
            aria-label={t("remote.hudDoorAria", { quality: qualityLabel, ms: hudRtt })}>
            <IconSliders size={13} className="remote-hud-door" />
            <span className="remote-hud-word">{t("remote.imageAndSound")}</span>
            <span className="remote-hud-dot" style={{ background: rttColor }} />
            <span className="remote-hud-extra">{qualityLabel}</span>
            <span className="remote-hud-ms">{hudRtt}ms</span>
            {showHudDetails && (
              <span className="remote-hud-extra">{fps}fps · {stats.width}w · {bitrate} · {formatTraffic(trafficBytes)}</span>
            )}
          </button>
        )}

        <button className={`remote-kb-btn ${showKb ? "active" : ""}`}
          onClick={toggleKeyboard}
          aria-label={t("remote.helpKeyboardTitle")}>{"\u2328"}</button>

        {/* Динамик — только когда трек вообще пришёл (виртуальный браузер со
            звуком); у обычного десктопа кнопки нет, чтобы не обещать лишнего.
            В браузерном режиме шапки звук переехал в меню ⋮ нижней панели —
            там вся оболочка браузера. */}
        {hasAudio && !browserTouch && (
          <button className={`remote-kb-btn ${soundMuted ? "" : "active"}`}
            onClick={toggleSound}
            aria-label={t(soundMuted ? "remote.unmuteSound" : "remote.muteSound")}>
            {soundMuted ? "\u{1F507}" : "\u{1F50A}"}
          </button>
        )}

        <button className="remote-actions-btn"
          onClick={() => { haptic("light"); setShowQuickActions(true); }}
          aria-label={t("remote.menu")}>{"\u22EE"}</button>
      </div>

      {/* Main area: screen + trackpad */}
      <div className={`remote-body ${controlMode === "screen" ? "screen-mode" : "pad-mode"}`}>
        <div className={`remote-canvas-wrap${browserTouch ? " browser-mode" : ""}`} ref={wrapRef}>
          {/* stage: zoom/pan transform container. Gestures land here (they were
              on the canvas before video went direct); <video> renders the H.264
              track natively, canvas serves the JPEG fallback + screenshots; the
              DOM cursor + tap ripple ride the same transform. */}
          <div ref={stageRef} className="remote-stage"
            // Фокусируемый экран нужен только там, где есть физическая
            // клавиатура: тогда её нажатия уходят на ПК напрямую (N148).
            tabIndex={physicalKeyboard ? 0 : undefined}
            // Имя рабочей поверхности: без него скринридер объявлял здесь два
            // безымянных объекта (<video> и <canvas>) и ни слова о том, чей это
            // экран. Картинку несёт контейнер, поэтому сами элементы скрыты от
            // дерева доступности — на воспроизведение это не влияет.
            aria-label={t("remote.screenSurface", { name: deviceName || t("remote.title") })}
            style={{ aspectRatio: `${aspect}`, display: screenInfo ? "block" : "none" }}
            onTouchStart={onCanvasTouchStart} onTouchMove={onCanvasTouchMove}
            onTouchEnd={onCanvasTouchEnd} onTouchCancel={onCanvasTouchEnd}
            onMouseDown={onCanvasMouseDown} onMouseMove={onCanvasMouseMove}
            onMouseUp={onCanvasMouseUp} onMouseLeave={onCanvasMouseLeave}
            onWheel={onCanvasWheel} onContextMenu={(e) => e.preventDefault()}>
            <video ref={videoElRef} className="remote-video" muted autoPlay playsInline
              onLoadedData={markFirstFrame} aria-hidden="true"
              style={{ display: h264Active ? "block" : "none" }} />
            {/* Звук виртуального браузера — отдельный трек/стрим от видео,
                поэтому свой элемент (без controls он невидим). */}
            <audio ref={audioElRef} autoPlay />
            <canvas ref={canvasRef} className="remote-canvas" aria-hidden="true"
              style={{ display: h264Active ? "none" : "block" }} />
            {/* Курсор — примета рабочего стола. В браузере под пальцем
                страница, и стрелка мыши посреди неё только сбивает: в телефоне
                курсора нет. */}
            <div ref={cursorRef} className={`remote-cursor${browserTouch ? " hidden" : ""}`} aria-hidden="true">
              <svg width="22" height="22" viewBox="0 0 22 22">
                <path d="M2 1 L2 16 L6 12.6 L9 19 L11.6 17.9 L8.7 11.7 L14 11.2 Z"
                  fill="#fff" stroke="#000" strokeWidth="1.2" strokeLinejoin="round" />
              </svg>
            </div>
            <div ref={rippleRef} className="remote-ripple" aria-hidden="true" />
          </div>

          {/* Стойкие плашки поверх кадра. Живут РЯДОМ со stage: внутри него их
              z-index заперт собственным контекстом наложения
              (will-change:transform), оверлей подключения лёг бы сверху и
              перехватил нажатие кнопки. Рисуем РОВНО ОДНУ (самую важную) и
              только когда оверлея нет — пока он висит, тот же текст с рабочей
              кнопкой показывает он сам.
              Порядок: захват не идёт → ввод не доходит → декодер не собрался →
              браузер скоро остановится по простою. */}
          {connected && firstFrame && (captureFailed ? (
            <div className="remote-stuck">
              <div className="remote-stuck-text">{t("remote.captureFailed")}</div>
              <button className="btn btn-primary btn-sm" onClick={retryRemote}>
                {t("remote.retry")}
              </button>
            </div>
          ) : (inputWarn || vbInput.missing) ? (
            <div className="remote-stuck">
              {/* Нечем нажимать — говорим это ДО первого тапа (машина сама
                  сообщает, что xdotool нет) и даём кнопку вместо совета пойти
                  в SSH. Заблокированный рабочий стол — по-прежнему «Повторить»:
                  там ставить нечего. */}
              <div className="remote-stuck-text">
                {inputWarn === "input_blocked"
                  ? t("remote.inputBlocked")
                  : vbInput.missing ? t("remote.inputMissing") : t("remote.inputUnavailable")}
              </div>
              {vbInput.error ? <div className="remote-stuck-note">{vbInput.error}</div> : null}
              {vbInput.missing && vbInput.canInstall ? (
                <button className="btn btn-primary btn-sm" onClick={handleInstallInput}
                  disabled={vbInput.installing}>
                  {vbInput.installing ? t("remote.inputInstalling") : t("remote.inputInstall")}
                </button>
              ) : vbInput.missing && vbInput.hint ? (
                <div className="remote-stuck-note remote-stuck-cmd">{vbInput.hint}</div>
              ) : (
                <button className="btn btn-primary btn-sm"
                  onClick={() => { setInputWarn(""); retryRemote(); }}>
                  {t("remote.retry")}
                </button>
              )}
            </div>
          ) : videoStuck ? (
            <div className="remote-stuck">
              <div className="remote-stuck-text">{t("remote.videoStuck")}</div>
              <button className="btn btn-primary btn-sm" onClick={() => { setVideoStuck(false); retryRemote(); }}>
                {t("remote.videoStuckRetry")}
              </button>
            </div>
          ) : vbIdleWarn > 0 ? (
            <div className="remote-stuck">
              <div className="remote-stuck-text">
                {t("remote.vbIdleWarn", { min: Math.max(1, Math.ceil(vbIdleWarn / 60)) })}
              </div>
              <button className="btn btn-primary btn-sm" onClick={handleVbKeepalive}>
                {t("remote.vbIdleKeep")}
              </button>
            </div>
          ) : null)}

          {/* Шапка браузера: в кадре её нет — там только страница. Без неё
              человек не знает, на каком он сайте и грузится ли он. Вкладки и
              навигация живут в нижней панели (браузерный режим), здесь остаётся
              информативная строка; кнопка вкладок в ней нужна только без
              панели — в режиме «Тачпад». */}
          {/* В браузерном режиме шапки НЕТ: адрес живёт в пилюле нижней панели,
              а строка сверху дублировала его вторым экземпляром и отбирала у
              страницы полосу высоты (у мобильного Chrome над страницей пусто).
              Режим «Тачпад» и компьютер шапку сохраняют: там нижней панели нет
              и адрес показать больше негде. */}
          {/* Компьютер: вкладки СТРОКОЙ, как в десктопном браузере. Прятать их
              в шит на широком экране незачем — там мышь, и переключение
              вкладки должно стоить один щелчок, а не «открыть список → выбрать».
              Условие по ширине, а не по режиму: в окне на компьютере человек
              может выбрать и «Экран». */}
          {vbDirect && wideScreen && (
            <div className="remote-tabstrip">
              {browserTabs.map((tab) => (
                <button key={tab.id}
                  className={`remote-tabstrip-tab${tab.active ? " active" : ""}`}
                  onClick={() => void openTab(tab.id)} title={tab.title || tab.url}>
                  {tab.icon
                    ? <img className="remote-tabstrip-icon" src={tab.icon} alt="" aria-hidden="true" />
                    : <span className="remote-tabstrip-dot" aria-hidden="true" />}
                  <span className="remote-tabstrip-title">{tab.title || hostOf(tab.url) || t("remote.browserNewTab")}</span>
                  <span className="remote-tabstrip-close" role="button" tabIndex={-1}
                    aria-label={t("remote.browserCloseTab")}
                    onClick={(e) => { e.stopPropagation(); void dropTab(tab.id); }}>×</span>
                </button>
              ))}
              <button className="remote-tabstrip-add" onClick={() => void addTab()}
                title={t("remote.tabsNewHint")} aria-label={t("remote.browserNewTab")}>+</button>
            </div>
          )}

          {vbDirect && !browserTouch && (
            <div className="remote-browser-bar">
              <button className="remote-browser-url" onClick={openAddress}>
                <span className="remote-browser-title">
                  {browserPage?.title || t("remote.browserNewTab")}
                </span>
                <span className="remote-browser-host">{hostOf(browserPage?.url || "")}</span>
              </button>
              <button className="remote-browser-tabs" onClick={openTabsSheet}>
                {browserTabs.length || 1}
              </button>
              {browserPage?.loading && <div className="remote-browser-progress" aria-hidden="true" />}
            </div>
          )}

          {/* Пустая вкладка: вместо внутренней страницы Chrome (десктопной и
              чужой) — свой стартовый экран с поиском, частыми сайтами и
              закладками. Лежит ПОВЕРХ кадра: появляется мгновенно и работает,
              пока браузер на машине ещё поднимается. */}
          {browserTouch && isBlankPage(browserPage?.url) && !addressOpen && (
            <BrowserStartPage
              visible
              onSearchFocus={openAddress}
              onOpen={(text) => {
                const url = searchOrUrl(text);
                if (!url) return;
                void navigateVBrowser({ url }).catch((e: any) => toastError(mapApiError(e)));
              }} />
          )}

          {/* Натяжение сверху — та самая «резинка» телефонного браузера. */}
          {browserTouch && (pullProgress > 0 || refreshing) && (
            <div className="vb-pull" style={{ opacity: refreshing ? 1 : Math.max(0.3, pullProgress) }}>
              <span className={`vb-pull-spinner${refreshing ? " spinning" : ""}`}
                style={{ transform: `rotate(${Math.round(pullProgress * 270)}deg)` }}
                aria-hidden="true">{"↻"}</span>
              {refreshing && <span className="vb-pull-text">{t("remote.refreshing")}</span>}
            </div>
          )}

          {/* Поиск по странице: длинную статью иначе пришлось бы листать
              экранами, а каждый экран здесь — это ещё и кадры по сети. */}
          {browserTouch && findOpen && <BrowserFindBar onClose={() => setFindOpen(false)} />}

          {/* Нижняя панель — как у настоящего мобильного браузера: навигация,
              адрес-пилюля по центру, вкладки со счётчиком в квадратике и меню.
              Не автоскрываемая: уезжает только вместе со скроллом страницы. */}
          {browserTouch && (
            <div className={`remote-browser-toolbar${chromeHidden ? " chrome-hidden" : ""}`}>
              <button className="remote-browser-toolbtn" onClick={() => void browserGo("back")}
                disabled={!browserPage?.can_back} aria-label={t("remote.browserBack")}>
                {"\u2190"}
              </button>
              <button className="remote-browser-toolbtn" onClick={() => void browserGo("forward")}
                disabled={!browserPage?.can_forward} aria-label={t("remote.browserForward")}>
                {"\u2192"}
              </button>
              <button className="remote-browser-toolbtn" onClick={() => void browserGo("reload")}
                aria-label={t("remote.browserReload")}>
                {"\u27F3"}
              </button>
              <button className="remote-browser-pill" onClick={openAddress}>
                {pillSecure ? (
                  <svg width="13" height="13" viewBox="0 0 24 24" aria-hidden="true">
                    <rect x="5" y="11" width="14" height="9" rx="2" fill="currentColor" />
                    <path d="M8 11V8a4 4 0 1 1 8 0v3" fill="none" stroke="currentColor" strokeWidth="2" />
                  </svg>
                ) : (
                  <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" aria-hidden="true">
                    <circle cx="12" cy="12" r="9" strokeWidth="2" />
                    <path d="M3 12h18M12 3c2.8 3.4 2.8 14.6 0 18M12 3c-2.8 3.4-2.8 14.6 0 18" strokeWidth="1.5" />
                  </svg>
                )}
                <span className={`remote-browser-pill-host${pillHost ? "" : " placeholder"}`}>
                  {pillHost || t("remote.browserAddressPlaceholder")}
                </span>
              </button>
              <button className="remote-browser-tabcount" onClick={openTabsSheet}
                aria-label={t("remote.browserTabs")}>
                <span>{browserTabs.length || 1}</span>
              </button>
              <button className="remote-browser-toolbtn" onClick={openBrowserMenu}
                aria-label={t("remote.menu")}>
                {"\u22EE"}
              </button>
            </div>
          )}

          {/* Плавающий «Стоп» — только там, где нет нижней панели с меню ⋮
              (обычный десктоп и режим «Тачпад»): в браузерном режиме эта
              команда живёт в меню панели, а кнопка перекрывала страницу. */}
          {vbRunning && !browserTouch && (
            <button className={`remote-vb-floating${vbDirect ? " below-bar" : ""}`} onClick={handleVbStop}>
              <span aria-hidden="true">{"\uD83C\uDF10"}</span>
              {t("remote.vbStop")}
            </button>
          )}

          {rotateHintVisible && (
            <button className="remote-tip remote-rotate-tip" onClick={dismissRotateHint}>
              {t("remote.rotateHint")} <span aria-hidden="true">{"\u00D7"}</span>
            </button>
          )}

          {networkSaverHint === "auto" && (
            <button className="remote-tip remote-network-tip" onClick={() => setNetworkSaverHint("")}>
              {t("remote.networkSaverHint")} <span aria-hidden="true">{"\u00D7"}</span>
            </button>
          )}

          {/* Признака мобильной сети у браузера нет (iOS, Telegram-iOS):
              «Эконом» не включается сам, поэтому предлагаем его нажатием. */}
          {networkSaverHint === "offer" && (
            <button className="remote-tip remote-network-tip" onClick={enableSaverProfile}>
              {t("remote.networkSaverOffer")}
            </button>
          )}

          {trafficHint && (
            <button className="remote-tip remote-traffic-tip" onClick={enableSaverProfile}>
              {t("remote.trafficSaverHint", { size: formatTraffic(trafficBytes) })}
            </button>
          )}

          {/* Оверлей подключения — РОВНО ОДНА плашка на состояние: он лежит
              поверх всего внутри .remote-canvas-wrap и перехватывает нажатия,
              поэтому вторая плашка под ним была бы нерабочей. */}
          {(!connected || !firstFrame) && (
            <div className="remote-overlay">
              {pcOffline ? (
                // Компьютер выключен/спит: то же честное состояние, что на
                // «Файлах», «Терминалах» и «Системе», — вместо «Ошибка
                // подключения» и минуты «Переподключение… (7/10)».
                <OfflineState
                  hostname={deviceName || undefined}
                  onRetry={retryRemote}
                  onDevices={getMode() === "cloud" ? () => navigate("/infrastructure") : undefined}
                />
              ) : captureFailed && !connectionError ? (
                // Захват экрана на ПК падает: соединение живое, агент повторяет
                // попытки — вместо бесконечного «Жду первый кадр» показываем
                // причину и кнопку. Плашка уйдёт сама, когда придёт кадр (N1).
                <>
                  <span>{t("remote.captureFailed")}</span>
                  <small className="remote-stage-label">
                    {connectSeconds} {t("remote.secondsShort")}
                  </small>
                  <button className="remote-retry-btn" onClick={retryRemote}>
                    {t("remote.retry")}
                  </button>
                </>
              ) : videoStuck && !connectionError ? (
                // Декодер не собрался: кнопка появляется сразу — ждать 8 секунд
                // перед единственным работающим выходом было нечестно. Работа
                // при этом идёт (агент переключается на JPEG), поэтому спиннер
                // остаётся; фатальная ошибка, если придёт, вытеснит эту ветку.
                <>
                  <div className="spinner" style={{ width: 28, height: 28 }} />
                  <span>{t("remote.videoStuck")}</span>
                  <small className="remote-stage-label">
                    {t("remote.compatSwitching")} · {connectSeconds} {t("remote.secondsShort")}
                  </small>
                  <button className="remote-retry-btn"
                    onClick={() => { setVideoStuck(false); retryRemote(); }}>
                    {t("remote.videoStuckRetry")}
                  </button>
                </>
              ) : (
                <>
                  {/* Спиннер — только пока реально подключаемся; при фатальной
                      ошибке крутить его = ложная надежда (UX ТОП-10 #6). */}
                  {!connectionError && <div className="spinner" style={{ width: 28, height: 28 }} />}
                  <span>{connectionError || (reconnecting
                    ? t("remote.reconnecting", { num: reconnectNum, max: maxReconnect })
                    : t("remote.connecting"))}</span>
                  {!connectionError && (
                    <small className="remote-stage-label">
                      {stageLabel} · {connectSeconds} {t("remote.secondsShort")}
                      {previewReady ? ` · ${t("remote.previewReady")}` : ""}
                    </small>
                  )}
                  {!connectionError && connectSeconds >= 8 && (
                    <span className="remote-slow-hint">{t("remote.connectSlow")}</span>
                  )}
                  {(connectionError || connectSeconds >= 8) && (
                    <button className="remote-retry-btn" onClick={retryRemote}>
                      {t("remote.retry")}
                    </button>
                  )}
                </>
              )}
            </div>
          )}
        </div>

        <div className={`remote-pad ${dragging ? "dragging" : ""}`}
          onTouchStart={onPadTouchStart} onTouchMove={onPadTouchMove}
          onTouchEnd={onPadTouchEnd} onTouchCancel={onPadTouchEnd}
        >
          <div className="remote-pad-icon">{"\uD83D\uDDB1"}</div>
          <div className="remote-pad-label">{dragging ? t("remote.drag") : t("remote.trackpad")}</div>
          <div className="remote-pad-grid" />
          <div className="remote-pad-actions">
            <button
              onTouchStart={(e) => e.stopPropagation()}
              onTouchEnd={(e) => e.stopPropagation()}
              onClick={(e) => { e.stopPropagation(); clickMouse("l"); }}
            >
              {t("remote.leftClick")}
            </button>
            <button
              onTouchStart={(e) => e.stopPropagation()}
              onTouchEnd={(e) => e.stopPropagation()}
              onClick={(e) => { e.stopPropagation(); clickMouse("r"); }}
            >
              {t("remote.rightClick")}
            </button>
          </div>
        </div>
      </div>

      {/* Старт-панель виртуального браузера: headless-агент без дисплея —
          предлагаем поднять Xvfb + браузер, дальше обычный RD-стрим. */}
      {vbGate === "needed" && (
        <div className="remote-vb-panel">
          <div className="remote-vb-icon">{"\uD83C\uDF10"}</div>
          <div className="remote-vb-title">{t("remote.vbTitle")}</div>
          <div className="remote-vb-desc">{t("remote.vbDesc")}</div>
          {vbHint ? <div className="remote-vb-hint">{vbHint}</div> : null}
          {vbError ? <div className="remote-vb-error">{vbError}</div> : null}
          <button
            className="btn btn-primary remote-vb-start"
            onClick={handleVbStart}
            disabled={vbStarting}
          >
            {vbStarting ? t("remote.vbStarting") : t("remote.vbStart")}
          </button>
        </div>
      )}

      {/* «Ручка» вместо полного исчезновения контролов: раньше от ряда ЛКМ/ПКМ,
          Esc/Tab/Enter и нижней навигации не оставалось ни следа, и вернуть их
          можно было только касанием картинки — а оно намеренно НЕ кликает, так
          что первый тап по ссылке на ПК пропадал впустую. */}
      {!showUI && !showKb && !browserTouch && vbGate !== "needed" && (
        <button className="remote-ui-handle"
          onClick={() => { haptic("light"); resetHideTimer(); }}
          aria-label={t("remote.showControls")}>
          <span aria-hidden="true">{"\u2303"}</span>
          {t("remote.showControls")}
        </button>
      )}

      {/* Controls: в браузерном режиме их место заняла нижняя панель браузера
          (выше) — пульт мыши и клавиш остаётся рабочему столу и «Тачпаду». */}
      {!browserTouch && (
      <div className={`remote-controls ${showUI && !showKb ? "" : "hidden"}`}>
        {/* Пульт мыши и клавиш — только когда смотрят РАБОЧИЙ СТОЛ. У браузера
            под пальцем страница, а не курсор: ряды «ЛКМ/ПКМ», модификаторов,
            служебных клавиш и стрелок отъедали половину экрана телефона ровно
            там, где должна быть сама страница (живая жалоба: «не вот эти
            стрелочки»). Клавиатура вызывается своей кнопкой в шапке. */}
        {!browserTouch && <div className="remote-clicks">
          <button className="remote-mouse-btn" onClick={() => clickMouse("l")}>{t("remote.leftClick")}</button>
          <button className="remote-mouse-btn" onClick={() => clickMouse("r")}>{t("remote.rightClick")}</button>
        </div>}
        {!browserTouch && <div className="remote-mods">
          <button className={`remote-mod ${modifiers.ctrl ? "active" : ""}`} onClick={() => toggleMod("ctrl")}>Ctrl</button>
          <button className={`remote-mod ${modifiers.alt ? "active" : ""}`} onClick={() => toggleMod("alt")}>Alt</button>
          <button className={`remote-mod ${modifiers.shift ? "active" : ""}`} onClick={() => toggleMod("shift")}>Shift</button>
        </div>}
        {!browserTouch && <div className="remote-keys">
          <button className="remote-key" onClick={() => sendKey("Escape")}>Esc</button>
          <button className="remote-key" onClick={() => sendKey("Tab")}>Tab</button>
          <button className="remote-key" onClick={() => sendKey("Enter")}>{"\u21B5"}</button>
          <button className="remote-key" onClick={() => sendKey("Backspace")}>{"\u232B"}</button>
          <button className="remote-key" onClick={() => sendKey("Delete")}>Del</button>
          <button className="remote-key" onClick={() => sendKey("Meta")}>{"\u229E"}</button>
        </div>}
        {/* \u0420\u044f\u0434 \u0434\u043b\u044f \u0432\u0438\u0440\u0442\u0443\u0430\u043b\u044c\u043d\u043e\u0433\u043e \u0431\u0440\u0430\u0443\u0437\u0435\u0440\u0430 \u2014 \u0442\u043e\u043b\u044c\u043a\u043e \u043a\u043e\u0433\u0434\u0430 \u043e\u043d \u0437\u0430\u043f\u0443\u0449\u0435\u043d (\u044d\u043a\u0440\u0430\u043d
            \u043f\u043e\u043a\u0430\u0437\u044b\u0432\u0430\u0435\u0442 \u0440\u043e\u0432\u043d\u043e \u0442\u043e, \u0447\u0442\u043e \u0443 \u0447\u0435\u043b\u043e\u0432\u0435\u043a\u0430 \u0435\u0441\u0442\u044c). \u0421 \u0442\u0435\u043b\u0435\u0444\u043e\u043d\u0430 \u0434\u043e \u044d\u0442\u0438\u0445
            \u0434\u0435\u0439\u0441\u0442\u0432\u0438\u0439 \u0438\u043d\u0430\u0447\u0435 \u043d\u0435 \u0434\u043e\u0431\u0440\u0430\u0442\u044c\u0441\u044f: \u0443 \u043e\u043a\u043d\u0430 \u043d\u0430 Xvfb \u043d\u0435\u0442 \u043d\u0438 \u0436\u0435\u0441\u0442\u0430 \u00ab\u043d\u0430\u0437\u0430\u0434\u00bb,
            \u043d\u0438 \u043f\u0430\u043d\u0435\u043b\u0438 \u0432\u043a\u043b\u0430\u0434\u043e\u043a, \u0432 \u043a\u043e\u0442\u043e\u0440\u0443\u044e \u043c\u043e\u0436\u043d\u043e \u043f\u043e\u043f\u0430\u0441\u0442\u044c \u043f\u0430\u043b\u044c\u0446\u0435\u043c. */}
        {vbRunning && (
          <>
            {addressOpen && (
              <div className="remote-address-row">
                <input className="remote-address-input" value={addressText} autoFocus
                  placeholder={t("remote.browserAddressPlaceholder")}
                  inputMode="url" autoCapitalize="off" autoCorrect="off" spellCheck={false}
                  onChange={(e) => setAddressText(e.target.value)}
                  onKeyDown={(e) => { if (e.key === "Enter") void submitAddress(); }} />
                <button className="btn btn-primary btn-sm" onClick={() => void submitAddress()}>
                  {t("remote.browserOpen")}
                </button>
              </div>
            )}
            <div className="remote-keys remote-browser-keys">
              <button className="remote-key" onClick={() => void browserGo("back")}>
                {"\u2190"} {t("remote.browserBack")}
              </button>
              <button className="remote-key" onClick={() => void browserGo("forward")}>
                {"\u2192"} {t("remote.browserForward")}
              </button>
              <button className="remote-key" onClick={() => void browserGo("reload")}>{"\u27f3"}</button>
              <button className={`remote-key${addressOpen ? " active" : ""}`} onClick={openAddress}>
                {t("remote.browserAddress")}
              </button>
              <button className="remote-key" onClick={() => sendCombo(["Control", "t"])}>
                {"+"} {t("remote.browserTab")}
              </button>
              {/* Меню ⋮ — вид сайта (Android/iPhone/Компьютер) и остановка
                  браузера; на телефоне оно в нижней панели, здесь — в ряду. */}
              <button className="remote-key" onClick={openBrowserMenu} aria-label={t("remote.menu")}>{"\u22EE"}</button>
            </div>
          </>
        )}
        {!browserTouch && <div className="remote-arrows">
          <button className="remote-key" onClick={() => sendKey("ArrowLeft")}>{"\u2190"}</button>
          <button className="remote-key" onClick={() => sendKey("ArrowUp")}>{"\u2191"}</button>
          <button className="remote-key" onClick={() => sendKey("ArrowDown")}>{"\u2193"}</button>
          <button className="remote-key" onClick={() => sendKey("ArrowRight")}>{"\u2192"}</button>
        </div>}
      </div>
      )}

      {/* Вкладки: то же, что в мобильном браузере по кнопке с их числом.
          Строки под палец, слева буква хоста в цветном кружке (без фавиконок),
          новая вкладка — большой кнопкой снизу и ТОЛЬКО через API: Ctrl+T в
          браузерном режиме больше не шлём. */}
      {tabsOpen && (
        <div className="remote-sheet-backdrop" onClick={() => setTabsOpen(false)}>
          <div className="remote-tabs-sheet" onClick={(e) => e.stopPropagation()}>
            <div className="remote-tabs-head">
              <span>{t("remote.browserTabs")}</span>
            </div>
            <div className="remote-tabs-list">
              {browserTabs.map((tab) => {
                const host = hostOf(tab.url);
                return (
                  <div key={tab.id} className={`remote-tab-row${tab.active ? " active" : ""}`}>
                    <button className="remote-tab-open" onClick={() => void openTab(tab.id)}>
                      {/* Значок сайта, если агент его знает: вкладка узнаётся
                          взглядом. Буква в кружке остаётся запасным вариантом —
                          у части сайтов значка нет вовсе. */}
                      {tab.icon ? (
                        <img className="remote-tab-icon" src={tab.icon} alt="" aria-hidden="true" />
                      ) : (
                        <span className="remote-tab-ava" style={{ background: tabAvatarColor(host) }} aria-hidden="true">
                          {tabLetter(host, tab.title)}
                        </span>
                      )}
                      <span className="remote-tab-texts">
                        <span className="remote-tab-title">{tab.title || t("remote.browserNewTab")}</span>
                        <span className="remote-tab-host">{host}</span>
                      </span>
                      {/* Картинка страницы: агент снял её в момент ухода с
                          вкладки, так что это ровно то, что человек видел. */}
                      {tab.preview && (
                        <img className="remote-tab-preview" src={tab.preview} alt="" aria-hidden="true" />
                      )}
                    </button>
                    <button className="remote-tab-close" onClick={() => void dropTab(tab.id)}
                      aria-label={t("remote.browserCloseTab")}>{"×"}</button>
                  </div>
                );
              })}
              {browserTabs.length === 0 && (
                <div className="remote-tabs-empty">{t("remote.browserNoTabs")}</div>
              )}
            </div>
            <button className="remote-tabs-add" onClick={() => void addTab()}>
              {"+"} {t("remote.browserNewTab")}
            </button>
          </div>
        </div>
      )}

      {/* Адресная строка — полноэкранная, как в настоящем браузере: тап по
          пилюле поднимает клавиатуру, текущий адрес выделен целиком (набор
          поверх заменит его), Enter открывает адрес или ищет в Google. */}
      {addressOpen && browserTouch && (
        <div className="remote-address-overlay">
          <div className="remote-address-topbar">
            <input className="remote-address-input remote-address-overlay-input"
              value={addressText} autoFocus
              placeholder={t("remote.browserAddressPlaceholder")}
              inputMode="url" autoCapitalize="off" autoCorrect="off" spellCheck={false}
              onFocus={(e) => e.currentTarget.select()}
              onChange={(e) => setAddressText(e.target.value)}
              onKeyDown={(e) => {
                if (e.key === "Enter") void submitAddress();
                if (e.key === "Escape") setAddressOpen(false);
              }} />
            <button className="remote-address-cancel" onClick={() => setAddressOpen(false)}>
              {t("modal.cancel")}
            </button>
          </div>
          {(browserPage?.title || pillHost) && (
            <div className="remote-address-current">
              <span className="remote-address-current-title">
                {browserPage?.title || t("remote.browserNewTab")}
              </span>
              <span className="remote-address-current-url">{browserPage?.url || ""}</span>
            </div>
          )}
        </div>
      )}

      {/* Меню ⋮ нижней панели: всё, что не поместилось в ряд, — открытие на
          телефоне, вид сайта (профиль устройства), звук и остановка браузера. */}
      {browserMenu && (
        <div className="remote-sheet-backdrop" onClick={() => setBrowserMenu(false)}>
          <div className="remote-menu-sheet" onClick={(e) => e.stopPropagation()}>
            <button className="remote-quick-row" onClick={() => {
              setBrowserMenu(false);
              const url = browserPage?.url || "";
              if (url) void openExternalLink(url);
            }}>
              <span className="remote-quick-row-icon">{"\u2197"}</span>
              <span className="remote-quick-row-label">{t("remote.browserOpenOnPhone")}</span>
            </button>

            {/* \u0417\u0430\u043F\u043E\u043B\u043D\u0435\u043D\u0438\u0435 \u0444\u043E\u0440\u043C \u0438 \u043F\u0430\u0440\u043E\u043B\u0438 \u2014 \u0442\u043E, \u0440\u0430\u0434\u0438 \u0447\u0435\u0433\u043E \u0441 \u0442\u0435\u043B\u0435\u0444\u043E\u043D\u0430 \u0432\u043E\u043E\u0431\u0449\u0435
                \u043C\u043E\u0436\u043D\u043E \u0440\u0435\u0433\u0438\u0441\u0442\u0440\u0438\u0440\u043E\u0432\u0430\u0442\u044C\u0441\u044F \u043D\u0430 \u0441\u0430\u0439\u0442\u0430\u0445, \u043E\u0442\u043A\u0440\u044B\u0442\u044B\u0445 \u043D\u0430 \u0441\u0435\u0440\u0432\u0435\u0440\u0435. */}
            <button className="remote-quick-row" onClick={() => { setBrowserMenu(false); setFillOpen(true); }}>
              <span className="remote-quick-row-icon">{"\uD83D\uDD11"}</span>
              <span className="remote-quick-row-label">{t("remote.fillTitle")}</span>
            </button>
            <button className="remote-quick-row" onClick={() => { setBrowserMenu(false); setFindOpen(true); }}>
              <span className="remote-quick-row-icon">{"\uD83D\uDD0D"}</span>
              <span className="remote-quick-row-label">{t("remote.findTitle")}</span>
            </button>
            <button className="remote-quick-row" onClick={() => void addBookmarkHere()}>
              <span className="remote-quick-row-icon">{"\u2605"}</span>
              <span className="remote-quick-row-label">{t("remote.bookmarkAdd")}</span>
            </button>
            <button className="remote-quick-row" onClick={() => { setBrowserMenu(false); setHistoryOpen(true); }}>
              <span className="remote-quick-row-icon">{"\ud83d\udd58"}</span>
              <span className="remote-quick-row-label">{t("remote.historyTitle")}</span>
            </button>

            <div className="remote-quick-section">{t("remote.browserSiteView")}</div>
            {(deviceList.length ? deviceList : FALLBACK_DEVICES).map((device) => (
              <button key={device.id} className="remote-quick-row"
                onClick={() => void switchDevice(device.id)}>
                <span className="remote-quick-row-icon">
                  {device.mobile ? "\uD83D\uDCF1" : "\uD83D\uDDA5"}
                </span>
                <span className="remote-quick-row-label">{device.title}</span>
                {browserPage?.device === device.id && <span className="remote-menu-check">{"\u2713"}</span>}
              </button>
            ))}

            {hasAudio && (
              <button className="remote-quick-row" onClick={toggleSound}>
                <span className="remote-quick-row-icon">{soundMuted ? "\u{1F507}" : "\u{1F50A}"}</span>
                <span className="remote-quick-row-label">
                  {t(soundMuted ? "remote.unmuteSound" : "remote.muteSound")}
                </span>
              </button>
            )}

            <button className="remote-quick-row" onClick={() => void openDownloads()}>
              <span className="remote-quick-row-icon">{"\u2B07"}</span>
              <span className="remote-quick-row-label">{t("remote.vbDownloads")}</span>
            </button>

            <button className="remote-quick-row" onClick={toggleReadOnly}>
              <span className="remote-quick-row-icon">{readOnly ? "\u{1F441}" : "\u{1F6D1}"}</span>
              <span className="remote-quick-row-label">{t("remote.vbReadOnly")}</span>
              {readOnly && <span className="remote-menu-check">{"\u2713"}</span>}
            </button>

            <button className="remote-quick-row remote-menu-danger" onClick={() => {
              setBrowserMenu(false);
              void handleVbStop();
            }}>
              <span className="remote-quick-row-icon">{"\u23FB"}</span>
              <span className="remote-quick-row-label">{t("remote.vbStop")}</span>
            </button>
          </div>
        </div>
      )}

      {/* Меню долгого нажатия: что делать со ссылкой, картинкой или текстом
          под пальцем. Нативное меню Chrome в видеокадр не попадает вовсе. */}
      {ctxHit && (
        <BrowserContextSheet
          hit={ctxHit}
          onClose={() => setCtxHit(null)}
          onOpenNewTab={(url) => {
            setCtxHit(null);
            void newBrowserTab(url)
              .then(() => {
                setRetryKey((v) => v + 1);
                void refreshBrowserTabs();
                void refreshBrowserPage();
              })
              .catch((e: any) => toastError(mapApiError(e)));
          }}
          onOpenHere={(url) => {
            setCtxHit(null);
            void navigateVBrowser({ url }).catch((e: any) => toastError(mapApiError(e)));
          }}
          onCopy={(text) => { setCtxHit(null); void copyToDevice(text); }}
          onShare={(url) => { setCtxHit(null); void openExternalLink(url); }} />
      )}

      {fillOpen && (
        <BrowserFillSheet onClose={() => setFillOpen(false)} onCopy={(text) => void copyToDevice(text)} />
      )}

      {historyOpen && (
        <BrowserHistorySheet
          onClose={() => setHistoryOpen(false)}
          onOpen={(url) => {
            setHistoryOpen(false);
            void navigateVBrowser({ url }).catch((e: any) => toastError(mapApiError(e)));
          }} />
      )}

      {downloadsOpen && (
        <div className="remote-sheet-backdrop" onClick={() => setDownloadsOpen(false)}>
          <div className="remote-menu-sheet" onClick={(e) => e.stopPropagation()}>
            <div className="remote-quick-section">{t("remote.vbDownloads")}</div>
            {downloadsLoading && <div className="remote-quick-row"><span className="remote-quick-row-label">{t("remote.vbDownloadsLoading")}</span></div>}
            {!downloadsLoading && downloads.length === 0 && (
              <div className="remote-quick-row"><span className="remote-quick-row-label">{t("remote.vbDownloadsEmpty")}</span></div>
            )}
            {!downloadsLoading && downloads.map((d) => (
              <a key={d.path} className="remote-quick-row" href={downloadUrl(d.path)} download={d.name}>
                <span className="remote-quick-row-icon">{"\u2B07"}</span>
                <span className="remote-quick-row-label">{d.name}</span>
                <span className="remote-menu-check">{humanSize(d.size)}</span>
              </a>
            ))}
          </div>
        </div>
      )}

      {/* <input type=file>: системный picker Chrome не входит в видеокадр.
          Выбор происходит на устройстве человека, байты едут на сервер с
          прогрессом, затем CDP назначает их тому самому полю страницы. */}
      {browserFileChooser && (
        <div className="remote-sheet-backdrop remote-native-backdrop"
          onClick={() => { if (!browserFileUploading) void dismissBrowserFileChooser(); }}>
          <div className="remote-browser-native-sheet" role="dialog" aria-modal="true"
            aria-labelledby="remote-browser-file-title" onClick={(e) => e.stopPropagation()}>
            <div className="remote-native-icon" aria-hidden="true">{"\uD83D\uDCCE"}</div>
            <div id="remote-browser-file-title" className="remote-native-title">
              {t("remote.browserChooseFileTitle")}
            </div>
            <div className="remote-native-text">
              {t(browserFileChooser.multiple
                ? "remote.browserChooseFilesText"
                : "remote.browserChooseFileText")}
            </div>
            <input ref={browserFileInputRef} className="remote-native-file-input"
              type="file" multiple={browserFileChooser.multiple}
              aria-hidden="true" tabIndex={-1}
              onChange={(event) => void pickBrowserFiles(event)} />
            {browserFileUploading ? (
              <div className="remote-native-progress" role="status" aria-live="polite">
                <div className="remote-native-progress-label">
                  <span>{t("remote.browserFileUploading")}</span>
                  <strong>{browserFileProgress}%</strong>
                </div>
                <div className="remote-native-progress-track" aria-hidden="true">
                  <span style={{ width: `${browserFileProgress}%` }} />
                </div>
              </div>
            ) : (
              <button className="btn btn-primary remote-native-primary"
                autoFocus
                onClick={() => browserFileInputRef.current?.click()}>
                {t(browserFileChooser.multiple
                  ? "remote.browserChooseFilesButton"
                  : "remote.browserChooseFileButton")}
              </button>
            )}
            {browserFileError && (
              <div className="remote-native-error" role="alert">{browserFileError}</div>
            )}
            <button className="btn btn-secondary remote-native-secondary"
              onClick={() => void dismissBrowserFileChooser()}>
              {browserFileUploading ? t("modal.cancel") : t("remote.browserChooseFileCancel")}
            </button>
          </div>
        </div>
      )}

      {/* alert/confirm/prompt/beforeunload: те же нативные невидимые окна,
          перенесённые в доступный диалог приложения. */}
      {browserDialog && (
        <div className="remote-sheet-backdrop remote-native-backdrop">
          <div className="remote-browser-native-sheet" role="alertdialog" aria-modal="true"
            aria-labelledby="remote-browser-dialog-title">
            <div className="remote-native-icon" aria-hidden="true">
              {browserDialog.type === "beforeunload" ? "\u26A0\uFE0F" : "\uD83C\uDF10"}
            </div>
            <div id="remote-browser-dialog-title" className="remote-native-title">
              {browserDialog.type === "beforeunload"
                ? t("remote.browserLeaveTitle")
                : t("remote.browserDialogTitle")}
            </div>
            {browserDialog.url && (
              <div className="remote-native-host">{hostOf(browserDialog.url)}</div>
            )}
            <div className="remote-native-message">
              {browserDialog.message || t("remote.browserDialogFallback")}
            </div>
            {browserDialog.type === "prompt" && (
              <input className="remote-address-input remote-native-prompt"
                value={browserDialogPrompt} autoFocus
                onChange={(event) => setBrowserDialogPrompt(event.target.value)}
                onKeyDown={(event) => {
                  if (event.key === "Enter") void respondBrowserDialog(true);
                }} />
            )}
            <div className="remote-native-actions">
              {browserDialog.type !== "alert" && (
                <button className="btn btn-secondary" disabled={browserDialogBusy}
                  onClick={() => void respondBrowserDialog(false)}>
                  {browserDialog.type === "beforeunload"
                    ? t("remote.browserStay")
                    : t("modal.cancel")}
                </button>
              )}
              <button className={`btn ${browserDialog.type === "beforeunload" ? "btn-danger" : "btn-primary"}`}
                autoFocus={browserDialog.type !== "prompt"}
                disabled={browserDialogBusy} onClick={() => void respondBrowserDialog(true)}>
                {browserDialogBusy
                  ? t("remote.browserDialogWait")
                  : browserDialog.type === "beforeunload"
                    ? t("remote.browserLeave")
                    : t("remote.browserDialogOk")}
              </button>
            </div>
          </div>
        </div>
      )}

      {readOnly && (
        <div className="remote-readonly-badge" onClick={toggleReadOnly}>
          {t("remote.vbReadOnlyBadge")}
        </div>
      )}

      {/* Typing bar */}
      {showKb && (
        <div className="remote-typing-bar">
          {/* \u0420\u044f\u0434 \u043f\u0440\u043e\u043a\u0440\u0443\u0447\u0438\u0432\u0430\u0435\u0442\u0441\u044f \u0432\u0431\u043e\u043a: \u043f\u0440\u0438 \u043e\u0442\u043a\u0440\u044b\u0442\u043e\u0439 \u043a\u043b\u0430\u0432\u0438\u0430\u0442\u0443\u0440\u0435 \u043d\u0443\u0436\u043d\u044b \u0442\u0435 \u0436\u0435
              \u043a\u043b\u0430\u0432\u0438\u0448\u0438, \u0447\u0442\u043e \u0438 \u0431\u0435\u0437 \u043d\u0435\u0451 (Shift, \u229e, \u0441\u0442\u0440\u0435\u043b\u043a\u0438, Del, Home/End), \u0430
              \u0440\u0430\u043d\u044c\u0448\u0435 \u0437\u0434\u0435\u0441\u044c \u0431\u044b\u043b\u0438 \u0442\u043e\u043b\u044c\u043a\u043e Ctrl/Alt/Tab/Esc/\u2191/\u2193 \u2014 \u043d\u0430\u0431\u043e\u0440 \u0441 \u043f\u043e\u043b\u0435\u043c
              \u0432\u0432\u043e\u0434\u0430 \u0438 \u043f\u0435\u0440\u0435\u0445\u043e\u0434 \u043f\u043e \u043f\u043e\u043b\u044f\u043c \u0444\u043e\u0440\u043c\u044b \u0442\u0440\u0435\u0431\u043e\u0432\u0430\u043b\u0438 \u0437\u0430\u043a\u0440\u044b\u0432\u0430\u0442\u044c \u043a\u043b\u0430\u0432\u0438\u0430\u0442\u0443\u0440\u0443. */}
          <div className="remote-typing-specials">
            <button className={`remote-mod ${modifiers.ctrl ? "active" : ""}`} onClick={() => toggleMod("ctrl")}>Ctrl</button>
            <button className={`remote-mod ${modifiers.alt ? "active" : ""}`} onClick={() => toggleMod("alt")}>Alt</button>
            <button className={`remote-mod ${modifiers.shift ? "active" : ""}`} onClick={() => toggleMod("shift")}>Shift</button>
            <button className="remote-key" onClick={() => sendKey("Tab")}>Tab</button>
            <button className="remote-key" onClick={() => sendKey("Escape")}>Esc</button>
            <button className="remote-key" onClick={() => sendKey("ArrowLeft")}>{"\u2190"}</button>
            <button className="remote-key" onClick={() => sendKey("ArrowUp")}>{"\u2191"}</button>
            <button className="remote-key" onClick={() => sendKey("ArrowDown")}>{"\u2193"}</button>
            <button className="remote-key" onClick={() => sendKey("ArrowRight")}>{"\u2192"}</button>
            <button className="remote-key" onClick={() => sendKey("Delete")}>Del</button>
            <button className="remote-key" onClick={() => sendKey("Home")}>Home</button>
            <button className="remote-key" onClick={() => sendKey("End")}>End</button>
            <button className="remote-key" onClick={() => sendKey("Meta")}>{"\u229e"}</button>
          </div>
          {/* Режим виден: пока Ctrl/Alt горят, следующая буква не попадёт в
              поле, а уйдёт на ПК сочетанием. */}
          {(modifiers.ctrl || modifiers.alt) && (
            <div className="remote-typing-hint">{t("remote.comboArmed")}</div>
          )}
          <div className="remote-typing-row">
            {/* Поле контролируемое: набранное переживает закрытие строки, а
                отправка текста больше не приклеена к Enter (N137). */}
            <input ref={typingInputRef} className="remote-typing-input" placeholder={t("remote.typePlaceholder")}
              autoFocus spellCheck={false}
              value={typingText}
              onChange={(e) => handleTypingChange(e.target.value)}
              onKeyDown={(e) => {
                // Enter с Ctrl/Alt/Win — это комбинация для ПК, её целиком
                // отправляет обработчик физической клавиатуры; если перехватить
                // её здесь, Enter уйдёт дважды.
                if (e.key === "Enter" && !e.nativeEvent.isComposing
                  && !(e.ctrlKey || e.altKey || e.metaKey)) {
                  e.preventDefault();
                  sendTypedText(true);
                }
              }} />
            {/* «→» вставляет только текст (логин, путь, поисковый запрос),
                «↵» — текст и Enter. Раньше Enter уходил всегда, и форма
                отправлялась с пустым паролем. */}
            <button className="remote-typing-send" aria-label={t("remote.typeInsert")}
              onClick={() => sendTypedText(false)}>{"\u2192"}</button>
            <button className="remote-typing-send" aria-label={t("remote.typeInsertEnter")}
              onClick={() => sendTypedText(true)}>{"\u21B5"}</button>
            <button className="remote-typing-close" onClick={toggleKeyboard}>{"\u2716"}</button>
          </div>
        </div>
      )}

      {/* Quick Actions / settings overlay */}
      {showQuickActions && (
        <div className="remote-quick-backdrop" onClick={() => setShowQuickActions(false)}>
          <div className="remote-quick-panel" onClick={(e) => e.stopPropagation()}>
            <div className="remote-quick-section">{t("remote.sectionControl")}</div>

            {/* Выключить экран, не уходя со страницы: «←» уводит наружу, а
                остановить стрим и остаться было нечем. Возвращает на экран
                запуска — туда же, откуда его включали. */}
            <button className="remote-quick-row" onClick={() => { haptic("light"); stopStream(""); }}>
              <span className="remote-quick-row-icon">{"■"}</span>
              <span className="remote-quick-row-label">{t("remote.stopStream")}</span>
            </button>

            {vbRunning && (
              <button className="remote-quick-row" onClick={handleVbStop}>
                <span className="remote-quick-row-icon">{"\uD83C\uDF10"}</span>
                <span className="remote-quick-row-label">{t("remote.vbStop")}</span>
              </button>
            )}

            <div className="remote-mode-toggle remote-quick-modes">
              <button
                className={controlMode === "screen" ? "active" : ""}
                onClick={() => changeControlMode("screen")}
              >{t("remote.modeScreen")}</button>
              <button
                className={controlMode === "trackpad" ? "active" : ""}
                onClick={() => changeControlMode("trackpad")}
              >{t("remote.modeTrackpad")}</button>
            </div>

            <button className="remote-quick-row" onClick={cycleSens}>
              <span className="remote-quick-row-icon">{"\u261D"}</span>
              <span className="remote-quick-row-label">{t("remote.sensitivity")}</span>
              <span className="remote-quick-val">{sensitivity}x</span>
            </button>

            {/* Режимы потока переехали в шит «Картинка и звук»: чёткость и
                звук — одно решение, а жили в двух разных местах, и связать их
                было нечем. Здесь остаётся строка-дверь туда же, куда ведёт чип
                связи в шапке, — с показанием текущего режима. */}
            <button className="remote-quick-row"
              onClick={() => { haptic("light"); setShowQuickActions(false); setShowQuality(true); }}>
              <span className="remote-quick-row-icon">{"\u26A1"}</span>
              <span className="remote-quick-row-label">{t("remote.imageAndSound")}</span>
              <span className="remote-quick-val">{profileLabel}</span>
            </button>

            {displays.length > 1 && (
              <div className="remote-quick-row remote-quick-row-static">
                <span className="remote-quick-row-icon">{"\uD83D\uDDA5"}</span>
                <span className="remote-quick-row-label">{t("remote.monitor")}</span>
                <span className="remote-quick-displays">
                  {displays.map((d) => (
                    <button key={d.id}
                      className={`remote-display-btn ${d.id === activeDisplay ? "active" : ""}`}
                      onClick={() => switchDisplay(d.id)}>
                      {d.id + 1} · {d.w}×{d.h}
                    </button>
                  ))}
                </span>
              </div>
            )}

            {zoomLevel > 1.05 && (
              <button className="remote-quick-row" onClick={resetZoom}>
                <span className="remote-quick-row-icon">{"\uD83D\uDD0D"}</span>
                <span className="remote-quick-row-label">{t("remote.resetZoom")}</span>
                <span className="remote-quick-val">{zoomLevel.toFixed(1)}x</span>
              </button>
            )}

            <div className="remote-quick-section">{t("remote.sectionActions")}</div>
            <div className="remote-quick-grid">
              {isWindows && (
                <button className="remote-quick-btn" onClick={() => { sendCombo(["Meta", "d"]); setShowQuickActions(false); }}>
                  <span className="remote-quick-icon">{"\u229E"}</span>{t("remote.desktop")}
                </button>
              )}
              <button className="remote-quick-btn" onClick={() => { sendCombo(["Alt", "Tab"]); setShowQuickActions(false); }}>
                <span className="remote-quick-icon">{"\u21C6"}</span>Alt+Tab
              </button>
              <button className="remote-quick-btn" onClick={takeScreenshot} disabled={screenshotBusy}>
                <span className="remote-quick-icon">{"\uD83D\uDCF8"}</span>
                {screenshotBusy ? t("remote.screenshotPreparing") : t("remote.screenshot")}
              </button>
              {isWindows && (
                <>
                  {/* Здесь стоял «Ctrl+Alt+Del», который физически не мог
                      сработать: SAS доставляет только winlogon (SendSAS из
                      службы), а синтезированную комбинацию Windows отбрасывает —
                      панель закрывалась, вибрация была, на ПК не происходило
                      ничего. Заменён на то, ради чего его и жали: диспетчер
                      задач (Ctrl+Shift+Esc — обычный хоткей, SendInput его
                      доставляет). Блокировка экрана (Win+L) — строкой ниже. */}
                  <button className="remote-quick-btn" onClick={() => sendCombo(["Control", "Shift", "Escape"])}>
                    <span className="remote-quick-icon">{"\uD83D\uDCCA"}</span>{t("remote.taskManager")}
                  </button>
                  <button className="remote-quick-btn" onClick={closeActiveWindow}>
                    <span className="remote-quick-icon">{"\u2716"}</span>{t("remote.closeWindow")}
                  </button>
                </>
              )}
              <button className="remote-quick-btn" onClick={openClipboardPanel}>
                <span className="remote-quick-icon">{"\uD83D\uDCCB"}</span>{t("remote.clipboardExchange")}
              </button>
            </div>

            {/* Копировать/вставить/сохранить/отменить — с телефона их иначе
                не отправить: модификаторы работают только для спецклавиш. */}
            <div className="remote-quick-section">{t("remote.sectionShortcuts")}</div>
            <div className="remote-quick-grid">
              {vbDirect && (
                <>
                  <button className="remote-quick-btn" onClick={() => void switchDevice("desktop")}>
                    {t("remote.browserDesktopView")}
                  </button>
                  <button className="remote-quick-btn" onClick={() => void switchDevice("android")}>
                    {t("remote.browserMobileView")}
                  </button>
                  <button className="remote-quick-btn" onClick={() => {
                    setShowQuickActions(false);
                    const url = browserPage?.url || "";
                    if (url) void openExternalLink(url);
                  }}>
                    {t("remote.browserOpenOnPhone")}
                  </button>
                </>
              )}
              <button className="remote-quick-btn remote-combo-btn" onClick={() => sendCombo(["Control", "c"])}>Ctrl+C</button>
              <button className="remote-quick-btn remote-combo-btn" onClick={() => sendCombo(["Control", "v"])}>Ctrl+V</button>
              <button className="remote-quick-btn remote-combo-btn" onClick={() => sendCombo(["Control", "x"])}>Ctrl+X</button>
              <button className="remote-quick-btn remote-combo-btn" onClick={() => sendCombo(["Control", "z"])}>Ctrl+Z</button>
              <button className="remote-quick-btn remote-combo-btn" onClick={() => sendCombo(["Control", "s"])}>Ctrl+S</button>
              <button className="remote-quick-btn remote-combo-btn" onClick={() => sendCombo(["Control", "a"])}>Ctrl+A</button>
            </div>

            {isWindows && (
              <button className="remote-quick-row" onClick={lockPc}>
                <span className="remote-quick-row-icon">{"\uD83D\uDD12"}</span>
                <span className="remote-quick-row-label">{t("remote.lockPc")}</span>
                <span className="remote-quick-val">Win+L</span>
              </button>
            )}

            <button className="remote-quick-row" onClick={() => { setShowQuickActions(false); setHelpOpen(true); }}>
              <span className="remote-quick-row-icon">{"?"}</span>
              <span className="remote-quick-row-label">{t("help.title")}</span>
            </button>
          </div>
        </div>
      )}

      {/* «Картинка и звук» — одно место для всего, что человек меняет,
          когда картинка плохая или звука нет. Раньше режимы потока жили в
          шторке «⋮», а чёткость и звук — здесь, и связать одно с другим было
          нечем. Порядок сверху вниз: как связь → чёткость картинки → звук
          компьютера → свёрнутые технические детали. */}
      {showQuality && (
        <div className="remote-clip-backdrop" onClick={() => setShowQuality(false)}>
          <div className="remote-clip-panel remote-quality-panel" onClick={(e) => e.stopPropagation()}>
            <div className="remote-clip-title">{t("remote.imageAndSound")}</div>
            {/* Прежнее имя шита остаётся подписью первого блока: человек знал
                это окно как «Качество соединения», и оно по-прежнему отвечает
                на вопрос «как связь» — просто теперь это не всё окно. */}
            <div className="remote-quick-section">{t("remote.connectionQuality")}</div>
            <div className={`remote-quality-hero ${qualityLevel}`}>
              <span className="remote-hud-dot" style={{ background: rttColor }} />
              <strong>{qualityLabel}</strong>
              <span>{hudRtt} ms</span>
            </div>

            <div className="remote-quick-section">{t("remote.imageBlock")}</div>
            <div className="remote-quick-profiles">
              {remoteProfiles.map((profile) => (
                <button key={profile}
                  className={`remote-profile-opt ${streamProfile === profile ? "active" : ""}`}
                  onClick={() => selectStreamProfile(profile)}>
                  {t(`remote.profile.${profile}`)}
                </button>
              ))}
            </div>
            <div className="remote-quick-hint">{t(`remote.profileDesc.${streamProfile}`)}</div>
            {/* Просевшая связь: раньше здесь стояла вторая зелёная кнопка во всю
                ширину, и «Эконом» выглядел не режимом, а отдельным лекарством.
                Теперь это подсказка над теми же четырьмя кнопками. */}
            {offerSaver && (
              <div className="remote-quick-hint remote-quality-warn">{t("remote.qualityHintSaver")}</div>
            )}

            {/* Звук компьютера — системный звук ПК (протокол {t:"audio"}), НЕ
                звук виртуального браузера: у того свой трек и своя кнопка-динамик
                в шапке. Смешивать их нельзя. */}
            <div className="remote-quick-section">{t("remote.soundBlock")}</div>
            <button
              className="remote-quality-fix"
              disabled={audioState === "unavailable" || audioState === "pending"}
              onClick={toggleAudio}
            >
              {audioState === "on"
                ? t("remote.audioOff")
                : audioState === "pending"
                  ? t("remote.audioStarting")
                  : audioState === "unavailable"
                    ? t("remote.audioUnavailable")
                    : t("remote.audioOn")}
            </button>

            {/* Восемь строк телеметрии — ответ на вопрос, которого человек не
                задавал: он пришёл сделать чётче и включить звук. Убираем под
                спойлер, состояние связи остаётся видно всегда (hero выше). */}
            <details className="remote-quality-tech">
              <summary>{t("remote.technicalDetails")}</summary>
              <div className="remote-quality-grid">
                <span>FPS</span><strong>{fps || stats.sentFps || "—"}</strong>
                <span>{t("remote.qualityWidth")}</span><strong>{stats.width || "—"} px</strong>
                <span>{t("remote.qualityBitrate")}</span><strong>{bitrate}</strong>
                <span>{t("remote.qualityJitter")}</span><strong>{cstats?.jbMs ?? "—"} ms</strong>
                <span>{t("remote.qualityRoute")}</span>
                <strong>{cstats?.route === "relay" ? "TURN" : cstats?.route ? "P2P" : "—"}</strong>
                <span>{t("remote.qualityLoss")}</span><strong>{cstats?.lossPct ?? "—"}%</strong>
                <span>{t("remote.qualityTraffic")}</span><strong>{formatTraffic(trafficBytes)}</strong>
                {/* Режим изображения был виден только в текстовой диагностике,
                    поэтому «мыло вместо потока» выглядело как поломка (N136). */}
                <span>{t("remote.qualityImageMode")}</span>
                <strong>{h264Active ? t("remote.imageModeH264") : t("remote.imageModeJpeg")}</strong>
              </div>
              <label className="remote-quality-toggle">
                <input type="checkbox" checked={showHudDetails} onChange={toggleHudDetails} />
                <span>{t("remote.showTechnicalDetails")}</span>
              </label>
              <div className="remote-clip-actions">
                {!h264Active && jpegForced && (
                  <button className="remote-clip-btn secondary" onClick={retryH264}>
                    {t("remote.retryH264")}
                  </button>
                )}
                <button className="remote-clip-btn primary" onClick={copyDiagnostics}>
                  {t("remote.copyDiagnostics")}
                </button>
              </div>
            </details>

            {/* «Закрыть» остаётся СНАРУЖИ спойлера: единственный выход из шита
                не должен зависеть от того, раскрыты технические детали. */}
            <div className="remote-clip-actions">
              <button className="remote-clip-btn secondary" onClick={() => setShowQuality(false)}>
                {t("modal.close")}
              </button>
            </div>
          </div>
        </div>
      )}

      {/* Clipboard panel */}
      {showClipboard && (
        <div className="remote-clip-backdrop" onClick={() => setShowClipboard(false)}>
          <div className="remote-clip-panel" onClick={(e) => e.stopPropagation()}>
            <div className="remote-clip-title">{t("remote.clipboardExchange")}</div>
            {/* Что с буфером ПК — состояние ВНУТРИ панели, а не условие её
                открытия: отказ компьютера не должен запирать отправку с
                телефона. */}
            <div className="remote-clip-subtitle">{t("remote.pcClipboard")}</div>
            <div className={`remote-clip-text ${clipboardError ? "error" : ""}`}>
              {clipboardLoading
                ? t("remote.clipReading")
                : clipboardError || clipboardText || t("remote.emptyClipboard")}
            </div>
            <label className="remote-clip-manual">
              <span>{t("remote.clipManualLabel")}</span>
              <textarea
                value={manualClipboardText}
                onChange={(event) => setManualClipboardText(event.target.value)}
                placeholder={t("remote.manualTextPlaceholder")}
                rows={3}
              />
            </label>
            <div className="remote-clip-actions">
              {clipboardError && (
                <button className="remote-clip-btn secondary" onClick={openClipboardPanel}>
                  {t("remote.retry")}
                </button>
              )}
              <button className="remote-clip-btn primary" onClick={copyToPhone}>
                {t("remote.copyToPhone")}
              </button>
              <button className="remote-clip-btn secondary" onClick={sendPhoneClipboard}>
                {t("remote.textFromPhoneClipboard")}
              </button>
              <button className="remote-clip-btn secondary" onClick={sendManualClipboard}>
                {t("remote.sendManualText")}
              </button>
              <button className="remote-clip-btn secondary" onClick={sendPhoneImage}>
                {t("remote.imageToPc")}
              </button>
              <button className="remote-clip-btn secondary" onClick={() => setShowClipboard(false)}>
                {t("modal.close")}
              </button>
            </div>
          </div>
        </div>
      )}

      {/* Нижняя навигация приложения в браузерном режиме уступает место
          панели браузера: два ряда кнопок внизу — это уже не браузер. */}
      {showUI && !showKb && !browserTouch && (
        <div className="remote-bottom-nav">
          <BottomNav active="remote" />
        </div>
      )}

      <HelpSheet
        open={helpOpen}
        onClose={() => setHelpOpen(false)}
        title={t("remote.helpTitle")}
        guide="remote"
        items={[
          { icon: "\uD83D\uDC46", title: t("remote.helpTapTitle"), text: t("remote.helpTapText") },
          { icon: "\u270B", title: t("remote.helpTrackpadTitle"), text: t("remote.helpTrackpadText") },
          { icon: "\uD83D\uDD0D", title: t("remote.helpZoomTitle"), text: t("remote.helpZoomText") },
          // На ноутбуке путь к клавиатуре другой — про строку ввода там писать
          // нечестно, она нужна только телефонам (N148).
          physicalKeyboard
            ? { icon: "\u2328", title: t("remote.helpKeyboardTitle"), text: t("remote.helpKeyboardDesktopText") }
            : { icon: "\u2328", title: t("remote.helpKeyboardTitle"), text: t("remote.helpKeyboardText") },
          { icon: "\uD83D\uDCCB", title: t("remote.helpClipboardTitle"), text: t("remote.helpClipboardText") },
          // \u041F\u0440\u043E \u043A\u0430\u0440\u0442\u0438\u043D\u043A\u0443 \u0438 \u0437\u0432\u0443\u043A \u0432 \u0441\u043F\u0440\u0430\u0432\u043A\u0435 \u043D\u0435 \u0431\u044B\u043B\u043E \u043D\u0438 \u0441\u043B\u043E\u0432\u0430, \u0445\u043E\u0442\u044F \u0438\u043C\u0435\u043D\u043D\u043E \u044D\u0442\u043E\u0433\u043E
          // \u0438\u0449\u0443\u0442, \u043A\u043E\u0433\u0434\u0430 \u00AB\u043C\u044B\u043B\u043E\u00BB \u0438\u043B\u0438 \u0442\u0438\u0448\u0438\u043D\u0430.
          { icon: "\u2699", title: t("remote.helpQualityTitle"), text: t("remote.helpQualityText") },
        ]}
      />
    </div>
  );
}
