/**
 * Правила экрана «Мои компьютеры». Каждый случай здесь — состояние, которое
 * владелец однажды увидел на своём телефоне и назвал поломкой.
 */
import { describe, expect, it } from "vitest";
import {
  agentUpdateState,
  compareVersions,
  isSimpleFleet,
  populatedWorkspaceCount,
  type FleetDevice,
} from "./fleetRules";

const SHARED = "__shared__";

function device(over: Partial<FleetDevice> = {}): FleetDevice {
  return { workspace_id: "w1", agent_version: "2.41.2", online: true, ...over };
}

describe("compareVersions", () => {
  it("сравнивает по трём числам", () => {
    expect(compareVersions("2.41.2", "2.41.1")).toBe(1);
    expect(compareVersions("2.41.2", "2.41.2")).toBe(0);
    expect(compareVersions("2.9.9", "2.10.0")).toBe(-1);
  });

  it("не путает порядок при разной длине: 2.41 и 2.41.0 — одно и то же", () => {
    expect(compareVersions("2.41", "2.41.0")).toBe(0);
  });

  it("мусор в версии не делает её новее", () => {
    expect(compareVersions("dev", "2.41.2")).toBe(-1);
  });
});

describe("agentUpdateState", () => {
  it("без версии агента или без манифеста релея — сравнивать не с чем", () => {
    expect(agentUpdateState(device({ agent_version: "" }), "2.41.2")).toBe("unknown");
    expect(agentUpdateState(device({ agent_version: null }), "2.41.2")).toBe("unknown");
    expect(agentUpdateState(device(), "")).toBe("unknown");
  });

  it("версия не отстала (в том числе новее релиза) — current", () => {
    expect(agentUpdateState(device({ agent_version: "2.41.2" }), "2.41.2")).toBe("current");
    expect(agentUpdateState(device({ agent_version: "2.42.0" }), "2.41.2")).toBe("current");
  });

  it("отстала в пределах мажора — обновится сама, красного быть не должно", () => {
    expect(agentUpdateState(device({ agent_version: "2.39.0" }), "2.41.2")).toBe("auto");
  });

  it("выключенная машина не «застряла» — она просто ещё не обновлялась", () => {
    expect(agentUpdateState(device({ agent_version: "1.9.0", online: false }), "2.41.2")).toBe("auto");
  });

  it("в сети и отстала на мажор — вот это stuck, и только это красное", () => {
    expect(agentUpdateState(device({ agent_version: "1.9.0", online: true }), "2.41.2")).toBe("stuck");
  });
});

describe("populatedWorkspaceCount", () => {
  const direct = new Set(["w1", "w2"]);

  it("пустое пространство не считается: две записи, машины в одной", () => {
    const devices = [device({ workspace_id: "w1" }), device({ workspace_id: "w1" })];
    expect(populatedWorkspaceCount(devices, direct, SHARED)).toBe(1);
  });

  it("машины в двух своих пространствах — выбор действительно есть", () => {
    const devices = [device({ workspace_id: "w1" }), device({ workspace_id: "w2" })];
    expect(populatedWorkspaceCount(devices, direct, SHARED)).toBe(2);
  });

  it("чужие пространства схлопываются в один «Общий доступ»", () => {
    const devices = [device({ workspace_id: "alien-1" }), device({ workspace_id: "alien-2" })];
    expect(populatedWorkspaceCount(devices, direct, SHARED)).toBe(1);
  });

  it("нет устройств — нет и пространств", () => {
    expect(populatedWorkspaceCount([], direct, SHARED)).toBe(0);
    expect(populatedWorkspaceCount(null, direct, SHARED)).toBe(0);
  });
});

describe("isSimpleFleet", () => {
  it("один компьютер без зон — организационный слой не показываем", () => {
    expect(isSimpleFleet(1, 0)).toBe(true);
    expect(isSimpleFleet(0, 0)).toBe(true);
  });

  it("вторая занятая зона или второе занятое пространство раскрывают слой", () => {
    expect(isSimpleFleet(2, 0)).toBe(false);
    expect(isSimpleFleet(1, 1)).toBe(false);
  });
});
