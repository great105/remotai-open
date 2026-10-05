import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { watchClientVersion } from "./clientUpdate";
import { MemoryRouter, HashRouter } from "react-router-dom";
import { App } from "./App";
import { ToastProvider, setPlatform, getLocalTokenInitData, DialogHost } from "@tgcontrol/shared";
import { connectWS } from "./api";
import { hasServerConfig, saveConfig, isNativeApp } from "./config";
import { resumePendingTelegramLogin, hasPendingTelegramLogin } from "./cloud/tgLogin";
import { ensureMiniAppJWT } from "./cloud/miniAppAuth";
import { resumePendingOAuthLogin, hasPendingOAuthLogin } from "./cloud/oauthLogin";
import { initSentry, captureError, SentryErrorBoundary, isSentryActive } from "./observability";
import { haptic, hapticSuccess, hapticError, tgConfirm, getTelegram } from "./telegram";
import { pushBackHandler } from "./hooks/backHandler";
import { saveBlob, isShareCancel } from "./saveFile";
import { t } from "./i18n";
import { loginNextPath } from "./nextPath";
import "./styles.css";

initSentry();

// Платформенный адаптер для общих компонентов из @tgcontrol/shared (Capacitor).
// saveBlob: нативно — кэш Capacitor + системный share sheet (см. saveFile.ts).
setPlatform({
  haptic, hapticSuccess, hapticError, confirm: tgConfirm, pushBackHandler,
  saveBlob, isSaveCancel: isShareCancel,
});

// Telegram Mini App: инициализация WebApp (раскрытие/фуллскрин). No-op вне
// Telegram (в окне exe/браузере/APK window.Telegram нет).
(() => {
  const tg = getTelegram();
  if (!tg) return;
  try {
    tg.ready();
    tg.expand();
    // Планшет, а окно телефонное. Живая жалоба с iPad (30.07): из кнопки меню
    // бота мини-апп открывается узким окном «как на iPhone», а из кнопки
    // «Открыть» под сообщением — во весь экран. Ширину окна выбирает клиент
    // Telegram, и единственная ручка с нашей стороны — requestFullscreen
    // (Bot API 8.0). Зовём его ТОЛЬКО в этом случае: экран большой
    // (screen.width ≥ 900), а вьюпорт узкий. На телефоне условие не выполняется
    // никогда, поэтому там всё как было — фуллскрин туда не приходит.
    try {
      const screenW = window.screen?.width ?? 0;
      if (screenW >= 900 && window.innerWidth < 700) tg.requestFullscreen?.();
    } catch { /* старый клиент: ручки нет — остаётся окно, как раньше */ }
    tg.disableVerticalSwipes?.();
    // iPad: Telegram показывает мини-апп окном со своей шапкой («✕ Закрыть»)
    // ПОВЕРХ верха вьюпорта, а env(safe-area-inset-top) внутрь не пробрасывает.
    // Высоту шапки берём из contentSafeAreaInset (Bot API 8.0) и кладём в CSS-
    // переменную, которую наши хедеры добавляют к верхнему отступу (см.
    // --tg-top-inset в styles.css), иначе кнопка «назад» уезжает под «Закрыть».
    // Нижний инсет Telegram пробрасывает так же плохо, как верхний: на iPhone
    // env(safe-area-inset-bottom) внутрь мини-аппа не доезжает, и нижняя
    // панель садится под полосу home-indicator — палец промахивается мимо
    // самой частой панели приложения. Ради этого Bot API 8.0 и завёл свои
    // инсеты: safeAreaInset — железо (полоса/чёлка), contentSafeAreaInset —
    // обвязка самого Telegram. Берём больший.
    //
    // В ПОЛНОЭКРАННОМ режиме кнопки «✕ Закрыть» и «⋯» Telegram висят ПОВЕРХ
    // нашего верха, и на iPad клиент сообщил contentSafeAreaInset.top = 0 —
    // заголовок «Remotai» и первый пункт бокового меню оказались прямо под
    // ними (живая жалоба 30.07: «кнопка Закрыть постоянно мешает»). Поэтому в
    // фуллскрине держим НИЖНЮЮ ГРАНИЦУ отступа: величина клиента, но не меньше
    // высоты его плавающих кнопок. Вне фуллскрина граница нулевая — там шапку
    // Telegram рисует сам и накладываться нечему.
    const FULLSCREEN_CHROME_TOP = 56;
    // На планшете граница выше телефонной, и это не запас «на всякий случай».
    // Пилюля «✕ Закрыть» там занимает 8..52 px, а сразу под ней стоит НАША «←»:
    // при отступе 56 между целями остаётся 4 px, и промах пальцем по самой левой
    // кнопке экрана закрывает мини-апп вместе с работой. 64 даёт зазор 12 px —
    // кнопки перестают быть одной целью (живая жалоба 04.08: «кнопка Закрыть
    // закрывает кнопку Назад, сделай Назад в другом месте»).
    const PAD_CHROME_TOP = 64;
    // Планшет Apple: кнопки Telegram висят поверх мини-аппа и в фуллскрине, и
    // когда клиент просто растянул его на весь экран, — а инсет он при этом
    // сообщает нулём. Отличить эти два состояния изнутри нечем, поэтому нижнюю
    // границу держим по факту «планшет Apple + инсет нулевой».
    // На телефоне (узкий экран) и на десктопном Telegram, где шапка честная,
    // условие не выполняется и лишнего отступа не появляется.
    //
    // ⚠ СЧИТАЕМ ПО ЭКРАНУ УСТРОЙСТВА, А НЕ ПО ШИРИНЕ ОКНА (правка 04.08.2026 по
    // живой жалобе «кнопка Закрыть закрывает кнопку Назад»). Порогов было два и
    // они не сходились: фуллскрин мы просим при innerWidth < 700, а «планшетный
    // пол» ставили при innerWidth >= 768 — в промежутке 700–767 px не
    // срабатывало НИЧЕГО, инсет оставался нулём, и наша «←» целиком ложилась
    // под пилюлю «✕ Закрыть» (замер стенда, окно 720×900: кнопка (12,8)-(56,52)
    // внутри пилюли (8,8)-(158,52), заголовок обрезан слева — байт в байт
    // скриншот владельца). Ширина окна на планшете гуляет (Split View, Slide
    // Over, поворот), а экран — нет: по нему и решаем.
    //
    // ⚠ ВТОРАЯ ПРАВКА ТОГО ЖЕ ДНЯ (жалоба «в мини-аппе на iPad всё равно не могу
    // нажать назад» уже ПОСЛЕ 2.49.21). Опираться на `platform` и `screen`
    // оказалось мало: что именно возвращает клиент на конкретном планшете, мы
    // не знаем, а стенд воспроизводит только то, что мы в него сами заложили.
    // Поэтому добавлен признак, который не зависит от того, как клиент себя
    // назвал: **окно мини-аппа почти во весь экран И обе его стороны
    // планшетные**. У Telegram Desktop окно мини-аппа заметно меньше экрана и
    // со своей шапкой — там лишнего отступа не появится; у телефона одна
    // сторона всегда меньше 600. Заодно клиент теперь СООБЩАЕТ эти числа в лог
    // агента (diag «tg-chrome» в PtyTermView) — чтобы следующий такой случай
    // разбирался по фактам, а не по догадкам.
    const tabletBox = () => {
      const w = window.innerWidth || 0;
      const h = window.innerHeight || 0;
      return Math.min(w, h) >= 600;
    };
    const nearlyFullScreen = () => {
      const scr = Math.max(window.screen?.width ?? 0, window.screen?.height ?? 0);
      const win = Math.max(window.innerWidth || 0, window.innerHeight || 0);
      return scr > 0 && win >= scr * 0.9;
    };
    const applePad = () => {
      const platform = (tg.platform || "").toLowerCase();
      const apple = platform === "ios" || platform === "macos";
      const screenW = window.screen?.width ?? 0;
      const screenH = window.screen?.height ?? 0;
      if (apple && Math.max(screenW, screenH) >= 900) return true;
      // Клиент назвался иначе (или экран сообщил не тот) — смотрим на факт.
      return tabletBox() && nearlyFullScreen();
    };
    const applyTgInsets = () => {
      const reported = tg.contentSafeAreaInset?.top ?? 0;
      const pad = applePad();
      const floor = pad ? PAD_CHROME_TOP : tg.isFullscreen ? FULLSCREEN_CHROME_TOP : 0;
      const top = Math.max(reported, floor);
      const bottom = Math.max(tg.safeAreaInset?.bottom ?? 0, tg.contentSafeAreaInset?.bottom ?? 0);
      // System chrome and Telegram's content margin are separate SDK insets.
      // Export system top independently; only Hermes opts into composing it.
      document.documentElement.style.setProperty("--hermes-tg-system-top", `${Math.max(0, tg.safeAreaInset?.top ?? 0)}px`);
      document.documentElement.style.setProperty("--tg-content-safe-area-inset-top", `${top}px`);
      document.documentElement.style.setProperty("--tg-content-safe-area-inset-bottom", `${bottom}px`);
    };
    applyTgInsets();
    tg.onEvent?.("contentSafeAreaChanged", applyTgInsets);
    tg.onEvent?.("safeAreaChanged", applyTgInsets);
    // Фуллскрин включается асинхронно (наш запрос выше или жест человека) —
    // без этой подписки отступ остался бы от прежнего режима.
    tg.onEvent?.("fullscreenChanged", applyTgInsets);
    // Фуллскрин может и НЕ включиться: старый клиент, уже полный экран, отказ
    // платформы. Тогда Telegram шлёт fullscreenFailed, и без этой подписки мы
    // остались бы с нулевым отступом под собственными плавающими кнопками
    // клиента. Пересчёт здесь ничего не ломает: пол считается заново.
    tg.onEvent?.("fullscreenFailed", applyTgInsets);
    // Размер окна на планшете меняют поворот, Split View и Slide Over — и от
    // него зависит и наш пол, и то, попадает ли шапка под кнопки клиента.
    tg.onEvent?.("viewportChanged", applyTgInsets);
    let insetTimer: ReturnType<typeof setTimeout> | null = null;
    window.addEventListener("resize", () => {
      if (insetTimer) clearTimeout(insetTimer);
      insetTimer = setTimeout(applyTgInsets, 120);
    });

    // Светлая тема Telegram делала половину интерфейса нечитаемой. Telegram
    // подставляет свои --tg-theme-* прямо в style документа, а :root читает их
    // как ИСТОЧНИК с нашим тёмным фолбэком (--tg-text: var(--tg-theme-text-color,
    // #c9d1d9)). При светлой теме текст становится почти чёрным, а фоны карточек,
    // шапок и шторок у нас собственные и всегда тёмные (--tg-surface: #111820) —
    // чёрное по тёмному. Своей светлой палитры у продукта нет, поэтому не
    // смешиваем чужие цвета со своими: снимаем переменные темы клиента, и :root
    // сразу откатывается на нашу тёмную палитру целиком. Поэтому же не читаем
    // tg.colorScheme — схема у нас одна, тёмная, и она одинакова во всех четырёх
    // интерфейсах.
    const dropTgThemeVars = () => {
      const s = document.documentElement.style;
      // Идём с конца: removeProperty сдвигает индексы живой CSSStyleDeclaration.
      for (let i = s.length - 1; i >= 0; i--) {
        const name = s.item(i);
        if (name.startsWith("--tg-theme-")) s.removeProperty(name);
      }
    };
    dropTgThemeVars();
    tg.onEvent?.("themeChanged", dropTgThemeVars);
    // Собственная обвязка Telegram (шапка над мини-аппом и полоса под ним)
    // красится отдельно — иначе вокруг тёмного приложения остаётся белая рамка.
    // Значения дублируют --tg-bg / --tg-section-bg из styles.css намеренно:
    // на этой строке стили ещё могут быть не применены, и getComputedStyle
    // вернул бы пустоту.
    try {
      tg.setHeaderColor?.("#161b22");
      tg.setBackgroundColor?.("#0d1117");
      tg.setBottomBarColor?.("#161b22");
    } catch { /* Bot API старее 6.1 — цвет обвязки останется дефолтным */ }
  } catch (e) {
    console.error("Telegram init error", e);
  }
})();

// Окно exe (WebView2) и браузер другого устройства открывают клиент как
// `/miniapp?token=<API_TOKEN>` на том же origin. Пейринга нет → засеваем
// self_hosted-конфиг (same-origin URL + токен), чтобы apk-клиент заработал
// без экрана входа. На телефоне config уже заполнен пейрингом → этот блок
// не срабатывает (?token= в URL нет).
if (!hasServerConfig()) {
  const initData = getLocalTokenInitData(); // "token:<API_TOKEN>" | ""
  if (initData.startsWith("token:")) {
    try {
      saveConfig({ mode: "self_hosted", url: window.location.origin, token: initData.slice(6) });
    } catch (e) {
      console.error("seed config from ?token= failed", e);
    }
  }
}

// Telegram Mini App: свежий initData сразу меняем на долгий JWT — иначе на
// вторые сутки открытый в Telegram мини-апп получает 401 на каждый запрос
// (см. cloud/miniAppAuth.ts). Вне Telegram — no-op. Не ждём: первые запросы
// уйдут с ещё свежим initData, а следующие — уже с JWT.
void ensureMiniAppJWT();

// Connect WS only if server is configured
if (hasServerConfig()) {
  try {
    connectWS();
  } catch (e) {
    console.error("WS error", e);
    captureError(e, { phase: "ws-connect" });
  }
}

/**
 * Перезагрузка ПОСЛЕ успешного входа — с главной, а не с экрана входа.
 *
 * Аудит путей 29.08.2026: адрес держит `#/cloud-login`, поэтому после reload
 * человек снова видел кнопку «Войти» и считал, что вход не удался. Адрес
 * переводим до перезагрузки; обратный гард на маршруте (App.tsx) закрывает
 * тот же случай со стороны роутера.
 */
function reloadAsSignedIn(): void {
  try {
    if (/^#\/(cloud-login|login)(?:\?|$)/.test(window.location.hash)) {
      const search = window.location.hash.includes("?") ? window.location.hash.slice(window.location.hash.indexOf("?")) : "";
      window.location.hash = "#" + loginNextPath(window.history.state?.usr, search);
    }
  } catch { /* экзотическая среда без hash */ }
  window.location.reload();
}

// Завершение входа через Telegram: уход в Telegram-приложение выгружает/морозит
// WebView и убивает in-memory опрос. Pending-токен сохранён в localStorage —
// до-опрашиваем при возврате в приложение и на старте; по успеху перезагружаем,
// чтобы подняться уже залогиненными (durable JWT сохранён в pollOnce → saveConfig).
function completeTgLoginOnResume(): void {
  resumePendingTelegramLogin()
    .then((ok) => { if (ok) reloadAsSignedIn(); })
    .catch(() => {});
  // То же для OAuth-входа (VK/Яндекс/Google): уход в браузер морозит WebView.
  resumePendingOAuthLogin()
    .then((ok) => { if (ok) reloadAsSignedIn(); })
    .catch(() => {});
}
document.addEventListener("visibilitychange", () => {
  if (document.visibilityState === "visible") completeTgLoginOnResume();
});
import("@capacitor/app")
  .then(({ App: CapApp }) => { void CapApp.addListener("resume", completeTgLoginOnResume); })
  .catch(() => { /* не Capacitor — visibilitychange достаточно */ });
if (hasPendingTelegramLogin() || hasPendingOAuthLogin()) completeTgLoginOnResume();

// Edge-to-edge: WebView рисует под status/nav bar — на планшете
// получается полный экран. Стили учитывают safe-area-inset-*.
(async () => {
  try {
    const { StatusBar, Style } = await import("@capacitor/status-bar");
    await StatusBar.setOverlaysWebView({ overlay: true });
    await StatusBar.setStyle({ style: Style.Dark });
    await StatusBar.setBackgroundColor({ color: "#00000000" });
  } catch {
    // Не Android / плагин не подключён — просто игнорируем.
  }
})();


/**
 * Куда открыть мини-апп: Telegram передаёт цель в start_param
 * (`t.me/bot?startapp=pty_<id>`), а веб-версия — в hash. Раньше не разбиралось
 * нигде, поэтому ссылки «открыть этот терминал» / «ответить агенту» из бота
 * технически не существовали: MemoryRouter всегда стартовал с "/".
 */
function initialRoute(): string {
  try {
    const tg = (window as { Telegram?: { WebApp?: { initDataUnsafe?: { start_param?: string } } } }).Telegram?.WebApp;
    // Direct WebApp buttons carry a real route fragment. MemoryRouter does not
    // consume it automatically, so make it the first in-memory entry.
    if (window.location.hash.startsWith("#/")) {
      return window.location.hash.slice(1);
    }
    const raw = tg?.initDataUnsafe?.start_param
      || new URLSearchParams(window.location.search).get("tgWebAppStartParam")
      || window.location.hash.replace(/^#/, "");
    if (!raw) return "/";
    const [kind, ...rest] = raw.split("_");
    const arg = rest.join("_");
    switch (kind) {
      case "pty": {
        // pty_<device>_<pty> first selects the right computer, then opens the
        // terminal. Legacy pty_<pty> links continue to work.
        // Целимся сразу в канонический `/infrastructure` (приёмник select/next
        // живёт там), чтобы не зависеть от лишнего прыжка через алиас /devices.
        if (rest.length >= 2) {
          const [device, ...ptyParts] = rest;
          const pty = ptyParts.join("_");
          return `/infrastructure?select=${encodeURIComponent(device)}&next=${encodeURIComponent(`/pty/${pty}`)}`;
        }
        return arg ? `/pty/${encodeURIComponent(arg)}` : "/pty";
      }
      case "device": return arg ? `/infrastructure?select=${encodeURIComponent(arg)}` : "/infrastructure";
      case "support": return "/support";
      case "files": return "/files";
      case "remote": return "/remote";
      // `system_power` → вкладка «Питание»: вкладка теперь в адресе (аудит ИА
      // 02.09.2026, P1-28), и кнопка бота может вести прямо к ней.
      case "system": return arg && /^(monitor|processes|power)$/.test(arg) ? `/system?tab=${arg}` : "/system";
      default: return "/";
    }
  } catch {
    return "/";
  }
}

// Render
const root = document.getElementById("root")!;

// Где живёт маршрут. MemoryRouter — только там, где адресной строки нет
// физически: нативный APK (Capacitor) и Telegram Mini App. В вебе
// (remotai.ru/app) и в окне exe адрес есть, и держать его пустым — прямой
// вред: «Назад» браузера выкидывало ИЗ приложения (человек читал это как
// падение), F5 всегда возвращало на «/» вместе с потерей открытого терминала
// и набранного текста, а ссылку «открой у себя вот этот экран» передать было
// нечем. HashRouter кладёт маршрут в #/… — сервер и его SPA-фолбэк трогать не
// нужно, а initialRoute() уже понимает такой адрес.
const memoryRouted = isNativeApp || !!getTelegram();
const startRoute = initialRoute();
if (!memoryRouted && startRoute !== "/" && !window.location.hash.startsWith("#/")) {
  // Легаси-адреса без слэша (#pty_<id>, ?tgWebAppStartParam=…) нормализуем в
  // обычный маршрут ДО монтирования, иначе HashRouter прочитает их как путь
  // «pty_<id>» и уведёт на главную.
  window.history.replaceState(null, "", `${window.location.pathname}${window.location.search}#${startRoute}`);
}

const routed = (
  <ToastProvider>
    <App />
    <DialogHost />
  </ToastProvider>
);
const tree = (
  <StrictMode>
    {memoryRouted
      ? <MemoryRouter initialEntries={[startRoute]}>{routed}</MemoryRouter>
      : <HashRouter>{routed}</HashRouter>}
  </StrictMode>
);
// Клиент следит за собственной свежестью: мини-апп на планшете живут
// свёрнутым сутками, и без этой проверки человек неделями работает со сборкой,
// в которой его же дефект ещё не починен (живой случай 04.08.2026).
watchClientVersion();
createRoot(root).render(
  isSentryActive() ? (
    <SentryErrorBoundary fallback={<div style={{ padding: 20, color: "red" }}>{t("error.somethingWrong")}</div>}>
      {tree}
    </SentryErrorBoundary>
  ) : (
    tree
  )
);
