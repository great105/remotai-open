import { test, expect } from "../fixtures/auth";

test.describe("AI session (shell agent)", () => {
  // Use the shell agent — it doesn't require an external CLI like Claude/Codex,
  // so this test is portable across machines.
  const sessionName = `e2e-shell-${Date.now()}`;

  test.afterAll(async ({ request, baseURL, auth }) => {
    await request.delete(`${baseURL}/api/sessions/${sessionName}`, {
      headers: { Authorization: `Bearer ${auth.accessToken}` },
    }).catch(() => { /* best-effort cleanup */ });
  });

  test("create session → send command → receive output", async ({ api, agentPlatform }) => {
    // 1. Create a shell session.
    const createRes = await api.post("/api/sessions", {
      data: {
        name: sessionName,
        agent_type: "shell",
        mode: "persistent",
      },
    });
    expect(createRes.ok(), `create session: ${await createRes.text()}`).toBeTruthy();
    const created = await createRes.json();
    expect(created.agent_type).toBe("shell");

    // 2. Verify it appears.
    const listRes = await api.get("/api/sessions");
    expect(listRes.ok()).toBeTruthy();
    const body = await listRes.json();
    const sessions: Array<{ name?: string; Name?: string; agent_type?: string; AgentType?: string }> =
      body.sessions || body.items || (Array.isArray(body) ? body : []);
    expect(
      sessions.some((s) => (s.name || s.Name) === sessionName && (s.agent_type || s.AgentType) === "shell"),
      "shell session should appear in list"
    ).toBeTruthy();

    // 3. Send a command.
    const marker = `tgctl-session-${Date.now()}`;
    const cmd = agentPlatform === "win32" || agentPlatform === "windows"
      ? `cmd /c echo ${marker}`
      : `echo ${marker}`;

    const sendRes = await api.post(`/api/sessions/${sessionName}/send`, {
      data: { prompt: cmd },
    });
    expect(sendRes.ok(), `send: ${await sendRes.text()}`).toBeTruthy();

    // 4. Poll the session detail until we see the marker in the output buffer.
    let saw = false;
    const deadline = Date.now() + 20_000;
    while (Date.now() < deadline) {
      const detail = await api.get(`/api/sessions/${sessionName}`);
      if (detail.ok()) {
        const body = await detail.json();
        const messages: Array<{ role?: string; Role?: string; text?: string; Text?: string }> =
          Array.isArray(body.messages) ? body.messages : [];
        const agentOutput = messages
          .filter((m) => (m.role || m.Role) === "agent")
          .map((m) => m.text || m.Text || "")
          .join("\n");
        if (agentOutput.includes(marker)) {
          saw = true;
          break;
        }
      }
      await new Promise((r) => setTimeout(r, 500));
    }

    expect(saw, "session output should contain the echo marker").toBeTruthy();
  });
});
