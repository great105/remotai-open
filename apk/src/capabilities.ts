/**
 * Server capabilities — fetched once from /api/system/service, cached in-module,
 * broadcast to subscribers. Unlike features.ts (localStorage user toggles) these
 * reflect what the *connected agent's machine* can actually do — e.g. Remote
 * Desktop is impossible on a headless server with no display, so the Remote tab
 * and its shortcuts are hidden.
 */
import { getServiceStatus } from "./api";
import {
  getMode,
  getSelectedDeviceId,
  getServerUrl,
  hasServerConfig,
} from "./config";

export interface Capabilities {
  /** Name reported by this device; shares its identity-scoped capability cache. */
  hostname?: string;
  /** OS connected agent reports ("windows", "linux", "darwin"). */
  platform: string;
  /** Remote Desktop is usable (a capturable display exists, not headless). */
  remoteDesktop: boolean;
  /** Agent has a capturable display right now (real or virtual). */
  hasDisplay: boolean;
  /** Linux agent can raise a virtual display (Xvfb + browser) on demand. */
  vbrowserAvailable: boolean;
  /** Virtual display session is up. */
  vbrowserRunning: boolean;
}

// Optimistic default: assume a display exists (the common desktop/phone-to-PC
// case) so the Remote tab doesn't flicker in on every normal machine. A headless
// server flips it off once the status arrives.
const DEFAULT: Capabilities = {
  platform: "",
  remoteDesktop: true,
  hasDisplay: true,
  vbrowserAvailable: false,
  vbrowserRunning: false,
};

let _cache: Capabilities = { ...DEFAULT };
let _loaded = false;
let _inflight: Promise<void> | null = null;
let _listeners: Array<(c: Capabilities) => void> = [];
let _identity = "";

function currentIdentity(): string {
  return getMode() === "cloud"
    ? `cloud:${getSelectedDeviceId() || "none"}`
    : `lan:${getServerUrl() || "none"}`;
}

/** DeviceId/URL — часть ключа кэша: ответ одной машины не переживает смену
 * контекста, даже если конкретный вызывающий забыл resetCapabilities(). */
function syncIdentity(): void {
  const next = currentIdentity();
  if (_identity === next) return;
  _identity = next;
  _cache = { ...DEFAULT };
  _loaded = false;
  _inflight = null;
}

export function getCapabilities(): Capabilities {
  syncIdentity();
  return _cache;
}

export function onCapabilitiesChange(cb: (c: Capabilities) => void): () => void {
  _listeners.push(cb);
  return () => {
    _listeners = _listeners.filter((l) => l !== cb);
  };
}

/**
 * Lazily fetch capabilities once. Safe to call repeatedly — concurrent calls
 * share one request, and a success is cached for the session. On error we keep
 * the optimistic default (don't hide the tab just because a probe hiccuped).
 */
export function ensureCapabilities(): Promise<void> {
  syncIdentity();
  // The app shell also renders on /login and /cloud-login. Until the user has
  // authenticated (and, in cloud mode, selected a machine), there is no agent
  // that can answer this endpoint. A relative self-hosted request would
  // otherwise hit remotai.ru/api/system/service and leave a noisy 404.
  if (!hasServerConfig()) return Promise.resolve();
  if (getMode() === "cloud" && !getSelectedDeviceId()) return Promise.resolve();
  if (_loaded) return Promise.resolve();
  if (_inflight) return _inflight;
  const requestIdentity = _identity;
  _inflight = getServiceStatus()
    .then((st) => {
      if (requestIdentity !== currentIdentity()) return;
      // Undefined (old agent) → treat as supported; only an explicit false hides it.
      // vbrowser.available keeps the Remote tab: no display yet, but the agent
      // can raise a virtual one (Xvfb + browser) from the start panel.
      const vbrowserAvailable = !!st?.vbrowser?.available;
      const vbrowserRunning = !!st?.vbrowser?.running;
      const hasDisplay = st?.has_display !== false || vbrowserRunning;
      const remoteDesktop = st?.remote_desktop_supported !== false || vbrowserAvailable;
      _cache = {
        hostname: st?.hostname || "",
        platform: (st?.os || "").toLowerCase(),
        remoteDesktop,
        hasDisplay,
        vbrowserAvailable,
        vbrowserRunning,
      };
      _loaded = true;
      _listeners.forEach((cb) => cb(_cache));
    })
    .catch(() => {
      /* keep optimistic default — a failed probe must not hide working UI */
    })
    .finally(() => {
      _inflight = null;
    });
  return _inflight;
}

/** Force a re-fetch (e.g. after switching the selected device). */
export function resetCapabilities(): void {
  _identity = currentIdentity();
  _cache = { ...DEFAULT };
  _loaded = false;
  _inflight = null;
  _listeners.forEach((cb) => cb(_cache));
}
