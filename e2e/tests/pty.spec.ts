import WebSocket from "ws";
import { test, expect } from "../fixtures/auth";

test.describe("PTY terminal", () => {
  test("create PTY → echo input → see output", async ({ api, baseURL, auth, agentPlatform }) => {
    // 1. Create a PTY session via API.
    const createRes = await api.post("/api/pty", {
      data: { cwd: agentPlatform === "win32" || agentPlatform === "windows" ? "C:\\" : "/tmp", cols: 80, rows: 24 },
    });
    expect(createRes.ok(), `create PTY: ${await createRes.text()}`).toBeTruthy();
    const { id } = await createRes.json();
    expect(id, "PTY id should be returned").toBeTruthy();

    // 2. Verify it appears in the list.
    const listRes = await api.get("/api/pty");
    expect(listRes.ok()).toBeTruthy();
    const body = await listRes.json();
    const sessions: Array<{ id?: string; ID?: string }> = body.sessions || body.items || (Array.isArray(body) ? body : []);
    expect(sessions.some((p) => p.id === id || p.ID === id)).toBeTruthy();

    // 3. Connect via WebSocket and exchange data.
    // The server accepts `?initData=token:<api_token>` (legacy auth) or
    // `?initData=Bearer <jwt>`. We use the token form because it doesn't expire.
    const wsURLWithToken =
      (baseURL || "http://localhost:8080").replace(/^http/, "ws") +
      `/ws/pty/${id}?initData=${encodeURIComponent("token:" + auth.apiToken)}`;

    const echoMarker = `tgctl-e2e-${Date.now()}`;
    const isWin = agentPlatform === "win32" || agentPlatform === "windows";
    const cmd = isWin
      ? `echo ${echoMarker}\r\n`
      : `echo ${echoMarker}\n`;

    const got = await new Promise<string>((resolveOk, rejectFail) => {
      const ws = new WebSocket(wsURLWithToken);
      let buf = "";
      const timer = setTimeout(() => {
        try { ws.close(); } catch { /* ignore */ }
        rejectFail(new Error(`Timeout waiting for marker. Got bytes=${buf.length}, tail: ${JSON.stringify(buf.slice(-200))}`));
      }, 30_000);

      ws.on("open", () => {
        // PowerShell needs ~2s to render its initial prompt. Send keystrokes
        // as a binary frame (matches the browser client which uses TextEncoder).
        setTimeout(() => ws.send(Buffer.from(cmd, "utf8"), { binary: true }), 2_500);
      });
      ws.on("message", (data) => {
        buf += Buffer.isBuffer(data) ? data.toString("utf8") : String(data);
        if (buf.includes(echoMarker)) {
          clearTimeout(timer);
          ws.close();
          resolveOk(buf);
        }
      });
      ws.on("error", (err) => {
        clearTimeout(timer);
        rejectFail(new Error(`WS error: ${err.message}`));
      });
    });

    expect(got).toContain(echoMarker);

    // 4. Cleanup.
    await api.delete(`/api/pty/${id}`);
  });
});
