/**
 * Shared cloud-pairing helpers used by both the in-app QR scanner (ScanView)
 * and the `remotai://pair` deep-link handler (App). Keeping the parse + confirm
 * logic in one place means a deep link and a scanned QR pair identically.
 */
import { setNetworkContext, toNetworkError } from "@tgcontrol/shared";
import { saveConfig } from "../config";
import { resetCapabilities } from "../capabilities";
import { pairNative, type DeviceType } from "./api";
import { connectWS } from "../api";
import { tlog } from "../debuglog";
import { trackAppOpen, trackPairSuccess, trackRegisterSource } from "./support";

/** Parse a `remotai://pair?relay=<url>&code=<code>` payload (QR or deep link). */
export function parsePairPayload(data: string): { relay: string; code: string } | null {
  if (!data || !data.includes("pair")) return null;
  try {
    const u = new URL(data.trim());
    const relay = u.searchParams.get("relay");
    const code = u.searchParams.get("code");
    if (relay && code) return { relay, code };
  } catch { /* fall through to regex */ }
  const rel = /[?&]relay=([^&]+)/.exec(data);
  const cod = /[?&]code=([^&]+)/.exec(data);
  if (rel && cod) {
    return { relay: decodeURIComponent(rel[1]), code: decodeURIComponent(cod[1]) };
  }
  return null;
}

/**
 * Confirm a pairing code against the relay, persist the cloud config and open
 * the live event WS. Throws on failure (caller maps the error for the UI).
 */
export async function runPair(
  relay: string,
  code: string,
  options: { workspaceId?: string; deviceType?: DeviceType; zoneId?: string } = {},
): Promise<{ deviceId: string }> {
  const base = relay.trim().replace(/\/+$/, "");
  const c = code.trim().toUpperCase();
  tlog("pair:start", { relay: base, code: c });
  try {
    const r = await pairNative(base, c, options);
    setNetworkContext({ route: "cloud" }); // дальше приложение работает через облако
    saveConfig({
      mode: "cloud",
      relayBase: base,
      jwt: r.user_jwt,
      jwtExpiresAt: Date.parse(r.expires_at) || undefined,
      selectedDeviceId: r.device_id,
      selectedDeviceType: r.device_type || options.deviceType,
      selectedWorkspaceId: r.workspace_id || options.workspaceId,
    });
    trackAppOpen();
    trackRegisterSource();
    trackPairSuccess();
    // Новое устройство — новый контекст: возможности прежней машины
    // (дисплей, виртуальный браузер) к нему не относятся.
    resetCapabilities();
    connectWS(); // open the live event stream immediately after pairing
    tlog("pair:ok", { deviceId: r.device_id });
    return { deviceId: r.device_id };
  } catch (e) {
    tlog("pair:error", { message: (e as Error)?.message, status: (e as { status?: number })?.status });
    // pairNative ходит голым fetch: без обёртки обрыв сети/VPN приходил в
    // мастер подключения как английское «Failed to fetch» (UX-аудит N63).
    throw toNetworkError(e, "cloud");
  }
}
