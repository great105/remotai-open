import { describe, expect, it } from "vitest";
import type { AgentSessionItem } from "@tgcontrol/shared";
import type { AgentInfo } from "../types";
import {
  dayLabel, folderName, groupByDay, isPosixShell, mergePages, resumeSessionCommand,
  sessionAction, skipPermissionsFlag,
} from "./rules";

const claude = {
  id: "claude", name: "Claude Code", supports_resume: true,
  resume_id_cli: "claude --resume {session_id}",
  account_env: "CLAUDE_CONFIG_DIR",
  launch_flags: [
    { flag: "--dangerously-skip-permissions", danger: true, env: ["IS_SANDBOX=1"] },
    { flag: "--permission-mode acceptEdits", danger: true },
  ],
} as unknown as AgentInfo;
const codex = {
  id: "codex", name: "Codex CLI", supports_resume: true,
  resume_id_cli: "codex resume {session_id}",
  launch_flags: [{ flag: "--dangerously-bypass-approvals-and-sandbox", danger: true }, { flag: "--search" }],
} as unknown as AgentInfo;
const SID = "7caa5648-8121-42c3-8868-d270b407a029";

function item(extra: Partial<AgentSessionItem> = {}): AgentSessionItem {
  return { agent: "claude", session_id: SID, account_id: "default", cwd: "C:\\work\\shop", title: "t", updated_at: 0, ...extra };
}

describe("экран «Беседы»: куда ведёт нажатие", () => {
  it("открытая в терминале беседа ведёт в ЭТОТ терминал, а не запускается второй раз", () => {
    expect(sessionAction(item({ open_pty_id: "pty-7" }), claude)).toEqual({ kind: "open", ptyId: "pty-7" });
    expect(sessionAction(item({ open_pty_id: "pty-7", sleeping: true }), claude)).toEqual({ kind: "open", ptyId: "pty-7" });
  });
  it("остальные — продолжение; без реестра или с кривым номером — нельзя", () => {
    expect(sessionAction(item(), claude)).toEqual({ kind: "resume" });
    expect(sessionAction(item(), null)).toEqual({ kind: "unavailable" });
    expect(sessionAction(item({ session_id: "x; rm -rf" }), claude)).toEqual({ kind: "unavailable" });
  });
});

describe("команда продолжения", () => {
  it("без подтверждений — первый опасный флаг реестра", () => {
    expect(skipPermissionsFlag(claude)).toBe("--dangerously-skip-permissions");
    expect(skipPermissionsFlag(codex)).toBe("--dangerously-bypass-approvals-and-sandbox");
    expect(resumeSessionCommand(codex, item({ agent: "codex" }), true, { agentID: "codex" }))
      .toBe(`codex resume ${SID} --dangerously-bypass-approvals-and-sandbox`);
    expect(resumeSessionCommand(codex, item({ agent: "codex" }), false, { agentID: "codex" }))
      .toBe(`codex resume ${SID}`);
  });

  it("аккаунт беседы уходит в окружение запуска (PowerShell)", () => {
    const cmd = resumeSessionCommand(claude, item(), true, {
      agentID: "claude", accountEnvName: "CLAUDE_CONFIG_DIR", proxyContractVersion: 1,
      account: { agentID: "claude", envName: "CLAUDE_CONFIG_DIR", envValue: "C:\\Users\\Иван Петров\\.claude-work", proxyContractVersion: 1 },
    });
    expect(cmd).toContain("$env:CLAUDE_CONFIG_DIR='C:\\Users\\Иван Петров\\.claude-work'");
    expect(cmd).toContain(`claude --resume ${SID} --dangerously-skip-permissions`);
  });

  it("заблокированный аккаунт и чужой агент — пусто", () => {
    expect(resumeSessionCommand(claude, item(), false, { agentID: "claude", blockedReason: "нет аккаунта" })).toBe("");
    expect(resumeSessionCommand(codex, item(), false, { agentID: "codex" })).toBe("");
  });

  it("на Linux окружение флага идёт через env", () => {
    const cmd = resumeSessionCommand(claude, item(), true, { agentID: "claude", posix: true });
    expect(cmd).toContain("IS_SANDBOX=1");
  });
});

describe("список", () => {
  const now = new Date(2026, 8, 29, 15, 0).getTime();
  it("делит по дням, свежие сверху", () => {
    const groups = groupByDay([
      item({ session_id: "a1234567", updated_at: new Date(2026, 8, 29, 14, 0).getTime() }),
      item({ session_id: "b1234567", updated_at: new Date(2026, 8, 29, 1, 0).getTime() }),
      item({ session_id: "c1234567", updated_at: new Date(2026, 8, 28, 23, 0).getTime() }),
      item({ session_id: "d1234567", updated_at: new Date(2026, 8, 3, 9, 0).getTime() }),
      item({ session_id: "e1234567", updated_at: new Date(2025, 2, 3, 9, 0).getTime() }),
    ], now, "Сегодня", "Вчера");
    expect(groups.map((g) => [g.label, g.items.length])).toEqual([
      ["Сегодня", 2], ["Вчера", 1], ["3 сентября", 1], ["3 марта 2025", 1],
    ]);
  });
  it("вчера считается по календарю, а не по 24 часам", () => {
    expect(dayLabel(new Date(2026, 8, 28, 0, 5).getTime(), now, "Сегодня", "Вчера")).toBe("Вчера");
  });
  it("папка — последний сегмент пути", () => {
    expect(folderName("C:\\Users\\me\\proj\\")).toBe("proj");
    expect(folderName("/home/me/remotai")).toBe("remotai");
    expect(folderName("")).toBe("");
  });
  it("POSIX определяется по ОС, иначе по виду пути", () => {
    expect(isPosixShell("windows", "/x")).toBe(false);
    expect(isPosixShell("linux", "C:\\x")).toBe(true);
    expect(isPosixShell("", "C:\\x")).toBe(false);
    expect(isPosixShell(undefined, "/home/x")).toBe(true);
  });
  it("страницы склеиваются без повторов", () => {
    const a = item({ session_id: "a1234567" });
    const b = item({ session_id: "b1234567" });
    const bCodex = item({ session_id: "b1234567", agent: "codex" });
    expect(mergePages([a, b], [b, bCodex]).length).toBe(3);
  });
});
