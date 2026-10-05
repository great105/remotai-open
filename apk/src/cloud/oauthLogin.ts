import { t } from "@tgcontrol/shared";
/** OAuth-вход через redirect-флоу релея (VK ID / Яндекс ID / Google).
 *
 * Поток: POST /v1/auth/oauth/{provider}/begin → {state, authorize_url}.
 * Открываем authorize_url в браузере, пользователь входит у провайдера,
 * провайдер возвращает браузер на callback релея, релей привязывает state к
 * аккаунту. Клиент поллит /v1/auth/oauth/poll и получает durable user-JWT.
 * Тот же паттерн, что у Telegram-входа; pending-state переживает выгрузку
 * WebView (localStorage), до-опрос при возврате в приложение. */
import { fetchOrNetworkError, setNetworkContext } from "@tgcontrol/shared";
import { getRelayBase, getCloudJWT, saveConfig } from "../config";
import { openExternalLink } from "../openExternal";
import { getTelegram } from "../telegram";
import { CloudError } from "./api";
import { tlog } from "../debuglog";

interface BeginResp {
  state: string;
  authorize_url: string;
  expires_at: string;
}

interface PollResp {
  confirmed: boolean;
  expired: boolean;
  jwt?: string;
  expires_at?: string;
  linked?: boolean;
  provider?: string;
}

export interface OAuthLoginResult {
  ok: boolean;
  provider?: string;
  reason?: "expired" | "timeout" | "cancelled";
}

const PENDING_KEY = "remotai.oauthlogin.pending";

interface Pending {
  state: string;
  provider: string;
  base: string;
  deadline: number;
  link?: boolean;
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
 * Та же дверь наружу, что у входа через Telegram (openExternal.ts): в окне на
 * ПК страницу провайдера открывает агент системным браузером. Своя копия с
 * фолбэком `location.href` уводила само приложение на сайт VK/Яндекса, откуда в
 * WebView2 не вернуться — вход подтверждался, а окно оставалось на чужой
 * странице.
 */
function openExternal(url: string): void {
  void openExternalLink(url);
}

const sleep = (ms: number) => new Promise<void>((r) => setTimeout(r, ms));

async function pollOnce(base: string, state: string, provider: string, link = false): Promise<OAuthLoginResult | "pending"> {
  const headers: Record<string, string> = {};
  const anonJWT = getCloudJWT(); // для слияния анонимного аккаунта
  if (anonJWT) headers["Authorization"] = `Bearer ${anonJWT}`;
  const r = await fetchOrNetworkError(base + "/v1/auth/oauth/poll?state=" + encodeURIComponent(state), { headers }, "cloud");
  const p = (await r.json()) as PollResp;
  if (p.expired) return { ok: false, reason: "expired" };
  if (p.confirmed && p.linked) return { ok: true, provider: p.provider || provider };
  if (p.confirmed && p.jwt) {
    if (link) throw new CloudError(409, t("ui.oauthlogin.mccd275c51a"));
    setNetworkContext({ route: "cloud" }); // дальше приложение работает через облако
    saveConfig({
      mode: "cloud",
      relayBase: base,
      jwt: p.jwt,
      jwtExpiresAt: (p.expires_at && Date.parse(p.expires_at)) || undefined,
    });
    tlog("oauth-login:confirmed", { provider });
    return { ok: true, provider };
  }
  return "pending";
}

export interface OAuthLoginHandle {
  done: Promise<OAuthLoginResult>;
  cancel: () => void;
}

/** Запускает OAuth-вход у провайдера ('vk' | 'yandex' | 'google'). */
export async function startOAuthLogin(provider: string, options: { link?: boolean } = {}): Promise<OAuthLoginHandle> {
  const base = getRelayBase().replace(/\/+$/, "");
  const link = options.link === true;
  const headers: Record<string, string> = {};
  if (link) {
    const initData = getTelegram()?.initData;
    if (initData) headers.Authorization = `tma ${initData}`;
    else if (getCloudJWT()) headers.Authorization = `Bearer ${getCloudJWT()}`;
  }
  // Типизированный сетевой сбой: на экране входа сырое «Failed to fetch»
  // недопустимо (UX-аудит N63).
  const r = await fetchOrNetworkError(base + "/v1/auth/oauth/" + encodeURIComponent(provider) + `/begin${link ? "?link=1" : ""}`, {
    method: "POST",
    headers,
  }, "cloud");
  if (!r.ok) {
    throw new CloudError(r.status, t("ui.oauthlogin.m53cd78b5dd"));
  }
  const b = (await r.json()) as BeginResp;
  tlog("oauth-login:begin", { provider });

  const deadline = Date.now() + 10 * 60 * 1000; // = TTL state на релее
  setPending({ state: b.state, provider, base, deadline, link });
  openExternal(b.authorize_url);

  let cancelled = false;
  const done: Promise<OAuthLoginResult> = (async () => {
    while (!cancelled && Date.now() < deadline) {
      await sleep(2000);
      if (cancelled) break;
      let res: OAuthLoginResult | "pending";
      try {
        res = await pollOnce(base, b.state, provider, link);
      } catch {
        continue; // сеть моргнула — продолжаем опрос
      }
      if (res === "pending") continue;
      setPending(null);
      return res;
    }
    return { ok: false, reason: cancelled ? "cancelled" : "timeout" };
  })();

  return { done, cancel: () => { cancelled = true; setPending(null); } };
}

/** Есть незавершённый OAuth-вход? */
export function hasPendingOAuthLogin(): boolean {
  const p = getPending();
  return !!p && Date.now() <= p.deadline;
}

/** До-опрашивает незавершённый вход при старте/возврате приложения.
 *  true = вход завершён и JWT сохранён. */
export async function resumePendingOAuthLogin(): Promise<boolean> {
  const p = getPending();
  if (!p) return false;
  if (Date.now() > p.deadline) {
    setPending(null);
    return false;
  }
  try {
    const res = await pollOnce(p.base, p.state, p.provider, p.link === true);
    if (res === "pending") return false;
    setPending(null);
    return res.ok;
  } catch {
    return false; // попробуем при следующем возврате
  }
}
