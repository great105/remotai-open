import { useCallback } from "react";
import { useLocation, useNavigate, type Location, type NavigateFunction } from "react-router-dom";
import { getMode, getSelectedDeviceId, hasServerConfig } from "./config";

/**
 * ОДНА «Назад» на экране.
 *
 * У вторичных экранов кнопок возврата две: нарисованная «←» в шапке и
 * системная (Android Back, свайп от края, кнопка Telegram). Они жили порознь:
 * общий обработчик в App.tsx уводил на главную, а «←» шагала по истории — и
 * человек не мог предсказать, куда его вернёт. Здесь лежит единственное
 * правило, которым теперь пользуются обе: ШАГ НАЗАД ПО ИСТОРИИ, а если шагать
 * некуда (экран открыт по прямой ссылке, из уведомления, из кнопки бота) —
 * переход к логическому родителю.
 *
 * Работает одинаково в обеих сборках роутера: в вебе и окне exe это
 * HashRouter, в APK и Telegram — MemoryRouter. Поэтому «есть ли куда шагать»
 * считается не по `window.history.length` (в MemoryRouter он про WebView, а не
 * про наши экраны), а по ключу записи: первая запись любой истории — "default",
 * у всех последующих ключ свой.
 */

/**
 * Годится ли `?from=` как дом для «Назад»: это ДОЛЖЕН быть внутренний путь.
 *
 * Два условия, и оба выстрадали:
 *  - начинается со слэша и не с двух (`//evil.com` браузер считает ЧУЖИМ
 *    адресом — так открытый терминал уводил бы человека с сайта);
 *  - обратный слэш тоже отбрасываем: `/\evil.com` некоторые движки нормализуют
 *    в `//evil.com`.
 *
 * ⚠ Корень «/» — валидный дом. Прежняя проверка `/^\/[^/]/` его НЕ пропускала:
 * после слэша требовался ещё один символ, а у главной его нет. Из-за этого
 * `?from=/` молча отбрасывался, и терминал, открытый плиткой с ГЛАВНОЙ,
 * возвращал в список терминалов — единственная строка «назад ведёт не туда» в
 * обходе карты 04.09.2026 держалась именно на этом.
 */
export function isInnerPath(path: string | null | undefined): path is string {
  if (!path || path[0] !== "/") return false;
  return path[1] !== "/" && path[1] !== "\\";
}

/** Часть Location, которой хватает, чтобы решить, куда ведёт «Назад». */
export type BackLocation = Pick<Location, "pathname" | "search" | "state" | "key">;

/**
 * Экраны, у которых обе кнопки «Назад» шагают по истории.
 *
 * Сюда попадает экран только после того, как его собственная «←» начала звать
 * `useGoBack()` — иначе системная кнопка и стрелка на одном экране ведут в
 * разные места. Аудит ИА 02.09.2026 нашёл четыре экрана с жёстким
 * `navigate("/")` (настройки, SSH-серверы, «Мои компьютеры», кабинет по
 * фолбэку): из настроек открыл «Подписку», нажал «←» — и оказался на главной.
 * Теперь их стрелки зовут `useGoBack()`, и они здесь. Вне списка остаются
 * терминал (`/pty/:id`, у него свой `?from=`) и файлы сервера (`/ssh-files`,
 * там системная «Назад» ходит по папкам через `useBackHandler`).
 */
function stepsThroughHistory(pathname: string): boolean {
  return pathname === "/remote"
    || pathname === "/agents"
    || pathname === "/agents/sessions"
    || pathname === "/hermes"
    || pathname === "/usage"
    || pathname === "/support"
    || pathname === "/guide"
    || pathname === "/settings"
    || pathname === "/account"
    || pathname === "/plan"
    || pathname === "/ssh"
    || /^\/ssh\/./.test(pathname)
    || pathname === "/infrastructure"
    || pathname === "/devices"
    || pathname === "/cloud-login"
    || pathname === "/start"
    || /^\/research\/./.test(pathname);
}

/**
 * Корень приложения: отсюда системная «Назад» не уходит вглубь, а сворачивает
 * окно (в Telegram — закрывает мини-апп).
 *
 * Корень зависит от состояния, а не только от адреса. «Мои компьютеры» — корень
 * лишь пока в облаке не выбран ни один ПК (тогда это и есть первый экран,
 * `DeviceGuard` возвращает сюда с главной); с выбранной машиной это обычный
 * раздел из шторки «Ещё», и «Назад» на нём сворачивала приложение при живой
 * «←» на главную (аудит ИА 02.09.2026, P0-1). Экран входа — корень только при
 * пустой конфигурации: «Войти в аккаунт» из живого локального подключения
 * открывал экран, с которого системная «Назад» сворачивала приложение (P0-4).
 */
export function isRootPath(pathname: string): boolean {
  if (pathname === "/") return true;
  if (pathname === "/login" || pathname === "/cloud-login") return !hasServerConfig();
  if (pathname === "/infrastructure" || pathname === "/devices") {
    return getMode() === "cloud" && !getSelectedDeviceId();
  }
  return false;
}

/**
 * Родитель, переданный явно: `state.from` (переход внутри приложения) или
 * `?from=/…` (ссылка, которую можно переслать). Берём только внутренние пути.
 */
function requestedParent(location: BackLocation): string | null {
  // Только внутренний путь — одно правило на всех (`isInnerPath`), включая
  // корень «/». Раньше здесь стояла своя копия регулярки, требовавшая символ
  // ПОСЛЕ слэша, и главная как дом молча отбрасывалась.
  const internal = (v: unknown): v is string => typeof v === "string" && isInnerPath(v);
  const fromState = (location.state as { from?: unknown } | null)?.from;
  if (internal(fromState)) return fromState;
  const fromQuery = new URLSearchParams(location.search).get("from");
  if (internal(fromQuery)) return fromQuery;
  return null;
}

/** Куда возвращаться, если истории нет: логический родитель экрана. */
export function backFallback(location: BackLocation): string {
  const requested = requestedParent(location);
  if (requested) return requested;
  const p = location.pathname;
  if (p === "/start") return location.search ? "/start" : "/";
  if (p === "/settings" && new URLSearchParams(location.search).has("section")) return "/settings";
  // Терминал SSH открыт из центра серверов — туда и возвращаемся.
  if (/^\/pty\/./.test(p)) {
    return new URLSearchParams(location.search).get("ssh") === "1" ? "/ssh" : "/pty";
  }
  if (p === "/ssh-files") return "/ssh";
  if (/^\/ssh\/./.test(p)) return "/ssh";
  if (p === "/ssh") return "/";
  if (p === "/agents/sessions" || p === "/hermes") return "/agents";
  const research = p.match(/^\/research\/(.+)$/);
  if (research) return `/session/${research[1]}`;
  // «Агенты» открывают из «Моих компьютеров» (облако) и из «Системы» (прямое
  // подключение) — родитель зависит от режима. `/usage` — прежний адрес этого
  // же раздела, он остался алиасом.
  if (p === "/agents" || p === "/usage") return getMode() === "cloud" ? "/infrastructure" : "/system";
  if (p === "/support") return "/settings";
  return "/";
}

/** Есть ли в истории наш собственный предыдущий экран. */
export function canStepBack(location: BackLocation): boolean {
  return !!location.key && location.key !== "default";
}

/** Общее действие «Назад» — то же самое для экранной «←» и для системной. */
export function goBack(navigate: NavigateFunction, location: BackLocation) {
  // A direct link to a settings section has no preceding index entry.
  // Replacing it creates an index with a new router key, but still no
  // previous app page. Remember that boundary so a second Back goes home.
  if (location.pathname === "/settings" && (location.state as { settingsIndexRoot?: boolean } | null)?.settingsIndexRoot) {
    navigate("/", { replace: true });
    return;
  }
  if (stepsThroughHistory(location.pathname) && canStepBack(location)) {
    navigate(-1);
    return;
  }
  const fallback = backFallback(location);
  if (location.pathname === "/settings" && fallback === "/settings") {
    navigate(fallback, { replace: true, state: { settingsIndexRoot: true } });
    return;
  }
  navigate(fallback);
}

/**
 * Хук для экранной «←». Возвращает обработчик, который ведёт туда же, куда
 * системная кнопка «Назад» на этом же экране.
 */
export function useGoBack(): () => void {
  const navigate = useNavigate();
  const location = useLocation();
  return useCallback(() => { goBack(navigate, location); }, [navigate, location]);
}
