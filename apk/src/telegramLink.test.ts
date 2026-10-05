import { describe, expect, it } from "vitest";
import { telegramLoginAppLink } from "./telegramLink";

describe("Telegram bot login app link", () => {
  it("keeps the bot and the entire one-time login payload", () => {
    expect(telegramLoginAppLink("https://t.me/remotai_bot?start=login_aB09_-"))
      .toBe("tg://resolve?domain=remotai_bot&start=login_aB09_-");
    expect(telegramLoginAppLink("https://telegram.me/remotai_bot/?start=login_0123"))
      .toBe("tg://resolve?domain=remotai_bot&start=login_0123");
  });
  it.each([
    "https://t.me.evil.invalid/remotai_bot?start=login_123",
    "https://t.me@evil.invalid/remotai_bot?start=login_123",
    "https://user:pass@t.me/remotai_bot?start=login_123",
    "http://t.me/remotai_bot?start=login_123",
    "https://t.me:8443/remotai_bot?start=login_123",
    "https://t.me/remotai_bot?start=login_123&domain=other_bot",
    "https://t.me/remotai_bot?start=login_123&start=login_456",
    "https://t.me/remotai_bot?start=login_123#other",
    "https://t.me/remotai_bot?start=login_foo%26domain%3Dother",
    "https://t.me/remotai_bot", "https://t.me/share/url?url=x", "not a URL",
  ])("leaves unsupported or ambiguous URL unchanged: %s", value => {
    expect(telegramLoginAppLink(value)).toBeNull();
  });
});
