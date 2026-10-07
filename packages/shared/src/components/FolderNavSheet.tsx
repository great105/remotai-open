import { useCallback, useEffect, useId, useMemo, useRef, useState, type MouseEvent as ReactMouseEvent } from "react";
import {
  getQuickPaths, listFiles, getBookmarks, getRecentFolders,
  addBookmark, removeBookmark, getProjects, mkDir,
} from "../api-endpoints";
import type { QuickPath, FileItem, Bookmark, Project, RecentFolder } from "../types";
import { platform } from "../platform";
import { useToast } from "./Toast";
import { useEscape } from "../hooks/useEscape";
import { t } from "../i18n";
import { folderLabel } from "../folderLabel";
import { isPcOffline, mapApiError } from "../api-core";
import { OfflineState } from "./OfflineState";
import { SheetShell } from "./DialogHost";
import { useLanguage } from "../locale";
import { selectFolders, type FolderSort, type FolderTab } from "../folderList";

type Tab = FolderTab;

function initialSorts(): Record<Tab, FolderSort> {
  const sorts: Record<Tab, FolderSort> = { fav: "name-asc", recent: "date-desc", projects: "date-desc", browse: "name-asc" };
  for (const tab of Object.keys(sorts) as Tab[]) {
    try {
      const saved = localStorage.getItem(`remotai.folderSort.${tab}`);
      if (saved === "name-asc" || saved === "name-desc" || (tab !== "fav" && saved === "date-desc")) sorts[tab] = saved;
    } catch { /* Private storage: keep the default. */ }
  }
  return sorts;
}

interface Props {
  open: boolean;
  onClose: () => void;
  /**
   * Основное действие: cd / открыть / перенести сюда.
   *
   * Может быть асинхронным (находка N108): раньше шторка закрывалась в тот же
   * миг, что и вызов onPick, поэтому копирование папки на 8 ГБ выглядело как
   * «ничего не произошло», а через 30 секунд приходила ошибка таймаута. Теперь
   * шторка ждёт результат, показывает pickLabel («Копирую…») и блокирует
   * повторный запуск. Вернуть false — операция не удалась, шторку не закрывать.
   */
  onPick: (path: string) => void | boolean | Promise<void | boolean>;
  onOpenHere?: (path: string) => void;      // alternate action: open new terminal here
  currentCwd?: string;                      // for pin-current button + highlight
  title?: string;
  /**
   * Action label for onPick. "cd" (default) for in-terminal nav,
   * "open" for /pty main launch.
   */
  pickLabel?: string;
  /** Open this tab initially (defaults to "fav"). */
  initialTab?: Tab;
}

const FOLDER_ICON = "\uD83D\uDCC1";
const PIN_ICON = "\uD83D\uDCCC";
const STAR_ICON = "\u2B50";
const CLOCK_ICON = "\uD83D\uDD52";
const GIT_ICON = "\uD83D\uDD00";
const UP_ICON = "\u2B06";
const SEARCH_ICON = "\uD83D\uDD0D";
const COPY_ICON = "\uD83D\uDCCB";

/**
 * Провал всех трёх списков — это «до ПК не доехали» или «доступ потерян», но
 * НЕ «агент старый». Древний агент отвечает 404 на закладки/последние папки,
 * и у него по-прежнему работает вкладка «Обзор» — глушить из-за него всю
 * шторку нельзя.
 */
function looksUnreachable(e: unknown): boolean {
  if (isPcOffline(e)) return true;
  const status = (e as { status?: unknown })?.status;
  if (typeof status !== "number" || status === 0) return true; // сеть/таймаут
  return status === 401 || status === 403 || status >= 500;
}

const quickIcons: Record<string, string> = {
  CWD: "\uD83D\uDCBB", Desktop: "\uD83D\uDDA5\uFE0F",
  Downloads: "\u2B07\uFE0F", Documents: "\uD83D\uDCC4",
  Home: "\uD83C\uDFE0",
};

export function FolderNavSheet(props: Props) {
  const language = useLanguage();
  const locale = language === "ru" ? "ru-RU" : "en-US";
  const { toast, toastSuccess, toastError } = useToast();
  const [tab, setTab] = useState<Tab>(props.initialTab || "fav");
  const [query, setQuery] = useState("");
  const [sorts, setSorts] = useState(initialSorts);
  const [filtersOpen, setFiltersOpen] = useState(false);
  const [hideDotFolders, setHideDotFolders] = useState(true);
  const [favoritesOnly, setFavoritesOnly] = useState(false);
  const [searchPath, setSearchPath] = useState(true);
  const filtersId = useId();
  const [showNewFolder, setShowNewFolder] = useState(false);
  const [newFolderName, setNewFolderName] = useState("");

  const [quick, setQuick] = useState<QuickPath[]>([]);
  const [bookmarks, setBookmarks] = useState<Bookmark[]>([]);
  const [recent, setRecent] = useState<RecentFolder[]>([]);
  const [projects, setProjects] = useState<Project[]>([]);
  const [projectsLoading, setProjectsLoading] = useState(false);
  // Все три источника списков отвалились — значит ПК недоступен (или доступ
  // потерян), и молча показывать пустое «Избранное» нельзя: человек листал
  // пустоту, закрывал шторку и пробовал снова.
  const [loadFailed, setLoadFailed] = useState<{ offline: boolean; msg: string } | null>(null);

  // Browse state
  const [browsePath, setBrowsePath] = useState("");
  const [browseItems, setBrowseItems] = useState<FileItem[]>([]);
  const [browseParent, setBrowseParent] = useState<string | null>(null);
  const [browseLoading, setBrowseLoading] = useState(false);
  const [browseTruncated, setBrowseTruncated] = useState(false);
  const browseRequest = useRef(0);
  const [pathInput, setPathInput] = useState("");
  const [showPathInput, setShowPathInput] = useState(false);

  // Long-press
  const longPressTimer = useRef<number | null>(null);
  const [menu, setMenu] = useState<{ path: string; name: string } | null>(null);

  // Операция onPick идёт: держим шторку открытой, пока не придёт результат.
  // Второй запуск того же переноса запрещаем — иначе первый успевал создать
  // папку, а второй получал «уже существует», и человек читал два
  // противоречащих ответа об одном действии (находка N108).
  const [picking, setPicking] = useState(false);

  // Back/ESC, от верхнего слоя к нижнему: контекст-меню → модалка «новая
  // папка» → сама шторка. Условия взаимоисключающие — активен один хендлер.
  useEscape(props.open && !!menu, () => setMenu(null));
  useEscape(props.open && !menu && showNewFolder, () => setShowNewFolder(false));
  // Пока идёт перенос, «Назад»/Esc шторку не закрывают: она — единственное место,
  // где видно, что операция ещё идёт.
  useEscape(props.open && !menu && !showNewFolder && !picking, props.onClose);

  /**
   * Начальные списки шторки. Частичный отказ терпим (нет закладок — не беда),
   * но если не ответил НИ ОДИН запрос, причина одна: до ПК не доехали. Тогда
   * вместо пустых вкладок показываем честное состояние с кнопкой «Проверить
   * снова» (см. рендер ниже).
   */
  const loadLists = useCallback(async () => {
    setLoadFailed(null);
    const results = await Promise.allSettled([getQuickPaths(), getBookmarks(), getRecentFolders()]);
    const [q, b, rec] = results;
    if (q.status === "fulfilled") setQuick(q.value.paths || []);
    if (b.status === "fulfilled") setBookmarks(b.value.bookmarks || []);
    if (rec.status === "fulfilled") setRecent(rec.value.folders || []);
    if (results.every((r) => r.status === "rejected")) {
      const reason = (results[0] as PromiseRejectedResult).reason;
      if (looksUnreachable(reason)) {
        setLoadFailed({ offline: isPcOffline(reason), msg: mapApiError(reason) });
      }
    }
  }, []);

  useEffect(() => {
    if (!props.open) return;
    setTab(props.initialTab || "fav");
    setQuery("");
    setFiltersOpen(false);
    void loadLists();
  }, [props.open, props.initialTab, loadLists]);

  useEffect(() => {
    for (const tab of Object.keys(sorts) as Tab[]) {
      try { localStorage.setItem(`remotai.folderSort.${tab}`, sorts[tab]); } catch { /* Private storage. */ }
    }
  }, [sorts]);

  // Auto-load current cwd into browse when that tab is active and empty.
  // При недоступном ПК не ходим вовсе: об этом уже сказано в теле шторки, а
  // запрос дал бы вторую жалобу тостом.
  useEffect(() => {
    if (!props.open || loadFailed || tab !== "browse" || browsePath || browseLoading) return;
    browseDir(props.currentCwd || ".");
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [props.open, tab, loadFailed]);

  // Lazy-load projects only when user switches to that tab.
  useEffect(() => {
    if (!props.open || loadFailed || tab !== "projects" || projects.length > 0 || projectsLoading) return;
    setProjectsLoading(true);
    getProjects()
      .then((d) => setProjects(d.projects || []))
      .catch(() => {})
      .finally(() => setProjectsLoading(false));
  }, [props.open, tab, projects.length, projectsLoading, loadFailed]);

  const refreshBookmarks = () => {
    getBookmarks().then((d) => setBookmarks(d.bookmarks || [])).catch(() => {});
  };

  const browseDir = async (path: string, sort = sorts.browse) => {
    const request = ++browseRequest.current;
    setBrowseLoading(true);
    try {
      // Let the agent sort before its result limit. Hidden folders are returned
      // once so the local dot-folder filter can toggle without another request.
      const data = await listFiles(path, { hidden: true, sort: sort === "date-desc" ? "date" : "name" });
      if (request !== browseRequest.current) return;
      setBrowseItems((data.items || []).filter((i: FileItem) => i.is_dir));
      setBrowsePath(data.path);
      setBrowseParent(data.parent);
      setBrowseTruncated(!!data.truncated);
      setTab("browse");
    } catch (e: any) {
      if (request === browseRequest.current) toastError(mapApiError(e));
    }
    if (request === browseRequest.current) setBrowseLoading(false);
  };

  const changeSort = (sort: FolderSort) => {
    setSorts(previous => ({ ...previous, [tab]: sort }));
    if (tab === "browse" && browsePath) void browseDir(browsePath, sort);
  };

  const pick = async (path: string) => {
    if (picking) return;
    platform().haptic();
    setPicking(true);
    try {
      const res = await props.onPick(path);
      if (res === false) return; // не удалось — остаёмся в шторке
      props.onClose();
    } catch (e) {
      // Причину называет сам вызывающий (тостом): здесь только не закрываем
      // шторку, чтобы человек мог выбрать другую папку или повторить.
      console.debug("[folder-nav] действие над папкой не выполнено:", e);
    } finally {
      setPicking(false);
    }
  };

  // Tap on a quick-tile / bookmark / recent / project → step into that folder
  // in the Browse tab instead of immediately applying the pick action. The
  // long-press gesture on each row still opens the context menu where the
  // user can explicitly choose "Open here".
  const browseInto = (path: string) => {
    platform().haptic();
    setTab("browse");
    browseDir(path);
  };

  const openHere = (path: string) => {
    if (!props.onOpenHere) return pick(path);
    platform().haptic("medium");
    props.onOpenHere(path);
    props.onClose();
  };

  const pinCurrent = async () => {
    const p = props.currentCwd?.trim();
    if (!p) return;
    const name = p.split(/[/\\]/).filter(Boolean).pop() || p;
    try {
      await addBookmark(name, p);
      platform().hapticSuccess();
      toastSuccess(t("toast.bookmarkAdded"));
      refreshBookmarks();
    } catch {
      toast(t("folder.alreadyPinned"));
    }
  };

  const handleCreateFolder = async () => {
    const name = newFolderName.trim();
    if (!name || !browsePath) return;
    // Build child path with the separator already used by browsePath.
    const sep = browsePath.includes("\\") ? "\\" : "/";
    const target = browsePath.replace(/[\\/]+$/, "") + sep + name;
    try {
      await mkDir(target);
      platform().hapticSuccess();
      toastSuccess(t("toast.folderCreated", { name }));
      setShowNewFolder(false);
      setNewFolderName("");
      await browseDir(target); // jump into the freshly-created folder
    } catch (e: any) {
      toastError(mapApiError(e));
    }
  };

  const togglePin = async (path: string, name: string) => {
    const isPinned = bookmarks.some((b) => b.path === path);
    try {
      if (isPinned) {
        await removeBookmark(path);
        toast(t("folder.unpinned"));
      } else {
        await addBookmark(name, path);
        toastSuccess(t("toast.bookmarkAdded"));
      }
      refreshBookmarks();
    } catch {
      /* ignore */
    }
  };

  const copyPath = async (path: string) => {
    try {
      await navigator.clipboard.writeText(path);
      toastSuccess(t("folder.pathCopied"));
    } catch {
      /* ignore */
    }
  };

  const onItemPressStart = (path: string, name: string) => {
    if (longPressTimer.current) window.clearTimeout(longPressTimer.current);
    longPressTimer.current = window.setTimeout(() => {
      platform().haptic("medium");
      setMenu({ path, name });
    }, 500);
  };

  const onItemPressEnd = () => {
    if (longPressTimer.current) {
      window.clearTimeout(longPressTimer.current);
      longPressTimer.current = null;
    }
  };

  // Десктоп (мышь): правый клик открывает то же контекст-меню, что и long-press
  // на тач — иначе пункты pin/copy-path/openHere недостижимы в окне exe/браузере.
  const onItemContext = (e: ReactMouseEvent, path: string, name: string) => {
    e.preventDefault();
    onItemPressEnd();
    platform().haptic("medium");
    setMenu({ path, name });
  };

  const selection = useMemo(() => ({
    query, sort: sorts[tab], locale, searchPath,
    hideDotFolders: tab === "browse" && hideDotFolders,
    favorites: tab !== "fav" && favoritesOnly ? bookmarks : undefined,
  }), [query, sorts, tab, locale, searchPath, hideDotFolders, favoritesOnly, bookmarks]);
  const filteredQuick = useMemo(() => selectFolders(quick, { ...selection, label: item => folderLabel(item.name) }), [selection, quick]);
  const filteredBookmarks = useMemo(() => selectFolders(bookmarks, selection), [selection, bookmarks]);
  const filteredRecent = useMemo(() => selectFolders(recent, selection), [selection, recent]);
  const filteredProjects = useMemo(() => selectFolders(projects, selection), [selection, projects]);
  const filteredBrowse = useMemo(() => selectFolders(browseItems, selection), [selection, browseItems]);
  const filterCount = Number(tab !== "fav" && favoritesOnly) + Number(tab === "browse" && !hideDotFolders) + Number(!searchPath);

  const pathSegments = browsePath.split(/[/\\]/).filter(Boolean);

  /**
   * Главное действие над строкой — явной кнопкой справа. До этого тап по
   * «Избранному»/«Недавним»/«Проектам» ВСЕГДА проваливался внутрь папки
   * (browseInto), а «Открыть здесь» пряталось за долгим тапом, который на
   * сенсоре не находят. Теперь оба пути видны: тап по строке — зайти внутрь,
   * кнопка — работать в этой папке.
   */
  const rowActionLabel = props.onOpenHere
    ? t("pty.openHere")
    : (props.pickLabel || t("folder.cdHere"));
  const rowAction = (path: string) => (
    <button
      className="folder-row-action"
      title={rowActionLabel}
      disabled={picking}
      onPointerDown={(e) => { e.stopPropagation(); onItemPressEnd(); }}
      onClick={(e) => { e.stopPropagation(); onItemPressEnd(); openHere(path); }}
    >
      {rowActionLabel}
    </button>
  );

  if (!props.open) return null;

  // Верхние слои шторки. Оба лежат ВНУТРИ затемнения (у .modal-overlay
  // z-index 200 против 500 у .folder-sheet-overlay — вынеси их наружу, и
  // «новая папка» уедет ПОД шторку), поэтому идут отдельным пропом extra.
  const layers = (
    <>
      {/* New folder modal */}
      {showNewFolder && (
        <SheetShell
          open={showNewFolder}
          onClose={() => setShowNewFolder(false)}
          overlayClassName="modal-overlay"
          className="modal-sheet"
          labelledBy="folder-nav-new-title"
        >
          <div className="modal-title" id="folder-nav-new-title">{t("files.newFolder")}</div>
          <div style={{ fontSize: 12, color: "var(--tg-hint)", marginBottom: 8, wordBreak: "break-all", fontFamily: "monospace" }}>
            {"📂"} {browsePath}
          </div>
          <input
            className="modal-input"
            value={newFolderName}
            onChange={(e) => setNewFolderName(e.target.value)}
            placeholder={t("files.folderName")}
            autoFocus
            onKeyDown={(e) => { if (e.key === "Enter") handleCreateFolder(); }}
          />
          <div className="modal-actions">
            <button className="btn btn-secondary" onClick={() => setShowNewFolder(false)}>
              {t("modal.cancel")}
            </button>
            <button
              className="btn btn-primary"
              onClick={handleCreateFolder}
              disabled={!newFolderName.trim()}
            >
              {t("modal.create")}
            </button>
          </div>
        </SheetShell>
      )}

      {/* Long-press context menu */}
      {menu && (
        <SheetShell
          open={!!menu}
          onClose={() => setMenu(null)}
          overlayClassName="folder-menu-overlay"
          className="folder-menu"
          labelledBy="folder-nav-menu-title"
        >
          <div className="folder-menu-title" id="folder-nav-menu-title">{menu.name}</div>
          <div className="folder-menu-path">{menu.path}</div>
          <button className="folder-menu-item" onClick={() => { setMenu(null); void pick(menu.path); }}>
            {props.pickLabel || t("folder.cdHere")}
          </button>
          {props.onOpenHere && (
            <button className="folder-menu-item" onClick={() => { setMenu(null); openHere(menu.path); }}>
              {t("pty.openHere")}
            </button>
          )}
          <button className="folder-menu-item" onClick={() => { setMenu(null); togglePin(menu.path, menu.name); }}>
            {bookmarks.some((b) => b.path === menu.path) ? t("folder.unpin") : t("folder.pin")}
          </button>
          <button className="folder-menu-item" onClick={() => { setMenu(null); copyPath(menu.path); }}>
            {COPY_ICON} {t("folder.copyPath")}
          </button>
          <button className="folder-menu-item cancel" onClick={() => setMenu(null)}>
            {t("modal.cancel")}
          </button>
        </SheetShell>
      )}
    </>
  );

  return (
    <SheetShell
      open={props.open}
      onClose={props.onClose}
      overlayClassName="folder-sheet-overlay"
      className="folder-sheet"
      labelledBy="folder-nav-title"
      // Пока идёт перенос, промах по фону не должен уносить единственное место,
      // где видно его состояние.
      closeOnBackdrop={!picking}
      extra={layers}
    >
        {/* ── Header ───────────────────────────────── */}
        <div className="folder-sheet-header">
          <div className="folder-sheet-title" id="folder-nav-title">{props.title || t("folder.title")}</div>
          <button className="folder-sheet-close" aria-label={t("modal.close")} onClick={props.onClose} disabled={picking}>{"\u2715"}</button>
        </div>

        {/* Current cwd strip */}
        {props.currentCwd && (
          <div className="folder-sheet-cwd">
            <span className="folder-sheet-cwd-label">{"\uD83D\uDCCD"}</span>
            <span
              className="folder-sheet-cwd-path"
              role="button"
              tabIndex={0}
              aria-label={t("folder.copyPath")}
              onClick={() => copyPath(props.currentCwd!)}
              onKeyDown={(e) => {
                if (e.key === "Enter" || e.key === " ") { e.preventDefault(); copyPath(props.currentCwd!); }
              }}
            >
              {props.currentCwd}
            </span>
            <button className="folder-sheet-pin-current" onClick={pinCurrent}>
              {PIN_ICON}
            </button>
          </div>
        )}

        {/* Search */}
        <div className="folder-sheet-search">
          <span className="folder-sheet-search-icon">{SEARCH_ICON}</span>
          <input
            type="text"
            placeholder={t("folder.searchPlaceholder")}
            aria-label={t("folder.searchPlaceholder")}
            value={query}
            onChange={(e) => setQuery(e.target.value)}
          />
          {query && (
            <button className="folder-sheet-search-clear" aria-label={t("dash.clearSearch")} onClick={() => setQuery("")}>
              {"\u2715"}
            </button>
          )}
        </div>

        {/* Tabs */}
        <div className="folder-sheet-tabs">
          <button className={`folder-tab ${tab === "fav" ? "active" : ""}`} onClick={() => { platform().haptic(); setTab("fav"); }}>
            {STAR_ICON} {t("folder.tab.fav")}
          </button>
          <button className={`folder-tab ${tab === "recent" ? "active" : ""}`} onClick={() => { platform().haptic(); setTab("recent"); }}>
            {CLOCK_ICON} {t("folder.tab.recent")}
          </button>
          <button className={`folder-tab ${tab === "projects" ? "active" : ""}`} onClick={() => { platform().haptic(); setTab("projects"); }}>
            {GIT_ICON} {t("folder.tab.projects")}
          </button>
          {/* При недоступном ПК в «Обзор» не ходим: тело шторки уже объясняет
              причину, а запрос добавил бы вторую жалобу тостом. */}
          <button className={`folder-tab ${tab === "browse" ? "active" : ""}`} onClick={() => { platform().haptic(); setTab("browse"); if (!browsePath && !loadFailed) browseDir(props.currentCwd || "."); }}>
            {FOLDER_ICON} {t("folder.tab.browse")}
          </button>
        </div>

        {/* Чем эта вкладка отличается от соседних — прямо под их рядом.
            Владелец 01.09.2026: «избранное недавняя проекты обзор — там
            как-то непонятно что». Четыре ярлыка сами по себе не объясняют
            ничего: «Проекты» ничего не говорят о git, а «Недавние» человек
            читает как историю. Ответ стоит там, где возникает вопрос. */}
        <div className="folder-tab-hint">{t(`folder.tabHint.${tab}`)}</div>

        <div className="folder-list-controls">
          <select className="folder-sort-select" aria-label={t("files.sort")} value={sorts[tab]} onChange={e => changeSort(e.target.value as FolderSort)}>
            <option value="name-asc">{t("folder.sort.nameAsc")}</option>
            <option value="name-desc">{t("folder.sort.nameDesc")}</option>
            {tab !== "fav" && <option value="date-desc">{t(tab === "recent" ? "folder.sort.visited" : "folder.sort.modified")}</option>}
          </select>
          <button className="folder-filters-toggle" aria-expanded={filtersOpen} aria-controls={filtersId} onClick={() => setFiltersOpen(value => !value)}>
            <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" aria-hidden="true"><path d="M4 7h16M4 17h16" /><circle cx="9" cy="7" r="3" fill="var(--tg-secondary-bg)" /><circle cx="15" cy="17" r="3" fill="var(--tg-secondary-bg)" /></svg>
            {t("folder.filters")}{filterCount > 0 && <span className="folder-filter-count">{filterCount}</span>}
          </button>
        </div>

        {/* ── Content ──────────────────────────────── */}
        <div className="folder-sheet-body">
          {filtersOpen && (
            <div className="folder-filter-panel" id={filtersId} role="group" aria-label={t("folder.filters")}>
              {tab === "browse" && <label><input className="folder-filter-dot" type="checkbox" checked={hideDotFolders} onChange={e => setHideDotFolders(e.target.checked)} />{t("folder.hideDotFolders")}</label>}
              {tab !== "fav" && <label><input className="folder-filter-favorites" type="checkbox" checked={favoritesOnly} onChange={e => setFavoritesOnly(e.target.checked)} />{t("folder.favoritesOnly")}</label>}
              <label><input className="folder-filter-path" type="checkbox" checked={searchPath} onChange={e => setSearchPath(e.target.checked)} />{t("folder.searchPath")}</label>
              <button className="folder-reset-filters" onClick={() => { setHideDotFolders(true); setFavoritesOnly(false); setSearchPath(true); }}>{t("folder.resetFilters")}</button>
            </div>
          )}
          {/* ПК не ответил ни на один запрос: одна честная причина вместо
              четырёх пустых вкладок. Кнопка перечитывает списки. */}
          {loadFailed && (loadFailed.offline ? (
            <OfflineState onRetry={() => { void loadLists(); }} />
          ) : (
            <div className="folder-empty">
              {loadFailed.msg}
              <div style={{ marginTop: 10 }}>
                <button className="btn btn-secondary btn-sm" onClick={() => { void loadLists(); }}>
                  {t("conn.retry")}
                </button>
              </div>
            </div>
          ))}

          {/* Favorites tab = quick paths + bookmarks */}
          {!loadFailed && tab === "fav" && (
            <>
              {filteredQuick.length > 0 && (
                <>
                  <div className="folder-section">{t("pty.quickAccess")}</div>
                  <div className="folder-grid">
                    {filteredQuick.map((qp) => (
                      <button
                        key={`${qp.name}:${qp.path}`}
                        className="folder-tile"
                        onClick={() => browseInto(qp.path)}
                        onPointerDown={() => onItemPressStart(qp.path, qp.name)}
                        onPointerUp={onItemPressEnd}
                        onPointerLeave={onItemPressEnd}
                        onContextMenu={(e) => onItemContext(e, qp.path, qp.name)}
                      >
                        <span className="folder-tile-icon">{quickIcons[qp.name] || "\uD83D\uDCBE"}</span>
                        <span className="folder-tile-label">{folderLabel(qp.name)}</span>
                      </button>
                    ))}
                  </div>
                </>
              )}

              {filteredBookmarks.length > 0 ? (
                <>
                  <div className="folder-section">{t("files.bookmarks")}</div>
                  <div className="folder-list">
                    {filteredBookmarks.map((bm) => (
                      <div
                        key={bm.path}
                        className="folder-row"
                        onClick={() => browseInto(bm.path)}
                        onPointerDown={() => onItemPressStart(bm.path, bm.name)}
                        onPointerUp={onItemPressEnd}
                        onPointerLeave={onItemPressEnd}
                        onContextMenu={(e) => onItemContext(e, bm.path, bm.name)}
                      >
                        <span className="folder-row-icon">{PIN_ICON}</span>
                        <div className="folder-row-info">
                          <div className="folder-row-name">{bm.name}</div>
                          <div className="folder-row-path">{bm.path}</div>
                        </div>
                        {rowAction(bm.path)}
                        <span className="folder-row-chevron">{"\u203A"}</span>
                      </div>
                    ))}
                  </div>
                </>
              ) : filteredQuick.length === 0 && (
                <div className="folder-empty">{query.trim() ? t("folder.noMatches") : t("folder.noFavorites")}</div>
              )}
            </>
          )}

          {!loadFailed && tab === "recent" && (
            filteredRecent.length > 0 ? (
              <div className="folder-list">
                {filteredRecent.map((rf) => (
                  <div
                    key={rf.path}
                    className="folder-row"
                    onClick={() => browseInto(rf.path)}
                    onPointerDown={() => onItemPressStart(rf.path, rf.name)}
                    onPointerUp={onItemPressEnd}
                    onPointerLeave={onItemPressEnd}
                    onContextMenu={(e) => onItemContext(e, rf.path, rf.name)}
                  >
                    <span className="folder-row-icon">{CLOCK_ICON}</span>
                    <div className="folder-row-info">
                      <div className="folder-row-name">{rf.name}</div>
                      <div className="folder-row-path">{rf.path}</div>
                    </div>
                    {rowAction(rf.path)}
                    <span className="folder-row-chevron">{"\u203A"}</span>
                  </div>
                ))}
              </div>
            ) : (
              <div className="folder-empty">{query ? t("folder.noMatches") : t("folder.noRecent")}</div>
            )
          )}

          {!loadFailed && tab === "projects" && (
            projectsLoading ? (
              <div className="loading-center" style={{ padding: 32 }}>
                <div className="spinner" />
              </div>
            ) : filteredProjects.length > 0 ? (
              <div className="folder-list">
                {filteredProjects.map((pr) => (
                  <div
                    key={pr.path}
                    className="folder-row"
                    onClick={() => browseInto(pr.path)}
                    onPointerDown={() => onItemPressStart(pr.path, pr.name)}
                    onPointerUp={onItemPressEnd}
                    onPointerLeave={onItemPressEnd}
                    onContextMenu={(e) => onItemContext(e, pr.path, pr.name)}
                  >
                    <span className="folder-row-icon">{GIT_ICON}</span>
                    <div className="folder-row-info">
                      <div className="folder-row-name">{pr.name}</div>
                      <div className="folder-row-path">{pr.path}</div>
                    </div>
                    {rowAction(pr.path)}
                    <span className="folder-row-chevron">{"\u203A"}</span>
                  </div>
                ))}
              </div>
            ) : (
              <div className="folder-empty">
                {query ? t("folder.noMatches") : t("folder.noProjects")}
              </div>
            )
          )}

          {!loadFailed && tab === "browse" && (
            <>
              <div className="folder-crumbs">
                <button className="folder-crumb" onClick={() => browseDir("/")}>
                  {"/"}
                </button>
                {pathSegments.map((seg, i) => {
                  const sep = browsePath.includes("/") ? "/" : "\\";
                  const clickPath = pathSegments.slice(0, i + 1).join(sep);
                  const resolved = i === 0 && sep === "\\" ? clickPath + sep : clickPath;
                  return (
                    <span key={i} className="folder-crumb-wrap">
                      <span className="folder-crumb-sep">{"\u203A"}</span>
                      <button className="folder-crumb" onClick={() => browseDir(resolved)}>{seg}</button>
                    </span>
                  );
                })}
              </div>

              {/* Browse toolbar: Up / Refresh / + New Folder */}
              <div className="folder-browse-toolbar">
                {browseParent && browseParent !== browsePath && (
                  <button className="folder-browse-tool" onClick={() => browseDir(browseParent)}>
                    {UP_ICON} {t("files.up")}
                  </button>
                )}
                <button
                  className="folder-browse-tool"
                  onClick={() => browseDir(browsePath)}
                  title={t("files.refresh")}
                >
                  {"↻"}
                </button>
                <button
                  className="folder-browse-tool folder-browse-tool-primary"
                  onClick={() => { platform().haptic(); setNewFolderName(""); setShowNewFolder(true); }}
                >
                  {"➕"} {t("files.newFolder")}
                </button>
              </div>

              <div className="folder-browse-input">
                <button className="folder-browse-toggle" onClick={() => setShowPathInput(!showPathInput)}>
                  {showPathInput ? "\u2303" : "\u270F\uFE0F"} {t("pty.enterPath")}
                </button>
                {showPathInput && (
                  <div style={{ display: "flex", gap: 8, marginTop: 6 }}>
                    <input
                      className="modal-input"
                      style={{ flex: 1, marginBottom: 0 }}
                      placeholder={t("pty.pathPlaceholder")}
                      value={pathInput}
                      onChange={(e) => setPathInput(e.target.value)}
                      onKeyDown={(e) => { if (e.key === "Enter" && pathInput.trim()) browseDir(pathInput.trim()); }}
                      autoFocus
                    />
                    <button className="btn btn-primary btn-sm" onClick={() => { if (pathInput.trim()) browseDir(pathInput.trim()); }}>
                      {t("generic.go")}
                    </button>
                  </div>
                )}
              </div>

              {browseTruncated && <div className="folder-results-note">{t("folder.partialResults")}</div>}

              {browseLoading ? (
                <div className="loading-center" style={{ padding: 32 }}>
                  <div className="spinner" />
                </div>
              ) : (
                <div className="folder-list">
                  {/* ".." скрываем при активном поиске — это шум в отфильтрованном списке. */}
                  {!query && browseParent && browseParent !== browsePath && (
                    <div className="folder-row" onClick={() => browseDir(browseParent)}>
                      <span className="folder-row-icon">{UP_ICON}</span>
                      <div className="folder-row-info">
                        <div className="folder-row-name">..</div>
                      </div>
                    </div>
                  )}
                  {/* Папка пуста — предложить создать; список отфильтрован поиском в ноль — «нет совпадений». */}
                  {browseItems.length === 0 && !browseLoading && (
                    <div className="folder-empty">
                      {t("modal.noSubfolders")}
                      <div style={{ marginTop: 10 }}>
                        <button
                          className="btn btn-primary btn-sm"
                          onClick={() => { platform().haptic(); setNewFolderName(""); setShowNewFolder(true); }}
                        >
                          {"➕"} {t("files.newFolder")}
                        </button>
                      </div>
                    </div>
                  )}
                  {browseItems.length > 0 && filteredBrowse.length === 0 && (
                    <div className="folder-empty">{t("folder.noMatches")}</div>
                  )}
                  {filteredBrowse.map((item) => (
                    <div
                      key={item.path}
                      className="folder-row"
                      onClick={() => browseDir(item.path)}
                      onPointerDown={() => onItemPressStart(item.path, item.name)}
                      onPointerUp={onItemPressEnd}
                      onPointerLeave={onItemPressEnd}
                      onContextMenu={(e) => onItemContext(e, item.path, item.name)}
                    >
                      <span className="folder-row-icon">{FOLDER_ICON}</span>
                      <div className="folder-row-info">
                        <div className="folder-row-name">{item.name}</div>
                      </div>
                      <span className="folder-row-chevron">{"\u203A"}</span>
                    </div>
                  ))}
                </div>
              )}

            </>
          )}
        </div>
        {/* Keep the current-folder action reachable while its list scrolls. */}
        {!loadFailed && tab === "browse" && (
              <div className="folder-browse-actions">
                <button
                  className="btn btn-primary"
                  style={{ flex: 1 }}
                  onClick={() => { void pick(browsePath); }}
                  disabled={picking || browseLoading || !browsePath}
                >
                  {props.pickLabel || t("folder.cdHere")}
                </button>
                {props.onOpenHere && (
                  <button className="btn btn-secondary" style={{ flex: 1 }} disabled={picking || browseLoading || !browsePath} onClick={() => openHere(browsePath)}>
                    {t("pty.openHere")}
                  </button>
                )}
              </div>
        )}
    </SheetShell>
  );
}
