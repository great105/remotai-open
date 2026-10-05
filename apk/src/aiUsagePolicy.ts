import { t } from "@tgcontrol/shared";
import type { AgentQuota } from "./types";
import type { AIProviderUsage, AIUsageSnapshot, AIUsageWindow } from "./aiUsage";

export function usageTime(value?: string): number {
  const n = Date.parse(value || "");
  return Number.isFinite(n) && n > 0 ? n : 0;
}

export function mainUsageWindows(provider: AIProviderUsage): { five?: AIUsageWindow; seven?: AIUsageWindow } {
  const result: { five?: AIUsageWindow; seven?: AIUsageWindow } = {};
  for (const window of provider.windows) {
    if (provider.id === "codex" && (window.limit_id || window.id.split(":")[0]) !== "codex") continue;
    if (provider.id === "claude" && !["five_hour", "seven_day"].includes(window.id)) continue;
    if (window.duration_minutes != null) {
      if (window.duration_minutes === 300) result.five ??= window;
      if (window.duration_minutes === 10080) result.seven ??= window;
      continue;
    }
    if (window.id === "five_hour") result.five ??= window;
    if (window.id === "seven_day") result.seven ??= window;
  }
  return result;
}

export function usageIsStale(provider: AIProviderUsage, now: number): boolean {
  const captured = usageTime(provider.captured_at);
  return !!provider.stale || !captured || now - captured > 120_000 || provider.windows.some(w =>
    !!w.resets_at && now >= w.resets_at * 1000 && captured < w.resets_at * 1000);
}

export function usageAgeLabel(provider: AIProviderUsage, now: number): string {
  const captured = usageTime(provider.captured_at);
  if (!captured) return t("usage.timeUnknown");
  const age = Math.max(0, Math.floor((now - captured) / 1000));
  const time = age < 60 ? t("usage.ageSeconds", { n: age })
    : age < 3600 ? t("usage.ageMinutes", { n: Math.floor(age / 60) })
    : t("usage.ageHours", { n: Math.floor(age / 3600) });
  return usageIsStale(provider, now) ? t("usage.staleAge", { time }) : t("usage.freshAge", { time });
}

export function preferUsage(current: AIProviderUsage, incoming: AIProviderUsage): AIProviderUsage {
  const aHasNumbers = current.windows.length > 0 || !!current.extra_usage?.enabled;
  const bHasNumbers = incoming.windows.length > 0 || !!incoming.extra_usage?.enabled;
  if (aHasNumbers !== bHasNumbers) return bHasNumbers ? incoming : current;
  const a = usageTime(current.captured_at) || usageTime(current.checked_at);
  const b = usageTime(incoming.captured_at) || usageTime(incoming.checked_at);
  return b > a || (b === a && current.stale && !incoming.stale) ? incoming : current;
}

export function quotaFromUsage(snapshot: AIUsageSnapshot, agentID: string): AgentQuota | undefined {
  const providers = snapshot.providers.filter(p => p.id === agentID);
  const p = providers.find(p => p.account_active) || providers[0];
  if (!p) return undefined;
  const { five, seven } = mainUsageWindows(p);
  const hasData = p.status === "available" || p.stale;
  return {
    status: p.status, message: p.message, captured_at: p.captured_at || snapshot.captured_at,
    stale: p.stale, account_id: p.account_id, account_label: p.account_label,
    five_hour_used: hasData ? five?.used_percent : undefined,
    seven_day_used: hasData ? seven?.used_percent : undefined,
    five_hour_resets_at: five?.resets_at, seven_day_resets_at: seven?.resets_at,
  };
}
