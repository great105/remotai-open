/**
 * Feature flags — runtime toggles persisted in localStorage.
 *
 * Default state hides AI-related UI (sessions, orchestrator, research,
 * templates, multi-send, discover, FAB) so the APK shows only Terminal /
 * Files / Remote / System / Settings — five "safe" features for App Store
 * and Play Store review.
 *
 * The user re-enables hidden UI via a secret 5-tap on the version label in
 * Settings → flips the `ai` flag → all gated views/routes/nav re-render.
 */

export interface Features {
  ai: boolean;
}

const STORAGE_KEY = "tgcontrol_features";
const DEFAULT: Features = { ai: false };

let _cache: Features = { ...DEFAULT };
let _listeners: Array<(f: Features) => void> = [];

// Sync init from localStorage on import — same pattern as config.ts:13-16.
try {
  const raw = localStorage.getItem(STORAGE_KEY);
  if (raw) {
    const parsed = JSON.parse(raw);
    if (parsed && typeof parsed === "object") {
      _cache = { ...DEFAULT, ...parsed };
    }
  }
} catch { /* ignore — keep defaults */ }

export function getFeatures(): Features {
  return _cache;
}

export function setFeatures(patch: Partial<Features>): void {
  _cache = { ..._cache, ...patch };
  try {
    localStorage.setItem(STORAGE_KEY, JSON.stringify(_cache));
  } catch { /* ignore quota errors */ }
  _listeners.forEach((cb) => cb(_cache));
}

export function onFeaturesChange(cb: (f: Features) => void): () => void {
  _listeners.push(cb);
  return () => {
    _listeners = _listeners.filter((l) => l !== cb);
  };
}
