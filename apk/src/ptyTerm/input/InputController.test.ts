import { expect, it, vi } from "vitest";
import { enterIntent, reviewedInput, sameInputTarget, transmitInput } from "./InputController";

// ST-07, I-08: путь загруженного файла сверяется с целью после await так же, как вставка.
it("sameInputTarget: то же соединение, тот же процесс и тот же режим вставки", () => {
  const socket = { readyState: 1, send: vi.fn() };
  const target = { transport: socket, identity: "process-1", bracketed: true };
  expect(sameInputTarget(target, { ...target })).toBe(true);
  expect(sameInputTarget(target, { ...target, identity: "process-2" })).toBe(false);
  expect(sameInputTarget(target, { ...target, bracketed: false })).toBe(false);
  expect(sameInputTarget(target, { ...target, transport: { ...socket } })).toBe(false);
  expect(sameInputTarget(target, { ...target, transport: null })).toBe(false);
});

// T-21: Enter во время IME-композиции никогда не отправляет; поле и композер различаются.
it.each([
  ["поле: Enter", { key: "Enter" }, "field", "submit"],
  ["поле: Shift+Enter — перевод строки", { key: "Enter", shiftKey: true }, "field", "default"],
  ["поле: Ctrl+Enter тоже отправляет", { key: "Enter", ctrlKey: true }, "field", "submit"],
  ["поле: композиция (isComposing)", { key: "Enter", isComposing: true }, "field", "default"],
  ["поле: композиция (keyCode 229)", { key: "Enter", keyCode: 229 }, "field", "default"],
  ["поле: не Enter", { key: "a" }, "field", "default"],
  ["композер: Enter — перевод строки", { key: "Enter" }, "composer", "default"],
  ["композер: Shift+Enter — перевод строки", { key: "Enter", shiftKey: true }, "composer", "default"],
  ["композер: Ctrl+Enter", { key: "Enter", ctrlKey: true }, "composer", "submit"],
  ["композер: ⌘+Enter", { key: "Enter", metaKey: true }, "composer", "submit"],
  ["композер: Ctrl+Enter в композиции", { key: "Enter", ctrlKey: true, isComposing: true }, "composer", "default"],
  ["композер: ⌘+Enter с кодом 229", { key: "Enter", metaKey: true, keyCode: 229 }, "composer", "default"],
] as const)("enterIntent — %s", (_name, event, surface, intent) => {
  expect(enterIntent(event, surface)).toBe(intent);
});

it("rejects oversized UTF-8/JSON paste before the socket can accept and then drop it", () => {
  const socket = { readyState: 1, send: vi.fn() };
  expect(transmitInput(socket, { kind: "paste", text: "😀".repeat(270000), bracketed: true })).toBe(false);
  expect(socket.send).not.toHaveBeenCalled();
  expect(transmitInput(socket, { kind: "paste", text: "Я".repeat(200000), bracketed: true })).toBe(true);
  expect(socket.send).toHaveBeenCalledTimes(1);
});

it.each(["identity", "bracketed", "transport"] as const)("does not send a reviewed paste after %s changes", async field => {
  const socket = { readyState: 1, send: vi.fn() };
  const target = { transport: socket, identity: "process-1", bracketed: false };
  let live = { ...target };
  const result = await reviewedInput(target, () => live, "one\ntwo", true, async () => {
    if (field === "identity") live.identity = "process-2";
    if (field === "bracketed") live.bracketed = true;
    if (field === "transport") live = { ...live, transport: { ...socket } };
    return true;
  });
  expect(result).toBe("stale");
  expect(socket.send).not.toHaveBeenCalled();
});
it("owns one operation with Enter after the paste and reports rejection without retry", async () => {
  const socket = { readyState: 1, send: vi.fn() };
  const target = { transport: socket, identity: "same", bracketed: true };
  const review = vi.fn();
  expect(await reviewedInput(target, () => target, "one\r\ntwo", true, review)).toBe("sent");
  expect(review).not.toHaveBeenCalled();
  expect(socket.send.mock.calls).toEqual([[JSON.stringify({ t: "paste", text: "\x1b[200~one\rtwo\x1b[201~\r" })]]);
  socket.send.mockImplementation(() => { throw new Error("closed"); });
  expect(await reviewedInput(target, () => target, "held", false, review)).toBe("failed");
  expect(socket.send).toHaveBeenCalledTimes(2);
});
