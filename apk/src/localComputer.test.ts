import { afterEach, describe, expect, it, vi } from "vitest";
import { finishLocalPairing, localComputerFromStatus } from "./localComputer";

afterEach(() => vi.unstubAllGlobals());

describe("installed local computer identity", () => {
  it.each(["darwin", "linux", "windows"])("recognizes the actual %s setup response", platform => {
    expect(localComputerFromStatus({ platform, device_id: "qa", version: "2.55.18" })?.platform).toBe(platform);
  });
  it.each([null, "<html>Vite</html>", {}, { platform: "darwin" },
    { platform: "darwin", version: "2.66.8" },
    { platform: "darwin", device_id: "qa", version: "" },
    { platform: "android", device_id: "qa", version: "2.66.8" },
  ])("does not confuse another local server with Remotai: %j", value => {
    expect(localComputerFromStatus(value)).toBeNull();
  });
  it("never enables cloud access when local pairing is not confirmed", async () => {
    const request = vi.fn().mockResolvedValue({ ok: true, json: async () => ({ confirmed: false }) });
    vi.stubGlobal("fetch", request);
    await expect(finishLocalPairing("AB12-CD34")).rejects.toThrow();
    expect(request).toHaveBeenCalledTimes(1);
    expect(request.mock.calls[0][0]).toContain("/pair-status?");
  });
});
