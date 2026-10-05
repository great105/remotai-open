import { useEffect, useState, useCallback, useMemo, useRef } from "react";
import { useNavigate, useSearchParams } from "react-router-dom";
import { SheetShell, useEscape } from "@tgcontrol/shared";
import { useBackHandler } from "../hooks/backHandler";
import {
  listFiles, uploadFiles, deleteFile, deleteDir, mkDir, renameFile,
  sendToTelegram, getQuickPaths, createSession, searchFiles, previewFile,
  getBookmarks, addBookmark, removeBookmark, getAgents, downloadBlob, getDiskInfo,
  getDirStat, copyFile, getRecentFolders, getProjects, onConnectionChange,
} from "../api";
import { haptic, hapticSuccess, hapticError, tgConfirm, getTelegram } from "../telegram";
import { getMode } from "../config";
import { useToast } from "@tgcontrol/shared";
import { useFeatures } from "../hooks/useFeatures";
import { useBotAvailable } from "../hooks/useBotAvailable";
import { saveBlob, isShareCancel } from "../saveFile";
import { t } from "../i18n";
import type {
  FileItem, QuickPath, Bookmark, FilePreview, AgentInfo, DiskInfo,
  RecentFolder, Project,
} from "../types";
import { BottomNav } from "../components/BottomNav";
import { DeviceChip } from "../components/DeviceChip";
import { HoldButton } from "../components/HoldButton";
import { HelpSheet } from "../components/HelpSheet";
import { IconCopy, IconDoc, IconDownload, IconDrive, IconFolder, IconHome, IconScreen, IconStar, IconTerminal } from "../components/icons";
import {
  folderLabel, useLoadState, OfflineState, mapApiError, FolderNavSheet, humanSize,
  type FileTransferResult,
} from "@tgcontrol/shared";
import { markHomeStep } from "../homeProgress";
// Правила путей (Windows + POSIX) живут отдельно и проверяются тестом:
// ошибка здесь тихая — человек просто попадает не в ту папку. См. files/paths.ts.
import { joinPath, parentOf, samePathKey, volumeKey } from "../files/paths";

// Размер тома почти не меняется — перечитываем не чаще раза в минуту.
const DISK_TTL_MS = 60_000;

// Признаки «это не текст» в ответе просмотра: нулевой байт и символ-замена, в
// который Go превращает не-UTF-8 при сериализации JSON. Через fromCharCode —
// чтобы невидимые символы не жили в исходнике сами по себе.
const NUL_CHAR = String.fromCharCode(0);
const REPLACEMENT_CHAR = String.fromCharCode(0xfffd);

// Потолок доставки файла ботом (tgcontrol-relay/internal/server/files_send.go).
// Выше него предлагать «Прислать в Telegram» нельзя — обещание не выполнится.
const TG_FILE_LIMIT = 49 * 1024 * 1024;
// С какого размера предупреждаем перед скачиванием на телефон. 30 МБ — это уже
// заметный мобильный трафик, и до 49 МБ ещё работает выход «прислать ботом».
const BIG_DOWNLOAD = 30 * 1024 * 1024;

// Дата файла. Без года «12.03 09:15» одинаково выглядело и у вчерашнего файла,
// и у файла 2019-го — выбрать «последнюю версию договора» глазами было нечем.
// Поэтому: сегодняшний файл — «сегодня 09:15», прошлые годы — с годом и без
// времени (год важнее минут), текущий год — как раньше.
function formatDate(ts: number | null): string {
  if (!ts) return "";
  const d = new Date(ts * 1000);
  const now = new Date();
  const day = d.getDate().toString().padStart(2, "0");
  const mon = (d.getMonth() + 1).toString().padStart(2, "0");
  const h = d.getHours().toString().padStart(2, "0");
  const m = d.getMinutes().toString().padStart(2, "0");
  if (d.getFullYear() !== now.getFullYear()) return `${day}.${mon}.${d.getFullYear()}`;
  if (d.getMonth() === now.getMonth() && d.getDate() === now.getDate()) {
    return `${t("files.dateToday")} ${h}:${m}`;
  }
  return `${day}.${mon} ${h}:${m}`;
}

/*
 * \u0422\u0430\u0431\u043B\u0438\u0446\u0430 \u0437\u043D\u0430\u0447\u043A\u043E\u0432 \u043F\u043E \u0440\u0430\u0441\u0448\u0438\u0440\u0435\u043D\u0438\u044E \u0443\u0434\u0430\u043B\u0435\u043D\u0430 \u0432 2.49.0: \u0441\u0442\u0440\u043E\u043A\u0438 \u0441\u043F\u0438\u0441\u043A\u0430 \u043F\u043E\u043C\u0435\u0447\u0430\u043B\u0438\u0441\u044C
 * \u0441\u0438\u0441\u0442\u0435\u043C\u043D\u044B\u043C\u0438 \u044D\u043C\u043E\u0434\u0437\u0438 (\u043F\u0430\u043F\u043A\u0430, \u043A\u0430\u0440\u0442\u0438\u043D\u043A\u0430, \u043A\u043E\u0440\u043E\u0431\u043A\u0430), \u0442\u043E \u0435\u0441\u0442\u044C \u043D\u0430 Android, iOS \u0438
 * Windows \u0432\u044B\u0433\u043B\u044F\u0434\u0435\u043B\u0438 \u043F\u043E-\u0440\u0430\u0437\u043D\u043E\u043C\u0443 \u0438 \u0432\u0441\u0435\u0433\u0434\u0430 \u0446\u0432\u0435\u0442\u043D\u044B\u043C\u0438 \u2014 \u0440\u044F\u0434\u043E\u043C \u0441 \u043B\u0438\u043D\u0435\u0439\u043D\u044B\u043C\u0438 \u043C\u044F\u0442\u043D\u044B\u043C\u0438
 * \u0438\u043A\u043E\u043D\u043A\u0430\u043C\u0438 \u0432\u0438\u0442\u0440\u0438\u043D\u044B \u0438 \u043E\u0441\u0442\u0430\u043B\u044C\u043D\u044B\u0445 \u0440\u0430\u0437\u0434\u0435\u043B\u043E\u0432. \u0422\u0435\u043F\u0435\u0440\u044C \u0441\u0442\u0440\u043E\u043A\u0430 \u0440\u0430\u0437\u043B\u0438\u0447\u0430\u0435\u0442 \u0433\u043B\u0430\u0432\u043D\u043E\u0435 \u2014
 * \u043F\u0430\u043F\u043A\u0443 \u0438 \u0444\u0430\u0439\u043B \u2014 \u0431\u0440\u0435\u043D\u0434\u043E\u0432\u044B\u043C\u0438 IconFolder/IconDoc, \u0430 \u0442\u0438\u043F \u0447\u0435\u043B\u043E\u0432\u0435\u043A \u0447\u0438\u0442\u0430\u0435\u0442 \u0442\u0430\u043C, \u0433\u0434\u0435
 * \u043E\u043D \u0438 \u043D\u0430\u043F\u0438\u0441\u0430\u043D: \u0432 \u0440\u0430\u0441\u0448\u0438\u0440\u0435\u043D\u0438\u0438 \u0438\u043C\u0435\u043D\u0438.
 */

/**
 * Значок плитки быстрого доступа. Бренд — линейные SVG (components/icons.tsx),
 * системные эмодзи на Android, iOS и Windows выглядят по-разному и всегда
 * цветные. Знак берём только там, где он есть и означает ровно это: «Домашняя»
 * — домик, «Рабочий стол» — монитор, папка программы и любые обычные каталоги
 * (/opt, /etc, домашние папки Linux) — папка. Корни дисков (C:, D:) пока
 * остаются со старым значком: иконки диска в icons.tsx ещё нет, а звать диск
 * папкой — врать. Появится IconDisk — сюда добавится ещё одна строка.
 */
function quickIcon(name: string): React.ReactNode {
  switch (name) {
    case "Home": return <IconHome size={26} />;
    case "Desktop": return <IconScreen size={26} />;
    case "CWD": return <IconFolder size={26} />;
    case "Downloads": return <IconDownload size={26} />;
    case "Documents": return <IconDoc size={26} />;
    // Диски (C:, D:) получили свой знак: дискета обозначала их с прошлого века
    // и рисовалась системным шрифтом — то есть на Android, iOS и Windows
    // по-разному и всегда цветной, рядом с линейными мятными иконками.
    default: return /^[A-Za-z]:$/.test(name) ? <IconDrive size={26} /> : <IconFolder size={26} />;
  }
}

/**
 * Коробка значка плитки. Нужна, чтобы SVG и оставшиеся эмодзи стояли на одной
 * высоте: у эмодзи её задаёт font-size (.fm-quick-icon), у svg — размер самого
 * элемента, и без общей коробки соседние плитки разъезжались по вертикали.
 */
function QuickIcon({ children }: { children: React.ReactNode }) {
  return (
    <span
      className="fm-quick-icon"
      style={{ display: "flex", alignItems: "center", justifyContent: "center", height: 28 }}
      aria-hidden
    >
      {children}
    </span>
  );
}

// Один и тот же путь приходит в разном написании: с хвостовым разделителем и в
// любом регистре (Windows). Приводим к одному виду, чтобы «C:\Work» из
// избранного и «c:\work\» из найденных проектов считались одной папкой. Путь из
// одного символа («/») не трогаем: там разделитель и есть весь путь.
// Where the user was last time — restored on mount so the file manager opens
// where they left off instead of resetting to quick access every visit.
const LAST_DIR_KEY = "tgc_files_last_dir";
const SORT_KEY = "tgc_files_sort";
const HIDDEN_KEY = "tgc_files_hidden";

export function FilesView() {
  const navigate = useNavigate();
  const { toast, toastSuccess, toastError } = useToast();
  // Агентские действия скрыты, пока AI-режим не разблокирован (пока продукт —
  // «только терминал»); «Отправить в Telegram» — только когда на ПК есть бот.
  const { ai } = useFeatures();
  const botAvailable = useBotAvailable();
  const [currentPath, setCurrentPath] = useState("");
  // `?cwd=` — папка, которую попросил открыть другой экран (терминал: «Файлы
  // этой папки»). До этого «Файлы» не читали адрес вовсе, и обратной двери из
  // терминала в файлы не было (аудит ИА 02.09.2026, P1-30).
  const [searchParams] = useSearchParams();
  const [items, setItems] = useState<FileItem[]>([]);
  const [parentPath, setParentPath] = useState<string | null>(null);
  const [loading, setLoading] = useState(true);
  // Различаем «папка пуста» и «компьютер не в сети».
  const load = useLoadState();
  const { succeed, fail } = load;
  // «Компьютер не в сети» узнаём из живого канала, а не из упавшего запроса:
  // при выключенном ПК ответа приходилось ждать 6,0 с, и всё это время экран
  // крутил спиннер, то есть обещал содержимое, которого не будет. Подписка та
  // же, что на главной (Dashboard) — там экран отвечает сразу.
  const [linkOffline, setLinkOffline] = useState(false);
  // «Проверить снова» обязано давать видимый отклик: на время повторной попытки
  // спиннер снова получает приоритет над плашкой офлайна.
  const [retrying, setRetrying] = useState(false);
  useEffect(() => onConnectionChange((state) => {
    if (state.reason === "pc_offline") { setLinkOffline(true); return; }
    if (state.connected) setLinkOffline(false);
  }), []);
  const [quickPaths, setQuickPaths] = useState<QuickPath[]>([]);
  const [showQuick, setShowQuick] = useState(true);
  const [pathInput, setPathInput] = useState("");
  const [showPathInput, setShowPathInput] = useState(false);
  const [showNewFolder, setShowNewFolder] = useState(false);
  const [newFolderName, setNewFolderName] = useState("");
  const [contextItem, setContextItem] = useState<FileItem | null>(null);
  // Групповой выбор (долгий тап по строке или «Выбрать несколько» в «⋯»).
  // Раньше выделение было ровно одно: забрать 30 фотографий из отпуска значило
  // 30 раз «тап → Скачать → дождаться → системный лист сохранения».
  const [selectMode, setSelectMode] = useState(false);
  const [selected, setSelected] = useState<string[]>([]);
  // Меню «⋯ Ещё» у ПАПКИ. У файла второго этажа больше нет: все его действия
  // стоят одним столбцом в шторке (2.49.0).
  const [toolsMenu, setToolsMenu] = useState(false);
  // Меню отмеченной пачки: перенос, копирование, удаление.
  const [selMenu, setSelMenu] = useState(false);
  // Поиск по папке открывается кнопкой: постоянное поле съедало 56 px шапки
  // ради действия, которым пользуются изредка.
  const [showSearch, setShowSearch] = useState(false);
  const [showHelp, setShowHelp] = useState(false);
  const [sending, setSending] = useState<string | null>(null);
  const [showRename, setShowRename] = useState<FileItem | null>(null);
  const [renameTo, setRenameTo] = useState("");
  const [showNewSession, setShowNewSession] = useState(false);
  const [sessionName, setSessionName] = useState("");
  const [sessionAgent, setSessionAgent] = useState("claude");
  const [searchQuery, setSearchQuery] = useState("");
  const [searchResults, setSearchResults] = useState<FileItem[] | null>(null);
  const [searching, setSearching] = useState(false);
  const [searchMeta, setSearchMeta] = useState<{
    truncated: boolean; timedOut: boolean; scanned: number; elapsedMs: number;
  } | null>(null);
  const searchAbort = useRef<AbortController | null>(null);
  const [searchReturn, setSearchReturn] = useState<{
    query: string; results: FileItem[];
    meta: { truncated: boolean; timedOut: boolean; scanned: number; elapsedMs: number } | null;
  } | null>(null);
  const [highlightPath, setHighlightPath] = useState<string | null>(null);
  const [bookmarks, setBookmarks] = useState<Bookmark[]>([]);
  const [placesView, setPlacesView] = useState<"list" | "tiles">(() => {
    try { return localStorage.getItem("files.placesView") === "tiles" ? "tiles" : "list"; }
    catch { return "list"; }
  });
  const changePlacesView = (view: "list" | "tiles") => {
    haptic(); setPlacesView(view);
    try { localStorage.setItem("files.placesView", view); } catch { /* private mode */ }
  };
  const [recentFolders, setRecentFolders] = useState<RecentFolder[]>([]);
  const [projects, setProjects] = useState<Project[]>([]);
  const [preview, setPreview] = useState<FilePreview | null>(null);
  // Файл, который смотрим: без него из модалки просмотра не было выхода к
  // самому файлу — только ✖ (находка N115).
  const [previewItem, setPreviewItem] = useState<FileItem | null>(null);
  // Меню файла закрывается перед модальным окном (его слой выше окна).
  // Возвращаем фокус на строку файла после отмены или просмотра.
  const fileModalReturnRef = useRef<HTMLElement | null>(null);
  const rememberFileRow = (path: string) => {
    fileModalReturnRef.current = [...document.querySelectorAll<HTMLElement>(".fm-item[data-fm-path]")]
      .find((row) => row.dataset.fmPath === path) ?? null;
  };
  const restoreFileRowFocus = () => {
    const row = fileModalReturnRef.current;
    fileModalReturnRef.current = null;
    if (row) requestAnimationFrame(() => { if (row.isConnected) row.focus(); });
  };
  const closeRename = () => { setShowRename(null); restoreFileRowFocus(); };
  const closePreview = () => { setPreview(null); restoreFileRowFocus(); };
  const [agents, setAgents] = useState<AgentInfo[]>([]);
  // Скрытые файлы (.env, .ssh, .gitignore) раньше были недостижимы вовсе.
  // Выбор запоминаем: разработчик, включивший их раз, ждёт их и завтра.
  const [showHidden, setShowHidden] = useState<boolean>(() => {
    try { return localStorage.getItem(HIDDEN_KEY) === "1"; } catch { return false; }
  });
  // Ref нужен, чтобы loadDir не пересоздавался при каждом переключении и не
  // тянул за собой цепочку эффектов (грабля 2.28.0 — лавина запросов).
  const showHiddenRef = useRef(showHidden);
  const [listInfo, setListInfo] = useState<{
    total: number; truncated: boolean; hiddenSkipped: number;
  } | null>(null);
  // Отказ ПАПКИ при живом списке прошлой (нет прав, путь исчез). Тост живёт
  // пару секунд, а на экране остаётся СТАРАЯ папка — без этой строки отказ
  // читался как «тап не сработал», и человек жал по папке снова и снова.
  const [dirError, setDirError] = useState<string | null>(null);
  // Прокрутка — у самой страницы. Запоминаем позицию папки на выходе и
  // возвращаем при подъёме «Наверх»: иначе возврат бросал человека в
  // произвольное место списка, и папку, из которой он вышел, приходилось
  // искать глазами. Заход в подпапку всегда начинается сверху.
  const currentPathRef = useRef("");
  const scrollMem = useRef<Map<string, number>>(new Map());
  // Переход из результатов поиска сам доводит строку до экрана (scrollIntoView),
  // и наша прокрутка в начало ему бы мешала.
  const skipScrollRef = useRef(false);
  const [sortBy, setSortBy] = useState<"name" | "date" | "size">(() => {
    const saved = localStorage.getItem(SORT_KEY);
    return saved === "date" || saved === "size" ? saved : "name";
  });
  // Порядок задаёт СЕРВЕР: он сортирует все записи и только потом режет лимит в
  // 2000. Локальная пересортировка обрезанного списка врала — «По дате» строила
  // порядок из произвольной алфавитной выборки. sortRef нужен, чтобы loadDir не
  // пересоздавался (грабля 2.28.0 — лавина запросов), serverSort — эхо ответа:
  // старый агент его не пришлёт, и тогда сортируем сами, как раньше.
  const sortRef = useRef(sortBy);
  const [serverSort, setServerSort] = useState<string | null>(null);
  const breadcrumbsRef = useRef<HTMLDivElement>(null);
  // «Свободно X из Y» для тома текущей папки. Статика: НИКАКОГО поллинга —
  // только при смене тома, по TTL и после операций записи.
  const [disk, setDisk] = useState<DiskInfo | null>(null);
  const diskCache = useRef<Map<string, { data: DiskInfo; at: number }>>(new Map());
  // Поздний ответ папки A не имеет права перезатереть уже открытую папку B.
  const loadSeq = useRef(0);

  const refreshDisk = useCallback(async (path: string) => {
    if (!path) { setDisk(null); return; }
    const key = volumeKey(path);
    const hit = diskCache.current.get(key);
    if (hit && Date.now() - hit.at < DISK_TTL_MS) { setDisk(hit.data); return; }
    try {
      const d = await getDiskInfo(path);
      // Сервер отдаёт 200 с нулями вместо ошибки — иначе получим «0 B из 0 B».
      if (!d || !d.total) { setDisk(null); return; }
      diskCache.current.set(key, { data: d, at: Date.now() });
      setDisk(d);
    } catch {
      // ПК не в сети / том недоступен — строку просто не показываем.
      // Тост здесь НЕ показывать: об офлайне уже говорит OfflineState.
      setDisk(null);
    }
  }, []);

  const loadDir = useCallback(async (path: string) => {
    const seq = ++loadSeq.current;
    setLoading(true);
    setContextItem(null);
    const from = currentPathRef.current;
    if (from) scrollMem.current.set(from, window.scrollY);
    try {
      const data = await listFiles(path, {
        hidden: showHiddenRef.current,
        sort: sortRef.current,
      });
      if (seq !== loadSeq.current) return;
      setServerSort(typeof data.sort === "string" ? data.sort : null);
      // Шаг чеклиста главной на headless-сервере (где нет экрана ПК).
      markHomeStep("files");
      // Папка прочитана — экран снова рабочий. Без этого phase после ОДНОЙ
      // неудачи оставался «offline» на всю сессию: «Проверить снова» перечитывало
      // папку, но любая пустая папка потом врала «Компьютер не в сети».
      succeed();
      setDirError(null);
      // Сервер отдаёт не больше 2000 записей. Без этой пары пользователь видел
      // бы обрезанный список как полный и решил, что файлов в папке нет.
      // hidden_skipped — сколько записей утаил фильтр скрытых: без него папка
      // с одними .env и .git уверяла, что она пустая.
      setListInfo({
        total: data.total ?? data.items.length,
        truncated: !!data.truncated,
        hiddenSkipped: data.hidden_skipped ?? 0,
      });
      setItems(data.items);
      setCurrentPath(data.path);
      setParentPath(data.parent);
      setShowQuick(false);
      setSearchQuery("");
      setSearchResults(null);
      setSearchMeta(null);
      void refreshDisk(data.path);
      try { localStorage.setItem(LAST_DIR_KEY, data.path); } catch { /* quota */ }
      // Прокрутка: вниз по дереву — с начала папки, вверх — туда, где вышли.
      // Перечитывание той же папки (↻, смена сортировки, после удаления)
      // позицию не трогает вовсе.
      const skip = skipScrollRef.current;
      skipScrollRef.current = false;
      if (!skip && data.path !== from) {
        const back = from && data.path === parentOf(from)
          ? scrollMem.current.get(data.path) ?? 0
          : 0;
        scrollMem.current.delete(data.path);
        // Два кадра: список рисуется после коммита React, до него scrollTo
        // упёрся бы в прежнюю высоту страницы.
        requestAnimationFrame(() => requestAnimationFrame(() => window.scrollTo(0, back)));
      }
      currentPathRef.current = data.path;
    } catch (e: any) {
      if (seq !== loadSeq.current) return;
      hapticError();
      setDisk(null);
      fail(e);
      // Причина остаётся НА ЭКРАНЕ строкой над списком: список прошлой папки мы
      // намеренно не чистим (иначе человек теряет и то, что уже нашёл), но и
      // молчать о том, что новая папка не открылась, нельзя.
      setDirError(mapApiError(e));
      toastError(mapApiError(e));
    }
    if (seq === loadSeq.current) setLoading(false);
  }, [fail, succeed, refreshDisk]);

  // Второстепенные списки быстрого доступа. Их провал не состояние экрана:
  // молчим, но перечитываем вместе с папкой по «Повторить».
  const loadSideLists = useCallback(() => {
    getBookmarks().then((d) => setBookmarks(d.bookmarks)).catch(() => {});
    getRecentFolders().then((d) => setRecentFolders((d.folders || []).slice(0, 8))).catch(() => {});
    getProjects().then((d) => setProjects((d.projects || []).slice(0, 8))).catch(() => {});
    getAgents().then((d) => {
      setAgents(d.agents);
      if (d.agents.length > 0) setSessionAgent(d.agents[0].id);
    }).catch(() => {});
  }, []);

  // Первичная загрузка экрана — отдельной функцией, чтобы «Повторить» в
  // офлайн/ошибке начинало ровно с того же места (раньше повторить первичный
  // getQuickPaths было нечем: ↻ и «Вверх» живут в тулбаре списка, а список при
  // выключенном ПК так и не появлялся).
  const bootstrap = useCallback(async () => {
    setLoading(true);
    try {
      const d = await getQuickPaths();
      setQuickPaths(d.paths);
      // Папка из адреса сильнее «последней открытой»: сюда пришли за ней.
      const wanted = searchParams.get("cwd");
      if (wanted) {
        await loadDir(wanted);
        return;
      }
      // Resume where the user left off.
      const last = localStorage.getItem(LAST_DIR_KEY);
      if (last) {
        await loadDir(last);
        return;
      }
      // Первый заход — «Быстрый доступ», а не paths[0]. Раньше экран молча
      // открывал первый быстрый путь (каталог установки Remotai), и человек,
      // поставивший программу «чтобы дотянуться до своих файлов», видел
      // remotai.exe, wintun.dll и logs — а плиток «Рабочий стол», «Документы»,
      // «C:» ему при этом даже не показали: loadDir гасит showQuick.
      // ПК ответил, просто не назвал ни одной папки — это «пусто», не «офлайн».
      succeed();
      setShowQuick(true);
      setLoading(false);
    } catch (e) {
      // Раньше пустой catch не вызывал loadDir вовсе, и setLoading(false) не
      // наступал НИКОГДА — экран крутил спиннер бесконечно.
      fail(e);
      setLoading(false);
    }
  }, [loadDir, fail, succeed]);

  useEffect(() => {
    void bootstrap();
    loadSideLists();
  }, [bootstrap, loadSideLists]);

  // Единая точка «Повторить» для офлайна и ошибки: перечитываем и папку, и
  // плитки. Если папку уже открывали — возвращаемся именно в неё.
  const retryAll = useCallback(() => {
    haptic();
    // Пока попытка идёт, экран показывает спиннер даже при заведомо мёртвом
    // канале: без этого нажатие на «Проверить снова» выглядело бы как «кнопка
    // не работает» — плашка офлайна просто оставалась на месте.
    setRetrying(true);
    loadSideLists();
    const done = () => setRetrying(false);
    if (currentPath) void loadDir(currentPath).finally(done);
    else void bootstrap().finally(done);
  }, [bootstrap, loadDir, loadSideLists, currentPath]);

  // Keep the tail of a deep path visible.
  useEffect(() => {
    const el = breadcrumbsRef.current;
    if (el) el.scrollLeft = el.scrollWidth;
  }, [currentPath]);

  // Найденный файл нужно ПОКАЗАТЬ, а не только подсветить: в папке на 600
  // файлов подсвеченная строка оставалась на 300 позиций ниже экрана, и поиск
  // выглядел «просто закрывшимся». Подсветку гасим через 4 секунды — иначе она
  // жила через все переходы до конца сессии (находка N113).
  useEffect(() => {
    if (!highlightPath || loading) return;
    const rows = Array.from(document.querySelectorAll<HTMLElement>("[data-fm-path]"));
    const target = rows.find((r) => r.dataset.fmPath === highlightPath);
    const frame = requestAnimationFrame(() => {
      target?.scrollIntoView({
        block: "center",
        behavior: window.matchMedia?.("(prefers-reduced-motion: reduce)").matches ? "auto" : "smooth",
      });
    });
    const timer = window.setTimeout(() => setHighlightPath(null), 4000);
    return () => { cancelAnimationFrame(frame); window.clearTimeout(timer); };
  }, [highlightPath, loading, items]);

  // System Back walks the file tree before leaving the screen:
  // context panel → close; folder → up; root folder → quick access; quick → home.
  useBackHandler(() => {
    // Меню и режим выбора «Назад» закрывает первыми: уйти с экрана из-под
    // открытого меню — потерять место, до которого человек добирался.
    if (toolsMenu) { setToolsMenu(false); return true; }
    if (selMenu) { setSelMenu(false); return true; }
    if (selectMode) { exitSelect(); return true; }
    if (contextItem) { setContextItem(null); return true; }
    if (!showQuick) {
      if (parentPath && parentPath !== currentPath) { haptic(); loadDir(parentPath); return true; }
      setShowQuick(true);
      return true;
    }
    return false;
  });

  // ── Выбор из нескольких выходов ───────────────────────────────
  // «Заменить / Сохранить копию / Отмена» в tgConfirm не вмещается: он умеет
  // только «да/нет», а третий выход здесь принципиален — иначе единственная
  // альтернатива перезаписи это отмена (находки N107/N112).
  type ChoiceOption = { id: string; label: string; danger?: boolean };
  const [choice, setChoice] = useState<{
    title: string; message: string; options: ChoiceOption[];
  } | null>(null);
  const choiceResolve = useRef<((id: string | null) => void) | null>(null);

  const answerChoice = useCallback((id: string | null) => {
    setChoice(null);
    const resolve = choiceResolve.current;
    choiceResolve.current = null;
    resolve?.(id);
  }, []);

  const ask = (req: { title: string; message: string; options: ChoiceOption[] }): Promise<string | null> => {
    haptic();
    // Предыдущий вопрос (если он как-то остался) закрываем отменой — два
    // одновременных диалога висели бы друг под другом.
    choiceResolve.current?.(null);
    return new Promise<string | null>((resolve) => {
      choiceResolve.current = resolve;
      setChoice(req);
    });
  };

  useEscape(!!choice, () => answerChoice(null));

  const [uploading, setUploading] = useState(false);
  const [uploadPct, setUploadPct] = useState<number | null>(null);
  const uploadAbort = useRef<AbortController | null>(null);

  const cancelUpload = () => {
    uploadAbort.current?.abort();
    uploadAbort.current = null;
    setUploading(false);
    setUploadPct(null);
  };

  /**
   * Свободное имя для «Сохранить копию»: «отчет (2).xlsx». Сверяемся с уже
   * загруженным списком папки — он неполон (лимит 2000, скрытые файлы), поэтому
   * это подсказка: если имя всё равно занято, сервер снова ответит
   * already_exists, и мы честно скажем об этом.
   */
  const copyName = (name: string): string => {
    const dot = name.lastIndexOf(".");
    const base = dot > 0 ? name.slice(0, dot) : name;
    const ext = dot > 0 ? name.slice(dot) : "";
    const taken = new Set(items.map((i) => i.name.toLowerCase()));
    for (let n = 2; n < 100; n++) {
      const candidate = `${base} (${n})${ext}`;
      if (!taken.has(candidate.toLowerCase())) return candidate;
    }
    return `${base} (${Date.now()})${ext}`;
  };

  /** Одна попытка загрузки. null — прервано пользователем или уже сообщили об ошибке. */
  const putFiles = async (
    files: File[],
    overwrite: boolean,
  ): Promise<{ saved: number; conflicts: File[] } | null> => {
    const ac = new AbortController();
    uploadAbort.current = ac;
    setUploading(true);
    setUploadPct(0);
    try {
      // Сервер отвечает {files, failed[]}: раньше тост считал ВЫБРАННЫЕ файлы
      // и говорил «Загружено 5», когда на диск легли два.
      const res: any = await uploadFiles(
        currentPath,
        files,
        (pct: number) => setUploadPct(pct),
        { signal: ac.signal, resumable: true, overwrite },
      );
      const failed: { name: string; code?: string }[] = Array.isArray(res?.failed) ? res.failed : [];
      const clash = new Set(failed.filter((f) => f.code === "already_exists").map((f) => f.name));
      const other = failed.filter((f) => f.code !== "already_exists");
      const saved = Array.isArray(res?.files) ? res.files.length : files.length - failed.length;
      if (other.length) {
        hapticError();
        toastError(t("toast.uploadedPartial", { n: saved, failed: other.length, name: other[0].name }));
      }
      return { saved, conflicts: files.filter((f) => clash.has(f.name)) };
    } catch (e: any) {
      if (ac.signal.aborted || e?.code === "aborted") return null;
      // already_exists — не ошибка, а вопрос: ни один файл не записан, спросим.
      if (e?.code === "already_exists") return { saved: 0, conflicts: files };
      hapticError();
      toastError(mapApiError(e));
      return null;
    } finally {
      if (uploadAbort.current === ac) uploadAbort.current = null;
      if (!ac.signal.aborted) {
        setUploading(false);
        setUploadPct(null);
      }
    }
  };

  /**
   * Загрузка с честным вопросом при совпадении имён (находка N107). Раньше файл
   * на ПК затирался молча: «Загружено 1 файл(ов)» — и работа человека исчезала
   * без вопроса и без возможности вернуть.
   */
  const uploadPicked = async (picked: File[]) => {
    const first = await putFiles(picked, false);
    if (!first) return;
    let saved = first.saved;
    if (first.conflicts.length > 0) {
      const many = first.conflicts.length > 1;
      const renamed = first.conflicts.map(
        (f) => new File([f], copyName(f.name), { type: f.type }),
      );
      const answer = await ask({
        title: t("files.uploadExistsTitle", { name: first.conflicts[0].name }),
        message: t("files.uploadExistsMsg")
          + (many ? " " + t("files.uploadExistsMany", { n: first.conflicts.length - 1 }) : ""),
        options: [
          { id: "replace", label: t("files.replace"), danger: true },
          {
            id: "copy",
            label: many ? t("files.keepCopies") : t("files.saveAsCopy", { name: renamed[0].name }),
          },
        ],
      });
      if (answer === "replace") saved += (await putFiles(first.conflicts, true))?.saved ?? 0;
      else if (answer === "copy") saved += (await putFiles(renamed, false))?.saved ?? 0;
    }
    if (saved > 0) {
      hapticSuccess();
      toastSuccess(t("toast.uploaded", { n: saved }));
    }
    // Заливка меняет занятое место: сбрасываем кэш тома ДО loadDir, чтобы
    // он перечитал disk-info сам (иначе строка показывала бы старую цифру).
    diskCache.current.delete(volumeKey(currentPath));
    void loadDir(currentPath);
  };

  const handleUpload = async () => {
    const input = document.createElement("input");
    input.type = "file";
    input.multiple = true;
    input.onchange = () => {
      if (!input.files?.length) return;
      void uploadPicked(Array.from(input.files));
    };
    input.click();
  };

  // ── Удаление и его отмена ─────────────────────────────────────
  // Сервер отвечает не «ok», а ЧЕМ именно закончилось удаление: ушло в корзину
  // ОС (trashed) и чем это вернуть (restore_path — только там, где вернуть
  // можно обычным перемещением). Старый агент этих полей не шлёт — тогда всё
  // честно считается безвозвратным, как оно и было.
  type DeleteOutcome = { trashed: boolean; restore: string; from: string };

  const deleteOne = async (item: FileItem): Promise<DeleteOutcome> => {
    const res: any = item.is_dir ? await deleteDir(item.path) : await deleteFile(item.path);
    return {
      trashed: !!res?.trashed,
      restore: typeof res?.restore_path === "string" ? res.restore_path : "",
      from: item.path,
    };
  };

  /** «Отменить» в тосте: возвращаем удалённое туда, откуда его взяли. */
  const undoDelete = async (undo: DeleteOutcome[]) => {
    let back = 0;
    for (const u of undo) {
      try {
        await renameFile(u.restore, u.from);
        back++;
      } catch { /* посчитаем и скажем, сколько не вернулось */ }
    }
    if (back > 0) hapticSuccess();
    if (back === undo.length) toastSuccess(t("files.restored", { n: back }));
    else toastError(t("files.restoreFailed", { n: undo.length - back }));
    diskCache.current.delete(volumeKey(currentPath));
    void loadDir(currentPath);
  };

  /**
   * Тост об удалении. «Удалено» одинаковым текстом и для корзины, и для
   * безвозвратного удаления — обещание возврата, которого может не быть:
   * поэтому исходы называются разными словами, а «Отменить» появляется
   * ровно тогда, когда действительно есть что вернуть.
   */
  const reportDeleted = (outcomes: DeleteOutcome[], name: string) => {
    const trashed = outcomes.filter((o) => o.trashed).length;
    const undo = outcomes.filter((o) => o.restore);
    const n = outcomes.length;
    let msg: string;
    if (n === 1) {
      msg = t(trashed === 1 ? "files.trashedOne" : "files.deletedForeverOne", { name });
    } else if (trashed === n) {
      msg = t("files.trashedMany", { n });
    } else if (trashed === 0) {
      msg = t("files.deletedForeverMany", { n });
    } else {
      msg = t("files.deletedMixed", { n });
    }
    const opts = undo.length > 0
      ? { action: { label: t("files.undoDelete"), onClick: () => void undoDelete(undo) } }
      : undefined;
    if (trashed > 0) toastSuccess(msg, opts);
    // Безвозвратное удаление — не повод для «✓ готово»: это уведомление, и
    // читать его дают дольше обычного успеха.
    else toast(msg, "info", { durationMs: 5000 });
  };

  const handleDelete = async (item: FileItem) => {
    // Для ФАЙЛА удержания HoldButton (900 мс) достаточно — второй модальный
    // confirm избыточен, а сама кнопка живёт теперь в меню «⋯ Ещё», а не в
    // прокручиваемой ленте (палец на ленте больше не удаляет файл). Для ПАПКИ
    // вопрос остаётся: удаление рекурсивное, поэтому сначала считаем, что
    // именно исчезнет, и называем цифры.
    if (item.is_dir) {
      let what = item.name;
      try {
        const st = await getDirStat(item.path);
        if (st.files > 0 || st.dirs > 0) {
          what = t("files.deleteDirStat", {
            name: item.name,
            files: st.files + (st.truncated ? "+" : ""),
            size: humanSize(st.bytes),
          });
        }
      } catch {
        // Не смогли посчитать — спрашиваем без цифр, но спрашиваем.
      }
      if (!(await tgConfirm(t("confirm.deleteDir", { what }), {
        danger: true, confirmText: t("confirm.btn.delete"),
      }))) return;
    }
    try {
      const outcome = await deleteOne(item);
      haptic("medium");
      reportDeleted([outcome], item.name);
      setContextItem(null);
      diskCache.current.delete(volumeKey(currentPath));
      loadDir(currentPath);
    } catch (e: any) {
      hapticError();
      toastError(mapApiError(e));
    }
  };

  // ── Групповой выбор ───────────────────────────────────────────
  const selectedItems = useMemo(
    () => items.filter((i) => selected.includes(i.path)),
    [items, selected],
  );

  const exitSelect = () => { setSelectMode(false); setSelected([]); };

  const toggleSelect = (path: string) => {
    haptic();
    setSelected((cur) => (cur.includes(path) ? cur.filter((p) => p !== path) : [...cur, path]));
  };

  const startSelect = (item?: FileItem) => {
    setContextItem(null);
    setSelectMode(true);
    setSelected(item ? [item.path] : []);
  };

  // Долгий тап включает выбор. Порог сдвига 10 px обязателен: без него палец,
  // начавший прокрутку с медленного движения, попадал бы в режим выбора.
  const longPress = useRef<{ timer: number; x: number; y: number } | null>(null);
  const suppressClick = useRef(false);

  const cancelLongPress = () => {
    if (longPress.current) {
      window.clearTimeout(longPress.current.timer);
      longPress.current = null;
    }
  };

  const startLongPress = (e: React.PointerEvent, item: FileItem) => {
    // Новое касание — новая история: если click после прошлого удержания так и
    // не пришёл (браузер его отменил), флаг не должен съесть следующий тап.
    suppressClick.current = false;
    if (e.pointerType === "mouse") return; // на мыши для этого есть правый клик
    if (selectMode) return; // в режиме выбора отметку ставит обычный тап
    cancelLongPress();
    const x = e.clientX;
    const y = e.clientY;
    const timer = window.setTimeout(() => {
      longPress.current = null;
      haptic("medium");
      suppressClick.current = true; // click после удержания уже не нужен
      startSelect(item);
    }, 500);
    longPress.current = { timer, x, y };
  };

  const moveLongPress = (e: React.PointerEvent) => {
    const lp = longPress.current;
    if (!lp) return;
    if (Math.abs(e.clientX - lp.x) > 10 || Math.abs(e.clientY - lp.y) > 10) cancelLongPress();
  };

  /** Удаление пачки. Здесь спрашиваем всегда: одним «да» исчезает N объектов. */
  const deleteSelected = async () => {
    const list = selectedItems;
    if (list.length === 0) return;
    if (!(await tgConfirm(t("files.deleteManyConfirm", { n: list.length }), {
      danger: true, confirmText: t("confirm.btn.delete"),
    }))) return;
    const done: DeleteOutcome[] = [];
    const failed: string[] = [];
    for (const it of list) {
      try { done.push(await deleteOne(it)); } catch { failed.push(it.name); }
    }
    if (done.length > 0) {
      haptic("medium");
      // Имя в тосте нужно только когда объект остался один — и это должен быть
      // именно УДАЛЁННЫЙ объект, а не первый в выборе.
      const firstName = list.find((it) => it.path === done[0].from)?.name ?? list[0].name;
      reportDeleted(done, firstName);
    }
    if (failed.length > 0) {
      hapticError();
      toastError(t("files.deleteFailedN", { n: failed.length, name: failed[0] }));
    }
    exitSelect();
    diskCache.current.delete(volumeKey(currentPath));
    void loadDir(currentPath);
  };

  /** Скачивание пачки — по одному файлу подряд, с теми же вопросами о размере. */
  const downloadSelected = async () => {
    const files = selectedItems.filter((i) => !i.is_dir);
    if (files.length === 0) {
      toastError(t("files.selectNoFiles"));
      return;
    }
    for (const f of files) await handleDownload(f);
    exitSelect();
  };

  const handleSendTelegram = async (item: FileItem) => {
    setSending(item.path);
    haptic();
    try {
      await sendToTelegram(item.path, item.name);
      hapticSuccess();
      toastSuccess(t("toast.sentToTelegram"));
    } catch (e: any) {
      hapticError();
      toastError(mapApiError(e));
    }
    setSending(null);
  };

  const handleCreateFolder = async () => {
    if (!newFolderName.trim()) return;
    const sep = currentPath.includes("/") ? "/" : "\\";
    const end = currentPath.endsWith(sep) ? "" : sep;
    const fullPath = currentPath + end + newFolderName.trim();
    try {
      await mkDir(fullPath);
      hapticSuccess();
      toastSuccess(t("toast.folderCreated", { name: newFolderName.trim() }));
      setShowNewFolder(false);
      setNewFolderName("");
      loadDir(currentPath);
    } catch (e: any) {
      hapticError();
      toastError(mapApiError(e));
    }
  };

  const handleRename = async () => {
    if (!showRename || !renameTo.trim()) return;
    const sep = showRename.path.includes("/") ? "/" : "\\";
    const lastSep = showRename.path.lastIndexOf(sep);
    const dir = showRename.path.substring(0, lastSep + 1);
    const newPath = dir + renameTo.trim();
    try {
      await renameFile(showRename.path, newPath);
      hapticSuccess();
      toastSuccess(t("toast.renamed", { name: renameTo.trim() }));
      setShowRename(null);
      setRenameTo("");
      setContextItem(null);
      restoreFileRowFocus();
      loadDir(currentPath);
    } catch (e: any) {
      hapticError();
      toastError(mapApiError(e));
    }
  };

  /** Путь папки в буфер. Живёт отдельной функцией — зовут её и из меню «⋯». */
  const copyCurrentPath = async () => {
    haptic();
    try {
      await navigator.clipboard.writeText(currentPath);
      toastSuccess(t("files.pathCopied"));
    } catch {
      // В части WebView запись в буфер запрещена без жеста/https — молчать
      // нельзя, иначе кнопка выглядит сломанной.
      toastError(t("files.pathCopyFailed"));
    }
  };

  /** Включить показ скрытых из пустого состояния («тут ничего нет» — ложь). */
  const enableHidden = () => {
    haptic();
    setShowHidden(true);
    showHiddenRef.current = true;
    try { localStorage.setItem(HIDDEN_KEY, "1"); } catch { /* quota */ }
    void loadDir(currentPath);
  };

  const handleGoPath = () => {
    if (pathInput.trim()) {
      loadDir(pathInput.trim());
      setShowPathInput(false);
    }
  };

  const handleNewSession = async () => {
    if (!sessionName.trim()) return;
    try {
      await createSession(sessionName.trim(), sessionAgent, currentPath);
      hapticSuccess();
      toastSuccess(t("toast.sessionCreatedNav", { name: sessionName.trim() }));
      setShowNewSession(false);
      setSessionName("");
      navigate("/");
    } catch (e: any) {
      hapticError();
      toastError(mapApiError(e));
    }
  };

  const handleSearch = async () => {
    const query = searchQuery.trim();
    if (query.length < 2) return;
    searchAbort.current?.abort();
    const ac = new AbortController();
    searchAbort.current = ac;
    setSearching(true);
    try {
      // Поиск ".env" должен находить скрытые файлы без отдельного похода в
      // сорт-бар: сам запрос с точки явно выражает намерение пользователя.
      const includeHidden = showHiddenRef.current || query.startsWith(".");
      if (includeHidden && !showHiddenRef.current) {
        setShowHidden(true);
        showHiddenRef.current = true;
        try { localStorage.setItem(HIDDEN_KEY, "1"); } catch { /* quota */ }
      }
      const d = await searchFiles(query, currentPath, {
        hidden: includeHidden,
        signal: ac.signal,
      });
      if (ac.signal.aborted) return;
      // Сервер может вернуть один путь дважды (пересекающиеся корни обхода).
      const seen = new Set<string>();
      setSearchResults(d.results.filter((r) => !seen.has(r.path) && seen.add(r.path)));
      setSearchMeta({
        truncated: d.truncated,
        timedOut: d.timed_out,
        scanned: d.scanned,
        elapsedMs: d.elapsed_ms,
      });
      setSearchReturn(null);
    } catch (e: any) {
      if (e?.code !== "aborted" && !ac.signal.aborted) {
        setSearchResults([]);
        setSearchMeta(null);
        toastError(mapApiError(e));
      }
    } finally {
      if (searchAbort.current === ac) searchAbort.current = null;
      if (!ac.signal.aborted) setSearching(false);
    }
  };

  const cancelSearch = () => {
    searchAbort.current?.abort();
    searchAbort.current = null;
    setSearching(false);
  };

  const openSearchResult = (item: FileItem) => {
    if (!searchResults) return;
    setSearchReturn({ query: searchQuery, results: searchResults, meta: searchMeta });
    setSearchResults(null);
    setSearchMeta(null);
    // Найденную ПАПКУ открываем саму: раньше открывался её родитель, и человек
    // оказывался на уровень выше того, что искал.
    if (item.is_dir) {
      setHighlightPath(null);
      void loadDir(item.path);
      return;
    }
    setHighlightPath(item.path);
    // Найденную строку доводит до экрана подсветка — своей прокруткой в начало
    // папки мы бы её сбили.
    skipScrollRef.current = true;
    void loadDir(parentOf(item.path));
  };

  const restoreSearchResults = () => {
    if (!searchReturn) return;
    setSearchQuery(searchReturn.query);
    setSearchResults(searchReturn.results);
    setSearchMeta(searchReturn.meta);
    setSearchReturn(null);
    setHighlightPath(null);
  };

  // Переносим СПИСОК, а не один объект: групповой выбор отдаёт сюда пачку, и
  // ради 12 файлов открывать шторку выбора папки 12 раз незачем.
  const [transfer, setTransfer] = useState<{ items: FileItem[]; mode: "move" | "copy" } | null>(null);
  // Пока идёт пачка, отчёт по каждому объекту молчит: три тоста подряд про
  // «скопирован» вытесняют друг друга и ни один не дочитывается. Итог — один.
  const bulkQuiet = useRef(false);
  // Не флаг, а описание операции: пока она идёт, на экране висит плашка
  // «Копирую «Фото 2026» → D:\Архив», а шторка выбора папки не закрывается.
  const [transferring, setTransferring] = useState<{
    name: string; dir: string; mode: "move" | "copy";
  } | null>(null);

  /** Перенос/копирование не мгновенны: обрыв по таймауту НЕ значит «не вышло». */
  const isTimeout = (code: unknown) => code === "timeout" || code === "pc_timeout";

  const reportCopy = (item: FileItem, res: FileTransferResult) => {
    if (bulkQuiet.current) return;
    hapticSuccess();
    // Сервер возвращает ФАКТИЧЕСКИЙ путь: «скопирован» без «куда» — половина
    // ответа, особенно когда копия легла внутрь одноимённой папки.
    const where = res.path ? parentOf(res.path) : "";
    if (res.skipped) {
      toast(t("files.copiedSkipped", { name: item.name, n: res.skipped }));
      return;
    }
    toastSuccess(where
      ? t("files.copiedTo", { name: item.name, path: where })
      : t("files.copied", { name: item.name }));
  };

  const reportMove = (item: FileItem, res: FileTransferResult) => {
    // Про оставшийся исходник молчать нельзя даже в пачке — это не «успех».
    if (bulkQuiet.current && res.source_removed !== false) return;
    if (res.source_removed === false) {
      // Данные уже на новом месте, но оригинал остался (ссылки/спецфайлы или
      // отказ удаления): молчать об этом нельзя — человек считает файл перенесённым.
      hapticError();
      toast(t("files.moveSourceKept", { name: item.name }));
      return;
    }
    hapticSuccess();
    const where = res.path ? parentOf(res.path) : "";
    toastSuccess(where
      ? t("files.movedTo", { name: item.name, path: where })
      : t("files.moved", { name: item.name }));
  };

  /** Цель занята. Для файла есть замена, для папки — только копия рядом/внутрь. */
  const askExists = async (item: FileItem, targetDir: string, mode: "move" | "copy"): Promise<boolean> => {
    const alt = copyName(item.name);
    const options: ChoiceOption[] = [];
    if (!item.is_dir) options.push({ id: "replace", label: t("files.replace"), danger: true });
    if (item.is_dir && mode === "copy") {
      options.push({ id: "inside", label: t("files.copyInside", { name: item.name }) });
    }
    options.push({
      id: "rename",
      label: t(mode === "move" ? "files.moveAsCopy" : "files.saveAsCopy", { name: alt }),
    });
    const answer = await ask({
      title: t("files.existsTitle", { name: item.name }),
      message: item.is_dir ? t("files.existsDirMsg") : t("files.existsFileMsg", { dir: targetDir }),
      options,
    });
    if (!answer) return false;
    if (mode === "copy") {
      if (answer === "replace") return runCopy(item, targetDir, { overwrite: true, retry: true });
      if (answer === "inside") {
        // Явное согласие на вложение: dst — сама одноимённая папка, сервер
        // положит копию внутрь неё под тем же именем.
        return runCopy(item, targetDir, { dst: joinPath(targetDir, item.name), retry: true });
      }
      return runCopy(item, targetDir, { dst: joinPath(targetDir, alt), retry: true });
    }
    if (answer === "replace") {
      return runMove(item, targetDir, joinPath(targetDir, item.name), { overwrite: true, retry: true });
    }
    return runMove(item, targetDir, joinPath(targetDir, alt), { retry: true });
  };

  /**
   * Копирование. dst по умолчанию — САМА папка-получатель: сервер добавит имя
   * источника один раз и проверит занятость итогового пути. Раньше клиент
   * подставлял имя сам, сервер добавлял его второй раз, и копия папки уезжала
   * в матрёшку D:\Бэкап\docs\docs без единого вопроса (находка N112).
   */
  const runCopy = async (
    item: FileItem,
    targetDir: string,
    opts: { overwrite?: boolean; dst?: string; retry?: boolean } = {},
  ): Promise<boolean> => {
    try {
      const res = await copyFile(item.path, opts.dst ?? targetDir, !!opts.overwrite);
      reportCopy(item, res);
      return true;
    } catch (e: any) {
      if (e?.code === "already_exists" && !opts.retry) return askExists(item, targetDir, "copy");
      if (isTimeout(e?.code)) {
        // Копирование идёт на ПК и после обрыва ответа — «Превышено время
        // ожидания» здесь было бы прямой ложью (а pc_timeout ещё и уводил в
        // «Компьютер не в сети» при работающем ПК).
        toast(t("files.transferSlowCopy"));
        return true;
      }
      hapticError();
      toastError(mapApiError(e));
      return false;
    }
  };

  /**
   * Перемещение. allow_copy:false просит сервер НЕ копировать между дисками
   * молча: на такой перенос он отвечает cross_device, и спрашиваем человека мы
   * (находка N109) — копия на другой том с последующим удалением исходника.
   */
  const runMove = async (
    item: FileItem,
    targetDir: string,
    dst: string,
    opts: { overwrite?: boolean; retry?: boolean; allowCopy?: boolean } = {},
  ): Promise<boolean> => {
    try {
      const res = await renameFile(item.path, dst, {
        overwrite: opts.overwrite,
        allowCopy: opts.allowCopy ?? false,
      });
      reportMove(item, res);
      return true;
    } catch (e: any) {
      if (e?.code === "cross_device") {
        const answer = await ask({
          title: t("files.crossDeviceTitle"),
          message: t("files.crossDeviceMsg", { name: item.name, dir: targetDir }),
          options: [{ id: "go", label: t("files.crossDeviceGo") }],
        });
        if (answer !== "go") return false;
        return runMove(item, targetDir, dst, { ...opts, allowCopy: true });
      }
      if (e?.code === "already_exists" && !opts.retry) return askExists(item, targetDir, "move");
      if (isTimeout(e?.code)) {
        toast(t("files.transferSlowMove"));
        return true;
      }
      hapticError();
      toastError(mapApiError(e));
      return false;
    }
  };

  /**
   * Возвращает false, когда переносить не начали или не получилось — тогда
   * шторка выбора папки остаётся открытой (раньше она закрывалась мгновенно, и
   * длинная операция выглядела как «ничего не произошло»).
   */
  const finishTransfer = async (targetDir: string): Promise<boolean> => {
    if (!transfer || transferring) return false;
    const { items: list, mode } = transfer;
    if (list.length === 0) return false;
    if (list.every((it) => joinPath(targetDir, it.name) === it.path)) {
      toastError(t("files.sameFolder"));
      return false;
    }
    const label = list.length === 1 ? list[0].name : t("files.nObjects", { n: list.length });
    setTransferring({ name: label, dir: targetDir, mode });
    bulkQuiet.current = list.length > 1;
    let done = 0;
    try {
      for (const item of list) {
        const dst = joinPath(targetDir, item.name);
        if (dst === item.path) continue; // «на месте» — не ошибка всей пачки
        const ok = mode === "move"
          ? await runMove(item, targetDir, dst)
          : await runCopy(item, targetDir);
        if (ok) done++;
        // Один отказ не должен уносить остальную пачку: продолжаем, а итог
        // назовём цифрой.
      }
      if (done === 0) return false;
      if (list.length > 1) {
        hapticSuccess();
        toastSuccess(t(mode === "move" ? "files.movedN" : "files.copiedN", {
          n: done, dir: targetDir,
        }));
      }
      setTransfer(null);
      setContextItem(null);
      exitSelect();
      diskCache.current.delete(volumeKey(currentPath));
      diskCache.current.delete(volumeKey(targetDir));
      await loadDir(currentPath);
      return true;
    } finally {
      bulkQuiet.current = false;
      setTransferring(null);
    }
  };

  /**
   * Закрепление папки (находка N114). Раньше кнопка молчала на любом исходе:
   * ни тоста об успехе, ни ошибки при переполнении лимита, ни признака, что
   * папка уже закреплена. Повторное нажатие теперь открепляет.
   */
  const pinned = !!currentPath && bookmarks.some((b) => b.path === currentPath);

  const handleTogglePin = async () => {
    if (!currentPath) return;
    haptic();
    const name = currentPath.split(/[/\\]/).filter(Boolean).pop() || currentPath;
    try {
      if (pinned) {
        await removeBookmark(currentPath);
        toast(t("files.favRemovedToast"));
      } else {
        await addBookmark(name, currentPath);
        hapticSuccess();
        toastSuccess(t("files.favToast"));
      }
      const d = await getBookmarks();
      setBookmarks(d.bookmarks);
    } catch (e: any) {
      hapticError();
      toastError(mapApiError(e));
    }
  };

  const handleRemoveBookmark = async (path: string) => {
    await removeBookmark(path);
    const d = await getBookmarks();
    setBookmarks(d.bookmarks);
  };

  /**
   * Убрать папку из избранного с плитки. Красный ✕ 28×28 занимал четверть
   * плитки и стоял наготове на КАЖДОЙ: самое опасное действие раздела было и
   * самым заметным, а промахнуться по нему было легче, чем попасть в саму
   * папку. Приём взят тот, что уже живёт на этом экране: долгое удержание на
   * телефоне (так же включается выбор нескольких файлов) и правый клик на
   * мыши (так же открываются действия над строкой). Безопасным его делает
   * вопрос — тот же самый, что был у крестика. Путь без жестов тоже остался и
   * виден глазами: открыть папку и нажать ★ в шапке.
   */
  const askRemoveBookmark = async (bm: Bookmark) => {
    if (!(await tgConfirm(t("files.favRemoveConfirm", { name: bm.name }), {
      danger: true, confirmText: t("confirm.btn.delete"),
    }))) return;
    try {
      await handleRemoveBookmark(bm.path);
      toast(t("files.favRemovedToast"));
    } catch (e: any) {
      hapticError();
      toastError(mapApiError(e));
    }
  };

  const startBookmarkPress = (e: React.PointerEvent, bm: Bookmark) => {
    // Те же правила, что у строк списка: мышь сюда не попадает (для неё правый
    // клик), сдвиг пальца больше 10 px отменяет удержание — иначе прокрутка
    // экрана заканчивалась бы вопросом об удалении.
    suppressClick.current = false;
    if (e.pointerType === "mouse") return;
    cancelLongPress();
    const x = e.clientX;
    const y = e.clientY;
    const timer = window.setTimeout(() => {
      longPress.current = null;
      haptic("medium");
      suppressClick.current = true; // click после удержания открыл бы папку
      void askRemoveBookmark(bm);
    }, 500);
    longPress.current = { timer, x, y };
  };

  const handlePreview = async (item: FileItem) => {
    haptic();
    try {
      const data = await previewFile(item.path);
      setPreview(data);
      setPreviewItem(item);
    } catch (e: any) {
      toastError(mapApiError(e));
      restoreFileRowFocus();
    }
  };

  // Скачивание идёт кусками (api.ts): большой файл одним запросом рвал
  // управляющий сокет ПК. Отсюда — процент и отмена вместо экрана, который
  // «просто висит».
  const [dl, setDl] = useState<{ path: string; pct: number } | null>(null);
  const dlAbort = useRef<AbortController | null>(null);

  const cancelDownload = () => {
    dlAbort.current?.abort();
    dlAbort.current = null;
    setDl(null);
  };

  const handleDownload = async (item: FileItem) => {
    haptic();
    const size = item.size ?? 0;
    // Крупный файл: трафик тратится задолго до того, как выяснится, что телефон
    // его не сохранил. Спрашиваем ДО старта и называем размер (находка N110).
    if (size >= BIG_DOWNLOAD) {
      const options: ChoiceOption[] = [];
      // Доставку ботом предлагаем только в пределах лимита релея — иначе это
      // обещание, которое заведомо не выполнится.
      if (botAvailable && size <= TG_FILE_LIMIT) {
        options.push({ id: "tg", label: t("files.sendByBot") });
      }
      options.push({ id: "pc", label: t("files.copyOnPc") });
      options.push({ id: "go", label: t("files.bigDownloadAnyway") });
      const answer = await ask({
        title: t("files.bigDownloadTitle", { size: humanSize(size) }),
        message: t("files.bigDownloadMsg", { size: humanSize(size) }),
        options,
      });
      if (answer === "tg") { await handleSendTelegram(item); return; }
      if (answer === "pc") { setTransfer({ items: [item], mode: "copy" }); return; }
      if (answer !== "go") return;
    }
    const ac = new AbortController();
    dlAbort.current = ac;
    setDl({ path: item.path, pct: 0 });
    try {
      const blob = await downloadBlob(item.path, {
        size: item.size ?? undefined,
        mtime: item.modified ?? undefined,
        signal: ac.signal,
        onProgress: (done, total) => {
          if (total > 0) setDl({ path: item.path, pct: Math.round((done / total) * 100) });
        },
      });
      const how = await saveBlob(blob, item.name);
      // "failed" — файла на телефоне нет (мобильный Telegram теряет blob молча).
      // Раньше здесь была только вибрация успеха: ни файла, ни тоста, ни ошибки.
      if (how === "failed") {
        hapticError();
        if (botAvailable && size <= TG_FILE_LIMIT) {
          const answer = await ask({
            title: t("files.saveFailedTgTitle"),
            message: t("files.saveFailedTgMsg", { name: item.name }),
            options: [{ id: "tg", label: t("files.sendByBot") }],
          });
          if (answer === "tg") { await handleSendTelegram(item); return; }
        }
        toastError(t("files.saveFailed"));
        return;
      }
      hapticSuccess();
      toastSuccess(how === "shared"
        ? t("download.shared", { name: item.name })
        : t("download.started", { name: item.name }));
    } catch (e: any) {
      // Отмену пользователь инициировал сам — молчим, как и при отмене шаринга.
      if (!ac.signal.aborted && !isShareCancel(e)) toastError(mapApiError(e));
    } finally {
      if (dlAbort.current === ac) dlAbort.current = null;
      setDl((cur) => (cur?.path === item.path ? null : cur));
    }
  };

  // Type-to-filter the current folder instantly; Enter / 🔍 still runs the
  // deep server-side search across subfolders.
  const filterQ = searchQuery.trim().toLowerCase();
  const visibleItems = filterQ && !searchResults
    ? items.filter((i) => i.name.toLowerCase().includes(filterQ))
    : items;

  // Порядок уже применён сервером ко ВСЕМ записям папки — трогать его нельзя,
  // иначе «первые 2000» опять перестанут соответствовать кнопке сортировки.
  // Сортируем сами только при разговоре со старым агентом (эха sort нет).
  const sortedItems = serverSort === sortBy
    ? visibleItems
    : [...visibleItems].sort((a, b) => {
      // Folders always first
      if (a.is_dir !== b.is_dir) return a.is_dir ? -1 : 1;
      if (sortBy === "name") return a.name.localeCompare(b.name, "ru");
      if (sortBy === "date") return (b.modified || 0) - (a.modified || 0);
      if (sortBy === "size") return (b.size || 0) - (a.size || 0);
      return 0;
    });

  useEscape(showNewFolder, () => setShowNewFolder(false));
  useEscape(!!showRename, closeRename);
  useEscape(showNewSession, () => setShowNewSession(false));
  useEscape(!!preview, closePreview);
  useEscape(toolsMenu && !choice, () => setToolsMenu(false));
  useEscape(selMenu && !choice, () => setSelMenu(false));
  useEscape(!!contextItem && !showRename && !preview && !choice, () => setContextItem(null));

  // ── Просмотр: честный разбор того, что прислал сервер (находка N115) ──
  // В текстовую ветку сервер пускает ЛЮБОЙ файл меньше 500 КБ, поэтому «это не
  // текст» распознаём по содержимому: нулевые байты и мусорные U+FFFD (в них
  // Go превращает не-UTF-8 при сериализации JSON).
  const previewText = preview?.type === "text" ? (preview.content || "") : "";
  const previewLooksBinary = useMemo(() => {
    if (!previewText) return false;
    const head = previewText.slice(0, 4000);
    if (head.includes(NUL_CHAR)) return true;
    let bad = 0;
    for (const ch of head) if (ch === REPLACEMENT_CHAR) bad++;
    return bad > Math.max(8, head.length * 0.02);
  }, [previewText]);
  // Сервер режет текст по 100 000 БАЙТ и об обрезке не сообщает. Считаем сами:
  // прочитанное короче файла — значит показан не весь текст. Заодно убираем
  // хвостовой обломок символа: срез по байтам рубит руну посередине.
  const previewShown = useMemo(() => {
    let out = previewText;
    while (out.endsWith(REPLACEMENT_CHAR)) out = out.slice(0, -1);
    return out;
  }, [previewText]);
  const previewTruncated = useMemo(() => {
    if (!previewText) return false;
    const bytes = new TextEncoder().encode(previewText).length;
    const size = previewItem?.size ?? preview?.size ?? 0;
    return size > 0 ? bytes < size : bytes >= 100000;
  }, [previewText, previewItem, preview]);

  const pathSegments = currentPath.split(/[/\\]/).filter(Boolean);

  // Найденные папки с кодом за вычетом избранного: одна и та же папка стояла на
  // экране дважды — человек сам положил её в избранное, а поиск проектов нашёл
  // её же и показал ещё раз. Избранное — выбор человека, поэтому лишней
  // считается подсказка, а не она.
  const foundProjects = useMemo(() => {
    const known = new Set(bookmarks.map((b) => samePathKey(b.path)));
    return projects.filter((p) => !known.has(samePathKey(p.path)));
  }, [projects, bookmarks]);

  // В «Быстром доступе» нет ни одной плитки: раньше экран рисовал заголовок
  // «Быстрый доступ» над пустотой — при выключенном ПК это и было всё, что
  // человек видел. Теперь пустой блок не показываем никогда, а вместо него —
  // честное состояние (офлайн / ошибка / «папок нет»).
  const quickEmpty = quickPaths.length === 0 && bookmarks.length === 0
    && recentFolders.length === 0 && foundProjects.length === 0;

  // ПК не в сети: пока экран ещё ничего не показал, ведущим считаем признак
  // связи (он приходит мгновенно), а не незавершённый запрос. Как только папка
  // прочитана (phase «ok»), правду снова говорит результат запроса — иначе
  // поздний кадр обрыва накрыл бы уже открытый список файлов.
  const pcOffline = load.offline || (linkOffline && load.phase !== "ok");
  // Спиннер имеет право на экран, только если ждать действительно есть чего.
  const waiting = loading && (retrying || !pcOffline);

  const offlineState = (
    <OfflineState
      onRetry={retryAll}
      onDevices={getMode() === "cloud" ? () => navigate("/infrastructure") : undefined}
    />
  );

  // Ошибка, не связанная с выключенным ПК (нет прав, путь недоступен, отказ
  // релея): называем причину человеческим текстом из useLoadState и даём повтор.
  const errorState = (
    <div className="empty">
      <div className="empty-icon">{"⚠️"}</div>
      <div className="empty-title">{t("files.loadFailed")}</div>
      {load.message && <div className="empty-desc">{load.message}</div>}
      <div className="empty-actions">
        <button className="btn btn-primary" onClick={retryAll}>{t("conn.retry")}</button>
      </div>
    </div>
  );

  // В Telegram сохранение файла на телефон ненадёжно, а доставка ботом работает
  // всегда — поэтому там «Отправить в Telegram» стоит перед «Скачать» (N51).
  const telegramFirst = !!getTelegram()?.initData;

  const ctxDownloadBtn = contextItem && !contextItem.is_dir ? (
    dl && dl.path === contextItem.path ? (
      // Идёт скачивание: показываем процент и даём прервать — на 200 МБ по
      // мобильной сети экран иначе выглядит зависшим.
      <button className="folder-menu-item" onClick={cancelDownload}>
        {"⏳"} {dl.pct}% {"·"} {t("modal.cancel")}
      </button>
    ) : (
      <button className="folder-menu-item" onClick={() => handleDownload(contextItem)} disabled={!!dl}>
        {"⬇️"} {t("files.downloadToPhone")}
      </button>
    )
  ) : null;

  const ctxTelegramBtn = contextItem && !contextItem.is_dir && botAvailable ? (
    <button
      className="folder-menu-item"
      onClick={() => handleSendTelegram(contextItem)}
      disabled={sending === contextItem.path}
    >
      {sending === contextItem.path ? "⏳" : "📤"} {t("files.sendTelegramBtn")}
    </button>
  ) : null;

  return (
    <div className="page">
      {/* Header */}
      <div className="page-header">
        <div className="page-header-context">
          <h1 style={{ fontSize: 18 }}>{t("files.title")}</h1>
          <DeviceChip />
        </div>
        {/* В шапке остаются два действия: найти и справка. Раньше их было
            четыре — ввод пути, звезда, «все папки» и «?». Звезда и возврат к
            «Местам» переехали в строку пути (там их и ищут), ручной путь — в
            меню «Ещё». Имена — по-русски и вслух: скринридер в русском разделе
            произносил «Enter path» и «Quick access». */}
        {showQuick ? (
          /* \u041d\u0430 \u0432\u0438\u0442\u0440\u0438\u043d\u0435 \u0438\u0441\u043a\u0430\u0442\u044c \u043d\u0435\u0447\u0435\u0433\u043e \u2014 \u0442\u0430\u043c \u043d\u0435\u0442 \u043e\u0442\u043a\u0440\u044b\u0442\u043e\u0439 \u043f\u0430\u043f\u043a\u0438. \u0417\u0430\u0442\u043e \u043d\u0443\u0436\u043d\u0430
             \u0434\u0432\u0435\u0440\u044c \u00ab\u043e\u0442\u043a\u0440\u044b\u0442\u044c \u043f\u043e \u043f\u043e\u043b\u043d\u043e\u043c\u0443 \u043f\u0443\u0442\u0438\u00bb: \u043e\u043d\u0430 \u0431\u044b\u043b\u0430 \u0432 \u0448\u0430\u043f\u043a\u0435 \u0432\u0441\u0435\u0433\u0434\u0430, \u0438 \u0443\u0431\u0440\u0430\u0442\u044c
             \u0435\u0451 \u0432\u043c\u0435\u0441\u0442\u0435 \u0441 \u043e\u0441\u0442\u0430\u043b\u044c\u043d\u044b\u043c\u0438 \u0437\u043d\u0430\u0447\u043a\u0430\u043c\u0438 \u0437\u043d\u0430\u0447\u0438\u043b\u043e \u0431\u044b \u043e\u0441\u0442\u0430\u0432\u0438\u0442\u044c \u043b\u044e\u0434\u0435\u0439 \u0441
             \u043d\u0435\u0441\u0442\u0430\u043d\u0434\u0430\u0440\u0442\u043d\u044b\u043c\u0438 \u043f\u0443\u0442\u044f\u043c\u0438 (\u0441\u0435\u0442\u0435\u0432\u044b\u0435 \u0434\u0438\u0441\u043a\u0438, /var/log) \u0432\u043e\u0432\u0441\u0435 \u0431\u0435\u0437 \u0432\u0445\u043e\u0434\u0430. */
          <button
            className="icon-btn"
            onClick={() => {
              haptic();
              if (!showPathInput) setPathInput(currentPath);
              setShowPathInput(!showPathInput);
            }}
            aria-label={t("files.enterPathBtn")}
            title={t("files.enterPathBtn")}
            aria-pressed={showPathInput}
          >
            {"\u2328"}
          </button>
        ) : (
          <button
            className="icon-btn"
            onClick={() => { haptic(); setShowSearch((v) => !v); }}
            aria-label={t("files.searchHere")}
            title={t("files.searchHere")}
            aria-pressed={showSearch}
          >
            {"\ud83d\udd0d"}
          </button>
        )}
        {/* Звезда «в избранное» и возврат к «Местам» переехали в строку пути:
            они относятся к ОТКРЫТОЙ ПАПКЕ, а в шапке читались как действия над
            разделом целиком. Заодно шапка перестала быть рядом из четырёх
            безымянных значков. */}
        <button className="header-action" onClick={() => setShowHelp(true)} aria-label={t("help.title")}>
          {"?"}
        </button>
      </div>

      {/* Path input */}
      {showPathInput && (
        <div className="fm-path-input-bar">
          <input
            className="fm-path-input"
            value={pathInput}
            onChange={(e) => setPathInput(e.target.value)}
            placeholder={t("files.enterPath")}
            onKeyDown={(e) => e.key === "Enter" && handleGoPath()}
            autoFocus
          />
          <button className="btn btn-primary btn-sm" onClick={handleGoPath}>{t("generic.go")}</button>
        </div>
      )}

      {/* \u0421\u0442\u0440\u043E\u043A\u0430 \u043F\u0443\u0442\u0438 \u2014 \u0433\u043B\u0430\u0432\u043D\u044B\u0439 \u043D\u0430\u0432\u0438\u0433\u0430\u0446\u0438\u043E\u043D\u043D\u044B\u0439 \u044D\u043B\u0435\u043C\u0435\u043D\u0442 \u044D\u043A\u0440\u0430\u043D\u0430.
          \u0420\u0430\u043D\u044C\u0448\u0435 \u043D\u0430\u0434 \u0441\u043F\u0438\u0441\u043A\u043E\u043C \u0441\u0442\u043E\u044F\u043B\u0438 \u0427\u0415\u0422\u042B\u0420\u0415 \u043F\u043E\u043B\u043E\u0441\u044B \u043F\u043E\u0434\u0440\u044F\u0434 (\u043A\u0440\u043E\u0448\u043A\u0438, \u0434\u0438\u0441\u043A, \u043B\u0435\u043D\u0442\u0430
          \u0438\u043D\u0441\u0442\u0440\u0443\u043C\u0435\u043D\u0442\u043E\u0432, \u0441\u043E\u0440\u0442\u0438\u0440\u043E\u0432\u043A\u0430) \u043F\u043B\u044E\u0441 \u043F\u043E\u043B\u0435 \u043F\u043E\u0438\u0441\u043A\u0430: 291 px \u0438\u0437 844, \u0442\u043E \u0435\u0441\u0442\u044C
          \u0442\u0440\u0435\u0442\u044C \u044D\u043A\u0440\u0430\u043D\u0430 \u0434\u043E \u043F\u0435\u0440\u0432\u043E\u0433\u043E \u0444\u0430\u0439\u043B\u0430. \u0422\u0435\u043F\u0435\u0440\u044C \u00AB\u0432\u0432\u0435\u0440\u0445\u00BB \u0438 \u043F\u0443\u0442\u044C \u0436\u0438\u0432\u0443\u0442 \u0432 \u043E\u0434\u043D\u043E\u0439
          \u0441\u0442\u0440\u043E\u043A\u0435, \u043F\u0435\u0440\u0432\u0430\u044F \u043A\u0440\u043E\u0448\u043A\u0430 \u2014 \u00AB\u041C\u0435\u0441\u0442\u0430\u00BB (\u0432\u0438\u0442\u0440\u0438\u043D\u0430), \u0441\u043F\u0440\u0430\u0432\u0430 \u0437\u0432\u0435\u0437\u0434\u0430 \u0438\u0437\u0431\u0440\u0430\u043D\u043D\u043E\u0433\u043E. */}
      {!showQuick && currentPath && (
        <div className="fm-nav">
          {parentPath && parentPath !== currentPath && (
            <button
              className="fm-nav-up"
              onClick={() => loadDir(parentPath)}
              aria-label={t("files.up")}
              title={t("files.up")}
            >
              {"\u2191"}
            </button>
          )}
          <div className="fm-crumbs" ref={breadcrumbsRef}>
            {/* \u0414\u0432\u0435\u0440\u044C \u043D\u0430 \u0432\u0438\u0442\u0440\u0438\u043D\u0443 \u0441\u0442\u043E\u0438\u0442 \u0442\u0430\u043C, \u0433\u0434\u0435 \u0435\u0439 \u043C\u0435\u0441\u0442\u043E, \u2014 \u0432 \u043D\u0430\u0447\u0430\u043B\u0435 \u043F\u0443\u0442\u0438.
                \u041F\u0440\u0435\u0436\u043D\u044F\u044F \u043A\u043D\u043E\u043F\u043A\u0430 \u00AB\u0441\u043F\u0438\u0441\u043A\u043E\u043C\u00BB \u0432 \u0448\u0430\u043F\u043A\u0435 \u0437\u0432\u0430\u043B\u0430\u0441\u044C \u00AB\u0412\u0441\u0435 \u043F\u0430\u043F\u043A\u0438\u00BB \u0438 \u043D\u0435
                \u0447\u0438\u0442\u0430\u043B\u0430\u0441\u044C \u043A\u0430\u043A \u00AB\u0432\u0435\u0440\u043D\u0443\u0442\u044C\u0441\u044F \u043A \u043C\u0435\u0441\u0442\u0430\u043C\u00BB. */}
            <button
              className="fm-crumb fm-crumb-places"
              onClick={() => { haptic(); setShowQuick(true); setContextItem(null); }}
            >
              {t("files.places")}
            </button>
            {pathSegments.map((seg, i) => {
              const sep = currentPath.includes("/") ? "/" : "\\";
              const clickPath = pathSegments.slice(0, i + 1).join(sep);
              const resolved = i === 0 && sep === "\\" ? clickPath + sep : clickPath;
              return (
                <span key={i} className="fm-crumb-wrap">
                  <span className="fm-sep">{"\u203A"}</span>
                  <button className="fm-crumb" onClick={() => loadDir(resolved)}>{seg}</button>
                </span>
              );
            })}
          </div>
          <button
            className={`fm-nav-star${pinned ? " on" : ""}`}
            onClick={() => void handleTogglePin()}
            aria-label={pinned ? t("files.favAdded") : t("files.favAdd")}
            title={pinned ? t("files.favAdded") : t("files.favAdd")}
            aria-pressed={pinned}
          >
            <IconStar size={19} filled={pinned} />
          </button>
        </div>
      )}

      {/* Диск. Раньше полоса стояла над КАЖДОЙ папкой и краснела при 90% —
          тревожный цвет ради справки, которую никто не спрашивал. Теперь цифра
          живёт в меню «Ещё», а строкой на экран выходит только настоящее
          предупреждение: места почти нет. */}
      {!showQuick && disk && disk.total > 0 && disk.used / disk.total >= 0.9 && (
        <div className="fm-disk is-low">
          <span className="fm-disk-bar">
            <span
              className="fm-disk-fill"
              style={{ width: `${Math.min(100, Math.max(0, (disk.used / disk.total) * 100))}%` }}
            />
          </span>
          <span className="fm-disk-text">
            {t("files.diskFree", { free: humanSize(disk.free), total: humanSize(disk.total) })}
          </span>
        </div>
      )}

      <div className="page-content">
        {/* Быстрый доступ. Пока плиток нет вовсе — вместо пустого блока состояние
            экрана: спиннер, «Компьютер не в сети» с повтором, ошибка или
            «папок нет» со входом в ручной путь. */}
        {showQuick && quickEmpty && (
          waiting ? (
            <div className="loading-center">
              <div className="spinner" />
            </div>
          ) : pcOffline ? (
            /* Честный ответ сразу, а не через 6 секунд: про выключенный ПК
               живой канал знает раньше, чем упадёт запрос списка папок. */
            offlineState
          ) : load.phase === "error" ? (
            errorState
          ) : (
            <div className="empty">
              <div className="empty-icon">{"⌨"}</div>
              <div className="empty-title">{t("files.quickEmptyTitle")}</div>
              <div className="empty-desc">{t("files.quickEmptyHint")}</div>
              <div className="empty-actions">
                <button className="btn btn-primary" onClick={() => { haptic(); setShowPathInput(true); }}>
                  {t("files.enterPathBtn")}
                </button>
              </div>
            </div>
          )
        )}
        {showQuick && !quickEmpty && (
          <div className={`fm-quick fm-places-${placesView}`}>
            {(bookmarks.length + recentFolders.length + foundProjects.length > 1) && (
              <div className="fm-places-view" role="group" aria-label={t("files.placesView")}>
                <button className="btn btn-secondary btn-sm" aria-pressed={placesView === "list"} onClick={() => changePlacesView("list")}>
                  {t("files.placesList")}
                </button>
                <button className="btn btn-secondary btn-sm" aria-pressed={placesView === "tiles"} onClick={() => changePlacesView("tiles")}>
                  {t("files.placesTiles")}
                </button>
              </div>
            )}
            {/* Заголовок и сетку рисуем только с плитками: «Быстрый доступ» над
                пустым местом — ровно то, что видел человек при выключенном ПК. */}
            {quickPaths.length > 0 && (
              <>
                <div className="fm-section-title">{t("files.quickAccess")}</div>
                <div className="fm-quick-grid">
                  {quickPaths.map((qp) => (
                    <button key={`${qp.name}:${qp.path}`} className="fm-quick-btn" onClick={() => { haptic(); loadDir(qp.path); }}>
                      <QuickIcon>{quickIcon(qp.name)}</QuickIcon>
                      <span className="fm-quick-label">{folderLabel(qp.name)}</span>
                    </button>
                  ))}
                </div>
              </>
            )}
            {bookmarks.length > 0 && (
              <>
                {/* Одно слово на все места: кнопка, тост, раздел и вопрос об
                    удалении звали одно и то же «закрепить», «в избранное»,
                    «⭐» и «закладки» — человек искал папку не там. */}
                <div className="fm-section-title" style={{ marginTop: 16 }}>{t("files.favSection")}</div>
                <div className="fm-quick-grid fm-place-items">
                  {bookmarks.map((bm) => (
                    // Знак избранного один на весь экран — та же ★, что в
                    // шапке папки; булавка была ещё одним именем для того же.
                    <div key={bm.path} className="fm-place-item">
                    <button
                      className="fm-quick-btn"
                      onClick={() => {
                        // Клик, пришедший следом за удержанием, открыл бы
                        // папку поверх вопроса об удалении.
                        if (suppressClick.current) { suppressClick.current = false; return; }
                        haptic();
                        loadDir(bm.path);
                      }}
                      onPointerDown={(e) => startBookmarkPress(e, bm)}
                      onPointerMove={moveLongPress}
                      onPointerUp={cancelLongPress}
                      onPointerLeave={cancelLongPress}
                      onPointerCancel={cancelLongPress}
                      onContextMenu={(e) => { e.preventDefault(); void askRemoveBookmark(bm); }}
                    >
                      <QuickIcon>{"\u2605"}</QuickIcon>
                      <span className="fm-place-text">
                        <span className="fm-quick-label">{bm.name}</span>
                        <span className="fm-place-path">{bm.path}</span>
                      </span>
                    </button>
                    <button className="fm-place-remove" aria-label={t("files.removeFavorite", { name: bm.name })}
                      onClick={() => { void askRemoveBookmark(bm); }}>
                      <IconStar size={18} />
                    </button>
                    </div>
                  ))}
                </div>
              </>
            )}
            {recentFolders.length > 0 && (
              <>
                <div className="fm-section-title" style={{ marginTop: 16 }}>{t("folder.tab.recent")}</div>
                <div className="fm-quick-grid fm-place-items">
                  {recentFolders.map((folder) => (
                    <button key={folder.path} className="fm-quick-btn" onClick={() => { haptic(); loadDir(folder.path); }}>
                      <QuickIcon>{"\uD83D\uDD52"}</QuickIcon>
                      <span className="fm-place-text">
                        <span className="fm-quick-label">{folder.name}</span>
                        <span className="fm-place-path">{folder.path}</span>
                      </span>
                    </button>
                  ))}
                </div>
              </>
            )}
            {foundProjects.length > 0 && (
              <>
                {/* «Проекты» человек не заводил — их нашёл сам компьютер, и
                    заголовок теперь так и говорит. Раньше раздел читался как
                    что-то, что пользователь когда-то создал и забыл, а рядом с
                    «Избранным» (которое он и правда заводил) это сбивало. */}
                <div className="fm-section-title" style={{ marginTop: 16 }}>{t("files.projectsFound")}</div>
                <div className="fm-quick-grid fm-place-items">
                  {foundProjects.map((project) => (
                    <button key={project.path} className="fm-quick-btn" onClick={() => { haptic(); loadDir(project.path); }}>
                      <QuickIcon><IconFolder size={26} /></QuickIcon>
                      <span className="fm-place-text">
                        <span className="fm-quick-label">{project.name}</span>
                        <span className="fm-place-path">{project.path}</span>
                      </span>
                    </button>
                  ))}
                </div>
              </>
            )}
          </div>
        )}

        {/* File list */}
        {!showQuick && (
          <>
            {/* Четыре действия папки РОВНЫМИ долями ширины — без прокрутки.
                Прежняя лента листалась вбок, и на телефоне 390 px за краем
                оставалось 333 px: «Терминал здесь» и «⋯ Ещё» не видел никто
                (то же правило, что у ряда клавиш терминала — ряд, который
                листается вбок, человек не листает). «Вверх» и «Обновить»
                уехали: первое живёт в строке пути, второе — в меню «Ещё».
                Всё редкое — там же, и меню вертикальное: оно не обрезает. */}
            <div className="fm-actions">
              {/* «Загрузить» читалось как «обновить содержимое»: направление
                  теперь названо явно — с телефона на компьютер. */}
              <button className="fm-action" onClick={uploading ? cancelUpload : handleUpload}>
                {/* \u0421\u0442\u0440\u0435\u043B\u043A\u0430 \u0432\u0432\u0435\u0440\u0445: \u0444\u0430\u0439\u043B \u0438\u0434\u0451\u0442 \u0421 \u0442\u0435\u043B\u0435\u0444\u043E\u043D\u0430 \u041D\u0410 \u043A\u043E\u043C\u043F\u044C\u044E\u0442\u0435\u0440. \u0422\u0430 \u0436\u0435
                    \u0438\u043A\u043E\u043D\u043A\u0430, \u0447\u0442\u043E \u0443 \u0441\u043A\u0430\u0447\u0438\u0432\u0430\u043D\u0438\u044F, \u043D\u043E \u0440\u0430\u0437\u0432\u0451\u0440\u043D\u0443\u0442\u0430\u044F \u2014 \u043D\u0430\u043F\u0440\u0430\u0432\u043B\u0435\u043D\u0438\u0435 \u0438
                    \u0435\u0441\u0442\u044C \u0435\u0434\u0438\u043D\u0441\u0442\u0432\u0435\u043D\u043D\u0430\u044F \u0440\u0430\u0437\u043D\u0438\u0446\u0430 \u043C\u0435\u0436\u0434\u0443 \u044D\u0442\u0438\u043C\u0438 \u0434\u0432\u0443\u043C\u044F \u0434\u0435\u0439\u0441\u0442\u0432\u0438\u044F\u043C\u0438. */}
                <span className="fm-action-icon up" aria-hidden>
                  {uploading ? "\u2715" : <IconDownload size={18} />}
                </span>
                <span className="fm-action-label">
                  {uploading ? (uploadPct != null ? `${uploadPct}%` : t("files.uploading")) : t("files.actionUpload")}
                </span>
              </button>
              <button className="fm-action" onClick={() => { setShowNewFolder(true); setNewFolderName(""); }}>
                {/* Плюс, а не папка: иконка папки рядом со списком папок
                    читалась бы как «открыть», а кнопка создаёт новую. */}
                <span className="fm-action-icon glyph" aria-hidden>{"+"}</span>
                <span className="fm-action-label">{t("files.actionFolder")}</span>
              </button>
              {/* Основной цикл «нашёл папку → работаю в ней»: терминал прямо
                  здесь. Deep-link ?cwd= у списка терминалов уже есть. */}
              <button className="fm-action" onClick={() => {
                haptic();
                navigate(`/pty?cwd=${encodeURIComponent(currentPath)}`);
              }}>
                <span className="fm-action-icon" aria-hidden><IconTerminal size={18} /></span>
                <span className="fm-action-label">{t("files.actionTerminal")}</span>
              </button>
              <button className="fm-action" onClick={() => { haptic(); setToolsMenu(true); }}>
                {/* Три точки, а не сетка 2×2: сеткой помечена вкладка «Ещё» в
                    нижней панели, и один знак на экране должен значить одно. */}
                <span className="fm-action-icon glyph" aria-hidden>{"⋯"}</span>
                <span className="fm-action-label">{t("files.actionMore")}</span>
              </button>
            </div>


            {/* Папка не открылась, а на экране осталась прежняя: без этой
                строки отказ читался как «тап не сработал» — тост про причину
                живёт две секунды, а список и крошки остаются от старой папки.
                Экран ошибки ниже показывается только когда показывать больше
                нечего, то есть ровно тогда, когда он не нужен. */}
            {dirError && items.length > 0 && (
              <div className="fm-error-bar" role="alert">
                <span className="fm-error-text">{"⚠️"} {dirError}</span>
                <button className="fm-error-retry" onClick={retryAll}>{t("conn.retry")}</button>
              </div>
            )}

            {/* Список обрезан сервером: молчаливая усечёнка читается как «в
                папке больше ничего нет». */}
            {listInfo?.truncated && (
              <div className="fm-disk">
                <span className="fm-disk-text">
                  {t("files.listTruncated", { shown: items.length, total: listInfo.total })}
                </span>
              </div>
            )}

            {/* Поиск открывается по кнопке. Раньше поле висело над каждой
                папкой (56 px в шапке из 291) ради действия, которое нужно
                изредка: список файлов важнее строки, в которую не печатают. */}
            {(showSearch || searchQuery || searchResults) && (
            <div className="fm-path-input-bar">
              <input className="fm-path-input" autoFocus value={searchQuery}
                onChange={(e) => { setSearchQuery(e.target.value); if (!e.target.value) setSearchResults(null); }}
                placeholder={t("files.search")} onKeyDown={(e) => e.key === "Enter" && handleSearch()} />
              {/* Отмена поиска была голым <span onClick> ВНУТРИ кнопки поиска:
                  до неё нельзя добраться с клавиатуры, а диктор читал одну
                  кнопку, которая по ходу дела меняла смысл. Две разные кнопки
                  вместо одной: пока ищем — «остановить», иначе — «искать».
                  Глиф у отмены свой, потому что справа стоит крестик закрытия
                  поиска: два одинаковых креста подряд не различить. */}
              {searching ? (
                <button
                  type="button"
                  className="btn btn-secondary btn-sm"
                  aria-label={t("files.searchStop")}
                  onClick={cancelSearch}
                >
                  {"\u25A0"}
                </button>
              ) : (
                <button className="btn btn-primary btn-sm" onClick={handleSearch}
                  disabled={searchQuery.length < 2}>
                  {"\uD83D\uDD0D"}
                </button>
              )}
              {/* \u0417\u0430\u043A\u0440\u044B\u0442\u044C \u043F\u043E\u0438\u0441\u043A \u0438 \u0432\u0435\u0440\u043D\u0443\u0442\u044C \u043F\u0430\u043F\u043A\u0443 \u0446\u0435\u043B\u0438\u043A\u043E\u043C \u2014 \u0442\u0435\u043C \u0436\u0435 \u043E\u0434\u043D\u0438\u043C \u043D\u0430\u0436\u0430\u0442\u0438\u0435\u043C,
                  \u043A\u0430\u043A\u0438\u043C \u0435\u0433\u043E \u043E\u0442\u043A\u0440\u044B\u043B\u0438. */}
              <button
                className="btn btn-secondary btn-sm"
                onClick={() => {
                  haptic();
                  setShowSearch(false);
                  setSearchQuery("");
                  setSearchResults(null);
                }}
                aria-label={t("files.close")}
              >
                {"\u2715"}
              </button>
            </div>
            )}

            {searchReturn && (
              <button className="fm-search-return" onClick={restoreSearchResults}>
                {"\u2190"} {t("files.backToResults", { n: searchReturn.results.length })}
              </button>
            )}

            {/* Search results */}
            {searchResults && (
              <div className="fm-list">
                <div style={{padding:"8px 16px",fontSize:12,color:"var(--tg-hint)"}}>
                  {searchResults.length} {t("files.results")}
                  {searchMeta?.truncated || searchMeta?.timedOut
                    ? ` \u00B7 ${t("files.resultsPartial")}`
                    : ""}
                  {searchMeta
                    ? ` \u00B7 ${t("files.searchScanned", { n: searchMeta.scanned, ms: searchMeta.elapsedMs })}`
                    : ""}
                  <button style={{marginLeft:8,fontSize:11,color:"var(--tg-link)",background:"none",border:"none",cursor:"pointer"}}
                    onClick={() => { setSearchResults(null); setSearchQuery(""); }}>{t("files.clearResults")}</button>
                </div>
                {searchResults.map((item) => (
                  <div key={item.path} className="fm-item"
                    onClick={() => openSearchResult(item)}>
                    <span className={`fm-icon${item.is_dir ? " dir" : ""}`} aria-hidden>
                      {item.is_dir ? <IconFolder size={24} /> : <IconDoc size={22} />}
                    </span>
                    <div className="fm-info">
                      <div className="fm-name">{item.name}</div>
                      <div className="fm-meta" style={{fontSize:11,wordBreak:"break-all"}}>{item.path}</div>
                    </div>
                  </div>
                ))}
              </div>
            )}

            {waiting ? (
              /* Спиннер идёт ПЕРВЫМ, пока ждать есть чего: «Проверить снова» в
                 офлайне должно давать видимый отклик, а не оставлять на экране
                 прежнюю плашку. При заведомо мёртвом канале (и без нажатой
                 кнопки повтора) ждать нечего — сразу плашка ниже. */
              <div className="loading-center">
                <div className="spinner" />
              </div>
            ) : pcOffline && items.length === 0 ? (
              /* ПК выключен: раньше здесь крутился спиннер, который не гас
                 никогда (loadDir не вызывался вовсе). */
              offlineState
            ) : load.phase === "error" && items.length === 0 ? (
              errorState
            ) : sortedItems.length === 0 ? (
              filterQ && items.length > 0 ? (
                <div className="empty">
                  <div className="empty-icon">{"\uD83D\uDD0D"}</div>
                  <div className="empty-text">{t("files.noMatchesHere")}</div>
                  <button className="btn btn-primary btn-sm" style={{ marginTop: 10 }}
                    onClick={handleSearch} disabled={searching || searchQuery.length < 2}>
                    {searching ? "\u23F3" : t("files.searchDeeper")}
                  </button>
                </div>
              ) : (
                <div className="empty">
                  <div className="empty-icon">{"\uD83D\uDCC2"}</div>
                  {/* «Пустая папка» в каталоге, где лежат только .env, .git и
                      .gitignore, — прямая ложь: сервер честно считает утаённое
                      (hidden_skipped), клиент это число просто не читал. */}
                  <div className="empty-text">
                    {listInfo && listInfo.hiddenSkipped > 0 && !showHidden
                      ? t("files.emptyHidden", { n: listInfo.hiddenSkipped })
                      : t("files.empty")}
                  </div>
                  {listInfo && listInfo.hiddenSkipped > 0 && !showHidden ? (
                    <button className="btn btn-primary btn-sm" style={{ marginTop: 10 }}
                      onClick={enableHidden}>
                      {t("files.showHidden")}
                    </button>
                  ) : (
                    <button className="btn btn-primary btn-sm" style={{ marginTop: 10 }}
                      onClick={uploading ? cancelUpload : handleUpload}>
                      {uploading ? "\u2715" : `\uD83D\uDCE5 ${t("files.uploadFromPhone")}`}
                      {uploading && uploadPct != null ? ` ${uploadPct}%` : ""}
                    </button>
                  )}
                </div>
              )
            ) : (
              <div className={`fm-list${selectMode ? " selecting" : ""}`}>
                {sortedItems.map((item) => {
                  const isSel = selected.includes(item.path);
                  const activateItem = () => {
                    // Клик после долгого удержания не должен отменять выбор.
                    if (suppressClick.current) { suppressClick.current = false; return; }
                    if (selectMode) { toggleSelect(item.path); return; }
                    if (item.is_dir) { haptic(); loadDir(item.path); }
                    else setContextItem(contextItem?.path === item.path ? null : item);
                  };
                  return (
                    <div
                      key={item.path}
                      data-fm-path={item.path}
                      className={`fm-item${contextItem?.path === item.path ? " fm-item-active" : ""}${isSel ? " fm-item-selected" : ""}${highlightPath === item.path ? " fm-item-highlight" : ""}`}
                      role="button"
                      tabIndex={0}
                      aria-label={`${selectMode ? (isSel ? "Снять выбор" : "Выбрать") : item.is_dir ? "Открыть папку" : "Действия файла"}: ${item.name}`}
                      aria-pressed={selectMode ? isSel : undefined}
                      aria-haspopup={!selectMode && !item.is_dir ? "dialog" : undefined}
                      aria-expanded={!selectMode && !item.is_dir ? contextItem?.path === item.path : undefined}
                      onClick={activateItem}
                      onKeyDown={(event) => {
                        if (event.key !== "Enter" && event.key !== " ") return;
                        event.preventDefault();
                        activateItem();
                      }}
                      onPointerDown={(e) => startLongPress(e, item)}
                      onPointerMove={moveLongPress}
                      onPointerUp={cancelLongPress}
                      onPointerLeave={cancelLongPress}
                      onPointerCancel={cancelLongPress}
                      onContextMenu={(e) => { e.preventDefault(); haptic(); setContextItem(item); }}
                    >
                      <span className={`fm-icon${item.is_dir ? " dir" : ""}`} aria-hidden>
                        {item.is_dir ? <IconFolder size={24} /> : <IconDoc size={22} />}
                      </span>
                      <div className="fm-info">
                        <div className="fm-name">{item.name}</div>
                        {/* \u0423 \u043F\u0430\u043F\u043A\u0438 \u0441\u043B\u043E\u0432\u043E \u00AB\u041F\u0430\u043F\u043A\u0430\u00BB \u043F\u043E\u0432\u0442\u043E\u0440\u044F\u043B\u043E \u0437\u043D\u0430\u0447\u043E\u043A \u0438 \u0448\u0435\u0432\u0440\u043E\u043D \u0432
                            \u0442\u043E\u0439 \u0436\u0435 \u0441\u0442\u0440\u043E\u043A\u0435 \u2014 \u0442\u0440\u0438 \u0440\u0430\u0437\u0430 \u043E\u0431 \u043E\u0434\u043D\u043E\u043C. \u041E\u0441\u0442\u0430\u043B\u0430\u0441\u044C \u0434\u0430\u0442\u0430;
                            \u0435\u0441\u043B\u0438 \u0435\u0451 \u043D\u0435\u0442, \u0441\u043B\u043E\u0432\u043E \u0432\u043E\u0437\u0432\u0440\u0430\u0449\u0430\u0435\u0442\u0441\u044F: \u043F\u0443\u0441\u0442\u0430\u044F \u0441\u0442\u0440\u043E\u043A\u0430 \u043F\u043E\u0434
                            \u0438\u043C\u0435\u043D\u0435\u043C \u0432\u044B\u0433\u043B\u044F\u0434\u0438\u0442 \u043A\u0430\u043A \u043F\u043E\u0442\u0435\u0440\u044F\u043D\u043D\u044B\u0435 \u0434\u0430\u043D\u043D\u044B\u0435. */}
                        <div className="fm-meta">
                          {item.is_dir
                            ? (item.modified ? formatDate(item.modified) : t("files.folder"))
                            : `${humanSize(item.size)}${item.modified ? ` \u00B7 ${formatDate(item.modified)}` : ""}`}
                        </div>
                      </div>
                      {selectMode ? (
                        <span className={`fm-check${isSel ? " on" : ""}`} aria-hidden>
                          {isSel ? "\u2713" : ""}
                        </span>
                      ) : item.is_dir ? (
                        <span className="fm-chevron">{"\u203A"}</span>
                      ) : null}
                    </div>
                  );
                })}
              </div>
            )}

            {/* Панель выбранной пачки. Заменяет одиночную: пока идёт выбор,
                действия относятся ко всем отмеченным строкам. */}
            {selectMode && (
              /* Шесть кнопок лентой не помещались никогда: «Удалить» и «Отмена»
                 стояли за правым краем. Осталось три ровные доли — главное
                 действие, меню и выход; остальное в меню, столбцом. */
              <div className="fm-select-bar">
                <div className="fm-select-count">{t("files.selectedN", { n: selected.length })}</div>
                <div className="fm-select-actions">
                  <button className="fm-action" disabled={selected.length === 0 || !!dl}
                    onClick={() => void downloadSelected()}>
                    <span className="fm-action-icon" aria-hidden><IconDownload size={18} /></span>
                    <span className="fm-action-label">{t("files.actionDownload")}</span>
                  </button>
                  <button className="fm-action" disabled={selected.length === 0}
                    onClick={() => { haptic(); setSelMenu(true); }}>
                    <span className="fm-action-icon glyph" aria-hidden>{"\u22EF"}</span>
                    <span className="fm-action-label">{t("files.actionMore")}</span>
                  </button>
                  {/* Удаление пачки — через вопрос, а не удержание: удерживать
                      кнопку в прокручиваемой ленте и было исходной бедой. */}
                  <button className="fm-action" onClick={() => { haptic(); exitSelect(); }}>
                    <span className="fm-action-icon glyph" aria-hidden>{"\u2715"}</span>
                    <span className="fm-action-label">{t("modal.cancel")}</span>
                  </button>
                </div>
              </div>
            )}

            {/* \u041c\u0435\u043d\u044e \u043f\u0430\u0447\u043a\u0438: \u043f\u0435\u0440\u0435\u043d\u043e\u0441, \u043a\u043e\u043f\u0438\u0440\u043e\u0432\u0430\u043d\u0438\u0435 \u0438 \u0443\u0434\u0430\u043b\u0435\u043d\u0438\u0435 \u2014 \u0441\u0442\u043e\u043b\u0431\u0446\u043e\u043c. \u0423\u0434\u0430\u043b\u0435\u043d\u0438\u0435
                \u0437\u0434\u0435\u0441\u044c \u0447\u0435\u0440\u0435\u0437 \u0432\u043e\u043f\u0440\u043e\u0441, \u0430 \u043d\u0435 \u0443\u0434\u0435\u0440\u0436\u0430\u043d\u0438\u0435: \u0443\u0434\u0435\u0440\u0436\u0438\u0432\u0430\u0442\u044c \u043a\u043d\u043e\u043f\u043a\u0443 \u0432
                \u043f\u0440\u043e\u043a\u0440\u0443\u0447\u0438\u0432\u0430\u0435\u043c\u043e\u0439 \u043b\u0435\u043d\u0442\u0435 \u0438 \u0431\u044b\u043b\u043e \u0438\u0441\u0445\u043e\u0434\u043d\u043e\u0439 \u0431\u0435\u0434\u043e\u0439. */}
            {selMenu && selectMode && (
              <SheetShell open={selMenu} onClose={() => setSelMenu(false)}
                overlayClassName="folder-menu-overlay" className="folder-menu" labelledBy="files-selection-menu-title">
                  <div className="folder-menu-title" id="files-selection-menu-title">{t("files.selectedN", { n: selected.length })}</div>
                  <button className="folder-menu-item" onClick={() => {
                    haptic();
                    setSelected(sortedItems.map((i) => i.path));
                    setSelMenu(false);
                  }}>
                    {"\u2611"} {t("files.selectAll")}
                  </button>
                  <button className="folder-menu-item" disabled={selected.length === 0}
                    onClick={() => { setSelMenu(false); setTransfer({ items: selectedItems, mode: "move" }); }}>
                    {"\u21aa"} {t("files.move")}
                  </button>
                  <button className="folder-menu-item" disabled={selected.length === 0}
                    onClick={() => { setSelMenu(false); setTransfer({ items: selectedItems, mode: "copy" }); }}>
                    {"\u2398"} {t("files.copy")}
                  </button>
                  <button className="folder-menu-item folder-menu-del" disabled={selected.length === 0}
                    onClick={() => { setSelMenu(false); void deleteSelected(); }}>
                    {"\ud83d\uddd1\ufe0f"} {t("files.delete")}
                  </button>
                  <button className="folder-menu-item cancel" onClick={() => setSelMenu(false)}>
                    {t("modal.cancel")}
                  </button>
              </SheetShell>
            )}

            {/* Действия над файлом. В ленте — то, чем пользуются каждый раз;
                «Переместить», «Копировать» и «Удалить» ушли в «⋯»: там кнопка
                удаления не живёт в прокручиваемой полосе, где палец, тянувший
                ленту к скрытым кнопкам, через 900 мс стирал файл. */}
            {contextItem && !selectMode && (
              <SheetShell open={!!contextItem} onClose={() => setContextItem(null)}
                overlayClassName="folder-menu-overlay" className="folder-menu" labelledBy="files-item-menu-title">
                  <div className="folder-menu-title" id="files-item-menu-title">{contextItem.name}</div>
                  <div className="folder-menu-path">
                    {contextItem.is_dir ? t("files.folder") : humanSize(contextItem.size)}
                    {contextItem.modified ? ` · ${formatDate(contextItem.modified)}` : ""}
                  </div>
                  {!contextItem.is_dir && (
                    <button className="folder-menu-item" onClick={() => {
                      rememberFileRow(contextItem.path);
                      const item = contextItem;
                      setContextItem(null);
                      void handlePreview(item);
                    }}>
                      {"\uD83D\uDC41\uFE0F"} {t("files.view")}
                    </button>
                  )}
                  {/* В Telegram файл забирают сообщением бота, а не «Скачать»:
                      там доставка идёт первой, вне Telegram — наоборот. */}
                  {telegramFirst
                    ? <>{ctxTelegramBtn}{ctxDownloadBtn}</>
                    : <>{ctxDownloadBtn}{ctxTelegramBtn}</>}
                  <button className="folder-menu-item" onClick={() => {
                    rememberFileRow(contextItem.path);
                    setShowRename(contextItem);
                    setRenameTo(contextItem.name);
                    setContextItem(null);
                  }}>
                    {"\u270F\uFE0F"} {t("files.rename")}
                  </button>
                  {/* \u0420\u0430\u043D\u044C\u0448\u0435 \u0437\u0434\u0435\u0441\u044C \u0441\u0442\u043E\u044F\u043B\u0430 \u043A\u043D\u043E\u043F\u043A\u0430 \u00AB\u22EF \u0415\u0449\u0451\u00BB, \u043E\u0442\u043A\u0440\u044B\u0432\u0430\u0432\u0448\u0430\u044F \u0412\u0422\u041E\u0420\u0423\u042E
                      \u0448\u0442\u043E\u0440\u043A\u0443: \u043F\u0435\u0440\u0435\u043D\u043E\u0441, \u043A\u043E\u043F\u0438\u0440\u043E\u0432\u0430\u043D\u0438\u0435 \u0438 \u0443\u0434\u0430\u043B\u0435\u043D\u0438\u0435 \u0436\u0438\u043B\u0438 \u043D\u0430 \u044D\u0442\u0430\u0436
                      \u043D\u0438\u0436\u0435 \u2014 \u0442\u043E \u0435\u0441\u0442\u044C \u0437\u0430 \u0434\u0432\u0443\u043C\u044F \u043D\u0430\u0436\u0430\u0442\u0438\u044F\u043C\u0438 \u0438 \u0432\u0441\u043B\u0435\u043F\u0443\u044E. \u0422\u0435\u043F\u0435\u0440\u044C \u0432\u0441\u0451,
                      \u0447\u0442\u043E \u043C\u043E\u0436\u043D\u043E \u0441\u0434\u0435\u043B\u0430\u0442\u044C \u0441 \u0444\u0430\u0439\u043B\u043E\u043C, \u0441\u0442\u043E\u0438\u0442 \u043E\u0434\u043D\u0438\u043C \u0441\u0442\u043E\u043B\u0431\u0446\u043E\u043C. */}
                  <button className="folder-menu-item" onClick={() => {
                    const it = contextItem;
                    setContextItem(null);
                    setTransfer({ items: [it], mode: "move" });
                  }}>
                    {"\u21AA"} {t("files.move")}
                  </button>
                  <button className="folder-menu-item" onClick={() => {
                    const it = contextItem;
                    setContextItem(null);
                    setTransfer({ items: [it], mode: "copy" });
                  }}>
                    {"\u2398"} {t("files.copy")}
                  </button>
                  <button className="folder-menu-item" onClick={() => { haptic(); startSelect(contextItem); }}>
                    {"\u2611"} {t("files.selectMany")}
                  </button>
                  {contextItem.is_dir && ai && (
                    <button className="folder-menu-item" onClick={() => {
                      haptic();
                      const dir = contextItem;
                      setContextItem(null);
                      loadDir(dir.path);
                      setShowNewSession(true);
                      setSessionName("");
                    }}>
                      {"\uD83E\uDD16"} {t("files.agent")}
                    </button>
                  )}
                  <HoldButton
                    className="folder-menu-item folder-menu-del"
                    onConfirm={() => { const it = contextItem; setContextItem(null); void handleDelete(it); }}
                  >
                    {"\uD83D\uDDD1\uFE0F"} {t("files.deleteHold")}
                  </HoldButton>
                  <button className="folder-menu-item cancel" onClick={() => setContextItem(null)}>
                    {t("modal.cancel")}
                  </button>
              </SheetShell>
            )}
          </>
        )}
      </div>

      {/* Пока перенос идёт, на экране висит его состояние: раньше шторка
          закрывалась мгновенно, и копирование 8 ГБ выглядело как «ничего не
          произошло», а через 30 секунд приходила ошибка о таймауте. */}
      {transferring && !choice && (
        // Шторка выбора папки на время операции остаётся открытой (z-index 500),
        // поэтому плашку поднимаем НАД ней и уводим наверх экрана: снизу она
        // накрыла бы кнопку «Переношу…» самой шторки.
        <div
          className="ssh-transfer-bar"
          style={{ zIndex: 550, top: "env(safe-area-inset-top, 0px)", bottom: "auto" }}
        >
          <span className="ssh-transfer-text">
            {t(transferring.mode === "move" ? "files.movingTo" : "files.copyingTo", {
              name: transferring.name, dir: transferring.dir,
            })}
          </span>
        </div>
      )}

      <FolderNavSheet
        open={!!transfer}
        onClose={() => { if (!transferring) setTransfer(null); }}
        onPick={(path) => finishTransfer(path)}
        currentCwd={currentPath}
        initialTab="browse"
        title={transfer
          ? t(transfer.mode === "move" ? "files.moveTitle" : "files.copyTitle", {
            name: transfer.items.length === 1
              ? transfer.items[0].name
              : t("files.nObjects", { n: transfer.items.length }),
          })
          : ""}
        pickLabel={transferring ? t("files.transferring") : t(transfer?.mode === "move" ? "files.moveHere" : "files.copyHere")}
      />

      {/* New folder modal */}
      {showNewFolder && (
        <SheetShell
          open={showNewFolder}
          onClose={() => setShowNewFolder(false)}
          overlayClassName="modal-overlay"
          className="modal-sheet"
          labelledBy="files-new-folder-title"
        >
            {/* У окна свой заголовок: ключ «Папка» работает подписью кнопки, а
                в шапке окна с полем «Имя папки» читался как «переименование». */}
            <div className="modal-title" id="files-new-folder-title">{t("files.newFolderTitle")}</div>
            <input className="modal-input" value={newFolderName} onChange={(e) => setNewFolderName(e.target.value)}
              aria-label={t("files.folderName")} placeholder={t("files.folderName")}
              onKeyDown={(e) => e.key === "Enter" && handleCreateFolder()} />
            <div className="modal-actions">
              <button className="btn btn-secondary" onClick={() => setShowNewFolder(false)}>{t("modal.cancel")}</button>
              <button className="btn btn-primary" onClick={handleCreateFolder} disabled={!newFolderName.trim()}>{t("modal.create")}</button>
            </div>
        </SheetShell>
      )}

      {/* Rename modal */}
      {showRename && (
        <SheetShell
          open={!!showRename}
          onClose={closeRename}
          overlayClassName="modal-overlay"
          className="modal-sheet"
          labelledBy="files-rename-title"
        >
            <div className="modal-title" id="files-rename-title">{t("files.rename")}</div>
            {/* Единственное поле в клиенте вообще без имени: ни подписи, ни
                подсказки внутри. Подсказку даём такую же, как у соседнего окна
                «Новая папка», плюс aria-label — заголовок окна диктор к полю не
                привязывает. */}
            <input className="modal-input" value={renameTo} onChange={(e) => setRenameTo(e.target.value)}
              aria-label={t("files.newName")} placeholder={t("files.newName")}
              onKeyDown={(e) => e.key === "Enter" && handleRename()} />
            <div className="modal-actions">
              <button className="btn btn-secondary" onClick={closeRename}>{t("modal.cancel")}</button>
              <button className="btn btn-primary" onClick={handleRename} disabled={!renameTo.trim()}>{t("files.rename")}</button>
            </div>
        </SheetShell>
      )}

      {/* New session modal */}
      {showNewSession && (
        <SheetShell
          open={showNewSession}
          onClose={() => setShowNewSession(false)}
          overlayClassName="modal-overlay"
          className="modal-sheet"
          labelledBy="files-new-session-title"
        >
            <div className="modal-title" id="files-new-session-title">{t("files.newAgentSession")}</div>
            <div style={{fontSize:12,color:"var(--tg-hint)",marginBottom:8,fontFamily:"monospace",wordBreak:"break-all"}}>
              {"\uD83D\uDCC2"} {currentPath}
            </div>
            <input className="modal-input" value={sessionName} onChange={(e) => setSessionName(e.target.value)}
              aria-label={t("modal.sessionName")} placeholder={t("modal.sessionName")} />
            <div className="setting-label">{t("modal.agent")}</div>
            <div className="agent-grid">
              {agents.map((a) => (
                <button key={a.id} className={`agent-chip ${sessionAgent === a.id ? "active" : ""}`}
                  onClick={() => setSessionAgent(a.id)} title={a.description}>
                  <span className="agent-chip-icon">{a.icon}</span>
                  <span className="agent-chip-name">{a.name}</span>
                </button>
              ))}
            </div>
            <div className="modal-actions">
              <button className="btn btn-secondary" onClick={() => setShowNewSession(false)}>{t("modal.cancel")}</button>
              <button className="btn btn-primary" onClick={handleNewSession} disabled={!sessionName.trim()}>{t("modal.create")}</button>
            </div>
        </SheetShell>
      )}

      {/* Preview modal */}
      {preview && (
        <SheetShell
          open={!!preview}
          onClose={closePreview}
          overlayClassName="modal-overlay"
          className="modal-sheet files-preview-sheet"
          labelledBy="files-preview-title"
        >
            <div style={{ display: "flex", justifyContent: "space-between", alignItems: "center", marginBottom: 12 }}>
              <div className="modal-title" id="files-preview-title" style={{ margin: 0 }}>{previewItem?.name || t("files.view")}</div>
              <button className="icon-btn" onClick={closePreview} aria-label={t("modal.close")}>{"\u2716"}</button>
            </div>
            {preview.type === "text" && !previewLooksBinary && (
              <>
                <pre style={{
                  fontSize: 12, lineHeight: 1.4, fontFamily: "'SF Mono', Consolas, monospace",
                  background: "var(--tg-secondary-bg)", padding: 12, borderRadius: 8,
                  overflow: "auto", maxHeight: "70vh", whiteSpace: "pre-wrap", wordBreak: "break-all",
                }}>{previewShown}</pre>
                {/* Молчаливая обрезка читалась как «файл такой и есть», а конец
                    лога — обычно самое нужное. */}
                {previewTruncated && (
                  <div className="empty-desc" style={{ marginTop: 8 }}>
                    {t("files.previewTruncated", { n: previewShown.length })}
                  </div>
                )}
              </>
            )}
            {preview.type === "image" && (
              <img src={`data:${preview.mime};base64,${preview.data}`}
                alt={previewItem?.name || ""} style={{ width: "100%", borderRadius: 8 }} />
            )}
            {/* Бинарник до 500 КБ сервер отдаёт текстовой веткой, и человек
                видел экран крокозябр вместо объяснения (находка N115). */}
            {(preview.type === "binary" || previewLooksBinary) && (
              <div className="empty">
                <div className="empty-icon">{"\ud83e\uddf1"}</div>
                <div className="empty-text">
                  {t("files.binaryFile")}
                  {(previewItem?.size ?? preview.size) != null
                    ? ` (${humanSize(previewItem?.size ?? preview.size)})`
                    : ""}
                </div>
                <div className="empty-desc">{t("files.previewNotText")}</div>
              </div>
            )}
            {/* Выход к самому файлу: из модалки был доступен только ✖. */}
            {previewItem && !previewItem.is_dir && (
              <div className="modal-actions">
                <button className="btn btn-secondary" onClick={closePreview}>
                  {t("files.close")}
                </button>
                <button
                  className="btn btn-primary"
                  onClick={() => {
                    const it = previewItem;
                    closePreview();
                    void handleDownload(it);
                  }}
                >
                  {"\u2b07\ufe0f"} {t("files.downloadToPhone")}
                </button>
              </div>
            )}
        </SheetShell>
      )}

      {/* Выбор из нескольких выходов: «Заменить» / «Сохранить копию» / «Отмена»
          и т.п. Отдельный лист, потому что confirm умеет только «да/нет». */}
      {choice && (
        <SheetShell open={!!choice} onClose={() => answerChoice(null)}
          overlayClassName="folder-menu-overlay" className="folder-menu" labelledBy="files-choice-title">
            <div className="folder-menu-title" id="files-choice-title">{choice.title}</div>
            <div className="folder-menu-path" style={{ fontFamily: "inherit", fontSize: 12 }}>
              {choice.message}
            </div>
            {/* Первый фокус — безопасная отмена, даже когда первый вариант перезаписывает файл. */}
            <button className="folder-menu-item cancel" onClick={() => answerChoice(null)}>
              {t("modal.cancel")}
            </button>
            {choice.options.map((opt) => (
              <button
                key={opt.id}
                className="folder-menu-item"
                // Необратимое действие («Заменить») обязано отличаться видом, а
                // не только подписью — красным, как остальные опасные пункты.
                style={opt.danger ? { color: "var(--tg-destructive)" } : undefined}
                onClick={() => answerChoice(opt.id)}
              >
                {opt.label}
              </button>
            ))}
        </SheetShell>
      )}

      {/* Редкие действия над ПАПКОЙ — то, что раньше уезжало за край тулбара. */}
      {toolsMenu && (
        <SheetShell open={toolsMenu} onClose={() => setToolsMenu(false)}
          overlayClassName="folder-menu-overlay" className="folder-menu" labelledBy="files-tools-menu-title">
            <div className="folder-menu-title" id="files-tools-menu-title">{t("files.more")}</div>
            <div className="folder-menu-path">{currentPath}</div>
            {/* Та же звезда и то же состояние, что у кнопки в шапке: один знак
                — одно действие, закрашенная ★ значит «уже в избранном». */}
            <button className="folder-menu-item" onClick={() => { setToolsMenu(false); void handleTogglePin(); }}>
              {pinned ? "★" : "☆"} {pinned ? t("files.favAdded") : t("files.favAdd")}
            </button>
            <button className="folder-menu-item" onClick={() => { setToolsMenu(false); void copyCurrentPath(); }}>
              <IconCopy size={16} /> {t("files.copyPath")}
            </button>
            <button className="folder-menu-item" onClick={() => { haptic(); setToolsMenu(false); startSelect(); }}>
              {"☑"} {t("files.selectMany")}
            </button>
            {ai && (
              <button className="folder-menu-item" onClick={() => {
                haptic();
                setToolsMenu(false);
                setShowNewSession(true);
                setSessionName("");
              }}>
                {"🤖"} {t("files.agent")}
              </button>
            )}
            {/* Сюда переехало то, что раньше стояло полосами над списком:
                обновление, сортировка, скрытые файлы, ручной путь и цифра
                свободного места. Пункт называет ТЕКУЩЕЕ состояние — в меню
                заходят и чтобы узнать, как отсортировано, и чтобы поменять. */}
            <button className="folder-menu-item" onClick={() => { haptic(); setToolsMenu(false); void loadDir(currentPath); }}>
              {"↻"} {t("files.refresh")}
            </button>
            <button
              className="folder-menu-item"
              onClick={() => {
                haptic();
                // Порядок перебирается по кругу: имя → дата → размер. Три
                // отдельных пункта заняли бы половину меню ради одной настройки.
                const next = sortBy === "name" ? "date" : sortBy === "date" ? "size" : "name";
                setSortBy(next);
                sortRef.current = next;
                try { localStorage.setItem(SORT_KEY, next); } catch { /* quota */ }
                // Перечитываем папку: сортирует сервер, иначе порядок менялся бы
                // только внутри уже обрезанной выборки.
                void loadDir(currentPath);
              }}
            >
              {"↕"} {t("files.sortLine", { value: t(`files.sort${sortBy.charAt(0).toUpperCase() + sortBy.slice(1)}`).toLowerCase() })}
            </button>
            <button
              className="folder-menu-item"
              onClick={() => {
                haptic();
                const next = !showHidden;
                setShowHidden(next);
                showHiddenRef.current = next;
                try { localStorage.setItem(HIDDEN_KEY, next ? "1" : "0"); } catch { /* quota */ }
                void loadDir(currentPath);
              }}
              title={t("files.hiddenHint")}
            >
              {showHidden ? "◉" : "○"} {t("files.hiddenLine", { value: showHidden ? t("files.hiddenShown") : t("files.hiddenNotShown") })}
            </button>
            <button
              className="folder-menu-item"
              onClick={() => {
                haptic();
                setToolsMenu(false);
                // Поле открывается С ТЕКУЩИМ путём: правят обычно один сегмент,
                // а не набирают «C:\Users\user\Projects\api» заново.
                setPathInput(currentPath);
                setShowPathInput(true);
              }}
            >
              {"⌨"} {t("files.goToPath")}
            </button>
            {disk && disk.total > 0 && (
              <div className="folder-menu-note">
                {t("files.diskLine", { free: humanSize(disk.free), total: humanSize(disk.total) })}
              </div>
            )}
            <button className="folder-menu-item cancel" onClick={() => setToolsMenu(false)}>
              {t("modal.cancel")}
            </button>
        </SheetShell>
      )}

      {/* Справка. В «Файлах» её не было вовсе, хотя именно здесь человеку
          страшнее всего: куда девается удалённое и куда попадает скачанное. */}
      <HelpSheet
        open={showHelp}
        onClose={() => setShowHelp(false)}
        title={t("files.helpTitle")}
        guide="files"
        items={[
          { icon: "🗑️", title: t("files.helpDeleteTitle"), text: t("files.helpDeleteText") },
          { icon: "⬇️", title: t("files.helpDownloadTitle"), text: t("files.helpDownloadText") },
          { icon: "📥", title: t("files.helpUploadTitle"), text: t("files.helpUploadText") },
          { icon: "☑", title: t("files.helpSelectTitle"), text: t("files.helpSelectText") },
          { icon: "▶", title: t("files.helpTerminalTitle"), text: t("files.helpTerminalText") },
        ]}
      />

      <BottomNav active="files" />
    </div>
  );
}
