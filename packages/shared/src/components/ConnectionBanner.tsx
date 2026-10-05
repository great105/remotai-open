import { useState, useEffect, useLayoutEffect, useRef, type CSSProperties } from "react";
import { useLocation, useNavigate } from "react-router-dom";
import { t } from "../i18n";
import { getNetworkRoute, type NetworkRoute } from "../api-core";

// На экранах входа/пейринга и в списке устройств «Переподключение…»
// бессмысленно и перекрывает заголовок. Маршруты существуют только в apk;
// в miniapp их нет — проверка no-op.
// «/infrastructure» — канонический список устройств; «/devices» остаётся
// транзитным алиасом старых ссылок (редирект на «/infrastructure»), но пока он
// в таблице маршрутов, держим его и здесь.
const LOGIN_ROUTES = new Set(["/login", "/cloud-login", "/scan", "/infrastructure", "/devices"]);

// Сколько держать «Переподключение…» до перехода в терминальное состояние.
// Было 30 с: при выключенном ПК человек столько же смотрел на спиннер, ничего
// о причине не узнавая (UX-аудит V3 — замер 30,6 с). За 8 с ладдер
// реконнектов успевает 2–3 попытки: моргнувшая сеть восстановится и баннер
// сам покажет «Подключено», а настоящий отказ назовём вслух вчетверо раньше.
const OFFLINE_AFTER_MS = 8_000;

/**
 * Сколько ждать, прежде чем показать полосу вообще.
 *
 * Подписка отдаёт текущее состояние сразу при монтировании, а на холодном
 * старте сокет ещё не поднят — поэтому ПЕРВЫМ, что человек видел, открыв
 * мини-апп, была тревожная полоса во всю ширину (в Telegram она закрашивала
 * заодно и зону под шапкой). Обычный запуск и моргнувшая сеть укладываются в
 * эту паузу и до экрана не доходят; настоящий обрыв опаздывает на секунду и от
 * этого не теряет ничего.
 *
 * Экспортируется наружу: экран терминала молчит ровно те секунды, когда говорит
 * полоса, — об одном обрыве человек должен читать одно сообщение, а не два.
 */
export const CONNECTION_BANNER_DELAY_MS = 1_200;

/**
 * Сколько ждать подтверждения, что компьютер ВЕРНУЛСЯ. Наш сокет живёт до
 * релея, и его «подключено» ничего не говорит про сам ПК: «в сети / не в сети»
 * приходит отдельным кадром сразу после подписки. Без этой паузы полоса на
 * каждой попытке реконнекта моргала зелёным «Подключено» и тут же возвращалась
 * к «Компьютер не в сети» — экран и полоса спорили об одном и том же.
 */
const PC_BACK_CONFIRM_MS = 2_000;

/** Браузер знает, что сети нет вовсе — ждать нечего, говорим сразу. */
function browserOffline(): boolean {
  return typeof navigator !== "undefined" && navigator.onLine === false;
}

/**
 * Геометрия «таблетки» с текстом. Цвет по-прежнему приходит классами
 * .conn-off/.conn-ok, но теперь он живёт на ней, а не на всей полосе: заливка
 * во всю ширину вместе с safe-area/шапкой Telegram занимала верхнюю треть
 * экрана, и тревога оказывалась первым, что видит человек. Раскладка задана
 * здесь, чтобы полоса стала спокойнее сразу, не дожидаясь правки styles.css.
 */
const PILL_STYLE: CSSProperties = {
  display: "flex",
  alignItems: "center",
  justifyContent: "center",
  // На 320 px «Нет связи с Remotai» + «Повторить» в одну строку не помещаются —
  // переносим внутри таблетки, а не выпускаем за край экрана. Высоту полосы
  // страницы читают из --connection-banner-height, перенос им не мешает.
  flexWrap: "wrap",
  gap: "8px",
  maxWidth: "100%",
  padding: "4px 12px",
  borderRadius: "999px",
};

interface Props {
  /** Подписка на изменения соединения — приложение передаёт свой api.onConnectionChange. */
  subscribe: (cb: (state: {
    connected: boolean;
    reason?: "pc_offline" | "net";
  }) => void) => () => void;
  /**
   * Показывать ли баннер вообще (по умолчанию true). Функция вычисляется на
   * каждом событии соединения — apk передаёт hasServerConfig, чтобы до
   * пейринга баннер был скрыт, а после пейринга появился без перерендера App.
   */
  enabled?: boolean | (() => boolean);
  /** Ручной ретрай для терминального состояния (apk передаёт connectWS). */
  onRetry?: () => void;
  entity?: "computer" | "server";
  /**
   * Имя машины, о которой идёт речь. Функция, потому что человек переключает
   * машину, не перемонтируя баннер: вычисляем на каждом рендере, иначе после
   * переключения баннер называл бы прежнюю.
   */
  deviceName?: string | (() => string);
  /**
   * Каким путём клиент ходит за данными — от этого зависит честный текст
   * отказа: прямой путь = «Компьютер не отвечает», облако = «Нет связи с
   * Remotai». По умолчанию берём маршрут, о котором приложение сообщило через
   * setNetworkContext(); функция вычисляется на каждом рендере, поэтому смена
   * режима видна без перемонтирования баннера.
   */
  route?: NetworkRoute | (() => NetworkRoute);
}

export function ConnectionBanner({
  subscribe,
  enabled = true,
  onRetry,
  entity = "computer",
  deviceName: deviceNameProp,
  route = getNetworkRoute,
}: Props) {
  const deviceName = typeof deviceNameProp === "function" ? deviceNameProp() : (deviceNameProp || "");
  const location = useLocation();
  const navigate = useNavigate();
  const [connected, setConnected] = useState(true);
  const [reason, setReason] = useState<"pc_offline" | "net">("net");
  const [showBanner, setShowBanner] = useState(false);
  const bannerRef = useRef<HTMLDivElement | null>(null);
  // Долгий офлайн: спиннер сменяется на честную причину + кнопку «Повторить».
  const [longOffline, setLongOffline] = useState(false);
  // Сеть у самого клиента. Отдельным состоянием, чтобы текст перестал врать
  // «Нет связи с интернетом», когда интернет как раз есть, и наоборот —
  // мгновенно называл пропавшую сеть.
  const [online, setOnline] = useState(!browserOffline());
  const connectedRef = useRef(true);
  connectedRef.current = connected;
  // Причина текущего обрыва названа вслух. Держится до подтверждённого возврата
  // связи: узнав «компьютер не в сети», полоса не откатывается обратно в
  // «Переподключение…» на каждой попытке реконнекта — иначе про один обрыв
  // человек читает две разные версии, да ещё и вразрез с карточкой экрана,
  // которая уже сказала, что компьютер выключен.
  const pcOfflineRef = useRef(false);

  useEffect(() => {
    let hideTimer: ReturnType<typeof setTimeout> | null = null;
    let offlineTimer: ReturnType<typeof setTimeout> | null = null;
    let showTimer: ReturnType<typeof setTimeout> | null = null;
    let backTimer: ReturnType<typeof setTimeout> | null = null;
    const clearHide = () => { if (hideTimer) { clearTimeout(hideTimer); hideTimer = null; } };
    const clearOffline = () => { if (offlineTimer) { clearTimeout(offlineTimer); offlineTimer = null; } };
    const clearShow = () => { if (showTimer) { clearTimeout(showTimer); showTimer = null; } };
    const clearBack = () => { if (backTimer) { clearTimeout(backTimer); backTimer = null; } };
    // Show "reconnected" briefly, then hide.
    const settleConnected = () => {
      setConnected(true);
      hideTimer = setTimeout(() => setShowBanner(false), 2000);
    };
    const unsub = subscribe((state) => {
      if (!(typeof enabled === "function" ? enabled() : enabled)) {
        clearHide();
        clearOffline();
        clearShow();
        clearBack();
        pcOfflineRef.current = false;
        setShowBanner(false);
        setLongOffline(false);
        return;
      }
      const c = state.connected;
      clearHide(); // cancel any pending hide before re-deciding
      if (!c) {
        clearBack();
        clearShow();
        if (state.reason === "pc_offline") pcOfflineRef.current = true;
        const known = pcOfflineRef.current;
        setConnected(false);
        setReason(known ? "pc_offline" : (state.reason || "net"));
        if (known) {
          // Причина известна — ждать нечего. Спиннер «Переподключение…» здесь
          // был бы обещанием, которого никто не даёт: пока компьютер выключен,
          // подключаться не к чему, и полоса сразу говорит это словами.
          clearOffline();
          setLongOffline(false);
          setShowBanner(true);
          return;
        }
        // Сети нет вовсе — спиннер «Переподключение…» был бы ложной надеждой.
        if (browserOffline()) {
          clearOffline();
          setLongOffline(true);
          setShowBanner(true);
          return;
        }
        if (!offlineTimer) {
          offlineTimer = setTimeout(() => {
            offlineTimer = null;
            setLongOffline(true);
            setShowBanner(true);
          }, OFFLINE_AFTER_MS);
        }
        // Причина неизвестна и сеть на месте — даём связи шанс восстановиться
        // молча (см. CONNECTION_BANNER_DELAY_MS).
        if (!showTimer) {
          showTimer = setTimeout(() => { showTimer = null; setShowBanner(true); }, CONNECTION_BANNER_DELAY_MS);
        }
      } else {
        clearOffline();
        clearShow();
        setLongOffline(false);
        if (pcOfflineRef.current) {
          // Наш канал ожил, но про сам компьютер это ещё ничего не значит:
          // ответ «в сети / не в сети» придёт следующим кадром. Пока он не
          // пришёл, держим прежний текст (см. PC_BACK_CONFIRM_MS).
          if (!backTimer) {
            backTimer = setTimeout(() => {
              backTimer = null;
              pcOfflineRef.current = false;
              settleConnected();
            }, PC_BACK_CONFIRM_MS);
          }
          return;
        }
        settleConnected();
      }
    });
    return () => { unsub(); clearHide(); clearOffline(); clearShow(); clearBack(); };
  }, [subscribe, enabled]);

  // Пропажа/возврат сети у клиента: и текст, и скорость перехода в терминальное
  // состояние зависят от этого, а WS-событие о разрыве может прийти намного позже.
  useEffect(() => {
    if (typeof window === "undefined") return;
    const goOffline = () => {
      setOnline(false);
      // Сеть пропала у самого телефона при уже мёртвом канале — причина
      // названа, и выжидать CONNECTION_BANNER_DELAY_MS больше незачем.
      if (!connectedRef.current && (typeof enabled === "function" ? enabled() : enabled)) {
        setLongOffline(true);
        setShowBanner(true);
      }
    };
    const goOnline = () => setOnline(true);
    window.addEventListener("offline", goOffline);
    window.addEventListener("online", goOnline);
    return () => {
      window.removeEventListener("offline", goOffline);
      window.removeEventListener("online", goOnline);
    };
  }, [enabled]);

  const visible = showBanner && !LOGIN_ROUTES.has(location.pathname);

  // Fixed-баннер не должен перекрывать шапку рабочего экрана. Публикуем его
  // реальную высоту (включая safe-area и возможный перенос текста на телефоне)
  // в CSS-переменную, чтобы обычные и полноэкранные страницы зарезервировали
  // ровно нужное место.
  useLayoutEffect(() => {
    if (typeof document === "undefined") return;
    const root = document.documentElement;
    if (!visible) {
      root.classList.remove("connection-banner-visible");
      root.style.removeProperty("--connection-banner-height");
      return;
    }

    root.classList.add("connection-banner-visible");
    const syncHeight = () => {
      const height = bannerRef.current?.getBoundingClientRect().height ?? 0;
      root.style.setProperty("--connection-banner-height", `${Math.ceil(height)}px`);
    };
    syncHeight();
    const observer = typeof ResizeObserver !== "undefined" && bannerRef.current
      ? new ResizeObserver(syncHeight)
      : null;
    // box: "border-box" обязателен. Инсет Telegram приезжает баннеру ПАДДИНГОМ
    // (см. .conn-banner в styles.css), а наблюдатель по умолчанию смотрит
    // content-box и на смену паддинга не реагирует — переменная остаётся
    // старой. Цена ошибки видна ровно там, где эта высота и нужна: у экрана
    // терминала с видимым баннером собственный верхний отступ снят
    // (html.connection-banner-visible .pty-page .pty-header), и всё разведение
    // держится на этой переменной — при устаревшем значении кнопка «←» уезжает
    // под плавающую «✕ Закрыть» на iPad (замер: 29 px вместо 85 px).
    if (observer && bannerRef.current) observer.observe(bannerRef.current, { box: "border-box" });
    window.addEventListener("resize", syncHeight);

    return () => {
      observer?.disconnect();
      window.removeEventListener("resize", syncHeight);
      root.classList.remove("connection-banner-visible");
      root.style.removeProperty("--connection-banner-height");
    };
  }, [visible]);

  if (!visible) return null;

  /**
   * Честный текст терминального состояния. Раньше здесь всегда было «Нет связи
   * с интернетом» — при живом интернете и выключенном ПК это прямая ложь
   * (UX-аудит V3): человек шёл проверять роутер вместо того, чтобы включить
   * компьютер.
   */
  const terminalLabel = (): string => {
    if (!online) return t("conn.noInternet");
    const r = typeof route === "function" ? route() : route;
    if (r === "cloud") return t("conn.noService");
    if (r === "direct") return t(entity === "server" ? "conn.serverNotResponding" : "conn.pcNotResponding");
    // Маршрут неизвестен (приложение не сообщило) — говорим только то, что видно.
    return t("conn.disconnected");
  };

  return (
    <div
      ref={bannerRef}
      className="conn-banner"
      role="status"
      aria-live="polite"
      aria-atomic="true"
    >
      {/* Цвет — на «таблетке», а не на всей полосе (см. PILL_STYLE): сплошная
          заливка во всю ширину читалась как авария всего приложения. Текст,
          кнопка и смысл прежние. */}
      <div className={`conn-banner-pill ${connected ? "conn-ok" : "conn-off"}`} style={PILL_STYLE}>
        {connected ? (
          <>{"✓"} {t("conn.reconnected")}</>
        ) : !online ? (
          /* Сеть пропала У САМОГО ТЕЛЕФОНА — это важнее всего, что мы знали о
             компьютере до обрыва. Проверено на живом телефоне (аудит путей
             29.08.2026): с известным офлайновым ПК потеря сети не меняла текст,
             и человек ехал к компьютеру, хотя дело было в кармане. Браузер
             здесь знает наверняка (navigator.onLine === false), поэтому ждать
             longOffline не нужно — говорим сразу. */
          <>
            {t("conn.noInternet")}
            {onRetry && (
              <button type="button" className="conn-retry-btn" onClick={onRetry}>
                {t("conn.retry")}
              </button>
            )}
          </>
        ) : reason === "pc_offline" ? (
          <>
            {/* КАКАЯ машина не в сети — по имени.
                Живой случай (2026-07-28): человек привязал сервер, переключился
                на него, а сверху висело безличное «Компьютер не в сети» — при
                том что в аккаунте две машины и вторая работала. Из такого
                сообщения нельзя понять ни про кого оно, ни что делать. Имени
                может не быть (первый заход, машина без имени) — тогда прежний
                общий текст. */}
            {deviceName
              ? t(entity === "server" ? "conn.serverOfflineNamed" : "conn.pcOfflineNamed", { name: deviceName })
              : t(entity === "server" ? "conn.serverOffline" : "conn.pcOffline")}
            <button type="button" className="conn-retry-btn" onClick={() => navigate("/infrastructure")}>
              {t("conn.myComputers")}
            </button>
          </>
        ) : longOffline ? (
          <>
            {terminalLabel()}
            {onRetry && (
              // Остаёмся в «Нет соединения» до подтверждённого реконнекта
              // (событие connected само переключит баннер) — возврат к вечному
              // спиннеру после клика был бы той же ложной надеждой.
              <button type="button" className="conn-retry-btn" onClick={onRetry}>
                {t("conn.retry")}
              </button>
            )}
          </>
        ) : (
          <>
            <div className="conn-spinner" />
            {t("conn.reconnecting")}
          </>
        )}
      </div>
    </div>
  );
}
