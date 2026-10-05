/** Вход через Telegram для НЕ-Mini-App клиентов (APK / web / окно exe).
 *
 * Поток (SOTA, кросс-платформенный): POST /v1/auth/tg/start → одноразовый nonce
 * + deep-link `t.me/<bot>?start=login_<nonce>`. Открываем бота, пользователь жмёт
 * Start, бот привязывает nonce к telegram_id. Клиент поллит /v1/auth/tg/poll и
 * получает durable user-JWT. Текущий анонимный JWT (если был) шлём — релей сольёт
 * анонимный аккаунт в Telegram-аккаунт, сохранив уже привязанные ПК.
 *
 * ВАЖНО (мобилки): уход в Telegram выгружает/замораживает WebView и убивает
 * in-memory цикл опроса. Поэтому pending-токен сохраняется в localStorage, а
 * `resumePendingTelegramLogin()` до-опрашивает при возврате/старте приложения —
 * вход завершается, даже если страница перезагрузилась. */
import { fetchOrNetworkError, setNetworkContext } from "@tgcontrol/shared";
import { getRelayBase, getCloudJWT, saveConfig } from "../config";
import { openExternalLink } from "../openExternal";
import { CloudError } from "./api";
import { tlog } from "../debuglog";

interface StartResp {
  login_token: string;
  deep_link: string;
  bot_username: string;
  expires_at: string;
}

interface PollResp {
  confirmed: boolean;
  expired: boolean;
  jwt?: string;
  expires_at?: string;
  telegram_id?: number;
  username?: string;
}

export interface TgLoginResult {
  ok: boolean;
  telegramId?: number;
  username?: string;
  reason?: "expired" | "timeout" | "cancelled";
}

const PENDING_KEY = "remotai.tglogin.pending";

interface Pending {
  token: string;
  base: string;
  deadline: number;
}

function setPending(p: Pending | null): void {
  try {
    if (p) localStorage.setItem(PENDING_KEY, JSON.stringify(p));
    else localStorage.removeItem(PENDING_KEY);
  } catch { /* ignore */ }
}

function getPending(): Pending | null {
  try {
    const raw = localStorage.getItem(PENDING_KEY);
    return raw ? (JSON.parse(raw) as Pending) : null;
  } catch {
    return null;
  }
}

/**
 * Открыть бота на текущей поверхности — общий помощник (openExternal.ts).
 *
 * Своя копия здесь заканчивалась фолбэком `location.href = url`, и в окне на ПК
 * (WebView2 без обработчика новых окон) это уводило САМО ПРИЛОЖЕНИЕ на страницу
 * Telegram: вход человек подтверждал, а вернуться было некуда — кнопки «назад» в
 * WebView2 нет. Теперь там ссылку открывает агент системным браузером, а окно
 * остаётся на экране входа и досматривает опрос до конца.
 */
function openExternal(url: string): void {
  void openExternalLink(url);
}

const sleep = (ms: number) => new Promise<void>((r) => setTimeout(r, ms));

/** Один опрос статуса. Возвращает результат или "pending". При успехе сохраняет JWT. */
async function pollOnce(base: string, token: string): Promise<TgLoginResult | "pending"> {
  const headers: Record<string, string> = {};
  const anonJWT = getCloudJWT(); // для слияния анонимного аккаунта
  if (anonJWT) headers["Authorization"] = `Bearer ${anonJWT}`;
  const r = await fetchOrNetworkError(base + "/v1/auth/tg/poll?login_token=" + encodeURIComponent(token), { headers }, "cloud");
  const p = (await r.json()) as PollResp;
  if (p.expired) return { ok: false, reason: "expired" };
  if (p.confirmed && p.jwt) {
    setNetworkContext({ route: "cloud" }); // дальше приложение работает через облако
    saveConfig({
      mode: "cloud",
      relayBase: base,
      jwt: p.jwt,
      jwtExpiresAt: (p.expires_at && Date.parse(p.expires_at)) || undefined,
    });
    tlog("tg-login:confirmed", { tg: p.telegram_id });
    return { ok: true, telegramId: p.telegram_id, username: p.username };
  }
  return "pending";
}

export interface TgLoginHandle {
  deepLink: string;
  /** Резолвится при подтверждении (foreground-кейс) либо истечении/отмене.
   *  На мобилке завершение страхует resumePendingTelegramLogin() при возврате. */
  done: Promise<TgLoginResult>;
  cancel: () => void;
}

export interface TgLoginOptions {
  /**
   * Открывать ли бота самим. На телефоне — да, это и есть весь вход. На
   * большом экране открывать нечего: Telegram там может быть не установлен и
   * не залогинен, поэтому ссылку показывают QR-кодом, а подтверждают с
   * телефона. Опрос в обоих случаях идёт одинаково.
   */
  open?: boolean;
}

/** Запускает вход через Telegram. */
export async function startTelegramLogin(options: TgLoginOptions = {}): Promise<TgLoginHandle> {
  const base = getRelayBase().replace(/\/+$/, "");
  // Сетевой сбой (VPN рвёт TLS к релею, пропал интернет) обязан прийти
  // типизированным: на экране входа сырое «Failed to fetch» было первым, что
  // человек видел о продукте (UX-аудит N63).
  const startRes = await fetchOrNetworkError(base + "/v1/auth/tg/start", { method: "POST" }, "cloud");
  if (!startRes.ok) {
    throw new CloudError(startRes.status, "Не удалось начать вход через Telegram");
  }
  const s = (await startRes.json()) as StartResp;
  tlog("tg-login:start", { bot: s.bot_username });

  const deadline = Date.now() + 5 * 60 * 1000; // = TTL nonce на релее
  setPending({ token: s.login_token, base, deadline }); // переживёт выгрузку WebView
  if (options.open !== false) openExternal(s.deep_link);

  let cancelled = false;
  const done: Promise<TgLoginResult> = (async () => {
    while (!cancelled && Date.now() < deadline) {
      await sleep(2000);
      if (cancelled) break;
      let res: TgLoginResult | "pending";
      try {
        res = await pollOnce(base, s.login_token);
      } catch {
        continue; // сеть моргнула — продолжаем опрос
      }
      if (res === "pending") continue;
      setPending(null);
      return res;
    }
    return { ok: false, reason: cancelled ? "cancelled" : "timeout" };
  })();

  return { deepLink: s.deep_link, done, cancel: () => { cancelled = true; setPending(null); } };
}

/** Есть незавершённый вход через Telegram? */
export function hasPendingTelegramLogin(): boolean {
  const p = getPending();
  return !!p && Date.now() <= p.deadline;
}

/** До-опрашивает незавершённый вход (вызывать при старте/возврате приложения).
 *  true = вход завершён и JWT сохранён. Чистит просроченный pending. */
export async function resumePendingTelegramLogin(): Promise<boolean> {
  const p = getPending();
  if (!p) return false;
  if (Date.now() > p.deadline) {
    setPending(null);
    return false;
  }
  try {
    const res = await pollOnce(p.base, p.token);
    if (res === "pending") return false;
    setPending(null);
    return res.ok;
  } catch {
    return false; // попробуем при следующем возврате
  }
}

/**
 * Подтвердить вход на большом экране прямо отсюда — без похода в Telegram.
 *
 * Человек с приложением в руках наводит на QR НАШ сканер: он уже вошёл здесь,
 * аккаунт тот самый, и гонять его через бота ради подтверждения того, что мы и
 * так знаем, — лишний круг (живой случай 23.08: сканер ответил «Это не код
 * Remotai» на код, который Remotai сам и показал).
 *
 * Возвращает false, если релей ещё не знает эту дверь (старая версия на бою) —
 * тогда вызывающий открывает Telegram, как раньше. Настоящие отказы (код
 * устарел, чужой код) бросаются наружу: их человеку надо показать.
 */
export async function approveQrLogin(loginToken: string): Promise<boolean> {
  const base = getRelayBase().replace(/\/+$/, "");
  const jwt = getCloudJWT();
  if (!jwt) return false; // на этом телефоне ещё нет аккаунта — подтверждать нечем
  const res = await fetchOrNetworkError(base + "/v1/auth/qr/approve", {
    method: "POST",
    headers: { "Content-Type": "application/json", Authorization: `Bearer ${jwt}` },
    body: JSON.stringify({ login_token: loginToken }),
  }, "cloud");
  if (res.ok) {
    tlog("qr-approve:ok", {});
    return true;
  }
  // 404/405 без машинного кода = релей старой версии: двери просто нет.
  if (res.status === 404 || res.status === 405) {
    let code = "";
    try {
      code = ((await res.clone().json()) as { code?: string })?.code || "";
    } catch { /* тела нет — тем более старый релей */ }
    if (code !== "login_not_found") {
      tlog("qr-approve:unsupported", { status: res.status });
      return false;
    }
  }
  let message = "Не удалось подтвердить вход";
  let code: string | undefined;
  try {
    const body = (await res.json()) as { error?: string; code?: string };
    message = body?.error || message;
    code = body?.code;
  } catch { /* нечитаемое тело — оставляем общий текст */ }
  throw new CloudError(res.status, message, code);
}
