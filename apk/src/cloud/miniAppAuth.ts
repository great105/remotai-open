/**
 * Telegram Mini App: обмен initData на долгий user-JWT.
 *
 * ЗАЧЕМ. Релей принимает `tma <initData>` только 24 часа с момента запуска
 * мини-аппа (`ParseInitData(…, 24h)` в middleware.go), а Telegram держит
 * WebView открытым сутками и initData в нём не обновляет. На вторые сутки
 * каждый запрос и каждый коннект служебного сокета получали 401: боевой лог
 * релея 01.09.2026 — 216 отказов на `/v1/client/<dev>/ws` за сутки, пачками
 * по три с паузой в четыре минуты (лестница переподключений), с телефона
 * владельца. Снаружи это «мини-апп крутится и не подключается», пока его не
 * закроешь и не откроешь заново из бота.
 *
 * КАК. `POST /v1/auth/tg/miniapp` на релее уже умел выдавать durable-JWT по
 * initData (год, продлевается скользяще) — клиент его просто не звал. Теперь
 * при каждом запуске мини-аппа со свежим initData меняем его на JWT и храним
 * там же, где хранит вход APK (`saveConfig({jwt})`). Дальше все вызовы к
 * релею — REST, служебный сокет, стрим терминала — идут с JWT
 * (cloudAuthHeader / cloudStreamAuthQuery / accountAuthHeader предпочитают
 * его, если он есть). Протухший initData не мешает: обмен по нему честно
 * откажет, а прежний JWT останется в силе.
 *
 * Вне Telegram функция ничего не делает.
 */
import { getTelegram } from "../telegram";
import { getCloudJWT, getRelayBase, saveConfig } from "../config";
import { tlog } from "../debuglog";

let inflight: Promise<boolean> | null = null;

/** Получить/обновить JWT мини-аппа. true — JWT есть (старый или свежий). */
export function ensureMiniAppJWT(): Promise<boolean> {
  const tg = getTelegram();
  if (!tg?.initData) return Promise.resolve(false);
  if (inflight) return inflight;
  inflight = exchange(tg.initData).finally(() => { inflight = null; });
  return inflight;
}

async function exchange(initData: string): Promise<boolean> {
  const base = getRelayBase().replace(/\/+$/, "");
  if (!base) return !!getCloudJWT();
  try {
    const res = await fetch(base + "/v1/auth/tg/miniapp", {
      method: "POST",
      headers: { Authorization: `tma ${initData}`, "X-Remotai-Client": "telegram" },
    });
    if (!res.ok) {
      // 401 здесь — протухший initData: релей не даст по нему JWT, но прежний
      // JWT (если был) по-прежнему годен. Иначе — сеть; тоже не приговор.
      tlog("miniapp-jwt:refused", { status: res.status, hadJwt: !!getCloudJWT() });
      return !!getCloudJWT();
    }
    const body = (await res.json()) as { jwt?: string; expires_at?: string };
    if (!body.jwt) return !!getCloudJWT();
    saveConfig({ jwt: body.jwt, jwtExpiresAt: (body.expires_at && Date.parse(body.expires_at)) || undefined });
    tlog("miniapp-jwt:ok", { exp: body.expires_at ?? "" });
    return true;
  } catch (e) {
    tlog("miniapp-jwt:error", { message: e instanceof Error ? e.message : String(e) });
    return !!getCloudJWT();
  }
}
