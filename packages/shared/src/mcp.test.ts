import { describe, expect, it } from "vitest";
import {
  checkMcpForm, mcpServerSummary, parseEnvLines, parseHeaderLines, splitCommandLine, suggestMcpName,
  type McpCaps, type McpForm,
} from "./mcp";

const CAPS: Record<string, McpCaps> = {
  claude: { types: ["stdio", "http", "sse"], headers: true },
  codex: { types: ["stdio", "http"], headers: false },
};

const form = (over: Partial<McpForm>): McpForm => ({
  agents: ["claude"], type: "stdio", name: "fs", commandLine: "npx -y pkg", url: "", envText: "", headersText: "", ...over,
});

describe("splitCommandLine", () => {
  it("делит по пробелам и склеивает кавычки, обратная косая — буква", () => {
    expect(splitCommandLine(`npx -y @mcp/server-filesystem "C:\\Мои файлы" 'a b'`))
      .toEqual(["npx", "-y", "@mcp/server-filesystem", "C:\\Мои файлы", "a b"]);
  });
  it("пустые кавычки — пустой аргумент, незакрытая — ошибка", () => {
    expect(splitCommandLine(`cmd ""`)).toEqual(["cmd", ""]);
    expect(splitCommandLine(`cmd "open`)).toBeNull();
    expect(splitCommandLine("   ")).toEqual([]);
  });
});

describe("parseEnvLines", () => {
  it("KEY=value, комментарии, export и кавычки", () => {
    const r = parseEnvLines("# ключ\nexport API_KEY=sk-1=2\n\nB = \"x y\"\n");
    expect(r.errors).toEqual([]);
    expect(r.map).toEqual({ API_KEY: "sk-1=2", B: "x y" });
  });
  it("ошибки называют строку", () => {
    const r = parseEnvLines("A=1\nбез равно\n1X=2\nA=3");
    expect(r.errors).toEqual([
      { line: 2, key: "mcp.err.envFormat" },
      { line: 3, key: "mcp.err.envKey" },
      { line: 4, key: "mcp.err.duplicate" },
    ]);
  });
});

describe("parseHeaderLines", () => {
  it("Имя: значение, двоеточие в значении не ломает", () => {
    expect(parseHeaderLines("Authorization: Bearer a:b\nX-Id: 1").map).toEqual({ Authorization: "Bearer a:b", "X-Id": "1" });
  });
  it("повтор без учёта регистра и плохое имя", () => {
    expect(parseHeaderLines("A: 1\na: 2").errors[0]).toEqual({ line: 2, key: "mcp.err.duplicate" });
    expect(parseHeaderLines("Bad Name: 1").errors[0].key).toBe("mcp.err.headerKey");
  });
});

describe("suggestMcpName", () => {
  it("по пакету и по адресу", () => {
    expect(suggestMcpName("stdio", "npx -y @modelcontextprotocol/server-filesystem C:\\work", "")).toBe("filesystem");
    expect(suggestMcpName("stdio", "npx -y @playwright/mcp@latest", "")).toBe("playwright");
    expect(suggestMcpName("stdio", "uvx mcp-server-git", "")).toBe("git");
    expect(suggestMcpName("http", "", "https://mcp.sentry.dev/mcp")).toBe("sentry");
    expect(suggestMcpName("http", "", "не адрес")).toBe("");
  });
});

describe("checkMcpForm", () => {
  it("stdio: команда и аргументы из одной строки, env в карту", () => {
    const r = checkMcpForm(form({ commandLine: `npx -y pkg "a b"`, envText: "K=v" }), CAPS);
    expect(r.errors).toEqual({});
    expect(r.spec).toEqual({ name: "fs", type: "stdio", command: "npx", args: ["-y", "pkg", "a b"], env: { K: "v" } });
  });
  it("имя: пусто, пробел, точка", () => {
    expect(checkMcpForm(form({ name: "" }), CAPS).errors.name?.key).toBe("mcp.err.nameEmpty");
    expect(checkMcpForm(form({ name: "my server" }), CAPS).errors.name?.key).toBe("mcp.err.nameFormat");
    expect(checkMcpForm(form({ name: "a.b" }), CAPS).errors.name?.key).toBe("mcp.err.nameFormat");
  });
  it("нет агента и SSE для Codex", () => {
    expect(checkMcpForm(form({ agents: [] }), CAPS).errors.agents?.key).toBe("mcp.err.noAgent");
    const sse = checkMcpForm(form({ agents: ["claude", "codex"], type: "sse", url: "https://x.io/sse" }), CAPS);
    expect(sse.errors.agents?.key).toBe("mcp.err.typeUnsupported");
    expect(sse.spec).toBeUndefined();
  });
  it("адрес: схема, логин в адресе, заголовки у Codex", () => {
    expect(checkMcpForm(form({ type: "http", url: "ftp://x" }), CAPS).errors.url?.key).toBe("mcp.err.urlFormat");
    expect(checkMcpForm(form({ type: "http", url: "https://u:p@x.io" }), CAPS).errors.url?.key).toBe("mcp.err.urlFormat");
    expect(checkMcpForm(form({ type: "http", url: "https://x.io", agents: ["codex"], headersText: "A: b" }), CAPS).errors.headersText?.key)
      .toBe("mcp.err.headersUnsupported");
    const ok = checkMcpForm(form({ type: "http", url: "https://x.io/mcp", headersText: "Authorization: Bearer t" }), CAPS);
    expect(ok.spec).toEqual({ name: "fs", type: "http", url: "https://x.io/mcp", headers: { Authorization: "Bearer t" } });
  });
  it("незакрытая кавычка в команде", () => {
    expect(checkMcpForm(form({ commandLine: `npx "pkg` }), CAPS).errors.commandLine?.key).toBe("mcp.err.quote");
  });
});

describe("mcpServerSummary", () => {
  it("адрес или команда с аргументами", () => {
    expect(mcpServerSummary({ name: "a", agent: "claude", scope: "user", type: "stdio", command: "npx", args: ["-y", "p"], enabled: true, can_toggle: true })).toBe("npx -y p");
    expect(mcpServerSummary({ name: "w", agent: "claude", scope: "user", type: "http", url: "https://x", enabled: true, can_toggle: true })).toBe("https://x");
  });
});
