import { Fragment, useCallback, useEffect, useRef, useState, type CSSProperties } from "react";
import { useLocation, useNavigate } from "react-router-dom";
import { useGoBack } from "../navBack";
import { SheetShell, useEscape, mapApiError, LanguageSelector } from "@tgcontrol/shared";
import { getConfig, updateConfig, getAgents, rescanAgents, getSystemStats, getQuickPaths, getBookmarks, getRecentFolders, listFiles, mkDir, removeBookmark, reconnectWS, disconnectWS, getAgentUpdateInfo, applyAgentUpdate, type AgentUpdateInfo, type RecentFolder } from "../api";
import { getTelegram, haptic, hapticSuccess, tgConfirm } from "../telegram";
import { RELAY_BASE, SUPPORT_URL } from "@tgcontrol/shared";
import { useToast } from "@tgcontrol/shared";
import { t } from "../i18n";
import type { AppConfig, AgentInfo, SystemStats, QuickPath, Bookmark, FileItem } from "../types";
import { useFeatures } from "../hooks/useFeatures";
import { setFeatures } from "../features";
import { clearConfig, getMode, getServerConfig, isNativeApp, isOnPCPanel, saveConfig } from "../config";
import { resetCapabilities } from "../capabilities";
import { checkAppUpdate, openApkDownload, type AppUpdateInfo } from "../appUpdate";
import {
  getMe,
  getIdentities,
  linkEmailIdentity,
  listLoginSessions,
  revokeLoginSession,
  revokeOtherLoginSessions,
  type CloudMe,
  type CloudIdentity,
  type CloudLoginSession,
} from "../cloud/api";
import { isAnalyticsEnabled, setAnalyticsEnabled } from "../cloud/support";
import { getCloudNotifyPrefs, setCloudNotifyPrefs, type CloudNotifyPrefs } from "../cloud/notifyPrefs";
import { cloudAccountAvailable, supportChatAvailable } from "../supportUnread";
import { useSupportUnread } from "../hooks/useSupportUnread";
import { BottomNav } from "../components/BottomNav";
import { ComputerSettings } from "../components/ComputerSettings";
import { SettingsIndex, SettingsUnavailable } from "../settings/SettingsIndex";
import { settingsPath, settingsSection } from "../settings/navigation";
import { LocalCapabilitiesSection } from "../transcription/LocalCapabilitiesSection";
import { isFirstStepsDismissed, restoreFirstSteps } from "../homeProgress";
import { sectionDesc } from "../sections";
import { startTelegramLogin } from "../cloud/tgLogin";
import { startEmailLogin } from "../cloud/emailLogin";
import { startOAuthLogin } from "../cloud/oauthLogin";
import { getAuthProviders, type AuthProvider } from "../cloud/authProviders";
import {
  requestNotificationPermission, getNotifyPrefs, setNotifyPrefs, notificationsAllowed,
  type NotifyPrefs,
} from "../notifications";
import { useCapabilities } from "../hooks/useCapabilities";

/** Политика конфиденциальности опубликована только на каноничном хосте.
 *  Намеренно не getRelayBase(): у self-hosted релея страницы /privacy нет. */
const PRIVACY_URL = RELAY_BASE + "/privacy";

/**
 * Строка настроек, которая ведёт в раздел, — настоящая <button>, а не div с
 * role="button": пять таких строк не имели обработчика клавиатуры вовсе, и с
 * клавиатуры в «Мои компьютеры», SSH и «Панель ПК» было не попасть.
 *
 * Сброс стоит здесь, а не классом: класс `.settings-info-row` носят и <div>
 * (просто значение), и <a> (внешняя ссылка), и <button>, а браузер даёт кнопке
 * свою рамку, серый фон, шрифт Arial и текст по центру — строка поехала бы.
 * `border-bottom` НЕ трогаем: разделитель между строками задаёт класс, а
 * инлайновый `border: 0` перебил бы его и склеил карточку в сплошное полотно.
 */
const INFO_ROW_BUTTON: CSSProperties = {
  width: "100%",
  background: "none",
  borderTop: 0,
  borderLeft: 0,
  borderRight: 0,
  color: "inherit",
  fontFamily: "inherit",
  textAlign: "left",
  cursor: "pointer",
};

function formatUptime(seconds: number): string {
  const d = Math.floor(seconds / 86400);
  const h = Math.floor((seconds % 86400) / 3600);
  const m = Math.floor((seconds % 3600) / 60);
  if (d > 0) return t("ui.settingsview.m2a2dc0fd67", { p0: (d), p1: (h), p2: (m) });
  if (h > 0) return t("ui.settingsview.m2e7d50d6e2", { p0: (h), p1: (m) });
  return t("ui.settingsview.mc3327909f9", { p0: (m) });
}

/**
 * Человеческое имя входа — одно на список и на диалог подтверждения. Раньше в
 * списке стояло «Приложение Android», а вопрос звучал «Завершить вход
 * „android“?»: при двух похожих устройствах непонятно, тот ли закрываешь (N22).
 */
function loginClientName(session: CloudLoginSession): string {
  if (session.client_name) return session.client_name;
  if (session.client_kind === "android") return t("infra.logins.android");
  if (session.client_kind === "telegram") return "Telegram Mini App";
  return t("infra.logins.web");
}

/** Клиент открыт по loopback — окно exe или браузер на самом ПК. */
function onLoopbackHost(): boolean {
  if (typeof window === "undefined") return false;
  const host = window.location.hostname.toLowerCase().replace(/^\[(.*)\]$/, "$1");
  return host === "localhost" || host === "127.0.0.1" || host === "::1" || host === "0:0:0:0:0:0:0:1";
}

function timeAgoForSession(iso: string): string {
  const timestamp = Date.parse(iso);
  if (!Number.isFinite(timestamp)) return t("ui.infrastructureview.m211493da17");
  const minutes = Math.max(0, Math.floor((Date.now() - timestamp) / 60_000));
  if (minutes < 1) return t("chat.justNow");
  if (minutes < 60) return t("ui.infrastructureview.m8ceea3d01a", { p0: (minutes) });
  const hours = Math.floor(minutes / 60);
  if (hours < 24) return t("ui.infrastructureview.mb748cc3575", { p0: (hours) });
  return t("ui.infrastructureview.m644d0bda1d", { p0: (Math.floor(hours / 24)) });
}

export function SettingsView() {
  const navigate = useNavigate();
  // «←» ведёт туда, откуда пришли (кабинет, «Мои компьютеры», гид), а не
  // жёстко на главную: цепочка «Настройки → Подписка → ←» выбрасывала на
  // главную (аудит ИА 02.09.2026, P0-2). Системная «Назад» — тем же правилом.
  const goBack = useGoBack();
  const { toastSuccess, toastError } = useToast();
  const [config, setConfig] = useState<AppConfig | null>(null);
  // Конфиг компьютера грузится отдельно от аккаунта, и до его приезда экран не
  // имеет права утверждать, что компьютер не в сети: раньше «Не в сети» горело
  // красным уже на первом кадре, а упавший запрос оставлял этот вид навсегда.
  // Отсюда три различимых состояния: грузим → ошибка (с причиной и повтором) →
  // приехал.
  const [configLoading, setConfigLoading] = useState(true);
  const [configError, setConfigError] = useState<string | null>(null);
  const configReqRef = useRef(false);
  const [saving, setSaving] = useState(false);
  // Список агентов и его пересканирование уехали в раздел «Агенты».
  // id агента, у которого раскрыта команда установки (N13).
  const [stats, setStats] = useState<SystemStats | null>(null);
  const [checkingConnection, setCheckingConnection] = useState(false);
  const [connectionCheck, setConnectionCheck] = useState<"ok" | "fail" | null>(null);
  const [connectionRoute, setConnectionRoute] = useState(getMode);
  const { platform: machinePlatform, hasDisplay: machineHasDisplay } = useCapabilities();
  const serverEntity = machinePlatform === "linux" && !machineHasDisplay;
  const [me, setMe] = useState<CloudMe | null>(null);
  const [tgBusy, setTgBusy] = useState(false);
  const [loginSessions, setLoginSessions] = useState<CloudLoginSession[] | null>(null);
  const [sessionsBusy, setSessionsBusy] = useState(false);
  // Отметка «путь выбрали проводником» — по ней каталог настроек перечитывает значения.
  const [cwdPickedAt, setCwdPickedAt] = useState(0);
  // Список входов свёрнут по умолчанию: он самый длинный на экране и нужен
  // реже всего (жалоба владельца 09.08 «настройки выглядят перегружено»).
  // Исключение — когда за ним и пришли: «Управлять входами» из личного
  // кабинета вело на верх настроек, а список оставался свёрнутым тремя
  // экранами ниже, и человек считал кнопку сломанной (аудит ИА 02.09.2026,
  // P1-6). Кабинет передаёт `state.focus === "logins"` — тогда список открыт
  // сразу и прокручен в вид.
  const location = useLocation();
  const focusLogins = (location.state as { focus?: string } | null)?.focus === "logins"
    || new URLSearchParams(location.search).get("focus") === "logins";
  const section = settingsSection(location.search, focusLogins ? "logins" : undefined);
  const settingsTitleRef = useRef<HTMLHeadingElement>(null);
  useEffect(() => {
    document.querySelector(".settings-content")?.scrollTo(0, 0);
    window.scrollTo(0, 0);
    settingsTitleRef.current?.focus({ preventScroll: true });
  }, [section]);
  const [loginsOpen, setLoginsOpen] = useState(focusLogins);
  useEffect(() => { if (focusLogins) setLoginsOpen(true); }, [focusLogins]);
  const loginsHeadRef = useRef<HTMLButtonElement>(null);
  const inTelegram = !!getTelegram()?.initData;

  // Способы входа (мульти-провайдерная идентичность): список + привязка email.
  const [identities, setIdentities] = useState<CloudIdentity[] | null>(null);
  // Прямая ссылка из кабинета раскрывает входы. После загрузки способов
  // входа повторяем прокрутку: содержимое над списком могло стать выше.
  useEffect(() => {
    if (!focusLogins) return;
    loginsHeadRef.current?.scrollIntoView({ block: "start" });
  }, [focusLogins, identities, loginSessions]);
  const [linkEmail, setLinkEmail] = useState("");
  const [linkToken, setLinkToken] = useState<string | null>(null);
  const [linkCode, setLinkCode] = useState("");
  const [linkBusy, setLinkBusy] = useState(false);
  const [showLinkEmail, setShowLinkEmail] = useState(false);
  const [authProviders, setAuthProviders] = useState<AuthProvider[]>([]);
  const [linkOAuthBusy, setLinkOAuthBusy] = useState<string | null>(null);

  const refreshIdentities = () => {
    getIdentities()
      .then((r) => setIdentities(r.identities || []))
      .catch(() => setIdentities([]));
  };

  // Обновление APK (только нативное приложение; exe/веб обновляются иначе).
  const [appUpdate, setAppUpdate] = useState<AppUpdateInfo | null>(null);
  const [checkingUpdate, setCheckingUpdate] = useState(false);
  const [agentUpdate, setAgentUpdate] = useState<AgentUpdateInfo | null>(null);
  // «Обновить сейчас» в работе: агент перезапускается, связь на несколько
  // секунд пропадает — кнопка не должна нажиматься повторно.
  const [applyingAgentUpdate, setApplyingAgentUpdate] = useState(false);

  // Folder browser for default CWD
  const [showCwdPicker, setShowCwdPicker] = useState(false);
  // Какое поле сейчас выбирает папку: рабочая папка или папка входящих (N15).
  const [pickerField, setPickerField] = useState<"default_cwd" | "inbox_dir">("default_cwd");
  const [cwdQuickPaths, setCwdQuickPaths] = useState<QuickPath[]>([]);
  const [cwdBookmarks, setCwdBookmarks] = useState<Bookmark[]>([]);
  const [cwdRecentFolders, setCwdRecentFolders] = useState<RecentFolder[]>([]);
  const [cwdBrowsePath, setCwdBrowsePath] = useState("");
  const [cwdBrowseItems, setCwdBrowseItems] = useState<FileItem[]>([]);
  const [cwdBrowseLoading, setCwdBrowseLoading] = useState(false);
  const [cwdBrowseVersion, setCwdBrowseVersion] = useState(0);

  // Bookmarks management
  const [allBookmarks, setAllBookmarks] = useState<Bookmark[]>([]);

  // Непрочитанные ответы поддержки — бейдж на строке «Чат с поддержкой».
  // Счётчик общий (supportUnread.ts): та же цифра питает точку в нижней
  // навигации и локальное уведомление, поэтому разового запроса при открытии
  // настроек больше нет. Поллинг здесь включён, потому что на экране настроек
  // нижней навигации (общего держателя) нет.
  const supportUnread = useSupportUnread(true);

  // Уведомления, помощь и приватность живут в отдельных подкомпонентах
  // (NotificationsSection / HelpSection ниже): все три от компьютера не зависят
  // и обязаны работать в ветке «конфиг ПК не приехал» — раньше они были
  // отрисованы ПОСЛЕ early-return и на выключенном ПК просто исчезали (N124).

  // Hidden Advanced-mode unlock — 5 taps on the version label within 3s.
  const { ai } = useFeatures();
  const [showAdvanced, setShowAdvanced] = useState(false);

  const switchConnectionRoute = (mode: "self_hosted" | "cloud") => {
    const current = getServerConfig();
    if (mode === getMode()) return;
    if (mode === "self_hosted" && (!current?.url || !current?.token)) {
      navigate("/scan");
      return;
    }
    if (mode === "cloud" && (!current?.relayBase || !current?.jwt)) {
      navigate("/cloud-login");
      return;
    }
    saveConfig({ mode });
    resetCapabilities();
    reconnectWS();
    setConnectionRoute(mode);
    hapticSuccess();
  };
  const tapCountRef = useRef(0);
  const tapTimerRef = useRef<number | null>(null);

  const handleVersionTap = () => {
    if (showAdvanced || ai) return; // already unlocked
    tapCountRef.current += 1;
    if (tapTimerRef.current) window.clearTimeout(tapTimerRef.current);
    if (tapCountRef.current >= 5) {
      tapCountRef.current = 0;
      haptic("medium");
      setShowAdvanced(true);
      // Сразу открываем раздел с разблокированным тумблером.
      navigate(settingsPath("advanced"));
      toastSuccess(t("settings.advancedUnlocked"));
      return;
    }
    tapTimerRef.current = window.setTimeout(() => { tapCountRef.current = 0; }, 3000);
  };

  const toggleAI = (next: boolean) => {
    setFeatures({ ai: next });
    haptic();
    toastSuccess(next ? t("settings.advanced.aiOn") : t("settings.advanced.aiOff"));
  };

  /**
   * Кнопки ответа на вопрос агента. Настройка живёт на КОМПЬЮТЕРЕ (читает экран
   * его терминалов), поэтому переключатель сразу уходит в конфиг агента, а не
   * остаётся памятью телефона.
   */
  const detectQuestions = !!config?.detect_agent_questions;

  useEscape(showCwdPicker, () => setShowCwdPicker(false));

  /** Загрузить конфиг компьютера, различая «ещё грузим» и «не ответил». */
  const loadConfig = useCallback(async () => {
    if (configReqRef.current) return; // повтор поверх незакрытого запроса
    configReqRef.current = true;
    setConfigLoading(true);
    try {
      const loaded = await getConfig();
      setConfig(loaded);
      setConfigError(null);
      // Компьютер ответил и прислал конфиг — это и есть проверка связи. Раньше
      // карточка писала «Статус — ещё не проверяли» и предлагала нажать
      // «Проверить», хотя приложение только что с ним говорило (N9). Кнопка
      // остаётся как ПЕРЕпроверка.
      setConnectionCheck("ok");
    } catch (e) {
      setConfigError(mapApiError(e));
    } finally {
      configReqRef.current = false;
      setConfigLoading(false);
    }
  }, []);

  const handleRetryConfig = () => {
    haptic();
    void loadConfig();
  };

  // Пока компьютер не ответил — тихо пробуем ещё раз при возврате на экран
  // (телефон разбудили, мини-апп развернули): к этому моменту ПК часто уже в
  // сети, и человеку не нужно догадываться уходить и возвращаться.
  useEffect(() => {
    if (config) return;
    const onVisible = () => {
      if (document.visibilityState === "visible") void loadConfig();
    };
    document.addEventListener("visibilitychange", onVisible);
    return () => document.removeEventListener("visibilitychange", onVisible);
  }, [config, loadConfig]);

  useEffect(() => {
    void loadConfig();
    getSystemStats().then(setStats).catch(() => {});
    getAgentUpdateInfo().then((info) => {
      setAgentUpdate(info);
      if (info.available && info.latest) {
        const key = `remotai.agent-update-seen.${info.latest}`;
        try {
          if (!localStorage.getItem(key)) localStorage.setItem(key, String(Date.now()));
        } catch { /* ignore */ }
      }
    }).catch(() => {});
    getBookmarks().then((d) => setAllBookmarks(d.bookmarks || [])).catch(() => {});
    if (getServerConfig()?.mode === "cloud") {
      getMe().then(setMe).catch(() => {});
      refreshIdentities();
      listLoginSessions().then((result) => setLoginSessions(result.sessions || [])).catch(() => setLoginSessions([]));
      getAuthProviders().then((result) => setAuthProviders(result.providers || [])).catch(() => {});
    }
    // Нативный APK: тихо проверяем обновление при открытии настроек, чтобы сразу
    // показать версию и предложить апдейт (ошибку проглатываем — не критично).
    if (isNativeApp) checkAppUpdate().then(setAppUpdate).catch(() => {});
  }, []);

  const handleCheckAppUpdate = async () => {
    setCheckingUpdate(true);
    haptic();
    try {
      const info = await checkAppUpdate();
      setAppUpdate(info);
      if (info.hasUpdate) hapticSuccess();
      else toastSuccess(t("settings.appUpdateLatest"));
    } catch {
      toastError(t("settings.appUpdateCheckFailed"));
    } finally {
      setCheckingUpdate(false);
    }
  };

  /**
   * «Обновить сейчас» — применить обновление агента с телефона.
   *
   * Живая жалоба 30.07: человек видел «ждёт перезапуска компьютера», спрашивал
   * «надо комп перезагрузить?» и ждал часами. Перезапускается только программа
   * Remotai, и делать это можно откуда угодно: `POST /api/system/update`
   * применяет уже скачанное обновление без сети, иначе качает сам.
   *
   * Обрыв связи после команды — ОЖИДАЕМЫЙ исход, а не ошибка: агент отвечает и
   * уходит в рестарт. Поэтому провал самого запроса тоже ведёт в ожидание — мы
   * спрашиваем версию, пока она не сменится, и только по таймауту говорим
   * «затянулось».
   */
  const handleApplyAgentUpdate = async () => {
    haptic();
    setApplyingAgentUpdate(true);
    toastSuccess(t("settings.updateApplyRunning"));
    const before = agentUpdate?.current || agentUpdate?.version || "";
    try {
      await applyAgentUpdate();
    } catch (e) {
      // 404 «нет обновления» — единственный отказ, о котором стоит говорить
      // сразу: остальное (обрыв на рестарте) разберёт опрос версии ниже.
      if ((e as { status?: number })?.status === 404) {
        setApplyingAgentUpdate(false);
        await handleRecheckAgentUpdate();
        return;
      }
    }
    // Опрос возврата: агент отпускает порт ~2.5 с, потом поднимается новый
    // процесс. Через релей путь длиннее, поэтому окно ожидания — минута.
    const deadline = Date.now() + 60_000;
    const poll = async (): Promise<void> => {
      if (Date.now() > deadline) {
        setApplyingAgentUpdate(false);
        toastError(t("settings.updateApplySlow"));
        return;
      }
      await new Promise((resolve) => setTimeout(resolve, 3000));
      try {
        const info = await getAgentUpdateInfo();
        const now = info.current || info.version || "";
        if (now && now !== before && !info.pending_restart) {
          setAgentUpdate(info);
          setApplyingAgentUpdate(false);
          hapticSuccess();
          toastSuccess(t("settings.updateApplyDone", { version: `v${now}` }));
          return;
        }
      } catch { /* агент ещё перезапускается — это и есть ожидаемое состояние */ }
      return poll();
    };
    void poll();
  };

  /**
   * Проверить состояние отложенного обновления: часто к этому моменту компьютер
   * уже обновился сам, и плашка просто устарела.
   */
  const handleRecheckAgentUpdate = async () => {
    haptic();
    try {
      const info = await getAgentUpdateInfo();
      setAgentUpdate(info);
      if (!info.pending_restart) {
        hapticSuccess();
        toastSuccess(t("settings.updatePendingGone", { version: `v${info.current || info.version}` }));
      } else {
        toastSuccess(t("settings.updatePendingStill"));
      }
    } catch (e) {
      toastError(mapApiError(e));
    }
  };

  const update = async (field: string, value: any) => {
    if (!config) return;
    haptic();
    const updated = { ...config, [field]: value };
    setConfig(updated);
    setSaving(true);
    try {
      await updateConfig({ [field]: value });
      hapticSuccess();
      toastSuccess(t("settings.saved"));
    } catch (e: any) {
      toastError(mapApiError(e));
    }
    setSaving(false);
  };

  const handleCheckConnection = async () => {
    setCheckingConnection(true);
    setConnectionCheck(null);
    haptic();
    try {
      const nextStats = await getSystemStats();
      setStats(nextStats);
      setConnectionCheck("ok");
      hapticSuccess();
      toastSuccess(t("settings.connectionOk"));
    } catch (e: any) {
      setConnectionCheck("fail");
      toastError(mapApiError(e));
    } finally {
      setCheckingConnection(false);
    }
  };

  const handleReconnect = () => {
    haptic();
    reconnectWS();
    toastSuccess(t("settings.reconnectStarted"));
  };

  // Апгрейд анонимного облачного аккаунта до постоянного через Telegram.
  // Релей сольёт текущий анонимный аккаунт (с привязанными ПК) в Telegram-аккаунт.
  const handleTgUpgrade = async () => {
    setTgBusy(true);
    try {
      const h = await startTelegramLogin();
      const res = await h.done;
      if (res.ok) {
        hapticSuccess();
        toastSuccess(t("ui.settingsview.m3eefb0ede2"));
        reconnectWS();
        getMe().then(setMe).catch(() => {});
      } else if (res.reason === "expired" || res.reason === "timeout") {
        toastError(t("settings.notifyTgLinkTimeout"));
      }
    } catch (e: any) {
      toastError(mapApiError(e));
    } finally {
      setTgBusy(false);
    }
  };

  // Привязка email как второго способа входа (страховка от потери/блокировки
  // любого провайдера). Код шлёт общий /v1/auth/email/start, а подтверждение
  // идёт в /v1/me/identities/link с текущим JWT — релей прикрепит identity.
  const handleLinkEmailStart = async () => {
    const clean = linkEmail.trim();
    if (!clean.includes("@")) {
      toastError(t("ui.cloudloginview.mee5e999b8e"));
      return;
    }
    setLinkBusy(true);
    try {
      const result = await startEmailLogin(clean);
      setLinkToken(result.loginToken);
    } catch (e: any) {
      toastError(mapApiError(e));
    } finally {
      setLinkBusy(false);
    }
  };

  const handleLinkEmailVerify = async () => {
    const clean = linkCode.trim();
    if (clean.length !== 6) {
      toastError(t("ui.cloudloginview.mb9184d66dd"));
      return;
    }
    setLinkBusy(true);
    try {
      await linkEmailIdentity(linkToken!, clean);
      hapticSuccess();
      toastSuccess(t("ui.settingsview.mb60b905af3"));
      setLinkToken(null);
      setLinkCode("");
      setLinkEmail("");
      setShowLinkEmail(false);
      refreshIdentities();
    } catch (e: any) {
      toastError(mapConflict(e, t("cloud.identityTaken")));
    } finally {
      setLinkBusy(false);
    }
  };

  const handleLinkOAuth = async (provider: AuthProvider) => {
    setLinkOAuthBusy(provider.id);
    try {
      const handle = await startOAuthLogin(provider.id, { link: true });
      const result = await handle.done;
      if (result.ok) {
        hapticSuccess();
        toastSuccess(t("ui.settingsview.m9e32cb9a53", { p0: (provider.label) }));
        refreshIdentities();
        getMe().then(setMe).catch(() => {});
      } else if (result.reason === "expired" || result.reason === "timeout") {
        toastError(t("ui.settingsview.m14042b9ce1"));
      }
    } catch (error) {
      toastError(mapConflict(error, t("cloud.identityTaken")));
    } finally {
      setLinkOAuthBusy(null);
    }
  };

  const identityLabel = (id: CloudIdentity): string => {
    const names: Record<string, string> = {
      telegram: "Telegram",
      email: "Email",
      vk: "VK ID",
      yandex: t("ui.settingsview.mea48ad896a"),
      google: "Google",
      apple: "Apple",
    };
    return names[id.provider] || id.provider;
  };

  /**
   * Забыть LAN-подключение (адрес + токен этого ПК). Только для self_hosted:
   * в облаке эта же кнопка называлась «Сменить компьютер» и стирала облачный
   * JWT — для анонимного аккаунта это потеря доступа ко ВСЕМ своим ПК, которую
   * можно исправить только физически на компьютере. Смена машины в облаке — это
   * «Мои компьютеры» (/infrastructure).
   */
  const handleForgetServer = async () => {
    // Один глагол на все три экрана: кнопка, заголовок и подтверждение говорят
    // «забыть подключение». Раньше кнопка звалась «Забыть это подключение»,
    // вопрос — «Сменить компьютер?», подтверждение — «Сменить компьютер», а
    // последствие описывалось словами «токен» и «APK» (которых нет ни в окне на
    // ПК, ни в браузере): человек решал, что промахнулся, и жал «Отмена» (N5/N10).
    if (!(await tgConfirm(t("confirm.forgetLan"), {
      danger: true,
      title: t("confirm.forgetLanTitle"),
      confirmText: t("confirm.btn.forgetLan"),
    }))) return;
    disconnectWS();
    clearConfig();
    hapticSuccess();
    navigate("/login", { replace: true });
  };

  /**
   * Выйти из облачного аккаунта (список ПК скроется до следующего входа).
   *
   * У гостевого (анонимного) аккаунта постоянного входа нет, поэтому выход из
   * него необратим: JWT стёрт, а вернуться нечем — компьютеры остаются
   * привязанными к аккаунту, который больше не открыть, и отвязать их можно
   * только физически на каждой машине. Обещать обратимость («список скроется до
   * следующего входа») там нельзя: у гостя спрашиваем другим текстом, красная
   * кнопка вторична, а безопасный выход из диалога — привязать вход.
   */
  const handleSignOut = async () => {
    if (me != null && !permanentAccount) {
      const leave = await tgConfirm(t("confirm.signOutGuest"), {
        danger: true,
        title: t("confirm.signOutGuestTitle"),
        confirmText: t("confirm.btn.signOutGuest"),
        cancelText: t("confirm.btn.linkLoginFirst"),
      });
      if (!leave) {
        // Отказ = «сначала привязать вход»: сразу разворачиваем форму привязки
        // почты, если она доступна (кнопка Telegram и так стоит выше). Блок
        // «Способы входа» теперь есть в ОБЕИХ ветках экрана — при выключенном
        // компьютере кнопка раньше не делала ничего, и человек, решив, что она
        // сломана, повторял выход и терял аккаунт навсегда (N3).
        if (canLinkEmail) setShowLinkEmail(true);
        else toastSuccess(t("settings.linkLoginWhere"));
        return;
      }
    } else if (!(await tgConfirm(t("confirm.signOut"), { danger: true, confirmText: t("confirm.btn.signOut") }))) {
      return;
    }
    disconnectWS();
    clearConfig();
    hapticSuccess();
    navigate("/cloud-login", { replace: true });
  };

  /**
   * Конфликт (409) на облачных действиях. Общая таблица mapApiError отдаёт на
   * 409 единственный текст — «этот компьютер уже привязан к другому аккаунту,
   * нажмите на ПК „Отключить облако“». Для привязки второго способа входа и для
   * сессий это опасная неправда: человек идёт отвязывать рабочую машину. Пока у
   * релея нет машинных кодов на такие ответы, текст выбирает вызывающий; про
   * пейринг говорит только явный код device_taken — его переводит mapApiError.
   */
  const mapConflict = (e: unknown, conflictText: string): string => {
    const status = (e as { status?: number })?.status;
    const code = (e as { code?: string })?.code;
    if (status === 409 && code !== "device_taken") return conflictText;
    return mapApiError(e);
  };

  const handleSwitchTelegram = async () => {
    if (inTelegram) {
      toastSuccess(t("ui.settingsview.m7848530c88"));
      getTelegram()?.close();
      return;
    }
    await handleTgUpgrade();
  };

  /**
   * Закрыть ЧУЖОЙ вход. Текущая сессия сюда не приходит: у неё кнопки больше
   * нет — раньше на месте «Завершить» стояло «Выйти», ведущее в clearConfig, и
   * промах пальцем по 34-пиксельной кнопке выкидывал из аккаунта целиком (N38).
   * Выход из аккаунта живёт отдельно, в блоке «Аккаунт».
   */
  const handleRevokeSession = async (session: CloudLoginSession) => {
    if (session.current) return;
    // Имя берём то же, что человек читает в строке списка (N22).
    if (!(await tgConfirm(
      t("confirm.revokeLogin", { name: loginClientName(session) }),
      { danger: true, confirmText: t("confirm.btn.revokeLogin") },
    ))) return;
    setSessionsBusy(true);
    try {
      await revokeLoginSession(session.id);
      const result = await listLoginSessions();
      setLoginSessions(result.sessions || []);
      hapticSuccess();
      toastSuccess(t("settings.loginRevoked"));
    } catch (error) {
      // 409 здесь = «это текущий вход» (релей не даёт закрыть себя этой ручкой),
      // а не конфликт пейринга компьютера.
      toastError(mapConflict(error, t("settings.sessionCurrentConflict")));
    } finally {
      setSessionsBusy(false);
    }
  };

  const handleRevokeOtherSessions = async () => {
    const otherCount = loginSessions?.filter((session) => !session.current).length || 0;
    if (!otherCount) return;
    // «Запуски агентов» здесь не при чём — говорим это прямо: у разработчика с
    // работающими агентами красная кнопка иначе выглядит опасной (N14).
    if (!(await tgConfirm(
      t("confirm.revokeOtherLogins", { n: otherCount }),
      { danger: true, confirmText: t("confirm.btn.revokeOtherLogins") },
    ))) return;
    setSessionsBusy(true);
    try {
      const result = await revokeOtherLoginSessions();
      const refreshed = await listLoginSessions();
      setLoginSessions(refreshed.sessions || []);
      hapticSuccess();
      toastSuccess(t("settings.loginsRevoked", { n: result.revoked }));
    } catch (error) {
      toastError(mapApiError(error));
    } finally {
      setSessionsBusy(false);
    }
  };

  // Копирование команды установки и пересканирование агентов уехали вместе со
  // списком агентов в раздел «Агенты» (pages/AgentsView.tsx): держать их здесь
  // значило бы иметь два места, где одно и то же делается по-разному.

  /** Один и тот же выбор папки на два поля: рабочая папка и папка входящих
   *  (у второй кнопки не было вовсе — путь набирали пальцем, и опечатка
   *  сохранялась молча, N15). */
  const openCwdPicker = async (field: "default_cwd" | "inbox_dir" = "default_cwd") => {
    setPickerField(field);
    setShowCwdPicker(true);
    setCwdBrowsePath("");
    setCwdBrowseItems([]);
    if (cwdQuickPaths.length === 0) {
      getQuickPaths().then((d) => setCwdQuickPaths(d.paths || [])).catch(() => {});
      getBookmarks().then((d) => setCwdBookmarks(d.bookmarks || [])).catch(() => {});
      getRecentFolders().then((d) => setCwdRecentFolders(d.folders || [])).catch(() => {});
    }
  };

  const cwdBrowseTo = async (path: string) => {
    // Нажатая строка исчезнет при загрузке; удерживаем фокус на самом окне.
    document.querySelector<HTMLDivElement>(".settings-cwd-picker-sheet")?.focus();
    setCwdBrowseLoading(true);
    try {
      const data = await listFiles(path);
      setCwdBrowsePath(data.path);
      setCwdBrowseItems(data.items.filter((i: FileItem) => i.is_dir));
    } catch { /* ignore */ }
    setCwdBrowseLoading(false);
    // Ответ может оставить тот же path (например, псевдокаталог). Отдельная
    // версия гарантирует фокус первой строки и при неизменном пути.
    setCwdBrowseVersion((version) => version + 1);
  };

  const cwdBrowseParent = () => {
    const parent = cwdBrowsePath.replace(/[\\/][^\\/]+$/, "") || cwdBrowsePath;
    if (parent !== cwdBrowsePath) void cwdBrowseTo(parent);
    else { setCwdBrowsePath(""); setCwdBrowseItems([]); }
  };

  // Навигация заменяет быстрые папки списком и обратно. Когда нажатая строка
  // исчезает из DOM, браузер сбрасывает фокус на body; возвращаем его в окно.
  useEffect(() => {
    if (!showCwdPicker) return;
    const frame = requestAnimationFrame(() => {
      const dialog = document.querySelector<HTMLDivElement>(".settings-cwd-picker-sheet");
      if (!dialog || (dialog.contains(document.activeElement) && document.activeElement !== dialog)) return;
      const next = cwdBrowseLoading ? dialog
        : dialog.querySelector<HTMLElement>(cwdBrowsePath ? '.fm-item[role="button"]' : ".fm-quick-btn")
          ?? dialog.querySelector<HTMLElement>(".modal-actions button") ?? dialog;
      next.focus();
    });
    return () => cancelAnimationFrame(frame);
  }, [showCwdPicker, cwdBrowsePath, cwdBrowseLoading, cwdBrowseVersion]);

  const selectCwd = (path: string) => {
    update(pickerField, path);
    setShowCwdPicker(false);
    haptic();
    // Каталог настроек читает значения с компьютера отдельным запросом —
    // после выбора папки он обязан их перечитать, иначе покажет прежний путь.
    setCwdPickedAt(Date.now());
  };

  /**
   * Папку входящих набирают вручную, и сервер её не проверяет: опечатка давала
   * зелёное «Сохранено», а файлы потом уезжали в созданную по ошибке папку.
   * Поэтому перед сохранением проверяем путь и, если его нет, предлагаем
   * создать — вместо молчаливого «Сохранено» (N15).
   */
  const handleInboxDirBlur = async () => {
    const path = (config?.inbox_dir || "").trim();
    if (!path) { void update("inbox_dir", ""); return; } // пусто = папка по умолчанию
    try {
      await listFiles(path);
    } catch {
      const create = await tgConfirm(t("settings.inboxDirMissing", { path }), {
        title: t("settings.inboxDirMissingTitle"),
        confirmText: t("settings.inboxDirCreate"),
      });
      if (!create) return; // путь не сохраняем: обещать «Сохранено» тут нельзя
      try {
        await mkDir(path);
      } catch (e) {
        toastError(mapApiError(e));
        return;
      }
    }
    void update("inbox_dir", path);
  };

  const handleRemoveBookmark = async (path: string) => {
    if (!(await tgConfirm(t("generic.removeBookmark", { name: path.split(/[/\\]/).pop() || path }), { danger: true, confirmText: t("confirm.btn.delete") }))) return;
    // Без try/catch отвалившийся компьютер давал молчание: ни тоста, ни
    // изменений в списке — человек жал крестик снова и снова (N21).
    try {
      await removeBookmark(path);
      const d = await getBookmarks();
      setAllBookmarks(d.bookmarks || []);
      hapticSuccess();
      toastSuccess(t("settings.bookmarkRemoved"));
    } catch (e) {
      toastError(mapApiError(e));
    }
  };

  const serverConfig = getServerConfig();
  // Есть что «забыть» и куда пересканировать QR: реальное LAN-подключение к
  // конкретному ПК. В Telegram mode всегда cloud, а serverConfig может быть
  // пуст — проверка по одному лишь `mode !== "cloud"` показала бы там кнопку
  // «Забыть это подключение», которая выкинула бы человека на экран входа.
  const lanConnected = connectionRoute === "self_hosted" && !!serverConfig?.url;
  const permanentAccount = !!me && (me.permanent ?? ((me.telegram_id ?? 0) > 0));
  const accountProviderLabel: Record<string, string> = {
    telegram: "Telegram", email: "Email", vk: "VK ID", yandex: t("ui.settingsview.mea48ad896a"),
    google: "Google", apple: "Apple",
  };
  const linkedProviders = new Set((identities || []).map((identity) => identity.provider));
  const canLinkEmail = authProviders.some((provider) => provider.id === "email") && !linkedProviders.has("email");
  const oauthToLink = authProviders.filter((provider) =>
    provider.kind === "oauth" && !linkedProviders.has(provider.id),
  );

  /**
   * Аккаунт, способы входа и входы в аккаунт — ОДИН блок на обе ветки экрана.
   * Офлайн-ветка рисовала свою урезанную копию без «Способов входа» и без формы
   * привязки почты: «Сначала привязать вход» из диалога выхода там не открывало
   * ничего, человек считал кнопку сломанной и следующим тапом терял гостевой
   * аккаунт навсегда. От компьютера этот блок не зависит — как уведомления и
   * помощь (N3).
   */
  const accountSection = serverConfig?.mode !== "cloud" ? null : (
          <div className="setting-group">
            {/* Заголовки секций — настоящие h2: на экране их тринадцать, и без
                уровня диктор не даёт по ним оглавления, а перебирать настройки
                приходится подряд, элемент за элементом. Вид не меняется —
                размер и начертание задаёт класс. */}
            <h2 className="setting-label">{t("agentCheck.account")}</h2>
            <div className="settings-info-card">
              {/* Строка «Вход» показывается только ГОСТЮ.
                  У постоянного аккаунта она слово в слово повторяла первую
                  строку «Способов входа» ниже — «Telegram · Kickpointa» дважды
                  подряд на одном экране (видно на скриншоте владельца 09.08).
                  Гостю же деваться некуда: способов входа у него ещё нет, и
                  без этой строки он не поймёт, что работает анонимно. */}
              {!permanentAccount && (
                <div className="settings-info-row">
                  <span className="settings-info-label">{t("login.title")}</span>
                  <span className="settings-info-value">{me == null ? "…" : t("ui.settingsview.m9378bd4ded")}</span>
                </div>
              )}
              {/* Один список машин — одно имя во всех дверях. Раньше сюда вели
                  «Инфраструктура» (при живом ПК), «Мои компьютеры» (при
                  выключенном) и «Устройства» в навигации: кто вчера нашёл
                  «Мои компьютеры», сегодня их уже не узнавал (N8). */}
              <button type="button" className="settings-info-row" style={INFO_ROW_BUTTON} onClick={() => { haptic(); navigate("/infrastructure"); }}>
                <span className="settings-info-label">{t("settings.myComputers")}</span>
                <span className="settings-info-value">
                  {(serverConfig?.selectedDeviceName || config?.hostname || t("settings.myComputersOpen")) + " ›"}
                </span>
              </button>
            </div>

            {me != null && !permanentAccount && (
              <>
                <div style={{ fontSize: 12, color: "var(--tg-hint)", padding: "8px 0", lineHeight: 1.5 }}>
                  {t("ui.settingsview.mba6021c352")}</div>
                <button className="btn btn-primary btn-sm" disabled={tgBusy} onClick={() => void handleTgUpgrade()}>
                  {tgBusy ? t("settings.notifyTgConnecting") : t("ui.settingsview.m40f3c3b9ac")}
                </button>
              </>
            )}

            {/* Без отдельного заголовка: «Аккаунт» → «Способы входа» → «Входы в
                аккаунт» шли тремя секциями подряд про одно и то же, а в каждой
                было по строке-две. Способы входа — это и есть аккаунт. */}
            {identities != null && identities.length > 0 && (
              <div className="settings-info-card" style={{ marginTop: 8 }}>
                {identities.map((identity) => (
                  <div className="settings-info-row" key={identity.provider + ":" + identity.uid}>
                    <span className="settings-info-label">{identityLabel(identity)}</span>
                    <span className="settings-info-value">{identity.display || identity.uid}</span>
                  </div>
                ))}
              </div>
            )}
            {identities != null && identities.length === 1 && (
              <div className="settings-single-login-warning">
                {t("ui.settingsview.me6c55208df")}</div>
            )}
            {identities != null && !showLinkEmail && (
              <div className="settings-actions-row" style={{ marginTop: 8 }}>
                {canLinkEmail && (
                  <button className="btn btn-secondary btn-sm" onClick={() => setShowLinkEmail(true)}>
                    {t("ui.settingsview.m97d90c03dd")}</button>
                )}
                {oauthToLink.map((provider) => (
                  <button
                    key={provider.id}
                    className="btn btn-secondary btn-sm"
                    disabled={linkOAuthBusy !== null}
                    onClick={() => void handleLinkOAuth(provider)}
                  >
                    {linkOAuthBusy === provider.id ? t("ui.cloudloginview.mcad753516b", { p0: (provider.label) }) : t("ui.settingsview.md3a0b6102d", { p0: (provider.label) })}
                  </button>
                ))}
                {permanentAccount && (
                  <button className="btn btn-secondary btn-sm" disabled={tgBusy} onClick={() => void handleSwitchTelegram()}>
                    {tgBusy ? t("settings.notifyTgConnecting") : inTelegram ? t("ui.settingsview.m4fffc0b49a") : t("ui.settingsview.m9b7db2100d")}
                  </button>
                )}
                {/* «Выйти» стоит не здесь, а отдельным блоком ниже: рядом с
                    «Привязать email» его нажимают по инерции (у гостя это ещё и
                    необратимо), и он не должен пропадать вместе со списком
                    способов входа — на него ссылается подсказка под сессиями
                    (N38). */}
              </div>
            )}
            {showLinkEmail && (
              <div style={{ marginTop: 8 }}>
                {!linkToken ? (
                  <>
                    <div className="login-field">
                      <label>{t("ui.settingsview.md565d5199c")}</label>
                      <input
                        value={linkEmail}
                        onChange={(e) => setLinkEmail(e.target.value)}
                        placeholder="you@example.ru"
                        autoCapitalize="off"
                        autoCorrect="off"
                        spellCheck={false}
                        inputMode="email"
                      />
                    </div>
                    <div className="settings-actions-row" style={{ marginTop: 8 }}>
                      <button className="btn btn-secondary btn-sm" disabled={linkBusy} onClick={() => void handleLinkEmailStart()}>
                        {linkBusy ? t("ui.cloudloginview.m4cbdc4d6d2") : t("ui.settingsview.m7c5f96cd82")}
                      </button>
                      <button className="btn btn-secondary btn-sm" onClick={() => setShowLinkEmail(false)}>
                        {t("agentSessions.cancel")}</button>
                    </div>
                  </>
                ) : (
                  <>
                    <div className="login-field">
                      <label>{t("ui.cloudloginview.mce3c5d2aec")}</label>
                      <input
                        value={linkCode}
                        onChange={(e) => setLinkCode(e.target.value.replace(/\D/g, "").slice(0, 6))}
                        placeholder="123456"
                        inputMode="numeric"
                        autoComplete="one-time-code"
                        style={{ fontFamily: "var(--font-mono)", fontSize: 18, letterSpacing: 4, textAlign: "center" }}
                      />
                    </div>
                    <div className="settings-actions-row" style={{ marginTop: 8 }}>
                      <button className="btn btn-secondary btn-sm" disabled={linkBusy} onClick={() => void handleLinkEmailVerify()}>
                        {linkBusy ? t("agents.rescanning") : t("ui.settingsview.m4736e3acda")}
                      </button>
                      <button className="btn btn-secondary btn-sm" onClick={() => { setLinkToken(null); setLinkCode(""); }}>
                        {t("agentSessions.back")}</button>
                    </div>
                  </>
                )}
              </div>
            )}

            {/* Выход — отдельно от привязок и с честным описанием последствий:
                у гостя вернуться в анонимный аккаунт после выхода нечем, у
                постоянного аккаунта выход касается только этого устройства
                (чужие входы закрывает «Завершить» в списке ниже) — N38. */}
            {me != null && (permanentAccount || !showLinkEmail) && (
              <>
                <div style={{ fontSize: 12, color: "var(--tg-hint)", padding: "12px 0 6px", lineHeight: 1.5 }}>
                  {permanentAccount ? t("settings.signOutHint") : t("settings.signOutGuestHint")}
                </div>
                <div className="settings-actions-row">
                  <button className="btn btn-secondary btn-sm" onClick={() => void handleSignOut()}>
                    {t("settings.signOut")}
                  </button>
                </div>
              </>
            )}

            {/* «Сессия» на этом экране значила три разные вещи: входы в аккаунт,
                запуски агентов («Макс. параллельных сессий») и легаси-раздел.
                Здесь слово оставлено только за входами — иначе «Завершить все
                остальные сессии» читается как «убить мои запуски» (N14). */}
            {/* СВЁРНУТО по умолчанию (09.08.2026, жалоба владельца «настройки
                выглядят перегружено»).
                Замер того экрана: девять секций, три экрана телефона в высоту,
                13 органов управления против 1049 символов пояснений. Список
                входов — самая длинная часть и при этом самая редкая: свой вход
                человек и так видит, а чужие проверяет раз в жизни. Заголовок
                стал дверью со счётчиком: сколько их, видно не открывая. */}
            <button
              ref={loginsHeadRef}
              className="settings-advanced-head"
              style={{ marginTop: 18 }}
              onClick={() => { haptic(); setLoginsOpen((v) => !v); }}
              aria-expanded={loginsOpen}
            >
              <span className="settings-advanced-title">
                {t("settings.loginsTitle")}
                {loginSessions?.length ? ` · ${loginSessions.length}` : ""}
              </span>
              <span className={`settings-advanced-chevron${loginsOpen ? " open" : ""}`} aria-hidden>{"›"}</span>
            </button>
            {loginsOpen && (
            <>
            <div className="settings-session-hint">
              {t("settings.loginsHint")}
            </div>
            <div className="settings-info-card settings-sessions-card">
              {loginSessions == null && (
                <div className="settings-info-row">
                  <span className="settings-info-label">{t("settings.loginsLoading")}</span>
                </div>
              )}
              {loginSessions?.length === 0 && (
                <div className="settings-info-row">
                  <span className="settings-info-label">{t("ui.settingsview.m49b61552c7")}</span>
                </div>
              )}
              {loginSessions?.map((session) => (
                <div className="settings-login-session" key={session.id}>
                  <span className="settings-session-icon" aria-hidden>
                    {session.client_kind === "android" ? "▯" : session.client_kind === "telegram" ? "✈" : "◫"}
                  </span>
                  <span className="settings-session-main">
                    <b>{loginClientName(session)}</b>
                    <small>{session.current ? t("settings.loginThisOne") : t("ui.settingsview.mbe9e231ee1", { p0: (timeAgoForSession(session.last_seen_at)) })}{session.ip_address ? ` · ${session.ip_address}` : ""}</small>
                  </span>
                  {session.current ? (
                    <span className="settings-session-you">{t("settings.sessionThisDevice")}</span>
                  ) : (
                    <button disabled={sessionsBusy} onClick={() => void handleRevokeSession(session)}>
                      {t("settings.sessionEnd")}
                    </button>
                  )}
                </div>
              ))}
            </div>
            <div className="settings-session-hint" style={{ marginTop: 8 }}>
              {t("settings.sessionsSignOutHint")}
            </div>
            {(loginSessions?.some((session) => !session.current) ?? false) && (
              <button className="btn btn-secondary btn-sm settings-revoke-others" disabled={sessionsBusy} onClick={() => void handleRevokeOtherSessions()}>
                {sessionsBusy ? t("ui.settingsview.med9f72f03d") : t("settings.revokeOtherLogins")}
              </button>
            )}
            </>
            )}
          </div>
  );

  const connectionSection = config ? (
        <div className="setting-group">
          <h2 className="setting-label">{t("settings.connectionAppSection")}</h2>
          <ConnectionRouteSwitch route={connectionRoute} onSwitch={switchConnectionRoute} />
          <div className="settings-info-card">
            {/* Адрес сервера/релея — в «Расширенные → Сведения о компьютере»:
                на базовом экране он ничего не решает. */}
            <div className="settings-info-row">
              <span className="settings-info-label">{t("settings.connectionStatus")}</span>
              <span className={`settings-info-value ${connectionCheck === "ok" ? "settings-ok" : connectionCheck === "fail" ? "settings-bad" : ""}`}>
                {checkingConnection
                  ? t("settings.connectionChecking")
                  : connectionCheck === "ok"
                  ? t("settings.connectionOkShort")
                  : connectionCheck === "fail"
                  ? t("settings.connectionFailShort")
                  : t("settings.connectionUnknown")}
              </span>
            </div>
            {/* hostname / платформа / аптайм переехали в «Расширенные →
                Сведения о компьютере»: на базовом экране важно «на связи или
                нет», а не инвентарь. */}
            <div className="settings-info-row">
              <span className="settings-info-label">{t("settings.version")}</span>
              <span
                className="settings-info-value"
                onClick={handleVersionTap}
                style={{ cursor: "default", userSelect: "none" }}
              >
                Remotai v{config.version}
              </span>
            </div>
            {agentUpdate?.pending_restart ? (
              <>
                <div className="settings-update-state warn">
                  {t("settings.updatePending", { version: agentUpdate.latest ? `v${agentUpdate.latest}` : t("settings.updateNewVersion") })}
                </div>
                {/* Что именно перезапустится. Раньше здесь стояло «ждёт
                    перезапуска компьютера», и человек шёл перезагружать ПК. */}
                <div className="settings-update-why">
                  {agentUpdate.deferred_since && Date.now() - agentUpdate.deferred_since > 86_400_000
                    ? t("settings.updatePendingStuck")
                    : t("settings.updatePendingWhy")}
                </div>
              </>
            ) : agentUpdate?.available ? (
              <div className={`settings-update-state${(() => {
                try {
                  const seen = Number(localStorage.getItem(`remotai.agent-update-seen.${agentUpdate.latest}`) || Date.now());
                  return Date.now() - seen > 86_400_000 ? " warn" : "";
                } catch { return ""; }
              })()}`}>
                {t("settings.updateAvailableAuto", { version: `v${agentUpdate.latest}` })}
              </div>
            ) : null}
          </div>
          <div className="settings-actions-row">
            <button className="btn btn-secondary btn-sm" onClick={handleCheckConnection} disabled={checkingConnection}>
              {checkingConnection ? "\u23F3" : "\u2713"} {t("settings.checkConnection")}
            </button>
            <button className="btn btn-secondary btn-sm" onClick={handleReconnect}>
              {"\u21BB"} {t("settings.reconnect")}
            </button>
            {/* Обновление применяется с любого пульта: агент перезапускает сам
                себя (POST /api/system/update), скачанное обновление ставится без
                сети. Рядом — «Проверить состояние»: часто компьютер обновился
                сам, пока человек шёл в настройки. */}
            {(agentUpdate?.pending_restart || agentUpdate?.available) && (
              <button
                className="btn btn-primary btn-sm"
                disabled={applyingAgentUpdate}
                onClick={() => void handleApplyAgentUpdate()}
              >
                {applyingAgentUpdate ? "⏳" : "↑"} {t("settings.updateApplyNow")}
              </button>
            )}
            {agentUpdate?.pending_restart && (
              <button
                className="btn btn-secondary btn-sm"
                disabled={applyingAgentUpdate}
                onClick={() => void handleRecheckAgentUpdate()}
              >
                {"↻"} {t("settings.updatePendingRefresh")}
              </button>
            )}
            {/* «Сменить компьютер» переехало в «Расширенные»: на базовом экране
                это кнопка, которую ищут раз в жизни, а нажимают по ошибке. */}
          </div>

          {/* Обновление приложения — только нативный APK (sideload). exe тихо
              обновляется сам, веб всегда свежий. Раньше была отдельная группа
              «Приложение»; теперь — подраздел (h3) той же группы, строки те же. */}
          {isNativeApp && (
            <>
              <h3 className="setting-label" style={{ marginTop: 16 }}>{t("settings.appSection")}</h3>
              <div className="settings-info-card">
                <AppVersionRows appUpdate={appUpdate} />
              </div>
              <AppUpdateActions
                appUpdate={appUpdate}
                checking={checkingUpdate}
                onCheck={handleCheckAppUpdate}
              />
            </>
          )}
        </div>
  ) : (
          <div className="setting-group">
            <h2 className="setting-label">{t("settings.connectionAppSection")}</h2>
            <ConnectionRouteSwitch route={connectionRoute} onSwitch={switchConnectionRoute} />
            <div className="settings-info-card">
              <div className="settings-info-row">
                <span className="settings-info-label">{serverEntity ? t("ui.infrastructureview.m917e05143a") : t("infra.local.thisPcName")}</span>
                {/* Красным — только после честного отказа: пока запрос в пути,
                    это нейтральное «проверяем связь…». */}
                <span className={`settings-info-value ${configLoading ? "" : "settings-bad"}`}>
                  {configLoading ? t("settings.configLoadingShort") : t("settings.configOfflineShort")}
                </span>
              </div>
              {!configLoading && configError && (
                <div style={{ fontSize: 12, color: "var(--tg-hint)", padding: "6px 0 0", lineHeight: 1.5 }}>
                  {configError}
                </div>
              )}
              <div style={{ fontSize: 12, color: "var(--tg-hint)", padding: "6px 0", lineHeight: 1.5 }}>
                {configLoading ? t("settings.configLoadingHint") : t("settings.configOfflineHint")}
              </div>
            </div>
            {!configLoading && (
              /* «Повторить» на смене IP будет давать один и тот же результат
                 вечно, а единственный выход — заново отсканировать QR или
                 забыть подключение — лежал внутри свёрнутых «Расширенных» И
                 внутри ветки «конфиг приехал», то есть был недоступен ровно
                 тогда, когда нужен (N2). */
              <div className="settings-actions-row">
                <button className="btn btn-secondary btn-sm" onClick={handleRetryConfig}>
                  {"↻"} {t("settings.configRetry")}
                </button>
                {lanConnected && isNativeApp && (
                  <button className="btn btn-primary btn-sm" onClick={() => { haptic(); navigate("/scan"); }}>
                    {t("settings.rescanQr")}
                  </button>
                )}
                {lanConnected && (
                  <button className="btn btn-secondary btn-sm" onClick={handleForgetServer}>
                    {t("settings.forgetLan")}
                  </button>
                )}
              </div>
            )}
            {lanConnected && !configLoading && (
              <div style={{ fontSize: 12, color: "var(--tg-hint)", padding: "8px 0 0", lineHeight: 1.5 }}>
                {t("settings.lanOfflineHint")}
              </div>
            )}

            {/* Приложение — подраздел той же группы (h3), строки те же, что
                были в отдельной группе. */}
            <h3 className="setting-label" style={{ marginTop: 16 }}>{t("settings.appSection")}</h3>
            <div className="settings-info-card">
              <AppVersionRows appUpdate={appUpdate} />
              <div style={{ fontSize: 12, color: "var(--tg-hint)", padding: "6px 0", lineHeight: 1.5 }}>
                {configLoading ? t("settings.configVersionLoading") : t("settings.configVersionOffline")}
              </div>
            </div>
            <AppUpdateActions
              appUpdate={appUpdate}
              checking={checkingUpdate}
              onCheck={handleCheckAppUpdate}
            />
          </div>
  );
  const unavailable = <SettingsUnavailable loading={configLoading} onConnection={() => navigate(settingsPath("connection"))} />;

  return (
    <div className="page settings-page">
      <header className="page-header">
        <button className="back-btn" onClick={goBack} aria-label={t("generic.back")}>{"\u2190"}</button>
        <h1 ref={settingsTitleRef} tabIndex={-1}>{t(section ? `settings.nav.${section}` : "settings.title")}</h1>
        {saving && <span role="status" style={{ fontSize: 12, color: "var(--tg-hint)" }}>{t("config.saving")}</span>}
      </header>
      <main className="page-content settings-content" id="main">
        {!section ? (
          <SettingsIndex cloud={serverConfig?.mode === "cloud"}
            machineName={serverConfig?.selectedDeviceName || config?.hostname || t("settings.thisComputer")}
            loading={configLoading} available={!!config} unread={supportUnread} />
        ) : (
          <div className="settings-detail" data-settings-detail={section}>
            {["computer", "connection", "advanced"].includes(section) &&
              <div className="settings-detail-context">{serverConfig?.selectedDeviceName || config?.hostname || t("settings.thisComputer")}</div>}
            {section === "account" && (accountSection || <div className="settings-unavailable">
              <p>{t("settings.nav.accountNeeded")}</p>
              <button className="btn btn-primary" onClick={() => navigate("/cloud-login")}>{t("settings.nav.openAccount")}</button>
            </div>)}
            {section === "notifications" && <NotificationsSection questionsDetected={detectQuestions} />}
            {section === "connection" && connectionSection}
            {section === "help" && <HelpSection unread={supportUnread} />}
            {section === "local" && <LocalCapabilitiesSection machineName={serverConfig?.selectedDeviceName || config?.hostname || t("settings.thisComputer")} />}
            {section === "computer" && (config ? <>
        {/* Снимок экрана по PrtScr. Настройка живёт НА КОМПЬЮТЕРЕ (клавишу
            занимает его агент), поэтому тумблер сразу уходит в его конфиг.
            Показываем ФАКТ, а не только желание: Windows отдаёт PrtScr первому,
            кто попросил, и «включено» при занятой клавише было бы враньём —
            агент присылает screenshot_hotkey_active и причину отказа. */}
        <div className="setting-group">
          <h2 className="setting-label">{t("settings.shotHotkey")}</h2>
          <div className="settings-info-card">
            <div className="settings-info-row">
              <span className="settings-info-label">{t("settings.shotHotkey")}</span>
              <button
                className={`advanced-toggle${config.screenshot_hotkey ? " on" : ""}`}
                onClick={() => void update("screenshot_hotkey", !config.screenshot_hotkey)}
                aria-pressed={!!config.screenshot_hotkey}
                aria-label={t("settings.shotHotkey")}
              >
                <span className="advanced-toggle-thumb" />
              </button>
            </div>
            <div style={{ fontSize: 11, color: "var(--tg-hint)", padding: "0 0 8px" }}>
              {config.screenshot_hotkey && config.screenshot_hotkey_error === "hotkey_taken"
                ? t("settings.shotHotkeyTaken")
                : config.screenshot_hotkey && config.screenshot_hotkey_error === "hotkey_unsupported"
                  ? t("settings.shotHotkeyUnsupported")
                  : config.screenshot_hotkey && config.screenshot_hotkey_active
                    ? t("settings.shotHotkeyActive")
                    : t("settings.shotHotkeyHint")}
            </div>
          </div>
        </div>
        {serverConfig?.mode !== "cloud" && (
          <div className="setting-group">
            <h2 className="setting-label">{t("infra.connected.devices")}</h2>
            <div className="settings-info-card">
              <button
                type="button"
                className="settings-info-row"
                style={INFO_ROW_BUTTON}
                onClick={() => { haptic(); navigate("/ssh"); }}
              >
                {/* Имя раздела — из словаря, а не литералом: раздел звался
                    пятью способами сразу. Подписи здесь больше нет: строка
                    настроек — это дверь формата «имя → значение», и полная
                    подпись раздела («Терминал, файлы и проброс портов… — через
                    этот компьютер») занимала в ней три строки, оттесняя само
                    имя в две. Что внутри раздела, рассказывает каталог — шторка
                    «Ещё» и боковая панель. */}
                <span className="settings-info-label">{t("settings.sshSection")}</span>
                <span className="settings-info-value">{"›"}</span>
              </button>
              {/* Про «это окно… на котором открыто» человеку с телефоном в руках
                  говорить нельзя — он читает про чужое устройство, а ссылка на
                  веб-клиент уводит его на экран входа в аккаунт, которого у него
                  нет (он просто отсканировал QR). Поэтому дверь разная: в окне
                  на ПК — веб-клиент, на телефоне по локальной сети —
                  подключение к другому компьютеру (N11). */}
              {isNativeApp ? (
                <button
                  type="button"
                  className="settings-info-row"
                  style={INFO_ROW_BUTTON}
                  onClick={() => { haptic(); navigate("/scan"); }}
                >
                  <span className="settings-info-label">{t("settings.connectAnotherPc")}</span>
                  <span className="settings-info-value">{t("settings.connectAnotherPcAction")}</span>
                </button>
              ) : (
                <button
                  type="button"
                  className="settings-info-row"
                  style={INFO_ROW_BUTTON}
                  onClick={() => { haptic(); window.open(`${RELAY_BASE.replace(/\/+$/, "")}/app`, "_blank", "noopener,noreferrer"); }}
                >
                  {/* Оба слова стояли здесь русскими литералами мимо словаря:
                      «Все машины аккаунта» было ЧЕТВЁРТЫМ именем того же списка
                      машин, а перевести интерфейс с такими строками нечем.
                      Ведёт строка не в раздел приложения, а на внешний сайт,
                      поэтому имя раздела дополнено местом назначения. */}
                  <span className="settings-info-label">{t("settings.webClient")}</span>
                  <span className="settings-info-value">{t("settings.webClientAction")}</span>
                </button>
              )}
              {/* «Панель» и «Настройки» — две двери в одно и то же (обновления,
                  привязка телефона, статус связи), и человек каждый раз гадает,
                  в какую идти. Здесь оставляем одну строку-указатель вместо
                  повтора содержимого (N23). */}
              {isOnPCPanel() && (
                <button
                  type="button"
                  className="settings-info-row"
                  style={INFO_ROW_BUTTON}
                  onClick={() => { haptic(); navigate("/panel"); }}
                >
                  <span className="settings-info-label">{t("settings.thisComputer")}</span>
                  <span className="settings-info-value">{t("settings.thisComputerAction")}</span>
                </button>
              )}
            </div>
            <div style={{ fontSize: 12, color: "var(--tg-hint)", padding: "8px 0", lineHeight: 1.5 }}>
              {isNativeApp ? t("settings.lanOnePcHint") : t("settings.pcWindowOnePcHint")}
            </div>
          </div>
        )}
              <ComputerSettings onPickFolder={(key) => void openCwdPicker(key as "default_cwd" | "inbox_dir")} refreshToken={cwdPickedAt} />
            </> : unavailable)}
            {section === "advanced" && (config ? <>

        {/* Всё про агентов уехало в раздел «Агенты»: аккаунты, лимиты, что
            установлено и как запускается — один разговор, одно место. Здесь
            осталась дверь туда, а не вторая копия тех же переключателей. */}
          <div className="setting-group">
            <button className="settings-link-row" onClick={() => { haptic(); navigate("/agents"); }}>
              <span>
                <b>{"\u{1F916}"} {t("agents.title")}</b>
                {/* Подпись раздела — из общей таблицы: у «Агентов» своих
                    описаний было четыре, и все разные. */}
                <small>{sectionDesc("usage")}</small>
              </span>
              <span aria-hidden>{"\u203A"}</span>
            </button>
          </div>

        {/* Рабочая папка и папка входящих переехали в «Настройки компьютера»
            выше — туда, где рядом лежит объяснение, что это такое и куда идёт.
            Два поля на одну настройку и есть то, что владелец назвал «в
            расширенных дублируется часть настроек» (09.08.2026). Выбор папки
            проводником уехал вместе с ними, ничего не потерялось. */}
        {/* Dispatch settings — AI-only.
            «Диспатч (ACP)» и «TTL по умолчанию (мин)» — слова из внутренних
            обсуждений: ни «диспатч», ни «ACP», ни «TTL» не встречаются больше
            нигде в продукте, и решить, надо ли сюда заходить, было нельзя.
            Заголовки говорят о деле — про одновременные запуски агентов и про
            закрытие неактивных (N4/N20). */}
        {ai && (
        <div className="setting-group">
          <h2 className="setting-label">{t("settings.runsSection")}</h2>

          <div className="settings-info-card">
            {/* «Одновременных запусков» ЗДЕСЬ БОЛЬШЕ НЕТ: та же настройка живёт
                в «Настройках компьютера» выше — с объяснением и пометкой риска,
                одна на человека и на AI-агента. Два поля на одну настройку —
                это то, что владелец назвал «в расширенных дублируется часть
                настроек» (09.08.2026). */}
            <div className="settings-info-row">
              <span className="settings-info-label">{t("settings.idleRunTtl")}</span>
              <input
                type="number"
                inputMode="numeric"
                className="settings-inline-input"
                min={0}
                value={config.default_ttl_minutes || 0}
                onChange={(e) => {
                  const v = parseInt(e.target.value) || 0;
                  setConfig({ ...config, default_ttl_minutes: v });
                }}
                onBlur={() => update("default_ttl_minutes", config.default_ttl_minutes || 0)}
                style={{ width: 60, textAlign: "center" }}
              />
            </div>
            <div style={{ fontSize: 11, color: "var(--tg-hint)", padding: "0 0 4px" }}>
              {t("settings.idleRunTtlHint")}
            </div>
          </div>
        </div>
        )}

        {/* Bookmarks management — пустую секцию-плейсхолдер не показываем. */}
        {allBookmarks.length > 0 && (
        <div className="setting-group">
          <h2 className="setting-label">{t("settings.bookmarks")}</h2>
          {(
            <div className="settings-info-card">
              {allBookmarks.map((bm) => (
                <div key={bm.path} className="settings-info-row" style={{ cursor: "pointer" }}>
                  <span className="settings-info-label" style={{ flex: 1 }}>
                    {"\uD83D\uDCCC"} {bm.name}
                  </span>
                  <span className="settings-info-value" style={{ fontSize: 11, flex: 2, wordBreak: "break-all" }}>
                    {bm.path}
                  </span>
                  <button
                    className="icon-btn"
                    style={{ fontSize: 14, padding: "4px 8px", flexShrink: 0 }}
                    onClick={() => handleRemoveBookmark(bm.path)}
                  >
                    {"\u2715"}
                  </button>
                </div>
              ))}
            </div>
          )}
        </div>
        )}

        {/* Tip — AI-only */}
        {ai && (
          <div style={{ padding: "8px 20px", fontSize: 12, color: "var(--tg-hint)", lineHeight: 1.5 }}>
            {t("settings.sessionSettingsHint")}
          </div>
        )}

        {/* Advanced mode (revealed via 5-tap on version) */}
        {(showAdvanced || ai) && (
          <div className="setting-group">
            <h2 className="setting-label">{t("settings.advanced.title")}</h2>
            <div className="settings-info-card">
              <div className="settings-info-row">
                <span className="settings-info-label">{t("settings.advanced.ai")}</span>
                <button
                  className={`advanced-toggle${ai ? " on" : ""}`}
                  onClick={() => toggleAI(!ai)}
                  aria-pressed={ai}
                  aria-label={t("settings.advanced.ai")}
                >
                  <span className="advanced-toggle-thumb" />
                </button>
              </div>
              <div style={{ fontSize: 11, color: "var(--tg-hint)", padding: "0 0 8px" }}>
                {t("settings.advanced.aiHint")}
              </div>
              {/* Кнопки ответа на вопрос агента. Выключено по умолчанию: чтобы
                  показать их, компьютер читает ЭКРАН терминала, а текст на
                  экране легко спутать — агент печатал ответ ПРО вопросы, и
                  терминал объявлял «ждёт подтверждения» при законченной работе. */}
              {/* Сам переключатель «кнопки ответа на вопросы агента» живёт в
                  разделе «Агенты» — там же, где всё остальное про них. Здесь
                  он только читается: от него зависят уведомления. */}
              {ai && (
                // «Легаси» — слово из внутренних обсуждений: человеку оно
                // сообщало только то, что разработчики сами не рады разделу, но
                // не что там лежит и стоит ли туда идти (N18).
                <button className="settings-link-row" onClick={() => navigate("/sessions")}>
                  <span>{t("settings.advanced.oldChat")}</span>
                  <span>›</span>
                </button>
              )}
            </div>
          </div>
        )}

        {/* Сведения о компьютере + смена ПК: техданные и редкое действие,
            которые раньше занимали место в базовой карточке подключения. */}
        <div className="setting-group">
          <h2 className="setting-label">{t(serverEntity ? "settings.systemInfoServer" : "settings.systemInfo")}</h2>
          <div className="settings-info-card">
            {serverConfig && (
              <div className="settings-info-row">
                <span className="settings-info-label">
                  {serverConfig.mode === "cloud" ? t("settings.cloudRelay") : t("settings.serverAddress")}
                </span>
                <span className="settings-info-value">
                  {serverConfig.mode === "cloud"
                    ? (serverConfig.relayBase || RELAY_BASE).replace(/^https?:\/\//, "")
                    : serverConfig.url}
                </span>
              </div>
            )}
            <div className="settings-info-row">
              <span className="settings-info-label">{t("settings.hostname")}</span>
              <span className="settings-info-value">{config.hostname}</span>
            </div>
            <div className="settings-info-row">
              <span className="settings-info-label">{t("settings.platform")}</span>
              <span className="settings-info-value">{config.platform}</span>
            </div>
            {stats && (
              <div className="settings-info-row">
                <span className="settings-info-label">{t("settings.uptime")}</span>
                <span className="settings-info-value">{formatUptime(stats.uptime)}</span>
              </div>
            )}
          </div>
          {/* Кнопки «Сменить компьютер» здесь больше нет: этот же экран уже
              ведёт в «Мои компьютеры» строкой выше, и одна дверь называлась
              двумя разными словами — человек читает их как два разных места
              (UX-аудит 2026-08-23, HOLISTIC-5). Осталось действие, которого
              больше нигде нет: забыть прямое подключение по локальной сети.
              Не разрушительное — без «красной кнопки» (подтверждение и так
              спрашивается в handleForgetServer). */}
          {serverConfig?.mode !== "cloud" && (
            <div className="settings-actions-row">
              <button className="btn btn-secondary btn-sm" onClick={handleForgetServer}>
                {t("settings.forgetLan")}
              </button>
            </div>
          )}
        </div>

            </> : unavailable)}
          </div>
        )}
      </main>

      {/* CWD Folder Picker Modal */}
      {showCwdPicker && (
        <SheetShell open={showCwdPicker} onClose={() => setShowCwdPicker(false)}
          overlayClassName="modal-overlay" className="modal-sheet settings-cwd-picker-sheet"
          labelledBy="settings-cwd-picker-title">
            {/* Заголовок называет поле, которое сейчас заполняем: один и тот же
                выбор папки открывают две разные строки настроек (N15). */}
            <div className="modal-title" id="settings-cwd-picker-title">{t(pickerField === "inbox_dir" ? "settings.inboxDir" : "settings.defaultCwd")}</div>

            {!cwdBrowsePath ? (
              <div style={{ overflow: "auto", flex: 1 }}>
                <div className="fm-section-title">{t("files.quickAccess")}</div>
                <div className="fm-quick-grid">
                  {cwdQuickPaths.map((qp) => (
                    <button key={`${qp.name}:${qp.path}`} className="fm-quick-btn" onClick={() => { haptic(); cwdBrowseTo(qp.path); }}>
                      <span className="fm-quick-icon">{"\uD83D\uDCC1"}</span>
                      <span className="fm-quick-label">{qp.name}</span>
                    </button>
                  ))}
                </div>
                {cwdBookmarks.length > 0 && (
                  <>
                    <div className="fm-section-title" style={{ marginTop: 12 }}>{t("files.bookmarks")}</div>
                    <div className="fm-quick-grid">
                      {cwdBookmarks.map((bm) => (
                        <button key={bm.path} className="fm-quick-btn" onClick={() => { haptic(); cwdBrowseTo(bm.path); }}>
                          <span className="fm-quick-icon">{"\uD83D\uDCCC"}</span>
                          <span className="fm-quick-label">{bm.name}</span>
                        </button>
                      ))}
                    </div>
                  </>
                )}
                {cwdRecentFolders.length > 0 && (
                  <>
                    <div className="fm-section-title" style={{ marginTop: 12 }}>{t("settings.recentFolders")}</div>
                    <div className="fm-quick-grid">
                      {cwdRecentFolders.map((rf) => (
                        <button key={rf.path} className="fm-quick-btn" onClick={() => { haptic(); cwdBrowseTo(rf.path); }}>
                          <span className="fm-quick-icon">{"\uD83D\uDD52"}</span>
                          <span className="fm-quick-label">{rf.name}</span>
                        </button>
                      ))}
                    </div>
                  </>
                )}
              </div>
            ) : (
              <div style={{ overflow: "auto", flex: 1 }}>
                <div style={{
                  background: "var(--tg-secondary-bg)", borderRadius: 8,
                  padding: "8px 12px", marginBottom: 8, fontSize: 13,
                  fontFamily: "monospace", wordBreak: "break-all",
                }}>
                  {"\uD83D\uDCC2"} {cwdBrowsePath}
                </div>

                {cwdBrowseLoading ? (
                  <div className="loading-center" style={{ padding: 24 }}><div className="spinner" /></div>
                ) : (
                  <div className="fm-list" style={{ margin: 0 }}>
                    <div className="fm-item" role="button" tabIndex={0} aria-label={t("files.up")}
                      onClick={cwdBrowseParent}
                      onKeyDown={(event) => {
                        if (event.key === "Enter" || event.key === " ") { event.preventDefault(); cwdBrowseParent(); }
                      }}>
                      <span className="fm-icon">{"\u2B06"}</span>
                      <div className="fm-info"><div className="fm-name">..</div></div>
                    </div>
                    {cwdBrowseItems.map((item) => (
                      <div key={item.path} className="fm-item" role="button" tabIndex={0}
                        aria-label={`${t("files.folder")}: ${item.name}`}
                        onClick={() => { haptic(); void cwdBrowseTo(item.path); }}
                        onKeyDown={(event) => {
                          if (event.key === "Enter" || event.key === " ") {
                            event.preventDefault(); haptic(); void cwdBrowseTo(item.path);
                          }
                        }}>
                        <span className="fm-icon">{"\uD83D\uDCC1"}</span>
                        <div className="fm-info"><div className="fm-name">{item.name}</div></div>
                        <span className="fm-chevron">{"\u203A"}</span>
                      </div>
                    ))}
                    {cwdBrowseItems.length === 0 && (
                      <div style={{ padding: 16, textAlign: "center", color: "var(--tg-hint)", fontSize: 13 }}>
                        {t("modal.noSubfolders")}
                      </div>
                    )}
                  </div>
                )}
              </div>
            )}

            <div className="modal-actions" style={{ marginTop: 12, flexShrink: 0 }}>
              <button className="btn btn-secondary" onClick={() => {
                if (cwdBrowsePath) { setCwdBrowsePath(""); setCwdBrowseItems([]); }
                else setShowCwdPicker(false);
              }}>
                {cwdBrowsePath ? `\u2190 ${t("pty.back")}` : t("modal.cancel")}
              </button>
              {cwdBrowsePath && (
                <button className="btn btn-primary" onClick={() => selectCwd(cwdBrowsePath)}>
                  {t("generic.select")}
                </button>
              )}
            </div>
        </SheetShell>
      )}
      <BottomNav active="settings" />
    </div>
  );
}

/**
 * Выбор маршрута до компьютера. Переключатель рисуется только там, где обе
 * стороны выбора реально работают, — то есть в нативном приложении:
 *
 *  • Telegram: getMode() жёстко возвращает cloud, а единственный путь к
 *    LAN-подключению — сканер QR, которого в мини-аппе нет (N59).
 *  • Окно на самом ПК (isOnPCPanel): «Через интернет» человек читает как
 *    «разрешить доступ снаружи» (так называется соседняя кнопка в Панели), а
 *    получает переезд клиента на релей и ИСЧЕЗНУВШУЮ вкладку «Панель» — она
 *    показывается только в self_hosted. Молча менять это окно нельзя (N6).
 *    Выход из cloud-режима обратно в LAN переключатель по-прежнему даёт: там
 *    isOnPCPanel() уже false.
 *  • Веб-клиент (remotai.ru/app): HTTPS-страница не может ходить на
 *    http://192.168.x.x, поэтому «По локальной сети» вело в камеру и потом в
 *    «проверьте, что телефон в одной сети с ПК» — человек шёл чинить роутер,
 *    хотя чинить нечего (N7).
 */
function ConnectionRouteSwitch({
  route,
  onSwitch,
}: {
  route: "self_hosted" | "cloud";
  onSwitch: (mode: "self_hosted" | "cloud") => void;
}) {
  if (getTelegram()?.initData) {
    return (
      <div className="settings-info-card">
        <div className="settings-info-row">
          <span className="settings-info-label">{t("settings.connectionRoute")}</span>
          <span className="settings-info-value">{t("settings.routeFixedCloud")}</span>
        </div>
        <div style={{ fontSize: 12, color: "var(--tg-hint)", padding: "6px 0", lineHeight: 1.5 }}>
          {t("settings.routeTelegramHint")}
        </div>
      </div>
    );
  }
  if (isOnPCPanel()) {
    return (
      <div className="settings-info-card">
        <div className="settings-info-row">
          <span className="settings-info-label">{t("settings.connectionRoute")}</span>
          <span className="settings-info-value">{t("settings.routeFixedDirect")}</span>
        </div>
        <div style={{ fontSize: 12, color: "var(--tg-hint)", padding: "6px 0", lineHeight: 1.5 }}>
          {t("settings.routeOnPcHint")}
        </div>
      </div>
    );
  }
  // Loopback-хост исключаем отдельно: окно exe, уже уехавшее в облако, тоже
  // не isOnPCPanel() — и без переключателя ему нечем вернуться в локальный
  // режим, где живёт вкладка «Панель».
  if (!isNativeApp && !onLoopbackHost()) {
    return (
      <div className="settings-info-card">
        <div className="settings-info-row">
          <span className="settings-info-label">{t("settings.connectionRoute")}</span>
          <span className="settings-info-value">{t("settings.routeFixedCloud")}</span>
        </div>
        <div style={{ fontSize: 12, color: "var(--tg-hint)", padding: "6px 0", lineHeight: 1.5 }}>
          {t("settings.routeWebHint")}
        </div>
      </div>
    );
  }
  return (
    <div className="settings-route-switch" role="group" aria-label={t("settings.connectionRoute")}>
      <button
        className={route === "self_hosted" ? "active" : ""}
        aria-pressed={route === "self_hosted"}
        onClick={() => onSwitch("self_hosted")}
      >
        {t("settings.routeLan")}
      </button>
      <button
        className={route === "cloud" ? "active" : ""}
        aria-pressed={route === "cloud"}
        onClick={() => onSwitch("cloud")}
      >
        {t("settings.routeCloud")}
      </button>
    </div>
  );
}

/** Версия приложения (и найденное обновление) — одинаково в обеих ветках экрана. */
function AppVersionRows({ appUpdate }: { appUpdate: AppUpdateInfo | null }) {
  if (!isNativeApp) {
    return (
      <div className="settings-info-row">
        <span className="settings-info-label">{t("settings.appUpdateWeb")}</span>
        <span className="settings-info-value">{t("settings.appUpdateWebAuto")}</span>
      </div>
    );
  }
  return (
    <>
      <div className="settings-info-row">
        <span className="settings-info-label">{t("settings.appVersion")}</span>
        <span className="settings-info-value">{appUpdate ? `v${appUpdate.current}` : "…"}</span>
      </div>
      {appUpdate?.hasUpdate && (
        <div style={{ fontSize: 12, color: "var(--color-success)", padding: "4px 0 2px", lineHeight: 1.5 }}>
          {"✨"} {t("settings.appUpdateAvailable", { version: `v${appUpdate.latest}` })}
          {appUpdate.changelog ? ` — ${appUpdate.changelog}` : ""}
        </div>
      )}
    </>
  );
}

/** Кнопки «Обновить»/«Проверить обновления» — только в нативном APK (sideload). */
function AppUpdateActions({
  appUpdate,
  checking,
  onCheck,
}: {
  appUpdate: AppUpdateInfo | null;
  checking: boolean;
  onCheck: () => void;
}) {
  if (!isNativeApp) return null;
  return (
    <div className="settings-actions-row">
      {appUpdate?.hasUpdate ? (
        <button
          className="btn btn-primary btn-sm"
          onClick={() => { haptic(); openApkDownload(appUpdate.apkUrl); }}
        >
          {"⬇"} {t("settings.appUpdateBtn", { version: `v${appUpdate.latest}` })}
        </button>
      ) : (
        <button className="btn btn-secondary btn-sm" onClick={onCheck} disabled={checking}>
          {checking ? "⏳" : "↻"} {t("settings.appUpdateCheck")}
        </button>
      )}
    </div>
  );
}

/**
 * Уведомления: два независимых канала.
 *
 *  1. Пуш на экран этого телефона — только нативный APK (Capacitor), настройка
 *     живёт в localStorage (notifications.ts).
 *  2. Сообщения бота в Telegram — свойство АККАУНТА на релее (/v1/me/notify).
 *     Раньше про этот канал экран не знал ничего: текст уверял, что вне Android
 *     уведомлений нет, а бот всё это время писал в личку, и выключался только
 *     командой /notify в чате (N67/N169).
 *
 * Секция вынесена из тела экрана, потому что от конфига компьютера не зависит и
 * обязана работать, когда ПК не в сети (N124).
 */
/**
 * @param questionsDetected — включено ли распознавание вопросов агента. Пока
 * оно выключено (по умолчанию с 2.49.4), сигнал «агент ждёт ответа» не придёт
 * НИКОГДА: событие рождается там же, где статус. Молча оставлять переключатель
 * нельзя — человек включит его и будет ждать сообщения, которого не будет.
 */
function NotificationsSection({ questionsDetected }: { questionsDetected?: boolean }) {
  const navigate = useNavigate();
  const { toastSuccess, toastError } = useToast();
  const [prefs, setPrefsState] = useState<NotifyPrefs>(getNotifyPrefs);
  const [allowed, setAllowed] = useState<boolean | null>(notificationsAllowed);
  const cloudAccount = cloudAccountAvailable();
  const [tgPrefs, setTgPrefs] = useState<CloudNotifyPrefs | null>(null);
  const [tgError, setTgError] = useState<string | null>(null);
  const [tgBusy, setTgBusy] = useState(false);
  // Привязка Telegram прямо отсюда. Вне APK секция была двумя заголовками и
  // двумя абзацами без единого действия: пуш умеет только Android, тумблеры
  // бота открываются после привязки, а кнопки, которая к этой привязке ведёт,
  // на экране не было вовсе — человек читал два объяснения и уходил (§6.4).
  const [tgLinkBusy, setTgLinkBusy] = useState(false);

  const loadTgPrefs = useCallback(() => {
    if (!cloudAccount) return;
    setTgError(null);
    getCloudNotifyPrefs()
      .then(setTgPrefs)
      .catch((e) => setTgError(mapApiError(e)));
  }, [cloudAccount]);

  useEffect(() => { loadTgPrefs(); }, [loadTgPrefs]);

  /**
   * Локальный пуш — свойство устройства: раньше тумблер писал строку в конфиг
   * АГЕНТА, показ уведомления её не читал, а JS считал truthy и "true", и
   * "false" — тумблер всегда выглядел включённым.
   */
  const toggleLocal = async (key: keyof NotifyPrefs) => {
    haptic();
    const next = !prefs[key];
    if (next && !allowed) {
      const granted = await requestNotificationPermission();
      setAllowed(granted);
      if (!granted) {
        toastError(t("settings.notificationsDenied"));
        return;
      }
    }
    setPrefsState(setNotifyPrefs({ [key]: next }));
  };

  const toggleTelegram = async (key: "agent_waiting" | "support_reply") => {
    if (!tgPrefs || tgBusy) return;
    haptic();
    const next = !tgPrefs[key];
    setTgBusy(true);
    try {
      // Тело частичное: релей не трогает не переданный флаг, поэтому два
      // тумблера не затирают друг друга.
      const updated = await setCloudNotifyPrefs(
        key === "agent_waiting" ? { agent_waiting: next } : { support_reply: next },
      );
      setTgPrefs(updated);
      hapticSuccess();
      toastSuccess(t("settings.notifyTgSaved"));
    } catch (e) {
      toastError(mapApiError(e));
    } finally {
      setTgBusy(false);
    }
  };

  /**
   * «Подключить Telegram» — тот же поток, что «Сменить Telegram-аккаунт» в
   * «Способах входа»: релей сливает текущий аккаунт с Telegram-аккаунтом, и
   * боту наконец есть куда писать. После успеха перечитываем настройки — на
   * месте абзаца-тупика появляются оба тумблера, и человек видит результат
   * своего нажатия, а не ещё одно объяснение.
   */
  const connectTelegram = async () => {
    haptic();
    // Внутри мини-аппа привязывать нечего: аккаунт там и есть тот, под кем
    // человек вошёл в Telegram, — сменить его можно только в самом Telegram.
    if (getTelegram()?.initData) {
      toastSuccess(t("settings.notifyTgInTelegram"));
      return;
    }
    setTgLinkBusy(true);
    try {
      const handle = await startTelegramLogin();
      const result = await handle.done;
      if (result.ok) {
        hapticSuccess();
        toastSuccess(t("settings.notifyTgLinked"));
        // JWT после слияния аккаунтов уже другой — сокет обязан переподключиться
        // с новым, иначе экран продолжит жить со старым аккаунтом.
        reconnectWS();
        loadTgPrefs();
      } else if (result.reason === "expired" || result.reason === "timeout") {
        toastError(t("settings.notifyTgLinkTimeout"));
      }
    } catch (e) {
      toastError(mapApiError(e));
    } finally {
      setTgLinkBusy(false);
    }
  };

  /**
   * Есть ли под первым абзацем настоящие переключатели. Абзац обещал «приходят
   * в Telegram — переключателями ниже», а ниже без аккаунта или без привязки
   * шёл второй абзац: обещание не выполнялось ровно в том случае, когда человек
   * и приходил его читать (§6.4).
   */
  const tgTogglesShown = !!tgPrefs?.telegram_linked && !tgError;

  const telegramRows: { key: "agent_waiting" | "support_reply"; label: string; available: boolean }[] = tgPrefs
    ? [
        { key: "agent_waiting", label: t("settings.notifyTgWaiting"), available: tgPrefs.agent_waiting_available },
        { key: "support_reply", label: t("settings.notifyTgSupport"), available: tgPrefs.support_reply_available },
      ]
    : [];

  return (
    <div className="setting-group">
      <h2 className="setting-label">{t("settings.notifications")}</h2>
      {isNativeApp ? (
        <>
          <div className="settings-session-hint">{t("settings.notifyPushTitle")}</div>
          {[
            { key: "waiting" as const, label: t("settings.notifyWaiting") },
            { key: "done" as const, label: t("settings.notifyDone") },
          ].map((row) => (
            <div
              key={row.key}
              className="settings-toggle-row"
              role="switch"
              tabIndex={0}
              aria-checked={prefs[row.key]}
              aria-label={row.label}
              onClick={() => void toggleLocal(row.key)}
              onKeyDown={(e) => {
                if (e.key === "Enter" || e.key === " ") { e.preventDefault(); void toggleLocal(row.key); }
              }}
            >
              <span>{row.label}</span>
              <span className={`settings-toggle ${prefs[row.key] ? "on" : ""}`}>
                <span className="settings-toggle-knob" />
              </span>
            </div>
          ))}
          {questionsDetected === false && (
            <div style={{ fontSize: 11, color: "var(--tg-hint)", lineHeight: 1.45, padding: "2px 4px 6px" }}>
              {t("settings.notifyWaitingNeedsDetect")}
            </div>
          )}
          {allowed === false && (
            <div className="settings-info-card" style={{ marginTop: 8 }}>
              <div style={{ fontSize: 12, color: "var(--tg-hint)", lineHeight: 1.5, padding: "4px 0" }}>
                {t("settings.notifyBlocked")}
              </div>
              <div className="settings-actions-row">
                <button
                  className="btn btn-secondary btn-sm"
                  onClick={async () => {
                    haptic();
                    setAllowed(await requestNotificationPermission());
                  }}
                >
                  {t("settings.notifyAllow")}
                </button>
              </div>
            </div>
          )}
        </>
      ) : (
        <div className="settings-info-card">
          <div style={{ fontSize: 12, color: "var(--tg-hint)", lineHeight: 1.5, padding: "6px 0" }}>
            {tgTogglesShown ? t("settings.notifyNativeOnly") : t("settings.notifyNativeOnlyNoTg")}
          </div>
        </div>
      )}

      {/* h3, а не h2: это подзаголовок ВНУТРИ «Уведомлений», и равный уровень
          развалил бы оглавление экрана на две одинаковые секции. */}
      <h3 className="setting-label" style={{ marginTop: 16 }}>{t("settings.notifyTgTitle")}</h3>
      {!cloudAccount ? (
        <div className="settings-info-card">
          <div style={{ fontSize: 12, color: "var(--tg-hint)", lineHeight: 1.5, padding: "6px 0" }}>
            {t("settings.notifyTgNeedAccount")}
          </div>
          {/* Дверь к результату вместо тупика: сообщения бота — свойство
              облачного аккаунта, поэтому единственное осмысленное действие
              здесь — вход в него (§6.4). */}
          <div className="settings-actions-row">
            <button className="btn btn-primary btn-sm" onClick={() => { haptic(); navigate("/cloud-login"); }}>
              {t("settings.notifySignIn")}
            </button>
          </div>
        </div>
      ) : tgError ? (
        <div className="settings-info-card">
          <div style={{ fontSize: 12, color: "var(--tg-hint)", lineHeight: 1.5, padding: "6px 0" }}>
            {tgError}
          </div>
          <div className="settings-actions-row">
            <button className="btn btn-secondary btn-sm" onClick={() => { haptic(); loadTgPrefs(); }}>
              {"↻"} {t("settings.notifyTgRetry")}
            </button>
          </div>
        </div>
      ) : !tgPrefs ? (
        <div className="settings-info-card">
          <div style={{ fontSize: 12, color: "var(--tg-hint)", lineHeight: 1.5, padding: "6px 0" }}>
            {t("settings.notifyTgLoading")}
          </div>
        </div>
      ) : !tgPrefs.telegram_linked ? (
        <div className="settings-info-card">
          <div style={{ fontSize: 12, color: "var(--tg-hint)", lineHeight: 1.5, padding: "6px 0" }}>
            {t("settings.notifyTgNeedTelegram")}
          </div>
          <div className="settings-actions-row">
            <button className="btn btn-primary btn-sm" disabled={tgLinkBusy} onClick={() => void connectTelegram()}>
              {tgLinkBusy ? t("settings.notifyTgConnecting") : t("settings.notifyConnectTelegram")}
            </button>
          </div>
        </div>
      ) : (
        <>
          {telegramRows.map((row) => (
            <div
              key={row.key}
              className="settings-toggle-row"
              role="switch"
              tabIndex={row.available ? 0 : -1}
              aria-checked={tgPrefs[row.key]}
              aria-disabled={!row.available || tgBusy}
              aria-label={row.label}
              onClick={() => { if (row.available) void toggleTelegram(row.key); }}
              onKeyDown={(e) => {
                if (!row.available) return;
                if (e.key === "Enter" || e.key === " ") { e.preventDefault(); void toggleTelegram(row.key); }
              }}
              style={row.available ? undefined : { opacity: 0.5 }}
            >
              <span>{row.label}</span>
              <span className={`settings-toggle ${tgPrefs[row.key] ? "on" : ""}`}>
                <span className="settings-toggle-knob" />
              </span>
            </div>
          ))}
          <div style={{ fontSize: 11, color: "var(--tg-hint)", padding: "8px 4px 0", lineHeight: 1.5 }}>
            {telegramRows.every((row) => row.available)
              ? t("settings.notifyTgHint")
              : t("settings.notifyTgUnavailable")}
          </div>
        </>
      )}
    </div>
  );
}

/**
 * Помощь и приватность: чат с поддержкой, бот, политика, «Что мы собираем» и
 * тумблер статистики. Ничего из этого не зависит от компьютера — секция общая
 * для обеих веток экрана (N124).
 *
 * Чат гейтится наличием облачного АККАУНТА, а не облачного маршрута: раньше
 * переключение на локальную сеть уносило и строку чата, и бейдж непрочитанного,
 * хотя переписка лежала на релее, а JWT никуда не девался (N160).
 */
function HelpSection({ unread }: { unread: number }) {
  const navigate = useNavigate();
  const { toastSuccess } = useToast();
  const [collectOpen, setCollectOpen] = useState(false);
  // Второй путь вернуть чеклист новичка: тост с кнопкой «Вернуть» живёт восемь
  // секунд, а передумать человек может и через неделю. Строку показываем
  // только когда чеклист действительно скрыт — иначе в настройках висел бы
  // мёртвый пункт.
  const [stepsHidden, setStepsHidden] = useState(isFirstStepsDismissed);
  const [analyticsOn, setAnalyticsOn] = useState<boolean>(isAnalyticsEnabled);
  const chatAvailable = supportChatAvailable();

  // Побочки (запись и тост) — снаружи updater'а: в StrictMode он вызывается
  // дважды и дал бы двойной тост.
  const toggleAnalytics = () => {
    haptic();
    const next = !analyticsOn;
    setAnalyticsOn(next);
    setAnalyticsEnabled(next);
    toastSuccess(next ? t("settings.help.analyticsOn") : t("settings.help.analyticsOff"));
  };

  return (
    <div className="setting-group">
      <h2 className="setting-label">{t("settings.help.title")}</h2>
      <div className="settings-info-card">
        {stepsHidden && (
          <button
            type="button"
            className="settings-info-row"
            style={INFO_ROW_BUTTON}
            onClick={() => {
              haptic();
              restoreFirstSteps();
              setStepsHidden(false);
              toastSuccess(t("home.steps.restored"));
            }}
          >
            <span className="settings-info-label">{t("settings.help.showFirstSteps")}</span>
            <span className="settings-info-value">›</span>
          </button>
        )}

        <button
          type="button"
          className="settings-info-row"
          style={INFO_ROW_BUTTON}
          onClick={() => { haptic(); navigate(chatAvailable ? "/support" : "/cloud-login"); }}
        >
          <span className="settings-info-label">
            {chatAvailable ? t("settings.help.supportChat") : t("settings.help.supportNeedAccount")}
          </span>
          <span style={{ display: "inline-flex", alignItems: "center", gap: 8 }}>
            {chatAvailable && unread > 0 && <span className="settings-badge">{unread}</span>}
            <span className="settings-info-value">›</span>
          </span>
        </button>
        {/* `?start=support` — бот сразу объясняет, что обращение принимается
            командой `/support текст`. Без этого человек писал в чат «помогите,
            не подключается», обращением это не считалось, и он ждал ответа
            там, куда обращение не попало (N17). Подпись говорит, что очередь у
            обеих строк одна. */}
        <a
          className="settings-info-row"
          href={SUPPORT_URL.includes("?") ? SUPPORT_URL : `${SUPPORT_URL}?start=support`}
          target="_blank"
          rel="noopener"
          style={{ textDecoration: "none" }}
        >
          <span className="settings-info-label">
            {t("settings.help.support")}
            <small className="settings-row-sub">{t("settings.help.supportSameQueue")}</small>
          </span>
          <span className="settings-info-value" style={{ color: "var(--tg-link)" }}>t.me ↗</span>
        </a>
        {/* Политика конфиденциальности — каноничная страница remotai.ru/privacy.
            Открываем как соседнюю ссылку на поддержку: target=_blank, в
            Telegram Mini App это встроенный браузер. */}
        <a
          className="settings-info-row"
          href={getLanguage() === "en" && RELAY_BASE === "https://remotai.ru" ? RELAY_BASE + "/en/privacy.html" : PRIVACY_URL}
          target="_blank"
          rel="noopener"
          style={{ textDecoration: "none" }}
        >
          <span className="settings-info-label">{t("settings.help.privacyLink")}</span>
          <span className="settings-info-value" style={{ color: "var(--tg-link)" }}>↗</span>
        </a>
        <div className="settings-info-row">
          <span className="settings-info-label">{t("settings.language")}</span>
          <LanguageSelector className="settings-info-value" />
        </div>
        {/* «Что мы собираем» — короткий разворот прямо в приложении: проверить
            утверждение можно, не уходя на внешнюю страницу. */}
        <button
          type="button"
          className="settings-info-row"
          aria-expanded={collectOpen}
          style={INFO_ROW_BUTTON}
          onClick={() => { haptic(); setCollectOpen((v) => !v); }}
        >
          <span className="settings-info-label">{t("settings.help.collect")}</span>
          <span className="settings-info-value">{collectOpen ? "▾" : "›"}</span>
        </button>
      </div>
      {collectOpen && (
        <div className="settings-advanced-hint">
          <div>{t("settings.help.collectYes")}</div>
          <div style={{ marginTop: 6 }}>{t("settings.help.collectNo")}</div>
          <div style={{ marginTop: 6 }}>{t("settings.help.collectServer")}</div>
        </div>
      )}
      {/* Тумблер согласия: одна проверка в sendEvent гасит все вехи. */}
      <div
        className="settings-toggle-row"
        style={{ marginTop: 8 }}
        role="switch"
        tabIndex={0}
        aria-checked={analyticsOn}
        aria-label={t("settings.help.analytics")}
        onClick={toggleAnalytics}
        onKeyDown={(e) => {
          if (e.key === "Enter" || e.key === " ") { e.preventDefault(); toggleAnalytics(); }
        }}
      >
        <span>{t("settings.help.analytics")}</span>
        <span className={`settings-toggle ${analyticsOn ? "on" : ""}`}>
          <span className="settings-toggle-knob" />
        </span>
      </div>
      <div style={{ fontSize: 11, color: "var(--tg-hint)", padding: "8px 4px 0", lineHeight: 1.5 }}>
        {t("settings.help.analyticsHint")}
      </div>
    </div>
  );
}
import { getLanguage } from "@tgcontrol/shared";
