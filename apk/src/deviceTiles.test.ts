/**
 * Порядок и тон плиток машин на главной.
 *
 * Ряд из жёлтых плиток («везде что-то не так») и потерянная среди них машина,
 * которая ЖДЁТ ответа, — это был не баг разметки, а правило. Здесь оно и
 * закреплено: место в очереди и цвет — разные вещи.
 */
import { describe, expect, it } from "vitest";
import { orderTiles, tileState, TONE_CLASS, TONE_RANK } from "./deviceTiles";
import type { CloudDevice } from "./cloud/api";
import type { DeviceRuntimeSummary } from "./deviceSummaries";

function dev(id: string, over: Partial<CloudDevice> = {}): CloudDevice {
  return { id, name: id, online: true, ...over } as CloudDevice;
}

function summary(over: Partial<DeviceRuntimeSummary> = {}): DeviceRuntimeSummary {
  return {
    waitingCount: 0,
    errorCount: 0,
    workingCount: 0,
    terminalCount: 0,
    sessions: [],
    ...over,
  } as DeviceRuntimeSummary;
}

describe("tileState", () => {
  it("выключенная машина говорит, когда её видели", () => {
    const state = tileState(dev("pc", { online: false, last_seen_at: new Date(Date.now() - 3600_000).toISOString() }));
    expect(state.tone).toBe("offline");
    expect(state.text).not.toBe("");
  });

  it("сводка ещё не доехала — «проверяю», а не «свободен»", () => {
    expect(tileState(dev("pc")).tone).toBe("idle");
  });

  it("вопрос агента — единственное, что тревожит", () => {
    expect(tileState(dev("pc"), summary({ waitingCount: 1 })).tone).toBe("attention");
    expect(tileState(dev("pc"), summary({ errorCount: 1 })).tone).toBe("attention");
  });

  it("работающий агент — не тревога: он печатает сам", () => {
    const state = tileState(dev("pc"), summary({ workingCount: 1 }));
    expect(state.tone).toBe("working");
    expect(TONE_CLASS[state.tone]).toBe("");
  });

  it("свободная машина с открытыми терминалами и без них — обе спокойны", () => {
    expect(tileState(dev("pc"), summary({ terminalCount: 2 })).tone).toBe("idle");
    expect(tileState(dev("pc"), summary()).tone).toBe("idle");
  });
});

describe("orderTiles", () => {
  const nameOf = (device: CloudDevice) => device.name;

  it("зовут → работают → свободны → офлайн", () => {
    const devices = [
      dev("офлайн", { online: false }),
      dev("свободна"),
      dev("зовёт"),
      dev("работает"),
    ];
    const summaries = {
      "зовёт": summary({ waitingCount: 1 }),
      "работает": summary({ workingCount: 1 }),
      "свободна": summary(),
    };
    expect(orderTiles(devices, summaries, nameOf).map((tile) => tile.device.id))
      .toEqual(["зовёт", "работает", "свободна", "офлайн"]);
  });

  it("при равном состоянии порядок стабилен — по имени, а не как пришло с релея", () => {
    const devices = [dev("Ноутбук"), dev("Ватсон"), dev("Прод")];
    const summaries = { "Ноутбук": summary(), "Ватсон": summary(), "Прод": summary() };
    expect(orderTiles(devices, summaries, nameOf).map((tile) => tile.device.id))
      .toEqual(["Ватсон", "Ноутбук", "Прод"]);
  });

  it("ранги не переставлены местами", () => {
    expect(TONE_RANK.attention).toBeLessThan(TONE_RANK.working);
    expect(TONE_RANK.working).toBeLessThan(TONE_RANK.idle);
    expect(TONE_RANK.idle).toBeLessThan(TONE_RANK.offline);
  });
});
