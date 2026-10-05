import { describe, expect, it } from "vitest";
import { serverAccessState } from "./serverAccess";
import { mapApiError } from "./api-core";

describe("server access", () => {
  const free = { allowed: false, trial_available: false, tier: "free" };
  it("distinguishes account, trial, expired access, and verification failure", () => {
    expect(serverAccessState(null)).toBe("loading");
    expect(serverAccessState({ ...free, code: "server_account_required" })).toBe("account");
    expect(serverAccessState({ ...free, trial_available: true })).toBe("trial");
    expect(serverAccessState(free)).toBe("subscription");
    expect(serverAccessState({ ...free, allowed: true, tier: "pro" })).toBe("allowed");
    expect(serverAccessState(null, "subscription_required")).toBe("subscription");
    expect(serverAccessState({ ...free, allowed: true }, "server_access_unavailable")).toBe("unavailable");
  });
  it("explains billing errors without blaming SSH credentials", () => {
    expect(mapApiError({ code: "server_subscription_required", status: 402 })).toContain("подписка Про");
    expect(mapApiError({ code: "server_account_required", status: 402 })).toContain("аккаунту");
    expect(mapApiError({ code: "server_access_unavailable", status: 503 })).toContain("проверить подписку");
  });
});
