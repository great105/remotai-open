import { beforeEach, describe, expect, it, vi } from "vitest";

// Модуль ходит на агент — подменяем транспорт целиком: правило должно
// проверяться без сети и без DOM.
const calls: { imported: any[][]; listed: number } = { imported: [], listed: 0 };
let serverList: any[] = [];
let listThrows = false;

vi.mock("../api", () => ({
  getUserCommands: async () => {
    calls.listed += 1;
    if (listThrows) throw new Error("нет связи");
    return { commands: serverList };
  },
  importUserCommands: async (cmds: any[]) => {
    calls.imported.push(cmds);
    for (const c of cmds) {
      if (!serverList.some((x) => x.cmd === c.cmd)) serverList.push({ id: "c-" + serverList.length, ...c });
    }
    return { commands: serverList, added: cmds.length };
  },
  saveUserCommand: async (c: any) => ({ commands: serverList, command: c }),
  deleteUserCommand: async () => ({ commands: serverList }),
}));

const { loadCommands, legacyLocalCommands, pinnedOf, commandLabel } = await import("./commands");

beforeEach(() => {
  const store = new Map<string, string>();
  (globalThis as any).localStorage = {
    getItem: (k: string) => store.get(k) ?? null,
    setItem: (k: string, v: string) => { store.set(k, v); },
    removeItem: (k: string) => { store.delete(k); },
  };
  calls.imported = [];
  calls.listed = 0;
  serverList = [];
  listThrows = false;
});

describe("перенос своих команд с пульта на компьютер", () => {
  it("ряд под терминалом переносится ЗАКРЕПЛЁННЫМ — человек его туда и ставил", async () => {
    localStorage.setItem("ptyQuickCmdsCustom", JSON.stringify(["bash deploy.sh", "git status"]));
    const got = legacyLocalCommands();
    expect(got).toEqual([
      { cmd: "bash deploy.sh", pinned: true },
      { cmd: "git status", pinned: true },
    ]);
  });

  it("сниппеты переносятся НЕзакреплёнными и сохраняют своё имя", async () => {
    localStorage.setItem("tg.snippets.custom.v1", JSON.stringify([{ label: "Мой деплой", cmd: "bash deploy.sh" }]));
    expect(legacyLocalCommands()).toEqual([{ cmd: "bash deploy.sh", label: "Мой деплой", pinned: false }]);
  });

  it("одна и та же команда из обоих списков не задваивается", async () => {
    localStorage.setItem("ptyQuickCmdsCustom", JSON.stringify(["git status"]));
    localStorage.setItem("tg.snippets.custom.v1", JSON.stringify([{ label: "Статус", cmd: "git status" }]));
    expect(legacyLocalCommands()).toEqual([{ cmd: "git status", pinned: true }]);
  });

  it("мусор в старых ключах не роняет перенос", async () => {
    localStorage.setItem("ptyQuickCmdsCustom", "не json");
    localStorage.setItem("tg.snippets.custom.v1", JSON.stringify([{ nope: 1 }, "", 5]));
    expect(legacyLocalCommands()).toEqual([]);
  });

  it("перенос делается ОДИН раз на пульт, дальше просто читаем с компьютера", async () => {
    localStorage.setItem("ptyQuickCmdsCustom", JSON.stringify(["bash deploy.sh"]));
    await loadCommands();
    expect(calls.imported.length).toBe(1);
    await loadCommands();
    await loadCommands();
    expect(calls.imported.length).toBe(1);
    expect(calls.listed).toBe(2);
  });

  it("переносить нечего — сразу читаем с компьютера, лишнего запроса нет", async () => {
    serverList = [{ id: "c-1", cmd: "ls", pinned: true }];
    const got = await loadCommands();
    expect(calls.imported.length).toBe(0);
    expect(got).toEqual(serverList);
  });

  it("связи с компьютером нет — показываем своё локальное, а не пустоту", async () => {
    localStorage.setItem("pty.commands.migrated.v1", "1");
    localStorage.setItem("ptyQuickCmdsCustom", JSON.stringify(["bash deploy.sh"]));
    listThrows = true;
    const got = await loadCommands();
    expect(got.map((c) => c.cmd)).toEqual(["bash deploy.sh"]);
    expect(got[0].pinned).toBe(true);
  });
});

describe("правила показа", () => {
  it("в ряду стоят только закреплённые и только непустые", () => {
    const list = [
      { id: "1", cmd: "a", pinned: true },
      { id: "2", cmd: "b" },
      { id: "3", cmd: "", pinned: true },
    ];
    expect(pinnedOf(list).map((c) => c.cmd)).toEqual(["a"]);
  });

  it("подпись — имя, если оно дано, иначе сама команда", () => {
    expect(commandLabel({ id: "1", cmd: "bash deploy.sh", label: "Деплой" })).toBe("Деплой");
    expect(commandLabel({ id: "1", cmd: "bash deploy.sh", label: "   " })).toBe("bash deploy.sh");
    expect(commandLabel({ id: "1", cmd: "bash deploy.sh" })).toBe("bash deploy.sh");
  });
});
