import { describe, expect, it, vi } from "vitest";
import { commitAccountScopedLaunch } from "./agentLaunchTransaction";

const previous = { id: "account-a", label: "A" };
const next = { id: "account-b", label: "B" };

describe("account metadata transaction", () => {
  it("cancel или ввод живому агенту не меняет metadata", async () => {
    const persist = vi.fn(async () => true);
    const sendToShell = vi.fn(() => true);
    const sendToExistingAgent = vi.fn(async () => false);

    await expect(commitAccountScopedLaunch({
      agentAlreadyRunning: true,
      sendToExistingAgent,
      previous,
      next,
      persist,
      sendToShell,
    })).resolves.toBe(false);
    expect(sendToExistingAgent).toHaveBeenCalledOnce();
    expect(persist).not.toHaveBeenCalled();
    expect(sendToShell).not.toHaveBeenCalled();
  });

  it("idle shell сначала фиксирует выбранный аккаунт и только затем шлёт CLI", async () => {
    const order: string[] = [];
    await expect(commitAccountScopedLaunch({
      agentAlreadyRunning: false,
      sendToExistingAgent: async () => false,
      previous,
      next,
      persist: async (marker) => { order.push(`persist:${marker.id}`); return true; },
      sendToShell: () => { order.push("send"); return true; },
    })).resolves.toBe(true);
    expect(order).toEqual(["persist:account-b", "send"]);
  });

  it("закрывшийся перед отправкой сокет возвращает прежний аккаунт", async () => {
    const order: string[] = [];
    await expect(commitAccountScopedLaunch({
      agentAlreadyRunning: false,
      sendToExistingAgent: async () => false,
      previous,
      next,
      persist: async (marker) => { order.push(`persist:${marker.id}`); return true; },
      sendToShell: () => { order.push("send:false"); return false; },
    })).resolves.toBe(false);
    expect(order).toEqual(["persist:account-b", "send:false", "persist:account-a"]);
  });

  it("ошибка metadata не запускает CLI", async () => {
    const sendToShell = vi.fn(() => true);
    await expect(commitAccountScopedLaunch({
      agentAlreadyRunning: false,
      sendToExistingAgent: async () => false,
      previous,
      next,
      persist: async () => false,
      sendToShell,
    })).rejects.toThrow("Не удалось запомнить аккаунт");
    expect(sendToShell).not.toHaveBeenCalled();
  });
});
