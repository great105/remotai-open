import { useEffect, useRef, useState } from "react";
import { listFiles, downloadUrl, sendToTelegram } from "../api-endpoints";
import type { FileItem } from "../types";
import { platform } from "../platform";
import { useToast } from "./Toast";
import { useEscape } from "../hooks/useEscape";
import { t } from "../i18n";
import { ApiError, mapApiError } from "../api-core";
import { humanSize } from "../humanSize";
import { confirmDialog } from "../dialog";
import { SheetShell } from "./DialogHost";

/**
 * Как качать файл. Ровно то же, что принимает downloadBlob в apk/src/api.ts:
 * размер и mtime из списка (чтобы качать кусками и сверять, что файл не
 * подменили), сигнал отмены и прогресс.
 */
export interface FetchBlobOpts {
  size?: number;
  mtime?: number;
  signal?: AbortSignal;
  onProgress?: (done: number, total: number) => void;
}

interface Props {
  open: boolean;
  onClose: () => void;
  initialPath: string;
  /**
   * Получить байты файла. apk передаёт downloadBlob (cloud-ветка + LAN-auth);
   * дефолт — прямой fetch по downloadUrl (auth через ?initData= в URL — работает
   * cross-origin без кастомных заголовков; так делал miniapp).
   */
  fetchBlob?: (path: string, opts?: FetchBlobOpts) => Promise<Blob>;
  /**
   * Показывать «Отправить в Telegram» в контекст-меню. apk передаёт
   * useBotAvailable() (cloud-инсталляции обычно без бота); дефолт true.
   */
  botAvailable?: boolean;
  /** Платформенный путь доставки (cloud relay bot / локальный own_bot). */
  sendTelegram?: (path: string, name?: string) => Promise<unknown>;
}

const FOLDER_ICON = "📁";
const FILE_ICON = "📄";
const UP_ICON = "⬆";
const DOWN_ICON = "⬇️";
const TG_ICON = "✈️";
const SEARCH_ICON = "🔍";
const WAIT_ICON = "⏳";

/**
 * Порог «большого файла» — тот же, что в «Файлах» (apk/src/pages/FilesView.tsx).
 * Смысл вопроса: трафик тратится задолго до того, как выяснится, что телефон
 * файл не сохранил, поэтому размер называем ДО старта.
 */
const BIG_DOWNLOAD = 30 * 1024 * 1024;

/**
 * Внутри Telegram сохранение файла на телефон ненадёжно (мобильный WebView
 * молча теряет blob-download), а доставка ботом работает всегда — поэтому там
 * «Отправить в Telegram» стоит первой кнопкой меню (находка N117).
 */
function inTelegram(): boolean {
  return !!(window as unknown as { Telegram?: { WebApp?: { initData?: string } } })
    ?.Telegram?.WebApp?.initData;
}

async function defaultFetchBlob(path: string, opts: FetchBlobOpts = {}): Promise<Blob> {
  const res = await fetch(downloadUrl(path), { signal: opts.signal });
  if (!res.ok) {
    // Раньше здесь бросался Error("HTTP 502") — в тост уходил машинный код, а
    // mapApiError не мог его разобрать (нет status/code). Теперь ошибка того же
    // вида, что и на обычных запросах: «Компьютер не в сети…» / «Нет прав…».
    const body = await res.json().catch(() => ({}) as { error?: string; code?: string });
    throw new ApiError(body.error || res.statusText, res.status, body.code);
  }
  const blob = await res.blob();
  // Одним запросом прогресса нет — отмечаем только завершение, чтобы строка
  // не осталась висеть на «0 %».
  opts.onProgress?.(blob.size, blob.size);
  return blob;
}

export function DownloadSheet({
  open, onClose, initialPath, fetchBlob, botAvailable = true, sendTelegram,
}: Props) {
  const { toastSuccess, toastError } = useToast();
  const [path, setPath] = useState(initialPath);
  const [items, setItems] = useState<FileItem[]>([]);
  const [parent, setParent] = useState<string | null>(null);
  const [loading, setLoading] = useState(false);
  const [query, setQuery] = useState("");
  const [manualPath, setManualPath] = useState("");
  const [menu, setMenu] = useState<FileItem | null>(null);
  const longPressTimer = useRef<number | null>(null);
  const longPressFired = useRef(false);

  // Скачивание идёт кусками (downloadBlob): большой файл одним запросом рвал
  // управляющий сокет ПК. Отсюда — процент и «Отмена» вместо строки, которая
  // «просто ничего не делает» (тот же путь, что в «Файлах»).
  const [dl, setDl] = useState<{ path: string; pct: number } | null>(null);
  const dlAbort = useRef<AbortController | null>(null);

  // Back/ESC: первый — закрыть контекст-меню файла, второй — саму шторку.
  useEscape(open && !!menu, () => setMenu(null));
  useEscape(open && !menu, onClose);

  // Sync path when sheet (re)opens
  useEffect(() => {
    if (!open) return;
    setPath(initialPath);
    setQuery("");
    setManualPath("");
  }, [open, initialPath]);

  // Load directory. cancelled-флаг: при быстрой смене папки медленный ответ на
  // СТАРЫЙ path не должен затирать содержимое нового.
  useEffect(() => {
    if (!open || !path) return;
    let cancelled = false;
    setLoading(true);
    listFiles(path)
      .then((d) => {
        if (cancelled) return;
        const sorted = [...(d.items || [])].sort((a, b) => {
          if (a.is_dir !== b.is_dir) return a.is_dir ? -1 : 1;
          return a.name.localeCompare(b.name);
        });
        setItems(sorted);
        setParent(d.parent || null);
      })
      .catch((e: any) => { if (!cancelled) toastError(mapApiError(e)); })
      .finally(() => { if (!cancelled) setLoading(false); });
    return () => { cancelled = true; };
  }, [open, path]);

  const enter = (p: string) => {
    platform().haptic();
    setPath(p);
    setQuery("");
  };

  const cancelDownload = () => {
    platform().haptic();
    dlAbort.current?.abort();
    dlAbort.current = null;
    setDl(null);
  };

  const triggerDownload = async (filePath: string, name: string, size?: number, mtime?: number) => {
    if (dl) return; // одно скачивание за раз: иначе процент не о чем рассказывает
    platform().haptic("medium");
    // Крупный файл: спрашиваем ДО старта и называем размер — иначе человек
    // узнаёт цену мегабайтами позже (та же логика в «Файлах», находка N110).
    if ((size ?? 0) >= BIG_DOWNLOAD) {
      const go = await confirmDialog(t("files.bigDownloadMsg", { size: humanSize(size!) }), {
        title: t("files.bigDownloadTitle", { size: humanSize(size!) }),
        confirmText: t("files.bigDownloadAnyway"),
        cancelText: t("modal.cancel"),
      });
      if (!go) return;
    }
    // Fetch the file as a Blob, then hand it to platform().saveBlob():
    // apk-native — Capacitor cache + system share sheet (Files / Drive /
    // Telegram / Gallery), Telegram/web — Web Share API with files, фолбэк
    // <a download>. Why not <a href="https://…" download>: WebView hands
    // cross-origin links to Chrome which has no auth token and gets 401, and
    // the Android DownloadManager also drops our header. blob + share is
    // portable.
    toastSuccess(t("download.starting", { name }));
    const ac = new AbortController();
    dlAbort.current = ac;
    setDl({ path: filePath, pct: 0 });
    try {
      const blob = await (fetchBlob ?? defaultFetchBlob)(filePath, {
        size: size || undefined,
        mtime: mtime || undefined,
        signal: ac.signal,
        onProgress: (done, total) => {
          if (total > 0) setDl({ path: filePath, pct: Math.round((done / total) * 100) });
        },
      });
      const how = await platform().saveBlob(blob, name);
      // "failed" — сохранить нечем (мобильный Telegram). Раньше тост об успехе
      // печатался безусловно, и человек искал несуществующий файл (N117).
      if (how === "failed") {
        platform().hapticError();
        if (botAvailable && await platform().confirm(t("download.saveFailedTg", { name }))) {
          await sendTg(filePath, name);
          return;
        }
        toastError(t("download.saveFailed", { name }));
        return;
      }
      platform().hapticSuccess();
      toastSuccess(how === "shared" ? t("download.shared", { name }) : t("download.started", { name }));
    } catch (e: any) {
      // Отмену человек инициировал сам — молчим, как и при отмене шаринга.
      if (ac.signal.aborted || platform().isSaveCancel(e)) return;
      toastError(mapApiError(e));
    } finally {
      if (dlAbort.current === ac) dlAbort.current = null;
      setDl((cur) => (cur?.path === filePath ? null : cur));
    }
  };

  const sendTg = async (filePath: string, name: string) => {
    try {
      await (sendTelegram ?? sendToTelegram)(filePath, name);
      platform().hapticSuccess();
      toastSuccess(t("download.sentTg", { name }));
    } catch (e: any) {
      toastError(mapApiError(e));
    }
  };

  const onItemTap = (item: FileItem) => {
    if (longPressFired.current) {
      longPressFired.current = false;
      return;
    }
    if (item.is_dir) enter(item.path);
    else void triggerDownload(item.path, item.name, item.size ?? undefined, item.modified ?? undefined);
  };

  const startLongPress = (item: FileItem) => {
    longPressFired.current = false;
    if (longPressTimer.current) window.clearTimeout(longPressTimer.current);
    longPressTimer.current = window.setTimeout(() => {
      longPressFired.current = true;
      platform().haptic("medium");
      setMenu(item);
    }, 500);
  };
  const cancelLongPress = () => {
    if (longPressTimer.current) {
      window.clearTimeout(longPressTimer.current);
      longPressTimer.current = null;
    }
  };

  const onManualDownload = () => {
    const p = manualPath.trim();
    if (!p) return;
    // Размер файла по одному пути неизвестен — вопрос о трафике задать не о чем,
    // но отмена и индикатор работают так же.
    void triggerDownload(p, p.split(/[\\/]/).pop() || "file");
    setManualPath("");
  };

  const filtered = query
    ? items.filter((i) => i.name.toLowerCase().includes(query.toLowerCase()))
    : items;

  if (!open) return null;

  /** Кнопка «идёт скачивание: N % · Отмена» на строке файла и в меню. */
  const progressLabel = (pct: number) => `${WAIT_ICON} ${pct}% · ${t("modal.cancel")}`;

  return (
    <>
      <SheetShell
        open={open}
        onClose={onClose}
        overlayClassName="folder-sheet-overlay"
        className="folder-sheet"
        labelledBy="download-sheet-title"
      >
        <div className="folder-sheet-header">
          <div className="folder-sheet-title" id="download-sheet-title">{DOWN_ICON} {t("download.title")}</div>
          <button className="folder-sheet-close" onClick={onClose}>{"✕"}</button>
        </div>

        <div className="folder-sheet-cwd">
          {parent && (
            <button className="folder-sheet-pin-current" onClick={() => enter(parent)} title={t("download.up")}>
              {UP_ICON}
            </button>
          )}
          <span className="folder-sheet-cwd-path" title={path}>{path}</span>
        </div>

        <div className="folder-sheet-search">
          <span className="folder-sheet-search-icon">{SEARCH_ICON}</span>
          <input
            type="text"
            placeholder={t("folder.searchPlaceholder")}
            value={query}
            onChange={(e) => setQuery(e.target.value)}
          />
          {query && (
            <button className="folder-sheet-search-clear" onClick={() => setQuery("")}>
              {"✕"}
            </button>
          )}
        </div>

        <div className="folder-sheet-body">
          {loading ? (
            <div className="folder-empty">{"…"}</div>
          ) : filtered.length === 0 ? (
            <div className="folder-empty">{t("download.empty")}</div>
          ) : (
            <div className="folder-list">
              {filtered.map((item) => {
                const busy = dl?.path === item.path;
                return (
                  <div
                    key={item.path}
                    className="folder-row"
                    onClick={() => onItemTap(item)}
                    onPointerDown={() => startLongPress(item)}
                    onPointerUp={cancelLongPress}
                    onPointerLeave={cancelLongPress}
                    onPointerCancel={cancelLongPress}
                    onContextMenu={(e) => { e.preventDefault(); cancelLongPress(); setMenu(item); }}
                  >
                    <span className="folder-row-icon">
                      {item.is_dir ? FOLDER_ICON : FILE_ICON}
                    </span>
                    <div className="folder-row-info">
                      <div className="folder-row-name">{item.name}</div>
                      {!item.is_dir && (
                        <div className="folder-row-path">{humanSize(item.size)}</div>
                      )}
                    </div>
                    {busy ? (
                      // Процент и отмена прямо на строке: 200 МБ по мобильной сети
                      // иначе выглядят как «тапнул и ничего не произошло».
                      <button
                        className="folder-row-dl"
                        onClick={(e) => { e.stopPropagation(); cancelDownload(); }}
                        onPointerDown={(e) => { e.stopPropagation(); cancelLongPress(); }}
                        title={t("modal.cancel")}
                      >
                        {progressLabel(dl!.pct)}
                      </button>
                    ) : (
                      <span className="folder-row-chevron">
                        {item.is_dir ? "›" : DOWN_ICON}
                      </span>
                    )}
                  </div>
                );
              })}
            </div>
          )}
        </div>

        <div className="folder-sheet-search" style={{ borderTop: "1px solid var(--tg-secondary-bg)", marginTop: 0 }}>
          <input
            type="text"
            placeholder={t("download.manualPlaceholder")}
            value={manualPath}
            onChange={(e) => setManualPath(e.target.value)}
            onKeyDown={(e) => { if (e.key === "Enter") onManualDownload(); }}
          />
          <button
            className="folder-sheet-search-clear"
            onClick={onManualDownload}
            disabled={!manualPath.trim() || !!dl}
            style={{ opacity: manualPath.trim() && !dl ? 1 : 0.4 }}
          >
            {DOWN_ICON}
          </button>
        </div>
      </SheetShell>

      {menu && (
        <SheetShell
          open={!!menu}
          onClose={() => setMenu(null)}
          overlayClassName="folder-menu-overlay"
          className="folder-menu"
          labelledBy="download-menu-title"
        >
          <div className="folder-menu-title" id="download-menu-title">{menu.name}</div>
          <div className="folder-menu-path">{humanSize(menu.size)}</div>
          {/* В Telegram доставка ботом — единственный надёжный способ забрать
              файл, поэтому она идёт первой; вне Telegram первым остаётся
              обычное скачивание. */}
          {botAvailable && inTelegram() && (
            <button
              className="folder-menu-item"
              onClick={() => { sendTg(menu.path, menu.name); setMenu(null); }}
            >
              {TG_ICON} {t("download.sendTg2")}
            </button>
          )}
          {dl?.path === menu.path ? (
            <button className="folder-menu-item" onClick={cancelDownload}>
              {progressLabel(dl.pct)}
            </button>
          ) : (
            <button
              className="folder-menu-item"
              disabled={!!dl}
              onClick={() => {
                void triggerDownload(menu.path, menu.name, menu.size ?? undefined, menu.modified ?? undefined);
                setMenu(null);
              }}
            >
              {DOWN_ICON} {t("download.downloadFile")}
            </button>
          )}
          {botAvailable && !inTelegram() && (
            <button
              className="folder-menu-item"
              onClick={() => { sendTg(menu.path, menu.name); setMenu(null); }}
            >
              {TG_ICON} {t("download.sendTg2")}
            </button>
          )}
          <button
            className="folder-menu-item cancel"
            onClick={() => setMenu(null)}
          >
            {t("modal.cancel")}
          </button>
        </SheetShell>
      )}
    </>
  );
}
