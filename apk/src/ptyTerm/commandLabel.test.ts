import { describe, it, expect } from "vitest";
import { shortCommandLabel } from "./commandLabel";
import { composeLaunch, composeRemoteLaunch, EMPTY_PREFS } from "./agentLaunch";

describe("shortCommandLabel", () => {
  it("хуки агента в подпись не попадают — это служебная часть запуска", () => {
    const claude = composeLaunch("claude", { flags: ["--continue"], extra: "" }, {
      launchArgs: ["--settings", "C:\\Users\\Иван Петров\\AppData\\Local\\Remotai\\agent-hooks\\claude-hooks.json"],
    });
    expect(shortCommandLabel(claude)).toBe("claude --continue");
    const codex = composeLaunch("codex", EMPTY_PREFS, {
      launchArgs: ["-c", "notify=['C:/Users/user/AppData/Local/Programs/Remotai/remotai.exe','hook','codex']"],
    });
    expect(shortCommandLabel(codex)).toBe("codex");
    const posix = composeLaunch("codex", EMPTY_PREFS, {
      posix: true, launchArgs: ["-c", 'notify=["/usr/local/bin/remotai","hook","codex"]'],
    });
    expect(shortCommandLabel(posix)).toBe("codex");
  });

  it("короткую команду не трогает", () => {
    expect(shortCommandLabel("claude --verbose")).toBe("claude --verbose");
  });

  it("из windows-обёртки достаёт сам запуск", () => {
    // Настоящая команда запуска под аккаунтом — та, что уходит в шелл.
    const cmd = composeLaunch("claude", EMPTY_PREFS, {
      account: { envName: "CLAUDE_CONFIG_DIR", envValue: "C:\\Users\\user\\.tgcontrol-accounts\\claude\\acc-1" },
    });
    expect(cmd.length).toBeGreaterThan(300); // обёртка PowerShell длинная
    expect(shortCommandLabel(cmd)).toBe("claude");
  });

  it("из posix-префикса окружения тоже", () => {
    const cmd = composeLaunch("claude", { flags: ["--dangerously-skip-permissions"], extra: "" }, {
      posix: true,
      specs: [{ flag: "--dangerously-skip-permissions", env: ["IS_SANDBOX=1"] }],
      account: { envName: "CLAUDE_CONFIG_DIR", envValue: "/root/.remotai-accounts/claude-2" },
    });
    expect(shortCommandLabel(cmd)).toBe("claude --dangerously-skip-permissions");
  });

  it("из проверки наличия CLI на сервере", () => {
    const cmd = composeRemoteLaunch("claude", EMPTY_PREFS, {}, "нет агента", ["claude"]);
    expect(shortCommandLabel(cmd)).toBe("claude");
  });

  it("длинную команду без известной обёртки обрезает многоточием", () => {
    const long = `git log ${"--pretty=format:%h ".repeat(20)}`;
    const label = shortCommandLabel(long);
    expect(label.length).toBeLessThanOrEqual(60);
    expect(label.endsWith("…")).toBe(true);
    expect(label.startsWith("git log")).toBe(true);
  });

  it("переводы строк схлопываются: вопрос — одна строка", () => {
    expect(shortCommandLabel("echo раз\n  echo два")).toBe("echo раз echo два");
  });

  it("пустая команда остаётся пустой", () => {
    expect(shortCommandLabel("   ")).toBe("");
  });
});
