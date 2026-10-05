import { describe, expect, it } from "vitest";
import {
  authText, canCheckConnection, checkHint, freeCheckModel, latencyText, modelText, noteText, reasonView,
  routeSummary, shadowText, shortPath, sourceText, whyText,
  type AgentConnection,
} from "./agentCheck";

const REASONS = [
  "key_rejected", "login_expired", "not_logged_in", "no_funds", "limit_reached",
  "rate_limited", "model_not_found", "network", "timeout", "server_error",
  "no_model", "cli_missing", "unsupported", "unknown",
];

function conn(over: Partial<AgentConnection> = {}): AgentConnection {
  return {
    agent_id: "claude",
    supported: true,
    account: { id: "default", is_default: true },
    route: "subscription",
    provider: "anthropic",
    endpoint: { value: "https://api.anthropic.com", source: { kind: "vendor_default" } },
    auth: { value: "max", kind: "subscription", source: { kind: "cli_login" } },
    model: { value: "", source: { kind: "cli_default" } },
    cli: { asked: true },
    check: "cli",
    ...over,
  };
}

describe("причины отказа — словами человека", () => {
  it.each(REASONS)("у причины %s есть заголовок и подсказка", (reason) => {
    const v = reasonView({ reason, host: "openrouter.ai" });
    expect(v.title).not.toMatch(/agentCheck|⟦/);
    expect(v.hint).not.toMatch(/agentCheck|⟦/);
    expect(v.title.length).toBeGreaterThan(3);
  });
  it("«нет сети» называет хост, до которого не достучались", () => {
    expect(reasonView({ reason: "network", host: "openrouter.ai" }).title).toBe("Нет сети до openrouter.ai");
    expect(reasonView({ reason: "network" }).title).toBe("Нет сети до сервера модели");
  });
  it("незнакомая причина с компьютера новее клиента не ломает экран", () => {
    expect(reasonView({ reason: "brand_new_reason" }).title).toBe("Не получилось");
  });
  it("просроченный вход по подписке сказан как в задаче", () => {
    expect(reasonView({ reason: "login_expired" }).title).toBe("Вход по подписке истёк — войдите заново");
    expect(reasonView({ reason: "no_funds" }).title).toBe("На балансе нет денег");
    expect(reasonView({ reason: "key_rejected" }).title).toBe("Ключ отклонён");
  });
});

describe("источники и приоритеты", () => {
  it("называет источник словами, а не кодом", () => {
    expect(sourceText({ kind: "system_env", name: "ANTHROPIC_API_KEY" })).toBe("переменная окружения системы ANTHROPIC_API_KEY");
    expect(sourceText({ kind: "openrouter_store" })).toBe("OpenRouter в Remotai");
    expect(sourceText({ kind: "account", account: "Работа" })).toBe("аккаунт «Работа»");
    expect(sourceText({ kind: "account" })).toBe("основной аккаунт");
    expect(sourceText({ kind: "cli_settings", path: "C:\\Users\\u\\.claude\\settings.json", name: "ANTHROPIC_BASE_URL" }))
      .toBe("настройки агента .claude/settings.json, ANTHROPIC_BASE_URL");
    expect(sourceText({ kind: "что-то-новое" })).toBe("неизвестно");
  });
  it("объясняет только настоящие победы, а не «единственный источник»", () => {
    expect(whyText("settings_over_env")).toMatch(/сильнее/);
    expect(whyText("only")).toBe("");
    expect(whyText(undefined)).toBe("");
  });
  it("недействующее значение помечено причиной", () => {
    expect(shadowText({ value: "http://127.0.0.1:1234/v1", source: { kind: "system_env", name: "OPENAI_BASE_URL" }, ignored: "not_read_by_cli" }))
      .toBe("Не действует: http://127.0.0.1:1234/v1 — переменная окружения системы OPENAI_BASE_URL, агент её не читает");
    expect(shadowText({ source: { kind: "cli_login" } })).toBe("Не действует: вход, сделанный в самом агенте, перекрыто");
  });
  it("shortPath оставляет два последних звена пути", () => {
    expect(shortPath("/root/.codex/config.toml")).toBe(".codex/config.toml");
    expect(shortPath("")).toBe("");
  });
});

describe("сводка и значения", () => {
  it("одна фраза о маршруте", () => {
    expect(routeSummary(conn())).toBe("Работает по вашей подписке — напрямую у Anthropic.");
    expect(routeSummary(conn({ route: "api_key", provider: "openai" }))).toMatch(/OpenAI списывает/);
    expect(routeSummary(conn({ route: "weird" }))).toBe("Не удалось понять, как подключается агент.");
    expect(routeSummary(conn({ supported: false }))).toMatch(/пока не умеет/);
  });
  it("вход: тариф с заглавной, ключ только маской", () => {
    expect(authText({ value: "max", kind: "subscription", source: { kind: "cli_login" } })).toBe("Вход по подписке · Max");
    expect(authText({ value: "sk-…a1b2", kind: "api_key", source: { kind: "system_env" } })).toBe("Ключ API sk-…a1b2");
    expect(authText({ value: "", kind: "strange", source: { kind: "x" } })).toBe("Не удалось узнать");
  });
  it("модель по умолчанию названа честно", () => {
    expect(modelText({ value: "", source: { kind: "cli_default" } })).toBe("Агент выбирает сам");
  });
  it("незнакомая заметка не рисуется ключом", () => {
    expect(noteText("system_proxy_dropped")).toMatch(/прокси/);
    expect(noteText("from_the_future")).toBe("");
  });
  it("задержка по-русски", () => {
    expect(latencyText(7464)).toBe("7,5 с");
    expect(latencyText(640)).toBe("640 мс");
    expect(latencyText(0)).toBe("");
  });
  it("подсказка под кнопкой зависит от способа проверки", () => {
    expect(checkHint({ check: "cli" })).toMatch(/до минуты/i);
    expect(checkHint({ check: "http" })).toMatch(/тем же ключом/);
  });
});

describe("кому показывать кнопку", () => {
  it("Claude, Codex и агенты с моделью OpenRouter — только установленные", () => {
    expect(canCheckConnection({ id: "claude", detected: true })).toBe(true);
    expect(canCheckConnection({ id: "codex", detected: false })).toBe(false);
    expect(canCheckConnection({ id: "opencode", detected: true, model_flag: "-m" })).toBe(true);
    expect(canCheckConnection({ id: "gemini", detected: true })).toBe(false);
  });
});

describe("модель для проверки по умолчанию", () => {
  it("OpenRouter с платной моделью — предлагаем бесплатный роутер", () => {
    expect(freeCheckModel(conn({ route: "openrouter", provider: "openrouter", check: "http",
      model: { value: "anthropic/claude-sonnet", source: { kind: "remotai_settings" } } }))).toBe("openrouter/free");
  });
  it("уже бесплатная — предлагать нечего", () => {
    expect(freeCheckModel(conn({ route: "openrouter", provider: "openrouter", check: "http",
      model: { value: "qwen/qwen3-coder:free", source: { kind: "cli_config" } } }))).toBeNull();
    expect(freeCheckModel(conn({ route: "openrouter", provider: "openrouter", check: "http",
      model: { value: "openrouter/free", source: { kind: "remotai_settings" } } }))).toBeNull();
  });
  it("не OpenRouter или проверка запуском CLI — не предлагаем", () => {
    expect(freeCheckModel(conn())).toBeNull();
    expect(freeCheckModel(conn({ route: "api_key", provider: "anthropic", check: "http" }))).toBeNull();
  });
});
