/**
 * Как зовут машину и что считается локальным адресом.
 *
 * «127.0.0.1 на связи» стояло в шапке всех рабочих экранов, пока фильтр имени
 * жил в одной карточке: адрес именем не является. Правило общее для ReadyCard,
 * DeviceChip и DeviceSwitcher — значит и проверяется один раз здесь.
 */
import { describe, expect, it } from "vitest";
import { humanDeviceName, isLocalAddress } from "./devices";

describe("humanDeviceName", () => {
  it("настоящее имя показываем как есть", () => {
    expect(humanDeviceName("Прод-сервер", "Этот компьютер")).toBe("Прод-сервер");
  });

  it("адрес вместо имени — это не имя", () => {
    expect(humanDeviceName("127.0.0.1", "Этот компьютер")).toBe("Этот компьютер");
    expect(humanDeviceName("192.168.1.5", "Этот компьютер")).toBe("Этот компьютер");
    expect(humanDeviceName("192.168.1.5:8080", "Этот компьютер")).toBe("Этот компьютер");
  });

  it("пустое имя и пробелы — тоже нечем назвать", () => {
    expect(humanDeviceName("", "Этот компьютер")).toBe("Этот компьютер");
    expect(humanDeviceName("   ", "Этот компьютер")).toBe("Этот компьютер");
    expect(humanDeviceName(null, "Этот компьютер")).toBe("Этот компьютер");
    expect(humanDeviceName(undefined, "Этот компьютер")).toBe("Этот компьютер");
  });

  it("пустой fallback означает «назвать нечем» — вызывающий решит сам", () => {
    expect(humanDeviceName("127.0.0.1", "")).toBe("");
  });
});

describe("isLocalAddress", () => {
  it("вся сеть 127.0.0.0/8, а не один адрес", () => {
    expect(isLocalAddress("127.0.0.1")).toBe(true);
    expect(isLocalAddress("http://127.0.1.1:8080")).toBe(true);
    expect(isLocalAddress("localhost")).toBe(true);
    expect(isLocalAddress("http://[::1]:8080/")).toBe(true);
  });

  it("адрес в локальной сети локальным для агента не считается", () => {
    expect(isLocalAddress("192.168.1.5")).toBe(false);
    expect(isLocalAddress("https://remotai.ru")).toBe(false);
    expect(isLocalAddress("")).toBe(false);
    expect(isLocalAddress(null)).toBe(false);
  });
});
