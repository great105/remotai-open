// SSH-центр: список серверов и регрессия v2.42.14 — повторное «Подключиться»
// к серверу с живой сессией обязано ВЕРНУТЬ в уже открытый терминал, а не
// заводить второй такой же (findOpenSshTerminal в SshSection.tsx). Живая
// находка владельца: в группе сервера висели две одинаковые сессии.
//
// Реального SSH-сервера в тестовом окружении нет (Docker на машине не
// поднят), поэтому поднимаем минимальный sshd-хелпер (e2e/helpers/sshd):
// настоящий SSH-протокол (x/crypto/ssh), пароль, PTY+shell — агент
// подключается к нему как к обычному серверу.
import { test, expect } from "../fixtures/auth";
import { resolve } from "node:path";
import {
  REPO_ROOT,
  ReadyProcess,
  buildGoBinary,
  spawnReady,
  stopChild,
} from "../fixtures/harness";

const SSHD_USER = "e2e";
const SSHD_PASS = "e2e-secret";
const HOST_NAME = "e2e-ssh-center";

// Сборка sshd в beforeAll может занять больше минуты на холодном кэше Go.
test.describe.configure({ timeout: 240_000 });

test.describe("SSH-центр", () => {
  let sshd: ReadyProcess | null = null;

  test.beforeAll(async () => {
    const binary = await buildGoBinary(REPO_ROOT, "./e2e/helpers/sshd", "e2e-sshd");
    sshd = await spawnReady(binary, {
      args: ["-user", SSHD_USER, "-pass", SSHD_PASS],
    });
  });

  test.afterAll(async () => {
    stopChild(sshd);
  });

  // Подчистка следов предыдущего (возможно, упавшего) прогона: сохранённый
  // хост, его секрет, known_hosts-запись и живые ssh-терминалы к 127.0.0.1.
  async function cleanup(api: any, port: number) {
    const list = await api.get("/api/ssh/hosts").catch(() => null);
    if (list?.ok()) {
      const body = await list.json();
      const hosts: Array<{ id?: string; name?: string }> = body.hosts || [];
      for (const h of hosts.filter((x) => x.name === HOST_NAME)) {
        await api.delete(`/api/ssh/hosts/${h.id}/unlock`).catch(() => {});
        await api.delete(`/api/ssh/hosts/${h.id}`).catch(() => {});
      }
    }
    await api
      .delete("/api/ssh/known-hosts", { data: { host: "127.0.0.1", port } })
      .catch(() => {});
    const pty = await api.get("/api/pty").catch(() => null);
    if (pty?.ok()) {
      const body = await pty.json();
      const sessions: Array<{ id?: string; kind?: string; ssh_host?: string }> =
        body.sessions || [];
      for (const s of sessions.filter(
        (x) => x.kind === "ssh" && x.ssh_host === "127.0.0.1",
      )) {
        await api.delete(`/api/pty/${s.id}`).catch(() => {});
      }
    }
  }

  test("добавить сервер → он в списке → удалить", async ({ api }) => {
    const port = sshd!.port;
    await cleanup(api, port);

    const created = await api.post("/api/ssh/hosts", {
      data: { name: HOST_NAME, host: "127.0.0.1", port, user: SSHD_USER },
    });
    expect(created.ok(), `create host: ${await created.text()}`).toBeTruthy();
    const hostId: string = (await created.json()).host.id;
    expect(hostId).toBeTruthy();

    const list = await api.get("/api/ssh/hosts");
    expect(list.ok()).toBeTruthy();
    const hosts: Array<{ id?: string; name?: string; user?: string; port?: number }> =
      (await list.json()).hosts || [];
    const found = hosts.find((h) => h.id === hostId);
    expect(found, "сервер должен появиться в списке").toBeTruthy();
    expect(found!.user).toBe(SSHD_USER);
    expect(found!.port).toBe(port);

    await api.delete(`/api/ssh/hosts/${hostId}`);
    const after = await api.get("/api/ssh/hosts");
    const rest: Array<{ id?: string }> = (await after.json()).hosts || [];
    expect(rest.some((h) => h.id === hostId), "сервер должен удалиться").toBeFalsy();
  });

  test("повторное «Подключиться» возвращает в тот же терминал, дубликата нет", async ({
    api,
    page,
  }) => {
    const port = sshd!.port;
    await cleanup(api, port);

    // Сервер заводим через API — UI-тест про подключение, не про форму.
    const created = await api.post("/api/ssh/hosts", {
      data: { name: HOST_NAME, host: "127.0.0.1", port, user: SSHD_USER },
    });
    expect(created.ok(), `create host: ${await created.text()}`).toBeTruthy();
    const hostId: string = (await created.json()).host.id;

    const sshSessionsForHost = async () => {
      const res = await api.get("/api/pty");
      const body = await res.json();
      const sessions: Array<{
        id?: string;
        kind?: string;
        alive?: boolean;
        ssh_host_id?: string;
      }> = body.sessions || [];
      return sessions.filter((s) => s.kind === "ssh" && s.ssh_host_id === hostId);
    };

    const connectBtn = page.getByRole("button", { name: `Подключиться: ${HOST_NAME}` });
    let termId = "";
    try {
      // 1. Первое подключение из SSH-центра.
      await page.goto("/miniapp/#/ssh");
      await expect(connectBtn).toBeVisible({ timeout: 15_000 });
      await connectBtn.click();

      // Порядок диалогов зависит от машины: если на ПК есть дефолтные
      // SSH-ключи (~/.ssh/id_ed25519 и т.п.), auth_ready=true и сначала идёт
      // TOFU-диалог, а пароль спросят только после auth_failed по ключу; без
      // ключей — наоборот, пароль, потом TOFU. Обрабатываем оба порядка.
      const overlay = page.locator(".modal-overlay");
      const pwInput = overlay.locator('input[placeholder="Пароль"]');
      const trustBtn = page.getByRole("button", { name: "Доверять" });
      let passwordFilled = false;
      const dlgDeadline = Date.now() + 30_000;
      while (Date.now() < dlgDeadline && !page.url().match(/#\/pty\//)) {
        if (await pwInput.isVisible().catch(() => false)) {
          await pwInput.fill(SSHD_PASS);
          await overlay.getByRole("button", { name: "Подключить" }).click();
          passwordFilled = true;
        } else if (await trustBtn.isVisible().catch(() => false)) {
          await trustBtn.click();
        } else {
          await page.waitForTimeout(200);
        }
      }
      expect(passwordFilled, "диалог пароля должен был появиться").toBeTruthy();

      // Открылся терминал этой SSH-сессии.
      await page.waitForURL(/#\/pty\/[^/?]+/, { timeout: 20_000 });
      termId = page.url().match(/#\/pty\/([^/?]+)/)![1];
      expect(termId).toBeTruthy();

      let sshSessions = await sshSessionsForHost();
      expect(sshSessions.length, "одна ssh-сессия после первого подключения").toBe(1);
      expect(sshSessions[0].id).toBe(termId);

      // 2. Повторное «Подключиться» к тому же серверу из списка.
      await page.goto("/miniapp/#/ssh");
      await expect(connectBtn).toBeVisible({ timeout: 15_000 });
      await connectBtn.click();

      // Пароль уже запомнен агентом → диалога нет, сразу возврат в терминал
      // с тостом ssh.reusedTerminal («Открыт ваш терминал этого сервера»).
      await Promise.all([
        page.waitForURL(new RegExp(`#\\/pty\\/${termId}`), { timeout: 20_000 }),
        page
          .locator(".toast", { hasText: "Открыт ваш терминал этого сервера" })
          .waitFor({ timeout: 20_000 }),
      ]);
      expect(page.url()).toContain(`#/pty/${termId}`);

      // Главное утверждение регрессии: дубликата сессии НЕТ.
      sshSessions = await sshSessionsForHost();
      expect(
        sshSessions.length,
        "повторное «Подключиться» не должно заводить вторую сессию",
      ).toBe(1);
      expect(sshSessions[0].id).toBe(termId);
    } finally {
      await cleanup(api, port);
    }
  });
});
