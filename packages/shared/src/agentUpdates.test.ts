import { describe, expect, it } from "vitest";
import {
  compareVersions, ownerLabel, runningWarning, updateFinished, updateLine, versionEventText,
  type CliAgentUpdate,
} from "./agentUpdates";

function info(over: Partial<CliAgentUpdate>): CliAgentUpdate {
  return {
    id: "kimi", name: "Kimi Code", path: "C:\\Users\\user\\AppData\\Roaming\\npm\\kimi.cmd",
    version: "0.37.2", latest: "2.1.1", owner: "npm", owner_title: "npm",
    update_command: "npm.cmd i -g @moonshot-ai/kimi-code@latest", can_update: true,
    latest_known: true, update_available: true, running_sessions: 0, checked_at: 0,
    ...over,
  };
}

describe("compareVersions", () => {
  it.each([
    ["2.1.3", "2.1.5", -1],
    ["2.1.10", "2.1.9", 1],
    ["0.37.2", "2.1.1", -1],
    ["1.0.4", "1.0.41", -1], // grok на машине владельца 29.09.2026
    ["1.2", "1.2.0", 0],
    ["1.2.0-beta.1", "1.2.0", -1],
    ["v1.2.0", "1.2.0", 0],
    ["", "1.0.0", -1],
  ])("%s vs %s = %i", (a, b, want) => {
    expect(compareVersions(a, b)).toBe(want);
  });
});

describe("updateLine", () => {
  it("есть обновление: версии и владелец в одной строке, кнопка есть", () => {
    const l = updateLine(info({}));
    expect(l.kind).toBe("update");
    expect(l.text).toBe("Есть обновление 0.37.2 → 2.1.1 · npm");
    expect(l.canUpdate).toBe(true);
  });

  it("последняя версия — тихая строка без кнопки", () => {
    const l = updateLine(info({ version: "2.1.284", latest: "2.1.284", update_available: false }));
    expect(l.kind).toBe("current");
    expect(l.text).toBe("2.1.284 · npm · последняя");
    expect(l.canUpdate).toBe(false);
  });

  it("нативный Claude: последнюю версию проверит установщик, кнопка есть", () => {
    const l = updateLine(info({ owner: "claude-native", owner_title: "", latest: "", latest_known: false, update_command: "claude.exe update" }));
    expect(l.kind).toBe("installer");
    expect(l.text).toContain("установщик Claude");
    expect(l.hint).toMatch(/установщик/);
    expect(l.canUpdate).toBe(true);
  });

  it("неизвестный владелец — честная причина и никакой кнопки", () => {
    const l = updateLine(info({ owner: "unknown", can_update: false, update_command: "", reason: "Не знаем, чем он установлен" }));
    expect(l.kind).toBe("cannot");
    expect(l.canUpdate).toBe(false);
    expect(l.hint).toBe("Не знаем, чем он установлен");
  });

  it("реестр не ответил — без кнопки и с пояснением", () => {
    const l = updateLine(info({ latest: "", latest_error: "timeout" }));
    expect(l.kind).toBe("unknown");
    expect(l.canUpdate).toBe(false);
    expect(l.hint).toBeTruthy();
  });

  it("клиент не верит флагу сервера, если версии говорят обратное", () => {
    const l = updateLine(info({ version: "2.1.1", latest: "2.1.1", update_available: true }));
    expect(l.kind).toBe("current");
  });
});

describe("ownerLabel", () => {
  it("знает всех владельцев и не показывает ключ словаря", () => {
    for (const o of ["npm", "pnpm", "bun", "volta", "claude-native", "brew", "winget", "scoop", "unknown"]) {
      expect(ownerLabel(o)).not.toMatch(/agentUpdate|⟦/);
    }
    expect(ownerLabel("nix", "Nix")).toBe("Nix");
  });
});

describe("runningWarning", () => {
  it("молчит, если агент не запущен", () => {
    expect(runningWarning(info({ running_sessions: 0 }))).toBe("");
  });
  it("склоняет число терминалов", () => {
    expect(runningWarning(info({ running_sessions: 1 }))).toContain("в 1 терминале");
    expect(runningWarning(info({ running_sessions: 3 }))).toContain("в 3 терминалах");
  });
});

describe("versionEventText", () => {
  it("смена и первый замер", () => {
    const now = new Date(2026, 8, 29, 15, 0);
    const at = new Date(2026, 8, 29, 14, 2).getTime();
    expect(versionEventText({ at, from: "2.1.3", to: "2.1.5", owner: "npm" }, now)).toBe("29.09, 14:02 · 2.1.3 → 2.1.5");
    expect(versionEventText({ at, from: "", to: "2.1.3", owner: "npm" }, now)).toBe("29.09, 14:02 · замечена 2.1.3");
    const old = new Date(2025, 0, 2, 3, 4).getTime();
    expect(versionEventText({ at: old, from: "1", to: "2", owner: "npm" }, now)).toMatch(/^02\.01\.2025, 03:04/);
  });
});

describe("updateFinished", () => {
  it("в первые секунды «idle» ещё не конец", () => {
    expect(updateFinished("idle", true, 2000)).toBe(false);
    expect(updateFinished("working", true, 60000)).toBe(false);
    expect(updateFinished("idle", true, 20000)).toBe(true);
    expect(updateFinished(undefined, false, 100)).toBe(true);
  });
});
