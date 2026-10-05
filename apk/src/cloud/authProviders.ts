/** Список способов входа с релея: GET /v1/auth/providers.
 * Клиент НЕ хардкодит кнопки: регион (ru/global) и настроенные креды решают,
 * что показать. Кэшируем в памяти на сессию, при смене сервера сбрасываем. */
import { isNetworkFailure } from "@tgcontrol/shared";
import { getRelayBase } from "../config";

export interface AuthProvider {
  id: string; // 'telegram' | 'email' | 'vk' | 'yandex' | 'google' | 'apple'
  kind: string; // 'deeplink' | 'email_code' | 'oauth'
  label: string;
}

interface ProvidersResp {
  region: string;
  providers: AuthProvider[];
}

let cache: { base: string; region: string; providers: AuthProvider[] } | null = null;

/** Способы входа, которые есть всегда — фолбэк для старого релея. */
function fallback(base: string): { base: string; region: string; providers: AuthProvider[] } {
  return { base, region: "ru", providers: [{ id: "telegram", kind: "deeplink", label: "Telegram" }] };
}

export async function getAuthProviders(): Promise<{ region: string; providers: AuthProvider[] }> {
  const base = getRelayBase().replace(/\/+$/, "");
  if (cache && cache.base === base) return cache;
  try {
    const r = await fetch(base + "/v1/auth/providers");
    if (!r.ok) throw new Error(String(r.status));
    const j = (await r.json()) as ProvidersResp;
    cache = { base, region: j.region || "ru", providers: j.providers || [] };
  } catch (e) {
    // Сеть мигнула (VPN рвёт TLS к релею) — фолбэк НЕ кэшируем: иначе список
    // способов входа оставался бы урезанным до перезапуска приложения даже
    // после того, как связь вернулась. Кэшируем только отказ самого релея:
    // старый релей без эндпоинта не «починится» в этой сессии.
    if (isNetworkFailure(e)) return fallback(base);
    cache = fallback(base);
  }
  return cache;
}

export function resetAuthProvidersCache(): void {
  cache = null;
}
