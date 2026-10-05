import { useEffect, useState } from "react";
import { getConfig } from "../api";
import { getMode } from "../config";

// Module-level cache: one /api/config fetch per app session is enough —
// bot availability doesn't change while the app is open.
let cached: boolean | null = null;

/**
 * Whether the connected PC has a Telegram bot configured. When false, UI
 * hides «Отправить в Telegram» actions (cloud installs typically run
 * without a bot, and the action would just error out).
 * Optimistically true until known, so the button never flashes in/out on
 * fast networks.
 */
export function useBotAvailable(): boolean {
  const cloud = getMode() === "cloud";
  const [available, setAvailable] = useState(cached ?? true);
  // В облаке документ отправляет релейный бот, локальный bot token на ПК не
  // нужен. Если аккаунт ещё не связан с Telegram, сам эндпоинт вернёт честный
  // telegram_not_linked вместо того, чтобы навсегда скрывать кнопку.
  useEffect(() => {
    if (cloud) { setAvailable(true); return; }
    if (cached !== null) { setAvailable(cached); return; }
    let cancelled = false;
    getConfig()
      .then((c) => {
        cached = c.bot_available !== false;
        if (!cancelled) setAvailable(cached);
      })
      .catch(() => { /* keep optimistic default */ });
    return () => { cancelled = true; };
  }, [cloud]);

  return cloud || available;
}
