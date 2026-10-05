/**
 * Пути чужой файловой системы. Телефон одинаково ходит по Windows, Linux и
 * сетевым шарам, и ошибка здесь тихая: человек попадает не в ту папку и узнаёт
 * об этом, только увидев чужие файлы.
 */
import { describe, expect, it } from "vitest";
import { joinPath, parentOf, samePathKey, volumeKey } from "./paths";

describe("volumeKey", () => {
  it("Windows — буква диска в верхнем регистре", () => {
    expect(volumeKey("C:\\Users\\user")).toBe("C:");
    expect(volumeKey("d:/work")).toBe("D:");
  });

  it("сетевая шара — сервер и ресурс вместе", () => {
    expect(volumeKey("\\\\server\\share\\dir")).toBe("server/share");
    expect(volumeKey("//server/share/dir")).toBe("server/share");
  });

  it("POSIX — первый сегмент", () => {
    expect(volumeKey("/home/user/proj")).toBe("/home");
    expect(volumeKey("/")).toBe("/");
  });

  it("точки монтирования различаются: /mnt/a и /mnt/b — разные тома", () => {
    expect(volumeKey("/mnt/a/data")).toBe("/mnt/a");
    expect(volumeKey("/mnt/b/data")).toBe("/mnt/b");
    expect(volumeKey("/media/usb1")).toBe("/media/usb1");
    expect(volumeKey("/Volumes/Backup/x")).toBe("/Volumes/Backup");
  });
});

describe("samePathKey", () => {
  it("хвостовой разделитель не делает папку другой", () => {
    expect(samePathKey("C:\\work\\")).toBe(samePathKey("C:\\work"));
    expect(samePathKey("/home/user/")).toBe(samePathKey("/home/user"));
  });

  it("регистр на Windows не различает папки", () => {
    expect(samePathKey("C:\\Work")).toBe(samePathKey("c:\\work"));
  });

  it("корень остаётся корнем, а не пустой строкой", () => {
    expect(samePathKey("/")).toBe("/");
    expect(samePathKey("/").length).toBeGreaterThan(0);
  });
});

describe("joinPath", () => {
  it("разделитель берётся у самой папки", () => {
    expect(joinPath("C:\\work", "file.txt")).toBe("C:\\work\\file.txt");
    expect(joinPath("/home/user", "file.txt")).toBe("/home/user/file.txt");
  });

  it("двойного разделителя не появляется", () => {
    expect(joinPath("C:\\work\\", "a.txt")).toBe("C:\\work\\a.txt");
    expect(joinPath("/home/", "a.txt")).toBe("/home/a.txt");
  });

  it("смешанный путь (сетевой, с обоими разделителями) остаётся рабочим", () => {
    expect(joinPath("//server/share", "a.txt")).toBe("//server/share/a.txt");
  });
});

describe("parentOf", () => {
  it("обычный подъём вверх", () => {
    expect(parentOf("C:\\work\\proj")).toBe("C:\\work");
    expect(parentOf("/home/user/proj")).toBe("/home/user");
  });

  it("корень диска не превращается в пустоту", () => {
    expect(parentOf("C:\\work")).toBe("C:\\");
    expect(parentOf("/home")).toBe("/");
  });

  it("выше корня не поднимаемся", () => {
    expect(parentOf("/")).toBe("/");
    expect(parentOf("C:")).toBe("C:");
  });

  it("хвостовой разделитель не мешает", () => {
    expect(parentOf("/home/user/")).toBe("/home");
  });
});
