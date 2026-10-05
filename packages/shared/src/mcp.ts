// MCP-серверы агентов без правки JSON: типы, запросы и правила формы.
//
// Сервер — internal/web/api_mcp.go + internal/mcpmgr. Правила здесь без React
// (AGENTS.md: правило, которое нельзя вызвать без React, нельзя и проверить):
// разбор строки команды, строк «KEY=value» и «Имя: значение», проверка формы.
// Тексты ошибок — КЛЮЧИ словаря (mcp.err.*), перевод делает экран.
//
// Значений env и заголовков сервер НЕ присылает никогда: только имена.

import { transport } from "./api-transport";

export type McpType = "stdio" | "http" | "sse";

export interface McpServer {
  name: string;
  agent: string;
  /** "user" — общий для аккаунта; "project" — только одной папки (только чтение). */
  scope: "user" | "project";
  project?: string;
  type: McpType | string;
  command?: string;
  /** Секреты в аргументах уже замаскированы сервером. */
  args?: string[];
  url?: string;
  env_keys?: string[];
  header_keys?: string[];
  enabled: boolean;
  read_only?: boolean;
  can_toggle: boolean;
  /** Почему нельзя трогать: "project" | "extra_settings" | "disabled_by_agent". */
  note?: string;
}

export interface McpCaps {
  types: McpType[];
  headers: boolean;
}

export interface McpAccountRef {
  id: string;
  label: string;
  is_default: boolean;
  selected: boolean;
}

export interface McpAgent {
  id: string;
  name: string;
  installed: boolean;
  caps: McpCaps;
  account: string;
  accounts: McpAccountRef[];
  servers: McpServer[];
  error?: string;
  error_code?: string;
}

export interface McpSpec {
  name: string;
  type: McpType;
  command?: string;
  args?: string[];
  env?: Record<string, string>;
  url?: string;
  headers?: Record<string, string>;
}

export interface McpTarget { agent: string; account: string }

export interface McpAddResult { agent: string; ok: boolean; error?: string; code?: string }

function api<T>(path: string, init?: RequestInit): Promise<T> {
  return transport().request<T>(path, init);
}

/** Серверы по агентам. accounts — выбранные аккаунты (id), по одному на агента. */
export function getMcpServers(accounts: string[] = []): Promise<{ agents: McpAgent[] }> {
  const q = accounts.filter(Boolean).map((a) => `account=${encodeURIComponent(a)}`).join("&");
  return api(q ? `/api/mcp?${q}` : "/api/mcp");
}

export function addMcpServer(targets: McpTarget[], server: McpSpec): Promise<{ ok: boolean; results: McpAddResult[] }> {
  return api("/api/mcp", { method: "POST", body: JSON.stringify({ targets, server }) });
}

export function deleteMcpServer(t: McpTarget, name: string): Promise<{ ok: boolean }> {
  const q = `agent=${encodeURIComponent(t.agent)}&account=${encodeURIComponent(t.account)}&name=${encodeURIComponent(name)}`;
  return api(`/api/mcp?${q}`, { method: "DELETE" });
}

export function toggleMcpServer(t: McpTarget, name: string, enabled: boolean): Promise<{ ok: boolean; enabled: boolean }> {
  return api("/api/mcp/toggle", { method: "POST", body: JSON.stringify({ ...t, name, enabled }) });
}

// ── Правила формы ───────────────────────────────────────────────────────────

/** Имя: то же пересечение правил Claude и Codex, что и на сервере. */
export const MCP_NAME_RE = /^[A-Za-z0-9_][A-Za-z0-9_-]{0,63}$/;
const ENV_KEY_RE = /^[A-Za-z_][A-Za-z0-9_]{0,127}$/;
const HEADER_KEY_RE = /^[A-Za-z0-9!#$%&'*+.^_|~-]{1,128}$/;

/**
 * Строка команды → слова, как их разобрал бы шелл: пробелы делят, кавычки
 * склеивают («"C:\Мои файлы"» — одно слово). Обратная косая — обычная буква:
 * это пути Windows, а не экранирование. Незакрытая кавычка — ошибка (null):
 * молча дописать её значило бы запустить не ту команду.
 *
 * Зачем: README любого MCP-сервера даёт команду одной строкой
 * («npx -y @modelcontextprotocol/server-filesystem C:\work»), и владелец её
 * просто вставляет — делить на «команду» и «аргументы» руками он не должен.
 */
export function splitCommandLine(line: string): string[] | null {
  const out: string[] = [];
  let cur = "";
  let quote: '"' | "'" | null = null;
  let has = false;
  for (const ch of line) {
    if (quote) {
      if (ch === quote) quote = null;
      else cur += ch;
      continue;
    }
    if (ch === '"' || ch === "'") { quote = ch; has = true; continue; }
    if (/\s/.test(ch)) {
      if (has) { out.push(cur); cur = ""; has = false; }
      continue;
    }
    cur += ch;
    has = true;
  }
  if (quote) return null;
  if (has) out.push(cur);
  return out;
}

export interface LinesResult {
  map: Record<string, string>;
  /** Номер строки (с 1) → ключ ошибки. */
  errors: { line: number; key: string }[];
}

/**
 * «KEY=value» по строке. Пустые строки и «# комментарии» пропускаются,
 * «export » в начале срезается (так пишут в .env и в инструкциях).
 */
export function parseEnvLines(text: string): LinesResult {
  const map: Record<string, string> = {};
  const errors: LinesResult["errors"] = [];
  text.split(/\r?\n/).forEach((raw, i) => {
    const line = raw.trim().replace(/^export\s+/, "");
    if (!line || line.startsWith("#")) return;
    const eq = line.indexOf("=");
    if (eq <= 0) { errors.push({ line: i + 1, key: "mcp.err.envFormat" }); return; }
    const k = line.slice(0, eq).trim();
    let v = line.slice(eq + 1).trim();
    if (v.length >= 2 && ((v.startsWith('"') && v.endsWith('"')) || (v.startsWith("'") && v.endsWith("'")))) v = v.slice(1, -1);
    if (!ENV_KEY_RE.test(k)) { errors.push({ line: i + 1, key: "mcp.err.envKey" }); return; }
    if (k in map) { errors.push({ line: i + 1, key: "mcp.err.duplicate" }); return; }
    map[k] = v;
  });
  return { map, errors };
}

/** «Имя: значение» по строке (как заголовки пишут в документации). */
export function parseHeaderLines(text: string): LinesResult {
  const map: Record<string, string> = {};
  const errors: LinesResult["errors"] = [];
  text.split(/\r?\n/).forEach((raw, i) => {
    const line = raw.trim();
    if (!line || line.startsWith("#")) return;
    const colon = line.indexOf(":");
    if (colon <= 0) { errors.push({ line: i + 1, key: "mcp.err.headerFormat" }); return; }
    const k = line.slice(0, colon).trim();
    const v = line.slice(colon + 1).trim();
    if (!HEADER_KEY_RE.test(k)) { errors.push({ line: i + 1, key: "mcp.err.headerKey" }); return; }
    if (Object.keys(map).some((x) => x.toLowerCase() === k.toLowerCase())) { errors.push({ line: i + 1, key: "mcp.err.duplicate" }); return; }
    map[k] = v;
  });
  return { map, errors };
}

/** Имя по команде или адресу — чтобы не выдумывать его самому. */
export function suggestMcpName(type: McpType, commandLine: string, url: string): string {
  let base = "";
  if (type === "stdio") {
    const words = splitCommandLine(commandLine) || [];
    // Самое содержательное слово — пакет: не флаг, не npx/uvx/node.
    const runners = new Set(["npx", "uvx", "node", "python", "python3", "bunx", "pnpm", "dlx", "docker", "run", "deno"]);
    const pkg = words.find((w, i) => i > 0 && !w.startsWith("-") && !runners.has(w.toLowerCase())) || words[0] || "";
    // «@playwright/mcp@latest» → версия долой; «mcp» ничего не говорит —
    // тогда имя берём из области пакета («playwright»).
    const parts = pkg.replace(/(.)@[^/\\]*$/, "$1").split(/[\\/]/).filter(Boolean);
    base = parts.pop() || "";
    if (/^(mcp|server|mcp-server)$/i.test(base) && parts.length) base = parts.pop()!.replace(/^@/, "");
    base = base.replace(/\.(exe|cmd|js|py)$/i, "").replace(/^(mcp-server-|server-)/i, "").replace(/(-mcp-server|-mcp|_mcp)$/i, "");
  } else {
    try {
      const host = new URL(url).hostname.split(".").filter((p) => !["www", "mcp", "api", "app", "com", "dev", "io", "ru", "net", "org", "ai"].includes(p));
      base = host[0] || "";
    } catch { base = ""; }
  }
  return base.replace(/[^A-Za-z0-9_-]+/g, "-").replace(/^-+|-+$/g, "").slice(0, 64).toLowerCase();
}

export interface McpForm {
  agents: string[];
  type: McpType;
  name: string;
  /** Вся строка запуска целиком: «npx -y пакет аргументы». */
  commandLine: string;
  url: string;
  envText: string;
  headersText: string;
}

export type McpFormField = "agents" | "name" | "commandLine" | "url" | "envText" | "headersText";

export interface McpFormCheck {
  /** Поле → ключ словаря (+ номер строки для многострочных полей). */
  errors: Partial<Record<McpFormField, { key: string; line?: number }>>;
  spec?: McpSpec;
}

/**
 * Проверка формы до отправки. Та же, что на сервере (mcpmgr.Spec.Normalize +
 * CheckCaps), — сервер всё равно проверит сам, а здесь это ради того, чтобы
 * ошибка стояла под полем, а не приходила тостом после ожидания CLI.
 */
export function checkMcpForm(form: McpForm, caps: Record<string, McpCaps>): McpFormCheck {
  const errors: McpFormCheck["errors"] = {};
  const name = form.name.trim();
  if (!form.agents.length) errors.agents = { key: "mcp.err.noAgent" };
  else {
    for (const a of form.agents) {
      const c = caps[a];
      if (!c || !c.types.includes(form.type)) { errors.agents = { key: "mcp.err.typeUnsupported" }; break; }
    }
  }
  if (!name) errors.name = { key: "mcp.err.nameEmpty" };
  else if (!MCP_NAME_RE.test(name)) errors.name = { key: "mcp.err.nameFormat" };

  const spec: McpSpec = { name, type: form.type };
  if (form.type === "stdio") {
    const words = splitCommandLine(form.commandLine.trim());
    if (words === null) errors.commandLine = { key: "mcp.err.quote" };
    else if (!words.length) errors.commandLine = { key: "mcp.err.commandEmpty" };
    else { spec.command = words[0]; spec.args = words.slice(1); }
    const env = parseEnvLines(form.envText);
    if (env.errors.length) errors.envText = env.errors[0];
    else if (Object.keys(env.map).length) spec.env = env.map;
  } else {
    const url = form.url.trim();
    let ok = false;
    try {
      const u = new URL(url);
      ok = (u.protocol === "https:" || u.protocol === "http:") && !!u.hostname && !u.username && !u.password;
    } catch { ok = false; }
    if (!url) errors.url = { key: "mcp.err.urlEmpty" };
    else if (!ok) errors.url = { key: "mcp.err.urlFormat" };
    else spec.url = url;
    const headers = parseHeaderLines(form.headersText);
    if (headers.errors.length) errors.headersText = headers.errors[0];
    else if (Object.keys(headers.map).length) {
      const noHeaders = form.agents.find((a) => caps[a] && !caps[a].headers);
      if (noHeaders) errors.headersText = { key: "mcp.err.headersUnsupported" };
      else spec.headers = headers.map;
    }
  }
  return Object.keys(errors).length ? { errors } : { errors, spec };
}

/** Короткая строка «что запускается» для карточки сервера. */
export function mcpServerSummary(s: McpServer): string {
  if (s.url) return s.url;
  return [s.command, ...(s.args || [])].filter(Boolean).join(" ");
}
