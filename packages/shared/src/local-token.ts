/** Локальный auth-мост для браузерного входа в встроенный клиент exe.
 *
 * Сервер принимает `initData = "token:<API_TOKEN>"` (HTTP-заголовок, query и
 * WS). Панель управления / трей открывают `/miniapp?token=<API_TOKEN>` —
 * здесь токен забирается из URL, прячется в localStorage и убирается из
 * адресной строки, чтобы не светился и не попадал в историю/скриншоты.
 *
 * Используется miniapp-фронтом как фолбэк, когда Telegram initData нет
 * (обычный браузер: окно exe, другой компьютер в той же сети).
 */

const STORAGE_KEY = "remotai.local.token";

/** Возвращает "token:<API_TOKEN>" для auth-слоёв или "" если токена нет. */
export function getLocalTokenInitData(): string {
  if (typeof window === "undefined") return "";
  try {
    const url = new URL(window.location.href);
    const fromQuery = url.searchParams.get("token");
    if (fromQuery) {
      window.localStorage.setItem(STORAGE_KEY, fromQuery);
      url.searchParams.delete("token");
      window.history.replaceState(null, "", url.pathname + url.search + url.hash);
    }
    const token = window.localStorage.getItem(STORAGE_KEY);
    return token ? `token:${token}` : "";
  } catch {
    return "";
  }
}
