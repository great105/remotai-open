import { describe, it, expect } from "vitest";
import { shouldOfferOpenRouter } from "./openrouterOffer";
import type { AIUsageSnapshot } from "./aiUsage";

const snapshot = (statuses: string[]): AIUsageSnapshot => ({
  captured_at: "2026-08-30T12:00:00Z",
  providers: statuses.map((status, i) => ({
    id: `agent${i}`, name: `Агент ${i}`, installed: true, status, windows: [],
  })),
});

describe("shouldOfferOpenRouter", () => {
  it("ключ уже подключён — предлагать нечего", () => {
    expect(shouldOfferOpenRouter({ configured: true, installedAgents: 0, usage: null })).toBe(false);
    expect(shouldOfferOpenRouter({ configured: true, installedAgents: 2, usage: snapshot(["signed_out"]) })).toBe(false);
  });

  it("агентов на компьютере нет — предлагаем сразу", () => {
    expect(shouldOfferOpenRouter({ configured: false, installedAgents: 0, usage: null })).toBe(true);
  });

  it("все подписки без входа — это и есть тот случай", () => {
    const usage = snapshot(["signed_out", "unavailable"]);
    expect(shouldOfferOpenRouter({ configured: false, installedAgents: 2, usage })).toBe(true);
  });

  it("хотя бы одна рабочая подписка — молчим", () => {
    const usage = snapshot(["signed_out", "available"]);
    expect(shouldOfferOpenRouter({ configured: false, installedAgents: 2, usage })).toBe(false);
  });

  it("лимиты ещё не приехали — ничего не утверждаем", () => {
    expect(shouldOfferOpenRouter({ configured: false, installedAgents: 2, usage: null })).toBe(false);
    expect(shouldOfferOpenRouter({ configured: false, installedAgents: 2, usage: snapshot([]) })).toBe(false);
  });
});
