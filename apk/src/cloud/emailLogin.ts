import { t } from "@tgcontrol/shared";
/** Вход по email-коду: POST /v1/auth/email/start → письмо с 6 цифрами →
 * POST /v1/auth/email/verify → durable user-JWT. Синхронно, без опроса.
 * Текущий анонимный JWT шлём в verify: релей сольёт анонимный аккаунт,
 * привязанные ПК сохранятся. */
import { fetchOrNetworkError, setNetworkContext } from "@tgcontrol/shared";
import { getRelayBase, getCloudJWT, saveConfig } from "../config";
import { CloudError } from "./api";
import { tlog } from "../debuglog";

export interface EmailLoginStart {
  loginToken: string;
  expiresAt: string;
}

export async function startEmailLogin(email: string): Promise<EmailLoginStart> {
  const base = getRelayBase().replace(/\/+$/, "");
  // «cloud» — сетевой сбой здесь означает «нет связи с сервисом», а не
  // «компьютер не отвечает» (UX-аудит N63: на входе показывалось «Failed to fetch»).
  const r = await fetchOrNetworkError(base + "/v1/auth/email/start", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ email }),
  }, "cloud");
  if (!r.ok) {
    const msg =
      r.status === 429
        ? t("ui.emaillogin.mb2e135e36f")
        : r.status === 400
          ? t("ui.emaillogin.mb4cf00c8ae")
          : t("ui.emaillogin.m29ba974a87");
    throw new CloudError(r.status, msg);
  }
  const j = (await r.json()) as { login_token: string; expires_at: string };
  tlog("email-login:start", { email });
  return { loginToken: j.login_token, expiresAt: j.expires_at };
}

export async function verifyEmailCode(loginToken: string, code: string): Promise<void> {
  const base = getRelayBase().replace(/\/+$/, "");
  const headers: Record<string, string> = { "Content-Type": "application/json" };
  const anonJWT = getCloudJWT(); // для слияния анонимного аккаунта
  if (anonJWT) headers["Authorization"] = `Bearer ${anonJWT}`;
  const r = await fetchOrNetworkError(base + "/v1/auth/email/verify", {
    method: "POST",
    headers,
    body: JSON.stringify({ login_token: loginToken, code }),
  }, "cloud");
  if (!r.ok) {
    throw new CloudError(r.status, r.status === 401 ? t("ui.emaillogin.mf5efba364e") : t("ui.emaillogin.m3f7a92f22c"));
  }
  const j = (await r.json()) as { jwt: string; expires_at?: string };
  setNetworkContext({ route: "cloud" }); // дальше приложение работает через облако
  saveConfig({
    mode: "cloud",
    relayBase: base,
    jwt: j.jwt,
    jwtExpiresAt: (j.expires_at && Date.parse(j.expires_at)) || undefined,
  });
  tlog("email-login:confirmed");
}
