import { describe, expect, it } from "vitest";
import { agentFocusFromSearch, agentSectionFocus } from "./agentsNavigation";

describe("ссылки на разделы Агентов", () => {
  it("открывает существующие секции по адресу", () => {
    for (const id of ["accounts", "openrouter", "limits", "tokens", "skills", "mcp", "installed", "behaviour"]) {
      expect(agentFocusFromSearch(`?focus=${id}`)).toBe(id);
    }
  });

  it("не принимает неизвестный раздел или значение вне строки", () => {
    expect(agentFocusFromSearch("?focus=unknown")).toBe("");
    expect(agentSectionFocus(null)).toBe("");
  });
});
