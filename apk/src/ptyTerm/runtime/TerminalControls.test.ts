import { expect, it } from "vitest";
import {
  FEATURE_CHOICES, FEATURE_ORDER, TERMINAL_FEATURES_KEY, defaultTerminalFeatures, parseTerminalControls,
  resetTerminalFeatures, terminalFeatures, writeTerminalFeatures,
} from "./TerminalControls";

// ST-10 не смешивается со стабилизацией (план PR-08+): viewportPan (волна 4)
// и commandBlocks (волна 6: OSC 133 шлют и сами оболочки — starship, VS Code)
// по умолчанию выключены; tapClickOnce (исправление дефекта) включён.
const DEFAULTS = { sizeOwner: true, agentHistory: true, trace: true, recoveryV1: true, retention: "policy",
  presentation: true, commandBlocks: false, viewportPan: false, capacity: true, occlusion: true, flowBacklog: true,
  navigation: "v2", inputSafety: true, tapClickOnce: true };
const stored = (raw: string | null) => ({ getItem: () => raw });

/** Хранилище-заглушка: то же, что localStorage, в одной переменной. */
function memoryStorage() {
  let raw: string | null = null;
  return {
    getItem: (key: string) => (key === TERMINAL_FEATURES_KEY ? raw : null),
    setItem: (key: string, value: string) => { if (key === TERMINAL_FEATURES_KEY) raw = value; },
    removeItem: (key: string) => { if (key === TERMINAL_FEATURES_KEY) raw = null; },
    get raw() { return raw; },
  };
}

it("keeps viewportPan and commandBlocks off by default and turns each on only with an explicit true (PR-08+)", () => {
  expect(defaultTerminalFeatures()).toEqual(DEFAULTS);
  for (const key of ["viewportPan", "commandBlocks"] as const) {
    expect(terminalFeatures(stored(`{"${key}":true}`))).toEqual({ ...DEFAULTS, [key]: true });
    for (const raw of [`{"${key}":"true"}`, `{"${key}":1}`, `{"${key}":false}`, `{"${key}":null}`, "{}"])
      expect(terminalFeatures(stored(raw))[key], raw).toBe(false);
  }
});

it("feature screen writes only differences from defaults into the same v1 key, reset removes it (9.1)", () => {
  // Каждый переключатель есть на экране ровно один раз.
  expect([...FEATURE_ORDER].sort()).toEqual(Object.keys(DEFAULTS).sort());
  expect(FEATURE_CHOICES.retention).toEqual(["legacy", "shadow", "policy"]);
  expect(FEATURE_CHOICES.navigation).toEqual(["v2", "legacy", "shadow"]);
  const s = memoryStorage();
  expect(writeTerminalFeatures(s, { ...defaultTerminalFeatures(), recoveryV1: false, viewportPan: true, commandBlocks: true,
    retention: "legacy" })).toBe(true);
  expect(JSON.parse(s.raw!)).toEqual({ recoveryV1: false, viewportPan: true, commandBlocks: true, retention: "legacy" });
  // Прочитанное парсером — ровно записанное экраном.
  expect(terminalFeatures(s)).toEqual({ ...DEFAULTS, recoveryV1: false, viewportPan: true, commandBlocks: true, retention: "legacy" });
  // Возврат к умолчаниям всех полей снимает ключ, а не пишет пустой объект.
  expect(writeTerminalFeatures(s, defaultTerminalFeatures())).toBe(true);
  expect(s.raw).toBeNull();
  writeTerminalFeatures(s, { ...defaultTerminalFeatures(), navigation: "shadow" });
  expect(JSON.parse(s.raw!)).toEqual({ navigation: "shadow" });
  expect(resetTerminalFeatures(s)).toBe(true);
  expect(s.raw).toBeNull();
  expect(terminalFeatures(s)).toEqual(DEFAULTS);
  // Непринимаемое парсером не пишется; недоступное хранилище — честный false.
  writeTerminalFeatures(s, { ...defaultTerminalFeatures(), retention: "POLICY" as never });
  expect(s.raw).toBeNull();
  expect(writeTerminalFeatures(null, DEFAULTS as never)).toBe(false);
  expect(writeTerminalFeatures({ setItem: () => { throw new Error("quota"); }, removeItem: () => {} },
    { ...defaultTerminalFeatures(), trace: false })).toBe(false);
  expect(resetTerminalFeatures({ removeItem: () => { throw new Error("blocked"); } })).toBe(false);
});

it("keeps the v1 opt-out format and falls back to defaults on any corrupt value (ST-01..ST-10)", () => {
  expect(terminalFeatures(stored(null))).toEqual(DEFAULTS);
  expect(terminalFeatures(stored('{"sizeOwner":false,"agentHistory":false}')))
    .toEqual({ ...DEFAULTS, sizeOwner: false, agentHistory: false });
  const allOff = '{"trace":false,"recoveryV1":false,"presentation":false,"commandBlocks":false,'
    + '"viewportPan":false,"capacity":false,"occlusion":false,"flowBacklog":false,"retention":"legacy","inputSafety":false,'
    + '"tapClickOnce":false}';
  expect(terminalFeatures(stored(allOff))).toEqual({ sizeOwner: true, agentHistory: true, trace: false,
    recoveryV1: false, retention: "legacy", presentation: false, commandBlocks: false, viewportPan: false,
    capacity: false, occlusion: false, flowBacklog: false, navigation: "v2", inputSafety: false, tapClickOnce: false });
  // ST-10: «одно касание — один клик» выключается только явным false.
  expect(terminalFeatures(stored('{"tapClickOnce":false}'))).toEqual({ ...DEFAULTS, tapClickOnce: false });
  expect(terminalFeatures(stored('{"tapClickOnce":0}'))).toEqual(DEFAULTS);
  // ST-08: контракт вместимости и плашки-перекрытие выключаются только явным false.
  expect(terminalFeatures(stored('{"capacity":false}'))).toEqual({ ...DEFAULTS, capacity: false });
  expect(terminalFeatures(stored('{"occlusion":false}'))).toEqual({ ...DEFAULTS, occlusion: false });
  expect(terminalFeatures(stored('{"capacity":"false","occlusion":0}'))).toEqual(DEFAULTS);
  // Порченое значение остатков ST-07 не выключает их молча.
  expect(terminalFeatures(stored('{"inputSafety":"false"}')).inputSafety).toBe(true);
  expect(terminalFeatures(stored('{"retention":"shadow"}')).retention).toBe("shadow");
  // Откат навигации (план 9.1): только известные режимы, иначе новое правило.
  expect(terminalFeatures(stored('{"navigation":"legacy"}')).navigation).toBe("legacy");
  expect(terminalFeatures(stored('{"navigation":"shadow"}')).navigation).toBe("shadow");
  for (const bad of ['{"navigation":false}', '{"navigation":"LEGACY"}', '{"navigation":["legacy"]}'])
    expect(terminalFeatures(stored(bad)).navigation).toBe("v2");
  // Порченое значение не выключает функцию молча.
  expect(terminalFeatures(stored('{"trace":"false","flowBacklog":0,"viewportPan":null,"retention":"POLICY"}'))).toEqual(DEFAULTS);
  expect(terminalFeatures(stored('{"retention":{"v":"legacy"}}')).retention).toBe("policy");
  for (const raw of ["not json", "[false]", "false", "42", '"legacy"', "null"])
    expect(terminalFeatures(stored(raw))).toEqual(DEFAULTS);
  expect(terminalFeatures({ getItem: () => { throw new Error("blocked storage"); } })).toEqual(DEFAULTS);
  // Нет localStorage вовсе (node, приватный режим) — дефолт, а не исключение.
  expect(terminalFeatures()).toEqual(DEFAULTS);
});

it("only enables known negotiated controls and allows a local rollout opt-out", () => {
  expect(terminalFeatures({ getItem: () => '{"sizeOwner":false}' })).toEqual({ ...DEFAULTS, sizeOwner: false });
  const frame = { t: "terminal-controls", v: 1, capabilities: ["size-owner-v1"], revision: 2,
    you: "1", owner: "2", lease_until: 900, viewers: [
      { id: "1", cols: 80, rows: 24, can_own: true }, { id: "2", cols: 80, rows: 40, can_own: true },
    ] };
  expect(parseTerminalControls(frame)?.owner).toBe("2");
  expect(parseTerminalControls({ ...frame, v: 2 })).toBeNull();
  expect(parseTerminalControls({ ...frame, owner: "missing" })).toBeNull();
  expect(parseTerminalControls({ ...frame, revision: 0.5 })).toBeNull();
  expect(parseTerminalControls({ ...frame, capabilities: [] })).toBeNull();
});
