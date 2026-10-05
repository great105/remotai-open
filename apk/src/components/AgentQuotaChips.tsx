import { t } from "@tgcontrol/shared";
import type { AgentQuota } from "../types";
import { useAIUsage, useUsageNow } from "../hooks/useAIUsage";
import { quotaFromUsage, usageTime } from "../aiUsagePolicy";

/**
 * Чип квоты подписки на профиле агента: «42% 5ч», «17% 7д».
 *
 * ЧИСЛО НА ЧИПЕ — ОСТАТОК, а не расход. Агент отдаёт израсходованное
 * (`five_hour_used`), и чип его же и показывал: рядом с «осталось 9% за 5 ч» в
 * аккаунтах стояло «91% 5ч» про тот же лимит — одна величина двумя шкалами
 * (UX-аудит 2026-08-23, п. 2.4). Считаем по-прежнему из расхода, показываем
 * остаток; порог подсветки тоже перевёрнут — «горячо» это МАЛО осталось.
 *
 * Вендор квоту не отдал — серый «?» с причиной в title: неизвестная квота
 * не равна нулевой (unknown cost is never $0), поэтому «0%» не рисуем.
 */
export function AgentQuotaChips({ quota: initial, agentName, agentID }: { quota?: AgentQuota | null; agentName: string; agentID: string }) {
  const usage = useAIUsage(true, false);
  const now = useUsageNow();
  const quota = usage.snapshot ? quotaFromUsage(usage.snapshot, agentID) : initial ? { status: "unknown" } as AgentQuota : null;
  if (!quota) return null;

  const fallback = t("quota.unknownTitle", { name: agentName });
  if (quota.status !== "available" && !quota.stale) {
    const reason = quota.message?.trim() || fallback;
    return (
      <span className="quota-chip quota-chip-na" title={reason} aria-label={reason}>?</span>
    );
  }

  const chips: { key: "5h" | "7d"; pct: number; title: string }[] = [];
  const captured = usageTime(quota.captured_at);
  const stale = quota.stale || !captured || now - captured > 120_000 ||
    [quota.five_hour_resets_at, quota.seven_day_resets_at].some(reset => reset && captured < reset * 1000 && now >= reset * 1000);
  if (quota.five_hour_used != null) {
    const pct = Math.max(0, Math.round(100 - quota.five_hour_used));
    chips.push({ key: "5h", pct, title: t("quota.fiveHourTitle", { name: agentName, pct }) });
  }
  if (quota.seven_day_used != null) {
    const pct = Math.max(0, Math.round(100 - quota.seven_day_used));
    chips.push({ key: "7d", pct, title: t("quota.sevenDayTitle", { name: agentName, pct }) });
  }
  if (chips.length === 0) {
    // available, но без единого процента — тоже «нет данных», а не 0.
    const reason = quota.message?.trim() || fallback;
    return (
      <span className="quota-chip quota-chip-na" title={reason} aria-label={reason}>?</span>
    );
  }

  return (
    <span className={`quota-chips${stale ? " quota-stale" : ""}`}>
      {chips.map((chip) => (
        <span
          key={chip.key}
          className={`quota-chip${chip.pct <= 10 ? " quota-chip-hot" : ""}`}
          title={`${chip.title}${stale ? ` · ${t("usage.staleShort")}` : ""}`}
        >
          {chip.pct}% {t(chip.key === "5h" ? "quota.fiveHour" : "quota.sevenDay")}
        </span>
      ))}
      {stale && <small>{t("usage.staleShort")}</small>}
    </span>
  );
}
