// Cloud pairing: «выпуск кода → подтверждение → устройство в аккаунте».
//
// Гоняется против ЛОКАЛЬНОГО релея (tgcontrol-relay/cmd/e2e-standalone) —
// полный HTTP API без Telegram-бота. BOT_TOKEN тестовый, поэтому initData
// подписываем сами (fixtures/harness.ts → buildInitData). Покрытые грабли
// из истории релизов: «такого кода нет» обязано быть понятной ошибкой
// (машинный код pair_code_not_found + русский текст), а не голым 404.
import { test, expect, request as pwRequest, APIRequestContext } from "@playwright/test";
import { resolve } from "node:path";
import { tmpdir } from "node:os";
import { rmSync } from "node:fs";
import {
  REPO_ROOT,
  ReadyProcess,
  buildGoBinary,
  buildInitData,
  spawnReady,
  stopChild,
} from "../fixtures/harness";

const BOT_TOKEN = "123456:E2E_FAKE_BOT_TOKEN_FOR_PAIRING";
const TG_USER_ID = 777001;

// Сборка релея в beforeAll может занять больше минуты на холодном кэше Go.
test.describe.configure({ timeout: 240_000 });

test.describe("Cloud pairing (локальный релей)", () => {
  let relay: ReadyProcess | null = null;
  let dbPath = "";
  let api: APIRequestContext;

  const tma = (uid: number, username: string) => ({
    Authorization: `tma ${buildInitData(BOT_TOKEN, uid, username)}`,
  });

  test.beforeAll(async () => {
    const binary = await buildGoBinary(
      resolve(REPO_ROOT, "tgcontrol-relay"),
      "./cmd/e2e-standalone",
      "relay-harness",
    );
    dbPath = resolve(tmpdir(), `relay-e2e-${process.pid}.db`);
    relay = await spawnReady(binary, {
      env: {
        PORT: "0",
        DB_PATH: dbPath,
        JWT_HMAC_SECRET: "e2e-relay-secret-0123456789abcdef",
        BOT_TOKEN,
        BOT_USERNAME: "E2ERelayBot",
        NOTIFY_TG_ENABLED: "false",
        CORS_ORIGINS: "*",
      },
    });
    api = await pwRequest.newContext({ baseURL: relay.baseURL });
  });

  test.afterAll(async () => {
    await api?.dispose();
    stopChild(relay);
    try {
      rmSync(dbPath, { force: true });
    } catch {
      /* best-effort */
    }
  });

  test("выпуск кода → привязка → устройство появляется в аккаунте", async () => {
    // 1. Десктоп выпускает pairing-код.
    const reqRes = await api.post("/v1/pair/request", {
      data: {
        device_id: "e2e-dev-pairing",
        hostname: "E2E-HOST",
        platform: "linux",
        agent_version: "e2e",
      },
    });
    expect(reqRes.ok(), `pair/request: ${await reqRes.text()}`).toBeTruthy();
    const issued = await reqRes.json();
    expect(issued.code, "код вида XXXX-XXXX").toMatch(/^[A-Z0-9]{4}-[A-Z0-9]{4}$/);
    expect(issued.expires_at).toBeTruthy();
    expect(issued.bot_link).toContain("E2ERelayBot");

    // 2. До подтверждения статус — «ждём».
    const pending = await api.get(`/v1/pair/status?code=${encodeURIComponent(issued.code)}`);
    expect(pending.ok()).toBeTruthy();
    expect((await pending.json()).confirmed).toBe(false);

    // 3. Пользователь подтверждает код (Mini App, initData-авторизация).
    const confirm = await api.post("/v1/pair/confirm", {
      headers: tma(TG_USER_ID, "e2e_tester"),
      data: { code: issued.code },
    });
    expect(confirm.ok(), `pair/confirm: ${await confirm.text()}`).toBeTruthy();
    const confirmed = await confirm.json();
    expect(confirmed.ok).toBe(true);
    expect(confirmed.device_id).toBe("e2e-dev-pairing");

    // 4. Десктоп опрашивает status и забирает device-JWT.
    const status = await api.get(`/v1/pair/status?code=${encodeURIComponent(issued.code)}`);
    const st = await status.json();
    expect(st.confirmed, "status обязан отдать подтверждение").toBe(true);
    expect(st.jwt, "status обязан отдать device-JWT").toBeTruthy();
    expect(st.device_id).toBe("e2e-dev-pairing");

    // 5. Устройство появилось в списке пользователя.
    const devices = await api.get("/v1/devices", {
      headers: tma(TG_USER_ID, "e2e_tester"),
    });
    expect(devices.ok(), `devices: ${await devices.text()}`).toBeTruthy();
    const list: Array<{ id?: string; name?: string; hostname?: string }> =
      (await devices.json()).devices || [];
    const found = list.find((d) => d.id === "e2e-dev-pairing");
    expect(found, "привязанное устройство должно быть в списке").toBeTruthy();
    expect(found!.name).toBe("E2E-HOST");
  });

  test("неверный код → 404 с машинным кодом pair_code_not_found", async () => {
    const res = await api.post("/v1/pair/confirm", {
      headers: tma(TG_USER_ID + 1, "e2e_wrong_code"),
      data: { code: "ZZZZ-ZZZZ" },
    });
    expect(res.status()).toBe(404);
    const body = await res.json();
    expect(body.code).toBe("pair_code_not_found");
    // Текст обязан объяснять, что делать, а не быть голым «Не найдено.»
    expect(body.error).toContain("кода подключения нет");

    // Без авторизации подтвердить нельзя вообще.
    const noauth = await api.post("/v1/pair/confirm", { data: { code: "ZZZZ-ZZZZ" } });
    expect(noauth.status()).toBe(401);

    // Статус несуществующего кода — тоже понятный «нет такого».
    const st = await api.get("/v1/pair/status?code=ZZZZ-ZZZZ");
    expect(st.status()).toBe(404);
    expect((await st.json()).error).toBe("not_found");
  });
});
