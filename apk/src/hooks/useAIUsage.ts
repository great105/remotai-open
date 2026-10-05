import { useEffect, useState, useSyncExternalStore } from "react";
import { selectedUsageResource } from "../aiUsage";
import { usageTime } from "../aiUsagePolicy";
import { usePolling } from "./usePolling";

export function useUsageNow(): number {
  const [now, setNow] = useState(Date.now);
  usePolling(() => setNow(Date.now()), 15_000);
  return now;
}

export function useAIUsage(enabled = true, poll = true) {
  const resource = selectedUsageResource();
  const result = useSyncExternalStore(resource.subscribe, resource.get);
  useEffect(() => { if (enabled && poll) void resource.load(); }, [enabled, poll, resource]);
  usePolling(() => resource.load(), 60_000, { enabled: enabled && poll });
  usePolling(() => {
    const now = Date.now();
    if (result.snapshot?.providers.some(p => !p.stale && p.windows.some(w =>
      !!w.resets_at && usageTime(p.captured_at) < w.resets_at * 1000 && now >= w.resets_at * 1000))) {
      return resource.load(true);
    }
  }, 15_000, { enabled: enabled && poll, immediate: false });
  return enabled ? result : {};
}
