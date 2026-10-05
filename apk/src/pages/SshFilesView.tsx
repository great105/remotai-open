// SSH-файлы (SFTP): файловый менеджер удалённого сервера через агент.
// Вход — кнопка «Файлы» у хоста в секции «Серверы» (PtyListView): параметры
// сервера приходят в state навигации; сохранённый пароль подставляет агент.
// Дизайн повторяет FilesView (fm-* классы), но компонент самостоятельный —
// здесь нет ни закладок, ни поиска, ни агентов, только SSH.

import { useCallback, useEffect, useRef, useState } from "react";
import { Navigate, useNavigate, useSearchParams } from "react-router-dom";
import { useEscape, useToast, mapApiError, isPcOffline, OfflineState, transport, humanSize, getLanguage, getLocale } from "@tgcontrol/shared";
import {
  sftpList, sftpPreview, sftpMkdir, sftpDelete, sftpRename, sshSftpDownloadBlob,
  sftpPull, sftpTransfers, sftpCancelTransfer, getSshHosts, unlockSshHost, forgetSshHostSecret,
  sendToTelegram,
} from "../api";
import type { SftpConn, SftpEntry, SshTransfer, SftpPreview, SshHost } from "../api";
import { t } from "../i18n";
import { FolderNavSheet } from "@tgcontrol/shared";
import { haptic, hapticSuccess, hapticError } from "../telegram";
import { saveBlob, isShareCancel } from "../saveFile";
import { useBackHandler } from "../hooks/backHandler";
import { useBotAvailable } from "../hooks/useBotAvailable";
import { IconEye, IconEyeOff } from "../components/icons";
import { BottomNav } from "../components/BottomNav";
import { ServerAccessNotice } from "../components/ServerAccessNotice";
import { HoldButton } from "../components/HoldButton";
import {
  getSshPassword, runWithSshTrust, setSshPassword, sshApiText, sshErrorText, sshTargetLabel,
} from "../sshCommon";
import { getMode } from "../config";
// Тот же путь в терминал, что у «⌨ Терминал» в списке серверов: одна логика
// подключения на обе двери (аудит ИА 02.09.2026, E4/E5).
import { connectSshHost } from "../components/SshSection";
import type { SshCredentials } from "../components/SshSection";

// Размеры печатает общий humanSize (@tgcontrol/shared): свой форматтер писал
// «1.4 KB» латиницей и с точкой, тогда как «Файлы» ПК, «Система» и шторка
// скачивания говорят «1,4 КБ». Одни и те же байты не могут называться в
// приложении по-разному, поэтому локальная копия удалена.

// Дата файла на сервере. Без года «12.03 09:15» одинаково выглядело и у
// вчерашнего файла, и у файла 2019 года — выбрать «последний дамп» глазами было
// нечем. Формула та же, что в «Файлах» ПК (FilesView.formatDate): сегодняшний
// файл — «сегодня 09:15», прошлые годы — с годом и без времени (год важнее
// минут), текущий год — как раньше.
function formatModTime(iso: string): string {
  const ms = Date.parse(iso);
  if (!Number.isFinite(ms)) return "";
  const d = new Date(ms);
  const now = new Date();
  if (getLanguage() === "en") {
    if (d.getFullYear() !== now.getFullYear()) {
      return new Intl.DateTimeFormat(getLocale(), { month: "short", day: "numeric", year: "numeric" }).format(d);
    }
    const time = new Intl.DateTimeFormat(getLocale(), { hour: "2-digit", minute: "2-digit", hour12: false }).format(d);
    if (d.getMonth() === now.getMonth() && d.getDate() === now.getDate()) return `${t("files.dateToday")} ${time}`;
    return `${new Intl.DateTimeFormat(getLocale(), { month: "short", day: "numeric" }).format(d)} ${time}`;
  }
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

/**
 * Куда ведёт «Назад».
 *
 * Экран всегда выбрасывал в список серверов, откуда бы человек ни пришёл: из
 * SSH-терминала («Скачать» и скрепка ведут сюда) возврат означал потерю
 * открытой сессии из виду — её приходилось искать в списке терминалов заново.
 * Источник перехода приезжает параметром from; принимаем только внутренний
 * путь приложения (`/что-то`), иначе кнопка «Назад» стала бы дырой наружу.
 */
function backTarget(from: string | null): string {
  return from && /^\/[^/\\]/.test(from) ? from : "/ssh";
}

function joinRemote(dir: string, name: string): string {
  if (!dir || dir === "/") return "/" + name;
  return dir.replace(/\/+$/, "") + "/" + name;
}

function parentOf(path: string): string | null {
  const clean = path.replace(/\/+$/, "");
  if (!clean || clean === "/") return null;
  const i = clean.lastIndexOf("/");
  return i <= 0 ? "/" : clean.slice(0, i);
}

// Ошибка авторизации SSH → предложить ввести пароль и повторить.
function isAuthError(e: any): boolean {
  return /auth|парол|password|permission denied/i.test(e?.message || "");
}

/**
 * Причина провала переноса человеческим языком.
 *
 * Агент отдаёт машинный код (no_permission, disk_full, io_error…) и свой
 * англоязычный текст. Показываем только перевод по коду: строка вида
 * «sftp pull: open /var/dump.sql: permission denied» человеку бесполезна.
 */
function transferReason(tr: SshTransfer): string {
  if (!tr.code && !tr.error) return "";
  return sshApiText({ code: tr.code, message: tr.error }, "");
}

/**
 * Загрузка файлов на сервер с ответом на вопрос «а он там уже есть».
 *
 * Агент нарочно отбивает одноимённый файл кодом already_exists: корзины на
 * сервере нет, а SFTP-запись обрезает цель молча. Общая обёртка sftpUpload
 * (packages/shared) параметра overwrite не знает — поэтому запрос к той же
 * ручке собираем здесь, поля соединения повторяют sftpQuery. Когда overwrite
 * переедет в общую обёртку, этот помощник уйдёт целиком.
 */
async function uploadToServer(
  c: SftpConn,
  dir: string,
  files: File[],
  onProgress: (pct: number) => void,
  overwrite: boolean,
  signal?: AbortSignal,
): Promise<{ files?: string[]; failed?: { name: string; code?: string }[] }> {
  const q = new URLSearchParams({
    host: c.host,
    port: String(c.port || 22),
    user: c.user,
    path: dir,
  });
  if (c.password) q.set("password", c.password);
  if (c.host_id) q.set("host_id", c.host_id);
  if (c.identity_file) q.set("identity_file", c.identity_file);
  if (c.key_passphrase) q.set("key_passphrase", c.key_passphrase);
  if (c.proxy_jump) q.set("proxy_jump", c.proxy_jump);
  if (c.proxy_password) q.set("proxy_password", c.proxy_password);
  if (c.trust_host) q.set("trust_host", "1");
  if (overwrite) q.set("overwrite", "1");
  const form = new FormData();
  for (const f of files) form.append("file", f, f.name);
  // signal — «Отмена» на самой кнопке: транспорт обрывает XHR (api.ts,
  // xhrUpload) и отвечает кодом aborted.
  return transport().uploadForm(`/api/ssh/sftp/upload?${q.toString()}`, form, onProgress, { signal });
}

export function SshFilesView() {
  const [searchParams] = useSearchParams();
  const navigate = useNavigate();
  const hostID = searchParams.get("host") || "";
  const back = backTarget(searchParams.get("from"));
  const [host, setHost] = useState<SshHost | null>(null);
  // Раньше и «сервер удалён», и «список не получен» давали один молчаливый
  // редирект на /ssh: при спящем ПК человек просто вылетал с экрана без
  // объяснения. Теперь исходы разделены и у каждого есть действие.
  const [phase, setPhase] = useState<"loading" | "ok" | "missing" | "offline" | "error">("loading");
  const [errText, setErrText] = useState("");
  const seq = useRef(0);

  const load = useCallback(() => {
    if (!hostID) return;
    const my = ++seq.current;
    setPhase("loading");
    getSshHosts()
      .then(({ hosts }) => {
        if (my !== seq.current) return;
        const found = (hosts || []).find((h) => h.id === hostID) || null;
        setHost(found);
        setPhase(found ? "ok" : "missing");
      })
      .catch((e) => {
        if (my !== seq.current) return;
        if (isPcOffline(e)) { setPhase("offline"); return; }
        setErrText(sshApiText(e, t("ssh.listFailed")));
        setPhase("error");
      });
  }, [hostID]);

  useEffect(() => { load(); }, [load]);

  if (!hostID) return <Navigate to="/ssh" replace />;
  if (phase === "loading") return <div className="loading-center"><div className="spinner" /></div>;
  if (phase === "ok" && host) return <SshFilesInner host={host} />;

  return (
    <div className="page">
      <div className="page-header">
        <button className="back-btn" onClick={() => navigate(back)} aria-label={t("generic.back")}>{"←"}</button>
        <h1 style={{ flex: 1, fontSize: 16 }}>{t("ssh.hostTitle")}</h1>
      </div>
      <div className="page-content">
        {phase === "offline" ? (
          <OfflineState
            onRetry={() => { haptic(); load(); }}
            onDevices={getMode() === "cloud" ? () => { haptic(); navigate("/devices"); } : undefined}
          />
        ) : phase === "missing" ? (
          <div className="ssh-empty-block">
            <div>{t("ssh.hostGone")}</div>
            <button className="btn btn-secondary btn-sm" onClick={() => { haptic(); navigate("/ssh"); }}>
              {t("ssh.allHosts")}
            </button>
          </div>
        ) : (
          <div className="ssh-empty-block">
            <div>{errText}</div>
            <button className="btn btn-secondary btn-sm" onClick={() => { haptic(); load(); }}>
              {t("ssh.retry")}
            </button>
          </div>
        )}
      </div>
      {/* Файлы сервера — часть SSH-центра, а не «Терминала»: раньше нижняя
          панель подсвечивала соседнюю вкладку и человек читал её как «я в
          терминале». */}
      <BottomNav active="ssh" />
    </div>
  );
}

function SshFilesInner({ host }: { host: SshHost }) {
  const navigate = useNavigate();
  const [searchParams, setSearchParams] = useSearchParams();
  const { toastSuccess, toastError } = useToast();
  // Файл с сервера умеет уехать в Telegram-чат только с ПК (мост релея знает
  // лишь пути ПК), поэтому кнопка появляется на завершённом переносе.
  const botAvailable = useBotAvailable();
  const conn = {
    host_id: host.id,
    host: host.host,
    port: host.port || 22,
    user: host.user,
    identity_file: host.identity_file,
    proxy_jump: host.proxy_jump,
  };
  const [password, setPassword] = useState(getSshPassword(host));
  const [keyPassphrase, setKeyPassphrase] = useState("");
  const [proxyPassword, setProxyPassword] = useState("");
  const secretRef = useRef({ password: getSshPassword(host), keyPassphrase: "", proxyPassword: "" });
  // Откуда пришли — запоминаем один раз: адрес папки переписывается на каждом
  // переходе внутрь (setSearchParams ниже), и параметр from иначе терялся бы
  // после первого же клика по папке.
  const fromRef = useRef(searchParams.get("from") || "");
  const back = backTarget(fromRef.current || null);
  const initialPath = searchParams.get("path") || localStorage.getItem(`ssh.lastPath.${host.id}`) || "";
  const [path, setPath] = useState(initialPath);
  const [entries, setEntries] = useState<SftpEntry[]>([]);
  const [loading, setLoading] = useState(true);
  const [contextItem, setContextItem] = useState<SftpEntry | null>(null);
  const [showMkdir, setShowMkdir] = useState(false);
  const [mkdirName, setMkdirName] = useState("");
  const [renameItem, setRenameItem] = useState<SftpEntry | null>(null);
  const [renameTo, setRenameTo] = useState("");
  const [uploading, setUploading] = useState(false);
  const [uploadPct, setUploadPct] = useState<number | null>(null);
  // Отправку на сервер тоже надо уметь прервать: файл на 200 МБ по мобильному
  // интернету идёт минутами, а кнопка «⏳ Загрузка…» была неотменяемой — оставался
  // только выход с экрана, после которого запрос всё равно продолжал жить.
  const uploadAbort = useRef<AbortController | null>(null);
  // Скачивание идёт кусками (api.ts): процент и «Отмена» вместо кнопки, которая
  // просто показывает ⏳ минутами, а в облаке гарантированно рвётся на 60-й
  // секунде — ровно как уже сделано для файлов ПК.
  const [dl, setDl] = useState<{ name: string; pct: number } | null>(null);
  const dlAbort = useRef<AbortController | null>(null);
  // Сохранить байты в память телефона удаётся не везде: в мобильном Telegram
  // нечем (saveFile.ts вернёт "failed"). Вместо вибрации успеха — честная
  // плашка и работающий обход через компьютер.
  const [saveFailed, setSaveFailed] = useState<SftpEntry | null>(null);
  // Плашка идущего переноса занимает низ экрана — знать о ней надо и до её
  // объявления ниже по файлу (обе полосы position:fixed на одной высоте).
  const transferRef = useRef<SshTransfer | null>(null);
  // Повтор операции после ввода пароля.
  const [pwOpen, setPwOpen] = useState(false);
  const [pwDraft, setPwDraft] = useState("");
  const [keyPassDraft, setKeyPassDraft] = useState("");
  const [proxyPwDraft, setProxyPwDraft] = useState("");
  const [rememberSecret, setRememberSecret] = useState(true);
  // Пароль ключа и пароль бастиона — вторичные поля диалога.
  const [moreAuthOpen, setMoreAuthOpen] = useState(false);
  const retryRef = useRef<(() => void) | null>(null);
  const requestSeq = useRef(0);
  const [showHidden, setShowHidden] = useState(
    () => localStorage.getItem(`ssh.hidden.${host.id}`) === "1",
  );
  const hiddenRef = useRef(showHidden);
  const [filter, setFilter] = useState("");
  // Порядок считает СЕРВЕР: он сортирует весь каталог и только потом режет
  // лимит в 2000 записей, поэтому «по размеру» на клиенте упорядочивало бы
  // произвольную алфавитную выборку. sortRef — чтобы loadDir не пересоздавался.
  const [sort, setSort] = useState<"name" | "size" | "date">("name");
  const sortRef = useRef(sort);
  const [serverSorted, setServerSorted] = useState(false);
  const [manualPath, setManualPath] = useState(initialPath);
  const [total, setTotal] = useState(0);
  const [truncated, setTruncated] = useState(false);
  const [preview, setPreview] = useState<{ item: SftpEntry; data: SftpPreview } | null>(null);
  // Отказ листинга папки: раньше любая ошибка (спящий ПК, нет прав, неверный
  // пароль после «Отмены») рисовалась как 📂 «Пустая папка» + «Загрузить» —
  // экран уверял, что подключились и на сервере ничего нет.
  const [dirError, setDirError] = useState<{ offline: boolean; auth: boolean; text: string } | null>(null);
  // «Помнить до перезапуска ПК» действует ровно на одну следующую попытку.
  const rememberOnceRef = useRef(false);
  // «Терминал сервера» в шапке — обратная дверь к «📁 Файлы» у строки сервера
  // (аудит ИА 02.09.2026, E4/E5): из файлов в терминал того же хоста без
  // возврата в список. Подключение общее (connectSshHost); пароль, если он
  // понадобится, спрашивает уже существующая здесь модалка — резолвер её
  // ответа живёт в ref, как в SshSection.
  const [termBusy, setTermBusy] = useState(false);
  const termBusyRef = useRef(false);
  const pwResolverRef = useRef<((credentials: SshCredentials | null) => void) | null>(null);

  /**
   * Честный ответ на обещание «помню пароль».
   *
   * Новый агент запоминает секрет сам при листинге с remember=1 и отвечает
   * secret_remembered. Агент постарше про этот параметр не знает — тогда просим
   * его отдельной ручкой и ЖДЁМ ответа: раньше вызов уходил в `.catch(() => {})`,
   * и человек вводил пароль заново, хотя интерфейс обещал его помнить.
   *
   * Своих секретов в памяти страницы может уже не быть (например, повтор после
   * возврата на экран) — тогда спрашиваем агент пустым запросом «ты ещё
   * помнишь?»: он ответит либо «помню», либо кодом secret_not_remembered,
   * который словарь показывает словами. Агент постарше такого вопроса не знает
   * и отвечает bad_request — проверять нечем, и пугать человека нечем.
   */
  const confirmRemembered = async (flag: boolean | undefined) => {
    if (flag === true) return;
    if (flag === false) { toastError(t("ssh.rememberFailed")); return; }
    const secrets = secretRef.current;
    const hasSecrets = !!(secrets.password || secrets.keyPassphrase || secrets.proxyPassword);
    try {
      const res = await unlockSshHost(host.id, hasSecrets ? {
        password: secrets.password || undefined,
        key_passphrase: secrets.keyPassphrase || undefined,
        proxy_password: secrets.proxyPassword || undefined,
      } : {});
      if (!res?.unlocked) toastError(t("ssh.rememberFailed"));
    } catch (e: any) {
      if (!hasSecrets && e?.code === "bad_request") return;
      toastError(sshApiText(e, t("ssh.rememberFailed")));
    }
  };

  const loadDir = useCallback(async (dir: string, pw?: string) => {
    const seq = ++requestSeq.current;
    setLoading(true);
    setContextItem(null);
    // «Помнить до перезапуска ПК» — только на попытке сразу после диалога
    // пароля: агент кладёт секрет в память ТОЛЬКО после удачного листинга и
    // отвечает secret_remembered, поэтому неверный пароль там не застревает.
    const remember = rememberOnceRef.current;
    rememberOnceRef.current = false;
    try {
      const d = await runWithSshTrust(host, (trust) => sftpList({
        ...conn,
        password: pw ?? secretRef.current.password ?? password,
        key_passphrase: secretRef.current.keyPassphrase || keyPassphrase || undefined,
        proxy_password: secretRef.current.proxyPassword || proxyPassword || undefined,
        trust_host: trust || undefined,
      }, dir, hiddenRef.current, { sort: sortRef.current, remember }));
      if (seq !== requestSeq.current) return;
      const cwd = d.cwd || dir || "/";
      setEntries(d.entries || []);
      setTotal(d.total ?? d.entries?.length ?? 0);
      setTruncated(!!d.truncated);
      // Эхо порядка есть только у нового агента. Пришло — значит каталог
      // отсортирован целиком, и локально пересортировывать список НЕЛЬЗЯ:
      // иначе «по размеру» опять упорядочит одну обрезанную страницу.
      setServerSorted(!!d.sort);
      setPath(cwd);
      setManualPath(cwd);
      setDirError(null);
      // Обещание «помню пароль» подтверждает только сервер.
      if (remember) void confirmRemembered(d.secret_remembered);
      localStorage.setItem(`ssh.lastPath.${host.id}`, cwd);
      const nextQuery = new URLSearchParams({ host: host.id, path: cwd });
      if (fromRef.current) nextQuery.set("from", fromRef.current);
      setSearchParams(nextQuery, { replace: true });
    } catch (e: any) {
      if (seq !== requestSeq.current) return;
      hapticError();
      const offline = isPcOffline(e);
      const authish = !offline &&
        (e?.code === "auth_failed" || e?.code === "key_encrypted" || isAuthError(e));
      setDirError({ offline, auth: authish, text: offline ? "" : sshErrorText(e) });
      // Список папки не приехал — плашка «показаны первые N из M» относилась бы
      // к прошлой папке, которой на экране уже нет.
      setTruncated(false);
      if (authish && !(pw ?? password) && !keyPassphrase) {
        retryRef.current = () => loadDir(dir);
        setKeyPassDraft("");
        setProxyPwDraft("");
        // Сервер сказал «ключ зашифрован» — нужное поле открываем сразу.
        setMoreAuthOpen(e?.code === "key_encrypted");
        setPwOpen(true);
      } else if (!offline) {
        // При офлайне тост не нужен: об этом говорит плашка с кнопкой повтора,
        // и она не исчезает через две секунды.
        toastError(sshErrorText(e));
      }
    }
    if (seq === requestSeq.current) setLoading(false);
  }, [password, keyPassphrase, proxyPassword, showHidden, host.id]); // eslint-disable-line react-hooks/exhaustive-deps

  useEffect(() => { void loadDir(initialPath); }, []); // eslint-disable-line react-hooks/exhaustive-deps

  useBackHandler(() => {
    // Вопрос «Заменить или сохранить копию» Back закрывает отменой: иначе он
    // увёл бы на папку вверх, оставив лист висеть над чужой папкой.
    if (choice) { answerChoice(null); return true; }
    if (contextItem) { setContextItem(null); return true; }
    const parent = parentOf(path);
    if (parent) { haptic(); void loadDir(parent); return true; }
    // Из корня системная «Назад» уходит туда же, куда стрелка в шапке. Без этой
    // ветки общий фолбэк App.tsx вёл бы в список серверов даже тогда, когда
    // человек пришёл сюда из SSH-терминала.
    if (fromRef.current) { haptic(); navigate(back); return true; }
    return false;
  });

  useEscape(showMkdir, () => setShowMkdir(false));
  useEscape(!!renameItem, () => setRenameItem(null));
  useEscape(pwOpen, () => setPwOpen(false));
  useEscape(!!preview, () => setPreview(null));

  const submitPassword = () => {
    setPwOpen(false);
    setPassword(pwDraft);
    setKeyPassphrase(keyPassDraft);
    setProxyPassword(proxyPwDraft);
    secretRef.current = {
      password: pwDraft,
      keyPassphrase: keyPassDraft,
      proxyPassword: proxyPwDraft,
    };
    setSshPassword(conn, pwDraft);
    // Секрет уезжает в память агента не отдельным запросом «на веру», а вместе
    // со следующим листингом: агент запомнит его только если вход удался.
    rememberOnceRef.current = rememberSecret && !!(pwDraft || keyPassDraft || proxyPwDraft);
    const retry = retryRef.current;
    retryRef.current = null;
    if (retry) retry();
  };

  // «Ввести пароль заново» после неверной попытки. Неверный секрет остаётся в
  // памяти агента до его перезапуска, поэтому сперва просим его забыть —
  // иначе новый пароль просто некуда положить, и попытки будут падать молча.
  const reenterPassword = async () => {
    haptic();
    setPassword("");
    setKeyPassphrase("");
    setProxyPassword("");
    secretRef.current = { password: "", keyPassphrase: "", proxyPassword: "" };
    setPwDraft("");
    setKeyPassDraft("");
    setProxyPwDraft("");
    try { await forgetSshHostSecret(host.id); } catch { /* нечего забывать */ }
    retryRef.current = () => loadDir(path);
    setPwOpen(true);
  };

  // Пароль для терминала — через ту же модалку, что и для SFTP. submitPassword
  // кладёт введённое в secretRef и зовёт retryRef — оттуда и забираем ответ;
  // заголовок модалки уже называет сервер, поэтому label не нужен.
  const askTerminalCredentials = (_label: string, needsKey = false): Promise<SshCredentials | null> =>
    new Promise((resolve) => {
      pwResolverRef.current = resolve;
      setPwDraft("");
      setKeyPassDraft("");
      setProxyPwDraft("");
      setRememberSecret(true);
      // Сервер сказал «ключ зашифрован» — открываем нужное поле сразу.
      setMoreAuthOpen(needsKey);
      retryRef.current = () => {
        const done = pwResolverRef.current;
        pwResolverRef.current = null;
        done?.({
          password: secretRef.current.password,
          keyPassphrase: secretRef.current.keyPassphrase,
          proxyPassword: secretRef.current.proxyPassword,
          remember: rememberOnceRef.current,
        });
      };
      setPwOpen(true);
    });

  // Модалку закрыли без ввода (Esc, тап мимо, «Отмена») — для терминала это
  // отказ: иначе обещание никогда не разрешится и кнопка останется «⏳».
  useEffect(() => {
    if (pwOpen) return;
    const done = pwResolverRef.current;
    if (!done) return;
    pwResolverRef.current = null;
    retryRef.current = null;
    done(null);
  }, [pwOpen]);

  const openTerminal = async () => {
    if (termBusyRef.current) return;
    termBusyRef.current = true;
    setTermBusy(true);
    haptic();
    try {
      await connectSshHost(host, { navigate, toastSuccess, toastError, askCredentials: askTerminalCredentials });
    } finally {
      termBusyRef.current = false;
      setTermBusy(false);
    }
  };

  const activeConn = () => ({
    ...conn,
    password: secretRef.current.password || password || undefined,
    key_passphrase: secretRef.current.keyPassphrase || keyPassphrase || undefined,
    proxy_password: secretRef.current.proxyPassword || proxyPassword || undefined,
  });

  // ── Выбор из нескольких выходов ───────────────────────────────
  // «Заменить / Сохранить копию / Отмена» в один confirm не помещается: он
  // умеет только «да/нет», а третий выход здесь принципиален — иначе
  // единственная альтернатива перезаписи это отмена. Лист тот же, что в
  // «Файлах» ПК: разные ответы на один и тот же конфликт объяснить нечем.
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
    // одновременных листа висели бы друг под другом.
    choiceResolve.current?.(null);
    return new Promise<string | null>((resolve) => {
      choiceResolve.current = resolve;
      setChoice(req);
    });
  };

  useEscape(!!choice, () => answerChoice(null));

  /**
   * Свободное имя для «Сохранить копию»: «config (2).yml». Сверяемся с уже
   * показанным списком папки — он неполон (лимит 2000, скрытые файлы), поэтому
   * это подсказка: если имя всё равно занято, сервер снова ответит
   * already_exists, и вопрос повторится честно.
   */
  const copyName = (name: string): string => {
    const dot = name.lastIndexOf(".");
    const base = dot > 0 ? name.slice(0, dot) : name;
    const ext = dot > 0 ? name.slice(dot) : "";
    const taken = new Set(entries.map((i) => i.name.toLowerCase()));
    for (let n = 2; n < 100; n++) {
      const candidate = `${base} (${n})${ext}`;
      if (!taken.has(candidate.toLowerCase())) return candidate;
    }
    return `${base} (${Date.now()})${ext}`;
  };

  /** Одна попытка загрузки. null — про исход уже сказали (ошибка или отмена). */
  const putFiles = async (
    files: File[],
    overwrite: boolean,
  ): Promise<{ saved: number; conflicts: File[] } | null> => {
    const ac = new AbortController();
    uploadAbort.current = ac;
    setUploading(true);
    setUploadPct(0);
    try {
      const res = await uploadToServer(
        activeConn(), path, files, (pct) => setUploadPct(pct), overwrite, ac.signal,
      );
      const failed = Array.isArray(res?.failed) ? res.failed : [];
      // Совпадение имён — это вопрос, а не отказ: его разбирает uploadPicked.
      // Остальные причины (нет прав, диск полон) — настоящие ошибки.
      const clash = new Set(failed.filter((f) => f.code === "already_exists").map((f) => f.name));
      const other = failed.filter((f) => f.code !== "already_exists");
      const saved = Array.isArray(res?.files) ? res.files.length : files.length - failed.length;
      if (other.length) {
        hapticError();
        toastError(t("toast.uploadedPartial", { n: saved, failed: other.length, name: other[0].name }));
      }
      return { saved, conflicts: files.filter((f) => clash.has(f.name)) };
    } catch (e: any) {
      // Отмену человек сделал сам — ни ошибки, ни вибрации.
      if (ac.signal.aborted || e?.code === "aborted") return null;
      // already_exists целым ответом — тоже вопрос: на сервер не записано ничего.
      if (e?.code === "already_exists") return { saved: 0, conflicts: files };
      hapticError();
      toastError(sshErrorText(e));
      return null;
    } finally {
      if (uploadAbort.current === ac) uploadAbort.current = null;
      // После отмены состояние уже сброшено самой кнопкой; трогать его здесь
      // значило бы «мигнуть» процентами уже прерванной загрузки.
      if (!ac.signal.aborted) {
        setUploading(false);
        setUploadPct(null);
      }
    }
  };

  /**
   * Загрузка с честным вопросом при совпадении имён.
   *
   * Раньше агент отбивал одноимённый файл (already_exists), а экран печатал
   * «Не загружено: config.yml» — без причины и без единственного выхода, кроме
   * удаления файла на сервере навсегда (корзины там нет). В «Файлах» ПК ровно
   * этот конфликт разобран вопросом с тремя выходами — здесь тот же вопрос.
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
        message: t("ssh.uploadExistsMsg")
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
    void loadDir(path);
  };

  // «Отмена» стоит на месте самой кнопки загрузки — как в «Файлах» ПК: пока
  // идёт отправка, второй раз выбирать файлы всё равно нельзя, а передумать
  // человеку нужно ровно здесь.
  const cancelUpload = () => {
    haptic();
    uploadAbort.current?.abort();
    uploadAbort.current = null;
    setUploading(false);
    setUploadPct(null);
  };

  const handleUpload = () => {
    const input = document.createElement("input");
    input.type = "file";
    input.multiple = true;
    input.onchange = () => {
      if (!input.files?.length) return;
      void uploadPicked(Array.from(input.files));
    };
    input.click();
  };

  const cancelDownload = () => {
    dlAbort.current?.abort();
    dlAbort.current = null;
    setDl(null);
  };

  const handleDownload = async (item: SftpEntry) => {
    haptic();
    const ac = new AbortController();
    dlAbort.current = ac;
    setDl({ name: item.name, pct: 0 });
    setSaveFailed(null);
    try {
      const blob = await sshSftpDownloadBlob({ ...activeConn(), path: joinRemote(path, item.name) }, {
        size: item.size || undefined,
        mtime: Number.isFinite(Date.parse(item.mod_time))
          ? Math.floor(Date.parse(item.mod_time) / 1000)
          : undefined,
        signal: ac.signal,
        onProgress: (done, total) => {
          if (total > 0) setDl({ name: item.name, pct: Math.round((done / total) * 100) });
        },
      });
      const outcome = await saveBlob(blob, item.name);
      if (outcome === "failed") {
        // Мобильный Telegram: положить файл в память телефона нечем. Молчать
        // об этом нельзя — показываем плашку с работающим обходом. Плашка
        // переноса стоит внизу на том же месте, поэтому при живом переносе
        // говорим то же самое тостом, а не кладём две полосы друг на друга.
        hapticError();
        if (transferRef.current) {
          toastError(`${t("ssh.saveFailed", { name: item.name })} ${t("ssh.saveFailedHint")}`);
        } else {
          setSaveFailed(item);
        }
      } else {
        hapticSuccess();
      }
    } catch (e: any) {
      // Отмену и закрытие системного листа человек инициировал сам — молчим.
      if (!ac.signal.aborted && !isShareCancel(e)) toastError(sshErrorText(e));
    } finally {
      if (dlAbort.current === ac) dlAbort.current = null;
      setDl((cur) => (cur?.name === item.name ? null : cur));
    }
  };

  // «Скачать на ПК» — тот самый сценарий, ради которого находка и писалась:
  // дамп на 200 МБ шёл сервер → релей → телефон → релей → ПК, хотя ПК и сервер
  // могут стоять в одном ЦОДе. Здесь байты идут внутри ПК, телефон только
  // запускает перенос: ответ приходит сразу, копирование живёт на агенте.
  const [pullFor, setPullFor] = useState<SftpEntry | null>(null);
  const [transfer, setTransfer] = useState<SshTransfer | null>(null);
  useEffect(() => { transferRef.current = transfer; }, [transfer]);
  // Что именно переносим — чтобы «Повторить» после провала не зависело от того,
  // в какой папке человек оказался за это время.
  const lastPullRef = useRef<{ remote: string; localDir: string; name: string } | null>(null);

  const runPull = async (req: { remote: string; localDir: string; name: string }) => {
    lastPullRef.current = req;
    try {
      const res = await sftpPull(activeConn(), req.remote, req.localDir);
      setTransfer(res.transfer);
      toastSuccess(t("ssh.transferStarted", { name: req.name }));
    } catch (e: any) {
      hapticError();
      toastError(sshApiText(e, t("ssh.transferFailed")));
    }
  };

  const startPull = (item: SftpEntry, localDir: string) => {
    setPullFor(null);
    haptic();
    void runPull({ remote: joinRemote(path, item.name), localDir, name: item.name });
  };

  // Пока перенос идёт — опрашиваем состояние. Исчезновение задания из списка
  // означает «прервано» (переносы живут в памяти агента), а не «готово».
  useEffect(() => {
    if (!transfer || transfer.state !== "running") return;
    const h = window.setInterval(async () => {
      try {
        const { transfers } = await sftpTransfers();
        const cur = transfers.find((x) => x.id === transfer.id);
        if (!cur) {
          setTransfer(null);
          toastError(t("ssh.transferLost"));
          return;
        }
        setTransfer(cur);
        if (cur.state === "done") {
          hapticSuccess();
          toastSuccess(t("ssh.transferDone", { name: cur.name }));
        } else if (cur.state === "error") {
          hapticError();
          const reason = transferReason(cur);
          toastError(reason
            ? t("ssh.transferErrorReason", { name: cur.name, reason })
            : t("ssh.transferError", { name: cur.name }));
        }
        // «canceled» без тоста: отмену видно по плашке, а инициатор (в том
        // числе другое устройство) уже знает, что сделал.
      } catch {
        // Сеть моргнула — не роняем экран, попробуем на следующем тике.
      }
    }, 1500);
    return () => window.clearInterval(h);
  }, [transfer?.id, transfer?.state]);

  // Успешный перенос уходит с экрана сам; провал и отмена остаются, пока их не
  // закроет человек. Таймер живёт отдельно от поллера: мелкий файл может
  // приехать «done» уже в ответе на запуск, и тогда поллер не запускается.
  // Когда файл уже на ПК и его можно отправить в Telegram, две секунды дают
  // 12: в Telegram это единственная работающая кнопка «забрать файл».
  useEffect(() => {
    if (!transfer || transfer.state !== "done") return;
    const canSend = botAvailable && transfer.dir === "pull" && !!transfer.local_path;
    const h = window.setTimeout(() => setTransfer(null), canSend ? 12000 : 2000);
    return () => window.clearTimeout(h);
  }, [transfer?.id, transfer?.state, botAvailable]);

  // Доставка сообщением в Telegram: мост релея умеет отправлять только файлы
  // ПК (files_send.go), поэтому путь честный — сперва перенос сервер → ПК,
  // потом отправка уже лежащего на ПК файла.
  const [sendingTelegram, setSendingTelegram] = useState(false);
  const sendTransferToTelegram = async (tr: SshTransfer) => {
    haptic();
    setSendingTelegram(true);
    try {
      await sendToTelegram(tr.local_path, tr.name);
      hapticSuccess();
      toastSuccess(t("toast.sentToTelegram"));
      setTransfer(null);
    } catch (e: any) {
      hapticError();
      toastError(mapApiError(e));
    }
    setSendingTelegram(false);
  };

  const handleMkdir = async () => {
    if (!mkdirName.trim()) return;
    try {
      await sftpMkdir(activeConn(), joinRemote(path, mkdirName.trim()));
      hapticSuccess();
      toastSuccess(t("ui.sshfilesview.mc794163b66", { p0: (mkdirName.trim()) }));
      setShowMkdir(false);
      setMkdirName("");
      void loadDir(path);
    } catch (e: any) {
      hapticError();
      toastError(mapApiError(e));
    }
  };

  const handleRename = async () => {
    if (!renameItem || !renameTo.trim()) return;
    try {
      await sftpRename(
        activeConn(),
        joinRemote(path, renameItem.name),
        joinRemote(path, renameTo.trim()),
      );
      hapticSuccess();
      toastSuccess(t("ui.sshfilesview.mdb9f8c926c", { p0: (renameTo.trim()) }));
      setRenameItem(null);
      setContextItem(null);
      void loadDir(path);
    } catch (e: any) {
      hapticError();
      toastError(mapApiError(e));
    }
  };

  const handleDelete = async (item: SftpEntry) => {
    // Только из HoldButton — удержание 900мс и есть подтверждение.
    try {
      await sftpDelete(activeConn(), joinRemote(path, item.name));
      haptic("medium");
      toastSuccess(t("ui.sshfilesview.m15b2f54546", { p0: (item.name) }));
      setContextItem(null);
      void loadDir(path);
    } catch (e: any) {
      hapticError();
      toastError(e?.code === "not_empty"
        ? t("ui.sshfilesview.m3d40a3a07f")
        : mapApiError(e));
    }
  };

  const handlePreview = async (item: SftpEntry) => {
    try {
      const data = await runWithSshTrust(host, (trust) => sftpPreview(
        { ...activeConn(), trust_host: trust || undefined },
        joinRemote(path, item.name),
      ));
      setPreview({ item, data });
    } catch (e: any) {
      toastError(sshErrorText(e));
    }
  };

  const parent = parentOf(path);
  const segments = path.split("/").filter(Boolean);
  const visibleEntries = entries
    .filter((item) => item.name.toLowerCase().includes(filter.trim().toLowerCase()));
  // Порядок, присланный сервером, не трогаем: он посчитан по ВСЕМУ каталогу.
  // Локально сортируем только ответ агента постарше (эха sort в нём нет).
  const filteredEntries = serverSorted
    ? visibleEntries
    : visibleEntries.slice().sort((a, b) => {
      if (a.is_dir !== b.is_dir) return a.is_dir ? -1 : 1;
      if (sort === "size") return b.size - a.size || a.name.localeCompare(b.name, "ru");
      if (sort === "date") return Date.parse(b.mod_time) - Date.parse(a.mod_time) || a.name.localeCompare(b.name, "ru");
      return a.name.localeCompare(b.name, "ru");
    });

  return (
    <div className="page">
      <div className="page-header">
        <button className="back-btn" onClick={() => { haptic(); navigate(back); }} aria-label={t("generic.back")}>{"←"}</button>
        {/* Имя раздела в заголовке, адрес сервера — строкой под ним: одно
            «user@host» не говорило, что это файлы, и расходилось с вкладкой
            браузера и подсветкой внизу (аудит ИА 02.09.2026, P0-7). */}
        <div style={{ flex: 1, minWidth: 0 }}>
          {/* Заголовок тоже в одну строку с многоточием: справа появилась
              кнопка, и на узком экране он иначе выталкивал бы её за край. */}
          <h1 style={{ fontSize: 16, overflow: "hidden", textOverflow: "ellipsis", whiteSpace: "nowrap" }}>{t("ssh.files")}</h1>
          <div style={{ fontSize: 12, color: "var(--tg-hint)", overflow: "hidden", textOverflow: "ellipsis", whiteSpace: "nowrap" }}>{sshTargetLabel(conn)}</div>
        </div>
        {/* Обратная дверь: у строки сервера есть «📁 Файлы», у файлов до сих
            пор не было пути в терминал того же сервера (аудит ИА 02.09.2026,
            E4/E5). Подключение то же, что по кнопке в списке серверов. */}
        <button className="btn btn-secondary btn-sm" disabled={termBusy}
          style={{ whiteSpace: "nowrap", flexShrink: 0 }}
          title={`${t("sshfiles.terminal")}: ${host.name || sshTargetLabel(conn)}`}
          aria-label={`${t("sshfiles.terminal")}: ${host.name || sshTargetLabel(conn)}`}
          onClick={() => void openTerminal()}>
          {termBusy ? `⏳ ${t("ssh.connecting")}` : `⌨ ${t("sshfiles.terminal")}`}
        </button>
      </div>

      {/* Путь: сегменты кликабельны */}
      <ServerAccessNotice />
      <div className="fm-breadcrumbs">
        <button className="fm-crumb" onClick={() => loadDir("/")}>/</button>
        {segments.map((seg, i) => (
          <span key={i}>
            <span className="fm-sep">{"›"}</span>
            <button className="fm-crumb" onClick={() => loadDir("/" + segments.slice(0, i + 1).join("/"))}>
              {seg}
            </button>
          </span>
        ))}
      </div>

      <div className="page-content">
        <form className="ssh-path-form" onSubmit={(e) => {
          e.preventDefault();
          if (manualPath.trim()) void loadDir(manualPath.trim());
        }}>
          <input className="modal-input" aria-label={t("ui.sshfilesview.mad67d5a640")}
            value={manualPath} onChange={(e) => setManualPath(e.target.value)}
            placeholder="/var/www" autoCapitalize="off" autoCorrect="off" />
          <button className="btn btn-secondary" type="submit">{t("ui.sshfilesview.m48db038c20")}</button>
        </form>
        {/* Слова и значки — те же, что в «Файлах» ПК (аудит ИА 02.09.2026,
            P1-27): «Вверх» стрелкой ↑ как fm-nav-up, а отправка с телефона —
            «Отправить ⬆», не «📥 Загрузить»: значок «в лоток» читался как
            скачивание, и два экрана одно действие звали по-разному. */}
        <div className="fm-toolbar">
          {parent && (
            <button className="fm-tool-btn" onClick={() => loadDir(parent)}>
              {"↑"} {t("sshfiles.up")}
            </button>
          )}
          <button className="fm-tool-btn" onClick={() => loadDir(path)}>{"↻"}</button>
          <button className="fm-tool-btn" onClick={uploading ? cancelUpload : handleUpload}>
            {uploading ? "✕" : "⬆"} {uploading ? (uploadPct != null ? `${uploadPct}%` : t("files.uploading")) : t("sshfiles.upload")}
          </button>
          <button className="fm-tool-btn" onClick={() => { setShowMkdir(true); setMkdirName(""); }}>
            {"➕"} {t("ui.sshfilesview.m06ce30f6ec")}</button>
          <button className={`fm-tool-btn${showHidden ? " active" : ""}`} onClick={() => {
            const next = !showHidden;
            setShowHidden(next);
            hiddenRef.current = next;
            localStorage.setItem(`ssh.hidden.${host.id}`, next ? "1" : "0");
            window.setTimeout(() => void loadDir(path), 0);
          }}>
            {showHidden ? <IconEyeOff size={16} /> : <IconEye size={16} />}
            {" "}{showHidden ? t("sshfiles.hiddenOn") : t("sshfiles.hidden")}
          </button>
        </div>

        <div className="ssh-files-controls">
          <input className="modal-input" value={filter} onChange={(e) => setFilter(e.target.value)}
            placeholder={t("ui.sshfilesview.m7a7265fc06")} />
          {/* Смена порядка = новый запрос: сортирует сервер по всему каталогу,
              иначе «по размеру» упорядочило бы первые 2000 записей из 50 000. */}
          <select className="modal-input" value={sort} onChange={(e) => {
            const next = e.target.value as typeof sort;
            setSort(next);
            sortRef.current = next;
            void loadDir(path);
          }}>
            <option value="name">{t("sshfiles.sortName")}</option>
            <option value="date">{t("sshfiles.sortDate")}</option>
            <option value="size">{t("sshfiles.sortSize")}</option>
          </select>
        </div>
        {truncated && (
          <div className="fm-truncated">{t("ui.sshfilesview.m0558775fd5")}{entries.length} {t("ui.infrastructureview.m6ce4fa393d")}{total} {t("ui.sshfilesview.mf3b65684a6")}</div>
        )}

        {loading ? (
          <div className="loading-center"><div className="spinner" /></div>
        ) : dirError ? (
          dirError.offline ? (
            <OfflineState
              onRetry={() => { haptic(); void loadDir(path); }}
              onDevices={getMode() === "cloud" ? () => { haptic(); navigate("/devices"); } : undefined}
            />
          ) : (
            <div className="ssh-empty-block">
              <div>{dirError.text}</div>
              <div className="ssh-empty-actions">
                <button className="btn btn-secondary btn-sm" onClick={() => { haptic(); void loadDir(path); }}>
                  {t("ssh.retry")}
                </button>
                {/* Неверный пароль уже закреплён в памяти агента: без этой
                    кнопки его нельзя сменить, не уходя в список серверов. */}
                {dirError.auth && (
                  <button className="btn btn-secondary btn-sm" onClick={() => void reenterPassword()}>
                    {t("ssh.reenterPassword")}
                  </button>
                )}
              </div>
            </div>
          )
        ) : filteredEntries.length === 0 ? (
          <div className="empty">
            <div className="empty-icon">{"📂"}</div>
            <div className="empty-text">{t("files.empty")}</div>
            <button className="btn btn-primary btn-sm" style={{ marginTop: 10 }}
              onClick={uploading ? cancelUpload : handleUpload}>
              {uploading ? "✕" : `⬆ ${t("sshfiles.upload")}`}
              {uploading && uploadPct != null ? ` ${uploadPct}%` : ""}
            </button>
          </div>
        ) : (
          <div className="fm-list">
            {filteredEntries.map((item) => (
              <div
                key={item.name}
                className={`fm-item${contextItem?.name === item.name ? " fm-item-active" : ""}`}
                onClick={() => {
                  if (item.is_dir) { haptic(); void loadDir(joinRemote(path, item.name)); }
                  else setContextItem(contextItem?.name === item.name ? null : item);
                }}
                onContextMenu={(e) => { e.preventDefault(); haptic(); setContextItem(item); }}
              >
                <span className="fm-icon">{item.is_dir ? "📁" : "📄"}</span>
                <div className="fm-info">
                  <div className="fm-name">{item.name}</div>
                  <div className="fm-meta">
                    {item.is_dir ? t("agentSessions.confirmFolder") : humanSize(item.size)}
                    {formatModTime(item.mod_time) ? ` · ${formatModTime(item.mod_time)}` : ""}
                    {item.permissions ? ` · ${item.permissions}` : ""}
                  </div>
                </div>
                {item.is_dir && <span className="fm-chevron">{"›"}</span>}
              </div>
            ))}
          </div>
        )}

        {/* Действия над файлом */}
        {contextItem && (
          <div className="fm-context">
            <div className="fm-context-name">{contextItem.name}</div>
            <div className="fm-context-actions">
              {!contextItem.is_dir && (
                <button className="fm-ctx-btn" onClick={() => void handlePreview(contextItem)}>
                  {"👁️"} {t("sshfiles.preview")}
                </button>
              )}
              {!contextItem.is_dir && (
                dl && dl.name === contextItem.name ? (
                  // Отмена прямо на кнопке: качать архив логов без права
                  // передумать — то же самое, что зависшая кнопка.
                  <button className="fm-ctx-btn" onClick={() => { haptic(); cancelDownload(); }}>
                    {"⏳"} {t("ssh.downloading", { pct: dl.pct })} {"·"} {t("modal.cancel")}
                  </button>
                ) : (
                  <button className="fm-ctx-btn" onClick={() => handleDownload(contextItem)} disabled={!!dl}>
                    {"⬇️"} {t("ssh.downloadToPhone")}
                  </button>
                )
              )}
              {!contextItem.is_dir && (
                // Перенос на ПК: байты не идут через телефон вовсе.
                <button className="fm-ctx-btn" onClick={() => { haptic(); setPullFor(contextItem); }}
                  disabled={!!transfer && transfer.state === "running"}>
                  {"💻"} {t("ssh.downloadToPc")}
                </button>
              )}
              <button className="fm-ctx-btn" onClick={() => { setRenameItem(contextItem); setRenameTo(contextItem.name); }}>
                {"✏️"} {t("ui.sshfilesview.m1c5a1dd974")}</button>
              <HoldButton className="fm-ctx-btn fm-ctx-del" onConfirm={() => handleDelete(contextItem)}>
                {"🗑️"} {t("ui.sshfilesview.m63817cc689")}</HoldButton>
              <button className="fm-ctx-btn" onClick={() => setContextItem(null)}>
                {"✖"} {t("ui.sshfilesview.m0a042e2e21")}</button>
            </div>
          </div>
        )}
      </div>

      {/* Куда положить файл на ПК */}
      <FolderNavSheet
        open={!!pullFor}
        onClose={() => setPullFor(null)}
        onPick={(dir) => { if (pullFor) startPull(pullFor, dir); }}
        title={pullFor ? t("ssh.pullTitle", { name: pullFor.name }) : ""}
        pickLabel={t("ssh.pullPick")}
        initialTab="browse"
      />

      {/* Идущий перенос: прогресс и отмена. Перенос живёт на ПК и переживает
          закрытие приложения — об этом сказано прямо, иначе человек будет
          держать экран открытым «чтобы не прервалось».
          Исход разведён по состояниям: раньше и провал, и отмена печатались
          как «перенесён на компьютер», а закрыть плашку было нечем. */}
      {transfer && (
        <div className={`ssh-transfer-bar${transfer.state === "error" ? " is-error" : ""}`}>
          <div className="ssh-transfer-text">
            {transfer.state === "running"
              ? t("ssh.transferProgress", {
                  name: transfer.name,
                  pct: transfer.total > 0 ? Math.round((transfer.done / transfer.total) * 100) : 0,
                })
              : transfer.state === "done"
              ? t("ssh.transferDone", { name: transfer.name })
              : transfer.state === "canceled"
              ? t("ssh.transferCanceled", { name: transfer.name })
              : transferReason(transfer)
              ? t("ssh.transferErrorReason", { name: transfer.name, reason: transferReason(transfer) })
              : t("ssh.transferError", { name: transfer.name })}
          </div>
          <div className="ssh-transfer-actions">
            {transfer.state === "running" && (
              <button className="btn btn-sm" onClick={async () => {
                haptic();
                try { await sftpCancelTransfer(transfer.id); } catch { /* уже завершился */ }
                setTransfer(null);
              }}>{t("modal.cancel")}</button>
            )}
            {transfer.state === "error" && lastPullRef.current && (
              <button className="btn btn-sm" onClick={() => {
                haptic();
                const req = lastPullRef.current;
                setTransfer(null);
                if (req) void runPull(req);
              }}>{t("ssh.retry")}</button>
            )}
            {/* Файл уже на ПК — отсюда его умеет отправить бот (мост релея
                знает только пути ПК). Для Telegram это единственный способ
                забрать файл сервера в телефон. */}
            {transfer.state === "done" && botAvailable && transfer.dir === "pull" && transfer.local_path && (
              <button className="btn btn-sm" disabled={sendingTelegram}
                onClick={() => void sendTransferToTelegram(transfer)}>
                {sendingTelegram ? "⏳" : "📤"} {t("ssh.sendToTelegram")}
              </button>
            )}
            {(transfer.state === "error" || transfer.state === "canceled"
              || (transfer.state === "done" && botAvailable && transfer.dir === "pull" && !!transfer.local_path)) && (
              <button className="btn btn-sm" onClick={() => { haptic(); setTransfer(null); }}>
                {t("modal.close")}
              </button>
            )}
          </div>
        </div>
      )}

      {/* Сохранить в память телефона не удалось (мобильный Telegram). Молчать
          нельзя: человек качал мегабайты и остался без файла. Показываем, что
          именно произошло, и работающий путь — перенести файл на компьютер, а
          оттуда отправить сообщением в Telegram. */}
      {saveFailed && !transfer && (
        <div className="ssh-transfer-bar is-error">
          <div className="ssh-transfer-text">
            {t("ssh.saveFailed", { name: saveFailed.name })} {t("ssh.saveFailedHint")}
          </div>
          <div className="ssh-transfer-actions">
            <button className="btn btn-sm" onClick={() => {
              haptic();
              const item = saveFailed;
              setSaveFailed(null);
              setPullFor(item);
            }}>{t("ssh.downloadToPc")}</button>
            <button className="btn btn-sm" onClick={() => { haptic(); setSaveFailed(null); }}>
              {t("modal.close")}
            </button>
          </div>
        </div>
      )}

      {/* Новая папка */}
      {showMkdir && (
        <div className="modal-overlay" onClick={(e) => { if (e.target === e.currentTarget) setShowMkdir(false); }}>
          <div className="modal-sheet">
            <div className="modal-title">{t("sshfiles.newFolder")}</div>
            <input className="modal-input" value={mkdirName} onChange={(e) => setMkdirName(e.target.value)}
              placeholder={t("files.folderName")} autoFocus onKeyDown={(e) => e.key === "Enter" && handleMkdir()} />
            <div className="modal-actions">
              <button className="btn btn-secondary" onClick={() => setShowMkdir(false)}>{t("agentSessions.cancel")}</button>
              <button className="btn btn-primary" onClick={handleMkdir} disabled={!mkdirName.trim()}>{t("modal.create")}</button>
            </div>
          </div>
        </div>
      )}

      {/* Переименование */}
      {renameItem && (
        <div className="modal-overlay" onClick={(e) => { if (e.target === e.currentTarget) setRenameItem(null); }}>
          <div className="modal-sheet">
            <div className="modal-title">{t("pty.renameAction")}</div>
            <input className="modal-input" value={renameTo} onChange={(e) => setRenameTo(e.target.value)}
              autoFocus onKeyDown={(e) => e.key === "Enter" && handleRename()} />
            <div className="modal-actions">
              <button className="btn btn-secondary" onClick={() => setRenameItem(null)}>{t("agentSessions.cancel")}</button>
              <button className="btn btn-primary" onClick={handleRename} disabled={!renameTo.trim()}>{t("pty.renameAction")}</button>
            </div>
          </div>
        </div>
      )}

      {/* Пароль (после auth-ошибки) */}
      {pwOpen && (
        <div className="modal-overlay" onClick={(e) => { if (e.target === e.currentTarget) setPwOpen(false); }}>
          <div className="modal-sheet">
            <div className="modal-title">{t("ui.sshfilesview.m6bed1ba7dc")}{sshTargetLabel(conn)}</div>
            <input className="modal-input" type="password" value={pwDraft}
              onChange={(e) => setPwDraft(e.target.value)} placeholder={t("ssh.form.password")} autoFocus
              onKeyDown={(e) => e.key === "Enter" && submitPassword()} />
            {/* Ключ и бастион — под раскрывашкой: три одинаковых поля пароля
                подряд человек читал как «введи пароль три раза». */}
            <button className="ssh-manual-toggle" onClick={() => { haptic(); setMoreAuthOpen(!moreAuthOpen); }}>
              {moreAuthOpen ? `▾ ${t("ssh.form.moreAuthHide")}` : `▸ ${t("ssh.form.moreAuthShow")}`}
            </button>
            {moreAuthOpen && (
              <>
                <label className="ssh-form-field">
                  <span>{t("ssh.form.keyPass")}</span>
                  <input className="modal-input" type="password" value={keyPassDraft}
                    onChange={(e) => setKeyPassDraft(e.target.value)} placeholder={t("ssh.form.keyPass")} />
                </label>
                <label className="ssh-form-field">
                  <span>{t("ssh.form.jumpPass")}</span>
                  <input className="modal-input" type="password" value={proxyPwDraft}
                    onChange={(e) => setProxyPwDraft(e.target.value)} placeholder={t("ssh.form.jumpPass")} />
                </label>
              </>
            )}
            <label className="settings-toggle-row">
              <span>{t("ssh.form.remember")}</span>
              <input type="checkbox" checked={rememberSecret}
                onChange={(e) => setRememberSecret(e.target.checked)} />
            </label>
            <div className="ssh-sheet-hint">{t("ssh.form.secretsHint")}</div>
            <div className="modal-actions">
              <button className="btn btn-secondary" onClick={() => setPwOpen(false)}>{t("agentSessions.cancel")}</button>
              <button className="btn btn-primary" onClick={submitPassword}
                disabled={!pwDraft && !keyPassDraft && !proxyPwDraft}>{t("agentCheck.retry")}</button>
            </div>
          </div>
        </div>
      )}

      {preview && (
        <div className="modal-overlay" onClick={(e) => { if (e.target === e.currentTarget) setPreview(null); }}>
          <div className="modal-sheet ssh-preview-sheet">
            <div className="help-sheet-header">
              <div className="modal-title">{preview.item.name}</div>
              <button className="icon-btn" onClick={() => setPreview(null)} aria-label={t("pty.searchClose")}>{"✕"}</button>
            </div>
            <div className="ssh-sheet-hint">
              {preview.data.content_type} · {humanSize(preview.data.size)}
              {preview.data.truncated ? t("ui.sshfilesview.m2c5089a746") : ""}
            </div>
            {preview.data.kind === "text"
              ? <pre className="ssh-preview-text">{preview.data.content || ""}</pre>
              : <div className="ssh-empty">{t("ui.sshfilesview.mae3fd1b86b")}</div>}
          </div>
        </div>
      )}

      {/* Выбор из нескольких выходов: «Заменить» / «Сохранить как копию» /
          «Отмена». Отдельный лист, потому что confirm умеет только «да/нет». */}
      {choice && (
        <div className="folder-menu-overlay" onClick={() => answerChoice(null)}>
          <div className="folder-menu" onClick={(e) => e.stopPropagation()}>
            <div className="folder-menu-title">{choice.title}</div>
            <div className="folder-menu-path" style={{ fontFamily: "inherit", fontSize: 12 }}>
              {choice.message}
            </div>
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
            <button className="folder-menu-item cancel" onClick={() => answerChoice(null)}>
              {t("modal.cancel")}
            </button>
          </div>
        </div>
      )}

      {/* Файлы сервера — часть SSH-центра, а не «Терминала»: раньше нижняя
          панель подсвечивала соседнюю вкладку и человек читал её как «я в
          терминале». */}
      <BottomNav active="ssh" />
    </div>
  );
}
