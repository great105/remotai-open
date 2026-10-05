import { expect, it, vi } from "vitest";
import { AgentHistoryClient, AgentHistoryError, parseAgentHistoryPage } from "./AgentHistoryClient";

// T-23: код отказа и версия доезжают до поверхности, текст ошибки остаётся кодом.
it("passes the server error code and unsupported agent version up to the surface", async () => {
  const client = new AgentHistoryClient(), send = vi.fn();
  const pending = client.read({ readyState: 1, send }, "");
  const request = JSON.parse(send.mock.calls[0][0]).request;
  client.accept({ t: "agent-history", v: 1, request, error: "history_version_unsupported", version: "0.200.0" });
  const error = await pending.catch(e => e);
  expect(error).toBeInstanceOf(AgentHistoryError);
  expect(error.message).toBe("history_version_unsupported");
  expect(error).toMatchObject({ code: "history_version_unsupported", version: "0.200.0" });
  const next = client.read({ readyState: 1, send }, "");
  client.accept({ t: "agent-history", v: 1, request: JSON.parse(send.mock.calls[1][0]).request });
  await expect(next).rejects.toMatchObject({ code: "history_unsupported_format", version: undefined });
});

const page = { source: "fixture", agent: "codex", version: "0.154.0", schema: "codex-app-server-turns-v2", text: "  code\n中😀", partial: false };
it("binds the reply to the request and rejects unknown or unbounded schemas", async () => {
  const client = new AgentHistoryClient(), send = vi.fn();
  const pending = client.read({ readyState: 1, send }, "");
  const request = JSON.parse(send.mock.calls[0][0]).request;
  client.accept({ t: "agent-history", v: 1, request: "stale", page });
  client.accept({ t: "agent-history", v: 1, request, page });
  expect(await pending).toMatchObject(page);
  expect(parseAgentHistoryPage({ ...page, schema: "unknown" })).toBeNull();
  expect(parseAgentHistoryPage({ ...page, text: "x".repeat(256 * 1024 + 1) })).toBeNull();
  expect(send).toHaveBeenCalledTimes(1);
});
it("preserves read-only failure semantics without reconnection retry", async () => {
  const client = new AgentHistoryClient(), send = vi.fn();
  const pending = client.read({ readyState: 1, send }, "older");
  const rejection = expect(pending).rejects.toThrow("history_source_changed");
  client.reset(); await rejection;
  expect(send).toHaveBeenCalledTimes(1);
  await expect(client.read({ readyState: 1, send: () => { throw new Error("disconnected"); } }, "")).rejects.toThrow("history_unavailable");
});
