/** Convert only Telegram bot-login links; preserve the one-time start payload. */
export function telegramLoginAppLink(value: string): string | null {
  try {
    const url = new URL(value);
    if (url.protocol !== "https:" || !["t.me", "telegram.me"].includes(url.hostname.toLowerCase())
      || url.username || url.password || url.port || url.hash) return null;
    const username = /^\/([A-Za-z0-9_]{5,32})\/?$/.exec(url.pathname)?.[1];
    const start = url.searchParams.get("start");
    if (!username || !start || !/^login_[A-Za-z0-9_-]+$/.test(start)) return null;
    if ([...url.searchParams.keys()].some(key => key !== "start") || url.searchParams.getAll("start").length !== 1) return null;
    const query = new URLSearchParams({ domain: username, start });
    return `tg://resolve?${query}`;
  } catch { return null; }
}
