import { describe, expect, it } from "vitest";
import {
  copyTargets, defaultInstallTargets, filterSkills, installConflicts, looksLikeGitHubUrl,
  ownSkillCount, recentDeleted, skillErrorKey, skillLocationTitle, summarizeResults, writableLocations,
  type SkillBackup, type SkillLocation,
} from "./skills";

const loc = (id: string, kind: SkillLocation["kind"], names: string[], extra: Partial<SkillLocation> = {}): SkillLocation => ({
  id, agent: id.split(":")[0], agent_name: id.startsWith("codex") ? "Codex CLI" : "Claude Code", kind,
  dir: `/x/${id}`, exists: true, skills: names.map((name) => ({ name, path: `/x/${id}/${name}` })), ...extra,
});

const LOCS: SkillLocation[] = [
  loc("claude", "main", ["pdf", "impeccable"], { shared_with: ["Работа"] }),
  loc("claude:plugins", "plugins", ["docx"], { readonly: true }),
  loc("codex", "main", ["pdf"]),
  loc("codex:acc-1", "account", [], { label: "Ксюша" }),
];

describe("looksLikeGitHubUrl", () => {
  it.each([
    "https://github.com/anthropics/skills",
    "github.com/anthropics/skills",
    "https://github.com/anthropics/skills/tree/main/skills/pdf",
    "https://github.com/o/r/blob/main/x/SKILL.md",
    "https://www.github.com/o/r.git",
    "  https://github.com/o/r/  ",
  ])("принимает %s", (u) => expect(looksLikeGitHubUrl(u)).toBe(true));

  it.each([
    "",
    "anthropics/skills",
    "https://gitlab.com/o/r",
    "https://github.com/o",
    "https://github.com/o/r/issues/1",
    "https://github.com/o/r/tree",
    "https://evil.com/github.com/o/r",
    "https://github.com.evil.com/o/r",
    "ftp://github.com/o/r",
    "https://github.com/o/r with space",
  ])("отклоняет %s", (u) => expect(looksLikeGitHubUrl(u)).toBe(false));
});

describe("места и цели", () => {
  it("записываемые — без плагинов", () => {
    expect(writableLocations(LOCS).map((l) => l.id)).toEqual(["claude", "codex", "codex:acc-1"]);
  });

  it("название места", () => {
    expect(skillLocationTitle(LOCS[0], "плагины")).toBe("Claude Code");
    expect(skillLocationTitle(LOCS[1], "плагины")).toBe("Claude Code · плагины");
    expect(skillLocationTitle(LOCS[3], "плагины")).toBe("Codex CLI · Ксюша");
  });

  it("копировать можно во все, кроме исходного, и видно, где уже есть", () => {
    const t = copyTargets(LOCS, "claude", "pdf");
    expect(t.map((x) => [x.location.id, x.has])).toEqual([["codex", true], ["codex:acc-1", false]]);
  });

  it("конфликты установки только в выбранных местах", () => {
    expect(installConflicts(LOCS, ["codex:acc-1"], ["pdf"])).toEqual([]);
    expect(installConflicts(LOCS, ["claude", "codex"], ["pdf", "new"])).toEqual(["pdf"]);
  });

  it("по умолчанию — основной профиль первого агента", () => {
    expect(defaultInstallTargets(LOCS)).toEqual(["claude"]);
    expect(defaultInstallTargets([LOCS[1]])).toEqual([]);
  });

  it("свои скиллы без встроенных", () => {
    const l = loc("codex", "main", ["a", "b"]);
    l.skills.push({ name: "imagegen", path: "", readonly: true, source: "system" });
    expect(ownSkillCount(l)).toBe(2);
  });
});

describe("итоги и поиск", () => {
  it("сводка результатов", () => {
    expect(summarizeResults([
      { location: "claude", name: "a", status: "installed" },
      { location: "codex", name: "a", status: "exists" },
      { location: "codex", name: "b", status: "replaced", backup: "x" },
      { location: "x", name: "c", status: "error", error: "нет места" },
    ])).toEqual({ installed: 1, replaced: 1, exists: 1, same: 0, errors: ["нет места"] });
    expect(summarizeResults(undefined).installed).toBe(0);
  });

  it("поиск по имени, описанию и плагину", () => {
    const skills = [
      { name: "pdf", path: "", description: "Работа с PDF" },
      { name: "remotion", path: "", title: "Remotion video" },
      { name: "doc", path: "", source: "document-skills" },
    ];
    expect(filterSkills(skills, "").length).toBe(3);
    expect(filterSkills(skills, "VIDEO").map((s) => s.name)).toEqual(["remotion"]);
    expect(filterSkills(skills, "работа").map((s) => s.name)).toEqual(["pdf"]);
    expect(filterSkills(skills, "document").map((s) => s.name)).toEqual(["doc"]);
  });

  it("недавно удалённые — без заменённых и не больше лимита", () => {
    const b = (i: number, reason: SkillBackup["reason"]): SkillBackup =>
      ({ id: String(i), location: "claude", agent: "claude", name: `s${i}`, reason, from: "", at: 100 - i });
    const list = [b(1, "delete"), b(2, "replace"), b(3, "delete"), b(4, "delete")];
    expect(recentDeleted(list, 2).map((x) => x.id)).toEqual(["1", "3"]);
  });

  it("коды ошибок сервера", () => {
    expect(skillErrorKey("unsafe_archive")).toBe("skills.err.unsafe");
    expect(skillErrorKey("что-то новое")).toBeNull();
    expect(skillErrorKey(undefined)).toBeNull();
  });
});
