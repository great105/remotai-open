// «Проверить подключение» агента: типы ответа, вызовы API и ПРАВИЛА — какие
// слова показать человеку по кодам с компьютера.
//
// Сервер (internal/agentcheck) отдаёт только коды и маски: какой источник,
// почему победил, почему проверка не прошла. Слова живут здесь, в модуле без
// React, — их можно проверить тестом в node (agentCheck.test.ts), и один и тот
// же перевод видят APK, окно на ПК и Telegram.
//
// КЛЮЧЕЙ ЗДЕСЬ НЕТ И БЫТЬ НЕ МОЖЕТ: компьютер отдаёт маску «sk-…a1b2», а сам
// ключ остаётся в окружении агента.

import { transport } from "./api-transport";
import { t } from "./i18n";

export interface AgentCheckSource {
  kind: string;
  name?: string;
  path?: string;
  account?: string;
}

export interface AgentCheckShadow {
  value?: string;
  source: AgentCheckSource;
  ignored?: string;
}

export interface AgentCheckItem {
  value: string;
  kind?: string;
  source: AgentCheckSource;
  why?: string;
  overridden?: AgentCheckShadow[];
}

export interface AgentConnection {
  agent_id: string;
  agent_name?: string;
  supported: boolean;
  account: { id: string; label?: string; is_default: boolean };
  route: string;
  provider?: string;
  endpoint: AgentCheckItem;
  auth: AgentCheckItem;
  model: AgentCheckItem;
  proxy?: string;
  login_file?: boolean;
  cli: { asked: boolean; logged_in?: boolean; method?: string; plan?: string };
  check: "http" | "cli" | "";
  notes?: string[];
}

export interface AgentCheckResult {
  ok: boolean;
  via: string;
  route?: string;
  reply?: string;
  model?: string;
  latency_ms: number;
  http_status?: number;
  reason?: string;
  detail?: string;
  host?: string;
  checked_at: number;
}

interface CheckResponse {
  state: "done" | "running";
  id: string;
  result?: AgentCheckResult;
  connection?: AgentConnection;
}

/** Действующее подключение агента под аккаунтом (пусто — активный). */
export function getAgentConnection(agentID: string, account = ""): Promise<AgentConnection> {
  const q = account ? `?account=${encodeURIComponent(account)}` : "";
  return transport().request<AgentConnection>(`/api/agents/${encodeURIComponent(agentID)}/connection${q}`);
}

/**
 * Один настоящий запрос к модели. Компьютер ждёт проверку не дольше 20 с
 * (облачный запрос обрывается на 30), поэтому долгую проверку по подписке
 * дожидаемся повтором с её номером — второй запрос к модели при этом не уходит.
 */
export async function runAgentCheck(
  agentID: string,
  opts: { account?: string; model?: string; signal?: AbortSignal } = {},
): Promise<{ result: AgentCheckResult; connection?: AgentConnection }> {
  const path = `/api/agents/${encodeURIComponent(agentID)}/check`;
  let body: Record<string, string> = { account: opts.account || "", model: opts.model || "" };
  // 90 с по подписке с запасом: шесть ожиданий по 20 с.
  for (let attempt = 0; attempt < 6; attempt++) {
    const res = await transport().request<CheckResponse>(path, {
      method: "POST",
      body: JSON.stringify(body),
      signal: opts.signal,
    });
    if (res.state === "done" && res.result) return { result: res.result, connection: res.connection };
    body = { id: res.id };
  }
  throw new Error(t("agentCheck.tooLong"));
}

// ── Правила ─────────────────────────────────────────────────────────

const VENDOR: Record<string, string> = {
  anthropic: "Anthropic",
  openai: "OpenAI",
  openrouter: "OpenRouter",
  bedrock: "Amazon Bedrock",
  vertex: "Google Vertex",
  foundry: "Microsoft Foundry",
};

export function vendorName(provider?: string): string {
  return VENDOR[provider || ""] || t("agentCheck.vendorOther");
}

/** Одна фраза «как работает агент» — первое, что человек читает в шторке. */
export function routeSummary(conn: AgentConnection): string {
  if (!conn.supported) return t("agentCheck.route.unsupported");
  const route = ROUTES.has(conn.route) ? conn.route : "unknown";
  return t(`agentCheck.route.${route}`, { vendor: vendorName(conn.provider) });
}

// Коды, для которых есть слова. Компьютер бывает новее клиента: незнакомый
// код показываем общим «не удалось», а не ключом словаря.
const ROUTES = new Set(["subscription", "api_key", "openrouter", "custom", "cloud", "local", "none", "unknown"]);
const AUTH_KINDS = new Set(["api_key", "bearer", "helper", "token", "none", "unknown"]);
const NOTES = new Set([
  "system_proxy_dropped", "login_file_but_logged_out", "codex_api_key_exec_only",
  "codex_provider_unknown", "env_key_missing", "openrouter_not_configured",
  "openrouter_model_unset", "agent_not_supported", "cli_missing",
]);
const IGNORED = new Set(["not_read_by_cli", "exec_only", "dropped_by_launch"]);

/** Короткое имя файла для подписи источника: «.claude/settings.json». */
export function shortPath(path?: string): string {
  if (!path) return "";
  const parts = path.split(/[\\/]+/).filter(Boolean);
  return parts.slice(-2).join("/");
}

/** Откуда значение — словами человека. */
export function sourceText(src: AgentCheckSource): string {
  switch (src.kind) {
    case "system_env":
      return t("agentCheck.src.system_env", { name: src.name || "" });
    case "env_file":
      return t("agentCheck.src.env_file", { name: src.name || "" });
    case "openrouter_store":
      return t("agentCheck.src.openrouter_store");
    case "account":
      return src.account
        ? t("agentCheck.src.account", { account: src.account })
        : t("agentCheck.src.accountDefault");
    case "cli_settings":
    case "cli_config": {
      const file = shortPath(src.path);
      const base = t(`agentCheck.src.${src.kind}`, { file });
      return src.name ? `${base}, ${src.name}` : base;
    }
    case "cli_login":
    case "vendor_default":
    case "cli_default":
    case "remotai_settings":
      return t(`agentCheck.src.${src.kind}`);
  }
  return t("agentCheck.src.unknown");
}

const WHY_WITH_TEXT = new Set([
  "settings_over_env", "account_over_env", "env_over_settings", "token_over_key",
  "key_over_login", "profile", "system_over_store", "cli_reported",
]);

/** Почему победило — или пусто, если объяснять нечего (единственный источник). */
export function whyText(why?: string): string {
  return why && WHY_WITH_TEXT.has(why) ? t(`agentCheck.why.${why}`) : "";
}

/** Строка про значение, которое тоже задано, но НЕ действует. */
export function shadowText(s: AgentCheckShadow): string {
  const src = sourceText(s.source);
  const reason = s.ignored && IGNORED.has(s.ignored) ? t(`agentCheck.ign.${s.ignored}`) : t("agentCheck.ign.overridden");
  return s.value
    ? t("agentCheck.shadow", { value: s.value, source: src, reason })
    : t("agentCheck.shadowNoValue", { source: src, reason });
}

/** Чем агент входит — словами. */
export function authText(item: AgentCheckItem): string {
  const kind = item.kind || "unknown";
  if (kind === "subscription") {
    return item.value ? t("agentCheck.auth.subscriptionPlan", { plan: planName(item.value) }) : t("agentCheck.auth.subscription");
  }
  if (!AUTH_KINDS.has(kind)) return t("agentCheck.auth.unknown");
  return t(`agentCheck.auth.${kind}`, { mask: item.value || "" }).trim();
}

function planName(plan: string): string {
  const p = plan.trim();
  return p ? p.charAt(0).toUpperCase() + p.slice(1) : p;
}

export function modelText(item: AgentCheckItem): string {
  return item.value || t("agentCheck.model.default");
}

export function noteText(note: string): string {
  return NOTES.has(note) ? t(`agentCheck.note.${note}`) : "";
}

export interface ReasonView {
  title: string;
  hint: string;
}

const KNOWN_REASONS = new Set([
  "key_rejected", "login_expired", "not_logged_in", "no_funds", "limit_reached",
  "rate_limited", "model_not_found", "network", "timeout", "server_error",
  "no_model", "cli_missing", "unsupported", "unknown",
]);

/**
 * Причина отказа — заголовок и что делать. «Нет сети до …» называет хост:
 * «OpenRouter не отвечает» и «нет интернета» человек чинит по-разному.
 */
export function reasonView(res: Pick<AgentCheckResult, "reason" | "host">): ReasonView {
  const reason = res.reason && KNOWN_REASONS.has(res.reason) ? res.reason : "unknown";
  if (reason === "network") {
    return {
      title: res.host ? t("agentCheck.reason.network", { host: res.host }) : t("agentCheck.reason.networkNoHost"),
      hint: t("agentCheck.reasonHint.network"),
    };
  }
  return { title: t(`agentCheck.reason.${reason}`), hint: t(`agentCheck.reasonHint.${reason}`) };
}

/** «7,5 с» или «640 мс». */
export function latencyText(ms: number): string {
  if (!Number.isFinite(ms) || ms <= 0) return "";
  if (ms < 1000) return t("agentCheck.ms", { n: Math.round(ms) });
  const s = (ms / 1000).toFixed(1).replace(".", ",");
  return t("agentCheck.sec", { n: s });
}

/**
 * Какой моделью предложить проверку, кроме действующей. Только у OpenRouter:
 * там у человека один ключ и бесплатный роутер, который отвечает без денег на
 * счету — так «ключ жив» отделяется от «на платную модель нет денег».
 * null — предлагать нечего (модель и так бесплатная или это не OpenRouter).
 */
export const FREE_CHECK_MODEL = "openrouter/free";

export function freeCheckModel(conn: Pick<AgentConnection, "route" | "provider" | "model" | "check">): string | null {
  if (conn.check !== "http") return null;
  if (conn.route !== "openrouter" && conn.provider !== "openrouter") return null;
  const m = (conn.model?.value || "").trim();
  if (m === FREE_CHECK_MODEL || m.endsWith(":free")) return null;
  return FREE_CHECK_MODEL;
}

/**
 * Показывать ли кнопку «Проверить подключение». Компьютер разбирает подключение
 * у Claude Code и Codex (их конфиги сверены запуском) и у агентов, которым
 * Remotai сам подставляет модель OpenRouter (`model_flag` из реестра на Go).
 * У остальных кнопка вела бы к «пока не умеем» — её лучше не рисовать.
 */
export function canCheckConnection(agent: { id: string; detected?: boolean; model_flag?: string }): boolean {
  if (!agent.detected) return false;
  return agent.id === "claude" || agent.id === "codex" || Boolean(agent.model_flag);
}

/** Подсказка под кнопкой: что именно сейчас произойдёт и сколько это стоит. */
export function checkHint(conn: Pick<AgentConnection, "check">): string {
  if (conn.check === "cli") return t("agentCheck.hintCli");
  if (conn.check === "http") return t("agentCheck.hintHttp");
  return t("agentCheck.hintNone");
}
