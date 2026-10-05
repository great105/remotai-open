/**
 * Слепок версии агента — тот самый выключатель, от которого зависит, стирается
 * ли история терминала при каждом возврате.
 *
 * Правило: в облаке клиент шлёт агенту `resume` только тем машинам, про которые
 * ЗНАЕТ, что они это умеют (≥2.49.11). Знание берётся из слепка в конфиге. Пока
 * слепок писало ровно одно место и никто не освежал, он врал двумя способами —
 * и оба означали «resume не шлём», то есть полный снимок и `term.reset()` на
 * каждом переподключении:
 *   • пусто — у всех, кто привязал ПК сканом QR, диплинком или кодом;
 *   • устарело — у всех остальных: агент обновляется САМ, а строку в
 *     localStorage переписать было нечем. После выхода 2.49.11 починка в облаке
 *     не включилась практически ни у кого.
 *
 * Здесь проверяется именно поведение слепка, потому что сам гейт (ptyWSUrl)
 * тянет за собой транспорт и в node без DOM не заводится.
 */
import { beforeEach, describe, expect, it } from "vitest";

import { agentSupportsResumeInPath } from "./streamPath";

// Мини-модель того, что делает config.ts: saveConfig раскладывает поля спредом,
// поэтому ЯВНЫЙ undefined затирает уже известное значение — на этом и терялся
// слепок при «выбрать эту же машину» без объекта устройства.
type Cfg = { selectedDeviceId?: string; selectedDeviceAgentVersion?: string };
let cfg: Cfg;
const save = (patch: Cfg) => { cfg = { ...cfg, ...patch }; };

/** Зеркало config.noteSelectedDeviceAgentVersion. */
function noteVersion(deviceId: string, version?: string | null): void {
  if (!deviceId || !version) return;
  if (cfg.selectedDeviceId !== deviceId) return;
  if (cfg.selectedDeviceAgentVersion === version) return;
  save({ selectedDeviceAgentVersion: version });
}

/** Зеркало devices.selectDevice в части версии. */
function selectDevice(deviceId: string, device?: { agent_version?: string | null }): void {
  const same = cfg.selectedDeviceId === deviceId;
  save({
    selectedDeviceId: deviceId,
    selectedDeviceAgentVersion: device?.agent_version
      || (same ? cfg.selectedDeviceAgentVersion || undefined : undefined),
  });
}

const canResume = () => agentSupportsResumeInPath(cfg.selectedDeviceAgentVersion ?? "");

beforeEach(() => { cfg = {}; });

describe("слепок версии агента", () => {
  it("привязка без объекта устройства оставляет слепок пустым — resume не шлём", () => {
    selectDevice("dev1");
    expect(canResume()).toBe(false);
  });

  it("список устройств освежает слепок, и resume включается сам", () => {
    selectDevice("dev1");
    noteVersion("dev1", "2.49.11");
    expect(cfg.selectedDeviceAgentVersion).toBe("2.49.11");
    expect(canResume()).toBe(true);
  });

  it("агент обновился сам — слепок догоняет его без участия человека", () => {
    selectDevice("dev1", { agent_version: "2.49.10" });
    expect(canResume()).toBe(false); // старый агент: путь с запросом он не разберёт
    noteVersion("dev1", "2.49.11");
    expect(canResume()).toBe(true);
  });

  it("слепок следует за фактом и НАЗАД: агент откатили — resume снова не шлём", () => {
    selectDevice("dev1", { agent_version: "2.49.11" });
    noteVersion("dev1", "2.49.10");
    expect(canResume()).toBe(false);
  });

  it("версия чужой машины в слепок не попадает", () => {
    selectDevice("dev1", { agent_version: "2.49.11" });
    noteVersion("dev2", "1.0.0");
    expect(cfg.selectedDeviceAgentVersion).toBe("2.49.11");
    expect(canResume()).toBe(true);
  });

  it("повторный выбор ТОЙ ЖЕ машины без объекта не обнуляет известную версию", () => {
    selectDevice("dev1", { agent_version: "2.49.11" });
    selectDevice("dev1");
    expect(canResume()).toBe(true);
  });

  it("переход на ДРУГУЮ машину слепок сбрасывает — версия там своя", () => {
    selectDevice("dev1", { agent_version: "2.49.11" });
    selectDevice("dev2");
    expect(cfg.selectedDeviceAgentVersion).toBeUndefined();
    expect(canResume()).toBe(false);
  });
});
