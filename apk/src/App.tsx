import { lazy, Suspense, useCallback, useEffect, useRef, useState, type ComponentType } from "react";
import { Routes, Route, Navigate, useNavigate, useLocation, useParams } from "react-router-dom";
import { App as CapApp } from "@capacitor/app";
import { APP_NAME, ConnectionBanner, ErrorBoundary, setAgentRegistry, useLanguage } from "@tgcontrol/shared";
import { useToast } from "@tgcontrol/shared";
import { hasServerConfig, getMode, getSelectedDeviceId, getSelectedDeviceName, isNativeApp, isOnPCPanel, restoreLocalControl, saveConfig } from "./config";
import { resetCapabilities } from "./capabilities";
import { humanDeviceName } from "./devices";
import { onWSEvent, wakeWS, reconnectWS, disconnectWS, onConnectionChange, getAgents } from "./api";
import { hasBackHandlers, onBackHandlersChange, runBackHandlers } from "./hooks/backHandler";
import { goBack, isRootPath } from "./navBack";
import { getTelegram } from "./telegram";
import {
  initNotifications, onNotificationTap, showNotifyEvent, showPtyEvent,
  maybeRequestNotificationPermission,
} from "./notifications";
import { getFeatures } from "./features";
import { BottomNav, type NavTab } from "./components/BottomNav";
import { MoreMenuProvider } from "./components/MoreMenuState";
import { useFeatures } from "./hooks/useFeatures";
import { parsePairPayload, runPair } from "./cloud/pair";
import { isAnalyticsEnabled, trackAppOpen, trackRegisterSource } from "./cloud/support";
import { initMetrikaVisit } from "./cloud/metrika";
import { tlog } from "./debuglog";
import { t } from "./i18n";
import { sectionName } from "./sections";
import { loginNextPath } from "./nextPath";
import { useCapabilities } from "./hooks/useCapabilities";

/**
 * lazy() с одной автоматической пересдачей. Чанк экрана не доехал по двум
 * бытовым причинам: моргнула сеть в лифте или мы задеплоили новую версию и
 * старое имя файла с хэшем исчезло. Обе лечатся повтором, а не экраном
 * «этот экран дал сбой» — до этой правки любое такое моргание выглядело как
 * поломка приложения. Если и повтор не удался, ошибка доходит до
 * RouteBoundary, где теперь есть кнопка «Повторить».
 */
function lazyRoute<T extends ComponentType<any>>(load: () => Promise<{ default: T }>) {
  return lazy(() => load().catch(() => new Promise<{ default: T }>((resolve, reject) => {
    // Пауза перед второй попыткой: мгновенный ретрай попадает в ту же
    // секунду обрыва и падает так же.
    setTimeout(() => { load().then(resolve, reject); }, 600);
  })));
}

const Dashboard = lazyRoute(() => import("./pages/Dashboard").then((m) => ({ default: m.Dashboard })));
const SessionsDashboard = lazyRoute(() => import("./pages/Dashboard").then((m) => ({ default: m.SessionsDashboard })));
const SessionView = lazyRoute(() => import("./pages/SessionView").then((m) => ({ default: m.SessionView })));
const FilesView = lazyRoute(() => import("./pages/FilesView").then((m) => ({ default: m.FilesView })));
const SystemView = lazyRoute(() => import("./pages/SystemView").then((m) => ({ default: m.SystemView })));
const SettingsView = lazyRoute(() => import("./pages/SettingsView").then((m) => ({ default: m.SettingsView })));
const AccountView = lazyRoute(() => import("./pages/AccountView").then((m) => ({ default: m.AccountView })));
const RemoteView = lazyRoute(() => import("./pages/RemoteView").then((m) => ({ default: m.RemoteView })));
const PtyListView = lazyRoute(() => import("./pages/PtyListView").then((m) => ({ default: m.PtyListView })));
const PtyTermView = lazyRoute(() => import("./pages/PtyTermView").then((m) => ({ default: m.PtyTermView })));
const SshFilesView = lazyRoute(() => import("./pages/SshFilesView").then((m) => ({ default: m.SshFilesView })));
const SshView = lazyRoute(() => import("./pages/SshView").then((m) => ({ default: m.SshView })));
const ResearchView = lazyRoute(() => import("./pages/ResearchView").then((m) => ({ default: m.ResearchView })));
const LoginView = lazyRoute(() => import("./pages/LoginView").then((m) => ({ default: m.LoginView })));
const CloudLoginView = lazyRoute(() => import("./pages/CloudLoginView").then((m) => ({ default: m.CloudLoginView })));
const StartView = lazyRoute(() => import("./pages/StartView").then((m) => ({ default: m.StartView })));
const ScanView = lazyRoute(() => import("./pages/ScanView").then((m) => ({ default: m.ScanView })));
const DeviceList = lazyRoute(() => import("./pages/DeviceList").then((m) => ({ default: m.DeviceList })));
const PanelView = lazyRoute(() => import("./pages/PanelView").then((m) => ({ default: m.PanelView })));
const SupportView = lazyRoute(() => import("./pages/SupportView").then((m) => ({ default: m.SupportView })));
const GuideView = lazyRoute(() => import("./pages/GuideView").then((m) => ({ default: m.GuideView })));
const AgentsView = lazyRoute(() => import("./pages/AgentsView").then((m) => ({ default: m.AgentsView })));
const HermesView = lazyRoute(() => import("./pages/HermesView").then((m) => ({ default: m.HermesView })));
const AgentSessionsView = lazyRoute(() => import("./pages/AgentSessionsView").then((m) => ({ default: m.AgentSessionsView })));

/**
 * React Router may reuse the same route element for `/pty/a` → `/pty/b`.
 * A terminal runtime owns sockets, timers, xterm parser state and resume epoch;
 * keying by id makes that boundary physical instead of asking thousands of
 * mutable refs to reset perfectly during one render.
 */
function PtyTermRoute() {
  const { id = "" } = useParams<{ id: string }>();
  const [viewGeneration, setViewGeneration] = useState(0);
  return <PtyTermView key={`${id}:${viewGeneration}`} onReopen={() => setViewGeneration(value => value + 1)} />;
}

/**
 * Обратный гард входа: вошедшему экран входа не показывается.
 *
 * Аудит путей 29.08.2026: вход через Telegram и OAuth завершается `reload`
 * (main.tsx), а HashRouter держит в адресе `#/cloud-login` — человек с уже
 * сохранённым JWT видел ту же кнопку «Войти» и считал, что вход не удался.
 *
 * Условие узкое НАМЕРЕННО — `mode === "cloud"` плюс действующая конфигурация.
 * На этот же экран законно приходят люди БЕЗ облачного входа: «Войти в
 * аккаунт» с главной, уведомления бота в настройках, переключение режима
 * связи. У них режим `self_hosted`, и гард их не трогает.
 */
function CloudLoginRoute() {
  const location = useLocation();
  if (getMode() === "cloud" && hasServerConfig()) return <Navigate to={loginNextPath(location.state, location.search)} replace />;
  return <CloudLoginView />;
}

function RequireAuth({ children }: { children: React.ReactNode }) {
  const location = useLocation();
  // Первый экран — всегда дружелюбный облачный вход (одна кнопка «Войти через
  // Telegram»). LAN-вход не пропадает: он доступен ссылкой «Компьютер рядом?»
  // прямо в CloudLoginView. Раньше нативный APK кидало на технический /login
  // (адрес+токен), что путало новых пользователей.
  //
  // Цель передаём в state: человек, пришедший по ссылке в чат поддержки или в
  // кабинет без входа, после входа попадал на главную, а не туда, куда шёл
  // (аудит ИА 02.09.2026, P1-13). CloudLoginView пропускает цель через
  // safeNextPath — чужой адрес туда не доедет.
  if (!hasServerConfig()) {
    const next = location.pathname + location.search;
    return <Navigate to={next !== "/" ? `/cloud-login?next=${encodeURIComponent(next)}` : "/cloud-login"} replace state={next !== "/" ? { next } : undefined} />;
  }
  return <>{children}</>;
}

/**
 * «Панель ПК» — фрейм loopback-страницы `/setup`; вне окна на ПК ему нечего
 * показать. Дверь в шторке «Ещё» и так стоит под `isOnPCPanel()`, но адрес
 * открывался откуда угодно (гид, старая ссылка) и рисовал пустой фрейм без
 * шапки и «←» (аудит ИА 02.09.2026, P0-5). С телефона ведём в настройки.
 */
function PanelRoute() {
  if (!isOnPCPanel()) return <Navigate to="/settings" replace />;
  return <PanelView />;
}

/** Block AI-only routes when the feature flag is off — redirects to home. */
function RequireAI({ children }: { children: React.ReactNode }) {
  const { ai } = useFeatures();
  if (!ai) return <Navigate to="/" replace />;
  return <>{children}</>;
}

/** Isolates route crashes so one broken screen doesn't white-screen the app. */
function RouteBoundary({ children }: { children: React.ReactNode }) {
  const location = useLocation();
  const navigate = useNavigate();
  return (
    <ErrorBoundary
      resetKey={location.pathname}
      fallback={(reset) => (
        <div className="route-error">
          {/* Сюда попадают не только настоящие падения экрана, но и не
              доехавший чанк (моргнула сеть, вышел новый релиз) — это самый
              частый случай. Прежнее «Что-то пошло не так — этот экран дал
              сбой» описывало поломку, которой не было, и звало уйти на
              главную. Теперь первое действие — «Повторить»: reset()
              перерисовывает поддерево, React заново запрашивает модуль ЭТОГО
              экрана, и человек остаётся на своём месте работы. */}
          <div className="route-error-emoji">{"\u{1F635}"}</div>
          <div className="route-error-title">{t("error.routeNotLoaded")}</div>
          <div className="route-error-desc">{t("error.routeRetry")}</div>
          <div className="route-error-actions">
            <button className="btn btn-primary" onClick={reset}>
              {t("conn.retry")}
            </button>
            <button className="btn btn-secondary" onClick={() => { reset(); navigate("/"); }}>
              {t("error.goHome")}
            </button>
          </div>
        </div>
      )}
    >
      {children}
    </ErrorBoundary>
  );
}

function NotificationBridge() {
  const navigate = useNavigate();

  useEffect(() => {
    if (!hasServerConfig()) return;

    let cancelled = false;
    initNotifications().then(() => {
      if (cancelled) return;
      onNotificationTap(
        // Раздел «Сессии» закрыт фича-флагом ai (по умолчанию выключен и
        // включается секретным пятикратным тапом по версии), поэтому тап по
        // уведомлению о сессии молча приводил на главную — без объяснений и
        // без единой двери к этой сессии. Сами такие уведомления при
        // выключенном флаге больше не показываются (см. showNotifyEvent), но
        // одно могло висеть в шторке ещё со времён включённого флага —
        // уводим его на терминалы, где работа агентов реально видна.
        (sessionName) => navigate(
          getFeatures().ai ? `/session/${encodeURIComponent(sessionName)}` : "/pty",
        ),
        (ptyId) => navigate(`/pty/${encodeURIComponent(ptyId)}`),
        // Уведомления с фиксированным экраном (ответ поддержки → /support).
        // Путь приходит из данных уведомления, поэтому список разрешённых
        // маршрутов держит сам notifications.ts — сюда доезжает уже проверенный.
        (path) => navigate(path),
      );
    });

    // Show OS notification for backend "notify" and "pty_event" broadcasts.
    //
    // Разрешение ОС спрашиваем ЗДЕСЬ, а не при старте: на Android 13+ оно по
    // умолчанию не выдано, и до этой правки его не просили нигде, кроме
    // перехода «выкл → вкл» тумблера в настройках. Человек ставил APK ровно
    // ради звонка «Claude спросил», видел два зелёных тумблера и не получал
    // ни одного уведомления. Момент первого события — единственный честный
    // повод показать системный диалог: он в контексте («сейчас прилетит вот
    // это») и случается один раз за установку (латч внутри функции).
    const offWS = onWSEvent((ev: any) => {
      if (ev?.type === "notify") {
        void maybeRequestNotificationPermission().then(() => showNotifyEvent(ev));
      } else if (ev?.type === "pty_event") {
        void maybeRequestNotificationPermission().then(() => showPtyEvent(ev));
      }
    });

    // Возврат приложения из фона — МЯГКОЕ пробуждение, а не жёсткий реконнект.
    // reconnectWS() рвал и живой сокет тоже: на релее это лишний цикл
    // detach/attach присутствия — ровно то окно, в которое сторож «агент ждёт
    // ответа» решает, что человека рядом нет, и шлёт лишнее сообщение в
    // Telegram. wakeWS() пересоздаёт соединение только если оно мертво или не
    // доказало пингом, что живо; мёртвое поднимается с тем же lastEventId, так
    // что реплей `?since=` (LAN) не теряется. Жёсткий реконнект остаётся за
    // ручным ретраем баннера и сменой ПК (devices.ts, DeviceList).
    const stateSub = CapApp.addListener("appStateChange", ({ isActive }) => {
      if (isActive) wakeWS();
    });

    return () => {
      cancelled = true;
      offWS();
      stateSub.then((s) => s.remove()).catch(() => {});
    };
  }, [navigate]);

  return null;
}

function AgentRegistryBridge() {
  useEffect(() => {
    if (!hasServerConfig()) return;
    getAgents().then((data) => setAgentRegistry(data.agents || [])).catch(() => {});
  }, []);
  return null;
}

/**
 * Handles `remotai://pair?relay=&code=` deep links — both cold-start (launchUrl)
 * and while running (appUrlOpen). Pairs without the camera, which also makes the
 * whole flow drivable over ADB: am start -a VIEW -d "remotai://pair?...".
 */
function DeepLinkBridge() {
  const navigate = useNavigate();

  useEffect(() => {
    let cancelled = false;

    const handle = async (url?: string | null) => {
      if (!url) return;
      const parsed = parsePairPayload(url);
      if (!parsed) return;
      tlog("deeplink:pair", { url });
      try {
        await runPair(parsed.relay, parsed.code);
        if (!cancelled) navigate("/", { replace: true });
      } catch {
        // Surface the error on the manual-entry screen (it shows toasts there).
        if (!cancelled) navigate("/cloud-login", { replace: true });
      }
    };

    CapApp.getLaunchUrl().then((r) => handle(r?.url)).catch(() => {});
    const sub = CapApp.addListener("appUrlOpen", (ev) => { void handle(ev.url); });

    return () => {
      cancelled = true;
      sub.then((s) => s.remove()).catch(() => {});
    };
  }, [navigate]);

  return null;
}

/**
 * Single Android Back handler for the whole app. Priority:
 *  1. open modals/sheets (registered via useEscape/useBackHandler) close first;
 *  2. sub-screens go back the same way their own «←» does (см. navBack.ts:
 *     шаг по истории, а если её нет — логический родитель);
 *  3. at the root the app is minimized — system Back must never feel like a crash.
 * Pages don't register their own Capacitor listeners (multiple listeners all
 * fire at once and used to double-navigate).
 *
 * Правило возврата живёт в одном месте (navBack.ts) намеренно: пока оно было
 * продублировано здесь, системная кнопка и экранная «←» на одном и том же
 * экране уводили в РАЗНЫЕ места — «Назад» переставал быть предсказуемым.
 */
function BackButtonBridge() {
  const navigate = useNavigate();
  const location = useLocation();
  // Держим Location целиком: решение о возврате смотрит и на путь, и на
  // строку запроса (?ssh=1, ?from=…), и на state.from, и на ключ записи
  // истории.
  const locationRef = useRef(location);
  locationRef.current = location;

  const handleBack = useCallback(() => {
    if (runBackHandlers()) return;
    const loc = locationRef.current;
    // Корень зависит от состояния (выбран ли ПК, есть ли конфиг) — правило
    // живёт в navBack.ts рядом с остальной логикой «Назад».
    if (isRootPath(loc.pathname)) {
      const tg = getTelegram();
      if (tg?.initData) tg.close();
      else CapApp.minimizeApp().catch(() => {});
      return;
    }
    goBack(navigate, loc);
  }, [navigate]);

  useEffect(() => {
    const sub = CapApp.addListener("backButton", handleBack);
    return () => { sub.then((s) => s.remove()).catch(() => {}); };
  }, [handleBack]);

  useEffect(() => {
    const tg = getTelegram();
    if (!tg?.initData) return;
    const root = isRootPath(location.pathname);
    const sync = () => {
      if (!root || hasBackHandlers()) tg.BackButton.show();
      else tg.BackButton.hide();
    };
    tg.BackButton.onClick(handleBack);
    const offChange = onBackHandlersChange(sync);
    sync();
    return () => {
      offChange();
      tg.BackButton.offClick(handleBack);
    };
  }, [handleBack, location.pathname]);

  useEffect(() => {
    const tg = getTelegram();
    if (!tg?.initData) return;
    const sensitive = /^\/pty\/./.test(location.pathname) || location.pathname === "/remote";
    if (sensitive) tg.enableClosingConfirmation?.();
    else tg.disableClosingConfirmation?.();
    return () => {
      if (sensitive) tg.disableClosingConfirmation?.();
    };
  }, [location.pathname]);

  return null;
}

/**
 * When the relay reports our JWT no longer owns the device (re-paired from
 * another client / expired), route the user to the pairing screen with a clear
 * message instead of leaving every screen erroring out.
 */
function CloudAuthGuard() {
  const navigate = useNavigate();
  const { toastError } = useToast();

  useEffect(() => {
    const onLost = () => {
      if (getMode() !== "cloud") return;
      // Останавливаем events-WS: его реконнекты со сброшенным JWT бессмысленны
      // и рисуют баннер «Переподключение…» поверх экрана пайринга.
      disconnectWS();
      // Человек сидит ЗА ЭТИМ компьютером (окно на ПК, loopback + локальный
      // токен) — облачный отказ не должен отбирать у него машину, которая
      // стоит перед ним. Возвращаем локальное управление и остаёмся на месте.
      //
      // Живая жалоба: «открывается новое окно и я теряю ДАЖЕ локальное
      // управление… какая разница, я же локально всё равно управляю».
      if (restoreLocalControl()) {
        toastError(t("cloud.authLostLocal"));
        resetCapabilities();
        reconnectWS();
        navigate("/", { replace: true });
        return;
      }
      toastError(t("cloud.authLost"));
      // Гасим недействительный JWT ДО перехода: иначе обратный гард
      // (CloudLoginRoute) увидит «человек вошёл» и вернёт его с экрана входа
      // обратно на главную — войти заново стало бы нечем.
      try { saveConfig({ jwt: "", jwtExpiresAt: 0 }); } catch { /* localStorage недоступен */ }
      navigate("/cloud-login", { replace: true });
    };
    window.addEventListener("tgc:cloud-auth-lost", onLost);
    return () => window.removeEventListener("tgc:cloud-auth-lost", onLost);
  }, [navigate, toastError]);

  return null;
}

/**
 * Экраны, которые в облачном режиме открываются и без выбранного ПК: сам выбор
 * устройства, аккаунтные разделы и вход.
 */
const DEVICE_GUARD_FREE = new Set([
  // Гид — статичная страница: ПК ей не нужен, как и поддержке/настройкам.
  // `/agents` рядом с `/usage`: раздел переименовали, а в список попал только
  // старый адрес — и новый пункт навигации в облаке без выбранной машины
  // отскакивал в список машин вместо того, чтобы открыться.
  // `/account` и `/plan` — кабинет и деньги: без них «Личный кабинет»,
  // «Подписка» и возврат ЮKassa после оплаты в облаке без выбранного ПК
  // отскакивали в список машин, а цель терялась (аудит ИА 02.09.2026, P0-3).
  "/devices", "/infrastructure", "/settings", "/usage", "/agents", "/support", "/guide", "/login", "/cloud-login", "/scan", "/start",
  "/account", "/plan",
]);

/**
 * Мульти-устройство: в облачном режиме без выбранного ПК отправляем на список
 * устройств. Срабатывает для Telegram-входа (нет selectedDeviceId) и когда
 * пользователь хочет сменить ПК. LAN/self_hosted и натив-cloud с выбранным
 * устройством не затрагиваются.
 */
function DeviceGuard() {
  const navigate = useNavigate();
  const location = useLocation();
  useEffect(() => {
    if (getMode() !== "cloud") return;
    if (getSelectedDeviceId()) return;
    const p = location.pathname;
    if (DEVICE_GUARD_FREE.has(p)) return;
    // Цель кладём прямо в адрес экрана выбора (`/infrastructure?next=…`):
    // InfrastructureView доводит до неё сразу после выбора ПК, включая
    // АВТОвыбор единственного компьютера. Раньше цель жила в модульной
    // переменной без срока годности: автовыбор её не забирал, и она
    // срабатывала позже — при случайном тапе по карточке компьютера человека
    // уносило в старый терминал.
    const want = p + location.search;
    navigate(want === "/" ? "/infrastructure" : `/infrastructure?next=${encodeURIComponent(want)}`, { replace: true });
  }, [navigate, location.pathname, location.search]);
  return null;
}

/**
 * Совместимость со старым адресом `/devices`: по нему приходят все кнопки бота
 * и start_param (`?select=<ПК>&next=<экран>`). Строковый `to` у <Navigate>
 * отбрасывает строку запроса (resolveTo подставляет пустые search/hash),
 * поэтому диплинки теряли и выбор ПК, и цель — любая кнопка бота приводила в
 * список инфраструктуры. Переносим search и hash руками.
 */
function DevicesAlias() {
  const location = useLocation();
  return <Navigate to={{ pathname: "/infrastructure", search: location.search, hash: location.hash }} replace />;
}

/**
 * `/usage` («Подписки и лимиты») стал разделом «Агенты»: лимиты подписок — это
 * половина разговора про агентов, вторая половина (аккаунты, что установлено,
 * как запускать) жила в двух других местах.
 *
 * Адрес оставлен алиасом: на него ведут кнопки бота, гид и старые ссылки в
 * переписке. Search и hash переносим руками — по той же причине, что у
 * `/devices`: строковый `to` их отбрасывает, и диплинк приходил бы в пустоту.
 */
function UsageAlias() {
  const location = useLocation();
  return <Navigate to={{ pathname: "/agents", search: location.search, hash: location.hash }} replace />;
}

/**
 * Какую вкладку подсветить, пока чанк экрана ещё едет. Список повторяет
 * `<BottomNav active=…/>` самих экранов; `null` — экраны без нижней панели
 * (терминал, вход, сканер), там навигацию рисовать нечего: нарисуешь — и она
 * мигнёт, а через полсекунды исчезнет.
 *
 * Возвращаем именно РАЗДЕЛ, а не вкладку телефона. Разделы, у которых на
 * телефоне своей вкладки нет («Мои компьютеры», «Система», «SSH-серверы»,
 * «Панель ПК»), BottomNav сам сводит к пятой вкладке «Ещё» — здесь это знание
 * не дублируем, иначе подсветка скелета и подсветка готового экрана начнут
 * расходиться.
 */
function navTabForPath(path: string): NavTab | null {
  if (path === "/") return "home";
  if (path === "/infrastructure" || path === "/devices") return "devices";
  if (path === "/files") return "files";
  if (path === "/pty" || path === "/terminal") return "terminal";
  if (path === "/system") return "system";
  // Файлы SSH-сервера и открытый хост (`/ssh/:hostId`) — тот же раздел
  // «SSH-серверы»: оба экрана рисуют BottomNav active="ssh", и пока грузится
  // их чанк подсветка не должна прыгать с одной вкладки на другую.
  if (path === "/ssh" || path === "/ssh-files" || path.startsWith("/ssh/")) return "ssh";
  if (path === "/panel") return "panel";
  // Эти четыре экрана рисовали себя без навигации, поэтому и в скелете её не
  // было. Теперь панель есть у них самих — скелет обязан показывать ту же,
  // иначе она мигнёт при загрузке чанка (UX-аудит 2026-08-23, п. 2.1).
  if (path === "/hermes") return "hermes";
  if (path === "/agents" || path === "/usage" || path === "/agents/sessions") return "usage";
  if (path === "/settings") return "settings";
  if (path === "/guide") return "guide";
  if (path === "/support") return "support";
  // Кабинет, «Экран компьютера» и старый режим чата рисуют панель сами, а в
  // скелете её не было — панель мигала на каждом первом заходе (аудит ИА
  // 02.09.2026, P1-3).
  if (path === "/account" || path === "/plan") return "plan";
  if (path === "/remote") return "remote";
  if (path === "/sessions") return "sessions";
  return null;
}

/**
 * Имя раздела для заголовка вкладки. Во ВСЕХ вкладках браузера стояло одно
 * слово «Remotai» из index.html: человек, у которого открыты терминал, файлы и
 * настройки, различал их только значком, а история браузера и переключатель
 * задач превращались в список одинаковых строк.
 *
 * Имена берём там же, где их берут сами экраны, — sections.ts и словарь: своя
 * табличка литералов разъехалась бы с заголовками экранов на первой же правке.
 * Пустая строка означает «имени нет» — тогда в заголовке остаётся имя продукта.
 */
function routeTitle(path: string): string {
  if (path === "/hermes") return "Hermes";
  if (path === "/") return t("nav.home");
  if (path === "/sessions" || path.startsWith("/session/")) return t("nav.sessions");
  if (path === "/infrastructure" || path === "/devices") return sectionName("devices");
  if (path === "/agents" || path === "/usage") return sectionName("usage");
  if (path === "/agents/sessions") return t("agentSessions.title");
  if (path === "/files") return t("files.title");
  // Файлы сервера — часть раздела «SSH-серверы» (так подсвечивает панель и так
  // говорит navTabForPath); вкладка «Файлы» при заголовке «user@host» давала
  // третий ответ на «где я» (аудит ИА 02.09.2026, P0-7).
  if (path === "/ssh-files") return t("ssh.files");
  // Кабинет не имел ветки — вкладка оставалась просто «Remotai».
  if (path === "/account" || path === "/plan") return t("account.title");
  if (path === "/pty" || path === "/terminal" || path.startsWith("/pty/")) return t("pty.title");
  if (path === "/ssh" || path.startsWith("/ssh/")) return sectionName("ssh");
  if (path === "/system") return sectionName("system");
  if (path === "/settings") return sectionName("settings");
  if (path === "/panel") return sectionName("panel");
  if (path === "/guide") return sectionName("guide");
  if (path === "/support") return t("settings.help.supportChat");
  if (path === "/remote") return t("nav.remote");
  if (path === "/scan") return t("scan.title");
  // Заголовок экрана входа написан литералом и меняется от причины отказа,
  // поэтому вкладке нужно собственное короткое имя.
  if (path === "/login" || path === "/cloud-login") return t("login.title");
  if (path.startsWith("/research/")) return t("research.title");
  return "";
}

/**
 * Заголовок вкладки следует за маршрутом. Отдельным компонентом, а не эффектом
 * внутри App: подписка на адрес перерисовывает только его, а не всё дерево
 * экранов. Рисует он ничего — работает один побочный эффект.
 */
function DocumentTitle() {
  const location = useLocation();
  useEffect(() => {
    const name = routeTitle(location.pathname);
    document.title = name ? `${name} — ${APP_NAME}` : APP_NAME;
  }, [location.pathname]);
  return null;
}

/**
 * Ожидание чанка экрана. Раньше здесь стояла заглушка на весь вьюпорт со
 * словом «Загрузка…»: на медленной сети (а вкладка терминала тянет ещё и
 * отдельный тяжёлый чанк xterm) человек получал пустой экран без единой
 * кнопки — ни вернуться, ни уйти на другую вкладку, ни понять, что
 * происходит. Приложение выглядело зависшим.
 *
 * Теперь остаются шапка-заготовка и НАСТОЯЩАЯ нижняя навигация: она не
 * lazy-компонент, работает сразу и позволяет уйти куда угодно, не дожидаясь
 * загрузки. Под ними — скелет содержимого вместо надписи.
 */
function RouteFallback() {
  const location = useLocation();
  const tab = navTabForPath(location.pathname);
  return (
    <div className="page route-skeleton">
      <div className="page-header route-skeleton-header">
        <div className="skeleton-line route-skeleton-title" />
      </div>
      <div className="page-content route-skeleton-body" role="status" aria-label={t("generic.loading")}>
        <div className="card skeleton-card" />
        <div className="card skeleton-card" />
        <div className="card skeleton-card" />
      </div>
      {tab && <BottomNav active={tab} />}
    </div>
  );
}

export function App() {
  useLanguage();
  const { platform, hasDisplay } = useCapabilities();
  const connectionEntity = platform === "linux" && !hasDisplay ? "server" : "computer";
  // Аналитика: app_open — один раз за запуск. register_source — разово и для уже
  // авторизованных (Telegram Mini App заходит сюда без экрана логина).
  useEffect(() => {
    // Метрика — до проверки режима: у человека, пришедшего с рекламы на
    // remotai.ru/app/, режим ещё не «cloud» (он только открыл экран входа), а
    // визит с лендинга обязан продолжиться здесь, иначе цель регистрации через
    // полчаса начнёт новый визит без рекламного источника. Публичная проверка
    // 07.09.2026 (2.66.3): на /app/ без аккаунта тег не грузился вовсе.
    // Правило «где Метрике можно быть» — в cloud/metrika.ts: только веб на
    // remotai.ru, не APK/TG, не стенд, уважает выключатель аналитики.
    initMetrikaVisit({
      nativeApp: isNativeApp,
      telegramMiniApp: Boolean(getTelegram()?.initData),
      analyticsEnabled: isAnalyticsEnabled(),
    });
    // Локальный/self-hosted режим не обращается к облаку ради аналитики.
    if (getMode() !== "cloud") return;
    trackAppOpen();
    if (hasServerConfig()) trackRegisterSource();
  }, []);

  return (
    <>
      <ConnectionBanner
        subscribe={onConnectionChange}
        enabled={hasServerConfig}
        onRetry={reconnectWS}
        entity={connectionEntity}
        // Имя машины вычисляем на каждом рендере (функцией): человек
        // переключает машину, не перемонтируя баннер, — иначе полоса называла
        // бы ту, с которой он уже ушёл.
        deviceName={() => humanDeviceName(getSelectedDeviceName(), "")}
      />
      <NotificationBridge />
      <AgentRegistryBridge />
      <DeepLinkBridge />
      <CloudAuthGuard />
      <DeviceGuard />
      <BackButtonBridge />
      <DocumentTitle />
      <MoreMenuProvider>
      <RouteBoundary>
      <Suspense fallback={<RouteFallback />}>
        <Routes>
          <Route path="/login" element={<LoginView />} />
          <Route path="/cloud-login" element={<CloudLoginRoute />} />
          <Route path="/start" element={<StartView />} />
          <Route path="/scan" element={<ScanView />} />
          <Route path="/infrastructure" element={<RequireAuth><DeviceList /></RequireAuth>} />
          <Route path="/agents" element={<RequireAuth><AgentsView /></RequireAuth>} />
          <Route path="/hermes" element={<RequireAuth><HermesView /></RequireAuth>} />
          <Route path="/agents/sessions" element={<RequireAuth><AgentSessionsView /></RequireAuth>} />
          <Route path="/usage" element={<UsageAlias />} />
          <Route path="/devices" element={<DevicesAlias />} />
          <Route path="/" element={<RequireAuth><Dashboard /></RequireAuth>} />
          <Route path="/sessions" element={<RequireAuth><RequireAI><SessionsDashboard /></RequireAI></RequireAuth>} />
          <Route path="/session/:name" element={<RequireAuth><RequireAI><SessionView /></RequireAI></RequireAuth>} />
          <Route path="/files" element={<RequireAuth><FilesView /></RequireAuth>} />
          <Route path="/terminal" element={<Navigate to="/pty" replace />} />
          <Route path="/pty" element={<RequireAuth><PtyListView /></RequireAuth>} />
          <Route path="/pty/:id" element={<RequireAuth><PtyTermRoute /></RequireAuth>} />
          <Route path="/ssh" element={<RequireAuth><SshView /></RequireAuth>} />
          <Route path="/ssh/:hostId" element={<RequireAuth><SshView /></RequireAuth>} />
          <Route path="/ssh-files" element={<RequireAuth><SshFilesView /></RequireAuth>} />
          <Route path="/system" element={<RequireAuth><SystemView /></RequireAuth>} />
          {/* Деньги — в одном месте: сюда ведут настройки, баннер конца пробы,
              карточка тарифа, и сюда же ЮKassa возвращает после оплаты. */}
          {/* Личный кабинет: аккаунт, устройства и деньги в одном месте.
              `/plan` оставлен алиасом — на него возвращает ЮKassa после оплаты
              и ведут прежние ссылки из гида, настроек и баннера пробы. */}
          <Route path="/account" element={<RequireAuth><AccountView /></RequireAuth>} />
          <Route path="/plan" element={<RequireAuth><AccountView /></RequireAuth>} />
          <Route path="/settings" element={<RequireAuth><SettingsView /></RequireAuth>} />
          <Route path="/support" element={<RequireAuth><SupportView /></RequireAuth>} />
          <Route path="/guide" element={<RequireAuth><GuideView /></RequireAuth>} />
          <Route path="/panel" element={<RequireAuth><PanelRoute /></RequireAuth>} />
          <Route path="/remote" element={<RequireAuth><RemoteView /></RequireAuth>} />
          <Route path="/research/:name" element={<RequireAuth><RequireAI><ResearchView /></RequireAI></RequireAuth>} />
          <Route path="*" element={<Navigate to="/" replace />} />
        </Routes>
      </Suspense>
      </RouteBoundary>
      </MoreMenuProvider>
    </>
  );
}
