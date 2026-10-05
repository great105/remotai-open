export type LocalComputer = { platform: "darwin" | "linux" | "windows" };

/** A localhost origin alone is not proof of an installed agent (e.g. Vite).
 * Require the actual setup API identity before hiding installation guidance. */
export function localComputerFromStatus(value: unknown): LocalComputer | null {
  if (!value || typeof value !== "object") return null;
  const status = value as Record<string, unknown>;
  if (typeof status.device_id !== "string" || !status.device_id ||
      typeof status.version !== "string" || !/^\d+\.\d+\.\d+$/.test(status.version)) return null;
  const platform = status.platform;
  if (platform !== "darwin" && platform !== "linux" && platform !== "windows") return null;
  return { platform };
}

/** Finish the same confirmed pairing as the native panel, including a machine
 * previously configured for LAN only. Never enable cloud before confirmation. */
export async function finishLocalPairing(code: string): Promise<void> {
  const status = await fetch("/api/setup/cloud/pair-status?code=" + encodeURIComponent(code), { cache: "no-store" });
  if (!status.ok || !(await status.json()).confirmed) throw new Error("local pairing not confirmed");
  const enabled = await fetch("/api/setup/cloud/enable", { method: "POST", headers: { "Content-Type": "application/json" }, body: "{}" });
  if (!enabled.ok) throw new Error("local cloud access not enabled");
}
