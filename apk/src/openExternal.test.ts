import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

const mocks = vi.hoisted(() => ({ native: true, launch: vi.fn(), telegram: vi.fn(), open: vi.fn(), pc: false }));
vi.mock("@capacitor/app-launcher", () => ({ AppLauncher: { openUrl: mocks.launch } }));
vi.mock("./config", () => ({ get isNativeApp() { return mocks.native; }, isOnPCPanel: () => mocks.pc, getServerUrl: () => "http://127.0.0.1:8080" }));
vi.mock("./telegram", () => ({ getTelegram: mocks.telegram }));
import { openExternalLink } from "./openExternal";

const link = "https://t.me/remotai_bot?start=login_abcdef";
describe("open Telegram login", () => {
  beforeEach(() => {
    vi.resetAllMocks(); mocks.native = true; mocks.pc = false;
    mocks.telegram.mockReturnValue(null); mocks.open.mockReturnValue({ opener: null });
    vi.stubGlobal("window", { open: mocks.open, location: { href: "app", origin: "http://127.0.0.1:8080" } });
  });
  afterEach(() => vi.unstubAllGlobals());
  it("opens installed Telegram without opening a browser", async () => {
    mocks.launch.mockResolvedValue({ completed: true });
    await openExternalLink(link);
    expect(mocks.launch).toHaveBeenCalledWith({ url: "tg://resolve?domain=remotai_bot&start=login_abcdef" });
    expect(mocks.open).not.toHaveBeenCalled();
    expect(window.location.href).toBe("app");
  });
  it.each(["missing", "error"])("keeps the original HTTPS fallback when native launch is %s", async mode => {
    if (mode === "missing") mocks.launch.mockResolvedValue({ completed: false });
    else mocks.launch.mockRejectedValue(new Error("no handler"));
    await openExternalLink(link);
    expect(mocks.open).toHaveBeenCalledWith(link, "_blank");
  });
  it("keeps Telegram Mini App navigation inside Telegram", async () => {
    const inside = vi.fn(); mocks.telegram.mockReturnValue({ openTelegramLink: inside });
    await openExternalLink(link);
    expect(inside).toHaveBeenCalledWith(link);
    expect(mocks.launch).not.toHaveBeenCalled(); expect(mocks.open).not.toHaveBeenCalled();
  });
  it("preserves the desktop agent's external navigation", async () => {
    mocks.native = false; mocks.pc = true;
    const fetch = vi.fn().mockResolvedValue({ ok: true, json: async () => ({ ok: true }) }); vi.stubGlobal("fetch", fetch);
    await openExternalLink(link);
    expect(fetch).toHaveBeenCalledWith("http://127.0.0.1:8080/api/setup/open-external", expect.objectContaining({ method: "POST" }));
    expect(mocks.launch).not.toHaveBeenCalled(); expect(mocks.open).not.toHaveBeenCalled();
  });
  it("keeps web links working without a native plugin", async () => {
    mocks.native = false;
    await openExternalLink(link);
    expect(mocks.launch).not.toHaveBeenCalled(); expect(mocks.open).toHaveBeenCalledWith(link, "_blank");
  });
  it("opens payment in the desktop browser while preserving the app window", async () => {
    mocks.native = false; mocks.pc = true;
    const fetch = vi.fn().mockResolvedValue({ ok: true, json: async () => ({ ok: true }) }); vi.stubGlobal("fetch", fetch);
    const payment = "https://yoomoney.ru/checkout/payments/v2/contract?orderId=unit";
    await openExternalLink(payment);
    expect(fetch).toHaveBeenCalledWith("http://127.0.0.1:8080/api/setup/open-external", expect.objectContaining({ body: JSON.stringify({ url: payment }) }));
    expect(window.location.href).toBe("app");
    expect(mocks.open).not.toHaveBeenCalled();
  });
  it("keeps links working when an old desktop returns HTML with status 200", async () => {
    mocks.native = false; mocks.pc = true;
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue({ ok: true, json: async () => { throw new SyntaxError("HTML fallback"); } }));
    await openExternalLink(link);
    expect(mocks.open).toHaveBeenCalledWith(link, "_blank");
    expect(window.location.href).toBe("app");
  });
});
