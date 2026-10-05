import { describe, expect, it } from "vitest";
import { legacyDecision, navigationShadowDiff } from "./legacyNavigation";
import { decideNavigation, type NavigationInput } from "./navigationDecision";

const base: NavigationInput = {
  override: "auto", pin: null, alt: false, mouse: "none", agent: true, declaredChannel: "",
  localCanScroll: false, owner: "application", page: "probe", wheel: "probe",
};
const input = (over: Partial<NavigationInput>): NavigationInput => ({ ...base, ...over });

describe("прежнее правило для отката и тени (план 13.09, 9.1)", () => {
  it("воспроизводит главные ветки edf251d", () => {
    // Claude в обычном буфере без своей истории — страницы.
    expect(legacyDecision(input({}), "application")).toMatchObject({ executor: "remote", channel: "page", mode: "probe" });
    // Молчащий страничный канал отдаёт свою историю.
    expect(legacyDecision(input({ localCanScroll: true, page: "local" }), "application")).toMatchObject({ executor: "local" });
    // Ручные режимы.
    expect(legacyDecision(input({ override: "terminal" }), "terminal").executor).toBe("local");
    expect(legacyDecision(input({ override: "agent", localCanScroll: true }), "application"))
      .toMatchObject({ executor: "remote", channel: "page", mode: "send" });
    // Пейджер.
    expect(legacyDecision(input({ alt: true, agent: false, mouse: "none" }), "application"))
      .toMatchObject({ executor: "remote", channel: "arrows" });
  });

  it("фиксирует дефект A02/T-05: ранняя локальная ветка обходила объявленный канал — тень его видит", () => {
    const s = input({ declaredChannel: "page", localCanScroll: true, owner: "terminal" });
    const old = legacyDecision(s, "terminal");
    const next = decideNavigation(s);
    expect(old.executor).toBe("local");
    expect(next).toMatchObject({ executor: "remote", channel: "page" });
    expect(navigationShadowDiff(next, old)).toBe("local/viewport→remote/page:declared");
  });

  it("совпавшие решения теневой журнал не засоряют", () => {
    const s = input({});
    expect(navigationShadowDiff(decideNavigation(s), legacyDecision(s, "application"))).toBe("");
  });
});
