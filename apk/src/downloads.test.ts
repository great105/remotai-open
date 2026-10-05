import { describe, expect, it } from "vitest";

import { desktopDownloadFor, detectDesktopOS } from "./downloads";

describe("detectDesktopOS", () => {
  it("узнаёт настольные системы", () => {
    expect(detectDesktopOS("Mozilla/5.0 (Windows NT 10.0; Win64; x64) Chrome/152")).toBe("windows");
    expect(detectDesktopOS("Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) Safari/605")).toBe("macos");
    // Ноутбук владельца 23.08: Ubuntu + WebKitGTK, именно с него пришла жалоба.
    expect(detectDesktopOS("Mozilla/5.0 (X11; Ubuntu; Linux x86_64) AppleWebKit/605.1.15 Version/60.5 Safari/605.1.15")).toBe("linux");
  });

  it("не принимает телефон за Linux-ПК", () => {
    // В Android UA стоит слово Linux — наивная проверка предлагала бы там
    // установку через curl.
    expect(detectDesktopOS("Mozilla/5.0 (Linux; Android 16; I2405) Chrome/152 Mobile Safari/537.36")).toBe("unknown");
    expect(detectDesktopOS("Mozilla/5.0 (iPhone; CPU iPhone OS 18_0 like Mac OS X) Safari/605")).toBe("unknown");
  });
});

describe("desktopDownloadFor", () => {
  it("даёт прямую ссылку на установщик Windows", () => {
    const d = desktopDownloadFor("windows");
    expect(d?.url).toMatch(/\/download\/remotai-setup\.exe$/);
    expect(d?.altUrl).toMatch(/\/download\/remotai\.exe$/);
  });

  it.each(["amd64", "arm64"] as const)("для Linux %s даёт оба установщика", arch => {
    const d = desktopDownloadFor("linux", arch);
    expect(d?.url).toMatch(new RegExp(`/download/remotai-linux-${arch}\\.deb$`));
    expect(d?.altUrl).toMatch(new RegExp(`/download/remotai-linux-${arch}\\.rpm$`));
    expect(d?.command).toBeUndefined();
  });

  it("для мака даёт один универсальный DMG без команды установки", () => {
    const d = desktopDownloadFor("macos");
    expect(d?.url).toMatch(/\/download\/remotai-macos\.dmg$/);
    expect(d?.command).toBeUndefined();
    expect(d?.guideUrl).toMatch(/\/app\/#\/start\?task=host&os=macos$/);
    expect(d?.altUrl).toBeUndefined();
  });

  it("на телефоне ничего не предлагает", () => {
    expect(desktopDownloadFor("unknown")).toBeNull();
  });
});
