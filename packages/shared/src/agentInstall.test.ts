import { describe, expect, it } from "vitest";
import { usesNpm } from "./agentInstall";

describe("Node.js installation prerequisite", () => {
  it.each(["npm i -g agent", "npm.cmd i -g agent", "NPM.CMD install agent", "sudo npm i -g agent"])("recognizes %s", (command) => {
    expect(usesNpm(command)).toBe(true);
  });
  it.each(["pip install agent", "winget install Agent", "pnpm add agent", ""])("does not invent a Node prerequisite for %s", (command) => {
    expect(usesNpm(command)).toBe(false);
  });
});
