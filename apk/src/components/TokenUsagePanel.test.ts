import { describe, expect, it } from "vitest";
import { compactTokens, shortPath } from "./TokenUsagePanel";

describe("compactTokens", () => {
  it("keeps small numbers as is", () => {
    expect(compactTokens(0)).toBe("0");
    expect(compactTokens(950)).toBe("950");
  });
  it("uses Russian units with a comma and trims zeros", () => {
    expect(compactTokens(1_339_732_609)).toBe("1,34 млрд");
    expect(compactTokens(25_900_000)).toBe("25,9 млн");
    expect(compactTokens(154_000_000)).toBe("154 млн");
    expect(compactTokens(2_000)).toBe("2 тыс.");
  });
});

describe("shortPath", () => {
  it("shows the last two folders of a Windows path", () => {
    expect(shortPath("C:\\Users\\user\\Desktop\\Проекты\\Аэрофлот")).toBe("…/Проекты/Аэрофлот");
  });
  it("keeps short and POSIX paths readable", () => {
    expect(shortPath("/home/dev")).toBe("home/dev");
    expect(shortPath("/srv/app/api")).toBe("…/app/api");
  });
  it("names a session without a folder", () => {
    expect(shortPath("")).toBe("без папки");
  });
});
