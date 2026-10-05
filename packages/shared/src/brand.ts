// Single source of truth for product identity. Both the Mini App and the APK
// import these so the visible brand never drifts between the two frontends
// (the audit found APK = "Remotai" vs Mini App = "TGControl" — fixed here).
//
// Decision (2026-06-08): unify on "Remotai" (matches the relay domain remotai.ru).

export const APP_NAME = "Remotai";

/** Cloud relay base URL (used by cloud-mode pairing / onboarding). */
const buildRelay = (import.meta as ImportMeta & { env?: Record<string, string> }).env?.VITE_RELAY_BASE;
export const RELAY_BASE = buildRelay?.replace(/\/+$/, "") || "https://remotai.ru";

/** Support contact (Telegram). */
export const SUPPORT_URL = "https://t.me/Autocode1_bot";
