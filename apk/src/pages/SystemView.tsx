import { useEffect, useState, useCallback, useRef } from "react";
import type { CSSProperties, PointerEvent as ReactPointerEvent, KeyboardEvent as ReactKeyboardEvent } from "react";
import { useNavigate, useSearchParams } from "react-router-dom";
import {
  getSystemStats, getProcesses, killProcess, takeScreenshot, powerAction, getCostStats,
  getAutostartStatus, setAutostart, getServiceStatus, listPtySessions, onConnectionChange,
} from "../api";
import { haptic, hapticSuccess, hapticError, tgConfirm } from "../telegram";
import { saveBlob, isShareCancel } from "../saveFile";
import { useToast, mapApiError, isPcOffline, humanSize, useEscape } from "@tgcontrol/shared";
import { t } from "../i18n";
import type { SystemStats, ProcessInfo, CostStats } from "../types";
import type { AutostartStatus, ServiceStatus } from "../api";
import { BottomNav } from "../components/BottomNav";
import { DeviceChip } from "../components/DeviceChip";
import { sectionDesc, sectionName } from "../sections";
import { useCapabilities } from "../hooks/useCapabilities";
import { usePolling } from "../hooks/usePolling";
import { useLoadState, OfflineState } from "@tgcontrol/shared";
import { getMode } from "../config";
import { HoldButton } from "../components/HoldButton";
import { HealthCard } from "../components/HealthCard";
import { IconScreen, IconCamera, IconRepeat, IconLock, IconMoon, IconRestart, IconPower } from "../components/icons";

/**
 * Проценты по-русски: «27,3 %».
 *
 * Карточки печатали `toFixed(1)` как есть — «27.3%» с точкой стояло вплотную к
 * «21,5 ГБ» от humanSize, где разделитель уже запятая: одно число на экране
 * выглядело числом из другой системы. Пробел перед знаком неразрывный, иначе
 * «96» и «%» разъезжаются по строкам на узкой карточке (то же правило, что
 * было записано вручную у диска, — теперь оно одно на все карточки экрана).
 */
function formatPercent(value: number, digits = 1): string {
  return `${value.toFixed(digits).replace(".", ",")} %`;
}

function formatUptime(seconds: number): string {
  const d = Math.floor(seconds / 86400);
  const h = Math.floor((seconds % 86400) / 3600);
  const m = Math.floor((seconds % 3600) / 60);
  if (d > 0) return `${d} д ${h} ч`;
  if (h > 0) return `${h} ч ${m} м`;
  return `${m} м`;
}

/**
 * Том диска из ответа агента (`disks[]`, а также одиночный `disk` старых
 * агентов, где mount/device/fstype необязательны).
 */
type Volume = {
  total: number; used: number; free: number; percent: number;
  mount?: string; device?: string; fstype?: string;
};

/**
 * Файловые системы, которые НЕ отвечают на вопрос «сколько осталось места».
 *
 * Агент отдаёт все тома, отсортированные по заполненности, и самым заполненным
 * на штатной Ubuntu всегда оказывается snap-образ (squashfs, 100% по
 * определению — это read-only архив), а на Windows — смонтированный ISO или
 * образ диска. Показывать их как «Диск · 100%» значит пугать человека там, где
 * на корне триста гигабайт свободно.
 */
const SERVICE_FSTYPES = new Set([
  "squashfs", "snapfuse", "fuse.snapfuse", "iso9660", "cd9660", "udf", "cdfs",
  "overlay", "overlayfs", "tmpfs", "devtmpfs", "ramfs", "efivarfs", "erofs",
]);

/** Меньше двух гигабайт — служебный раздел (EFI, recovery, образ), не «диск». */
const MIN_VOLUME_BYTES = 2 * 1024 * 1024 * 1024;

function isServiceVolume(volume: Volume): boolean {
  if (SERVICE_FSTYPES.has((volume.fstype || "").toLowerCase())) return true;
  const device = (volume.device || "").toLowerCase();
  if (device.startsWith("/dev/loop")) return true;
  const mount = (volume.mount || "").toLowerCase();
  if (mount.startsWith("/snap/") || mount.startsWith("/var/snap/") || mount.startsWith("/var/lib/snapd/")) return true;
  return !Number.isFinite(volume.total) || volume.total < MIN_VOLUME_BYTES;
}

/**
 * Системный том: тот, где живут профиль пользователя и рабочие папки агента.
 * Остальные буквы дисков системными не считаем — иначе «главным» снова станет
 * самый заполненный (внешний диск, флешка), а это ровно та ложь, от которой
 * фильтр и заведён. Нестандартной установки (система не на C:) фильтр не знает —
 * там карточка честно назовёт том, который показывает.
 */
function isSystemVolume(volume: Volume): boolean {
  const mount = (volume.mount || "").trim();
  return mount === "/" || /^c:[\\/]?$/i.test(mount);
}

/**
 * Человеческая подпись тома: «C:» вместо «C:\» и «C:\Users\user» (старый агент
 * присылает одиночный том папкой профиля), «/var» вместо сырого mount.
 */
function volumeLabel(volume: Volume): string {
  const mount = (volume.mount || "").trim();
  const winDrive = /^([a-zA-Z]):(?:[\\/]|$)/.exec(mount);
  if (winDrive) return `${winDrive[1].toUpperCase()}:`;
  if (mount) return mount.replace(/(.)[\\/]+$/, "$1");
  return volume.device || "";
}

function GaugeBar({ percent, color }: { percent: number; color: string }) {
  return (
    <div className="sys-gauge">
      <div className="sys-gauge-fill" style={{ width: `${Math.min(percent, 100)}%`, background: color }} />
    </div>
  );
}

/** data:URL скриншота → Blob для saveBlob (шаринг/сохранение на телефон). */
function dataUrlToBlob(dataUrl: string): Blob {
  const [head, b64] = dataUrl.split(",");
  const mime = head.match(/data:(.*?);base64/)?.[1] || "image/png";
  const bin = atob(b64);
  const bytes = new Uint8Array(bin.length);
  for (let i = 0; i < bin.length; i++) bytes[i] = bin.charCodeAt(i);
  return new Blob([bytes], { type: mime });
}

/**
 * Ловушка фокуса для окна предпросмотра скриншота.
 *
 * Без неё Tab из открытого окна уходит на страницу под затемнением и «нажимает»
 * невидимые кнопки (в окне exe это ловится сразу мышью и клавиатурой), а
 * скринридер продолжает читать «Систему» под оверлеем. Правило то же, что в
 * DialogHost, — вынесено сюда, потому что окно рисуется своей разметкой.
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

/**
 * Строка списка процессов + флаги защиты от агента.
 *
 * `self` — процесс самого Remotai (агент, окно панели на WebView2, pty-хост),
 * `critical` — системный процесс ОС. Оба опциональны: старый агент их не
 * присылает, и экран ведёт себя как раньше.
 *
 * В строке показываем ОДИН бейдж, и `self` важнее: «Remotai» объясняет
 * последствия точнее, чем «Системный».
 *
 * `cpu_warmup` — у ЭТОЙ строки пригодной дельты ещё нет (процесс появился после
 * базового замера): в колонке CPU рисуем «—», а не «0.0%», иначе «нет данных»
 * читается как «процессор свободен». Старый агент флага не присылает.
 */
type GuardedProcess = ProcessInfo & { self?: boolean; critical?: boolean; cpu_warmup?: boolean };

/**
 * Как строка списка зовётся для человека — в заголовке, в вопросе диалога, в
 * тосте и для скринридера.
 *
 * Заголовком безымянного процесса стояло «PID 3060»: номер занимал место имени,
 * повторялся тут же в подписи и третий раз — в вопросе «Завершить "PID 3060"
 * (PID 3060)?». Номер сам по себе не отвечает на вопрос «что я закрываю»,
 * поэтому имени, которого агент не отдал, честнее сказать словами. Пустое имя
 * приходит редко (защищённые системные процессы), но именно у них цена промаха
 * самая высокая.
 */
function procTitle(p: GuardedProcess): string {
  return (p.name || "").trim() || t("sys.procNoName");
}

/**
 * Новые цифры в СТАРОМ порядке строк.
 *
 * Сортировку считает агент, и каждые пять секунд «chrome», найденный глазами в
 * четвёртой строке, уезжает на седьмую — палец при этом уже опускается на
 * четвёртую. Полная заморозка списка (см. holdingProc) начинается только с
 * нажатия на «✖», то есть ПОСЛЕ этого промаха. Поэтому пока человек ведёт
 * список пальцем и целится, порядок держим прежним: числа памяти и CPU
 * обновляются, а строки остаются на своих местах. Новые процессы дописываются в
 * конец, исчезнувшие просто уходят.
 */
function keepRowOrder(prev: GuardedProcess[], next: GuardedProcess[]): GuardedProcess[] {
  if (prev.length === 0) return next;
  const rank = new Map(prev.map((p, index) => [p.pid, index]));
  const known = next.filter((p) => rank.has(p.pid))
    .sort((a, b) => rank.get(a.pid)! - rank.get(b.pid)!);
  const fresh = next.filter((p) => !rank.has(p.pid));
  return known.length === 0 ? next : [...known, ...fresh];
}

/** Сколько держать HoldButton (совпадает с holdMs по умолчанию в компоненте). */
const HOLD_MS = 900;

/**
 * Сколько порядок строк держится после того, как палец отпустил список.
 * Хватает, чтобы прицелиться в «✖» той строки, которую человек только что
 * увидел, и не мешает следующему обновлению переставить список.
 */
const PROC_ORDER_HOLD_MS = 3000;

/**
 * Ответ агента на команду питания.
 *
 * Новый агент присылает окно отмены, которое у него РЕАЛЬНО есть: на Windows
 * его даёт ОС (`shutdown /s /t 5`, снимается `shutdown /a`), на Linux/macOS —
 * сам агент (у `shutdown` там нет секундной задержки). У старого агента полей
 * нет: на Windows окно есть исторически, на Linux команда уходила немедленно, и
 * плашка «Отменить» была обещанием без основания.
 */
type PowerResult = { delay_ms?: number; cancelable?: boolean } | null;

/**
 * Кнопка «Снимок экрана» в шапке: подписанная словом, она шире квадрата 44×44,
 * которым в styles.css описан `.header-action`. Стили инлайном намеренно —
 * styles.css правит другой исполнитель, а высота цели и запрет переноса подписи
 * не должны ждать его правки (тот же приём, что у точки непрочитанного в
 * BottomNav).
 */
const SCREENSHOT_BTN_STYLE: CSSProperties = { minHeight: 44, whiteSpace: "nowrap", flexShrink: 0 };

/** Короткая подпись статуса терминала для списка «что закроется вместе с ПК». */
function sessionStatusLabel(status?: string): string {
  switch (status) {
    case "working": return t("pty.statusWorking");
    case "stalled": return t("sys.busyStalled");
    case "waiting": return t("pty.statusWaiting");
    case "error": return t("sys.busyError");
    default: return t("pty.statusReady");
  }
}

/**
 * Вкладка «Системы» живёт в адресе: `/system?tab=power` (аудит ИА 02.09.2026,
 * P1-28/V34). Так работают диплинки — пункт «Питание и нагрузка» в меню машины,
 * ссылка с главной, кнопка бота `system`. Неизвестное или пустое значение даёт
 * null, а не «monitor»: эффект синхронизации в SystemView не должен сбрасывать
 * вкладку, когда параметра в адресе просто нет.
 */
const SYSTEM_TABS = ["monitor", "processes", "power"] as const;
type SystemTab = (typeof SYSTEM_TABS)[number];
function systemTabFromQuery(raw: string | null): SystemTab | null {
  return (SYSTEM_TABS as readonly string[]).includes(raw || "") ? (raw as SystemTab) : null;
}

export function SystemView() {
  const navigate = useNavigate();
  const { toast, toastSuccess, toastError } = useToast();
  const [stats, setStats] = useState<SystemStats | null>(null);
  const [procs, setProcs] = useState<GuardedProcess[]>([]);
  const [procsLoading, setProcsLoading] = useState(false);
  // Отказ загрузки процессов больше не глотается пустым catch: раньше экран
  // возвращался к «Процессы не загружены» с кнопкой «Обновить» — ни причины, ни
  // офлайн-состояния, хотя рядом на «Мониторе» оно уже есть.
  const [procsError, setProcsError] = useState<string | null>(null);
  // Когда список процессов получен: у выключенного ПК снимок нельзя выдавать за
  // живой (та же причина, что и у statsAt).
  const [procsAt, setProcsAt] = useState<number | null>(null);
  // Загрузку CPU агент считает дельтой между двумя замерами. cpu_warmup = ни
  // одной пригодной дельты (крайний случай), cpu_window_ms — промежуток, за
  // который посчитан процент: без подписи цифра означала «непонятно что».
  const [procsWarmup, setProcsWarmup] = useState(false);
  const [cpuWindowMs, setCpuWindowMs] = useState<number | null>(null);
  const [costs, setCosts] = useState<CostStats | null>(null);
  const [screenshot, setScreenshot] = useState<string | null>(null);
  const [screenshotDisplays, setScreenshotDisplays] = useState<Array<{ id: number; w: number; h: number }>>([]);
  const [screenshotDisplay, setScreenshotDisplay] = useState(0);
  const [searchParams, setSearchParams] = useSearchParams();
  const [tab, setTab] = useState<SystemTab>(() => systemTabFromQuery(searchParams.get("tab")) ?? "monitor");
  // Адрес сменился, пока экран открыт (диплинк на уже открытую «Систему») —
  // вкладка идёт за адресом. Обратно: смена вкладки пальцем пишет ?tab= с
  // replace, чтобы системная «Назад» не листала вкладки вместо выхода с экрана.
  useEffect(() => {
    const fromQuery = systemTabFromQuery(searchParams.get("tab"));
    if (fromQuery) setTab(fromQuery);
  }, [searchParams]);
  const selectTab = (next: SystemTab) => {
    setTab(next);
    const params = new URLSearchParams(searchParams);
    params.set("tab", next);
    setSearchParams(params, { replace: true });
  };
  const [procSort, setProcSort] = useState<"memory" | "cpu">("memory");
  // Фильтр по имени или PID: сортировка «по памяти» не помогает, когда ищешь
  // конкретный chrome среди полусотни svchost.
  const [procQuery, setProcQuery] = useState("");
  // Процесс, на «✖» которого сейчас лежит палец. Пока он держится, список НЕ
  // перерисовывается: цифры памяти и CPU скачут, строки меняются местами каждые
  // 5 секунд, и удержание убивало того, кто успел занять это место. Ref — чтобы
  // ответ уже улетевшего запроса тоже не подменил список под пальцем.
  const [holdingProc, setHoldingProc] = useState<GuardedProcess | null>(null);
  const holdingProcRef = useRef<GuardedProcess | null>(null);
  // До какого момента порядок строк заморожен: палец на списке (0 = «прямо
  // сейчас держит») или отпустил его меньше PROC_ORDER_HOLD_MS назад. Смена
  // сортировки и ручное «↻» снимают заморозку сразу — там переставить список
  // как раз и просят.
  const procOrderHoldUntil = useRef(0);
  const procTouching = useRef(false);
  // Скорость сети: считается по разнице соседних ответов (см. refreshStats).
  const [netRate, setNetRate] = useState<{ up: number; down: number } | null>(null);
  const netPrev = useRef<{ sent: number; recv: number; at: number } | null>(null);
  // Когда цифры были получены. Если ПК выключился уже после загрузки экрана,
  // карточки продолжали показывать последний снимок как живой (#60).
  const [statsAt, setStatsAt] = useState<number | null>(null);
  const statsFailures = useRef(0);
  const [screenshotLoading, setScreenshotLoading] = useState(false);
  // Окно отмены выключения: команда срабатывает с задержкой, и это единственный
  // шанс отыграть промах назад. Длину окна называет АГЕНТ (delay_ms) — плашку
  // показываем только когда отмена там действительно есть.
  const [pendingPower, setPendingPower] = useState<{ action: string; until: number } | null>(null);
  const [pendingLeft, setPendingLeft] = useState(0);
  // Возможности МАШИНЫ: на headless-сервере нет дисплея — часть действий
  // предлагать нельзя (см. capabilities.ts).
  const { hasDisplay, platform } = useCapabilities();
  // Состояние загрузки: «ПК не в сети» вместо бесконечного skeleton.
  const load = useLoadState();
  const { succeed, fail } = load;
  /**
   * Выключенный ПК из ЖИВОГО канала, а не из упавшего запроса.
   *
   * Раньше «Система» узнавала об офлайне только когда /api/system/stats
   * отваливался по таймауту: замер живого стенда — 9,7 секунды молчания, дольше
   * всех экранов продукта («Терминалы» 4,8 с, «Файлы» 6,0 с). И всё это время
   * экран рисовал скелетоны, то есть обещал цифры, которых не будет. Кадр
   * agent_status от релея знает правду сразу — той же подпиской живут главная,
   * карточка готовности и точка в чипе устройства.
   *
   * Реагируем именно на `reason === "pc_offline"`, а не на `!connected`: в вебе,
   * Telegram и окне exe сокет намеренно рвётся в фоне, и это не значит, что
   * компьютер умер.
   */
  const [linkOffline, setLinkOffline] = useState(false);
  useEffect(() => onConnectionChange((state) => {
    if (state.reason === "pc_offline") { setLinkOffline(true); return; }
    if (state.connected) setLinkOffline(false);
  }), []);
  // Один признак «компьютера нет» на весь экран: канал сказал раньше — верим
  // ему, запрос упал раньше — верим ему. Поллинг тоже смотрит сюда, поэтому
  // выключенную машину перестаём долбить с первой же секунды, а не через минуту.
  const offline = load.offline || linkOffline;
  // Пришёл ответ от машины — она жива, что бы ни говорил канал до этого. Без
  // этого «Обновить» на офлайн-карточке возвращал цифры, но экран продолжал
  // писать «компьютер не отвечает»: признак из канала сам собой не гаснет,
  // пока релей не пришлёт следующий кадр agent_status.
  const markAlive = useCallback(() => {
    setLinkOffline(false);
    succeed();
  }, [succeed]);
  const [autostartStatus, setAutostartStatus] = useState<AutostartStatus | null>(null);
  const [serviceStatus, setServiceStatus] = useState<ServiceStatus | null>(null);
  // Headless — Linux-сервер без дисплея. Там часть действий («Сон»,
  // «Блокировка») не имеет смысла, а на Windows отсутствие дисплея бывает
  // временным (RDP-сессия свёрнута), поэтому платформу учитываем явно.
  const headless = serviceStatus?.os === "linux" && !hasDisplay;
  const [remoteRepairing, setRemoteRepairing] = useState(false);
  const autostartEnabled = autostartStatus?.enabled ?? null;

  const refreshStats = useCallback(async () => {
    try {
      const next = await getSystemStats();
      // Скорость сети считаем сами по разнице двух ответов: сервер отдаёт
      // счётчики с момента загрузки ОС, и «⬆ 4,2 ГБ» ничего не говорит о том,
      // занят ли канал сейчас.
      const prev = netPrev.current;
      const now = Date.now();
      if (prev && next.network && now > prev.at) {
        const dt = (now - prev.at) / 1000;
        const up = (next.network.bytes_sent - prev.sent) / dt;
        const down = (next.network.bytes_recv - prev.recv) / dt;
        // Счётчик мог уехать назад (перезапуск ОС/интерфейса) — тогда молчим
        // до следующего замера, а не рисуем отрицательную скорость.
        setNetRate(up >= 0 && down >= 0 ? { up, down } : null);
      }
      if (next.network) {
        netPrev.current = { sent: next.network.bytes_sent, recv: next.network.bytes_recv, at: now };
      }
      setStats(next);
      setStatsAt(now);
      statsFailures.current = 0;
      markAlive();
    } catch (e) {
      // Пустой catch держал skeleton навсегда, а поллинг долбил выключенный ПК
      // каждые 3 секунды.
      statsFailures.current++;
      // One lost packet must not turn healthy metrics into an offline alarm.
      // Two consecutive failures are enough to mark the existing snapshot stale.
      if (statsFailures.current >= 2) fail(e);
    }
  }, [markAlive, fail]);

  const refreshProcs = useCallback(async () => {
    setProcsLoading(true);
    try {
      const d = await getProcesses(procSort);
      // Палец уже на «✖»: подменять строки под ним нельзя — человек целился в
      // конкретный процесс, а не в место в списке. Ответ просто выбрасываем,
      // следующий поллинг придёт сразу после отпускания.
      if (holdingProcRef.current) {
        markAlive();
        return;
      }
      // Палец на списке (или только что был) — цифры обновляем, строки местами
      // не меняем: пересортировка под пальцем ведёт к завершению НЕ ТОГО
      // процесса, а отмены у kill нет.
      const frozen = procTouching.current || Date.now() < procOrderHoldUntil.current;
      setProcs((prev) => (frozen ? keepRowOrder(prev, d.processes) : d.processes));
      setProcsWarmup(d.cpu_warmup === true);
      setCpuWindowMs(typeof d.cpu_window_ms === "number" ? d.cpu_window_ms : null);
      setProcsAt(Date.now());
      setProcsError(null);
      // Процессы пришли — значит ПК на связи, даже если предыдущий замер
      // метрик провалился.
      markAlive();
    } catch (e) {
      // Выключенный ПК — честное офлайн-состояние (как на «Мониторе»), любой
      // другой отказ — понятная фраза в пустом состоянии и тост: «Обновить»
      // обязано давать обратную связь.
      if (isPcOffline(e)) {
        setProcsError(null);
        fail(e);
      } else {
        const message = mapApiError(e);
        setProcsError(message);
        toastError(message);
      }
    } finally {
      setProcsLoading(false);
    }
  }, [procSort, markAlive, fail, toastError]);

  const refreshRemoteHealth = useCallback(async () => {
    try { setAutostartStatus(await getAutostartStatus()); } catch { /* */ }
    try { setServiceStatus(await getServiceStatus()); } catch { /* */ }
  }, []);

  useEffect(() => {
    getCostStats().then(setCosts).catch(() => {});
    refreshRemoteHealth();
  }, [refreshRemoteHealth]);

  // Метрики нужны только на видимой вкладке «Монитор»: каждый /api/system/stats
  // занимает агента примерно на 500 мс (cpu.Percent), а на «Процессах» и
  // «Питании» stats не рендерятся вовсе. Заодно чинится старый баг: офлайн
  // не был в deps эффекта, поэтому ветка 15000 никогда не применялась.
  usePolling(refreshStats, offline ? 15000 : 5000, { enabled: tab === "monitor" });

  useEffect(() => {
    if (tab === "processes") refreshProcs();
  }, [tab, refreshProcs]);

  // Список процессов открывают с вопросом «кто ест процессор ПРЯМО СЕЙЧАС».
  // Раньше цифры стояли до ручного «↻», и процент означал среднее за промежуток
  // между двумя нажатиями. Пять секунд выбраны не наугад: базовый замер у агента
  // живёт 10 с (cpuSnapMaxAge), поэтому при таком темпе он не прогревается
  // заново на каждом запросе. `immediate: false` — первый запрос уже сделал
  // эффект выше, второй был бы лишним снимком дерева процессов.
  usePolling(refreshProcs, offline ? 15000 : 5000, {
    // Пока палец на «✖», список замораживаем целиком: пересортировка под
    // пальцем — прямой путь завершить не тот процесс.
    enabled: tab === "processes" && !holdingProc,
    immediate: false,
  });

  /**
   * Диалог с последствиями для «своих» (агент, окно панели, pty-хост) и
   * системных процессов. В окне exe нативный confirm подавлен, поэтому
   * tgConfirm рисует свой DialogHost.
   */
  const confirmGuardedKill = (p: GuardedProcess, reason: "self" | "critical") => {
    const label = procTitle(p);
    // Номер процесса в вопросе не повторяем: он уже стоит в строке, по которой
    // человек и нажал, а в диалоге отвечает на «что закрываю» только имя.
    const message = reason === "self"
      ? t("confirm.killSelf", { name: label })
      : t("confirm.killCritical", { name: label });
    return tgConfirm(message, { danger: true, confirmText: t("sys.killProcess") });
  };

  const handleKill = async (p: GuardedProcess) => {
    // Список разморожен: дальше всё решает диалог с именем процесса, а не то,
    // какая строка стоит под пальцем (и refreshProcs ниже обязан примениться).
    holdingProcRef.current = null;
    setHoldingProc(null);
    const label = procTitle(p);
    // Спрашиваем ВСЕГДА и с именем жертвы: удержание подтверждает «я нажал
    // осознанно», но не «я целился именно в этот процесс» — строка могла
    // приехать под палец при пересортировке, а отмены у kill нет. Свои и
    // системные процессы получают более подробный текст с последствиями.
    let confirmed = Boolean(p.self || p.critical);
    if (confirmed) {
      if (!(await confirmGuardedKill(p, p.self ? "self" : "critical"))) return;
    } else if (!(await tgConfirm(t("confirm.killProcess", { name: label }), {
      danger: true, confirmText: t("sys.killProcess"),
    }))) {
      return;
    }

    // Флаги строки — снимок списка, а агент классифицирует процесс заново в
    // момент kill и видит больше: в списке `self` проставляется только по
    // процессам снимка и только когда отработал Ppid(), а в kill — подъёмом по
    // реальным предкам. При расхождении агент отвечает `confirm_required` —
    // это НЕ ошибка, а запрос подтверждения. Раньше здесь показывался тост
    // «повторите действие», повтор слал тот же confirm:false, и процесс нельзя
    // было завершить в принципе. Теперь спрашиваем и повторяем — ровно один
    // раз, чтобы не зациклиться, если агент требует подтверждение и дальше.
    for (let attempt = 0; attempt < 2; attempt++) {
      try {
        const res = await killProcess(p.pid, confirmed);
        hapticSuccess();
        // Агент завершил сам себя — обновлять список уже некому и незачем.
        if (res?.self) {
          toastSuccess(t("sys.killSelfDone"));
          return;
        }
        toastSuccess(t("toast.processKilled", { name: label }));
        refreshProcs();
        return;
      } catch (e: any) {
        // Ветвимся ТОЛЬКО по машинному коду, не по HTTP-статусу: статусы этих
        // ответов у агента меняются, а mapApiError к тому же превращает 409 в
        // «компьютер привязан к другому аккаунту», 403 — в «сессия истекла».
        const code = e?.code;
        // Спрашиваем только если ещё не подтверждали: если агент требует
        // подтверждение и на confirm:true — это уже не тупик клиента, повтор не
        // поможет (страховкой стоит и предел итераций цикла).
        if (code === "confirm_required" && !confirmed) {
          // Причина в теле ответа агента: reason = self|critical. До клиента
          // сейчас доезжает только code (api-core кладёт на ApiError code и
          // fingerprint), поэтому читаем reason «на вырост» и падаем на строку
          // списка: расхождение бывает по self — critical считается по имени
          // одинаково и в списке, и в kill.
          const serverReason = (e as { reason?: string })?.reason;
          const reason: "self" | "critical" =
            serverReason === "critical" || (!serverReason && p.critical && !p.self) ? "critical" : "self";
          // Отказался — обычная отмена, без тоста и вибро.
          if (!(await confirmGuardedKill(p, reason))) return;
          confirmed = true;
          continue;
        }
        hapticError();
        // Два разных отказа: process_protected — не хватило прав (админ помог бы),
        // process_system — ядро ОС, которое не завершается ни при каких правах.
        if (code === "process_system") toastError(t("sys.killSystem"));
        else if (code === "process_protected") toastError(t("sys.killProtected"));
        else if (code === "confirm_required") toastError(t("sys.killNeedsConfirm"));
        else toastError(mapApiError(e));
        return;
      }
    }
  };

  const handleScreenshot = async (display = screenshotDisplay) => {
    setScreenshotLoading(true);
    haptic();
    try {
      const data = await takeScreenshot({ width: 1280, display });
      setScreenshot(`data:${data.mime};base64,${data.data}`);
      setScreenshotDisplay(data.display ?? display);
      setScreenshotDisplays(data.displays || []);
      toastSuccess(t("toast.screenshotCaptured"));
    } catch (e: any) {
      toastError(mapApiError(e));
    }
    setScreenshotLoading(false);
  };

  const handleDownloadOriginalScreenshot = async () => {
    haptic();
    try {
      // Агент отдаёт ТОТ кадр, что показан в превью (он остаётся у него в
      // памяти после снимка) — раньше кнопка просила новый, и в галерею уезжал
      // другой момент экрана. `cached:false` = кадр протух и снят заново;
      // молчать об этом нельзя, человек сохраняет момент, а не «что там сейчас».
      const data = await takeScreenshot({ display: screenshotDisplay, original: true });
      const original = `data:${data.mime};base64,${data.data}`;
      await saveBlob(dataUrlToBlob(original), `screenshot-${Date.now()}.png`);
      hapticSuccess();
      if ((data as { cached?: boolean }).cached === false) toast(t("sys.screenshotOriginalStale"), "info");
    } catch (e: any) {
      if (!isShareCancel(e)) toastError(mapApiError(e));
    }
  };

  // Окно предпросмотра — модалка продукта, а не картинка поверх экрана: Esc и
  // системная «Назад» обязаны закрывать ЕГО. До этого Back с открытым снимком
  // уводил с «Системы» на главную (App.tsx сначала спрашивает открытые
  // оверлеи — useEscape как раз в этой очереди и регистрируется).
  const closeScreenshot = useCallback(() => setScreenshot(null), []);
  useEscape(!!screenshot, closeScreenshot);
  // Фокус внутрь окна при открытии и возврат на кнопку 📷, с которой его
  // позвали: иначе Tab продолжает ходить по «Системе» под затемнением.
  const screenshotSheetRef = useRef<HTMLDivElement | null>(null);
  useEffect(() => {
    if (!screenshot) return;
    const opener = document.activeElement as HTMLElement | null;
    screenshotSheetRef.current?.focus();
    return () => {
      if (opener && typeof opener.focus === "function" && document.contains(opener)) opener.focus();
    };
    // Только факт открытия: перевыбор монитора меняет сам снимок, и на каждый
    // новый кадр фокус прыгать не должен.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [!!screenshot]);

  // Компьютер уходит: цифры на карточках больше не живые, и сам ПК сейчас
  // исчезнет из приложения.
  const markPowerGone = useCallback(() => {
    setStats(null);
    fail({ status: 502, code: "power_sent" });
  }, [fail]);

  useEffect(() => {
    if (!pendingPower) {
      setPendingLeft(0);
      return;
    }
    const delay = Math.max(0, pendingPower.until - Date.now());
    // Обратный отсчёт в плашке: окно отмены должно быть видно, а не обещано
    // текстом «через 5 секунд», который не двигается.
    const tick = () => setPendingLeft(Math.max(0, Math.ceil((pendingPower.until - Date.now()) / 1000)));
    tick();
    const ticker = window.setInterval(tick, 250);
    const timer = window.setTimeout(() => {
      setPendingPower(null);
      markPowerGone();
      // Итог называет то, что реально будет: перезагруженный ПК вернётся сам,
      // выключенный — только после включения на самой машине.
      toastSuccess(t(pendingPower.action === "restart" ? "sys.powerSentRestart" : "sys.powerSentShutdown"));
    }, delay);
    return () => {
      window.clearInterval(ticker);
      window.clearTimeout(timer);
    };
  }, [markPowerGone, pendingPower, toastSuccess]);

  const handleAutostart = async () => {
    const newVal = !autostartEnabled;
    // Выключение автозапуска — необратимое с телефона действие: после
    // ближайшей перезагрузки агент не поднимется, компьютер пропадёт из
    // приложения, и включить обратно можно будет только сидя за машиной.
    // Вопрос без последствия («Отключить автозапуск?») об этом молчал.
    const label = newVal
      ? t("confirm.enableAutostart")
      : [t("confirm.disableAutostart"), t("confirm.disableAutostartConsequence")].join("\n\n");
    if (!(await tgConfirm(label, {
      danger: !newVal,
      confirmText: newVal ? t("confirm.btn.enable") : t("confirm.btn.disable"),
    }))) return;
    try {
      const status = await setAutostart(newVal);
      setAutostartStatus(status);
      refreshRemoteHealth();
      // Ответ агента несёт ФАКТИЧЕСКОЕ состояние. Раньше тост об успехе уходил
      // безусловно: на Linux с системным юнитом «Автозапуск выключен» отвечало
      // «Автозапуск выключен», а кнопка продолжала говорить «включён» — человек
      // не знал, поднимется ли сервер после ребута.
      if (status?.enabled !== newVal) {
        hapticError();
        toastError(status?.managed_externally ? t("sys.autostartManagedError") : t("sys.autostartMismatch"));
        return;
      }
      hapticSuccess();
      toastSuccess(newVal ? t("toast.autostartEnabled") : t("toast.autostartDisabled"));
    } catch (e: any) {
      hapticError();
      // Машинный код важнее статуса: mapApiError превратил бы 409 в «компьютер
      // привязан к другому аккаунту».
      if (e?.code === "managed_externally") {
        refreshRemoteHealth();
        toastError(t("sys.autostartManagedError"));
        return;
      }
      toastError(mapApiError(e));
    }
  };

  const handleRemoteRepair = async () => {
    if (!(await tgConfirm(t("confirm.repairRemoteDesktop"), { confirmText: t("confirm.btn.repair") }))) return;
    setRemoteRepairing(true);
    haptic();
    try {
      setAutostartStatus(await setAutostart(true));
      await refreshRemoteHealth();
      hapticSuccess();
      toastSuccess(t("toast.remoteRepaired"));
    } catch (e: any) {
      hapticError();
      toastError(mapApiError(e));
    } finally {
      setRemoteRepairing(false);
    }
  };

  // skipConfirm=true из HoldButton (restart/shutdown): удержание 900мс — уже
  // подтверждение, модальный confirm сверху был двойной защитой. Обычные
  // кнопки (lock/sleep) по-прежнему спрашивают через tgConfirm.
  const handlePower = async (action: string, skipConfirm = false) => {
    const labels: Record<string, string> = {
      shutdown: t(headless ? "confirm.shutdownServer" : "confirm.shutdown"),
      restart: t(headless ? "confirm.restartServer" : "confirm.restart"),
      sleep: t("confirm.sleep"),
      lock: t("confirm.lock"),
    };
    // Красная кнопка обязана называть действие: «Подтвердить» не говорит, что
    // именно случится с компьютером, а отменить это уже нельзя.
    const btns: Record<string, string> = {
      shutdown: t("confirm.btn.shutdown"),
      restart: t("confirm.btn.restart"),
      sleep: t("confirm.btn.sleep"),
      lock: t("confirm.btn.lock"),
    };
    // Последствие, которое человек не может отыграть назад. Раньше оно было
    // написано только для выключения, хотя «Сон» рвёт связь ровно так же:
    // Wake-on-LAN в продукте нет, разбудить машину с телефона нечем. У
    // блокировки последствие мягче, но тоже реальное — экран блокировки не
    // принимает удалённый ввод.
    const consequences: Record<string, string> = {
      shutdown: t("confirm.shutdownConsequence"),
      sleep: t("confirm.sleepConsequence"),
      lock: t("confirm.lockConsequence"),
    };
    const consequence = consequences[action] || "";
    // Перезагрузка и выключение уносят ВСЕ терминалы: PTY-сессии живут только в
    // памяти ПК. Сон их не закрывает, но замораживает вместе со всей машиной —
    // работающий агент встанет посреди задачи. Молча это делать нельзя:
    // сначала показываем, что именно сейчас остановится.
    if (action === "shutdown" || action === "restart" || action === "sleep") {
      // Считаем ЖИВЫЕ терминалы, а не только working/waiting: агент, который
      // думает больше двух минут, имеет статус stalled, а закончивший —
      // ready/idle. Раньше эти сессии в предупреждение не попадали, и удержание
      // кнопки проходило молча ровно тогда, когда агент работал.
      let open: string[] = [];
      let active = 0;
      try {
        const { sessions } = await listPtySessions({ aliveOnly: true });
        const alive = sessions.filter((s) => s.alive !== false);
        active = alive.filter((s) => s.status === "working" || s.status === "stalled" || s.status === "waiting").length;
        open = alive.map((s) => {
          const title = s.agent_kind || s.fg_process || t("sys.busyTerminal");
          return `${title} — ${s.name || s.id} · ${sessionStatusLabel(s.status)}`;
        });
      } catch {
        // Не смогли спросить — не выдумываем, идём обычным путём.
      }
      if (open.length) {
        const list = open.slice(0, 5).join("\n")
          + (open.length > 5 ? `\n${t("confirm.powerBusyMore", { n: open.length - 5 })}` : "");
        // Сон терминалы не закрывает — он их замораживает: врать про «будут
        // закрыты» нельзя, иначе следующий раз человек не поверит и выключению.
        const busyKey = action === "sleep" ? "confirm.sleepBusy" : "confirm.powerBusy";
        const listKey = action === "sleep" ? "confirm.sleepTerminals" : "confirm.powerTerminals";
        const message = [
          active
            ? t(busyKey, { n: open.length, busy: active, list })
            : t(listKey, { n: open.length, list }),
          consequence,
        ].filter(Boolean).join("\n\n");
        if (!(await tgConfirm(message, { danger: true, confirmText: btns[action] || t("dialog.confirm") }))) return;
      } else if (!skipConfirm) {
        const message = [labels[action] || `${action}?`, consequence].filter(Boolean).join("\n\n");
        if (!(await tgConfirm(message, { danger: true, confirmText: btns[action] || t("dialog.confirm") }))) return;
      }
    } else if (!skipConfirm && !(await tgConfirm(labels[action] || `${action}?`, { danger: true, confirmText: btns[action] || t("dialog.confirm") }))) {
      return;
    }
    try {
      // «Сон» отвечать не обязан: на Windows `SetSuspendState` возвращается
      // только ПОСЛЕ пробуждения, поэтому штатный исход запроса — обрыв или
      // таймаут (до 30 с молчащего экрана). Отклик даём сразу; реальный отказ
      // ниже перекроет его ошибкой и вернёт компьютер в «на связи».
      if (action === "sleep") {
        markPowerGone();
        toastSuccess(t("toast.pcSleeping"));
      }
      const res = (await powerAction(action)) as PowerResult;
      hapticSuccess();
      if (action === "shutdown" || action === "restart") {
        // Плашку «Отменить» рисуем ТОЛЬКО там, где отмена реально существует.
        // Новый агент присылает delay_ms/cancelable (Windows — окно от ОС,
        // Linux/macOS — отложенный запуск внутри агента). Старый агент полей не
        // присылает: на Windows окно есть исторически (`shutdown /s /t 5`), на
        // остальных ОС команда уходит немедленно — там честнее сказать
        // «команда отправлена», чем предлагать отмену, которой нет.
        const os = platform || serviceStatus?.os || "";
        const delay = typeof res?.delay_ms === "number" ? res.delay_ms : (os === "windows" ? 5000 : 0);
        const cancelable = res?.cancelable ?? (os === "windows" && delay > 0);
        if (cancelable && delay > 0) {
          setPendingPower({ action, until: Date.now() + delay });
        } else {
          markPowerGone();
          // Перезагрузка и выключение — разные обещания: одна машина вернётся
          // сама, вторую придётся включать руками. Общее «может временно
          // исчезнуть» противоречило предупреждению, прочитанному секунду назад.
          toastSuccess(t(action === "restart" ? "sys.powerSentRestart" : "sys.powerSentShutdown"));
        }
      } else if (action === "lock") {
        // Блокировка компьютер на связи оставляет — офлайн-состояние ставить
        // нельзя, но подтвердить, что команда сработала, обязаны.
        toastSuccess(t("toast.pcLocked"));
      }
    } catch (e: any) {
      // Оборванная связь после «Сна» — не ошибка, а ровно то, о чём
      // предупреждал диалог: компьютер уснул и ушёл из приложения. Тост о сне
      // уже показан, поверх него ставить «Не удалось» нельзя.
      if (action === "sleep" && (isPcOffline(e) || e?.status === 0 || e?.code === "timeout")) return;
      hapticError();
      // Сон не удался по существу — компьютер на связи, снимаем метку «ушёл»
      // (в том числе ту, что поставил живой канал).
      if (action === "sleep") markAlive();
      // Машинный код важнее статуса: mapApiError превратил бы 409 в «компьютер
      // привязан к другому аккаунту».
      if (e?.code === "power_unsupported") toastError(t("sys.powerUnsupported"));
      else toastError(mapApiError(e));
    }
  };

  // Фильтруем на клиенте: список уже в памяти, а лишний запрос к агенту стоит
  // ему снимка дерева процессов.
  const procFilter = procQuery.trim().toLowerCase();
  const visibleProcs = procFilter
    ? procs.filter((p) => (p.name || "").toLowerCase().includes(procFilter) || String(p.pid).includes(procFilter))
    : procs;

  const cancelPower = async () => {
    haptic();
    try {
      await powerAction("cancel");
      setPendingPower(null);
      toastSuccess(t("sys.powerCanceled"));
    } catch (e: any) {
      // Агент ответил «отменять нечего» — команда уже ушла в ОС. Врать про
      // «Отменено» нельзя: компьютер выключается.
      if (e?.code === "power_not_pending") {
        setPendingPower(null);
        markPowerGone();
        toastError(t("sys.powerCancelTooLate"));
        return;
      }
      toastError(mapApiError(e));
    }
  };

  // Короткое нажатие HoldButton гасится (onClick preventDefault), и мышью в
  // окне exe кнопка выглядит сломанной: ни тоста, ни вибро. Замеряем нажатие в
  // CAPTURE-фазе — собственные onPointerDown/onPointerUp кнопки перезаписать
  // нельзя, они объявлены после {...rest}.
  const holdStartRef = useRef(0);
  const holdPointRef = useRef<{ x: number; y: number } | null>(null);

  /** Смещение пальца, после которого жест считается прокруткой, а не удержанием. */
  const HOLD_SLOP_PX = 10;

  /**
   * Обработчики вокруг HoldButton: объяснить короткое нажатие, погасить
   * удержание при прокрутке и (для списка процессов) назвать жертву.
   *
   * Гашение по движению — защита от того, что палец, ведущий список, доводит
   * счётчик до выключения компьютера: у кнопки удержания `touch-action: none`,
   * поэтому браузер не шлёт `pointercancel` сам, а неявный pointer capture не
   * даёт сработать `onPointerLeave` до отрыва пальца. Свой `pointercancel`
   * всплывает до корня React и попадает в собственный обработчик HoldButton —
   * снаружи таймер иначе не остановить.
   */
  const holdHandlers = (victim?: GuardedProcess) => {
    const release = () => {
      holdStartRef.current = 0;
      holdPointRef.current = null;
      if (victim) {
        holdingProcRef.current = null;
        setHoldingProc(null);
      }
    };
    return {
      onPointerDownCapture: (e: ReactPointerEvent<HTMLButtonElement>) => {
        holdStartRef.current = Date.now();
        holdPointRef.current = { x: e.clientX, y: e.clientY };
        if (victim) {
          holdingProcRef.current = victim;
          setHoldingProc(victim);
        }
      },
      onPointerMoveCapture: (e: ReactPointerEvent<HTMLButtonElement>) => {
        const from = holdPointRef.current;
        if (!from) return;
        if (Math.abs(e.clientX - from.x) < HOLD_SLOP_PX && Math.abs(e.clientY - from.y) < HOLD_SLOP_PX) return;
        const target = e.currentTarget;
        release();
        target.dispatchEvent(typeof PointerEvent === "function"
          ? new PointerEvent("pointercancel", { bubbles: true })
          : new Event("pointercancel", { bubbles: true }));
      },
      onPointerUpCapture: () => {
        const started = holdStartRef.current;
        release();
        if (started && Date.now() - started < HOLD_MS) toast(t("sys.holdTooShort"), "info");
      },
      onPointerCancelCapture: release,
    };
  };
  const holdExplain = holdHandlers();

  /**
   * Касание строки списка процессов = «человек сейчас читает и целится».
   * Вешаем на КАЖДУЮ строку, а не на весь блок: тап по «По памяти / По CPU / ↻»
   * обязан переставить список немедленно, ради этого их и нажимают.
   * Отпускание ловим на окне (см. эффект ниже), а не на самой строке: строка
   * может исчезнуть из списка прямо под пальцем, и её pointerup не придёт
   * никогда — заморозка осталась бы вечной.
   */
  const procTouchHandlers = {
    onPointerDownCapture: () => { procTouching.current = true; },
  };

  /** Явная просьба переставить список: заморозку порядка снимаем сразу. */
  const releaseProcOrder = () => {
    procTouching.current = false;
    procOrderHoldUntil.current = 0;
  };

  // Палец ушёл (или жест забрала прокрутка) — порядок держим ещё несколько
  // секунд: ровно в этот момент человек и целится в «✖» строки, которую увидел.
  useEffect(() => {
    const release = () => {
      if (!procTouching.current) return;
      procTouching.current = false;
      procOrderHoldUntil.current = Date.now() + PROC_ORDER_HOLD_MS;
    };
    window.addEventListener("pointerup", release);
    window.addEventListener("pointercancel", release);
    return () => {
      window.removeEventListener("pointerup", release);
      window.removeEventListener("pointercancel", release);
    };
  }, []);

  const autostartLabel = (method?: string) => {
    switch (method) {
      case "scheduled_task": return t("sys.autostartMethodScheduled");
      case "startup_shortcut": return t("sys.autostartMethodShortcut");
      case "systemd_user": return t("sys.autostartMethodSystemd");
      case "launch_agent": return t("sys.autostartMethodLaunchAgent");
      case "systemd_system": return t("sys.autostartMethodSystemdSystem");
      default: return t("sys.autostartMethodNone");
    }
  };

  const macAgent = (platform || serviceStatus?.os) === "darwin";
  const autostartOnLabel = autostartStatus?.method === "launch_agent"
    ? autostartLabel("launch_agent") : t("sys.autostartOn");
  const remoteModeBad = !macAgent && serviceStatus?.running_as_service === true;
  // Автозапуском управляет система (system-юнит systemd от install.sh) — это
  // ПРАВИЛЬНАЯ установка сервера, а не проблема: раньше телефон писал
  // «Автозапуск выключен», а кнопка «Исправить» поднимала второй агент на 8080.
  const autostartExternal = autostartStatus?.managed_externally === true;
  const remoteAutostartBad = Boolean(autostartStatus && !autostartExternal && (
    !autostartStatus.enabled ||
    autostartStatus.legacy ||
    (autostartStatus.recommended && autostartStatus.method && autostartStatus.method !== autostartStatus.recommended)
  ));
  const remoteSeverity = remoteModeBad ? "bad" : remoteAutostartBad ? "warn" : "ok";
  // Строка статуса называет то же самое, что и поле «Режим» двумя рядами ниже:
  // «Запущено как Windows Service» и «Служба Windows» читались как два разных
  // факта. Про выключенный автозапуск говорим про ПРОГРАММУ, а не про «экран»:
  // у экрана своего автозапуска нет, а тумблер «Автозапуск» стоит на этой же
  // вкладке — два одинаковых слова про одну сущность сбивали с толку.
  const remoteStatusText = !serviceStatus || !autostartStatus
    ? t("sys.remoteChecking")
    : remoteModeBad
      ? t("sys.runModeService")
      : remoteAutostartBad
        ? (!autostartStatus.enabled ? t("sys.remoteStatusAutostartOffApp") : t("sys.remoteStatusLegacy"))
        : t("sys.remoteStatusOk");
  const canRepairRemote = !remoteModeBad && remoteAutostartBad;

  // ── Диск: настоящие тома, а не snap-образы ────────────────────────
  // Агент присылает тома, отсортированные по заполненности, и «главным» раньше
  // считался просто первый — на Ubuntu это snap на 100%, на Windows —
  // смонтированный ISO. Сначала выбрасываем служебные тома, затем главным берём
  // системный (там профиль и рабочие папки), а не самый заполненный.
  const allVolumes: Volume[] = stats?.disks?.length
    ? stats.disks
    : stats?.disk ? [stats.disk] : [];
  const volumes = allVolumes.filter((volume) => !isServiceVolume(volume));
  const hiddenVolumes = allVolumes.length - volumes.length;
  const mainDisk = volumes.find(isSystemVolume) || volumes[0] || stats?.disk;
  const mainDiskPercent = mainDisk?.percent ?? 0;
  const mainDiskLabel = mainDisk ? volumeLabel(mainDisk) : "";
  // Заполненные тома, которые в карточку не попали: «/var 94%» — причина, по
  // которой сборка встала, даже когда на системном диске место есть.
  const otherFull = volumes
    .filter((volume) => volume !== mainDisk && volume.percent >= 90)
    .map((volume) => `${volumeLabel(volume)} ${formatPercent(volume.percent, 0)}`);

  const cpuWindowLabel = procsWarmup
    ? t("sys.cpuWarmup")
    : cpuWindowMs && cpuWindowMs > 0
      ? t("sys.cpuWindow", {
        sec: cpuWindowMs >= 1000
          ? String(Math.round(cpuWindowMs / 1000))
          : (cpuWindowMs / 1000).toFixed(1).replace(".", ","),
      })
      : "";

  return (
    <div className="page">
      <div className="page-header">
        {/* На экране с кнопкой «Выключить» обязано быть написано, КАКОЙ
            компьютер выключаем: при нескольких машинах и Linux-сервере это
            единственная страховка от промаха (#69). */}
        <div style={{ flex: 1, minWidth: 0 }}>
          <h1 style={{ fontSize: 18 }}>{t("sys.title")}</h1>
          <DeviceChip />
        </div>
        {/* В шапке стояли два безымянных квадрата — монитор и фотоаппарат, и
            подпись у обоих жила только в title, которого на телефоне не вызвать.
            Монитор вёл в «Экран компьютера» — тот же раздел, что стоит вкладкой
            в нижней панели и пунктом сайдбара (BottomNav показывает её по тому
            же признаку remoteDesktop), поэтому дубль из шапки убран целиком.
            Осталось действие, которого больше нигде нет, — и теперь оно названо
            словом: фотоаппарат читался как «включить веб-камеру», хотя это
            снимок экрана компьютера. На headless-сервере дисплея нет — снимать
            нечего, кнопки нет вовсе. */}
        {hasDisplay && (
          <button
            className="btn btn-secondary btn-sm"
            // Стили инлайном намеренно: styles.css правит другой исполнитель, а
            // палец обязан попадать по кнопке уже сейчас (норма цели — 44 px).
            style={SCREENSHOT_BTN_STYLE}
            onClick={() => void handleScreenshot()}
            // У выключенного компьютера снимать нечего: яркая живая кнопка
            // вела бы в тупик с ошибкой. Правило то же, что на главной
            // (Dashboard: disabled={pcOffline}).
            disabled={screenshotLoading || offline}
          >
            <IconCamera size={18} />
            <span>{screenshotLoading ? t("quick.capturing") : t("sys.screenshot")}</span>
          </button>
        )}
      </div>

      {/* Tab bar */}
      <div className="sys-tabs">
        {SYSTEM_TABS.map((t_) => (
          <button key={t_} className={`sys-tab ${tab === t_ ? "active" : ""}`}
            onClick={() => { haptic(); selectTab(t_); }}>
            {{ monitor: t("sys.monitor"), processes: t("sys.processes"), power: t("sys.power") }[t_]}
          </button>
        ))}
      </div>

      <div className="page-content">
        {/* Monitor tab — loading skeleton */}
        {/* ПК не в сети — говорим прямо, а не крутим skeleton вечно. Признак
            берётся из живого канала тоже (см. linkOffline), поэтому честный
            ответ приходит сразу, а не через 9,7 секунды ожидания запроса. */}
        {tab === "monitor" && !stats && offline && (
          <OfflineState
            onRetry={() => { haptic(); void refreshStats(); }}
            onDevices={getMode() === "cloud" ? () => navigate("/infrastructure") : undefined}
          />
        )}
        {/* Скелетоны — обещание, что цифры сейчас появятся. У выключенного
            компьютера они не появятся, поэтому в офлайне их не рисуем вовсе. */}
        {tab === "monitor" && !stats && !offline && (
          <div className="sys-monitor sys-monitor-grid">
            {[1, 2, 3, 4].map((i) => (
              <div key={i} className="sys-card skeleton-card" aria-hidden="true">
                <div className="skeleton-line" style={{ width: "30%", height: 12 }} />
                <div className="skeleton-line" style={{ width: "20%", height: 24, marginTop: 8 }} />
                <div className="sys-gauge"><div className="skeleton-line" style={{ width: "60%", height: "100%" }} /></div>
                <div className="skeleton-line" style={{ width: "50%", height: 10, marginTop: 6 }} />
              </div>
            ))}
          </div>
        )}

        {/* Monitor tab */}
        {/* ПК перестал отвечать уже после загрузки экрана: цифры на карточках
            устарели, и выдавать их за текущие нельзя — человек по ним примет
            решение (например, «памяти хватает, запущу ещё агента»). */}
        {tab === "monitor" && stats && offline && statsAt && (
          <div className="sys-stale-banner">
            {t("sys.statsStale", { time: new Date(statsAt).toLocaleTimeString("ru-RU", { hour: "2-digit", minute: "2-digit" }) })}
          </div>
        )}
        {tab === "monitor" && stats && (
          <div className={`sys-monitor sys-monitor-grid ${offline ? "stale" : ""}`}>
            <div className="sys-card">
              <div className="sys-card-title">{t("sys.cpu")}</div>
              <div className="sys-card-value">{formatPercent(stats.cpu.percent)}</div>
              <GaugeBar percent={stats.cpu.percent} color={stats.cpu.percent > 80 ? "var(--color-danger)" : "var(--color-info)"} />
              <div className="sys-card-sub">{stats.cpu.cores} {t("sys.cores")}</div>
            </div>

            {/* «ПАМЯТЬ 21,5 ГБ / 31,2 ГБ» не отвечало на вопрос, занято это или
                свободно, тогда как соседняя карточка диска говорила словами.
                Теперь обе устроены одинаково: слово «занято» — в заголовке,
                цифры — целыми фразами, и свободное место названо отдельно, ради
                него карточку и открывают. */}
            <div className="sys-card">
              <div className="sys-card-title">{t("sys.memoryTitleUsed")}</div>
              <div className="sys-card-value">{formatPercent(stats.memory.percent)}</div>
              <GaugeBar percent={stats.memory.percent} color={stats.memory.percent > 85 ? "var(--color-danger)" : "var(--color-success)"} />
              <div className="sys-card-sub">
                {t("sys.memoryUsedOf", { used: humanSize(stats.memory.used), total: humanSize(stats.memory.total) })}
                {" · "}
                {t("sys.memoryFree", { free: humanSize(stats.memory.available) })}
              </div>
            </div>

            {/* «ДИСК 96.3%» не говорило, занято это или свободно: слово
                «занято» стоит в заголовке рядом с именем тома. */}
            <div className="sys-card">
              <div className="sys-card-title">
                {mainDiskLabel
                  ? t("sys.diskVolumeTitle", { volume: mainDiskLabel })
                  : t("sys.diskTitleUsed")}
              </div>
              {/* Одна типографика на все карточки экрана (см. formatPercent):
                  у диска целые проценты, у процессора и памяти — десятая доля. */}
              <div className="sys-card-value">{formatPercent(mainDiskPercent, 0)}</div>
              <GaugeBar percent={mainDiskPercent} color={mainDiskPercent > 90 ? "var(--color-danger)" : "var(--color-warning)"} />
              <div className="sys-card-sub">
                {humanSize(mainDisk?.free ?? 0)} {t("sys.freeOf")} {humanSize(mainDisk?.total ?? 0)}
              </div>
              {otherFull.length > 0 && (
                <div className="sys-card-sub sys-disk-warn">
                  {t("sys.diskAlsoFull", { list: otherFull.slice(0, 3).join(" · ") })}
                </div>
              )}
            </div>

            <div className="sys-card">
              <div className="sys-card-title">{t("sys.uptime")}</div>
              <div className="sys-card-value">{formatUptime(stats.uptime)}</div>
            </div>

            <div className="sys-card sys-card-wide">
              <div className="sys-card-title">{t("sys.network")}</div>
              <div className="sys-card-sub" style={{ marginTop: 6 }}>
                {/* \u0421\u043A\u043E\u0440\u043E\u0441\u0442\u044C \u0441\u0435\u0439\u0447\u0430\u0441 \u2014 \u0442\u043E, \u0440\u0430\u0434\u0438 \u0447\u0435\u0433\u043E \u043A\u0430\u0440\u0442\u043E\u0447\u043A\u0443 \u0438 \u043E\u0442\u043A\u0440\u044B\u0432\u0430\u044E\u0442.
                    \u041A\u0443\u043C\u0443\u043B\u044F\u0442\u0438\u0432 \u0441 \u043C\u043E\u043C\u0435\u043D\u0442\u0430 \u0437\u0430\u0433\u0440\u0443\u0437\u043A\u0438 \u041E\u0421 \u043E\u0441\u0442\u0430\u0432\u043B\u0435\u043D \u043C\u0435\u043B\u043A\u043E\u0439 \u043F\u043E\u0434\u043F\u0438\u0441\u044C\u044E:
                    \u043D\u0430 \u0432\u043E\u043F\u0440\u043E\u0441 \u00AB\u043A\u0430\u043D\u0430\u043B \u0437\u0430\u043D\u044F\u0442?\u00BB \u043E\u043D \u043D\u0435 \u043E\u0442\u0432\u0435\u0447\u0430\u0435\u0442. */}
                {netRate
                  ? <>{"\u2B06"} {humanSize(netRate.up)}/\u0441 {"\u00B7"} {"\u2B07"} {humanSize(netRate.down)}/\u0441</>
                  : <>{"\u2B06"} {humanSize(stats.network.bytes_sent)} {t("sys.sent")} {"\u00B7"} {"\u2B07"} {humanSize(stats.network.bytes_recv)} {t("sys.received")}</>}
              </div>
              {netRate && (
                <div className="sys-card-sub sys-network-total">
                  {t("sys.networkTotal")}: {"\u2B06"} {humanSize(stats.network.bytes_sent)} · {"\u2B07"} {humanSize(stats.network.bytes_recv)}
                </div>
              )}
            </div>

            {/* \u0417\u0430\u0442\u0440\u0430\u0442\u044B AI \u2014 \u0442\u043E\u043B\u044C\u043A\u043E \u043A\u043E\u0433\u0434\u0430 \u0435\u0441\u0442\u044C \u0447\u0442\u043E \u043F\u043E\u043A\u0430\u0437\u044B\u0432\u0430\u0442\u044C (\u0434\u043B\u044F \u043F\u043E\u043B\u044C\u0437\u043E\u0432\u0430\u0442\u0435\u043B\u0435\u0439
                \u0431\u0435\u0437 AI-\u0430\u0433\u0435\u043D\u0442\u043E\u0432 \u043A\u0430\u0440\u0442\u043E\u0447\u043A\u0430 \u0441 $0.00 \u2014 \u043F\u0440\u043E\u0441\u0442\u043E \u0448\u0443\u043C). */}
            {/* Сумма считается ТОЛЬКО по сессиям агентов из бота. Терминалы, где
                и работают Claude Code/Codex, в неё не попадают, а у подписочных
                CLI цена не приходит вовсе — поэтому «$0.00» здесь означает «не
                измерено», а читалось как «AI мне ничего не стоит». Нулевую
                карточку не показываем совсем, а у ненулевой называем границу
                («unknown cost is never $0»). */}
            {costs && costs.total_cost > 0 && (
              <div className="sys-card sys-card-wide">
                <div className="sys-card-title">{t("sys.costs")}</div>
                <div className="sys-card-value">${costs.total_cost.toFixed(2)}</div>
                <div className="sys-card-sub">
                  {t("sys.today")}: ${costs.today_cost.toFixed(2)} | {t("sys.week")}: ${costs.week_cost.toFixed(2)}
                </div>
                <div className="sys-card-sub">{costs.total_messages} {t("sys.messagesTotal")}</div>
                <div className="sys-card-sub sys-card-scope">{t("sys.costsScope")}</div>
              </div>
            )}

            {/* Разделы диска: нижняя половина «Монитора» была пустой, а вопрос
                «где кончилось место» решался догадками — в карточке «Диск»
                помещается только один том. */}
            {volumes.length > 1 && (
              <div className="sys-card sys-card-wide">
                <div className="sys-card-title">{t("sys.diskVolumes")}</div>
                <div className="sys-vol-list">
                  {volumes.map((volume) => (
                    <div className="sys-vol" key={`${volume.device || ""}-${volume.mount || ""}`}>
                      <div className="sys-vol-head">
                        <span className="sys-vol-name">{volumeLabel(volume)}</span>
                        <span className="sys-vol-pct">{formatPercent(volume.percent, 0)}</span>
                      </div>
                      <GaugeBar
                        percent={volume.percent}
                        color={volume.percent > 90 ? "var(--color-danger)" : "var(--color-warning)"}
                      />
                      <div className="sys-vol-sub">
                        {humanSize(volume.free)} {t("sys.freeOf")} {humanSize(volume.total)}
                      </div>
                    </div>
                  ))}
                </div>
                {hiddenVolumes > 0 && (
                  <div className="sys-card-sub">{t("sys.diskHidden", { n: hiddenVolumes })}</div>
                )}
              </div>
            )}

            {/* Единственная дверь в лимиты AI вне облака: вкладки «Устройства»
                в окне exe и в LAN-режиме нет, а агенты запускают именно здесь. */}
            <button
              type="button"
              className="sys-card sys-card-wide sys-card-link"
              onClick={() => { haptic(); navigate("/agents"); }}
            >
              <span>
                {/* Имя и подпись — из общей таблицы разделов, а не своими
                    ключами: у раздела агентов их набралось четыре штуки, и
                    человек читал четыре описания одного экрана как четыре
                    разных места. */}
                <span className="sys-card-title">{sectionName("usage")}</span>
                <span className="sys-card-sub">{sectionDesc("usage")}</span>
              </span>
              <span className="sys-card-chevron" aria-hidden="true">{"→"}</span>
            </button>
          </div>
        )}

        {/* Processes tab */}
        {/* ПК не в сети: то же честное состояние, что и на «Мониторе». Раньше
            здесь оставалось «Процессы не загружены» с кнопкой «Обновить» —
            тупик без объяснения ровно там, где объяснение уже есть рядом. */}
        {tab === "processes" && offline && procs.length === 0 && (
          <OfflineState
            onRetry={() => { haptic(); void refreshProcs(); }}
            onDevices={getMode() === "cloud" ? () => navigate("/infrastructure") : undefined}
          />
        )}
        {tab === "processes" && !(offline && procs.length === 0) && (
          <div className="sys-procs">
            {/* Список уже загружен, а ПК перестал отвечать: снимок нельзя
                выдавать за текущее состояние (по нему решают, что «прибить»). */}
            {offline && procsAt && (
              <div className="sys-stale-banner">
                {t("sys.statsStale", { time: new Date(procsAt).toLocaleTimeString("ru-RU", { hour: "2-digit", minute: "2-digit" }) })}
              </div>
            )}
            <div className="sys-proc-header">
              {/* Смена сортировки уходит в сеть сама: refreshProcs зависит от
                  procSort, и эффект выше перезапрашивает список. */}
              <button className={`sys-sort-btn ${procSort === "memory" ? "active" : ""}`}
                onClick={() => { releaseProcOrder(); setProcSort("memory"); }}>{t("sys.byMemory")}</button>
              {/* «По памяти» рядом с «По CPU» — одна и та же нагрузка названа
                  по-русски и по-английски в одном ряду кнопок. */}
              <button className={`sys-sort-btn ${procSort === "cpu" ? "active" : ""}`}
                onClick={() => { releaseProcOrder(); setProcSort("cpu"); }}>{t("sys.byProcessor")}</button>
              <button className="sys-sort-btn" onClick={() => { releaseProcOrder(); void refreshProcs(); }}>{"\u21BB"}</button>
            </div>
            {/* \u041F\u0440\u043E\u0446\u0435\u043D\u0442 CPU \u2014 \u0434\u0435\u043B\u044C\u0442\u0430 \u043C\u0435\u0436\u0434\u0443 \u0434\u0432\u0443\u043C\u044F \u0437\u0430\u043C\u0435\u0440\u0430\u043C\u0438 \u0430\u0433\u0435\u043D\u0442\u0430, \u0438 \u0435\u0451 \u043D\u0430\u0434\u043E
                \u043F\u043E\u0434\u043F\u0438\u0441\u0430\u0442\u044C: \u0431\u0435\u0437 \u044D\u0442\u043E\u0433\u043E \u0446\u0438\u0444\u0440\u0430 \u043E\u0437\u043D\u0430\u0447\u0430\u043B\u0430 \u00AB\u043D\u0435\u043F\u043E\u043D\u044F\u0442\u043D\u043E \u0437\u0430 \u043A\u0430\u043A\u043E\u0439 \u0441\u0440\u043E\u043A\u00BB, \u0430
                \u0434\u043E \u0430\u0432\u0442\u043E\u043E\u0431\u043D\u043E\u0432\u043B\u0435\u043D\u0438\u044F \u043E\u043D\u0430 \u0432\u043E\u043E\u0431\u0449\u0435 \u0441\u0442\u043E\u044F\u043B\u0430 \u0434\u043E \u0440\u0443\u0447\u043D\u043E\u0433\u043E \u043E\u0431\u043D\u043E\u0432\u043B\u0435\u043D\u0438\u044F. */}
            {cpuWindowLabel && <div className="sys-proc-hint">{cpuWindowLabel}</div>}
            {/* \u0420\u0435\u0430\u043B\u044C\u043D\u044B\u0439 \u0441\u0446\u0435\u043D\u0430\u0440\u0438\u0439 \u2014 \u00AB\u043D\u0430\u0439\u0434\u0438 chrome/node \u0438 \u043F\u0440\u0438\u0431\u0435\u0439\u00BB \u2014 \u0440\u0435\u0448\u0430\u043B\u0441\u044F
                \u0441\u043A\u0440\u043E\u043B\u043B\u043E\u043C \u043F\u043E 50 \u0441\u0442\u0440\u043E\u043A\u0430\u043C svchost.exe. \u0421\u0442\u0440\u043E\u043A\u0430 \u043F\u0435\u0440\u0435\u0432\u043E\u0434\u0430 \u0438 \u0441\u0442\u0438\u043B\u044C \u043F\u043E\u0434
                \u043D\u0435\u0451 \u043B\u0435\u0436\u0430\u043B\u0438 \u0432 \u0440\u0435\u043F\u043E\u0437\u0438\u0442\u043E\u0440\u0438\u0438 \u043D\u0435\u0438\u0441\u043F\u043E\u043B\u044C\u0437\u043E\u0432\u0430\u043D\u043D\u044B\u043C\u0438. */}
            <input
              className="sys-proc-search-input"
              value={procQuery}
              onChange={(e) => setProcQuery(e.target.value)}
              placeholder={t("sys.processSearch")}
              inputMode="search"
            />
            {/* Про удержание надо сказать ДО нажатия: короткий клик по «✖»
                гасится, и кнопка выглядит сломанной (особенно мышью в окне exe). */}
            {visibleProcs.length > 0 && <div className="sys-proc-hint">{t("sys.holdKillHint")}</div>}
            {procs.length === 0 ? (
              procsLoading ? (
                <div className="empty"><div className="spinner" /></div>
              ) : (
                <div className="empty">
                  <div className="empty-icon">{"\u2699\ufe0f"}</div>
                  {/* Причина отказа словами: «Процессы не загружены» без неё
                      читалось как поломка функции. */}
                  <div className="empty-text">{procsError || t("sys.procsEmpty")}</div>
                  <button className="btn btn-secondary" onClick={() => void refreshProcs()}>{t("files.refresh")}</button>
                </div>
              )
            ) : visibleProcs.length === 0 ? (
              <div className="empty">
                <div className="empty-text">{t("sys.noProcesses")}</div>
              </div>
            ) : (
              visibleProcs.map((p) => (
                <div
                  key={p.pid}
                  className={`sys-proc-item${p.self || p.critical ? " sys-proc-guarded" : ""}${holdingProc?.pid === p.pid ? " sys-proc-holding" : ""}`}
                  {...procTouchHandlers}
                >
                  <div className="sys-proc-info">
                    {/* Заголовок строки — имя программы. Номер процесса стоял
                        здесь сам по себе («PID 3060») и не отвечал на вопрос
                        «что я закрываю»; теперь он уехал мелкой строкой ниже,
                        где и место служебному номеру. */}
                    <div className="sys-proc-name">{procTitle(p)}</div>
                    <div className="sys-proc-meta">
                      {/* Дельты по этой строке ещё нет: «0.0%» выдавал бы «нет
                          данных» за «процессор свободен». */}
                      {t("sys.procNumber", { pid: p.pid })} · {t("sys.cpu")}{" "}
                      {p.cpu_warmup
                        ? <span title={t("sys.cpuMeasuring")}>—</span>
                        : formatPercent(p.cpu)}
                      {" · "}{humanSize(p.memory)}
                    </div>
                    {/* Кто именно погибнет: на самой кнопке в этот момент
                        написано «Удерживайте…», а имя стоит отдельной строкой —
                        целясь пальцем, человек его не читает. */}
                    {holdingProc?.pid === p.pid && (
                      <div className="sys-proc-killing">
                        {t("sys.holdKillTarget", { name: procTitle(p) })}
                      </div>
                    )}
                    {(p.self || p.critical) && (
                      <div className="sys-proc-badges">
                        <span className={`sys-proc-badge ${p.self ? "self" : "critical"}`}>
                          {p.self ? t("sys.procSelfBadge") : t("sys.procCriticalBadge")}
                        </span>
                      </div>
                    )}
                  </div>
                  <HoldButton
                    className="sys-proc-kill"
                    onConfirm={() => handleKill(p)}
                    // Скринридер обязан назвать жертву: «Завершить процесс» не
                    // говорит, на какой строке стоит фокус.
                    aria-label={t("sys.killProcessHoldNamed", { name: procTitle(p) })}
                    title={t("sys.holdKillHint")}
                    {...holdHandlers(p)}
                  >
                    {"\u2716"}
                  </HoldButton>
                </div>
              ))
            )}
          </div>
        )}

        {/* Power tab */}
        {tab === "power" && (
          <div className="sys-power">
            {/* Почему компьютер пропадал и что у него со связью — здесь же, где
                человек включает автозапуск: это один и тот же разговор о том,
                чтобы машина возвращалась сама. Карточка молчит, когда сказать
                нечего (см. HealthCard). */}
            <HealthCard />
            {/* \u0421\u0435\u0440\u0432\u0438\u0441\u043D\u0430\u044F \u043A\u0430\u0440\u0442\u043E\u0447\u043A\u0430 \u043F\u0440\u043E \u0430\u0432\u0442\u043E\u0437\u0430\u043F\u0443\u0441\u043A \u0440\u0430\u0431\u043E\u0447\u0435\u0433\u043E \u0441\u0442\u043E\u043B\u0430 \u043F\u043E\u043A\u0430\u0437\u044B\u0432\u0430\u0435\u0442\u0441\u044F
                \u0442\u043E\u043B\u044C\u043A\u043E \u043A\u043E\u0433\u0434\u0430 \u0435\u0441\u0442\u044C \u0440\u0435\u0430\u043B\u044C\u043D\u0430\u044F \u043F\u0440\u043E\u0431\u043B\u0435\u043C\u0430 \u2014 \u044D\u0442\u043E \u0430\u043B\u044F\u0440\u043C, \u0430 \u043D\u0435 \u043C\u0435\u0431\u0435\u043B\u044C. */}
            {!macAgent && remoteSeverity !== "ok" && (
              <div className={`sys-remote-card sys-remote-${remoteSeverity}`}>
                <div className="sys-remote-head">
                  <span className="sys-remote-icon"><IconScreen size={22} /></span>
                  <div className="sys-remote-main">
                    <div className="sys-remote-title">{t("sys.remoteDesktop")}</div>
                    <div className="sys-remote-status">{remoteStatusText}</div>
                  </div>
                  <span className={`sys-remote-badge ${remoteSeverity}`}>
                    {remoteSeverity === "warn" ? t("sys.remoteBadgeWarn") : t("sys.remoteBadgeBad")}
                  </span>
                </div>
                <div className="sys-remote-grid">
                  <div>
                    <span>{t("sys.runMode")}</span>
                    <strong>{serviceStatus?.running_as_service ? t("sys.runModeService") : t("sys.runModeUser")}</strong>
                  </div>
                  <div>
                    <span>{t("sys.autostart")}</span>
                    <strong>{autostartStatus?.enabled ? autostartLabel(autostartStatus.method) : t("sys.autostartMethodNone")}</strong>
                  </div>
                </div>
                {/* Служба не видит рабочий стол — диагноз без лечения был
                    тупиком: удалённо это не чинится, зато на самой машине
                    чинится в два шага. Пишем шаги словами. */}
                {remoteModeBad && (
                  <div className="sys-remote-fix">{t("sys.remoteServiceFix")}</div>
                )}
                {/* Кнопка «Открыть» в режиме службы вела ровно в тот экран,
                    который заведомо не работает, — предлагать её нельзя.
                    Обе кнопки названы делом: «Открыть» и «Исправить» не
                    говорили, что откроется и что будет исправлено, — а стоят
                    они в карточке-диагнозе, где цена непонимания выше всего. */}
                {!remoteModeBad && (
                  <div className="sys-remote-actions">
                    <button className="sys-remote-action primary" onClick={() => { haptic(); navigate("/remote"); }}>
                      {t("sys.remoteOpen")}
                    </button>
                    {canRepairRemote && (
                      <button className="sys-remote-action secondary" onClick={handleRemoteRepair} disabled={remoteRepairing}>
                        {remoteRepairing ? t("sys.remoteRepairing") : t("sys.remoteRepair")}
                      </button>
                    )}
                  </div>
                )}
              </div>
            )}
            {/* Автозапуском владеет система (system-юнит systemd от install.sh):
                тумблер здесь только врал — «отключаю» отвечало успехом, а
                состояние не менялось. Показываем факт и место, где его менять. */}
            {autostartExternal ? (
              <div className="sys-power-note">
                <span className="sys-power-icon"><IconRepeat size={22} /></span>
                <div>
                  <strong>{t("sys.autostartManaged")}</strong>
                  <span>{t("sys.autostartManagedHint", { method: autostartLabel(autostartStatus?.method) })}</span>
                </div>
              </div>
            ) : autostartEnabled !== null && (
              <button
                className={`sys-power-btn ${autostartEnabled ? "sys-power-active" : ""}`}
                onClick={handleAutostart}
              >
                <span className="sys-power-icon"><IconRepeat size={22} /></span>
                {/* Строка в ряду «Блокировка / Сон / Перезагрузка» обязана
                    называть ДЕЙСТВИЕ: подписанная состоянием («Автозапуск
                    включён»), она выглядела справкой, а была переключателем.
                    Состояние ушло вниз мелкой подписью. */}
                <span>{autostartEnabled ? t("sys.autostartDisable") : t("sys.autostartEnable")}</span>
                <span className="sys-power-hint">{autostartEnabled ? autostartOnLabel : t("sys.autostartOff")}</span>
              </button>
            )}
            {pendingPower && (
              // Секунды, пока команда ещё не сработала: единственный шанс
              // отыграть промах назад. Отсчёт живой, а длину окна назвал агент.
              <div className="sys-power-pending">
                <span>
                  {t(pendingPower.action === "restart" ? "sys.restartingSoon" : "sys.shuttingDownSoon", { sec: pendingLeft })}
                </span>
                <button className="btn btn-sm" onClick={cancelPower}>{t("sys.powerCancel")}</button>
              </div>
            )}
            {/* Блокировка и сон бессмысленны на headless-сервере: там нет ни
                сессии, которую можно запереть, ни смысла усыплять машину, к
                которой ходят по сети. Кнопки, которые «ничего не делают»,
                читаются как поломка. */}
            {!headless && (
              <button className="sys-power-btn" onClick={() => handlePower("lock")}>
                <span className="sys-power-icon"><IconLock size={22} /></span>
                <span>{t("sys.lock")}</span>
              </button>
            )}
            {!headless && (
              <button className="sys-power-btn" onClick={() => handlePower("sleep")}>
                <span className="sys-power-icon"><IconMoon size={22} /></span>
                <span>{t("sys.sleep")}</span>
              </button>
            )}
            {/* «Удерживайте» написано на самой кнопке: короткий клик по ней
                гасится, и без подписи ДО нажатия она выглядит сломанной. */}
            <HoldButton className="sys-power-btn sys-power-warn" onConfirm={() => handlePower("restart", true)} {...holdExplain}>
              <span className="sys-power-icon"><IconRestart size={22} /></span>
              <span>{t("sys.restart")}</span>
              <span className="sys-power-hint">{t("sys.holdToConfirm")}</span>
            </HoldButton>
            <HoldButton className="sys-power-btn sys-power-danger" onConfirm={() => handlePower("shutdown", true)} {...holdExplain}>
              <span className="sys-power-icon"><IconPower size={22} /></span>
              <span>{t("sys.shutdown")}</span>
              <span className="sys-power-hint">{t("sys.holdToConfirm")}</span>
            </HoldButton>
          </div>
        )}
      </div>

      {/* Screenshot preview is independent of the active tab: taking it from
          Processes or Power must not produce a success toast with no result. */}
      {/* Окно объявлено диалогом (role + aria-modal + ловушка фокуса) и
          закрывается по Esc и системной «Назад» (useEscape): раньше «Назад»
          уносила с «Системы» на главную, а закрыть снимок можно было только
          тапом мимо него. */}
      {screenshot && (
        <div className="sys-screenshot-overlay" onClick={closeScreenshot}>
          <div
            ref={screenshotSheetRef}
            className="sys-screenshot-sheet"
            role="dialog"
            aria-modal="true"
            aria-labelledby="sys-screenshot-title"
            tabIndex={-1}
            onClick={(event) => event.stopPropagation()}
            onKeyDown={trapTabInSheet}
          >
            <div className="sys-screenshot-head">
              <strong id="sys-screenshot-title">{t("sys.screenshotPreview")}</strong>
              <button onClick={closeScreenshot} aria-label={t("modal.close")}>{"\u00D7"}</button>
            </div>
            {screenshotDisplays.length > 1 && (
              <div className="sys-screenshot-displays">
                {screenshotDisplays.map((display) => (
                  <button key={display.id}
                    className={display.id === screenshotDisplay ? "active" : ""}
                    disabled={screenshotLoading}
                    onClick={() => void handleScreenshot(display.id)}>
                    {display.id + 1} · {display.w}×{display.h}
                  </button>
                ))}
              </div>
            )}
            <img src={screenshot} alt={t("sys.screenshotPreview")} />
            <div className="sys-screenshot-actions">
              <button className="btn btn-secondary" onClick={closeScreenshot}>
                {t("sys.closePreview")}
              </button>
              <button className="btn btn-primary" onClick={handleDownloadOriginalScreenshot}>
                {"\u2B07"} {t("sys.downloadOriginal")}
              </button>
            </div>
          </div>
        </div>
      )}

      <BottomNav active="system" />
    </div>
  );
}
