import { test as base, expect, request as pwRequest, APIRequestContext } from "@playwright/test";
import { readFileSync, writeFileSync, existsSync } from "node:fs";
import { resolve } from "node:path";
import { tmpdir } from "node:os";

type AuthData = {
  apiToken: string;
  accessToken: string;
  refreshToken: string;
};

// Кэш рабочего токена на машине прогона: без него КАЖДЫЙ тест сперва платит
// заведомо неверную попытку логина (репозиторный config.json против
// установленного агента на :8080), а /api/auth/login/token режется
// rate-limit'ом (429) уже после нескольких тестов сьютa.
function tokenCachePath(baseURL: string): string {
  const key = baseURL.replace(/[^a-z0-9]+/gi, "_");
  return resolve(tmpdir(), `tgcontrol-e2e-token-${key}.txt`);
}

// Loads candidate API tokens: config.json next to the repo exe first, then
// the installed layout (%LOCALAPPDATA%\Remotai\config.json) — на этой машине
// на :8080 слушает установленный Remotai, а не репозиторный бинарь, и его
// токен другой. Login пробует кандидатов по очереди (см. auth-фикстуру);
// токен, залогинившийся в прошлый раз, идёт первым.
function loadApiTokens(baseURL: string): string[] {
  const fromEnv = process.env.TGCONTROL_API_TOKEN;
  if (fromEnv) return [fromEnv];

  const candidates = [
    resolve(process.cwd(), "../config.json"),
    resolve(process.cwd(), "config.json"),
    resolve(process.env.LOCALAPPDATA || "", "Remotai", "config.json"),
  ];
  const tokens: string[] = [];
  for (const path of candidates) {
    if (!existsSync(path)) continue;
    const cfg = JSON.parse(readFileSync(path, "utf8"));
    if (cfg.api_token && !tokens.includes(cfg.api_token)) tokens.push(cfg.api_token);
  }
  if (tokens.length === 0) {
    throw new Error(
      "API token not found. Set TGCONTROL_API_TOKEN env var or run from a directory next to config.json."
    );
  }
  try {
    const cached = readFileSync(tokenCachePath(baseURL), "utf8").trim();
    const i = tokens.indexOf(cached);
    if (i > 0) tokens.unshift(...tokens.splice(i, 1));
  } catch {
    /* кэша ещё нет */
  }
  return tokens;
}

function rememberToken(baseURL: string, apiToken: string) {
  try {
    writeFileSync(tokenCachePath(baseURL), apiToken, "utf8");
  } catch {
    /* best-effort */
  }
}

async function login(api: APIRequestContext, baseURL: string, apiToken: string) {
  const res = await api.post(`${baseURL}/api/auth/login/token`, {
    data: { api_token: apiToken },
  });
  if (!res.ok()) {
    const err = new Error(`Login failed (${res.status()}): ${await res.text()}`) as Error & {
      status?: number;
    };
    err.status = res.status();
    throw err;
  }
  const body = await res.json();
  return {
    accessToken: body.access_token,
    refreshToken: body.refresh_token,
  };
}

export const test = base.extend<{
  auth: AuthData;
  api: APIRequestContext;
  agentPlatform: string;
}>({
  auth: async ({ baseURL }, use) => {
    if (!baseURL) throw new Error("baseURL required");
    // Кандидатов может быть несколько (репозиторный и установленный агент
    // слушают один порт попеременно) — принимаем первый, кто залогинился.
    // 429 (rate-limit на логин) не перебирает кандидатов: ждём и повторяем
    // того же — смена токена лимит не снимает.
    const candidates = loadApiTokens(baseURL);
    let lastErr: unknown = null;
    for (const apiToken of candidates) {
      for (let attempt = 0; attempt < 4; attempt++) {
        const ctx = await pwRequest.newContext();
        try {
          const { accessToken, refreshToken } = await login(ctx, baseURL, apiToken);
          rememberToken(baseURL, apiToken);
          await use({ apiToken, accessToken, refreshToken });
          return;
        } catch (e) {
          lastErr = e;
          if ((e as { status?: number }).status !== 429) break;
          await new Promise((r) => setTimeout(r, 5_000 * (attempt + 1)));
        } finally {
          await ctx.dispose();
        }
      }
    }
    throw lastErr;
  },

  // Платформа АГЕНТА (может отличаться от машины, где гоняют Playwright —
  // напр. кросс-проверка Linux-агента с Windows-хоста через TGCONTROL_URL).
  agentPlatform: async ({ baseURL }, use) => {
    const ctx = await pwRequest.newContext();
    const res = await ctx.get(`${baseURL}/api/setup/status`);
    const body = res.ok() ? await res.json() : {};
    await ctx.dispose();
    await use(typeof body.platform === "string" ? body.platform : process.platform);
  },

  api: async ({ baseURL, auth }, use) => {
    const ctx = await pwRequest.newContext({
      baseURL,
      extraHTTPHeaders: {
        Authorization: `Bearer ${auth.accessToken}`,
      },
    });
    await use(ctx);
    await ctx.dispose();
  },

  page: async ({ page, baseURL, auth }, use) => {
    // Inject JWT into localStorage before any app code runs so the React app
    // picks it up on first render.
    // tgcontrol_server/remotai.local.token повторяют то, что делает окно exe
    // при открытии /miniapp?token=…: без них RequireAuth уводит на /cloud-login
    // (см. hasServerConfig в apk/src/config.ts).
    await page.addInitScript((seed) => {
      localStorage.setItem("tg_access_token", seed.access);
      localStorage.setItem("tg_refresh_token", seed.refresh);
      localStorage.setItem(
        "tgcontrol_server",
        JSON.stringify({ url: seed.origin, token: seed.apiToken, mode: "self_hosted" }),
      );
      localStorage.setItem("remotai.local.token", seed.apiToken);
    }, {
      access: auth.accessToken,
      refresh: auth.refreshToken,
      apiToken: auth.apiToken,
      origin: new URL(baseURL).origin,
    });
    await use(page);
  },
});

export { expect };
