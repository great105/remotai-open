import { describe, expect, it } from "vitest";
import type { AgentInfo } from "../types";
import { canSleepAgent, sleepBlockedByStatus, wakeCommand } from "./agentSleep";

const claude = {
  id: "claude", name: "Claude Code", supports_resume: true,
  resume_cli: "claude --continue",
  resume_id_cli: "claude --resume {session_id}",
  launch_flags: [{ flag: "--dangerously-skip-permissions", env: ["IS_SANDBOX=1"] }],
} as unknown as AgentInfo;
const SID = "7caa5648-8121-42c3-8868-d270b407a029";

describe("усыпление агента", () => {
  it("будит ту же беседу в том же режиме", () => {
    const cmd = wakeCommand(claude, {
      agent: "claude", session_id: SID, flags: ["--dangerously-skip-permissions"], at: 1,
    }, { agentID: "claude" });
    expect(cmd).toBe(`claude --resume ${SID} --dangerously-skip-permissions`);
  });

  it("на сервере добавляет окружение флага, как обычный запуск", () => {
    const cmd = wakeCommand(claude, {
      agent: "claude", session_id: SID, flags: ["--dangerously-skip-permissions"], at: 1,
    }, { agentID: "claude", posix: true });
    expect(cmd).toContain("IS_SANDBOX=1");
    expect(cmd).toContain(`--resume ${SID}`);
  });

  it("не собирает команду для чужого агента и кривого номера", () => {
    expect(wakeCommand(claude, { agent: "codex", session_id: SID, at: 1 }, {})).toBe("");
    expect(wakeCommand(claude, { agent: "claude", session_id: "x; rm -rf ~", at: 1 }, {})).toBe("");
    expect(wakeCommand(null, { agent: "claude", session_id: SID, at: 1 }, {})).toBe("");
  });

  it("усыплять можно только агента с продолжением по номеру и не посреди работы", () => {
    expect(canSleepAgent(claude)).toBe(true);
    expect(canSleepAgent({ ...claude, resume_id_cli: "" } as AgentInfo)).toBe(false);
    for (const s of ["working", "stalled", "waiting"]) expect(sleepBlockedByStatus(s)).toBe(true);
    for (const s of ["ready", "idle", undefined]) expect(sleepBlockedByStatus(s)).toBe(false);
  });
});

describe("пробуждение после жалобы 29.09 ([O в строке PowerShell)", () => {
  it("гасит режимы мыши, фокуса и вставки, не трогая alt-экран", async () => {
    const { INPUT_MODES_OFF } = await import("./agentSleep");
    for (const m of ["1000", "1002", "1003", "1004", "1006", "2004"]) expect(INPUT_MODES_OFF).toContain(`\x1b[?${m}l`);
    expect(INPUT_MODES_OFF).not.toContain("1049");
  });
  it("показывает отказ сервера его словами, а не «конфликт состояния»", async () => {
    const { sleepErrorText } = await import("./agentSleep");
    expect(sleepErrorText({ code: "waking", message: "Агент уже просыпается." })).toBe("Агент уже просыпается.");
    expect(sleepErrorText({ code: "file_in_use", message: "x" })).toBe("");
    expect(sleepErrorText(new Error("boom"))).toBe("");
  });
});

describe("очистка строки перед пробуждением (живой прогон 29.09)", () => {
  it("Esc для консоли Windows, Ctrl+U для POSIX — не Ctrl+C и не Enter", async () => {
    const { wakeClearLine } = await import("./agentSleep");
    expect(wakeClearLine(false)).toBe("\x1b");
    expect(wakeClearLine(true)).toBe("\x15");
  });
});
