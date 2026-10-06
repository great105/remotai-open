import { getLocale } from "@tgcontrol/shared";
import { useEffect, useLayoutEffect, useState, useCallback, useRef } from "react";
import type { PointerEvent as ReactPointerEvent, KeyboardEvent as ReactKeyboardEvent, ReactNode } from "react";
import { useNavigate, useSearchParams } from "react-router-dom";
import {
  listPtySessions, createPtySession, closePtySession, closeDeadPtySessions, reattachPtySession,
  getBookmarks, getRecentFolders, renamePty, setPtyPlacements, setPtyFolders,
  addBookmark, removeBookmark, getAgents, getAgentAccounts, ptyInput, setPtyAccount,
  onConnectionChange, getConfig, onWSEvent, getQuickPaths,
} from "../api";
import type { RecentFolder, PtySessionInfo, PtyLostSession } from "../api";
import { haptic, hapticSuccess, tgConfirm, getTelegram } from "../telegram";
import {
  promptDialog, useToast, useLoadState, OfflineState, mapApiError, ptyDisplayTitle,
  formatAgoValue, isAgentKind, agentDisplayName, setAgentRegistry, useEscape,
} from "@tgcontrol/shared";
import { getMode } from "../config";
import { AllComputerTerminals } from "./AllComputerTerminals";
import { ptyListStatus } from "../ptyTerm/listStatus";
import { ptyStatusIcon } from "../components/PtyStatusIcon";
import { readTerminalScope, saveTerminalScope } from "../ptyTerm/computerList";
import { t } from "../i18n";
import { BottomNav } from "../components/BottomNav";
import { ProcessBadge, FolderNavSheet } from "@tgcontrol/shared";
import type { AgentKind } from "@tgcontrol/shared";
import type { AgentAccount, AgentInfo, Bookmark } from "../types";
import { HelpSheet } from "../components/HelpSheet";
import { LostTerminals } from "../components/LostTerminals";
import { DeviceChip } from "../components/DeviceChip";
import {
  IconRobot, IconStar, IconClock, IconFolder, IconGroup,
  IconArrow, IconCheck, IconChevron, IconClose, IconDots, IconDownload, IconGrip,
  IconHourglass, IconPencil, IconPlus, IconRefresh, IconSearch, IconSend, IconServer,
  IconUngroup, IconUnlink, IconWarning,
} from "../components/icons";
import { usePolling } from "../hooks/usePolling";
import { useBotAvailable } from "../hooks/useBotAvailable";
import { useHorizontalWheel } from "../hooks/useHorizontalWheel";
import { emptyTapGuard, noteDown, noteMove, noteScroll, tapVerdict } from "../ptyTerm/tapGuard";
import { trackFirstTerminal } from "../cloud/support";
import { isShareCancel } from "../saveFile";
import { savePtyLog, sendPtyLogToTelegram, ptyLogFileName } from "../ptyLogExport";
import { isInnerPath } from "../navBack";
import {
  composeLaunch, EMPTY_PREFS, launchAccountForAgent, recordedResumeAccount,
} from "../ptyTerm/agentLaunch";

type PtySession = PtySessionInfo;

// ── Группы и ручной порядок ────────────────────────────────────
// Группа — это просто значение `group` у сессий (хранится на агенте в pty.json).
// Именно ГРУППА, а не «папка»: папкой на этом экране зовётся каталог
// компьютера («…\TGControl-ALL» на карточке), и одно слово на два смысла
// заставляло человека гадать, что он создаёт кнопкой «+».
// Ручной порядок — `sort`; 0/отсутствует = стабильный порядок создания.
// Активность терминала намеренно НЕ участвует: новый вывод не должен двигать
// карточку под пальцем человека во время фонового обновления.

type PtyStatusFilter = "all" | "waiting" | "working" | "error" | "dead";

/** Эффективный ключ сортировки: заданный вручную sort, иначе время создания. */
const effSort = (s: PtySession): number => (
  s.sort && s.sort !== 0
    ? s.sort
    : -s.created
);

interface PtyGroup { name: string; items: PtySession[] }

/** Группировка сессий по группам; внутри группы — по sort. «Без группы» всегда в конце. */
function groupOrdered(sessions: PtySession[]): PtyGroup[] {
  const map = new Map<string, PtySession[]>();
  for (const s of sessions) {
    const g = s.group || "";
    const arr = map.get(g);
    if (arr) arr.push(s); else map.set(g, [s]);
  }
  const groups = [...map.entries()].map(([name, items]) => ({
    name,
    items: items.slice().sort((a, b) => effSort(a) - effSort(b) || a.id.localeCompare(b.id)),
  }));
  groups.sort((a, b) => {
    if (a.name === "") return 1;
    if (b.name === "") return -1;
    return Math.min(...a.items.map(effSort))
      - Math.min(...b.items.map(effSort))
      || a.name.localeCompare(b.name, "ru");
  });
  return groups;
}

/** Элемент плоского списка: заголовок группы, карточка или линия-индикатор дропа. */
type ListItem =
  | { kind: "header"; name: string; count: number }
  | { kind: "card"; s: PtySession }
  | { kind: "indicator"; key: string };

/** Куда упадёт перетаскиваемая карточка: группа + перед какой карточкой (null = в конец). */
interface DropPos { group: string; beforeId: string | null }

interface MoveRequest {
  ids: string[];
  source: "menu" | "selection";
}

/** Плитка «Быстрого запуска»: закреплённая папка или недавняя. */
interface QuickTile {
  name: string;
  path: string;
  pinned: boolean;
}

/**
 * Имя папки на плитке «Быстрого запуска».
 *
 * Замер на стенде: подписи не влезали в плитку на 5 px, и «PlatexGorny»
 * превращалось в «PlatexGorn…» — обрезка хвостом бесполезна ещё и потому, что
 * у соседних проектов различается как раз хвост («…-old» / «…-new»). Длинное
 * имя сокращаем по СЕРЕДИНЕ; плитка при этом стала шире (см. CSS).
 */
function tileLabel(name: string): string {
  const MAX = 15;
  if (name.length <= MAX) return name;
  const head = Math.ceil((MAX - 1) / 2);
  const tail = MAX - 1 - head;
  return `${name.slice(0, head)}…${name.slice(name.length - tail)}`;
}

/**
 * Ловушка фокуса для собственных шторок экрана.
 *
 * Без неё Tab из открытой шторки уходит в список под затемнением и «нажимает»
 * невидимые кнопки, а скринридер продолжает читать страницу под оверлеем.
 * Правило то же, что в DialogHost, — просто вынесено сюда, потому что шторки
 * этого экрана рисуются своей разметкой.
 */
function trapTabInSheet(e: ReactKeyboardEvent<HTMLDivElement>) {
  if (e.key !== "Tab") return;
  const items = [...e.currentTarget.querySelectorAll<HTMLElement>(
    'button:not([disabled]), input:not([disabled]), textarea:not([disabled]), select:not([disabled]), a[href], [tabindex]:not([tabindex="-1"])',
  )];
  if (items.length === 0) return;
  const index = items.indexOf(document.activeElement as HTMLElement);
  const next = e.shiftKey
    ? (index <= 0 ? items.length - 1 : index - 1)
    : (index < 0 || index === items.length - 1 ? 0 : index + 1);
  e.preventDefault();
  items[next]?.focus();
}

/** Начальный фокус внутрь шторки и возврат его на кнопку-источник при закрытии. */
function useSheetFocus(open: boolean) {
  const ref = useRef<HTMLDivElement | null>(null);
  useEffect(() => {
    if (!open) return;
    const opener = document.activeElement as HTMLElement | null;
    ref.current?.focus();
    return () => {
      if (opener && typeof opener.focus === "function" && document.contains(opener)) opener.focus();
    };
  }, [open]);
  return ref;
}


/** Optional aggregate view; loading its folders never switches the active PC. */
export function PtyListView() {
  const [allComputers, setAllComputers] = useState(() => getMode() === "cloud" && readTerminalScope());
  const cloud = getMode() === "cloud";
  // A launch deep link must keep targeting the currently selected computer.
  const [params] = useSearchParams();
  // The single-PC launch handler clears query parameters before opening its
  // chooser. Keep that view mounted until the launch finishes or scope is chosen.
  const hasLaunch = ["shell", "cwd", "agent", "new", "ssh", "install"].some(key => params.has(key));
  const [launchRequested, setLaunchRequested] = useState(hasLaunch);
  useLayoutEffect(() => { if (hasLaunch) setLaunchRequested(true); }, [hasLaunch]);
  const aggregate = cloud && allComputers && !launchRequested && !hasLaunch;
  const scopeControl = cloud ? (
    <div className="pty-computer-scope" role="group" aria-label={t("pty.computers.scope")}>
      {([false, true] as const).map(all => (
        <button key={String(all)} type="button" className={`btn ${aggregate === all ? "btn-primary" : "btn-secondary"}`}
          aria-pressed={aggregate === all} onClick={() => { setLaunchRequested(false); saveTerminalScope(all); setAllComputers(all); }}>
          {t(all ? "pty.computers.all" : "pty.computers.current")}
        </button>
      ))}
    </div>
  ) : null;
  return aggregate ? <AllComputerTerminals scopeControl={scopeControl} /> : <SingleComputerTerminals scopeControl={scopeControl} />;
}

function SingleComputerTerminals({ scopeControl }: { scopeControl: ReactNode }) {
  const navigate = useNavigate();
  // «Назад» из терминала возвращает СЮДА, в этот список. Своего дома у SSH-сессии
  // два: карточка сервера в SSH-центре и список терминалов, и раньше экран
  // терминала всегда выбирал первый — открыв SSH-сессию из «Терминалов»,
  // человек уходил назад в другой раздел (живая жалоба владельца). Кто открыл,
  // тот и называет, куда возвращаться.
  /**
   * Дом для «Назад» у терминала, открытого ЧЕРЕЗ этот список.
   *
   * ⚠ Читаем из ref, а не из адреса: плитка быстрого запуска с главной ведёт
   * на `/pty?shell=…&from=/`, и обработчик deep-link ВЫЧИЩАЕТ параметры
   * (`setSearchParams(new URLSearchParams())`) ещё до того, как терминал
   * создан, — к моменту `openTerminal` в адресе не осталось ничего. Поэтому
   * источник запоминается в тот же миг, когда читается (см. эффект ниже).
   *
   * Своего `from` нет — дом прежний, этот список.
   */
  const deepLinkFrom = useRef("");
  const openTerminal = (id: string, params?: Record<string, string>) => {
    const home = isInnerPath(deepLinkFrom.current) ? deepLinkFrom.current : "/pty";
    const q = new URLSearchParams({ ...(params || {}), from: home });
    // ⚠ Пришли сюда транзитом (плитка с главной) — ЗАМЕНЯЕМ запись в истории,
    // а не добавляем. Иначе история выходит «главная → список → терминал», и
    // системная «Назад» честно возвращает в список, в котором человек не был
    // ни секунды: `?from=` тут не спасает, потому что до `backFallback` дело
    // не доходит — сначала срабатывает обычный шаг по истории.
    const transit = isInnerPath(deepLinkFrom.current);
    navigate(`/pty/${encodeURIComponent(id)}?${q.toString()}`, { replace: transit });
    if (transit) deepLinkFrom.current = "";
  };
  const [searchParams, setSearchParams] = useSearchParams();
  const { toastSuccess, toastError, toast } = useToast();
  // Единое состояние загрузки: различает «пусто» и «ПК не в сети».
  const load = useLoadState();
  const { succeed, fail } = load;
  const [sessions, setSessions] = useState<PtySession[]>([]);
  // Терминалы, прерванные перезагрузкой компьютера (см. components/LostTerminals).
  const [lost, setLost] = useState<PtyLostSession[]>([]);
  // id терминала, которому прямо сейчас возвращаем связь (см. handleReattach).
  const [reattachingId, setReattachingId] = useState("");
  // Терминалы, у которых «Вернуть связь» уже не сработала: у них процесс жив, а
  // канал не поднимается (терминал от прежней версии). Кнопку прячем, остаётся
  // «Открыть в этой папке».
  const [noReattach, setNoReattach] = useState<string[]>([]);
  const [loading, setLoading] = useState(true);
  const [query, setQuery] = useState("");
  const [statusFilter, setStatusFilter] = useState<PtyStatusFilter>("all");
  const [nowMs, setNowMs] = useState(Date.now());
  // Когда список получен последний раз: при потере связи карточки не должны
  // молча выдавать себя за свежие — над ними появляется «Данные на HH:MM».
  const [lastOkAt, setLastOkAt] = useState(0);
  const [folderOpen, setFolderOpen] = useState(false);
  const [folderStart, setFolderStart] = useState<string>("");
  // Рабочая папка по умолчанию — в ref, а не в state: она нужна внутри
  // handleCreate, который живёт вне рендера, и перерисовывать из-за неё список
  // терминалов незачем.
  const defaultCwdRef = useRef("");
  const platformRef = useRef("");
  // Зачем терминал открывают: обычный шелл или сразу AI-агент. От этого зависят
  // подписи в шторке выбора папки и то, откроется ли выбор агента после запуска.
  const [folderIntent, setFolderIntent] = useState<"terminal" | "agent">("terminal");
  // Команды «продолжить беседу» из реестра агентов: нужны мёртвым карточкам,
  // чтобы «↻» возвращал в разговор с Claude, а не в пустой шелл.
  const [resumeCmds, setResumeCmds] = useState<Record<string, AgentInfo>>({});
  const resumeLoadedRef = useRef(false);
  const botAvailable = useBotAvailable();
  const inTelegram = !!getTelegram()?.initData;
  const [bookmarks, setBookmarks] = useState<Bookmark[]>([]);
  const [recent, setRecent] = useState<RecentFolder[]>([]);
  const longPressTimer = useRef<number | null>(null);
  const longPressFired = useRef(false);
  const cardLongPressTimer = useRef<number | null>(null);
  const cardLongPressFired = useRef(false);
  const [showHelp, setShowHelp] = useState(false);
  // Терминал, у которого прямо сейчас правят имя в карточке, и черновик имени.
  const [renameId, setRenameId] = useState<string | null>(null);
  const [renameDraft, setRenameDraft] = useState("");
  const [menuSession, setMenuSession] = useState<PtySession | null>(null);
  const [moveRequest, setMoveRequest] = useState<MoveRequest | null>(null);
  // Меню плитки «Быстрого запуска» — долгий тап / правый клик / клавиша «Меню».
  const [tileMenu, setTileMenu] = useState<QuickTile | null>(null);
  // Меню шапки группы («⋮»): редкие действия над группой живут строками с
  // текстом, а не значками в ряд — см. комментарий у renderFolderHeader.
  const [folderMenu, setFolderMenu] = useState<string | null>(null);
  const [selectionMode, setSelectionMode] = useState(false);
  const [selectedIds, setSelectedIds] = useState<string[]>([]);

  // Группы / drag-and-drop.
  const [supportsGroups, setSupportsGroups] = useState(false);
  const [pendingFolders, setPendingFolders] = useState<string[]>(() => {
    try {
      const value = JSON.parse(localStorage.getItem("ptyFolders") || "[]");
      return Array.isArray(value) ? value.filter((x): x is string => typeof x === "string") : [];
    } catch {
      return [];
    }
  });
  const [collapsedMap, setCollapsedMap] = useState<Record<string, boolean>>(() => {
    try { return JSON.parse(localStorage.getItem("ptyFoldersCollapsed") || "{}"); } catch { return {}; }
  });
  // Верхняя панель (плитки «Быстрого запуска», поиск с фильтрами, переход в
  // SSH) при десятке закреплённых папок съедала треть экрана, и список
  // терминалов начинался за его серединой. Сворачивается в одну строку;
  // состояние помним так же, как у свёрнутых папок.
  const [toolsCollapsed, setToolsCollapsed] = useState<boolean>(() => {
    try { return localStorage.getItem("ptyToolsCollapsed") !== "0"; } catch { return true; }
  });
  const [draggingId, setDraggingId] = useState<string | null>(null);
  const [dropPos, setDropPos] = useState<DropPos | null>(null);
  const dragRef = useRef<{ id: string; startX: number; startY: number; moved: boolean } | null>(null);
  const draggingRef = useRef(false); // синхронный гард для refresh (поллинг)
  const suppressClickRef = useRef(false); // клик после drag не должен открывать терминал
  const autoScrollRAF = useRef(0);
  const autoScrollDirection = useRef(0);
  const attentionEpisodes = useRef(new Set<string>());

  const refresh = useCallback(async () => {
    if (draggingRef.current) return; // не дёргаем список во время drag
    try {
      const data = await listPtySessions();
      // Стабильный порядок (новые сверху) — иначе карточки прыгают на каждом
      // обновлении (бэкенд раньше отдавал их в случайном порядке map).
      const list = (data.sessions || []).slice().sort(
        (a, b) => (b.created - a.created) || (a.id < b.id ? -1 : 1)
      );
      setSessions(list);
      // Терминалы, прерванные перезагрузкой компьютера. У старого агента ключа
      // нет вовсе — тогда блока просто не будет (пустой массив).
      setLost(Array.isArray(data.lost) ? data.lost : []);
      if (Array.isArray(data.folders)) {
        setSupportsGroups(true);
        setPendingFolders(data.folders);
        try { localStorage.setItem("ptyFolders", JSON.stringify(data.folders)); } catch { /* ignore */ }
      } else if (list.some((s) => "sort" in s)) {
        setSupportsGroups(true);
      }
      setLastOkAt(Date.now());
      succeed();
    } catch (e) {
      // Пустой catch показывал «Нет терминалов» при выключенном ПК — то есть
      // ровно то же, что при пустом списке на работающем компьютере.
      fail(e);
    }
    setLoading(false);
  }, [succeed, fail]);

  useEffect(() => {
    getBookmarks().then((d) => setBookmarks(d.bookmarks || [])).catch(() => {});
    getRecentFolders().then((d) => setRecent(d.folders || [])).catch(() => {});
  }, []);

  // Про выключенный компьютер узнаём ОТ КАНАЛА СВЯЗИ, а не по провалу запроса.
  // Замер на стенде: при выключенном ПК этот экран молчал 4,8 с — ровно столько
  // умирал HTTP-запрос, — и всё это время крутил спиннер, обещающий загрузку.
  // Главная отвечает мгновенно именно потому, что подписана (Dashboard.tsx):
  // кадр agent_status живого канала приносит причину `pc_offline` сразу.
  const [linkOffline, setLinkOffline] = useState(false);
  useEffect(() => onConnectionChange((state) => {
    if (state.reason === "pc_offline") { setLinkOffline(true); return; }
    if (state.connected) setLinkOffline(false);
  }), []);
  // Одна правда об офлайне на весь экран: канал сказал раньше, запрос — позже.
  const offline = linkOffline || load.offline;

  // Live-обновление: терминал, открытый на другом устройстве (ПК ⇄ телефон),
  // появляется в списке за несколько секунд без ручного обновления. Первый
  // запрос делает сам хук, поэтому отдельного refresh() на маунте больше нет.
  // В фоне (свёрнутое приложение, другая вкладка) не опрашиваем вовсе: каждый
  // ответ стоит агенту снимка дерева процессов на КАЖДУЮ сессию. При офлайне
  // реже: смысла долбить выключенный ПК нет, вернуться в строй он должен сам.
  // Смена интервала перезапускает хук, поэтому возвращение ПК в сеть даёт
  // немедленный запрос — ждать очередного такта не приходится.
  // ⚠ `!renameId` в списке условий обязателен: пока человек печатает имя,
  // список перерисовывать нельзя — ответ опроса приносит СТАРОЕ имя и переставляет
  // карточки, а под правкой это означает потерянный ввод.
  const pollEnabled = !folderOpen && !showHelp && !menuSession && !moveRequest && !tileMenu && !folderMenu && !renameId;
  // Опрос — страховка, а не основной канал. С 2.61.17 изменения приходят
  // событиями по уже открытому сокету: статусы агента (`pty_event`) и жизнь
  // списка (`pty_list_changed` — создан, закрыт, переименован). Раньше каждые
  // 6 с шёл запрос через релей (боевой лог 01.09.2026: 4655 запросов за
  // 8 часов), и каждый стоил агенту снимка дерева процессов на все сессии.
  // Со старым агентом события списка не приходят — 20 с опроса всё ещё держат
  // список живым, просто новый терминал с другого устройства появится позже.
  usePolling(refresh, offline ? 30000 : 20000, { enabled: pollEnabled });
  // Событие → один запрос списка, склеенный по 400 мс: агент шлёт статусы
  // пачками (ready/working у нескольких сессий подряд), и без склейки список
  // дёргался бы на каждое.
  const eventRefreshTimer = useRef<ReturnType<typeof setTimeout> | null>(null);
  useEffect(() => {
    if (!pollEnabled) return;
    const off = onWSEvent((ev) => {
      const type = (ev as { type?: string })?.type;
      if (type !== "pty_event" && type !== "pty_list_changed") return;
      if (document.visibilityState !== "visible") return;
      if (eventRefreshTimer.current) return;
      eventRefreshTimer.current = setTimeout(() => {
        eventRefreshTimer.current = null;
        void refresh();
      }, 400);
    });
    return () => {
      off();
      if (eventRefreshTimer.current) {
        clearTimeout(eventRefreshTimer.current);
        eventRefreshTimer.current = null;
      }
    };
  }, [pollEnabled, refresh]);

  // Возраст вывода и честное оставшееся окно dead-scrollback тикают только
  // на видимом экране. В фоне телефон не просыпается каждую секунду.
  const needsClock = sessions.some(
    (s) => s.status === "working" || s.status === "stalled" || !s.alive,
  );
  useEffect(() => {
    if (!needsClock) return;
    let timer = 0;
    const start = () => {
      if (timer || document.visibilityState !== "visible") return;
      setNowMs(Date.now());
      timer = window.setInterval(() => setNowMs(Date.now()), 1000);
    };
    const stop = () => {
      if (timer) window.clearInterval(timer);
      timer = 0;
    };
    const onVisibility = () => (document.visibilityState === "visible" ? start() : stop());
    start();
    document.addEventListener("visibilitychange", onVisibility);
    return () => {
      stop();
      document.removeEventListener("visibilitychange", onVisibility);
    };
  }, [needsClock]);

  useEffect(() => {
    const liveIds = new Set(sessions.map((s) => s.id));
    setSelectedIds((prev) => prev.filter((id) => liveIds.has(id)));
  }, [sessions]);

  // Гард от дабл-тапа: без него два быстрых тапа по плитке создавали два
  // терминала. ref — синхронный (setState не успевает до второго тапа),
  // state — для визуального busy на плитках.
  const creatingRef = useRef(false);
  const agentAfterCreateRef = useRef(false);
  // Аккаунт из `?account=` (аудит ИА 02.09.2026, D4): «Войти» у аккаунта в
  // разделе «Агенты» приводит сюда, а запуск происходит уже в терминале —
  // выбранный аккаунт надо донести туда, иначе шторка запустит под активным.
  const agentAccountRef = useRef("");
  const [creating, setCreating] = useState(false);

  /**
   * Последний известный агент терминала.
   *
   * У ЗАВЕРШЁННОЙ сессии сервер agent_kind не отдаёт вовсе (infoOf возвращает
   * dead-инфо до заполнения FgProcess), поэтому пять серых карточек
   * «powershell · project» неразличимы — по ним нельзя понять, где работал
   * Claude Code с нужной беседой. Экран терминала пишет вид агента в
   * localStorage, и для терминалов, открывавшихся на этом устройстве, память
   * возвращается сразу; серверное поле нужно только для чужих устройств.
   */
  const lastAgentCache = useRef(new Map<string, string>());
  const lastAgentOf = (s: PtySession): string => {
    if (isAgentKind(s.agent_kind)) {
      lastAgentCache.current.set(s.id, s.agent_kind!);
      return s.agent_kind!;
    }
    // Кэш на маунт: заголовок карточки считается на каждый тик часов и для
    // каждой сессии, а localStorage — синхронный. Экран терминала (единственный
    // писатель) живёт на другом маршруте, поэтому вернувшись сюда, компонент
    // перемонтируется и кэш соберётся заново.
    const hit = lastAgentCache.current.get(s.id);
    if (hit !== undefined) return hit;
    let stored = "";
    try { stored = localStorage.getItem(`pty.lastAgent.${s.id}`) || ""; } catch { /* приватный режим */ }
    lastAgentCache.current.set(s.id, stored);
    return stored;
  };

  // Реестр агентов подтягиваем лениво: он нужен, только когда в списке есть
  // завершённый терминал с известным агентом (иначе лишний запрос на каждый
  // заход в раздел).
  // Рабочая папка по умолчанию — один запрос при открытии списка. Отказ
  // молчаливый: не приехала — терминал откроется как раньше, в текущей папке.
  useEffect(() => {
    getConfig().then((cfg) => {
      defaultCwdRef.current = (cfg.default_cwd || "").trim();
      platformRef.current = (cfg.platform || "").trim().toLowerCase();
    }).catch(() => {});
  }, []);

  useEffect(() => {
    if (resumeLoadedRef.current) return;
    if (!sessions.some((s) => !s.alive && lastAgentOf(s))) return;
    resumeLoadedRef.current = true;
    getAgents().then((d) => {
      const agents = d.agents || [];
      setAgentRegistry(agents);
      const map: Record<string, AgentInfo> = {};
      for (const a of agents) {
        if (a.supports_resume && a.resume_cli) {
          map[a.id] = a;
        }
      }
      setResumeCmds(map);
      // Отказ не переспрашиваем: список обновляется поллингом каждые 6с, и
      // повтор превратился бы в шторм запросов к недоступному ПК. Кнопка
      // «Перезапустить в этой папке» работает и без реестра.
    }).catch(() => { /* остаёмся без «Продолжить» до следующего захода */ });
  }, [sessions]);

  const handleCreate = async (cwd: string, shell = "") => {
    // Рабочая папка по умолчанию НАКОНЕЦ применяется.
    //
    // Вопрос владельца 09.08.2026: «зачем нам папка по умолчанию?». Ответ по
    // коду оказался неприятным: она не применялась НИГДЕ, кроме старого экрана
    // сессий. Новый терминал открывался в `"."` — там, где стоит процесс
    // агента, то есть в случайном для человека месте. Настройка была, смысл
    // был, связи между ними не было.
    if (!cwd) cwd = defaultCwdRef.current;
    if (creatingRef.current) {
      // Молчаливый выход из гарда читался как «кнопка сломана»: через облако
      // создание идёт 1–3 секунды, и человек за это время успевает тапнуть ещё.
      toast(t("pty.creatingAlready"));
      return;
    }
    creatingRef.current = true;
    setCreating(true);
    // Кнопки на экране показывают «Открываем…», но после выбора папки шторка
    // закрывается, и нижний ряд может быть за пределами видимой части списка.
    // Поэтому если ответ идёт дольше «мгновенного» (облако — 1–3 с), говорим
    // об этом тостом; в LAN он не успевает появиться.
    const slowHint = window.setTimeout(() => toast(t("pty.creating")), 700);
    try {
      const res = await createPtySession(cwd || ".", shell, 80, 24);
      trackFirstTerminal();
      hapticSuccess();
      const account = agentAccountRef.current;
      openTerminal(res.id, agentAfterCreateRef.current
        ? { agent: "1", ...(account ? { account } : {}) }
        : undefined);
      agentAfterCreateRef.current = false;
      agentAccountRef.current = "";
    } catch (e: any) {
      toastError(mapApiError(e));
    } finally {
      window.clearTimeout(slowHint);
      agentAfterCreateRef.current = false;
      agentAccountRef.current = "";
      creatingRef.current = false;
      setCreating(false);
    }
  };

  /**
   * Открыть выбор папки под конкретное намерение.
   *
   * «Запустить AI-агента» — главная ценность продукта, и до сих пор она
   * включалась ТОЛЬКО диплинком `?agent=1` из чеклиста первых шагов: обе кнопки
   * создания на экране просто открывали шторку, а выбор агента приходилось
   * искать в терминале под «⋮ Ещё». Намерение выбирается здесь, до шторки —
   * поэтому и подписи в ней честные («Открыть и запустить агента»).
   */
  const openFolderSheet = (intent: "terminal" | "agent", start = "", withHaptic = true) => {
    if (withHaptic) haptic();
    agentAfterCreateRef.current = intent === "agent";
    setFolderIntent(intent);
    setFolderStart(start);
    setFolderOpen(true);
  };

  // Закрыли шторку, ничего не выбрав — намерение не должно пережить отмену и
  // сработать на следующем созданном терминале.
  const closeFolderSheet = () => {
    setFolderOpen(false);
    agentAfterCreateRef.current = false;
    agentAccountRef.current = "";
    setFolderIntent("terminal");
  };

  // Deep-link с главной: плитка «Быстрого запуска» (`?shell=&cwd=`) должна
  // сразу открыть терминал — подпись плитки обещает запуск, а не переход в
  // список; `?new=1` открывает выбор папки. Параметры одноразовые: сразу
  // вычищаем их из URL, иначе возврат назад/refresh создаст второй терминал.
  const deepLinkDone = useRef(false);
  useEffect(() => {
    if (deepLinkDone.current) return;
    const shell = searchParams.get("shell");
    const cwd = searchParams.get("cwd");
    const wantNew = searchParams.get("new");
    // `agent=1` — «после создания открыть шторку агентов»; раздел «Агенты»
    // передаёт сюда и идентификатор агента (`agent=gemini`) — намерение то же.
    const wantAgent = !!searchParams.get("agent");
    const wantInstall = searchParams.get("install") === "1";
    const wantAccount = searchParams.get("account") || "";
    const wantSsh = searchParams.get("ssh");
    if (!shell && !cwd && !wantNew && !wantSsh) return;
    deepLinkDone.current = true;
    // Запоминаем ДО очистки параметров — иначе «Назад» из созданного терминала
    // приведёт в этот список, где человек не был (он нажал плитку на главной).
    deepLinkFrom.current = searchParams.get("from") || "";
    agentAfterCreateRef.current = wantAgent;
    agentAccountRef.current = wantAgent ? wantAccount : "";
    setSearchParams(new URLSearchParams(), { replace: true });
    if (wantSsh) {
      navigate("/ssh", { replace: true });
    } else if (shell || cwd) {
      void handleCreate(cwd || "", shell || "");
    } else if (wantAgent && wantInstall) {
      // Аудит ИА 02.09.2026, P1-29: «Установить» из раздела «Агенты» открывало
      // выбор папки, хотя `npm i -g` от папки не зависит — вопрос без смысла
      // перед первым же шагом. Терминал открываем сразу, в домашней папке
      // (quick-paths отдаёт её строкой «Home»); не ответил — папка по умолчанию.
      void getQuickPaths()
        .then((d) => (d.paths || []).find((p) => p.name === "Home")?.path || "")
        .catch(() => "")
        .then((home) => handleCreate(home));
    } else {
      openFolderSheet(wantAgent ? "agent" : "terminal", "", false);
    }
  }, [searchParams, setSearchParams]);

  // «Быстрый запуск» оправдывает имя: тап — сразу терминал в этой папке.
  const openBrowser = (path: string) => {
    openFolderSheet("terminal", path);
  };
  const openSsh = () => {
    haptic();
    navigate("/ssh");
  };

  /**
   * Второстепенные действия плитки — листом, а не значками в её углах.
   *
   * Раньше на плитке 104×76 стояли ТРИ цели по 24 px: 🤖 «агент здесь»,
   * 📌/★ «в избранное» и сама плитка. Замер: 19 целей меньше 44 px на одном
   * экране, промах гарантирован — и промахивались как раз в необратимую
   * сторону (вместо агента снимали закладку). Теперь у плитки ровно одно
   * очевидное действие — тап открывает терминал в этой папке, — а остальное
   * живёт в листе, как действия машины в «Моих компьютерах».
   *
   * Открывают лист три жеста: долгий тап (телефон), правый клик и клавиша
   * «Меню» (обе дают событие contextmenu, поэтому клавиатура ничего не
   * теряет — до этого угловые кнопки были её единственным путём).
   */
  const openTileMenu = (q: QuickTile) => {
    // Любой путь к листу гасит отложенный тап: иначе одно нажатие открывало
    // разом и лист, и терминал (contextmenu Chrome срабатывает на тех же
    // ~500 мс, что и наш таймер).
    if (longPressTimer.current) {
      window.clearTimeout(longPressTimer.current);
      longPressTimer.current = null;
    }
    longPressFired.current = true;
    haptic("medium");
    setTileMenu(q);
  };

  /**
   * Прокрутка ленты — не нажатие на плитку.
   *
   * Замер 01.09.2026 (настоящий touch-жест через CDP): свайп по ленте
   * ОТКРЫВАЛ ТЕРМИНАЛ — палец опускался на плитку, лента ехала под ним, и
   * `pointerup` на той же плитке считался выбором папки. Ровно эта жалоба уже
   * была про ряд быстрых команд («когда скроллю, иногда попадаю по ним»), и
   * правила для неё написаны и покрыты тестами в ptyTerm/tapGuard.ts — здесь
   * они просто не применялись.
   */
  const tapGuardRef = useRef(emptyTapGuard());

  const quickTilePressStart = (q: QuickTile) => {
    longPressFired.current = false;
    if (longPressTimer.current) window.clearTimeout(longPressTimer.current);
    longPressTimer.current = window.setTimeout(() => openTileMenu(q), 500);
  };
  const quickTilePressEnd = (path: string) => {
    if (longPressTimer.current) {
      window.clearTimeout(longPressTimer.current);
      longPressTimer.current = null;
    }
    // Палец ехал, лента ещё скользит — терминал не открываем.
    if (!longPressFired.current && tapVerdict(tapGuardRef.current, Date.now()).ok) handleCreate(path);
  };
  const quickTilePressCancel = () => {
    if (longPressTimer.current) {
      window.clearTimeout(longPressTimer.current);
      longPressTimer.current = null;
    }
    longPressFired.current = false;
  };

  /**
   * Агент в папке плитки одним действием.
   *
   * До этого «спросить Claude во вчерашнем проекте» стоило 5–6 тапов: кнопка
   * агента → шторка на «Избранном» → провалиться в папку → серая кнопка внизу
   * → шторка агентов → «Запустить». Плитка знает папку, а `?agent=1` открывает
   * выбор агента прямо в созданном терминале (тот же путь, что у диплинка).
   */
  const quickTileAgent = (path: string) => {
    haptic("medium");
    agentAfterCreateRef.current = true;
    void handleCreate(path);
  };

  // Лента плиток шире экрана (замер: 812 px при видимых 388), а признака
  // прокрутки не было ни одного — половина закреплённых папок для человека
  // просто не существовала. Тени у краёв включаются по реальному положению
  // ленты, поэтому «есть что листать» видно и слева, и справа.
  const railRef = useRef<HTMLDivElement | null>(null);
  const [railEdges, setRailEdges] = useState({ left: false, right: false });
  const syncRailEdges = useCallback(() => {
    const el = railRef.current;
    if (!el) return;
    const left = el.scrollLeft > 4;
    const right = el.scrollLeft + el.clientWidth < el.scrollWidth - 4;
    setRailEdges((prev) => (prev.left === left && prev.right === right ? prev : { left, right }));
  }, []);

  /**
   * Имя правится ТАМ, ГДЕ ОНО НАПИСАНО, — прямо в карточке.
   *
   * История этого места за один день, три обращения владельца подряд:
   *
   * 1. поле в карточке сохраняло имя по `onBlur` — и любая потеря фокуса
   *    (клавиатура, перерисовка списка опросом, тап мимо, системная панель)
   *    молча записывала СТАРОЕ имя и закрывала ввод. Симптом его словами:
   *    «нажимаю переименовать — сразу пишет "имя сохранено", и всё»;
   * 2. я заменил поле диалогом — дефект ушёл, но вместе с ним ушла и правка на
   *    месте: «раньше я название менял прямо там, где название терминала,
   *    теперь в отдельном окне»;
   * 3. поэтому поле вернулось, но БЕЗ сохранения по потере фокуса.
   *
   * Сохранение теперь только явное: «✓», Enter. Отмена — «✕» или Escape.
   * `onBlur` не делает НИЧЕГО: ввод переживает и клавиатуру, и обновление
   * списка, и промах пальцем. Опрос на время правки выключен (`enabled` в
   * `usePolling`), чтобы карточка не перерисовывалась под руками.
   */
  const startRename = (s: PtySession) => {
    setRenameId(s.id);
    setRenameDraft(s.name || "");
  };

  /** Сохранение — ТОЛЬКО по явному действию: «✓» или Enter. */
  const handleRenameSave = async () => {
    if (!renameId) return;
    const id = renameId;
    const name = renameDraft.trim();
    try {
      await renamePty(id, name);
      hapticSuccess();
      toastSuccess(t("pty.renameDone"));
      setRenameId(null);
      setRenameDraft("");
      refresh();
    } catch (e: any) {
      // Ввод НЕ закрываем: имя не сохранено, и человек не должен искать, куда
      // делся его текст.
      toastError(mapApiError(e));
    }
  };

  const handleRenameCancel = () => {
    setRenameId(null);
    setRenameDraft("");
  };

  const cardPressStart = (s: PtySession) => {
    if (renameId) return;
    cardLongPressFired.current = false;
    if (cardLongPressTimer.current) window.clearTimeout(cardLongPressTimer.current);
    cardLongPressTimer.current = window.setTimeout(() => {
      cardLongPressFired.current = true;
      haptic("medium");
      setMenuSession(s);
    }, 500);
  };
  const cardPressCancel = () => {
    if (cardLongPressTimer.current) {
      window.clearTimeout(cardLongPressTimer.current);
      cardLongPressTimer.current = null;
    }
  };
  const handleCardClick = (s: PtySession) => {
    if (suppressClickRef.current) {
      suppressClickRef.current = false;
      return;
    }
    if (cardLongPressFired.current) {
      cardLongPressFired.current = false;
      return;
    }
    // Пока правят имя, тап по карточке не уносит в терминал: иначе первое же
    // касание рядом с полем закрывало бы правку сменой экрана.
    if (renameId) return;
    if (selectionMode) {
      setSelectedIds((prev) => (
        prev.includes(s.id) ? prev.filter((id) => id !== s.id) : [...prev, s.id]
      ));
      return;
    }
    openTerminal(s.id);
  };

  const handleClose = async (s: PtySession) => {
    // Спрашиваем и у завершённой карточки — просто о другом. У живого терминала
    // на кону процессы, у мёртвого — единственная копия вывода: соседняя кнопка
    // обещает «⤓ Сохранить лог», массовая операция об удалении честно
    // предупреждает, а одиночный ✕ (в 6 px от «⋮») уносил лог молча.
    const question = s.alive ? t("confirm.closeTerminal") : t("pty.closeDeadOneConfirm");
    if (!(await tgConfirm(question, { danger: true, confirmText: t("confirm.btn.closeTerminal") }))) return;
    try {
      await closePtySession(s.id);
      haptic("medium");
      toastSuccess(t("toast.terminalClosed"));
      setMenuSession(null);
      refresh();
    } catch (e: any) {
      toastError(mapApiError(e));
    }
  };

  /**
   * Перезапуск завершённого терминала в той же папке.
   *
   * resume — команда «продолжить беседу» того агента, что работал здесь: с ней
   * терминал возвращается В РАЗГОВОР (claude --continue), а не в пустой шелл.
   * Без неё поведение прежнее.
   */
  /**
   * Вернуть связь с ЖИВЫМ процессом терминала (`host_alive`).
   *
   * Это НЕ перезапуск: процесс работает, оборвался только канал до него.
   * Перезапуск здесь завёл бы второй терминал в той же папке поверх первого,
   * а работа первого продолжала бы идти в никуда.
   */
  const handleReattach = async (s: PtySession) => {
    if (reattachingId) return;
    setMenuSession(null);
    haptic();
    setReattachingId(s.id);
    try {
      await reattachPtySession(s.id);
      hapticSuccess();
      toast(t("pty.reattached"));
      await refresh();
      openTerminal(s.id);
    } catch (e) {
      // Связь вернуть не удалось. Кнопку убираем: процесс жив, но канал до него
      // не поднимается (так ведут себя терминалы, запущенные прежней версией
      // Remotai — их процесс отдаёт всю историю одним куском и рвёт связь).
      // Оставлять кнопку, которая заведомо не сработает, значит звать человека
      // жать её снова и снова.
      setNoReattach((ids) => (ids.includes(s.id) ? ids : [...ids, s.id]));
      toastError(mapApiError(e));
    } finally {
      setReattachingId("");
    }
  };

  const handleRestart = async (s: PtySession, resume?: { kind: string; agent: AgentInfo }) => {
    setMenuSession(null);
    if (s.kind === "ssh") {
      // Хост известен самой сессии — ведём на его карточку, где «⌨ Терминал»
      // подключает одним тапом. Общий список серверов был наказанием: нужный
      // хост приходилось искать заново среди двадцати.
      if (s.ssh_host_id) {
        navigate(`/ssh/${encodeURIComponent(s.ssh_host_id)}`);
        return;
      }
      navigate("/ssh");
      toast(t("pty.sshReconnectHint"));
      return;
    }
    if (creatingRef.current) {
      toast(t("pty.creatingAlready"));
      return;
    }
    creatingRef.current = true;
    setCreating(true);
    try {
      let resumeCommand = "";
      let resumeAccount: AgentAccount | null = null;
      if (resume) {
        if (!resume.agent.account_env) {
          // Агент без account contract не зависит от `/api/accounts`.
          resumeCommand = resume.agent.resume_cli || "";
        } else {
        // Conversation принадлежит записанному в PTY аккаунту, а не тому,
        // который человек сделал active позже. Удалённый профиль — стоп.
        const [accountsPayload, config] = await Promise.all([
          getAgentAccounts(undefined, s.cwd || "."),
          platformRef.current ? Promise.resolve(null) : getConfig(),
        ]);
        if (config?.platform) platformRef.current = config.platform.toLowerCase();
        const version = Number.isSafeInteger(accountsPayload.proxy_contract_version)
          ? Number(accountsPayload.proxy_contract_version)
          : 0;
        const accounts = accountsPayload.accounts || [];
        resumeAccount = recordedResumeAccount(accounts, resume.kind, s.account_id);
        const launchAccount = launchAccountForAgent(resume.agent, {
          accounts,
          proxyContractVersion: version,
          accountsReady: true,
        }, resumeAccount);
        if (resume.agent.account_env && !platformRef.current) {
          throw new Error(t("ui.ptylistview.m3bde919d7c"));
        }
        resumeCommand = composeLaunch(resume.agent.resume_cli || "", EMPTY_PREFS, {
          agentID: resume.agent.id,
          accountEnvName: resume.agent.account_env || "",
          proxyContractVersion: version,
          blockedReason: launchAccount?.blockedReason,
          account: launchAccount,
          posix: platformRef.current !== "windows",
          launchArgs: resume.agent.launch_args,
        });
        if (!resumeCommand) {
          throw new Error(launchAccount?.blockedReason || t("ui.ptylistview.mc16c569984"));
        }
        }
      }
      const created = await createPtySession(s.cwd || ".", s.shell || "", 80, 24);
      if (s.name) await renamePty(created.id, s.name);
      if (supportsGroups) {
        await setPtyPlacements([{ id: created.id, group: s.group || "", sort: s.sort || 0 }]);
      }
      if (resume) {
        await setPtyAccount(
          created.id,
          resumeAccount?.is_default ? "" : (resumeAccount?.id || ""),
          resumeAccount && !resumeAccount.is_default ? resumeAccount.label : "",
        );
        await ptyInput(created.id, { data: resumeCommand + "\r" });
        // Память об агенте переносим сразу: иначе бейдж на новой карточке
        // появится только после того, как её откроют на этом устройстве.
        try { localStorage.setItem(`pty.lastAgent.${created.id}`, resume.kind); } catch { /* ignore */ }
      }
      hapticSuccess();
      openTerminal(created.id);
    } catch (e: any) {
      toastError(mapApiError(e));
    } finally {
      creatingRef.current = false;
      setCreating(false);
    }
  };

  /**
   * Сохранить лог завершённого терминала.
   *
   * Тост показываем по ФАКТУ доставки: раньше зелёное «Экспортировано» уходило
   * всегда, а в мобильном Telegram blob-якорь молча не сохраняет ничего —
   * человек считал лог полученным и закрывал терминал, теряя его насовсем.
   * Если сохранять нечем, лог уходит файлом в чат (см. apk/src/ptyLogExport.ts).
   */
  const handleExport = async (s: PtySession) => {
    setMenuSession(null);
    try {
      const how = await savePtyLog(
        s.id, "txt", ptyLogFileName(s.name || cardTitle(s), "txt"), botAvailable,
      );
      if (how === "failed") {
        toastError(t("pty.exportUnavailable"));
        return;
      }
      hapticSuccess();
      toastSuccess(
        how === "telegram" ? t("pty.exportSentTg")
          : how === "shared" ? t("pty.exportShared")
            : t("pty.exportSaved"),
      );
    } catch (e: any) {
      if (!isShareCancel(e)) toastError(`${t("pty.exportFailed")}: ${mapApiError(e)}`);
    }
  };

  /** Явная доставка лога ботом — там, где сохранение файла ненадёжно. */
  const handleExportToTelegram = async (s: PtySession) => {
    setMenuSession(null);
    toast(t("pty.exportSending"));
    try {
      await sendPtyLogToTelegram(s.id, "txt", ptyLogFileName(s.name || cardTitle(s), "txt"));
      hapticSuccess();
      toastSuccess(t("pty.exportSentTg"));
    } catch (e: any) {
      toastError(`${t("pty.exportFailed")}: ${mapApiError(e)}`);
    }
  };

  const beginSelection = (s?: PtySession) => {
    setMenuSession(null);
    setSelectionMode(true);
    setSelectedIds(s ? [s.id] : []);
  };

  const endSelection = () => {
    setSelectionMode(false);
    setSelectedIds([]);
  };

  const closeMany = async (targets: PtySession[], label: string) => {
    if (targets.length === 0) return;
    if (!(await tgConfirm(label, { danger: true, confirmText: t("confirm.btn.closeTerminal") }))) return;
    const results = await Promise.allSettled(targets.map((s) => closePtySession(s.id)));
    const closed = results.filter((r) => r.status === "fulfilled").length;
    if (closed > 0) {
      haptic("medium");
      toastSuccess(t("pty.closedTerminals", { n: closed }));
    }
    const failed = results.length - closed;
    if (failed > 0) toastError(t("pty.bulkCloseFailed", { n: failed }));
    endSelection();
    await refresh();
  };

  const closeFolder = async (name: string) => {
    const targets = sessions.filter((s) => (s.group || "") === name);
    await closeMany(targets, t("pty.closeFolderConfirm", { name: name || t("pty.ungrouped"), n: targets.length }));
  };

  // Массовое «убрать завершённые» удаляет логи насовсем, а карточки рядом
  // обещают «лог ещё N мин» и дают «⤓ Сохранить лог» — поэтому спрашиваем.
  // Одиночный ✕ мёртвой карточки теперь спрашивает тоже (см. handleClose):
  // необратимость одна и та же, а промах по «⋮» рядом уносил лог без вопроса.
  const handleCloseDead = async () => {
    const dead = sessions.filter((s) => !s.alive);
    if (dead.length === 0) {
      toast(t("pty.noDeadSessions"));
      return;
    }
    if (!(await tgConfirm(
      t("pty.closeDeadConfirm", { n: dead.length }),
      { danger: true, confirmText: t("confirm.btn.closeDead") },
    ))) return;
    try {
      const res = await closeDeadPtySessions();
      haptic("medium");
      refresh();
      if (res.closed === 0) toast(t("pty.noDeadSessions"));
      else toastSuccess(t("pty.closedDead", { n: res.closed }));
    } catch (e: any) {
      toastError(mapApiError(e));
    }
  };

  const shortPath = (p: string) => {
    const parts = p.replace(/\\/g, "/").split("/");
    if (parts.length <= 3) return p;
    return ".../" + parts.slice(-2).join("/");
  };

  // Тот же канон, что на главной («Claude · myproj»): без preferAgent список
  // называл терминал с работающим агентом «powershell · project», хотя на
  // главной он же подписан именем агента. Для завершённых имя агента берётся из
  // локальной памяти (сервер его уже не отдаёт) — см. lastAgentOf.
  const cardTitle = (s: PtySession) => ptyDisplayTitle(
    { ...s, agent_kind: lastAgentOf(s) || s.agent_kind },
    { preferAgent: true },
  );

  /** Время последнего успешного ответа — «Данные на 09:41». */
  const clockAt = (ms: number) => new Date(ms).toLocaleTimeString(getLocale(), {
    hour: "2-digit",
    minute: "2-digit",
  });

  // Статус-бейдж: «работает / готово · когда / ждёт ввода / ошибка». Если бэкенд
  // ещё старый (нет status) — деградируем к alive/dead.
  // icon — ReactNode, а не строка: знаки статуса переехали на общий линейный
  // набор. Размер задаём пропом (size=11 под .pty-status-icon, styles.css):
  // у SVG величина от font-size не зависит, и без явного числа пилюля распухла
  // бы с 11 до 20 px и разорвала высоту строки карточки.
  const statusInfo = (s: PtySession) => {
    const info = ptyListStatus(s, nowMs, !!lastAgentOf(s));
    return { ...info, icon: ptyStatusIcon(info.icon) };
  };

  // Значок плитки больше не хранится строкой: он выводится из `pinned` и
  // рисуется линейной иконкой (звезда — избранное, часы — недавняя). Эмодзи
  // рисовались системным шрифтом и на Android, iOS и Windows выглядели
  // по-разному, споря с мятно-графитовой палитрой бренда.
  const quickLaunch = (() => {
    const seen = new Set<string>();
    const out: QuickTile[] = [];
    for (const b of bookmarks) {
      if (seen.has(b.path)) continue;
      seen.add(b.path);
      out.push({ name: b.name, path: b.path, pinned: true });
    }
    for (const r of recent) {
      if (seen.has(r.path)) continue;
      seen.add(r.path);
      out.push({ name: r.name, path: r.path, pinned: false });
    }
    return out.slice(0, 10);
  })();

  // Мышью лента листается тем же колесом, что и страница: пальцем она ездит, а
  // в окне на ПК до дальних плиток было не добраться вовсе — полоса прокрутки
  // спрятана дизайном («на компе не скроллится избранное», владелец 01.09.2026).
  // Зависимости те же, что у теней: лента появляется позже первого рендера.
  useHorizontalWheel(railRef, [quickLaunch.length, toolsCollapsed]);

  // Тени краёв ленты пересчитываем при её появлении/смене состава и при
  // повороте экрана: сам скролл сообщает о себе через onScroll.
  useEffect(() => {
    syncRailEdges();
    window.addEventListener("resize", syncRailEdges);
    return () => window.removeEventListener("resize", syncRailEdges);
  }, [syncRailEdges, quickLaunch.length]);

  // Избранное и недавние — разной судьбы: избранное переживает рестарт, а
  // недавние вычисляются из живых сессий и уходят вместе с ними. Поэтому
  // «закрепить» здесь и означает «не потерять». Зовут отсюда — из листа
  // плитки: собственной кнопки-звезды на плитке больше нет.
  const togglePin = async (q: { name: string; path: string; pinned: boolean }) => {
    haptic();
    try {
      if (q.pinned) {
        await removeBookmark(q.path);
        setBookmarks((prev) => prev.filter((b) => b.path !== q.path));
        toast(t("pty.unpinnedFolder"));
      } else {
        await addBookmark(q.name, q.path);
        setBookmarks((prev) => [...prev, { name: q.name, path: q.path }]);
        hapticSuccess();
        toastSuccess(t("pty.pinnedFolder"));
      }
    } catch (e: any) {
      toastError(mapApiError(e));
      getBookmarks().then((d) => setBookmarks(d.bookmarks || [])).catch(() => {});
    }
  };

  // ── Группы: производная структура и операции ─────────────────
  const normalizedStatus = (s: PtySession): PtyStatusFilter => {
    const raw = s.status || (s.alive ? "working" : "dead");
    // «stalled» — тот же «работает», только без вывода: так его считает и
    // главная (deviceSummaries.ts). Иначе подвисший агент выпадал из ОБОИХ
    // фильтров, из счётчиков чипов и из сводки в шапке группы — то есть
    // прятался ровно в том случае, ради которого открывают список.
    const value = raw === "stalled" ? "working" : raw;
    return value === "waiting" || value === "working" || value === "error" || value === "dead"
      ? value
      : "all";
  };
  const q = query.trim().toLocaleLowerCase("ru");
  const visibleSessions = sessions.filter((s) => {
    if (statusFilter !== "all" && normalizedStatus(s) !== statusFilter) return false;
    if (!q) return true;
    return [
      s.name, s.cwd, s.fg_process, s.agent_kind, s.ssh_host, s.ssh_user,
      cardTitle(s),
    ].some((value) => (value || "").toLocaleLowerCase("ru").includes(q));
  });
  const filtersActive = q !== "" || statusFilter !== "all";
  const statusCounts: Record<Exclude<PtyStatusFilter, "all">, number> = {
    waiting: sessions.filter((s) => normalizedStatus(s) === "waiting").length,
    working: sessions.filter((s) => normalizedStatus(s) === "working").length,
    error: sessions.filter((s) => normalizedStatus(s) === "error").length,
    dead: sessions.filter((s) => normalizedStatus(s) === "dead").length,
  };
  // Считаем по alive, как и сама операция «убрать завершённые» на агенте.
  const deadCount = sessions.filter((s) => !s.alive).length;
  const allOrderedGroups = groupOrdered(sessions);
  const allFolders = allOrderedGroups.filter((g) => g.name !== "");
  const visibleOrderedGroups = groupOrdered(visibleSessions);
  const folders = visibleOrderedGroups.filter((g) => g.name !== "");
  const ungrouped = visibleOrderedGroups.find((g) => g.name === "")?.items ?? [];
  const discoveredFolderNames = allFolders.map((folder) => folder.name);
  const allFolderNames = supportsGroups
    ? [
        ...pendingFolders,
        ...discoveredFolderNames
          .filter((name) => !pendingFolders.includes(name))
          .sort((a, b) => a.localeCompare(b, "ru")),
      ]
    : [];
  const folderNames = supportsGroups
    ? filtersActive
      ? allFolderNames.filter((name) => folders.some((folder) => folder.name === name))
      : allFolderNames
    : [];

  const persistFolderRegistry = async (next: string[]) => {
    const clean = [...new Set(next.map((name) => name.trim()).filter(Boolean))];
    setPendingFolders(clean);
    try { localStorage.setItem("ptyFolders", JSON.stringify(clean)); } catch { /* ignore */ }
    if (!supportsGroups) return;
    try {
      await setPtyFolders(clean);
    } catch (e: any) {
      // Локальная копия всё равно сохраняется: так группа не исчезает при
      // работе со старым агентом, а новый агент синхронизирует её сам.
      toastError(mapApiError(e));
    }
  };

  const applyPlacements = async (items: { id: string; group: string; sort: number }[], optimistic: PtySession[]) => {
    setSessions(optimistic);
    try {
      await setPtyPlacements(items);
    } catch (e: any) {
      toastError(mapApiError(e));
      refresh();
    }
  };

  const handleNewFolder = async () => {
    haptic();
    const name = (await promptDialog(t("pty.folderNamePrompt")))?.trim();
    if (!name) return;
    if (allFolderNames.includes(name)) return;
    await persistFolderRegistry([...pendingFolders, name]);
  };

  const handleRenameFolder = async (name: string) => {
    const next = (await promptDialog(t("pty.renameFolder"), { defaultValue: name }))?.trim();
    if (!next || next === name) return;
    const members = sessions.filter((s) => (s.group || "") === name);
    await persistFolderRegistry([
      ...pendingFolders.filter((n) => n !== name && n !== next),
      next,
    ]);
    if (members.length === 0) {
      return;
    }
    hapticSuccess();
    await applyPlacements(
      members.map((s) => ({ id: s.id, group: next, sort: s.sort || 0 })),
      sessions.map((s) => ((s.group || "") === name ? { ...s, group: next } : s)),
    );
  };

  const handleDeleteFolder = async (name: string) => {
    // Не danger: терминалы остаются открытыми, группа лишь расформировывается.
    if (!(await tgConfirm(t("pty.deleteFolderConfirm", { name }), { confirmText: t("confirm.btn.disbandFolder") }))) return;
    const members = sessions.filter((s) => (s.group || "") === name);
    await persistFolderRegistry(pendingFolders.filter((n) => n !== name));
    if (members.length === 0) {
      return;
    }
    haptic("medium");
    // sort=0 → вернутся в порядок по умолчанию (новые сверху).
    await applyPlacements(
      members.map((s) => ({ id: s.id, group: "", sort: 0 })),
      sessions.map((s) => ((s.group || "") === name ? { ...s, group: "", sort: 0 } : s)),
    );
  };

  const moveToFolder = async (group: string) => {
    const request = moveRequest;
    if (!request || request.ids.length === 0) return;
    const chosen = new Set(request.ids);
    const existing = sessions
      .filter((s) => !chosen.has(s.id) && (s.group || "") === group)
      .sort((a, b) => effSort(a) - effSort(b) || a.id.localeCompare(b.id));
    const base = existing.reduce((max, s) => Math.max(max, s.sort || 0), 0);
    const items = request.ids.map((id, index) => ({
      id,
      group,
      sort: base + (index + 1) * 10,
    }));
    const optimistic = sessions.map((s) => {
      const item = items.find((candidate) => candidate.id === s.id);
      return item ? { ...s, group: item.group, sort: item.sort } : s;
    });
    if (group && !pendingFolders.includes(group)) {
      await persistFolderRegistry([...pendingFolders, group]);
    }
    setMoveRequest(null);
    await applyPlacements(items, optimistic);
    if (request.source === "selection") endSelection();
    hapticSuccess();
    toastSuccess(t("pty.moved", { n: items.length }));
  };

  const moveToNewFolder = async () => {
    const name = (await promptDialog(t("pty.folderNamePrompt")))?.trim();
    if (!name) return;
    if (!pendingFolders.includes(name)) {
      await persistFolderRegistry([...pendingFolders, name]);
    }
    await moveToFolder(name);
  };

  const toggleCollapse = (name: string) => {
    haptic();
    setCollapsedMap((prev) => {
      const next = { ...prev, [name]: !prev[name] };
      try { localStorage.setItem("ptyFoldersCollapsed", JSON.stringify(next)); } catch { /* ignore */ }
      return next;
    });
  };

  const toggleTools = () => {
    haptic();
    setToolsCollapsed((prev) => {
      const next = !prev;
      try { localStorage.setItem("ptyToolsCollapsed", next ? "1" : "0"); } catch { /* ignore */ }
      return next;
    });
  };

  const scrollToSession = (id: string) => {
    requestAnimationFrame(() => {
      document.querySelector<HTMLElement>(`[data-pty-id="${id}"]`)?.scrollIntoView({
        block: "center",
        behavior: window.matchMedia?.("(prefers-reduced-motion: reduce)").matches ? "auto" : "smooth",
      });
    });
  };

  const jumpToFolderStatus = (name: string, status: "waiting" | "working" | "error") => {
    const target = sessions.find((s) => (s.group || "") === name && normalizedStatus(s) === status);
    if (!target) return;
    setCollapsedMap((prev) => {
      if (!prev[name]) return prev;
      const next = { ...prev, [name]: false };
      try { localStorage.setItem("ptyFoldersCollapsed", JSON.stringify(next)); } catch { /* ignore */ }
      return next;
    });
    scrollToSession(target.id);
  };

  // Новый эпизод ожидания/ошибки не должен прятаться в свёрнутой группе.
  // Ключ включает status_at: пользователь может свернуть группу обратно, и
  // тот же эпизод больше не будет насильно раскрывать её.
  useEffect(() => {
    const active = new Set<string>();
    const foldersToOpen = new Map<string, string>();
    for (const s of sessions) {
      const status = normalizedStatus(s);
      if (status !== "waiting" && status !== "error") continue;
      const key = `${s.id}:${status}:${s.status_at || 0}`;
      active.add(key);
      if (!attentionEpisodes.current.has(key) && collapsedMap[s.group || ""]) {
        foldersToOpen.set(s.group || "", s.id);
      }
    }
    attentionEpisodes.current = active;
    if (foldersToOpen.size === 0) return;
    setCollapsedMap((prev) => {
      const next = { ...prev };
      for (const folder of foldersToOpen.keys()) next[folder] = false;
      try { localStorage.setItem("ptyFoldersCollapsed", JSON.stringify(next)); } catch { /* ignore */ }
      return next;
    });
    const first = foldersToOpen.values().next().value as string | undefined;
    if (first) scrollToSession(first);
  }, [sessions, collapsedMap]);

  // ── Drag-and-drop за ручку ⠿ ─────────────────────────────────
  const stopAutoScroll = () => {
    autoScrollDirection.current = 0;
    if (autoScrollRAF.current) cancelAnimationFrame(autoScrollRAF.current);
    autoScrollRAF.current = 0;
  };

  const setAutoScroll = (clientY: number) => {
    const top = document.querySelector(".page-header")?.getBoundingClientRect().bottom ?? 0;
    const bottom = window.innerHeight;
    const direction = clientY < top + 48 ? -1 : clientY > bottom - 48 ? 1 : 0;
    if (direction === autoScrollDirection.current) return;
    stopAutoScroll();
    if (!direction) return;
    autoScrollDirection.current = direction;
    const step = () => {
      if (!autoScrollDirection.current || !draggingRef.current) {
        stopAutoScroll();
        return;
      }
      window.scrollBy(0, autoScrollDirection.current * 11);
      autoScrollRAF.current = requestAnimationFrame(step);
    };
    autoScrollRAF.current = requestAnimationFrame(step);
  };

  useEffect(() => () => stopAutoScroll(), []);

  const dragStart = (e: ReactPointerEvent, s: PtySession) => {
    e.stopPropagation();
    (e.target as Element).setPointerCapture(e.pointerId);
    dragRef.current = { id: s.id, startX: e.clientX, startY: e.clientY, moved: false };
  };

  const dragMove = (e: ReactPointerEvent) => {
    const d = dragRef.current;
    if (!d) return;
    if (!d.moved) {
      if (Math.hypot(e.clientX - d.startX, e.clientY - d.startY) < 8) return;
      d.moved = true;
      draggingRef.current = true;
      setDraggingId(d.id);
      haptic("medium");
    }
    setAutoScroll(e.clientY);
    const el = document.elementFromPoint(e.clientX, e.clientY);
    const cardEl = el?.closest?.("[data-pty-id]");
    const folderEl = el?.closest?.("[data-folder-name]");
    if (cardEl) {
      const overId = cardEl.getAttribute("data-pty-id")!;
      if (overId === d.id) return;
      const over = sessions.find((x) => x.id === overId);
      if (!over) return;
      const group = over.group || "";
      const rect = cardEl.getBoundingClientRect();
      let beforeId: string | null = overId;
      if (e.clientY > rect.top + rect.height / 2) {
        // Ниже середины — вставить ПОСЛЕ этой карточки (перед следующей в группе).
        const items = groupOrdered(sessions).find((g) => g.name === group)?.items ?? [];
        const idx = items.findIndex((x) => x.id === overId);
        beforeId = idx >= 0 && idx + 1 < items.length ? items[idx + 1].id : null;
      }
      setDropPos((prev) => (prev && prev.group === group && prev.beforeId === beforeId ? prev : { group, beforeId }));
    } else if (folderEl) {
      const group = folderEl.getAttribute("data-folder-name")!;
      setDropPos((prev) => (prev && prev.group === group && prev.beforeId === null ? prev : { group, beforeId: null }));
    }
  };

  const dragFinish = (drop: boolean) => {
    stopAutoScroll();
    const d = dragRef.current;
    dragRef.current = null;
    draggingRef.current = false;
    setDraggingId(null);
    const pos = dropPos;
    setDropPos(null);
    if (!d || !d.moved) return;
    suppressClickRef.current = true; // click после pointerup не должен открыть терминал
    if (!drop || !pos) return;
    void applyDrop(d.id, pos);
  };

  const applyDrop = async (id: string, pos: DropPos) => {
    const groups = groupOrdered(sessions);
    let moved: PtySession | undefined;
    for (const g of groups) {
      const i = g.items.findIndex((x) => x.id === id);
      if (i >= 0) moved = g.items.splice(i, 1)[0];
    }
    if (!moved) return;
    moved = { ...moved, group: pos.group };
    let target = groups.find((g) => g.name === pos.group);
    if (!target) {
      target = { name: pos.group, items: [] };
      groups.push(target);
    }
    const idx = pos.beforeId ? target.items.findIndex((x) => x.id === pos.beforeId) : -1;
    if (idx >= 0) target.items.splice(idx, 0, moved);
    else target.items.push(moved);

    // Перенумеровываем все группы шагом 10 — одним bulk-запросом.
    const items: { id: string; group: string; sort: number }[] = [];
    const optimistic = sessions.map((s) => ({ ...s }));
    for (const g of groups) {
      g.items.forEach((s, i) => {
        const sort = (i + 1) * 10;
        items.push({ id: s.id, group: g.name, sort });
        const o = optimistic.find((x) => x.id === s.id)!;
        o.group = g.name;
        o.sort = sort;
      });
    }
    if (pos.group && !pendingFolders.includes(pos.group)) {
      await persistFolderRegistry([...pendingFolders, pos.group]);
    }
    haptic();
    await applyPlacements(items, optimistic);
  };

  const moveSessionBy = async (session: PtySession, direction: -1 | 1) => {
    const group = session.group || "";
    const ordered = groupOrdered(sessions).find((item) => item.name === group)?.items ?? [];
    const index = ordered.findIndex((item) => item.id === session.id);
    const target = index + direction;
    if (index < 0 || target < 0 || target >= ordered.length) return;

    const next = ordered.slice();
    [next[index], next[target]] = [next[target], next[index]];
    const items = next.map((item, itemIndex) => ({
      id: item.id,
      group,
      sort: (itemIndex + 1) * 10,
    }));
    const sortByID = new Map(items.map((item) => [item.id, item.sort]));
    const optimistic = sessions.map((item) => (
      sortByID.has(item.id) ? { ...item, sort: sortByID.get(item.id)! } : item
    ));

    setMenuSession(null);
    haptic();
    await applyPlacements(items, optimistic);
  };

  // Плоский список: заголовки папок + карточки (+ линия-индикатор во время drag).
  const listItems: ListItem[] = [];
  const pushCards = (group: string, cards: PtySession[]) => {
    for (const s of cards) {
      if (dropPos && dropPos.group === group && dropPos.beforeId === s.id) {
        listItems.push({ kind: "indicator", key: "before-" + s.id });
      }
      // Перетаскиваемую карточку НЕ убираем из рендера: её ручка держит
      // pointer capture — уберём, и drag оборвётся без pointerup.
      listItems.push({ kind: "card", s });
    }
    if (dropPos && dropPos.group === group && dropPos.beforeId === null) {
      listItems.push({ kind: "indicator", key: "end-" + group });
    }
  };
  for (const name of folderNames) {
    const cards = folders.find((f) => f.name === name)?.items ?? [];
    listItems.push({ kind: "header", name, count: cards.length });
    if (!collapsedMap[name]) pushCards(name, cards);
  }
  if (folderNames.length > 0 && (!filtersActive || ungrouped.length > 0)) {
    listItems.push({ kind: "header", name: "", count: ungrouped.length });
    if (!collapsedMap[""]) pushCards("", ungrouped);
  } else {
    pushCards("", ungrouped);
  }

  const renderFolderHeader = (name: string, count: number) => {
    const allItems = allOrderedGroups.find((group) => group.name === name)?.items ?? [];
    const summary = {
      waiting: allItems.filter((s) => normalizedStatus(s) === "waiting").length,
      working: allItems.filter((s) => normalizedStatus(s) === "working").length,
      error: allItems.filter((s) => normalizedStatus(s) === "error").length,
    };
    return (
    <div
      key={"folder-" + name}
      className={`pty-folder-head${dropPos && dropPos.group === name && dropPos.beforeId === null ? " drop-target" : ""}`}
      data-folder-name={name}
      onClick={() => toggleCollapse(name)}
    >
      <span className="pty-folder-chev">
        <IconChevron size={14} dir={collapsedMap[name] ? "right" : "down"} />
      </span>
      <span className="pty-folder-name">{name === "" ? t("pty.ungrouped") : name}</span>
      <span className="pty-folder-count">{count}</span>
      <span className="pty-folder-summary">
        {summary.waiting > 0 && (
          <button
            className="pty-folder-status waiting"
            title={t("pty.filterWaiting")}
            onClick={(e) => { e.stopPropagation(); jumpToFolderStatus(name, "waiting"); }}
          >
            <IconHourglass size={11} />{summary.waiting}
          </button>
        )}
        {summary.working > 0 && (
          <button
            className="pty-folder-status working"
            title={t("pty.filterWorking")}
            onClick={(e) => { e.stopPropagation(); jumpToFolderStatus(name, "working"); }}
          >
            {"●"}{summary.working}
          </button>
        )}
        {summary.error > 0 && (
          <button
            className="pty-folder-status error"
            title={t("pty.filterError")}
            onClick={(e) => { e.stopPropagation(); jumpToFolderStatus(name, "error"); }}
          >
            <IconWarning size={11} />{summary.error}
          </button>
        )}
      </span>
      {/* Было три значка без подписей — «⊗ ✎ 🗑», причём метафоры стояли
          наоборот: крест-в-круге на разрушительном «закрыть все терминалы»
          (там гибнет вся идущая работа) и корзина на безвредном
          «расформировать» (терминалы остаются открытыми, просто выходят из
          группы). Угадать по картинкам, какая из трёх что делает, нельзя.

          Стало: на поверхности ОДНО частое действие — закрыть терминалы
          группы — и оно подписано словом; редкие ушли в меню «⋮» текстовыми
          строками. Текст надёжнее любой метафоры: «Расформировать группу» ни с
          чем не спутать, а корзина спутывалась.

          Значок закрытия — крест, тот же, что закрывает одиночный терминал на
          карточке: «то же самое, но для всех». Корзина в этом продукте нигде
          ничего не закрывает, она означает «удалить навсегда». */}
      {name !== "" && (
        <>
          {allItems.length > 0 && (
            <button
              className="pty-folder-btn"
              aria-label={t("pty.closeFolder")}
              title={t("pty.closeFolder")}
              onClick={(e) => { e.stopPropagation(); void closeFolder(name); }}
            >
              <IconClose size={14} />{" "}{t("pty.closeFolderShort")}
            </button>
          )}
          <button
            className="pty-folder-btn"
            aria-label={t("pty.folderActions", { name })}
            title={t("pty.folderActions", { name })}
            onClick={(e) => { e.stopPropagation(); haptic(); setFolderMenu(name); }}
          >
            <IconDots size={16} />
          </button>
        </>
      )}
    </div>
    );
  };

  // Фокус в шторках: ставим внутрь при открытии и возвращаем на «⋮», с которого
  // её открыли, при закрытии (Esc/Назад закрывают их тем же useEscape, что и
  // остальные модалки продукта).
  const menuSheetRef = useSheetFocus(!!menuSession);
  const moveSheetRef = useSheetFocus(!!moveRequest);
  // Лист плитки открывается долгим тапом, поэтому фокус возвращать не на что —
  // но Esc и системный «Назад» обязаны закрывать его так же, как остальные.
  const tileSheetRef = useSheetFocus(!!tileMenu);
  // Меню группы открывается кнопкой «⋮» в шапке — фокус возвращается на неё.
  const folderSheetRef = useSheetFocus(!!folderMenu);
  const closeMenuSheet = useCallback(() => setMenuSession(null), []);
  const closeMoveSheet = useCallback(() => setMoveRequest(null), []);
  const closeTileSheet = useCallback(() => setTileMenu(null), []);
  const closeFolderMenu = useCallback(() => setFolderMenu(null), []);
  useEscape(!!menuSession, closeMenuSheet);
  useEscape(!!moveRequest && !menuSession, closeMoveSheet);
  useEscape(!!tileMenu, closeTileSheet);
  useEscape(!!folderMenu, closeFolderMenu);

  const menuGroupItems = menuSession
    ? groupOrdered(sessions).find((group) => group.name === (menuSession.group || ""))?.items ?? []
    : [];
  const menuGroupIndex = menuSession
    ? menuGroupItems.findIndex((session) => session.id === menuSession.id)
    : -1;

  return (
    <div className="page">
      <div className="page-header">
        <div className="page-header-context">
          <h1>{t("pty.title")}</h1>
          <DeviceChip />
        </div>
        <button className="header-action" onClick={() => setShowHelp(true)} aria-label={t("help.title")}>
          {"?"}
        </button>
      </div>

      <div className="page-content">
        {scopeControl}
        {/* Работа, прерванная перезагрузкой компьютера, — ПЕРВОЙ на экране.
            Человек, включивший компьютер, возвращается именно к ней, а не к
            закладкам и фильтрам: ниже блок оказывался на 419 px, то есть за
            половиной экрана. Состояние редкое (только после выключения), так
            что постоянный порядок экрана оно не ломает — блока просто нет,
            когда терять было нечего. */}
        {!offline && !loading && (
          <LostTerminals
            lost={lost}
            compact={sessions.some((session) => session.alive)}
            onOpen={(id) => openTerminal(id)}
            onChanged={() => void refresh()}
          />
        )}

        {/* Свёртка верхней панели — та же идиома, что у папок терминалов
            (▸/▾, клик по строке, состояние в localStorage). Строка остаётся
            всегда: она единственный способ развернуть панель обратно. */}
        <div className="pty-tools-heading">
        <button
          className="pty-tools-bar"
          onClick={toggleTools}
          aria-expanded={!toolsCollapsed}
        >
          <span className="pty-folder-chev" aria-hidden="true">
            <IconChevron size={14} dir={toolsCollapsed ? "right" : "down"} />
          </span>
          <span>{t("pty.toolsBar")}</span>
          {toolsCollapsed && quickLaunch.length > 0 && (
            <span className="pty-tools-bar-count">{quickLaunch.length}</span>
          )}
        </button>
        <button className="btn btn-secondary pty-all-folders" onClick={() => openFolderSheet("terminal")}>
          {t("pty.allFolders")}
        </button>
        </div>

        {!toolsCollapsed && quickLaunch.length > 0 && (
          <div className="pty-quick-launch">
            <div className="pty-quick-launch-title">
              {t("pty.quickLaunch")}
              {/* Подсказка называет оба жеста плитки: тап и долгий тап. Без неё
                  лист со вторыми действиями (агент, избранное, вложенная папка)
                  никто не находит — на плитке для него нет значка. */}
              <span className="pty-quick-launch-hint"> · {t("pty.quickTileHintAgent")}</span>
            </div>
            <div
              className={`pty-quick-launch-scroll${railEdges.left ? " has-left" : ""}${railEdges.right ? " has-right" : ""}`}
              ref={railRef}
              onScroll={() => {
                tapGuardRef.current = noteScroll(tapGuardRef.current, Date.now());
                syncRailEdges();
              }}
              // Слушаем на КОНТЕЙНЕРЕ, а не на плитке: палец опускается на одну,
              // а уезжает над соседней, и до неё события уже не доходят.
              onPointerDown={(e) => {
                if (e.pointerType === "mouse") return; // мышью прокруткой не промахнёшься
                tapGuardRef.current = noteDown(tapGuardRef.current, e.clientX, e.clientY);
              }}
              onPointerMove={(e) => {
                if (e.pointerType === "mouse") return;
                tapGuardRef.current = noteMove(tapGuardRef.current, e.clientX, e.clientY);
                // Уехал за порог — снимаем и заряженный долгий тап: иначе свайп
                // через полсекунды открывал лист действий чужой плитки.
                if (tapGuardRef.current.moved) quickTilePressCancel();
              }}
            >
              {quickLaunch.map((q) => (
                <div key={q.path} className="pty-quick-launch-tile-wrap">
                  {/* Запуск и действия — отдельные цели без перекрытия.
                      Долгий тап остаётся дополнительным способом открыть меню. */}
                  <button
                    className={`pty-quick-launch-tile${creating ? " busy" : ""}`}
                    disabled={creating}
                    aria-label={q.name}
                    onPointerDown={() => quickTilePressStart(q)}
                    onPointerUp={() => quickTilePressEnd(q.path)}
                    onPointerLeave={quickTilePressCancel}
                    onPointerCancel={quickTilePressCancel}
                    onContextMenu={(e) => { e.preventDefault(); openTileMenu(q); }}
                    // Терминал открывается по pointerup (иначе долгий тап не
                    // отличить от короткого), а клавиатура pointer-событий не
                    // шлёт вовсе — без этой ветки плитка была для неё немой.
                    onKeyDown={(e) => {
                      if (e.key !== "Enter" && e.key !== " ") return;
                      e.preventDefault();
                      void handleCreate(q.path);
                    }}
                    title={`${q.name}\n${q.path}\n${t("pty.quickTileHintAgent")}`}
                  >
                    {/* Звезда — закреплённая папка, часы — недавняя: ровно тот
                        же смысл, что у строки «В избранное» в листе плитки. */}
                    <span className="pty-quick-launch-icon" aria-hidden="true">
                      {q.pinned ? <IconStar size={22} filled /> : <IconClock size={22} />}
                    </span>
                    {/* Полное имя — в aria-label и title: подпись сокращена
                        по середине (tileLabel). */}
                    <span className="pty-quick-launch-label">{tileLabel(q.name)}</span>
                  </button>
                  <button className="pty-quick-launch-menu" aria-label={t("pty.quickFolderActions", { name: q.name })}
                    disabled={creating}
                    onClick={(e) => {
                      if (e.detail !== 0 && !tapVerdict(tapGuardRef.current, Date.now()).ok) return;
                      openTileMenu(q);
                    }}>
                    {t("generic.actions")}
                  </button>
                </div>
              ))}
            </div>
          </div>
        )}

        {/* Счётчики-фильтры нужны раньше поиска: с 3–8 терминалами (обычный
            день) искать по имени нечего, а вот «у кого из них вопрос» — тот же
            вопрос, что и с двадцатью. Поиск по-прежнему появляется с девятого:
            строка ввода в шапке при пяти карточках только отнимает место. */}
        {!toolsCollapsed && sessions.length > 2 && (
          <section className="pty-list-tools" aria-label={t("pty.listTools")}>
            {sessions.length > 8 && (
              <label className="pty-list-search">
                <span aria-hidden="true"><IconSearch size={16} /></span>
                <input
                  value={query}
                  onChange={(e) => setQuery(e.target.value)}
                  placeholder={t("pty.listSearchPlaceholder")}
                  aria-label={t("pty.listSearchPlaceholder")}
                />
                {query && (
                  <button onClick={() => setQuery("")} aria-label={t("pty.clearSearch")}><IconClose size={14} /></button>
                )}
              </label>
            )}
            {/* Ряд переносится по строкам, а не уезжает за край: чипов всего
                четыре, а признака прокрутки у ряда не было — на 320–390 px
                последний обрезался словом «Завершённь» и выглядел опечаткой.
                Модификатор, а не новый класс: остальные правила ряда те же. */}
            <div
              className="pty-filter-row pty-filter-row-wrap"
              role="group"
              aria-label={t("pty.statusFilters")}
            >
              {/* Значок чипа — тот же, что на карточке в этом состоянии:
                  фильтр и статус обязаны выглядеть одинаково, иначе «⏳» в
                  списке и «⏳» в фильтре читаются как разные вещи. Точки
                  «работает» и «завершён» остаются точками — это не метафора,
                  а цветная отметка, рисовать её линией нечем. */}
              {([
                ["waiting", <IconHourglass size={12} />, "pty.filterWaiting"],
                ["working", "●", "pty.filterWorking"],
                ["error", <IconWarning size={12} />, "pty.filterError"],
                ["dead", "○", "pty.filterDead"],
              ] as const).map(([value, icon, label]) => (
                <button
                  key={value}
                  className={`pty-filter-chip ${value}${statusFilter === value ? " active" : ""}`}
                  aria-pressed={statusFilter === value}
                  onClick={() => {
                    haptic();
                    setStatusFilter((prev) => prev === value ? "all" : value);
                  }}
                >
                  <span aria-hidden="true">{icon}</span>
                  {t(label)} {statusCounts[value]}
                </button>
              ))}
            </div>
            {/* Про ручной порядок говорим там же, где живёт перетаскивание, —
                на длинном списке; при трёх карточках это лишняя строка. */}
            {sessions.length > 8 && (
              <div className="pty-order-note" role="note">
                <span aria-hidden="true">{"↕"}</span>
                {t("pty.orderFixed")}
              </div>
            )}
          </section>
        )}

        {!toolsCollapsed && (
          <button className="pty-ssh-center-link" onClick={openSsh}>
            <span><IconServer size={16} />{" "}{t("pty.sshCenter")}</span>
            <span>{t("pty.sshCenterHint")}{" ›"}</span>
          </button>
        )}

        {selectionMode && (
          <div className="pty-selection-bar" role="toolbar" aria-label={t("pty.selectionActions")}>
            <strong>{t("pty.selected", { n: selectedIds.length })}</strong>
            <button
              className="btn btn-secondary"
              disabled={selectedIds.length === 0}
              onClick={() => setMoveRequest({ ids: selectedIds, source: "selection" })}
            >
              {t("pty.moveToFolder")}
            </button>
            <button
              className="btn btn-danger"
              disabled={selectedIds.length === 0}
              onClick={() => void closeMany(
                sessions.filter((s) => selectedIds.includes(s.id)),
                t("pty.closeSelectedConfirm", { n: selectedIds.length }),
              )}
            >
              {t("pty.closeSelected")}
            </button>
            <button className="btn btn-secondary" onClick={endSelection}>{t("modal.cancel")}</button>
          </div>
        )}

        {/* Список уже был получен, а связь пропала: карточки устарели, и об
            этом надо сказать прямо — иначе человек читает вчерашние статусы
            как сегодняшние. */}
        {!loading && sessions.length > 0 && (offline || load.phase === "error") && lastOkAt > 0 && (
          <div className="offline-inline pty-stale-note" role="status">
            <span className="offline-inline-ic" aria-hidden="true"><IconWarning size={14} /></span>
            <span>{t("pty.staleData", { time: clockAt(lastOkAt) })}</span>
            <button
              className="offline-inline-retry"
              onClick={() => { haptic(); void refresh(); }}
            >
              {t("offline.retry")}
            </button>
          </div>
        )}

        {/* Массовое «убрать завершённые» жило четвёртой кнопкой нижнего ряда и
            на телефоне уезжало за край. Здесь оно и заметнее, и всегда влезает. */}
        {!loading && deadCount > 0 && (
          <div className="pty-dead-summary" role="status">
            <span aria-hidden="true">{"○"}</span>
            <span>{t("pty.deadSummary", { n: deadCount })}</span>
            <button className="pty-dead-summary-btn" onClick={() => void handleCloseDead()}>
              {t("pty.closeDead")}
            </button>
          </div>
        )}

        {/* Офлайн проверяется ДО загрузки: пока ответа нет, спиннер обещает,
            что список вот-вот приедет, — а с выключенного компьютера он не
            приедет никогда. Канал связи знает это сразу (linkOffline), запрос
            подтверждает через 4,8 с; показываем тот же блок, что на главной. */}
        {offline && sessions.length === 0 ? (
          <OfflineState
            onRetry={() => { haptic(); void refresh(); }}
            onDevices={getMode() === "cloud" ? () => navigate("/infrastructure") : undefined}
          />
        ) : loading ? (
          <div className="loading-center">
            <div className="spinner" />
          </div>
        ) : load.phase === "error" && sessions.length === 0 ? (
          // Ошибка запроса — НЕ «нет терминалов»: раньше выключенный в LAN ПК
          // (сырой «Failed to fetch», который не классифицируется как офлайн)
          // выглядел как пустой список и предлагал создать ещё один терминал,
          // хотя на компьютере всю ночь работал агент.
          <div className="empty">
            {/* Крупный знак пустого экрана остаётся эмодзи ОСОЗНАННО: .empty-icon
                задаёт 48px через font-size и наследует серый var(--tg-hint) от
                .empty, а линейный SVG в этом размере стал бы бледным волоском.
                Переводить его можно только вместе с правкой styles.css (свой
                цвет и толщина штриха) — это чужая область правки. */}
            <div className="empty-icon">{"⚠️"}</div>
            <div className="empty-title">{t("pty.loadFailed")}</div>
            {load.message && <div className="empty-desc">{load.message}</div>}
            <div className="empty-actions">
              <button className="btn btn-primary" onClick={() => { haptic(); void refresh(); }}>
                <IconRefresh size={16} />{" "}{t("offline.retry")}
              </button>
              {getMode() === "cloud" && (
                <button className="btn btn-secondary" onClick={() => navigate("/infrastructure")}>
                  {t("offline.devices")}
                </button>
              )}
            </div>
          </div>
        ) : sessions.length === 0 ? (
          <div className="empty">
            <div className="empty-icon">{"\uD83D\uDDA5\uFE0F"}</div>
            <div className="empty-title">{t("pty.empty")}</div>
            <div className="empty-desc">{t("pty.emptyDesc")}</div>
            <div className="empty-actions">
              {/* За Claude Code сюда и приходят: на пустом экране это первое
                  действие, а не спрятанный шаг внутри терминала. */}
              <button
                className="btn btn-primary"
                disabled={creating}
                onClick={() => openFolderSheet("agent")}
              >
                {creating ? t("pty.creating") : <><IconRobot size={16} />{t("pty.launchAgent")}</>}
              </button>
              <button
                className="btn btn-secondary"
                disabled={creating}
                onClick={() => openFolderSheet("terminal")}
              >
                {creating ? t("pty.creating") : t("pty.newTerminal")}
              </button>
              {/* Кнопки «Серверы» здесь нет намеренно: вход в SSH один — строка
                  «🗄️ Серверы SSH» над списком. Два входа под разными именами
                  на одном экране читались как два разных раздела. */}
              {supportsGroups && (
                <button className="btn btn-secondary" onClick={() => void handleNewFolder()}>
                  {t("pty.newFolder")}
                </button>
              )}
            </div>
          </div>
        ) : filtersActive && visibleSessions.length === 0 ? (
          <div className="empty pty-list-no-results">
            {/* Этот знак и раньше был серым однотонным «⌕», а не цветным
                эмодзи, — линейная лупа встаёт на его место без потери цвета
                (в отличие от ⚠️ выше, см. комментарий там). */}
            <div className="empty-icon"><IconSearch size={44} /></div>
            <div className="empty-title">{t("pty.noMatches")}</div>
            <button
              className="btn btn-secondary"
              onClick={() => { setQuery(""); setStatusFilter("all"); }}
            >
              {t("pty.resetFilters")}
            </button>
          </div>
        ) : (
          <div className="cards-grid">
          {listItems.map((it) => { if (it.kind === "header") return renderFolderHeader(it.name, it.count); if (it.kind === "indicator") return <div key={it.key} className="pty-drop-indicator" />; const s = it.s; const si = statusInfo(s); const lastKind = lastAgentOf(s); const resume = !s.alive && s.kind !== "ssh" && lastKind ? resumeCmds[lastKind] : undefined; return (
            <div
              key={s.id}
              data-pty-id={s.id}
              className={`card pty-card status-${si.cls}${draggingId === s.id ? " dragging" : ""}${selectedIds.includes(s.id) ? " selected" : ""}`}
              /* Карточка остаётся div (внутри живут свои кнопки — «переименовать»,
                 «закрыть», а кнопка в кнопке невалидна), но перестаёт быть
                 недоступной: роль, фокус и Enter/Space. До этого наружу были
                 выставлены только «перетащить», «действия» и «ЗАКРЫТЬ» — то
                 есть разрушительное действие доступно, а основное нет
                 (аудит путей 29.08.2026). */
              role="button"
              tabIndex={0}
              aria-label={t("pty.openCard", { name: cardTitle(s), state: si.text })}
              onKeyDown={(e) => {
                // Только собственный фокус карточки: Enter на вложенной кнопке
                // должен нажимать её, а не открывать терминал.
                if (e.target !== e.currentTarget) return;
                if (e.key !== "Enter" && e.key !== " ") return;
                e.preventDefault();
                handleCardClick(s);
              }}
              onClick={() => handleCardClick(s)}
              onPointerDown={() => cardPressStart(s)}
              onPointerUp={cardPressCancel}
              onPointerLeave={cardPressCancel}
              onPointerCancel={cardPressCancel}
              onContextMenu={(e) => e.preventDefault()}
            >
              <div className="pty-card-top">
                <div className="pty-card-identity">
                  {selectionMode ? (
                    <button
                      className={`pty-select-check${selectedIds.includes(s.id) ? " checked" : ""}`}
                      role="checkbox"
                      aria-checked={selectedIds.includes(s.id)}
                      aria-label={t("pty.selectTerminal", { name: cardTitle(s) })}
                      onPointerDown={(e) => e.stopPropagation()}
                      onClick={(e) => { e.stopPropagation(); handleCardClick(s); }}
                    >
                      {selectedIds.includes(s.id) ? <IconCheck size={14} /> : null}
                    </button>
                  ) : supportsGroups && (
                    <button
                      type="button"
                      className="pty-drag-handle"
                      aria-label={t("pty.dragTerminal", { name: cardTitle(s) })}
                      title={t("pty.dragTerminal", { name: cardTitle(s) })}
                      onPointerDown={(e) => dragStart(e, s)}
                      onPointerMove={dragMove}
                      onPointerUp={() => dragFinish(true)}
                      onPointerCancel={() => dragFinish(false)}
                      onClick={(e) => e.stopPropagation()}
                    >
                      <IconGrip size={16} />
                    </button>
                  )}
                  <span className={`dash-dot ${s.alive ? "alive" : "dead"}`} />
                  {renameId === s.id ? (
                    /* Правка на месте: поле стоит ровно там, где было имя.
                       Сохранение — «✓» или Enter, отказ — «✕» или Escape.
                       ⚠ onBlur НЕ сохраняет и не закрывает: именно это в
                       прежней версии записывало старое имя от клавиатуры,
                       обновления списка и промаха пальцем. */
                    <span className="pty-rename-box" onClick={(e) => e.stopPropagation()}>
                      <input
                        className="pty-rename-input"
                        autoFocus
                        value={renameDraft}
                        placeholder={t("pty.renamePlaceholder")}
                        aria-label={t("pty.renameAction")}
                        onPointerDown={(e) => e.stopPropagation()}
                        onChange={(e) => setRenameDraft(e.target.value)}
                        onKeyDown={(e) => {
                          if (e.key === "Enter") { e.preventDefault(); void handleRenameSave(); }
                          else if (e.key === "Escape") { e.preventDefault(); handleRenameCancel(); }
                        }}
                      />
                      <button
                        className="pty-rename-ok"
                        aria-label={t("dialog.confirm")}
                        title={t("dialog.confirm")}
                        onPointerDown={(e) => e.stopPropagation()}
                        onClick={(e) => { e.stopPropagation(); void handleRenameSave(); }}
                      >
                        <IconCheck size={16} />
                      </button>
                      <button
                        className="pty-rename-cancel"
                        aria-label={t("modal.cancel")}
                        title={t("modal.cancel")}
                        onPointerDown={(e) => e.stopPropagation()}
                        onClick={(e) => { e.stopPropagation(); handleRenameCancel(); }}
                      >
                        <IconClose size={14} />
                      </button>
                    </span>
                  ) : (
                    <span className="pty-card-shell">
                      {cardTitle(s)}
                    </span>
                  )}
                </div>
                <div className="pty-card-meta">
                  {/* SSH виден бейджем независимо от имени: переименованная
                      сессия ничем не отличалась от локального шелла ПК. */}
                  {s.kind === "ssh" && (
                    <span className="pty-ssh-badge" title={s.ssh_host ? `${s.ssh_user || ""}@${s.ssh_host}` : "SSH"}>
                      SSH{s.ssh_host ? ` · ${s.ssh_host}` : ""}
                    </span>
                  )}
                  {/* Бейдж остаётся и на завершённой карточке: иначе пять серых
                      «powershell · project» неразличимы, и найти тот, где жил
                      нужный разговор с Claude, невозможно. */}
                  {(lastKind || (s.agent_kind && s.agent_kind !== "shell")) && (
                    <ProcessBadge
                      kind={(lastKind || s.agent_kind) as AgentKind}
                      name={s.fg_process}
                      account={s.account_label}
                    />
                  )}
                  <span className={`pty-status ${si.cls}`} title={si.title}>
                    {si.icon && <span className="pty-status-icon">{si.icon}</span>}
                    <span className="pty-status-text">{si.text}</span>
                  </span>
                </div>
              </div>
              <div className="pty-card-path">
                {shortPath(s.cwd)}
              </div>
              {/* Строку показываем не только вопросу агента, но и исходу:
                  сервер кладёт в hint саму сработавшую строку ошибки, а список
                  рисовал её только у waiting — и пять карточек «⚠ ошибка · 3м»
                  были неразличимы, пока каждую не откроешь. Завершённая
                  карточка показывает исход тем же местом, как только агент
                  начнёт его отдавать. */}
              {s.hint && (s.status === "waiting" || s.status === "error" || !s.alive) && (
                <div
                  className={`pty-card-hint${s.status === "waiting" ? "" : " outcome"}`}
                  title={s.hint}
                >
                  {s.hint}
                </div>
              )}
              {s.kind === "ssh" && s.alive && (
                <div className="pty-card-note">{t("pty.sshEphemeral")}</div>
              )}
              {!s.alive && (
                <div className="pty-dead-actions">
                  {/* Первое действие мёртвой карточки с известным агентом —
                      вернуться В БЕСЕДУ, а не в пустой шелл: до этого кнопка
                      «Продолжить» жила только внутри экрана терминала, куда
                      человек как раз и не заходил. */}
                  {resume && (
                    <button
                      className="btn btn-primary"
                      disabled={creating}
                      onPointerDown={(e) => e.stopPropagation()}
                      onClick={(e) => {
                        e.stopPropagation();
                        void handleRestart(s, { kind: lastKind, agent: resume });
                      }}
                    >
                      <IconRefresh size={16} />{" "}{t("pty.resumeAgent", { name: resume.name || agentDisplayName(lastKind) })}
                    </button>
                  )}
                  {/* Живой процесс — возвращаем связь с НИМ, а не заводим
                      второй терминал в той же папке: работа идёт именно там. */}
                  {s.host_alive && !noReattach.includes(s.id) && (
                    <button
                      className="btn btn-primary"
                      disabled={reattachingId === s.id}
                      onPointerDown={(e) => e.stopPropagation()}
                      onClick={(e) => { e.stopPropagation(); void handleReattach(s); }}
                    >
                      {reattachingId === s.id
                        ? t("pty.reattaching")
                        : <><IconUnlink size={16} />{" "}{t("pty.reattach")}</>}
                    </button>
                  )}
                  {/* «Перезапустить в этой папке» стоит рядом ВСЕГДА, даже когда
                      процесс жив и связь можно вернуть. Просьба владельца
                      (2026-07-30): у карточки должно быть три понятные кнопки, а
                      не текст с намёком. И это единственный выход, когда связь
                      вернуть нельзя — например, терминал запущен прежней
                      версией Remotai (её процесс не умеет отдавать длинную
                      историю по частям). Имя — то же, что у пункта меню «⋮»:
                      одно действие (handleRestart) — одно имя (аудит ИА
                      02.09.2026, P1-18). */}
                  <button
                    className="btn btn-secondary"
                    disabled={creating}
                    onPointerDown={(e) => e.stopPropagation()}
                    onClick={(e) => { e.stopPropagation(); void handleRestart(s); }}
                  >
                    {s.kind === "ssh"
                      ? <><IconRefresh size={16} />{" "}{t("pty.sshReconnect")}</>
                      : <><IconRefresh size={16} />{" "}{t("pty.restartHere")}</>}
                  </button>
                  <button
                    className="btn btn-secondary"
                    onPointerDown={(e) => e.stopPropagation()}
                    onClick={(e) => { e.stopPropagation(); void handleExport(s); }}
                  >
                    <IconDownload size={16} />{" "}{t("pty.saveLog")}
                  </button>
                </div>
              )}
              <button
                className="pty-card-rename"
                aria-label={t("pty.actions")}
                title={t("pty.actions")}
                onPointerDown={(e) => e.stopPropagation()}
                onClick={(e) => { e.stopPropagation(); haptic(); setMenuSession(s); }}
              >
                <IconDots size={16} />
              </button>
              <button
                className="pty-card-close"
                aria-label={t("confirm.btn.closeTerminal")}
                onPointerDown={(e) => e.stopPropagation()}
                onClick={(e) => { e.stopPropagation(); void handleClose(s); }}
              >
                <IconClose size={14} />
              </button>
            </div>
            );})}
          </div>
        )}

        {/* Ряд переносится по строкам: инлайновый flex без wrap на 320–390px
            обрезал последнюю кнопку по краю экрана (body { overflow-x: hidden }),
            и убрать завершённые терминалы с телефона было нечем. «Закрыть
            завершённые» переехала в строку-итог над списком. */}
        {sessions.length > 0 && (
          <div className="pty-list-actions">
            {/* Главное действие раздела — запустить агента: раньше явного входа
                в него на /pty не было вовсе, и его искали внутри терминала. */}
            <button
              className="btn btn-primary pty-list-actions-main"
              disabled={creating}
              onClick={() => openFolderSheet("agent")}
            >
              {creating ? t("pty.creating") : <><IconRobot size={16} />{t("pty.launchAgent")}</>}
            </button>
            <button
              className="btn btn-secondary"
              disabled={creating}
              onClick={() => openFolderSheet("terminal")}
            >
              {creating ? t("pty.creating") : t("pty.newTerminal")}
            </button>
            {/* «Серверы» отсюда убраны: тот же /ssh уже открывает строка
                «🗄️ Серверы SSH» над списком, а место в ряду нужно главному
                действию — запуску агента. */}
            {supportsGroups && (
              <button className="btn btn-secondary" onClick={() => void handleNewFolder()}>
                {t("pty.newFolder")}
              </button>
            )}
          </div>
        )}
      </div>

      {/* Шторка объявлена диалогом (role + aria-modal + ловушка фокуса), как
          DialogHost и «Инфраструктура»: до этого Tab уходил в список под
          затемнением и «нажимал» невидимые кнопки, а скринридер продолжал
          читать страницу под оверлеем. */}
      {menuSession && (
        <div className="modal-overlay" onClick={() => setMenuSession(null)}>
          <div
            ref={menuSheetRef}
            className="modal-sheet pty-actions-sheet"
            role="dialog"
            aria-modal="true"
            aria-labelledby="pty-actions-title"
            tabIndex={-1}
            onClick={(e) => e.stopPropagation()}
            onKeyDown={trapTabInSheet}
          >
            <div className="help-sheet-header">
              <div>
                <strong id="pty-actions-title">{cardTitle(menuSession)}</strong>
                <div className="pty-action-subtitle">{shortPath(menuSession.cwd)}</div>
              </div>
              {/* aria-label обязателен: имя кнопке давал сам символ «✕», а
                  линейный знак объявлен aria-hidden — без подписи скринридер
                  прочитал бы «кнопка». */}
              <button className="folder-sheet-close" aria-label={t("modal.close")} onClick={() => setMenuSession(null)}>
                <IconClose size={16} />
              </button>
            </div>
            <div className="pty-action-list">
              {/* Первым — ОСНОВНОЕ действие. В меню его не было вовсе: «закрыть»
                  было, «переименовать» было, а «открыть» — нет, хотя ради этого
                  терминал и заводят (аудит путей 29.08.2026). */}
              <button onClick={() => {
                const target = menuSession;
                setMenuSession(null);
                openTerminal(target.id);
              }}>
                <span aria-hidden="true"><IconArrow size={18} dir="right" /></span>{t("pty.openAction")}
              </button>
              <button onClick={() => {
                const target = menuSession;
                setMenuSession(null);
                void startRename(target);
              }}>
                <span><IconPencil size={18} /></span>{t("pty.renameAction")}
              </button>
              {supportsGroups && (
                <>
                  <button
                    disabled={menuGroupIndex <= 0}
                    aria-label={t("pty.moveUp")}
                    onClick={() => void moveSessionBy(menuSession, -1)}
                  >
                    <span aria-hidden="true"><IconArrow size={18} dir="up" /></span>{t("pty.moveUp")}
                  </button>
                  <button
                    disabled={menuGroupIndex < 0 || menuGroupIndex >= menuGroupItems.length - 1}
                    aria-label={t("pty.moveDown")}
                    onClick={() => void moveSessionBy(menuSession, 1)}
                  >
                    <span aria-hidden="true"><IconArrow size={18} dir="down" /></span>{t("pty.moveDown")}
                  </button>
                  <button onClick={() => {
                    setMoveRequest({ ids: [menuSession.id], source: "menu" });
                    setMenuSession(null);
                  }}>
                    {/* Стопка карточек — то же, чем группа обозначена в списке
                        назначения ниже: «перенести» и «куда переносим»
                        показываем одним знаком. */}
                    <span><IconGroup size={18} /></span>{t("pty.moveToFolder")}
                  </button>
                </>
              )}
              <button onClick={() => beginSelection(menuSession)}>
                <span><IconCheck size={18} /></span>{t("pty.selectAction")}
              </button>
              {!menuSession.alive && (
                <>
                  {(() => {
                    const kind = lastAgentOf(menuSession);
                    const resume = menuSession.kind !== "ssh" && kind ? resumeCmds[kind] : undefined;
                    return resume ? (
                      <button onClick={() => void handleRestart(menuSession, { kind, agent: resume })}>
                        <span><IconRefresh size={18} /></span>{t("pty.resumeAgent", { name: resume.name || agentDisplayName(kind) })}
                      </button>
                    ) : null;
                  })()}
                  <button onClick={() => void handleRestart(menuSession)}>
                    <span><IconRefresh size={18} /></span>
                    {menuSession.kind === "ssh" ? t("pty.sshReconnect") : t("pty.restartHere")}
                  </button>
                  <button onClick={() => void handleExport(menuSession)}>
                    <span><IconDownload size={18} /></span>{t("pty.saveLog")}
                  </button>
                  {/* Отдельным пунктом, потому что в Telegram это единственный
                      путь, который точно доносит файл (см. handleExport). */}
                  {botAvailable && (
                    <button onClick={() => void handleExportToTelegram(menuSession)}>
                      <span><IconSend size={18} /></span>{t("pty.exportSendTg")}
                    </button>
                  )}
                </>
              )}
              <button className="danger" onClick={() => void handleClose(menuSession)}>
                <span><IconClose size={18} /></span>{t("confirm.btn.closeTerminal")}
              </button>
            </div>
          </div>
        </div>
      )}

      {moveRequest && (
        <div className="modal-overlay" onClick={() => setMoveRequest(null)}>
          <div
            ref={moveSheetRef}
            className="modal-sheet pty-actions-sheet"
            role="dialog"
            aria-modal="true"
            aria-labelledby="pty-move-title"
            tabIndex={-1}
            onClick={(e) => e.stopPropagation()}
            onKeyDown={trapTabInSheet}
          >
            <div className="help-sheet-header">
              <strong id="pty-move-title">{t("pty.moveToFolder")}</strong>
              <button className="folder-sheet-close" aria-label={t("modal.close")} onClick={() => setMoveRequest(null)}>
                <IconClose size={16} />
              </button>
            </div>
            <div className="pty-action-list">
              <button onClick={() => void moveToFolder("")}>
                {/* «Без группы» — карточки, которые не лежат стопкой: тот же
                    знак, что у «Расформировать группу», и смысл тот же —
                    терминал выходит из стопки, но остаётся открытым. */}
                <span><IconUngroup size={18} /></span>{t("pty.ungrouped")}
              </button>
              {allFolderNames.map((name) => (
                <button key={name} onClick={() => void moveToFolder(name)}>
                  {/* Стопка карточек, а не папка: «папкой» на этом экране
                      зовётся каталог компьютера, и один значок на два смысла
                      заставлял гадать, куда именно переезжает терминал. */}
                  <span><IconGroup size={18} /></span>{name}
                </button>
              ))}
              <button onClick={() => void moveToNewFolder()}>
                <span><IconPlus size={18} /></span>{t("pty.newFolder")}
              </button>
            </div>
          </div>
        </div>
      )}

      {/* Лист плитки «Быстрого запуска»: всё, что раньше висело значками по
          24 px в её углах. Разметка та же, что у листа действий терминала, —
          строки по 48 px, заголовок называет папку, поэтому в самих строках
          повторять её незачем. */}
      {tileMenu && (
        <div className="modal-overlay" onClick={closeTileSheet}>
          <div
            ref={tileSheetRef}
            className="modal-sheet pty-actions-sheet"
            role="dialog"
            aria-modal="true"
            aria-labelledby="pty-tile-actions-title"
            tabIndex={-1}
            onClick={(e) => e.stopPropagation()}
            onKeyDown={trapTabInSheet}
          >
            <div className="help-sheet-header">
              <div>
                <strong id="pty-tile-actions-title">{tileMenu.name}</strong>
                <div className="pty-action-subtitle">{shortPath(tileMenu.path)}</div>
              </div>
              <button className="folder-sheet-close" aria-label={t("modal.close")} onClick={closeTileSheet}>
                <IconClose size={16} />
              </button>
            </div>
            <div className="pty-action-list">
              <button
                disabled={creating}
                onClick={() => { const q = tileMenu; setTileMenu(null); quickTileAgent(q.path); }}
              >
                <span><IconRobot size={18} /></span>{t("pty.launchAgentIn", { name: tileMenu.name })}
              </button>
              {/* Обзор начиная с этой папки: единственный быстрый путь открыть
                  терминал в подпроекте. Раньше это был долгий тап по плитке — а
                  он теперь открывает сам лист. */}
              <button onClick={() => { const q = tileMenu; setTileMenu(null); openBrowser(q.path); }}>
                <span><IconFolder size={18} /></span>{t("folder.title")}
              </button>
              <button onClick={() => { const q = tileMenu; setTileMenu(null); void togglePin(q); }}>
                <span><IconStar size={18} filled={tileMenu.pinned} /></span>
                {tileMenu.pinned ? t("pty.unpinFolder") : t("pty.pinFolder")}
              </button>
            </div>
          </div>
        </div>
      )}

      {/* Меню шапки группы. Всё, что раньше стояло значками в ряд, здесь —
          строкой с текстом: «Расформировать группу» ни с чем не спутать, а
          корзина спутывалась с закрытием. Порядок — от безобидного к
          необратимому, разрушительное внизу и красным (класс danger), как в
          меню карточки терминала.

          «Закрыть все терминалы» стоит и здесь, и кнопкой в шапке: в шапке это
          короткая ежедневная кнопка, а тут — полная фраза с числом, чтобы до
          подтверждения было видно, сколько работы закрывается. */}
      {folderMenu !== null && (() => {
        const groupName = folderMenu;
        const groupItems = allOrderedGroups.find((group) => group.name === groupName)?.items ?? [];
        return (
        <div className="modal-overlay" onClick={closeFolderMenu}>
          <div
            ref={folderSheetRef}
            className="modal-sheet pty-actions-sheet"
            role="dialog"
            aria-modal="true"
            aria-labelledby="pty-folder-actions-title"
            tabIndex={-1}
            onClick={(e) => e.stopPropagation()}
            onKeyDown={trapTabInSheet}
          >
            <div className="help-sheet-header">
              <div>
                <strong id="pty-folder-actions-title">{groupName}</strong>
                <div className="pty-action-subtitle">{t("pty.folderTerminals", { n: groupItems.length })}</div>
              </div>
              <button className="folder-sheet-close" aria-label={t("modal.close")} onClick={closeFolderMenu}>
                <IconClose size={16} />
              </button>
            </div>
            <div className="pty-action-list">
              <button onClick={() => { setFolderMenu(null); void handleRenameFolder(groupName); }}>
                <span><IconPencil size={18} /></span>{t("pty.renameFolder")}
              </button>
              {/* Не danger: группа исчезает, а терминалы остаются открытыми и
                  возвращаются в «Без группы» — ровно это и рисует значок. */}
              <button onClick={() => { setFolderMenu(null); void handleDeleteFolder(groupName); }}>
                <span><IconUngroup size={18} /></span>{t("pty.deleteFolder")}
              </button>
              {groupItems.length > 0 && (
                <button className="danger" onClick={() => { setFolderMenu(null); void closeFolder(groupName); }}>
                  <span><IconClose size={18} /></span>{t("pty.closeFolderAll", { n: groupItems.length })}
                </button>
              )}
            </div>
          </div>
        </div>
        );
      })()}

      {/* Подписи зависят от намерения: «Открыть здесь» для обычного терминала и
          «Открыть и запустить агента» — когда пришли за Claude Code. Иначе
          человек выбирал папку, не понимая, что будет дальше. */}
      <FolderNavSheet
        open={folderOpen}
        onClose={closeFolderSheet}
        onPick={handleCreate}
        title={folderIntent === "agent" ? t("pty.launchAgentTitle") : t("pty.newTerminalTitle")}
        pickLabel={folderIntent === "agent" ? t("pty.launchAgentPick") : t("pty.openHere")}
        currentCwd={folderStart || undefined}
        initialTab={folderStart ? "browse" : "fav"}
      />

      <HelpSheet
        open={showHelp}
        onClose={() => setShowHelp(false)}
        // Аудит ИА 02.09.2026, P1-5: из подсказок экрана — в тему гида.
        guide="terminal"
        title={t("pty.helpTitle")}
        items={[
          { icon: "\uD83E\uDD16", title: t("pty.helpAgentTitle"), text: t("pty.helpAgentText") },
          { icon: "\uD83D\uDDA5\uFE0F", title: t("pty.helpWhatTitle"), text: t("pty.helpWhatText") },
          { icon: "\uD83D\uDC46", title: t("pty.helpOpenTitle"), text: t("pty.helpOpenText") },
          { icon: "\u22EE", title: t("pty.helpRenameTitle"), text: t("pty.helpRenameText") },
          { icon: "\u2B50", title: t("pty.helpPinTitle"), text: t("pty.helpPinText") },
          { icon: "\uD83D\uDCC1", title: t("pty.helpFolderTitle"), text: t("pty.helpFolderText") },
          { icon: "⠿", title: t("pty.helpGroupsTitle"), text: t("pty.helpGroupsText") },
        ]}
      />

      <BottomNav active="terminal" />
    </div>
  );
}


// SSH-UI (серверы, история, форвардинг, ручной коннект) переехал в
// ../components/SshSection.tsx + SshForwardsSheet.tsx; SFTP — в SshFilesView.tsx.
