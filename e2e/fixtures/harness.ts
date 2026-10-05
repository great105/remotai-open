// Общие хелперы для e2e-харнесса: сборка Go-хелперов, запуск процесса с
// ожиданием строки "READY host:port" в stdout, синтетический Telegram initData.
import { spawn, ChildProcess, execFile } from "node:child_process";
import { promisify } from "node:util";
import { fileURLToPath } from "node:url";
import { dirname, resolve } from "node:path";
import { tmpdir } from "node:os";
import crypto from "node:crypto";

const execFileAsync = promisify(execFile);

// Корень репозитория (МОЕ): fixtures/ → e2e/ → МОЕ.
export const REPO_ROOT = resolve(dirname(fileURLToPath(import.meta.url)), "..", "..");

export type ReadyProcess = {
  child: ChildProcess;
  host: string;
  port: number;
  baseURL: string;
  /** Накопленный stdout+stderr — для диагностики при падении. */
  logs: () => string;
};

// buildGoBinary собирает Go-пакет в бинарь во временной папке.
// go build инкрементален — повторный прогон занимает секунды.
export async function buildGoBinary(moduleDir: string, pkg: string, outName: string): Promise<string> {
  const out = resolve(tmpdir(), `${outName}-${process.pid}.exe`);
  try {
    await execFileAsync("go", ["build", "-o", out, pkg], {
      cwd: moduleDir,
      timeout: 180_000,
    });
  } catch (e: any) {
    throw new Error(`go build ${pkg} failed: ${e.message}\n${e.stderr || ""}`);
  }
  return out;
}

// spawnReady запускает бинарь и ждёт строку "READY host:port" в stdout —
// так оба хелпера (релей и sshd) сообщают реальный порт при listen на :0.
export function spawnReady(
  binary: string,
  opts: { args?: string[]; env?: NodeJS.ProcessEnv; timeoutMs?: number },
): Promise<ReadyProcess> {
  const timeoutMs = opts.timeoutMs ?? 30_000;
  return new Promise((resolveOk, rejectFail) => {
    const child = spawn(binary, opts.args || [], {
      env: { ...process.env, ...(opts.env || {}) },
      stdio: ["ignore", "pipe", "pipe"],
    });
    let buf = "";
    let settled = false;
    const timer = setTimeout(() => {
      if (settled) return;
      settled = true;
      child.kill();
      rejectFail(new Error(`timeout waiting for READY from ${binary}.\nLogs:\n${buf}`));
    }, timeoutMs);

    const onData = (data: Buffer) => {
      buf += data.toString("utf8");
      if (settled) return;
      const m = buf.match(/^READY (\S+):(\d+)\s*$/m);
      if (m) {
        settled = true;
        clearTimeout(timer);
        const host = m[1] === "[::]" || m[1] === "0.0.0.0" ? "127.0.0.1" : m[1];
        const port = Number(m[2]);
        resolveOk({
          child,
          host,
          port,
          baseURL: `http://${host}:${port}`,
          logs: () => buf,
        });
      }
    };
    child.stdout?.on("data", onData);
    child.stderr?.on("data", onData); // логи Go идут в stderr — копим для отчёта
    child.on("error", (err) => {
      if (settled) return;
      settled = true;
      clearTimeout(timer);
      rejectFail(new Error(`spawn ${binary}: ${err.message}`));
    });
    child.on("exit", (code) => {
      if (settled) return;
      settled = true;
      clearTimeout(timer);
      rejectFail(new Error(`${binary} exited early (code=${code}).\nLogs:\n${buf}`));
    });
  });
}

export function stopChild(p?: ReadyProcess | null) {
  if (!p) return;
  try {
    p.child.kill();
  } catch {
    /* уже умер */
  }
}

// buildInitData — синтетический Telegram WebApp initData с корректным HMAC
// (алгоритм: secret = HMAC_SHA256(key="WebAppData", data=bot_token),
// hash = HMAC_SHA256(key=secret, data=check_string)). Зеркалит buildInitData
// из tgcontrol-relay/internal/server/pairing_e2e_test.go: на e2e-standalone
// релее BOT_TOKEN — тестовый, поэтому подпись валидна.
export function buildInitData(botToken: string, uid: number, username: string): string {
  const user = JSON.stringify({ id: uid, first_name: "E2E", username, language_code: "ru" });
  const authDate = Math.floor(Date.now() / 1000).toString();
  const checkString = `auth_date=${authDate}\nquery_id=AAEAAA\nuser=${user}`;
  const secret = crypto.createHmac("sha256", "WebAppData").update(botToken).digest();
  const hash = crypto.createHmac("sha256", secret).update(checkString).digest("hex");
  const params = new URLSearchParams();
  params.set("auth_date", authDate);
  params.set("query_id", "AAEAAA");
  params.set("user", user);
  params.set("hash", hash);
  return params.toString();
}
