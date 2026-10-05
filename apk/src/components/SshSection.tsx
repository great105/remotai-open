// Секция «Серверы» на экране терминалов: сохранённые SSH-хосты (свои и из
// ~/.ssh/config агента), история подключений, ручной коннект, вход в проброс
// портов и SFTP-файлы. Канонические пользовательские термины живут в ssh.*
// общего i18n-словаря; «компьютер» означает машину с Remotai, «SSH-сервер» —
// удалённый хост за ней.
//
// Пароли и ключи хранит АГЕНТ на своём компьютере (зашифрованными), клиент их
// не получает: отсюда уходит только «запомни это», обратно — флаг unlocked.

import { useEffect, useRef, useState } from "react";
import type { RefObject } from "react";
import { useNavigate, useSearchParams } from "react-router-dom";
import {
  connectSSH, createSshHost, updateSshHost, deleteSshHost, listPtySessions,
  getSshHistory, getSshForwards, unlockSshHost, forgetSshHostSecret,
  getSshKeys,
} from "../api";
import type { SshHost, SshHostInput, SshHistoryEntry, SshKey } from "../api";
import { haptic, hapticSuccess, tgConfirm } from "../telegram";
import { t } from "../i18n";
import { SheetShell, useToast, useEscape, mapApiError, REMOTAI_SERVER_INSTALL_COMMAND } from "@tgcontrol/shared";
import { isPcOffline, OfflineState } from "@tgcontrol/shared";
import {
  getSshPassword, setSshPassword, forgetSshPassword, runWithSshTrust, sshApiText,
  sshErrorText, sshLastAgo, sshTargetLabel, listSshHostsCached, invalidateSshHostsCache,
} from "../sshCommon";
import type { SshTarget } from "../sshCommon";
import { IconLock, IconUnlock } from "./icons";
import { ServerIdentity } from "./ServerIdentity";
import { ServerAccessNotice } from "./ServerAccessNotice";
import { SshForwardsSheet } from "./SshForwardsSheet";
import { SshKeysSheet } from "./SshKeysSheet";
import { listDevices } from "../cloud/api";
import type { CloudDevice } from "../cloud/api";
import { getMode, getSelectedDeviceName } from "../config";
import { humanDeviceName } from "../devices";

interface Props {
  expanded: boolean;
  onToggle: (v: boolean) => void;
  /** Якорь для scrollIntoView: секция живёт выше списка терминалов, и тап по
   *  кнопке «SSH» под списком иначе не даёт видимых изменений. */
  sectionRef?: RefObject<HTMLDivElement | null>;
  pageMode?: boolean;
  focusHostId?: string;
  /** Имя открытого сервера — чтобы страница /ssh/:id звала его по имени, а не
   *  «SSH-сервер» (это название класса объектов, а не конкретной машины). */
  onFocusHostName?: (name: string) => void;
}

export interface SshCredentials {
  password: string;
  keyPassphrase: string;
  proxyPassword: string;
  remember: boolean;
}

/** Команда, которая печатает код подключения сервера заново (см. cmd/tgcontrol/pair.go). */
const REMOTAI_PAIR_COMMAND = "remotai pair";

/**
 * Что нужно подключению к серверу от экрана, который его позвал.
 *
 * Логика подключения одна на все двери — строка сервера, карточка сервера и
 * «Терминал сервера» из файлов SSH-сервера (аудит ИА 02.09.2026, E4/E5). Раньше
 * она жила внутри SshSection и с другого экрана была недоступна: экран отдаёт
 * сюда только навигацию, тосты и способ спросить пароль, остальное общее.
 */
export interface SshConnectDeps {
  navigate: (to: string) => void;
  toastSuccess: (message: string) => void;
  toastError: (message: string) => void;
  /** Спросить пароль (needsKey — пароль ключа). null — человек отменил. */
  askCredentials: (label: string, needsKey?: boolean) => Promise<SshCredentials | null>;
  /** Свежий шелл под сценарий установки Remotai: открытый терминал не переиспользуем. */
  install?: boolean;
}

/** Сервер, к которому подключаемся: сохранённый хост или строка истории. */
type SshConnectTarget = {
  id?: string; host: string; port?: number; user: string;
  identity_file?: string; proxy_jump?: string;
};

/**
 * Уже открытый терминал ЭТОГО сервера, если он есть.
 *
 * Живая находка владельца: «посмотри, как я запустил два раза» — в группе
 * сервера висели две одинаковые сессии, различить их нечем (одно имя, один
 * адрес, оба «свободен · 4м назад»). Каждое нажатие «Подключиться» заводило
 * НОВУЮ SSH-сессию, хотя человек в 99% случаев хочет вернуться в свою.
 *
 * Сравниваем по host_id, а при его отсутствии (хост из ~/.ssh/config,
 * ручной коннект) — по «user@host», как показано в интерфейсе.
 */
async function findOpenSshTerminal(
  target: { id?: string; host: string; user: string },
): Promise<string> {
  try {
    const { sessions } = await listPtySessions();
    const label = `${target.user}@${target.host}`.toLowerCase();
    const found = (sessions || []).find((session) => {
      if (!session.alive || session.kind !== "ssh") return false;
      if (target.id && session.ssh_host_id) return session.ssh_host_id === target.id;
      return `${session.ssh_user || ""}@${session.ssh_host || ""}`.toLowerCase() === label;
    });
    return found?.id || "";
  } catch {
    return ""; // список не доехал — просто подключимся заново
  }
}

async function doSshConnect(
  target: SshConnectTarget,
  credentials: SshCredentials,
  trustHost: boolean,
  deps: SshConnectDeps,
): Promise<void> {
  // «Запомнить пароль» — обещание, за которое отвечает агент. Раньше
  // отказ этой ручки (хост переименован, ПК моргнул) обрывал подключение до
  // единственного нужного действия: человек вводил верный пароль и всё равно
  // не попадал в терминал. Теперь неудача — повод сказать правду, а не повод
  // не подключаться.
  if (target.id && credentials.remember &&
      (credentials.password || credentials.keyPassphrase || credentials.proxyPassword)) {
    try {
      const res = await unlockSshHost(target.id, {
        password: credentials.password || undefined,
        key_passphrase: credentials.keyPassphrase || undefined,
        proxy_password: credentials.proxyPassword || undefined,
        // Сохранить на компьютере: смысл галочки в том, чтобы завтра пароль
        // не спрашивали заново.
        persist: true,
      });
      if (!res?.unlocked) deps.toastError(t("ssh.rememberFailed"));
    } catch (e: any) {
      deps.toastError(sshApiText(e, t("ssh.rememberFailed")));
    }
  }
  // Уже открытый терминал этого сервера — возвращаемся в него, а не заводим
  // второй такой же (см. findOpenSshTerminal). Установка Remotai — исключение:
  // там нужен свежий шелл под сценарий с командами.
  if (!deps.install) {
    const open = await findOpenSshTerminal(target);
    if (open) {
      hapticSuccess();
      deps.toastSuccess(t("ssh.reusedTerminal"));
      deps.navigate(`/pty/${open}?ssh=1&host=${encodeURIComponent(target.host)}`);
      return;
    }
  }
  const res = await connectSSH({
    host_id: target.id,
    host: target.host,
    port: target.port || 22,
    user: target.user,
    password: credentials.password || undefined,
    identity_file: target.identity_file || undefined,
    key_passphrase: credentials.keyPassphrase || undefined,
    proxy_jump: target.proxy_jump || undefined,
    proxy_password: credentials.proxyPassword || undefined,
    trust_host: trustHost || undefined,
  });
  if (credentials.password) setSshPassword(target, credentials.password);
  if (target.id) localStorage.setItem("ssh.lastHostId", target.id);
  hapticSuccess();
  deps.navigate(`/pty/${res.id}?ssh=1&host=${encodeURIComponent(target.host)}${deps.install ? "&install=1" : ""}`);
}

/**
 * Подключиться к серверу и перейти в его терминал.
 *
 * Сохранённый пароль подставляет агент (auth_ready), пароль этой вкладки —
 * getSshPassword; человека спрашиваем только когда нет ни того, ни другого,
 * или сервер ответил auth_failed / key_encrypted. Ошибки показывает сама
 * (тосты) и наружу не бросает; защиту от двойного нажатия держит вызывающий.
 */
export async function connectSshHost(
  h: SshHost | (SshHistoryEntry & Partial<SshHost>),
  deps: SshConnectDeps,
): Promise<void> {
  let credentials: SshCredentials = {
    password: getSshPassword(h),
    keyPassphrase: "",
    proxyPassword: "",
    // Пароль из памяти вкладки НЕ пересохраняем: запоминает агент только
    // то, что человек ввёл сам и с галочкой. Иначе «Забыть» отменялось бы
    // первым же подключением — пароль возвращался бы на компьютер, и
    // человек об этом не знал.
    remember: false,
  };
  if (!credentials.password && h.auth_ready === false) {
    const entered = await deps.askCredentials(t("ui.sshsection.m4c7c738fc0", { p0: (sshTargetLabel(h)) }));
    if (!entered) return;
    credentials = entered;
  }
  try {
    await runWithSshTrust(h, (trust) => doSshConnect(h, credentials, trust, deps));
  } catch (e: any) {
    if (e?.code === "auth_failed" || e?.code === "key_encrypted") {
      const entered = await deps.askCredentials(
        e?.code === "key_encrypted"
          ? t("ui.sshsection.m53aa6242fd", { p0: (sshTargetLabel(h)) })
          : t("ui.sshsection.m4c7c738fc0", { p0: (sshTargetLabel(h)) }),
        e?.code === "key_encrypted",
      );
      if (entered) {
        try {
          await runWithSshTrust(h, (trust) => doSshConnect(h, entered, trust, deps));
        } catch (e2: any) {
          deps.toastError(sshErrorText(e2));
        }
      }
    } else {
      deps.toastError(sshErrorText(e));
    }
  }
}

export function SshSection({ expanded, onToggle, sectionRef, pageMode = false, focusHostId, onFocusHostName }: Props) {
  const navigate = useNavigate();
  const [searchParams] = useSearchParams();
  const { toastSuccess, toastError } = useToast();
  const [hosts, setHosts] = useState<SshHost[]>([]);
  const [history, setHistory] = useState<SshHistoryEntry[]>([]);
  const [loading, setLoading] = useState(false);
  const [loadErr, setLoadErr] = useState(false);
  const loadedOnce = useRef(false);
  const [filter, setFilter] = useState("");
  // Гард от дабл-тапа по коннекту (ref синхронный, state — для визуала).
  const busyRef = useRef(false);
  const installNextRef = useRef(false);
  const [busyKey, setBusyKey] = useState("");
  const connectOpenerRef = useRef<HTMLElement | null>(null);
  // Диалог пароля: promise-based, резолвер в ref.
  const [pwPrompt, setPwPrompt] = useState<{ label: string; needsKey?: boolean } | null>(null);
  const [pwDraft, setPwDraft] = useState("");
  const [keyPassDraft, setKeyPassDraft] = useState("");
  const [proxyPwDraft, setProxyPwDraft] = useState("");
  const [rememberSecret, setRememberSecret] = useState(true);
  // Пароль ключа и пароль бастиона — вторичные поля диалога подключения.
  const [moreAuthOpen, setMoreAuthOpen] = useState(false);
  const pwResolver = useRef<((credentials: SshCredentials | null) => void) | null>(null);
  const passwordInputRef = useRef<HTMLInputElement>(null);
  const keyPassInputRef = useRef<HTMLInputElement>(null);
  // Шторки: добавление/редактирование сервера, проброс портов, ручной коннект.
  const [editHost, setEditHost] = useState<SshHost | null | undefined>(undefined); // undefined=закрыто, null=новый
  const [offline, setOffline] = useState(false);
  const [fwdOpen, setFwdOpen] = useState(false);
  const [fwdPreset, setFwdPreset] = useState<SshHost | null>(null);
  const [manualOpen, setManualOpen] = useState(false);
  const [keysOpen, setKeysOpen] = useState(false);
  // Шторка «поставить Remotai на сервер»: команда установки, команда показа
  // кода и дверь туда, где код вводят.
  const [installHost, setInstallHost] = useState<SshHost | null>(null);
  const [overrideHost, setOverrideHost] = useState<SshHost | null>(null);
  const [overrideUser, setOverrideUser] = useState("");
  const [forwardSummary, setForwardSummary] = useState({ active: 0, error: 0 });

  /**
   * Имя компьютера, через который работают эти серверы.
   *
   * Продуктовая модель: устройство с Remotai живёт в аккаунте само по себе, и
   * управлять им можно с любого пульта. SSH-сервер в неё не входит — он
   * закладка на ОДНОМ компьютере, и без него недоступен. Раз так, компьютер
   * надо назвать по имени везде, где мы про это говорим.
   */
  const pcName = humanDeviceName(getSelectedDeviceName(), t("devices.chipThisPc"));

  // silent — тихая перепроверка по таймеру, пока ПК не в сети: спиннер в этом
  // случае не показываем, иначе список мигает каждые 15 секунд.
  const refresh = async (silent = false) => {
    if (!silent) setLoading(true);
    try {
      // force: раздел серверов — источник правды для общего списка машин, и
      // он же наполняет его кэш. Иначе после правки сервера список машин ещё
      // минуту показывал бы старое имя.
      const list = await listSshHostsCached(true);
      setHosts(list);
      setOffline(false);
      setLoadErr(false);
    } catch (e) {
      // Раньше ЛЮБАЯ ошибка трактовалась как «старый агент» и советовала
      // «обновите Remotai на ПК» — при выключенном компьютере это прямая
      // дезинформация. Отделяем офлайн от отсутствия эндпоинтов.
      // Флаги выставляем только по исходу: сброс «до» показывал бы на каждой
      // тихой перепроверке чужой текст («Нет сохранённых SSH-серверов»).
      setHosts([]);
      const pcOffline = isPcOffline(e);
      setOffline(pcOffline);
      setLoadErr(!pcOffline);
    }
    try {
      const d = await getSshHistory();
      setHistory((d.entries || []).slice(0, 8));
    } catch { setHistory([]); }
    try {
      const { forwards } = await getSshForwards();
      setForwardSummary({
        active: (forwards || []).filter((f) => f.status === "active").length,
        error: (forwards || []).filter((f) => f.status === "error").length,
      });
    } catch { setForwardSummary({ active: 0, error: 0 }); }
    if (!silent) setLoading(false);
  };

  useEffect(() => {
    if (expanded && !loadedOnce.current) {
      loadedOnce.current = true;
      void refresh();
    }
  }, [expanded]);

  // Офлайн — не тупик: пока ПК не в сети, тихо перепрашиваем список, чтобы
  // после включения компьютера серверы появились сами. Раньше загрузка была
  // ровно одна за монтирование, и человеку приходилось уходить с экрана и
  // возвращаться, чтобы список ожил.
  useEffect(() => {
    if (!expanded || !offline) return;
    const timer = window.setInterval(() => { void refresh(true); }, 15000);
    return () => window.clearInterval(timer);
  }, [expanded, offline]);

  // Имя открытого сервера уезжает в заголовок страницы. Колбэк намеренно не в
  // зависимостях: родитель передаёт стрелку заново на каждый рендер, а
  // setState тем же значением React и так гасит.
  useEffect(() => {
    if (!focusHostId || !onFocusHostName) return;
    const focused = hosts.find((h) => h.id === focusHostId);
    if (focused) onFocusHostName(focused.name || sshTargetLabel(focused));
  }, [focusHostId, hosts]);

  const askCredentials = (label: string, needsKey = false): Promise<SshCredentials | null> =>
    new Promise((resolve) => {
      pwResolver.current = resolve;
      setPwDraft("");
      setKeyPassDraft("");
      setProxyPwDraft("");
      setRememberSecret(true);
      // Сервер сказал «ключ зашифрован» — открываем нужное поле сразу.
      setMoreAuthOpen(needsKey);
      setPwPrompt({ label, needsKey });
    });

  const resolveCredentials = (credentials: SshCredentials | null) => {
    pwResolver.current?.(credentials);
    pwResolver.current = null;
    setPwPrompt(null);
  };

  useEscape(!!pwPrompt, () => resolveCredentials(null));
  useEscape(!!overrideHost, () => setOverrideHost(null));
  useEscape(!!installHost, () => setInstallHost(null));

  useEffect(() => {
    if (!pwPrompt) return;
    const frame = requestAnimationFrame(() => {
      (pwPrompt.needsKey ? keyPassInputRef : passwordInputRef).current?.focus();
    });
    return () => cancelAnimationFrame(frame);
  }, [pwPrompt]);

  // Кнопка подключения отключена во время запроса пароля. Когда окно
  // открывается, SheetShell уже не может сохранить её как opener; после
  // отмены возвращаем фокус, когда кнопка снова доступна.
  useEffect(() => {
    if (busyKey || !connectOpenerRef.current) return;
    const opener = connectOpenerRef.current;
    const frame = requestAnimationFrame(() => {
      if (document.contains(opener) && !opener.matches(":disabled")) opener.focus();
      connectOpenerRef.current = null;
    });
    return () => cancelAnimationFrame(frame);
  }, [busyKey]);

  // Прямая ссылка `/ssh/:id?install=1` — из общего списка машин: там у
  // SSH-сервера есть строка «нет своего Remotai», и вести она обязана сразу к
  // шагам установки, а не «куда-то на карточку, ищите кнопку сами».
  // Открываем ровно один раз: закрытая шторка не должна воскресать на каждом
  // обновлении списка, пока `?install=1` висит в адресе.
  const installDeepLinkDone = useRef(false);
  useEffect(() => {
    if (installDeepLinkDone.current) return;
    if (!focusHostId || searchParams.get("install") !== "1") return;
    const target = hosts.find((h) => h.id === focusHostId);
    if (!target) return;
    installDeepLinkDone.current = true;
    setInstallHost(target);
  }, [focusHostId, hosts, searchParams]);

  // Само подключение — общий connectSshHost наверху файла (им же пользуется
  // «Терминал сервера» в файлах SSH-сервера); здесь остаётся только то, что
  // принадлежит этому экрану: защита от двойного нажатия и флаг установки.
  const handleConnect = async (h: SshHost | (SshHistoryEntry & Partial<SshHost>), opener?: HTMLElement) => {
    if (busyRef.current) return;
    connectOpenerRef.current = opener ?? (document.activeElement instanceof HTMLElement ? document.activeElement : null);
    busyRef.current = true;
    setBusyKey(sshTargetLabel(h));
    // Флаг установки действует ровно на эту попытку. Раньше он гас только
    // после удачного connectSSH: после «Отмены» в диалоге пароля следующее
    // обычное подключение к любому серверу уезжало бы с &install=1.
    const install = installNextRef.current;
    installNextRef.current = false;
    try {
      await connectSshHost(h, { navigate, toastSuccess, toastError, askCredentials, install });
    } finally {
      busyRef.current = false;
      setBusyKey("");
    }
  };

  // Подключение идёт по одному серверу за раз (busyRef), и говорить об этом
  // обязана сама кнопка. В строке списка «Подключиться» на нажатие не отвечала
  // ничем: ни подписи, ни блокировки — пока агент поднимал SSH (секунды, а на
  // медленном сервере и дольше), человек жал её второй и третий раз. В ручной
  // форме ниже это давно сделано верно, теперь так же и в списке, и на карточке.
  const isConnecting = (target: SshTarget) => busyKey === sshTargetLabel(target);

  const handleDelete = async (h: SshHost) => {
    if (!(await tgConfirm(t("ui.sshsection.m6d0abf67a1", { p0: (h.name || sshTargetLabel(h)) }), { danger: true, confirmText: t("confirm.btn.delete") }))) return;
    try {
      await deleteSshHost(h.id);
      // Общий список машин держит серверы в кэше на минуту: без сброса
      // удалённый сервер остался бы там стоять как живой.
      invalidateSshHostsCache();
      haptic("medium");
      toastSuccess(t("ui.sshsection.mfeaecd05db"));
      void refresh();
    } catch (e: any) {
      toastError(mapApiError(e));
    }
  };

  const openFiles = (h: SshHost) => {
    haptic();
    const savedPath = localStorage.getItem(`ssh.lastPath.${h.id}`) || "";
    const q = new URLSearchParams({ host: h.id, path: savedPath });
    // Откуда открыли — туда и вернёт «Назад» в файлах сервера. С карточки
    // сервера (/ssh/:id) возврат в общий список выглядел как «меня выкинуло»:
    // человек уходил на один шаг, а возвращался на два.
    q.set("from", focusHostId ? `/ssh/${focusHostId}` : "/ssh");
    navigate(`/ssh-files?${q.toString()}`);
  };

  /**
   * «Недавние» → сохранённый сервер одним тапом.
   *
   * Строка недавнего подключения умела ровно одно — подключиться заново, и
   * каждый следующий раз человек снова искал её среди разовых. Адрес, логин,
   * порт и бастион уже известны из самой истории, поэтому спрашивать нечего;
   * имя потом правится карандашом. Пароль здесь не запрашиваем: сохранять его
   * человек решает сам при подключении — галочкой «Запомнить пароль».
   */
  const saveRecent = async (r: SshHistoryEntry) => {
    haptic();
    try {
      await createSshHost({
        name: r.host,
        host: r.host,
        port: r.port || 22,
        user: r.user,
        proxy_jump: r.proxy_jump || undefined,
      });
      invalidateSshHostsCache();
      hapticSuccess();
      toastSuccess(t("pty.sshHostSaved"));
      await refresh(true);
    } catch (e: any) {
      toastError(mapApiError(e));
    }
  };

  const openForwards = (preset: SshHost | null) => {
    haptic();
    setFwdPreset(preset);
    setFwdOpen(true);
  };

  const q = filter.trim().toLowerCase();
  const filteredHosts = q
    ? hosts.filter((h) =>
        [h.name, h.host, h.user, ...(h.tags || [])].join(" ").toLowerCase().includes(q))
    : hosts;
  const visibleHosts = focusHostId
    ? filteredHosts.filter((h) => h.id === focusHostId)
    : filteredHosts;
  const hostKeys = new Set(hosts.map((h) => `${h.user}@${h.host}:${h.port || 22}`));
  const visibleHistory = history.filter(
    (h) => !hostKeys.has(`${h.user}@${h.host}:${h.port || 22}`),
  );

  return (
    <div className="ssh-section" id="ssh-section" ref={sectionRef}>
      {/* На карточке одного сервера шапка списка не нужна: она предлагала «＋
          Добавить SSH-сервер» и «⇄ Пробросы» того списка, из которого человек
          только что вышел, — тап по ＋ на экране «этот сервер» заводил новый. */}
      {!focusHostId && (
      <div className="ssh-section-header" onClick={() => {
        if (pageMode) return;
        haptic();
        onToggle(!expanded);
      }}>
        {!pageMode && <span className="ssh-section-chevron">{expanded ? "▾" : "▸"}</span>}
        <span className="ssh-section-title">{t("ssh.sectionTitle")}</span>
        {hosts.length > 0 && <span className="ssh-section-count">{hosts.length}</span>}
        {/* Через какой компьютер они работают — прямо в шапке. Иначе раздел
            читается как «мои серверы в аккаунте», хотя это закладки одного
            компьютера, и с выключенным компьютером список пуст без причины. */}
        {pageMode && <span className="ssh-section-via">{t("ssh.viaPcShort", { name: pcName })}</span>}
        <span className="ssh-section-actions" onClick={(e) => e.stopPropagation()}>
          {/* Ключи — дверь того же уровня, что пробросы: без неё вход по ключу
              остаётся невидимой возможностью, о которой знает только тот, кто
              заглянул в форму сервера. */}
          <button
            className="ssh-mini-btn"
            title={t("ssh.keys.title")}
            aria-label={t("ssh.keys.title")}
            disabled={offline}
            onClick={() => { haptic(); setKeysOpen(true); }}
          >
            {"🔑"}
          </button>
          <button
            className="ssh-mini-btn"
            title={t("ssh.forwards")}
            aria-label={t("ssh.forwards")}
            disabled={offline}
            onClick={() => openForwards(null)}
          >
            {"⇄"}
            {forwardSummary.active > 0 && <span className="ssh-mini-count">{forwardSummary.active}</span>}
          </button>
          <button
            className="ssh-mini-btn"
            title={t("ssh.addHost")}
            aria-label={t("ssh.addHost")}
            disabled={offline}
            onClick={() => { haptic(); setEditHost(null); }}
          >
            {"＋"}
          </button>
        </span>
      </div>
      )}

      {expanded && (
        <div className="ssh-section-body">
          <ServerAccessNotice />
          {(forwardSummary.active > 0 || forwardSummary.error > 0) && (
            <button className="ssh-forward-summary" onClick={() => openForwards(null)}>
              {t("ssh.tunnelsActive", { n: forwardSummary.active })}
              {forwardSummary.error ? ` · ${t("ssh.tunnelsErrors", { n: forwardSummary.error })}` : ""}
            </button>
          )}
          {!focusHostId && visibleHistory.length > 0 && (
            <div className="ssh-recent">
              <div className="ssh-recent-title">{t("ssh.recent")}</div>
              <div className="ssh-recent-list">
                {/* Два действия на одну строку: подключиться (как было) и
                    сохранить сервер, чтобы он перестал быть «разовым». */}
                {visibleHistory.map((r) => (
                  <span className="ssh-recent-row" key={`${r.user}@${r.host}:${r.port}`}>
                    <button
                      className="ssh-recent-item"
                      disabled={isConnecting(r)}
                      onClick={(e) => { haptic(); void handleConnect(r, e.currentTarget); }}
                    >
                      {isConnecting(r) ? "⏳ " : ""}{sshTargetLabel(r)}
                    </button>
                    <button
                      className="ssh-recent-save"
                      title={t("pty.sshSaveHost")}
                      aria-label={`${t("pty.sshSaveHost")}: ${sshTargetLabel(r)}`}
                      onClick={() => void saveRecent(r)}
                    >
                      {"＋"}
                    </button>
                  </span>
                ))}
              </div>
            </div>
          )}

          {/* Поиск появлялся только с четвёртого сервера, а теги при этом
              выглядели фильтрами — тап по «prod» с тремя серверами не давал
              ничего и не показывал, куда этот тег вообще подставляется.
              Теперь строка есть везде, где есть чем фильтровать. */}
          {!focusHostId && (hosts.length > 3 || hosts.some((h) => (h.tags || []).length > 0)) && (
            <div className="ssh-search-row">
              {/* aria-label, а не только placeholder: подсказка внутри поля
                  пропадает с первой набранной буквой, и диктор с этого момента
                  называет поле просто «поле ввода». */}
              <input
                className="modal-input"
                aria-label={t("ssh.search")}
                placeholder={t("ssh.search")}
                value={filter}
                onChange={(e) => setFilter(e.target.value)}
              />
              {filter && (
                <button className="ssh-icon-btn" title={t("ssh.searchClear")} aria-label={t("ssh.searchClear")}
                  onClick={() => { haptic(); setFilter(""); }}>{"✕"}</button>
              )}
            </div>
          )}

          {loading ? (
            <div className="loading-center"><div className="spinner" /></div>
          ) : visibleHosts.length === 0 ? (
            offline ? (
              // Готовое состояние «компьютер не в сети» с кнопкой повтора —
              // раньше здесь была серая строчка без единого действия.
              <OfflineState
                compact={!pageMode}
                onRetry={() => { haptic(); void refresh(); }}
                onDevices={getMode() === "cloud" ? () => { haptic(); navigate("/infrastructure"); } : undefined}
              />
            ) : loadErr ? (
              <div className="ssh-empty-block">
                <div>{t("ssh.agentUnavailable")}</div>
                <button className="btn btn-secondary btn-sm" onClick={() => { haptic(); void refresh(); }}>
                  {t("ssh.retry")}
                </button>
              </div>
            ) : focusHostId ? (
              // Прямая ссылка на карточку сервера, которого уже нет: раньше
              // экран «SSH-сервер» отвечал «Нет сохранённых SSH-серверов».
              <div className="ssh-empty-block">
                <div>{t("ssh.hostGone")}</div>
                <button className="btn btn-secondary btn-sm" onClick={() => { haptic(); navigate("/ssh"); }}>
                  {t("ssh.allHosts")}
                </button>
              </div>
            ) : q ? (
              <div className="ssh-empty">{t("ssh.noneFound")}</div>
            ) : (
              // Список хостов приходит с ВЫБРАННОЙ машины Remotai. Человек,
              // сохранивший прод-сервер на ноутбуке, назавтра открывает
              // «Серверы» с домашнего ПК, видит пустое состояние и заводит
              // дубль. Пустота обязана назвать причину и дать переключатель.
              <div className="ssh-empty-block">
                <div>{t("ssh.empty")}</div>
                <div>{t("ssh.emptyPerDevice")}</div>
                {getMode() === "cloud" && (
                  <button className="btn btn-secondary btn-sm" onClick={() => { haptic(); navigate("/infrastructure"); }}>
                    {t("ssh.switchDevice")}
                  </button>
                )}
              </div>
            )
          ) : (
            /* Настоящий список, а не стопка div: диктор объявляет «список,
               N элементов» и говорит, на котором из них человек стоит, — иначе
               серверы читаются сплошной лентой без начала и конца. list-style
               здесь, а не классом: маркер наследуется от ul, и одного свойства
               на месте разметки достаточно. */
            <ul className="ssh-hosts-list" style={{ listStyle: "none" }}>
              {visibleHosts.map((h) => (
                <li
                  key={h.id}
                  className="ssh-host-item"
                  role="listitem"
                  tabIndex={focusHostId ? undefined : 0}
                  aria-label={focusHostId ? undefined : `${t("ssh.hostDetails")}: ${h.name || sshTargetLabel(h)}`}
                  aria-keyshortcuts={focusHostId ? undefined : "Enter Space"}
                  /* Тап по серверу открывает САМ СЕРВЕР — карточку со всеми его
                     дверями (терминал, файлы, пробросы, ключи, установка
                     Remotai), а не одну из них.

                     Живая жалоба владельца: «нажимаю на сервер — просто
                     открывается терминал; а если нажать назад, то вот это
                     окно». Карточка существовала, но попасть в неё можно было
                     только НАЗАД из терминала — то есть случайно: в строке она
                     пряталась за бледной «Подробнее ›», а тап по имени сервера
                     молча выполнял подключение.

                     Терминал при этом не стал дальше: он остался кнопкой
                     «⌨ Подключиться» в этой же строке (один тап, как и был) и
                     первой кнопкой на самой карточке. На карточке сервера
                     (focusHostId) строка — это заголовок объекта, и тапать по
                     ней некуда: все действия крупными кнопками ниже. */
                  onClick={focusHostId
                    ? undefined
                    : () => { haptic(); navigate(`/ssh/${encodeURIComponent(h.id)}`); }}
                  onKeyDown={focusHostId ? undefined : (event) => {
                    // Кнопки «Терминал», «Файлы», теги и действия живут внутри
                    // строки; их Enter/Space должны выполнять только своё дело.
                    if (event.target !== event.currentTarget) return;
                    if (event.key === "Enter" || event.key === " ") {
                      event.preventDefault();
                      haptic();
                      navigate(`/ssh/${encodeURIComponent(h.id)}`);
                    }
                  }}
                >
                  {/* Лицо сервера — общее с «Моими компьютерами» (ServerIdentity):
                      один объект нельзя рисовать двумя разными карточками. Там,
                      где раньше адрес показывался только у сервера СО СВОИМ
                      именем, теперь он есть всегда — иначе один и тот же сервер
                      звался «prod» на одном экране и «root@prod» на другом.
                      Песочные часы перед именем убраны раньше: о подключении
                      говорит сама кнопка, а два сообщения об одном событии
                      человек читает как два разных. */}
                  <ServerIdentity
                    name={h.name || sshTargetLabel(h)}
                    address={sshTargetLabel(h)}
                    badge={
                      /* Откуда взялся сервер, сказано по-человечески:
                         «ssh/config» — имя файла из мира разработчика, а бейдж
                         читает и тот, кто про этот файл никогда не слышал. */
                      <span className={`ssh-badge ${h.source}`}>
                        {h.source === "config" ? t("ssh.sourceConfig") : t("ssh.sourceOwn")}
                      </span>
                    }
                    meta={
                      <>
                        {h.user_assumed ? `${t("ssh.assumedUser")} · ` : ""}
                        {h.last_at ? sshLastAgo(h.last_at) : t("ssh.neverConnected")}
                        {h.count ? ` · ${h.count}×` : ""}
                      </>
                    }
                  >
                    {h.user_assumed && (
                      <button className="ssh-inline-action" onClick={(e) => {
                        e.stopPropagation();
                        setOverrideUser(h.user);
                        setOverrideHost(h);
                      }}>{t("ssh.chooseUser")}</button>
                    )}
                    {/* Ключ виден в строке: «этот сервер открывается ключом
                        „Прод“» — ответ на вопрос «почему пароль не спрашивают»,
                        и он же подсказывает, что сломается при удалении ключа. */}
                    {h.key_name && (
                      <div className="ssh-host-sub">{`🔑 ${t("ssh.keyBadge", { name: h.key_name })}`}</div>
                    )}
                    {h.unlocked && (
                      <div className="ssh-host-sub">
                        {/* «Сохранён на компьютере» и «живёт до перезапуска» —
                            разные обещания, и путать их нельзя: от этого
                            зависит, спросят ли пароль завтра. */}
                        {h.secret_persisted ? <IconLock size={14} /> : <IconUnlock size={14} />}
                        {` ${h.secret_persisted ? t("ssh.secretSaved") : t("ssh.secretTemporary")} · `}
                        {/* Отказ «Забыть» нельзя проглатывать: строка 🔓 после
                            обновления списка вернётся на место, и человек
                            решит, что кнопка просто не работает. */}
                        <button className="ssh-inline-action" onClick={async (e) => {
                          e.stopPropagation();
                          haptic();
                          try {
                            await forgetSshHostSecret(h.id);
                            // Пароль живёт в двух местах: на компьютере и в
                            // памяти этой вкладки. Убрать надо оба, иначе
                            // «Забыть» — только видимость.
                            forgetSshPassword(h);
                          } catch (err: any) {
                            toastError(sshApiText(err, t("ssh.forgetFailed")));
                          }
                          void refresh();
                        }}>{t("ssh.forget")}</button>
                      </div>
                    )}
                    {h.linked_device_id && (
                      <button className="ssh-linked-device" onClick={(e) => {
                        e.stopPropagation();
                        // Сразу на карточку той машины, а не в общий список:
                        // «/infrastructure» — канонический маршрут, «/devices»
                        // остаётся только алиасом старых ссылок.
                        navigate(`/infrastructure?focus=${encodeURIComponent(h.linked_device_id || "")}`);
                      }}>{`● ${t("ssh.managed")}`}</button>
                    )}
                    {(h.tags || []).length > 0 && (
                      <div className="ssh-host-tags">
                        {/* Тег выглядит фильтром-чипом — значит, обязан им и
                            быть. На карточке одного сервера фильтровать нечего,
                            там тег остаётся подписью. */}
                        {h.tags.map((tg) => (focusHostId ? (
                          <span key={tg} className="ssh-tag">{tg}</span>
                        ) : (
                          <button key={tg} type="button" className="ssh-tag ssh-tag-btn"
                            title={t("ssh.filterByTag", { tag: tg })}
                            onClick={(e) => { e.stopPropagation(); haptic(); setFilter(tg); }}>{tg}</button>
                        )))}
                      </div>
                    )}
                  </ServerIdentity>
                  {/* На карточке хоста из ряда остаются только «✎» и «🗑»: если
                      сервер пришёл из ~/.ssh/config, править нечего — рисовать
                      пустую полосу с разделителем незачем. */}
                  {(!focusHostId || h.source === "saved") && (
                  <div className="ssh-host-actions ssh-host-actions-wrap" onClick={(e) => e.stopPropagation()}>
                    {/* Тап по строке подключается к серверу, но раньше об этом
                        ничего не говорило — рядом стоял шеврон «›», который
                        обычно и значит «открыть подробности». Основное действие
                        подписано словом, и по той же причине подписаны соседние:
                        «›», «📁» и «⇄» стояли немыми значками, а подсказка жила
                        только в title — на телефоне навести мышью нечем, и
                        разгадывать их приходилось тапом. Проброс портов из строки
                        убран вовсе: самая техническая дверь продукта не может
                        открываться безымянной стрелкой — она осталась на карточке
                        сервера («⇄ Пробросы») и в шапке раздела.
                        На самой карточке дублей нет: там крупные кнопки ниже. */}
                    {!focusHostId && (
                      <>
                        {/* Читалке нужен ещё и адресат: в списке из десяти строк
                            десять одинаковых «Подключиться» неразличимы. */}
                        <button className="ssh-host-connect"
                          disabled={isConnecting(h)}
                          title={isConnecting(h) ? t("ssh.connecting") : t("ssh.connect")}
                          aria-label={`${isConnecting(h) ? t("ssh.connecting") : t("ssh.connect")}: ${h.name || sshTargetLabel(h)}`}
                          onClick={(e) => { haptic(); void handleConnect(h, e.currentTarget); }}>
                          {/* Одно действие — одно слово: на карточке сервера
                              та же кнопка зовётся «Терминал», и в строке
                              «Подключиться» читалось как что-то иное (аудит ИА
                              02.09.2026, P1-30). Полное «Подключиться: имя»
                              остаётся читалке в title/aria-label выше. */}
                          {isConnecting(h) ? `⏳ ${t("ssh.connecting")}` : `⌨ ${t("ssh.connectShort")}`}
                        </button>
                        <button className="ssh-host-files" title={t("ssh.files")}
                          aria-label={`${t("ssh.files")}: ${h.name || sshTargetLabel(h)}`}
                          onClick={() => openFiles(h)}>{`📁 ${t("ssh.filesShort")}`}</button>
                        {/* «Подробнее ›» убрана: теперь это и есть тап по строке.
                            Две двери в одно место рядом друг с другом заставляют
                            выбирать там, где выбора нет. */}
                      </>
                    )}
                    {h.source === "saved" && (
                      <>
                        <button className="ssh-icon-btn" title={t("ssh.edit")} aria-label={t("ssh.edit")}
                          onClick={() => { haptic(); setEditHost(h); }}>{"✎"}</button>
                        <button className="ssh-icon-btn" title={t("ssh.delete")} aria-label={t("ssh.delete")}
                          onClick={() => void handleDelete(h)}>{"🗑"}</button>
                      </>
                    )}
                  </div>
                  )}
                  {focusHostId && (
                    <div className="ssh-host-detail-actions" onClick={(e) => e.stopPropagation()}>
                      <button className="btn btn-primary" disabled={isConnecting(h)}
                        onClick={(e) => void handleConnect(h, e.currentTarget)}>
                        {isConnecting(h) ? `⏳ ${t("ssh.connecting")}` : `⌨ ${t("ssh.terminal")}`}
                      </button>
                      <button className="btn btn-secondary" onClick={() => openFiles(h)}>{`📁 ${t("ssh.files")}`}</button>
                      <button className="btn btn-secondary" onClick={() => openForwards(h)}>{`⇄ ${t("ssh.tunnels")}`}</button>
                      {/* Отсюда — прямой путь «поставить ключ на этот сервер»:
                          именно здесь человек понимает, что вводит пароль
                          каждый раз, и именно здесь это лечится. */}
                      <button className="btn btn-secondary" onClick={() => { haptic(); setKeysOpen(true); }}>
                        {`🔑 ${t("ssh.keys.open")}`}
                      </button>
                      {/* Раньше кнопка молча уводила в терминал сервера и
                          оставляла человека там: команда установки печатала
                          код, а что с этим кодом делать — не говорил никто.
                          Теперь она открывает шаги до конца сценария. */}
                      {!h.linked_device_id && (
                        <button className="btn btn-secondary" onClick={() => {
                          haptic();
                          setInstallHost(h);
                        }}>{`⬇ ${t("ssh.installRemotai")}`}</button>
                      )}
                    </div>
                  )}
                  {/* Чем этот сервер отличается от устройства аккаунта —
                      словами и на самом видном месте, а не внутри диалога
                      установки: раньше об этом узнавал только тот, кто уже
                      собрался ставить Remotai. */}
                  {focusHostId && (
                    <div className="ssh-standalone-note">
                      {h.linked_device_id
                        ? t("ssh.standaloneDone")
                        : t("ssh.standaloneOffer", { name: pcName })}
                    </div>
                  )}
                </li>
              ))}
            </ul>
          )}

          {!focusHostId && !offline && (
            <>
              <button className="ssh-manual-toggle" onClick={() => { haptic(); setManualOpen(!manualOpen); }}>
                {manualOpen ? `▾ ${t("ssh.manualHide")}` : `▸ ${t("ssh.manualShow")}`}
              </button>
              {manualOpen && (
                <ManualConnectForm busyRef={busyRef} busyKey={busyKey} setBusyKey={setBusyKey}
                  onSaved={() => { void refresh(true); }} />
              )}
            </>
          )}
        </div>
      )}

      {/* Диалог пароля */}
      {pwPrompt && (
        <SheetShell open={!!pwPrompt} onClose={() => resolveCredentials(null)}
          overlayClassName="modal-overlay" className="modal-sheet" labelledBy="ssh-credentials-title">
            <div className="modal-title" id="ssh-credentials-title">{pwPrompt.label}</div>
            <input
              ref={passwordInputRef}
              className="modal-input"
              type="password"
              aria-label={t("ssh.form.password")}
              placeholder={t("ui.sshsection.m14f7c63cc1")}
              value={pwDraft}
              onChange={(e) => setPwDraft(e.target.value)}
              onKeyDown={(e) => {
                if (e.key === "Enter") {
                  e.preventDefault();
                  resolveCredentials({ password: pwDraft, keyPassphrase: keyPassDraft, proxyPassword: proxyPwDraft, remember: rememberSecret });
                }
              }}
            />
            {/* Ключ и бастион — под раскрывашкой: три одинаковых поля пароля
                подряд человек читал как «введи пароль три раза». Открыто
                сразу, когда сервер сам попросил пароль ключа. */}
            <button className="ssh-manual-toggle" onClick={() => { haptic(); setMoreAuthOpen(!moreAuthOpen); }}>
              {moreAuthOpen ? `▾ ${t("ssh.form.moreAuthHide")}` : `▸ ${t("ssh.form.moreAuthShow")}`}
            </button>
            {moreAuthOpen && (
              <>
                <label className="ssh-form-field">
                  <span>{t("ssh.form.keyPass")}</span>
                  <input className="modal-input" type="password"
                    ref={keyPassInputRef}
                    placeholder={t("ssh.form.keyPass")}
                    value={keyPassDraft} onChange={(e) => setKeyPassDraft(e.target.value)} />
                </label>
                <label className="ssh-form-field">
                  <span>{t("ssh.form.jumpPass")}</span>
                  <input className="modal-input" type="password"
                    placeholder={t("ssh.form.jumpPass")}
                    value={proxyPwDraft} onChange={(e) => setProxyPwDraft(e.target.value)} />
                </label>
              </>
            )}
            <label className="settings-toggle-row">
              <span>{t("ssh.form.remember")}</span>
              <input type="checkbox" checked={rememberSecret} onChange={(e) => setRememberSecret(e.target.checked)} />
            </label>
            <div className="ssh-sheet-hint">{t("ssh.form.secretsHint")}</div>
            <div className="modal-actions">
              <button className="btn btn-secondary" onClick={() => resolveCredentials(null)}>{t("agentSessions.cancel")}</button>
              <button className="btn btn-primary" onClick={() => resolveCredentials({
                password: pwDraft, keyPassphrase: keyPassDraft,
                proxyPassword: proxyPwDraft, remember: rememberSecret,
              })}>{t("openrouter.connect")}</button>
            </div>
        </SheetShell>
      )}

      {overrideHost && (
        <SheetShell open={!!overrideHost} onClose={() => setOverrideHost(null)}
          overlayClassName="modal-overlay" className="modal-sheet" labelledBy="ssh-override-title">
            <div className="modal-title" id="ssh-override-title">{t("ui.sshsection.ma8b7ef80ec")}{overrideHost.name}</div>
            <input className="modal-input" value={overrideUser} aria-label={t("ssh.form.user")}
              onChange={(e) => setOverrideUser(e.target.value)}
              autoCapitalize="off" autoCorrect="off" />
            <div className="ssh-sheet-hint">
              {t("ui.sshsection.mfc1463faa3")}</div>
            <div className="modal-actions">
              <button className="btn btn-secondary" onClick={() => setOverrideHost(null)}>{t("agentSessions.cancel")}</button>
              <button className="btn btn-primary" disabled={!overrideUser.trim()} onClick={async () => {
                try {
                  await createSshHost({
                    name: overrideHost.name,
                    host: overrideHost.host,
                    port: overrideHost.port,
                    user: overrideUser.trim(),
                    identity_file: overrideHost.identity_file,
                    proxy_jump: overrideHost.proxy_jump,
                    tags: overrideHost.tags,
                  });
                  invalidateSshHostsCache();
                  setOverrideHost(null);
                  await refresh();
                  toastSuccess(t("ui.sshsection.m2e05c5c36d"));
                } catch (e: any) {
                  toastError(mapApiError(e));
                }
              }}>{t("ui.sshsection.m085fc757fc")}</button>
            </div>
        </SheetShell>
      )}

      <SshInstallSheet
        host={installHost}
        onClose={() => setInstallHost(null)}
        onOpenTerminal={(h) => {
          installNextRef.current = true;
          setInstallHost(null);
          void handleConnect(h);
        }}
      />

      <SshHostSheet
        open={editHost !== undefined}
        initial={editHost ?? null}
        onClose={() => setEditHost(undefined)}
        onSaved={async (saved, credentials) => {
          setEditHost(undefined);
          invalidateSshHostsCache();
          await refresh();
          if (credentials.password || credentials.keyPassphrase || credentials.proxyPassword) {
            try {
              await runWithSshTrust(saved, (trust) =>
                doSshConnect(saved, credentials, trust, { navigate, toastSuccess, toastError, askCredentials }));
            } catch (e: any) {
              toastError(sshErrorText(e));
            }
          }
        }}
      />

      <SshForwardsSheet
        open={fwdOpen}
        onClose={() => { setFwdOpen(false); void refresh(); }}
        hosts={hosts}
        presetHost={fwdPreset}
      />

      <SshKeysSheet
        open={keysOpen}
        onClose={() => setKeysOpen(false)}
        hosts={hosts.filter((h) => h.source === "saved")}
        onHostsChanged={() => { void refresh(true); }}
      />
    </div>
  );
}

// ── «Поставить Remotai на сервер» ───────────────────────────────
//
// Конец сценария владельца: зашёл на сервер по SSH → поставил Remotai →
// получил код → добавил сервер к себе в аккаунт, не уходя из приложения.
// Автоподхвата кода из вывода терминала здесь нет намеренно: install.sh печатает
// код вместе с QR и рамками, разметка меняется от версии к версии, и парсер,
// который однажды поймает не то, дороже трёх честных шагов с копированием.

function SshInstallSheet({ host, onClose, onOpenTerminal }: {
  host: SshHost | null;
  onClose: () => void;
  /** Открыть терминал этого сервера с готовым баннером установки. */
  onOpenTerminal: (host: SshHost) => void;
}) {
  const navigate = useNavigate();
  const { toastSuccess, toastError } = useToast();
  if (!host) return null;

  const copy = async (text: string) => {
    try {
      await navigator.clipboard.writeText(text);
      hapticSuccess();
      toastSuccess(t("ssh.install.copied"));
    } catch {
      toastError(t("ssh.install.copyFailed"));
    }
  };

  return (
    <SheetShell open={!!host} onClose={onClose}
      overlayClassName="modal-overlay" className="modal-sheet ssh-install-sheet"
      labelledBy="ssh-install-title">
        <div className="help-sheet-header">
          <div className="modal-title" id="ssh-install-title">{t("ssh.install.title", { name: host.name || sshTargetLabel(host) })}</div>
          <button className="icon-btn" onClick={onClose} aria-label={t("modal.close")}>{"✕"}</button>
        </div>
        {/* Честная разница между SSH-сервером и машиной с Remotai: она решает,
            стоит ли вообще ставить агента, и объясняет, почему у сервера в
            общем списке нет экрана. */}
        <p className="ssh-install-intro">{t("ssh.install.intro")}</p>

        <ol className="ssh-install-steps">
          <li>
            <b>1</b>
            <div className="ssh-install-step">
              <span>{t("ssh.install.step1")}</span>
              <code>{REMOTAI_SERVER_INSTALL_COMMAND}</code>
              <small>{t("ssh.install.step1Hint")}</small>
              <div className="ssh-install-step-actions">
                <button className="btn btn-secondary btn-sm"
                  onClick={() => void copy(REMOTAI_SERVER_INSTALL_COMMAND)}>
                  {t("ssh.install.copy")}
                </button>
                <button className="btn btn-secondary btn-sm" onClick={() => { haptic(); onOpenTerminal(host); }}>
                  {`⌨ ${t("ssh.install.openTerminal")}`}
                </button>
              </div>
            </div>
          </li>
          <li>
            <b>2</b>
            <div className="ssh-install-step">
              <span>{t("ssh.install.step2")}</span>
              <code>{REMOTAI_PAIR_COMMAND}</code>
              <small>{t("ssh.install.step2Hint")}</small>
              <div className="ssh-install-step-actions">
                <button className="btn btn-secondary btn-sm" onClick={() => void copy(REMOTAI_PAIR_COMMAND)}>
                  {t("ssh.install.copy")}
                </button>
              </div>
            </div>
          </li>
          <li>
            <b>3</b>
            <div className="ssh-install-step">
              <span>{t("ssh.install.step3")}</span>
              <div className="ssh-install-step-actions">
                {/* Тот же адрес, что у «Добавить компьютер»: экран машин сам
                    открывает поле кода и сканер — из приложения не выходим. */}
                <button className="btn btn-primary" onClick={() => {
                  haptic();
                  onClose();
                  navigate("/infrastructure?add=1&type=server");
                }}>{t("ssh.install.enterCode")}</button>
              </div>
            </div>
          </li>
        </ol>
    </SheetShell>
  );
}

// ── Ручное подключение (ад-hoc, без сохранения) ─────────────────

function ManualConnectForm({ busyRef, busyKey, setBusyKey, onSaved }: {
  busyRef: React.MutableRefObject<boolean>;
  busyKey: string;
  setBusyKey: (v: string) => void;
  /** Список серверов обновить: сохранённый сервер должен появиться сразу. */
  onSaved: () => void;
}) {
  const navigate = useNavigate();
  const { toastError, toastSuccess } = useToast();
  const [host, setHost] = useState("");
  const [port, setPort] = useState("22");
  const [user, setUser] = useState("");
  const [password, setPassword] = useState("");
  const [identityFile, setIdentityFile] = useState("");
  const [keyPassphrase, setKeyPassphrase] = useState("");
  const [proxyJump, setProxyJump] = useState("");
  const [proxyPassword, setProxyPassword] = useState("");
  // Ключ и бастион — под той же раскрывашкой, что в форме сервера и в диалоге
  // пароля: подряд идущие поля «пароль», «пароль ключа», «пароль бастиона»
  // человек читает как «введи пароль три раза».
  const [moreAuthOpen, setMoreAuthOpen] = useState(false);

  const portNum = () => {
    const n = parseInt(port, 10);
    return Number.isFinite(n) && n > 0 ? n : 22;
  };

  const submit = async () => {
    if (busyRef.current) return;
    if (!host.trim() || !user.trim()) {
      toastError(t("ssh.form.needHostUser"));
      return;
    }
    busyRef.current = true;
    setBusyKey("manual");
    const target = { host: host.trim(), port: portNum(), user: user.trim(), proxy_jump: proxyJump.trim() || undefined };
    try {
      const res = await runWithSshTrust(target, (trust) => connectSSH({
        host: target.host, port: target.port, user: target.user,
        password: password || undefined,
        identity_file: identityFile.trim() || undefined,
        key_passphrase: keyPassphrase || undefined,
        proxy_jump: target.proxy_jump,
        proxy_password: proxyPassword || undefined,
        trust_host: trust || undefined,
      }));
      if (password) setSshPassword(target, password);
      hapticSuccess();
      navigate(`/pty/${res.id}?ssh=1&host=${encodeURIComponent(target.host)}`);
    } catch (e: any) {
      toastError(sshErrorText(e));
    } finally {
      busyRef.current = false;
      setBusyKey("");
    }
  };

  const enterKey = (e: React.KeyboardEvent) => {
    if (e.key === "Enter") { e.preventDefault(); void submit(); }
  };

  /**
   * «Сохранить сервер» рядом с «Подключить».
   *
   * У формы был один выход — подключиться, и адрес с логином приходилось
   * вбивать заново при каждом возвращении к тому же серверу: попасть в список
   * сохранённых можно было только через отдельную форму ＋ с теми же полями.
   * Здесь всё уже заполнено, поэтому сохраняем как есть; имя = адрес, его потом
   * правит карандаш. Пароль в сервер не уезжает — он живёт только в памяти.
   */
  const [saving, setSaving] = useState(false);
  const saveHost = async () => {
    if (saving) return;
    if (!host.trim() || !user.trim()) {
      toastError(t("ssh.form.needHostUser"));
      return;
    }
    setSaving(true);
    try {
      await createSshHost({
        name: host.trim(),
        host: host.trim(),
        port: portNum(),
        user: user.trim(),
        identity_file: identityFile.trim() || undefined,
        proxy_jump: proxyJump.trim() || undefined,
      });
      invalidateSshHostsCache();
      hapticSuccess();
      toastSuccess(t("pty.sshHostSaved"));
      onSaved();
    } catch (e: any) {
      toastError(mapApiError(e));
    } finally {
      setSaving(false);
    }
  };

  // Формулировки берём из ssh.form.* — те же, что в карточке сервера. Раньше
  // одно и то же поле называлось здесь «Хост», а там «Адрес сервера», и человек
  // во второй форме заново гадал, что куда вписывать.
  return (
    <div className="ssh-manual-form">
      <input
        className="modal-input"
        placeholder={t("ssh.form.host")}
        value={host}
        onChange={(e) => setHost(e.target.value)}
        onKeyDown={enterKey}
        autoCapitalize="off"
        autoCorrect="off"
      />
      <div className="ssh-sheet-row">
        <input
          className="modal-input ssh-sheet-port"
          placeholder={t("ssh.form.port")}
          inputMode="numeric"
          value={port}
          onChange={(e) => setPort(e.target.value)}
          onKeyDown={enterKey}
        />
        <input
          className="modal-input"
          placeholder={t("ssh.form.user")}
          value={user}
          onChange={(e) => setUser(e.target.value)}
          onKeyDown={enterKey}
          autoCapitalize="off"
          autoCorrect="off"
        />
      </div>
      <input
        className="modal-input"
        type="password"
        placeholder={t("ssh.form.password")}
        value={password}
        onChange={(e) => setPassword(e.target.value)}
        onKeyDown={enterKey}
      />
      <div className="ssh-sheet-hint">{t("ssh.form.passwordHint")}</div>
      <button className="ssh-manual-toggle" onClick={() => { haptic(); setMoreAuthOpen(!moreAuthOpen); }}>
        {moreAuthOpen ? `▾ ${t("ssh.form.moreAuthHide")}` : `▸ ${t("ssh.form.moreAuthShow")}`}
      </button>
      {moreAuthOpen && (
        <>
          <label className="ssh-form-field">
            <span>{t("ssh.form.keyFile")}</span>
            <input
              className="modal-input"
              placeholder="~/.ssh/id_ed25519"
              value={identityFile}
              onChange={(e) => setIdentityFile(e.target.value)}
              onKeyDown={enterKey}
              autoCapitalize="off"
              autoCorrect="off"
            />
          </label>
          <label className="ssh-form-field">
            <span>{t("ssh.form.keyPass")}</span>
            <input
              className="modal-input"
              type="password"
              placeholder={t("ssh.form.keyPass")}
              value={keyPassphrase}
              onChange={(e) => setKeyPassphrase(e.target.value)}
              onKeyDown={enterKey}
            />
          </label>
          <label className="ssh-form-field">
            <span>{t("ssh.form.jump")}</span>
            <input
              className="modal-input"
              placeholder={t("ssh.form.jumpHint")}
              value={proxyJump}
              onChange={(e) => setProxyJump(e.target.value)}
              onKeyDown={enterKey}
              autoCapitalize="off"
              autoCorrect="off"
            />
          </label>
          <label className="ssh-form-field">
            <span>{t("ssh.form.jumpPass")}</span>
            <input
              className="modal-input"
              type="password"
              placeholder={t("ssh.form.jumpPass")}
              value={proxyPassword}
              onChange={(e) => setProxyPassword(e.target.value)}
              onKeyDown={enterKey}
            />
          </label>
        </>
      )}
      <div className="ssh-manual-actions">
        <button
          className="btn btn-primary ssh-sheet-submit"
          disabled={busyKey === "manual"}
          onClick={() => void submit()}
        >
          {busyKey === "manual" ? t("ssh.connecting") : t("ssh.connect")}
        </button>
        <button
          className="btn btn-secondary"
          disabled={saving || busyKey === "manual"}
          onClick={() => void saveHost()}
        >
          {saving ? t("ssh.form.saving") : t("pty.sshSaveHost")}
        </button>
      </div>
    </div>
  );
}

// ── Форма добавления/редактирования сервера ─────────────────────

function SshHostSheet({ open, initial, onClose, onSaved }: {
  open: boolean;
  initial: SshHost | null; // null — создание
  onClose: () => void;
  onSaved: (host: SshHost, credentials: SshCredentials) => void;
}) {
  useEscape(open, onClose);
  const { toastError } = useToast();
  const [name, setName] = useState("");
  const [host, setHost] = useState("");
  const [port, setPort] = useState("22");
  const [user, setUser] = useState("");
  const [identityFile, setIdentityFile] = useState("");
  const [proxyJump, setProxyJump] = useState("");
  const [tags, setTags] = useState("");
  const [password, setPassword] = useState("");
  const [keyPassphrase, setKeyPassphrase] = useState("");
  const [proxyPassword, setProxyPassword] = useState("");
  const [remember, setRemember] = useState(true);
  const [linkedDeviceID, setLinkedDeviceID] = useState("");
  const [devices, setDevices] = useState<CloudDevice[]>([]);
  const [keys, setKeys] = useState<SshKey[]>([]);
  const [keyID, setKeyID] = useState("");
  const [busy, setBusy] = useState(false);
  const nameInputRef = useRef<HTMLInputElement>(null);
  // «Дополнительно»: ключ, бастион, теги, связанная машина. У сервера, который
  // редактируют, эти поля уже заполнены — тогда блок открыт сразу, иначе
  // человек не увидит, что именно он правит.
  const [advancedOpen, setAdvancedOpen] = useState(false);

  useEffect(() => {
    if (!open) return;
    setName(initial?.name || "");
    setHost(initial?.host || "");
    setPort(String(initial?.port || 22));
    setUser(initial?.user || "");
    setIdentityFile(initial?.identity_file || "");
    setProxyJump(initial?.proxy_jump || "");
    setTags((initial?.tags || []).join(", "));
    setPassword("");
    setKeyPassphrase("");
    setProxyPassword("");
    setRemember(true);
    setLinkedDeviceID(initial?.linked_device_id || "");
    setKeyID(initial?.key_id || "");
    setAdvancedOpen(!!(initial?.identity_file || initial?.proxy_jump
      || (initial?.tags || []).length > 0 || initial?.linked_device_id || initial?.key_id));
    // Список ключей нужен, только пока форма открыта. Ошибку глотаем: у старого
    // агента этой ручки нет, и форма сервера не должна из-за неё падать —
    // просто останется вход по паролю и по файлу ключа.
    void getSshKeys().then((r) => setKeys(r.keys || [])).catch(() => setKeys([]));
    if (getMode() === "cloud") {
      void listDevices().then((r) => setDevices(r.devices || [])).catch(() => setDevices([]));
    } else {
      setDevices([]);
    }
  }, [open, initial]);

  useEffect(() => {
    if (!open) return;
    const frame = requestAnimationFrame(() => nameInputRef.current?.focus());
    return () => cancelAnimationFrame(frame);
  }, [open]);

  if (!open) return null;

  const submit = async () => {
    if (busy) return;
    if (!host.trim() || !user.trim()) {
      toastError(t("ssh.form.needHostUser"));
      return;
    }
    const n = parseInt(port, 10);
    const input: SshHostInput = {
      name: name.trim() || host.trim(),
      host: host.trim(),
      port: Number.isFinite(n) && n > 0 ? n : 22,
      user: user.trim(),
      identity_file: identityFile.trim() || undefined,
      proxy_jump: proxyJump.trim() || undefined,
      tags: tags.split(",").map((s) => s.trim()).filter(Boolean),
      linked_device_id: linkedDeviceID || undefined,
      // Пустая строка, а не undefined: так снимается ранее назначенный ключ —
      // undefined в PATCH означал бы «не трогать это поле».
      key_id: keyID,
    };
    setBusy(true);
    try {
      const saved = initial
        ? (await updateSshHost(initial.id, input)).host
        : (await createSshHost(input)).host;
      hapticSuccess();
      onSaved(saved, {
        password,
        keyPassphrase,
        proxyPassword,
        remember,
      });
    } catch (e: any) {
      toastError(mapApiError(e));
    } finally {
      setBusy(false);
    }
  };

  // Кнопка называлась «Добавить», а при заполненном пароле сразу подключалась и
  // уводила в терминал (onSaved → doConnect). Подпись обязана говорить правду.
  const willConnect = !!(password || keyPassphrase || proxyPassword);
  const submitLabel = busy
    ? t("ssh.form.saving")
    : initial
    ? (willConnect ? t("ssh.form.saveConnect") : t("ssh.form.save"))
    : (willConnect ? t("ssh.form.addConnect") : t("ssh.form.add"));

  return (
    <SheetShell open={open} onClose={onClose}
      overlayClassName="modal-overlay" className="modal-sheet ssh-sheet"
      labelledBy="ssh-host-title">
        <div className="help-sheet-header">
          <div className="modal-title" id="ssh-host-title">{initial ? t("ssh.form.titleEdit") : t("ssh.form.titleNew")}</div>
          <button className="icon-btn" onClick={onClose} aria-label={t("modal.close")}>{"✕"}</button>
        </div>
        {/* Обязательное — сверху: адрес, пользователь, пароль. Ключ, бастион и
            теги ушли под раскрывашку: человек, который знает только адрес,
            логин и пароль, раньше упирался в одиннадцать полей подряд. */}
        {/* Первые четыре поля стояли голыми: подсказка внутри поля исчезает,
            как только начал печатать, и заполненная форма превращалась в
            четыре одинаковые строки — «192.168.1.10» и «root» без единого
            слова о том, что где. Диктор не называл их и до печати. Подписи —
            те же <label className="ssh-form-field">, что у полей ниже. */}
        <label className="ssh-form-field">
          <span>{t("ssh.form.nameLabel")}</span>
          <input className="modal-input" placeholder={t("ssh.form.name")}
            ref={nameInputRef} value={name} onChange={(e) => setName(e.target.value)} />
        </label>
        <label className="ssh-form-field">
          <span>{t("ssh.form.hostLabel")}</span>
          <input className="modal-input" placeholder={t("ssh.form.host")}
            value={host} onChange={(e) => setHost(e.target.value)}
            autoCapitalize="off" autoCorrect="off" />
        </label>
        <div className="ssh-sheet-row">
          {/* Порт и пользователь — одна флекс-строка, поэтому каждому нужен
              СВОЙ label: общая обёртка сложила бы их в столбик. Колонка порта
              не растягивается (ssh-sheet-port-field), остаток забирает
              пользователь. */}
          <label className="ssh-form-field ssh-sheet-port-field">
            <span>{t("ssh.form.port")}</span>
            <input className="modal-input ssh-sheet-port" placeholder={t("ssh.form.port")} inputMode="numeric"
              value={port} onChange={(e) => setPort(e.target.value)} />
          </label>
          <label className="ssh-form-field">
            <span>{t("ssh.form.user")}</span>
            <input className="modal-input" placeholder={t("ssh.form.user")}
              value={user} onChange={(e) => setUser(e.target.value)}
              autoCapitalize="off" autoCorrect="off" />
          </label>
        </div>
        <label className="ssh-form-field">
          <span>{t("ssh.form.password")}</span>
          <input className="modal-input" type="password" placeholder={t("ssh.form.password")}
            value={password} onChange={(e) => setPassword(e.target.value)} />
        </label>
        <div className="ssh-sheet-hint">{t("ssh.form.passwordHint")}</div>
        <label className="settings-toggle-row">
          <span>{t("ssh.form.remember")}</span>
          <input type="checkbox" checked={remember} onChange={(e) => setRemember(e.target.checked)} />
        </label>
        <div className="ssh-sheet-hint">{t("ssh.form.rememberHint")}</div>
        {/* Ключ — не «дополнительное»: это второй из двух способов войти, и
            прятать его под раскрывашку значит прятать половину ответа на
            вопрос «как этот сервер открывается». Список пуст — строки нет:
            выбор из ничего только мешает. */}
        {keys.length > 0 && (
          <label className="ssh-form-field">
            <span>{t("ssh.form.key")}</span>
            <select className="modal-input" value={keyID} onChange={(e) => setKeyID(e.target.value)}>
              <option value="">{t("ssh.form.keyNone")}</option>
              {keys.map((k) => (
                <option key={k.id} value={k.id}>{k.name}</option>
              ))}
            </select>
          </label>
        )}

        <button className="ssh-manual-toggle" onClick={() => { haptic(); setAdvancedOpen(!advancedOpen); }}>
          {advancedOpen ? `▾ ${t("ssh.form.advancedHide")}` : `▸ ${t("ssh.form.advancedShow")}`}
        </button>
        {advancedOpen && (
          <>
            <label className="ssh-form-field">
              <span>{t("ssh.form.keyFile")}</span>
              <input className="modal-input" placeholder="~/.ssh/id_ed25519"
                value={identityFile} onChange={(e) => setIdentityFile(e.target.value)}
                autoCapitalize="off" autoCorrect="off" />
            </label>
            <label className="ssh-form-field">
              <span>{t("ssh.form.keyPass")}</span>
              <input className="modal-input" type="password" placeholder={t("ssh.form.keyPass")}
                value={keyPassphrase} onChange={(e) => setKeyPassphrase(e.target.value)} />
            </label>
            <label className="ssh-form-field">
              <span>{t("ssh.form.jump")}</span>
              <input className="modal-input" placeholder={t("ssh.form.jumpHint")}
                value={proxyJump} onChange={(e) => setProxyJump(e.target.value)}
                autoCapitalize="off" autoCorrect="off" />
            </label>
            <label className="ssh-form-field">
              <span>{t("ssh.form.jumpPass")}</span>
              <input className="modal-input" type="password" placeholder={t("ssh.form.jumpPass")}
                value={proxyPassword} onChange={(e) => setProxyPassword(e.target.value)} />
            </label>
            <label className="ssh-form-field">
              <span>{t("ssh.form.tagsLabel")}</span>
              <input className="modal-input" placeholder={t("ssh.form.tags")}
                value={tags} onChange={(e) => setTags(e.target.value)}
                autoCapitalize="off" autoCorrect="off" />
            </label>
            {devices.length > 0 && (
              <label className="ssh-device-link-field">
                <span>{t("ssh.form.linkedDevice")}</span>
                <select className="modal-input" value={linkedDeviceID}
                  onChange={(e) => setLinkedDeviceID(e.target.value)}>
                  <option value="">{t("ssh.form.linkedNone")}</option>
                  {devices.map((device) => (
                    <option key={device.id} value={device.id}>
                      {device.name || device.hostname}
                      {device.online ? ` · ${t("ssh.form.deviceOnline")}` : ` · ${t("ssh.form.deviceOffline")}`}
                    </option>
                  ))}
                </select>
              </label>
            )}
          </>
        )}
        <div className="ssh-sheet-hint">{t("ssh.form.secretsHint")}</div>
        <button className="btn btn-primary ssh-sheet-submit" disabled={busy} onClick={() => void submit()}>
          {submitLabel}
        </button>
    </SheetShell>
  );
}
