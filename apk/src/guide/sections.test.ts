import { describe, expect, it } from "vitest";
import { GUIDE_GROUPS, GUIDE_SECTIONS, resolveGuideTopic, searchGuide, searchGuideShortcuts } from "./sections";
import { SECTIONS } from "../sections";
import { t } from "@tgcontrol/shared";
import { AGENT_SECTION_IDS } from "../agentsNavigation";

/** Тексты в тесте не нужны: проверяем структуру, поиск отдаёт ключ как есть. */
const asKey = (key: string) => key;

describe("состав гида", () => {
  it("каждый раздел лежит в существующей группе", () => {
    const groups = new Set(GUIDE_GROUPS.map((group) => group.id));
    for (const section of GUIDE_SECTIONS) {
      expect(groups.has(section.group), `группа ${section.group} у ${section.id}`).toBe(true);
    }
  });

  it("у раздела есть заголовок, строка-лид и хотя бы один абзац", () => {
    for (const section of GUIDE_SECTIONS) {
      expect(section.titleKey, section.id).toBeTruthy();
      expect(section.leadKey, section.id).toBeTruthy();
      expect(section.bodyKeys.length, section.id).toBeGreaterThan(0);
    }
  });

  it("идентификаторы разделов не повторяются", () => {
    const ids = GUIDE_SECTIONS.map((section) => section.id);
    expect(new Set(ids).size).toBe(ids.length);
  });

  // Гид обещает описать ВСЕ функции: если появился раздел продукта, о котором
  // здесь не сказано, человек узнает о нём случайно — как было с `remotai send`.
  it("покрывает все главные возможности", () => {
    const ids = new Set(GUIDE_SECTIONS.map((section) => section.id));
    for (const must of [
      "terminals", "answers", "files", "telegram", "agentsWork",
      "screen", "browser", "devices", "ssh", "system",
      "autostart", "logins", "updates", "limits",
      // Звук и режимы картинки: сценарий «сделать чётче и включить звук»
      // проходил примерно один человек из десяти, и в гиде о нём не было ни
      // слова (COGNITIVE-9).
      "quality", "agentSleep", "agentSessions", "agentSkills", "agentMcp",
      "agentCheck", "agentUpdate", "tokenUsage", "computerPower", "sshAdd",
    ]) {
      expect(ids.has(must), `нет раздела ${must}`).toBe(true);
    }
  });

  // Гид — карта продукта: заголовок группы обязан быть ИМЕНЕМ раздела из общей
  // таблицы, а не своим словом. Собственный ключ у гида — это ровно то, как
  // разъехались названия в прошлый раз (HOLISTIC-7).
  it("группы названы именами разделов приложения", () => {
    const byId = new Map(GUIDE_GROUPS.map((group) => [group.id, group.titleKey]));
    for (const [group, section] of [
      ["agents", "usage"], ["devices", "devices"], ["ssh", "ssh"],
      ["system", "system"], ["settings", "settings"],
    ] as const) {
      expect(byId.get(group), group).toBe(SECTIONS[section].nameKey);
    }
    // У вкладок панели строки в таблице разделов нет — имя берётся тем же
    // ключом навигации, которым подписана сама вкладка.
    expect(byId.get("terminal")).toBe("nav.terminal");
    expect(byId.get("files")).toBe("nav.files");
    expect(byId.get("remote")).toBe("nav.remote");
  });

  // Пустой заголовок группы на экране не появится (GuideView прячет группу без
  // карточек), но пустая группа означает опечатку в `group` у раздела: раздел
  // тихо пропадёт из гида, а гид обещает описать всё.
  it("в каждой группе есть хотя бы один раздел", () => {
    for (const group of GUIDE_GROUPS) {
      const count = GUIDE_SECTIONS.filter((section) => section.group === group.id).length;
      expect(count, `группа ${group.id} пуста`).toBeGreaterThan(0);
    }
  });

  it("переходы к инструментам Агентов раскрывают существующую секцию", () => {
    const ids = new Set<string>(AGENT_SECTION_IDS);
    for (const section of GUIDE_SECTIONS) {
      for (const action of section.actions ?? []) {
        if (!action.route.startsWith("/agents?")) continue;
        const focus = new URLSearchParams(action.route.split("?")[1]).get("focus");
        expect(ids.has(focus ?? ""), `${section.id}: ${action.route}`).toBe(true);
      }
    }
  });

  it.each([
    ["settingsOverview", "/settings"],
    ["autostart", "/settings?section=computer"],
    ["logins", "/settings?section=account&focus=logins"],
    ["updates", "/settings?section=connection"],
    ["peer", "/settings?section=computer"],
  ])("тема %s ведёт к своей группе настроек", (id, route) => {
    expect(GUIDE_SECTIONS.find((section) => section.id === id)?.actions?.[0]?.route).toBe(route);
  });
});

describe("поиск по гиду", () => {
  it("пустой запрос отдаёт всё", () => {
    expect(searchGuide(GUIDE_SECTIONS, "  ", asKey)).toHaveLength(GUIDE_SECTIONS.length);
  });

  it("находит по тексту команды, а не только по заголовку", () => {
    // Человек ищет то, что видел в терминале: команду целиком или её кусок.
    const found = searchGuide(GUIDE_SECTIONS, "remotai send", asKey);
    expect(found.map((section) => section.id)).toContain("telegram");
  });

  it("понимает несколько слов без точной фразы", () => {
    const found = searchGuide(GUIDE_SECTIONS, "сон компьютера", t);
    expect(found.map((section) => section.id)).toContain("computerPower");
  });

  it("находит по ключу абзаца (в проде — по его тексту)", () => {
    const found = searchGuide(GUIDE_SECTIONS, "guide.browser.body2", asKey);
    expect(found.map((section) => section.id)).toEqual(["browser"]);
  });

  it("непонятный запрос не выдаёт ничего — экран скажет об этом словами", () => {
    expect(searchGuide(GUIDE_SECTIONS, "зззз", asKey)).toHaveLength(0);
  });
});

describe("поиск по основе слова", () => {
  // Человек ищет словом в именительном падеже, а в тексте оно стоит в другом:
  // «пароль» против «пароли». Без обрезки окончания половина запросов давала
  // пустой экран при живом ответе в соседнем абзаце.
  const withText = (key: string) => ({
    "guide.logins.body2": "Пароли SSH и браузера хранятся на самом компьютере",
    "guide.ssh.body1": "помнит адреса, пароли и ключи",
  } as Record<string, string>)[key] ?? key;

  it("находит «пароли» по запросу «пароль»", () => {
    const found = searchGuide(GUIDE_SECTIONS, "пароль", withText);
    expect(found.map((s) => s.id)).toContain("logins");
  });

  it("слишком короткое слово не режется до бессмыслицы", () => {
    // «файл» (4 буквы) ищется как есть: обрезка сделала бы из него «фай».
    const found = searchGuide(GUIDE_SECTIONS, "файл", asKey);
    expect(found.every((s) => s.id !== "limits")).toBe(true);
  });
});

describe("короткий поиск переходов", () => {
  it("поднимает совпадение в названии над упоминанием в тексте", () => {
    const found = searchGuideShortcuts(GUIDE_SECTIONS, "скилл", t);
    expect(found[0]?.id).toBe("agentSkills");
  });

  it("различает сон компьютера и усыпление агента", () => {
    const found = searchGuideShortcuts(GUIDE_SECTIONS, "сон", t).map((section) => section.id);
    expect(found).toContain("computerPower");
    expect(found).toContain("agentSleep");
  });

  it.each([
    ["скилл", "agentSkills"],
    ["MCP", "agentMcp"],
    ["расход токенов", "tokenUsage"],
    ["проверить подключение", "agentCheck"],
    ["обновить агента", "agentUpdate"],
    ["добавить SSH-сервер", "sshAdd"],
  ])("находит «%s» как отдельное действие", (query, id) => {
    expect(searchGuideShortcuts(GUIDE_SECTIONS, query, t).map((section) => section.id)).toContain(id);
  });

  it.each([
    ["подключить MCP", "agentMcp"],
    ["история чатов", "agentSessions"],
    ["возобновить беседу", "agentSessions"],
    ["остановить агента", "agentSleep"],
    ["сколько токенов потратил", "tokenUsage"],
    ["проверить доступ к модели", "agentCheck"],
    ["подключить второй компьютер", "devices"],
    ["подключить новый ноутбук", "devices"],
    ["добавить компьютер", "devices"],
    ["сон ПК", "computerPower"],
    ["разбудить ПК", "computerPower"],
    ["усыпить компьютер", "computerPower"],
    ["перезагрузить ПК", "computerPower"],
    ["установить агента", "whichAgent"],
    ["подключиться по SSH", "ssh"],
    ["посмотреть экран", "screen"],
    ["загрузить файл", "files"],
    ["четкость картинки", "quality"],
    ["нагрузка процессора", "system"],
    ["агент по умолчанию", "agentBehaviour"],
    ["как настроить автозапуск", "autostart"],
  ])("«%s» ведёт сначала к нужной функции", (query, id) => {
    expect(searchGuideShortcuts(GUIDE_SECTIONS, query, t)[0]?.id).toBe(id);
  });
});

describe("тексты гида существуют в словаре", () => {
  // Гид — данные с ключами, и опечатка в ключе ничего не ломает при сборке:
  // на экране человек увидит «guide.peer.body1» вместо предложения. Проверяем
  // каждый ключ каждого раздела, включая подписи кнопок и примеров.
  it("у каждого ключа есть перевод", () => {
    const missing: string[] = [];
    const check = (key: string) => {
      const text = t(key);
      // В прод-сборке t() отдаёт сам ключ, в DEV — оборачивает в ⟦…⟧.
      if (text === key || text.startsWith("⟦")) missing.push(key);
    };
    for (const section of GUIDE_SECTIONS) {
      check(section.titleKey);
      check(section.leadKey);
      section.bodyKeys.forEach(check);
      section.examples?.forEach((example) => check(example.labelKey));
      section.actions?.forEach((action) => check(action.labelKey));
    }
    GUIDE_GROUPS.forEach((group) => check(group.titleKey));
    expect(missing).toEqual([]);
  });
});

describe("тема из адреса (?topic=) — дверь из подсказок по экрану", () => {
  // Подсказки по экрану (HelpSheet) ведут в гид с id ГРУППЫ; ссылка из текста
  // может нести id РАЗДЕЛА. Оба открывают гид на нужной теме, незнакомое —
  // обычный гид, а не пустой экран (аудит ИА 02.09.2026, P1-5, P1-31).
  it("id группы → эта группа и её первый раздел", () => {
    expect(resolveGuideTopic(GUIDE_SECTIONS, GUIDE_GROUPS, "terminal"))
      .toEqual({ group: "terminal", sectionId: "ask-agent" });
    expect(resolveGuideTopic(GUIDE_SECTIONS, GUIDE_GROUPS, "remote"))
      .toEqual({ group: "remote", sectionId: "screen" });
  });

  it("id раздела → его группа и сам раздел", () => {
    expect(resolveGuideTopic(GUIDE_SECTIONS, GUIDE_GROUPS, "snippets"))
      .toEqual({ group: "terminal", sectionId: "snippets" });
  });

  it("старая ссылка на общую карточку открывает список бесед", () => {
    expect(resolveGuideTopic(GUIDE_SECTIONS, GUIDE_GROUPS, "agentTools"))
      .toEqual({ group: "agents", sectionId: "agentSessions" });
  });

  // Группа побеждает раздел с тем же id: «files» из подсказок по экрану — это
  // группа «Файлы», даже если раздел «files» когда-нибудь переедет в другую.
  it("каждая группа открывается своим разделом — ни одна дверь из подсказок не ведёт в пустоту", () => {
    for (const group of GUIDE_GROUPS) {
      const focus = resolveGuideTopic(GUIDE_SECTIONS, GUIDE_GROUPS, group.id);
      expect(focus?.group, group.id).toBe(group.id);
      expect(GUIDE_SECTIONS.find((s) => s.id === focus?.sectionId)?.group, group.id).toBe(group.id);
    }
  });

  it("пусто, пробелы и незнакомое → null: гид открывается как обычно", () => {
    expect(resolveGuideTopic(GUIDE_SECTIONS, GUIDE_GROUPS, null)).toBeNull();
    expect(resolveGuideTopic(GUIDE_SECTIONS, GUIDE_GROUPS, "  ")).toBeNull();
    expect(resolveGuideTopic(GUIDE_SECTIONS, GUIDE_GROUPS, "nope")).toBeNull();
  });
});
